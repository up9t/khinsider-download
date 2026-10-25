package main

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"iter"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/PuerkitoBio/goquery"
)

// var logger = slog.New(slog.NewJSONHandler(os.Stdout, nil))
var logger = slog.New(slog.NewTextHandler(os.Stdout, nil))
var WorkerCounts = runtime.NumCPU()

const DefaultOutDirectory = "./kids"
const MaxJobsCountsAtTime = 100

// TODO: make this a better way , maybe dynamically
const Host = "https://downloads.khinsider.com"
const (
	HTTPHeaderUserAgent    = "Mozilla/5.0 (X11; Linux x86_64; rv:158.0) Gecko/20100101 Firefox/158.0"
	HTTPHeaderAccept       = "text/html"
	HTTPHeaderSecFetchDest = "document"
	HTTPHeaderSecFetchMode = "navigate"
	HTTPHeaderSecFetchSite = "same-origin"
)

type DownloadURLs struct {
	MP3  string `json:"mp3"`
	Flac string `json:"flac"`
}

func ApplyHeaders(req *http.Request) {
	req.Header.Set("User-Agent", HTTPHeaderUserAgent)
	req.Header.Set("Accept", HTTPHeaderAccept)
	// req.Header.Set("Referer", HTTPHeaderReferer)
	req.Header.Set("Sec-Fetch-Dest", HTTPHeaderSecFetchDest)
	req.Header.Set("Sec-Fetch-Mode", HTTPHeaderSecFetchMode)
	req.Header.Set("Sec-Fetch-Site", HTTPHeaderSecFetchSite)
}

type StructWriter interface {
	Write(v any) error
}

type Metadata struct {
	Number   string `json:"number"`
	SongName string `json:"song_name"`
	Duration string `json:"duration"`
	MP3Size  string `json:"mp3_size"`
	FlacSize string `json:"flac_size"`
	// PageLink     string `json:"page_link"` // this shouldn't be visible i think
	DownloadURLs `json:"downloads_urls"`
}

type JSONStructWriter struct {
	w io.Writer
}

func (jw JSONStructWriter) Write(v any) error {
	return json.MarshalWrite(jw.w, v, jsontext.WithIndent("  "), jsontext.Multiline(true))
}

type Jobs struct {
	Index    int
	PageLink string
}

type DoneJobs struct {
	Index int
	DownloadURLs
}

func ParseURL(urls []string) ([]*url.URL, error) {
	result := make([]*url.URL, 0, len(urls))

	for _, ur := range urls {
		// If you put ur = bunchrandomstrings, it won't return error
		// Idk why they did that, so I have to an add extra empty string validation below
		r, err := url.Parse(ur)

		if err != nil {
			return nil, err
		}

		if r.Scheme == "" || r.Host == "" || r.Path == "" {
			logger.Warn("Invalid url. Skipped.",
				slog.String("url", ur),
			)
			continue
		}

		result = append(result, r)
	}

	return result, nil
}

// returns the whole html document in string
func GetDocument(client *http.Client, url *url.URL) (string, error) {
	//  *http.Request shouldn't be reused.
	req, err := http.NewRequest(http.MethodGet, url.String(), nil)

	ApplyHeaders(req)

	if err != nil {
		return "", fmt.Errorf("Failed to construct a new HTTP request. %w", err)
	}

	res, err := client.Do(req)

	if err != nil {
		return "", fmt.Errorf("Failed to perform HTTP request. %w", err)
	}

	if res.StatusCode != http.StatusOK {
		return "", fmt.Errorf("http request failed with status: %d", res.StatusCode)
	}

	// Must always close the response body, because it's a stream, as the documentation said
	defer res.Body.Close()

	// Note to self
	// make a readable stream of string
	// strings.NewReader()

	// then we need to stream data from HTTP Response to In memory variable
	// 1. make a writable stream of string
	document := strings.Builder{}

	// 2. write stream from HTTP stream -> document variable
	//// First approach
	// writer := bufio.NewWriter(&document)
	// writer.ReadFrom(res.Body)
	// writer.Flush()

	//// second approach (easier, recommended)
	_, err = io.Copy(&document, res.Body)

	if err != nil {
		return "", fmt.Errorf("Failed to read and write stream data. %w", err)
	}

	return document.String(), nil
}

// 1. write all the first results to a json file
// pro:
// - easier on internet usage
// cons:
// - user is given with unfinsihed urls

// or 2. get to the final url first before writing it to json
// pro:
// - user can immidiately the url they want to go
// cons:
// - also fetch unwanted urls

// slices is passed by value (copy), so I need to set the metadatas to use pointer
func UpdateDownloadURLs(metadatas *[]Metadata, mu *sync.Mutex, doneJobs <-chan DoneJobs, wg *sync.WaitGroup) {
	wg.Add(1)
	go func() {
		defer wg.Done()
		for job := range doneJobs {
			mu.Lock()
			(*metadatas)[job.Index].DownloadURLs = job.DownloadURLs
			mu.Unlock()
		}
	}()
}

func ProcessInputURL(client *http.Client, url *url.URL, w StructWriter) {
	document, err := GetDocument(client, url)
	wg := sync.WaitGroup{}

	if err != nil {
		logger.Error("Failed to get document", slog.Any("reason", err))
		return
	}

	jobs := make(chan Jobs, MaxJobsCountsAtTime)
	doneCh := make(chan DoneJobs)
	getUrlFinish := make(chan struct{})

	data := make([]Metadata, 0, 100)
	dataMutex := sync.Mutex{}

	// retrieve download url from doneChannel and update the `data` variable
	UpdateDownloadURLs(&data, &dataMutex, doneCh, &wg)

	// get download url and then send url to doneChannel
	// this function should close the doneChannel after finish
	go GetDownloadURL(client, jobs, doneCh, getUrlFinish)

	// data ready -> wait visiting -> return url -> place it to data -> write
	for i, meta := range GetDestinationURLFromDocument(document) {
		dataMutex.Lock()
		data = append(data, Metadata{
			Number:   meta[0],
			SongName: meta[1],
			Duration: meta[2],
			MP3Size:  meta[3],
			FlacSize: meta[4],
			// PageLink: Host + meta[5],
		})
		dataMutex.Unlock()

		jobs <- Jobs{
			Index:    i,
			PageLink: Host + meta[5],
		}

	}

	close(jobs)

	<-getUrlFinish

	close(doneCh)

	wg.Wait()

	// for _, d := range data {
	if err := w.Write(data); err != nil {
		logger.Warn("failed to write json", slog.Any("reason", err))
	}
	// }
}

func GetDownloadURL(client *http.Client, jobs <-chan Jobs, doneCh chan<- DoneJobs, finish chan<- struct{}) {
	wg := sync.WaitGroup{}

	for i := 0; i < WorkerCounts; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()

			for job := range jobs {
				u, err := url.Parse(job.PageLink)

				if err != nil {
					logger.Error("failed to parse page url", slog.Any("reason", err))
					return
				}

				document, err := GetDocument(client, u)

				if err != nil {
					logger.Error("failed to get document", slog.Any("reason", err))
					return
				}

				downloadURLs := GetDownloadURLFromDocument(document)

				doneCh <- DoneJobs{
					Index:        job.Index,
					DownloadURLs: downloadURLs,
				}
			}
		}()
	}

	wg.Wait()

	finish <- struct{}{}
}

func GetDownloadURLFromDocument(document string) DownloadURLs {
	reader := strings.NewReader(document)

	doc, err := goquery.NewDocumentFromReader(reader)

	if err != nil {
		logger.Error("failed to read document", slog.Any("reason", err))
		return DownloadURLs{}
	}

	mp3URL, ok := doc.Find("#pageContent > p:nth-child(9) > a:nth-child(1)").Attr("href")

	if !ok {
		logger.Warn("failed to retrieve href attribute")
		return DownloadURLs{}
	}

	flacURL, ok := doc.Find("#pageContent > p:nth-child(10) > a:nth-child(1)").Attr("href")

	if !ok {
		logger.Warn("failed to retrieve href attribute")
		return DownloadURLs{}
	}

	return DownloadURLs{
		MP3:  mp3URL,
		Flac: flacURL,
	}
}

// get multiple urls from a html document
func GetDestinationURLFromDocument(document string) iter.Seq2[int, [6]string] {
	reader := strings.NewReader(document)

	doc, err := goquery.NewDocumentFromReader(reader)

	if err != nil {
		logger.Error("failed to read document", slog.Any("reason", err))
		return nil
	}

	return func(yield func(int, [6]string) bool) {
		doc.Find("#songlist > tbody > tr").Slice(1, -1).Each(func(i int, row *goquery.Selection) {
			td := row.Find("td")

			number := td.Eq(1).Text()
			songName := td.Eq(2).Text()
			duration := td.Eq(3).Text()
			mp3Size := td.Eq(4).Text()
			flacSize := td.Eq(5).Text()
			// this link goes to another url, and there is an actual download url in that page
			songPageLink, ok := td.Find("a").Attr("href")

			if ok {
				if !yield(i, [6]string{number, songName, duration, mp3Size, flacSize, songPageLink}) {
					return
				}
			} else {
				logger.Warn("can't find href attribute", slog.String("song", songName))
			}
		})
	}
}

// example: ./kid https://xxxxxxx
// or maybe: ./kid download https://xxxxxxx
// kid help, kid -h, kid
// kid fetch xxx --> to fetch only
// kid download --> fetch and download
// kid download -f file1 file2--> if -f specified download only, from files
// kid download -o --> output directory/file
// -- if multiple urls after -o xxx, then o = output directory
// -- if only one url after -o xxx, then o = output file
func main() {
	urls := os.Args[1:]

	parsedURLs, err := ParseURL(urls)

	if err != nil {
		logger.Error("Failed to parse URLs.", slog.Any("urls", urls))
	}

	// Go's http.Get function doesn't allow to set headers
	// we need other way by constructing http.Client then client.Do
	// or directly using http.DefaultClient.Do
	// http.Client can be reused, and in fact, should.
	client := http.Client{
		Timeout: 30 * time.Second,
	}

	// make a buffered channel
	// stage 1 -> visit url, parse document, return download page urls
	// stage 2 -> visit url, parse document, return download url
	// stage 3 -> donwload the given url with tcp stream
	wg := sync.WaitGroup{}

	outdir := DefaultOutDirectory

	// create directory if not exists
	stat, err := os.Stat(outdir)
	if errors.Is(err, os.ErrNotExist) {
		logger.Info("Directory doesn't exists, creating output directory", slog.String("directory", outdir))

		// apparently you need the x (execute) permission in directory
		// which is slightly different from what in know in file permission
		if err := os.MkdirAll(outdir, 0755); err != nil {
			logger.Error("Failed to create output directory",
				slog.String("directory", outdir),
				slog.Any("reason", err),
			)
		}
	} else if err != nil {
		logger.Error("Failed to get the stat of the output directory",
			slog.String("directory", outdir),
			slog.Any("reason", err),
		)
		return
	} else if !stat.IsDir() {
		logger.Error("Output directory is not a directory",
			slog.String("file", outdir),
			slog.String("action", "Delete or move the file"),
		)
		return
	}

	for _, url := range parsedURLs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			logger.Info("Searching", slog.String("url", url.String()))

			outname := filepath.Join(outdir, path.Base(url.Path)) + ".json"

			f, err := os.OpenFile(outname, os.O_CREATE|os.O_WRONLY, 0644)

			if err != nil {
				logger.Error("failed to open file",
					slog.String("filename", outname),
					slog.Any("reason", err),
				)
				return
			}

			jsonw := JSONStructWriter{w: f}
			ProcessInputURL(&client, url, jsonw)
		}()
	}

	wg.Wait()
	logger.Info("Done!")
}

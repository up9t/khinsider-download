package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"iter"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/PuerkitoBio/goquery"
)

// NOTE TO SELF:
// using pprof really helps with debugging waiting goroutines

// var logger = slog.New(slog.NewJSONHandler(os.Stdout, nil))
var logger = slog.New(slog.NewTextHandler(os.Stdout, nil))

const DefaultOutDirectory = "./kids"
const MaxJobsCountsAtTime = 100

// TODO: make this a better way , maybe dynamically
const Host = "https://downloads.khinsider.com"

const (
	DownloadFormatMP3 = iota
	DownloadFormatFlac
)

type DownloadURLs struct {
	MP3  string `json:"mp3"`
	Flac string `json:"flac"`
}

type StructWriter interface {
	Write(v any) error
}

type Data struct {
	Number   string `json:"number"`
	SongName string `json:"song_name"`
	Duration string `json:"duration"`
	MP3Size  string `json:"mp3_size"`
	FlacSize string `json:"flac_size"`

	DownloadURLs `json:"downloads_urls"`
	// group for collector
	Group string `json:"-"`
}

type DataQueueType = int

const (
	Init DataQueueType = iota
	Update
)

type DataQueue struct {
	Type DataQueueType
	Data *Data
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

// get to the final url first before writing it to json
// pro:
// - user can immidiately the url they want to go
// cons:
// - fetch unwanted urls

// slices is passed by value (copy), so I need to set the metadatas to use pointer

func GetDownloadPageURLs(client *http.Client, url *url.URL) (iter.Seq[[6]string], error) {
	document, err := GetDocument(client, url)

	if err != nil {
		return nil, err
	}

	return GetDownloadPageURLFromDocument(document), nil
}

func GetDownloadURL(client *http.Client, url *url.URL) (DownloadURLs, error) {
	document, err := GetDocument(client, url)

	if err != nil {
		return DownloadURLs{}, err
	}

	return GetDownloadURLFromDocument(document), nil
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
func GetDownloadPageURLFromDocument(document string) iter.Seq[[6]string] {
	reader := strings.NewReader(document)

	doc, err := goquery.NewDocumentFromReader(reader)

	if err != nil {
		logger.Error("failed to read document", slog.Any("reason", err))
		return nil
	}

	// document title
	// #pageContent > h2:nth-child(2)

	return func(yield func([6]string) bool) {
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
				if !yield([6]string{number, songName, duration, mp3Size, flacSize, songPageLink}) {
					return
				}
			} else {
				logger.Warn("can't find href attribute", slog.String("song", songName))
			}
		})
	}
}

func CreateDirectoryIfNotExists(dirname string) error {
	stat, err := os.Stat(dirname)

	if errors.Is(err, os.ErrNotExist) {
		logger.Info("Directory doesn't exists, creating output directory", slog.String("directory", dirname))

		// apparently you need the x (execute) permission in directory
		// which is slightly different from what is known in file permission
		if err := os.MkdirAll(dirname, 0755); err != nil {
			return fmt.Errorf("Failed to create output directory: %s%w\n", dirname, err)
		}

	} else if err != nil {
		return fmt.Errorf("Failed to get the stat of the output directory %s%w\n", dirname, err)
	} else if !stat.IsDir() {
		return fmt.Errorf("Output directory is not a directory: %s", dirname)
	}

	return nil
}

// example: ./kid https://xxxxxxx
// or maybe: ./kid download https://xxxxxxx
// kid help, kid -h, kid
// kid fetch xxx --> to fetch only
// WILL download mp3 by default, can be set with
// --format mp3, --format flac
// kid download --> fetch and download
// kid download -f file1 file2--> if -f --file specified download only, from files
// kid download -o --> output directory/file
// -- if multiple urls after -o xxx, then o = output directory
// -- if only one url after -o xxx, then o = output file
func main() {
	// if len(os.Args) > 0 {
	// 	subcommand = os.Args[1]
	// }

	// return

	urls := os.Args[1:]

	// one thing I realized, when we execute the binary with -h flag
	// the code below .Parse() will not be executed
	flag.Parse()

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

	if err := CreateDirectoryIfNotExists(outdir); err != nil {
		logger.Error("Failed to create directory",
			slog.Any("reason", err))
		return
	}

	type Stat struct {
		Group string
		Total int
	}

	type DownloadQueue struct {
		Group string
		Name  string
		Url   *url.URL
	}

	type WriteQueue struct {
		Group string
		Data  []Data
	}

	// visit download page url -> visit and get download url -> update data -> write to file
	statChange := make(chan Stat)
	visitPageQueue := make(chan *url.URL, 5)
	visitDownloadQueue := make(chan DownloadQueue, 10)
	dataQueue := make(chan DataQueue, 40)
	collectorQueue := make(chan *Data, 5)
	writeQueue := make(chan WriteQueue, 5)

	dataProducerGroup := sync.WaitGroup{}

	wg.Go(func() {
		defer close(visitPageQueue)

		for _, url := range parsedURLs {
			visitPageQueue <- url
		}
	})

	// visit main page
	dataProducerGroup.Add(1)
	wg.Go(func() {
		workerCount := 5

		wgg := sync.WaitGroup{}
		wgg.Add(workerCount)

		defer close(visitDownloadQueue)
		defer close(statChange)
		defer dataProducerGroup.Done()

		for range workerCount {
			go func() {
				defer wgg.Done()

				for url := range visitPageQueue {
					logger.Info("Searching", slog.String("url", url.String()))

					filename := filepath.Join(outdir, path.Base(url.Path)) + ".json"

					// create output file to test permission
					file, err := os.OpenFile(filename, os.O_CREATE|os.O_WRONLY, 0644)
					if err != nil {
						logger.Error("failed to open file",
							slog.String("filename", filename),
							slog.Any("reason", err),
						)
						return
					}
					file.Close()

					result, err := GetDownloadPageURLs(&client, url)

					if err != nil {
						logger.Error("failed to get download page urls",
							slog.Any("reason", err),
						)
					}

					total := 0

					for r := range result {
						uri := Host + r[5]
						url, err := url.Parse(uri)

						if err != nil {
							logger.Error("failed to parse url",
								slog.String("url", uri),
								slog.Any("reason", err),
							)
							continue
						}

						total += 1

						statChange <- Stat{
							Group: filename,
							Total: total,
						}
						visitDownloadQueue <- DownloadQueue{
							Group: filename,
							Name:  r[1],
							Url:   url,
						}
						dataQueue <- DataQueue{
							Type: Init,
							Data: &Data{
								Group:    filename,
								Number:   r[0],
								SongName: r[1],
								Duration: r[2],
								MP3Size:  r[3],
								FlacSize: r[4],
							},
						}
					}

				}
			}()
		}

		wgg.Wait()
	})

	// visit download page
	dataProducerGroup.Add(1)
	wg.Go(func() {
		workerCount := 5

		wgg := sync.WaitGroup{}
		wgg.Add(workerCount)

		defer dataProducerGroup.Done()

		for range workerCount {
			go func() {
				defer wgg.Done()

				for downloadQueue := range visitDownloadQueue {

					downUrl, err := GetDownloadURL(&client, downloadQueue.Url)

					if err != nil {
						logger.Error("failed to get download url",
							slog.String("url", downloadQueue.Url.String()),
							slog.Any("reason", err),
						)
					}

					dataQueue <- DataQueue{
						Type: Update,
						Data: &Data{
							SongName:     downloadQueue.Name,
							Group:        downloadQueue.Group,
							DownloadURLs: downUrl,
						},
					}
				}
			}()
		}

		wgg.Wait()
	})

	// retrieve download url and update the `data` variable
	wg.Go(func() {
		// url 1:
		// - song name: [song data]  (fast lookup)
		dataMap := make(map[string]map[string]*Data)
		defer close(collectorQueue)

		for data := range dataQueue {
			switch data.Type {
			case Init:
				group, ok := dataMap[data.Data.Group]

				if !ok {
					group = make(map[string]*Data)
					dataMap[data.Data.Group] = group
				}

				group[data.Data.SongName] = data.Data
			case Update:
				group := dataMap[data.Data.Group]
				result := group[data.Data.SongName]
				result.DownloadURLs = data.Data.DownloadURLs

				collectorQueue <- result

				delete(dataMap[data.Data.Group], data.Data.SongName)
			}
		}
	})

	// collector
	wg.Go(func() {
		group := make(map[string][]Data)
		stats := make(map[string]Stat)
		defer close(writeQueue)

		ca := true
		cb := true

		for ca || cb {
			select {
			case stat, ok := <-statChange:
				if !ok {
					ca = false
					continue
				}

				stats[stat.Group] = stat
			case collect, ok := <-collectorQueue:
				if !ok {
					cb = false
					continue
				}

				// just because group[collect.Group] is a slice, doesn't mean group[collect.Group]
				// will automatically reflect variable g
				// you need to assign it again after doing operation of g
				g := group[collect.Group]
				// idk if using * is a good idea
				g = append(g, *collect)

				group[collect.Group] = g

				if len(g) >= stats[collect.Group].Total {
					// TODO: sort the result
					// this function doesn't work because "2" is bigger than "10"
					// slices.SortFunc(g, func(a Data, b Data) int {
					// 	return cmp.Compare(a.Number, b.Number)
					// })

					writeQueue <- WriteQueue{
						Group: collect.Group,
						Data:  g,
					}
					delete(group, collect.Group)
				}
			}
		}
	})

	// write
	wg.Go(func() {
		workerCount := 5

		wgg := sync.WaitGroup{}
		wgg.Add(workerCount)

		for range workerCount {
			go func() {
				defer wgg.Done()
				for write := range writeQueue {
					filename := write.Group

					file, err := os.OpenFile(filename, os.O_CREATE|os.O_WRONLY, 0644)
					if err != nil {
						logger.Error("failed to open file",
							slog.String("filename", filename),
							slog.Any("reason", err),
						)
						return
					}
					defer file.Close()

					jsonw := NewJSONStructWriter(file)

					// json.MarshalWrite(os.Stdout, write.Data, jsontext.WithIndent("  "), jsontext.Multiline(true))

					if err := jsonw.Write(write.Data); err != nil {
						logger.Warn("failed to write json", slog.Any("reason", err))
					}
				}
			}()
		}

		wgg.Wait()
	})

	wg.Go(func() {
		dataProducerGroup.Wait()
		close(dataQueue)
	})

	wg.Wait()

	logger.Info("Done!")
}

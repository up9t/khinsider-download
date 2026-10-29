package main

import (
	"cmp"
	"context"
	"encoding/json/v2"
	"errors"
	"flag"
	"fmt"
	"io"
	"iter"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/PuerkitoBio/goquery"
)

// NOTE TO SELF:
// using pprof really helps with debugging waiting goroutines

// var logger = slog.New(slog.NewJSONHandler(os.Stdout, nil))
var logger = slog.New(slog.NewTextHandler(os.Stdout, nil))

const DefaultOutDataDirectory = "./kidmeta"
const DefaultOutDownloadDirectory = "./downloads"
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

type StructReader interface {
	Read()
}

type Data struct {
	Number   int    `json:"number"`
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

type Stat struct {
	Group string
	Total int
}

type VisitDownloadQueue struct {
	Group string
	Name  string
	Url   *url.URL
}

type WriteQueue struct {
	Group string
	Data  []Data
}

type Filename = string

type DownloadQueue struct {
	OutPath string
	URL     *url.URL
}

func ParseURLs(urls []string) ([]*url.URL, error) {
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
	//  NOTE TO SELF:
	// *http.Request shouldn't be reused.
	req, err := http.NewRequest(http.MethodGet, url.String(), nil)

	ApplyHeaders(req)

	if err != nil {
		return "", fmt.Errorf("failed to construct a new HTTP request. %w", err)
	}

	res, err := client.Do(req)

	if err != nil {
		return "", fmt.Errorf("failed to perform HTTP request. %w", err)
	}

	if res.StatusCode != http.StatusOK {
		return "", fmt.Errorf("http request failed with status: %d", res.StatusCode)
	}

	// NOTE TO SELF:
	// Must always close the response body, because it's a stream, as the documentation said
	defer res.Body.Close()

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
		return "", fmt.Errorf("failed to read and write stream data. %w", err)
	}

	return document.String(), nil
}

// NOTE TO SELF (DELETE LATER): slices is passed by value (copy), so I need to set the metadatas to use pointer
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
		return DownloadURLs{}
	}

	// not every song have flac download so, p:nth-child(10) may not be available
	flacURL, ok := doc.Find("#pageContent > p:nth-child(10) > a:nth-child(1)").Attr("href")

	if !ok {
		flacURL = ""
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

	hasNumber := 0
	hasMp3 := 0
	hasFlac := 0

	doc.Find("#songlist_header > th").Each(func(i int, s *goquery.Selection) {
		switch strings.ToLower(s.Text()) {
		case "#":
			hasNumber = 1
		case "mp3":
			hasMp3 = 1
		case "flac":
			hasFlac = 1
		}
	})

	return func(yield func([6]string) bool) {
		doc.Find("#songlist > tbody > tr").Slice(1, -1).Each(func(i int, row *goquery.Selection) {
			td := row.Find("td")

			number := fmt.Sprint(i + 1)

			songName := td.Eq(hasNumber + 1).Text()
			duration := td.Eq(hasNumber + 2).Text()
			mp3Size := ""

			if hasMp3 == 1 {
				mp3Size = td.Eq(hasNumber + 3).Text()
			}

			flacSize := ""

			if hasFlac == 1 {
				flacSize = td.Eq(hasNumber + hasMp3 + 3).Text()
			}

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

		// NOTE TO SELF:
		// apparently you need the x (execute) permission in directory
		// which is slightly different from what is known in file permission
		if err := os.MkdirAll(dirname, 0755); err != nil {
			return fmt.Errorf("failed to create output directory: %s%w", dirname, err)
		}

	} else if err != nil {
		return fmt.Errorf("failed to get the stat of the output directory %s%w", dirname, err)
	} else if !stat.IsDir() {
		return fmt.Errorf("output directory is not a directory: %s", dirname)
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
	start := time.Now()

	// NOTE TO SELF:
	// the reason why Interrupt signal is in the os package is because it is available in all operating systems (windows, linux, mac)
	// while SIGTERM, we use syscall.SIGTERM, because it is a UNIX standard (no windows)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)

	defer stop()

	defer func() {
		duration := time.Since(start)
		logger.Info("Done!", slog.String("duration", duration.String()))
	}()

	urls := os.Args[1:]

	// NOTE TO SELF:
	// one thing I realized, when we execute the binary with -h flag
	// the code below .Parse() will not be executed
	flag.Parse()

	parsedURLs, err := ParseURLs(urls)

	if err != nil {
		logger.Error("Failed to parse URLs.", slog.Any("urls", urls))
	}

	// NOTE TO SELF:
	// Go's http.Get function doesn't allow to set headers
	// we need other way by constructing http.Client then client.Do
	// or directly using http.DefaultClient.Do
	// http.Client can be reused, and in fact, should.
	client := http.Client{
		Timeout: 30 * time.Second,
	}

	wg := sync.WaitGroup{}

	outdatadir := DefaultOutDataDirectory
	outDownloadDir := DefaultOutDownloadDirectory

	if err := CreateDirectoryIfNotExists(outdatadir); err != nil {
		logger.Error("Failed to create directory",
			slog.Any("reason", err))
		return
	}

	// visit download page url -> visit and get download url -> update data -> write to file -> read and parse the file -> download
	// adjust the number, I just put the number intuitively there
	statChange := make(chan Stat)
	visitPageQueue := make(chan *url.URL, 5)
	visitDownloadQueue := make(chan VisitDownloadQueue, 40)
	dataQueue := make(chan DataQueue, 40)
	collectorQueue := make(chan *Data, 5)
	writeQueue := make(chan WriteQueue, 5)
	readQueue := make(chan Filename, 2)
	downloadQueue := make(chan DownloadQueue, 5)

	// array of pathname
	fileRemovalQueue := make([]Filename, 0, 10)

	dataProducerGroup := sync.WaitGroup{}

	wg.Go(func() {
		defer close(visitPageQueue)

		for _, url := range parsedURLs {
			// NOTE TO SELF:
			// check ctx.Err will check if the ctx is closed.
			if ctx.Err() != nil {
				return
			}

			// NOTE TO SELF:
			// even though ctx.Err above will stop all the remaining urls
			// it doesn't however, stop the waiting-to-send operation, this select does
			select {
			case <-ctx.Done():
				return
			case visitPageQueue <- url:
			}
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
					if ctx.Err() != nil {
						return
					}

					logger.Info("Searching", slog.String("url", url.String()))

					filename := filepath.Join(outdatadir, path.Base(url.Path)) + ".json"

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
						// TODO: implement backoff instead of directly returns nothing
						logger.Error("failed to get download page urls",
							slog.Any("reason", err),
						)
						continue
					}

					total := 0
					i := 0

					for r := range result {
						i += 1
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

						visitDownloadQueue <- VisitDownloadQueue{
							Group: filename,
							Name:  r[1],
							Url:   url,
						}

						dataQueue <- DataQueue{
							Type: Init,
							Data: &Data{
								Group:    filename,
								Number:   i,
								SongName: r[1],
								Duration: r[2],
								MP3Size:  r[3],
								FlacSize: r[4],
							},
						}
					}

					statChange <- Stat{
						Group: filename,
						Total: total,
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

				for visitDownload := range visitDownloadQueue {
					if ctx.Err() != nil {
						return
					}

					downUrl, err := GetDownloadURL(&client, visitDownload.Url)

					if err != nil {
						logger.Error("failed to get download url",
							slog.String("url", visitDownload.Url.String()),
							slog.Any("reason", err),
						)
					}

					select {
					case <-ctx.Done():
						return
					case dataQueue <- DataQueue{
						Type: Update,
						Data: &Data{
							SongName:     visitDownload.Name,
							Group:        visitDownload.Group,
							DownloadURLs: downUrl,
						},
					}:
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
			if ctx.Err() != nil {
				return
			}

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

				select {
				case <-ctx.Done():
					return
				case collectorQueue <- result:
				}

				delete(dataMap[data.Data.Group], data.Data.SongName)
			}
		}
	})

	// collector
	wg.Go(func() {
		group := make(map[string][]Data)
		stats := make(map[string]Stat)
		defer close(writeQueue)

		statWait := true
		collectWait := true

		for statWait || collectWait {
			if ctx.Err() != nil {
				return
			}

			select {
			case <-ctx.Done():
				return
			case stat, ok := <-statChange:
				if !ok {
					statWait = false
					continue
				}

				stats[stat.Group] = stat
			case collect, ok := <-collectorQueue:
				if !ok {
					collectWait = false
					continue
				}

				// just because group[collect.Group] is a slice, doesn't mean group[collect.Group]
				// will automatically reflect variable g
				// you need to assign it again after doing operation of g
				g := group[collect.Group]
				// idk if using * is a good idea
				g = append(g, *collect)

				group[collect.Group] = g

				if _, ok := stats[collect.Group]; ok && len(g) >= stats[collect.Group].Total {
					slices.SortFunc(g, func(a Data, b Data) int {
						return cmp.Compare(a.Number, b.Number)
					})

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
		defer close(readQueue)

		for range workerCount {
			go func() {
				defer wgg.Done()
				for write := range writeQueue {
					if ctx.Err() != nil {
						return
					}

					filename := write.Group

					file, err := os.OpenFile(filename, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0644)
					if err != nil {
						logger.Error("failed to open file",
							slog.String("filename", filename),
							slog.Any("reason", err),
						)
						return
					}

					jsonw := NewJSONStructWriter(file)

					if err := jsonw.Write(write.Data); err != nil {
						logger.Error("failed to write json", slog.Any("reason", err))
						file.Close()
						continue
					}

					file.Close()

					select {
					case <-ctx.Done():
						return
					case readQueue <- filename:
					}
				}
			}()
		}

		wgg.Wait()
	})

	// read
	wg.Go(func() {
		defer close(downloadQueue)

		for filename := range readQueue {
			if ctx.Err() != nil {
				return
			}

			file, err := os.Open(filename)

			if err != nil {
				logger.Error("failed to open file",
					slog.String("filename", filename),
					slog.Any("reason", err),
				)

				continue
			}

			data := make([]Data, 0, 10)

			// TODO: using this is good, but maybe add a little abstraction so it stays consistent with the writer
			if err := json.UnmarshalRead(file, &data); err != nil {
				logger.Error("failed to read file",
					slog.String("filename", filename),
					slog.Any("reason", err),
				)

				file.Close()

				continue
			}

			outdir := filepath.Join(outDownloadDir, filepath.Base(filename))

			entries, _ := os.ReadDir(outdir)

			// cache, download only the needed file
			existsMap := make(map[string]struct{})

			for _, entry := range entries {
				existsMap[entry.Name()] = struct{}{}
			}

			// send each data
			for _, d := range data {
				// TODO: dyanmically choose either flac/mp3 based on user choice, fallback to mp3 if flac unavailable
				filename := d.SongName + ".mp3"
				outpath := filepath.Join(outdir, filename)
				url, err := url.Parse(d.MP3)

				if err != nil {
					logger.Error("failed to parse download url",
						slog.String("url", d.MP3),
						slog.Any("reason", err),
					)
					continue
				}

				if _, exists := existsMap[filename]; !exists {
					select {
					case <-ctx.Done():
						return
					case downloadQueue <- DownloadQueue{
						OutPath: outpath,
						URL:     url,
					}:
					}
				}
			}

			file.Close()
		}
	})

	// download
	wg.Go(func() {
		workerCount := 5

		wgg := sync.WaitGroup{}
		wgg.Add(workerCount)

		for range workerCount {
			go func() {
				defer wgg.Done()
				for download := range downloadQueue {
					if ctx.Err() != nil {
						return
					}

					func() {
						url := download.URL.String()
						resp, err := client.Get(url)

						if err != nil {
							logger.Error("failed to download",
								slog.String("url", url),
								slog.Any("reason", err),
							)

							return
						}

						defer resp.Body.Close()

						dir := filepath.Dir(download.OutPath)

						if err := CreateDirectoryIfNotExists(dir); err != nil {
							logger.Error("failed to create directory",
								slog.String("directory", dir),
								slog.Any("reason", err),
							)

							return
						}

						file, err := os.OpenFile(download.OutPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY|os.O_TRUNC, 0644)

						if err != nil {
							logger.Error("failed to open file",
								slog.String("file", download.OutPath),
								slog.Any("reason", err),
							)

							return
						}

						defer file.Close()

						logger.Info("Downloading",
							slog.String("name", filepath.Base(download.OutPath)),
						)

						done := make(chan struct{})
						defer close(done)

						// stopped by the user
						intentional := false

						go func() {
							select {
							case <-ctx.Done():
								// either close the writer or the reader to stop the io.Copy
								// file.Close()
								intentional = true
								resp.Body.Close()
							case <-done:
							}
						}()

						if _, err = io.Copy(file, resp.Body); err != nil {
							if !intentional {
								logger.Error("Failed to write downloaded bytes to a file",
									slog.String("file", download.OutPath),
									slog.Any("reason", err),
								)
							}

							fileRemovalQueue = append(fileRemovalQueue, download.OutPath)
							return
						}
					}()
				}
			}()
		}

		wgg.Wait()
	})

	wg.Go(func() {
		dataProducerGroup.Wait()
		close(dataQueue)
	})

	done := make(chan struct{})

	go func() {
		wg.Wait()
		done <- struct{}{}
	}()

	select {
	case <-ctx.Done():
		logger.Info("Cancelling the process", slog.String("reason", "Cancel signal retrieved"))
	case <-done:
	}

	wg.Wait()

	if len(fileRemovalQueue) > 0 {
		logger.Info("Performing removal of unfinished downloaded files")

		for _, filename := range fileRemovalQueue {
			logger.Info("Removing unfinished file", slog.String("filename", filename))
			if err = os.Remove(filename); err != nil {
				logger.Warn("Failed to remove unfinished file",
					slog.String("filename", filename),
					slog.Any("reason", err),
				)
			}
		}
	}
}

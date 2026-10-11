package main

import (
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"
)

type Queue[T any] interface {
	Len() int
	Enqueue(T)
	Dequeue() T
}

type InMemoryQueue[T any] struct {
	items []T
	head  int
	// mutex is used if you have multiple processes access to the same variable
	// preventing race condition. It works by acquiring and releasing lock
	mu *sync.Mutex
	// sync.Cond solves another problem, that is, what would the workers/consumers do
	// if there's no more items to consume/process, you tell them to wait/sleep,
	// and wake them up later when there is a task available, calling .Signal will wake up one consumer,
	// and .Broadcast will wake up all the consumers that are waiting.
	cond *sync.Cond
}

func (q *InMemoryQueue[T]) Enqueue(t T) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.items = append(q.items, t)
}

func (q *InMemoryQueue[T]) Dequeue() T {
	q.mu.Lock()
	defer q.mu.Unlock()

	i := q.head
	q.head++
	return q.items[i]
}

// var logger = slog.New(slog.NewJSONHandler(os.Stdout, nil))
var logger = slog.New(slog.NewTextHandler(os.Stdout, nil))

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

	if err != nil {
		return "", fmt.Errorf("Failed to construct a new HTTP request. %w", err)
	}

	res, err := client.Do(req)

	if err != nil {
		return "", fmt.Errorf("Failed to perform HTTP request. %w", err)
	}

	// Must always close the response body, because it's a stream
	defer res.Body.Close()

	// note to self
	// make a readable stream of string
	// strings.NewReader()

	// then we need to stream data from HTTP Response to In memory variable
	// 1. make a writable stream of string
	document := strings.Builder{}

	// 2. write stream from HTTP stream -> document variable
	// first approach
	// writer := bufio.NewWriter(&document)
	// writer.ReadFrom(res.Body)
	// writer.Flush()

	// second approach (easier, recommended)
	_, err = io.Copy(&document, res.Body)

	if err != nil {
		return "", fmt.Errorf("Failed to read and write stream data. %w", err)
	}

	return document.String(), nil
}

func ProcessInputURL(client *http.Client, url *url.URL, queue Queue) {
	document, err := GetDocument(client, url)

	if err != nil {
		logger.Error("Failed to get document", slog.Any("reason", err))
		return
	}

	processDocuments(document)

	queue.Enqueue
	// TODO: do something with the document
}

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

	for _, url := range parsedURLs {
		// parse immidiately and returns all the urls
		// using another function to parse document table into list of urls
		go ProcessInputURL(&client, url)
	}

}

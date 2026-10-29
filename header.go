package main

import "net/http"

const (
	HTTPHeaderUserAgent    = "Mozilla/5.0 (X11; Linux x86_64; rv:158.0) Gecko/20100101 Firefox/158.0"
	HTTPHeaderAccept       = "text/html"
	HTTPHeaderSecFetchDest = "document"
	HTTPHeaderSecFetchMode = "navigate"
	HTTPHeaderSecFetchSite = "same-origin"
)

func ApplyHeaders(req *http.Request) {
	req.Header.Set("User-Agent", HTTPHeaderUserAgent)
	req.Header.Set("Accept", HTTPHeaderAccept)
	req.Header.Set("Sec-Fetch-Dest", HTTPHeaderSecFetchDest)
	req.Header.Set("Sec-Fetch-Mode", HTTPHeaderSecFetchMode)
	req.Header.Set("Sec-Fetch-Site", HTTPHeaderSecFetchSite)
}

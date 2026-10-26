package main

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"io"
)

type JSONStructWriter struct {
	w io.Writer
}

func (jw JSONStructWriter) Write(v any) error {
	return json.MarshalWrite(jw.w, v, jsontext.WithIndent("  "), jsontext.Multiline(true))
}

func NewJSONStructWriter(w io.Writer) JSONStructWriter {
	return JSONStructWriter{w}
}

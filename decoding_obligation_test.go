package http

import (
	"bytes"
	"compress/flate"
	"compress/gzip"
	"compress/zlib"
	"github.com/andybalholm/brotli"
	"github.com/klauspost/compress/zstd"
	"io"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func encodedObligation(t *testing.T, coding string) []byte {
	t.Helper()
	var b bytes.Buffer
	var w io.WriteCloser
	switch coding {
	case "br":
		w = brotli.NewWriter(&b)
	case "zstd":
		var err error
		w, err = zstd.NewWriter(&b)
		if err != nil {
			t.Fatal(err)
		}
	case "gzip":
		w = gzip.NewWriter(&b)
	case "deflate":
		w = zlib.NewWriter(&b)
	case "raw-deflate":
		var err error
		w, err = flate.NewWriter(&b, flate.DefaultCompression)
		if err != nil {
			t.Fatal(err)
		}
	}
	if _, err := io.WriteString(w, "complete decoded body"); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func TestDecodingKeepsUnencodedMetadata(t *testing.T) {
	for _, coding := range []string{"", "identity", "unknown", "GZip", "deflate", "br", "zstd"} {
		t.Run(coding, func(t *testing.T) {
			data := []byte("complete decoded body")
			encoded := coding == "GZip" || coding == "deflate" || coding == "br" || coding == "zstd"
			if encoded {
				data = encodedObligation(t, strings.ToLower(coding))
			}
			body := io.NopCloser(bytes.NewReader(data))
			res := &Response{Header: Header{"Content-Encoding": {coding}, "Content-Length": {"21"}}, Body: body, ContentLength: int64(len(data))}
			got := DecompressBody(res)
			if encoded {
				if !res.Uncompressed || res.ContentLength != -1 || res.Header.Get("Content-Encoding") != "" || res.Header.Get("Content-Length") != "" {
					t.Fatalf("encoded metadata retained: %#v", res)
				}
			} else if got != body || res.Uncompressed || res.ContentLength != int64(len(data)) || res.Header.Get("Content-Encoding") != coding || res.Header.Get("Content-Length") != "21" {
				t.Fatalf("unencoded metadata changed: %#v", res)
			}
			decoded, err := io.ReadAll(got)
			if err != nil || string(decoded) != "complete decoded body" {
				t.Fatalf("body=%q error=%v", decoded, err)
			}
			got.Close()
		})
	}
}

type decodingProbe struct {
	io.ReadCloser
	reads  atomic.Int32
	closes atomic.Int32
}

func (p *decodingProbe) Read(b []byte) (int, error) { p.reads.Add(1); return p.ReadCloser.Read(b) }
func (p *decodingProbe) Close() error               { p.closes.Add(1); return p.ReadCloser.Close() }

func TestDecodersAreLazyAndCloseTheirSource(t *testing.T) {
	for _, coding := range []string{"gzip", "deflate", "raw-deflate", "br", "zstd"} {
		for _, readFirst := range []bool{false, true} {
			t.Run(coding+map[bool]string{false: "/unread", true: "/reading"}[readFirst], func(t *testing.T) {
				reader, writer := io.Pipe()
				defer writer.Close()
				probe := &decodingProbe{ReadCloser: reader}
				kind := coding
				if kind == "raw-deflate" {
					kind = "deflate"
				}
				decoder := DecompressBodyByType(probe, kind)
				if probe.reads.Load() != 0 {
					t.Fatal("decoder eagerly reads source")
				}
				done := make(chan error, 1)
				if readFirst {
					go func() { _, err := decoder.Read(make([]byte, 1)); done <- err }()
					deadline := time.Now().Add(time.Second)
					for probe.reads.Load() == 0 {
						if time.Now().After(deadline) {
							t.Fatal("decoder never read source")
						}
						time.Sleep(time.Millisecond)
					}
				}
				closed := make(chan struct{})
				go func() { decoder.Close(); close(closed) }()
				select {
				case <-closed:
				case <-time.After(time.Second):
					t.Fatal("Close did not unblock source")
				}
				if readFirst {
					select {
					case err := <-done:
						if err == nil {
							t.Fatal("blocked read succeeded without data")
						}
					case <-time.After(time.Second):
						t.Fatal("Read remained blocked")
					}
				}
				if n, err := decoder.Read(make([]byte, 1)); n != 0 || err == nil {
					t.Fatalf("Read after Close=%d,%v", n, err)
				}
				decoder.Close()
				if probe.closes.Load() != 1 {
					t.Fatalf("source closes=%d", probe.closes.Load())
				}
			})
		}
	}
}

func TestDeflatePreservesPrefixAndCompleteBody(t *testing.T) {
	for _, coding := range []string{"deflate", "raw-deflate"} {
		source := &decodingProbe{ReadCloser: io.NopCloser(bytes.NewReader(encodedObligation(t, coding)))}
		decoder := DecompressBodyByType(source, "deflate")
		if source.reads.Load() != 0 {
			t.Fatal("eager source read")
		}
		body, err := io.ReadAll(decoder)
		if err != nil || string(body) != "complete decoded body" {
			t.Fatalf("body=%q error=%v", body, err)
		}
		decoder.Close()
		if source.closes.Load() != 1 {
			t.Fatal("underlying source not closed")
		}
	}
}

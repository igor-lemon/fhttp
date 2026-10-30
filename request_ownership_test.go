package http_test

import (
	"bytes"
	"reflect"
	"strings"
	"testing"

	. "github.com/igor-lemon/fhttp"
)

func TestRequestWritesDoNotOwnCallerHeaders(t *testing.T) {
	for _, proxy := range []bool{false, true} {
		for _, kind := range []string{"nil", "plain", "callback-input", "callback-result"} {
			t.Run(kind+map[bool]string{false: "/origin", true: "/proxy"}[proxy], func(t *testing.T) {
				req, err := NewRequest("POST", "http://owned.example/path", strings.NewReader("abc"))
				if err != nil {
					t.Fatal(err)
				}
				req.Close = true
				if kind == "nil" {
					req.Header = nil
				} else {
					req.Header = Header{"X-Input": {"original"}, HeaderOrderKey: {"host", "x-input", "x-wire", "content-length"}}
				}
				original := req.Header.Clone()
				shared := Header{"X-Wire": {"result"}, "User-Agent": {}, HeaderOrderKey: {"host", "x-wire", "content-length"}}
				sharedBefore := shared.Clone()
				calls := 0
				switch kind {
				case "callback-input":
					req.HeaderFor = func(_ string, header Header) Header {
						calls++
						header["X-Input"][0] = "wire"
						header.Set("X-Wire", "input")
						return nil
					}
				case "callback-result":
					req.HeaderFor = func(_ string, _ Header) Header { calls++; return shared }
				}
				first := ""
				for attempt := 0; attempt < 2; attempt++ {
					req.Body, err = req.GetBody()
					if err != nil {
						t.Fatal(err)
					}
					var out bytes.Buffer
					if proxy {
						err = req.WriteProxy(&out)
					} else {
						err = req.Write(&out)
					}
					if err != nil {
						t.Fatal(err)
					}
					wire := out.String()
					if attempt == 0 {
						first = wire
					} else if wire != first {
						t.Errorf("serialization accumulated state:\nfirst %q\nnext %q", first, wire)
					}
					if strings.Count(wire, "Content-Length: 3\r\n") != 1 || strings.Count(wire, "Connection: close\r\n") != 1 || !strings.HasSuffix(wire, "\r\n\r\nabc") {
						t.Errorf("incomplete or duplicate framing: %q", wire)
					}
					if !reflect.DeepEqual(req.Header, original) {
						t.Errorf("caller header changed: %#v, want %#v", req.Header, original)
					}
					if !reflect.DeepEqual(shared, sharedBefore) {
						t.Errorf("callback result changed: %#v", shared)
					}
					if kind == "callback-input" && !strings.Contains(wire, "X-Input: wire\r\n") {
						t.Errorf("callback changes did not reach wire: %q", wire)
					}
				}
				if strings.HasPrefix(kind, "callback-") && calls != 2 {
					t.Errorf("callback calls=%d,want2", calls)
				}
			})
		}
	}
}

// These are raw caller fields, not browser Fetch overrides. A declared length
// must delimit the complete body even when Request.ContentLength is unknown.
func TestRequestExplicitFraming(t *testing.T) {
	for _, proxy := range []bool{false, true} {
		for _, name := range []string{"Content-Length", "content-length", "cOnTeNt-LeNgTh"} {
			for _, length := range []int64{3, -1} {
				req, _ := NewRequest("POST", "http://owned.example/", strings.NewReader("abc"))
				req.ContentLength = length
				req.Header = Header{name: {"003"}, "uSeR-aGeNt": {}, "hOsT": {"owned.example"}, "connection": {"close"}}
				req.Close = true
				original := req.Header.Clone()
				var out bytes.Buffer
				var err error
				if proxy {
					err = req.WriteProxy(&out)
				} else {
					err = req.Write(&out)
				}
				if err != nil {
					t.Fatal(err)
				}
				wire := out.String()
				if !strings.Contains(wire, name+": 003\r\n") || strings.Count(strings.ToLower(wire), "content-length:") != 1 || strings.Contains(strings.ToLower(wire), "transfer-encoding:") || strings.Contains(strings.ToLower(wire), "user-agent:") || strings.Count(strings.ToLower(wire), "host:") != 1 || strings.Count(strings.ToLower(wire), "connection: close") != 1 || !strings.HasSuffix(wire, "\r\n\r\nabc") {
					t.Errorf("bad raw framing: %q", wire)
				}
				if !reflect.DeepEqual(req.Header, original) {
					t.Fatal("caller mutated")
				}
			}
		}
		for _, kind := range []string{"mismatch", "negative", "invalid", "empty", "multiple", "casing-duplicate", "transfer-coding", "short", "long"} {
			t.Run(kind+map[bool]string{false: "/origin", true: "/proxy"}[proxy], func(t *testing.T) {
				req, _ := NewRequest("POST", "http://owned.example/", strings.NewReader("abc"))
				req.Header = Header{"Content-Length": {"3"}}
				switch kind {
				case "mismatch":
					req.Header.Set("Content-Length", "4")
				case "negative":
					req.Header.Set("Content-Length", "-3")
				case "invalid":
					req.Header.Set("Content-Length", "3x")
				case "empty":
					req.Header.Set("Content-Length", "")
				case "multiple":
					req.Header["Content-Length"] = []string{"3", "3"}
				case "casing-duplicate":
					req.Header["content-length"] = []string{"3"}
				case "transfer-coding":
					req.TransferEncoding = []string{"chunked"}
				case "short":
					req.ContentLength = -1
					req.Header.Set("Content-Length", "4")
				case "long":
					req.ContentLength = -1
					req.Header.Set("Content-Length", "2")
				}
				var out bytes.Buffer
				var err error
				if proxy {
					err = req.WriteProxy(&out)
				} else {
					err = req.Write(&out)
				}
				if err == nil {
					t.Fatal("invalid framing accepted")
				}
				if kind != "short" && kind != "long" && out.Len() != 0 {
					t.Fatalf("invalid framing emitted %q", out.String())
				}
			})
		}
	}
}

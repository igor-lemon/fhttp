package http_test

import (
	"context"
	"io"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	http "github.com/igor-lemon/fhttp"
	"github.com/igor-lemon/fhttp/http2"
	"github.com/igor-lemon/fhttp/httptest"
	"github.com/igor-lemon/fhttp/httptrace"
	tls "github.com/igor-lemon/utls"
)

func TestResponseObservationTracksActualBodyOwnership(t *testing.T) {
	for _, protocol := range []string{"h1", "h2-auto", "h2-separate"} {
		for _, ending := range []string{"eof", "close", "drain", "empty"} {
			t.Run(protocol+"-"+ending, func(t *testing.T) {
				release := make(chan struct{})
				var once sync.Once
				letGo := func() { once.Do(func() { close(release) }) }
				defer letGo()
				server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if ending == "empty" {
						w.WriteHeader(204)
						return
					}
					w.Header().Set("Content-Length", "4")
					_, _ = io.WriteString(w, "ok")
					w.(http.Flusher).Flush()
					select {
					case <-release:
						_, _ = io.WriteString(w, "ay")
					case <-r.Context().Done():
					}
				}))
				server.EnableHTTP2 = protocol != "h1"
				server.StartTLS()
				defer server.Close()
				var transport http.RoundTripper
				var closeIdle func()
				if protocol == "h2-separate" {
					tr := &http2.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}
					transport = tr
					closeIdle = tr.CloseIdleConnections
				} else {
					tr := server.Client().Transport.(*http.Transport)
					transport = tr
					closeIdle = tr.CloseIdleConnections
				}
				defer closeIdle()
				var starts, headers, done, bodyBytes atomic.Int64
				trace := &httptrace.ClientTrace{WriteHeadersStart: func(m string) { starts.Add(1) }, GotResponseHeaders: func(m string, status int) { headers.Add(1) }, GotResponseBodyBytes: func(m string, n int) { bodyBytes.Add(int64(n)) }, GotResponseBodyDone: func(m string) {
					if m != "GET" {
						t.Error("wrong method", m)
					}
					done.Add(1)
				}}
				ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
				defer cancel()
				req, _ := http.NewRequestWithContext(httptrace.WithClientTrace(ctx, trace), "GET", server.URL, nil)
				res, e := transport.RoundTrip(req)
				if e != nil {
					t.Fatal(e)
				}
				if res.ProtoMajor != map[bool]int{false: 1, true: 2}[protocol != "h1"] {
					t.Fatal("wrong actual protocol", res.Proto)
				}
				if starts.Load() != 1 || headers.Load() != 1 {
					t.Fatal("missing timing hooks", starts.Load(), headers.Load())
				}
				if ending == "empty" {
					if done.Load() != 1 {
						t.Fatal("empty response not complete")
					}
					res.Body.Close()
					return
				}
				if done.Load() != 0 {
					t.Fatal("header return completed an unread response")
				}
				prefix := make([]byte, 2)
				if _, e := io.ReadFull(res.Body, prefix); e != nil || string(prefix) != "ok" {
					t.Fatal("prefix", prefix, e)
				}
				if done.Load() != 0 {
					t.Fatal("nonterminal body read ended ownership")
				}
				switch ending {
				case "eof":
					letGo()
					rest, e := io.ReadAll(res.Body)
					if e != nil || string(rest) != "ay" {
						t.Fatal("suffix", string(rest), e)
					}
					res.Body.Close()
				case "close":
					if e := res.Body.Close(); e != nil {
						t.Fatal(e)
					}
					letGo()
				case "drain":
					drained := http.DrainBody(ctx, res, 16, time.Second, 0)
					if done.Load() != 1 {
						t.Fatal("background transfer retained request ownership")
					}
					select {
					case <-drained:
						t.Fatal("held body already drained")
					default:
					}
					letGo()
					select {
					case <-drained:
					case <-ctx.Done():
						t.Fatal("drainer did not finish")
					}
				}
				if done.Load() != 1 {
					t.Fatal("completion count", done.Load())
				}
				if ending != "drain" {
					res.Body.Close()
				}
				if done.Load() != 1 {
					t.Fatal("duplicate close completion")
				}
				if ending == "eof" && bodyBytes.Load() != 4 {
					t.Fatal("encoded activity count", bodyBytes.Load())
				}
			})
		}
	}
}

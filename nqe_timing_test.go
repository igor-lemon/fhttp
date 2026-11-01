package http_test

import (
	"bufio"
	"context"
	"io"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	http "github.com/igor-lemon/fhttp"
	"github.com/igor-lemon/fhttp/httptrace"
)

func TestHeaderTimingWaitsForCompleteFinalHTTP1Headers(t *testing.T) {
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	release := make(chan struct{})
	var once sync.Once
	letGo := func() { once.Do(func() { close(release) }) }
	t.Cleanup(letGo)
	peerDone := make(chan error, 1)
	go func() {
		c, e := ln.Accept()
		if e != nil {
			peerDone <- e
			return
		}
		defer c.Close()
		r := bufio.NewReader(c)
		for {
			line, e := r.ReadString('\n')
			if e != nil {
				peerDone <- e
				return
			}
			if line == "\r\n" {
				break
			}
		}
		if _, e = io.WriteString(c, "HTTP/1.1 103 Early Hints\r\nLink: </a>\r\n\r\nHTTP/1.1 200 OK\r\nX-Held: "); e != nil {
			peerDone <- e
			return
		}
		<-release
		_, e = io.WriteString(c, "complete\r\nContent-Length: 2\r\n\r\nok")
		peerDone <- e
	}()
	var bodyBytes atomic.Int64
	began, first, headers := make(chan struct{}, 1), make(chan struct{}, 1), make(chan struct{}, 1)
	trace := &httptrace.ClientTrace{GotResponseBodyBytes: func(method string, n int) {
		if method != "GET" {
			t.Error("body method", method)
		}
		bodyBytes.Add(int64(n))
	}, WriteHeadersStart: func(method string) { began <- struct{}{} }, GotFirstResponseByte: func() { first <- struct{}{} }, GotResponseHeaders: func(method string, status int) { headers <- struct{}{} }}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(httptrace.WithClientTrace(ctx, trace), "GET", "http://"+ln.Addr().String()+"/", nil)
	if err != nil {
		t.Fatal(err)
	}
	tr := &http.Transport{}
	t.Cleanup(tr.CloseIdleConnections)
	done := make(chan error, 1)
	go func() {
		res, e := tr.RoundTrip(req)
		if e == nil {
			b, re := io.ReadAll(res.Body)
			_ = res.Body.Close()
			if re != nil {
				e = re
			} else if string(b) != "ok" {
				e = io.ErrUnexpectedEOF
			}
		}
		done <- e
	}()
	for _, ch := range []<-chan struct{}{began, first} {
		select {
		case <-ch:
		case <-ctx.Done():
			t.Fatal("missing trace edge", ctx.Err())
		}
	}
	select {
	case <-headers:
		t.Fatal("first byte/103 substituted for final complete headers")
	default:
	}
	letGo()
	select {
	case <-headers:
	case <-ctx.Done():
		t.Fatal("missing final headers edge")
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if bodyBytes.Load() != 2 {
		t.Fatal("body activity bytes", bodyBytes.Load())
	}
	if err := <-peerDone; err != nil {
		t.Fatal(err)
	}
	select {
	case <-headers:
		t.Fatal("duplicate final header event")
	default:
	}
}

func TestHeaderWriteStartFollowsHeaderPreparation(t *testing.T) {
	prepared := false
	req, _ := http.NewRequest("GET", "http://example.test/", nil)
	req.HeaderFor = func(protocol string, header http.Header) http.Header {
		prepared = true
		return http.Header{"X-Test": {"yes"}}
	}
	fired := false
	req = req.WithContext(httptrace.WithClientTrace(req.Context(), &httptrace.ClientTrace{WriteHeadersStart: func(method string) {
		if !prepared {
			t.Error("send timing includes header preparation")
		}
		fired = true
	}}))
	var wire strings.Builder
	if err := req.Write(&wire); err != nil {
		t.Fatal(err)
	}
	if !fired || !strings.Contains(wire.String(), "X-Test: yes") {
		t.Fatal("missing actual serialization edge", wire.String())
	}
}

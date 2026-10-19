// Copyright 2026 The fhttp Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package http_test

import (
	"bufio"
	"bytes"
	"context"
	"io"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	http "github.com/igor-lemon/fhttp"
)

// writeLog keeps what each write a client made on a connection wrote.
type writeLog struct {
	mu     sync.Mutex
	writes []string
}

// taken are the writes made since the last taken.
func (l *writeLog) taken() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := l.writes
	l.writes = nil
	return out
}

type loggedConn struct {
	net.Conn
	log *writeLog
}

func (c loggedConn) Write(p []byte) (int, error) {
	c.log.mu.Lock()
	c.log.writes = append(c.log.writes, string(p))
	c.log.mu.Unlock()
	return c.Conn.Write(p)
}

// http1Writes is a transport whose MaxMergedHeadAndBody is merge, on
// connections to a server that answers every request with 200, after 100
// Continue when it is expected, and the writes it made.
func http1Writes(t *testing.T, merge int) (*http.Transport, *writeLog) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer func() { _ = c.Close() }()
				br := bufio.NewReader(c)
				for {
					req, err := http.ReadRequest(br)
					if err != nil {
						return
					}
					if req.Header.Get("Expect") == "100-continue" {
						_, _ = io.WriteString(c, "HTTP/1.1 100 Continue\r\n\r\n")
					}
					_, _ = io.Copy(io.Discard, req.Body)
					_, _ = io.WriteString(c, "HTTP/1.1 200 OK\r\nContent-Length: 0\r\n\r\n")
				}
			}()
		}
	}()
	log := &writeLog{}
	tr := &http.Transport{
		MaxMergedHeadAndBody: merge,
		DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			c, err := (&net.Dialer{}).DialContext(ctx, network, ln.Addr().String())
			if err != nil {
				return nil, err
			}
			return loggedConn{Conn: c, log: log}, nil
		},
		ExpectContinueTimeout: 10 * time.Second,
	}
	t.Cleanup(tr.CloseIdleConnections)
	return tr, log
}

// sent sends req and is the writes it took, and the length of its head.
func sent(t *testing.T, tr *http.Transport, log *writeLog, req *http.Request) ([]string, int) {
	t.Helper()
	res, err := tr.RoundTrip(req)
	if err != nil {
		t.Fatalf("RoundTrip: %v", err)
	}
	_ = res.Body.Close()
	writes := log.taken()
	return writes, strings.Index(strings.Join(writes, ""), "\r\n\r\n") + 4
}

// posting is a POST to the server of body, held in memory.
func posting(t *testing.T, body string) *http.Request {
	t.Helper()
	req, err := http.NewRequest("POST", "http://example.test/p", strings.NewReader(body))
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	return req
}

// lengths are the lengths of writes.
func lengths(writes []string) []int {
	var out []int
	for _, w := range writes {
		out = append(out, len(w))
	}
	return out
}

func TestAHeadGoesInOneWriteWhateverItsSize(t *testing.T) {
	tr, log := http1Writes(t, 1400)
	for _, n := range []int{0, 5000, 30000, 70000} {
		req, _ := http.NewRequest("GET", "http://example.test/g", nil)
		if n > 0 {
			req.Header.Set("X-Big", strings.Repeat("a", n))
		}
		if writes, head := sent(t, tr, log, req); len(writes) != 1 || len(writes[0]) != head {
			t.Errorf("an X-Big of %d: writes of %v, a head of %d", n, lengths(writes), head)
		}
	}
}

func TestABodyHeldInMemoryGoesWithItsHeadUpToTheLimit(t *testing.T) {
	tr, log := http1Writes(t, 1400)
	// The head of a body of 1,000 bytes: the bodies around the limit have
	// lengths of as many digits.
	_, head := sent(t, tr, log, posting(t, strings.Repeat("a", 1000)))
	for _, n := range []int{1400 - head - 1, 1400 - head, 1400 - head + 1, 1400 - head + 2} {
		body := strings.Repeat("b", n)
		writes, head := sent(t, tr, log, posting(t, body))
		if head+n <= 1400 {
			if len(writes) != 1 || !strings.HasSuffix(writes[0], "\r\n\r\n"+body) {
				t.Errorf("a head of %d and a body of %d: writes of %v, want one", head, n, lengths(writes))
			}
			continue
		}
		if len(writes) != 2 || len(writes[0]) != head || writes[1] != body {
			t.Errorf("a head of %d and a body of %d: writes of %v, want the head's and the body's", head, n, lengths(writes))
		}
	}
}

func TestABodyAfterItsHeadGoesInWritesOfItsOwn(t *testing.T) {
	tr, log := http1Writes(t, 1400)
	body := strings.Repeat("c", 65536)
	writes, head := sent(t, tr, log, posting(t, body))
	if len(writes) < 2 || len(writes[0]) != head || strings.Join(writes[1:], "") != body {
		t.Errorf("writes of %v, a head of %d", lengths(writes), head)
	}
	// A body not held in memory goes after its head, however small; and so
	// does one the server is to ask for.
	req, _ := http.NewRequest("POST", "http://example.test/p", struct{ io.Reader }{strings.NewReader("d")})
	req.ContentLength = 1
	if writes, head := sent(t, tr, log, req); len(writes) != 2 || len(writes[0]) != head || writes[1] != "d" {
		t.Errorf("a body not in memory: writes of %v, a head of %d", lengths(writes), head)
	}
	req = posting(t, "e")
	req.Header.Set("Expect", "100-continue")
	if writes, head := sent(t, tr, log, req); len(writes) != 2 || len(writes[0]) != head || writes[1] != "e" {
		t.Errorf("a body after 100 Continue: writes of %v, a head of %d", lengths(writes), head)
	}
}

func TestABodyShortOfItsLengthIsNotSent(t *testing.T) {
	tr, log := http1Writes(t, 1400)
	req, _ := http.NewRequest("POST", "http://example.test/p", bytes.NewReader([]byte("abcde")))
	req.ContentLength = 10
	if _, err := tr.RoundTrip(req); err == nil || !strings.Contains(err.Error(), "ContentLength=10 with Body length 5") {
		t.Errorf("RoundTrip: %v", err)
	}
	if writes := log.taken(); len(writes) != 0 {
		t.Errorf("writes of %v", lengths(writes))
	}
}

func TestUnsetARequestGoesThroughTheConnectionsBuffer(t *testing.T) {
	tr, log := http1Writes(t, 0)
	req, _ := http.NewRequest("GET", "http://example.test/g", nil)
	req.Header.Set("X-Big", strings.Repeat("a", 30000))
	if writes, _ := sent(t, tr, log, req); len(writes) < 2 || len(writes[0]) != 4096 {
		t.Errorf("a head of 30 KB: writes of %v", lengths(writes))
	}
	// A body in the NopCloser NewRequest puts it in is flushed apart, as
	// before: isKnownInMemoryReader does not see through it.
	if writes, head := sent(t, tr, log, posting(t, "f")); len(writes) != 2 || len(writes[0]) != head {
		t.Errorf("a small body: writes of %v, a head of %d", lengths(writes), head)
	}
}

// brokenConn writes nothing.
type brokenConn struct{ net.Conn }

func (brokenConn) Write([]byte) (int, error) { return 0, io.ErrClosedPipe }

func TestAHeadThatCannotBeWrittenFailsTheRequest(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	tr := &http.Transport{MaxMergedHeadAndBody: 1400, DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
		c, err := (&net.Dialer{}).DialContext(ctx, network, ln.Addr().String())
		return brokenConn{c}, err
	}}
	t.Cleanup(tr.CloseIdleConnections)
	// Larger than the connection's buffer, the head goes past it, and its
	// write fails at once.
	req, _ := http.NewRequest("GET", "http://example.test/g", nil)
	req.Header.Set("X-Big", strings.Repeat("a", 5000))
	if _, err := tr.RoundTrip(req); err == nil || !strings.Contains(err.Error(), io.ErrClosedPipe.Error()) {
		t.Errorf("RoundTrip: %v", err)
	}
}

func TestTheMergeLimitIsCloned(t *testing.T) {
	if got := (&http.Transport{MaxMergedHeadAndBody: 1400}).Clone().MaxMergedHeadAndBody; got != 1400 {
		t.Errorf("cloned %d", got)
	}
}

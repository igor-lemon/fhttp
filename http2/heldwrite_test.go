// Copyright 2026 The fhttp Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package http2

import (
	"bytes"
	"context"
	"fmt"
	"testing"
	"time"

	http "github.com/igor-lemon/fhttp"
	"github.com/igor-lemon/fhttp/http2/hpack"
)

// What the connection does while a write the peer does not take is held,
// under cc.wmu, is decided under cc.mu and written without it, as x/net's:
// a SETTINGS acknowledgement, a GOAWAY, the WINDOW_UPDATE of a body read, the
// RST_STREAM and WINDOW_UPDATE of a body let go of, what the reader frees at
// once of a frame. Before, each held cc.mu while it waited for the write, and
// the pool, which asks every connection under its own lock whether it takes a
// request (idleState), held up every request of the transport, to any host.

// heldWith is a connection of tr whose peer has answered a first GET whole
// (stream 1, gone), and a second with its HEADERS and a body of ten bytes, the
// stream left open, and whose writes are then held behind an upload the peer
// does not take.
func heldWith(t *testing.T, tr *Transport) (*ClientConn, *peer, *http.Response) {
	t.Helper()
	clients, servers := pairs(t, 1)
	p := newPeer(t, servers[0])
	conn := newHeldConn(t, clients[0], 32<<10)
	cc, err := tr.NewClientConn(conn)
	if err != nil {
		t.Fatal(err)
	}
	settled(t, cc, p)
	first, _ := http.NewRequest("GET", "https://dummy.tld/first", nil)
	done := make(chan error, 1)
	go func() {
		res, err := cc.RoundTrip(first)
		if err == nil {
			err = res.Body.Close()
		}
		done <- err
	}()
	s := next(t, p)
	if s.kind != FrameHeaders || s.stream != 1 {
		t.Fatalf("the peer was sent %v, want HEADERS on stream 1", s)
	}
	p.answer(s.stream)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	req, _ := http.NewRequest("GET", "https://dummy.tld/answered", nil)
	answered := make(chan *http.Response, 1)
	go func() {
		res, err := cc.RoundTrip(req)
		if err != nil {
			t.Error(err)
		}
		answered <- res
	}()
	s = next(t, p)
	if s.kind != FrameHeaders {
		t.Fatalf("the peer was sent %v, want HEADERS", s)
	}
	var block bytes.Buffer
	hpack.NewEncoder(&block).WriteField(hpack.HeaderField{Name: ":status", Value: "200"})
	p.write(func() error {
		if err := p.fr.WriteHeaders(HeadersFrameParam{StreamID: s.stream, EndHeaders: true, BlockFragment: block.Bytes()}); err != nil {
			return err
		}
		return p.fr.WriteData(s.stream, false, []byte("0123456789"))
	})
	var res *http.Response
	select {
	case res = <-answered:
	case <-time.After(5 * time.Second):
		t.Fatal("no answer")
	}
	if res == nil {
		t.FailNow()
	}
	// The body is there to read before the writes are held.
	for began := time.Now(); bufferedOf(cc, res) < 10; time.Sleep(5 * time.Millisecond) {
		if time.Since(began) > 5*time.Second {
			t.Fatal("the body never came")
		}
	}
	uploading(t, cc, conn, 1<<20)
	return cc, p, res
}

// bufferedOf is how much of res's body its stream holds unread.
func bufferedOf(cc *ClientConn, res *http.Response) int {
	cc.mu.Lock()
	defer cc.mu.Unlock()
	return res.Body.(transportResponseBody).cs.bufPipe.Len()
}

// settled answers a PING of cc's: the reader has then taken, and
// acknowledged, the SETTINGS the peer sent before it.
func settled(t *testing.T, cc *ClientConn, p *peer) {
	t.Helper()
	pinged := make(chan error, 1)
	go func() { pinged <- cc.Ping(context.Background()) }()
	s := next(t, p)
	if s.kind != FramePing {
		t.Fatalf("the peer was sent %v, want a PING", s)
	}
	p.ack(s.ping)
	if err := <-pinged; err != nil {
		t.Fatal(err)
	}
}

// next is the next frame the peer was sent.
func next(t *testing.T, p *peer) seen {
	t.Helper()
	select {
	case s := <-p.seen:
		return s
	case <-time.After(5 * time.Second):
		t.Fatal("the peer was sent nothing")
	}
	return seen{}
}

// answers fails t unless cc says within a second, while what it was told to
// do waits for the held write, that it takes a request as takes says.
func answers(t *testing.T, cc *ClientConn, what string, takes bool) {
	t.Helper()
	// What cc was told to do has then come to wait for the held write.
	time.Sleep(100 * time.Millisecond)
	within(t, time.Second, what, func() error {
		for began := time.Now(); ; time.Sleep(5 * time.Millisecond) {
			got := cc.idleState().canTakeNewRequest
			if got == takes {
				return nil
			}
			if time.Since(began) > 500*time.Millisecond {
				return fmt.Errorf("the connection takes a request: %v, want %v", got, takes)
			}
		}
	})
}

// TestWhatWaitsForAHeldWriteHoldsUpNothingElse: each thing the connection
// writes while a write the peer does not take is held waits for it alone, and
// the connection still says whether it takes a request.
func TestWhatWaitsForAHeldWriteHoldsUpNothingElse(t *testing.T) {
	// Told of at once: freed more than the delay after the last told.
	delayed := func() *Transport { return &Transport{WindowUpdateDelay: 50 * time.Millisecond} }
	for _, tc := range []struct {
		name  string
		tr    func() *Transport
		takes bool
		do    func(cc *ClientConn, p *peer, res *http.Response)
	}{
		{"the peer's settings acknowledged", func() *Transport { return &Transport{} }, true,
			func(cc *ClientConn, p *peer, res *http.Response) {
				p.write(func() error { return p.fr.WriteSettings(Setting{ID: SettingMaxConcurrentStreams, Val: 7}) })
			}},
		{"a GOAWAY of a shutdown", func() *Transport { return &Transport{} }, false,
			func(cc *ClientConn, p *peer, res *http.Response) {
				go cc.Shutdown(context.Background())
			}},
		{"a body read, told of at once", delayed, true,
			func(cc *ClientConn, p *peer, res *http.Response) {
				time.Sleep(60 * time.Millisecond)
				go res.Body.Read(make([]byte, 1))
			}},
		{"a body let go of", func() *Transport { return &Transport{} }, true,
			func(cc *ClientConn, p *peer, res *http.Response) { go res.Body.Close() }},
		{"a body let go of, told of at once", delayed, true,
			func(cc *ClientConn, p *peer, res *http.Response) { go res.Body.Close() }},
		{"a frame's padding", func() *Transport { return &Transport{} }, true,
			func(cc *ClientConn, p *peer, res *http.Response) {
				stream := res.Body.(transportResponseBody).cs.ID
				p.write(func() error { return p.fr.WriteDataPadded(stream, false, []byte("ab"), make([]byte, 10)) })
			}},
		{"a frame's padding, told of at once", delayed, true,
			func(cc *ClientConn, p *peer, res *http.Response) {
				time.Sleep(60 * time.Millisecond)
				stream := res.Body.(transportResponseBody).cs.ID
				p.write(func() error { return p.fr.WriteDataPadded(stream, false, []byte("ab"), make([]byte, 10)) })
			}},
		{"a frame of a stream no longer there, told of at once", delayed, true,
			func(cc *ClientConn, p *peer, res *http.Response) {
				// The first GET's, answered whole and gone.
				time.Sleep(60 * time.Millisecond)
				p.write(func() error { return p.fr.WriteData(1, false, []byte("ab")) })
			}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cc, p, res := heldWith(t, tc.tr())
			tc.do(cc, p, res)
			answers(t, cc, tc.name, tc.takes)
		})
	}
}

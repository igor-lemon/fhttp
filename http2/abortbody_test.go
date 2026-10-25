// Copyright 2026 The fhttp Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package http2

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	http "github.com/igor-lemon/fhttp"
	"github.com/igor-lemon/fhttp/http2/hpack"
)

// status answers stream with code and ends it.
func (p *peer) status(stream uint32, code string) {
	var block bytes.Buffer
	hpack.NewEncoder(&block).WriteField(hpack.HeaderField{Name: ":status", Value: code})
	p.write(func() error {
		return p.fr.WriteHeaders(HeadersFrameParam{StreamID: stream, EndHeaders: true, EndStream: true, BlockFragment: block.Bytes()})
	})
}

// TestARequestWhoseBodyWaitsEndsWhenTheAnswerSaysNo: a body whose Read waits
// for its writer, as a CONNECT's pipe does, is closed under that Read when the
// answer refuses the request (a status over 299), a GOAWAY takes its stream or
// the request ends, so that RoundTrip, which waits for the body's writer,
// returns at once. It waited for the writer's next byte, which a refused
// tunnel never has. A request that ended resets its stream; nothing else is
// said after a refusal.
func TestARequestWhoseBodyWaitsEndsWhenTheAnswerSaysNo(t *testing.T) {
	for _, tc := range []struct {
		name   string
		answer func(*peer, context.CancelFunc)
		status int      // 0: RoundTrip fails
		says   string   // what its error says
		then   []string // what the client says after
	}{
		{"refused", func(p *peer, _ context.CancelFunc) { p.status(1, "403") }, 403, "", nil},
		{"gone away", func(p *peer, _ context.CancelFunc) { p.goAway(0, ErrCodeNo) }, 0, "GOAWAY", nil},
		{"ended", func(_ *peer, cancel context.CancelFunc) { cancel() }, 0, "context canceled", []string{"R1/8"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clients, servers := pairs(t, 1)
			tr, _ := pingTransport(t, resendPolicy(t, chromeRetries, nil), clients...)
			p := newPeer(t, servers[0])
			body, writer := io.Pipe()
			defer writer.Close()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			req, _ := http.NewRequestWithContext(ctx, "POST", "https://dummy.tld/", body)
			req.ContentLength = -1
			type answered struct {
				res *http.Response
				err error
			}
			done := make(chan answered, 1)
			go func() {
				res, err := tr.RoundTrip(req)
				done <- answered{res, err}
			}()
			p.expect("H1")
			tc.answer(p, cancel)
			var got answered
			select {
			case got = <-done:
			case <-time.After(5 * time.Second):
				t.Fatal("RoundTrip waited for the body's writer")
			}
			switch {
			case tc.status != 0 && (got.err != nil || got.res.StatusCode != tc.status):
				t.Errorf("%+v, want %d", got, tc.status)
			case tc.status == 0 && (got.err == nil || !strings.Contains(got.err.Error(), tc.says)):
				t.Errorf("%+v, want an error that says %q", got, tc.says)
			}
			if got.res != nil {
				got.res.Body.Close()
			}
			// The body was closed under the Read that waited.
			if _, err := writer.Write([]byte("x")); !errors.Is(err, io.ErrClosedPipe) {
				t.Errorf("the body's writer wrote on: %v", err)
			}
			if tc.then != nil {
				p.expect(tc.then...)
			}
			p.quiet(200 * time.Millisecond)
		})
	}
}

// countedClose is a body that counts its closes.
type countedClose struct {
	io.Reader
	closes int
}

func (c *countedClose) Close() error {
	c.closes++
	return nil
}

// TestABodyIsClosedOnceWhoeverClosesIt: the writer done with the body and an
// abort after it both close it, and it is closed once.
func TestABodyIsClosedOnceWhoeverClosesIt(t *testing.T) {
	clients, servers := pairs(t, 1)
	tr, _ := pingTransport(t, resendPolicy(t, chromeRetries, nil), clients...)
	p := newPeer(t, servers[0])
	body := &countedClose{Reader: strings.NewReader("x")}
	req, _ := http.NewRequest("POST", "https://dummy.tld/", body)
	req.ContentLength = 1
	done := fetch(tr, req)
	p.expect("H1", "D1e:x")
	p.status(1, "500")
	if err := waitFor(t, done); err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond) // the abort's close, in its own goroutine
	if body.closes != 1 {
		t.Errorf("closed %d times", body.closes)
	}
}

// Copyright 2026 The fhttp Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package http

import (
	"bufio"
	"context"
	"io"
	"net"
	"strings"
	"testing"
	"time"
)

// TestARequestEndingOnceItsBodyWasLetGoOfEndsNothing: a request's context or
// Cancel ending once its body was detached is passed over: the connection
// stays, the body comes and is read, and the connection is used again.
func TestARequestEndingOnceItsBodyWasLetGoOfEndsNothing(t *testing.T) {
	for _, how := range []string{"context", "cancel"} {
		t.Run(how, func(t *testing.T) {
			ln := newLocalListener(t)
			defer ln.Close()
			conns := make(chan net.Conn, 2)
			go func() {
				for {
					c, err := ln.Accept()
					if err != nil {
						return
					}
					conns <- c
				}
			}()
			tr := &Transport{}
			defer tr.CloseIdleConnections()
			ctx, end := context.WithCancel(context.Background())
			defer end()
			cancelc := make(chan struct{})
			req, _ := NewRequestWithContext(ctx, "GET", "http://"+ln.Addr().String()+"/", nil)
			if how == "cancel" {
				req.Cancel = cancelc
			}
			answered := make(chan *Response, 1)
			go func() {
				res, err := tr.RoundTrip(req)
				if err != nil {
					res = nil
				}
				answered <- res
			}()
			c := <-conns
			defer c.Close()
			br := bufio.NewReader(c)
			if _, err := ReadRequest(br); err != nil {
				t.Fatal(err)
			}
			io.WriteString(c, "HTTP/1.1 200 OK\r\nContent-Length: 3\r\n\r\n")
			res := <-answered
			if res == nil {
				t.Fatal("no answer")
			}
			res.Body.(*bodyEOFSignal).Detach()
			if how == "cancel" {
				close(cancelc)
			} else {
				end()
			}
			time.Sleep(50 * time.Millisecond)
			io.WriteString(c, "abc")
			if b, err := io.ReadAll(res.Body); err != nil || string(b) != "abc" {
				t.Fatalf("%q, %v", b, err)
			}
			res.Body.Close()
			// Kept, the connection carries the next request.
			next, _ := NewRequest("GET", "http://"+ln.Addr().String()+"/", nil)
			go func() {
				if _, err := ReadRequest(br); err == nil {
					io.WriteString(c, "HTTP/1.1 200 OK\r\nContent-Length: 1\r\n\r\nx")
				}
			}()
			again, err := tr.RoundTrip(next)
			if err != nil {
				t.Fatal(err)
			}
			again.Body.Close()
			select {
			case <-conns:
				t.Fatal("the next request went on another connection")
			default:
			}
		})
	}
}

// TestABodyNotOfTheTransfersEndsInNoBuffer: a body under the signal that is
// not the transfer's own says nothing of a buffer.
func TestABodyNotOfTheTransfersEndsInNoBuffer(t *testing.T) {
	es := &bodyEOFSignal{body: io.NopCloser(nil)}
	if es.endsInBuffer(1 << 62) {
		t.Fatal("it ends in a buffer")
	}
}

// TestABodyClosedAsItsRequestEndsLeavesNoOneWaiting: Close holds the body's
// lock while it waits for the read loop to let the connection go, and the
// read loop, woken by the request's end, asks whether the body was let go
// of: that takes no lock, or the two would wait for each other for good (a
// drain ended as its request did).
func TestABodyClosedAsItsRequestEndsLeavesNoOneWaiting(t *testing.T) {
	ln := newLocalListener(t)
	defer ln.Close()
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		if _, err := ReadRequest(bufio.NewReader(c)); err == nil {
			io.WriteString(c, "HTTP/1.1 200 OK\r\nContent-Length: 3\r\n\r\n")
			io.Copy(io.Discard, c) // until the client closes it
		}
	}()
	tr := &Transport{}
	defer tr.CloseIdleConnections()
	ctx, end := context.WithCancel(context.Background())
	defer end()
	req, _ := NewRequestWithContext(ctx, "GET", "http://"+ln.Addr().String()+"/", nil)
	res, err := tr.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	es := res.Body.(*bodyEOFSignal)
	es.Detach()
	es.mu.Lock() // as Close holds it
	defer es.mu.Unlock()
	end() // the read loop wakes, and asks
	time.Sleep(50 * time.Millisecond)
	closed := make(chan struct{})
	go func() {
		es.earlyCloseFn() // as Close goes on, waiting for the read loop
		close(closed)
	}()
	select {
	case <-closed:
	case <-time.After(2 * time.Second):
		t.Fatal("Close and the read loop wait for each other")
	}
}

// TestABodyClosedBeforeItsEndEndsInNoBuffer: a transfer body closed before
// its end says no, as what is left of it will never be read; one read to its
// end says yes whatever the limit.
func TestABodyClosedBeforeItsEndEndsInNoBuffer(t *testing.T) {
	br := bufio.NewReader(strings.NewReader("abc"))
	br.Peek(3)
	closed := &bodyEOFSignal{body: &body{src: io.LimitReader(br, 3), closed: true}}
	if closed.endsInBuffer(1 << 62) {
		t.Fatal("a body closed before its end ends in a buffer")
	}
	read := &bodyEOFSignal{body: &body{src: io.LimitReader(br, 3), sawEOF: true}}
	if !read.endsInBuffer(0) {
		t.Fatal("a body read to its end does not end in a buffer")
	}
}

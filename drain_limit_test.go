// Copyright 2026 The fhttp Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package http_test

import (
	"bufio"
	"context"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	. "github.com/igor-lemon/fhttp"
)

// TestAChunkedBodyEndingAtTheLimitIsDrained: a chunked body of exactly the
// limit's bytes, its last chunk and empty trailer come with them, is read to
// its end once the limit is read, and its connection kept: the next request
// goes on it. The server writes the body in one write, after the head, small
// enough to come in one read with what ends it.
func TestAChunkedBodyEndingAtTheLimitIsDrained(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	accepted := make(chan net.Conn, 2)
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			accepted <- c
		}
	}()
	tr := &Transport{}
	defer tr.CloseIdleConnections()
	answered := make(chan *Response, 1)
	go func() {
		req, _ := NewRequest("GET", "http://"+ln.Addr().String()+"/", nil)
		res, err := tr.RoundTrip(req)
		if err != nil {
			res = nil
		}
		answered <- res
	}()
	c := <-accepted
	defer c.Close()
	br := bufio.NewReader(c)
	if _, err := ReadRequest(br); err != nil {
		t.Fatal(err)
	}
	io.WriteString(c, "HTTP/1.1 200 OK\r\nTransfer-Encoding: chunked\r\n\r\n")
	res := <-answered
	if res == nil {
		t.Fatal("no answer")
	}
	DrainBody(context.Background(), res, 1000, 2*time.Second, 0)
	time.Sleep(100 * time.Millisecond)
	io.WriteString(c, "3e8\r\n"+strings.Repeat("x", 1000)+"\r\n0\r\n\r\n")
	// Drained to its end, the connection is the next request's.
	next := make(chan *Response, 1)
	go func() {
		time.Sleep(300 * time.Millisecond)
		req, _ := NewRequest("GET", "http://"+ln.Addr().String()+"/", nil)
		res, err := tr.RoundTrip(req)
		if err != nil {
			res = nil
		}
		next <- res
	}()
	if _, err := ReadRequest(br); err != nil {
		t.Fatalf("the next request did not come on the drained connection: %v", err)
	}
	io.WriteString(c, "HTTP/1.1 200 OK\r\nContent-Length: 1\r\n\r\nx")
	if res := <-next; res == nil {
		t.Fatal("no answer to the next request")
	} else {
		res.Body.Close()
	}
	select {
	case <-accepted:
		t.Fatal("the next request opened a connection of its own")
	default:
	}
}

// Copyright 2026 The fhttp Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package http_test

import (
	"context"
	"io"
	"net"
	"sync"
	"testing"

	. "github.com/igor-lemon/fhttp"
)

func TestACloneKeepsUnusedIdle(t *testing.T) {
	asked := 0
	tr := &Transport{UnusedIdle: func(net.Conn) bool { asked++; return true }}
	if clone := tr.Clone(); clone.UnusedIdle == nil || !clone.UnusedIdle(nil) || asked != 1 {
		t.Error("the clone does not say of a connection what its transport says")
	}
}

// losesFirst is a plain HTTP server that lets go of its first connection
// under its request and answers on every other.
func losesFirst(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var served sync.WaitGroup
	t.Cleanup(func() {
		ln.Close()
		served.Wait()
	})
	served.Add(1)
	go func() {
		defer served.Done()
		for n := 0; ; n++ {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			served.Add(1)
			go func(c net.Conn, n int) {
				defer served.Done()
				defer c.Close()
				b := make([]byte, 4096)
				if _, err := c.Read(b); err != nil || n == 0 {
					return
				}
				_, _ = io.WriteString(c, "HTTP/1.1 200 OK\r\nContent-Length: 2\r\nConnection: close\r\n\r\nok")
			}(c, n)
		}
	}()
	return ln.Addr().String()
}

func TestAConnectionThatWaitedUnusedIsAReusedOne(t *testing.T) {
	for _, unused := range []bool{true, false} {
		addr := losesFirst(t)
		var first net.Conn
		var mu sync.Mutex
		tr := &Transport{DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			c, err := (&net.Dialer{}).DialContext(ctx, network, addr)
			mu.Lock()
			if first == nil {
				first = c
			}
			mu.Unlock()
			return c, err
		}}
		if unused {
			// It waited idle before it was given: lost under the request, it
			// is a reused one's, and the request goes again.
			tr.UnusedIdle = func(c net.Conn) bool {
				mu.Lock()
				defer mu.Unlock()
				return c == first
			}
		}
		req, _ := NewRequest("GET", "http://"+addr+"/", nil)
		res, err := tr.RoundTrip(req)
		if err == nil {
			_, _ = io.Copy(io.Discard, res.Body)
			res.Body.Close()
		}
		if unused != (err == nil) {
			t.Errorf("unused %v: %v", unused, err)
		}
		tr.CloseIdleConnections()
	}
}

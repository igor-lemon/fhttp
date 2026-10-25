// Copyright 2026 The fhttp Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package http_test

import (
	"io"
	"net"
	"net/url"
	"testing"
	"time"

	. "github.com/igor-lemon/fhttp"
	"github.com/igor-lemon/fhttp/httptest"
	tls "github.com/igor-lemon/utls"
)

func TestATransportSaysWhetherItHasAConnectionToAHost(t *testing.T) {
	entered := make(chan struct{}, 1)
	release := make(chan struct{})
	ts := httptest.NewServer(HandlerFunc(func(w ResponseWriter, r *Request) {
		if r.URL.Path == "/hold" {
			entered <- struct{}{}
			<-release
		}
		_, _ = io.WriteString(w, "ok")
	}))
	defer ts.Close()
	addr := ts.Listener.Addr().String()
	get := func(tr *Transport, u string) chan error {
		done := make(chan error, 1)
		go func() {
			req, _ := NewRequest("GET", u, nil)
			res, err := tr.RoundTrip(req)
			if err == nil {
				_, _ = io.Copy(io.Discard, res.Body)
				_ = res.Body.Close()
			}
			done <- err
		}()
		return done
	}
	for _, limit := range []int{6, 0} {
		tr := &Transport{MaxConnsPerHost: limit}
		if tr.HasConn("http", addr) {
			t.Errorf("limit %d: a connection before any request", limit)
		}
		done := get(tr, ts.URL+"/hold")
		<-entered
		// One in use is counted where the transport counts them.
		if got := tr.HasConn("http", addr); got != (limit > 0) {
			t.Errorf("limit %d: in use: %v", limit, got)
		}
		release <- struct{}{}
		if err := <-done; err != nil {
			t.Fatal(err)
		}
		if !waitCondition(time.Second, 5*time.Millisecond, func() bool { return tr.HasConn("http", addr) }) {
			t.Errorf("limit %d: no idle connection", limit)
		}
		if tr.HasConn("https", addr) || tr.HasConn("http", "127.0.0.1:1") {
			t.Errorf("limit %d: a connection to another scheme or host", limit)
		}
		tr.CloseIdleConnections()
		if !waitCondition(time.Second, 5*time.Millisecond, func() bool { return !tr.HasConn("http", addr) }) {
			t.Errorf("limit %d: a connection once every one is closed", limit)
		}
	}
	// Through a proxy, a connection is the proxy's: no host has one.
	proxy, _ := url.Parse(ts.URL)
	tr := &Transport{MaxConnsPerHost: 6, Proxy: ProxyURL(proxy)}
	defer tr.CloseIdleConnections()
	if err := <-get(tr, "http://www.example.com/"); err != nil {
		t.Fatal(err)
	}
	if !waitCondition(time.Second, 5*time.Millisecond, func() bool { return tr.IdleConnKeyCountForTesting() == 1 }) {
		t.Fatal("the proxy's connection is not idle")
	}
	if tr.HasConn("http", "www.example.com:80") || tr.HasConn("http", addr) {
		t.Error("a host has the proxy's connection")
	}
	// Nor a tunnel's: one to an https host through a CONNECT is the proxy's.
	site := httptest.NewTLSServer(HandlerFunc(func(w ResponseWriter, r *Request) { _, _ = io.WriteString(w, "ok") }))
	defer site.Close()
	tunnel := httptest.NewServer(HandlerFunc(func(w ResponseWriter, r *Request) {
		client, _, err := w.(Hijacker).Hijack()
		if err != nil {
			return
		}
		up, err := net.Dial("tcp", r.URL.Host)
		if err != nil {
			_ = client.Close()
			return
		}
		_, _ = io.WriteString(client, "HTTP/1.1 200 OK\r\n\r\n")
		go func() { _, _ = io.Copy(up, client) }()
		go func() { _, _ = io.Copy(client, up); _ = client.Close() }()
	}))
	defer tunnel.Close()
	through, _ := url.Parse(tunnel.URL)
	tr = &Transport{MaxConnsPerHost: 6, Proxy: ProxyURL(through), TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}
	defer tr.CloseIdleConnections()
	target := site.Listener.Addr().String()
	if err := <-get(tr, "https://"+target+"/"); err != nil {
		t.Fatal(err)
	}
	if !waitCondition(time.Second, 5*time.Millisecond, func() bool { return tr.IdleConnKeyCountForTesting() == 1 }) {
		t.Fatal("the tunnel's connection is not idle")
	}
	if tr.HasConn("https", target) {
		t.Error("a host has a tunnel's connection")
	}
}

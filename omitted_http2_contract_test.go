//go:build nethttpomithttp2

package http

import (
	"io"
	"net"
	"strings"
	"testing"
	"time"
)

// This witness runs separately after the full root suite and its cooldown.
// It checks the actual fork's tag, fresh Clone and an owned complete H1 exchange.
func TestOmittedHTTP2Contracts(t *testing.T) {
	if !omitBundledHTTP2 {
		t.Fatal("bundled HTTP2 was not omitted")
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	srv := &Server{Handler: HandlerFunc(func(w ResponseWriter, r *Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil || string(body) != "request body" || r.ProtoMajor != 1 {
			t.Errorf("request body=%q error=%v proto=%s", body, err, r.Proto)
		}
		w.Header().Set("Content-Length", "13")
		io.WriteString(w, "response body")
	})}
	done := make(chan error, 1)
	go func() { done <- srv.Serve(listener) }()
	defer func() {
		srv.Close()
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Error("server did not stop")
		}
	}()
	original := &Transport{ForceAttemptHTTP2: true, PseudoHeaderOrder: []string{":method", ":path"}}
	clone := original.Clone()
	defer original.CloseIdleConnections()
	defer clone.CloseIdleConnections()
	if clone.H2transport != nil || clone.TLSNextProto != nil {
		t.Fatal("clone introduced bundled H2")
	}
	client := &Client{Transport: clone, Timeout: 3 * time.Second}
	req, err := NewRequest("POST", "http://"+listener.Addr().String()+"/", io.NopCloser(strings.NewReader("request body")))
	if err != nil {
		t.Fatal(err)
	}
	req.ContentLength = 12
	res, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	body, err := io.ReadAll(res.Body)
	if err != nil || res.ProtoMajor != 1 || res.ContentLength != 13 || string(body) != "response body" {
		t.Fatalf("response body=%q error=%v proto=%s length=%d", body, err, res.Proto, res.ContentLength)
	}
}

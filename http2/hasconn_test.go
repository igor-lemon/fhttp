// Copyright 2026 The fhttp Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package http2

import (
	"errors"
	"net"
	"testing"
	"time"

	http "github.com/igor-lemon/fhttp"
	tls "github.com/igor-lemon/utls"
)

func TestATransportSaysWhetherItHasAConnectionForAnAddress(t *testing.T) {
	clients, servers := pairs(t, 1)
	p := newPeer(t, servers[0])
	dialing := make(chan struct{})
	let := make(chan struct{})
	tr := &Transport{TLSClientConfig: tlsConfigInsecure, DialTLS: func(string, string, *tls.Config) (net.Conn, error) {
		close(dialing)
		<-let
		return clients[0], nil
	}}
	defer tr.CloseIdleConnections()
	const addr = "dummy.tld:443"
	if tr.HasConn(addr) {
		t.Error("a connection before any request")
	}
	req, _ := http.NewRequest("GET", "https://dummy.tld/", nil)
	done := fetch(tr, req)
	<-dialing
	if !tr.HasConn(addr) {
		t.Error("none while one is dialed")
	}
	close(let)
	stream := p.expect("H1e")[0].stream
	p.headersOnly(stream)
	p.write(func() error { return p.fr.WriteData(stream, true, nil) })
	if err := waitFor(t, done); err != nil {
		t.Fatal(err)
	}
	if !tr.HasConn(addr) {
		t.Error("none once it answered")
	}
	if tr.HasConn("other.tld:443") {
		t.Error("one for another address")
	}
	tr.CloseIdleConnections()
	if !waitCondition(time.Second, 5*time.Millisecond, func() bool { return !tr.HasConn(addr) }) {
		t.Error("one once it is closed")
	}
	// A pool of the caller's says nothing.
	if (&Transport{ConnPool: refusingPool{}}).HasConn(addr) {
		t.Error("a pool of the caller's said it had one")
	}
}

type refusingPool struct{}

func (refusingPool) GetClientConn(*http.Request, string) (*ClientConn, error) {
	return nil, errors.New("no")
}
func (refusingPool) MarkDead(*ClientConn) {}

// Copyright 2026 The fhttp Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package http2

// HasConn says whether t's own pool holds a connection for addr (host:port),
// dialed for it or shared with it, that can take a request, or dials one, as
// Chromium asks whether a request's socket group has a socket (an available
// HTTP/2 session's among them) before it holds the request's TCP connection
// behind its QUIC one. A connection closed, closing or gone away is none,
// though the pool holds it until its read loop lets it go. A ConnPool of the
// caller's says nothing.
func (t *Transport) HasConn(addr string) bool {
	p, ok := t.connPool().(*clientConnPool)
	if !ok {
		return false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, cc := range p.conns[addr] {
		if cc.CanTakeNewRequest() {
			return true
		}
	}
	return p.dialing[addr] != nil
}

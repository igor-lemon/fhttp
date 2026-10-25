// Copyright 2026 The fhttp Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package http

// HasConn says whether t holds a connection to addr (host:port) for scheme
// with no proxy: idle, in use or being dialed, as Chromium asks its socket
// pool whether a request's socket group has a socket before it holds the
// request's TCP connection behind its QUIC one
// (HttpStreamFactory::Job::TargettedSocketGroupHasActiveSocket, HasGroup).
// A connection in use or being dialed is counted only where MaxConnsPerHost
// says how many there may be, as only then does t count them.
func (t *Transport) HasConn(scheme, addr string) bool {
	ours := func(k connectMethodKey) bool { return k.proxy == "" && k.scheme == scheme && k.addr == addr }
	t.connsPerHostMu.Lock()
	for k, n := range t.connsPerHost {
		if n > 0 && ours(k) {
			t.connsPerHostMu.Unlock()
			return true
		}
	}
	t.connsPerHostMu.Unlock()
	t.idleMu.Lock()
	defer t.idleMu.Unlock()
	// The transport keeps no key without an idle connection.
	for k := range t.idleConn {
		if ours(k) {
			return true
		}
	}
	return false
}

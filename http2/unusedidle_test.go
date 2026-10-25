// Copyright 2026 The fhttp Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package http2

import (
	"context"
	"net"
	"testing"
)

func TestAConnectionThatWaitedUnusedHasItsStreamsReused(t *testing.T) {
	for _, unused := range []bool{true, false} {
		clients, servers := pairs(t, 2)
		tr, logs := pingTransport(t, resendPolicy(t, chromeRetries, nil), clients...)
		if unused {
			// Its first connection waited idle before it was given: lost under
			// the request, it is a reused one's, and the request goes again.
			tr.UnusedIdle = func(c net.Conn) bool { return c == logs[0] }
		}
		a, b := newPeer(t, servers[0]), newPeer(t, servers[1])
		done := fetch(tr, request(context.Background(), false))
		a.expect("H1e")
		a.hangUp(false)
		if unused {
			b.expect("H1e")
			b.answer(1)
		}
		if err := waitFor(t, done); unused != (err == nil) {
			t.Errorf("unused %v: %v", unused, err)
		}
	}
}

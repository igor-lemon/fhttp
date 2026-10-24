// Copyright 2026 The fhttp Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package http

import (
	"context"
	"errors"
	"io"
	"net"
	"sync"
)

// Resends counts the times a request went again, as Chrome's
// HttpNetworkTransaction counts them: the times it went again after an
// HTTP/2 failure that says it was not processed (a stream refused, a GOAWAY
// above it, a PING not answered: retry_attempts_), and apart from those, the
// times it went again after a reused connection was lost under it
// (retry_attempts_on_connection_errors_). Chrome keeps both for a
// transaction: a hop of a redirect chain, with the answers to its 401 and its
// 421; a request whose context carries none counts afresh at each round trip.
// The profile says how many each may be (h2.retries,
// http.connection_error_retries).
type Resends struct {
	mu          sync.Mutex
	unprocessed int
	lost        int
}

type resendsKey struct{}

// WithResends is ctx with r, in which every round trip of a request made with
// it counts the times it went again.
func WithResends(ctx context.Context, r *Resends) context.Context {
	return context.WithValue(ctx, resendsKey{}, r)
}

// ResendsOf is the count ctx carries, or a new one of its own.
func ResendsOf(ctx context.Context) *Resends {
	if r, ok := ctx.Value(resendsKey{}).(*Resends); ok && r != nil {
		return r
	}
	return &Resends{}
}

// Reset forgets both counts, as a hop of a redirect begins with none.
func (r *Resends) Reset() {
	r.mu.Lock()
	r.unprocessed, r.lost = 0, 0
	r.mu.Unlock()
}

// Unprocessed counts a request going again after a failure that says it was
// not processed, when it went again fewer than limit times for one, and says
// whether it did.
func (r *Resends) Unprocessed(limit int) bool {
	return r.spend(&r.unprocessed, limit)
}

// Lost counts a request going again after its connection was lost, when it
// went again fewer than limit times for one, and says whether it did.
func (r *Resends) Lost(limit int) bool {
	return r.spend(&r.lost, limit)
}

func (r *Resends) spend(n *int, limit int) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if *n >= limit {
		return false
	}
	*n++
	return true
}

// ConnectionLost reports whether err says a connection was lost under a
// request: closed by the server, or reset, aborted or not connected, as
// Chrome tells ERR_CONNECTION_CLOSED, _RESET, _ABORTED and
// ERR_SOCKET_NOT_CONNECTED apart from a failure of the protocol, of TLS or
// of a deadline, and from a connection the client closed itself.
func ConnectionLost(err error) bool {
	var opErr *net.OpError
	return errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) ||
		errors.As(err, &opErr) && (opErr.Op == "read" || opErr.Op == "write") && !opErr.Timeout() && !errors.Is(err, net.ErrClosed)
}

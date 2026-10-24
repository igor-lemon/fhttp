// Copyright 2026 The fhttp Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package http2

import (
	"errors"
	"fmt"
	"slices"
	"sync/atomic"
	"time"

	http "github.com/igor-lemon/fhttp"
)

// A request sent again as Chrome's network stack sends one again
// (HttpNetworkTransaction::HandleIOError, SpdySession::OnRstStream and
// OnGoAway; measured on Chrome 155 by tls-forge), under a profile that says
// h2.retries. A request a failure says was not processed goes again
// h2.retry_delay_ms after it, when h2.retry_errors names the failure and the
// request went again fewer than h2.retries times for one: "refused-stream"
// (RST_STREAM REFUSED_STREAM: on the same connection, while it takes
// streams), "goaway-no-error" and "goaway-error" (a stream above the last a
// GOAWAY with NO_ERROR, or with another code, says the server took: on
// another connection), "ping-failed" (its connection given up for its PING:
// on another). A request whose connection was lost under it before it had an
// answer, not even a 1xx, goes again at once on another, when the connection
// was reused (it had read a frame before the request's stream opened, as
// Chrome's SpdySession::IsReused counts frames received), as long as its
// method may (http.connection_error_methods: any, or an idempotent one) and
// it went again fewer than http.connection_error_retries times for one. Both
// counts are the request's own, or those its context carries
// (http.WithResends). A request whose connection could take it no more before
// it was sent goes again at once, seven times at most. Without h2.retries,
// the transport tries as it always did: a refused stream or a stream above
// any GOAWAY again, seven times at most, a second apart and longer each time.

// goneAwayError is what a stream above the last a GOAWAY says the server took
// ends with: none of it was processed.
type goneAwayError struct{ code ErrCode }

func (e goneAwayError) Error() string {
	if e.code == ErrCodeNo {
		return errClientConnGotGoAway.Error()
	}
	return fmt.Sprintf("http2: Transport received GOAWAY from server ErrCode:%v", e.code)
}

// lostConnError is what a stream ends with when its connection was lost
// under it before its answer: what the connection said, whether it was
// reused, and whether a 1xx answer had come.
type lostConnError struct {
	err      error
	reused   bool
	informed bool
}

func (e lostConnError) Error() string { return e.err.Error() }
func (e lostConnError) Unwrap() error { return e.err }

// unlost is err as the connection said it.
func unlost(err error) error {
	if e, ok := err.(lostConnError); ok {
		return e.err
	}
	return err
}

// lost is err as the loss of the connection of cs, when it says the
// connection was lost: closed by the server or reset under the stream, as
// Chrome tells ERR_CONNECTION_CLOSED, _RESET, _ABORTED and
// ERR_SOCKET_NOT_CONNECTED apart from a protocol error; otherwise err.
func lost(cs *clientStream, err error) error {
	var goneAway GoAwayError
	if !http.ConnectionLost(err) && !errors.As(err, &goneAway) {
		return err
	}
	return lostConnError{err: err, reused: cs.reused, informed: cs.informed.Load()}
}

// failureOf names what err says of a request that had no answer, as
// h2.retry_errors names it, "lost" for a connection lost under it,
// "unusable" for a connection that could take it no more before it was
// sent, and "" for anything else.
func failureOf(err error) string {
	switch e := err.(type) {
	case StreamError:
		if e.Code == ErrCodeRefusedStream {
			return "refused-stream"
		}
	case goneAwayError:
		if e.code == ErrCodeNo {
			return "goaway-no-error"
		}
		return "goaway-error"
	case lostConnError:
		return "lost"
	}
	switch err {
	case errPrefacePingFailed:
		return "ping-failed"
	case errClientConnUnusable:
		return "unusable"
	}
	return ""
}

// idempotent says whether req may go again after its connection was lost by
// http.connection_error_methods "idempotent", as Go's net/http sends one
// again: its method is GET, HEAD, OPTIONS or TRACE, or it says it may
// (Idempotency-Key, X-Idempotency-Key).
func idempotent(req *http.Request) bool {
	switch req.Method {
	case "", "GET", "HEAD", "OPTIONS", "TRACE":
		return true
	}
	_, key := req.Header["Idempotency-Key"]
	_, xkey := req.Header["X-Idempotency-Key"]
	return key || xkey
}

// unusableRetries is how many times at most a request goes again for a
// connection that could take it no more before it was sent.
const unusableRetries = 7

// resending sends req to addr as Chrome sends a request again, as the
// profile says (h2.retries and those after it).
func (t *Transport) resending(req *http.Request, opt RoundTripOpt, addr string) (*http.Response, error) {
	p := t.policy()
	counts := http.ResendsOf(req.Context())
	retries, named := p.Int("h2.retries"), p.Strings("h2.retry_errors")
	delay := time.Duration(p.Uint("h2.retry_delay_ms")) * time.Millisecond
	first := true
	for unusable := 0; ; {
		cc, err := t.getClientConn(req, addr, opt.OnlyCachedConn && first)
		first = false
		if err != nil {
			t.vlogf("http2: Transport failed to get client conn for %s: %v", addr, err)
			return nil, err
		}
		reused := !atomic.CompareAndSwapUint32(&cc.reused, 0, 1)
		traceGotConn(req, cc, reused)
		res, afterBodyWrite, err := cc.roundTrip(req)
		if err == nil {
			return res, nil
		}
		again, pause := false, time.Duration(0)
		switch kind := failureOf(err); {
		case kind == "unusable":
			again = unusable < unusableRetries
			unusable++
		case slices.Contains(named, kind):
			again, pause = counts.Unprocessed(retries), delay
		case kind == "lost":
			e := err.(lostConnError)
			again = e.reused && !e.informed &&
				(p.Text("http.connection_error_methods") == "any" || idempotent(req)) &&
				counts.Lost(p.Int("http.connection_error_retries"))
		}
		if !again {
			t.vlogf("RoundTrip failure: %v", err)
			return nil, unlost(err)
		}
		if req, err = rewound(req, unlost(err), afterBodyWrite); err != nil {
			return nil, err
		}
		if pause > 0 {
			timer := time.NewTimer(pause)
			select {
			case <-timer.C:
			case <-req.Context().Done():
				timer.Stop()
				return nil, req.Context().Err()
			}
		}
	}
}

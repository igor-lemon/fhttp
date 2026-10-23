// Copyright 2026 The fhttp Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package http

import (
	"bufio"
	"context"
	"io"
	"math"
	"time"

	"github.com/igor-lemon/fhttp/internal"
)

// DrainBody lets go of an answer nobody will read the rest of, as Chrome's
// network stack lets go of one it moves on from (HttpResponseBodyDrainer, a
// session's, not the request's): the body is read to its end apart from the
// request it answered, whose context and Cancel no longer end it, and no more
// than limit bytes of it; at once when what is left is all in the transport's
// buffer, else in the background, within timeout and until ctx ends. Over
// HTTP/1.1 a connection whose body ended goes back to be used again, any
// other is closed; an HTTP/2 stream that did not end is reset. An answer
// whose connection is to be closed is let go of at once, and so is one whose
// rest is not in the buffer when timeout is not above zero: nothing more is
// waited for. DrainBody returns once the body is done with, or grace has
// passed: a body whose rest comes within it leaves its connection free for
// what is sent next, as one does in Chrome, which decides what to send next
// in another process, as long as that takes. The channel it returns is
// closed once the body is done with, its connection back to be used again or
// closed. r.Body is NoBody after.
func DrainBody(ctx context.Context, r *Response, limit int64, timeout, grace time.Duration) <-chan struct{} {
	body := r.Body
	r.Body = NoBody
	if r.Close {
		body.Close()
		return doneAlready
	}
	detach(body)
	if inBuffer(body, limit) {
		io.Copy(io.Discard, body)
		body.Close()
		return doneAlready
	}
	if timeout <= 0 {
		body.Close()
		return doneAlready
	}
	stop := closeOn(ctx, body, timeout)
	drained := make(chan struct{})
	go func() {
		defer close(drained)
		defer stop()
		// A body that ends within limit says so with its last bytes: the
		// transport reads what ends it (a length run out, the last chunk and
		// the trailer) with them when it is there, as Chrome's drainer finds
		// the body complete. One that does not is let go of.
		io.CopyN(io.Discard, body, limit)
		body.Close()
	}()
	if grace > 0 {
		t := time.NewTimer(grace)
		defer t.Stop()
		select {
		case <-drained:
		case <-t.C:
		}
	}
	return drained
}

// doneAlready is the channel of a body done with before its function returns.
var doneAlready = func() <-chan struct{} {
	c := make(chan struct{})
	close(c)
	return c
}()

// AwaitBody lets go of an answer once the first of its body's bytes come, as
// Chrome lets go of a CORS preflight's (its SimpleURLLoader takes none of a
// body and stops at the first byte), apart from the request it answered:
// within timeout, or as long as the connection lasts when it is not above
// zero, and until ctx ends. When the rest comes with them, or within grace
// after, it is read, and an HTTP/1.1 connection goes back to be used again;
// otherwise, and when the time is up, the connection is closed, and an HTTP/2
// stream the server has not ended is reset. A body all in the transport's
// buffer already is read at once. AwaitBody returns at once, with a channel
// closed once the body is done with; r.Body is NoBody after.
func AwaitBody(ctx context.Context, r *Response, timeout, grace time.Duration) <-chan struct{} {
	body := r.Body
	r.Body = NoBody
	detach(body)
	if inBuffer(body, math.MaxInt64) {
		io.Copy(io.Discard, body)
		body.Close()
		return doneAlready
	}
	stop := closeOn(ctx, body, timeout)
	awaited := make(chan struct{})
	go func() {
		defer close(awaited)
		defer stop()
		if _, err := body.Read(make([]byte, 1)); err == nil {
			rest(body, grace)
		}
		body.Close()
	}()
	return awaited
}

// rest reads what is left of body when it is all in the buffer, or comes
// within grace; else it closes it.
func rest(body io.ReadCloser, grace time.Duration) {
	if !inBuffer(body, math.MaxInt64) {
		if grace <= 0 {
			return
		}
		defer time.AfterFunc(grace, func() { body.Close() }).Stop()
	}
	io.Copy(io.Discard, body)
}

// closeOn closes body once timeout passes, when it is above zero, or ctx
// ends, unless the func it returns is called first.
func closeOn(ctx context.Context, body io.Closer, timeout time.Duration) func() {
	ended := context.AfterFunc(ctx, func() { body.Close() })
	var timer *time.Timer
	if timeout > 0 {
		timer = time.AfterFunc(timeout, func() { body.Close() })
	}
	return func() {
		ended()
		if timer != nil {
			timer.Stop()
		}
	}
}

// detach makes body its request's no more, where the transport can.
func detach(body io.ReadCloser) {
	if d, ok := body.(interface{ Detach() }); ok {
		d.Detach()
	}
}

// inBuffer says whether what is left of an HTTP/1.1 body of the transport's
// is all in its buffer, with no more than limit bytes of data.
func inBuffer(body io.ReadCloser, limit int64) bool {
	es, ok := body.(*bodyEOFSignal)
	return ok && es.endsInBuffer(limit)
}

// Detach makes the body its request's no more: the request's context or
// Cancel ending no longer closes the connection under it.
func (es *bodyEOFSignal) Detach() { es.detached.Store(true) }

// isDetached says whether Detach was called.
func (es *bodyEOFSignal) isDetached() bool { return es.detached.Load() }

// endsInBuffer says whether what is left of the body is all in the
// transport's buffer, with no more than limit bytes: reading it to its end
// waits for nothing.
func (es *bodyEOFSignal) endsInBuffer(limit int64) bool {
	b, ok := es.body.(*body)
	if !ok {
		return false
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.sawEOF || b.closed {
		return b.sawEOF
	}
	if lr, ok := b.src.(*io.LimitedReader); ok {
		br, ok := lr.R.(*bufio.Reader)
		return ok && lr.N <= limit && int64(br.Buffered()) >= lr.N
	}
	return b.hdr != nil && internal.EndsInBuffer(b.src, uint64(limit))
}

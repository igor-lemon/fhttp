// Copyright 2026 The fhttp Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package http2

import (
	"github.com/igor-lemon/utls/behavior"
	"io"
	"time"
)

// timeNow is time.Now, which tests replace.
var timeNow = time.Now

// Under Transport.WindowUpdateDelay the transport tells the peer of the window
// its reader frees as Chrome's network stack does (SpdySession and
// SpdyStream::IncreaseRecvWindowSize, measured on Chrome 155 by tls-forge):
// what is freed and untold of a window is told once it is more than half of
// that window, or at the first byte freed more than WindowUpdateDelay after
// the peer was last told, or after the connection or stream began, however
// little. The connection's window is the RFC's initial 65535 and the
// ConnectionFlow the transport gave it at the start; a stream's is the one it
// advertised (SETTINGS_INITIAL_WINDOW_SIZE), whatever the peer's own is. What
// the peer sends padded, or to a stream that is gone, is freed at once; what
// a reader never reads, when it closes the body. A stream the peer ended, or
// that was reset, is no longer the connection's, and is told nothing more,
// as Chrome closes its stream on END_STREAM: a frame that ends a stream ends
// it before what it carries is read.

// freedOfConn takes n more bytes of the connection's window as freed, now,
// and is what to tell the peer of, 0 for nothing yet. Requires cc.mu.
func (cc *ClientConn) freedOfConn(n int32, now time.Time) int32 {
	cc.freed += n
	if int64(cc.freed) <= (int64(cc.connFlow)+initialWindowSize)/cc.windowDenominator() && now.Sub(cc.toldAt) <= cc.t.WindowUpdateDelay {
		return 0
	}
	add := cc.freed
	cc.freed, cc.toldAt = 0, now
	cc.inflow.add(add)
	return add
}

// freedOfStream takes n more bytes of cs's window as freed, now, and is what
// to tell the peer of, 0 for nothing yet or for a stream that is gone.
// Requires cc.mu.
func (cc *ClientConn) freedOfStream(cs *clientStream, n int32, now time.Time) int32 {
	if cc.streams[cs.ID] != cs {
		return 0
	}
	cs.freed += n
	if int64(cs.freed) <= int64(cc.streamFlow)/cc.windowDenominator() && now.Sub(cs.toldAt) <= cc.t.WindowUpdateDelay {
		return 0
	}
	add := cs.freed
	cs.freed, cs.toldAt = 0, now
	cs.inflow.add(add)
	return add
}

// closeBody lets go of cs's body under WindowUpdateDelay: what it held unread
// is freed and told of by the rule, as Chrome takes a body it lets go of as
// read, the stream is reset, the reset first, when the peer has not ended it,
// and it is the connection's no more. All under cc.mu, which processData puts
// a frame into a body under: a frame is in the body, unread, or comes for a
// stream that is gone and is freed at once.
func (cc *ClientConn) closeBody(cs *clientStream) {
	cc.mu.Lock()
	defer cc.mu.Unlock()
	if cc.streams[cs.ID] == cs && !cc.closed {
		defer cc.forgetStreamLocked(cs)
	}
	ended := cs.bufPipe.Err() == io.EOF
	cs.bufPipe.BreakWithError(errClosedResponseBody)
	var connAdd int32
	if unread := cs.bufPipe.Len(); unread > 0 {
		connAdd = cc.freedOfConn(int32(unread), timeNow())
	}
	if ended && connAdd == 0 {
		return
	}
	cc.wmu.Lock()
	defer cc.wmu.Unlock()
	if !ended && cs.resetErr == nil && !cc.closed {
		cc.fr.WriteRSTStream(cs.ID, ErrCodeCancel)
		cs.didReset = true
	}
	if connAdd != 0 {
		cc.fr.WriteWindowUpdate(0, uint32(connAdd))
	}
	cc.bw.Flush()
}

// writeWindowUpdates tells the peer of what was freed of the connection's
// window and of cs's, the connection's first, as Chrome tells them; nothing
// for a 0. Requires cc.mu.
func (cc *ClientConn) writeWindowUpdates(cs *clientStream, connAdd, streamAdd int32) {
	if connAdd == 0 && streamAdd == 0 {
		return
	}
	cc.wmu.Lock()
	defer cc.wmu.Unlock()
	if connAdd != 0 {
		cc.fr.WriteWindowUpdate(0, uint32(connAdd))
	}
	if streamAdd != 0 {
		cc.fr.WriteWindowUpdate(cs.ID, uint32(streamAdd))
	}
	cc.bw.Flush()
}

func (cc *ClientConn) windowDenominator() int64 {
	if n := cc.t.policy().Uint("h2.window_update_denominator"); n > 0 {
		return int64(n)
	}
	return int64(behavior.Legacy().Uint("h2.window_update_denominator"))
}

// refilledConnWindow is what the connection's window is refilled to when the
// transport tells the peer of what is freed without a delay: the increment
// it was given, as x/net refills to its own, but never less than RFC 9113's
// initial 65,535 bytes. An increment under half of that refilled nothing, and
// one of 0 or 1 left the connection stopped once its first 65,535 bytes came.
// Requires cc.mu.
func (cc *ClientConn) refilledConnWindow() int32 {
	return max(int32(cc.connFlow), initialWindowSize)
}

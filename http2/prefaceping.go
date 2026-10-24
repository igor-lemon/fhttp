// Copyright 2026 The fhttp Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package http2

import (
	"encoding/binary"
	"errors"
	"io"
	"sync/atomic"
	"time"
)

// A connection's own PING, as Chrome's network stack sends it
// (SpdySession::MaybeSendPrefacePing, WritePingFrame, CheckPingStatus and
// OnPing; measured on Chrome 155 by tls-forge). When a HEADERS block, or a
// DATA frame that carries any of a body, goes to a connection that has read
// nothing for longer than h2.preface_ping_idle_ms, a PING goes with it: after
// the HEADERS and before the DATA, each in a write of its own. Its eight
// bytes are a count, h2.preface_ping_first_id for the connection's first and
// one more for each after. No other goes while one is unanswered, nor while
// the check of one is pending. The check, h2.preface_ping_hung_ms after the
// PING, gives the connection up when the PING is still unanswered and nothing
// has been read for that long, or since the check before; else it looks again
// that long after the last read. Any acknowledgement answers the PING,
// whatever it says. A connection given up fails its streams, says GOAWAY
// (h2.preface_ping_failed_goaway_code and _debug) and closes; a request that
// had no answer yet goes again on another connection when h2.retry_errors
// names "ping-failed", as many times as h2.retries says for it and the other
// failures it names together (resend.go; Chrome's
// HttpNetworkTransaction::HandleIOError of ERR_HTTP2_PING_FAILED).

// errPrefacePingFailed is what the streams of a connection given up for an
// unanswered PING end with.
var errPrefacePingFailed = errors.New("http2: connection given up: its PING was not answered")

// readStamp stamps every read that brings anything, as Chrome keeps the time
// of its session's last read.
type readStamp struct {
	io.Reader
	cc *ClientConn
}

func (r readStamp) Read(p []byte) (int, error) {
	n, err := r.Reader.Read(p)
	if n > 0 {
		atomic.StoreInt64(&r.cc.lastRead, int64(timeNow().Sub(r.cc.born)))
	}
	return n, err
}

// lastReadAt is when the connection last read anything, or began.
func (cc *ClientConn) lastReadAt() time.Time {
	return cc.born.Add(time.Duration(atomic.LoadInt64(&cc.lastRead)))
}

// prefacePingLocked writes the connection's own PING when it is due. Requires
// cc.wmu.
func (cc *ClientConn) prefacePingLocked() {
	if cc.pingIdle == 0 {
		return
	}
	cc.pingMu.Lock()
	now := timeNow()
	if cc.pingInFlight || cc.pingCheckPending || now.Sub(cc.lastReadAt()) <= cc.pingIdle {
		cc.pingMu.Unlock()
		return
	}
	var data [8]byte
	binary.BigEndian.PutUint64(data[:], cc.nextPingID)
	cc.nextPingID++
	cc.pingInFlight = true
	if cc.pingHung > 0 {
		cc.pingCheckPending = true
		time.AfterFunc(cc.pingHung, func() { cc.checkPrefacePing(now) })
	}
	cc.pingMu.Unlock()
	cc.fr.WritePing(false, data)
	cc.bw.Flush()
}

// prefacePingAnswered takes an acknowledgement as the answer to the
// connection's own PING, when one is unanswered.
func (cc *ClientConn) prefacePingAnswered() bool {
	cc.pingMu.Lock()
	defer cc.pingMu.Unlock()
	answered := cc.pingInFlight
	cc.pingInFlight = false
	return answered
}

// checkPrefacePing looks at the connection's own PING; lastCheck is when it
// was sent, or when it was looked at before.
func (cc *ClientConn) checkPrefacePing(lastCheck time.Time) {
	cc.pingMu.Lock()
	if !cc.pingInFlight {
		cc.pingCheckPending = false
		cc.pingMu.Unlock()
		return
	}
	now, lastRead := timeNow(), cc.lastReadAt()
	if now.Sub(lastRead) > cc.pingHung || lastRead.Before(lastCheck) {
		cc.pingCheckPending = false
		cc.pingMu.Unlock()
		cc.closeForFailedPing()
		return
	}
	time.AfterFunc(lastRead.Add(cc.pingHung).Sub(now), func() { cc.checkPrefacePing(now) })
	cc.pingMu.Unlock()
}

// closeForFailedPing gives the connection up. Its streams fail first, so that
// their requests may go again on another connection while the GOAWAY is
// written, as Chrome's do: its session ends its streams before its write
// loop writes the GOAWAY.
func (cc *ClientConn) closeForFailedPing() {
	cc.startReadLoop()
	cc.mu.Lock()
	cc.failStreamsLocked(errPrefacePingFailed)
	cc.cond.Broadcast()
	cc.mu.Unlock()
	if p := cc.t.policy(); p.Has("h2.preface_ping_failed_goaway_code") {
		// A write the connection holds up, under cc.wmu, gives up within
		// the time the PING was given; the GOAWAY, which Chrome writes after
		// it, then goes no more than Chrome's would.
		cc.tconn.SetWriteDeadline(time.Now().Add(cc.pingHung))
		cc.wmu.Lock()
		cc.fr.WriteGoAway(0, ErrCode(p.Uint("h2.preface_ping_failed_goaway_code")), []byte(p.Text("h2.preface_ping_failed_goaway_debug")))
		cc.bw.Flush()
		cc.wmu.Unlock()
	}
	cc.tconn.Close()
}

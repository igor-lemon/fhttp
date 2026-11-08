// Copyright 2026 The fhttp Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package http2

import (
	http "github.com/igor-lemon/fhttp"
)

// A connection's header blocks are encoded and written one at a time, in
// turn: stream IDs go out in the order they are given, and a block goes out
// before the next is encoded or the peer's SETTINGS change the encoder: the
// reader takes the turn to take them and acknowledge them (processSettings).
// The turn is held while a block, or an acknowledgement, waits for the
// connection's writes, cc.mu is not: a write the peer does not take, under
// cc.wmu, then holds up neither what asks whether the connection takes a
// request (the pool, under its own lock) nor the giving up of the connection
// for an unanswered PING (closeForFailedPing), and a request waiting for the
// turn waits no longer than it is wanted. Before, a request held cc.mu from
// encoding its HEADERS to writing them, as x/net's did until it made the same
// change, and the reader held it until its acknowledgement was written.

// headerTurns is the connection's turn, made at first use: a ClientConn made
// apart from newClientConn has none yet.
func (cc *ClientConn) headerTurns() chan struct{} {
	cc.headerTurnOnce.Do(func() { cc.headerTurn = make(chan struct{}, 1) })
	return cc.headerTurn
}

// takeHeaderTurn waits for the connection's turn as long as req is wanted.
func (cc *ClientConn) takeHeaderTurn(req *http.Request) error {
	select {
	case cc.headerTurns() <- struct{}{}:
		return nil
	case <-req.Context().Done():
		return req.Context().Err()
	case <-req.Cancel:
		return errRequestCanceled
	}
}

// giveHeaderTurn gives the turn taken back.
func (cc *ClientConn) giveHeaderTurn() {
	<-cc.headerTurns()
}

// takeHeaderSlot takes the turn and a slot for req's stream, and returns
// holding both the turn and cc.mu, or neither with why there is none. It
// waits for a slot without the turn: a stream's trailers take the turn too,
// and may be what ends the stream whose slot it waits for.
func (cc *ClientConn) takeHeaderSlot(req *http.Request) error {
	for {
		if err := cc.takeHeaderTurn(req); err != nil {
			return err
		}
		cc.mu.Lock()
		if !cc.slotWaitLocked() {
			// A slot, or why there is none, at once.
			if err := cc.awaitOpenSlotForRequest(req); err != nil {
				cc.mu.Unlock()
				cc.giveHeaderTurn()
				return err
			}
			return nil
		}
		cc.giveHeaderTurn()
		err := cc.awaitOpenSlotForRequest(req)
		cc.mu.Unlock()
		if err != nil {
			return err
		}
	}
}

// slotWaitLocked says whether awaitOpenSlotForRequest would wait for a
// slot: the connection takes requests, and all of its slots are taken.
// Requires cc.mu.
func (cc *ClientConn) slotWaitLocked() bool {
	return !cc.closed && cc.canTakeNewRequestLocked() && int64(len(cc.streams))+1 > int64(cc.maxConcurrentStreams)
}

// headersWritten says the block encoded is written, whatever came of it, and
// gives the turn back.
func (cc *ClientConn) headersWritten() {
	cc.mu.Lock()
	cc.writingHeaders = false
	cc.cond.Broadcast()
	cc.mu.Unlock()
	cc.giveHeaderTurn()
}

// endedByConn is what the connection ended cs with before any answer could
// come, the connection given up or gone away, or nil.
func (cs *clientStream) endedByConn() error {
	select {
	case re := <-cs.resc:
		return re.err
	default:
		return nil
	}
}

// endedUnwritten is what ended cs, or its request, while its HEADERS waited
// for the connection's writes, or nil: they then do not go, as Chrome takes a
// frame not yet written out of its queue. Requires cc.wmu, which the HEADERS
// would be written under.
func (cs *clientStream) endedUnwritten() error {
	if err := cs.endedByConn(); err != nil {
		return err
	}
	select {
	case <-cs.req.Context().Done():
		return cs.req.Context().Err()
	case <-cs.req.Cancel:
		return errRequestCanceled
	default:
		return nil
	}
}

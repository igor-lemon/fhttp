// Copyright 2026 The fhttp Authors. All rights reserved.
// Use of this source code is governed by a BSD-style license.
package http2

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"io"
	"sync/atomic"
	"testing"
	"time"
)

type pingQueueWriter struct {
	bytes.Buffer
	writes       [][]byte
	afterHeaders func()
}

func (w *pingQueueWriter) Write(b []byte) (int, error) {
	w.writes = append(w.writes, bytes.Clone(b))
	n, err := w.Buffer.Write(b)
	if len(b) >= 9 && b[3] == byte(FrameHeaders) && w.afterHeaders != nil {
		w.afterHeaders()
	}
	return n, err
}

func TestHeadersPreparePingBeforeThePeerCanAnswer(t *testing.T) {
	for _, name := range []string{"late-read", "disabled", "in-flight-ends", "pending-check-ends", "just-read"} {
		t.Run(name, func(t *testing.T) {
			wire := &pingQueueWriter{}
			cc := &ClientConn{t: &Transport{Behavior: pingPolicy(t, 100, 0, nil)}, born: timeNow().Add(-time.Second), pingIdle: 100 * time.Millisecond, nextPingID: 1}
			cc.bw = bufio.NewWriter(wire)
			cc.fr = NewFramer(cc.bw, nil)
			switch name {
			case "disabled":
				cc.pingIdle = 0
			case "in-flight-ends":
				cc.pingInFlight = true
			case "pending-check-ends":
				cc.pingCheckPending = true
			case "just-read":
				atomic.StoreInt64(&cc.lastRead, int64(timeNow().Sub(cc.born)))
			}
			wire.afterHeaders = func() {
				// Actual incoming bytes may update the read stamp while Write is in
				// progress. A pending check/ACK may also finish during that write.
				if name == "late-read" {
					atomic.StoreInt64(&cc.lastRead, int64(timeNow().Sub(cc.born)))
				}
				if name == "in-flight-ends" {
					cc.pingInFlight = false
				}
				if name == "pending-check-ends" {
					cc.pingCheckPending = false
				}
			}
			if err := cc.writeHeaders(3, true, 16384, []byte{0x82}, PriorityParam{}); err != nil {
				t.Fatal(err)
			}
			fr := NewFramer(nil, bytes.NewReader(wire.Bytes()))
			first, err := fr.ReadFrame()
			if err != nil || first.Header().Type != FrameHeaders {
				t.Fatalf("first frame %v %v", first, err)
			}
			frame, err := fr.ReadFrame()
			if name != "late-read" {
				if err != io.EOF || frame != nil || cc.nextPingID != 1 {
					t.Fatalf("unexpected queued PING: %v %v id%d", frame, err, cc.nextPingID)
				}
				return
			}
			ping, ok := frame.(*PingFrame)
			if err != nil || !ok || ping.IsAck() || binary.BigEndian.Uint64(ping.Data[:]) != 1 || len(wire.writes) != 2 || len(wire.writes[1]) != 17 || cc.nextPingID != 2 {
				t.Fatalf("missing separate prepared PING: %v %v writes%d id%d", frame, err, len(wire.writes), cc.nextPingID)
			}
		})
	}
}

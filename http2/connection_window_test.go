// Copyright 2026 The fhttp Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package http2

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"testing"
	"time"

	http "github.com/igor-lemon/fhttp"
	"github.com/igor-lemon/fhttp/http2/hpack"
)

// A body larger than the connection's window goes on being received however
// small the increment the profile gives the connection: without a delay to
// tell the server of what is freed, the client tells it once less than half
// of the window it refills is left, and that window is never below the
// 65,535 bytes of RFC 9113's initial one. An increment of 0 or 1 used to be
// the window refilled to, so the connection stopped after 65,535 bytes.
func TestASmallConnectionIncrementStillRefillsTheWindow(t *testing.T) {
	for _, flow := range []uint32{0, 1, 1000, 65535, 15663105} {
		t.Run(fmt.Sprint(flow), func(t *testing.T) {
			const total = 256 << 10
			ct := newClientTester(t)
			ct.tr.ConnectionFlow, ct.tr.ConnectionFlowSet = flow, true
			ct.tr.Settings = map[SettingID]uint32{SettingInitialWindowSize: 6291456}
			ct.tr.SettingsOrder = []SettingID{SettingInitialWindowSize}
			ct.client = func() error {
				req, _ := http.NewRequest("GET", "https://dummy.tld/", nil)
				res, err := ct.tr.RoundTrip(req)
				if err != nil {
					return err
				}
				defer res.Body.Close()
				body, err := io.ReadAll(res.Body)
				if err != nil {
					return err
				}
				if len(body) != total {
					return fmt.Errorf("read %d bytes, want %d", len(body), total)
				}
				return nil
			}
			ct.server = func() error {
				ct.greet()
				var sid uint32
				for sid == 0 {
					f, err := ct.fr.ReadFrame()
					if err != nil {
						return err
					}
					if h, ok := f.(*HeadersFrame); ok {
						sid = h.StreamID
					}
				}
				var head bytes.Buffer
				_ = hpack.NewEncoder(&head).WriteField(hpack.HeaderField{Name: ":status", Value: "200"})
				if err := ct.fr.WriteHeaders(HeadersFrameParam{StreamID: sid, EndHeaders: true, BlockFragment: head.Bytes()}); err != nil {
					return err
				}
				// What the client tells the connection's window of from here
				// on: its first WINDOW_UPDATE, the increment, came before the
				// request.
				told := make(chan uint32, 64)
				go func() {
					defer close(told)
					for {
						f, err := ct.fr.ReadFrame()
						if err != nil {
							return
						}
						if u, ok := f.(*WindowUpdateFrame); ok && u.StreamID == 0 {
							told <- u.Increment
						}
					}
				}()
				window, sent := int(initialWindowSize)+int(flow), 0
				chunk := make([]byte, 16384)
				for sent < total {
					if window == 0 {
						select {
						case increment, ok := <-told:
							if !ok {
								return errors.New("the connection ended")
							}
							window += int(increment)
						case <-time.After(5 * time.Second):
							return fmt.Errorf("the client told of no connection window after %d bytes", sent)
						}
						continue
					}
					n := min(len(chunk), total-sent, window)
					if err := ct.fr.WriteData(sid, sent+n == total, chunk[:n]); err != nil {
						return err
					}
					sent, window = sent+n, window-n
				}
				return nil
			}
			ct.run()
		})
	}
}

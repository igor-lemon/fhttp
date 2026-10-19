// Copyright 2026 The fhttp Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package http2

import (
	"encoding/binary"
	"fmt"
)

// alpsError is ALPS a server gave for h2 that does not read, by the number
// Chrome says it with in its GOAWAY (Chromium's AlpsDecoder::Error).
type alpsError int

const (
	// alpsFramingError is a frame that does not read: a SETTINGS frame on a
	// stream, or one of a length no settings make.
	alpsFramingError alpsError = 1
	// alpsForbiddenFrame is a frame of a type HTTP/2 has that ALPS may not
	// carry: any but SETTINGS.
	alpsForbiddenFrame alpsError = 2
	// alpsNotOnFrameBoundary is a frame cut short.
	alpsNotOnFrameBoundary alpsError = 3
	// alpsSettingsWithAck is a SETTINGS frame that acknowledges.
	alpsSettingsWithAck alpsError = 4
)

func (e alpsError) Error() string { return "http2: the server's ALPS does not read: " + e.debug() }

// debug is what Chrome's GOAWAY says of e.
func (e alpsError) debug() string { return fmt.Sprintf("Error parsing ALPS: %d", int(e)) }

// alpsSettings are the settings of what a server gave in ALPS for h2: HTTP/2
// frames, SETTINGS taken, a frame of a type HTTP/2 does not have (ACCEPT_CH
// among them) passed over, any other refused; or why it does not read, as
// Chrome reads it: a frame that does not read first, then one ALPS may not
// carry, then a SETTINGS acknowledgement, then a frame cut short. Chrome
// reads past a refused frame, and stops at one that does not read.
func alpsSettings(data []byte) ([]Setting, error) {
	var settings []Setting
	var forbidden, ack bool
	for len(data) >= frameHeaderLen {
		length := int(data[0])<<16 | int(data[1])<<8 | int(data[2])
		typ, flags := FrameType(data[3]), Flags(data[4])
		stream := binary.BigEndian.Uint32(data[5:frameHeaderLen]) & (1<<31 - 1)
		if len(data) < frameHeaderLen+length {
			break
		}
		payload := data[frameHeaderLen : frameHeaderLen+length]
		data = data[frameHeaderLen+length:]
		switch {
		case typ == FrameSettings && (stream != 0 || length%6 != 0 || flags.Has(FlagSettingsAck) && length != 0):
			return nil, alpsFramingError
		case typ == FrameSettings && flags.Has(FlagSettingsAck):
			ack = true
		case typ == FrameSettings:
			for i := 0; i < length; i += 6 {
				settings = append(settings, Setting{ID: SettingID(binary.BigEndian.Uint16(payload[i:])),
					Val: binary.BigEndian.Uint32(payload[i+2:])})
			}
		case typ <= FrameContinuation:
			forbidden = true
		}
	}
	switch {
	case forbidden:
		return nil, alpsForbiddenFrame
	case ack:
		return nil, alpsSettingsWithAck
	case len(data) > 0:
		return nil, alpsNotOnFrameBoundary
	}
	return settings, nil
}

// takeALPS takes what the server gave in ALPS for h2 before the connection's
// first request, as Chrome does: its SETTINGS applied as a SETTINGS frame's
// are, unacknowledged; and ALPS that does not read refused.
func (cc *ClientConn) takeALPS(data []byte) error {
	settings, err := alpsSettings(data)
	if err != nil {
		return err
	}
	cc.mu.Lock()
	defer cc.mu.Unlock()
	for _, s := range settings {
		if err := cc.applySetting(s); err != nil {
			return err
		}
	}
	return nil
}

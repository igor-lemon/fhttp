// Copyright 2026 The fhttp Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package http2

import (
	"encoding/binary"
	"fmt"
	"math"
	"slices"

	"github.com/igor-lemon/fhttp/http2/hpack"
)

// What a server gives in ALPS for h2, read as Chrome 155 was measured reading
// it (Chromium's AlpsDecoder and SpdySession::ParseAlps): HTTP/2 frames, held
// to what HTTP/2 says each type of frame may be as its decoder holds a
// connection's, SETTINGS taken as a SETTINGS frame's are, and ACCEPT_CH left
// to whoever sends requests on the connection.

// alpsError is ALPS a server gave for h2 that does not read, by the number
// Chrome says it with in its GOAWAY (Chromium's AlpsDecoder::Error).
type alpsError int

const (
	// alpsFramingError is a frame that does not read as HTTP/2 says a frame
	// of its type may (alpsFrameReads, alpsFragment), or a header block HPACK
	// does not decode.
	alpsFramingError alpsError = 1
	// alpsForbiddenFrame is a frame of a type HTTP/2 has that ALPS may not
	// carry: any but SETTINGS.
	alpsForbiddenFrame alpsError = 2
	// alpsNotOnFrameBoundary is a frame cut short.
	alpsNotOnFrameBoundary alpsError = 3
	// alpsSettingsWithAck is a SETTINGS frame that acknowledges.
	alpsSettingsWithAck alpsError = 4
	// alpsAcceptCHOnStream is an ACCEPT_CH frame on a stream, and
	// alpsAcceptCHWithFlags one with a flag.
	alpsAcceptCHOnStream  alpsError = 5
	alpsAcceptCHWithFlags alpsError = 6
)

func (e alpsError) Error() string { return "http2: the server's ALPS does not read: " + e.debug() }

// debug is what Chrome's GOAWAY says of e.
func (e alpsError) debug() string { return fmt.Sprintf("Error parsing ALPS: %d", int(e)) }

// alpsSettingError is a setting of ALPS of a value Chrome refuses, by what its
// GOAWAY says of it (SpdySession::HandleSetting).
type alpsSettingError string

const (
	alpsConnectProtocol alpsSettingError = "Invalid value for spdy::SETTINGS_ENABLE_CONNECT_PROTOCOL."
	alpsPriorities      alpsSettingError = "Invalid value for spdy::SETTINGS_DEPRECATE_HTTP2_PRIORITIES."
)

func (e alpsSettingError) Error() string {
	return "http2: the server's ALPS says a setting Chrome refuses: " + string(e)
}

// debug is what Chrome's GOAWAY says of e.
func (e alpsSettingError) debug() string { return string(e) }

// alpsRefusal is ALPS Chrome ends the connection over, with a GOAWAY that
// says why (debug).
type alpsRefusal interface {
	error
	debug() string
}

// alpsFrameAcceptCH is the type of an ACCEPT_CH frame, which carries client
// hints a server asks for in ALPS (draft-davidben-http-client-hint-reliability).
const alpsFrameAcceptCH FrameType = 0x89

// alpsMaxFrame is the largest frame payload ALPS may have: what HTTP/2 allows
// before SETTINGS_MAX_FRAME_SIZE says more, as Chrome's decoder allows it.
const alpsMaxFrame = 16384

// alpsSettings are the settings of what a server gave in ALPS for h2, or why
// it does not read (0 when it does), as Chrome reads it: frame by frame, a
// frame that does not read as HTTP/2 says one of its type may ends the reading
// at once; past the rest, of what was read, a frame ALPS may not carry is said
// first, then a SETTINGS acknowledgement, then a frame cut short, then an
// ACCEPT_CH frame on a stream or with flags, of which the first is said. A
// frame of a type HTTP/2 does not have is passed over, ACCEPT_CH's entries
// among them.
func alpsSettings(data []byte) ([]Setting, alpsError) {
	var settings []Setting
	var forbidden, ack, cut bool
	var acceptCH alpsError
	// open is the stream whose header block goes on in a CONTINUATION, none
	// when it is 0, and block what the block holds so far.
	var open uint32
	var block []byte
	headers := hpack.NewDecoder(initialHeaderTableSize, nil)
	for len(data) > 0 {
		if len(data) < frameHeaderLen {
			cut = true
			break
		}
		length := int(data[0])<<16 | int(data[1])<<8 | int(data[2])
		typ, flags := FrameType(data[3]), Flags(data[4])
		stream := binary.BigEndian.Uint32(data[5:frameHeaderLen]) & (1<<31 - 1)
		if !alpsFrameReads(typ, flags, stream, length, open) {
			return nil, alpsFramingError
		}
		if len(data) < frameHeaderLen+length {
			cut = true
			break
		}
		payload := data[frameHeaderLen : frameHeaderLen+length]
		data = data[frameHeaderLen+length:]
		switch {
		case typ == FrameSettings && flags.Has(FlagSettingsAck):
			ack = true
		case typ == FrameSettings:
			for i := 0; i < length; i += 6 {
				settings = append(settings, Setting{ID: SettingID(binary.BigEndian.Uint16(payload[i:])),
					Val: binary.BigEndian.Uint32(payload[i+2:])})
			}
		case typ == alpsFrameAcceptCH && acceptCH == 0 && stream != 0:
			acceptCH = alpsAcceptCHOnStream
		case typ == alpsFrameAcceptCH && acceptCH == 0 && flags != 0:
			acceptCH = alpsAcceptCHWithFlags
		case typ == FrameHeaders || typ == FramePushPromise || typ == FrameContinuation:
			fragment, ok := alpsFragment(typ, flags, payload)
			if !ok {
				return nil, alpsFramingError
			}
			forbidden, open, block = true, stream, append(block, fragment...)
			if flags.Has(FlagHeadersEndHeaders) {
				if _, err := headers.DecodeFull(block); err != nil {
					return nil, alpsFramingError
				}
				open, block = 0, nil
			}
		case typ == FrameData:
			if _, ok := alpsUnpadded(flags, payload); !ok {
				return nil, alpsFramingError
			}
			forbidden = true
		case typ <= FrameContinuation:
			// PRIORITY, RST_STREAM, PING, GOAWAY and WINDOW_UPDATE, which
			// read as their header does, an increment of 0 too.
			forbidden = true
		}
	}
	switch {
	case forbidden:
		return nil, alpsForbiddenFrame
	case ack:
		return nil, alpsSettingsWithAck
	case cut:
		return nil, alpsNotOnFrameBoundary
	case acceptCH != 0:
		return nil, acceptCH
	}
	return settings, 0
}

// alpsFrameReads reports whether a frame of type typ, with flags, on stream,
// of length bytes, reads where the header block of stream open goes on (none
// when it is 0), as HTTP/2 says (RFC 9113, section 6) and as Chrome's decoder
// holds a frame to it before it reads the frame: within alpsMaxFrame; a
// CONTINUATION of the open block's stream, and nothing else, while one is
// open, and none otherwise; DATA, HEADERS, PRIORITY, RST_STREAM and
// PUSH_PROMISE on a stream, SETTINGS, PING and GOAWAY on none; each of the
// length its type has, a SETTINGS acknowledgement empty.
func alpsFrameReads(typ FrameType, flags Flags, stream uint32, length int, open uint32) bool {
	if length > alpsMaxFrame {
		return false
	}
	if open != 0 || typ == FrameContinuation {
		return typ == FrameContinuation && stream == open && open != 0
	}
	switch typ {
	case FrameData, FrameHeaders, FramePushPromise:
		return stream != 0
	case FramePriority:
		return stream != 0 && length == 5
	case FrameRSTStream:
		return stream != 0 && length == 4
	case FrameSettings:
		return stream == 0 && length%6 == 0 && (length == 0 || !flags.Has(FlagSettingsAck))
	case FramePing:
		return stream == 0 && length == 8
	case FrameGoAway:
		return stream == 0 && length >= 8
	case FrameWindowUpdate:
		return length == 4
	}
	return true
}

// alpsFragment is the header block fragment of a HEADERS, PUSH_PROMISE or
// CONTINUATION frame's payload: without its padding, the priority of a
// HEADERS frame and the stream a PUSH_PROMISE promises; and whether the
// payload holds them.
func alpsFragment(typ FrameType, flags Flags, payload []byte) ([]byte, bool) {
	if typ == FrameContinuation {
		return payload, true
	}
	payload, ok := alpsUnpadded(flags, payload)
	skip := 0
	switch {
	case typ == FramePushPromise:
		skip = 4
	case flags.Has(FlagHeadersPriority):
		skip = 5
	}
	if !ok || len(payload) < skip {
		return nil, false
	}
	return payload[skip:], true
}

// alpsUnpadded is a DATA, HEADERS or PUSH_PROMISE frame's payload without its
// padding (the PADDED flag is the same bit for each), and whether the padding
// is within it.
func alpsUnpadded(flags Flags, payload []byte) ([]byte, bool) {
	if !flags.Has(FlagDataPadded) {
		return payload, true
	}
	if len(payload) == 0 || int(payload[0]) >= len(payload) {
		return nil, false
	}
	return payload[1 : len(payload)-int(payload[0])], true
}

// takeALPS takes what the server gave in ALPS for h2 before the connection's
// first request, as Chrome does: each setting as alpsSetting says,
// unacknowledged; or refuses ALPS that does not read, or a setting Chrome
// refuses, nil when it takes them.
func (cc *ClientConn) takeALPS(data []byte) alpsRefusal {
	settings, refused := alpsSettings(data)
	if refused != 0 {
		return refused
	}
	cc.mu.Lock()
	defer cc.mu.Unlock()
	for _, s := range settings {
		if refused := cc.alpsSetting(s); refused != "" {
			return refused
		}
	}
	return nil
}

// alpsSetting takes a setting of ALPS as Chrome's SpdySession::HandleSetting
// takes one: the header table, the streams at a time and the window as a
// SETTINGS frame's are taken, a window past 2^31-1 passed over;
// ENABLE_CONNECT_PROTOCOL and NO_RFC7540_PRIORITIES other than 0 or 1 refused,
// whatever the ones before them said; and the rest passed over, MAX_FRAME_SIZE
// and MAX_HEADER_LIST_SIZE among them, which Chrome does not heed. It says
// what Chrome would of a setting it refuses, nothing of one it takes. cc.mu
// must be held.
func (cc *ClientConn) alpsSetting(s Setting) alpsSettingError {
	if p := cc.t.policy(); p.Has("h2.alps_settings") && !slices.Contains(p.Numbers("h2.alps_settings"), uint64(s.ID)) {
		return ""
	}
	switch s.ID {
	case SettingHeaderTableSize:
		cc.setHeaderTableSize(s.Val)
	case SettingMaxConcurrentStreams:
		cc.maxConcurrentStreams = s.Val
	case SettingInitialWindowSize:
		if s.Val <= math.MaxInt32 {
			cc.setInitialWindowSize(s.Val)
		}
	case SettingEnableConnectProtocol:
		if s.Val > 1 {
			return alpsConnectProtocol
		}
	case SettingNoRFC7540Priorities:
		if s.Val > 1 {
			return alpsPriorities
		}
	}
	return ""
}

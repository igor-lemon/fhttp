// Copyright 2026 The fhttp Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package http2

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"math"
	"net"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	tls "github.com/bogdanfinn/utls"
	http "github.com/igor-lemon/fhttp"
	"github.com/igor-lemon/fhttp/http2/hpack"
)

// alpsConn is a connection whose TLS gave alps as ALPS for h2.
type alpsConn struct {
	net.Conn
	alps []byte
}

func (c alpsConn) ConnectionState() tls.ConnectionState {
	return tls.ConnectionState{NegotiatedProtocol: "h2", PeerApplicationSettings: c.alps}
}

// alpsPeer is a server of HTTP/2 that keeps, of each connection, the first
// header block it was sent and the GOAWAY, and answers every request with
// 200; and counts the connections a client left open for a second after its
// GOAWAY.
type alpsPeer struct {
	mu      sync.Mutex
	conns   int
	blocks  [][]byte
	goAways []string
	open    int
}

func (p *alpsPeer) serve(c net.Conn) {
	defer func() { _ = c.Close() }()
	if _, err := io.ReadFull(c, make([]byte, len(ClientPreface))); err != nil {
		return
	}
	fr := NewFramer(c, c)
	_ = fr.WriteSettings()
	for {
		f, err := fr.ReadFrame()
		var timeout net.Error
		if errors.As(err, &timeout) && timeout.Timeout() {
			p.mu.Lock()
			p.open++
			p.mu.Unlock()
		}
		if err != nil {
			return
		}
		switch f := f.(type) {
		case *SettingsFrame:
			if !f.IsAck() {
				_ = fr.WriteSettingsAck()
			}
		case *HeadersFrame:
			p.mu.Lock()
			p.blocks = append(p.blocks, slices.Clone(f.HeaderBlockFragment()))
			p.mu.Unlock()
			var buf bytes.Buffer
			_ = hpack.NewEncoder(&buf).WriteField(hpack.HeaderField{Name: ":status", Value: "200"})
			_ = fr.WriteHeaders(HeadersFrameParam{StreamID: f.StreamID, EndHeaders: true, EndStream: true, BlockFragment: buf.Bytes()})
		case *GoAwayFrame:
			p.mu.Lock()
			p.goAways = append(p.goAways, f.ErrCode.String()+": "+string(f.DebugData()))
			p.mu.Unlock()
			_ = c.SetReadDeadline(time.Now().Add(time.Second))
		}
	}
}

// alpsRoundTrip sends a request with header on a transport whose connections'
// TLS gave alps, to a server it dials anew each time: what the request came
// to, and what the server saw.
func alpsRoundTrip(t *testing.T, alps []byte, header ...string) (*http.Response, error, *alpsPeer) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	peer := &alpsPeer{}
	var dials int
	var served sync.WaitGroup
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			peer.mu.Lock()
			peer.conns++
			peer.mu.Unlock()
			served.Add(1)
			go func() {
				defer served.Done()
				peer.serve(c)
			}()
		}
	}()
	tr := &Transport{MaxEncoderHeaderTableSize: math.MaxUint32,
		DialTLS: func(network, _ string, _ *tls.Config) (net.Conn, error) {
			dials++
			c, err := net.Dial(network, ln.Addr().String())
			return alpsConn{Conn: c, alps: alps}, err
		}}
	t.Cleanup(tr.CloseIdleConnections)
	req, _ := http.NewRequest("GET", "https://example.test/", nil)
	for i := 0; i < len(header); i += 2 {
		req.Header.Set(header[i], header[i+1])
	}
	res, err := tr.RoundTrip(req)
	// Every connection dialed has said what it says by the time it is
	// served: a request, or a GOAWAY.
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(5 * time.Millisecond) {
		peer.mu.Lock()
		said := len(peer.blocks)+len(peer.goAways) >= dials
		peer.mu.Unlock()
		if said {
			break
		}
	}
	tr.CloseIdleConnections()
	_ = ln.Close()
	served.Wait()
	return res, err, peer
}

// h2Frame is a frame of type typ and flags on stream, carrying payload.
func h2Frame(typ FrameType, flags Flags, stream uint32, payload []byte) []byte {
	n := len(payload)
	frame := []byte{byte(n >> 16), byte(n >> 8), byte(n), byte(typ), byte(flags), 0, 0, 0, 0}
	binary.BigEndian.PutUint32(frame[5:], stream)
	return append(frame, payload...)
}

// settingsPayload is a SETTINGS frame's payload of each ID and value.
func settingsPayload(pairs ...uint32) []byte {
	var out []byte
	for i := 0; i < len(pairs); i += 2 {
		out = binary.BigEndian.AppendUint16(out, uint16(pairs[i]))
		out = binary.BigEndian.AppendUint32(out, pairs[i+1])
	}
	return out
}

// acceptCH is an ACCEPT_CH frame on stream with flags, of an entry for each
// origin and value of pairs, each with its length in two bytes.
func acceptCH(stream uint32, flags Flags, pairs ...string) []byte {
	var payload []byte
	for _, s := range pairs {
		payload = append(binary.BigEndian.AppendUint16(payload, uint16(len(s))), s...)
	}
	return h2Frame(alpsFrameAcceptCH, flags, stream, payload)
}

// frames are frames one after another.
func frames(each ...[]byte) []byte { return slices.Concat(each...) }

func TestALPSSettingsAreTakenBeforeTheFirstRequest(t *testing.T) {
	// A table of 65536: the first header block opens with a size update to
	// it, which without ALPS it does not.
	for _, tc := range []struct {
		name   string
		alps   []byte
		update bool
	}{
		{"a table of 65536", h2Frame(FrameSettings, 0, 0, settingsPayload(uint32(SettingHeaderTableSize), 65536)), true},
		{"no ALPS", nil, false},
		{"empty ALPS", []byte{}, false},
		{"frames ALPS passes over", append(h2Frame(0xfa, 0, 0, []byte("x")), acceptCH(0, 0, "https://example.test", "x")...), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res, err, peer := alpsRoundTrip(t, tc.alps)
			if err != nil || res.StatusCode != 200 {
				t.Fatalf("RoundTrip: %v, %v", err, res)
			}
			if peer.conns != 1 || len(peer.blocks) != 1 || (peer.blocks[0][0] == 0x3f) != tc.update {
				t.Errorf("%d connections, blocks %x", peer.conns, peer.blocks)
			}
		})
	}
}

func TestALPSChromeRefusesEndsTheConnectionAndTheRequest(t *testing.T) {
	// Chrome sends the preface and its SETTINGS, then a GOAWAY that says
	// why, and the request fails: it is not tried again on another
	// connection (the second connection Chrome was measured opening is its
	// error page reloading itself a second later).
	for _, tc := range []struct {
		name string
		alps []byte
		says alpsRefusal
	}{
		{"SETTINGS on a stream", h2Frame(FrameSettings, 0, 1, nil), alpsFramingError},
		{"a DATA frame", h2Frame(FrameData, 0, 1, []byte("x")), alpsForbiddenFrame},
		{"a frame cut short", h2Frame(FrameSettings, 0, 0, settingsPayload(1, 65536))[:12], alpsNotOnFrameBoundary},
		{"an acknowledgement", h2Frame(FrameSettings, FlagSettingsAck, 0, nil), alpsSettingsWithAck},
		{"ACCEPT_CH on a stream", acceptCH(1, 0, "https://example.test", "x"), alpsAcceptCHOnStream},
		{"ACCEPT_CH with a flag", acceptCH(0, 1, "https://example.test", "x"), alpsAcceptCHWithFlags},
		{"ENABLE_CONNECT_PROTOCOL of 2", h2Frame(FrameSettings, 0, 0, settingsPayload(uint32(SettingEnableConnectProtocol), 2)),
			alpsConnectProtocol},
		{"NO_RFC7540_PRIORITIES of 2, then ENABLE_CONNECT_PROTOCOL of 2", h2Frame(FrameSettings, 0, 0,
			settingsPayload(uint32(SettingNoRFC7540Priorities), 2, uint32(SettingEnableConnectProtocol), 2)), alpsPriorities},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err, peer := alpsRoundTrip(t, tc.alps)
			var got alpsRefusal
			if !errors.As(err, &got) || got != tc.says || err.Error() != tc.says.Error() {
				t.Errorf("RoundTrip: %v, want %v", err, tc.says)
			}
			want := "PROTOCOL_ERROR: " + tc.says.debug()
			if peer.conns != 1 || len(peer.blocks) != 0 || !slices.Equal(peer.goAways, []string{want}) || peer.open != 0 {
				t.Errorf("%d connections, %d requests, GOAWAYs %q, %d left open", peer.conns, len(peer.blocks), peer.goAways, peer.open)
			}
		})
	}
}

func TestARefusalSaysWhatChromeSays(t *testing.T) {
	for _, tc := range []struct {
		refusal     alpsRefusal
		said, debug string
	}{
		{alpsFramingError, "http2: the server's ALPS does not read: Error parsing ALPS: 1", "Error parsing ALPS: 1"},
		{alpsAcceptCHWithFlags, "http2: the server's ALPS does not read: Error parsing ALPS: 6", "Error parsing ALPS: 6"},
		{alpsPriorities, "http2: the server's ALPS says a setting Chrome refuses: " +
			"Invalid value for spdy::SETTINGS_DEPRECATE_HTTP2_PRIORITIES.",
			"Invalid value for spdy::SETTINGS_DEPRECATE_HTTP2_PRIORITIES."},
	} {
		if tc.refusal.Error() != tc.said || tc.refusal.debug() != tc.debug {
			t.Errorf("%q, %q", tc.refusal.Error(), tc.refusal.debug())
		}
	}
}

func TestSettingsChromeDoesNotHeedLetTheRequestGo(t *testing.T) {
	// A window past 2^31-1, ENABLE_PUSH of 2, frames of at most 100 bytes
	// and a header list of 100: Chrome goes on as if ALPS had not said them,
	// and so does the request, its header list longer than 100 bytes.
	alps := h2Frame(FrameSettings, 0, 0, settingsPayload(uint32(SettingInitialWindowSize), 1<<31,
		uint32(SettingEnablePush), 2, uint32(SettingMaxFrameSize), 100, uint32(SettingMaxHeaderListSize), 100,
		uint32(SettingEnableConnectProtocol), 1, uint32(SettingNoRFC7540Priorities), 1))
	res, err, peer := alpsRoundTrip(t, alps, "x-long", strings.Repeat("a", 200))
	if err != nil || res.StatusCode != 200 || peer.conns != 1 || len(peer.blocks) != 1 || len(peer.goAways) != 0 {
		t.Errorf("RoundTrip: %v, %v; %d connections, %d requests, GOAWAYs %q", err, res, peer.conns, len(peer.blocks),
			peer.goAways)
	}
}

func TestALPSIsReadAsChromeReadsIt(t *testing.T) {
	settings := h2Frame(FrameSettings, 0, 0, settingsPayload(1, 65536))
	cut := settings[:12]
	ack := h2Frame(FrameSettings, FlagSettingsAck, 0, nil)
	data := h2Frame(FrameData, 0, 1, []byte("x"))
	headers := h2Frame(FrameHeaders, FlagHeadersEndHeaders, 1, []byte{0x82})
	open := h2Frame(FrameHeaders, 0, 1, []byte{0x82})
	onStream := acceptCH(1, 0, "https://example.test", "x")
	// A literal field with incremental indexing of a new name, a: b, which
	// the dynamic table then holds as 62, and a block of 62 indexed.
	adds := h2Frame(FrameHeaders, FlagHeadersEndHeaders, 1, []byte{0x40, 1, 'a', 1, 'b'})
	names := h2Frame(FrameHeaders, FlagHeadersEndHeaders, 3, []byte{0xbe})
	for _, tc := range []struct {
		name     string
		alps     []byte
		settings []Setting
		refused  alpsError
	}{
		{"nothing", nil, nil, 0},
		{"no frames", []byte{}, nil, 0},
		{"SETTINGS, of two frames", frames(h2Frame(FrameSettings, 0, 0, settingsPayload(1, 65536, 3, 100)),
			h2Frame(FrameSettings, 0, 0, nil)), []Setting{{SettingHeaderTableSize, 65536}, {SettingMaxConcurrentStreams, 100}}, 0},
		{"SETTINGS with flags but ACK", h2Frame(FrameSettings, 0xfe, 0, settingsPayload(1, 65536)),
			[]Setting{{SettingHeaderTableSize, 65536}}, 0},
		{"SETTINGS with the reserved bit of its stream set", h2Frame(FrameSettings, 0, 1<<31, settingsPayload(1, 65536)),
			[]Setting{{SettingHeaderTableSize, 65536}}, 0},
		{"a frame of a type HTTP/2 does not have", h2Frame(0xfa, 0, 0, []byte("x")), nil, 0},
		{"one on a stream, every flag set", h2Frame(0xfa, 0xff, 1, []byte("x")), nil, 0},
		{"ALTSVC", h2Frame(0xa, 0, 0, append([]byte{0, 0}, `h3=":443"`...)), nil, 0},
		{"PRIORITY_UPDATE", h2Frame(0x10, 0, 0, append([]byte{0, 0, 0, 1}, "u=1"...)), nil, 0},
		{"ORIGIN", h2Frame(0xc, 0, 0, append([]byte{0, 20}, "https://example.test"...)), nil, 0},
		{"ACCEPT_CH", acceptCH(0, 0, "https://example.test", "sec-ch-ua-arch"), nil, 0},
		{"ACCEPT_CH whose entry is cut short", h2Frame(alpsFrameAcceptCH, 0, 0, []byte{0, 10, 'o'}), nil, 0},
		{"ACCEPT_CH on a stream", onStream, nil, alpsAcceptCHOnStream},
		{"ACCEPT_CH with a flag", acceptCH(0, 1, "https://example.test", "x"), nil, alpsAcceptCHWithFlags},
		{"ACCEPT_CH on a stream with a flag", acceptCH(1, 1, "https://example.test", "x"), nil, alpsAcceptCHOnStream},
		{"ACCEPT_CH on a stream, then one on none", frames(onStream, acceptCH(0, 0, "https://example.test", "x")), nil,
			alpsAcceptCHOnStream},
		{"ACCEPT_CH, then one on a stream", frames(acceptCH(0, 0, "https://example.test", "x"), onStream), nil,
			alpsAcceptCHOnStream},
		{"ACCEPT_CH with a flag, then one on a stream", frames(acceptCH(0, 1, "https://example.test", "x"), onStream), nil,
			alpsAcceptCHWithFlags},
		{"ACCEPT_CH on a stream, then one with a flag", frames(onStream, acceptCH(0, 1, "https://example.test", "x")), nil,
			alpsAcceptCHOnStream},
		{"ACCEPT_CH on a stream, then SETTINGS", frames(onStream, settings), nil, alpsAcceptCHOnStream},
		{"ACCEPT_CH on a stream, then an acknowledgement", frames(onStream, ack), nil, alpsSettingsWithAck},
		{"ACCEPT_CH on a stream, then a frame cut short", frames(onStream, cut), nil, alpsNotOnFrameBoundary},
		{"DATA, then ACCEPT_CH on a stream", frames(data, onStream), nil, alpsForbiddenFrame},
		{"SETTINGS on a stream", h2Frame(FrameSettings, 0, 1, nil), nil, alpsFramingError},
		{"SETTINGS of five bytes", h2Frame(FrameSettings, 0, 0, make([]byte, 5)), nil, alpsFramingError},
		{"an acknowledgement with a setting", h2Frame(FrameSettings, FlagSettingsAck, 0, settingsPayload(1, 1)), nil,
			alpsFramingError},
		{"an acknowledgement", ack, nil, alpsSettingsWithAck},
		{"an acknowledgement, then a frame cut short", frames(ack, cut), nil, alpsSettingsWithAck},
		{"an acknowledgement, then one that does not read", frames(ack, h2Frame(FrameSettings, 0, 1, nil)), nil,
			alpsFramingError},
		{"DATA", data, nil, alpsForbiddenFrame},
		{"DATA, then an acknowledgement", frames(data, ack), nil, alpsForbiddenFrame},
		{"DATA, then a frame cut short", frames(data, cut), nil, alpsForbiddenFrame},
		{"DATA, then one that does not read", frames(data, h2Frame(FrameSettings, 0, 1, nil)), nil, alpsFramingError},
		{"DATA on stream 0", h2Frame(FrameData, 0, 0, []byte("x")), nil, alpsFramingError},
		{"DATA padded within it", h2Frame(FrameData, FlagDataPadded, 1, []byte{1, 'x', 0}), nil, alpsForbiddenFrame},
		{"DATA padded by all but its length", h2Frame(FrameData, FlagDataPadded, 1, []byte{1, 'x'}), nil, alpsForbiddenFrame},
		{"DATA padded by all it has", h2Frame(FrameData, FlagDataPadded, 1, []byte{2, 'x'}), nil, alpsFramingError},
		{"DATA padded, of nothing", h2Frame(FrameData, FlagDataPadded, 1, nil), nil, alpsFramingError},
		{"HEADERS", headers, nil, alpsForbiddenFrame},
		{"HEADERS whose block does not decode", h2Frame(FrameHeaders, FlagHeadersEndHeaders, 1, []byte{0xff, 0xff, 0xff}),
			nil, alpsFramingError},
		{"HEADERS without END_HEADERS", open, nil, alpsForbiddenFrame},
		{"HEADERS, then its CONTINUATION", frames(open, h2Frame(FrameContinuation, FlagContinuationEndHeaders, 1, []byte{0x84})),
			nil, alpsForbiddenFrame},
		{"HEADERS, its block cut in a field by a CONTINUATION", frames(h2Frame(FrameHeaders, 0, 1, []byte{0x40, 1, 'a'}),
			h2Frame(FrameContinuation, FlagContinuationEndHeaders, 1, []byte{1, 'b'})), nil, alpsForbiddenFrame},
		{"HEADERS, then a CONTINUATION with flags it has no use for",
			frames(open, h2Frame(FrameContinuation, FlagContinuationEndHeaders|0x8|0x20, 1, []byte{0x84})), nil,
			alpsForbiddenFrame},
		{"HEADERS, then a CONTINUATION whose block does not decode",
			frames(open, h2Frame(FrameContinuation, FlagContinuationEndHeaders, 1, []byte{0xff})), nil, alpsFramingError},
		{"HEADERS without END_HEADERS, then SETTINGS", frames(open, settings), nil, alpsFramingError},
		{"HEADERS without END_HEADERS, then a CONTINUATION of another stream",
			frames(open, h2Frame(FrameContinuation, FlagContinuationEndHeaders, 3, []byte{0x84})), nil, alpsFramingError},
		{"HEADERS on stream 0", h2Frame(FrameHeaders, FlagHeadersEndHeaders, 0, []byte{0x82}), nil, alpsFramingError},
		{"HEADERS padded within it", h2Frame(FrameHeaders, FlagHeadersEndHeaders|FlagHeadersPadded, 1, []byte{1, 0x82, 0}),
			nil, alpsForbiddenFrame},
		{"HEADERS padded past it", h2Frame(FrameHeaders, FlagHeadersEndHeaders|FlagHeadersPadded, 1, []byte{5, 0x82}), nil,
			alpsFramingError},
		{"HEADERS with a priority", h2Frame(FrameHeaders, FlagHeadersEndHeaders|FlagHeadersPriority, 1,
			[]byte{0, 0, 0, 0, 15, 0x82}), nil, alpsForbiddenFrame},
		{"HEADERS with a priority cut short", h2Frame(FrameHeaders, FlagHeadersEndHeaders|FlagHeadersPriority, 1,
			[]byte{0, 0, 0, 0}), nil, alpsFramingError},
		{"two HEADERS, the second naming what the first added", frames(adds, names), nil, alpsForbiddenFrame},
		{"a HEADERS naming what was never added", names, nil, alpsFramingError},
		{"PRIORITY", h2Frame(FramePriority, 0, 1, []byte{0, 0, 0, 0, 15}), nil, alpsForbiddenFrame},
		{"PRIORITY of four bytes", h2Frame(FramePriority, 0, 1, []byte{0, 0, 0, 0}), nil, alpsFramingError},
		{"PRIORITY on stream 0", h2Frame(FramePriority, 0, 0, []byte{0, 0, 0, 0, 15}), nil, alpsFramingError},
		{"RST_STREAM", h2Frame(FrameRSTStream, 0, 1, []byte{0, 0, 0, 8}), nil, alpsForbiddenFrame},
		{"RST_STREAM of three bytes", h2Frame(FrameRSTStream, 0, 1, []byte{0, 0, 8}), nil, alpsFramingError},
		{"RST_STREAM on stream 0", h2Frame(FrameRSTStream, 0, 0, []byte{0, 0, 0, 8}), nil, alpsFramingError},
		{"PUSH_PROMISE", h2Frame(FramePushPromise, FlagPushPromiseEndHeaders, 1, []byte{0, 0, 0, 2, 0x82}), nil,
			alpsForbiddenFrame},
		{"PUSH_PROMISE of three bytes", h2Frame(FramePushPromise, FlagPushPromiseEndHeaders, 1, []byte{0, 0, 0}), nil,
			alpsFramingError},
		{"PUSH_PROMISE padded past it", h2Frame(FramePushPromise, FlagPushPromiseEndHeaders|FlagPushPromisePadded, 1,
			[]byte{9, 0, 0, 0, 2, 0x82}), nil, alpsFramingError},
		{"PUSH_PROMISE on stream 0", h2Frame(FramePushPromise, FlagPushPromiseEndHeaders, 0, []byte{0, 0, 0, 2, 0x82}),
			nil, alpsFramingError},
		{"PING", h2Frame(FramePing, 0, 0, make([]byte, 8)), nil, alpsForbiddenFrame},
		{"PING of seven bytes", h2Frame(FramePing, 0, 0, make([]byte, 7)), nil, alpsFramingError},
		{"PING on stream 1", h2Frame(FramePing, 0, 1, make([]byte, 8)), nil, alpsFramingError},
		{"PING on stream 1, cut short", h2Frame(FramePing, 0, 1, make([]byte, 8))[:12], nil, alpsFramingError},
		{"GOAWAY", h2Frame(FrameGoAway, 0, 0, make([]byte, 8)), nil, alpsForbiddenFrame},
		{"GOAWAY of seven bytes", h2Frame(FrameGoAway, 0, 0, make([]byte, 7)), nil, alpsFramingError},
		{"GOAWAY on stream 1", h2Frame(FrameGoAway, 0, 1, make([]byte, 8)), nil, alpsFramingError},
		{"WINDOW_UPDATE", h2Frame(FrameWindowUpdate, 0, 0, []byte{0, 0, 0, 1}), nil, alpsForbiddenFrame},
		{"WINDOW_UPDATE of nothing", h2Frame(FrameWindowUpdate, 0, 0, []byte{0, 0, 0, 0}), nil, alpsForbiddenFrame},
		{"WINDOW_UPDATE of three bytes", h2Frame(FrameWindowUpdate, 0, 0, []byte{0, 0, 1}), nil, alpsFramingError},
		{"CONTINUATION", h2Frame(FrameContinuation, FlagContinuationEndHeaders, 1, []byte{0x82}), nil, alpsFramingError},
		{"CONTINUATION on stream 0", h2Frame(FrameContinuation, FlagContinuationEndHeaders, 0, []byte{0x82}), nil,
			alpsFramingError},
		{"a frame of 16 KiB", h2Frame(0xfa, 0, 0, make([]byte, 16384)), nil, 0},
		{"a frame past 16 KiB", h2Frame(0xfa, 0, 0, make([]byte, 16385)), nil, alpsFramingError},
		{"a frame past 16 KiB, cut short", h2Frame(0xfa, 0, 0, make([]byte, 20000))[:20], nil, alpsFramingError},
		{"SETTINGS past 16 KiB", h2Frame(FrameSettings, 0, 0, make([]byte, 16386)), nil, alpsFramingError},
		{"a frame cut short", cut, nil, alpsNotOnFrameBoundary},
		{"a frame's header cut short", settings[:5], nil, alpsNotOnFrameBoundary},
		{"SETTINGS, then a frame cut short", frames(settings, cut), nil, alpsNotOnFrameBoundary},
	} {
		t.Run(tc.name, func(t *testing.T) {
			settings, refused := alpsSettings(tc.alps)
			if refused != tc.refused || !slices.Equal(settings, tc.settings) {
				t.Errorf("alpsSettings: %v, %v; want %v, %v", settings, refused, tc.settings, tc.refused)
			}
		})
	}
}

// alpsClientConn is a connection as newClientConn leaves one before it takes
// ALPS: a window of 65535, frames of 16 KiB, no header list limit, 100
// streams at a time, a table of 4096 that may grow as far as the server lets
// it, and one stream open.
func alpsClientConn() *ClientConn {
	cc := &ClientConn{t: &Transport{MaxEncoderHeaderTableSize: math.MaxUint32}, initialWindowSize: initialWindowSize,
		maxFrameSize: 16384, peerMaxHeaderListSize: math.MaxUint64, maxConcurrentStreams: 100,
		streams: map[uint32]*clientStream{1: {}}, hencMaxTableSize: initialHeaderTableSize}
	cc.cond = sync.NewCond(&cc.mu)
	cc.henc = hpack.NewEncoder(&cc.hbuf)
	cc.streams[1].flow.add(initialWindowSize)
	return cc
}

func TestALPSSettingsAreTakenAsChromeTakesThem(t *testing.T) {
	setting := func(pairs ...uint32) []byte { return h2Frame(FrameSettings, 0, 0, settingsPayload(pairs...)) }
	for _, tc := range []struct {
		name            string
		alps            []byte
		refused         alpsRefusal
		window, streams uint32
		table           uint32
	}{
		{"a window of 2^31-1", setting(4, math.MaxInt32), nil, math.MaxInt32, 100, 4096},
		{"a window of 2^31", setting(4, 1<<31), nil, initialWindowSize, 100, 4096},
		{"no stream at a time", setting(3, 0), nil, initialWindowSize, 0, 4096},
		{"a table of 65536", setting(1, 65536), nil, initialWindowSize, 100, 65536},
		{"frames, a header list and ENABLE_PUSH Chrome does not heed", setting(5, 100, 6, 100, 2, 2, 0x8a8a, 7), nil,
			initialWindowSize, 100, 4096},
		{"ENABLE_CONNECT_PROTOCOL of 0, then 1, then 0", setting(8, 0, 8, 1, 8, 0), nil, initialWindowSize, 100, 4096},
		{"ENABLE_CONNECT_PROTOCOL of 2", setting(8, 2), alpsConnectProtocol, initialWindowSize, 100, 4096},
		{"NO_RFC7540_PRIORITIES of 0, then 1, then 0", setting(9, 0, 9, 1, 9, 0), nil, initialWindowSize, 100, 4096},
		{"NO_RFC7540_PRIORITIES of 2", setting(9, 2), alpsPriorities, initialWindowSize, 100, 4096},
		{"a setting after a refused one", setting(8, 2, 1, 65536), alpsConnectProtocol, initialWindowSize, 100, 4096},
		{"what does not read", h2Frame(FrameSettings, FlagSettingsAck, 0, nil), alpsSettingsWithAck, initialWindowSize, 100,
			4096},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cc := alpsClientConn()
			if refused := cc.takeALPS(tc.alps); refused != tc.refused {
				t.Errorf("takeALPS: %v, want %v", refused, tc.refused)
			}
			if cc.initialWindowSize != tc.window || cc.streams[1].flow.available() != int32(tc.window) ||
				cc.maxConcurrentStreams != tc.streams || cc.hencMaxTableSize != tc.table || cc.maxFrameSize != 16384 ||
				cc.peerMaxHeaderListSize != math.MaxUint64 {
				t.Errorf("window %d (a stream's %d), %d streams, table %d, frames %d, header list %d", cc.initialWindowSize,
					cc.streams[1].flow.available(), cc.maxConcurrentStreams, cc.hencMaxTableSize, cc.maxFrameSize,
					cc.peerMaxHeaderListSize)
			}
		})
	}
}

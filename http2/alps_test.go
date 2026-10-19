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

// alpsRoundTrip sends a request on a transport whose connections' TLS gave
// alps, to a server it dials anew each time: what the request came to, and
// what the server saw.
func alpsRoundTrip(t *testing.T, alps []byte) (*http.Response, error, *alpsPeer) {
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
		{"frames ALPS passes over", append(h2Frame(0xfa, 0, 0, []byte("x")), h2Frame(0x89, 0, 0, []byte{0, 9})...), false},
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

func TestALPSThatDoesNotReadEndsTheConnectionAndIsTriedOnceMore(t *testing.T) {
	settings := h2Frame(FrameSettings, 0, 0, settingsPayload(uint32(SettingHeaderTableSize), 65536))
	for _, tc := range []struct {
		name string
		alps []byte
		says alpsError
	}{
		{"SETTINGS on a stream", h2Frame(FrameSettings, 0, 1, nil), alpsFramingError},
		{"SETTINGS of a length no settings make", h2Frame(FrameSettings, 0, 0, []byte{1, 2, 3, 4, 5}), alpsFramingError},
		{"an acknowledgement with settings", h2Frame(FrameSettings, FlagSettingsAck, 0, settingsPayload(1, 1)), alpsFramingError},
		{"a frame that does not read after a forbidden one",
			append(h2Frame(FrameData, 0, 1, []byte("x")), h2Frame(FrameSettings, 0, 1, nil)...), alpsFramingError},
		{"a DATA frame", h2Frame(FrameData, 0, 1, []byte("x")), alpsForbiddenFrame},
		{"a PING", h2Frame(FramePing, 0, 0, make([]byte, 8)), alpsForbiddenFrame},
		{"a CONTINUATION", h2Frame(FrameContinuation, 0, 1, []byte{0}), alpsForbiddenFrame},
		{"a forbidden frame and an acknowledgement", append(h2Frame(FrameData, 0, 1, []byte("x")),
			h2Frame(FrameSettings, FlagSettingsAck, 0, nil)...), alpsForbiddenFrame},
		{"a forbidden frame, then one cut short", append(h2Frame(FramePing, 0, 0, make([]byte, 8)), settings[:12]...),
			alpsForbiddenFrame},
		{"a frame cut short", settings[:12], alpsNotOnFrameBoundary},
		{"a frame's header cut short", settings[:5], alpsNotOnFrameBoundary},
		{"an acknowledgement", h2Frame(FrameSettings, FlagSettingsAck, 0, nil), alpsSettingsWithAck},
		{"an acknowledgement, then a frame cut short", append(h2Frame(FrameSettings, FlagSettingsAck, 0, nil), settings[:12]...),
			alpsSettingsWithAck},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err, peer := alpsRoundTrip(t, tc.alps)
			var got alpsError
			if !errors.As(err, &got) || got != tc.says || err.Error() != "http2: the server's ALPS does not read: "+tc.says.debug() {
				t.Errorf("RoundTrip: %v, want %v", err, tc.says)
			}
			want := "PROTOCOL_ERROR: " + tc.says.debug()
			if peer.conns != 2 || len(peer.blocks) != 0 || !slices.Equal(peer.goAways, []string{want, want}) || peer.open != 0 {
				t.Errorf("%d connections, %d requests, GOAWAYs %q, %d left open", peer.conns, len(peer.blocks), peer.goAways, peer.open)
			}
		})
	}
}

func TestASettingInALPSTheTransportRefusesEndsTheConnection(t *testing.T) {
	_, err, peer := alpsRoundTrip(t, h2Frame(FrameSettings, 0, 0, settingsPayload(uint32(SettingInitialWindowSize), 1<<31)))
	if err != ConnectionError(ErrCodeFlowControl) || peer.conns != 1 || !slices.Equal(peer.goAways, []string{"FLOW_CONTROL_ERROR: "}) {
		t.Errorf("RoundTrip: %v; %d connections, GOAWAYs %q", err, peer.conns, peer.goAways)
	}
}

func TestALPSIsReadAsChromeReadsIt(t *testing.T) {
	settings, err := alpsSettings(append(h2Frame(FrameSettings, 0, 0, settingsPayload(1, 65536, 3, 100)),
		h2Frame(FrameSettings, 0, 0, nil)...))
	if err != nil || !slices.Equal(settings, []Setting{{SettingHeaderTableSize, 65536}, {SettingMaxConcurrentStreams, 100}}) {
		t.Errorf("alpsSettings: %v, %v", settings, err)
	}
	if settings, err := alpsSettings(nil); settings != nil || err != nil {
		t.Errorf("no ALPS: %v, %v", settings, err)
	}
}

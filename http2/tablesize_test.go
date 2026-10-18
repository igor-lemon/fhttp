package http2

import (
	"bytes"
	"encoding/hex"
	"io"
	"math"
	"net"
	"strings"
	"testing"

	http "github.com/igor-lemon/fhttp"
	"github.com/igor-lemon/fhttp/http2/hpack"
)

// blocksAfterSettings sends requests one at a time on a connection of tr's to
// a peer whose SETTINGS say settings, sent once the first request's header
// block has come, as across a network, before the answer to it; and is the
// header block each request was written in.
func blocksAfterSettings(t *testing.T, tr *Transport, settings []Setting, requests ...*http.Request) [][]byte {
	t.Helper()
	// Over TCP rather than a pipe: each side writes while the other may be
	// writing too, and a pipe holds nothing.
	ln := newLocalListener(t)
	client, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	server, err := ln.Accept()
	_ = ln.Close()
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	defer server.Close()
	blocks := make(chan []byte, len(requests))
	go func() {
		if _, err := io.ReadFull(server, make([]byte, len(ClientPreface))); err != nil {
			return
		}
		fr := NewFramer(server, server)
		var answer bytes.Buffer
		enc := hpack.NewEncoder(&answer)
		_ = enc.WriteField(hpack.HeaderField{Name: ":status", Value: "200"})
		for {
			f, err := fr.ReadFrame()
			if err != nil {
				return
			}
			switch f := f.(type) {
			case *SettingsFrame:
				if !f.IsAck() {
					_ = fr.WriteSettingsAck()
				}
			case *HeadersFrame:
				blocks <- append([]byte(nil), f.HeaderBlockFragment()...)
				if settings != nil {
					_ = fr.WriteSettings(settings...)
					settings = nil
				}
				_ = fr.WriteHeaders(HeadersFrameParam{StreamID: f.StreamID, BlockFragment: answer.Bytes(),
					EndStream: true, EndHeaders: true})
			}
		}
	}()
	cc, err := tr.NewClientConn(client)
	if err != nil {
		t.Fatalf("NewClientConn: %v", err)
	}
	var out [][]byte
	for _, req := range requests {
		res, err := cc.RoundTrip(req)
		if err != nil {
			t.Fatalf("RoundTrip: %v", err)
		}
		_ = res.Body.Close()
		out = append(out, <-blocks)
	}
	return out
}

func TestTheEncodersTableFollowsThePeerUpToItsLimit(t *testing.T) {
	// The first request is answered once the peer's SETTINGS have been
	// read, so the block of the second says what they did to the table:
	// nothing, when the encoder had the size they allow, or a Dynamic Table
	// Size Update to the size it takes, capped at MaxEncoderHeaderTableSize
	// (4096 unless set).
	get := func() *http.Request {
		req, err := http.NewRequest("GET", "https://example.com/", nil)
		if err != nil {
			t.Fatal(err)
		}
		return req
	}
	// update is a Dynamic Table Size Update (RFC 7541, section 6.3).
	update := func(size uint32) string {
		b := []byte{0x20 | byte(min(size, 31))}
		if size >= 31 {
			for size -= 31; size >= 128; size >>= 7 {
				b = append(b, byte(size)|0x80)
			}
			b = append(b, byte(size))
		}
		return hex.EncodeToString(b)
	}
	for _, tc := range []struct {
		name    string
		limit   uint32
		peer    []Setting
		first   string
		updated string
	}{
		{"a bigger table, unset", 0, []Setting{{SettingHeaderTableSize, 65536}}, "", ""},
		{"a smaller table, unset", 0, []Setting{{SettingHeaderTableSize, 1024}}, "", update(1024)},
		{"no table at all, unset", 0, []Setting{{SettingHeaderTableSize, 0}}, "", update(0)},
		{"the default said, unset", 0, []Setting{{SettingHeaderTableSize, 4096}}, "", ""},
		{"a bigger table, no limit", math.MaxUint32, []Setting{{SettingHeaderTableSize, 65536}}, "", update(65536)},
		{"the largest, no limit", math.MaxUint32, []Setting{{SettingHeaderTableSize, math.MaxUint32}}, "", update(math.MaxUint32)},
		{"the default said, no limit", math.MaxUint32, []Setting{{SettingHeaderTableSize, 4096}}, "", ""},
		{"nothing said, no limit", math.MaxUint32, []Setting{}, "", ""},
		{"a bigger table, past the limit", 16384, []Setting{{SettingHeaderTableSize, 65536}}, "", update(16384)},
		{"a limit below the default", 1024, nil, update(1024), ""},
		{"a limit below the default, the peer's below it", 1024, []Setting{{SettingHeaderTableSize, 256}}, update(1024), update(256)},
		{"a limit below the default, the default said", 1024, []Setting{{SettingHeaderTableSize, 4096}}, update(1024), ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tr := &Transport{MaxEncoderHeaderTableSize: tc.limit}
			blocks := blocksAfterSettings(t, tr, tc.peer, get(), get())
			for i, want := range []string{tc.first, tc.updated} {
				got := hex.EncodeToString(blocks[i])
				// After what is wanted, a field: no other update.
				if !strings.HasPrefix(got, want) || blocks[i][len(want)/2]&0xe0 == 0x20 {
					t.Errorf("block %d is %s, want it to begin %q and a field", i+1, got, want)
				}
			}
		})
	}
}

func TestAFieldPastTheDefaultTableIsIndexedInABiggerOne(t *testing.T) {
	// A table the peer let grow takes a field larger than 4096 bytes: the
	// next request finds it whole and indexes it.
	big := strings.Repeat("v", 5000)
	request := func() *http.Request {
		req, err := http.NewRequest("GET", "https://example.com/", nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("x-big", big)
		return req
	}
	for _, tc := range []struct {
		limit   uint32
		indexed bool
	}{
		{math.MaxUint32, true},
		{0, false},
	} {
		tr := &Transport{MaxEncoderHeaderTableSize: tc.limit}
		blocks := blocksAfterSettings(t, tr, []Setting{{SettingHeaderTableSize, 65536}}, request(), request(), request())
		if indexed := len(blocks[2]) < 100; indexed != tc.indexed {
			t.Errorf("limit %d: the third block is %d bytes, indexed %v, want %v", tc.limit, len(blocks[2]), indexed, tc.indexed)
		}
	}
}

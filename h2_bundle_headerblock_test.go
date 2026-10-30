//go:build !nethttpomithttp2

package http

import (
	"bytes"
	"fmt"
	"strings"
	"testing"

	"github.com/igor-lemon/fhttp/http2/hpack"
)

// The bundled HTTP/2 serves fhttp's own Server and a Transport left to set up
// HTTP/2 itself: its framer reads header blocks as http2/frame.go does.

func bundleBlock(t *testing.T, fields ...hpack.HeaderField) []byte {
	t.Helper()
	var b bytes.Buffer
	e := hpack.NewEncoder(&b)
	for _, field := range fields {
		if err := e.WriteField(field); err != nil {
			t.Fatal(err)
		}
	}
	return b.Bytes()
}

// bundleFlood writes a header block that opens with first and goes on with n
// CONTINUATION frames carrying frag each, and one that ends it when end.
func bundleFlood(t *testing.T, first []byte, n int, frag []byte, end bool) []byte {
	t.Helper()
	var wire bytes.Buffer
	writer := http2NewFramer(&wire, nil)
	if err := writer.WriteHeaders(http2HeadersFrameParam{StreamID: 1, BlockFragment: first}); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < n; i++ {
		if err := writer.WriteContinuation(1, false, frag); err != nil {
			t.Fatal(err)
		}
	}
	if end {
		if err := writer.WriteContinuation(1, true, nil); err != nil {
			t.Fatal(err)
		}
	}
	return wire.Bytes()
}

func bundleReader(wire []byte, limit uint32) (*http2Framer, *bytes.Reader) {
	reader := bytes.NewReader(wire)
	fr := http2NewFramer(nil, reader)
	fr.MaxHeaderListSize = limit
	fr.ReadMetaHeaders = hpack.NewDecoder(4096, nil)
	return fr, reader
}

// CVE-2023-45288: past the field list, or after an invalid field, a
// CONTINUATION frame ends the connection rather than being decoded.
func TestTheBundledFramerStopsAContinuationFlood(t *testing.T) {
	for _, invalid := range []bool{false, true} {
		t.Run(fmt.Sprintf("invalid=%v", invalid), func(t *testing.T) {
			field := hpack.HeaderField{Name: "x-audit", Value: strings.Repeat("a", 40)}
			if invalid {
				field = hpack.HeaderField{Name: "Bad", Value: "a"}
			}
			block := bundleBlock(t, hpack.HeaderField{Name: ":status", Value: "200"}, field)
			fr, reader := bundleReader(bundleFlood(t, block, 3, []byte{0x88}, false), 64)
			if _, err := fr.ReadFrame(); err != http2ConnectionError(http2ErrCodeProtocol) {
				t.Fatalf("ReadFrame = %v, want connection PROTOCOL_ERROR", err)
			}
			if reader.Len() == 0 {
				t.Fatal("consumed the entire continuation flood")
			}
		})
	}
}

// Empty CONTINUATION frames and fragments that only update the table's size
// emit no field: the block is bounded on the wire too, at twice the list.
func TestTheBundledFramerBoundsABlockOnTheWire(t *testing.T) {
	for _, test := range []struct {
		name string
		frag []byte
	}{
		{"empty continuations", nil},
		{"table size updates", bytes.Repeat([]byte{0x20}, 40)},
	} {
		t.Run(test.name, func(t *testing.T) {
			wire := bundleFlood(t, []byte{0x88}, 1000, test.frag, true)
			fr, reader := bundleReader(wire, 64)
			if _, err := fr.ReadFrame(); err != http2ConnectionError(http2ErrCodeProtocol) {
				t.Fatalf("ReadFrame = %v, want connection PROTOCOL_ERROR", err)
			}
			if read := len(wire) - reader.Len(); read > 2*64+http2frameHeaderLen+len(test.frag) {
				t.Fatalf("read %d bytes of a block bounded at %d", read, 2*64)
			}
		})
	}
}

func TestTheBundledFramerReadsABlockAtItsWireBound(t *testing.T) {
	// 9+2 bytes of HEADERS, then frames of 9: 128 bytes with 12 empty ones
	// and the last, twice 64.
	for _, test := range []struct {
		empty int
		want  error
	}{{12, nil}, {13, http2ConnectionError(http2ErrCodeProtocol)}} {
		t.Run(fmt.Sprint(test.empty), func(t *testing.T) {
			fr, _ := bundleReader(bundleFlood(t, []byte{0x88, 0x20}, test.empty, nil, true), 64)
			frame, err := fr.ReadFrame()
			if err != test.want {
				t.Fatalf("ReadFrame = %v, want %v", err, test.want)
			}
			if err == nil && len(frame.(*http2MetaHeadersFrame).Fields) != 1 {
				t.Fatalf("fields %v", frame.(*http2MetaHeadersFrame).Fields)
			}
		})
	}
}

func TestTheBundledFramerReadsSmallFramesWithinItsBound(t *testing.T) {
	block := bundleBlock(t, hpack.HeaderField{Name: ":status", Value: "200"}, hpack.HeaderField{Name: "x-audit", Value: strings.Repeat("a", 40)})
	var wire bytes.Buffer
	writer := http2NewFramer(&wire, nil)
	if err := writer.WriteHeaders(http2HeadersFrameParam{StreamID: 1, BlockFragment: block[:1]}); err != nil {
		t.Fatal(err)
	}
	for i := 1; i < len(block); i++ {
		if err := writer.WriteContinuation(1, i == len(block)-1, block[i:i+1]); err != nil {
			t.Fatal(err)
		}
	}
	fr, _ := bundleReader(wire.Bytes(), 4096)
	frame, err := fr.ReadFrame()
	if err != nil {
		t.Fatal(err)
	}
	if fields := frame.(*http2MetaHeadersFrame).Fields; len(fields) != 2 || fields[1].Value != strings.Repeat("a", 40) {
		t.Fatalf("fields %v", fields)
	}
}

func TestTheBundledFramerDoesNotSayAnInvalidFieldsValue(t *testing.T) {
	var wire bytes.Buffer
	block := bundleBlock(t, hpack.HeaderField{Name: ":status", Value: "200"}, hpack.HeaderField{Name: "x-token", Value: "s3cret\x00"})
	if err := http2NewFramer(&wire, nil).WriteHeaders(http2HeadersFrameParam{StreamID: 1, EndHeaders: true, BlockFragment: block}); err != nil {
		t.Fatal(err)
	}
	fr, _ := bundleReader(wire.Bytes(), 0)
	_, err := fr.ReadFrame()
	if err == nil || strings.Contains(err.Error(), "s3cret") || !strings.Contains(err.Error(), `"x-token"`) {
		t.Fatalf("ReadFrame = %v, want the field's name and not its value", err)
	}
	if detail := fr.ErrorDetail(); detail == nil || strings.Contains(detail.Error(), "s3cret") {
		t.Fatalf("ErrorDetail = %v", detail)
	}
}

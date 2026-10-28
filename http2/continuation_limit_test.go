package http2

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	http "github.com/igor-lemon/fhttp"
	"github.com/igor-lemon/fhttp/http2/hpack"
)

func limitBlock(t *testing.T, fields ...hpack.HeaderField) []byte {
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

func TestContinuationStopsAfterHeaderLimitOrInvalidField(t *testing.T) {
	for _, push := range []bool{false, true} {
		for _, invalid := range []bool{false, true} {
			t.Run(fmt.Sprintf("push=%v/invalid=%v", push, invalid), func(t *testing.T) {
				field := hpack.HeaderField{Name: "x-audit", Value: strings.Repeat("a", 40)}
				if invalid {
					field = hpack.HeaderField{Name: "Bad", Value: "a"}
				}
				block := limitBlock(t, hpack.HeaderField{Name: ":status", Value: "200"}, field)
				var wire bytes.Buffer
				writer := NewFramer(&wire, nil)
				var err error
				if push {
					err = writer.WritePushPromise(PushPromiseParam{StreamID: 1, PromiseID: 2, BlockFragment: block})
				} else {
					err = writer.WriteHeaders(HeadersFrameParam{StreamID: 1, BlockFragment: block})
				}
				if err != nil {
					t.Fatal(err)
				}
				for i := 0; i < 3; i++ {
					if err := writer.WriteContinuation(1, false, []byte{0x88}); err != nil {
						t.Fatal(err)
					}
				}
				reader := bytes.NewReader(wire.Bytes())
				fr := NewFramer(nil, reader)
				fr.MaxHeaderListSize = 64
				fr.ReadMetaHeaders = hpack.NewDecoder(4096, nil)
				_, err = fr.ReadFrame()
				if err != ConnectionError(ErrCodeProtocol) {
					t.Fatalf("ReadFrame = %v, want connection PROTOCOL_ERROR", err)
				}
				if reader.Len() == 0 {
					t.Fatal("consumed the entire continuation flood")
				}
			})
		}
	}
}

func TestHeaderFragmentBudgetAndCompleteTruncation(t *testing.T) {
	for _, test := range []struct {
		name  string
		limit uint32
		block []byte
		want  error
	}{
		{"excess encoded bytes", 64, bytes.Repeat([]byte{0x88}, 129), ConnectionError(ErrCodeProtocol)},
		{"large limit does not overflow", 1 << 31, []byte{0x88}, nil},
		{"complete truncated block", 64, limitBlock(t, hpack.HeaderField{Name: ":status", Value: "200"}, hpack.HeaderField{Name: "x-audit", Value: strings.Repeat("a", 40)}), nil},
	} {
		t.Run(test.name, func(t *testing.T) {
			var wire bytes.Buffer
			if err := NewFramer(&wire, nil).WriteHeaders(HeadersFrameParam{StreamID: 1, EndHeaders: true, BlockFragment: test.block}); err != nil {
				t.Fatal(err)
			}
			fr := NewFramer(nil, &wire)
			fr.MaxHeaderListSize = test.limit
			fr.ReadMetaHeaders = hpack.NewDecoder(4096, nil)
			frame, err := fr.ReadFrame()
			if err != test.want {
				t.Fatalf("ReadFrame = %v, want %v", err, test.want)
			}
			if test.name == "complete truncated block" && !frame.(*MetaHeadersFrame).Truncated {
				t.Fatal("complete over-limit block was not marked truncated")
			}
		})
	}
}

func TestTransportClosesContinuationFlood(t *testing.T) {
	ct := newClientTester(t)
	ct.tr.Settings = map[SettingID]uint32{SettingMaxHeaderListSize: 64}
	ct.tr.SettingsOrder = []SettingID{SettingMaxHeaderListSize}
	_ = ct.sc.SetDeadline(time.Now().Add(2 * time.Second))
	ct.client = func() error {
		ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
		defer cancel()
		req, _ := http.NewRequestWithContext(ctx, "GET", "https://dummy.tld/", nil)
		res, err := ct.tr.RoundTrip(req)
		if res != nil {
			res.Body.Close()
		}
		if err == nil || errors.Is(err, context.DeadlineExceeded) {
			return fmt.Errorf("flood was not rejected before the request deadline: %v", err)
		}
		return nil
	}
	ct.server = func() error {
		ct.greet()
		hf, err := ct.firstHeaders()
		if err != nil {
			return err
		}
		block := limitBlock(t, hpack.HeaderField{Name: ":status", Value: "200"}, hpack.HeaderField{Name: "x-audit", Value: strings.Repeat("a", 40)})
		if err := ct.fr.WriteHeaders(HeadersFrameParam{StreamID: hf.StreamID, BlockFragment: block}); err != nil {
			return err
		}
		if err := ct.fr.WriteContinuation(hf.StreamID, false, []byte{0x88}); err != nil {
			return err
		}
		for {
			if _, err := ct.fr.ReadFrame(); err != nil {
				if errors.Is(err, io.EOF) {
					return nil
				}
				return fmt.Errorf("flood connection did not close: %w", err)
			}
		}
	}
	ct.run()
}

// emptyFlood writes a header block that opens with first and goes on with n
// CONTINUATION frames carrying frag each before the one that ends it.
func emptyFlood(t *testing.T, first []byte, n int, frag []byte) []byte {
	t.Helper()
	var wire bytes.Buffer
	writer := NewFramer(&wire, nil)
	if err := writer.WriteHeaders(HeadersFrameParam{StreamID: 1, BlockFragment: first}); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < n; i++ {
		if err := writer.WriteContinuation(1, false, frag); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.WriteContinuation(1, true, nil); err != nil {
		t.Fatal(err)
	}
	return wire.Bytes()
}

// A header block is bounded on the wire as well as by the fields it carries:
// CONTINUATION frames that carry nothing, and fragments that only update the
// table's size, emit no field, and were read for as long as a server sent them.
func TestAHeaderBlockIsBoundedOnTheWire(t *testing.T) {
	for _, test := range []struct {
		name string
		frag []byte
	}{
		{"empty continuations", nil},
		// 40 bytes stay within twice what is left of 64 after :status.
		{"table size updates", bytes.Repeat([]byte{0x20}, 40)},
	} {
		t.Run(test.name, func(t *testing.T) {
			wire := emptyFlood(t, []byte{0x88}, 1000, test.frag)
			reader := bytes.NewReader(wire)
			fr := NewFramer(nil, reader)
			fr.MaxHeaderListSize = 64
			fr.ReadMetaHeaders = hpack.NewDecoder(4096, nil)
			if _, err := fr.ReadFrame(); err != ConnectionError(ErrCodeProtocol) {
				t.Fatalf("ReadFrame = %v, want connection PROTOCOL_ERROR", err)
			}
			if read := len(wire) - reader.Len(); read > 2*64+frameHeaderLen+len(test.frag) {
				t.Fatalf("read %d bytes of a block bounded at %d", read, 2*64)
			}
		})
	}
}

// The bound is twice the field list's on the wire, frame headers included: a
// block of exactly that many bytes is read, one more frame is not.
func TestAHeaderBlockAtItsWireBoundIsRead(t *testing.T) {
	// HEADERS of :status 200 and a table size update, 9+2 bytes, then 13
	// frames of 9: 128 bytes, twice 64.
	for _, test := range []struct {
		empty int
		want  error
	}{{12, nil}, {13, ConnectionError(ErrCodeProtocol)}} {
		t.Run(fmt.Sprint(test.empty), func(t *testing.T) {
			fr := NewFramer(nil, bytes.NewReader(emptyFlood(t, []byte{0x88, 0x20}, test.empty, nil)))
			fr.MaxHeaderListSize = 64
			fr.ReadMetaHeaders = hpack.NewDecoder(4096, nil)
			frame, err := fr.ReadFrame()
			if err != test.want {
				t.Fatalf("ReadFrame = %v, want %v", err, test.want)
			}
			if err == nil && len(frame.(*MetaHeadersFrame).Fields) != 1 {
				t.Fatalf("fields %v", frame.(*MetaHeadersFrame).Fields)
			}
		})
	}
}

// A sender may cut a block as small as it likes within the bound.
func TestABlockOfSmallFramesWithinItsBoundIsRead(t *testing.T) {
	block := limitBlock(t, hpack.HeaderField{Name: ":status", Value: "200"}, hpack.HeaderField{Name: "x-audit", Value: strings.Repeat("a", 40)})
	var wire bytes.Buffer
	writer := NewFramer(&wire, nil)
	if err := writer.WriteHeaders(HeadersFrameParam{StreamID: 1, BlockFragment: block[:1]}); err != nil {
		t.Fatal(err)
	}
	for i := 1; i < len(block); i++ {
		if err := writer.WriteContinuation(1, i == len(block)-1, block[i:i+1]); err != nil {
			t.Fatal(err)
		}
	}
	fr := NewFramer(nil, &wire)
	fr.MaxHeaderListSize = 4096
	fr.ReadMetaHeaders = hpack.NewDecoder(4096, nil)
	frame, err := fr.ReadFrame()
	if err != nil {
		t.Fatal(err)
	}
	if fields := frame.(*MetaHeadersFrame).Fields; len(fields) != 2 || fields[1].Value != strings.Repeat("a", 40) {
		t.Fatalf("fields %v", fields)
	}
}

func TestTransportClosesAnEmptyContinuationFlood(t *testing.T) {
	ct := newClientTester(t)
	ct.tr.Settings = map[SettingID]uint32{SettingMaxHeaderListSize: 64}
	ct.tr.SettingsOrder = []SettingID{SettingMaxHeaderListSize}
	_ = ct.sc.SetDeadline(time.Now().Add(2 * time.Second))
	ct.client = func() error {
		ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
		defer cancel()
		req, _ := http.NewRequestWithContext(ctx, "GET", "https://dummy.tld/", nil)
		res, err := ct.tr.RoundTrip(req)
		if res != nil {
			res.Body.Close()
		}
		if err == nil || errors.Is(err, context.DeadlineExceeded) {
			return fmt.Errorf("flood was not rejected before the request deadline: %v", err)
		}
		return nil
	}
	ct.server = func() error {
		ct.greet()
		hf, err := ct.firstHeaders()
		if err != nil {
			return err
		}
		if err := ct.fr.WriteHeaders(HeadersFrameParam{StreamID: hf.StreamID, BlockFragment: []byte{0x88}}); err != nil {
			return err
		}
		for i := 0; ; i++ {
			if err := ct.fr.WriteContinuation(hf.StreamID, false, nil); err != nil {
				if errors.Is(err, os.ErrDeadlineExceeded) {
					return fmt.Errorf("the client read %d empty CONTINUATION frames and kept the connection", i)
				}
				return nil
			}
		}
	}
	ct.run()
}

// A field value may be a secret: an invalid one is reported by its field's
// name, as x/net does.
func TestAnInvalidFieldValueIsNotSaidInTheError(t *testing.T) {
	var wire bytes.Buffer
	block := limitBlock(t, hpack.HeaderField{Name: ":status", Value: "200"}, hpack.HeaderField{Name: "x-token", Value: "s3cret\x00"})
	if err := NewFramer(&wire, nil).WriteHeaders(HeadersFrameParam{StreamID: 1, EndHeaders: true, BlockFragment: block}); err != nil {
		t.Fatal(err)
	}
	fr := NewFramer(nil, &wire)
	fr.ReadMetaHeaders = hpack.NewDecoder(4096, nil)
	_, err := fr.ReadFrame()
	if err == nil || strings.Contains(err.Error(), "s3cret") || !strings.Contains(err.Error(), `"x-token"`) {
		t.Fatalf("ReadFrame = %v, want the field's name and not its value", err)
	}
	if detail := fr.ErrorDetail(); detail == nil || strings.Contains(detail.Error(), "s3cret") {
		t.Fatalf("ErrorDetail = %v", detail)
	}
}

package http2

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
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

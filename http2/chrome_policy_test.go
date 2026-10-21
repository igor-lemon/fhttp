package http2

import (
	"bytes"
	"context"
	http "github.com/igor-lemon/fhttp"
	"github.com/igor-lemon/fhttp/http2/hpack"
	tls "github.com/igor-lemon/utls"
	"io"
	"net"
	"testing"
	"time"
)

func TestChromeResponsePoliciesReachThePeer(t *testing.T) {
	for _, variant := range []string{"valid", "unsolicited-pong", "transfer-encoding", "early-data", "partial-reset", "invalid-index", "oversize", "zero-stream-window", "overflow-window", "connection", "keep-alive"} {
		t.Run(variant, func(t *testing.T) {
			ln, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			defer ln.Close()
			controls := make(chan uint32, 4)
			done := make(chan struct{})
			go func() {
				defer close(done)
				conn, err := ln.Accept()
				if err != nil {
					return
				}
				defer conn.Close()
				conn.SetDeadline(time.Now().Add(2 * time.Second))
				if _, err = io.ReadFull(conn, make([]byte, len(ClientPreface))); err != nil {
					return
				}
				fr := NewFramer(conn, conn)
				fr.WriteSettings()
				var stream uint32
				for stream == 0 {
					f, err := fr.ReadFrame()
					if err != nil {
						return
					}
					if h, ok := f.(*HeadersFrame); ok {
						stream = h.StreamID
					}
				}
				var block bytes.Buffer
				enc := hpack.NewEncoder(&block)
				enc.WriteField(hpack.HeaderField{Name: ":status", Value: "200"})
				switch variant {
				case "zero-stream-window":
					fr.WriteRawFrame(FrameWindowUpdate, 0, stream, make([]byte, 4))
				case "overflow-window":
					fr.WriteWindowUpdate(0, 0x7fffffff)
				case "unsolicited-pong":
					fr.WritePing(true, [8]byte{1})
				case "early-data":
					fr.WriteData(stream, true, []byte("x"))
				case "invalid-index":
					fr.WriteHeaders(HeadersFrameParam{StreamID: stream, EndHeaders: true, EndStream: true, BlockFragment: []byte{0xff, 0xff, 0xff, 0x7f}})
				case "oversize":
					fr.WriteRawFrame(FrameData, 0, stream, make([]byte, 16385))
				default:
					if variant == "connection" || variant == "keep-alive" {
						enc.WriteField(hpack.HeaderField{Name: variant, Value: "keep-alive"})
					}
					if variant == "transfer-encoding" {
						enc.WriteField(hpack.HeaderField{Name: "transfer-encoding", Value: "chunked"})
					}
					fr.WriteHeaders(HeadersFrameParam{StreamID: stream, EndHeaders: true, EndStream: variant != "partial-reset", BlockFragment: block.Bytes()})
					if variant == "partial-reset" {
						fr.WriteData(stream, false, []byte("partial"))
						fr.WriteRSTStream(stream, ErrCodeCancel)
					}
				}
				conn.SetReadDeadline(time.Now().Add(300 * time.Millisecond))
				for {
					f, err := fr.ReadFrame()
					if err != nil {
						return
					}
					switch v := f.(type) {
					case *GoAwayFrame:
						controls <- uint32(v.ErrCode)
					case *RSTStreamFrame:
						controls <- 100 + uint32(v.ErrCode)
					}
				}
			}()
			tr := &Transport{ChromeBehavior: true, DialTLSContext: func(ctx context.Context, network, addr string, _ *tls.Config) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, "tcp", ln.Addr().String())
			}}
			defer tr.CloseIdleConnections()
			req, _ := http.NewRequest("GET", "https://example.com/", nil)
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			req = req.WithContext(ctx)
			res, err := tr.RoundTrip(req)
			if err == nil {
				_, err = io.ReadAll(res.Body)
				res.Body.Close()
			}
			if (variant == "valid" || variant == "connection" || variant == "keep-alive") && err != nil {
				t.Fatal(err)
			}
			if (variant != "valid" && variant != "connection" && variant != "keep-alive") && err == nil {
				t.Fatal("malformed/aborted response succeeded")
			}
			<-done
			close(controls)
			var got []uint32
			for n := range controls {
				got = append(got, n)
			}
			expected := map[string]uint32{"unsolicited-pong": 1, "transfer-encoding": 101, "early-data": 101, "invalid-index": 9, "oversize": 6, "zero-stream-window": 103, "overflow-window": 1}
			if want, ok := expected[variant]; ok {
				found := false
				for _, n := range got {
					found = found || n == want
				}
				if !found {
					t.Fatalf("wire controls%v,want%d", got, want)
				}
			}
			if variant == "partial-reset" {
				for _, n := range got {
					if n == 108 {
						t.Fatal("redundant CANCEL after peer reset")
					}
				}
			}
		})
	}
}

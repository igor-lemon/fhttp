package http2

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"testing"
	"time"

	http "github.com/igor-lemon/fhttp"
	"github.com/igor-lemon/utls/behavior"
	witness "golang.org/x/net/http2/hpack"
)

func startupPolicy(mode string) behavior.Policy {
	p := behavior.Legacy()
	p["h2.read_loop_start"], _ = json.Marshal(mode)
	return p
}

// Real TCP, original request blocks, independently decoded table state. The
// peer sends its SETTINGS before any request, then accepts several requests.
// An immediate-mode positive control must actually see the early ACK, proving
// this peer can expose the startup race that first-headers mode prevents.
func TestInitialReaderPolicyPreservesHPACKState(t *testing.T) {
	for _, mode := range []string{"first-headers", "connection"} {
		for _, sizes := range [][]uint32{{0}, {128}, {4096}, {65536}, {0, 128, 65536}} {
			t.Run(fmt.Sprint(mode, "/", sizes), func(t *testing.T) {
				ln, err := net.Listen("tcp4", "127.0.0.1:0")
				if err != nil {
					t.Fatal(err)
				}
				defer ln.Close()
				type frame struct {
					kind   FrameType
					ack    bool
					block  []byte
					fields []witness.HeaderField
				}
				frames := make(chan frame, 32)
				ready := make(chan struct{})
				done := make(chan error, 1)
				go func() {
					peer, e := ln.Accept()
					if e != nil {
						done <- e
						return
					}
					defer peer.Close()
					peer.SetDeadline(time.Now().Add(4 * time.Second))
					fr := NewFramer(peer, peer)
					for _, size := range sizes {
						if e = fr.WriteSettings(Setting{ID: SettingHeaderTableSize, Val: size}); e != nil {
							done <- e
							return
						}
					}
					prefix := make([]byte, len(ClientPreface))
					if _, e = io.ReadFull(peer, prefix); e != nil {
						done <- e
						return
					}
					if string(prefix) != ClientPreface {
						done <- fmt.Errorf("bad preface")
						return
					}
					close(ready)
					decoder := witness.NewDecoder(1<<20, nil)
					decoder.SetAllowedMaxDynamicTableSize(1 << 20)
					n := 0
					for {
						f, e := fr.ReadFrame()
						if e != nil {
							done <- e
							return
						}
						r := frame{kind: f.Header().Type}
						switch f := f.(type) {
						case *SettingsFrame:
							r.ack = f.IsAck()
							if !r.ack {
								if e = fr.WriteSettingsAck(); e != nil {
									done <- e
									return
								}
							}
						case *HeadersFrame:
							if !f.HeadersEnded() {
								done <- fmt.Errorf("unexpected fragmented small request")
								return
							}
							r.block = append([]byte(nil), f.HeaderBlockFragment()...)
							r.fields, e = decoder.DecodeFull(r.block)
							if e != nil {
								done <- e
								return
							}
							n++
							if e = fr.WriteHeaders(HeadersFrameParam{StreamID: f.StreamID, EndHeaders: true, EndStream: true, BlockFragment: []byte{0x88}}); e != nil {
								done <- e
								return
							}
						}
						frames <- r
						if n == 3 {
							done <- nil
							return
						}
					}
				}()
				socket, err := net.Dial("tcp4", ln.Addr().String())
				if err != nil {
					t.Fatal(err)
				}
				tr := &Transport{Behavior: startupPolicy(mode), MaxEncoderHeaderTableSize: 1 << 20}
				cc, err := tr.NewClientConn(socket)
				if err != nil {
					t.Fatal(err)
				}
				defer cc.Close()
				<-ready
				observed := []frame{}
				if mode == "connection" {
					acked := 0
					timer := time.NewTimer(2 * time.Second)
					defer timer.Stop()
					for acked < len(sizes) {
						select {
						case r := <-frames:
							observed = append(observed, r)
							if r.kind == FrameSettings && r.ack {
								acked++
							}
						case <-timer.C:
							t.Fatal("positive control did not read peer SETTINGS")
						}
					}
				} else {
					// Waiting between constructor and first request is deliberate: the old
					// reader had ample time to consume and ACK already-buffered SETTINGS.
					time.Sleep(50 * time.Millisecond)
				}
				for i := range 3 {
					ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
					req, e := http.NewRequestWithContext(ctx, "GET", "https://localhost/"+fmt.Sprint(i), nil)
					if e != nil {
						t.Fatal(e)
					}
					req.Header.Set("X-Repeated", "same")
					req.Header.Set("X-Sequence", fmt.Sprint(i))
					rsp, e := cc.RoundTrip(req)
					cancel()
					if e != nil {
						t.Fatal(e)
					}
					rsp.Body.Close()
				}
				if e := <-done; e != nil {
					t.Fatal(e)
				}
				close(frames)
				for r := range frames {
					observed = append(observed, r)
				}
				blocks := [][]byte{}
				firstHeaders := -1
				firstAck := -1
				for i, r := range observed {
					if r.kind == FrameHeaders {
						if firstHeaders < 0 {
							firstHeaders = i
						}
						blocks = append(blocks, r.block)
						for _, field := range r.fields {
							if field.Name == "x-sequence" && field.Value != fmt.Sprint(len(blocks)-1) {
								t.Fatal("request state became static")
							}
						}
					}
					if firstAck < 0 && r.kind == FrameSettings && r.ack {
						firstAck = i
					}
				}
				if len(blocks) != 3 {
					t.Fatal("missing actual request blocks")
				}
				updates := func(block []byte) []uint64 {
					out := []uint64{}
					for len(block) > 0 && block[0]&0xe0 == 0x20 {
						n := uint64(block[0] & 31)
						block = block[1:]
						if n == 31 {
							var shift uint
							for {
								v := block[0]
								block = block[1:]
								n += uint64(v&127) << shift
								if v&128 == 0 {
									break
								}
								shift += 7
							}
						}
						out = append(out, n)
					}
					return out
				}
				if mode == "first-headers" {
					if firstAck >= 0 && firstAck < firstHeaders {
						t.Fatal("initial SETTINGS ACK overtook first request HEADERS")
					}
					if len(updates(blocks[0])) != 0 {
						t.Fatal("peer table update changed initial HPACK block")
					}
					if sizes[len(sizes)-1] != 4096 {
						u := updates(blocks[1])
						if len(u) == 0 || u[len(u)-1] != uint64(sizes[len(sizes)-1]) {
							t.Fatalf("later SETTINGS table state not applied: %v", u)
						}
						if len(sizes) > 1 && (len(u) != 2 || u[0] != 0) {
							t.Fatalf("minimum intervening table size lost: %v", u)
						}
					}
				} else if firstAck < 0 || firstAck > firstHeaders {
					t.Fatal("immediate positive control failed")
				}
				if bytes.Equal(blocks[1], blocks[2]) {
					t.Fatal("request-derived path and sequence did not change")
				}
			})
		}
	}
}

func TestUnclaimedReaderPolicyControlAndClose(t *testing.T) {
	// Explicit PING is a demand for input even without a request stream. Closing
	// an unclaimed connection must terminate readerDone with exactly one reader.
	for _, op := range []string{"ping", "close", "idle-close", "shutdown"} {
		t.Run(op, func(t *testing.T) {
			ln, e := net.Listen("tcp4", "127.0.0.1:0")
			if e != nil {
				t.Fatal(e)
			}
			defer ln.Close()
			go func() {
				p, e := ln.Accept()
				if e != nil {
					return
				}
				defer p.Close()
				p.SetDeadline(time.Now().Add(2 * time.Second))
				prefix := make([]byte, len(ClientPreface))
				io.ReadFull(p, prefix)
				fr := NewFramer(p, p)
				fr.WriteSettings()
				for {
					f, e := fr.ReadFrame()
					if e != nil {
						return
					}
					if f, ok := f.(*PingFrame); ok && !f.IsAck() {
						fr.WritePing(true, f.Data)
					}
				}
			}()
			c, e := net.Dial("tcp4", ln.Addr().String())
			if e != nil {
				t.Fatal(e)
			}
			tr := &Transport{Behavior: startupPolicy("first-headers")}
			cc, e := tr.NewClientConn(c)
			if e != nil {
				t.Fatal(e)
			}
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			switch op {
			case "ping":
				if e = cc.Ping(ctx); e != nil {
					t.Fatal(e)
				}
				cc.Close()
			case "close":
				cc.Close()
			case "idle-close":
				cc.closeIfIdle()
			case "shutdown":
				if e = cc.Shutdown(ctx); e != nil {
					t.Fatal(e)
				}
			}
			select {
			case <-cc.readerDone:
			case <-ctx.Done():
				t.Fatal("unclaimed reader did not terminate")
			}
		})
	}
}

func TestInitialReaderStartsForALPSBlockedStream(t *testing.T) {
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	done := make(chan error, 1)
	go func() {
		peer, e := ln.Accept()
		if e != nil {
			done <- e
			return
		}
		defer peer.Close()
		peer.SetDeadline(time.Now().Add(3 * time.Second))
		prefix := make([]byte, len(ClientPreface))
		if _, e = io.ReadFull(peer, prefix); e != nil {
			done <- e
			return
		}
		fr := NewFramer(peer, peer)
		if e = fr.WriteSettings(Setting{ID: SettingMaxConcurrentStreams, Val: 1}); e != nil {
			done <- e
			return
		}
		for {
			f, e := fr.ReadFrame()
			if e != nil {
				done <- e
				return
			}
			switch f := f.(type) {
			case *SettingsFrame:
				if !f.IsAck() {
					fr.WriteSettingsAck()
				}
			case *HeadersFrame:
				if e = fr.WriteHeaders(HeadersFrameParam{StreamID: f.StreamID, EndHeaders: true, EndStream: true, BlockFragment: []byte{0x88}}); e != nil {
					done <- e
					return
				}
				done <- nil
				return
			}
		}
	}()
	socket, e := net.Dial("tcp4", ln.Addr().String())
	if e != nil {
		t.Fatal(e)
	}
	var alps bytes.Buffer
	NewFramer(&alps, nil).WriteSettings(Setting{ID: SettingMaxConcurrentStreams, Val: 0})
	tr := &Transport{Behavior: startupPolicy("first-headers"), StrictMaxConcurrentStreams: true}
	cc, e := tr.NewClientConn(alpsConn{Conn: socket, alps: alps.Bytes()})
	if e != nil {
		t.Fatal(e)
	}
	defer cc.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	req, e := http.NewRequestWithContext(ctx, "GET", "https://localhost/", nil)
	if e != nil {
		t.Fatal(e)
	}
	rsp, e := cc.RoundTrip(req)
	if e != nil {
		t.Fatalf("ALPS stream wait did not consume later peer SETTINGS: %v", e)
	}
	rsp.Body.Close()
	if e = <-done; e != nil {
		t.Fatal(e)
	}
}

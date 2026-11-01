package http2

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	http "github.com/igor-lemon/fhttp"
	"github.com/igor-lemon/fhttp/http2/hpack"
	"github.com/igor-lemon/fhttp/httptrace"
	tls "github.com/igor-lemon/utls"
	"io"
	"net"
	"sync"
	"testing"
	"time"
)

func TestHeaderTimingWaitsForFinalContinuation(t *testing.T) {
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	release := make(chan struct{})
	var once sync.Once
	letGo := func() { once.Do(func() { close(release) }) }
	defer letGo()
	partial := make(chan struct{})
	peer := make(chan error, 1)
	go func() {
		peer <- func() error {
			c, e := ln.Accept()
			if e != nil {
				return e
			}
			defer c.Close()
			_ = c.SetDeadline(time.Now().Add(4 * time.Second))
			r := bufio.NewReader(c)
			preface := make([]byte, len(ClientPreface))
			if _, e = io.ReadFull(r, preface); e != nil {
				return e
			}
			if string(preface) != ClientPreface {
				return fmt.Errorf("bad preface")
			}
			fr := NewFramer(c, r)
			if e = fr.WriteSettings(); e != nil {
				return e
			}
			var stream uint32
			for stream == 0 {
				f, e := fr.ReadFrame()
				if e != nil {
					return e
				}
				switch f := f.(type) {
				case *SettingsFrame:
					if !f.IsAck() {
						if e = fr.WriteSettingsAck(); e != nil {
							return e
						}
					}
				case *HeadersFrame:
					stream = f.StreamID
				}
			}
			var b bytes.Buffer
			enc := hpack.NewEncoder(&b)
			_ = enc.WriteField(hpack.HeaderField{Name: ":status", Value: "103"})
			if e = fr.WriteHeaders(HeadersFrameParam{StreamID: stream, BlockFragment: b.Bytes(), EndHeaders: true}); e != nil {
				return e
			}
			b.Reset()
			_ = enc.WriteField(hpack.HeaderField{Name: ":status", Value: "200"})
			_ = enc.WriteField(hpack.HeaderField{Name: "x-held", Value: "complete"})
			block := append([]byte(nil), b.Bytes()...)
			if e = fr.WriteHeaders(HeadersFrameParam{StreamID: stream, BlockFragment: block[:1], EndHeaders: false}); e != nil {
				return e
			}
			close(partial)
			<-release
			if e = fr.WriteContinuation(stream, true, block[1:]); e != nil {
				return e
			}
			return fr.WriteData(stream, true, []byte("ok"))
		}()
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	var mu sync.Mutex
	starts, ends, bodyBytes := 0, 0, 0
	first := make(chan struct{}, 1)
	done := make(chan error, 1)
	trace := &httptrace.ClientTrace{GotResponseBodyBytes: func(method string, n int) {
		mu.Lock()
		defer mu.Unlock()
		if method != "GET" {
			t.Error("body method", method)
		}
		bodyBytes += n
	}, WriteHeadersStart: func(method string) {
		mu.Lock()
		defer mu.Unlock()
		if method != "GET" {
			t.Errorf("method=%s", method)
		}
		starts++
	}, GotFirstResponseByte: func() { first <- struct{}{} }, GotResponseHeaders: func(method string, status int) {
		mu.Lock()
		defer mu.Unlock()
		if method != "GET" || status != 200 {
			t.Errorf("final event=%s/%d", method, status)
		}
		ends++
	}}
	req, _ := http.NewRequestWithContext(httptrace.WithClientTrace(ctx, trace), "GET", "http://"+ln.Addr().String()+"/", nil)
	tr := &Transport{AllowHTTP: true, DialTLS: func(network, addr string, _ *tls.Config) (net.Conn, error) { return net.Dial(network, addr) }}
	defer tr.CloseIdleConnections()
	go func() {
		res, e := tr.RoundTrip(req)
		if e == nil {
			b, re := io.ReadAll(res.Body)
			res.Body.Close()
			if re != nil {
				e = re
			} else if string(b) != "ok" {
				e = fmt.Errorf("body=%q", b)
			}
		}
		done <- e
	}()
	select {
	case <-partial:
	case <-ctx.Done():
		t.Fatal("missing partial response", ctx.Err())
	}
	select {
	case <-first:
	case <-ctx.Done():
		t.Fatal("missing first byte", ctx.Err())
	}
	mu.Lock()
	before := ends
	mu.Unlock()
	if before != 0 {
		t.Fatal("103/partial HEADERS reported as final complete headers")
	}
	letGo()
	if e := <-done; e != nil {
		t.Fatal(e)
	}
	if e := <-peer; e != nil {
		t.Fatal(e)
	}
	mu.Lock()
	defer mu.Unlock()
	if starts != 1 || ends != 1 || bodyBytes != 2 {
		t.Fatalf("timing counts start%d/end%d", starts, ends)
	}
}

func TestPingRTTUsesActualSentTimeAndCallsOutsideLocks(t *testing.T) {
	client, server := net.Pipe()
	defer client.Close()
	defer server.Close()
	cc := &ClientConn{t: &Transport{}, tconn: client, pings: make(map[[8]byte]chan struct{})}
	calls := 0
	cc.t.PingRTT = func(remote net.Addr, rtt time.Duration) {
		cc.mu.Lock()
		defer cc.mu.Unlock()
		cc.pingMu.Lock()
		defer cc.pingMu.Unlock()
		calls++
		if remote != server.LocalAddr() {
			t.Error("wrong peer")
		}
		if rtt < 20*time.Millisecond || rtt > time.Second {
			t.Errorf("invalid actual RTT %v", rtt)
		}
	}
	cc.pingSentAt = timeNow().Add(-25 * time.Millisecond)
	cc.pingInFlight = true
	frame := &PingFrame{FrameHeader: FrameHeader{Flags: FlagPingAck}}
	if e := (&clientConnReadLoop{cc: cc}).processPing(frame); e != nil {
		t.Fatal(e)
	}
	if calls != 1 || cc.pingInFlight {
		t.Fatalf("observer calls=%d inFlight=%v", calls, cc.pingInFlight)
	}
	if e := (&clientConnReadLoop{cc: cc}).processPing(frame); e != nil {
		t.Fatal(e)
	}
	if calls != 1 {
		t.Fatal("unsolicited ACK generated another sample")
	}
}

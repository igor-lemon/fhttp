package http2

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"os"
	"testing"
	"time"

	http "github.com/igor-lemon/fhttp"
	"github.com/igor-lemon/fhttp/http2/hpack"
	tls "github.com/igor-lemon/utls"
	"github.com/igor-lemon/utls/behavior"
)

// goAwayPeer is an HTTP/2 server of one connection that says GOAWAY when the
// test asks, and reports how long the client kept the connection open after
// the GOAWAY: closed is the client's EOF, kept a read that timed out.
type goAwayPeer struct {
	ln     net.Listener
	result chan goAwayResult
}

type goAwayResult struct {
	closedAfter time.Duration
	kept        bool
	err         error
}

// startGoAwayPeer answers the first request. order says when the GOAWAY
// goes: "after" the whole answer, "before" the answer (the stream still
// open, so the client must finish it first), or "cancel" with no answer at
// all, the client cancelling its stream after the GOAWAY. wait is how long
// the peer waits for the client to close before it says kept.
func startGoAwayPeer(t *testing.T, order string, wait time.Duration) *goAwayPeer {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	p := &goAwayPeer{ln: ln, result: make(chan goAwayResult, 1)}
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			p.result <- goAwayResult{err: err}
			return
		}
		defer conn.Close()
		conn.SetDeadline(time.Now().Add(5 * time.Second))
		if _, err := io.ReadFull(conn, make([]byte, len(ClientPreface))); err != nil {
			p.result <- goAwayResult{err: err}
			return
		}
		fr := NewFramer(conn, conn)
		fr.WriteSettings()
		var stream uint32
		for stream == 0 {
			f, err := fr.ReadFrame()
			if err != nil {
				p.result <- goAwayResult{err: err}
				return
			}
			if h, ok := f.(*HeadersFrame); ok {
				stream = h.StreamID
			}
		}
		var block bytes.Buffer
		enc := hpack.NewEncoder(&block)
		enc.WriteField(hpack.HeaderField{Name: ":status", Value: "200"})
		answer := func() {
			fr.WriteHeaders(HeadersFrameParam{StreamID: stream, EndHeaders: true, BlockFragment: block.Bytes()})
			fr.WriteData(stream, true, []byte("body"))
		}
		switch order {
		case "after":
			answer()
			time.Sleep(50 * time.Millisecond) // the client has read and closed the body
		case "before":
			fr.WriteGoAway(stream, ErrCodeNo, nil)
			time.Sleep(50 * time.Millisecond) // the stream is still the client's
			answer()
		}
		sent := time.Now()
		if order != "before" {
			fr.WriteGoAway(stream, ErrCodeNo, nil)
		}
		conn.SetReadDeadline(time.Now().Add(wait))
		for {
			if _, err := fr.ReadFrame(); err != nil {
				var ne net.Error
				if errors.As(err, &ne) && ne.Timeout() {
					p.result <- goAwayResult{kept: true}
				} else {
					p.result <- goAwayResult{closedAfter: time.Since(sent)}
				}
				return
			}
		}
	}()
	return p
}

func (p *goAwayPeer) transport(policy behavior.Policy) *Transport {
	return &Transport{Behavior: policy, DialTLSContext: func(ctx context.Context, network, addr string, _ *tls.Config) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "tcp", p.ln.Addr().String())
	}}
}

// TestASessionGoingAwayIsClosedOnceItHasNoStream holds "h2.close_on_goaway"
// to Chrome's SpdySession: closed as soon as a GOAWAY finds no stream, or its
// last stream ends, whether the server ends it or the client cancels it;
// without the field the connection stays until the server closes it, as Go's
// transport keeps it.
func TestASessionGoingAwayIsClosedOnceItHasNoStream(t *testing.T) {
	on := behavior.Policy{"h2.close_on_goaway": []byte("true")}
	off := behavior.Policy{"h2.close_on_goaway": []byte("false")}
	for _, tc := range []struct {
		name   string
		order  string
		policy behavior.Policy
		closes bool
	}{
		{"after-answer", "after", on, true},
		{"before-answer", "before", on, true},
		{"cancelled", "cancel", on, true},
		{"after-answer-off", "after", off, false},
		{"after-answer-absent", "after", nil, false},
		{"before-answer-off", "before", off, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := startGoAwayPeer(t, tc.order, 400*time.Millisecond)
			defer p.ln.Close()
			tr := p.transport(tc.policy)
			defer tr.CloseIdleConnections()
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			if tc.order == "cancel" {
				var cancelReq context.CancelFunc
				ctx, cancelReq = context.WithCancel(ctx)
				go func() {
					time.Sleep(100 * time.Millisecond) // the GOAWAY has come
					cancelReq()
				}()
			}
			req, _ := http.NewRequestWithContext(ctx, "GET", "https://example.com/", nil)
			res, err := tr.RoundTrip(req)
			if tc.order == "cancel" {
				if err == nil {
					res.Body.Close()
					t.Fatal("a request that had no answer succeeded")
				}
			} else {
				if err != nil {
					t.Fatal(err)
				}
				if body, err := io.ReadAll(res.Body); err != nil || string(body) != "body" {
					t.Fatalf("body %q, %v", body, err)
				}
				res.Body.Close()
			}
			got := <-p.result
			if got.err != nil {
				t.Fatal(got.err)
			}
			if tc.closes && got.kept {
				t.Fatal("the client kept a session going away with no stream")
			}
			if tc.closes && got.closedAfter > 250*time.Millisecond {
				t.Fatalf("closed %v after the GOAWAY", got.closedAfter)
			}
			if !tc.closes && !got.kept {
				t.Fatalf("the client closed %v after the GOAWAY without the policy", got.closedAfter)
			}
		})
	}
}

// TestCloseOnGoAwayLeavesALiveSessionAlone: the policy acts on a GOAWAY only;
// a session that never had one keeps its connection between requests.
func TestCloseOnGoAwayLeavesALiveSessionAlone(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	result := make(chan error, 1)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			result <- err
			return
		}
		defer conn.Close()
		conn.SetDeadline(time.Now().Add(5 * time.Second))
		io.ReadFull(conn, make([]byte, len(ClientPreface)))
		fr := NewFramer(conn, conn)
		fr.WriteSettings()
		for {
			f, err := fr.ReadFrame()
			if err != nil {
				result <- err
				return
			}
			if h, ok := f.(*HeadersFrame); ok {
				var block bytes.Buffer
				hpack.NewEncoder(&block).WriteField(hpack.HeaderField{Name: ":status", Value: "204"})
				fr.WriteHeaders(HeadersFrameParam{StreamID: h.StreamID, EndHeaders: true, EndStream: true, BlockFragment: block.Bytes()})
				conn.SetReadDeadline(time.Now().Add(300 * time.Millisecond))
			}
		}
	}()
	tr := &Transport{Behavior: behavior.Policy{"h2.close_on_goaway": []byte("true")}, DialTLSContext: func(ctx context.Context, network, addr string, _ *tls.Config) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "tcp", ln.Addr().String())
	}}
	defer tr.CloseIdleConnections()
	req, _ := http.NewRequest("GET", "https://example.com/", nil)
	res, err := tr.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if err := <-result; !errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatalf("server read %v, want its own timeout on an open connection", err)
	}
}

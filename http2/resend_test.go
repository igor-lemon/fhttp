// Copyright 2026 The fhttp Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package http2

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	http "github.com/igor-lemon/fhttp"
	"github.com/igor-lemon/fhttp/http2/hpack"
	"github.com/igor-lemon/utls/behavior"
)

// chromeRetries is what Chrome 155 does, as the shipped profiles say it: a
// request goes again at once, twice at most, for a refused stream or a GOAWAY
// with NO_ERROR above it, and 50 times at most, whatever its method, for a
// reused connection lost under it. Its read loop starts after the first
// HEADERS, as Chrome reads nothing before a stream's first request.
var chromeRetries = map[string]any{
	"h2.read_loop_start":            "first-headers",
	"h2.retries":                    2,
	"h2.retry_errors":               []string{"refused-stream", "goaway-no-error"},
	"h2.retry_delay_ms":             0,
	"http.connection_error_retries": 50,
	"http.connection_error_methods": "any",
}

// resendPolicy is base with the keys of change set (nil values taken out).
func resendPolicy(t *testing.T, base, change map[string]any) behavior.Policy {
	t.Helper()
	p := behavior.Policy{}
	for _, m := range []map[string]any{base, change} {
		for k, v := range m {
			if v == nil {
				delete(p, k)
				continue
			}
			b, err := json.Marshal(v)
			if err != nil {
				t.Fatal(err)
			}
			p[k] = b
		}
	}
	if err := p.Validate(); err != nil {
		t.Fatal(err)
	}
	return p
}

func (p *peer) goAway(last uint32, code ErrCode) {
	p.write(func() error { return p.fr.WriteGoAway(last, code, nil) })
}

// inform says 103 on stream, which goes on.
func (p *peer) inform(stream uint32) {
	var block bytes.Buffer
	hpack.NewEncoder(&block).WriteField(hpack.HeaderField{Name: ":status", Value: "103"})
	p.write(func() error {
		return p.fr.WriteHeaders(HeadersFrameParam{StreamID: stream, EndHeaders: true, BlockFragment: block.Bytes()})
	})
}

// hangUp closes the connection: a FIN, or a RST when reset says so.
func (p *peer) hangUp(reset bool) {
	if reset {
		p.conn.(*net.TCPConn).SetLinger(0)
	}
	p.conn.Close()
}

// quiet is a peer that reads nothing more for d, or only the end.
func (p *peer) quiet(d time.Duration) {
	p.t.Helper()
	select {
	case s, ok := <-p.seen:
		if ok && !s.eof {
			p.t.Fatalf("got %v, want nothing more", s)
		}
	case <-time.After(d):
	}
}

// request is a GET, or a POST of "x" when post says so, made with ctx.
func request(ctx context.Context, post bool) *http.Request {
	if post {
		req, _ := http.NewRequestWithContext(ctx, "POST", "https://dummy.tld/", strings.NewReader("x"))
		return req
	}
	req, _ := http.NewRequestWithContext(ctx, "GET", "https://dummy.tld/", nil)
	return req
}

// TestARefusedStreamGoesAgainAtOnceOnItsConnection: a request whose stream is
// refused goes again at once, on its connection, as many times as h2.retries
// says; refused once more, it fails with the refusal.
func TestARefusedStreamGoesAgainAtOnceOnItsConnection(t *testing.T) {
	for _, refusals := range []int{1, 2, 3} {
		t.Run(fmt.Sprint(refusals), func(t *testing.T) {
			clients, servers := pairs(t, 1)
			tr, _ := pingTransport(t, resendPolicy(t, chromeRetries, nil), clients...)
			p := newPeer(t, servers[0])
			began := time.Now()
			done := fetch(tr, request(context.Background(), false))
			for i := range refusals {
				p.expect(fmt.Sprintf("H%de", 2*i+1))
				p.refuse(uint32(2*i + 1))
			}
			answered := refusals <= 2
			if answered {
				p.expect(fmt.Sprintf("H%de", 2*refusals+1))
				p.answer(uint32(2*refusals + 1))
			}
			err := waitFor(t, done)
			if se, ok := err.(StreamError); answered && err != nil || !answered && (!ok || se.Code != ErrCodeRefusedStream) {
				t.Fatalf("got %v, answered %v", err, answered)
			}
			p.quiet(200 * time.Millisecond)
			if took := time.Since(began); took > 700*time.Millisecond {
				t.Errorf("took %v: a try waited", took)
			}
		})
	}
}

// TestARefusedPOSTGoesAgainWithItsBody: the body goes again too.
func TestARefusedPOSTGoesAgainWithItsBody(t *testing.T) {
	clients, servers := pairs(t, 1)
	tr, _ := pingTransport(t, resendPolicy(t, chromeRetries, nil), clients...)
	p := newPeer(t, servers[0])
	done := fetch(tr, request(context.Background(), true))
	p.expect("H1", "D1e:x")
	p.refuse(1)
	p.expect("H3", "D3e:x")
	p.answer(3)
	if err := waitFor(t, done); err != nil {
		t.Fatal(err)
	}
}

// TestAGoAwayAboveARequestSendsItAgainAsTheProfileSays: a stream above the
// last a GOAWAY says the server took goes again at once on another
// connection when h2.retry_errors names the GOAWAY's kind; one with an error
// code fails, as Chrome's ERR_HTTP2_PROTOCOL_ERROR does, unless the profile
// names "goaway-error" too.
func TestAGoAwayAboveARequestSendsItAgainAsTheProfileSays(t *testing.T) {
	for _, tc := range []struct {
		name  string
		code  ErrCode
		named []string
		again bool
		err   string
	}{
		{"no-error", ErrCodeNo, nil, true, ""},
		{"protocol-error", ErrCodeProtocol, nil, false, "GOAWAY from server ErrCode:PROTOCOL_ERROR"},
		{"enhance-your-calm", ErrCodeEnhanceYourCalm, nil, false, "GOAWAY from server ErrCode:ENHANCE_YOUR_CALM"},
		{"error-named", ErrCodeInternal, []string{"goaway-error"}, true, ""},
		{"no-error-not-named", ErrCodeNo, []string{"refused-stream"}, false, "graceful shutdown GOAWAY"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			change := map[string]any{}
			if tc.named != nil {
				change["h2.retry_errors"] = tc.named
			}
			clients, servers := pairs(t, 2)
			tr, _ := pingTransport(t, resendPolicy(t, chromeRetries, change), clients...)
			a, b := newPeer(t, servers[0]), newPeer(t, servers[1])
			exchange(t, tr, a, false, "H1e")
			began := time.Now()
			done := fetch(tr, request(context.Background(), false))
			a.expect("H3e")
			a.goAway(1, tc.code)
			if tc.again {
				b.expect("H1e")
				b.answer(1)
			}
			err := waitFor(t, done)
			if tc.again != (err == nil) || err != nil && !strings.Contains(err.Error(), tc.err) {
				t.Fatalf("got %v, want it sent again %v (%q)", err, tc.again, tc.err)
			}
			if !tc.again {
				b.quiet(200 * time.Millisecond)
			}
			if took := time.Since(began); took > 700*time.Millisecond {
				t.Errorf("took %v: a try waited", took)
			}
		})
	}
}

// TestRefusalsAndGoAwaysShareTheirRetries: a request refused, then above a
// GOAWAY, then refused again has spent the two retries both count in.
func TestRefusalsAndGoAwaysShareTheirRetries(t *testing.T) {
	clients, servers := pairs(t, 2)
	tr, _ := pingTransport(t, resendPolicy(t, chromeRetries, nil), clients...)
	a, b := newPeer(t, servers[0]), newPeer(t, servers[1])
	exchange(t, tr, a, false, "H1e")
	done := fetch(tr, request(context.Background(), false))
	a.expect("H3e")
	a.refuse(3)
	a.expect("H5e")
	a.goAway(3, ErrCodeNo)
	b.expect("H1e")
	b.refuse(1)
	if err := waitFor(t, done); err == nil || failureOf(err) != "refused-stream" {
		t.Fatalf("got %v, want the last refusal", err)
	}
	b.quiet(200 * time.Millisecond)
}

// TestALostConnectionSendsARequestAgainAsChromeDoes: a request whose reused
// connection is closed or reset under it before its answer goes again at
// once on another, a POST with its body; not one whose connection was new,
// nor one that had a 1xx, nor a POST when the profile names idempotent
// methods alone, nor any without http.connection_error_retries.
func TestALostConnectionSendsARequestAgainAsChromeDoes(t *testing.T) {
	for _, tc := range []struct {
		name    string
		change  map[string]any
		fresh   bool
		reset   bool
		post    bool
		inform  bool
		again   bool
		resends *http.Resends
	}{
		{name: "closed", again: true},
		{name: "reset", reset: true, again: true},
		{name: "post", post: true, again: true},
		{name: "post-idempotent", change: map[string]any{"http.connection_error_methods": "idempotent"}, post: true},
		{name: "get-idempotent", change: map[string]any{"http.connection_error_methods": "idempotent"}, again: true},
		{name: "fresh", fresh: true},
		{name: "informed", inform: true},
		{name: "none", change: map[string]any{"http.connection_error_retries": 0}},
		{name: "unsaid", change: map[string]any{"http.connection_error_retries": nil, "http.connection_error_methods": nil}},
		{name: "spent", change: map[string]any{"http.connection_error_retries": 1}, resends: spentLost()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clients, servers := pairs(t, 2)
			tr, _ := pingTransport(t, resendPolicy(t, chromeRetries, tc.change), clients...)
			a, b := newPeer(t, servers[0]), newPeer(t, servers[1])
			stream := uint32(1)
			if !tc.fresh {
				exchange(t, tr, a, false, "H1e")
				stream = 3
			}
			ctx := context.Background()
			if tc.resends != nil {
				ctx = http.WithResends(ctx, tc.resends)
			}
			done := fetch(tr, request(ctx, tc.post))
			if tc.post {
				a.expect(fmt.Sprintf("H%d", stream), fmt.Sprintf("D%de:x", stream))
			} else {
				a.expect(fmt.Sprintf("H%de", stream))
			}
			if tc.inform {
				a.inform(stream)
				time.Sleep(50 * time.Millisecond)
			}
			a.hangUp(tc.reset)
			if tc.again {
				if tc.post {
					b.expect("H1", "D1e:x")
				} else {
					b.expect("H1e")
				}
				b.answer(1)
			}
			err := waitFor(t, done)
			if tc.again != (err == nil) {
				t.Fatalf("got %v, want it sent again %v", err, tc.again)
			}
			if !tc.again {
				if _, wrapped := err.(lostConnError); wrapped || !http.ConnectionLost(err) {
					t.Fatalf("got %T %v, want the connection's own error", err, err)
				}
				b.quiet(200 * time.Millisecond)
			}
		})
	}
}

// spentLost is a count of resends that went again once for a lost connection.
func spentLost() *http.Resends {
	r := &http.Resends{}
	r.Lost(1)
	return r
}

// TestAGoAwayThenALostConnectionSendsTheRequestAgain: a stream at or below
// the last a GOAWAY says the server took, whose connection then closes, goes
// again on another, as Chrome's stream closed with ERR_CONNECTION_CLOSED
// does; without the profile's retries it fails with the GOAWAY's error.
func TestAGoAwayThenALostConnectionSendsTheRequestAgain(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change map[string]any
		again  bool
	}{
		{"chrome", nil, true},
		{"legacy", map[string]any{"h2.retries": nil, "h2.retry_errors": nil, "h2.retry_delay_ms": nil, "http.connection_error_retries": nil, "http.connection_error_methods": nil}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clients, servers := pairs(t, 2)
			tr, _ := pingTransport(t, resendPolicy(t, chromeRetries, tc.change), clients...)
			a, b := newPeer(t, servers[0]), newPeer(t, servers[1])
			exchange(t, tr, a, false, "H1e")
			done := fetch(tr, request(context.Background(), false))
			a.expect("H3e")
			a.goAway(3, ErrCodeNo)
			time.Sleep(50 * time.Millisecond)
			a.hangUp(false)
			if tc.again {
				b.expect("H1e")
				b.answer(1)
			}
			err := waitFor(t, done)
			if goneAway, ok := err.(GoAwayError); tc.again != (err == nil) || !tc.again && (!ok || goneAway.LastStreamID != 3) {
				t.Fatalf("got %T %v, want it sent again %v", err, err, tc.again)
			}
		})
	}
}

// TestWithoutRetriesTheTransportTriesAsItAlwaysDid: with no h2.retries, a
// stream above any GOAWAY goes again on another connection, and a lost
// connection fails the request with what the connection said.
func TestWithoutRetriesTheTransportTriesAsItAlwaysDid(t *testing.T) {
	legacy := map[string]any{"h2.read_loop_start": "first-headers"}
	t.Run("goaway", func(t *testing.T) {
		clients, servers := pairs(t, 2)
		tr, _ := pingTransport(t, resendPolicy(t, legacy, nil), clients...)
		a, b := newPeer(t, servers[0]), newPeer(t, servers[1])
		exchange(t, tr, a, false, "H1e")
		done := fetch(tr, request(context.Background(), false))
		a.expect("H3e")
		a.goAway(1, ErrCodeProtocol)
		b.expect("H1e")
		b.answer(1)
		if err := waitFor(t, done); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("lost", func(t *testing.T) {
		clients, servers := pairs(t, 2)
		tr, _ := pingTransport(t, resendPolicy(t, legacy, nil), clients...)
		a, b := newPeer(t, servers[0]), newPeer(t, servers[1])
		exchange(t, tr, a, false, "H1e")
		done := fetch(tr, request(context.Background(), false))
		a.expect("H3e")
		a.hangUp(false)
		if err := waitFor(t, done); err != io.ErrUnexpectedEOF {
			t.Fatalf("got %v, want %v", err, io.ErrUnexpectedEOF)
		}
		b.quiet(200 * time.Millisecond)
	})
}

// TestARetryWaitsTheDelayTheProfileSays: a request refused goes again
// h2.retry_delay_ms later, or ends with its context while it waits.
func TestARetryWaitsTheDelayTheProfileSays(t *testing.T) {
	delayed := map[string]any{"h2.retry_delay_ms": 300}
	t.Run("waits", func(t *testing.T) {
		clients, servers := pairs(t, 1)
		tr, _ := pingTransport(t, resendPolicy(t, chromeRetries, delayed), clients...)
		p := newPeer(t, servers[0])
		done := fetch(tr, request(context.Background(), false))
		got := p.expect("H1e")
		p.refuse(1)
		again := p.expect("H3e")
		p.answer(3)
		if err := waitFor(t, done); err != nil {
			t.Fatal(err)
		}
		if waited := again[0].at.Sub(got[0].at); waited < 300*time.Millisecond || waited > 600*time.Millisecond {
			t.Errorf("went again %v later, want 300ms", waited)
		}
	})
	t.Run("cancelled", func(t *testing.T) {
		clients, servers := pairs(t, 1)
		tr, _ := pingTransport(t, resendPolicy(t, chromeRetries, delayed), clients...)
		p := newPeer(t, servers[0])
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		done := fetch(tr, request(ctx, false))
		p.expect("H1e")
		p.refuse(1)
		time.Sleep(100 * time.Millisecond)
		cancel()
		if err := waitFor(t, done); err != context.Canceled {
			t.Fatalf("got %v, want %v", err, context.Canceled)
		}
		p.quiet(400 * time.Millisecond)
	})
}

// TestTheCountsAreTheContextsWhenItCarriesThem: round trips made with one
// context count in its Resends: a second one refused has none left, until
// the count is reset.
func TestTheCountsAreTheContextsWhenItCarriesThem(t *testing.T) {
	clients, servers := pairs(t, 1)
	tr, _ := pingTransport(t, resendPolicy(t, chromeRetries, nil), clients...)
	p := newPeer(t, servers[0])
	counts := &http.Resends{}
	ctx := http.WithResends(context.Background(), counts)
	done := fetch(tr, request(ctx, false))
	p.expect("H1e")
	p.refuse(1)
	p.expect("H3e")
	p.refuse(3)
	p.expect("H5e")
	p.answer(5)
	if err := waitFor(t, done); err != nil {
		t.Fatal(err)
	}
	done = fetch(tr, request(ctx, false))
	p.expect("H7e")
	p.refuse(7)
	if err := waitFor(t, done); failureOf(err) != "refused-stream" {
		t.Fatalf("got %v, want the refusal", err)
	}
	counts.Reset()
	done = fetch(tr, request(ctx, false))
	p.expect("H9e")
	p.refuse(9)
	p.expect("H11e")
	p.answer(11)
	if err := waitFor(t, done); err != nil {
		t.Fatal(err)
	}
}

// TestAConnectionThatCanTakeNoMoreIsPassedOver: a request given connections
// that can take it no more goes again at once on the next, seven times at
// most.
func TestAConnectionThatCanTakeNoMoreIsPassedOver(t *testing.T) {
	for _, tc := range []struct {
		closed int
		err    error
	}{{1, nil}, {7, nil}, {8, errClientConnUnusable}} {
		t.Run(fmt.Sprint(tc.closed), func(t *testing.T) {
			clients, servers := pairs(t, tc.closed+1)
			pool := &listPool{}
			tr := &Transport{Behavior: resendPolicy(t, chromeRetries, nil), ConnPool: pool}
			var last *peer
			for i, c := range clients {
				p := newPeer(t, servers[i])
				cc, err := tr.NewClientConn(c)
				if err != nil {
					t.Fatal(err)
				}
				if i < tc.closed {
					cc.Close()
				}
				pool.conns = append(pool.conns, cc)
				last = p
			}
			done := fetch(tr, request(context.Background(), false))
			if tc.err == nil {
				last.expect("H1e")
				last.answer(1)
			}
			if err := waitFor(t, done); err != tc.err {
				t.Fatalf("got %v, want %v", err, tc.err)
			}
		})
	}
}

// TestFailureOf names each failure a request may go again for, and no other.
func TestFailureOf(t *testing.T) {
	for _, tc := range []struct {
		err  error
		want string
	}{
		{StreamError{Code: ErrCodeRefusedStream}, "refused-stream"},
		{StreamError{Code: ErrCodeProtocol}, ""},
		{goneAwayError{code: ErrCodeNo}, "goaway-no-error"},
		{goneAwayError{code: ErrCodeProtocol}, "goaway-error"},
		{lostConnError{err: io.EOF}, "lost"},
		{errPrefacePingFailed, "ping-failed"},
		{errClientConnUnusable, "unusable"},
		{io.EOF, ""},
	} {
		if got := failureOf(tc.err); got != tc.want {
			t.Errorf("failureOf(%v) = %q, want %q", tc.err, got, tc.want)
		}
	}
	if err := (lostConnError{err: io.EOF}); !errors.Is(err, io.EOF) || err.Error() != "EOF" || unlost(err) != io.EOF || unlost(io.ErrClosedPipe) != io.ErrClosedPipe {
		t.Error("a lost connection's error is not the connection's own")
	}
}

// TestIdempotent: GET, HEAD, OPTIONS and TRACE, and a request that says it is.
func TestIdempotent(t *testing.T) {
	for _, tc := range []struct {
		method, key string
		want        bool
	}{
		{"", "", true}, {"GET", "", true}, {"HEAD", "", true}, {"OPTIONS", "", true}, {"TRACE", "", true},
		{"POST", "", false}, {"PUT", "", false}, {"POST", "Idempotency-Key", true}, {"PATCH", "X-Idempotency-Key", true},
	} {
		req := &http.Request{Method: tc.method, Header: http.Header{}}
		if tc.key != "" {
			req.Header[tc.key] = []string{"k"}
		}
		if got := idempotent(req); got != tc.want {
			t.Errorf("%q %q: %v, want %v", tc.method, tc.key, got, tc.want)
		}
	}
}

// TestARequestThatCannotGoAgainFailsSayingWhy: a refused POST whose body
// cannot be read again fails, saying why; a request the pool gives no
// connection fails with the pool's error.
func TestARequestThatCannotGoAgainFailsSayingWhy(t *testing.T) {
	t.Run("body", func(t *testing.T) {
		clients, servers := pairs(t, 1)
		tr, _ := pingTransport(t, resendPolicy(t, chromeRetries, nil), clients...)
		p := newPeer(t, servers[0])
		req, _ := http.NewRequest("POST", "https://dummy.tld/", struct{ io.Reader }{strings.NewReader("x")})
		done := fetch(tr, req)
		p.expect("H1", "D1:x", "D1e:")
		p.refuse(1)
		if err := waitFor(t, done); err == nil || !strings.Contains(err.Error(), "define Request.GetBody") {
			t.Fatalf("got %v, want the body that cannot go again", err)
		}
	})
	t.Run("connection", func(t *testing.T) {
		tr := &Transport{Behavior: resendPolicy(t, chromeRetries, nil), ConnPool: &listPool{}}
		if _, err := tr.RoundTrip(request(context.Background(), false)); err == nil || err.Error() != "no more connections" {
			t.Fatalf("got %v, want the pool's", err)
		}
	})
}

// failingWrites is a connection whose writes fail as those to a reset one
// do, once armed, after letting through as many as it was told.
type failingWrites struct {
	net.Conn
	armed atomic.Bool
	after atomic.Int32
}

func (c *failingWrites) arm(after int32) {
	c.after.Store(after)
	c.armed.Store(true)
}

func (c *failingWrites) Write(p []byte) (int, error) {
	if c.armed.Load() && c.after.Add(-1) < 0 {
		return 0, &net.OpError{Op: "write", Net: "tcp", Err: syscall.EPIPE}
	}
	return c.Conn.Write(p)
}

// TestAWriteThatFindsTheConnectionLostSendsTheRequestAgain: the HEADERS of a
// request, or the DATA of its body, that a reused connection takes no more
// is a lost connection too: the request goes again on another.
func TestAWriteThatFindsTheConnectionLostSendsTheRequestAgain(t *testing.T) {
	for _, tc := range []struct {
		name  string
		post  bool
		after int32
	}{{"headers", false, 0}, {"body", true, 1}} {
		t.Run(tc.name, func(t *testing.T) {
			clients, servers := pairs(t, 2)
			failing := &failingWrites{Conn: clients[0]}
			tr, _ := pingTransport(t, resendPolicy(t, chromeRetries, nil), failing, clients[1])
			a, b := newPeer(t, servers[0]), newPeer(t, servers[1])
			exchange(t, tr, a, false, "H1e")
			failing.arm(tc.after)
			done := fetch(tr, request(context.Background(), tc.post))
			if tc.post {
				b.expect("H1", "D1e:x")
			} else {
				b.expect("H1e")
			}
			b.answer(1)
			if err := waitFor(t, done); err != nil {
				t.Fatal(err)
			}
		})
	}
}

// TestAClientConnSaysWhatItsConnectionSaid: a request sent straight through a
// ClientConn whose connection is lost under it fails with what the
// connection said, as one through the Transport does.
func TestAClientConnSaysWhatItsConnectionSaid(t *testing.T) {
	clients, servers := pairs(t, 1)
	tr := &Transport{Behavior: resendPolicy(t, chromeRetries, nil)}
	p := newPeer(t, servers[0])
	cc, err := tr.NewClientConn(clients[0])
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := cc.RoundTrip(request(context.Background(), false))
		done <- err
	}()
	p.expect("H1e")
	p.hangUp(false)
	// The peer's close is a FIN, or a reset where the client's last frames
	// were still unread on its side: either is said as it was, unwrapped.
	err = waitFor(t, done)
	_, raw := err.(*net.OpError)
	if err != io.ErrUnexpectedEOF && !(raw && errors.Is(err, syscall.ECONNRESET)) {
		t.Fatalf("got %T %v, want %v or the connection's reset", err, err, io.ErrUnexpectedEOF)
	}
}

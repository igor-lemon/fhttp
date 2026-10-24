// Copyright 2026 The fhttp Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package http_test

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	. "github.com/igor-lemon/fhttp"
	tls "github.com/igor-lemon/utls"
	"github.com/igor-lemon/utls/behavior"
)

// chromeResends is what Chrome 155 does over HTTP/1.1, as the shipped
// profiles say it: a request goes again at once, whatever its method, 50
// times at most, when the reused connection it went on was lost before any of
// its answer came, and after a 408 on a reused connection.
var chromeResends = map[string]any{
	"h2.retries":                    2,
	"h2.retry_errors":               []string{"refused-stream", "goaway-no-error"},
	"http.connection_error_retries": 50,
	"http.connection_error_methods": "any",
	"h1.request_timeout_resend":     true,
}

// resendTransport is a transport whose profile says base with the keys of
// change (nil taking one out); none at all for a nil base.
func resendTransport(t *testing.T, base, change map[string]any) *Transport {
	t.Helper()
	tr := &Transport{DisableCompression: true}
	t.Cleanup(tr.CloseIdleConnections)
	if base == nil {
		return tr
	}
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
	tr.TLSClientConfig = &tls.Config{Behavior: p}
	return tr
}

// h1Asked is a request an h1Server read: on which of its connections, the
// how-manyth on it, and what it said.
type h1Asked struct {
	conn, seq    int
	method, body string
}

// h1Server answers HTTP/1.1 over TCP as do says for each request it reads:
// "ok" 200 kept alive, "close" a FIN, "reset" a RST, "partial" a status line
// cut short and a FIN, "408" a 408 with Connection: close, the connection
// left for the client to close (closed tells when it did), "408-keep" one
// without, the connection kept alive, "408-empty" one with no body either.
type h1Server struct {
	t        *testing.T
	ln       net.Listener
	asked    chan h1Asked
	closed   chan int
	accepted atomic.Int32
	do       func(h1Asked) string
}

func newH1Server(t *testing.T, do func(h1Asked) string) *h1Server {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s := &h1Server{t: t, ln: ln, asked: make(chan h1Asked, 16), closed: make(chan int, 16), do: do}
	t.Cleanup(func() { ln.Close() })
	go s.accept()
	return s
}

func (s *h1Server) url() string { return "http://" + s.ln.Addr().String() + "/" }

func (s *h1Server) accept() {
	for n := 0; ; n++ {
		c, err := s.ln.Accept()
		if err != nil {
			return
		}
		s.accepted.Add(1)
		go s.serve(n, c)
	}
}

func (s *h1Server) serve(n int, c net.Conn) {
	defer c.Close()
	br := bufio.NewReader(c)
	for seq := 0; ; seq++ {
		req, err := ReadRequest(br)
		if err != nil {
			s.closed <- n
			return
		}
		body, _ := io.ReadAll(req.Body)
		a := h1Asked{conn: n, seq: seq, method: req.Method, body: string(body)}
		s.asked <- a
		switch s.do(a) {
		case "close":
			return
		case "reset":
			c.(*net.TCPConn).SetLinger(0)
			return
		case "partial":
			io.WriteString(c, "HTTP/1.1 200 OK\r\nContent-")
			return
		case "408":
			io.WriteString(c, "HTTP/1.1 408 Request Timeout\r\nConnection: close\r\nContent-Length: 3\r\n\r\n408")
			io.Copy(io.Discard, c)
			s.closed <- n
			return
		case "408-keep":
			io.WriteString(c, "HTTP/1.1 408 Request Timeout\r\nContent-Length: 3\r\n\r\n408")
			continue
		case "408-empty":
			io.WriteString(c, "HTTP/1.1 408 Request Timeout\r\nContent-Length: 0\r\n\r\n")
			continue
		}
		io.WriteString(c, "HTTP/1.1 200 OK\r\nContent-Length: 2\r\n\r\nok")
	}
}

// expect reads the next requests, which must be want.
func (s *h1Server) expect(want ...h1Asked) {
	s.t.Helper()
	for _, w := range want {
		select {
		case got := <-s.asked:
			if got != w {
				s.t.Fatalf("got %+v, want %+v", got, w)
			}
		case <-time.After(5 * time.Second):
			s.t.Fatalf("nothing asked for 5s, want %+v", w)
		}
	}
}

// quiet is a server asked nothing more for d.
func (s *h1Server) quiet(d time.Duration) {
	s.t.Helper()
	select {
	case got := <-s.asked:
		s.t.Fatalf("got %+v, want nothing more", got)
	case <-time.After(d):
	}
}

// get sends method (a body "x" with a POST) to s through tr with ctx, and
// reads its answer: its status, or the error.
func (s *h1Server) get(tr *Transport, ctx context.Context, method string) (int, error) {
	var body io.Reader
	if method == "POST" {
		body = strings.NewReader("x")
	}
	req, _ := NewRequestWithContext(ctx, method, s.url(), body)
	res, err := tr.RoundTrip(req)
	if err != nil {
		return 0, err
	}
	defer res.Body.Close()
	io.Copy(io.Discard, res.Body)
	return res.StatusCode, nil
}

// TestALostReusedConnectionSendsARequestAgainAsChromeDoesOverHTTP1: a request
// whose reused connection is closed or reset before any of its answer came
// goes again at once on another, a POST with its body, as Chrome's
// HttpNetworkTransaction sends one again; not one whose connection was new,
// nor one whose answer had begun, nor a POST when the profile names
// idempotent methods alone, nor any once the count is spent. Without the
// profile's keys, Go's rules: a GET goes again, a POST does not.
func TestALostReusedConnectionSendsARequestAgainAsChromeDoesOverHTTP1(t *testing.T) {
	spent := func() *Resends { r := &Resends{}; r.Lost(1); return r }
	for _, tc := range []struct {
		name    string
		base    map[string]any
		change  map[string]any
		fresh   bool
		act     string
		method  string
		resends *Resends
		again   bool
	}{
		{name: "closed", act: "close", method: "GET", again: true},
		{name: "reset", act: "reset", method: "GET", again: true},
		{name: "post", act: "close", method: "POST", again: true},
		{name: "post-reset", act: "reset", method: "POST", again: true},
		{name: "post-idempotent", change: map[string]any{"http.connection_error_methods": "idempotent"}, act: "close", method: "POST"},
		{name: "get-idempotent", change: map[string]any{"http.connection_error_methods": "idempotent"}, act: "close", method: "GET", again: true},
		{name: "fresh", fresh: true, act: "close", method: "GET"},
		{name: "partial", act: "partial", method: "GET"},
		{name: "none", change: map[string]any{"http.connection_error_retries": 0}, act: "close", method: "GET"},
		{name: "spent", change: map[string]any{"http.connection_error_retries": 1}, resends: spent(), act: "close", method: "GET"},
		{name: "go-get", base: map[string]any{}, act: "close", method: "GET", again: true},
		{name: "go-post", base: map[string]any{}, act: "close", method: "POST"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			base := tc.base
			if base == nil {
				base = chromeResends
			}
			seq := 1
			if tc.fresh {
				seq = 0
			}
			s := newH1Server(t, func(a h1Asked) string {
				if a.conn == 0 && a.seq == seq {
					return tc.act
				}
				return "ok"
			})
			tr := resendTransport(t, base, tc.change)
			if !tc.fresh {
				if _, err := s.get(tr, context.Background(), "GET"); err != nil {
					t.Fatal(err)
				}
				s.expect(h1Asked{0, 0, "GET", ""})
			}
			ctx := context.Background()
			if tc.resends != nil {
				ctx = WithResends(ctx, tc.resends)
			}
			body := ""
			if tc.method == "POST" {
				body = "x"
			}
			status, err := s.get(tr, ctx, tc.method)
			s.expect(h1Asked{0, seq, tc.method, body})
			if tc.again {
				s.expect(h1Asked{1, 0, tc.method, body})
			}
			if tc.again != (err == nil && status == 200) {
				t.Fatalf("got %d %v, want it sent again %v", status, err, tc.again)
			}
			if !tc.again && tc.act == "close" && err != io.EOF {
				t.Fatalf("got %v, want the connection's own %v", err, io.EOF)
			}
			s.quiet(100 * time.Millisecond)
		})
	}
}

// TestA408OnAReusedConnectionSendsTheRequestAgain: a 408 that comes on a
// reused connection is not the answer: the client closes the connection,
// unread, and sends the request again on another, a POST with its body, as
// Chrome does; a 408 on a new connection is the answer, and so is any when
// the profile does not say h1.request_timeout_resend.
func TestA408OnAReusedConnectionSendsTheRequestAgain(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change map[string]any
		fresh  bool
		twice  bool
		method string
		again  bool
	}{
		{name: "reused", method: "GET", again: true},
		{name: "post", method: "POST", again: true},
		{name: "fresh", fresh: true, method: "GET"},
		{name: "twice", twice: true, method: "GET"},
		{name: "unsaid", change: map[string]any{"h1.request_timeout_resend": nil}, method: "GET"},
		{name: "off", change: map[string]any{"h1.request_timeout_resend": false}, method: "GET"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			seq := 1
			if tc.fresh {
				seq = 0
			}
			s := newH1Server(t, func(a h1Asked) string {
				if a.conn == 0 && a.seq == seq || tc.twice && a.conn == 1 {
					return "408"
				}
				return "ok"
			})
			tr := resendTransport(t, chromeResends, tc.change)
			if !tc.fresh {
				if _, err := s.get(tr, context.Background(), "GET"); err != nil {
					t.Fatal(err)
				}
				s.expect(h1Asked{0, 0, "GET", ""})
			}
			body := ""
			if tc.method == "POST" {
				body = "x"
			}
			began := time.Now()
			status, err := s.get(tr, context.Background(), tc.method)
			if err != nil {
				t.Fatal(err)
			}
			s.expect(h1Asked{0, seq, tc.method, body})
			want := 408
			if tc.again || tc.twice {
				s.expect(h1Asked{1, 0, tc.method, body})
				if tc.again {
					want = 200
				}
				// The connection the 408 came on closed by the client, at once.
				select {
				case n := <-s.closed:
					if n != 0 || time.Since(began) > time.Second {
						t.Fatalf("connection %d closed after %v", n, time.Since(began))
					}
				case <-time.After(2 * time.Second):
					t.Fatal("the 408's connection was left open")
				}
			}
			if status != want {
				t.Fatalf("got %d, want %d", status, want)
			}
			s.quiet(100 * time.Millisecond)
		})
	}
}

// TestACancelledRequestIsNotSentAgain: a request whose context ended is not
// sent again for its lost connection.
func TestACancelledRequestIsNotSentAgain(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s := newH1Server(t, func(a h1Asked) string {
		if a.seq == 1 {
			cancel()
			time.Sleep(50 * time.Millisecond)
			return "close"
		}
		return "ok"
	})
	tr := resendTransport(t, chromeResends, nil)
	if _, err := s.get(tr, context.Background(), "GET"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.get(tr, ctx, "GET"); err == nil {
		t.Fatal("answered")
	}
	s.expect(h1Asked{0, 0, "GET", ""}, h1Asked{0, 1, "GET", ""})
	s.quiet(200 * time.Millisecond)
}

// TestResends counts each kind apart, to its limit, from none again once reset.
func TestResends(t *testing.T) {
	r := ResendsOf(context.Background())
	for i, want := range []bool{true, true, false} {
		if got := r.Unprocessed(2); got != want {
			t.Fatalf("unprocessed %d: %v, want %v", i, got, want)
		}
	}
	if !r.Lost(1) || r.Lost(1) {
		t.Fatal("the lost count is not its own")
	}
	r.Reset()
	if !r.Unprocessed(1) || !r.Lost(1) {
		t.Fatal("not reset")
	}
	if ResendsOf(WithResends(context.Background(), r)) != r {
		t.Fatal("a context's count is not its own")
	}
	if ResendsOf(WithResends(context.Background(), nil)) == nil {
		t.Fatal("no count for a context carrying none")
	}
}

// TestConnectionLost: closed or reset under a request, not a deadline, the
// client's own close, nor anything else.
func TestConnectionLost(t *testing.T) {
	reset := &net.OpError{Op: "read", Err: fmt.Errorf("connection reset by peer")}
	for _, tc := range []struct {
		err  error
		want bool
	}{
		{io.EOF, true},
		{io.ErrUnexpectedEOF, true},
		{fmt.Errorf("wrapped: %w", io.EOF), true},
		{reset, true},
		{&net.OpError{Op: "write", Err: fmt.Errorf("broken pipe")}, true},
		{&net.OpError{Op: "dial", Err: fmt.Errorf("refused")}, false},
		{&net.OpError{Op: "read", Err: timeoutError{}}, false},
		{&net.OpError{Op: "read", Err: net.ErrClosed}, false},
		{fmt.Errorf("tls: bad record MAC"), false},
		{nil, false},
	} {
		if got := ConnectionLost(tc.err); got != tc.want {
			t.Errorf("ConnectionLost(%v) = %v, want %v", tc.err, got, tc.want)
		}
	}
}

type timeoutError struct{}

func (timeoutError) Error() string { return "i/o timeout" }
func (timeoutError) Timeout() bool { return true }

// TestA408WhoseRequestCannotGoAgainFails: a 408 on a reused connection to a
// request whose body cannot be read again is no reason to send it again: the
// request fails, saying why.
func TestA408WhoseRequestCannotGoAgainFails(t *testing.T) {
	s := newH1Server(t, func(a h1Asked) string {
		if a.seq == 1 {
			return "408"
		}
		return "ok"
	})
	tr := resendTransport(t, chromeResends, nil)
	if _, err := s.get(tr, context.Background(), "GET"); err != nil {
		t.Fatal(err)
	}
	req, _ := NewRequest("POST", s.url(), struct{ io.Reader }{strings.NewReader("x")})
	if _, err := tr.RoundTrip(req); err == nil || !strings.Contains(err.Error(), "cannot rewind body") {
		t.Fatalf("got %v, want the body that cannot go again", err)
	}
}

// TestA408KeptAliveIsClosedAndTheRequestGoesAgain: a 408 on a reused
// connection that the server keeps alive is not the answer either, with a
// body or none: the client closes the connection, as Chrome does whatever
// the server says, and the request goes again on another.
func TestA408KeptAliveIsClosedAndTheRequestGoesAgain(t *testing.T) {
	for _, act := range []string{"408-keep", "408-empty"} {
		t.Run(act, func(t *testing.T) {
			s := newH1Server(t, func(a h1Asked) string {
				if a.conn == 0 && a.seq == 1 {
					return act
				}
				return "ok"
			})
			tr := resendTransport(t, chromeResends, nil)
			if _, err := s.get(tr, context.Background(), "GET"); err != nil {
				t.Fatal(err)
			}
			if status, err := s.get(tr, context.Background(), "GET"); err != nil || status != 200 {
				t.Fatalf("got %d %v", status, err)
			}
			s.expect(h1Asked{0, 0, "GET", ""}, h1Asked{0, 1, "GET", ""}, h1Asked{1, 0, "GET", ""})
			select {
			case n := <-s.closed:
				if n != 0 {
					t.Fatalf("connection %d closed, want the 408's", n)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("the 408's connection was left open")
			}
		})
	}
}

// failingBody reads "x", then fails as a body cut short would.
type failingBody struct{ read bool }

func (b *failingBody) Read(p []byte) (int, error) {
	if b.read {
		return 0, io.ErrUnexpectedEOF
	}
	b.read = true
	return copy(p, "x"), nil
}

// TestABodyThatFailsToReadIsNoLostConnection: a request whose own body fails
// to read, on a reused connection, is not sent again, though it could be.
func TestABodyThatFailsToReadIsNoLostConnection(t *testing.T) {
	s := newH1Server(t, func(h1Asked) string { return "ok" })
	tr := resendTransport(t, chromeResends, nil)
	if _, err := s.get(tr, context.Background(), "GET"); err != nil {
		t.Fatal(err)
	}
	s.expect(h1Asked{0, 0, "GET", ""})
	req, _ := NewRequest("POST", s.url(), &failingBody{})
	req.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(&failingBody{}), nil }
	if _, err := tr.RoundTrip(req); err == nil || !strings.Contains(err.Error(), io.ErrUnexpectedEOF.Error()) {
		t.Fatalf("got %v, want the body's own failure", err)
	}
	// Sent again, it would have gone on a connection of its own.
	time.Sleep(200 * time.Millisecond)
	if n := s.accepted.Load(); n != 1 {
		t.Fatalf("%d connections, want the one: the request went again", n)
	}
}

// Copyright 2026 The fhttp Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package http2

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	http "github.com/igor-lemon/fhttp"
	tls "github.com/igor-lemon/utls"
)

// heldConn is the client's end of a connection whose peer stops taking what
// it is sent: once limit bytes have gone, a write is held until the test lets
// writes through again, the write deadline passes or the connection closes.
type heldConn struct {
	net.Conn
	limit int

	mu       sync.Mutex
	written  int
	deadline time.Time
	released bool

	held      chan struct{} // closed when a write is first held
	release   chan struct{} // closed to let writes through
	closed    chan struct{}
	heldOnce  sync.Once
	closeOnce sync.Once
}

func newHeldConn(t *testing.T, c net.Conn, limit int) *heldConn {
	h := &heldConn{Conn: c, limit: limit, held: make(chan struct{}), release: make(chan struct{}), closed: make(chan struct{})}
	t.Cleanup(func() { h.Close() })
	return h
}

func (h *heldConn) Write(p []byte) (int, error) {
	h.mu.Lock()
	if h.released || h.written+len(p) <= h.limit {
		h.written += len(p)
		h.mu.Unlock()
		return h.Conn.Write(p)
	}
	h.mu.Unlock()
	h.heldOnce.Do(func() { close(h.held) })
	for {
		h.mu.Lock()
		deadline, released := h.deadline, h.released
		h.mu.Unlock()
		if released {
			return h.Conn.Write(p)
		}
		if !deadline.IsZero() && !time.Now().Before(deadline) {
			return 0, os.ErrDeadlineExceeded
		}
		select {
		case <-h.closed:
			return 0, net.ErrClosed
		case <-h.release:
		case <-time.After(5 * time.Millisecond):
		}
	}
}

func (h *heldConn) letThrough() {
	h.mu.Lock()
	h.released = true
	h.mu.Unlock()
	close(h.release)
}

func (h *heldConn) SetWriteDeadline(t time.Time) error {
	h.mu.Lock()
	h.deadline = t
	h.mu.Unlock()
	return h.Conn.SetWriteDeadline(t)
}

func (h *heldConn) Close() error {
	h.closeOnce.Do(func() { close(h.closed) })
	return h.Conn.Close()
}

// within runs f and fails the test with what f says, or when f takes longer
// than d. f says nothing to the test itself: it may outlive it.
func within(t *testing.T, d time.Duration, what string, f func() error) {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- f() }()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("%s: %v", what, err)
		}
	case <-time.After(d):
		t.Fatalf("%s took longer than %v", what, d)
	}
}

// uploading sends a POST of size bytes on cc, in the background, once the
// connection has read nothing for longer than the policy's 100 ms, and waits
// until one of its writes is held.
func uploading(t *testing.T, cc *ClientConn, conn *heldConn, size int) chan error {
	t.Helper()
	req, _ := http.NewRequest("POST", "https://dummy.tld/", bytes.NewReader(make([]byte, size)))
	done := make(chan error, 1)
	go func() {
		_, err := cc.RoundTrip(req)
		done <- err
	}()
	select {
	case <-conn.held:
	case err := <-done:
		t.Fatalf("the upload ended before any of its writes was held: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("no write of the upload was held")
	}
	return done
}

// sending is a GET of ctx sent on cc in the background.
func sending(ctx context.Context, cc *ClientConn, path string) chan error {
	req, _ := http.NewRequestWithContext(ctx, "GET", "https://dummy.tld"+path, nil)
	done := make(chan error, 1)
	go func() {
		res, err := cc.RoundTrip(req)
		if err == nil {
			res.Body.Close()
		}
		done <- err
	}()
	return done
}

// TestARequestBehindAWriteThePeerDoesNotTakeHoldsTheConnectionUpNoMore: a
// request whose HEADERS wait for a write the peer does not take holds none of
// the connection's state while it waits, so the connection still says at once
// whether it takes a request, another request waits for its turn no longer
// than it is wanted, and the connection's unanswered PING gives it up: every
// request on it ends within the time the PING was given, as one that went
// before the PING does, with the PING's failure, which sends it again.
func TestARequestBehindAWriteThePeerDoesNotTakeHoldsTheConnectionUpNoMore(t *testing.T) {
	clients, servers := pairs(t, 1)
	p := newPeer(t, servers[0]) // reads all, answers no PING
	conn := newHeldConn(t, clients[0], 32<<10)
	tr := &Transport{Behavior: pingPolicy(t, 100, 300, map[string]any{"h2.preface_ping_failed_goaway_code": 1})}
	cc, err := tr.NewClientConn(conn)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(200 * time.Millisecond)
	began := time.Now()
	upload := uploading(t, cc, conn, 1<<20)
	behind := sending(context.Background(), cc, "/behind")
	time.Sleep(50 * time.Millisecond) // the GET waits for the upload's write

	within(t, 100*time.Millisecond, "asking whether the connection takes a request", func() error {
		if !cc.CanTakeNewRequest() {
			return errors.New("it takes none before it is given up")
		}
		return nil
	})
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	unwanted := sending(ctx, cc, "/unwanted")
	select {
	case err := <-unwanted:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("a request no longer wanted while it waited for its turn got %v, want %v", err, context.DeadlineExceeded)
		}
	case <-time.After(time.Second):
		t.Fatal("a request waited for its turn longer than it was wanted")
	}

	for _, done := range []chan error{upload, behind} {
		select {
		case err := <-done:
			if err != errPrefacePingFailed {
				t.Errorf("got %v, want %v", err, errPrefacePingFailed)
			}
		case <-time.After(3 * time.Second):
			t.Fatal("a request did not end with its connection, given up for its PING")
		}
	}
	if took := time.Since(began); took > 2*time.Second {
		t.Errorf("the requests ended %v after the upload began, want about 700ms: the PING's 300 and the held write's 300", took)
	}
	if cc.CanTakeNewRequest() {
		t.Error("a connection given up still takes requests")
	}
	// Neither the GET's nor the unwanted request's HEADERS went: the peer saw
	// the upload's alone.
	for s := range p.seen {
		if s.kind == FrameHeaders && s.stream != 1 {
			t.Errorf("the peer was sent %v", s)
		}
	}
}

// TestARequestNoLongerWantedSendsNoHeadersWhenItsTurnComes: a request whose
// context ends while its HEADERS wait for the connection's writes sends no
// HEADERS when the writes go again, and says why it ended, as Chrome takes a
// frame not yet written out of its queue.
func TestARequestNoLongerWantedSendsNoHeadersWhenItsTurnComes(t *testing.T) {
	clients, servers := pairs(t, 1)
	p := newPeer(t, servers[0])
	conn := newHeldConn(t, clients[0], 1<<10)
	tr := &Transport{}
	cc, err := tr.NewClientConn(conn)
	if err != nil {
		t.Fatal(err)
	}
	upload := uploading(t, cc, conn, 16<<10)
	ctx, cancel := context.WithCancel(context.Background())
	behind := sending(ctx, cc, "/behind")
	time.Sleep(50 * time.Millisecond)
	cancel()
	time.Sleep(50 * time.Millisecond)
	conn.letThrough()
	select {
	case err := <-behind:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("got %v, want %v", err, context.Canceled)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the request did not end")
	}
	// The upload's HEADERS and body, and no HEADERS of the request no longer
	// wanted.
	for ended := false; !ended; {
		select {
		case s := <-p.seen:
			switch {
			case s.kind == FrameHeaders && s.stream != 1:
				t.Errorf("the peer was sent %v", s)
			case s.kind == FrameData && s.flags.Has(FlagDataEndStream):
				ended = true
			}
		case <-time.After(5 * time.Second):
			t.Fatal("the upload's body did not end")
		}
	}
	p.answer(1)
	if err := waitFor(t, upload); err != nil {
		t.Fatal(err)
	}
	cc.Close()
	for s := range p.seen {
		if s.kind == FrameHeaders {
			t.Errorf("the peer was sent %v", s)
		}
	}
}

// TestAWriteThePeerDoesNotTakeHoldsUpNoOtherHostsRequest: a connection whose
// writes are held, and a request waiting for them on it, hold up neither the
// pool nor a request to another host, which asks the held connection whether
// it may go on it (Transport.Coalesce, as tls-client sets it) and dials its
// own; a request to the held host waits for its turn no longer than it is
// wanted. No PING bounds the held write here.
func TestAWriteThePeerDoesNotTakeHoldsUpNoOtherHostsRequest(t *testing.T) {
	clients, servers := pairs(t, 2)
	newPeer(t, servers[0])
	other := newPeer(t, servers[1])
	held := newHeldConn(t, clients[0], 32<<10)
	var mu sync.Mutex
	conns := []net.Conn{held, clients[1]}
	tr := &Transport{
		TLSClientConfig: tlsConfigInsecure,
		DialTLS: func(string, string, *tls.Config) (net.Conn, error) {
			mu.Lock()
			defer mu.Unlock()
			if len(conns) == 0 {
				return nil, errors.New("no more connections")
			}
			c := conns[0]
			conns = conns[1:]
			return c, nil
		},
		Coalesce: func(*http.Request, string, []net.Conn) net.Conn { return nil },
	}
	t.Cleanup(tr.CloseIdleConnections)
	// Closed first: the held write ends, whatever waits for it with it.
	t.Cleanup(func() { held.Close() })
	req, _ := http.NewRequest("POST", "https://a.tld/", bytes.NewReader(make([]byte, 1<<20)))
	go tr.RoundTrip(req)
	select {
	case <-held.held:
	case <-time.After(5 * time.Second):
		t.Fatal("no write of the upload was held")
	}
	behind, _ := http.NewRequest("GET", "https://a.tld/behind", nil)
	go tr.RoundTrip(behind)
	time.Sleep(50 * time.Millisecond)

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	unwanted, _ := http.NewRequestWithContext(ctx, "GET", "https://a.tld/unwanted", nil)
	within(t, time.Second, "a request to the held host no longer wanted", func() error {
		if _, err := tr.RoundTrip(unwanted); !errors.Is(err, context.DeadlineExceeded) {
			return fmt.Errorf("got %v, want %v", err, context.DeadlineExceeded)
		}
		return nil
	})

	go func() {
		for s := range other.seen {
			if s.kind == FrameHeaders {
				other.answer(s.stream)
			}
		}
	}()
	elsewhere, _ := http.NewRequest("GET", "https://b.tld/", nil)
	within(t, time.Second, "a request to another host", func() error {
		res, err := tr.RoundTrip(elsewhere)
		if err != nil {
			return err
		}
		return res.Body.Close()
	})
}

// TestManyRequestsAtOnceGoOutInOrderOnOneConnection: requests sent at once on
// one connection, each with fields of its own and half of them with a body
// and trailers, reach a server whose decoder holds every header block to the
// order they were encoded in: each block is written before the next is
// encoded, its stream's ID given in the order the blocks go.
func TestManyRequestsAtOnceGoOutInOrderOnOneConnection(t *testing.T) {
	var mu sync.Mutex
	conns := 0
	st := newServerTester(t, func(w http.ResponseWriter, r *http.Request) {
		n := r.URL.Query().Get("n")
		if got := r.Header.Get("X-N"); got != n {
			http.Error(w, "X-N "+got, http.StatusBadRequest)
			return
		}
		if r.Method == "POST" {
			body, err := io.ReadAll(r.Body)
			if err != nil || string(body) != "body "+n {
				http.Error(w, fmt.Sprintf("body %q %v", body, err), http.StatusBadRequest)
				return
			}
			if got := r.Trailer.Get("X-Trail"); got != n {
				http.Error(w, "X-Trail "+got, http.StatusBadRequest)
				return
			}
		}
		w.WriteHeader(http.StatusNoContent)
	}, optOnlyServer, func(_ net.Conn, s http.ConnState) {
		if s == http.StateNew {
			mu.Lock()
			conns++
			mu.Unlock()
		}
	})
	defer st.Close()
	tr := &Transport{TLSClientConfig: tlsConfigInsecure}
	defer tr.CloseIdleConnections()
	const requests = 64
	errs := make(chan error, requests)
	for i := range requests {
		go func() {
			n := fmt.Sprint(i)
			method, body := "GET", io.Reader(nil)
			if i%2 == 1 {
				method, body = "POST", strings.NewReader("body "+n)
			}
			req, _ := http.NewRequest(method, st.ts.URL+"/?n="+n, body)
			req.Header.Set("X-N", n)
			req.Header.Set("X-Same", "the same in every request, indexed after the first")
			if body != nil {
				req.Trailer = http.Header{"X-Trail": {n}}
			}
			res, err := tr.RoundTrip(req)
			if err == nil {
				if res.StatusCode != http.StatusNoContent {
					msg, _ := io.ReadAll(res.Body)
					err = fmt.Errorf("request %d: %s: %s", i, res.Status, msg)
				}
				res.Body.Close()
			}
			errs <- err
		}()
	}
	for range requests {
		select {
		case err := <-errs:
			if err != nil {
				t.Error(err)
			}
		case <-time.After(10 * time.Second):
			t.Fatal("the requests did not end")
		}
	}
	if conns != 1 {
		t.Errorf("the requests went on %d connections, want 1", conns)
	}
}

// TestARequestWaitingForAStreamLeavesTheTurnToTheTrailersThatEndOne: a
// request that waits for a stream of a connection whose streams are all taken
// waits without the turn to write a header block, which the trailers of the
// stream it waits for take to end it; one no longer wanted while it waits
// says so.
func TestARequestWaitingForAStreamLeavesTheTurnToTheTrailersThatEndOne(t *testing.T) {
	st := newServerTester(t, func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		w.Header().Set("X-Trail", r.Trailer.Get("X-Trail"))
		w.WriteHeader(http.StatusNoContent)
	}, optOnlyServer, func(s *Server) { s.MaxConcurrentStreams = 1 })
	defer st.Close()
	tr := &Transport{TLSClientConfig: tlsConfigInsecure, StrictMaxConcurrentStreams: true}
	defer tr.CloseIdleConnections()

	warm, _ := http.NewRequest("GET", st.ts.URL, nil)
	res, err := tr.RoundTrip(warm) // the server's SETTINGS are read with its answer
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()

	body, finish := io.Pipe()
	upload, _ := http.NewRequest("POST", st.ts.URL, body)
	upload.Trailer = http.Header{"X-Trail": {"the end"}}
	uploaded := make(chan *http.Response, 1)
	go func() {
		res, err := tr.RoundTrip(upload)
		if err != nil {
			t.Error(err)
		}
		uploaded <- res
	}()
	if _, err := finish.Write([]byte("part")); err != nil { // its HEADERS went, its stream is the one
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	unwanted, _ := http.NewRequestWithContext(ctx, "GET", st.ts.URL, nil)
	if _, err := tr.RoundTrip(unwanted); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("a request no longer wanted while it waited for a stream got %v, want %v", err, context.DeadlineExceeded)
	}

	waiting := make(chan error, 1)
	go func() {
		req, _ := http.NewRequest("GET", st.ts.URL, nil)
		res, err := tr.RoundTrip(req)
		if err == nil {
			res.Body.Close()
		}
		waiting <- err
	}()
	time.Sleep(50 * time.Millisecond) // it waits for the stream
	finish.Close()
	select {
	case res := <-uploaded:
		if res != nil {
			if got := res.Header.Get("X-Trail"); got != "the end" {
				t.Errorf("the server read the trailer %q, want %q", got, "the end")
			}
			res.Body.Close()
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the upload's trailers never went: the request waiting for its stream held the turn")
	}
	if err := waitFor(t, waiting); err != nil {
		t.Fatal(err)
	}
}

// TestAHeaderBlockThatCannotBeEncodedGivesTheTurnBack: a request whose
// fields cannot be encoded fails, and the connection's next request goes.
func TestAHeaderBlockThatCannotBeEncodedGivesTheTurnBack(t *testing.T) {
	st := newServerTester(t, func(w http.ResponseWriter, r *http.Request) {}, optOnlyServer)
	defer st.Close()
	tr := &Transport{TLSClientConfig: tlsConfigInsecure}
	defer tr.CloseIdleConnections()
	cc, err := tr.dialClientConn(context.Background(), st.ts.Listener.Addr().String(), false)
	if err != nil {
		t.Fatal(err)
	}
	for _, value := range []string{"bad\x00value", "fine"} {
		req, _ := http.NewRequest("GET", st.ts.URL, nil)
		req.Header.Set("X-Value", value)
		within(t, 5*time.Second, "a request after one that could not be encoded", func() error {
			res, err := cc.RoundTrip(req)
			if value == "fine" {
				if err == nil {
					res.Body.Close()
				}
				return err
			}
			if err == nil || !strings.Contains(err.Error(), "invalid HTTP header value") {
				return fmt.Errorf("got %v, want the field's", err)
			}
			return nil
		})
	}
}

// TestSettingsWaitForAHeaderBlockEncodedBeforeThem: the peer's SETTINGS are
// taken once the header block encoded before they came is written, as the
// block was encoded for the table the peer had before them.
func TestSettingsWaitForAHeaderBlockEncodedBeforeThem(t *testing.T) {
	clients, servers := pairs(t, 1)
	p := newPeer(t, servers[0])
	cc, err := (&Transport{}).NewClientConn(clients[0])
	if err != nil {
		t.Fatal(err)
	}
	defer cc.Close()
	table := func() uint32 {
		cc.mu.Lock()
		defer cc.mu.Unlock()
		return cc.hencMaxTableSize
	}
	req, _ := http.NewRequest("GET", "https://dummy.tld/", nil)
	if err := cc.takeHeaderTurn(req); err != nil {
		t.Fatal(err)
	}
	cc.mu.Lock()
	cc.writingHeaders = true
	cc.mu.Unlock()
	was := table()
	p.write(func() error { return p.fr.WriteSettings(Setting{ID: SettingHeaderTableSize, Val: 0}) })
	time.Sleep(100 * time.Millisecond)
	if got := table(); got != was {
		t.Fatalf("the encoder took a table of %d before the block encoded for %d was written", got, was)
	}
	cc.headersWritten()
	for began := time.Now(); table() != 0; time.Sleep(5 * time.Millisecond) {
		if time.Since(began) > 5*time.Second {
			t.Fatalf("the encoder kept a table of %d once the block was written, want 0", table())
		}
	}
}

// TestWhatEndsARequestBeforeItsHeadersGo: the turn is waited for as long as
// the request is wanted, by its context or its Cancel; and what ended a
// stream, or its request, keeps its HEADERS from going.
func TestWhatEndsARequestBeforeItsHeadersGo(t *testing.T) {
	cancelled := make(chan struct{})
	close(cancelled)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	byCancel, _ := http.NewRequest("GET", "https://dummy.tld/", nil)
	byCancel.Cancel = cancelled
	byContext, _ := http.NewRequestWithContext(ctx, "GET", "https://dummy.tld/", nil)
	wanted, _ := http.NewRequest("GET", "https://dummy.tld/", nil)

	cc := &ClientConn{}
	if err := cc.takeHeaderTurn(wanted); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		req  *http.Request
		want error
	}{{byCancel, errRequestCanceled}, {byContext, context.Canceled}} {
		if err := cc.takeHeaderTurn(c.req); err != c.want {
			t.Errorf("waiting for a turn taken: got %v, want %v", err, c.want)
		}
	}
	cc.giveHeaderTurn()

	ended := errors.New("the connection is given up")
	for _, c := range []struct {
		req  *http.Request
		resc error
		want error
	}{
		{wanted, nil, nil},
		{wanted, ended, ended},
		{byCancel, nil, errRequestCanceled},
		{byContext, nil, context.Canceled},
		{byContext, ended, ended},
	} {
		cs := &clientStream{req: c.req, resc: make(chan resAndError, 1)}
		if c.resc != nil {
			cs.resc <- resAndError{err: c.resc}
		}
		if err := cs.endedUnwritten(); err != c.want {
			t.Errorf("endedUnwritten of a stream ended by %v: got %v, want %v", c.resc, err, c.want)
		}
	}
}

// TestAPingFailedEndsARequestWhoseOwnHeadersAreHeld: a request whose own
// HEADERS the peer does not take, on a connection given up for its PING,
// ends within the time the PING was given with the PING's failure, not with
// its write cut short.
func TestAPingFailedEndsARequestWhoseOwnHeadersAreHeld(t *testing.T) {
	clients, servers := pairs(t, 1)
	p := newPeer(t, servers[0])
	conn := newHeldConn(t, clients[0], 1<<20)
	tr := &Transport{Behavior: pingPolicy(t, 100, 300, map[string]any{"h2.preface_ping_failed_goaway_code": 1})}
	cc, err := tr.NewClientConn(conn)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(200 * time.Millisecond)
	first := sending(context.Background(), cc, "/first")
	p.expect("H1e", "P1") // and the PING's check begins
	conn.mu.Lock()
	conn.limit = conn.written // what goes next is held
	conn.mu.Unlock()
	second := sending(context.Background(), cc, "/second")
	for _, done := range []chan error{first, second} {
		select {
		case err := <-done:
			if err != errPrefacePingFailed {
				t.Errorf("got %v, want %v", err, errPrefacePingFailed)
			}
		case <-time.After(3 * time.Second):
			t.Fatal("a request did not end with its connection, given up for its PING")
		}
	}
}

// TestAPingFailedGivesUpAConnectionWhoseReaderWaitsForAHeldWrite: the
// connection's reader, which waits to acknowledge the peer's SETTINGS behind a
// write the peer does not take, holds up neither the giving up of the
// connection for its unanswered PING nor the requests on it.
func TestAPingFailedGivesUpAConnectionWhoseReaderWaitsForAHeldWrite(t *testing.T) {
	clients, servers := pairs(t, 1)
	p := newPeer(t, servers[0])
	conn := newHeldConn(t, clients[0], 32<<10)
	tr := &Transport{Behavior: pingPolicy(t, 100, 300, map[string]any{"h2.preface_ping_failed_goaway_code": 1})}
	cc, err := tr.NewClientConn(conn)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(200 * time.Millisecond)
	upload := uploading(t, cc, conn, 1<<20)
	p.write(func() error { return p.fr.WriteSettings(Setting{ID: SettingMaxConcurrentStreams, Val: 100}) })
	select {
	case err := <-upload:
		if err != errPrefacePingFailed {
			t.Errorf("got %v, want %v", err, errPrefacePingFailed)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the upload did not end with its connection, given up for its PING")
	}
}

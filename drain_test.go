// Copyright 2026 The fhttp Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package http_test

import (
	"context"
	"io"
	"net"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	. "github.com/igor-lemon/fhttp"
	"github.com/igor-lemon/fhttp/httptest"
)

// drainServer answers /?n=N&delay=MS[&parts=P&gap=MS][&chunked][&trailer][&close]
// with its head at once and a body of N bytes delay ms later, in P parts gap
// ms apart, and says when each connection went idle or closed, by the
// client's address.
type drainServer struct {
	*httptest.Server
	mu     sync.Mutex
	opened []string // the clients' addresses, in the order they came
	states chan connState
}

type connState struct {
	addr  string
	state ConnState
	at    time.Time
}

func newDrainServer(t *testing.T) *drainServer {
	t.Helper()
	s := &drainServer{states: make(chan connState, 64)}
	s.Server = httptest.NewUnstartedServer(HandlerFunc(func(w ResponseWriter, r *Request) {
		n, _ := strconv.Atoi(r.URL.Query().Get("n"))
		delay, _ := strconv.Atoi(r.URL.Query().Get("delay"))
		if _, ok := r.URL.Query()["close"]; ok {
			w.Header().Set("Connection", "close")
		}
		if _, ok := r.URL.Query()["trailer"]; ok {
			w.Header().Set("Trailer", "X-T")
		}
		if _, ok := r.URL.Query()["chunked"]; !ok {
			w.Header().Set("Content-Length", strconv.Itoa(n))
		}
		w.WriteHeader(200)
		if delay > 0 {
			w.(Flusher).Flush()
			select {
			case <-time.After(time.Duration(delay) * time.Millisecond):
			case <-r.Context().Done():
				return
			}
		}
		parts, _ := strconv.Atoi(r.URL.Query().Get("parts"))
		gap, _ := strconv.Atoi(r.URL.Query().Get("gap"))
		for i := 1; i < parts; i++ {
			io.WriteString(w, strings.Repeat("x", n/parts))
			w.(Flusher).Flush()
			select {
			case <-time.After(time.Duration(gap) * time.Millisecond):
			case <-r.Context().Done():
				return
			}
		}
		io.WriteString(w, strings.Repeat("x", n-max(parts-1, 0)*(n/max(parts, 1))))
		if _, ok := r.URL.Query()["trailer"]; ok {
			w.Header().Set("X-T", "1")
		}
	}))
	s.Config.ConnState = func(c net.Conn, state ConnState) {
		if state == StateNew {
			s.mu.Lock()
			s.opened = append(s.opened, c.RemoteAddr().String())
			s.mu.Unlock()
		}
		if state == StateIdle || state == StateClosed {
			s.states <- connState{c.RemoteAddr().String(), state, time.Now()}
		}
	}
	s.Start()
	t.Cleanup(s.Close)
	return s
}

func (s *drainServer) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.opened)
}

// closed is when the n-th connection closed, within d, or the zero time.
func (s *drainServer) closed(n int, d time.Duration) time.Time {
	s.mu.Lock()
	addr := s.opened[n]
	s.mu.Unlock()
	deadline := time.After(d)
	for {
		select {
		case st := <-s.states:
			if st.addr == addr && st.state == StateClosed {
				return st.at
			}
		case <-deadline:
			return time.Time{}
		}
	}
}

// next is the next connection state the server saw, within 3 s.
func (s *drainServer) next(t *testing.T) connState {
	t.Helper()
	select {
	case st := <-s.states:
		return st
	case <-time.After(3 * time.Second):
		t.Fatal("no connection went idle or closed")
	}
	return connState{}
}

// quiet says no connection went idle or closed within d.
func (s *drainServer) quiet(t *testing.T, d time.Duration) {
	t.Helper()
	select {
	case st := <-s.states:
		t.Fatalf("connection %s %v", st.addr, st.state)
	case <-time.After(d):
	}
}

func drainGet(t *testing.T, tr *Transport, ctx context.Context, url string) *Response {
	t.Helper()
	req, _ := NewRequestWithContext(ctx, "GET", url, nil)
	res, err := tr.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	return res
}

// TestABodyAllInTheBufferIsDrainedAtOnce: what is left of a body is all in the
// buffer: DrainBody reads it before it returns, so the next request goes on
// the same connection, plain and chunked, a trailer too.
func TestABodyAllInTheBufferIsDrainedAtOnce(t *testing.T) {
	for _, query := range []string{"n=100", "n=100&chunked", "n=100&chunked&trailer", "n=0&chunked"} {
		t.Run(query, func(t *testing.T) {
			s := newDrainServer(t)
			tr := &Transport{}
			defer tr.CloseIdleConnections()
			res := drainGet(t, tr, context.Background(), s.URL+"/?"+query)
			time.Sleep(50 * time.Millisecond) // the body is in the buffer
			s.next(t)                         // idle, its answer written
			DrainBody(context.Background(), res, 16384, time.Second, 0)
			if res.Body != NoBody {
				t.Fatal("the body was left")
			}
			io.Copy(io.Discard, drainGet(t, tr, context.Background(), s.URL+"/?n=1").Body)
			if n := s.count(); n != 1 {
				t.Fatalf("%d connections, want the one", n)
			}
		})
	}
}

// TestABodyStillComingIsDrainedInTheBackground: DrainBody returns at once,
// the next request goes on another connection, and the first, its body come
// within the time and the bytes, goes back to be used again; else it closes.
func TestABodyStillComingIsDrainedInTheBackground(t *testing.T) {
	for _, tc := range []struct {
		name  string
		query string
		kept  bool
	}{
		{"in time", "n=4096&delay=300", true},
		{"in time chunked", "n=4096&delay=300&chunked", true},
		{"at the limit", "n=16384&delay=300", true},
		{"past the limit", "n=16385&delay=300", false},
		{"past the time", "n=10&delay=1500", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newDrainServer(t)
			tr := &Transport{}
			defer tr.CloseIdleConnections()
			res := drainGet(t, tr, context.Background(), s.URL+"/?"+tc.query)
			began := time.Now()
			DrainBody(context.Background(), res, 16384, 800*time.Millisecond, 0)
			if took := time.Since(began); took > 100*time.Millisecond {
				t.Fatalf("DrainBody took %v", took)
			}
			io.Copy(io.Discard, drainGet(t, tr, context.Background(), s.URL+"/?n=1").Body)
			if n := s.count(); n != 2 {
				t.Fatalf("%d connections, want another", n)
			}
			at := s.closed(0, 1500*time.Millisecond)
			if tc.kept != at.IsZero() {
				t.Fatalf("the first closed %v after DrainBody (zero: not)", at.Sub(began))
			}
			if tc.name == "past the time" {
				if took := at.Sub(began); took < 700*time.Millisecond || took > 1300*time.Millisecond {
					t.Errorf("closed %v after, want the 800ms given", took)
				}
			}
			if tc.kept {
				// Kept, it carries the next request: no third.
				io.Copy(io.Discard, drainGet(t, tr, context.Background(), s.URL+"/?n=1").Body)
				io.Copy(io.Discard, drainGet(t, tr, context.Background(), s.URL+"/?n=1").Body)
				if n := s.count(); n != 2 {
					t.Fatalf("%d connections, want the two kept", n)
				}
			}
		})
	}
}

// TestADrainedBodyIsItsRequestsNoMore: the request's context ending after
// DrainBody does not end the drain, and Cancel does not either.
func TestADrainedBodyIsItsRequestsNoMore(t *testing.T) {
	for _, how := range []string{"context", "cancel"} {
		t.Run(how, func(t *testing.T) {
			s := newDrainServer(t)
			tr := &Transport{}
			defer tr.CloseIdleConnections()
			ctx, cancel := context.WithCancel(context.Background())
			cancelc := make(chan struct{})
			req, _ := NewRequestWithContext(ctx, "GET", s.URL+"/?n=100&delay=300", nil)
			if how == "cancel" {
				req.Cancel = cancelc
			}
			res, err := tr.RoundTrip(req)
			if err != nil {
				t.Fatal(err)
			}
			DrainBody(context.Background(), res, 16384, time.Second, 0)
			if how == "context" {
				cancel()
			} else {
				close(cancelc)
			}
			if at := s.closed(0, 800*time.Millisecond); !at.IsZero() {
				t.Fatal("the request ending closed the connection being drained")
			}
			cancel()
		})
	}
	// Without DrainBody, the context ending closes it, as it did.
	s := newDrainServer(t)
	tr := &Transport{}
	defer tr.CloseIdleConnections()
	ctx, cancel := context.WithCancel(context.Background())
	res := drainGet(t, tr, ctx, s.URL+"/?n=100&delay=300")
	cancel()
	if at := s.closed(0, time.Second); at.IsZero() {
		t.Fatal("the context ending did not close it")
	}
	res.Body.Close()
}

// TestAnAnswerClosingItsConnectionIsLetGoOfAtOnce: Connection: close leaves
// nothing to drain for.
func TestAnAnswerClosingItsConnectionIsLetGoOfAtOnce(t *testing.T) {
	s := newDrainServer(t)
	tr := &Transport{}
	defer tr.CloseIdleConnections()
	res := drainGet(t, tr, context.Background(), s.URL+"/?n=100&delay=1000&close")
	began := time.Now()
	DrainBody(context.Background(), res, 16384, 5*time.Second, 0)
	if at := s.closed(0, time.Second); at.IsZero() || at.Sub(began) > 300*time.Millisecond {
		t.Fatalf("closed %v after (zero: not)", at.Sub(began))
	}
}

// TestABodyNotTheTransportsIsDrainedInTheBackground: any other body is read
// the same way, in the background, and closed.
func TestABodyNotTheTransportsIsDrainedInTheBackground(t *testing.T) {
	r, w := io.Pipe()
	res := &Response{Body: r}
	DrainBody(context.Background(), res, 4, time.Second, 0)
	if _, err := w.Write([]byte("abc")); err != nil {
		t.Fatal(err)
	}
	w.Close()
	if res.Body != NoBody {
		t.Fatal("the body was left")
	}
	// The drain closed the pipe's reader: a write after says so.
	time.Sleep(50 * time.Millisecond)
	if _, err := w.Write([]byte("x")); err == nil {
		t.Fatal("the pipe was not closed")
	}
}

// TestWithNoTimeOnlyWhatCameIsRead: given no time, DrainBody reads a body
// whose rest is all in the buffer, within the limit, and keeps its
// connection; one still coming, or past the limit, it lets go of at once, the
// connection closed.
func TestWithNoTimeOnlyWhatCameIsRead(t *testing.T) {
	for _, tc := range []struct {
		query string
		limit int64
		kept  bool
	}{
		{"n=100", 1 << 62, true},
		{"n=100&chunked", 1 << 62, true},
		{"n=100", 100, true},
		{"n=100", 99, false},
		{"n=100&delay=1000", 1 << 62, false},
		{"n=100&delay=1000&chunked", 1 << 62, false},
	} {
		t.Run(tc.query+"/"+strconv.FormatInt(tc.limit, 10), func(t *testing.T) {
			s := newDrainServer(t)
			tr := &Transport{}
			defer tr.CloseIdleConnections()
			res := drainGet(t, tr, context.Background(), s.URL+"/?"+tc.query)
			if !strings.Contains(tc.query, "delay") {
				time.Sleep(50 * time.Millisecond) // the body is in the buffer
				s.next(t)                         // idle, its answer written
			}
			began := time.Now()
			DrainBody(context.Background(), res, tc.limit, 0, 0)
			if res.Body != NoBody || time.Since(began) > 100*time.Millisecond {
				t.Fatalf("the body was left, or DrainBody took %v", time.Since(began))
			}
			if !tc.kept {
				if at := s.closed(0, time.Second); at.IsZero() || at.Sub(began) > 300*time.Millisecond {
					t.Fatalf("closed %v after (zero: not)", at.Sub(began))
				}
				return
			}
			io.Copy(io.Discard, drainGet(t, tr, context.Background(), s.URL+"/?n=1").Body)
			if n := s.count(); n != 1 {
				t.Fatalf("%d connections, want the one kept", n)
			}
		})
	}
}

// TestAnAwaitedBodyIsLetGoOfAtItsFirstBytes: AwaitBody returns at once and
// waits, apart from the request, for the first of the body's bytes, however
// long they take when given no time: a body whose rest comes with them, or
// within the grace, keeps its connection, any other has it closed; the time
// running out closes it, and so does ctx ending.
func TestAnAwaitedBodyIsLetGoOfAtItsFirstBytes(t *testing.T) {
	for _, tc := range []struct {
		name    string
		query   string
		timeout time.Duration
		cancel  time.Duration // ctx ends this long after AwaitBody, if not 0
		grace   time.Duration
		kept    bool
		closed  time.Duration // else when the first connection closes, about
		ended   bool          // the request's own context ends after AwaitBody
	}{
		{"whole", "n=4096&delay=300", 0, 0, 0, true, 0, false},
		{"whole, its request ended", "n=4096&delay=300", 0, 0, 0, true, 0, true},
		{"whole with a trailer", "n=100&delay=300&chunked&trailer", 0, 0, 0, true, 0, false},
		// Its last chunk past what one read of the buffer holds.
		{"whole chunked, with no grace", "n=4096&delay=300&chunked", 0, 0, 0, false, 300 * time.Millisecond, false},
		{"whole chunked", "n=4096&delay=300&chunked", 0, 0, 100 * time.Millisecond, true, 0, false},
		{"whole and large", "n=20000&delay=300", 0, 0, 100 * time.Millisecond, true, 0, false},
		{"whole and late, given no time", "n=100&delay=1200", 0, 0, 0, true, 0, false},
		{"in time", "n=100&delay=300", time.Second, 0, 0, true, 0, false},
		{"in parts within the grace", "n=8192&delay=300&parts=2&gap=30", 0, 0, 300 * time.Millisecond, true, 0, false},
		{"in parts", "n=8192&delay=300&parts=2&gap=1000", 0, 0, 0, false, 300 * time.Millisecond, false},
		{"in parts past the grace", "n=8192&delay=300&parts=2&gap=1000", 0, 0, 200 * time.Millisecond, false, 500 * time.Millisecond, false},
		{"in parts chunked", "n=8192&delay=300&parts=2&gap=1000&chunked", 0, 0, 0, false, 300 * time.Millisecond, false},
		{"past the time", "n=100&delay=1200", 300 * time.Millisecond, 0, 0, false, 300 * time.Millisecond, false},
		{"ctx ended", "n=100&delay=1200", 0, 300 * time.Millisecond, 0, false, 300 * time.Millisecond, false},
		{"of a connection to be closed", "n=100&delay=300&close", 0, 0, 0, false, 300 * time.Millisecond, false},
		{"ending with its connection", "n=100&delay=300&chunked&close", 0, 0, 0, false, 300 * time.Millisecond, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newDrainServer(t)
			tr := &Transport{}
			defer tr.CloseIdleConnections()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			asked, end := context.WithCancel(context.Background())
			defer end()
			res := drainGet(t, tr, asked, s.URL+"/?"+tc.query)
			began := time.Now()
			AwaitBody(ctx, res, tc.timeout, tc.grace)
			if res.Body != NoBody || time.Since(began) > 100*time.Millisecond {
				t.Fatalf("the body was left, or AwaitBody took %v", time.Since(began))
			}
			if tc.ended {
				end()
			}
			if tc.cancel > 0 {
				time.AfterFunc(tc.cancel, cancel)
			}
			at := s.closed(0, 2500*time.Millisecond)
			if tc.kept {
				if !at.IsZero() {
					t.Fatalf("closed %v after", at.Sub(began))
				}
				// Kept, it carries the next request.
				io.Copy(io.Discard, drainGet(t, tr, context.Background(), s.URL+"/?n=1").Body)
				if n := s.count(); n != 1 {
					t.Fatalf("%d connections, want the one kept", n)
				}
				return
			}
			if took := at.Sub(began); at.IsZero() || took < tc.closed-100*time.Millisecond || took > tc.closed+400*time.Millisecond {
				t.Fatalf("closed %v after (zero: not), want about %v", took, tc.closed)
			}
		})
	}
}

// TestAnAwaitedBodyAllInTheBufferIsReadAtOnce: what came with the head is
// read before AwaitBody returns, its connection carrying the next request.
func TestAnAwaitedBodyAllInTheBufferIsReadAtOnce(t *testing.T) {
	s := newDrainServer(t)
	tr := &Transport{}
	defer tr.CloseIdleConnections()
	res := drainGet(t, tr, context.Background(), s.URL+"/?n=100&chunked")
	time.Sleep(50 * time.Millisecond) // the body is in the buffer
	s.next(t)                         // idle, its answer written
	AwaitBody(context.Background(), res, 0, 0)
	if n := tr.IdleConnCountForTesting("http", s.Listener.Addr().String()); n != 1 {
		t.Fatalf("%d idle connections once AwaitBody returned, want the one", n)
	}
	io.Copy(io.Discard, drainGet(t, tr, context.Background(), s.URL+"/?n=1").Body)
	if n := s.count(); n != 1 {
		t.Fatalf("%d connections, want the one", n)
	}
}

// TestADrainEndsWithItsContext: ctx ending closes a connection whose body is
// being drained, before the time is up.
func TestADrainEndsWithItsContext(t *testing.T) {
	s := newDrainServer(t)
	tr := &Transport{}
	defer tr.CloseIdleConnections()
	ctx, cancel := context.WithCancel(context.Background())
	res := drainGet(t, tr, context.Background(), s.URL+"/?n=100&delay=2000")
	began := time.Now()
	DrainBody(ctx, res, 16384, 5*time.Second, 0)
	time.AfterFunc(300*time.Millisecond, cancel)
	if at := s.closed(0, 2*time.Second); at.IsZero() || at.Sub(began) > time.Second {
		t.Fatalf("closed %v after (zero: not)", at.Sub(began))
	}
}

// TestADrainWaitsItsGraceForWhatComesJustAfter: DrainBody returns once the
// body is read, within its grace, its connection free for the next request,
// or once the grace is over, the body drained on in the background; given no
// time past the grace, a body whose rest did not come within it is let go of
// then, its connection closed.
func TestADrainWaitsItsGraceForWhatComesJustAfter(t *testing.T) {
	for _, tc := range []struct {
		name           string
		query          string
		limit          int64
		timeout, grace time.Duration
		returns        time.Duration // about when DrainBody returns
		same           bool          // the next request goes on the same connection
		closed         time.Duration // when the first connection closes, about; 0 for not
	}{
		{"within the grace", "n=4096&delay=30", 16384, time.Second, 300 * time.Millisecond, 30 * time.Millisecond, true, 0},
		{"with no grace", "n=4096&delay=30", 16384, time.Second, 0, 0, false, 0},
		{"large, with its head", "n=20000", 32768, time.Second, 300 * time.Millisecond, 0, true, 0},
		{"large, with its head, past the limit", "n=20000", 16384, time.Second, 300 * time.Millisecond, 0, false, 30 * time.Millisecond},
		{"past the grace", "n=4096&delay=600", 16384, time.Second, 100 * time.Millisecond, 100 * time.Millisecond, false, 0},
		// As a body is let go of at once: what comes within the grace, and
		// no more.
		{"at once, within the grace", "n=4096&delay=30", 1 << 62, 300 * time.Millisecond, 300 * time.Millisecond, 30 * time.Millisecond, true, 0},
		{"at once, past the grace", "n=4096&delay=600", 1 << 62, 100 * time.Millisecond, 100 * time.Millisecond, 100 * time.Millisecond, false, 100 * time.Millisecond},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newDrainServer(t)
			tr := &Transport{}
			defer tr.CloseIdleConnections()
			res := drainGet(t, tr, context.Background(), s.URL+"/?"+tc.query)
			began := time.Now()
			DrainBody(context.Background(), res, tc.limit, tc.timeout, tc.grace)
			if took := time.Since(began); took < tc.returns-20*time.Millisecond || took > tc.returns+150*time.Millisecond {
				t.Fatalf("DrainBody returned %v after, want about %v", took, tc.returns)
			}
			io.Copy(io.Discard, drainGet(t, tr, context.Background(), s.URL+"/?n=1").Body)
			if same := s.count() == 1; same != tc.same {
				t.Fatalf("%d connections", s.count())
			}
			at := s.closed(0, 1200*time.Millisecond)
			if tc.closed == 0 {
				if !at.IsZero() {
					t.Fatalf("closed %v after", at.Sub(began))
				}
				return
			}
			if took := at.Sub(began); at.IsZero() || took < tc.closed-50*time.Millisecond || took > tc.closed+400*time.Millisecond {
				t.Fatalf("closed %v after (zero: not), want about %v", took, tc.closed)
			}
		})
	}
}

// TestALetGoSaysWhenTheBodyIsDone: the channel DrainBody and AwaitBody
// return is closed once the body is done with, the connection back to be
// used again or closed by then; at once when nothing was waited for.
func TestALetGoSaysWhenTheBodyIsDone(t *testing.T) {
	for _, tc := range []struct {
		name  string
		query string
		await bool          // AwaitBody, else DrainBody
		done  time.Duration // when the channel is closed, about; 0 at once
		idle  int           // idle connections then
	}{
		{"drained at once", "n=100&chunked", false, 0, 1},
		{"drained", "n=100&delay=300", false, 300 * time.Millisecond, 1},
		{"drained past its time", "n=100&delay=1200", false, 600 * time.Millisecond, 0},
		{"of a connection to be closed", "n=100&delay=300&close", false, 0, 0},
		{"awaited at once", "n=100&chunked", true, 0, 1},
		{"awaited", "n=100&delay=300", true, 300 * time.Millisecond, 1},
		{"awaited, in parts", "n=8192&delay=300&parts=2&gap=1000", true, 300 * time.Millisecond, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newDrainServer(t)
			tr := &Transport{}
			defer tr.CloseIdleConnections()
			res := drainGet(t, tr, context.Background(), s.URL+"/?"+tc.query)
			if tc.done == 0 && !strings.Contains(tc.query, "close") {
				time.Sleep(50 * time.Millisecond) // the body is in the buffer
			}
			began := time.Now()
			var done <-chan struct{}
			if tc.await {
				done = AwaitBody(context.Background(), res, 0, 0)
			} else {
				done = DrainBody(context.Background(), res, 16384, 600*time.Millisecond, 0)
			}
			if tc.done == 0 {
				select {
				case <-done:
				default:
					t.Fatal("not done once it returned")
				}
			} else {
				select {
				case <-done:
					t.Fatal("done before the body was")
				default:
				}
				select {
				case <-done:
				case <-time.After(3 * time.Second):
					t.Fatal("never done")
				}
				if took := time.Since(began); took < tc.done-100*time.Millisecond || took > tc.done+400*time.Millisecond {
					t.Errorf("done %v after, want about %v", took, tc.done)
				}
			}
			if n := tr.IdleConnCountForTesting("http", s.Listener.Addr().String()); n != tc.idle {
				t.Errorf("%d idle connections once done, want %d", n, tc.idle)
			}
		})
	}
}

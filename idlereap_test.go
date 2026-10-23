package http_test

import (
	"net"
	"sync"
	"testing"
	"time"

	. "github.com/igor-lemon/fhttp"
	"github.com/igor-lemon/fhttp/httptest"
)

// connWatch counts the connections a test server takes and tells when one
// closes.
type connWatch struct {
	mu     sync.Mutex
	opened int
	closed chan net.Conn
}

func newWatchedServer(t *testing.T) (*httptest.Server, *connWatch) {
	t.Helper()
	w := &connWatch{closed: make(chan net.Conn, 16)}
	ts := httptest.NewUnstartedServer(HandlerFunc(func(rw ResponseWriter, r *Request) {
		rw.Write([]byte("ok"))
	}))
	ts.Config.ConnState = func(c net.Conn, s ConnState) {
		switch s {
		case StateNew:
			w.mu.Lock()
			w.opened++
			w.mu.Unlock()
		case StateClosed:
			w.closed <- c
		}
	}
	ts.Start()
	t.Cleanup(ts.Close)
	return ts, w
}

func (w *connWatch) count() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.opened
}

func reapGet(t *testing.T, tr *Transport, url string) {
	t.Helper()
	req, _ := NewRequest("GET", url, nil)
	res, err := tr.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 8)
	for {
		if _, err := res.Body.Read(buf); err != nil {
			break
		}
	}
	res.Body.Close()
}

// TestIdleConnReapOnRequestClosesOnlyWhenAConnectionIsAskedFor holds
// IdleConnReapOnRequest to Chromium's socket pool: no timer closes an idle
// HTTP/1 connection; the next request for a connection, to any host, closes
// every idle one older than IdleConnTimeout, and one younger is reused.
func TestIdleConnReapOnRequestClosesOnlyWhenAConnectionIsAskedFor(t *testing.T) {
	a, watchA := newWatchedServer(t)
	b, watchB := newWatchedServer(t)
	tr := &Transport{IdleConnTimeout: 100 * time.Millisecond, IdleConnReapOnRequest: true}
	defer tr.CloseIdleConnections()

	reapGet(t, tr, a.URL)
	select {
	case <-watchA.closed:
		t.Fatal("an idle connection was closed with no request asking for one")
	case <-time.After(300 * time.Millisecond):
	}
	// A request to another host is a request for a connection: the pool
	// closes the old idle one to a.
	reapGet(t, tr, b.URL)
	select {
	case <-watchA.closed:
	case <-time.After(time.Second):
		t.Fatal("the next request for a connection left a too old idle connection open")
	}
	if n := watchB.count(); n != 1 {
		t.Fatalf("%d connections to b", n)
	}
	// One that is not too old yet is reused.
	reapGet(t, tr, b.URL)
	if n := watchB.count(); n != 1 {
		t.Fatalf("a fresh idle connection was not reused: %d connections", n)
	}
	// A too old one to the very host asked for is closed, and another dialed.
	time.Sleep(300 * time.Millisecond)
	reapGet(t, tr, b.URL)
	select {
	case <-watchB.closed:
	case <-time.After(time.Second):
		t.Fatal("a too old idle connection to the host asked for stayed open")
	}
	if n := watchB.count(); n != 2 {
		t.Fatalf("%d connections to b, want a new one", n)
	}
}

// TestIdleConnTimeoutStillClosesByTimerWithoutReapOnRequest keeps Go's
// default: the timer closes an idle connection on its own.
func TestIdleConnTimeoutStillClosesByTimerWithoutReapOnRequest(t *testing.T) {
	a, watch := newWatchedServer(t)
	tr := &Transport{IdleConnTimeout: 100 * time.Millisecond}
	defer tr.CloseIdleConnections()
	reapGet(t, tr, a.URL)
	select {
	case <-watch.closed:
	case <-time.After(2 * time.Second):
		t.Fatal("the idle timer did not close the connection")
	}
}

// TestIdleConnReapOnRequestWithoutATimeoutKeepsEveryConnection: with no
// IdleConnTimeout nothing is ever too old.
func TestIdleConnReapOnRequestWithoutATimeoutKeepsEveryConnection(t *testing.T) {
	a, watchA := newWatchedServer(t)
	b, _ := newWatchedServer(t)
	tr := &Transport{IdleConnReapOnRequest: true}
	defer tr.CloseIdleConnections()
	reapGet(t, tr, a.URL)
	time.Sleep(50 * time.Millisecond)
	reapGet(t, tr, b.URL)
	select {
	case <-watchA.closed:
		t.Fatal("a connection was closed with no timeout to be too old by")
	case <-time.After(200 * time.Millisecond):
	}
}

// TestTransportCloneKeepsIdleConnReapOnRequest: Clone copies the field.
func TestTransportCloneKeepsIdleConnReapOnRequest(t *testing.T) {
	tr := &Transport{IdleConnTimeout: time.Minute, IdleConnReapOnRequest: true}
	if !tr.Clone().IdleConnReapOnRequest {
		t.Fatal("Clone lost IdleConnReapOnRequest")
	}
}

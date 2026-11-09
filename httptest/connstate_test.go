package httptest

import (
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	http "github.com/igor-lemon/fhttp"
)

// Close returns only once the ConnState hook of every connection it waits
// for has ended, as Go's own httptest does: it used to forget a closed
// connection before the hook ran, so that a hook logging to its test (the
// http2 package's onSameConn) logged after the test had ended, which the
// race detector reported now and then.
func TestCloseWaitsForTheConnStateHookOfAClosedConnection(t *testing.T) {
	entered := make(chan struct{})
	gate := make(chan struct{})
	var enter sync.Once
	var ended atomic.Bool
	ts := NewUnstartedServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	ts.Config.ConnState = func(_ net.Conn, s http.ConnState) {
		if s != http.StateClosed {
			return
		}
		enter.Do(func() { close(entered) })
		<-gate
		ended.Store(true)
	}
	ts.Start()
	res, err := ts.Client().Get(ts.URL)
	if err != nil {
		close(gate)
		ts.Close()
		t.Fatal(err)
	}
	res.Body.Close()

	closed := make(chan struct{})
	go func() {
		ts.Close()
		close(closed)
	}()
	<-entered
	select {
	case <-closed:
		close(gate)
		t.Fatal("Close returned while the ConnState hook of a closed connection was still running")
	case <-time.After(100 * time.Millisecond):
	}
	close(gate)
	<-closed
	if !ended.Load() {
		t.Fatal("Close returned before the ConnState hook of a closed connection ended")
	}
}

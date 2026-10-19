package http2

import (
	"context"
	"errors"
	"io"
	"net"
	"os"
	"testing"
	"time"

	tls "github.com/igor-lemon/utls"

	http "github.com/igor-lemon/fhttp"
	"github.com/igor-lemon/fhttp/httptrace"
)

// dialKey is a value a request's context carries, which tells a dial whose
// context it was given.
type dialKey struct{}

// gatedDialer is a Transport.DialTLSContext that says each context it is
// called with on calls, then dials addr once open is closed (or fails with
// fail), unless the context ends first.
type gatedDialer struct {
	addr  string
	calls chan context.Context
	open  chan struct{}
	fail  error
}

func newGatedDialer(addr string) *gatedDialer {
	return &gatedDialer{addr: addr, calls: make(chan context.Context, 8), open: make(chan struct{})}
}

func (d *gatedDialer) dial(ctx context.Context, network, _ string, cfg *tls.Config) (net.Conn, error) {
	d.calls <- ctx
	select {
	case <-d.open:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	if d.fail != nil {
		return nil, d.fail
	}
	return (&tls.Dialer{Config: cfg}).DialContext(ctx, network, d.addr)
}

// dialing is a Transport that dials with d, and a server it dials to.
func dialing(t *testing.T) (*Transport, *gatedDialer, string) {
	t.Helper()
	st := newServerTester(t, func(http.ResponseWriter, *http.Request) {}, optOnlyServer)
	t.Cleanup(st.Close)
	d := newGatedDialer(st.ts.Listener.Addr().String())
	tr := &Transport{TLSClientConfig: tlsConfigInsecure, DialTLSContext: d.dial}
	t.Cleanup(tr.CloseIdleConnections)
	return tr, d, st.ts.URL
}

// getting sends a GET of url with tr in ctx, and says how it ended on the
// channel it returns.
func getting(tr *Transport, url string, ctx context.Context, header ...string) <-chan error {
	ended := make(chan error, 1)
	go func() {
		req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
		if err != nil {
			ended <- err
			return
		}
		for i := 0; i+1 < len(header); i += 2 {
			req.Header.Set(header[i], header[i+1])
		}
		res, err := tr.RoundTrip(req)
		if err == nil {
			_, _ = io.Copy(io.Discard, res.Body)
			_ = res.Body.Close()
		}
		ended <- err
	}()
	return ended
}

// endsWithin is how the channel ended, which it must within limit.
func endsWithin(t *testing.T, ended <-chan error, limit time.Duration) error {
	t.Helper()
	select {
	case err := <-ended:
		return err
	case <-time.After(limit):
		t.Fatalf("still going after %v", limit)
		return nil
	}
}

// next is the context of the next dial d is called for, which it must be
// within two seconds.
func (d *gatedDialer) next(t *testing.T) context.Context {
	t.Helper()
	select {
	case ctx := <-d.calls:
		return ctx
	case <-time.After(2 * time.Second):
		t.Fatal("no dial")
		return nil
	}
}

// waiting is a context whose request says on joined when it looks for a
// connection, which it does under the pool's lock, just before it starts a
// dial or joins one.
func waiting(ctx context.Context) (context.Context, <-chan struct{}) {
	joined := make(chan struct{}, 4)
	return httptrace.WithClientTrace(ctx, &httptrace.ClientTrace{GetConn: func(string) { joined <- struct{}{} }}), joined
}

func TestADialEndsWithTheContextOfTheRequestItIsFor(t *testing.T) {
	tr, d, url := dialing(t)
	ctx, cancel := context.WithTimeout(context.WithValue(context.Background(), dialKey{}, "mine"), 100*time.Millisecond)
	defer cancel()
	if err := endsWithin(t, getting(tr, url, ctx), 2*time.Second); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("RoundTrip: %v, want the request's deadline", err)
	}
	dialed := d.next(t)
	if dialed.Value(dialKey{}) != "mine" || dialed.Err() == nil {
		t.Errorf("the dial's context: %v, ended: %v", dialed.Value(dialKey{}), dialed.Err())
	}
}

func TestARequestThatAsksForItsConnectionClosedDialsWithItsContext(t *testing.T) {
	tr, d, url := dialing(t)
	ctx, cancel := context.WithTimeout(context.WithValue(context.Background(), dialKey{}, "mine"), 100*time.Millisecond)
	defer cancel()
	if err := endsWithin(t, getting(tr, url, ctx, "Connection", "close"), 2*time.Second); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("RoundTrip: %v, want the request's deadline", err)
	}
	if dialed := d.next(t); dialed.Value(dialKey{}) != "mine" {
		t.Errorf("the dial's context: %v", dialed.Value(dialKey{}))
	}
}

func TestARequestWaitsForAnotherRequestsDialNoLongerThanItsContextLets(t *testing.T) {
	tr, d, url := dialing(t)
	first := getting(tr, url, context.Background())
	d.next(t)
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	if err := endsWithin(t, getting(tr, url, ctx), 2*time.Second); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("the second: %v, want its deadline", err)
	}
	if took := time.Since(start); took > time.Second {
		t.Errorf("the second waited %v", took)
	}
	// The dial goes on for the first, which it was started for.
	close(d.open)
	if err := endsWithin(t, first, 5*time.Second); err != nil {
		t.Fatalf("the first: %v", err)
	}
	select {
	case <-d.calls:
		t.Error("dialed again")
	default:
	}
}

func TestARequestDialsForItselfWhenTheDialItWaitedForEndedWithAnothersContext(t *testing.T) {
	tr, d, url := dialing(t)
	short, cancel := context.WithCancel(context.Background())
	defer cancel()
	first := getting(tr, url, short)
	d.next(t)
	long, joined := waiting(context.WithValue(context.Background(), dialKey{}, "second"))
	second := getting(tr, url, long)
	select {
	case <-joined:
	case <-time.After(2 * time.Second):
		t.Fatal("the second request never looked for a connection")
	}
	cancel()
	if err := endsWithin(t, first, 2*time.Second); !errors.Is(err, context.Canceled) {
		t.Fatalf("the first: %v, want its context's end", err)
	}
	again := d.next(t)
	if again.Value(dialKey{}) != "second" {
		t.Errorf("dialed again with %v", again.Value(dialKey{}))
	}
	close(d.open)
	if err := endsWithin(t, second, 5*time.Second); err != nil {
		t.Fatalf("the second: %v", err)
	}
}

func TestADialThatFailsOfItselfFailsEveryRequestWaitingForIt(t *testing.T) {
	tr, d, url := dialing(t)
	refused := errors.New("refused")
	d.fail = refused
	first := getting(tr, url, context.Background())
	d.next(t)
	ctx, joined := waiting(context.Background())
	second := getting(tr, url, ctx)
	select {
	case <-joined:
	case <-time.After(2 * time.Second):
		t.Fatal("the second request never looked for a connection")
	}
	close(d.open)
	for _, ended := range []<-chan error{first, second} {
		if err := endsWithin(t, ended, 2*time.Second); !errors.Is(err, refused) {
			t.Errorf("RoundTrip: %v, want %v", err, refused)
		}
	}
	select {
	case <-d.calls:
		t.Error("dialed again")
	default:
	}
}

func TestDialTLSContextIsTakenBeforeDialTLS(t *testing.T) {
	tr, d, url := dialing(t)
	tr.DialTLS = func(string, string, *tls.Config) (net.Conn, error) { return nil, errors.New("DialTLS") }
	close(d.open)
	if err := endsWithin(t, getting(tr, url, context.Background()), 5*time.Second); err != nil {
		t.Fatalf("RoundTrip: %v", err)
	}
	// Without it, DialTLS, which no request's context ends.
	tr2 := &Transport{TLSClientConfig: tlsConfigInsecure, DialTLS: tr.DialTLS}
	defer tr2.CloseIdleConnections()
	if err := endsWithin(t, getting(tr2, url, context.Background()), 5*time.Second); err == nil || err.Error() != "DialTLS" {
		t.Fatalf("RoundTrip: %v, want DialTLS's", err)
	}
}

func TestTheDefaultDialEndsWithItsRequestsContext(t *testing.T) {
	// A server that takes the connection and never answers its handshake.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	accepted := make(chan net.Conn, 1)
	go func() {
		if c, err := ln.Accept(); err == nil {
			accepted <- c
		}
	}()
	tr := &Transport{TLSClientConfig: tlsConfigInsecure}
	defer tr.CloseIdleConnections()
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if err := endsWithin(t, getting(tr, "https://"+ln.Addr().String()+"/", ctx), 2*time.Second); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("RoundTrip: %v, want the request's deadline", err)
	}
	// And the client let go of the connection.
	c := <-accepted
	defer c.Close()
	_ = c.SetReadDeadline(time.Now().Add(2 * time.Second))
	_, err = io.Copy(io.Discard, c)
	if errors.Is(err, os.ErrDeadlineExceeded) {
		t.Errorf("the connection is still open: %v", err)
	}
}

func TestShouldRetryDial(t *testing.T) {
	ended, end := context.WithCancel(context.Background())
	end()
	live := context.WithValue(context.Background(), dialKey{}, "live")
	for _, tc := range []struct {
		name string
		call *dialCall
		req  context.Context
		want bool
	}{
		{"dialed", &dialCall{ctx: ended}, live, false},
		{"the request's own context ended it", &dialCall{ctx: ended, err: context.Canceled}, ended, false},
		{"failed of itself", &dialCall{ctx: ended, err: errors.New("refused")}, live, false},
		{"another's context ended it", &dialCall{ctx: ended, err: context.Canceled}, live, true},
		{"another's deadline ended it", &dialCall{ctx: ended, err: &net.OpError{Err: context.DeadlineExceeded}}, live, true},
		// Its own timeout, while the other's context goes on.
		{"timed out of itself", &dialCall{ctx: context.Background(), err: context.DeadlineExceeded}, live, false},
	} {
		req, err := http.NewRequestWithContext(tc.req, "GET", "https://example.com/", nil)
		if err != nil {
			t.Fatal(err)
		}
		if got := shouldRetryDial(tc.call, req); got != tc.want {
			t.Errorf("%s: %v, want %v", tc.name, got, tc.want)
		}
	}
}

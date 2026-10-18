package http2

import (
	"bytes"
	"errors"
	"io"
	"net"
	"slices"
	"sync"
	"sync/atomic"
	"testing"

	tls "github.com/bogdanfinn/utls"

	http "github.com/igor-lemon/fhttp"
	"github.com/igor-lemon/fhttp/http2/hpack"
	"github.com/igor-lemon/fhttp/httptrace"
)

// answerFrom answers every request with the address it came from, which
// tells one connection from another.
func answerFrom(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, r.RemoteAddr) }

// coalescer is a Transport.Coalesce that keeps what it was asked and answers
// with answer.
type coalescer struct {
	mu     sync.Mutex
	addrs  []string
	conns  [][]net.Conn
	answer func(addr string, conns []net.Conn) net.Conn
}

func (c *coalescer) coalesce(_ *http.Request, addr string, conns []net.Conn) net.Conn {
	c.mu.Lock()
	c.addrs = append(c.addrs, addr)
	c.conns = append(c.conns, conns)
	c.mu.Unlock()
	return c.answer(addr, conns)
}

func (c *coalescer) asked() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Clone(c.addrs)
}

// coalescing is a Transport whose every dial goes to the server at addr,
// whatever host:port it is for, counted in dials, and whose Coalesce is c's.
func coalescing(t *testing.T, addr string, c *coalescer) (tr *Transport, dials *atomic.Int32) {
	t.Helper()
	dials = new(atomic.Int32)
	tr = &Transport{
		TLSClientConfig: tlsConfigInsecure,
		DialTLS: func(network, _ string, cfg *tls.Config) (net.Conn, error) {
			dials.Add(1)
			return tls.Dial(network, addr, cfg)
		},
		Coalesce: c.coalesce,
	}
	t.Cleanup(tr.CloseIdleConnections)
	return tr, dials
}

// sendTo sends a GET of url with tr, as opt says, and is what it was
// answered: the address it came from.
func sendTo(t *testing.T, tr *Transport, url string, opt RoundTripOpt) (string, error) {
	t.Helper()
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		t.Fatal(err)
	}
	return send(tr, req, opt)
}

func send(tr *Transport, req *http.Request, opt RoundTripOpt) (string, error) {
	res, err := tr.RoundTripOpt(req, opt)
	if err != nil {
		return "", err
	}
	defer res.Body.Close()
	body, err := io.ReadAll(res.Body)
	return string(body), err
}

// from is what sendTo answers, or a failed test.
func from(t *testing.T, tr *Transport, url string) string {
	t.Helper()
	got, err := sendTo(t, tr, url, RoundTripOpt{})
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	return got
}

// pooled is the pool's ClientConn of conn.
func pooled(t *testing.T, tr *Transport, conn net.Conn) *ClientConn {
	t.Helper()
	p := tr.connPool().(*clientConnPool)
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, cc := range p.made {
		if cc.tconn == conn {
			return cc
		}
	}
	t.Fatalf("no connection of the pool's is %v", conn.LocalAddr())
	return nil
}

func TestCoalesceSharesAConnectionWithAnotherHost(t *testing.T) {
	st := newServerTester(t, answerFrom, optOnlyServer)
	defer st.Close()
	c := &coalescer{answer: func(_ string, conns []net.Conn) net.Conn { return conns[0] }}
	tr, dials := coalescing(t, st.ts.Listener.Addr().String(), c)

	first := from(t, tr, "https://a.test/")
	var wanted []string
	var got []httptrace.GotConnInfo
	req, err := http.NewRequest("GET", "https://b.test/", nil)
	if err != nil {
		t.Fatal(err)
	}
	req = req.WithContext(httptrace.WithClientTrace(req.Context(), &httptrace.ClientTrace{
		GetConn: func(hostPort string) { wanted = append(wanted, hostPort) },
		GotConn: func(info httptrace.GotConnInfo) { got = append(got, info) },
	}))
	second, err := send(tr, req, RoundTripOpt{})
	if err != nil {
		t.Fatalf("GET b.test: %v", err)
	}
	third := from(t, tr, "https://b.test/x")
	if second != first || third != first {
		t.Errorf("a.test from %s, then b.test from %s and %s", first, second, third)
	}
	if n := dials.Load(); n != 1 {
		t.Errorf("%d dials", n)
	}
	// Asked once, for b.test, which it holds a connection to from then on;
	// not for a.test, while it held none to share.
	if asked := c.asked(); !slices.Equal(asked, []string{"b.test:443"}) || len(c.conns[0]) != 1 {
		t.Errorf("asked for %q, offered %v", asked, c.conns)
	}
	if !slices.Equal(wanted, []string{"b.test:443"}) || len(got) != 1 || !got[0].Reused {
		t.Errorf("GetConn %q, GotConn %+v", wanted, got)
	}
	// Shared with two, it is offered once.
	if fourth := from(t, tr, "https://c.test/"); fourth != first || len(c.conns) != 2 || len(c.conns[1]) != 1 {
		t.Errorf("c.test from %s, offered %v", fourth, c.conns)
	}
}

func TestCoalesceDialsWhenItNamesNoConnection(t *testing.T) {
	st := newServerTester(t, answerFrom, optOnlyServer)
	defer st.Close()
	c := &coalescer{answer: func(string, []net.Conn) net.Conn { return nil }}
	tr, dials := coalescing(t, st.ts.Listener.Addr().String(), c)

	first, second := from(t, tr, "https://a.test/"), from(t, tr, "https://b.test/")
	again := from(t, tr, "https://b.test/")
	if second == first || again != second || dials.Load() != 2 {
		t.Errorf("a.test from %s, b.test from %s and %s, %d dials", first, second, again, dials.Load())
	}
	if asked := c.asked(); !slices.Equal(asked, []string{"b.test:443"}) {
		t.Errorf("asked for %q", asked)
	}
}

func TestCoalesceIsOfferedTheConnectionsThatCanTakeARequestInTheOrderMade(t *testing.T) {
	st := newServerTester(t, answerFrom, optOnlyServer)
	defer st.Close()
	c := &coalescer{answer: func(string, []net.Conn) net.Conn { return nil }}
	tr, _ := coalescing(t, st.ts.Listener.Addr().String(), c)
	var made []string
	for _, host := range []string{"a", "b", "c", "d"} {
		made = append(made, from(t, tr, "https://"+host+".test/"))
	}
	offered := func() []string {
		c.mu.Lock()
		defer c.mu.Unlock()
		var out []string
		for _, conn := range c.conns[len(c.conns)-1] {
			out = append(out, conn.LocalAddr().String())
		}
		return out
	}
	from(t, tr, "https://e.test/")
	if got := offered(); !slices.Equal(got, made) {
		t.Fatalf("offered %q, made %q", got, made)
	}
	// One no longer the pool's, and one that can take no more.
	c.mu.Lock()
	conns := c.conns[len(c.conns)-1]
	c.mu.Unlock()
	tr.connPool().MarkDead(pooled(t, tr, conns[1]))
	full := pooled(t, tr, conns[2])
	full.mu.Lock()
	full.closing = true
	full.mu.Unlock()
	from(t, tr, "https://f.test/")
	got := offered()
	// e.test's own connection is offered too, last.
	if len(got) != 3 || got[0] != made[0] || got[1] != made[3] {
		t.Errorf("offered %q, made %q", got, made)
	}
}

func TestCoalesceDialsWhenTheConnectionItNamedIsGone(t *testing.T) {
	st := newServerTester(t, answerFrom, optOnlyServer)
	defer st.Close()
	var tr *Transport
	c := &coalescer{answer: func(_ string, conns []net.Conn) net.Conn {
		// Taken out of the pool while it was being asked.
		tr.connPool().MarkDead(pooled(t, tr, conns[0]))
		return conns[0]
	}}
	tr, dials := coalescing(t, st.ts.Listener.Addr().String(), c)
	first, second := from(t, tr, "https://a.test/"), from(t, tr, "https://b.test/")
	if second == first || dials.Load() != 2 {
		t.Errorf("a.test from %s, b.test from %s, %d dials", first, second, dials.Load())
	}
}

func TestCoalesceDialsWhenTheConnectionItNamedCanTakeNoMore(t *testing.T) {
	st := newServerTester(t, answerFrom, optOnlyServer)
	defer st.Close()
	var tr *Transport
	var once atomic.Bool
	c := &coalescer{answer: func(_ string, conns []net.Conn) net.Conn {
		if !once.CompareAndSwap(false, true) {
			return nil
		}
		// Closing while it was being asked.
		cc := pooled(t, tr, conns[0])
		cc.mu.Lock()
		cc.closing = true
		cc.mu.Unlock()
		return conns[0]
	}}
	tr, dials := coalescing(t, st.ts.Listener.Addr().String(), c)
	first := from(t, tr, "https://a.test/")
	var got []httptrace.GotConnInfo
	req, err := http.NewRequest("GET", "https://b.test/", nil)
	if err != nil {
		t.Fatal(err)
	}
	req = req.WithContext(httptrace.WithClientTrace(req.Context(), &httptrace.ClientTrace{
		GotConn: func(info httptrace.GotConnInfo) { got = append(got, info) },
	}))
	second, err := send(tr, req, RoundTripOpt{})
	if err != nil || second == first || dials.Load() != 2 {
		t.Errorf("a.test from %s, b.test from %s, %v, %d dials", first, second, err, dials.Load())
	}
	// Handed the connection it dialed, and only that one.
	if asked := c.asked(); len(asked) != 1 || len(got) != 1 || got[0].Reused {
		t.Errorf("asked for %q, GotConn %+v", asked, got)
	}
}

func TestCoalesceSharesNoConnectionWithARequestThatClosesItsOwn(t *testing.T) {
	st := newServerTester(t, answerFrom, optOnlyServer)
	defer st.Close()
	c := &coalescer{answer: func(_ string, conns []net.Conn) net.Conn { return conns[0] }}
	tr, dials := coalescing(t, st.ts.Listener.Addr().String(), c)
	first := from(t, tr, "https://a.test/")
	for _, onlyCached := range []bool{false, true} {
		req, err := http.NewRequest("GET", "https://b.test/", nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Close = true
		got, err := send(tr, req, RoundTripOpt{OnlyCachedConn: onlyCached})
		switch {
		case onlyCached && !errors.Is(err, ErrNoCachedConn):
			t.Errorf("only cached: %q, %v", got, err)
		case !onlyCached && (err != nil || got == first):
			t.Errorf("a.test from %s, b.test from %s, %v", first, got, err)
		}
	}
	if asked := c.asked(); len(asked) != 0 || dials.Load() != 2 {
		t.Errorf("asked for %q, %d dials", asked, dials.Load())
	}
}

func TestOnlyCachedConnTakesOneHeldOrShared(t *testing.T) {
	st := newServerTester(t, answerFrom, optOnlyServer)
	defer st.Close()
	c := &coalescer{answer: func(addr string, conns []net.Conn) net.Conn {
		if addr == "b.test:443" {
			return conns[0]
		}
		return nil
	}}
	tr, dials := coalescing(t, st.ts.Listener.Addr().String(), c)
	only := RoundTripOpt{OnlyCachedConn: true}
	if got, err := sendTo(t, tr, "https://a.test/", only); !errors.Is(err, ErrNoCachedConn) || dials.Load() != 0 {
		t.Fatalf("with no connection: %q, %v, %d dials", got, err, dials.Load())
	}
	first := from(t, tr, "https://a.test/")
	held, err := sendTo(t, tr, "https://a.test/", only)
	if err != nil || held != first {
		t.Errorf("held: %q, %v; a.test from %s", held, err, first)
	}
	shared, err := sendTo(t, tr, "https://b.test/", only)
	if err != nil || shared != first {
		t.Errorf("shared: %q, %v; a.test from %s", shared, err, first)
	}
	if got, err := sendTo(t, tr, "https://c.test/", only); !errors.Is(err, ErrNoCachedConn) {
		t.Errorf("shared none: %q, %v", got, err)
	}
	if n := dials.Load(); n != 1 {
		t.Errorf("%d dials", n)
	}
}

// askingPool is a pool that counts what it is asked.
type askingPool struct {
	ClientConnPool
	asked atomic.Int32
}

func (p *askingPool) GetClientConn(req *http.Request, addr string) (*ClientConn, error) {
	p.asked.Add(1)
	return p.ClientConnPool.GetClientConn(req, addr)
}

func TestOnlyCachedConnAsksAnotherPoolAsItIs(t *testing.T) {
	st := newServerTester(t, answerFrom, optOnlyServer)
	defer st.Close()
	tr, dials := coalescing(t, st.ts.Listener.Addr().String(), &coalescer{})
	pool := &askingPool{ClientConnPool: &clientConnPool{t: tr}}
	tr.ConnPool = pool
	if _, err := sendTo(t, tr, "https://a.test/", RoundTripOpt{OnlyCachedConn: true}); err != nil ||
		pool.asked.Load() != 1 || dials.Load() != 1 {
		t.Errorf("%v, asked %d times, %d dials", err, pool.asked.Load(), dials.Load())
	}
}

// goingAway serves HTTP/2 without TLS on each connection it accepts, and
// answers the n-th request it is sent, over all of them, with a GOAWAY that
// leaves it unprocessed when away says so, or with 200.
func goingAway(t *testing.T, away func(n int) bool) string {
	t.Helper()
	ln := newLocalListener(t)
	var mu sync.Mutex
	var accepted []net.Conn
	t.Cleanup(func() {
		_ = ln.Close()
		mu.Lock()
		defer mu.Unlock()
		for _, conn := range accepted {
			_ = conn.Close()
		}
	})
	var n atomic.Int32
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			mu.Lock()
			accepted = append(accepted, conn)
			mu.Unlock()
			go func() {
				defer conn.Close()
				if _, err := io.ReadFull(conn, make([]byte, len(ClientPreface))); err != nil {
					return
				}
				fr := NewFramer(conn, conn)
				_ = fr.WriteSettings()
				var ok bytes.Buffer
				_ = hpack.NewEncoder(&ok).WriteField(hpack.HeaderField{Name: ":status", Value: "200"})
				for {
					f, err := fr.ReadFrame()
					if err != nil {
						return
					}
					switch f := f.(type) {
					case *SettingsFrame:
						if !f.IsAck() {
							_ = fr.WriteSettingsAck()
						}
					case *HeadersFrame:
						if away(int(n.Add(1))) {
							_ = fr.WriteGoAway(f.StreamID-2, ErrCodeNo, nil)
							continue
						}
						_ = fr.WriteHeaders(HeadersFrameParam{StreamID: f.StreamID, BlockFragment: ok.Bytes(),
							EndStream: true, EndHeaders: true})
					}
				}
			}()
		}
	}()
	return ln.Addr().String()
}

func TestOnlyCachedConnDialsWhenAShareGoesAway(t *testing.T) {
	// The request to b.test goes out on a.test's connection, which the
	// server then leaves; tried again, it may dial.
	addr := goingAway(t, func(n int) bool { return n == 2 })
	dials := new(atomic.Int32)
	c := &coalescer{answer: func(addr string, conns []net.Conn) net.Conn { return conns[0] }}
	tr := &Transport{
		DialTLS: func(network, _ string, _ *tls.Config) (net.Conn, error) {
			dials.Add(1)
			return net.Dial(network, addr)
		},
		Coalesce: c.coalesce,
	}
	defer tr.CloseIdleConnections()
	if _, err := sendTo(t, tr, "https://a.test/", RoundTripOpt{}); err != nil {
		t.Fatalf("a.test: %v", err)
	}
	if _, err := sendTo(t, tr, "https://b.test/", RoundTripOpt{OnlyCachedConn: true}); err != nil || dials.Load() != 2 {
		t.Errorf("b.test: %v, %d dials", err, dials.Load())
	}
	if asked := c.asked(); !slices.Equal(asked, []string{"b.test:443"}) {
		t.Errorf("asked for %q", asked)
	}
}

// alone sends a GET of url with tr, as opt says, made with a context that
// says whether it may go out on a connection shared with its host, and is
// what it was answered: the address it came from.
func alone(t *testing.T, tr *Transport, url string, allowed bool, opt RoundTripOpt) (string, error) {
	t.Helper()
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		t.Fatal(err)
	}
	return send(tr, req.WithContext(WithCoalescing(req.Context(), allowed)), opt)
}

// pooledFrom is the pool's ClientConn whose connection is from addr.
func pooledFrom(t *testing.T, tr *Transport, addr string) *ClientConn {
	t.Helper()
	p := tr.connPool().(*clientConnPool)
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, cc := range p.made {
		if cc.tconn.LocalAddr().String() == addr {
			return cc
		}
	}
	t.Fatalf("no connection of the pool's is from %s", addr)
	return nil
}

// keysOf are the host:ports the pool keeps the connection from addr under,
// the one it was dialed for first.
func keysOf(t *testing.T, tr *Transport, addr string) []string {
	t.Helper()
	cc := pooledFrom(t, tr, addr)
	p := tr.connPool().(*clientConnPool)
	p.mu.Lock()
	defer p.mu.Unlock()
	return slices.Clone(p.keys[cc])
}

// heldFor are the addresses of the connections the pool holds for key.
func heldFor(tr *Transport, key string) []string {
	p := tr.connPool().(*clientConnPool)
	p.mu.Lock()
	defer p.mu.Unlock()
	var out []string
	for _, cc := range p.conns[key] {
		out = append(out, cc.tconn.LocalAddr().String())
	}
	return out
}

func TestARequestThatMayNotCoalesceGoesOutOnAConnectionDialedForItsHost(t *testing.T) {
	st := newServerTester(t, answerFrom, optOnlyServer)
	defer st.Close()
	c := &coalescer{answer: func(_ string, conns []net.Conn) net.Conn { return conns[0] }}
	tr, dials := coalescing(t, st.ts.Listener.Addr().String(), c)

	first, shared := from(t, tr, "https://a.test/"), from(t, tr, "https://b.test/")
	own, err := alone(t, tr, "https://b.test/", false, RoundTripOpt{})
	if err != nil {
		t.Fatalf("GET b.test alone: %v", err)
	}
	// The connection dialed for b.test carries its requests from then on,
	// and a.test's carries a.test's alone.
	later, again := from(t, tr, "https://b.test/x"), from(t, tr, "https://a.test/x")
	if shared != first || own == first || later != own || again != first || dials.Load() != 2 {
		t.Errorf("a.test from %s and %s, b.test from %s, %s alone, then %s; %d dials", first, again, shared, own,
			later, dials.Load())
	}
	if asked := c.asked(); !slices.Equal(asked, []string{"b.test:443"}) {
		t.Errorf("asked for %q", asked)
	}
	if keys, held := keysOf(t, tr, first), heldFor(tr, "b.test:443"); !slices.Equal(keys, []string{"a.test:443"}) ||
		!slices.Equal(held, []string{own}) {
		t.Errorf("a.test's connection kept for %q, b.test's held %q", keys, held)
	}
	// One of its host's own it takes as any request does.
	if got, err := alone(t, tr, "https://a.test/y", false, RoundTripOpt{}); err != nil || got != first || dials.Load() != 2 {
		t.Errorf("a.test alone from %s, %v; %d dials", got, err, dials.Load())
	}
}

func TestARequestThatMayNotCoalesceTakesNoSharedConnectionCached(t *testing.T) {
	st := newServerTester(t, answerFrom, optOnlyServer)
	defer st.Close()
	c := &coalescer{answer: func(_ string, conns []net.Conn) net.Conn { return conns[0] }}
	tr, dials := coalescing(t, st.ts.Listener.Addr().String(), c)
	first := from(t, tr, "https://a.test/")
	only := RoundTripOpt{OnlyCachedConn: true}
	// Shared, it is not taken; not shared yet, it is not asked for.
	for range 2 {
		if got, err := alone(t, tr, "https://b.test/", false, only); !errors.Is(err, ErrNoCachedConn) {
			t.Errorf("b.test alone, only cached: %q, %v", got, err)
		}
		from(t, tr, "https://b.test/")
	}
	if held, err := alone(t, tr, "https://a.test/", false, only); err != nil || held != first {
		t.Errorf("a.test alone, only cached: %q, %v; a.test from %s", held, err, first)
	}
	if asked := c.asked(); !slices.Equal(asked, []string{"b.test:443"}) || dials.Load() != 1 {
		t.Errorf("asked for %q, %d dials", asked, dials.Load())
	}
}

func TestARequestThatMayCoalesceIsSharedAConnection(t *testing.T) {
	st := newServerTester(t, answerFrom, optOnlyServer)
	defer st.Close()
	c := &coalescer{answer: func(_ string, conns []net.Conn) net.Conn { return conns[0] }}
	tr, dials := coalescing(t, st.ts.Listener.Addr().String(), c)
	first := from(t, tr, "https://a.test/")
	if got, err := alone(t, tr, "https://b.test/", true, RoundTripOpt{}); err != nil || got != first || dials.Load() != 1 {
		t.Errorf("b.test from %s, %v; a.test from %s; %d dials", got, err, first, dials.Load())
	}
}

func TestAConnectionDialedForAHostTakesItsRequestsFromThoseSharedWithIt(t *testing.T) {
	st := newServerTester(t, answerFrom, optOnlyServer)
	defer st.Close()
	c := &coalescer{answer: func(_ string, conns []net.Conn) net.Conn { return conns[0] }}
	tr, _ := coalescing(t, st.ts.Listener.Addr().String(), c)
	closing := func(addr string, closing bool) {
		cc := pooledFrom(t, tr, addr)
		cc.mu.Lock()
		cc.closing = closing
		cc.mu.Unlock()
	}
	// c.test dials its own while a.test's can take no more; b.test is shared
	// a.test's, then c.test's when a.test's can take no more, and dials its
	// own when neither can.
	a := from(t, tr, "https://a.test/")
	closing(a, true)
	other := from(t, tr, "https://c.test/")
	closing(a, false)
	closing(other, true)
	sharedA := from(t, tr, "https://b.test/")
	closing(a, true)
	closing(other, false)
	sharedOther := from(t, tr, "https://b.test/x")
	closing(other, true)
	own := from(t, tr, "https://b.test/y")
	if sharedA != a || sharedOther != other || own == a || own == other {
		t.Errorf("a.test from %s, c.test from %s, b.test from %s, %s and %s", a, other, sharedA, sharedOther, own)
	}
	keysA, keysOther, held := keysOf(t, tr, a), keysOf(t, tr, other), heldFor(tr, "b.test:443")
	if !slices.Equal(keysA, []string{"a.test:443"}) || !slices.Equal(keysOther, []string{"c.test:443"}) ||
		!slices.Equal(held, []string{own}) {
		t.Errorf("a.test's connection kept for %q, c.test's for %q, b.test's held %q", keysA, keysOther, held)
	}
}

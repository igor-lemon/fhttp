// Copyright 2015 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Transport code's client connection pooling.

package http2

import (
	"net"
	"slices"
	"sync"

	tls "github.com/igor-lemon/utls"

	http "github.com/igor-lemon/fhttp"
)

// ClientConnPool manages a pool of HTTP/2 client connections.
type ClientConnPool interface {
	GetClientConn(req *http.Request, addr string) (*ClientConn, error)
	MarkDead(*ClientConn)
}

// clientConnPoolIdleCloser is the interface implemented by ClientConnPool
// implementations which can close their idle connections.
type clientConnPoolIdleCloser interface {
	ClientConnPool
	closeIdleConnections()
}

var (
	_ clientConnPoolIdleCloser = (*clientConnPool)(nil)
	_ clientConnPoolIdleCloser = noDialClientConnPool{}
)

// TODO: use singleflight for dialing and addConnCalls?
type clientConnPool struct {
	t *Transport

	mu sync.Mutex // TODO: maybe switch to RWMutex
	// A conn is shared with another host:port, one it was not dialed for,
	// when Transport.Coalesce says so: it is then kept under both keys. The
	// first of a conn's keys is the host:port it was dialed for, and those
	// after it the ones it is shared with.
	conns        map[string][]*ClientConn // key is host:port
	dialing      map[string]*dialCall     // currently in-flight dials
	keys         map[*ClientConn][]string
	addConnCalls map[string]*addConnCall // in-flight addConnIfNeede calls
	// made are the conns in the order they were added, the order
	// Transport.Coalesce is offered them in.
	made []*ClientConn
}

func (p *clientConnPool) GetClientConn(req *http.Request, addr string) (*ClientConn, error) {
	return p.getClientConn(req, addr, dialOnMiss)
}

const (
	dialOnMiss   = true
	noDialOnMiss = false
)

// shouldTraceGetConn reports whether getClientConn should call any
// ClientTrace.GetConn hook associated with the http.Request.
//
// This complexity is needed to avoid double calls of the GetConn hook
// during the back-and-forth between net/http and x/net/http2 (when the
// net/http.Transport is upgraded to also speak http2), as well as support
// the case where x/net/http2 is being used directly.
func (p *clientConnPool) shouldTraceGetConn(st clientConnIdleState) bool {
	// If our Transport wasn't made via ConfigureTransport, always
	// trace the GetConn hook if provided, because that means the
	// http2 package is being used directly and it's the one
	// dialing, as opposed to net/http.
	if _, ok := p.t.ConnPool.(noDialClientConnPool); !ok {
		return true
	}
	// Otherwise, only use the GetConn hook if this connection has
	// been used previously for other requests. For fresh
	// connections, the net/http package does the dialing.
	return !st.freshConn
}

func (p *clientConnPool) getClientConn(req *http.Request, addr string, dialOnMiss bool) (*ClientConn, error) {
	if isConnectionCloseRequest(req) && dialOnMiss {
		// It gets its own connection.
		traceGetConn(req, addr)
		const singleUse = true
		cc, err := p.t.dialClientConn(addr, singleUse)
		if err != nil {
			return nil, err
		}
		return cc, nil
	}
	p.mu.Lock()
	shares := mayCoalesce(req)
	for _, cc := range p.conns[addr] {
		if !shares && p.keys[cc][0] != addr {
			// Dialed for another host:port.
			continue
		}
		if st := cc.idleState(); st.canTakeNewRequest {
			if p.shouldTraceGetConn(st) {
				traceGetConn(req, addr)
			}
			p.mu.Unlock()
			return cc, nil
		}
	}
	if shares && p.t.Coalesce != nil && !isConnectionCloseRequest(req) {
		if cc := p.coalesceLocked(req, addr); cc != nil {
			traceGetConn(req, addr)
			p.mu.Unlock()
			return cc, nil
		}
	}
	if !dialOnMiss {
		p.mu.Unlock()
		return nil, ErrNoCachedConn
	}
	traceGetConn(req, addr)
	call := p.getStartDialLocked(addr)
	p.mu.Unlock()
	<-call.done
	return call.res, call.err
}

// coalesceLocked is a conn of the pool's to another host:port that
// Transport.Coalesce says req to addr may go out on, from then on one of
// addr's too; or nil. p.mu is held, and let go while Coalesce is asked, which
// may take a while: it may look a name up.
func (p *clientConnPool) coalesceLocked(req *http.Request, addr string) *ClientConn {
	var offered []*ClientConn
	var conns []net.Conn
	for _, cc := range p.made {
		if cc.CanTakeNewRequest() {
			offered = append(offered, cc)
			conns = append(conns, cc.tconn)
		}
	}
	if len(offered) == 0 {
		return nil
	}
	p.mu.Unlock()
	chosen := p.t.Coalesce(req, addr, conns)
	p.mu.Lock()
	for i, cc := range offered {
		// Still the pool's, and still able to take it: the lock was let go.
		if conns[i] == chosen && len(p.keys[cc]) > 0 && cc.CanTakeNewRequest() {
			p.addConnLocked(addr, cc)
			return cc
		}
	}
	return nil
}

// dialCall is an in-flight Transport dial call to a host.
type dialCall struct {
	_    incomparable
	p    *clientConnPool
	done chan struct{} // closed when done
	res  *ClientConn   // valid after done is closed
	err  error         // valid after done is closed
}

// requires p.mu is held.
func (p *clientConnPool) getStartDialLocked(addr string) *dialCall {
	if call, ok := p.dialing[addr]; ok {
		// A dial is already in-flight. Don't start another.
		return call
	}
	call := &dialCall{p: p, done: make(chan struct{})}
	if p.dialing == nil {
		p.dialing = make(map[string]*dialCall)
	}
	p.dialing[addr] = call
	go call.dial(addr)
	return call
}

// run in its own goroutine.
func (c *dialCall) dial(addr string) {
	const singleUse = false // shared conn
	c.res, c.err = c.p.t.dialClientConn(addr, singleUse)
	close(c.done)

	c.p.mu.Lock()
	delete(c.p.dialing, addr)
	if c.err == nil {
		c.p.addConnLocked(addr, c.res)
	}
	c.p.mu.Unlock()
}

// addConnIfNeeded makes a NewClientConn out of c if a connection for key doesn't
// already exist. It coalesces concurrent calls with the same key.
// This is used by the http1 Transport code when it creates a new connection. Because
// the http1 Transport doesn't de-dup TCP dials to outbound hosts (because it doesn't know
// the protocol), it can get into a situation where it has multiple TLS connections.
// This code decides which ones live or die.
// The return value used is whether c was used.
// c is never closed.
func (p *clientConnPool) addConnIfNeeded(key string, t *Transport, c *tls.Conn) (used bool, err error) {
	p.mu.Lock()
	for _, cc := range p.conns[key] {
		if cc.CanTakeNewRequest() {
			p.mu.Unlock()
			return false, nil
		}
	}
	call, dup := p.addConnCalls[key]
	if !dup {
		if p.addConnCalls == nil {
			p.addConnCalls = make(map[string]*addConnCall)
		}
		call = &addConnCall{
			p:    p,
			done: make(chan struct{}),
		}
		p.addConnCalls[key] = call
		go call.run(t, key, c)
	}
	p.mu.Unlock()

	<-call.done
	if call.err != nil {
		return false, call.err
	}
	return !dup, nil
}

type addConnCall struct {
	_    incomparable
	p    *clientConnPool
	done chan struct{} // closed when done
	err  error
}

func (c *addConnCall) run(t *Transport, key string, tc *tls.Conn) {
	cc, err := t.NewClientConn(tc)

	p := c.p
	p.mu.Lock()
	if err != nil {
		c.err = err
	} else {
		p.addConnLocked(key, cc)
	}
	delete(p.addConnCalls, key)
	p.mu.Unlock()
	close(c.done)
}

// p.mu must be held
func (p *clientConnPool) addConnLocked(key string, cc *ClientConn) {
	for _, v := range p.conns[key] {
		if v == cc {
			return
		}
	}
	if p.conns == nil {
		p.conns = make(map[string][]*ClientConn)
	}
	if p.keys == nil {
		p.keys = make(map[*ClientConn][]string)
	}
	if len(p.keys[cc]) == 0 {
		// Dialed for key, it takes key's requests from those shared with it.
		p.unshareLocked(key)
		p.made = append(p.made, cc)
	}
	p.conns[key] = append(p.conns[key], cc)
	p.keys[cc] = append(p.keys[cc], key)
}

// unshareLocked has the conns shared with key, dialed for another host:port,
// carry its requests no more, as Chrome's pool drops the alias of a key for
// the session it makes for the key. p.mu must be held.
func (p *clientConnPool) unshareLocked(key string) {
	for _, cc := range slices.Clone(p.conns[key]) {
		if p.keys[cc][0] != key {
			p.conns[key] = filterOutClientConn(p.conns[key], cc)
			p.keys[cc] = slices.DeleteFunc(p.keys[cc], func(k string) bool { return k == key })
		}
	}
}

func (p *clientConnPool) MarkDead(cc *ClientConn) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, key := range p.keys[cc] {
		vv, ok := p.conns[key]
		if !ok {
			continue
		}
		newList := filterOutClientConn(vv, cc)
		if len(newList) > 0 {
			p.conns[key] = newList
		} else {
			delete(p.conns, key)
		}
	}
	delete(p.keys, cc)
	p.made = filterOutClientConn(p.made, cc)
}

func (p *clientConnPool) closeIdleConnections() {
	p.mu.Lock()
	defer p.mu.Unlock()
	// TODO: don't close a cc if it was just added to the pool
	// milliseconds ago and has never been used. There's currently
	// a small race window with the HTTP/1 Transport's integration
	// where it can add an idle conn just before using it, and
	// somebody else can concurrently call CloseIdleConns and
	// break some caller's RoundTrip.
	for _, vv := range p.conns {
		for _, cc := range vv {
			cc.closeIfIdle()
		}
	}
}

func filterOutClientConn(in []*ClientConn, exclude *ClientConn) []*ClientConn {
	out := in[:0]
	for _, v := range in {
		if v != exclude {
			out = append(out, v)
		}
	}
	// If we filtered it out, zero out the last item to prevent
	// the GC from seeing it.
	if len(in) != len(out) {
		in[len(in)-1] = nil
	}
	return out
}

// noDialClientConnPool is an implementation of http2.ClientConnPool
// which never dials. We let the HTTP/1.1 client dial and use its TLS
// connection instead.
type noDialClientConnPool struct{ *clientConnPool }

func (p noDialClientConnPool) GetClientConn(req *http.Request, addr string) (*ClientConn, error) {
	return p.getClientConn(req, addr, noDialOnMiss)
}

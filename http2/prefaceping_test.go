// Copyright 2026 The fhttp Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package http2

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	http "github.com/igor-lemon/fhttp"
	"github.com/igor-lemon/fhttp/http2/hpack"
	tls "github.com/igor-lemon/utls"
	"github.com/igor-lemon/utls/behavior"
)

// pingPolicy is the connection's own PING after idle milliseconds of reading
// nothing, the first saying 1, given up hung milliseconds after it (0 for
// never), and the rest of the policy.
func pingPolicy(t *testing.T, idle, hung int, more map[string]any) behavior.Policy {
	t.Helper()
	p := behavior.Policy{}
	set := func(k string, v any) {
		b, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		p[k] = b
	}
	set("h2.preface_ping_idle_ms", idle)
	set("h2.preface_ping_first_id", 1)
	set("h2.preface_ping_hung_ms", hung)
	for k, v := range more {
		set(k, v)
	}
	if err := p.Validate(); err != nil {
		t.Fatal(err)
	}
	return p
}

// seen is a frame the client sent, as the server read it.
type seen struct {
	at     time.Time
	kind   FrameType
	stream uint32
	flags  Flags
	prio   PriorityParam // a HEADERS frame's
	ping   [8]byte
	code   ErrCode
	last   uint32
	data   string
	eof    bool
}

// String is the frame in short: H3 (HEADERS of stream 3), H3e (with
// END_STREAM), D3:x (DATA), D3e:x, P1 (a PING saying 1), G0/1:why (GOAWAY:
// last stream, code, debug data), R3/7 (RST_STREAM), EOF.
func (s seen) String() string {
	end := ""
	if s.flags.Has(FlagHeadersEndStream) {
		end = "e"
	}
	switch {
	case s.eof:
		return "EOF"
	case s.kind == FrameHeaders:
		return fmt.Sprintf("H%d%s", s.stream, end)
	case s.kind == FrameData:
		return fmt.Sprintf("D%d%s:%s", s.stream, end, s.data)
	case s.kind == FramePing && !s.flags.Has(FlagPingAck):
		return fmt.Sprintf("P%d", binary.BigEndian.Uint64(s.ping[:]))
	case s.kind == FrameGoAway:
		return fmt.Sprintf("G%d/%d:%s", s.last, s.code, s.data)
	case s.kind == FrameRSTStream:
		return fmt.Sprintf("R%d/%d", s.stream, s.code)
	}
	return fmt.Sprintf("%v/%d", s.kind, s.stream)
}

// peer is the server end of one connection: it keeps what the client sends,
// frame by frame, and answers as the test says.
type peer struct {
	t    *testing.T
	conn net.Conn
	fr   *Framer
	seen chan seen
	wmu  sync.Mutex
}

func newPeer(t *testing.T, conn net.Conn) *peer {
	p := &peer{t: t, conn: conn, fr: NewFramer(conn, conn), seen: make(chan seen, 64)}
	go p.read()
	return p
}

// read greets the client and reads what it sends from then on, SETTINGS and
// WINDOW_UPDATE aside.
func (p *peer) read() {
	defer close(p.seen)
	if _, err := io.ReadFull(p.conn, make([]byte, len(ClientPreface))); err != nil {
		p.seen <- seen{at: time.Now(), eof: true}
		return
	}
	p.write(func() error { return p.fr.WriteSettings() })
	for {
		f, err := p.fr.ReadFrame()
		at := time.Now()
		if err != nil {
			p.seen <- seen{at: at, eof: true}
			return
		}
		h := f.Header()
		s := seen{at: at, kind: h.Type, stream: h.StreamID, flags: h.Flags}
		switch f := f.(type) {
		case *SettingsFrame:
			if !f.IsAck() {
				p.write(func() error { return p.fr.WriteSettingsAck() })
			}
			continue
		case *WindowUpdateFrame:
			continue
		case *PingFrame:
			s.ping = f.Data
		case *HeadersFrame:
			s.prio = f.Priority
		case *DataFrame:
			s.data = string(f.Data())
		case *GoAwayFrame:
			s.code, s.last, s.data = f.ErrCode, f.LastStreamID, string(f.DebugData())
		case *RSTStreamFrame:
			s.code = f.ErrCode
		}
		p.seen <- s
	}
}

func (p *peer) write(f func() error) {
	p.wmu.Lock()
	defer p.wmu.Unlock()
	f()
}

// expect reads the next frames, which must be want.
func (p *peer) expect(want ...string) []seen {
	p.t.Helper()
	var got []seen
	for range want {
		select {
		case s, ok := <-p.seen:
			if !ok {
				p.t.Fatalf("got %v, then nothing, want %v", got, want)
			}
			got = append(got, s)
		case <-time.After(5 * time.Second):
			p.t.Fatalf("got %v, then nothing for 5s, want %v", got, want)
		}
	}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		p.t.Fatalf("got %v, want %v", got, want)
	}
	return got
}

// answer ends stream with a 200.
func (p *peer) answer(stream uint32) {
	var block bytes.Buffer
	hpack.NewEncoder(&block).WriteField(hpack.HeaderField{Name: ":status", Value: "200"})
	p.write(func() error {
		return p.fr.WriteHeaders(HeadersFrameParam{StreamID: stream, EndHeaders: true, EndStream: true, BlockFragment: block.Bytes()})
	})
}

func (p *peer) ack(data [8]byte) { p.write(func() error { return p.fr.WritePing(true, data) }) }

func (p *peer) refuse(stream uint32) {
	p.write(func() error { return p.fr.WriteRSTStream(stream, ErrCodeRefusedStream) })
}

// pairs are n connections over loopback: the client's ends, the server's.
func pairs(t *testing.T, n int) (clients, servers []net.Conn) {
	t.Helper()
	ln := newLocalListener(t)
	defer ln.Close()
	for range n {
		c, err := net.Dial("tcp", ln.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		s, err := ln.Accept()
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			c.Close()
			s.Close()
		})
		clients, servers = append(clients, c), append(servers, s)
	}
	return clients, servers
}

// pingTransport is a transport with policy that dials the client ends of
// conns in order, and keeps each write of each.
func pingTransport(t *testing.T, policy behavior.Policy, conns ...net.Conn) (*Transport, []*writeLog) {
	logs := make([]*writeLog, len(conns))
	for i, c := range conns {
		logs[i] = &writeLog{Conn: c}
	}
	var mu sync.Mutex
	dialed := 0
	tr := &Transport{Behavior: policy, TLSClientConfig: tlsConfigInsecure, DialTLS: func(string, string, *tls.Config) (net.Conn, error) {
		mu.Lock()
		defer mu.Unlock()
		if dialed == len(logs) {
			return nil, errors.New("no more connections")
		}
		dialed++
		return logs[dialed-1], nil
	}}
	t.Cleanup(tr.CloseIdleConnections)
	return tr, logs
}

// fetch sends req through tr and reads all of its answer, in the background.
func fetch(tr *Transport, req *http.Request) chan error {
	done := make(chan error, 1)
	go func() {
		res, err := tr.RoundTrip(req)
		if err == nil {
			_, err = io.Copy(io.Discard, res.Body)
			res.Body.Close()
		}
		done <- err
	}()
	return done
}

func waitFor(t *testing.T, done chan error) error {
	t.Helper()
	select {
	case err := <-done:
		return err
	case <-time.After(5 * time.Second):
		t.Fatal("the request did not end")
	}
	return nil
}

// exchange sends a GET through tr, reads want from p, acknowledges every PING
// among them when ack says so, and answers.
func exchange(t *testing.T, tr *Transport, p *peer, ack bool, want ...string) []seen {
	t.Helper()
	req, _ := http.NewRequest("GET", "https://dummy.tld/", nil)
	done := fetch(tr, req)
	got := p.expect(want...)
	for _, s := range got {
		if ack && s.kind == FramePing {
			p.ack(s.ping)
		}
	}
	p.answer(got[0].stream)
	if err := waitFor(t, done); err != nil {
		t.Fatal(err)
	}
	return got
}

// writes are the frames of each write, in short, the preface aside.
func (c *writeLog) frames() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	var all []string
	for _, w := range c.writes {
		w = bytes.TrimPrefix(w, []byte(ClientPreface))
		var in []string
		for len(w) >= frameHeaderLen {
			n := int(w[0])<<16 | int(w[1])<<8 | int(w[2])
			if len(w) < frameHeaderLen+n {
				break
			}
			s := seen{kind: FrameType(w[3]), flags: Flags(w[4]), stream: binary.BigEndian.Uint32(w[5:9]) & (1<<31 - 1)}
			switch s.kind {
			case FramePing:
				copy(s.ping[:], w[frameHeaderLen:])
			case FrameData:
				s.data = string(w[frameHeaderLen : frameHeaderLen+n])
			}
			in = append(in, s.String())
			w = w[frameHeaderLen+n:]
		}
		all = append(all, strings.Join(in, "+"))
	}
	return all
}

// wroteAlone says whether the PING saying n went in a write of its own, the
// one of next right after it (a PING and what asked for it go under one
// hold of the connection's writes).
func wroteAlone(t *testing.T, log *writeLog, n int, before, next string) {
	t.Helper()
	writes := log.frames()
	for i, w := range writes {
		if w != fmt.Sprintf("P%d", n) {
			continue
		}
		if before != "" && (i == 0 || writes[i-1] != before) || next != "" && (i == len(writes)-1 || writes[i+1] != next) {
			break
		}
		return
	}
	t.Errorf("no write of P%d alone between %q and %q: %q", n, before, next, writes)
}

// TestAConnectionPingsAfterHeadersOnceItHasReadNothingForLong holds the
// connection's own PING to Chrome's: after the HEADERS of a request to a
// connection that has read nothing for longer than h2.preface_ping_idle_ms,
// in a write of its own, saying 1 and then one more each time; none to a
// connection that has just read.
func TestAConnectionPingsAfterHeadersOnceItHasReadNothingForLong(t *testing.T) {
	clients, servers := pairs(t, 1)
	tr, logs := pingTransport(t, pingPolicy(t, 100, 0, nil), clients...)
	p := newPeer(t, servers[0])
	exchange(t, tr, p, true, "H1e")
	time.Sleep(200 * time.Millisecond)
	exchange(t, tr, p, true, "H3e", "P1")
	exchange(t, tr, p, true, "H5e")
	time.Sleep(200 * time.Millisecond)
	exchange(t, tr, p, true, "H7e", "P2")
	wroteAlone(t, logs[0], 1, "H3e", "")
	wroteAlone(t, logs[0], 2, "H7e", "")
}

// TestAConnectionWithoutThePolicyNeverPings: no h2.preface_ping_idle_ms, or
// 0, is no PING however long the connection read nothing.
func TestAConnectionWithoutThePolicyNeverPings(t *testing.T) {
	for name, policy := range map[string]behavior.Policy{"absent": {}, "zero": pingPolicy(t, 0, 300, nil)} {
		t.Run(name, func(t *testing.T) {
			clients, servers := pairs(t, 1)
			tr, _ := pingTransport(t, policy, clients...)
			p := newPeer(t, servers[0])
			exchange(t, tr, p, true, "H1e")
			time.Sleep(200 * time.Millisecond)
			exchange(t, tr, p, true, "H3e")
			exchange(t, tr, p, true, "H5e")
		})
	}
}

// TestAPingGoesBeforeTheBodyAfterLongSilence: a DATA frame that carries any of
// a body, to a connection that has read nothing for long, goes after a PING,
// in a write of its own; the empty DATA that ends the body asks for none.
func TestAPingGoesBeforeTheBodyAfterLongSilence(t *testing.T) {
	clients, servers := pairs(t, 1)
	tr, logs := pingTransport(t, pingPolicy(t, 100, 0, nil), clients...)
	p := newPeer(t, servers[0])
	body, w := io.Pipe()
	req, _ := http.NewRequest("POST", "https://dummy.tld/", body)
	done := fetch(tr, req)
	p.expect("H1")
	w.Write([]byte("a"))
	p.expect("D1:a")
	time.Sleep(200 * time.Millisecond)
	w.Write([]byte("b"))
	got := p.expect("P1", "D1:b")
	p.ack(got[0].ping)
	time.Sleep(200 * time.Millisecond)
	w.Close()
	p.expect("D1e:")
	p.answer(1)
	if err := waitFor(t, done); err != nil {
		t.Fatal(err)
	}
	wroteAlone(t, logs[0], 1, "", "D1:b")
}

// TestAPingGoesAfterTheTrailers: trailers are a HEADERS block too.
func TestAPingGoesAfterTheTrailers(t *testing.T) {
	clients, servers := pairs(t, 1)
	tr, _ := pingTransport(t, pingPolicy(t, 100, 0, nil), clients...)
	p := newPeer(t, servers[0])
	body, w := io.Pipe()
	req, _ := http.NewRequest("POST", "https://dummy.tld/", body)
	req.Trailer = http.Header{"X-Trailer": nil}
	done := fetch(tr, req)
	p.expect("H1")
	w.Write([]byte("a"))
	p.expect("D1:a")
	time.Sleep(200 * time.Millisecond)
	req.Trailer.Set("X-Trailer", "1")
	w.Close()
	got := p.expect("H1e", "P1")
	p.ack(got[1].ping)
	p.answer(1)
	if err := waitFor(t, done); err != nil {
		t.Fatal(err)
	}
}

// TestNoPingGoesWhileOneIsUnansweredAndAnyAcknowledgementAnswersIt holds the
// PING to Chrome's: none goes while one is unanswered; an acknowledgement
// answers it whatever it says, and one more, which answers nothing, is still
// the connection error h2.reject_unsolicited_ping_ack makes it.
func TestNoPingGoesWhileOneIsUnansweredAndAnyAcknowledgementAnswersIt(t *testing.T) {
	clients, servers := pairs(t, 1)
	tr, _ := pingTransport(t, pingPolicy(t, 100, 0, map[string]any{"h2.reject_unsolicited_ping_ack": true}), clients...)
	p := newPeer(t, servers[0])
	exchange(t, tr, p, true, "H1e")
	time.Sleep(200 * time.Millisecond)
	exchange(t, tr, p, false, "H3e", "P1")
	time.Sleep(200 * time.Millisecond)
	exchange(t, tr, p, false, "H5e")
	p.ack([8]byte{0xff})
	time.Sleep(200 * time.Millisecond)
	exchange(t, tr, p, true, "H7e", "P2")
	time.Sleep(50 * time.Millisecond)
	p.ack([8]byte{7})
	p.expect("G0/1:", "EOF")
}

// TestNoPingGoesWhileTheCheckOfOneIsPending: answered at once, a PING's check
// is still pending until h2.preface_ping_hung_ms after it, and none goes
// before, as Chrome's.
func TestNoPingGoesWhileTheCheckOfOneIsPending(t *testing.T) {
	clients, servers := pairs(t, 1)
	tr, _ := pingTransport(t, pingPolicy(t, 100, 600, nil), clients...)
	p := newPeer(t, servers[0])
	exchange(t, tr, p, true, "H1e")
	time.Sleep(200 * time.Millisecond)
	got := exchange(t, tr, p, true, "H3e", "P1")
	time.Sleep(200 * time.Millisecond)
	exchange(t, tr, p, true, "H5e")
	time.Sleep(time.Until(got[1].at.Add(800 * time.Millisecond)))
	exchange(t, tr, p, true, "H7e", "P2")
}

// TestAnUnansweredPingGivesTheConnectionUpAndTheRequestGoesAgain: a PING
// unanswered with nothing read for h2.preface_ping_hung_ms gives the
// connection up: GOAWAY as the profile says, if it says one, then the
// connection closes, and the request, its body again, goes on another
// connection, h2.preface_ping_failed_retries times at most.
func TestAnUnansweredPingGivesTheConnectionUpAndTheRequestGoesAgain(t *testing.T) {
	for _, tc := range []struct {
		name   string
		more   map[string]any
		goaway []string // what the connection says before it closes
		again  bool
	}{
		{"chrome", map[string]any{"h2.preface_ping_failed_goaway_code": 1, "h2.preface_ping_failed_goaway_debug": "Failed ping.", "h2.preface_ping_failed_retries": 2}, []string{"G0/1:Failed ping."}, true},
		{"no-debug", map[string]any{"h2.preface_ping_failed_goaway_code": 2, "h2.preface_ping_failed_retries": 1}, []string{"G0/2:"}, true},
		{"no-goaway", map[string]any{"h2.preface_ping_failed_retries": 1}, nil, true},
		{"no-retry", map[string]any{"h2.preface_ping_failed_goaway_code": 1}, []string{"G0/1:"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clients, servers := pairs(t, 2)
			tr, _ := pingTransport(t, pingPolicy(t, 100, 300, tc.more), clients...)
			a, b := newPeer(t, servers[0]), newPeer(t, servers[1])
			exchange(t, tr, a, true, "H1e")
			time.Sleep(200 * time.Millisecond)
			req, _ := http.NewRequest("POST", "https://dummy.tld/", strings.NewReader("x"))
			done := fetch(tr, req)
			pinged := a.expect("H3", "P1", "D3e:x")[1].at
			closed := a.expect(append(tc.goaway, "EOF")...)
			if took := closed[0].at.Sub(pinged); took < 250*time.Millisecond || took > 1500*time.Millisecond {
				t.Errorf("given up %v after its PING, want 300ms", took)
			}
			if !tc.again {
				if err := waitFor(t, done); err != errPrefacePingFailed {
					t.Fatalf("got %v, want %v", err, errPrefacePingFailed)
				}
				return
			}
			b.expect("H1", "D1e:x")
			b.answer(1)
			if err := waitFor(t, done); err != nil {
				t.Fatal(err)
			}
		})
	}
}

// TestAReadPutsTheGivingUpOff: what is read after the PING, its answer aside,
// puts the giving up off until nothing has been read for
// h2.preface_ping_hung_ms, as Chrome looks again: given 600 ms and read 300
// ms after it, the PING gives the connection up 900 ms after it, not 600 (no
// second look) nor 1200 (a second look as long as the first).
func TestAReadPutsTheGivingUpOff(t *testing.T) {
	clients, servers := pairs(t, 1)
	tr, _ := pingTransport(t, pingPolicy(t, 100, 600, map[string]any{"h2.preface_ping_failed_goaway_code": 1}), clients...)
	p := newPeer(t, servers[0])
	exchange(t, tr, p, true, "H1e")
	time.Sleep(200 * time.Millisecond)
	req, _ := http.NewRequest("GET", "https://dummy.tld/", nil)
	done := fetch(tr, req)
	pinged := p.expect("H3e", "P1")[1].at
	time.Sleep(time.Until(pinged.Add(300 * time.Millisecond)))
	p.answer(3)
	if err := waitFor(t, done); err != nil {
		t.Fatal(err)
	}
	if took := p.expect("G0/1:", "EOF")[0].at.Sub(pinged); took < 750*time.Millisecond || took > 1080*time.Millisecond {
		t.Errorf("given up %v after its PING, want 900ms: 600 after the read 300ms after it", took)
	}
}

// TestAConnectionHeldUpInAWriteIsGivenUpToo: a body the server stopped
// reading holds the connection's writes up; the PING still gives the
// connection up, the write giving up within h2.preface_ping_hung_ms, and the
// request fails rather than wait for good.
func TestAConnectionHeldUpInAWriteIsGivenUpToo(t *testing.T) {
	clients, servers := pairs(t, 1)
	clients[0].(*net.TCPConn).SetWriteBuffer(16 << 10)
	servers[0].(*net.TCPConn).SetReadBuffer(16 << 10)
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		// A server of large windows that reads up to the PING and no more.
		conn := servers[0]
		if _, err := io.ReadFull(conn, make([]byte, len(ClientPreface))); err != nil {
			return
		}
		fr := NewFramer(conn, conn)
		fr.WriteSettings(Setting{ID: SettingInitialWindowSize, Val: 1 << 30})
		fr.WriteWindowUpdate(0, 1<<30)
		for {
			f, err := fr.ReadFrame()
			if err != nil {
				return
			}
			if sf, ok := f.(*SettingsFrame); ok && !sf.IsAck() {
				fr.WriteSettingsAck()
			}
			if _, ok := f.(*PingFrame); ok {
				break
			}
		}
		<-stop
	}()
	pool := &listPool{}
	tr := &Transport{Behavior: pingPolicy(t, 100, 300, map[string]any{"h2.preface_ping_failed_goaway_code": 1}), ConnPool: pool}
	cc, err := tr.NewClientConn(clients[0])
	if err != nil {
		t.Fatal(err)
	}
	pool.conns = []*ClientConn{cc}
	time.Sleep(200 * time.Millisecond)
	req, _ := http.NewRequest("POST", "https://dummy.tld/", bytes.NewReader(make([]byte, 64<<20)))
	began := time.Now()
	_, err = tr.RoundTrip(req)
	if err != errPrefacePingFailed {
		t.Fatalf("got %v, want %v", err, errPrefacePingFailed)
	}
	if took := time.Since(began); took > 2*time.Second {
		t.Errorf("failed %v after it was sent, want 600ms: the PING's and the held write's", took)
	}
}

// TestARequestOnlyForAConnectionHeldMayDialWhenItGoesAgain: a request asked
// only of a connection the pool holds, which was given up for its PING, goes
// again on one dialed for it.
func TestARequestOnlyForAConnectionHeldMayDialWhenItGoesAgain(t *testing.T) {
	clients, servers := pairs(t, 2)
	tr, _ := pingTransport(t, pingPolicy(t, 100, 300, map[string]any{"h2.preface_ping_failed_retries": 1}), clients...)
	a, b := newPeer(t, servers[0]), newPeer(t, servers[1])
	exchange(t, tr, a, true, "H1e")
	time.Sleep(200 * time.Millisecond)
	req, _ := http.NewRequest("GET", "https://dummy.tld/", nil)
	done := make(chan error, 1)
	go func() {
		res, err := tr.RoundTripOpt(req, RoundTripOpt{OnlyCachedConn: true})
		if err == nil {
			res.Body.Close()
		}
		done <- err
	}()
	a.expect("H3e", "P1", "EOF")
	b.expect("H1e")
	b.answer(1)
	if err := waitFor(t, done); err != nil {
		t.Fatal(err)
	}
}

// TestARequestWhoseBodyCannotGoAgainSaysSo: a body without GetBody that began
// to go cannot go again.
func TestARequestWhoseBodyCannotGoAgainSaysSo(t *testing.T) {
	clients, servers := pairs(t, 2)
	tr, _ := pingTransport(t, pingPolicy(t, 100, 300, map[string]any{"h2.preface_ping_failed_retries": 2}), clients...)
	a, _ := newPeer(t, servers[0]), newPeer(t, servers[1])
	exchange(t, tr, a, true, "H1e")
	time.Sleep(200 * time.Millisecond)
	req, _ := http.NewRequest("POST", "https://dummy.tld/", io.MultiReader(strings.NewReader("x")))
	req.ContentLength = 1
	done := fetch(tr, req)
	a.expect("H3", "P1", "D3e:x", "EOF")
	if err := waitFor(t, done); err == nil || !strings.Contains(err.Error(), "define Request.GetBody") {
		t.Fatalf("got %v, want the body's", err)
	}
}

// listPool hands out its connections in order, one for each try.
type listPool struct {
	mu    sync.Mutex
	conns []*ClientConn
	given int
}

func (p *listPool) GetClientConn(*http.Request, string) (*ClientConn, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.given == len(p.conns) {
		return nil, errors.New("no more connections")
	}
	p.given++
	return p.conns[p.given-1], nil
}

func (p *listPool) MarkDead(*ClientConn) {}

// idleConns are n connections of a transport with policy, each to its own
// peer, that have read nothing for longer than the policy's 100 ms.
func idleConns(t *testing.T, n int, policy behavior.Policy) (*Transport, *listPool, []*peer) {
	t.Helper()
	clients, servers := pairs(t, n)
	pool := &listPool{}
	tr := &Transport{Behavior: policy, ConnPool: pool}
	var peers []*peer
	for i := range clients {
		peers = append(peers, newPeer(t, servers[i]))
		cc, err := tr.NewClientConn(clients[i])
		if err != nil {
			t.Fatal(err)
		}
		pool.conns = append(pool.conns, cc)
	}
	time.Sleep(200 * time.Millisecond)
	return tr, pool, peers
}

// TestARequestGoesAgainAsOftenAsTheProfileSaysAndAtOnce: each time the
// connection it went on is given up for its PING the request goes again, at
// once, h2.preface_ping_failed_retries times at most, then fails.
func TestARequestGoesAgainAsOftenAsTheProfileSaysAndAtOnce(t *testing.T) {
	for retries := range 3 {
		t.Run(fmt.Sprint(retries), func(t *testing.T) {
			tr, pool, peers := idleConns(t, 4, pingPolicy(t, 100, 200, map[string]any{"h2.preface_ping_failed_retries": retries}))
			req, _ := http.NewRequest("GET", "https://dummy.tld/", nil)
			began := time.Now()
			_, err := tr.RoundTrip(req)
			took := time.Since(began)
			if err != errPrefacePingFailed {
				t.Fatalf("got %v, want %v", err, errPrefacePingFailed)
			}
			if pool.given != retries+1 {
				t.Fatalf("went on %d connections, want %d", pool.given, retries+1)
			}
			for _, p := range peers[:retries+1] {
				p.expect("H1e", "P1", "EOF")
			}
			if limit := time.Duration(retries+1)*200*time.Millisecond + 500*time.Millisecond; took > limit {
				t.Errorf("took %v: waited between the tries", took)
			}
		})
	}
}

// TestATryAfterAPingIsNotOneOfTheTransportsOwn: after a request went again for
// a PING, the transport's own first try again (a refused stream) is still at
// once, where its second waits a second.
func TestATryAfterAPingIsNotOneOfTheTransportsOwn(t *testing.T) {
	tr, _, peers := idleConns(t, 3, pingPolicy(t, 100, 200, map[string]any{"h2.preface_ping_failed_retries": 2}))
	req, _ := http.NewRequest("GET", "https://dummy.tld/", nil)
	began := time.Now()
	done := fetch(tr, req)
	peers[0].expect("H1e", "P1")
	got := peers[1].expect("H1e", "P1")
	peers[1].ack(got[1].ping)
	peers[1].refuse(1)
	got = peers[2].expect("H1e", "P1")
	peers[2].ack(got[1].ping)
	peers[2].answer(1)
	if err := waitFor(t, done); err != nil {
		t.Fatal(err)
	}
	if took := time.Since(began); took > 900*time.Millisecond {
		t.Errorf("took %v: the refused stream's try waited", took)
	}
}

// TestRewound: a request goes again as it is with no body, or a body not
// begun; with the body GetBody gives; or not at all.
func TestRewound(t *testing.T) {
	failed := errors.New("GetBody failed")
	plain, _ := http.NewRequest("GET", "https://dummy.tld/", nil)
	unread, _ := http.NewRequest("POST", "https://dummy.tld/", io.MultiReader(strings.NewReader("x")))
	given, _ := http.NewRequest("POST", "https://dummy.tld/", strings.NewReader("x"))
	broken, _ := http.NewRequest("POST", "https://dummy.tld/", strings.NewReader("x"))
	broken.GetBody = func() (io.ReadCloser, error) { return nil, failed }
	for _, tc := range []struct {
		name    string
		req     *http.Request
		written bool
		same    bool
		err     string
	}{
		{"no body", plain, true, true, ""},
		{"not begun", unread, false, true, ""},
		{"begun", unread, true, false, "define Request.GetBody"},
		{"GetBody", given, true, false, ""},
		{"GetBody fails", broken, true, false, "GetBody failed"},
	} {
		got, err := rewound(tc.req, errPrefacePingFailed, tc.written)
		if tc.err != "" {
			if err == nil || !strings.Contains(err.Error(), tc.err) {
				t.Errorf("%s: %v, want %q", tc.name, err, tc.err)
			}
			continue
		}
		if err != nil || (got == tc.req) != tc.same {
			t.Errorf("%s: %v, the same request %v", tc.name, err, got == tc.req)
		}
		if tc.name == "GetBody" {
			if b, _ := io.ReadAll(got.Body); string(b) != "x" {
				t.Errorf("GetBody: body %q", b)
			}
		}
	}
}

// Copyright 2026 The fhttp Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package http2

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"strings"
	"testing"
	"time"

	http "github.com/igor-lemon/fhttp"
	"github.com/igor-lemon/fhttp/http2/hpack"
	"github.com/igor-lemon/fhttp/httptrace"
	"github.com/igor-lemon/utls/behavior"
)

// chromePrio is what Chrome 155.0.8059.85 did on one connection of tls-forge's
// review bench (2026-10-23, chrome-prio): a stream opened with a weight, as
// the wire says it, and the stream its HEADERS depended on, or a stream that
// ended (its answer came) before the next was opened.
var chromePrio = []struct {
	open, weight uint8 // a stream opened (its ID) and its weight
	dep          uint32
	end          uint8 // or a stream that ended
}{
	{open: 1, weight: 255},         // the page
	{end: 1},                       // its answer
	{open: 3, weight: 219},         // fetch()
	{open: 5, weight: 146, dep: 3}, // fetch(), priority low
	{open: 7, weight: 255},         // a style sheet
	{open: 9, weight: 219, dep: 3}, // fetch(): 3, not 7, opened after it
	{open: 11, weight: 146, dep: 5},
	{open: 13, weight: 146, dep: 11}, // an async script
	{open: 15, weight: 219, dep: 9},  // fetch(), priority high
	{end: 3},
	{open: 17, weight: 219, dep: 15},
	{open: 19, weight: 255, dep: 7},
	{open: 21, weight: 146, dep: 13},
	{end: 5}, {end: 7}, {end: 9}, {end: 11}, {end: 13}, {end: 15}, {end: 17}, {end: 19}, {end: 21},
	{open: 23, weight: 255}, // the next page
}

// TestStreamsDependAsChromesDid holds the lists to Chrome's measured
// connection: each HEADERS depends on the last open stream of the lowest
// weight not below its own, exclusively as the priority says, and a stream
// that ended is depended on no more.
func TestStreamsDependAsChromesDid(t *testing.T) {
	cc := &ClientConn{nearestWeight: true}
	for _, step := range chromePrio {
		if step.end != 0 {
			cc.undependLocked(uint32(step.end))
			continue
		}
		got := cc.dependOnLocked(uint32(step.open), PriorityParam{Exclusive: true, Weight: step.weight})
		if want := (PriorityParam{StreamDep: step.dep, Exclusive: true, Weight: step.weight}); got != want {
			t.Errorf("stream %d: %+v, want %+v", step.open, got, want)
		}
	}
	if len(cc.weightOf) != 1 || len(cc.byWeight[255]) != 1 || len(cc.byWeight[219])+len(cc.byWeight[146]) != 0 {
		t.Errorf("left %v %v", cc.weightOf, cc.byWeight)
	}
	// Ending a stream twice, or one never opened, changes nothing.
	cc.undependLocked(23)
	cc.undependLocked(23)
	cc.undependLocked(99)
	if len(cc.weightOf) != 0 || len(cc.byWeight[255]) != 0 {
		t.Errorf("left %v %v", cc.weightOf, cc.byWeight)
	}
}

// TestADependencyIsChosenOnlyUnderThePolicy: "root", no policy and a
// priority that says nothing leave a priority as it is and join no list;
// the policy's own choice overrides a dependency given.
func TestADependencyIsChosenOnlyUnderThePolicy(t *testing.T) {
	given := PriorityParam{StreamDep: 7, Exclusive: false, Weight: 100}
	if got := (&ClientConn{}).dependOnLocked(1, given); got != given {
		t.Errorf("root: %+v", got)
	}
	cc := &ClientConn{nearestWeight: true}
	if got := cc.dependOnLocked(1, PriorityParam{}); !got.IsZero() || len(cc.weightOf) != 0 {
		t.Errorf("no priority: %+v, %v", got, cc.weightOf)
	}
	if got := cc.dependOnLocked(3, given); got != (PriorityParam{Exclusive: false, Weight: 100}) {
		t.Errorf("a dependency given: %+v", got)
	}
	if got := cc.dependOnLocked(5, given); got.StreamDep != 3 {
		t.Errorf("after 3: %+v", got)
	}
}

func dependencyPolicy(t *testing.T, mode string) behavior.Policy {
	t.Helper()
	p := behavior.Policy{}
	if mode != "" {
		b, _ := json.Marshal(mode)
		p["h2.priority_dependency"] = b
	}
	if err := p.Validate(); err != nil {
		t.Fatal(err)
	}
	return p
}

// prioritized is a GET whose HEADERS carry weight w, or no priority at all
// for -1, through tr, in the background, cancelled by cancel.
func prioritized(tr *Transport, w int) (done chan error, cancel context.CancelFunc) {
	req, _ := http.NewRequest("GET", "https://dummy.tld/", nil)
	ctx, cancel := context.WithCancel(context.Background())
	priority := PriorityParam{}
	if w >= 0 {
		priority = PriorityParam{Exclusive: true, Weight: uint8(w)}
	}
	return fetch(tr, req.WithContext(WithHeaderPriority(ctx, priority))), cancel
}

// headers reads a HEADERS frame of stream id from p and says what it
// depended on.
func (p *peer) headers(id uint32) PriorityParam {
	p.t.Helper()
	return p.expect(fmt.Sprintf("H%de", id))[0].prio
}

// TestHeadersDependAsChromesOnTheWire sends Chrome's measured connection
// through the transport: what each HEADERS frame says it depends on is what
// Chrome's said; a stream answered is depended on no more.
func TestHeadersDependAsChromesOnTheWire(t *testing.T) {
	clients, servers := pairs(t, 1)
	tr, _ := pingTransport(t, dependencyPolicy(t, "nearest-weight"), clients...)
	p := newPeer(t, servers[0])
	waiting := map[uint8]chan error{}
	for _, step := range chromePrio {
		if step.end != 0 {
			p.answer(uint32(step.end))
			if err := waitFor(t, waiting[step.end]); err != nil {
				t.Fatal(err)
			}
			continue
		}
		waiting[step.open], _ = prioritized(tr, int(step.weight))
		if got, want := p.headers(uint32(step.open)), (PriorityParam{StreamDep: step.dep, Exclusive: true, Weight: step.weight}); got != want {
			t.Errorf("stream %d: %+v, want %+v", step.open, got, want)
		}
	}
	p.answer(23)
	if err := waitFor(t, waiting[23]); err != nil {
		t.Fatal(err)
	}
}

// TestAStreamResetIsDependedOnNoMore: a stream the client cancels, or the
// server resets, leaves its list; a request whose HEADERS say no priority
// joins none.
func TestAStreamResetIsDependedOnNoMore(t *testing.T) {
	clients, servers := pairs(t, 1)
	tr, _ := pingTransport(t, dependencyPolicy(t, "nearest-weight"), clients...)
	p := newPeer(t, servers[0])
	one, _ := prioritized(tr, 219)
	p.headers(1)
	_, cancel := prioritized(tr, 219)
	if got := p.headers(3); got.StreamDep != 1 {
		t.Fatalf("3 depends on %d", got.StreamDep)
	}
	cancel()
	p.expect(fmt.Sprintf("R3/%d", ErrCodeCancel))
	five, _ := prioritized(tr, -1)
	if got := p.headers(5); !got.IsZero() {
		t.Fatalf("5 without a priority: %+v", got)
	}
	seven, _ := prioritized(tr, 219)
	if got := p.headers(7); got.StreamDep != 1 {
		t.Fatalf("7 depends on %d, want 1: 3 was cancelled, 5 said no priority", got.StreamDep)
	}
	// INTERNAL_ERROR, which the transport sends no request again for.
	p.write(func() error { return p.fr.WriteRSTStream(1, ErrCodeInternal) })
	if err := waitFor(t, one); err == nil {
		t.Fatal("a stream the server reset answered")
	}
	nine, _ := prioritized(tr, 219)
	if got := p.headers(9); got.StreamDep != 7 {
		t.Fatalf("9 depends on %d, want 7: 1 was reset", got.StreamDep)
	}
	for _, id := range []uint32{5, 7, 9} {
		p.answer(id)
	}
	for _, done := range []chan error{five, seven, nine} {
		if err := waitFor(t, done); err != nil {
			t.Fatal(err)
		}
	}
}

// connOf is the connection tr's pool holds, or nil.
func connOf(tr *Transport) *ClientConn {
	pool := tr.connPool().(*clientConnPool)
	pool.mu.Lock()
	defer pool.mu.Unlock()
	for _, conns := range pool.conns {
		return conns[0]
	}
	return nil
}

// head answers stream with its head alone, its body to come.
func (p *peer) head(stream uint32) {
	var block bytes.Buffer
	hpack.NewEncoder(&block).WriteField(hpack.HeaderField{Name: ":status", Value: "200"})
	p.write(func() error {
		return p.fr.WriteHeaders(HeadersFrameParam{StreamID: stream, EndHeaders: true, BlockFragment: block.Bytes()})
	})
}

// headed is a GET of weight w through tr whose answer's head is p's, its
// body to come.
func headed(t *testing.T, tr *Transport, p *peer, stream uint32, w uint8) *http.Response {
	t.Helper()
	req, _ := http.NewRequest("GET", "https://dummy.tld/", nil)
	got := make(chan *http.Response, 1)
	go func() {
		res, err := tr.RoundTrip(req.WithContext(WithHeaderPriority(context.Background(), PriorityParam{Exclusive: true, Weight: w})))
		if err != nil {
			t.Error(err)
		}
		got <- res
	}()
	p.expect(fmt.Sprintf("H%de", stream))
	p.head(stream)
	select {
	case res := <-got:
		if res == nil {
			t.FailNow()
		}
		return res
	case <-time.After(5 * time.Second):
		t.Fatal("no answer")
	}
	return nil
}

// TestAStreamItsAnswerEndedIsDependedOnNoMoreBeforeTheAnswerIsHanded: the
// stream of a request with a body that its answer's HEADERS end leaves its
// list before the answer reaches whoever waits for it, who may send the next
// request at once, as a form's redirect is: the transport forgets such a
// stream only once it has handed the answer on, and the hop of tls-forge's
// redir-form-302 depended on the form's stream on a Linux runner (CI run
// 40222769334), where Chrome's depended on none.
func TestAStreamItsAnswerEndedIsDependedOnNoMoreBeforeTheAnswerIsHanded(t *testing.T) {
	clients, servers := pairs(t, 1)
	tr, _ := pingTransport(t, dependencyPolicy(t, "nearest-weight"), clients...)
	p := newPeer(t, servers[0])
	listed := make(chan bool, 1)
	trace := &httptrace.ClientTrace{GotFirstResponseByte: func() {
		// On the connection's read loop, which hands the answer on next.
		cc := connOf(tr)
		if cc == nil {
			listed <- true
			return
		}
		cc.mu.Lock()
		_, still := cc.weightOf[1]
		cc.mu.Unlock()
		listed <- still
	}}
	req, _ := http.NewRequest("POST", "https://dummy.tld/", strings.NewReader("x"))
	ctx := httptrace.WithClientTrace(WithHeaderPriority(context.Background(), PriorityParam{Exclusive: true, Weight: 255}), trace)
	done := fetch(tr, req.WithContext(ctx))
	p.expect("H1", "D1e:x")
	p.answer(1)
	if err := waitFor(t, done); err != nil {
		t.Fatal(err)
	}
	if <-listed {
		t.Error("the answer was handed on while its stream was depended on")
	}
	next, _ := prioritized(tr, 255)
	if got := p.headers(3); got.StreamDep != 0 {
		t.Errorf("3 depends on %d", got.StreamDep)
	}
	p.answer(3)
	if err := waitFor(t, next); err != nil {
		t.Fatal(err)
	}
}

// TestAStreamLetGoOfIsDependedOnNoMore: an answer let go of with
// http.DrainBody, its body still to come, is a stream Chrome has closed: the
// next request's HEADERS depend on it no more, though its body is read on
// apart from them. One whose body is still read is depended on.
func TestAStreamLetGoOfIsDependedOnNoMore(t *testing.T) {
	clients, servers := pairs(t, 1)
	tr, _ := pingTransport(t, dependencyPolicy(t, "nearest-weight"), clients...)
	p := newPeer(t, servers[0])
	read := headed(t, tr, p, 1, 255)
	let := headed(t, tr, p, 3, 255)
	drained := http.DrainBody(context.Background(), let, math.MaxInt64, 5*time.Second, 0)
	next, _ := prioritized(tr, 255)
	if got := p.headers(5); got.StreamDep != 1 {
		t.Errorf("5 depends on %d, want 1: 3 was let go of, and 1 is still read", got.StreamDep)
	}
	// The body let go of comes, ends its stream and is read apart from the
	// requests.
	p.write(func() error { return p.fr.WriteData(3, true, []byte("rest")) })
	select {
	case <-drained:
	case <-time.After(5 * time.Second):
		t.Fatal("the body let go of was not read")
	}
	p.write(func() error { return p.fr.WriteData(1, true, []byte("rest")) })
	if b, err := io.ReadAll(read.Body); err != nil || string(b) != "rest" {
		t.Errorf("read %q, %v", b, err)
	}
	p.answer(5)
	if err := waitFor(t, next); err != nil {
		t.Fatal(err)
	}
}

// TestAStreamAwaitedIsDependedOnUntilItsFirstBytes: an answer awaited with
// http.AwaitBody, a preflight's, is a stream Chrome keeps open until the
// first bytes of its body come (tls-forge's bench measured a preflight's
// kept 7 s and reset at its first bytes): the next request's HEADERS depend
// on it until then.
func TestAStreamAwaitedIsDependedOnUntilItsFirstBytes(t *testing.T) {
	clients, servers := pairs(t, 1)
	tr, _ := pingTransport(t, dependencyPolicy(t, "nearest-weight"), clients...)
	p := newPeer(t, servers[0])
	awaited := http.AwaitBody(context.Background(), headed(t, tr, p, 1, 255), 5*time.Second, 0)
	next, _ := prioritized(tr, 255)
	if got := p.headers(3); got.StreamDep != 1 {
		t.Errorf("3 depends on %d, want 1: its body is awaited", got.StreamDep)
	}
	p.write(func() error { return p.fr.WriteData(1, false, []byte("first")) })
	select {
	case <-awaited:
	case <-time.After(5 * time.Second):
		t.Fatal("the body awaited was not let go of at its first bytes")
	}
	p.expect(fmt.Sprintf("R1/%d", ErrCodeCancel))
	p.answer(3)
	if err := waitFor(t, next); err != nil {
		t.Fatal(err)
	}
}

// TestEveryStreamDependsOnNoneWithoutThePolicy: "root" and no policy leave
// every HEADERS on stream 0, however many are open.
func TestEveryStreamDependsOnNoneWithoutThePolicy(t *testing.T) {
	for _, mode := range []string{"", "root"} {
		t.Run(mode, func(t *testing.T) {
			clients, servers := pairs(t, 1)
			tr, _ := pingTransport(t, dependencyPolicy(t, mode), clients...)
			p := newPeer(t, servers[0])
			var waiting []chan error
			for i, w := range []int{219, 146, 219, 255} {
				done, _ := prioritized(tr, w)
				waiting = append(waiting, done)
				if got := p.headers(uint32(2*i + 1)); got.StreamDep != 0 || got.Weight != uint8(w) || !got.Exclusive {
					t.Errorf("stream %d: %+v", 2*i+1, got)
				}
			}
			for i, done := range waiting {
				p.answer(uint32(2*i + 1))
				if err := waitFor(t, done); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}

// TestAnAnswerWithoutABodyIsReadByManyAtOnce: the body every answer without
// one shares is read at once by many, as io.Copy reads it (through WriteTo
// where there is one), under -race, and says EOF.
func TestAnAnswerWithoutABodyIsReadByManyAtOnce(t *testing.T) {
	done := make(chan error)
	for range 8 {
		go func() {
			n, err := io.Copy(io.Discard, noBody)
			if n != 0 || err != nil {
				err = fmt.Errorf("copied %d, %v", n, err)
			} else if n, err := noBody.Read(make([]byte, 8)); n != 0 || err != io.EOF {
				err = fmt.Errorf("read %d, %v", n, err)
			} else {
				err = noBody.Close()
			}
			done <- err
		}()
	}
	for range 8 {
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}
}

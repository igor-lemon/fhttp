// Copyright 2026 The fhttp Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package http2

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"testing"

	http "github.com/igor-lemon/fhttp"
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

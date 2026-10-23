// Copyright 2026 The fhttp Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package http2

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"testing"
	"time"

	http "github.com/igor-lemon/fhttp"
	"github.com/igor-lemon/fhttp/http2/hpack"
	"github.com/igor-lemon/utls/behavior"
)

// headersOnly answers stream with a 200 whose body is still to come.
func (p *peer) headersOnly(stream uint32) {
	var block bytes.Buffer
	hpack.NewEncoder(&block).WriteField(hpack.HeaderField{Name: ":status", Value: "200"})
	p.write(func() error {
		return p.fr.WriteHeaders(HeadersFrameParam{StreamID: stream, EndHeaders: true, BlockFragment: block.Bytes()})
	})
}

// opened is a GET through tr whose answer's head the peer gave and whose body
// is still to come, with the cancel of its context.
func opened(t *testing.T, tr *Transport, p *peer) (*http.Response, context.CancelFunc) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, "GET", "https://dummy.tld/", nil)
	answered := make(chan *http.Response, 1)
	go func() {
		res, err := tr.RoundTrip(req)
		if err != nil {
			res = nil
		}
		answered <- res
	}()
	stream := p.expect("H1e")[0].stream
	p.headersOnly(stream)
	select {
	case res := <-answered:
		if res == nil {
			t.Fatal("no answer")
		}
		return res, cancel
	case <-time.After(5 * time.Second):
		t.Fatal("no answer")
	}
	return nil, cancel
}

// noReset says the peer read no RST_STREAM within d.
func (p *peer) noReset(t *testing.T, d time.Duration) {
	t.Helper()
	deadline := time.After(d)
	for {
		select {
		case s, ok := <-p.seen:
			if !ok {
				return
			}
			if s.kind == FrameRSTStream {
				t.Fatalf("%v", s)
			}
		case <-deadline:
			return
		}
	}
}

// TestADetachedStreamOutlivesItsRequest: after Detach the request's context
// ending breaks neither the stream nor its body, which is read to its end when
// it comes, with no RST_STREAM; without, the context ending breaks the body.
func TestADetachedStreamOutlivesItsRequest(t *testing.T) {
	for _, detach := range []bool{true, false} {
		t.Run(fmt.Sprint(detach), func(t *testing.T) {
			clients, servers := pairs(t, 1)
			tr, _ := pingTransport(t, behavior.Policy{}, clients...)
			p := newPeer(t, servers[0])
			res, cancel := opened(t, tr, p)
			if detach {
				res.Body.(interface{ Detach() }).Detach()
				res.Body.(interface{ Detach() }).Detach() // twice is once
			}
			cancel()
			time.Sleep(50 * time.Millisecond)
			p.write(func() error { return p.fr.WriteData(1, true, []byte("abc")) })
			b, err := io.ReadAll(res.Body)
			if detach && (err != nil || string(b) != "abc") {
				t.Fatalf("detached: %q, %v", b, err)
			}
			if !detach && err == nil {
				t.Fatal("the context ending did not break the body")
			}
			if detach {
				res.Body.Close()
				p.noReset(t, 200*time.Millisecond)
			}
		})
	}
}

// TestDrainBodyReadsAStreamApartFromItsRequest: http.DrainBody of an HTTP/2
// answer reads its body in the background past its request's context, and a
// stream that ends in time is reset by no one; one that does not is reset
// when the time is up.
func TestDrainBodyReadsAStreamApartFromItsRequest(t *testing.T) {
	for _, inTime := range []bool{true, false} {
		t.Run(fmt.Sprint(inTime), func(t *testing.T) {
			clients, servers := pairs(t, 1)
			tr, _ := pingTransport(t, behavior.Policy{}, clients...)
			p := newPeer(t, servers[0])
			res, cancel := opened(t, tr, p)
			began := time.Now()
			http.DrainBody(context.Background(), res, 16384, 300*time.Millisecond, 0)
			if res.Body != http.NoBody || time.Since(began) > 100*time.Millisecond {
				t.Fatalf("DrainBody kept the body, or took %v", time.Since(began))
			}
			cancel()
			if !inTime {
				reset := p.expect(fmt.Sprintf("R1/%d", ErrCodeCancel))[0].at
				if took := reset.Sub(began); took < 250*time.Millisecond || took > time.Second {
					t.Errorf("reset %v after, want the 300ms given", took)
				}
				return
			}
			time.Sleep(100 * time.Millisecond)
			p.write(func() error { return p.fr.WriteData(1, true, bytes.Repeat([]byte("x"), 4096)) })
			p.noReset(t, 500*time.Millisecond)
		})
	}
}

// TestAnAwaitedStreamIsLetGoOfAtItsFirstBytes: http.AwaitBody of an HTTP/2
// answer waits for its first DATA apart from its request: a stream that did
// not end with it, or within the grace after, is reset (CANCEL) then, one
// that did is not; the time running out, or ctx ending, resets it.
func TestAnAwaitedStreamIsLetGoOfAtItsFirstBytes(t *testing.T) {
	data := func(end bool) func(p *peer) {
		return func(p *peer) { p.write(func() error { return p.fr.WriteData(1, end, []byte("abc")) }) }
	}
	for _, tc := range []struct {
		name           string
		timeout, grace time.Duration
		cancel         bool
		sends          []func(p *peer) // 150ms apart, the first 150ms after AwaitBody
		reset          time.Duration   // when the stream is reset, about; 0 for not
	}{
		{"ended with them", 0, 0, false, []func(*peer){data(true)}, 0},
		{"not ended with them", 0, 0, false, []func(*peer){data(false)}, 150 * time.Millisecond},
		{"ended within the grace", 0, 300 * time.Millisecond, false, []func(*peer){data(false), data(true)}, 0},
		{"not ended within the grace", 0, 100 * time.Millisecond, false, []func(*peer){data(false), data(true)}, 250 * time.Millisecond},
		{"past the time", 100 * time.Millisecond, 0, false, nil, 100 * time.Millisecond},
		{"ctx ended", 0, 0, true, nil, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clients, servers := pairs(t, 1)
			tr, _ := pingTransport(t, behavior.Policy{}, clients...)
			p := newPeer(t, servers[0])
			res, cancel := opened(t, tr, p)
			ctx, end := context.WithCancel(context.Background())
			defer end()
			began := time.Now()
			http.AwaitBody(ctx, res, tc.timeout, tc.grace)
			if res.Body != http.NoBody || time.Since(began) > 100*time.Millisecond {
				t.Fatalf("AwaitBody kept the body, or took %v", time.Since(began))
			}
			cancel() // the request's: it ends nothing now
			if tc.cancel {
				time.AfterFunc(100*time.Millisecond, end)
				tc.reset = 100 * time.Millisecond
			}
			for _, send := range tc.sends {
				time.Sleep(150 * time.Millisecond)
				send(p)
			}
			if tc.reset == 0 {
				p.noReset(t, 400*time.Millisecond)
				return
			}
			at := p.expect(fmt.Sprintf("R1/%d", ErrCodeCancel))[0].at
			if took := at.Sub(began); took < tc.reset-50*time.Millisecond || took > tc.reset+300*time.Millisecond {
				t.Errorf("reset %v after, want about %v", took, tc.reset)
			}
		})
	}
}

// TestALetGoStreamWaitsItsGrace: http.DrainBody given its grace as its time,
// as a body is let go of at once, returns once the stream ends within it,
// reset by no one, and resets it when the grace is over.
func TestALetGoStreamWaitsItsGrace(t *testing.T) {
	for _, ends := range []bool{true, false} {
		t.Run(fmt.Sprint(ends), func(t *testing.T) {
			clients, servers := pairs(t, 1)
			tr, _ := pingTransport(t, behavior.Policy{}, clients...)
			p := newPeer(t, servers[0])
			res, _ := opened(t, tr, p)
			if ends {
				time.AfterFunc(50*time.Millisecond, func() {
					p.write(func() error { return p.fr.WriteData(1, true, []byte("abc")) })
				})
			}
			began := time.Now()
			http.DrainBody(context.Background(), res, 1<<62, 300*time.Millisecond, 300*time.Millisecond)
			took := time.Since(began)
			if ends {
				if took < 40*time.Millisecond || took > 250*time.Millisecond {
					t.Errorf("DrainBody returned %v after, want once the stream ended", took)
				}
				p.noReset(t, 400*time.Millisecond)
				return
			}
			if took < 250*time.Millisecond || took > 600*time.Millisecond {
				t.Errorf("DrainBody returned %v after, want the 300ms grace", took)
			}
			at := p.expect(fmt.Sprintf("R1/%d", ErrCodeCancel))[0].at
			if reset := at.Sub(began); reset < 250*time.Millisecond || reset > 600*time.Millisecond {
				t.Errorf("reset %v after, want the 300ms grace", reset)
			}
		})
	}
}

// TestARequestEndingOnceItsStreamWasLetGoOfEndsNothing: the request's context
// ending once its body was detached is passed over even when it is seen
// before the detaching: no RST_STREAM, and the body is read to its end.
func TestARequestEndingOnceItsStreamWasLetGoOfEndsNothing(t *testing.T) {
	clients, servers := pairs(t, 1)
	tr, _ := pingTransport(t, behavior.Policy{}, clients...)
	p := newPeer(t, servers[0])
	res, cancel := opened(t, tr, p)
	cs := res.Body.(transportResponseBody).cs
	cs.cc.mu.Lock()
	cs.detached = true
	cs.cc.mu.Unlock()
	cancel()
	time.Sleep(50 * time.Millisecond)
	p.write(func() error { return p.fr.WriteData(1, true, []byte("abc")) })
	if b, err := io.ReadAll(res.Body); err != nil || string(b) != "abc" {
		t.Fatalf("%q, %v", b, err)
	}
	res.Body.Close()
	p.noReset(t, 200*time.Millisecond)
}

// Copyright 2026 The fhttp Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package http2

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"sync"
	"testing"
	"time"

	http "github.com/igor-lemon/fhttp"
	"github.com/igor-lemon/fhttp/http2/hpack"
)

// testClock is the time the transport reads, which a test turns.
type testClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *testClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *testClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// windowUpdate is a WINDOW_UPDATE the client sent.
type windowUpdate struct{ stream, increment uint32 }

// windowTest is a clientTester whose transport has Chrome's windows, 6 MiB a
// stream and 15 MiB the connection, and tells the server of them as Chrome
// does, on the test's clock; its server keeps every WINDOW_UPDATE after the
// connection's first.
type windowTest struct {
	*clientTester
	clock   *testClock
	updates chan windowUpdate
	headers chan uint32
	resets  chan uint32
}

func newWindowTest(t *testing.T) *windowTest {
	ct := newClientTester(t)
	ct.tr.Settings = map[SettingID]uint32{SettingInitialWindowSize: 6291456}
	ct.tr.SettingsOrder = []SettingID{SettingInitialWindowSize}
	ct.tr.ConnectionFlow = 15663105
	ct.tr.PseudoHeaderOrder = []string{":method", ":authority", ":scheme", ":path"}
	ct.tr.WindowUpdateDelay = 5 * time.Second
	clock := &testClock{now: time.Unix(1_000_000_000, 0)}
	old := timeNow
	timeNow = clock.Now
	t.Cleanup(func() { timeNow = old })
	return &windowTest{clientTester: ct, clock: clock, updates: make(chan windowUpdate, 64),
		headers: make(chan uint32, 4), resets: make(chan uint32, 4)}
}

// serve greets the client with peerWindow as the server's
// SETTINGS_INITIAL_WINDOW_SIZE, and reads what it sends from then on.
func (w *windowTest) serve(peerWindow uint32) {
	w.greet(Setting{ID: SettingInitialWindowSize, Val: peerWindow})
	go func() {
		first := true
		for {
			f, err := w.fr.ReadFrame()
			if err != nil {
				close(w.updates)
				return
			}
			switch f := f.(type) {
			case *WindowUpdateFrame:
				if first && f.StreamID == 0 {
					// The connection's window, at the start.
					first = false
					continue
				}
				w.updates <- windowUpdate{f.StreamID, f.Increment}
			case *HeadersFrame:
				w.headers <- f.StreamID
			case *RSTStreamFrame:
				w.resets <- f.StreamID
			}
		}
	}()
}

// answer waits for a request and answers it with a 200, its body to come.
func (w *windowTest) answer() (uint32, error) {
	select {
	case sid := <-w.headers:
		var buf bytes.Buffer
		enc := hpack.NewEncoder(&buf)
		_ = enc.WriteField(hpack.HeaderField{Name: ":status", Value: "200"})
		return sid, w.fr.WriteHeaders(HeadersFrameParam{StreamID: sid, EndHeaders: true, BlockFragment: buf.Bytes()})
	case <-time.After(5 * time.Second):
		return 0, errors.New("no request came")
	}
}

// data sends n bytes of sid's body in frames of 16 KiB, the last ending the
// stream when end says so.
func (w *windowTest) data(sid uint32, n int, end bool) error {
	if n == 0 {
		return w.fr.WriteData(sid, end, nil)
	}
	chunk := make([]byte, 16384)
	for n > 0 {
		size := min(n, len(chunk))
		n -= size
		if err := w.fr.WriteData(sid, end && n == 0, chunk[:size]); err != nil {
			return err
		}
	}
	return nil
}

// expect holds the client to sending want, in order, and nothing else for a
// moment after.
func (w *windowTest) expect(want ...windowUpdate) error {
	var got []windowUpdate
	for len(got) < len(want) {
		select {
		case u, ok := <-w.updates:
			if !ok {
				return fmt.Errorf("the connection ended; sent %v, want %v", got, want)
			}
			got = append(got, u)
		case <-time.After(5 * time.Second):
			return fmt.Errorf("sent %v, want %v", got, want)
		}
	}
	select {
	case u, ok := <-w.updates:
		if ok {
			got = append(got, u)
		}
	case <-time.After(200 * time.Millisecond):
	}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		return fmt.Errorf("sent %v, want %v", got, want)
	}
	return nil
}

// get asks for https://dummy.tld/ and is the answer.
func (w *windowTest) get() (*http.Response, error) {
	req, _ := http.NewRequest("GET", "https://dummy.tld/", nil)
	return w.tr.RoundTrip(req)
}

// read reads n bytes of body in reads of 16 KiB.
func read(body io.Reader, n int) error {
	buf := make([]byte, 16384)
	for n > 0 {
		size := min(n, len(buf))
		if _, err := io.ReadFull(body, buf[:size]); err != nil {
			return err
		}
		n -= size
	}
	return nil
}

// ended holds body to being at its end.
func ended(body io.Reader) error {
	if n, err := body.Read(make([]byte, 1)); n != 0 || err != io.EOF {
		return fmt.Errorf("after the body: %d, %v", n, err)
	}
	return nil
}

func TestAStreamIsToldOfItsWindowOnceMoreThanHalfOfItIsFreed(t *testing.T) {
	// Chrome tells a stream of the window its reader frees once more than
	// half of the 6 MiB it advertised is untold, whatever window the server
	// itself advertised.
	for _, peerWindow := range []uint32{65535, 65536, 1 << 20, 16 << 20} {
		t.Run(fmt.Sprint(peerWindow), func(t *testing.T) {
			w := newWindowTest(t)
			done := make(chan struct{})
			w.client = func() error {
				defer close(done)
				res, err := w.get()
				if err != nil {
					return err
				}
				defer res.Body.Close()
				if err := read(res.Body, 4<<20); err != nil {
					return err
				}
				return ended(res.Body)
			}
			w.server = func() error {
				w.serve(peerWindow)
				sid, err := w.answer()
				if err != nil {
					return err
				}
				if err := w.data(sid, 4<<20-16384, false); err != nil {
					return err
				}
				// Read in 16 KiB, 3 MiB is half and the next read more.
				if err := w.expect(windowUpdate{sid, 3<<20 + 16384}); err != nil {
					return err
				}
				if err := w.data(sid, 16384, true); err != nil {
					return err
				}
				<-done
				return w.expect()
			}
			w.run()
		})
	}
}

func TestTheConnectionIsToldOfItsWindowOnceMoreThanHalfOfItIsFreed(t *testing.T) {
	// Its window is the RFC's 65535 and the 15663105 the transport gave it,
	// 15 MiB: told once more than 7.5 MiB is freed and untold. 16 MiB, past
	// the windows as they began, read in 16 KiB.
	w := newWindowTest(t)
	done := make(chan struct{})
	w.client = func() error {
		defer close(done)
		res, err := w.get()
		if err != nil {
			return err
		}
		defer res.Body.Close()
		if err := read(res.Body, 16<<20); err != nil {
			return err
		}
		return ended(res.Body)
	}
	w.server = func() error {
		w.serve(65535)
		sid, err := w.answer()
		if err != nil {
			return err
		}
		stream, conn := windowUpdate{sid, 3<<20 + 16384}, windowUpdate{0, 15<<19 + 16384}
		want := []windowUpdate{stream, stream, conn, stream, stream, conn, stream}
		got, err := w.flow(sid, 16<<20, len(want))
		if err != nil {
			return err
		}
		if fmt.Sprint(got) != fmt.Sprint(want) {
			return fmt.Errorf("sent %v, want %v", got, want)
		}
		if err := w.data(sid, 0, true); err != nil {
			return err
		}
		<-done
		return w.expect()
	}
	w.run()
}

// flow sends n bytes of sid's body as the client's windows let it, 6 MiB the
// stream's and 15 MiB the connection's to begin with, grown by what it tells,
// and is what it told until it has told count WINDOW_UPDATEs.
func (w *windowTest) flow(sid uint32, n, count int) ([]windowUpdate, error) {
	connWin, streamWin := 65535+15663105, 6291456
	var got []windowUpdate
	chunk := make([]byte, 16384)
	for n > 0 || len(got) < count {
		if n > 0 && connWin > 0 && streamWin > 0 {
			size := min(n, len(chunk), connWin, streamWin)
			if err := w.fr.WriteData(sid, false, chunk[:size]); err != nil {
				return got, err
			}
			n, connWin, streamWin = n-size, connWin-size, streamWin-size
			continue
		}
		select {
		case u, ok := <-w.updates:
			if !ok {
				return got, fmt.Errorf("the connection ended; sent %v", got)
			}
			got = append(got, u)
			if u.stream == 0 {
				connWin += int(u.increment)
			} else {
				streamWin += int(u.increment)
			}
		case <-time.After(5 * time.Second):
			return got, fmt.Errorf("sent %v, and nothing more", got)
		}
	}
	return got, nil
}

func TestAWindowIsToldOfMoreThanFiveSecondsOnHoweverLittleIsFreed(t *testing.T) {
	// The first byte freed more than five seconds after the peer was last
	// told, or after the connection or stream began, tells it of all that is
	// untold: the connection's first. Five seconds is not more. A stream the
	// server ended is told nothing more.
	w := newWindowTest(t)
	step := make(chan struct{})
	done := make(chan struct{})
	w.client = func() error {
		defer close(done)
		res, err := w.get()
		if err != nil {
			return err
		}
		defer res.Body.Close()
		for _, s := range []struct {
			read  int
			after time.Duration
		}{{64 << 10, 6 * time.Second}, {64 << 10, 5 * time.Second}, {16384, time.Nanosecond}, {16384, 0}} {
			if err := read(res.Body, s.read); err != nil {
				return err
			}
			w.clock.advance(s.after)
			step <- struct{}{}
		}
		return ended(res.Body)
	}
	w.server = func() error {
		w.serve(65535)
		sid, err := w.answer()
		if err != nil {
			return err
		}
		if err := w.data(sid, 64<<10, false); err != nil {
			return err
		}
		<-step
		if err := w.expect(); err != nil {
			return err
		}
		// Six seconds on: the part read before, and the first read of this.
		if err := w.data(sid, 64<<10, false); err != nil {
			return err
		}
		if err := w.expect(windowUpdate{0, 64<<10 + 16384}, windowUpdate{sid, 64<<10 + 16384}); err != nil {
			return err
		}
		<-step
		// Five seconds on, exactly: nothing.
		if err := w.data(sid, 16384, false); err != nil {
			return err
		}
		<-step
		if err := w.expect(); err != nil {
			return err
		}
		// A moment more, on the frame that ends the stream: the
		// connection's alone.
		if err := w.data(sid, 16384, true); err != nil {
			return err
		}
		<-step
		<-done
		return w.expect(windowUpdate{0, 48<<10 + 16384 + 16384})
	}
	w.run()
}

func TestAStreamIsToldOfItsWindowFromWhenItBegan(t *testing.T) {
	// A request six seconds into a connection: the connection is told of the
	// first byte freed, and the stream, just begun, is not.
	w := newWindowTest(t)
	step := make(chan struct{})
	w.client = func() error {
		res, err := w.get()
		if err != nil {
			return err
		}
		if err := read(res.Body, 1000); err != nil {
			return err
		}
		if err := ended(res.Body); err != nil {
			return err
		}
		res.Body.Close()
		w.clock.advance(6 * time.Second)
		res, err = w.get()
		if err != nil {
			return err
		}
		defer res.Body.Close()
		if err := read(res.Body, 2000); err != nil {
			return err
		}
		step <- struct{}{}
		return ended(res.Body)
	}
	w.server = func() error {
		w.serve(65535)
		sid, err := w.answer()
		if err != nil {
			return err
		}
		if err := w.data(sid, 1000, true); err != nil {
			return err
		}
		if sid, err = w.answer(); err != nil {
			return err
		}
		if err := w.data(sid, 2000, false); err != nil {
			return err
		}
		<-step
		if err := w.expect(windowUpdate{0, 3000}); err != nil {
			return err
		}
		return w.data(sid, 0, true)
	}
	w.run()
}

func TestWhatABodyClosedUnreadAndAResetStreamHeldIsFreedAndToldOfByTheRule(t *testing.T) {
	// Chrome takes a body it never reads as freed when it lets it go, and what
	// comes for a stream it reset as freed at once, and tells of either as of
	// any: the connection's window, once more than half is untold, or later.
	w := newWindowTest(t)
	step := make(chan struct{})
	w.client = func() error {
		res, err := w.get()
		if err != nil {
			return err
		}
		if err := read(res.Body, 16384); err != nil {
			return err
		}
		res.Body.Close()
		// What it held is no reader's now, and the stream not the
		// connection's.
		if n, err := res.Body.Read(make([]byte, 1)); n != 0 || err == nil {
			return fmt.Errorf("read after Close: %d, %v", n, err)
		}
		cs := res.Body.(transportResponseBody).cs
		cs.cc.mu.Lock()
		_, kept := cs.cc.streams[cs.ID]
		cs.cc.mu.Unlock()
		select {
		case <-cs.done:
		default:
			kept = true
		}
		if kept {
			return errors.New("the stream of a body let go of is the connection's still")
		}
		<-step
		w.clock.advance(6 * time.Second)
		res, err = w.get()
		if err != nil {
			return err
		}
		defer res.Body.Close()
		if err := read(res.Body, 16384); err != nil {
			return err
		}
		return ended(res.Body)
	}
	w.server = func() error {
		w.serve(65535)
		sid, err := w.answer()
		if err != nil {
			return err
		}
		if err := w.data(sid, 64<<10, false); err != nil {
			return err
		}
		select {
		case reset := <-w.resets:
			if reset != sid {
				return fmt.Errorf("reset %d, want %d", reset, sid)
			}
		case <-time.After(5 * time.Second):
			return errors.New("the closed body's stream was not reset")
		}
		if err := w.data(sid, 16384, false); err != nil {
			return err
		}
		if err := w.expect(); err != nil {
			return err
		}
		step <- struct{}{}
		next, err := w.answer()
		if err != nil {
			return err
		}
		if err := w.data(next, 16384, true); err != nil {
			return err
		}
		return w.expect(windowUpdate{0, 64<<10 + 16384 + 16384})
	}
	w.run()
}

func TestPaddingIsFreedAtOnceAndToldOfByTheRule(t *testing.T) {
	// Five bytes of padding and the byte saying so, freed at once, and told
	// of with what the reader frees later, not on their own.
	w := newWindowTest(t)
	step := make(chan struct{})
	w.client = func() error {
		res, err := w.get()
		if err != nil {
			return err
		}
		defer res.Body.Close()
		if err := read(res.Body, 5000); err != nil {
			return err
		}
		w.clock.advance(6 * time.Second)
		step <- struct{}{}
		if err := read(res.Body, 1000); err != nil {
			return err
		}
		w.clock.advance(6 * time.Second)
		step <- struct{}{}
		if err := read(res.Body, 100); err != nil {
			return err
		}
		return ended(res.Body)
	}
	w.server = func() error {
		w.serve(65535)
		sid, err := w.answer()
		if err != nil {
			return err
		}
		if err := w.fr.WriteDataPadded(sid, false, make([]byte, 5000), make([]byte, 5)); err != nil {
			return err
		}
		<-step
		if err := w.expect(); err != nil {
			return err
		}
		if err := w.data(sid, 1000, false); err != nil {
			return err
		}
		if err := w.expect(windowUpdate{0, 6006}, windowUpdate{sid, 6006}); err != nil {
			return err
		}
		<-step
		// Padding freed six seconds on tells of itself, the connection's
		// first.
		if err := w.fr.WriteDataPadded(sid, false, make([]byte, 100), make([]byte, 9)); err != nil {
			return err
		}
		if err := w.expect(windowUpdate{0, 10}, windowUpdate{sid, 10}); err != nil {
			return err
		}
		return w.data(sid, 0, true)
	}
	w.run()
}

func TestABodyEndedAndLetGoOfIsToldOfByTheRule(t *testing.T) {
	// The server ended it, so no reset; what it held unread is freed, and
	// told of when its time has come.
	w := newWindowTest(t)
	sent, done := make(chan struct{}), make(chan struct{})
	w.client = func() error {
		defer close(done)
		res, err := w.get()
		if err != nil {
			return err
		}
		if err := read(res.Body, 16384); err != nil {
			return err
		}
		// The whole body in, its end too, before it is let go of.
		<-sent
		time.Sleep(100 * time.Millisecond)
		w.clock.advance(6 * time.Second)
		return res.Body.Close()
	}
	w.server = func() error {
		w.serve(65535)
		sid, err := w.answer()
		if err != nil {
			return err
		}
		if err := w.data(sid, 64<<10, true); err != nil {
			return err
		}
		close(sent)
		<-done
		if err := w.expect(windowUpdate{0, 64 << 10}); err != nil {
			return err
		}
		select {
		case reset := <-w.resets:
			return fmt.Errorf("stream %d reset", reset)
		default:
			return nil
		}
	}
	w.run()
}

package http2

import (
	"bytes"
	"context"
	"io"
	"testing"
	"time"

	http "github.com/igor-lemon/fhttp"
	"github.com/igor-lemon/fhttp/http2/hpack"
)

// openAnswer answers stream with a 200 and some of a body, and leaves it
// open, the rest to come.
func (p *peer) openAnswer(stream uint32) {
	var block bytes.Buffer
	hpack.NewEncoder(&block).WriteField(hpack.HeaderField{Name: ":status", Value: "200"})
	p.write(func() error {
		return p.fr.WriteHeaders(HeadersFrameParam{StreamID: stream, EndHeaders: true, BlockFragment: block.Bytes()})
	})
	p.write(func() error { return p.fr.WriteData(stream, false, []byte("part")) })
}

// resets are the RST_STREAMs the peer is sent until it hears nothing for a
// while, with when each came.
func (p *peer) resets(quiet time.Duration) []time.Time {
	var out []time.Time
	for {
		select {
		case s, ok := <-p.seen:
			if !ok {
				return out
			}
			if s.kind == FrameRSTStream {
				out = append(out, s.at)
			}
		case <-time.After(quiet):
			return out
		}
	}
}

// openBody sends a GET through a transport set by set and returns its answer,
// its body open, and the peer.
func openBody(t *testing.T, ctx context.Context, set func(*Transport)) (*http.Response, *peer) {
	t.Helper()
	clients, servers := pairs(t, 1)
	p := newPeer(t, servers[0])
	tr, _ := pingTransport(t, nil, clients[0])
	if set != nil {
		set(tr)
	}
	done := make(chan *http.Response, 1)
	go func() {
		res, err := tr.RoundTrip(request(ctx, false))
		if err != nil {
			t.Error(err)
		}
		done <- res
	}()
	for s := range p.seen {
		if s.kind == FrameHeaders {
			p.openAnswer(s.stream)
			break
		}
	}
	res := <-done
	if res == nil {
		t.FailNow()
	}
	if _, err := io.ReadFull(res.Body, make([]byte, 4)); err != nil {
		t.Fatal(err)
	}
	return res, p
}

func TestABodyClosedTwiceResetsItsStreamOnce(t *testing.T) {
	for _, delay := range []time.Duration{0, 5 * time.Second} {
		res, p := openBody(t, context.Background(), func(tr *Transport) { tr.WindowUpdateDelay = delay })
		res.Body.Close()
		res.Body.Close()
		if got := p.resets(300 * time.Millisecond); len(got) != 1 {
			t.Errorf("WindowUpdateDelay %v: %d RST_STREAMs, want one", delay, len(got))
		}
	}
}

func TestARequestGivenUpResetsItsStreamOnceAsItIsGivenUp(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	res, p := openBody(t, ctx, nil)
	cancel()
	// The reset goes as the request is given up, its body not closed yet,
	// as Chrome resets a stream it cancels.
	if got := p.resets(300 * time.Millisecond); len(got) != 1 {
		t.Fatalf("%d RST_STREAMs as the request was given up, want one", len(got))
	}
	res.Body.Close()
	if got := p.resets(300 * time.Millisecond); len(got) != 0 {
		t.Errorf("%d RST_STREAMs more as its body was closed", len(got))
	}
}

func TestABodyLetGoOfAfterItsTimeResetsItsStreamOnce(t *testing.T) {
	// DrainBody's timer closes the body, and its goroutine, its read cut
	// short so, closes it again: one RST_STREAM, as Chrome sends.
	res, p := openBody(t, context.Background(), nil)
	done := http.DrainBody(context.Background(), res, 1<<20, 50*time.Millisecond, 0)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the body was not let go of")
	}
	if got := p.resets(300 * time.Millisecond); len(got) != 1 {
		t.Errorf("%d RST_STREAMs, want one", len(got))
	}
}

// A stream ended on both sides, its request written whole and its answer's
// END_STREAM read, sends no reset when its request is given up: x/net's
// cleanupWriteRequest has nothing to reset, and Chrome sends none. Such a
// stream is still on its connection where the connection was closing as its
// END_STREAM came (streamByID forgets none of a closed connection's), and a
// request's context ends as its caller is done with it: the reset went as
// the connection closed, and a write in flight then kept utls's Close from
// sending the TLS close_notify (tls-forge's a12 shutdown test, 3 runs of 60).
// One whose answer is not over is reset, once.
func TestAStreamEndedOnBothSidesIsNotResetAsItsRequestIsGivenUp(t *testing.T) {
	for _, tc := range []struct {
		name        string
		sent, ended bool
		resets      int
	}{{"ended on both sides", true, true, 0}, {"its answer not over", true, false, 1},
		{"its request not sent whole", false, true, 1}} {
		t.Run(tc.name, func(t *testing.T) {
			res, p := openBody(t, context.Background(), nil)
			cs := res.Body.(transportResponseBody).cs
			cs.sentEndStream.Store(tc.sent)
			cs.peerEnded.Store(tc.ended)
			cs.cancelStream()
			if got := p.resets(300 * time.Millisecond); len(got) != tc.resets {
				t.Errorf("%d RST_STREAMs, want %d", len(got), tc.resets)
			}
		})
	}
}

// A stream says when each side ended: a GET's END_STREAM with its HEADERS, a
// POST's with its body's last DATA frame or its trailers, the answer's with
// its last DATA frame or its trailers.
func TestAStreamSaysWhenEachSideEnded(t *testing.T) {
	for _, tc := range []struct {
		name                  string
		post, trailers, ended bool
	}{{"GET", false, false, false}, {"POST", true, false, false}, {"POST of trailers", true, true, true}} {
		clients, servers := pairs(t, 1)
		p := newPeer(t, servers[0])
		tr, _ := pingTransport(t, nil, clients[0])
		req := request(context.Background(), tc.post)
		if tc.trailers {
			req.Trailer = http.Header{"X-Request-Trailer": {"1"}}
		}
		done := make(chan *http.Response, 1)
		go func() {
			res, err := tr.RoundTrip(req)
			if err != nil {
				t.Error(err)
			}
			done <- res
		}()
		var stream uint32
		headers := 0
		for s := range p.seen {
			if s.kind == FrameHeaders {
				stream = s.stream
				headers++
			}
			if !tc.post && s.kind == FrameHeaders || tc.post && !tc.trailers && s.kind == FrameData ||
				tc.trailers && headers == 2 {
				break
			}
		}
		p.openAnswer(stream)
		res := <-done
		if res == nil {
			t.FailNow()
		}
		cs := res.Body.(transportResponseBody).cs
		if _, err := io.ReadFull(res.Body, make([]byte, 4)); err != nil {
			t.Fatal(err)
		}
		if !cs.sentEndStream.Load() || cs.peerEnded.Load() {
			t.Errorf("%s: the request ended %v, the answer %v; want true, false", tc.name,
				cs.sentEndStream.Load(), cs.peerEnded.Load())
		}
		if tc.ended {
			// The answer's trailers end it.
			var block bytes.Buffer
			hpack.NewEncoder(&block).WriteField(hpack.HeaderField{Name: "x-trailer", Value: "1"})
			p.write(func() error {
				return p.fr.WriteHeaders(HeadersFrameParam{StreamID: stream, EndStream: true, EndHeaders: true,
					BlockFragment: block.Bytes()})
			})
		} else {
			p.write(func() error { return p.fr.WriteData(stream, true, nil) })
		}
		if _, err := io.ReadAll(res.Body); err != nil {
			t.Fatal(err)
		}
		if !cs.peerEnded.Load() {
			t.Errorf("%s: the answer's END_STREAM was not noted", tc.name)
		}
		res.Body.Close()
	}
}

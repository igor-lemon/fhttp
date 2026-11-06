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

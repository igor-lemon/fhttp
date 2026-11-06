package http2

import (
	"bytes"
	"context"
	"sync"
	"testing"
	"time"

	"github.com/igor-lemon/fhttp/http2/hpack"
)

// TestAServersResetAndABodyClosedAtOnceDoNotRace holds the read loop's
// RST_STREAM and a body closed on the caller's goroutine to the stream's
// lock: Close reads resetErr under cc.mu to decide whether it resets the
// stream itself, and the read loop wrote it without. Run under -race.
func TestAServersResetAndABodyClosedAtOnceDoNotRace(t *testing.T) {
	for i := range 400 {
		clients, servers := pairs(t, 1)
		p := newPeer(t, servers[0])
		tr, _ := pingTransport(t, nil, clients[0])
		req := request(context.Background(), false)
		answered := make(chan struct{})
		go func() {
			for s := range p.seen {
				if s.kind == FrameHeaders {
					var block bytes.Buffer
					hpack.NewEncoder(&block).WriteField(hpack.HeaderField{Name: ":status", Value: "200"})
					p.write(func() error {
						return p.fr.WriteHeaders(HeadersFrameParam{StreamID: s.stream, EndHeaders: true, BlockFragment: block.Bytes()})
					})
					<-answered
					p.write(func() error { return p.fr.WriteRSTStream(s.stream, ErrCodeInternal) })
				}
			}
		}()
		res, err := tr.RoundTrip(req)
		if err != nil {
			t.Fatal(err)
		}
		var wg sync.WaitGroup
		wg.Add(1)
		go func() {
			defer wg.Done()
			// Within the reset's way over loopback and through the read
			// loop, at a point of its own each time.
			for start := time.Now(); time.Since(start) < time.Duration(i%80)*time.Microsecond; {
			}
			res.Body.Close()
		}()
		close(answered)
		wg.Wait()
		tr.CloseIdleConnections()
	}
}

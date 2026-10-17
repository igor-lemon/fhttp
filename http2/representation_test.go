package http2

import (
	"bytes"
	"testing"

	http "github.com/igor-lemon/fhttp"
	"github.com/igor-lemon/fhttp/http2/hpack"
)

func TestTransportHeaderRepresentation(t *testing.T) {
	// :path /a without indexing named by the first static :path, as Chrome
	// writes it, and, by default, with incremental indexing by the last.
	chrome := func(f hpack.HeaderField) hpack.Representation {
		return hpack.Representation{WithoutIndexing: f.Name == ":path", FirstStaticName: f.Name == ":path"}
	}
	for _, tc := range []struct {
		of   func(hpack.HeaderField) hpack.Representation
		want []byte
	}{
		{chrome, []byte{0x04, 0x02, '/', 'a'}},
		{nil, []byte{0x45, 0x02, '/', 'a'}},
	} {
		tr := &Transport{HeaderRepresentation: tc.of}
		cc := &ClientConn{t: tr, peerMaxHeaderListSize: 0xffffffffffffffff}
		cc.henc = tr.newEncoder(&cc.hbuf)
		req, err := http.NewRequest("GET", "https://example.com/a", nil)
		if err != nil {
			t.Fatal(err)
		}
		cc.mu.Lock()
		hdrs, err := cc.encodeHeaders(req, false, "", -1)
		cc.mu.Unlock()
		if err != nil || !bytes.Contains(hdrs, tc.want) {
			t.Errorf("headers %x, %v; want %x in them", hdrs, err, tc.want)
		}
	}
}

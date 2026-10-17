package http2

import (
	"bufio"
	"bytes"
	"context"
	"testing"

	http "github.com/igor-lemon/fhttp"
)

func TestARequestCarriesTheHeaderPriorityItsContextSays(t *testing.T) {
	page := PriorityParam{Exclusive: true, Weight: 255}
	script := PriorityParam{Exclusive: true, Weight: 219}
	req, err := http.NewRequest("GET", "https://example.com/", nil)
	if err != nil {
		t.Fatal(err)
	}
	asked := req.WithContext(WithHeaderPriority(context.Background(), script))
	for _, tc := range []struct {
		name string
		t    *Transport
		req  *http.Request
		want PriorityParam
	}{
		{"its context's", &Transport{HeaderPriority: &page}, asked, script},
		{"the transport's", &Transport{HeaderPriority: &script}, req, script},
		{"neither", &Transport{}, req, page},
	} {
		cc := &ClientConn{t: tc.t}
		if got := cc.headerPriority(tc.req); got != tc.want {
			t.Errorf("%s: %+v, want %+v", tc.name, got, tc.want)
		}
	}

	// And the first frame of a header block carries it.
	var buf bytes.Buffer
	cc := &ClientConn{t: &Transport{}, bw: bufio.NewWriter(&buf)}
	cc.fr = NewFramer(cc.bw, nil)
	if err := cc.writeHeaders(1, true, 16384, []byte{0x82}, script); err != nil {
		t.Fatal(err)
	}
	f, err := NewFramer(nil, &buf).ReadFrame()
	if err != nil {
		t.Fatal(err)
	}
	if headers, ok := f.(*HeadersFrame); !ok || headers.Priority != script {
		t.Errorf("the frame %v", f)
	}
}

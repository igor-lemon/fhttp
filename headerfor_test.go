package http_test

import (
	"bytes"
	"reflect"
	"strings"
	"testing"

	. "github.com/igor-lemon/fhttp"
	"github.com/igor-lemon/fhttp/httptest"
)

func TestARequestIsWrittenWithTheHeaderItsHeaderForGives(t *testing.T) {
	for _, tc := range []struct {
		name  string
		close bool
		give  Header
		want  string
	}{
		{"its own", false, Header{"X-One": {"1"}, "User-Agent": {}, HeaderOrderKey: {"host", "x-one"}},
			"POST /a HTTP/1.1\r\nHost: example.com\r\nX-One: 1\r\nContent-Length: 2\r\n\r\nhi"},
		{"none", false, nil,
			"POST /a HTTP/1.1\r\nContent-Length: 2\r\nHost: example.com\r\nUser-Agent: Go-http-client/1.1\r\nX-Two: 2\r\n\r\nhi"},
		// The writer adds what the request needs to the header written, and
		// reads it for what it says: a request to close says so once.
		{"closing as it says", true, Header{"Connection": {"close"}, "User-Agent": {}},
			"POST /a HTTP/1.1\r\nConnection: close\r\nContent-Length: 2\r\nHost: example.com\r\n\r\nhi"},
	} {
		req, err := NewRequest("POST", "http://example.com/a", strings.NewReader("hi"))
		if err != nil {
			t.Fatal(err)
		}
		req.Header = Header{"X-Two": {"2"}}
		req.Close = tc.close
		var asked []string
		req.HeaderFor = func(protocol string, header Header) Header {
			asked = append(asked, protocol+" "+header.Get("X-Two"))
			return tc.give
		}
		var buf bytes.Buffer
		if err := req.Write(&buf); err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if got := buf.String(); got != tc.want {
			t.Errorf("%s: wrote %q, want %q", tc.name, got, tc.want)
		}
		if !reflect.DeepEqual(asked, []string{"http/1.1 2"}) {
			t.Errorf("%s: asked %v", tc.name, asked)
		}
		if tc.give != nil && !reflect.DeepEqual(req.Header, Header{"X-Two": {"2"}}) {
			t.Errorf("%s: the request's own header became %v", tc.name, req.Header)
		}
	}
}

func TestTransportWritesTheHeaderItsHeaderForGives(t *testing.T) {
	got := make(chan Header, 1)
	ts := httptest.NewServer(HandlerFunc(func(_ ResponseWriter, r *Request) { got <- r.Header }))
	defer ts.Close()
	tr := &Transport{}
	defer tr.CloseIdleConnections()
	req, err := NewRequest("GET", ts.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header = Header{"X-Two": {"2"}}
	var asked []string
	req.HeaderFor = func(protocol string, header Header) Header {
		asked = append(asked, protocol)
		return Header{"X-One": {header.Get("X-Two") + "1"}, "Accept-Encoding": {"identity"}}
	}
	res, err := tr.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	// What it gave went out, and what the transport decided it decided on
	// that: it asked for no gzip, since the header written names a coding.
	h := <-got
	if h.Get("X-One") != "21" || h.Get("X-Two") != "" || !reflect.DeepEqual(h.Values("Accept-Encoding"), []string{"identity"}) {
		t.Errorf("the server got %v", h)
	}
	if !reflect.DeepEqual(asked, []string{"http/1.1"}) || res.Request != req {
		t.Errorf("asked %v; answered %p for %p", asked, res.Request, req)
	}
	if !reflect.DeepEqual(req.Header, Header{"X-Two": {"2"}}) {
		t.Errorf("the request's own header became %v", req.Header)
	}
}

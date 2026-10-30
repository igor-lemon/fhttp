package http

import (
	"bytes"
	"net/url"
	"strings"
	"testing"
)

// [fhttp] Over HTTP/1.1 a User-Agent with no value writes none, and one set
// to the empty string goes out as given, an empty "User-Agent:" line: this
// fork's raw User-Agent contract (A11), where upstream (#28) writes none of it.
func TestRequestWriteOmitsAUserAgentWithNoValue(t *testing.T) {
	write := func(t *testing.T, header Header) string {
		t.Helper()
		req := &Request{
			Method:     "GET",
			URL:        &url.URL{Scheme: "https", Host: "example.com", Path: "/"},
			Proto:      "HTTP/1.1",
			ProtoMajor: 1,
			ProtoMinor: 1,
			Header:     header,
			Host:       "example.com",
		}
		var buf bytes.Buffer
		if err := req.Write(&buf); err != nil {
			t.Fatal(err)
		}
		return buf.String()
	}

	for _, key := range []string{"User-Agent", "user-agent"} {
		for _, values := range [][]string{nil, {}} {
			got := write(t, Header{key: values})
			if strings.Contains(strings.ToLower(got), "user-agent") {
				t.Errorf("%s set to %q still wrote a User-Agent:\n%s", key, values, got)
			}
		}
		got := write(t, Header{key: {""}})
		if strings.Count(got, key+": \r\n") != 1 || strings.Count(strings.ToLower(got), "user-agent") != 1 {
			t.Errorf("%s set to the empty string was not written once as given:\n%s", key, got)
		}
	}

	// The controls: without the header Go's own goes out, and a value goes
	// out as given.
	if got := write(t, Header{}); !strings.Contains(got, "User-Agent: Go-http-client/1.1\r\n") {
		t.Errorf("without a User-Agent the default was not written:\n%s", got)
	}
	if got := write(t, Header{"User-Agent": {"Fake"}}); !strings.Contains(got, "User-Agent: Fake\r\n") {
		t.Errorf("a given User-Agent was not written:\n%s", got)
	}
}

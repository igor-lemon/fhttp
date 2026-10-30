package http_test

import (
	"bytes"
	"context"
	http "github.com/igor-lemon/fhttp"
	"github.com/igor-lemon/fhttp/httptrace"
	"reflect"
	"strings"
	"testing"
)

func TestTraceMatchesEveryWrittenRequestField(t *testing.T) {
	for _, chunked := range []bool{false, true} {
		req, _ := http.NewRequest("POST", "http://owned.example/path", strings.NewReader("abc"))
		req.Close = true
		req.Header = http.Header{"X-Caller": {"one", "two"}}
		if chunked {
			req.TransferEncoding = []string{"chunked"}
			req.ContentLength = -1
			req.Trailer = http.Header{"X-Final": {"done"}}
		}
		got := map[string][]string{}
		trace := &httptrace.ClientTrace{WroteHeaderField: func(key string, values []string) { got[key] = append(got[key], values...) }}
		req = req.WithContext(httptrace.WithClientTrace(context.Background(), trace))
		var wire bytes.Buffer
		if err := req.Write(&wire); err != nil {
			t.Fatal(err)
		}
		want := map[string][]string{}
		head := strings.SplitN(wire.String(), "\r\n\r\n", 2)[0]
		for _, line := range strings.Split(head, "\r\n")[1:] {
			key, value, ok := strings.Cut(line, ": ")
			if !ok {
				t.Fatalf("invalid header %q", line)
			}
			want[key] = append(want[key], value)
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("trace fields=%#v, actual wire fields=%#v", got, want)
		}
		for _, required := range []string{"Connection", "Host", "User-Agent", "X-Caller"} {
			if len(want[required]) == 0 {
				t.Fatalf("missing %s", required)
			}
		}
		if chunked {
			if !reflect.DeepEqual(want["Transfer-Encoding"], []string{"chunked"}) || !reflect.DeepEqual(want["Trailer"], []string{"X-Final"}) {
				t.Fatal("missing chunked/trailer framing")
			}
		} else if !reflect.DeepEqual(want["Content-Length"], []string{"3"}) {
			t.Fatal("missing content length")
		}
	}
}

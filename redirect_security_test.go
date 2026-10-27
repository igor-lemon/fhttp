package http

import (
	"net/url"
	"reflect"
	"strings"
	"testing"
)

func TestRedirectSensitiveHeadersStayRemoved(t *testing.T) {
	for _, firstHop := range []string{"https://origin.test/one", "https://sub.origin.test/one"} {
		t.Run(firstHop, func(t *testing.T) {
			initial, _ := NewRequest("GET", "https://origin.test/start", nil)
			initial.Header = Header{"X-Safe": {"safe"}}
			for _, name := range []string{"Authorization", "Cookie", "Cookie2", "WWW-Authenticate", "Proxy-Authorization", "Proxy-Authenticate"} {
				initial.Header[name] = []string{"caller-secret"}
			}
			original := initial.Header.Clone()
			copyHeaders := (&Client{}).makeHeadersCopier(initial)
			for i, target := range []string{firstHop, "https://other.test/one", "https://other.test/two", "https://origin.test/back"} {
				u, _ := url.Parse(target)
				next := &Request{URL: u, Header: make(Header), Response: &Response{Header: make(Header)}}
				copyHeaders(next)
				for name := range original {
					want := ""
					if name == "X-Safe" {
						want = "safe"
					} else if i == 0 {
						want = "caller-secret"
					}
					if got := next.Header[name]; strings.Join(got, ",") != want {
						t.Errorf("hop %d to %s: %s=%q, want %q", i, target, name, got, want)
					}
				}
			}
			if !reflect.DeepEqual(initial.Header, original) {
				t.Fatal("redirect mutated the caller's headers")
			}
		})
	}
}

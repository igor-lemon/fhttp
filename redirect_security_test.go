package http

import (
	"context"
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

// Under WithCredentialsKeptToOrigin the credentials go only to hops of the
// request's own origin, its host in other capitals or its port said too, and
// to none after a hop elsewhere: a subdomain, another port or scheme, the
// scheme alone too.
func TestRedirectCredentialsKeptToTheirOrigin(t *testing.T) {
	type hop struct {
		url  string
		kept bool
	}
	chains := [][]hop{{{"https://origin.test/same", true}, {"https://ORIGIN.test:443/same", true}, {"https://origin.test/again", true}}}
	for _, away := range []string{"https://sub.origin.test/one", "https://origin.test:8443/one", "http://origin.test/one", "http://origin.test:443/one", "https://other.test/one"} {
		chains = append(chains, []hop{{"https://origin.test/same", true}, {away, false}, {"https://origin.test/back", false}})
	}
	for _, chain := range chains {
		t.Run(chain[1].url, func(t *testing.T) {
			initial, _ := NewRequestWithContext(WithCredentialsKeptToOrigin(context.Background()), "GET", "https://origin.test/start", nil)
			initial.Header = Header{"X-Safe": {"safe"}}
			for _, name := range []string{"Authorization", "Cookie", "Cookie2", "WWW-Authenticate", "Proxy-Authorization", "Proxy-Authenticate"} {
				initial.Header[name] = []string{"caller-secret"}
			}
			original := initial.Header.Clone()
			copyHeaders := (&Client{}).makeHeadersCopier(initial)
			for i, h := range chain {
				u, _ := url.Parse(h.url)
				next := &Request{URL: u, Header: make(Header), Response: &Response{Header: make(Header)}}
				copyHeaders(next)
				for name := range original {
					want := ""
					if name == "X-Safe" {
						want = "safe"
					} else if h.kept {
						want = "caller-secret"
					}
					if got := next.Header[name]; strings.Join(got, ",") != want {
						t.Errorf("hop %d to %s: %s=%q, want %q", i, h.url, name, got, want)
					}
				}
			}
			if !reflect.DeepEqual(initial.Header, original) {
				t.Fatal("redirect mutated the caller's headers")
			}
		})
	}
}

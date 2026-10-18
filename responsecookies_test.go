package http_test

import (
	"io"
	"net/url"
	"testing"

	. "github.com/igor-lemon/fhttp"
	"github.com/igor-lemon/fhttp/cookiejar"
	"github.com/igor-lemon/fhttp/httptest"
)

func TestAClientKeepsTheCookiesOfTheAnswersARequestsContextSaysTo(t *testing.T) {
	// Every answer sets a cookie named after the path, /421 answers 421, and
	// /r redirects to /421.
	ts := httptest.NewServer(HandlerFunc(func(w ResponseWriter, r *Request) {
		SetCookie(w, &Cookie{Name: r.URL.Path[1:], Value: "1", Path: "/"})
		switch r.URL.Path {
		case "/r":
			Redirect(w, r, "/421", StatusFound)
		case "/421":
			w.WriteHeader(StatusMisdirectedRequest)
		}
	}))
	defer ts.Close()
	notMisdirected := func(res *Response) bool { return res.StatusCode != StatusMisdirectedRequest }
	for _, tc := range []struct {
		name string
		keep func(*Response) bool
		path string
		want string
	}{
		{"no say", nil, "/421", "421=1"},
		{"kept", notMisdirected, "/200", "200=1"},
		{"not kept", notMisdirected, "/421", ""},
		{"a redirect's kept, its hop's not", notMisdirected, "/r", "r=1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			jar, err := cookiejar.New(nil)
			if err != nil {
				t.Fatal(err)
			}
			c := &Client{Jar: jar}
			req, err := NewRequest("GET", ts.URL+tc.path, nil)
			if err != nil {
				t.Fatal(err)
			}
			var asked []int
			if tc.keep != nil {
				req = req.WithContext(WithResponseCookies(req.Context(), func(res *Response) bool {
					asked = append(asked, res.StatusCode)
					return tc.keep(res)
				}))
			}
			res, err := c.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			_, _ = io.Copy(io.Discard, res.Body)
			_ = res.Body.Close()
			u, _ := url.Parse(ts.URL)
			var got string
			for _, cookie := range jar.Cookies(u) {
				got += cookie.Name + "=" + cookie.Value
			}
			if got != tc.want {
				t.Errorf("the jar holds %q, want %q", got, tc.want)
			}
			if tc.keep != nil && (len(asked) == 0 || asked[len(asked)-1] != res.StatusCode) {
				t.Errorf("asked of %v, answered %d", asked, res.StatusCode)
			}
		})
	}
}

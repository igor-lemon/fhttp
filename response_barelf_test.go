package http_test

import (
	"bufio"
	"encoding/json"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	http "github.com/igor-lemon/fhttp"
	tls "github.com/igor-lemon/utls"
	"github.com/igor-lemon/utls/behavior"
)

// answeredWith is what a transport of policy makes of a server's answer.
func answeredWith(t *testing.T, policy behavior.Policy, answer string) (string, http.Header, error) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		c.SetDeadline(time.Now().Add(3 * time.Second))
		r := bufio.NewReader(c)
		for {
			line, err := r.ReadString('\n')
			if err != nil || line == "\r\n" {
				break
			}
		}
		io.WriteString(c, answer)
	}()
	tr := &http.Transport{TLSClientConfig: &tls.Config{Behavior: policy}}
	defer tr.CloseIdleConnections()
	res, err := (&http.Client{Transport: tr, Timeout: 3 * time.Second}).Get("http://" + ln.Addr().String() + "/")
	if err != nil {
		return "", nil, err
	}
	defer res.Body.Close()
	body, err := io.ReadAll(res.Body)
	return string(body), res.Trailer, err
}

func TestAChunkedAnswerOfLFAloneIsReadAsTheProfileSays(t *testing.T) {
	chrome := behavior.Policy{"h1.chunked_bare_lf": json.RawMessage("true")}
	const head = "HTTP/1.1 200 OK\r\nTransfer-Encoding: chunked\r\nTrailer: X-T\r\n\r\n"
	for _, tc := range []struct {
		name, body string
		trailer    string
	}{
		{"no trailer", "5\nhello\n0\n\n", ""},
		{"no trailer, CRLF last", "5\nhello\n0\r\n\r\n", ""},
		{"a trailer of LF alone", "5\nhello\n0\nX-T: 1\n\n", "1"},
		{"a trailer ended by CRLF", "5\nhello\n0\nX-T: 2\n\r\n", "2"},
	} {
		got, trailer, err := answeredWith(t, chrome, head+tc.body)
		if err != nil || got != "hello" || trailer.Get("X-T") != tc.trailer {
			t.Errorf("%s: %q, trailer %q, %v", tc.name, got, trailer.Get("X-T"), err)
		}
	}
	// Without it, RFC 9112's CRLF, as before.
	if _, _, err := answeredWith(t, behavior.Policy{}, head+"5\nhello\n0\n\n"); err == nil {
		t.Error("LF alone was taken without the profile's word")
	}
	// A trailer whose end does not come in the buffer is refused.
	long := head + "5\nhello\n0\nX-T: " + strings.Repeat("a", 8000) + "\n"
	if _, _, err := answeredWith(t, chrome, long); err == nil {
		t.Error("a trailer with no end was taken")
	}
}

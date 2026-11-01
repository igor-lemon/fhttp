package http_test

import (
	http "github.com/igor-lemon/fhttp"
	"github.com/igor-lemon/fhttp/httptest"
	"github.com/igor-lemon/fhttp/httptrace"
	"io"
	"sync/atomic"
	"testing"
)

func TestAutomaticHTTP2ReportsCompleteObservation(t *testing.T) {
	s := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "okay") }))
	s.EnableHTTP2 = true
	s.StartTLS()
	defer s.Close()
	var start, head, body atomic.Int64
	req, _ := http.NewRequest("GET", s.URL, nil)
	req = req.WithContext(httptrace.WithClientTrace(req.Context(), &httptrace.ClientTrace{
		WriteHeadersStart:    func(string) { start.Add(1) },
		GotResponseHeaders:   func(string, int) { head.Add(1) },
		GotResponseBodyBytes: func(_ string, n int) { body.Add(int64(n)) },
	}))
	res, e := s.Client().Do(req)
	if e != nil {
		t.Fatal(e)
	}
	b, e := io.ReadAll(res.Body)
	res.Body.Close()
	if e != nil || string(b) != "okay" || res.ProtoMajor != 2 {
		t.Fatal(res.Proto, string(b), e)
	}
	if start.Load() != 1 || head.Load() != 1 || body.Load() != 4 {
		t.Fatal("omitted automatic H2 hooks", start.Load(), head.Load(), body.Load())
	}
}

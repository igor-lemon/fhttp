package http2

import (
	"io"
	"net"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	tls "github.com/bogdanfinn/utls"
	http "github.com/igor-lemon/fhttp"
)

func TestTransportWritesTheHeaderItsHeaderForGives(t *testing.T) {
	got := make(chan http.Header, 1)
	st := newServerTester(t, func(_ http.ResponseWriter, r *http.Request) { got <- r.Header }, optOnlyServer)
	defer st.Close()
	tr := &Transport{TLSClientConfig: tlsConfigInsecure}
	defer tr.CloseIdleConnections()
	req, err := http.NewRequest("GET", st.ts.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header = http.Header{"X-Two": {"2"}}
	var asked []string
	req.HeaderFor = func(protocol string, header http.Header) http.Header {
		asked = append(asked, protocol)
		return http.Header{"X-One": {header.Get("X-Two") + "1"}, "Accept-Encoding": {"identity"}}
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
	if !reflect.DeepEqual(asked, []string{"h2"}) || res.Request != req {
		t.Errorf("asked %v; answered %p for %p", asked, res.Request, req)
	}

	// What is checked is what goes out: an Upgrade it gives is refused,
	// though the request's own header has none.
	req, err = http.NewRequest("GET", st.ts.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.HeaderFor = func(string, http.Header) http.Header { return http.Header{"Upgrade": {"websocket"}} }
	if _, err := tr.RoundTrip(req); err == nil || !strings.Contains(err.Error(), "invalid Upgrade request header") {
		t.Errorf("an Upgrade went: %v", err)
	}
}

// closeSignal is a connection that says when it is closed.
type closeSignal struct {
	net.Conn
	once   sync.Once
	closed chan struct{}
}

func (c *closeSignal) Close() error {
	c.once.Do(func() { close(c.closed) })
	return c.Conn.Close()
}

func TestTransportClosesAConnectionTheHeaderWrittenSaysTo(t *testing.T) {
	// The stream reads the header written, too: a Connection: close it
	// gives closes the connection once the answer is read.
	st := newServerTester(t, func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "ok") }, optOnlyServer)
	defer st.Close()
	closed := make(chan struct{})
	tr := &Transport{TLSClientConfig: tlsConfigInsecure, DialTLS: func(network, addr string, cfg *tls.Config) (net.Conn, error) {
		conn, err := tls.Dial(network, addr, cfg)
		if err != nil {
			return nil, err
		}
		return &closeSignal{Conn: conn, closed: closed}, nil
	}}
	defer tr.CloseIdleConnections()
	req, err := http.NewRequest("GET", st.ts.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.HeaderFor = func(string, http.Header) http.Header { return http.Header{"Connection": {"close"}} }
	res, err := tr.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.ReadAll(res.Body); err != nil {
		t.Fatal(err)
	}
	_ = res.Body.Close()
	select {
	case <-closed:
	case <-time.After(5 * time.Second):
		t.Error("the connection stayed open")
	}
}

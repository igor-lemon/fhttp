package http2

import (
	http "github.com/igor-lemon/fhttp"
	"github.com/igor-lemon/fhttp/httptest"
	tls "github.com/igor-lemon/utls"
	"github.com/igor-lemon/utls/behavior"
	"io"
	"net"
	"sync/atomic"
	"testing"
)

func TestConfiguredCloneOwnsPolicyAndConnections(t *testing.T) {
	var connections atomic.Int32
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "complete") }))
	if err := ConfigureServer(server.Config, &Server{}); err != nil {
		t.Fatal(err)
	}
	server.TLS = server.Config.TLSConfig
	server.EnableHTTP2 = true
	server.Config.ConnState = func(_ net.Conn, s http.ConnState) {
		if s == http.StateNew {
			connections.Add(1)
		}
	}
	server.StartTLS()
	defer server.Close()
	original := &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, ForceAttemptHTTP2: true, PseudoHeaderOrder: []string{":method", ":authority", ":scheme", ":path"}}
	h2, err := ConfigureTransports(original)
	if err != nil {
		t.Fatal(err)
	}
	original.H2transport = h2
	h2.Settings = map[SettingID]uint32{SettingMaxFrameSize: 16384}
	h2.SettingsOrder = []SettingID{SettingMaxFrameSize}
	h2.PseudoHeaderOrder = []string{":method", ":authority", ":scheme", ":path"}
	h2.Priorities = []Priority{{StreamID: 3, PriorityParam: PriorityParam{Weight: 15}}}
	h2.HeaderPriority = &PriorityParam{Weight: 31}
	h2.Behavior = behavior.Policy{"h2.priority_dependency": []byte(`"nearest-weight"`)}
	do := func(tr *http.Transport) {
		t.Helper()
		r, err := (&http.Client{Transport: tr}).Get(server.URL)
		if err != nil {
			t.Fatal(err)
		}
		defer r.Body.Close()
		b, err := io.ReadAll(r.Body)
		if err != nil || string(b) != "complete" || r.ProtoMajor != 2 {
			t.Fatalf("response=%q err=%v proto=%s", b, err, r.Proto)
		}
	}
	do(original)
	clone := original.Clone()
	defer original.CloseIdleConnections()
	defer clone.CloseIdleConnections()
	copied, ok := clone.H2transport.(*Transport)
	if !ok || copied == h2 || copied.t1 != clone {
		t.Fatalf("clone H2 owner=%#v", clone.H2transport)
	}
	if copied.ConnPool == h2.ConnPool || copied.connPoolOrDef != nil {
		t.Fatal("clone reused original connection pool")
	}
	do(clone)
	if n := connections.Load(); n != 2 {
		t.Fatalf("connections=%d,want separate original and clone", n)
	}
	clone.CloseIdleConnections()
	do(original)
	if n := connections.Load(); n != 2 {
		t.Fatalf("closing clone affected original: connections=%d", n)
	}
	copied.Settings[SettingMaxFrameSize] = 32768
	copied.SettingsOrder[0] = SettingMaxConcurrentStreams
	copied.PseudoHeaderOrder[0] = ":path"
	copied.Priorities[0].PriorityParam.Weight = 255
	copied.HeaderPriority.Weight = 255
	copied.Behavior["h2.priority_dependency"][1] = 'X'
	clone.PseudoHeaderOrder[0] = ":path"
	if h2.Settings[SettingMaxFrameSize] != 16384 || h2.SettingsOrder[0] != SettingMaxFrameSize || h2.PseudoHeaderOrder[0] != ":method" || h2.Priorities[0].PriorityParam.Weight != 15 || h2.HeaderPriority.Weight != 31 || h2.Behavior.Text("h2.priority_dependency") != "nearest-weight" || original.PseudoHeaderOrder[0] != ":method" {
		t.Fatal("clone changed original configuration")
	}
}

package http2

import (
	http "github.com/igor-lemon/fhttp"
	"io"
	"math"
	"net"
	"testing"
)

func TestProfileReceiveWindowAndLastStream(t *testing.T) {
	for _, window := range []uint32{0, math.MaxInt32} {
		a, b := net.Pipe()
		drained := make(chan struct{})
		go func() { _, _ = io.Copy(io.Discard, b); close(drained) }()
		tr := &Transport{Settings: map[SettingID]uint32{SettingInitialWindowSize: window}, SettingsOrder: []SettingID{SettingInitialWindowSize}, ConnectionFlow: math.MaxInt32 - 65535, InitialStreamID: math.MaxInt32}
		cc, err := tr.NewClientConn(a)
		if err != nil {
			t.Fatal(err)
		}
		if cc.streamFlow != window {
			t.Errorf("advertised %d;receive accounting uses %d", window, cc.streamFlow)
		}
		if !cc.CanTakeNewRequest() {
			t.Error("last legal client stream is unavailable")
		}
		_ = cc.Close()
		_ = b.Close()
		<-drained
	}
}

func TestProfileConnectionWindowCannotOverflow(t *testing.T) {
	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()
	// Rejection must precede preface I/O; a Pipe Write would otherwise block.
	tr := &Transport{ConnectionFlow: math.MaxInt32 - 65535 + 1}
	done := make(chan error, 1)
	go func() { _, err := tr.NewClientConn(a); done <- err }()
	// Close the peer after observing any write, allowing the negative baseline
	// to finish. A real early bounds error returns without writing at all.
	read := make(chan struct{})
	go func() { var one [1]byte; _, _ = b.Read(one[:]); close(read); _ = b.Close() }()
	err := <-done
	select {
	case <-read:
		t.Error("overflow emitted preface bytes before rejection")
	default:
	}
	if err == nil {
		t.Fatal("connection window overflow accepted")
	}
}

func TestExplicitConnectionWindowSurvivesCloneAndConfigure(t *testing.T) {
	for _, flow := range []uint32{0, 1, math.MaxInt32 - 65535} {
		t1 := (&http.Transport{ConnectionFlow: flow, ConnectionFlowSet: true}).Clone()
		t2, err := ConfigureTransports(t1)
		if err != nil {
			t.Fatal(err)
		}
		if !t2.ConnectionFlowSet || t2.ConnectionFlow != flow {
			t.Fatalf("configuration lost: %+v", t2)
		}
		a, b := net.Pipe()
		drained := make(chan struct{})
		go func() { _, _ = io.Copy(io.Discard, b); close(drained) }()
		cc, err := t2.NewClientConn(a)
		if err != nil {
			t.Fatal(err)
		}
		if cc.connFlow != flow {
			t.Fatalf("configured %d, accounting %d", flow, cc.connFlow)
		}
		_ = cc.Close()
		_ = b.Close()
		<-drained
	}
	a, b := net.Pipe()
	drained := make(chan struct{})
	go func() { _, _ = io.Copy(io.Discard, b); close(drained) }()
	cc, err := (&Transport{}).NewClientConn(a)
	if err != nil {
		t.Fatal(err)
	}
	if cc.connFlow != transportDefaultConnFlow {
		t.Fatal("legacy backend default changed", cc.connFlow)
	}
	_ = cc.Close()
	_ = b.Close()
	<-drained
}

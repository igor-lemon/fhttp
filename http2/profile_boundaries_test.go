package http2

import (
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

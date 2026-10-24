// Copyright 2026 The fhttp Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package http

import (
	"errors"
	"io"
	"net"
	"testing"
)

// TestLostBeforeAnswer: an idle connection the server closed, a request of
// which nothing was written, a first read or a write that found the
// connection lost; not one whose answer had begun, nor a write that failed
// otherwise.
func TestLostBeforeAnswer(t *testing.T) {
	reset := &net.OpError{Op: "write", Err: errors.New("connection reset by peer")}
	other := errors.New("tls: bad record MAC")
	for _, tc := range []struct {
		name          string
		err, writeErr error
		want          bool
	}{
		{"closed idle", errServerClosedIdle, nil, true},
		{"nothing written", nothingWrittenError{other}, nil, true},
		{"first read lost", transportReadFromServerError{io.EOF}, nil, true},
		{"first read failed", transportReadFromServerError{other}, nil, false},
		{"write lost", errors.New("net/http: HTTP/1.x transport connection broken: x"), reset, true},
		{"write failed", errors.New("net/http: HTTP/1.x transport connection broken: x"), other, false},
		{"answer begun", errors.New("malformed MIME header"), nil, false},
	} {
		if got := lostBeforeAnswer(tc.err, tc.writeErr); got != tc.want {
			t.Errorf("%s: %v, want %v", tc.name, got, tc.want)
		}
	}
}

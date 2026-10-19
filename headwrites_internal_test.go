// Copyright 2026 The fhttp Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package http

import (
	"bytes"
	"io"
	"strings"
	"testing"
)

// ownReader is a reader of a type no one knows.
type ownReader struct{ io.Reader }

func TestHeldInMemorySeesThroughWhatABodyComesIn(t *testing.T) {
	for _, tc := range []struct {
		name string
		r    io.Reader
		held bool
	}{
		{"a bytes.Reader", bytes.NewReader(nil), true},
		{"a bytes.Buffer", new(bytes.Buffer), true},
		{"a strings.Reader", strings.NewReader(""), true},
		{"in a NopCloser, which writes to", io.NopCloser(strings.NewReader("")), true},
		{"in a readTrackingBody", &readTrackingBody{ReadCloser: io.NopCloser(bytes.NewReader(nil))}, true},
		{"in a NopCloser, which does not write to", io.NopCloser(&readTrackingBody{ReadCloser: io.NopCloser(new(bytes.Buffer))}), true},
		{"a reader of its own", ownReader{strings.NewReader("")}, false},
		{"in a NopCloser", io.NopCloser(ownReader{strings.NewReader("")}), false},
		{"none", nil, false},
	} {
		if held := heldInMemory(tc.r); held != tc.held {
			t.Errorf("%s: %v", tc.name, held)
		}
	}
}

func TestABodyGoesWithItsHeadHeldInMemoryWithinTheLimit(t *testing.T) {
	inMemory := strings.NewReader("0123456789")
	for _, tc := range []struct {
		name   string
		tw     transferWriter
		head   int
		merges bool
	}{
		{"within", transferWriter{ContentLength: 10, Body: inMemory}, 1300, true},
		{"at the limit", transferWriter{ContentLength: 10, Body: inMemory}, 1390, true},
		{"past it", transferWriter{ContentLength: 10, Body: inMemory}, 1391, false},
		{"no body", transferWriter{Body: inMemory}, 10, false},
		{"a length not known", transferWriter{ContentLength: -1, Body: inMemory}, 10, false},
		{"chunked", transferWriter{ContentLength: 10, Body: inMemory, TransferEncoding: []string{"chunked"}}, 10, false},
		{"not in memory", transferWriter{ContentLength: 10, Body: ownReader{inMemory}}, 10, false},
	} {
		if merges := tc.tw.mergesWith(tc.head, 1400); merges != tc.merges {
			t.Errorf("%s: %v", tc.name, merges)
		}
	}
}

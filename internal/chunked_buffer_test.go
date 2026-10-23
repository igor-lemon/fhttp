// Copyright 2026 The fhttp Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package internal

import (
	"bufio"
	"io"
	"strings"
	"testing"
)

// TestEndsInBuffer holds EndsInBuffer to what the chunked reader and the
// trailer after it would read from the buffer alone: the rest of a chunk,
// its CRLF, sizes with extensions, the last chunk, the trailer to its empty
// line, and no more data than the limit; anything malformed or missing says
// no.
func TestEndsInBuffer(t *testing.T) {
	for _, tc := range []struct {
		name  string
		wire  string
		read  int // body bytes read before asking; -1 to its end
		limit uint64
		ends  bool
	}{
		{"whole", "5\r\nhello\r\n0\r\n\r\n", 0, 16, true},
		{"two chunks", "2\r\nhe\r\n3\r\nllo\r\n0\r\n\r\n", 0, 5, true},
		{"over the limit", "5\r\nhello\r\n0\r\n\r\n", 0, 4, false},
		{"empty", "0\r\n\r\n", 0, 0, true},
		{"bare LFs", "5\nhello\r\n0\n\n", 0, 16, true},
		{"extension", "5;ext=1\r\nhello\r\n0;x\r\n\r\n", 0, 16, true},
		{"trailer", "5\r\nhello\r\n0\r\nX-T: 1\r\n\r\n", 0, 16, true},
		{"trailer cut short", "5\r\nhello\r\n0\r\nX-T: 1\r\n", 0, 16, false},
		{"no empty line", "5\r\nhello\r\n0\r\n", 0, 16, false},
		{"chunk cut short", "5\r\nhel", 0, 16, false},
		{"size line cut short", "5", 0, 16, false},
		{"no CRLF after the data", "5\r\nhelloXX0\r\n\r\n", 0, 16, false},
		{"data cut before its CRLF", "5\r\nhello", 0, 16, false},
		{"bad size", "zz\r\nhello\r\n0\r\n\r\n", 0, 16, false},
		{"any extension, as the reader takes it", "5;\"x\r\nhello\r\n0\r\n\r\n", 0, 16, true},
		{"in a chunk", "5\r\nhello\r\n0\r\n\r\n", 2, 16, true},
		{"in a chunk, over", "5\r\nhello\r\n3\r\nabc\r\n0\r\n\r\n", 2, 5, false},
		{"at a chunk's end", "5\r\nhello\r\n0\r\n\r\n", 5, 16, true},
		{"after the last chunk", "5\r\nhello\r\n0\r\n\r\n", -1, 0, true},
		{"after the last chunk, the trailer cut short", "5\r\nhello\r\n0\r\nX: 1\r\n", -1, 0, false},
		{"line too long", strings.Repeat("0", maxLineLength) + "5\r\nhello\r\n0\r\n\r\n", 0, 16, false},
		// A size the reader would take but for its line's length.
		{"extension past the line's limit", "5;" + strings.Repeat("x", maxLineLength) + "\r\nhello\r\n0\r\n\r\n", 0, 16, false},
	} {
		br := bufio.NewReaderSize(strings.NewReader(tc.wire), 2*maxLineLength)
		br.Peek(len(tc.wire))
		cr := NewChunkedReader(br)
		if tc.read > 0 {
			if _, err := io.ReadFull(cr, make([]byte, tc.read)); err != nil {
				t.Fatalf("%s: %v", tc.name, err)
			}
		}
		if tc.read < 0 {
			if _, err := io.ReadAll(cr); err != nil {
				t.Fatalf("%s: %v", tc.name, err)
			}
		}
		if got := EndsInBuffer(cr, tc.limit); got != tc.ends {
			t.Errorf("%s: %v, want %v", tc.name, got, tc.ends)
		}
	}
	// A reader that is not the chunked one, and one that failed, say no.
	if EndsInBuffer(strings.NewReader("0\r\n\r\n"), 16) {
		t.Error("a strings.Reader ends in a buffer")
	}
	// What follows the bad line would read as a trailer's end.
	cr := NewChunkedReader(bufio.NewReader(strings.NewReader("zz\r\n\r\n")))
	cr.Read(make([]byte, 4))
	if EndsInBuffer(cr, 16) {
		t.Error("a reader that failed ends in a buffer")
	}
}

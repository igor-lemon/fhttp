package internal

import (
	"bufio"
	"io"
	"strings"
	"testing"
)

func TestAChunkedBodyIsReadAsChromiumReadsItWhereAskedTo(t *testing.T) {
	for _, tc := range []struct {
		name, wire string
		body       string
		ok         bool
	}{
		{"CRLF throughout", "5\r\nhello\r\n0\r\n", "hello", true},
		{"LF alone throughout", "5\nhello\n0\n", "hello", true},
		{"both, line by line", "5\r\nhello\n3\nabc\r\n0\n", "helloabc", true},
		{"an extension, and spaces before it", "5 ;a=b\nhello\n0;x\n", "hello", true},
		{"a CR in an extension", "5;a\rb\nhello\n0\n", "hello", true},
		{"spaces after the size", "5   \nhello\n0\n", "hello", true},
		{"a tab after the size", "5\t\nhello\n0\n", "", false},
		{"no size", "\nhello\n0\n", "", false},
		{"not hex", "g\nhello\n0\n", "", false},
		{"data past its size", "5\nhelloX\n0\n", "hello", false},
		{"a CR alone after the data", "5\nhello\rX0\n", "hello", false},
		{"cut after the data", "5\nhello", "hello", false},
		{"cut in its end", "5\nhello\r", "hello", false},
	} {
		got, err := io.ReadAll(NewChunkedReaderBareLF(bufio.NewReader(strings.NewReader(tc.wire))))
		if (err == nil) != tc.ok || string(got) != tc.body {
			t.Errorf("%s: %q, %v", tc.name, got, err)
		}
	}
	// NewChunkedReader is as strict as before: RFC 9112's CRLF.
	if _, err := io.ReadAll(NewChunkedReader(bufio.NewReader(strings.NewReader("5\nhello\n0\n")))); err == nil {
		t.Error("NewChunkedReader took LF alone")
	}
	// A line past the limit, as readChunkLine's.
	long := strings.Repeat("0", maxLineLength) + "5\nhello\n0\n"
	if _, err := io.ReadAll(NewChunkedReaderBareLF(bufio.NewReaderSize(strings.NewReader(long), 2*maxLineLength))); err != ErrLineTooLong {
		t.Errorf("a long line: %v", err)
	}
	if _, err := io.ReadAll(NewChunkedReaderBareLF(bufio.NewReaderSize(strings.NewReader(strings.Repeat("0", 64)), 16))); err != ErrLineTooLong {
		t.Errorf("a line past the buffer: %v", err)
	}
}

func TestAChunkedBodyOfLFAloneReturnsWhatItHasWithoutWaiting(t *testing.T) {
	// A read with data returns before an end not yet come, as for CRLF.
	for _, tc := range []struct{ wire, want string }{
		{"5\nhello", "hello"},
		{"5\nhello\r", "hello"},
		{"5\nhello\n", "hello"},
		{"5\nhello\r\n", "hello"},
	} {
		r := NewChunkedReaderBareLF(bufio.NewReader(&onceReader{data: []byte(tc.wire)}))
		buf := make([]byte, 64)
		n, err := r.Read(buf)
		if string(buf[:n]) != tc.want || err != nil {
			t.Errorf("%q: %q, %v", tc.wire, buf[:n], err)
		}
	}
}

// onceReader gives its data in one read, then blocks no read: the next says
// nothing more is there for now.
type onceReader struct {
	data []byte
	done bool
}

func (r *onceReader) Read(p []byte) (int, error) {
	if r.done {
		return 0, io.EOF
	}
	r.done = true
	return copy(p, r.data), nil
}

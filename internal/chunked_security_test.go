package internal

import (
	"io"
	"strings"
	"testing"
)

func TestChunkedRejectsInvalidLineEndings(t *testing.T) {
	for _, line := range []string{"1\n", "1;ext=x\n", "1\r;ext=x\r\n", "1\r\r\n", "0\n"} {
		t.Run(strings.ReplaceAll(line, "\n", "LF"), func(t *testing.T) {
			_, err := io.ReadAll(NewChunkedReader(strings.NewReader(line + "x\r\n0\r\n\r\n")))
			if err == nil {
				t.Fatalf("accepted malformed chunk line %q", line)
			}
		})
	}
}

func TestChunkedBoundsExtensionOverhead(t *testing.T) {
	input := strings.Repeat("1;"+strings.Repeat("a", 3000)+"\r\nx\r\n", 100) + "0\r\n\r\n"
	reader := strings.NewReader(input)
	_, err := io.Copy(io.Discard, NewChunkedReader(reader))
	if err == nil || !strings.Contains(err.Error(), "too much non-data") {
		t.Fatalf("excessive extensions: %v", err)
	}
	if reader.Len() < len(input)/2 {
		t.Fatal("read an unbounded amount of overhead before rejecting it")
	}
	// One-byte streaming, ordinary quoted extensions, and extensions
	// proportional to useful payload must remain valid.
	for _, valid := range []string{
		strings.Repeat("1\r\nx\r\n", 10000) + "0\r\n\r\n",
		"1;foo=\"quoted\"\r\nx\r\n0\r\n\r\n",
		strings.Repeat("400;"+strings.Repeat("a", 1000)+"\r\n"+strings.Repeat("x", 1024)+"\r\n", 30) + "0\r\n\r\n",
	} {
		if _, err := io.Copy(io.Discard, NewChunkedReader(strings.NewReader(valid))); err != nil {
			t.Fatalf("valid chunked input rejected: %v", err)
		}
	}
}

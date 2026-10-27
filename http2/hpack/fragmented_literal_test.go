package hpack

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"testing"
	"time"
)

// Decode in a disposable process: a regression must not exhaust or hang the
// whole suite, including under the race detector.
func TestSplitLiteralHasBoundedDecodingCost(t *testing.T) {
	if os.Getenv("FHTTP_SPLIT_LITERAL_CHILD") != "1" {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		executable, err := os.Executable()
		if err != nil {
			t.Fatal(err)
		}
		cmd := exec.CommandContext(ctx, executable, "-test.run=^TestSplitLiteralHasBoundedDecodingCost$", "-test.timeout=12s", "-test.v")
		cmd.Env = append(os.Environ(), "FHTTP_SPLIT_LITERAL_CHILD=1", "GOMEMLIMIT=64MiB")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("bounded decoding failed: %v (deadline: %v)\n%s", err, ctx.Err(), out)
		}
		t.Logf("%s", out)
		return
	}

	want := HeaderField{Name: strings.Repeat("a", 128<<10), Value: strings.Repeat("v", 2000)}
	var encoded bytes.Buffer
	if err := NewEncoder(&encoded).WriteField(want); err != nil {
		t.Fatal(err)
	}
	wire := encoded.Bytes()
	// Keep the large Huffman name intact; feed the trailing encoded value one
	// byte at a time. The name must not be decompressed again for each byte.
	cut := len(wire) - int(HuffmanEncodeLength(want.Value))
	count := 0
	d := NewDecoder(1<<20, func(got HeaderField) {
		count++
		if got != want {
			t.Fatal("fragmented literal changed the decoded field")
		}
	})
	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	started := time.Now()
	if _, err := d.Write(wire[:cut]); err != nil {
		t.Fatal(err)
	}
	for i := cut; i < len(wire); i++ {
		if _, err := d.Write(wire[i : i+1]); err != nil {
			t.Fatal(err)
		}
	}
	if err := d.Close(); err != nil {
		t.Fatal(err)
	}
	runtime.ReadMemStats(&after)
	allocated := after.TotalAlloc - before.TotalAlloc
	t.Logf("decoded %d fragments in %v, allocated %d bytes", len(wire)-cut, time.Since(started), allocated)
	if count != 1 {
		t.Fatalf("emitted %d fields, want 1", count)
	}
	if allocated > 16<<20 {
		t.Fatalf("fragmented literal allocated %d bytes, limit %d", allocated, 16<<20)
	}
}

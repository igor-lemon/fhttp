package hpack

import (
	"bytes"
	"encoding/hex"
	"strings"
	"testing"
)

func TestEncoderRepresentation(t *testing.T) {
	chrome := func(f HeaderField) Representation {
		return Representation{WithoutIndexing: f.Name == ":path", FirstStaticName: f.Name == ":path"}
	}
	only := func(r Representation) func(HeaderField) Representation {
		return func(HeaderField) Representation { return r }
	}
	for _, tc := range []struct {
		name  string
		of    func(HeaderField) Representation
		field HeaderField
		want  string
		added bool
	}{
		{"default: indexed, named by the last :path", nil, HeaderField{Name: ":path", Value: "/a"}, "45022f61", true},
		{"Chrome's :path: without indexing, named by the first", chrome, HeaderField{Name: ":path", Value: "/a"}, "04022f61", false},
		{"without indexing alone", only(Representation{WithoutIndexing: true}), HeaderField{Name: ":path", Value: "/a"}, "05022f61", false},
		{"the first name alone", only(Representation{FirstStaticName: true}), HeaderField{Name: ":path", Value: "/a"}, "44022f61", true},
		{"the first :method", only(Representation{FirstStaticName: true}), HeaderField{Name: ":method", Value: "PUT"}, "4203505554", true},
		{"a field found whole is indexed whatever", chrome, HeaderField{Name: ":path", Value: "/"}, "84", false},
		{"a name of no table, without indexing", only(Representation{WithoutIndexing: true}), HeaderField{Name: "x-a", Value: "b"}, "0003782d610162", false},
		{"one sensitive is never indexed, named by the first", chrome, HeaderField{Name: ":path", Value: "/a", Sensitive: true}, "14022f61", false},
		{"a field Chrome's policy leaves alone", chrome, HeaderField{Name: ":method", Value: "PUT"}, "4303505554", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			e := NewEncoder(&buf)
			e.SetRepresentation(tc.of)
			if err := e.WriteField(tc.field); err != nil {
				t.Fatalf("WriteField: %v", err)
			}
			if got := hex.EncodeToString(buf.Bytes()); got != tc.want {
				t.Errorf("wrote %s, want %s", got, tc.want)
			}
			if added := e.dynTab.table.len() == 1; added != tc.added {
				t.Errorf("added to the dynamic table: %v", added)
			}
		})
	}
	// A name the dynamic table holds keeps its index there.
	var buf bytes.Buffer
	e := NewEncoder(&buf)
	e.SetRepresentation(only(Representation{FirstStaticName: true}))
	_ = e.WriteField(HeaderField{Name: "x-a", Value: "1"})
	buf.Reset()
	_ = e.WriteField(HeaderField{Name: "x-a", Value: "2"})
	if got := hex.EncodeToString(buf.Bytes()); got != "7e0132" {
		t.Errorf("wrote %s, want the dynamic name, 7e0132", got)
	}
	if first := firstNames([]HeaderField{{Name: "a"}, {Name: "b"}, {Name: "a", Value: "x"}}); first["a"] != 1 || first["b"] != 2 {
		t.Errorf("firstNames = %v", first)
	}
}

func TestEncoderIndexTooLarge(t *testing.T) {
	// A field larger than the dynamic table: written without indexing by
	// default, the table left as it was; with IndexTooLarge written with
	// incremental indexing, as Chrome writes it, which empties the table
	// (RFC 7541, section 4.4). One that fits is indexed either way, and one
	// sensitive or said to go without indexing never is.
	only := func(r Representation) func(HeaderField) Representation {
		return func(HeaderField) Representation { return r }
	}
	// A table of 64 bytes holds x-a: 1 (36), and x-b with a value of 29
	// bytes (64) alone, but not with one of 30 (65).
	kept := HeaderField{Name: "x-a", Value: "1"}
	tooLarge := HeaderField{Name: "x-b", Value: strings.Repeat("v", 30)}
	fits := HeaderField{Name: "x-b", Value: strings.Repeat("v", 29)}
	for _, tc := range []struct {
		name    string
		of      func(HeaderField) Representation
		field   HeaderField
		prefix  byte
		entries int
		size    uint32
	}{
		{"too large, by default", nil, tooLarge, 0x00, 1, 36},
		{"too large, indexed all the same", only(Representation{IndexTooLarge: true}), tooLarge, 0x40, 0, 0},
		{"one that fits", only(Representation{IndexTooLarge: true}), fits, 0x40, 1, 64},
		{"one that fits, by default", nil, fits, 0x40, 1, 64},
		{"too large and sensitive", only(Representation{IndexTooLarge: true}), HeaderField{Name: "x-b", Value: tooLarge.Value, Sensitive: true}, 0x10, 1, 36},
		{"too large, without indexing", only(Representation{IndexTooLarge: true, WithoutIndexing: true}), tooLarge, 0x00, 1, 36},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			e := NewEncoder(&buf)
			e.SetMaxDynamicTableSizeLimit(64)
			_ = e.WriteField(kept)
			buf.Reset()
			e.SetRepresentation(tc.of)
			if err := e.WriteField(tc.field); err != nil {
				t.Fatalf("WriteField: %v", err)
			}
			if got := buf.Bytes(); len(got) == 0 || got[0] != tc.prefix {
				t.Errorf("wrote %x, want it to begin %02x", got, tc.prefix)
			}
			if got := e.dynTab.table.len(); got != tc.entries {
				t.Errorf("the dynamic table holds %d entries, want %d", got, tc.entries)
			}
			if e.dynTab.size != tc.size {
				t.Errorf("the dynamic table holds %d bytes, want %d", e.dynTab.size, tc.size)
			}
		})
	}
}

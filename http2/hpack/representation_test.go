package hpack

import (
	"bytes"
	"encoding/hex"
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

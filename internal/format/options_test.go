package format_test

import (
	"bytes"
	"encoding/json/jsontext"
	"strings"
	"testing"

	"github.com/178inaba/rdsh/internal/format"
	"github.com/178inaba/rdsh/internal/redash"
)

// The tests below pin the JSON spelling rdsh prints. The output format is a
// contract callers branch on mechanically (see CLAUDE.md), so each is here to
// fail if one of the encoder options behind that spelling is dropped as
// redundant — none of the other tests in this repository reaches these two
// dimensions, because a fixture built with an encoder cannot express them.

// TestWriteObjectEscapesLineSeparators covers EscapeForHTML and EscapeForJS
// together. U+2028 and U+2029 are legal in a JSON string but end a line in
// JavaScript, so leaving them raw breaks a consumer that evaluates the
// output; < > & are escaped for the same lineage of reasons.
func TestWriteObjectEscapesLineSeparators(t *testing.T) {
	var buf bytes.Buffer
	if err := format.WriteObject(&buf, map[string]string{
		"seps": "a\u2028b\u2029c",
		"html": "x<y>z&w",
	}); err != nil {
		t.Fatalf("WriteObject() error = %v", err)
	}

	got := buf.String()
	for _, want := range []string{`\u2028`, `\u2029`, `\u003c`, `\u003e`, `\u0026`} {
		if !strings.Contains(got, want) {
			t.Errorf("output = %s, want it to contain %s", got, want)
		}
	}
	// The raw characters must not survive alongside the escapes.
	for _, unwanted := range []string{"\u2028", "\u2029", "<", ">", "&"} {
		if strings.Contains(got, unwanted) {
			t.Errorf("output = %q, want %q escaped", got, unwanted)
		}
	}
}

// TestWriteObjectKeepsRawStringsAsSpelled covers PreserveRawStrings. A
// visualization's stored options reach the output as the raw JSON Redash
// sent, and Redash spells non-ASCII with \uXXXX; expanding those to the
// characters they stand for would change the bytes of `rdsh query show`.
func TestWriteObjectKeepsRawStringsAsSpelled(t *testing.T) {
	var buf bytes.Buffer
	if err := format.WriteObject(&buf, map[string]jsontext.Value{
		"stored": jsontext.Value(`"caf\u00e9 \u65e5\u672c"`),
	}); err != nil {
		t.Fatalf("WriteObject() error = %v", err)
	}

	if want := `"caf\u00e9 \u65e5\u672c"`; !strings.Contains(buf.String(), want) {
		t.Errorf("output = %s, want it to contain %s", buf.String(), want)
	}
}

// TestWriteObjectSortsMapKeys covers Deterministic. A Go map iterates in a
// different order each run, so without it the same result would print
// differently from one invocation to the next.
func TestWriteObjectSortsMapKeys(t *testing.T) {
	value := map[string]int{"z": 1, "a": 2, "m": 3}
	want := `{"a":2,"m":3,"z":1}` + "\n"

	// Repeated because an unordered encoding can match by luck once.
	for range 8 {
		var buf bytes.Buffer
		if err := format.WriteObject(&buf, value); err != nil {
			t.Fatalf("WriteObject() error = %v", err)
		}
		if got := buf.String(); got != want {
			t.Fatalf("WriteObject() = %q, want %q", got, want)
		}
	}
}

// TestWriteJSONEndsWithNewline covers the newline MarshalWrite does not
// write. A caller reading the output line by line needs it.
func TestWriteJSONEndsWithNewline(t *testing.T) {
	var buf bytes.Buffer
	res := &redash.QueryResult{
		Columns: []redash.Column{{Name: "n"}},
		Rows:    []map[string]jsontext.Value{{"n": jsontext.Value(`1`)}},
	}
	if err := format.Write(&buf, format.JSON, res); err != nil {
		t.Fatalf("Write(json) error = %v", err)
	}
	if want := "[{\"n\":1}]\n"; buf.String() != want {
		t.Errorf("Write(json) = %q, want %q", buf.String(), want)
	}
}

// TestCellStringDistinguishesNullFromAbsent covers what holding a cell as
// raw JSON buys: a column present and null is a different thing from a
// column the row does not carry, and only the raw value can say which.
func TestCellStringDistinguishesNullFromAbsent(t *testing.T) {
	res := &redash.QueryResult{
		Columns: []redash.Column{{Name: "a"}, {Name: "b"}},
		Rows: []map[string]jsontext.Value{
			{"a": jsontext.Value(`null`)}, // b is absent
		},
	}

	var buf bytes.Buffer
	if err := format.Write(&buf, format.CSV, res); err != nil {
		t.Fatalf("Write(csv) error = %v", err)
	}
	// Both render as an empty field, which is what a separated format has
	// to say about either.
	if want := "a,b\n,\n"; buf.String() != want {
		t.Errorf("Write(csv) = %q, want %q", buf.String(), want)
	}

	buf.Reset()
	if err := format.Write(&buf, format.JSON, res); err != nil {
		t.Fatalf("Write(json) error = %v", err)
	}
	// The JSON output keeps them apart.
	if want := `[{"a":null}]` + "\n"; buf.String() != want {
		t.Errorf("Write(json) = %q, want %q", buf.String(), want)
	}
}

package output_test

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/178inaba/rdsh/internal/output"
	"github.com/178inaba/rdsh/internal/redash"
)

// result uses column names whose alphabetical order (amount, id, note)
// differs from the columns-array order (id, note, amount) so the tests
// catch any implementation that iterates the row maps instead.
func result() *redash.QueryResult {
	return &redash.QueryResult{
		Columns: []redash.Column{{Name: "id"}, {Name: "note"}, {Name: "amount"}},
		Rows: []map[string]any{
			{"id": json.Number("1"), "note": "hello, world", "amount": json.Number("12345678901234567890")},
			{"id": json.Number("2"), "note": nil, "amount": json.Number("0.5")},
		},
	}
}

func TestWriteCSV(t *testing.T) {
	var buf bytes.Buffer
	if err := output.Write(&buf, "csv", result()); err != nil {
		t.Fatalf("Write(csv) error = %v", err)
	}
	want := "id,note,amount\n" +
		"1,\"hello, world\",12345678901234567890\n" +
		"2,,0.5\n"
	if got := buf.String(); got != want {
		t.Errorf("Write(csv) =\n%q\nwant\n%q", got, want)
	}
}

func TestWriteTSV(t *testing.T) {
	var buf bytes.Buffer
	if err := output.Write(&buf, "tsv", result()); err != nil {
		t.Fatalf("Write(tsv) error = %v", err)
	}
	want := "id\tnote\tamount\n" +
		"1\thello, world\t12345678901234567890\n" +
		"2\t\t0.5\n"
	if got := buf.String(); got != want {
		t.Errorf("Write(tsv) =\n%q\nwant\n%q", got, want)
	}
}

func TestWriteJSON(t *testing.T) {
	var buf bytes.Buffer
	if err := output.Write(&buf, "json", result()); err != nil {
		t.Fatalf("Write(json) error = %v", err)
	}

	// An array of row objects; null preserved; big integers not mangled
	// into float64 scientific notation.
	var rows []map[string]any
	if err := json.Unmarshal(buf.Bytes(), &rows); err != nil {
		t.Fatalf("output is not valid JSON: %v\n%s", err, buf.String())
	}
	if len(rows) != 2 {
		t.Fatalf("len(rows) = %d, want 2", len(rows))
	}
	if rows[0]["note"] != "hello, world" {
		t.Errorf("rows[0].note = %v", rows[0]["note"])
	}
	if v, present := rows[1]["note"]; !present || v != nil {
		t.Errorf("rows[1].note = %v (present=%v), want explicit null", v, present)
	}
	if !strings.Contains(buf.String(), "12345678901234567890") {
		t.Errorf("big integer lost precision: %s", buf.String())
	}
}

func TestWriteUnknownFormat(t *testing.T) {
	var buf bytes.Buffer
	err := output.Write(&buf, "xml", result())
	if err == nil || !strings.Contains(err.Error(), "xml") {
		t.Errorf("Write(xml) error = %v, want unsupported-format error naming it", err)
	}
}

func TestWriteEmptyResult(t *testing.T) {
	var buf bytes.Buffer
	res := &redash.QueryResult{Columns: []redash.Column{{Name: "a"}}, Rows: nil}

	if err := output.Write(&buf, "csv", res); err != nil {
		t.Fatalf("Write(csv, empty) error = %v", err)
	}
	if got := buf.String(); got != "a\n" {
		t.Errorf("csv header only = %q, want %q", got, "a\n")
	}

	buf.Reset()
	if err := output.Write(&buf, "json", res); err != nil {
		t.Fatalf("Write(json, empty) error = %v", err)
	}
	if got := strings.TrimSpace(buf.String()); got != "[]" {
		t.Errorf("json empty = %q, want []", got)
	}
}

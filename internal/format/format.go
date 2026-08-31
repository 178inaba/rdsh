// Package format renders query results as CSV, TSV, or JSON.
package format

import (
	"encoding/csv"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"io"

	"github.com/178inaba/rdsh/internal/redash"
)

// Format is an output format accepted by Write. It implements pflag.Value,
// so a --format typo is rejected while cobra parses the flags — before the
// command runs and submits the query to the server.
type Format string

// Formats accepted by Write.
const (
	CSV  Format = "csv"
	TSV  Format = "tsv"
	JSON Format = "json"
)

func (f Format) String() string { return string(f) }

// Set implements pflag.Value.
func (f *Format) Set(s string) error {
	switch v := Format(s); v {
	case CSV, TSV, JSON:
		*f = v
		return nil
	}
	return unsupportedError(Format(s))
}

// Type names the value shown in the --format help line. It reports "string"
// rather than "format" because that help text is part of the agent-facing
// contract; see the documentation sync rule in CLAUDE.md.
func (Format) Type() string { return "string" }

// unsupportedError is shared by Set and Write: a Format converted from an
// arbitrary string still type-checks, so Write cannot assume its argument
// went through Set.
func unsupportedError(f Format) error {
	return fmt.Errorf("unsupported format %q (supported: %s, %s, %s)", string(f), CSV, TSV, JSON)
}

// Write renders the result in the given format. Rows arrive as maps keyed
// by column name, so CSV/TSV column order is taken from result.Columns.
func Write(w io.Writer, f Format, result *redash.QueryResult) error {
	switch f {
	case CSV:
		return writeSeparated(w, ',', result)
	case TSV:
		return writeSeparated(w, '\t', result)
	case JSON:
		return writeJSON(w, result)
	default:
		return unsupportedError(f)
	}
}

func writeSeparated(w io.Writer, comma rune, result *redash.QueryResult) error {
	cw := csv.NewWriter(w)
	cw.Comma = comma

	header := make([]string, len(result.Columns))
	for i, col := range result.Columns {
		header[i] = col.Name
	}
	if err := cw.Write(header); err != nil {
		return err
	}

	record := make([]string, len(result.Columns))
	for _, row := range result.Rows {
		for i, col := range result.Columns {
			record[i] = cellString(row[col.Name])
		}
		if err := cw.Write(record); err != nil {
			return err
		}
	}
	cw.Flush()
	return cw.Error()
}

// writeJSON emits an array of row objects — the shape fixed by the spec.
// Column order and type metadata are intentionally not carried.
func writeJSON(w io.Writer, result *redash.QueryResult) error {
	// [] for a result with no rows, spelled here rather than left to the
	// encoder's default so that the contract does not move if that default
	// does.
	rows := result.Rows
	if rows == nil {
		rows = []map[string]jsontext.Value{}
	}
	return WriteObject(w, rows)
}

// outputOptions pin the bytes of every JSON stream rdsh prints. The format
// is a contract callers branch on mechanically (see CLAUDE.md), so each of
// these is a requirement rather than a preference, and dropping any one of
// them changes the output:
//
// EscapeForHTML and EscapeForJS cover the characters a consumer evaluating
// the output would trip over; PreserveRawStrings keeps a visualization's
// stored options spelled the way Redash spelled them, since rdsh reads that
// blob without interpreting it; Deterministic keeps two runs printing the
// same result identically.
//
// Result rows arrive already normalised (see queryResultPayload.result in
// the redash package), so what is left here is to hold that spelling rather
// than to choose it.
var outputOptions = json.JoinOptions(
	jsontext.EscapeForHTML(true),
	jsontext.EscapeForJS(true),
	jsontext.PreserveRawStrings(true),
	json.Deterministic(true),
)

// WriteObject renders one value as JSON, for output that is a single record
// rather than the row set Write renders — the saved query `rdsh query show`
// reads. Both go through here so that every JSON stream rdsh prints is
// spelled one way rather than by coincidence between two call sites.
func WriteObject(w io.Writer, v any) error {
	if err := json.MarshalWrite(w, v, outputOptions); err != nil {
		return err
	}
	// MarshalWrite stops at the end of the value, where the v1 encoder wrote
	// a newline. It is part of the output a caller reads line by line.
	_, err := io.WriteString(w, "\n")
	return err
}

// cellString renders one cell for a separated format. The cell is raw JSON,
// so the only kind needing work is a string, which is unquoted because a
// CSV field carries the text rather than its JSON spelling. Everything else
// — numbers, booleans, nested arrays and objects — is already the least
// ambiguous single-cell representation there is, and a number renders as the
// text the warehouse wrote rather than as a float64 rounded back.
func cellString(v jsontext.Value) string {
	switch v.Kind() {
	// The zero value is the column being absent from this row; 'n' is the
	// column present and null. Both render empty here, but only the JSON
	// output can tell them apart, and it should.
	case 0, 'n':
		return ""
	case '"':
		unquoted, err := jsontext.AppendUnquote(nil, v)
		if err != nil {
			// Unreachable for a value that parsed as a JSON string, but
			// printing the raw spelling beats dropping the cell.
			return string(v)
		}
		return string(unquoted)
	default:
		return string(v)
	}
}

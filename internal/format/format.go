// Package format renders query results as CSV, TSV, or JSON.
package format

import (
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io"
	"strconv"

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
	rows := result.Rows
	if rows == nil {
		rows = []map[string]any{}
	}
	enc := json.NewEncoder(w)
	return enc.Encode(rows)
}

func cellString(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return t
	case json.Number:
		return t.String()
	case bool:
		return strconv.FormatBool(t)
	default:
		// Native scalars from a row a command assembled itself (the saved
		// query listing's IDs), nested arrays/objects, and anything
		// unexpected: JSON is the least ambiguous single-cell
		// representation, and renders an int exactly as strconv would.
		b, err := json.Marshal(t)
		if err != nil {
			return fmt.Sprint(t)
		}
		return string(b)
	}
}

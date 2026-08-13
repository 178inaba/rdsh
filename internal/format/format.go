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

// Formats accepted by Write.
const (
	FormatCSV  = "csv"
	FormatTSV  = "tsv"
	FormatJSON = "json"
)

// ValidateFormat rejects unsupported format names. Commands call it before
// doing expensive work so a typo fails before the query is submitted.
func ValidateFormat(format string) error {
	switch format {
	case FormatCSV, FormatTSV, FormatJSON:
		return nil
	}
	return fmt.Errorf("unsupported format %q (supported: csv, tsv, json)", format)
}

// Write renders the result in the given format. Rows arrive as maps keyed
// by column name, so CSV/TSV column order is taken from result.Columns.
func Write(w io.Writer, format string, result *redash.QueryResult) error {
	switch format {
	case FormatCSV:
		return writeSeparated(w, ',', result)
	case FormatTSV:
		return writeSeparated(w, '\t', result)
	case FormatJSON:
		return writeJSON(w, result)
	default:
		return ValidateFormat(format)
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
		// Nested arrays/objects and anything unexpected: JSON is the least
		// ambiguous single-cell representation.
		b, err := json.Marshal(t)
		if err != nil {
			return fmt.Sprint(t)
		}
		return string(b)
	}
}

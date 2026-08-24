package cmd

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/178inaba/rdsh/internal/redash"
)

// TestCollectPlaceholders pins the extraction against Redash's own, which
// reads the keys off a pystache parse tree: escape nodes and section nodes
// only. Everything else is a node kind Redash never collects, so treating
// one as a parameter would demand a definition the server does not want —
// and missing one would let an undefined parameter through.
func TestCollectPlaceholders(t *testing.T) {
	tests := []struct {
		name string
		sql  string
		want []placeholder
	}{
		{
			name: "escape node",
			sql:  "SELECT {{a}}",
			want: []placeholder{{name: "a"}},
		},
		{
			name: "surrounding whitespace is not part of the name",
			sql:  "SELECT {{  a\t}}",
			want: []placeholder{{name: "a"}},
		},
		{
			name: "section and its contents",
			sql:  "SELECT {{#s}}{{b}}{{/s}}",
			want: []placeholder{{name: "s", section: true}, {name: "b"}},
		},
		{
			name: "nested sections",
			sql:  "{{#x}}{{#y}}{{z}}{{/y}}{{/x}}",
			want: []placeholder{{name: "x", section: true}, {name: "y", section: true}, {name: "z"}},
		},
		{
			name: "inverted section is neither collected nor descended into",
			sql:  "SELECT {{^x}}{{b}}{{/x}}",
			want: nil,
		},
		{
			name: "inverted section nested in a plain one stops suppressing at its end",
			sql:  "{{#a}}{{^b}}{{c}}{{/b}}{{d}}{{/a}}",
			want: []placeholder{{name: "a", section: true}, {name: "d"}},
		},
		{
			name: "triple mustache is a literal node",
			sql:  "SELECT {{{x}}}",
			want: nil,
		},
		{
			name: "ampersand is a literal node too",
			sql:  "SELECT {{&x}}",
			want: nil,
		},
		{
			name: "comment",
			sql:  "SELECT 1 {{! not a parameter }}",
			want: nil,
		},
		{
			name: "partial",
			sql:  "SELECT {{>p}}",
			want: nil,
		},
		{
			name: "dotted key",
			sql:  "SELECT {{d.start}}",
			want: []placeholder{{name: "d.start"}},
		},
		{
			name: "delimiter change applies to what follows",
			sql:  "{{a}}{{=<% %>=}}<%b%>{{c}}",
			want: []placeholder{{name: "a"}, {name: "b"}},
		},
		{
			name: "repeats collapse in first-appearance order",
			sql:  "{{b}} {{a}} {{b}}",
			want: []placeholder{{name: "b"}, {name: "a"}},
		},
		{
			name: "no placeholders",
			sql:  "SELECT 1",
			want: nil,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := collectPlaceholders(tt.sql)
			if err != nil {
				t.Fatalf("collectPlaceholders(%q) error = %v", tt.sql, err)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("collectPlaceholders(%q) = %+v, want %+v", tt.sql, got, tt.want)
			}
		})
	}
}

// TestCollectPlaceholdersRejectsBrokenSections covers the templates pystache
// either refuses or silently reinterprets. Refusing both is the safe
// direction: a template rdsh cannot read the same way the server does is one
// whose coverage check would be meaningless.
func TestCollectPlaceholdersRejectsBrokenSections(t *testing.T) {
	tests := []struct {
		name string
		sql  string
	}{
		{name: "end tag for another section", sql: "{{#a}}{{/b}}"},
		{name: "end tag with no section open", sql: "SELECT {{/a}}"},
		{name: "section never closed", sql: "{{#a}}SELECT 1"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := collectPlaceholders(tt.sql); err == nil {
				t.Errorf("collectPlaceholders(%q) succeeded, want an error", tt.sql)
			}
		})
	}
}

func TestParseParamFlagsRejections(t *testing.T) {
	tests := []struct {
		name    string
		flags   paramFlagValues
		wantErr string
	}{
		{
			name:    "no name to bind to",
			flags:   paramFlagValues{defaults: []string{"days"}},
			wantErr: "--param-default",
		},
		{
			name:    "empty name",
			flags:   paramFlagValues{defaults: []string{"=7"}},
			wantErr: "--param-default",
		},
		{
			name:    "duplicate default",
			flags:   paramFlagValues{defaults: []string{"days=7", "days=30"}},
			wantErr: "twice",
		},
		{
			name:    "duplicate type",
			flags:   paramFlagValues{types: []string{"days=number", "days=text"}},
			wantErr: "twice",
		},
		{
			name:    "duplicate regex",
			flags:   paramFlagValues{regexes: []string{"c=a", "c=b"}},
			wantErr: "twice",
		},
		{
			name:    "unknown type",
			flags:   paramFlagValues{types: []string{"days=integer"}},
			wantErr: "integer",
		},
		{
			name:    "type rdsh cannot define",
			flags:   paramFlagValues{types: []string{"days=date-range"}},
			wantErr: "date-range",
		},
		{
			name:    "uncompilable pattern",
			flags:   paramFlagValues{regexes: []string{"c=[a-"}},
			wantErr: "--param-regex",
		},
		{
			name: "regex conflicting with an explicit type",
			flags: paramFlagValues{
				types:   []string{"c=text"},
				regexes: []string{"c=[a-z]+"},
			},
			wantErr: "text-pattern",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := parseParamFlags(tt.flags)
			if err == nil {
				t.Fatalf("parseParamFlags(%+v) succeeded, want an error", tt.flags)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("error = %v, want it to mention %q", err, tt.wantErr)
			}
		})
	}
}

// TestParseParamFlagsSplitsAtTheFirstEquals keeps a pattern carrying an =
// from needing an escape of its own.
func TestParseParamFlagsSplitsAtTheFirstEquals(t *testing.T) {
	set, err := parseParamFlags(paramFlagValues{
		defaults: []string{"expr=a=b"},
		regexes:  []string{"expr=^[a-z]+=[a-z]+$"},
	})
	if err != nil {
		t.Fatalf("parseParamFlags() error = %v", err)
	}
	f := set.byName["expr"]
	if f == nil || f.value != "a=b" || f.regex != "^[a-z]+=[a-z]+$" {
		t.Errorf("flags for expr = %+v, want the whole text after the first = as the value", f)
	}
}

// TestParseParamFlagsAllowsRedundantTextPattern covers the one combination
// that says the same thing twice: --param-regex already implies the type.
func TestParseParamFlagsAllowsRedundantTextPattern(t *testing.T) {
	if _, err := parseParamFlags(paramFlagValues{
		defaults: []string{"c=AB"},
		types:    []string{"c=text-pattern"},
		regexes:  []string{"c=[A-Z]+"},
	}); err != nil {
		t.Errorf("parseParamFlags() error = %v, want the redundant type to be allowed", err)
	}
}

// definitions runs the whole composition the way a command does, so a test
// states only the query as it stands and the flags that were passed.
func definitions(t *testing.T, existing []redash.QueryParameter, sql string,
	flags paramFlagValues) ([]redash.QueryParameter, error) {
	t.Helper()
	set, err := parseParamFlags(flags)
	if err != nil {
		return nil, err
	}
	return paramDefinitions(existing, set, sql, true)
}

func TestParamDefinitionsCreates(t *testing.T) {
	got, err := definitions(t, nil, "SELECT {{user_id}}, {{team}}", paramFlagValues{
		defaults: []string{"user_id=42", "team=core"},
		types:    []string{"user_id=number"},
	})
	if err != nil {
		t.Fatalf("paramDefinitions() error = %v", err)
	}
	want := []redash.QueryParameter{
		{Name: "user_id", Title: "user_id", Type: "number", Value: json.Number("42")},
		{Name: "team", Title: "team", Type: "text", Value: "core"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("paramDefinitions() = %+v, want %+v", got, want)
	}
}

// TestParamDefinitionsMergesPartially is the property an update rests on:
// only the keys the flags carry are overwritten, and everything else — the
// title someone set in the UI, the type, the other definitions — survives.
func TestParamDefinitionsMergesPartially(t *testing.T) {
	existing := []redash.QueryParameter{
		{Name: "since", Title: "Target date", Type: "date", Value: "2026-01-01"},
		{Name: "team", Title: "Team", Type: "text", Value: "core"},
	}
	got, err := definitions(t, existing, "SELECT {{since}}, {{team}}", paramFlagValues{
		defaults: []string{"since=2026-08-01"},
	})
	if err != nil {
		t.Fatalf("paramDefinitions() error = %v", err)
	}
	want := []redash.QueryParameter{
		{Name: "since", Title: "Target date", Type: "date", Value: "2026-08-01"},
		{Name: "team", Title: "Team", Type: "text", Value: "core"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("paramDefinitions() = %+v, want %+v", got, want)
	}
}

// TestParamDefinitionsAppendsAfterExisting keeps the definitions a query
// already holds where they are, so an update reads as an edit rather than as
// a rewrite of the whole list.
func TestParamDefinitionsAppendsAfterExisting(t *testing.T) {
	existing := []redash.QueryParameter{{Name: "team", Title: "Team", Type: "text", Value: "core"}}
	got, err := definitions(t, existing, "SELECT {{team}}, {{days}}", paramFlagValues{
		defaults: []string{"days=7"},
		types:    []string{"days=number"},
	})
	if err != nil {
		t.Fatalf("paramDefinitions() error = %v", err)
	}
	want := []redash.QueryParameter{
		{Name: "team", Title: "Team", Type: "text", Value: "core"},
		{Name: "days", Title: "days", Type: "number", Value: json.Number("7")},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("paramDefinitions() = %+v, want %+v", got, want)
	}
}

// TestParamDefinitionsRegexPromotesType covers the implication --param-regex
// carries: the pattern only means anything on a text-pattern parameter, so
// setting one sets the type with it.
func TestParamDefinitionsRegexPromotesType(t *testing.T) {
	existing := []redash.QueryParameter{{Name: "code", Title: "Code", Type: "text", Value: "AB12"}}
	got, err := definitions(t, existing, "SELECT {{code}}", paramFlagValues{
		regexes: []string{"code=[A-Z]{2}[0-9]{2}"},
	})
	if err != nil {
		t.Fatalf("paramDefinitions() error = %v", err)
	}
	want := []redash.QueryParameter{
		{Name: "code", Title: "Code", Type: "text-pattern", Value: "AB12", Regex: "[A-Z]{2}[0-9]{2}"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("paramDefinitions() = %+v, want %+v", got, want)
	}
}

// TestParamDefinitionsTypeOnlyUsesTheStoredRegex covers the other half of
// the consistency rule: the pattern an entry already holds is what makes
// --param-type text-pattern valid on its own.
func TestParamDefinitionsTypeOnlyUsesTheStoredRegex(t *testing.T) {
	existing := []redash.QueryParameter{
		{Name: "code", Title: "Code", Type: "text", Value: "AB12", Regex: "[A-Z]{2}[0-9]{2}"},
	}
	got, err := definitions(t, existing, "SELECT {{code}}", paramFlagValues{
		types: []string{"code=text-pattern"},
	})
	if err != nil {
		t.Fatalf("paramDefinitions() error = %v", err)
	}
	if len(got) != 1 || got[0].Type != "text-pattern" || got[0].Regex != "[A-Z]{2}[0-9]{2}" {
		t.Errorf("paramDefinitions() = %+v, want the stored pattern kept under the new type", got)
	}
}

// TestParamDefinitionsRevalidatesStoredValue covers a type change with no new
// default: the value the entry already holds is what has to pass, or the
// definition would be saved in a state Redash refuses at execution time.
func TestParamDefinitionsRevalidatesStoredValue(t *testing.T) {
	tests := []struct {
		name     string
		existing []redash.QueryParameter
		flags    paramFlagValues
		wantErr  bool
	}{
		{
			name:     "stored text is not a number",
			existing: []redash.QueryParameter{{Name: "n", Type: "text", Value: "abc"}},
			flags:    paramFlagValues{types: []string{"n=number"}},
			wantErr:  true,
		},
		{
			name:     "stored text is a number",
			existing: []redash.QueryParameter{{Name: "n", Type: "text", Value: "42"}},
			flags:    paramFlagValues{types: []string{"n=number"}},
		},
		{
			name:     "stored value does not match the new pattern",
			existing: []redash.QueryParameter{{Name: "n", Type: "text", Value: "zz"}},
			flags:    paramFlagValues{regexes: []string{"n=[0-9]+"}},
			wantErr:  true,
		},
		{
			name:     "no stored default to check",
			existing: []redash.QueryParameter{{Name: "n", Type: "text"}},
			flags:    paramFlagValues{types: []string{"n=number"}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := definitions(t, tt.existing, "SELECT {{n}}", tt.flags)
			if tt.wantErr != (err != nil) {
				t.Errorf("paramDefinitions() error = %v, want an error: %v", err, tt.wantErr)
			}
		})
	}
}

func TestParamDefinitionsValueTypes(t *testing.T) {
	tests := []struct {
		name      string
		typ       string
		value     string
		regex     string
		want      any
		wantErr   bool
		wantWhere string
	}{
		{name: "number", typ: "number", value: "42", want: json.Number("42")},
		{name: "negative number", typ: "number", value: "-1.5", want: json.Number("-1.5")},
		{
			name:  "integer too large for a float64",
			typ:   "number",
			value: "9007199254740993",
			want:  json.Number("9007199254740993"),
		},
		{name: "number that is not one", typ: "number", value: "abc", wantErr: true},
		{name: "number spelled as an infinity", typ: "number", value: "inf", wantErr: true},
		{name: "empty number", typ: "number", value: "", wantErr: true},
		{name: "date", typ: "date", value: "2026-08-01", want: "2026-08-01"},
		{name: "date in another layout", typ: "date", value: "2026/08/01", wantErr: true},
		{name: "date with a time", typ: "date", value: "2026-08-01 09:00", wantErr: true},
		{
			name:  "datetime-local",
			typ:   "datetime-local",
			value: "2026-08-01 09:00",
			want:  "2026-08-01 09:00",
		},
		{name: "datetime-local without a time", typ: "datetime-local", value: "2026-08-01", wantErr: true},
		{
			name:  "datetime-with-seconds",
			typ:   "datetime-with-seconds",
			value: "2026-08-01 09:00:30",
			want:  "2026-08-01 09:00:30",
		},
		{name: "text takes anything", typ: "text", value: "a b, c", want: "a b, c"},
		{name: "empty text is a value", typ: "text", value: "", want: ""},
		{
			name:  "text-pattern matching in full",
			typ:   "text-pattern",
			value: "AB12",
			regex: "[A-Z]{2}[0-9]{2}",
			want:  "AB12",
		},
		{
			name:    "text-pattern matching only a part",
			typ:     "text-pattern",
			value:   "xxAB12",
			regex:   "[A-Z]{2}[0-9]{2}",
			wantErr: true,
		},
		{
			name:  "alternation is anchored as a whole",
			typ:   "text-pattern",
			value: "b",
			regex: "a|b",
			want:  "b",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			flags := paramFlagValues{
				defaults: []string{"p=" + tt.value},
				types:    []string{"p=" + tt.typ},
			}
			if tt.regex != "" {
				flags.regexes = []string{"p=" + tt.regex}
			}
			got, err := definitions(t, nil, "SELECT {{p}}", flags)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("paramDefinitions() succeeded, want an error")
				}
				return
			}
			if err != nil {
				t.Fatalf("paramDefinitions() error = %v", err)
			}
			if len(got) != 1 || got[0].Value != tt.want {
				t.Errorf("paramDefinitions() = %+v, want the value %#v", got, tt.want)
			}
		})
	}
}

func TestParamDefinitionsRejections(t *testing.T) {
	tests := []struct {
		name     string
		existing []redash.QueryParameter
		sql      string
		flags    paramFlagValues
		wantErr  string
	}{
		{
			name:    "placeholder covered by nothing",
			sql:     "SELECT {{a}}, {{b}}",
			flags:   paramFlagValues{defaults: []string{"a=1"}},
			wantErr: "b",
		},
		{
			name:    "default for a name the SQL never uses",
			sql:     "SELECT {{a}}",
			flags:   paramFlagValues{defaults: []string{"a=1", "typo=2"}},
			wantErr: "typo",
		},
		{
			name:    "type without a default and without an entry",
			sql:     "SELECT {{a}}",
			flags:   paramFlagValues{defaults: []string{"a=1"}, types: []string{"b=number"}},
			wantErr: "b",
		},
		{
			name:    "text-pattern with no pattern anywhere",
			sql:     "SELECT {{a}}",
			flags:   paramFlagValues{defaults: []string{"a=1"}, types: []string{"a=text-pattern"}},
			wantErr: "--param-regex",
		},
		{
			name:     "existing definition rdsh cannot express",
			existing: []redash.QueryParameter{{Name: "status", Type: "enum", Value: "core"}},
			sql:      "SELECT {{status}}",
			flags:    paramFlagValues{defaults: []string{"status=active"}},
			wantErr:  "enum",
		},
		{
			name:     "existing definition carrying no type",
			existing: []redash.QueryParameter{{Name: "status", Value: "core"}},
			sql:      "SELECT {{status}}",
			flags:    paramFlagValues{defaults: []string{"status=active"}},
			wantErr:  "status",
		},
		{
			name:    "dotted placeholder",
			sql:     "SELECT {{d.start}}",
			flags:   paramFlagValues{},
			wantErr: "d.start",
		},
		{
			name:    "section placeholder",
			sql:     "SELECT {{#cond}}1{{/cond}}",
			flags:   paramFlagValues{},
			wantErr: "cond",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := definitions(t, tt.existing, tt.sql, tt.flags)
			if err == nil {
				t.Fatalf("paramDefinitions() succeeded, want an error")
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("error = %v, want it to mention %q", err, tt.wantErr)
			}
		})
	}
}

// TestParamDefinitionsIgnoresNonParameterTags is the coverage check's other
// side: a tag Redash never collects must not be demanded here either, or a
// query the server is happy with would be unsavable.
func TestParamDefinitionsIgnoresNonParameterTags(t *testing.T) {
	sql := "SELECT {{{raw}}}, {{&also}} {{!note}} {{>part}} {{^unless}}1{{/unless}}"
	got, err := definitions(t, nil, sql, paramFlagValues{})
	if err != nil {
		t.Fatalf("paramDefinitions() error = %v", err)
	}
	if got != nil {
		t.Errorf("paramDefinitions() = %+v, want no definitions", got)
	}
}

// TestParamDefinitionsSkipsSQLChecks covers the metadata-only update: a
// query whose placeholders no definition covers can still be renamed, which
// is what keeps the coverage check from blocking edits it has no business
// blocking.
func TestParamDefinitionsSkipsSQLChecks(t *testing.T) {
	set, err := parseParamFlags(paramFlagValues{})
	if err != nil {
		t.Fatalf("parseParamFlags() error = %v", err)
	}
	if _, err := paramDefinitions(nil, set, "SELECT {{a}}, {{d.start}}", false); err != nil {
		t.Errorf("paramDefinitions() error = %v, want the SQL left unchecked", err)
	}
}

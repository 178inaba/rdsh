package cmd

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/178inaba/rdsh/internal/redash"
)

// The parameter types rdsh can define: the ones whose whole definition is a
// name, a type and one scalar default. The rest — ranges, enums and
// dropdown queries — need values or extra fields (enumOptions, queryId)
// that no name=value flag expresses, so they stay the Redash UI's business.
const (
	paramTypeText        = "text"
	paramTypeTextPattern = "text-pattern"
	paramTypeNumber      = "number"
	paramTypeDate        = "date"
	paramTypeDateTime    = "datetime-local"
	paramTypeDateTimeSec = "datetime-with-seconds"
)

// dateParamLayouts is what a default has to be written as for each of the
// date types. The server parses these far more loosely (dateutil takes
// almost anything), so this is the stricter of the two contracts: one
// spelling per type, which is also what the Redash UI stores.
var dateParamLayouts = map[string]string{
	paramTypeDate:        "2006-01-02",
	paramTypeDateTime:    "2006-01-02 15:04",
	paramTypeDateTimeSec: "2006-01-02 15:04:05",
}

// scalarParamTypeNames is the same set in the order --param-type's help and
// errors list them, and the source the lookup below is built from so the two
// cannot name different sets.
var scalarParamTypeNames = []string{paramTypeText, paramTypeTextPattern, paramTypeNumber,
	paramTypeDate, paramTypeDateTime, paramTypeDateTimeSec}

// scalarParamTypes is that list as a lookup. A type outside it is one rdsh
// refuses to write — including on an entry that already carries it, since
// rewriting a definition that cannot be expressed would silently drop the
// fields that make it work.
var scalarParamTypes = func() map[string]bool {
	types := make(map[string]bool, len(scalarParamTypeNames))
	for _, name := range scalarParamTypeNames {
		types[name] = true
	}
	return types
}()

// paramFlagValues is the raw --param-* input of one invocation, in the order
// the flags were given.
type paramFlagValues struct {
	defaults []string
	types    []string
	regexes  []string
}

// given reports whether the invocation says anything about parameters at
// all, which is what decides whether an update composes an options object
// and whether its SQL is held to the coverage check.
func (v paramFlagValues) given() bool {
	return len(v.defaults) > 0 || len(v.types) > 0 || len(v.regexes) > 0
}

// paramFlags is everything one invocation says about one parameter name.
// Whether a flag was given is kept apart from its value, because an update
// overwrites only the keys the flags carry: --param-default name= sets an
// empty default, where leaving it out keeps whatever the query holds.
type paramFlags struct {
	value    string
	hasValue bool
	typ      string
	hasType  bool
	regex    string
	hasRegex bool
}

// paramFlagSet is the --param-* flags of one invocation, indexed by the name
// they bind to. order lists those names as they were first mentioned, so the
// definitions a command composes come out in a stable order rather than a
// map's.
type paramFlagSet struct {
	byName map[string]*paramFlags
	order  []string
}

// parseParamFlags reads the three --param-* flags into one set, refusing
// everything that can be judged from the flags alone. The rest of the
// checking needs the query, and lives in paramDefinitions.
func parseParamFlags(v paramFlagValues) (*paramFlagSet, error) {
	set := &paramFlagSet{byName: map[string]*paramFlags{}}
	take := func(flag string, tokens []string, apply func(*paramFlags, string) error) error {
		for _, token := range tokens {
			name, value, err := splitParamToken(flag, token)
			if err != nil {
				return err
			}
			f := set.byName[name]
			if f == nil {
				f = &paramFlags{}
				set.byName[name] = f
				set.order = append(set.order, name)
			}
			if err := apply(f, value); err != nil {
				return fmt.Errorf("%s %s: %w", flag, name, err)
			}
		}
		return nil
	}

	// A name given twice is refused rather than resolved last-wins: two
	// values for one parameter is far likelier a mistake than an intent,
	// and picking one silently is how the other ends up in the query.
	if err := take("--param-default", v.defaults, func(f *paramFlags, value string) error {
		if f.hasValue {
			return fmt.Errorf("given twice")
		}
		f.value, f.hasValue = value, true
		return nil
	}); err != nil {
		return nil, err
	}
	if err := take("--param-type", v.types, func(f *paramFlags, value string) error {
		if f.hasType {
			return fmt.Errorf("given twice")
		}
		if !scalarParamTypes[value] {
			return fmt.Errorf("%q is not a parameter type rdsh can define (%s); "+
				"the Redash UI defines the rest", value, strings.Join(scalarParamTypeNames, ", "))
		}
		f.typ, f.hasType = value, true
		return nil
	}); err != nil {
		return nil, err
	}
	if err := take("--param-regex", v.regexes, func(f *paramFlags, value string) error {
		if f.hasRegex {
			return fmt.Errorf("given twice")
		}
		// Compiled here so a broken pattern fails the command rather than
		// the executions that come later: Redash stores the definition
		// without looking at it, and runs the pattern with Python's re,
		// which accepts a little more than Go's does.
		if _, err := regexp.Compile(value); err != nil {
			return fmt.Errorf("%q is not a valid pattern: %w", value, err)
		}
		f.regex, f.hasRegex = value, true
		return nil
	}); err != nil {
		return nil, err
	}

	for _, name := range set.order {
		f := set.byName[name]
		if f.hasRegex && f.hasType && f.typ != paramTypeTextPattern {
			return nil, fmt.Errorf("--param-regex %s sets the type to %s, "+
				"which --param-type %s=%s contradicts", name, paramTypeTextPattern, name, f.typ)
		}
	}
	return set, nil
}

// paramDefinitions composes the parameter definitions one invocation saves,
// from the ones the query already holds and the flags that were passed.
// existing is nil on a create.
//
// checkSQL says whether the definitions are held against sql. It is always
// on for a create, and on for an update only when the invocation touches the
// SQL body or passes a --param-* flag — a rename has no business demanding
// definitions for a query that never had any.
//
// Everything is checked here rather than left to the server, because a
// broken definition is not refused: Redash stores it and recomputes the
// query hash, and the only symptom is a cached result that never links.
func paramDefinitions(existing []redash.QueryParameter, set *paramFlagSet, sql string,
	checkSQL bool) ([]redash.QueryParameter, error) {
	params, err := mergeParamDefinitions(existing, set)
	if err != nil {
		return nil, err
	}
	if checkSQL {
		if err := checkParamCoverage(sql, params, set); err != nil {
			return nil, err
		}
	}
	if err := checkParamValues(params, set); err != nil {
		return nil, err
	}
	return params, nil
}

// mergeParamDefinitions overlays the flags onto the definitions the query
// holds. The merge is per name and per key: a name no flag mentions is left
// alone, and a name one does keeps every key the flags do not carry.
func mergeParamDefinitions(existing []redash.QueryParameter,
	set *paramFlagSet) ([]redash.QueryParameter, error) {
	params := append([]redash.QueryParameter(nil), existing...)
	index := make(map[string]int, len(params))
	for i, p := range params {
		index[p.Name] = i
	}

	for _, name := range set.order {
		f := set.byName[name]
		i, ok := index[name]
		if !ok {
			// A type or a pattern with nothing to apply them to cannot form
			// a definition: Redash reads the default out of the entry, so
			// one without a value is the state this whole command exists to
			// avoid.
			if !f.hasValue {
				return nil, fmt.Errorf("%s has no default; a parameter rdsh defines needs "+
					"--param-default %s=<value>", name, name)
			}
			params = append(params, redash.QueryParameter{
				Name: name,
				// Set explicitly rather than left out: every definition the
				// Redash UI writes carries a title, and an absent one is not
				// a shape to rely on.
				Title: name,
				Type:  paramTypeText,
			})
			i = len(params) - 1
			index[name] = i
		} else if !scalarParamTypes[params[i].Type] {
			return nil, fmt.Errorf("%s is defined as %s, which rdsh cannot rewrite; "+
				"edit it in the Redash UI", name, describeParamType(params[i].Type))
		}

		p := &params[i]
		if f.hasType {
			p.Type = f.typ
		}
		if f.hasRegex {
			p.Regex, p.Type = f.regex, paramTypeTextPattern
		}
		if f.hasValue {
			p.Value = f.value
		}
		if p.Type == paramTypeTextPattern && p.Regex == "" {
			return nil, fmt.Errorf("%s is a %s parameter with no pattern; pass --param-regex %s=<pattern>",
				name, paramTypeTextPattern, name)
		}
	}
	return params, nil
}

// describeParamType names a stored type in an error. An entry with no type
// at all is a definition Redash itself chokes on, so it is reported as the
// missing field rather than as an empty string.
func describeParamType(typ string) string {
	if typ == "" {
		return "a definition with no type"
	}
	return typ
}

// checkParamCoverage holds the definitions against the SQL they belong to:
// every placeholder must be covered, no default may name one that is not
// there, and a placeholder no scalar definition can cover has to say so
// rather than pass silently.
func checkParamCoverage(sql string, params []redash.QueryParameter, set *paramFlagSet) error {
	placeholders, err := collectPlaceholders(sql)
	if err != nil {
		return err
	}

	// Reported before anything else: a placeholder of a kind rdsh cannot
	// define would otherwise look uncovered, and the suggestion to pass
	// --param-default for it would be one that cannot work.
	var unsupported []string
	for _, ph := range placeholders {
		if ph.section || strings.Contains(ph.name, ".") {
			unsupported = append(unsupported, ph.String())
		}
	}
	if len(unsupported) > 0 {
		return fmt.Errorf("%s needs a parameter type rdsh cannot define "+
			"(a section or a range); define it in the Redash UI", strings.Join(unsupported, ", "))
	}

	defined := make(map[string]bool, len(params))
	for _, p := range params {
		defined[p.Name] = true
	}
	named := make(map[string]bool, len(placeholders))
	for _, ph := range placeholders {
		named[ph.name] = true
	}

	// The typo check comes first because it explains the coverage failure
	// that usually follows it: a default spelled wrong leaves the parameter
	// it was meant for uncovered.
	var absent []string
	for _, name := range set.order {
		if set.byName[name].hasValue && !named[name] {
			absent = append(absent, name)
		}
	}
	if len(absent) > 0 {
		return fmt.Errorf("the SQL has no {{%s}} to give a default to",
			strings.Join(absent, "}}, no {{"))
	}

	var uncovered []string
	for _, ph := range placeholders {
		if !defined[ph.name] {
			uncovered = append(uncovered, ph.name)
		}
	}
	if len(uncovered) > 0 {
		return fmt.Errorf("no default is defined for {{%s}}; a parameter without one keeps the "+
			"query's shared result from ever linking, so pass --param-default %s=<value>",
			strings.Join(uncovered, "}}, {{"), uncovered[0])
	}
	return nil
}

// checkParamValues holds every definition the invocation touched to its
// effective type, and rewrites the defaults that came from a flag into the
// JSON the Redash UI would have stored. A stored default is checked but
// never rewritten: it is already the text the query's hash was taken over.
func checkParamValues(params []redash.QueryParameter, set *paramFlagSet) error {
	for i := range params {
		p := &params[i]
		f := set.byName[p.Name]
		if f == nil {
			continue
		}
		value, err := paramValue(p.Type, p.Regex, p.Value, f.hasValue)
		if err != nil {
			return fmt.Errorf("%s: %w", p.Name, err)
		}
		p.Value = value
	}
	return nil
}

// paramValue checks one default against the type it is stored under and
// returns what to store. fromFlag says the value arrived as flag text, which
// is the only case where it is converted — a number is sent as JSON's, the
// way the UI stores it, so the query hashes to the same text either tool
// wrote it.
func paramValue(typ, regex string, value any, fromFlag bool) (any, error) {
	// A definition with no default is a definition this command has nothing
	// to check: it is the state --param-default exists to leave, and a type
	// change alone does not create one.
	if value == nil {
		return nil, nil
	}
	if typ == paramTypeNumber {
		if number, ok := value.(json.Number); ok {
			return number, nil
		}
	}
	text, ok := value.(string)
	if !ok {
		return nil, fmt.Errorf("the stored default is not a value rdsh can check against %s; "+
			"edit it in the Redash UI", typ)
	}

	switch typ {
	case paramTypeText:
		return text, nil
	case paramTypeTextPattern:
		// Redash matches the whole value (re.fullmatch), which Go has no
		// call for; anchoring the pattern as a group is the same thing and
		// keeps a top-level alternation from binding to one side only.
		matcher, err := regexp.Compile(`\A(?:` + regex + `)\z`)
		if err != nil {
			return nil, fmt.Errorf("the stored pattern %q does not compile: %w", regex, err)
		}
		if !matcher.MatchString(text) {
			return nil, fmt.Errorf("the default %q does not match %s in full", text, regex)
		}
		return text, nil
	case paramTypeNumber:
		// Judged as JSON rather than with strconv so what passes is exactly
		// what can be sent as a JSON number: no infinities, no leading or
		// trailing space, and a large integer kept as the text it was
		// written with rather than rounded through a float64.
		var number json.Number
		if err := json.Unmarshal([]byte(text), &number); err != nil {
			return nil, fmt.Errorf("the default %q is not a number", text)
		}
		if !fromFlag {
			return text, nil
		}
		return number, nil
	}

	layout, ok := dateParamLayouts[typ]
	if !ok {
		return nil, fmt.Errorf("rdsh cannot check a default of type %s", describeParamType(typ))
	}
	if _, err := time.Parse(layout, text); err != nil {
		return nil, fmt.Errorf("the default %q is not a %s written as %s", text, typ, layout)
	}
	return text, nil
}

// placeholder is one parameter reference found in a query's SQL.
type placeholder struct {
	name string
	// section marks a {{#name}} block. Redash counts one as a parameter,
	// but what fills it is a value no scalar definition holds.
	section bool
}

func (p placeholder) String() string {
	if p.section {
		return "{{#" + p.name + "}}"
	}
	return "{{" + p.name + "}}"
}

// mustacheDelimiters is the pair a template starts with, before any
// {{=<% %>=}} in it says otherwise.
var mustacheDelimiters = [2]string{"{{", "}}"}

// collectPlaceholders finds the parameters a query's SQL asks for, matching
// what Redash collects rather than what looks like a placeholder: the server
// reads the keys off a pystache parse tree (_collect_key_names), taking
// escape nodes ({{name}}) and section nodes ({{#name}}, descending into
// them) and nothing else. A triple mustache, an &, an inverted section, a
// comment and a partial are all other node kinds, so none of them is a
// parameter — and treating one as such would demand a definition that then
// makes the query unrunnable, since a defined schema refuses any name
// outside it.
//
// Names come back deduplicated in first appearance order, as funcy.distinct
// leaves them on the server. What pystache does with the whitespace around a
// standalone tag is not reproduced: it only moves where the literal text
// between tags is cut, never which tags are found.
func collectPlaceholders(sql string) ([]placeholder, error) {
	delimiters := mustacheDelimiters
	tag, err := compileMustacheTag(delimiters)
	if err != nil {
		return nil, err
	}

	var (
		found []placeholder
		seen  = map[string]bool{}
		// open is the sections still waiting for their end tag. Anything
		// under an inverted one is skipped at any depth, because the server
		// never descends into that node.
		open    []openSection
		rest    = sql
		collect = func(p placeholder) {
			if underInverted(open) || seen[p.name] {
				return
			}
			seen[p.name] = true
			found = append(found, p)
		}
	)
	for {
		m := tag.FindStringSubmatchIndex(rest)
		if m == nil {
			break
		}
		// The submatch indices point into the text as it was before the
		// cursor moved past this tag, so that text is what they are read
		// from.
		src := rest
		rest = rest[m[1]:]
		group := func(i int) string {
			if m[2*i] < 0 {
				return ""
			}
			return src[m[2*i]:m[2*i+1]]
		}
		matched := src[m[0]:m[1]]

		switch {
		case group(tagChange) != "":
			// A delimiter change rebuilds the pattern for everything after
			// it, the way pystache recompiles its own.
			parts := strings.Fields(group(tagDelimiters))
			if len(parts) != 2 {
				return nil, fmt.Errorf("%s does not set two delimiters", matched)
			}
			delimiters = [2]string{parts[0], parts[1]}
			if tag, err = compileMustacheTag(delimiters); err != nil {
				return nil, err
			}
			continue
		case group(tagRaw) != "":
			// {{{name}}}, which pystache normalises to & — a literal node,
			// so not a parameter.
			continue
		}

		key := group(tagKey)
		switch group(tagType) {
		case "#":
			collect(placeholder{name: key, section: true})
			open = append(open, openSection{key: key})
		case "^":
			open = append(open, openSection{key: key, inverted: true})
		case "/":
			if len(open) == 0 {
				return nil, fmt.Errorf("%s closes a section that was never opened", matched)
			}
			if last := open[len(open)-1]; last.key != key {
				return nil, fmt.Errorf("%s closes {{#%s}}, which is not the section it ends", matched, last.key)
			}
			open = open[:len(open)-1]
		case "":
			collect(placeholder{name: key})
		}
	}
	if len(open) > 0 {
		// pystache does not fail here — it quietly drops the unclosed
		// section and everything outside it — so refusing is the safer
		// reading: a template the server and rdsh parse differently is one
		// whose coverage check proves nothing.
		return nil, fmt.Errorf("{{#%s}} is never closed", open[len(open)-1].key)
	}
	return found, nil
}

// openSection is a section tag whose end tag has not been seen yet.
type openSection struct {
	key      string
	inverted bool
}

// underInverted reports whether the tag being read sits inside an inverted
// section, which is the part of the template the server's collection never
// looks at.
func underInverted(open []openSection) bool {
	for _, s := range open {
		if s.inverted {
			return true
		}
	}
	return false
}

// Submatch indices of the pattern compileMustacheTag builds.
const (
	tagChange = iota + 1
	tagDelimiters
	tagRaw
	tagRawName
	tagType
	tagKey
)

// compileMustacheTag builds the tag pattern for one pair of delimiters. It
// is pystache's own, minus the leading-whitespace capture that only the
// standalone-tag handling uses: an optional delimiter-change, a triple
// mustache, or a type character and a key. The two inner alternatives keep
// pystache's `.` where it has one, so a tag body carrying a newline is only
// a tag where pystache reads one.
func compileMustacheTag(delimiters [2]string) (*regexp.Regexp, error) {
	return regexp.Compile(regexp.QuoteMeta(delimiters[0]) + `\s*(?:` +
		`(=)\s*(.+?)\s*=` + `|` +
		`(\{)\s*(.+?)\s*\}` + `|` +
		`([!>&/#^]?)\s*([\s\S]+?)` +
		`)\s*` + regexp.QuoteMeta(delimiters[1]))
}

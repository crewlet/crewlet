package dashspec

import (
	"encoding/json"
	"fmt"
	"math"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ColumnType is what one column of a dataset holds.
type ColumnType string

const (
	// ColumnText is a string.
	ColumnText ColumnType = "text"

	// ColumnNumber is a finite number.
	ColumnNumber ColumnType = "number"

	// ColumnDate is a calendar day (`YYYY-MM-DD`) or an instant (RFC 3339,
	// held in UTC).
	ColumnDate ColumnType = "date"

	// ColumnBoolean is true or false.
	ColumnBoolean ColumnType = "boolean"
)

// ColumnTypes are every column type a dataset may hold.
var ColumnTypes = []ColumnType{ColumnText, ColumnNumber, ColumnDate, ColumnBoolean}

// Valid reports whether t is a column type this build draws.
func (t ColumnType) Valid() bool { return slices.Contains(ColumnTypes, t) }

// Column is one declared column of a dataset.
type Column struct {
	Key   string     `json:"key"`
	Type  ColumnType `json:"type"`
	Label string     `json:"label,omitempty"`

	// Words is the vocabulary a text column's values are drawn in: its
	// values stay the domain's ids (`in_progress`), which a filter or a
	// calculation compares, and a reader is shown the words the domain's
	// own screens show for them (In progress).
	Words Words `json:"words,omitempty"`
}

// Words is a closed vocabulary a column's values are named in.
type Words string

// The vocabularies a column may be drawn in.
const (
	WordsNone               Words = ""
	WordsTrackerStatus      Words = "tracker_status"
	WordsTrackerStatusGroup Words = "tracker_status_group"
	WordsTrackerPriority    Words = "tracker_priority"
	WordsAgentActivity      Words = "agent_activity"
)

// WordSets are every vocabulary a column may name.
var WordSets = []Words{WordsTrackerStatus, WordsTrackerStatusGroup, WordsTrackerPriority,
	WordsAgentActivity}

// Valid reports whether w is no vocabulary or one this build draws.
func (w Words) Valid() bool { return w == WordsNone || slices.Contains(WordSets, w) }

// Row is one row of a dataset: each value nil, a string, a float64 or a bool,
// a date held as its canonical text ([Coerce]).
type Row map[string]any

// Table is a dataset's rows as one viewer may read them.
type Table struct {
	Columns []Column `json:"columns"`

	// Key is the column naming a row, for a comment's anchor and the
	// Sources rail's lit cell; empty for a dataset with no natural key.
	Key string `json:"key,omitempty"`

	Rows []Row `json:"rows"`

	// Truncated reports rows past [artifacts.MaxDatasetRows], or past a
	// source's own page, were not answered.
	Truncated bool `json:"truncated,omitempty"`
}

// column is t's column named key.
func (t Table) column(key string) (Column, bool) {
	i := slices.IndexFunc(t.Columns, func(c Column) bool { return c.Key == key })
	if i < 0 {
		return Column{}, false
	}
	return t.Columns[i], true
}

// idPattern is the grammar of a dataset's, a widget's and a filter's id:
// short, lowercase and safe in a URL path and a presence region.
// Compiled at first use, not at init: the code host's push hook is this
// binary, and pays every package's init (cmd/crewlet's hookinit_test.go).
var idPattern = sync.OnceValue(func() *regexp.Regexp {
	return regexp.MustCompile(`^[a-z][a-z0-9_]{0,39}$`)
})

// columnPattern is the grammar of a column key: a datastore field name or a
// file header, within the datastore's field name bound.
// Compiled at first use, not at init: the code host's push hook is this
// binary, and pays every package's init (cmd/crewlet's hookinit_test.go).
var columnPattern = sync.OnceValue(func() *regexp.Regexp {
	return regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]{0,63}$`)
})

// ValidID reports whether id is a dataset, widget or filter id.
func ValidID(id string) bool { return idPattern().MatchString(id) }

// ValidColumnKey reports whether key is a column key.
func ValidColumnKey(key string) bool { return columnPattern().MatchString(key) }

// dayLayout is a calendar day.
const dayLayout = "2006-01-02"

// Coerce is v as a value of a column of type t, in the one canonical form
// every row holds: a finite float64 for a number, a bool, a day as
// `YYYY-MM-DD` or an instant as RFC 3339 in UTC, a string. Nil and an empty
// string are nil, an absence rather than a zero. A value that is not one of
// the type's is an error naming it, never a guess.
func Coerce(t ColumnType, v any) (any, error) {
	if v == nil {
		return nil, nil
	}
	if s, ok := v.(string); ok && strings.TrimSpace(s) == "" {
		return nil, nil
	}
	switch t {
	case ColumnText:
		switch x := v.(type) {
		case string:
			return x, nil
		case float64:
			return strconv.FormatFloat(x, 'f', -1, 64), nil
		case json.Number:
			return x.String(), nil
		case bool:
			return strconv.FormatBool(x), nil
		}
	case ColumnNumber:
		var f float64
		switch x := v.(type) {
		case float64:
			f = x
		case int:
			f = float64(x)
		case int64:
			f = float64(x)
		case json.Number:
			parsed, err := x.Float64()
			if err != nil {
				return nil, fmt.Errorf("%q is not a number", x.String())
			}
			f = parsed
		case string:
			parsed, err := strconv.ParseFloat(strings.TrimSpace(x), 64)
			if err != nil {
				return nil, fmt.Errorf("%q is not a number", x)
			}
			f = parsed
		default:
			return nil, fmt.Errorf("%v is not a number", v)
		}
		if math.IsNaN(f) || math.IsInf(f, 0) {
			return nil, fmt.Errorf("%v is not a finite number", v)
		}
		return f, nil
	case ColumnBoolean:
		switch x := v.(type) {
		case bool:
			return x, nil
		case string:
			switch strings.ToLower(strings.TrimSpace(x)) {
			case "true":
				return true, nil
			case "false":
				return false, nil
			}
		}
		return nil, fmt.Errorf("%v is not true or false", v)
	case ColumnDate:
		s, ok := v.(string)
		if !ok {
			return nil, fmt.Errorf("%v is not a date", v)
		}
		s = strings.TrimSpace(s)
		if d, err := time.Parse(dayLayout, s); err == nil {
			return d.Format(dayLayout), nil
		}
		at, err := time.Parse(time.RFC3339Nano, s)
		if err != nil {
			return nil, fmt.Errorf("%q is not a date (YYYY-MM-DD) or an instant (RFC 3339)", s)
		}
		return at.UTC().Format(time.RFC3339Nano), nil
	default:
		return nil, fmt.Errorf("the column type %q is not one this build draws", t)
	}
	return nil, fmt.Errorf("%v is not %s", v, t)
}

// instant is a canonical date value as an instant: a day is its midnight in
// UTC, which is where [Coerce] leaves it and how two days compare.
func instant(v any) (time.Time, bool) {
	s, ok := v.(string)
	if !ok {
		return time.Time{}, false
	}
	if d, err := time.Parse(dayLayout, s); err == nil {
		return d, true
	}
	at, err := time.Parse(time.RFC3339Nano, s)
	return at, err == nil
}

// compare orders two canonical values of one column type: nil before every
// value, numbers numerically, dates chronologically, false before true, text
// bytewise. It is total and deterministic, which is what lets a group's rows
// and a sort's come out in one order on every node.
func compare(t ColumnType, a, b any) int {
	switch {
	case a == nil && b == nil:
		return 0
	case a == nil:
		return -1
	case b == nil:
		return 1
	}
	switch t {
	case ColumnNumber:
		x, _ := a.(float64)
		y, _ := b.(float64)
		switch {
		case x < y:
			return -1
		case x > y:
			return 1
		}
		return 0
	case ColumnDate:
		x, okx := instant(a)
		y, oky := instant(b)
		if okx && oky {
			return x.Compare(y)
		}
	case ColumnBoolean:
		x, _ := a.(bool)
		y, _ := b.(bool)
		switch {
		case x == y:
			return 0
		case !x:
			return -1
		}
		return 1
	}
	return strings.Compare(fmt.Sprint(a), fmt.Sprint(b))
}

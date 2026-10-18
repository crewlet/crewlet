package query_test

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/crewlet/crewlet/internal/artifactdata/query"
	"github.com/crewlet/crewlet/internal/artifacts"
)

// index is a collection whose top-level fields and `meta.city` are indexed,
// with every path but `never` interned.
type index struct{}

func (index) Indexed(path string) bool {
	return !strings.Contains(path, ".") || path == "meta.city"
}

func (index) PathNo(path string) (int64, bool) {
	if path == "never" {
		return 0, false
	}
	return int64(len(path)), true
}

func f(path string, op query.Op, value string) query.Filter {
	return query.Filter{Path: path, Op: op, Value: json.RawMessage(value)}
}

// A QUERY THE GRAMMAR DOES NOT EXPRESS IS REFUSED, NAMING THE FIX, and never
// compiled into a scan: an unindexed path, an unknown op, a malformed path,
// too many filters, a page past the bound, an `in` without values, a range on
// a boolean, and a cursor nobody minted.
func TestAQueryOutsideTheGrammarIsRefused(t *testing.T) {
	t.Parallel()
	many := make([]query.Filter, query.MaxFilters+1)
	for i := range many {
		many[i] = f("a", query.OpEq, `1`)
	}
	for name, tc := range map[string]struct {
		q    query.Query
		want error
	}{
		"an unindexed dotted path": {query.Query{Where: []query.Filter{f("meta.zip", query.OpEq, `"x"`)}}, query.ErrNotIndexed},
		"an unknown op":            {query.Query{Where: []query.Filter{f("a", "like", `"x"`)}}, query.ErrInvalid},
		"a path two levels deep":   {query.Query{Where: []query.Filter{f("a.b.c", query.OpEq, `1`)}}, query.ErrInvalid},
		"an engine field":          {query.Query{Where: []query.Filter{f("_meta", query.OpEq, `1`)}}, query.ErrInvalid},
		"too many filters":         {query.Query{Where: many}, query.ErrInvalid},
		"a page past the bound":    {query.Query{Limit: artifacts.MaxQueryLimit + 1}, query.ErrInvalid},
		"an in without values":     {query.Query{Where: []query.Filter{{Path: "a", Op: query.OpIn}}}, query.ErrInvalid},
		"a range on a boolean":     {query.Query{Where: []query.Filter{f("a", query.OpGt, `true`)}}, query.ErrInvalid},
		"an array literal":         {query.Query{Where: []query.Filter{f("a", query.OpEq, `[1]`)}}, query.ErrInvalid},
		"an empty prefix":          {query.Query{Where: []query.Filter{f("a", query.OpPrefix, `""`)}}, query.ErrInvalid},
		"a forged cursor":          {query.Query{After: "not-a-cursor"}, query.ErrInvalid},
		"an unindexed order":       {query.Query{OrderBy: &query.Order{Path: "meta.zip"}}, query.ErrNotIndexed},
	} {
		if _, err := query.Compile(tc.q, query.Scope{StoreNo: 1, CollectionNo: 1}, index{}); !errors.Is(err, tc.want) {
			t.Errorf("%s: %v, want %v", name, err, tc.want)
		}
	}
}

// A COMPILED QUERY BINDS EVERY VALUE AND INTERPOLATES NONE: what a caller
// sends never reaches the SQL text, and the scope's creator restriction is
// always a bound argument.
func TestACompiledQueryBindsEveryValue(t *testing.T) {
	t.Parallel()
	hostile := `"x' OR 1=1 --"`
	c, err := query.Compile(query.Query{Where: []query.Filter{
		f("a", query.OpEq, hostile), f("b", query.OpPrefix, hostile),
		{Path: "c", Op: query.OpIn, Values: []json.RawMessage{json.RawMessage(hostile)}},
	}, OrderBy: &query.Order{Path: "a", Desc: true}},
		query.Scope{StoreNo: 7, CollectionNo: 3, CreatedBy: "bob' --"}, index{})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(c.SQL, "OR 1=1") || strings.Contains(c.SQL, "bob") {
		t.Fatalf("a value reached the SQL text: %s", c.SQL)
	}
	if got := strings.Count(c.SQL, "?"); got != len(c.Args) {
		t.Fatalf("the SQL carries %d placeholders and %d arguments", got, len(c.Args))
	}
	if c.Limit != query.DefaultLimit+1 {
		t.Fatalf("the default page asks %d rows, want %d", c.Limit, query.DefaultLimit+1)
	}
}

// A PATH NO DOCUMENT HAS EVER HELD MATCHES NOTHING, and its absence is
// everything a `ne` asks.
func TestAPathNobodyHeldMatchesNothing(t *testing.T) {
	t.Parallel()
	eq, err := query.Compile(query.Query{Where: []query.Filter{f("never", query.OpEq, `1`)}},
		query.Scope{StoreNo: 1, CollectionNo: 1}, index{})
	if err != nil || !strings.Contains(eq.SQL, "1 = 0") {
		t.Fatalf("eq on a path nobody held: %s, %v", eq.SQL, err)
	}
	ne, err := query.Compile(query.Query{Where: []query.Filter{f("never", query.OpNe, `1`)}},
		query.Scope{StoreNo: 1, CollectionNo: 1}, index{})
	if err != nil || !strings.Contains(ne.SQL, "1 = 1") {
		t.Fatalf("ne on a path nobody held: %s, %v", ne.SQL, err)
	}
}

// THE INDEX'S TEXT IS A STRING'S PREFIX CUT AT A RUNE BOUNDARY, so it is always
// valid UTF-8, and a long literal is re-checked against the body.
func TestTheIndexKeepsAValidPrefixAndRechecksALongString(t *testing.T) {
	t.Parallel()
	long := strings.Repeat("a", query.TextIndexBytes-1) + "é" + "tail"
	cut := query.TruncateText(long)
	if !utf8.ValidString(cut) || len(cut) > query.TextIndexBytes || !strings.HasPrefix(long, cut) {
		t.Fatalf("the index keeps %q (%d bytes)", cut[len(cut)-4:], len(cut))
	}
	c, err := query.Compile(query.Query{Where: []query.Filter{f("a", query.OpEq, `"`+long+`"`)}},
		query.Scope{StoreNo: 1, CollectionNo: 1}, index{})
	if err != nil || len(c.Recheck) != 1 {
		t.Fatalf("a long literal: recheck %v, %v", c.Recheck, err)
	}
	keep, err := c.Keep([]byte(`{"a":"` + long + `"}`))
	if err != nil || !keep {
		t.Fatalf("the exact body is not kept: %v, %v", keep, err)
	}
	keep, err = c.Keep([]byte(`{"a":"` + long + `x"}`))
	if err != nil || keep {
		t.Fatalf("a longer string sharing the prefix is kept: %v, %v", keep, err)
	}
}

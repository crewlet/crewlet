package argval_test

import (
	"encoding/json"
	"math"
	"reflect"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/tools/argval"
)

// EACH READ IS ITS HELPER'S, case for case: what the builtin tools accepted
// before the rule moved here is what they accept now, and what the surface's
// check of a call reads a value as. A change to a rule is a change to every
// tool and to the check at once, so it is made here, in this table.
//
// Mutation: make Whole truncate a fraction, or Bool read "true", and a row
// below fails.
func TestEachReadIsItsHelpers(t *testing.T) {
	t.Parallel()
	type read struct {
		v  any
		ok bool
	}
	big := json.Number("9007199254740993")
	strCases := map[string]struct {
		in   any
		want string
		ok   bool
	}{
		"a string":      {"a", "a", true},
		"null":          {nil, "", true},
		"a number":      {2.5, "2.5", true},
		"a literal":     {json.Number("1e3"), "1e3", true},
		"a boolean":     {true, "true", true},
		"an object":     {map[string]any{"a": 1}, "", false},
		"a list":        {[]any{"a"}, "", false},
		"a big literal": {big, "9007199254740993", true},
	}
	for name, c := range strCases {
		if got, ok := argval.String(c.in); got != c.want || ok != c.ok {
			t.Errorf("String(%s) = %q, %v; want %q, %v", name, got, ok, c.want, c.ok)
		}
	}
	listCases := map[string]struct {
		in   any
		want []string
		ok   bool
	}{
		"a list":           {[]any{" a ", "b", "a", 3.0, map[string]any{}}, []string{"a", "b", "3"}, true},
		"one string":       {"backend", []string{"backend"}, true},
		"a comma string":   {"backend, api,,", []string{"backend", "api"}, true},
		"a list of string": {[]string{"x", ""}, []string{"x"}, true},
		"null":             {nil, nil, true},
		"a number":         {3.0, nil, false},
		"an object":        {map[string]any{}, nil, false},
	}
	for name, c := range listCases {
		if got, ok := argval.Strings(c.in); !reflect.DeepEqual(got, c.want) || ok != c.ok {
			t.Errorf("Strings(%s) = %q, %v; want %q, %v", name, got, ok, c.want, c.ok)
		}
	}
	type numCase struct {
		in     any
		prefix read
		whole  read
		float  read
		number read
	}
	for name, c := range map[string]numCase{
		"a whole number": {3.0, read{3, true}, read{3, true}, read{3.0, true}, read{3.0, true}},
		"a fraction":     {2.5, read{2, true}, read{0, false}, read{2.5, true}, read{2.5, true}},
		"an exact big":   {big, read{9007199254740993, true}, read{nil, true}, read{nil, true}, read{nil, true}},
		"digits":         {" 42 ", read{42, true}, read{42, true}, read{42.0, true}, read{42.0, true}},
		"a prefix":       {"5 days", read{5, true}, read{0, false}, read{0.0, false}, read{0.0, false}},
		"a decimal text": {"2.5", read{2, true}, read{0, false}, read{2.5, true}, read{2.5, true}},
		"NaN text":       {"NaN", read{0, false}, read{0, false}, read{nil, true}, read{nil, false}},
		"infinity":       {math.Inf(1), read{nil, true}, read{0, false}, read{nil, true}, read{nil, false}},
		"a boolean":      {true, read{0, false}, read{0, false}, read{0.0, false}, read{0.0, false}},
		"an int":         {7, read{7, true}, read{7, true}, read{7.0, true}, read{7.0, true}},
		"an int64":       {int64(7), read{7, true}, read{7, true}, read{0.0, false}, read{7.0, true}},
	} {
		check := func(fn string, got any, ok bool, want read) {
			if ok != want.ok || (want.v != nil && got != want.v) {
				t.Errorf("%s(%s) = %v, %v; want %v, %v", fn, name, got, ok, want.v, want.ok)
			}
		}
		n, ok := argval.IntPrefix(c.in)
		check("IntPrefix", n, ok, c.prefix)
		w, ok := argval.Whole(c.in)
		check("Whole", w, ok, c.whole)
		f, ok := argval.Float(c.in)
		check("Float", f, ok, c.float)
		x, ok := argval.Number(c.in)
		check("Number", x, ok, c.number)
	}
	for _, v := range []any{"true", "false", 1.0, "yes", nil} {
		if _, ok := argval.Bool(v); ok {
			t.Errorf("Bool(%v) read a boolean", v)
		}
	}
	if b, ok := argval.Bool(true); !b || !ok {
		t.Error("Bool(true) is not true")
	}
}

// A LIST READ ELEMENT BY ELEMENT is a list as it is, or the single string a
// model sends for a list of one (or a comma-separated one), and nothing else:
// a list of objects is a list or nothing.
func TestAListIsReadAsTheCheckReadsIt(t *testing.T) {
	t.Parallel()
	for in, want := range map[string]struct {
		items []any
		ok    bool
	}{
		`["a", 3]`: {[]any{"a", json.Number("3")}, true},
		`"GP-12"`:  {[]any{"GP-12"}, true},
		`"a, ,b"`:  {[]any{"a", "b"}, true},
		`12`:       {nil, false},
		`{"a": 1}`: {nil, false},
		`null`:     {nil, false},
	} {
		var v any
		dec := json.NewDecoder(strings.NewReader(in))
		dec.UseNumber()
		if err := dec.Decode(&v); err != nil {
			t.Fatal(err)
		}
		if got, ok := argval.StringItems(v); ok != want.ok || !reflect.DeepEqual(got, want.items) {
			t.Errorf("StringItems(%s) = %v, %v; want %v, %v", in, got, ok, want.items, want.ok)
		}
	}
	if _, ok := argval.List("x"); ok {
		t.Error("List read a string as a list of objects")
	}
	if got, ok := argval.List([]any{map[string]any{}}); !ok || len(got) != 1 {
		t.Error("List did not read a list")
	}
}

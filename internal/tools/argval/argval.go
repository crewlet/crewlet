// Package argval is how a first-party tool reads one argument a model sent:
// the one rule for what each kind of value is read as, which the builtin
// tools' helpers and the surface's check of a call against its schema both
// call, so the two cannot read one value two ways (ADR-0059).
//
// THE RULES ARE THE TOOLS' OWN, moved here unchanged: a string field takes a
// number or a boolean rendered as the model wrote it, a list of strings takes
// one string or a comma-separated one, the lenient integer reads the leading
// digits of "5 days", the whole and finite reads refuse a fraction and a
// non-finite number, and a boolean is only true or false. None is looser or
// tighter than the helper it came from; a table test holds each to the cases
// its helper's callers were written against.
package argval

import (
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"strings"
)

// String is a value as a string argument reads it: a string as it is, a
// number or a boolean as the model wrote it (refusing one teaches nothing),
// and null as absent. ok is false for an object or a list, which the read
// takes as absent too, and which a check of the call names.
func String(v any) (string, bool) {
	switch v := v.(type) {
	case string:
		return v, true
	case nil:
		return "", true
	case float64:
		return fmt.Sprintf("%g", v), true
	case json.Number:
		// THE LITERAL, not `%g` of it: this is the spelling the model
		// sent, and it is exact where the float rendering is not.
		return v.String(), true
	case bool:
		return fmt.Sprintf("%t", v), true
	}
	return "", false
}

// Strings is a value as a list-of-strings argument reads it: a list, each
// element read as [String] (one it cannot read dropped), or the single string
// a model sends where a list belongs, split on commas ("backend, api" is a
// list written as prose far more often than one label holding a comma). Each
// is trimmed, the empty ones dropped and the repeats removed, in order. ok is
// false for any other value, which the read takes as absent.
func Strings(v any) ([]string, bool) {
	switch v := v.(type) {
	case []string:
		return clean(v), true
	case []any:
		out := make([]string, 0, len(v))
		for _, item := range v {
			s, _ := String(item)
			out = append(out, s)
		}
		return clean(out), true
	case string:
		return clean(strings.Split(v, ",")), true
	case nil:
		return nil, true
	}
	return nil, false
}

// clean trims, drops empties and deduplicates, preserving order.
func clean(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	seen := make(map[string]bool, len(in))
	out := make([]string, 0, len(in))
	for _, v := range in {
		v = strings.TrimSpace(v)
		if v == "" || seen[v] {
			continue
		}
		seen[v] = true
		out = append(out, v)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// IntPrefix is the lenient integer read: a number truncated toward zero, an
// integer literal exact past 2^53, or a string's leading integer ("5 days" is
// 5). ok is false for anything else, which the read takes as its fallback.
func IntPrefix(v any) (int, bool) {
	switch v := v.(type) {
	case float64:
		return int(v), true
	case json.Number:
		// INT64 FIRST, because that is the whole reason the decoders on
		// this path read through json.Number: an id past 2^53 is exact
		// here and rounded the moment it becomes a float.
		if i, err := v.Int64(); err == nil {
			return int(i), true
		}
		if f, err := v.Float64(); err == nil {
			return int(f), true
		}
	case int:
		return v, true
	case int64:
		return int(v), true
	case string:
		var n int
		if _, err := fmt.Sscanf(v, "%d", &n); err == nil {
			return n, true
		}
	}
	return 0, false
}

// Whole is the strict integer read, for a value whose zero is a setting
// rather than an absence (zero minutes, a precision of 0): a finite whole
// number, or a string that is one whole integer and nothing else. A fraction
// is refused rather than truncated to one nobody typed, and "2 days" is not
// 2.
func Whole(v any) (int, bool) {
	switch v := v.(type) {
	case json.Number:
		// BACK THROUGH THIS FUNCTION rather than repeating the
		// whole-number and finiteness discipline below. An integer past
		// 2^53 is none of the things this reads (minutes, story points,
		// decimal places), so the float is the honest intermediate here,
		// unlike in [IntPrefix].
		f, err := v.Float64()
		if err != nil {
			return 0, false
		}
		return Whole(f)
	case float64:
		// FINITE FIRST, because the fraction test does not cover it: an
		// infinity IS its own truncation, and `int(+Inf)` is not defined
		// by the language.
		if !Finite(v) || v != math.Trunc(v) {
			return 0, false
		}
		return int(v), true
	case int:
		return v, true
	case int64:
		return int(v), true
	case string:
		n, err := strconv.Atoi(strings.TrimSpace(v))
		return n, err == nil
	}
	return 0, false
}

// Float is the lenient number read: a number, or a string that parses as one.
// ok is false for anything else, which the read takes as zero (its callers'
// default). It admits what [strconv.ParseFloat] admits, the non-finite
// spellings included; [Number] is the read that refuses them.
func Float(v any) (float64, bool) {
	switch v := v.(type) {
	case float64:
		return v, true
	case json.Number:
		if f, err := v.Float64(); err == nil {
			return f, true
		}
	case int:
		return float64(v), true
	case string:
		if n, err := strconv.ParseFloat(strings.TrimSpace(v), 64); err == nil {
			return n, true
		}
	}
	return 0, false
}

// Number is the strict number read: a finite number, or a string that parses
// as one. "NaN" and "Inf" parse, and are refused: neither is a size a total
// can be summed from, and JSON cannot encode either.
func Number(v any) (float64, bool) {
	switch v := v.(type) {
	case json.Number:
		f, err := v.Float64()
		if err != nil {
			return 0, false
		}
		return Number(f)
	case float64:
		return v, Finite(v)
	case int:
		return float64(v), true
	case int64:
		return float64(v), true
	case string:
		n, err := strconv.ParseFloat(strings.TrimSpace(v), 64)
		return n, err == nil && Finite(n)
	}
	return 0, false
}

// Finite is what a size has to be: a real number, which NaN and the
// infinities are not.
func Finite(v float64) bool { return !math.IsNaN(v) && !math.IsInf(v, 0) }

// Bool is a value as a boolean argument reads it: true or false and nothing
// else. ok is false for any other value, which the read takes as false: a
// flag a tool sets, where "say nothing" and "false" are one instruction.
func Bool(v any) (bool, bool) {
	b, ok := v.(bool)
	return b, ok
}

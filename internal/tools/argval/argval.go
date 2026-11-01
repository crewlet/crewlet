// Package argval is how a first-party tool reads one argument a model sent:
// the one rule for what each kind of value is read as, which the builtin
// tools' helpers and the surface's check of a call against its schema both
// call, so the two cannot read one value two ways (ADR-0059).
//
// ONE READER PER DECLARED TYPE, which decides acceptance and returns the
// value both: a string field takes a number or a boolean rendered as the
// model wrote it, a list of strings takes one string or a comma-separated
// one, an integer and a number take a value or a decimal text that spells
// one exactly ([Whole], [Number]), a boolean is only true or false, and a
// closed set takes its member trimmed and without regard to case ([Choice]).
// A property test holds the check and every tool reader to the same value
// over a corpus of awkward inputs.
package argval

import (
	"encoding/json"
	"fmt"
	"maps"
	"math"
	"math/big"
	"regexp"
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
		// AS JSON WRITES IT, the JavaScript rule a model's JSON intends
		// (encoding/json is that rule): digits from 1e-6 to below 1e21, the
		// exponent form outside them, the shortest spelling that reads back
		// as the same number. So 1000000 is "1000000", where `%g` wrote
		// "1e+06", and 1e300 is "1e+300", never 301 digits. A non-finite
		// number has no JSON spelling and is no value a model wrote, so it
		// is read as no string, which the check names.
		text, err := json.Marshal(v)
		if err != nil {
			return "", false
		}
		return string(text), true
	case json.Number:
		// THE LITERAL, not a rendering of it: this is the spelling the model
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

// StringItems is a value as a list-of-strings argument reads it, element by
// element, for a caller that refuses an element it cannot use by its place:
// a list as it is, or the single string a model sends where a list belongs,
// split on commas, as [Strings] reads it. ok is false for anything else, null
// included.
func StringItems(v any) ([]any, bool) {
	switch v := v.(type) {
	case []any:
		return v, true
	case string:
		parts := strings.Split(v, ",")
		out := make([]any, 0, len(parts))
		for _, p := range parts {
			if p = strings.TrimSpace(p); p != "" {
				out = append(out, p)
			}
		}
		return out, true
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

// Whole is THE integer read, the one rule for an argument declared an
// integer: the check at the surface calls it to decide whether the value is
// one, and the tool calls it to read the value, so the two cannot disagree on
// what a value is (ADR-0059). It reads a finite whole number inside the int
// range, or a text that spells one exactly in decimal ("90", " +5 ", "5.0",
// "1e3"). An integer literal is read exactly, past 2^53 included. A fraction
// is refused rather than truncated to one nobody typed, "2 days" is not 2,
// a spelling outside decimal notation ("0x10", "1_000", "Inf") is refused,
// and a number past the int range (1e300) is refused rather than overflowed
// into some other number.
func Whole(v any) (int, bool) {
	switch v := v.(type) {
	case json.Number:
		return wholeText(v.String())
	case string:
		return wholeText(v)
	case float64:
		// FINITE FIRST, because the fraction test does not cover it: an
		// infinity IS its own truncation, and `int(+Inf)` is not defined
		// by the language.
		if !Finite(v) || v != math.Trunc(v) || !inIntRange(v) {
			return 0, false
		}
		return int(v), true
	case int:
		return v, true
	case int64:
		return int(v), true
	}
	return 0, false
}

// wholeText is the integer a decimal text spells exactly, or none. The value
// is computed exactly rather than through a float, so "9007199254740993.0"
// is that integer and not the float beside it.
func wholeText(s string) (int, bool) {
	text := strings.TrimSpace(s)
	if n, err := strconv.Atoi(text); err == nil {
		return n, true
	}
	r, ok := decimal(text)
	if !ok || !r.IsInt() || !r.Num().IsInt64() {
		return 0, false
	}
	return int(r.Num().Int64()), true
}

// decimalText is the one spelling a number is read from as text: decimal
// notation, a sign, a fraction and an exponent allowed. It is what a model
// means by a number written as a string, and it leaves out what a Go parser
// would also admit and no model means (hex, underscores, "Inf", "NaN").
var decimalText = regexp.MustCompile(`^[+-]?(?:[0-9]+\.?[0-9]*|\.[0-9]+)(?:[eE][+-]?([0-9]+))?$`)

// maxDecimalText and maxDecimalExponent bound the reads of a decimal text.
//
//   - THE EXPONENT, for every read: no finite float64 and no int is spelt
//     with an exponent past 400 in either direction, and "1e-500" is a
//     number no reader here can hold.
//   - THE LENGTH, for the exact read ([Whole], through math/big) and for a
//     number written as a STRING: 64 characters spell any int64 or float64
//     in full, and the bound keeps a text like "1e999999999" from making the
//     exact read build an enormous number. A decoded JSON literal
//     (json.Number) where a number belongs has no length bound: it is read
//     as the float64 nearest it, by a parse whose cost is linear in its
//     length, as the decoder itself would have read it.
const (
	maxDecimalText     = 64
	maxDecimalExponent = 400
)

// decimalSpelt reports a text is a number in decimal notation within the
// exponent bound.
func decimalSpelt(text string) bool {
	m := decimalText.FindStringSubmatch(text)
	if m == nil {
		return false
	}
	if m[1] == "" {
		return true
	}
	exp, err := strconv.Atoi(m[1])
	return err == nil && exp <= maxDecimalExponent
}

// decimal is the exact value of a decimal text, or none.
func decimal(text string) (*big.Rat, bool) {
	if len(text) > maxDecimalText || !decimalSpelt(text) {
		return nil, false
	}
	return new(big.Rat).SetString(text)
}

// inIntRange reports a whole float64 converts to an int without overflow:
// above the smallest int64, and below 2^63, which is the first float past
// the largest.
func inIntRange(v float64) bool {
	return v >= math.MinInt64 && v < -math.MinInt64
}

// Number is THE number read, the one rule for an argument declared a number,
// which the check and the tool both call: a finite number, or a text that
// spells one in decimal, read as the float64 nearest it. "NaN" and "Inf" are
// refused: neither is a size a total can be summed from, and JSON cannot
// encode either.
func Number(v any) (float64, bool) {
	switch v := v.(type) {
	case json.Number:
		return numberText(v.String(), false)
	case string:
		return numberText(v, true)
	case float64:
		return finite(v)
	case int:
		return float64(v), true
	case int64:
		return float64(v), true
	}
	return 0, false
}

// numberText is the float64 nearest the number a decimal text spells, or
// none for a text that spells no finite number. bounded holds the text to
// [maxDecimalText], which a number written as a string is and a decoded JSON
// literal is not.
func numberText(s string, bounded bool) (float64, bool) {
	text := strings.TrimSpace(s)
	if (bounded && len(text) > maxDecimalText) || !decimalSpelt(text) {
		return 0, false
	}
	// [strconv.ParseFloat] rounds to the nearest float64, as math/big
	// would, and refuses an overflow (ErrRange) rather than answering an
	// infinity as a size.
	f, err := strconv.ParseFloat(text, 64)
	if err != nil {
		return 0, false
	}
	return finite(f)
}

// finite is a number read as itself when it is finite, and as none otherwise.
func finite(f float64) (float64, bool) {
	if !Finite(f) {
		return 0, false
	}
	return f, true
}

// Finite is what a size has to be: a real number, which NaN and the
// infinities are not.
func Finite(v float64) bool { return !math.IsNaN(v) && !math.IsInf(v, 0) }

// List is a value as a list-of-objects argument reads it: a list, its
// elements each read by the caller as the object it declares. ok is false for
// anything else, null included, so a caller that requires the list refuses an
// absent one by name, as it does any other.
func List(v any) ([]any, bool) {
	list, ok := v.([]any)
	return list, ok
}

// Object is a value as an object argument reads it: a JSON object, each
// member that is null left out. NULL IS ABSENT, as the check at the surface
// reads it and as every other reader here reads it, so `{"set": null}` asks
// for nothing rather than for an empty set. ok is false for anything else,
// null included, which the read takes as absent.
func Object(v any) (map[string]any, bool) {
	object, ok := v.(map[string]any)
	if !ok {
		return nil, false
	}
	for _, member := range object {
		if member == nil {
			out := maps.Clone(object)
			maps.DeleteFunc(out, func(_ string, m any) bool { return m == nil })
			return out, true
		}
	}
	return object, true
}

// Bool is a value as a boolean argument reads it: true or false and nothing
// else. ok is false for any other value, which the read takes as false: a
// flag a tool sets, where "say nothing" and "false" are one instruction.
func Bool(v any) (bool, bool) {
	b, ok := v.(bool)
	return b, ok
}

// Choice is a string read as one member of a closed set: the member itself,
// or the member it spells once trimmed and read without regard to case ("
// Body" is "body"). ok is false for a string no member is. THE ONE RULE for
// a closed set, which the check of a call reads by and the surface hands a
// tool already applied, so no tool keeps a normalisation of its own that the
// check would not know.
func Choice(s string, set []string) (string, bool) {
	trimmed := strings.TrimSpace(s)
	for _, member := range set {
		if member == trimmed {
			return member, true
		}
	}
	for _, member := range set {
		if strings.EqualFold(member, trimmed) {
			return member, true
		}
	}
	return s, false
}

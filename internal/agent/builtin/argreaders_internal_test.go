package builtin

import (
	"encoding/json"
	"math"
	"math/big"
	"strconv"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/tools"
	"github.com/crewlet/crewlet/internal/tools/toolargs"
)

// argCorpus is every awkward spelling a model has sent, or could, where a
// declared argument is read: exponents, underscores, hex and hex floats, a
// leading sign, whitespace, trailing words, values past every range, decoded
// literals, non-whole and non-finite floats, and each JSON type in the slot
// of every other.
var argCorpus = []any{
	"1e3", "5e2", "1E+3", "1e-3", "1_000", "0x10", "0x1p4", "0b101", "0o17",
	"+5", "-3", " 7 ", "\t12\n", "007", "-0", "5.", ".5", "5.0", "2.5",
	"5 days", "1e3x", "12abc", "", " ", "Inf", "-Inf", "NaN", "infinity",
	"1/2", "１２", "1e400", "9007199254740993", "9007199254740993.0",
	"9223372036854775807", "9223372036854775808", "-9223372036854775809",
	"true", "false", "yes", "body", " Body ", "BODY", "GP-12", "a, b", "a,,b",
	json.Number("1e3"), json.Number("10"), json.Number("2.5"),
	json.Number("9007199254740993"), json.Number("1e300"),
	json.Number("-0"), json.Number("0x10"), json.Number("1_0"),
	3.0, 2.5, -2.5, 1e3, 1e300, -1e300, 9.2e18, math.Ldexp(1, 63),
	math.Inf(1), math.Inf(-1), math.NaN(), math.Copysign(0, -1),
	true, false, 7, int64(7),
	map[string]any{"a": 1.0}, []any{"a"}, []any{12.0}, []any{}, []any{map[string]any{}},
}

// exact is the value a numeric input spells, exactly, or none: the oracle the
// readers are held to, computed by math/big rather than by any reader under
// test.
func exact(v any) (*big.Rat, bool) {
	switch v := v.(type) {
	case float64:
		if math.IsNaN(v) || math.IsInf(v, 0) {
			return nil, false
		}
		return new(big.Rat).SetFloat64(v), true
	case int:
		return new(big.Rat).SetInt64(int64(v)), true
	case int64:
		return new(big.Rat).SetInt64(v), true
	case json.Number:
		return new(big.Rat).SetString(v.String())
	case string:
		return new(big.Rat).SetString(strings.TrimSpace(v))
	}
	return nil, false
}

// misread reports the check refused the value at `x` before the tool ran.
func misread(schema map[string]any, v any) bool {
	args := map[string]any{"x": v}
	for _, f := range tools.CheckArgs("probe", map[string]any{
		"type":       "object",
		"properties": map[string]any{"x": schema},
	}, args) {
		if f.Finding == tools.Misread {
			return true
		}
	}
	return false
}

// WHAT THE CHECK ADMITS IS WHAT THE TOOL READS, for every declared type over
// the corpus (ADR-0059, #423 re-verify): whenever the check at the surface
// lets a value through to the tool, each of the tool's readers for that type
// returns exactly the value the input spells. Where a reader would read a
// value as anything else (a truncation, a prefix, a default), the check must
// refuse it. "1e3" passed the check as an integer while argInt read it as 1;
// a set of "GP-12" passed as a list while the tool read it as none.
//
// Mutation: give argInt a reader of its own (fmt.Sscanf's prefix) and "1e3"
// is named here; let the check admit "0x10" and its value is named.
func TestWhatTheCheckAdmitsIsWhatTheToolReads(t *testing.T) {
	t.Parallel()
	const fallback = math.MinInt32 // a value no input in the corpus spells
	for _, v := range argCorpus {
		args := map[string]any{"x": v}
		name := strconv.Quote(strings.TrimSpace(func() string {
			b, _ := json.Marshal(v)
			return string(b) + " " + typeName(v)
		}()))

		if !misread(map[string]any{"type": "integer"}, v) {
			want, ok := exact(v)
			if !ok || !want.IsInt() || !want.Num().IsInt64() {
				t.Errorf("integer %s: the check admitted a value that spells no integer", name)
			} else {
				n := int(want.Num().Int64())
				if got := argInt(args, "x", fallback); got != n {
					t.Errorf("integer %s: the check admitted %d and argInt read %d", name, n, got)
				}
				if got, ok := argIntValue(v); !ok || got != n {
					t.Errorf("integer %s: the check admitted %d and argIntValue read %d, %v", name, n, got, ok)
				}
			}
		}

		if !misread(map[string]any{"type": "number"}, v) {
			want, ok := exact(v)
			if !ok {
				t.Errorf("number %s: the check admitted a value that spells no number", name)
			} else {
				f, _ := want.Float64()
				if got := argFloat(args, "x"); got != f {
					t.Errorf("number %s: the check admitted %v and argFloat read %v", name, f, got)
				}
				if got, ok := argFloatValue(v); !ok || got != f {
					t.Errorf("number %s: the check admitted %v and argFloatValue read %v, %v", name, f, got, ok)
				}
			}
		}

		if !misread(map[string]any{"type": "boolean"}, v) {
			want, ok := v.(bool)
			if got := argBool(args, "x"); !ok || got != want {
				t.Errorf("boolean %s: the check admitted it and argBool read %v", name, got)
			}
		}

		if !misread(map[string]any{"type": "string"}, v) {
			if got, want := argString(args, "x"), spelt(v); got != want {
				t.Errorf("string %s: the check admitted %q and argString read %q", name, want, got)
			}
		}

		if !misread(map[string]any{"type": "array", "items": map[string]any{"type": "string"}}, v) {
			if wrote := listSpelt(v); len(wrote) > 0 && len(argStrings(args, "x")) == 0 {
				t.Errorf("list %s: the check admitted %q and argStrings read no list", name, wrote)
			}
		}

		set := []any{"body", "comments"}
		enum := map[string]any{"type": "string", "enum": set}
		if !misread(enum, v) && v != nil {
			read := argString(toolargs.Of(map[string]any{
				"type": "object", "properties": map[string]any{"x": enum},
			}, args).Map(), "x")
			outside := false
			for _, f := range tools.CheckArgs("probe", map[string]any{
				"type": "object", "properties": map[string]any{"x": enum},
			}, args) {
				outside = outside || f.Finding == tools.Outside
			}
			if !outside && read != "body" && read != "comments" {
				t.Errorf("enum %s: the check admitted it as a member and the tool read %q", name, read)
			}
		}
	}
}

// spelt is a value as the model wrote it, where a string belongs.
func spelt(v any) string {
	switch v := v.(type) {
	case string:
		return v
	case json.Number:
		return v.String()
	case bool:
		return strconv.FormatBool(v)
	case float64:
		// AS JSON WRITES IT (argval.String's rule), which reads back as v.
		text, _ := json.Marshal(v)
		return string(text)
	}
	return ""
}

// listSpelt is each non-empty string a list-of-strings value carries.
func listSpelt(v any) []string {
	var items []any
	switch v := v.(type) {
	case []any:
		items = v
	case string:
		for _, p := range strings.Split(v, ",") {
			items = append(items, p)
		}
	}
	var out []string
	for _, item := range items {
		if s := strings.TrimSpace(spelt(item)); s != "" {
			out = append(out, s)
		}
	}
	return out
}

func typeName(v any) string {
	switch v.(type) {
	case json.Number:
		return "(json.Number)"
	case float64:
		return "(float64)"
	case string:
		return "(string)"
	}
	return ""
}

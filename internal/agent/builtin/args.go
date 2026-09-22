package builtin

import (
	"encoding/json"
	"fmt"
	"maps"
	"math"
	"slices"
	"strconv"
	"strings"

	"github.com/crewlet/crewlet/internal/tools"
)

// The shared argument and output helpers.
//
// Arguments arrive from a MODEL, so every read here treats a missing, wrongly
// typed or absurdly long value as an ordinary case rather than an error: the
// tool's job is to tell the model what it should have sent, in a message the
// model can act on.

// argString reads a string argument, tolerating a number or bool the model
// sent where a string belongs — which they do, and refusing it teaches
// nothing.
func argString(args map[string]any, key string) string {
	switch v := args[key].(type) {
	case string:
		return v
	case nil:
		return ""
	case float64:
		return fmt.Sprintf("%g", v)
	case json.Number:
		// THE LITERAL, not `%g` of it: this is the spelling the model
		// sent, and it is exact where the float rendering is not.
		return v.String()
	case bool:
		return fmt.Sprintf("%t", v)
	}
	return ""
}

// argInt reads an integer argument, or the fallback.
//
// JSON has one number type, so an int arrives as a number through every
// decoder in this path; the int cases are for the callers that hand a map
// straight in.
func argInt(args map[string]any, key string, fallback int) int {
	switch v := args[key].(type) {
	case float64:
		return int(v)
	case json.Number:
		// INT64 FIRST, because that is the whole reason the decoders on
		// this path read through json.Number: an id past 2^53 is exact
		// here and rounded the moment it becomes a float.
		if i, err := v.Int64(); err == nil {
			return int(i)
		}
		if f, err := v.Float64(); err == nil {
			return int(f)
		}
	case int:
		return v
	case int64:
		return int(v)
	case string:
		var n int
		if _, err := fmt.Sscanf(v, "%d", &n); err == nil {
			return n
		}
	}
	return fallback
}

// argStrings reads a list-of-strings argument, tolerating the single string a
// model sends where a list belongs — which they do, constantly, and refusing
// it costs a round to teach nothing.
func argStrings(args map[string]any, key string) []string {
	switch v := args[key].(type) {
	case []string:
		return cleanStrings(v)
	case []any:
		out := make([]string, 0, len(v))
		for _, item := range v {
			out = append(out, argString(map[string]any{"v": item}, "v"))
		}
		return cleanStrings(out)
	case string:
		// A COMMA-SEPARATED STRING IS ALSO ACCEPTED, for the same reason:
		// "backend, api" is what a model writes when it has decided a
		// list field takes prose, and splitting it is right far more
		// often than treating it as one label containing a comma.
		return cleanStrings(strings.Split(v, ","))
	}
	return nil
}

// cleanStrings trims, drops empties and deduplicates, preserving order.
func cleanStrings(in []string) []string {
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

// argBool reads a boolean argument, false when absent or not one.
//
// ABSENT AND FALSE ARE THE SAME ANSWER here, unlike in the query grammar,
// because every caller of this one is a flag a tool sets rather than a filter
// with three states: "do not protect this view" and "say nothing about
// protecting it" are the same instruction.
func argBool(args map[string]any, key string) bool {
	value, _ := args[key].(bool)
	return value
}

// argFloat reads a numeric argument, zero when absent or not one.
//
// ZERO RATHER THAN A REFUSAL, because every caller of this one has a field
// whose zero IS its default — a target that starts at zero, a current nobody
// has moved. A field where zero were a distinct setting would take a pointer,
// which is the rule the config models follow for the same reason.
func argFloat(args map[string]any, key string) float64 {
	switch v := args[key].(type) {
	case float64:
		return v
	case json.Number:
		if f, err := v.Float64(); err == nil {
			return f
		}
	case int:
		return float64(v)
	case string:
		if n, err := strconv.ParseFloat(strings.TrimSpace(v), 64); err == nil {
			return n
		}
	}
	return 0
}

// argStringMap reads an object argument whose values are strings.
//
// A NON-STRING VALUE IS RENDERED rather than dropped, because a model writing
// `{"limit": 10}` means the same thing as `{"limit": "10"}` and the grammar
// these maps feed reads both identically — see the API's own parameter bag.
// Dropping it
// would save a saved view with a filter the caller believes is in it.
func argStringMap(args map[string]any, key string) map[string]string {
	raw, ok := args[key].(map[string]any)
	if !ok || len(raw) == 0 {
		return nil
	}
	out := make(map[string]string, len(raw))
	for k, v := range raw {
		if k = strings.TrimSpace(k); k == "" || v == nil {
			continue
		}
		switch typed := v.(type) {
		case string:
			out[k] = typed
		case bool:
			out[k] = strconv.FormatBool(typed)
		case float64:
			out[k] = strconv.FormatFloat(typed, 'f', -1, 64)
		default:
			out[k] = fmt.Sprint(v)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// failed is a tool result the model can act on.
//
// Failed rather than an error: the turn is fine, this call is not, and the
// difference is what lets the model try again with a better argument instead
// of the loop tearing down.
func failed(msg string) tools.Result { return tools.Result{Output: msg, Failed: true} }

// clip flattens a caller-supplied string echoed back into a tool result or a
// log line.
//
// NEWLINES ONLY, no length cut. A smuggled newline genuinely breaks a
// line-structured render, so folding whitespace earns its place. Cutting the
// string did not: what is echoed here is the model's OWN argument, quoted back
// so it can see what failed to match, and a shortened echo names a query the
// model never sent — which is worse than a long line, because the model then
// retries against the wrong string.
func clip(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

func sortStrings(s []string) { slices.Sort(s) }

func sortedKeys(m map[string]string) []string {
	out := slices.Sorted(maps.Keys(m))
	return out
}

// argIntValue and argFloatValue read a number, reporting whether they COULD.
//
// # Why not [argInt] and [argFloat]
//
// Because both answer a fallback for a value they cannot read, and every
// caller of these two holds a value whose zero is a SETTING rather than an
// absence: zero minutes and zero points both mean UNESTIMATED, a `min` of 0 is
// a real floor, and a `precision` of 0 declares a field exact to whole
// numbers. So an unreadable value became `&0` and the write succeeded —
// `estimate_minutes: "two hours"` answered `applied` and wiped the estimate,
// which is the exact failure the schedule reader's own header says it exists
// to prevent, in the half of it that was not written to the rule. [argFloat]'s
// doc states the condition under which its zero is right — "every caller of
// this one has a field whose zero IS its default" — and these are the callers
// that broke it.
//
// The parse is the WHOLE string rather than [fmt.Sscanf]'s prefix, which is
// the other half of the same bug: `Sscanf("%d")` reads "2 days" as 2, so a
// two-day estimate was stored as two MINUTES and nothing was refused.
//
// THEY LIVE HERE rather than beside the schedule, because the custom-field
// declaration reads its `precision` and its `min`/`max` bounds by the same
// rule and a second spelling of the json.Number / finiteness discipline is how
// one of them stops matching the other — the objection [textcut] and [whsec]
// each record for a grammar that was written twice.
func argIntValue(raw any) (int, bool) {
	switch v := raw.(type) {
	case json.Number:
		// BACK THROUGH THIS FUNCTION rather than repeating the whole-number
		// and finiteness discipline below, which is the half a second
		// spelling always gets wrong. An integer past 2^53 is none of the
		// things this reads — minutes, story points, decimal places — so
		// the float is the honest intermediate here, unlike in [argInt].
		f, err := v.Float64()
		if err != nil {
			return 0, false
		}
		return argIntValue(f)
	case float64:
		// JSON has one number type, so a whole number arrives here. A
		// fraction is not a whole number of minutes, nor of decimal
		// places, and is refused rather than truncated to one nobody
		// typed.
		//
		// FINITE FIRST, because the fraction test does not cover it: an
		// infinity IS its own truncation, so it passed, and `int(+Inf)`
		// is not defined by the language — it lands on the platform's
		// minimum int, which the negative check below then refuses as a
		// NEGATIVE estimate. Right answer, wrong reason, and a message
		// naming a sign nobody typed.
		if math.IsNaN(v) || math.IsInf(v, 0) || v != math.Trunc(v) {
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

func argFloatValue(raw any) (float64, bool) {
	switch v := raw.(type) {
	case json.Number:
		f, err := v.Float64()
		if err != nil {
			return 0, false
		}
		return argFloatValue(f)
	case float64:
		return v, finite(v)
	case int:
		return float64(v), true
	case int64:
		return float64(v), true
	case string:
		// [strconv.ParseFloat] ACCEPTS "NaN", "Inf" and "infinity" in
		// every casing, which is why the check is here rather than left
		// to the caller: a size of NaN passed the `points < 0` guard
		// below — every comparison with NaN is false — and an infinity
		// passed it honestly, so both reached the writer. Downstream
		// neither is a number a total can be summed from, and JSON
		// cannot even encode them, so the failure surfaced as a broken
		// answer somewhere with no memory of who typed it.
		n, err := strconv.ParseFloat(strings.TrimSpace(v), 64)
		return n, err == nil && finite(n)
	}
	return 0, false
}

// finite is what a size has to be: a real number a total can be summed from,
// which NaN and the infinities are not.
func finite(v float64) bool { return !math.IsNaN(v) && !math.IsInf(v, 0) }

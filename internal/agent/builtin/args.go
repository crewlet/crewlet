package builtin

import (
	"fmt"
	"maps"
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
	case bool:
		return fmt.Sprintf("%t", v)
	}
	return ""
}

// argInt reads an integer argument, or the fallback.
//
// JSON has one number type, so an int arrives as float64 through every decoder
// in this path; the int cases are for the callers that hand a map straight in.
func argInt(args map[string]any, key string, fallback int) int {
	switch v := args[key].(type) {
	case float64:
		return int(v)
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

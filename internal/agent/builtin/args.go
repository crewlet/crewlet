package builtin

import (
	"fmt"
	"maps"
	"slices"
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

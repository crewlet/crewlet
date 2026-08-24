package builtin

import (
	"fmt"
	"slices"
	"strings"
	"unicode/utf8"

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

// clip bounds a caller-supplied string echoed into output or a log line, and
// flattens newlines: a model's argument reaches both, and a smuggled newline
// breaks a line-structured render while a pathological length bloats an audit
// payload.
func clip(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if utf8.RuneCountInString(s) <= displayMax {
		return s
	}
	r := []rune(s)
	return string(r[:displayMax]) + "…"
}

func sortStrings(s []string) { slices.Sort(s) }

func sortedKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}

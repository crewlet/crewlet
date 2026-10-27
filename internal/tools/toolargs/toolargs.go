// Package toolargs holds a tool call's arguments in the one form every step
// of the call reads them in, once the call has entered (ADR-0059).
//
// THE CANONICAL FORM IS MADE ONCE, AT THE ENTRY. A closed set's member is
// handed on as its set spells it (" Draft" is "draft"), and every step after
// that reads only this form: the admission into a run's call log and the
// repeat count an operation id carries, the skill guard, the refusal-loop
// target, the check, the tool, the ledger, logging and metrics. When the form
// was made in the middle of that pipeline, a retry spelt "Draft" after
// "draft" was counted as a different call before the tool and the same one
// inside it, so it derived a fresh operation id and wrote twice.
//
// [Args] is a distinct type, made only by [Of], so a step that requires it
// cannot be handed the arguments as the model sent them by mistake.
package toolargs

import (
	"maps"
	"slices"

	"github.com/crewlet/crewlet/internal/tools/argval"
)

// Args are one call's arguments in their canonical form. The zero value is a
// call with no arguments.
type Args struct {
	m map[string]any
}

// Of is a call's arguments as the entry hands them on: every string at a
// place the schema closes to a set ([argval.Choice]) replaced by the member it
// spells, and nothing else changed. A nil schema (a third-party tool, which
// holds its own schema, or a name nothing registered) hands them on as sent.
// raw itself is never changed; a copy is made where anything changes.
func Of(schema, raw map[string]any) Args {
	if raw == nil {
		return Args{m: map[string]any{}}
	}
	if out, changed := canonical(schema, raw); changed {
		if m, ok := out.(map[string]any); ok {
			return Args{m: m}
		}
	}
	return Args{m: raw}
}

// Map is the arguments as the tool reads them. It is the canonical form; a
// caller does not change it.
func (a Args) Map() map[string]any {
	if a.m == nil {
		return map[string]any{}
	}
	return a.m
}

// canonical is v with every closed-set string as its member, and whether
// anything changed (v is then left untouched, and a copy answered).
func canonical(schema map[string]any, v any) (any, bool) {
	if schema == nil || v == nil {
		return v, false
	}
	switch v := v.(type) {
	case string:
		if set := StringsOf(schema["enum"]); len(set) > 0 {
			if member, ok := argval.Choice(v, set); ok && member != v {
				return member, true
			}
			return v, false
		}
		items, _ := schema["items"].(map[string]any)
		if items == nil || !slices.Contains(TypesOf(schema), "array") {
			return v, false
		}
		// ONE STRING STANDING FOR A LIST is canonical element by element,
		// as the tool reads it ([argval.StringItems]).
		parts, _ := argval.StringItems(v)
		if out, changed := canonical(schema, parts); changed {
			return out, true
		}
		return v, false
	case []any:
		items, _ := schema["items"].(map[string]any)
		var out []any
		for i, e := range v {
			if c, changed := canonical(items, e); changed {
				if out == nil {
					out = slices.Clone(v)
				}
				out[i] = c
			}
		}
		if out == nil {
			return v, false
		}
		return out, true
	case map[string]any:
		props, _ := schema["properties"].(map[string]any)
		extra, _ := schema["additionalProperties"].(map[string]any)
		var out map[string]any
		for key, e := range v {
			sub, ok := props[key].(map[string]any)
			if !ok {
				sub = extra
			}
			if c, changed := canonical(sub, e); changed {
				if out == nil {
					out = maps.Clone(v)
				}
				out[key] = c
			}
		}
		if out == nil {
			return v, false
		}
		return out, true
	}
	return v, false
}

// TypesOf is a schema's `type`, one or a list.
func TypesOf(schema map[string]any) []string {
	switch t := schema["type"].(type) {
	case string:
		return []string{t}
	case []any:
		return StringsOf(t)
	case []string:
		return t
	}
	return nil
}

// StringsOf is a schema list of strings (an `enum`, a `required`), its
// non-string members left out.
func StringsOf(v any) []string {
	switch v := v.(type) {
	case []string:
		return v
	case []any:
		out := make([]string, 0, len(v))
		for _, e := range v {
			if s, ok := e.(string); ok {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}

// Package toolargs reads a tool call's arguments off the transport that
// carried them, the same way over every one ([Decode]), and holds them in the
// one form every step of the call reads them in, once the call has entered
// (ADR-0059).
//
// ONE DECODE FOR EVERY TRANSPORT INTO A TOOL: a person's request on the HTTP
// write surface or the act transport, an operator's assistant over MCP, and a
// coding CLI's call through its tool session. It is here, in a leaf beneath
// internal/tools rather than in that package, because the tool session a
// CLI's calls arrive through sits below internal/tools: the tools import the
// tool loop, which imports the session, so a decode kept beside the tools
// could not serve the session, which kept a copy of its own. This package
// imports no transport, nor anything that imports one, which is what lets
// every one of them import it.
//
// THE CANONICAL FORM IS MADE ONCE, AT THE ENTRY. A closed set's member is
// handed on as its set spells it (" Draft" is "draft"), an argument sent as
// null is left out unless its schema declares null, and every step after that
// reads only this form: the admission into a run's call log and the repeat
// count an operation id carries, the skill guard, the refusal-loop target, the
// check, the tool, the ledger, logging and metrics. When the form was made in
// the middle of that pipeline, a retry spelt "Draft" after "draft" was counted
// as a different call before the tool and the same one inside it, so it
// derived a fresh operation id and wrote twice.
//
// NULL IS ABSENT (ADR-0059), and this is where a tool stops being handed one.
// The check at the surface reads a null as a missing value, but most tools
// name what a call changes by the arguments it SENDS, testing each for
// presence, and read that way a null was a value: a null `body` emptied a
// page, a null `assignee` unassigned a work item and a null `archived` took a
// project out of the archive, each answered as the change the caller had not
// asked for. Left out here, a null reads as the argument nobody sent to every
// tool, one added later included. An argument whose schema declares null
// keeps it, because null is then its value (an update's `due` is cleared by
// one). A nested null is left as sent: a custom field's value in `fields` is
// cleared by one, and an object argument's reader drops its own
// ([argval.Object]). A tool with no schema here, a third-party MCP server's,
// is handed every argument as sent: it holds its own schema and reads a null
// by it.
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
// spells, every argument sent as null left out unless its schema declares
// null (the package doc says why), and nothing else changed. A nil schema (a
// third-party tool, which holds its own schema, or a name nothing registered)
// hands them on as sent. raw itself is never changed; a copy is made where
// anything changes.
func Of(schema, raw map[string]any) Args {
	if raw == nil {
		return Args{m: map[string]any{}}
	}
	m := raw
	if out, changed := canonical(schema, raw); changed {
		if c, ok := out.(map[string]any); ok {
			m = c
		}
	}
	return Args{m: withoutNulls(schema, m)}
}

// withoutNulls is m without each member sent as null whose schema does not
// declare null, copied where one is left out (m itself is never changed).
//
// THE TOP LEVEL ONLY, and a nil schema drops nothing: see the package doc.
func withoutNulls(schema, m map[string]any) map[string]any {
	if schema == nil {
		return m
	}
	absent := func(key string, v any) bool {
		return v == nil && !slices.Contains(TypesOf(memberSchema(schema, key)), "null")
	}
	for key, v := range m {
		if absent(key, v) {
			out := maps.Clone(m)
			maps.DeleteFunc(out, absent)
			return out
		}
	}
	return m
}

// memberSchema is the schema an object's member is held to: its declared
// property's, else the object's `additionalProperties` where that is a
// schema, else nil.
func memberSchema(schema map[string]any, key string) map[string]any {
	props, _ := schema["properties"].(map[string]any)
	if sub, ok := props[key].(map[string]any); ok {
		return sub
	}
	extra, _ := schema["additionalProperties"].(map[string]any)
	return extra
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
		var out map[string]any
		for key, e := range v {
			if c, changed := canonical(memberSchema(schema, key), e); changed {
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

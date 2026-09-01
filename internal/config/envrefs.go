package config

import (
	"reflect"
	"slices"

	"github.com/crewlet/crewlet/internal/envref"
)

// The ${VAR} references a config payload actually holds.
//
// Answering "which variables does this config need" is a question two very
// different callers ask: an operator wants to know what to set before a boot
// fails on a missing one, and the example suite wants to know that every
// reference a shipped config makes is one the docs explain.
//
// It is a REFLECTION WALK rather than a re-parse of the YAML, because the
// question is about the loaded config — the thing the engine will actually
// resolve — and a document walk would miss defaults the loader filled in and
// include keys it discarded.

// ReferencedNames is every ${VAR} a payload mentions anywhere, sorted and
// de-duplicated.
//
// It walks the value rather than a serialised form so it works on a decoded
// [Company], on the map a store row decodes to, and on a raw yaml.Node
// alike — the three shapes a payload actually arrives in.
func ReferencedNames(payload any) []string {
	seen := map[string]struct{}{}
	walkStrings(reflect.ValueOf(payload), func(s string) {
		for _, name := range envref.Names(s) {
			seen[name] = struct{}{}
		}
	})
	out := make([]string, 0, len(seen))
	for name := range seen {
		out = append(out, name)
	}
	slices.Sort(out)
	return out
}

// walkStrings visits every string reachable from v.
//
// Unexported fields are skipped: reflection cannot read them, and nothing
// in a config payload hides a reference behind one — the one type with
// unexported state (org.Toggle) holds no strings at all.
func walkStrings(v reflect.Value, visit func(string)) {
	if !v.IsValid() {
		return
	}
	switch v.Kind() {
	case reflect.String:
		visit(v.String())
	case reflect.Pointer, reflect.Interface:
		if !v.IsNil() {
			walkStrings(v.Elem(), visit)
		}
	case reflect.Slice, reflect.Array:
		for i := range v.Len() {
			walkStrings(v.Index(i), visit)
		}
	case reflect.Map:
		iter := v.MapRange()
		for iter.Next() {
			// Keys too: an mcp_env server name or a skill variable is a
			// key, and while neither should carry a reference, missing one
			// that did would make the fingerprint blind to it.
			walkStrings(iter.Key(), visit)
			walkStrings(iter.Value(), visit)
		}
	case reflect.Struct:
		// A yaml.Node's own Value carries the scalar text; its Content
		// carries the children. Walking the struct generically reaches
		// both, so there is no special case to keep in sync.
		t := v.Type()
		for i := range v.NumField() {
			if t.Field(i).PkgPath != "" {
				continue // unexported
			}
			walkStrings(v.Field(i), visit)
		}
	}
}

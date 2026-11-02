package caveat

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"sync"
)

// # A peer's words, rebuilt at one boundary (ADR-0060)
//
// A caveat or a refusal phrasing is constructed by this package's catalogs
// and nowhere else, so the walks over those catalogs read every sentence a
// person can be shown. One more source is legitimate: a PEER's catalog. A
// write routed to a data node answers its caveats, and a refusal its phrasing,
// to the node the caller is on, as JSON. Those words are rebuilt here, by the
// two functions below, and the estate router is their only caller (a gate in
// this package's suite holds that). Neither type implements
// json.Unmarshaler, so JSON decoded anywhere else (a tool's output, a request
// body) leaves a zero value, which no surface answers.

// RebuildFromPeer fills every caveat in out, at any depth, from raw, the JSON
// a peer answered and out was just decoded from. A caveat missing its code or
// either phrasing is left zero. out is a pointer to the decoded value.
//
// FOR THE ESTATE ROUTER ALONE: its result types carry caveats as ordinary
// fields (a WriteResult's, a nested one's), and the router decodes every one
// of them generically, so this walks the decoded value beside the raw JSON by
// the rules encoding/json named the fields with.
func RebuildFromPeer(raw []byte, out any) error {
	v := reflect.ValueOf(out)
	if v.Kind() != reflect.Pointer || v.IsNil() {
		return fmt.Errorf("caveat: rebuild into %T, which is not a pointer", out)
	}
	if !carries(v.Elem().Type()) || len(raw) == 0 {
		return nil
	}
	var tree any
	if err := json.Unmarshal(raw, &tree); err != nil {
		return fmt.Errorf("caveat: a peer's answer: %w", err)
	}
	rebuild(v.Elem(), tree)
	return nil
}

// SaidFromPeer is a refusal's phrasing a peer wrote, as the error chain
// carries it, or nil for none: raw is the JSON the peer's
// [Refusal.MarshalJSON] wrote, and an error is JSON that is not that. A
// remedy this build does not know is dropped and the words kept, since the
// words alone are still the person's. FOR THE ESTATE ROUTER ALONE, for
// [RebuildFromPeer]'s reason.
func SaidFromPeer(raw []byte) (*Said, error) {
	var peer peerRefusal
	if err := json.Unmarshal(raw, &peer); err != nil {
		return nil, fmt.Errorf("caveat: a peer's refusal phrasing: %w", err)
	}
	person := strings.TrimSpace(peer.Person)
	if person == "" {
		return nil, nil
	}
	words := Refusal{person: person}
	if peer.Remedy != nil && peer.Remedy.valid() {
		words.remedy = *peer.Remedy
	}
	return &Said{Words: words}, nil
}

var caveatType = reflect.TypeFor[Caveat]()

// carrying caches whether a type holds a caveat at any depth.
var carrying sync.Map

// carries reports whether a value of t can hold a caveat, so a result type
// that cannot is never walked.
func carries(t reflect.Type) bool {
	if known, ok := carrying.Load(t); ok {
		return known.(bool)
	}
	// A recursive type is answered false while it is being asked, which
	// only its own fields can observe.
	carrying.Store(t, false)
	held := false
	switch t.Kind() {
	case reflect.Struct:
		if t == caveatType {
			held = true
			break
		}
		for i := range t.NumField() {
			if f := t.Field(i); (f.IsExported() || f.Anonymous) && carries(f.Type) {
				held = true
				break
			}
		}
	case reflect.Pointer, reflect.Slice, reflect.Array:
		held = carries(t.Elem())
	}
	carrying.Store(t, held)
	return held
}

// rebuild fills v's caveats from tree, the same value as JSON decodes it.
func rebuild(v reflect.Value, tree any) {
	if !carries(v.Type()) {
		return
	}
	switch v.Kind() {
	case reflect.Pointer:
		if !v.IsNil() {
			rebuild(v.Elem(), tree)
		}
	case reflect.Slice, reflect.Array:
		items, _ := tree.([]any)
		for i := 0; i < v.Len() && i < len(items); i++ {
			rebuild(v.Index(i), items[i])
		}
	case reflect.Struct:
		if v.Type() == caveatType {
			if c, ok := fromPeer(tree); ok && v.CanSet() {
				v.Set(reflect.ValueOf(c))
			}
			return
		}
		fields, _ := tree.(map[string]any)
		if fields == nil {
			return
		}
		t := v.Type()
		for i := range t.NumField() {
			f := t.Field(i)
			name, keep := jsonName(f)
			switch {
			case !keep:
			case f.Anonymous && name == "" && f.Type.Kind() == reflect.Struct:
				// AN EMBEDDED STRUCT'S FIELDS ARE THE OUTER OBJECT'S,
				// as encoding/json writes them.
				rebuild(v.Field(i), tree)
			case f.IsExported():
				if sub, ok := fields[name]; ok {
					rebuild(v.Field(i), sub)
				}
			}
		}
	}
}

// jsonName is the key encoding/json writes field under, "" for an embedded
// struct it flattens, and false for a field it never writes.
func jsonName(f reflect.StructField) (string, bool) {
	tag := f.Tag.Get("json")
	if tag == "-" {
		return "", false
	}
	name, _, _ := strings.Cut(tag, ",")
	if name != "" {
		return name, true
	}
	if f.Anonymous {
		return "", true
	}
	return f.Name, f.IsExported()
}

// fromPeer is one caveat as a peer's [Caveat.MarshalJSON] wrote it, or false
// for one missing its code or either phrasing. A code this build does not
// know is kept as written, words and all: a newer peer's caveat is still
// true, and dropping it would tell the caller less than the write said.
func fromPeer(tree any) (Caveat, bool) {
	fields, _ := tree.(map[string]any)
	code, _ := fields["code"].(string)
	agent, _ := fields["agent"].(string)
	person, _ := fields["person"].(string)
	if code == "" || agent == "" || person == "" {
		return Caveat{}, false
	}
	return Caveat{code: Code(code), agent: agent, person: person}, true
}

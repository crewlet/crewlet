// Package jsoncarry is how a record round-trips through a build that only half
// understands it.
//
// # The rule, and the one clause that makes it safe
//
// A rolling upgrade puts a record a newer build wrote on the wire, and every
// state-log domain has to read it, hold it and sometimes REPUBLISH it — the
// reanchor path does exactly that. So a decode keeps the fields the struct has
// no home for, and the next encode folds them back in.
//
// A CARRIED FIELD LOSES TO A KNOWN ONE. That is the whole of it, and it is
// what a second implementation gets wrong: with the precedence the other way
// round, a stale carried copy silently undoes the write that set the field
// this build does understand, on exactly the path — an upgrade in progress —
// where nobody is looking.
//
// # Why it is a package
//
// It was written twice, in internal/pages and internal/chart, and the copies
// had ALREADY DRIFTED: one decoder carried a fourth parameter the other did
// not have and neither read, which is the shape internal/textcut,
// internal/whsec and internal/jsprovision each record as the moment a rule
// written down in two places stopped being one rule. A fifth state-log domain
// would have made three copies of a precedence rule nothing compares.
//
// It is a LEAF over the standard library alone: every state-log domain is above
// it and nothing here may reach back into one.
package jsoncarry

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
)

// Fields is the set of JSON names a record type defines.
//
// A NAMED TYPE rather than a bare map, because it is the one argument whose
// meaning a caller can invert without the compiler noticing: pass the names a
// type does NOT define and every field is carried AND decoded, so the next
// encode writes a stale copy of every value the caller just set.
type Fields map[string]bool

// Encode marshals a record and folds the carried fields back in.
func Encode(record any, extra map[string]json.RawMessage) ([]byte, error) {
	data, err := json.Marshal(record)
	if err != nil {
		return nil, fmt.Errorf("jsoncarry: encode %T: %w", record, err)
	}
	if len(extra) == 0 {
		return data, nil
	}
	var merged map[string]json.RawMessage
	//nolint:govet // shadow: scoped to this block; see .golangci.yml
	if err := json.Unmarshal(data, &merged); err != nil {
		return nil, fmt.Errorf("jsoncarry: encode %T: %w", record, err)
	}
	for name, value := range extra {
		// A CARRIED FIELD LOSES TO A KNOWN ONE. See the package doc: with
		// this test the other way round, an upgrade in progress would
		// silently revert every field this build does understand.
		if _, known := merged[name]; !known {
			merged[name] = value
		}
	}
	out, err := json.Marshal(merged)
	if err != nil {
		return nil, fmt.Errorf("jsoncarry: encode %T: %w", record, err)
	}
	return out, nil
}

// Decode unmarshals into out and returns the fields out has no home for.
func Decode(data []byte, out any, known Fields) (map[string]json.RawMessage, error) {
	if err := json.Unmarshal(data, out); err != nil {
		return nil, err
	}
	var all map[string]json.RawMessage
	if err := json.Unmarshal(data, &all); err != nil {
		return nil, err
	}
	var extra map[string]json.RawMessage
	for name, value := range all {
		if known[name] {
			continue
		}
		if extra == nil {
			extra = map[string]json.RawMessage{}
		}
		extra[name] = value
	}
	return extra, nil
}

// Names is the JSON names a record type defines: every field encoding/json
// would marshal, read off the TYPE rather than off a marshalled zero value.
//
// # Why the type, and never a marshalled value
//
// It used to marshal a zero value and ask each caller to LIST the omitempty
// names beside it, because a zero value does not marshal those. That was the
// trap the package doc warns about, left for every domain to fall into by
// hand: a name missing from the list is decoded into the struct AND carried as
// unknown, so the next encode writes the stale carried copy back over what the
// caller set — and it had been fallen into four times, silently (a person's
// and an invitation's `colleague`, a unit's `origin_key`, a seat's
// `origin_handle`), each one a field that could never be cleared back to its
// zero value. The tags already say every name; reading them is the only list
// nobody has to remember to update.
//
// THE RULES ARE encoding/json's OWN: an unexported field is skipped, a field
// tagged `-` is skipped, an embedded struct with no name of its own has its
// fields promoted, and an untagged field is named by its Go name.
//
// IT PANICS rather than returning an error, because the only caller shape is a
// package-level var over a type literal: a record type that is not a struct
// is a build that cannot encode any record at all, and discovering that at the
// first publish rather than at init is strictly worse.
func Names(v any) Fields {
	t := reflect.TypeOf(v)
	for t != nil && t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if t == nil || t.Kind() != reflect.Struct {
		panic(fmt.Sprintf("jsoncarry: %T is not a struct, so it does not "+
			"marshal to a JSON object a field could be carried through", v))
	}
	out := Fields{}
	collect(t, out)
	return out
}

// collect adds the JSON names one struct type defines, promoting an embedded
// struct's the way encoding/json does.
func collect(t reflect.Type, out Fields) {
	for i := range t.NumField() {
		field := t.Field(i)
		tag := field.Tag.Get("json")
		if tag == "-" {
			continue
		}
		name, _, _ := strings.Cut(tag, ",")
		if field.Anonymous && name == "" {
			embedded := field.Type
			if embedded.Kind() == reflect.Pointer {
				embedded = embedded.Elem()
			}
			if embedded.Kind() == reflect.Struct {
				collect(embedded, out)
				continue
			}
		}
		if !field.IsExported() {
			continue
		}
		if name == "" {
			name = field.Name
		}
		out[name] = true
	}
}

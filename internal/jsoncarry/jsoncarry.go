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
// It is a LEAF and imports only encoding/json: every state-log domain is above
// it and nothing here may reach back into one.
package jsoncarry

import (
	"encoding/json"
	"fmt"
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

// Names is the JSON names a record type defines: the ones a zero value
// marshals, plus the omitempty names given explicitly.
//
// THE OMITEMPTY NAMES HAVE TO BE LISTED, and that is the trap this function
// cannot close: a zero value does not marshal them, so a name missing from the
// list is decoded into the struct AND carried as unknown — and the next encode
// writes the stale carried copy back over what the caller set. Each domain's
// own declaration is where the list lives, beside the struct it describes.
//
// IT PANICS rather than returning an error, because the only caller shape is a
// package-level var over a type literal: a record type that does not marshal
// to a JSON object is a build that cannot encode any record at all, and
// discovering that at the first publish rather than at init is strictly worse.
func Names(v any, omitted ...string) Fields {
	data, err := json.Marshal(v)
	if err != nil {
		panic(fmt.Sprintf("jsoncarry: %T does not marshal: %v", v, err))
	}
	var named map[string]json.RawMessage
	if err := json.Unmarshal(data, &named); err != nil {
		panic(fmt.Sprintf("jsoncarry: %T does not marshal to an object: %v", v, err))
	}
	out := make(Fields, len(named)+len(omitted))
	for name := range named {
		out[name] = true
	}
	for _, name := range omitted {
		out[name] = true
	}
	return out
}

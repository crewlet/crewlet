package tracker

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"sync"
)

// The unknown-field round trip, for every object this package stores as a
// document or carries as a record payload.
//
// # Why a document keeps what this build cannot read
//
// A task commit decodes the stored document, merges the record onto it and
// encodes the result ([Applier.applyTask]); a purge's re-parent does the same
// to every child it moves. Whenever a stored document was written by a build
// that knows a field this one does not, a decode into a struct with no field
// for it would drop it, and the next encode would write the document without
// it. That node then holds a document without the field while every peer that
// knows it holds one with it: two copies of one log holding different rows for
// the same record, which is the one property the replicated estate may not
// lose.
//
// So each type with an `Extra` field decodes the keys it has no field for into
// it and writes them back, as [MutationRecord] does for the record envelope.
//
// # What is and is not preserved
//
// The KEYS AND THEIR VALUES, exactly as stored. Not the ORDER: an encode that
// carries unknown keys is a map merge, and a map encodes with its keys sorted,
// so the result has every key a build that knows them writes and the order that
// build's struct gives is not reproducible by one that does not have it. That
// is [MutationRecord.Encode]'s own shape. An object with nothing in `Extra`
// encodes byte for byte as its struct does, which is every object this build
// writes itself.

// decodeKeeping decodes b into into, a pointer to a struct type whose own
// fields are the known keys, and keeps every other top-level key in extra.
//
// A KEY IS KNOWN WHEN THE STRUCT DECODES IT, and encoding/json matches a key
// to a field case-insensitively — so the comparison is too, or a key spelled
// with different case would be decoded into its field AND kept, and written
// back twice.
func decodeKeeping(b []byte, into any, extra *map[string]json.RawMessage) error {
	if err := json.Unmarshal(b, into); err != nil {
		return err
	}
	var all map[string]json.RawMessage
	if err := json.Unmarshal(b, &all); err != nil {
		// NOT AN OBJECT — `null`, which the struct decode above accepted
		// as leaving it unchanged — so there are no keys to keep.
		*extra = nil
		return nil //nolint:nilerr // A non-object has no unknown keys.
	}
	known := knownKeysOf(reflect.TypeOf(into).Elem())
	var kept map[string]json.RawMessage
	for key, value := range all {
		if known[strings.ToLower(key)] {
			continue
		}
		if kept == nil {
			kept = make(map[string]json.RawMessage)
		}
		kept[key] = value
	}
	*extra = kept
	return nil
}

// encodeKeeping encodes v, a struct value, and writes extra's keys back beside
// its own. A key the struct writes wins over one extra carries: extra is only
// ever filled with keys the struct does not decode, and if it holds one
// anyway, this build's own value is the one it can be held to.
func encodeKeeping(v any, extra map[string]json.RawMessage) ([]byte, error) {
	body, err := json.Marshal(v)
	if err != nil || len(extra) == 0 {
		return body, err
	}
	var merged map[string]json.RawMessage
	if err := json.Unmarshal(body, &merged); err != nil {
		return nil, fmt.Errorf("tracker: re-encode a %T carrying %d field(s) "+
			"this build does not know: %w", v, len(extra), err)
	}
	for key, value := range extra {
		if _, taken := merged[key]; !taken {
			merged[key] = value
		}
	}
	return json.Marshal(merged)
}

// fieldKeys caches [knownKeysOf] per struct type.
var fieldKeys sync.Map // reflect.Type → map[string]bool

// knownKeysOf is the lower-cased set of top-level keys a struct type decodes:
// each exported field's JSON name, a promoted field's through an embedded
// struct, and nothing a `json:"-"` tag excludes.
//
// DERIVED FROM THE TYPE rather than listed beside it, because a list is
// correct only until the next field is added — and a field missing from it
// would be decoded into its field and kept in `Extra` as well, and written
// twice.
func knownKeysOf(t reflect.Type) map[string]bool {
	if cached, ok := fieldKeys.Load(t); ok {
		return cached.(map[string]bool)
	}
	out := map[string]bool{}
	collectKeys(t, out)
	fieldKeys.Store(t, out)
	return out
}

func collectKeys(t reflect.Type, into map[string]bool) {
	for i := range t.NumField() {
		field := t.Field(i)
		tag, _, _ := strings.Cut(field.Tag.Get("json"), ",")
		if tag == "-" && field.Tag.Get("json") == "-" {
			continue
		}
		if field.Anonymous && tag == "" {
			embedded := field.Type
			if embedded.Kind() == reflect.Pointer {
				embedded = embedded.Elem()
			}
			if embedded.Kind() == reflect.Struct {
				collectKeys(embedded, into)
				continue
			}
		}
		if !field.IsExported() {
			continue
		}
		name := tag
		if name == "" {
			name = field.Name
		}
		into[strings.ToLower(name)] = true
	}
}

// The methods below are each type's own use of the pair above, through a
// local type with the same fields and none of the methods — which is what
// stops the struct's own encode calling back into itself.

// UnmarshalJSON keeps the keys this build has no field for in Extra.
func (t *Task) UnmarshalJSON(b []byte) error {
	type fields Task
	return decodeKeeping(b, (*fields)(t), &t.Extra)
}

// MarshalJSON writes Extra back beside the known fields.
func (t Task) MarshalJSON() ([]byte, error) {
	type fields Task
	return encodeKeeping(fields(t), t.Extra)
}

// UnmarshalJSON keeps the keys this build has no field for in Extra.
func (p *TaskPatch) UnmarshalJSON(b []byte) error {
	type fields TaskPatch
	return decodeKeeping(b, (*fields)(p), &p.Extra)
}

// MarshalJSON writes Extra back beside the known fields.
func (p TaskPatch) MarshalJSON() ([]byte, error) {
	type fields TaskPatch
	return encodeKeeping(fields(p), p.Extra)
}

// UnmarshalJSON keeps the keys this build has no field for in Extra.
func (c *Counter) UnmarshalJSON(b []byte) error {
	type fields Counter
	return decodeKeeping(b, (*fields)(c), &c.Extra)
}

// MarshalJSON writes Extra back beside the known fields.
func (c Counter) MarshalJSON() ([]byte, error) {
	type fields Counter
	return encodeKeeping(fields(c), c.Extra)
}

// UnmarshalJSON keeps the keys this build has no field for in Extra.
func (o *RankOrder) UnmarshalJSON(b []byte) error {
	type fields RankOrder
	return decodeKeeping(b, (*fields)(o), &o.Extra)
}

// MarshalJSON writes Extra back beside the known fields.
func (o RankOrder) MarshalJSON() ([]byte, error) {
	type fields RankOrder
	return encodeKeeping(fields(o), o.Extra)
}

// UnmarshalJSON keeps the keys this build has no field for in Extra.
func (e *Eviction) UnmarshalJSON(b []byte) error {
	type fields Eviction
	return decodeKeeping(b, (*fields)(e), &e.Extra)
}

// MarshalJSON writes Extra back beside the known fields.
func (e Eviction) MarshalJSON() ([]byte, error) {
	type fields Eviction
	return encodeKeeping(fields(e), e.Extra)
}

// UnmarshalJSON keeps the keys this build has no field for in Extra.
func (g *Generation) UnmarshalJSON(b []byte) error {
	type fields Generation
	return decodeKeeping(b, (*fields)(g), &g.Extra)
}

// MarshalJSON writes Extra back beside the known fields.
func (g Generation) MarshalJSON() ([]byte, error) {
	type fields Generation
	return encodeKeeping(fields(g), g.Extra)
}

// UnmarshalJSON keeps the keys this build has no field for in Extra.
func (c *TypeCatalogue) UnmarshalJSON(b []byte) error {
	type fields TypeCatalogue
	return decodeKeeping(b, (*fields)(c), &c.Extra)
}

// MarshalJSON writes Extra back beside the known fields.
func (c TypeCatalogue) MarshalJSON() ([]byte, error) {
	type fields TypeCatalogue
	return encodeKeeping(fields(c), c.Extra)
}

// UnmarshalJSON keeps the keys this build has no field for in Extra.
func (c *FieldCatalogue) UnmarshalJSON(b []byte) error {
	type fields FieldCatalogue
	return decodeKeeping(b, (*fields)(c), &c.Extra)
}

// MarshalJSON writes Extra back beside the known fields.
func (c FieldCatalogue) MarshalJSON() ([]byte, error) {
	type fields FieldCatalogue
	return encodeKeeping(fields(c), c.Extra)
}

// UnmarshalJSON keeps the keys this build has no field for in Extra.
func (p *Project) UnmarshalJSON(b []byte) error {
	type fields Project
	return decodeKeeping(b, (*fields)(p), &p.Extra)
}

// MarshalJSON writes Extra back beside the known fields.
func (p Project) MarshalJSON() ([]byte, error) {
	type fields Project
	return encodeKeeping(fields(p), p.Extra)
}

// UnmarshalJSON keeps the keys this build has no field for in Extra.
func (s *TagSet) UnmarshalJSON(b []byte) error {
	type fields TagSet
	return decodeKeeping(b, (*fields)(s), &s.Extra)
}

// MarshalJSON writes Extra back beside the known fields.
func (s TagSet) MarshalJSON() ([]byte, error) {
	type fields TagSet
	return encodeKeeping(fields(s), s.Extra)
}

// UnmarshalJSON keeps the keys this build has no field for in Extra.
func (v *View) UnmarshalJSON(b []byte) error {
	type fields View
	return decodeKeeping(b, (*fields)(v), &v.Extra)
}

// MarshalJSON writes Extra back beside the known fields.
func (v View) MarshalJSON() ([]byte, error) {
	type fields View
	return encodeKeeping(fields(v), v.Extra)
}

// UnmarshalJSON keeps the keys this build has no field for in Extra.
func (g *Goal) UnmarshalJSON(b []byte) error {
	type fields Goal
	return decodeKeeping(b, (*fields)(g), &g.Extra)
}

// MarshalJSON writes Extra back beside the known fields.
func (g Goal) MarshalJSON() ([]byte, error) {
	type fields Goal
	return encodeKeeping(fields(g), g.Extra)
}

// UnmarshalJSON keeps the keys this build has no field for in Extra.
func (p *Person) UnmarshalJSON(b []byte) error {
	type fields Person
	return decodeKeeping(b, (*fields)(p), &p.Extra)
}

// MarshalJSON writes Extra back beside the known fields.
func (p Person) MarshalJSON() ([]byte, error) {
	type fields Person
	return encodeKeeping(fields(p), p.Extra)
}

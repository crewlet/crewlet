package jsoncarry_test

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/crewlet/crewlet/internal/jsoncarry"
)

// record is a build's view of a shape a newer build has since extended.
type record struct {
	V    int    `json:"v"`
	Name string `json:"name,omitempty"`
}

var fields = jsoncarry.Names(record{})

// A FIELD THIS BUILD DOES NOT KNOW SURVIVES A DECODE AND AN ENCODE.
//
// The whole point of the package: a rolling upgrade puts a record a newer
// build wrote on the wire, and the reanchor path REPUBLISHES what it read. A
// build that stripped what it did not understand would make every upgrade a
// silent data loss on exactly the records a newer peer had just written.
func TestAnUnknownFieldRoundTripsThroughABuildThatCannotReadIt(t *testing.T) {
	t.Parallel()
	const written = `{"v":1,"name":"jane","tier":"gold","tags":["a","b"]}`

	var got record
	extra, err := jsoncarry.Decode([]byte(written), &got, fields)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.V != 1 || got.Name != "jane" {
		t.Fatalf("the known half decoded as %+v", got)
	}
	if len(extra) != 2 {
		t.Fatalf("carried %v, want the two fields this build has no home for", extra)
	}

	out, err := jsoncarry.Encode(got, extra)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	var back map[string]any
	if err := json.Unmarshal(out, &back); err != nil {
		t.Fatalf("the re-encoded record is not an object: %v", err)
	}
	want := map[string]any{
		"v": float64(1), "name": "jane", "tier": "gold",
		"tags": []any{"a", "b"},
	}
	if !reflect.DeepEqual(back, want) {
		t.Errorf("re-encoded as %v, want %v — a field this build cannot read "+
			"must come back byte for byte", back, want)
	}
}

// A CARRIED FIELD LOSES TO A KNOWN ONE.
//
// THE clause, and the one a second implementation gets backwards. With the
// precedence the other way round a stale carried copy silently reverts the
// write that set the field — on exactly the path, an upgrade in progress,
// where nobody is looking. The control is the assertion itself: swap the
// `if _, known` test in Encode and this case goes red.
func TestACarriedFieldNeverOverwritesOneThisBuildSet(t *testing.T) {
	t.Parallel()
	// The carried map still holds the value as it was READ, which is what
	// happens when a caller decodes, edits and re-encodes.
	stale := map[string]json.RawMessage{"name": json.RawMessage(`"the old name"`)}

	out, err := jsoncarry.Encode(record{V: 1, Name: "the new name"}, stale)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	var back record
	if err := json.Unmarshal(out, &back); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if back.Name != "the new name" {
		t.Fatalf("the carried copy won and the record says %q — a carried "+
			"field must lose to a known one, or an upgrade in progress reverts "+
			"every value this build did understand", back.Name)
	}
}

// AN OMITEMPTY FIELD IS KNOWN WITHOUT ANYBODY LISTING IT, so clearing it to
// its zero value is not undone by the copy the decode carried.
//
// A zero value does not marshal an omitempty field, so a name set derived from
// a marshalled zero value cannot see it — and every domain had to list those
// names by hand beside the struct. Four had been missed (a person's and an
// invitation's `colleague`, a unit's `origin_key`, a seat's `origin_handle`),
// and each missed name was decoded into the struct AND carried as unknown, so
// a caller clearing it re-encoded the carried copy straight back. Mutation:
// derive the names from a marshalled zero value again and this reverts.
func TestAnOmitemptyFieldIsKnownWithoutBeingListed(t *testing.T) {
	t.Parallel()
	var got record
	extra, err := jsoncarry.Decode([]byte(`{"v":1,"name":"jane"}`), &got,
		jsoncarry.Names(record{}))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if _, carried := extra["name"]; carried {
		t.Fatal("an omitempty field this build owns was carried as unknown")
	}
	got.Name = "" // cleared back to its zero value, which omitempty drops
	out, err := jsoncarry.Encode(got, extra)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	var back record
	if err := json.Unmarshal(out, &back); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if back.Name != "" {
		t.Errorf("the cleared field came back as %q — the carried copy of a "+
			"field this build owns overwrote the caller's own write", back.Name)
	}
}

// NAMES FOLLOWS encoding/json's OWN RULES for which fields exist and what
// they are called.
func TestNamesReadsTheNamesEncodingJSONWouldWrite(t *testing.T) {
	t.Parallel()
	type envelope struct {
		Kind string `json:"kind"`
		When int    `json:"when,omitempty"`
	}
	type shaped struct {
		envelope                   // promoted, as encoding/json does
		Named    string            `json:"named,omitzero"`
		Untagged string            // named by its Go name
		Skipped  string            `json:"-"`
		Extra    map[string]string `json:"-"`
		hidden   string
	}
	_ = shaped{hidden: ""}
	got := jsoncarry.Names(shaped{})
	want := jsoncarry.Fields{"kind": true, "when": true, "named": true,
		"Untagged": true}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Names answered %v, want %v", got, want)
	}
}

// A RECORD TYPE THAT IS NOT A JSON OBJECT IS A BUILD FAILURE AT INIT, not a
// refusal at the first publish.
func TestNamesPanicsOnATypeThatIsNotAnObject(t *testing.T) {
	t.Parallel()
	defer func() {
		if recover() == nil {
			t.Error("Names accepted a type that does not marshal to an object, " +
				"so a domain could declare a record shape no encode can ever " +
				"carry a field through")
		}
	}()
	_ = jsoncarry.Names([]int{1, 2, 3})
}

// AN ENCODE WITH NOTHING CARRIED IS THE PLAIN MARSHAL, byte for byte.
//
// The early return is what keeps the common case — every record this build
// wrote itself — from a marshal, an unmarshal into a map and a second marshal,
// which would also reorder every key.
func TestAnEncodeWithNothingCarriedIsThePlainMarshal(t *testing.T) {
	t.Parallel()
	r := record{V: 2, Name: "jane"}
	got, err := jsoncarry.Encode(r, nil)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	want, err := json.Marshal(r)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if string(got) != string(want) {
		t.Errorf("encoded as %s, want %s", got, want)
	}
}

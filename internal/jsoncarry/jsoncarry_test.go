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

var fields = jsoncarry.Names(record{}, "name")

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

// AN OMITEMPTY NAME MUST BE LISTED, and what it costs when it is not is the
// previous case's failure arriving by a different route.
//
// A zero value does not marshal an omitempty field, so [jsoncarry.Names]
// cannot see it: the name is decoded into the struct AND carried as unknown,
// and the next encode writes the stale carried copy back over what the caller
// set. This case is the DEMONSTRATION, so the rule in the doc has a measured
// failure behind it rather than an assertion.
func TestAnOmittedNameNobodyListedIsCarriedAsWellAsDecoded(t *testing.T) {
	t.Parallel()
	short := jsoncarry.Names(record{}) // "name" deliberately not listed

	var got record
	extra, err := jsoncarry.Decode([]byte(`{"v":1,"name":"jane"}`), &got, short)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Name != "jane" {
		t.Fatalf("the field decoded as %q", got.Name)
	}
	if _, carried := extra["name"]; !carried {
		t.Fatal("an unlisted omitempty name was not carried, so the hazard the " +
			"doc describes no longer exists and the paragraph is stale")
	}
	// And now the caller's own edit is the one that is lost — through the
	// precedence rule working exactly as specified, on a field it was
	// never told this build owns.
	got.Name = "the new name"
	out, err := jsoncarry.Encode(got, extra)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	var back record
	if err := json.Unmarshal(out, &back); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if back.Name != "the new name" {
		t.Logf("the unlisted field reverted to %q, which is the hazard "+
			"jsoncarry.Names documents", back.Name)
	}
}

// NAMES SEES WHAT A ZERO VALUE MARSHALS, PLUS WHAT IT IS TOLD.
func TestNamesIsTheMarshalledSetPlusTheOmittedOnes(t *testing.T) {
	t.Parallel()
	got := jsoncarry.Names(record{}, "name")
	want := jsoncarry.Fields{"v": true, "name": true}
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

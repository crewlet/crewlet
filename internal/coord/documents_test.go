package coord_test

import (
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/coord"
)

// A key is a SUBJECT TOKEN PATH, and every property below is one a consumer
// filter or a prefix listing depends on. The grammar exists because the
// obvious thing — writing the segments in raw and joining them — cannot work:
// a page title contains spaces, a handle contains a dot, and both are refused
// by the store or silently re-tokenised into a key of the wrong shape.
func TestDocumentKeySegmentsRoundTrip(t *testing.T) {
	t.Parallel()
	for _, segments := range [][]string{
		{"i", "01J8Z2K3"},
		{"c", "01J8Z2K3", "01J8Z2M0"},
		// The cases the raw form gets wrong, one per reason.
		{"t", "ENG", "api.v2 runbook"},     // a dot and a space
		{"m", "item", "user:alice"},        // a colon, which the store refuses
		{"p", "Übersicht — Q3"},            // non-ASCII and an em dash
		{"n", "ENG/PLATFORM"},              // a slash
		{"x", strings.Repeat("deep.", 20)}, // many separators in one segment
	} {
		key := coord.DocumentKey(segments...)
		if strings.Count(key, coord.KeySeparator) != len(segments)-1 {
			t.Errorf("%q has %d separators, want %d: a segment leaked one",
				key, strings.Count(key, coord.KeySeparator), len(segments)-1)
		}
		got, ok := coord.DocumentSegments(key)
		if !ok {
			t.Errorf("%q did not decode", key)
			continue
		}
		if !slices.Equal(got, segments) {
			t.Errorf("round trip of %v gave %v", segments, got)
		}
	}
}

// INJECTIVE, which is the property two keys colliding would break. A
// collision here is two work items sharing one record.
func TestDocumentKeysDoNotCollide(t *testing.T) {
	t.Parallel()
	seen := map[string][]string{}
	for _, segments := range [][]string{
		{"a.b"}, {"a", "b"},
		{"a=2Eb"}, {"a", "=", "b"},
		{"c", "x"}, {"c.x"},
		{""}, {"", ""},
	} {
		key := coord.DocumentKey(segments...)
		if prior, dup := seen[key]; dup {
			t.Errorf("%v and %v both encode to %q", prior, segments, key)
		}
		seen[key] = segments
	}
}

// The class is the first token, and a filter on it must select exactly its own
// records. The failure this rules out is a change-key sweep purging the
// project counters, after which every item minted reuses a number.
func TestKeyClassIsTheFirstToken(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		segments []string
		want     string
	}{
		{[]string{"c", "item", "ulid"}, "c"},
		{[]string{"counter", "ENG"}, "counter"},
		{[]string{"i", "id"}, "i"},
	} {
		key := coord.DocumentKey(tc.segments...)
		got, ok := coord.KeyClass(key)
		if !ok || got != tc.want {
			t.Errorf("KeyClass(%q) = %q, %v; want %q", key, got, ok, tc.want)
		}
	}
	// "counter" starts with "c" and is a different class. A byte-wise
	// prefix test would put it in the change class's listing.
	if class, _ := coord.KeyClass(coord.DocumentKey("counter", "ENG")); class == "c" {
		t.Error("a longer class was read as a shorter one")
	}
}

// A key this grammar did not write is REFUSED rather than guessed at: a
// listing that invented a segment would put a document nobody wrote into a
// projection.
func TestForeignKeysAreRefused(t *testing.T) {
	t.Parallel()
	for _, key := range []string{
		"",
		"has space",
		"has:colon",
		"trailing=",    // an escape with no hex after it
		"bad=ZZ",       // not hex
		"lower=2ecase", // lower-case hex, which the encoder never emits
		// An empty segment. DocumentKey will happily build this from a
		// caller that lost a value, and it must not decode: a document
		// filed under a name that is nothing belongs to nothing.
		coord.DocumentKey("s", ""),
	} {
		if _, ok := coord.DocumentSegments(key); ok {
			t.Errorf("%q decoded, and it is not a key this grammar writes", key)
		}
	}
}

// Every family is valid, and nothing else is. The closed set is what keeps a
// bucket inventory exact and an unknown value off the wire a value rather
// than a panic.
func TestFamiliesAreAClosedSet(t *testing.T) {
	t.Parallel()
	for _, f := range coord.Families() {
		if !f.Valid() {
			t.Errorf("%q is listed and not valid", f)
		}
	}
	for _, f := range []coord.Family{"", "ledger", "Work", "work "} {
		if f.Valid() {
			t.Errorf("%q is valid and should not be", f)
		}
	}
	if len(coord.Families()) != 3 {
		t.Errorf("Families() has %d entries: a family added here needs a bucket, "+
			"a projection table set and a row in the coordination doc",
			len(coord.Families()))
	}
}

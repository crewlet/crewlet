package coord_test

import (
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/coord"
)

// A KEY BUILT FROM MORE SEGMENTS NESTS UNDER THE KEY IT EXTENDS, AND NOTHING
// ELSE DOES.
//
// The bridged-call log files a call's parts beneath the call's own key, and
// two things rest on this grammar alone: the purge of a launch's filter has to
// take the parts with their calls, and a reader that decodes a call by its
// depth must never take a part for one. The segments here are the awkward
// ones — a separator, a colon, a space, a letter outside ASCII — because a
// segment that could smuggle a separator in would let a key pose as a child
// it is not, or hide one that it is.
func TestAKeyWithOneMoreSegmentNestsUnderTheKeyItExtends(t *testing.T) {
	t.Parallel()
	for _, parent := range [][]string{
		{"call", "turn-1", "launch-1", "7"},
		{"call", "run.a", "launch.b", "7"},
		{"call", "t:é", "l 1", "12"},
		{"seat", "alice"},
	} {
		key := coord.DocumentKey(parent...)
		child := coord.DocumentKey(append(slices.Clone(parent), "1")...)

		if !strings.HasPrefix(child, key+coord.KeySeparator) {
			t.Errorf("%q is not under %q", child, key)
		}
		// The parent's filter is its key and a trailing wildcard, so what
		// it selects is exactly what starts with the key and a separator.
		filter := coord.DocumentFilter(parent...)
		if want := key + coord.KeySeparator + ">"; filter != want {
			t.Fatalf("DocumentFilter(%q) = %q, want %q", parent, filter, want)
		}
		segs, ok := coord.DocumentSegments(child)
		if !ok || len(segs) != len(parent)+1 || !slices.Equal(segs[:len(parent)], parent) || segs[len(parent)] != "1" {
			t.Errorf("DocumentSegments(%q) = %q, %v; want %q and one segment more", child, segs, ok, parent)
		}

		last := len(parent) - 1
		// A last segment that merely begins like the parent's is a sibling,
		// not a child: "70" is not beneath "7".
		longer := append(slices.Clone(parent[:last]), parent[last]+"0")
		if sibling := coord.DocumentKey(longer...); strings.HasPrefix(sibling, key+coord.KeySeparator) {
			t.Errorf("%q, a sibling, reads as under %q", sibling, key)
		}
		// And a last segment carrying the separator stays ONE segment: it
		// neither poses as the child nor adds a level.
		dotted := append(slices.Clone(parent[:last]), parent[last]+coord.KeySeparator+"1")
		posed := coord.DocumentKey(dotted...)
		if posed == child || strings.HasPrefix(posed, key+coord.KeySeparator) {
			t.Errorf("%q, one segment carrying a separator, reads as a child of %q", posed, key)
		}
		if segs, ok := coord.DocumentSegments(posed); !ok || len(segs) != len(parent) {
			t.Errorf("DocumentSegments(%q) = %d segments, want %d", posed, len(segs), len(parent))
		}
	}
}

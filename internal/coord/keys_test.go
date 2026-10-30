package coord_test

import (
	"slices"
	"testing"

	"github.com/crewlet/crewlet/internal/coord"
)

// A KEY WITH AN EMPTY SEGMENT DECODES TO NOTHING, wherever the empty segment
// sits.
//
// An empty segment is a caller that lost a value on its way to
// [coord.DocumentKey], and the grammar's answer is that the key it builds is
// one [coord.DocumentSegments] refuses, so every listing skips it. A decode
// that accepted one would hand every listing a record naming nothing — a lease
// on no seat, a follow on no thread — for its reader to act on as if a writer
// had meant it.
func TestAKeyWithAnEmptySegmentIsRefused(t *testing.T) {
	t.Parallel()
	for name, key := range map[string]string{
		"a lost id":         coord.DocumentKey("seat", ""),
		"a lost class":      coord.DocumentKey("", "alice"),
		"a lost middle":     coord.DocumentKey("follow", "slack", "", "C1"),
		"no segment at all": coord.DocumentKey(""),
		"a trailing dot":    "seat.",
		"a doubled dot":     "seat..alice",
	} {
		if got, ok := coord.DocumentSegments(key); ok {
			t.Errorf("%s: DocumentSegments(%q) = (%q, true), want the key refused",
				name, key, got)
		}
		if class, ok := coord.KeyClass(key); ok {
			t.Errorf("%s: KeyClass(%q) = (%q, true), want the key refused",
				name, key, class)
		}
	}

	// THE CONTROL: the same shapes with every value present decode to the
	// segments they were built from, a dot inside one of them included.
	for _, segments := range [][]string{
		{"seat", "alice"},
		{"follow", "slack", "agent-swe", "C1", "1712345678.123456"},
		{"node", "node-a.eu"},
	} {
		got, ok := coord.DocumentSegments(coord.DocumentKey(segments...))
		if !ok || !slices.Equal(got, segments) {
			t.Errorf("DocumentSegments(DocumentKey(%q)) = (%q, %v), want them back",
				segments, got, ok)
		}
	}
}

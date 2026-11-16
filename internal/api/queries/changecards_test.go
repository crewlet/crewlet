package queries_test

import (
	"errors"
	"testing"

	"github.com/crewlet/crewlet/internal/api/queries"
	"github.com/crewlet/crewlet/internal/gitlet"
)

// A CHANGES LISTING ASKS FOR CARDS ONLY WHERE ITS CALLER NAMES THEM, `cards=true`, and a
// spelling that is neither `true` nor `false` is refused before the reader is asked.
// Mutation: read `cards` as nothing, read it whatever the caller named, or read an unknown
// spelling as false.
func TestAChangesListingAsksForCardsOnlyWhereItsCallerNamesThem(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		args map[string]any
		want bool
	}{
		{"nothing", map[string]any{"repo": "web"}, false},
		{"cards", map[string]any{"repo": "web", "cards": "true"}, true},
		{"no cards, spelled out", map[string]any{"repo": "web", "cards": "false"}, false},
	} {
		stub := &stubCode{}
		if _, err := askNative(t, queries.Sources{Code: stub}, "code_changes", tc.args); err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if got := stub.last(); got.Kind != gitlet.ReadChanges || got.Cards != tc.want {
			t.Errorf("code_changes naming %s asked %s with cards %t, want a changes read with cards %t", tc.name,
				got.Kind, got.Cards, tc.want)
		}
	}
	stub := &stubCode{}
	if _, err := askNative(t, queries.Sources{Code: stub}, "code_changes",
		map[string]any{"repo": "web", "cards": "yes"}); !errors.Is(err, queries.ErrBadParams) || len(stub.asked) != 0 {
		t.Errorf("cards=yes: answered %v having asked the reader %d times, want bad_params before it", err,
			len(stub.asked))
	}
}

package queries_test

import (
	"testing"

	"github.com/crewlet/crewlet/internal/api/queries"
	"github.com/crewlet/crewlet/internal/gitlet"
)

// A CHANGES LISTING ASKS THE WORDS ITS CALLER NAMED: `text` reaches the reader as given (a text past
// its bounds is the reader's to refuse), none named asks none, and no other kind reads it.
// Mutation: read `text` on another kind, or drop it.
func TestAChangesListingAsksTheWordsItsCallerNamed(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		params map[string]any
		want   string
	}{
		{map[string]any{"repo": "web"}, ""},
		{map[string]any{"text": "login"}, "login"},
		{map[string]any{"state": "open", "text": "fix login"}, "fix login"},
	} {
		stub := &stubCode{}
		if _, err := askNative(t, queries.Sources{Code: stub}, "code_changes", tc.params); err != nil {
			t.Fatalf("code_changes %v: %v", tc.params, err)
		}
		if asked := stub.last(); asked.Kind != gitlet.ReadChanges || asked.Text != tc.want {
			t.Errorf("code_changes %v asked %+v, want a changes read asking text %q", tc.params, asked, tc.want)
		}
	}
	stub := &stubCode{}
	if _, err := askNative(t, queries.Sources{Code: stub}, "code_reviews", map[string]any{"reviewer": "cto",
		"text": "login"}); err != nil {
		t.Fatalf("code_reviews with words: %v", err)
	}
	if asked := stub.last(); asked.Text != "" {
		t.Errorf("code_reviews with words asked %+v, want none: only a listing of changes is narrowed by them",
			asked)
	}
}

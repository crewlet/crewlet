package queries_test

import (
	"testing"

	"github.com/crewlet/crewlet/internal/api/queries"
	"github.com/crewlet/crewlet/internal/gitlet"
)

// A CHANGES LISTING ASKS THE READINESS ITS CALLER NAMED: `readiness` reaches the reader as given (an
// unknown bucket is the reader's to refuse), and none named narrows nothing.
// Mutation: read `readiness` on another kind, or drop it.
func TestAChangesListingAsksTheReadinessItsCallerNamed(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		params map[string]any
		want   gitlet.ReadinessBucket
	}{
		{map[string]any{"repo": "web"}, ""},
		{map[string]any{"readiness": "ready"}, gitlet.BucketReady},
		{map[string]any{"repo": "web", "readiness": "checks"}, gitlet.BucketChecks},
		{map[string]any{"readiness": "sideways"}, "sideways"},
	} {
		stub := &stubCode{}
		if _, err := askNative(t, queries.Sources{Code: stub}, "code_changes", tc.params); err != nil {
			t.Fatalf("code_changes %v: %v", tc.params, err)
		}
		if asked := stub.last(); asked.Kind != gitlet.ReadChanges || asked.Readiness != tc.want {
			t.Errorf("code_changes %v asked a %s read narrowed to %q, want a changes read narrowed to %q", tc.params,
				asked.Kind, asked.Readiness, tc.want)
		}
	}
	stub := &stubCode{}
	if _, err := askNative(t, queries.Sources{Code: stub}, "code_reviews", map[string]any{"reviewer": "cto",
		"readiness": "ready"}); err != nil {
		t.Fatalf("code_reviews with a readiness: %v", err)
	}
	if asked := stub.last(); asked.Readiness != "" {
		t.Errorf("code_reviews with a readiness asked one narrowed to %q, want none: only a listing of changes "+
			"is narrowed so", asked.Readiness)
	}
}

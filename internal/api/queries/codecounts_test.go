package queries_test

import (
	"errors"
	"testing"

	"github.com/crewlet/crewlet/internal/api/queries"
	"github.com/crewlet/crewlet/internal/gitlet"
)

// A COUNTS QUESTION ASKS ITS SCOPE AND ITS PARTS as the read's own keys: none named reaches the
// reader as nil, an unknown part or a `parts` that is no list is bad_params, `repo_facts` asks facts.
// Mutation: read `parts` off another key, accept an unknown part, or drop `repo_facts`.
func TestACountsQuestionAsksItsScopeAndParts(t *testing.T) {
	t.Parallel()
	for _, parts := range []any{"changes", "changes, changes", []any{"changes", "changes"}} {
		stub := &stubCode{}
		if _, err := askNative(t, queries.Sources{Code: stub}, "code_counts", map[string]any{"repo": "web",
			"parts": parts}); err != nil {
			t.Fatalf("the counts of web's %v: %v", parts, err)
		}
		asked := stub.last()
		if asked.Kind != gitlet.ReadCounts || asked.Repo != "web" || asked.Counts == nil ||
			*asked.Counts != (gitlet.CountsQuery{Changes: true}) {
			t.Errorf("the counts of web's %v asked %+v, want one counts read of web's changes", parts, asked)
		}
	}
	stub := &stubCode{}
	if _, err := askNative(t, queries.Sources{Code: stub}, "code_counts", map[string]any{}); err != nil {
		t.Fatalf("the counts naming no part: %v", err)
	}
	if asked := stub.last(); asked.Kind != gitlet.ReadCounts || asked.Counts != nil || asked.Repo != "" {
		t.Errorf("the counts naming no part asked %+v, want the company's with no part, the reader's to refuse",
			asked)
	}
	for _, parts := range []any{"nope", "changes,nope", 7} {
		none := &stubCode{}
		if _, err := askNative(t, queries.Sources{Code: none}, "code_counts", map[string]any{"parts": parts}); !errors.Is(err, queries.ErrBadParams) || len(none.asked) != 0 {
			t.Errorf("parts %v answered %v having read %d, want bad_params", parts, err, len(none.asked))
		}
	}
	for _, q := range []struct {
		what   string
		params map[string]any
		kind   gitlet.ReadKind
	}{
		{"code_repos", map[string]any{"repo_facts": "true"}, gitlet.ReadRepos},
		{"code_repo", map[string]any{"name": "web", "repo_facts": "true"}, gitlet.ReadRepo},
	} {
		stub := &stubCode{}
		if _, err := askNative(t, queries.Sources{Code: stub}, q.what, q.params); err != nil {
			t.Fatalf("%s with its facts: %v", q.what, err)
		}
		if asked := stub.last(); asked.Kind != q.kind || !asked.RepoFacts {
			t.Errorf("%s with its facts asked %+v, want a %s read asking RepoFacts", q.what, asked, q.kind)
		}
	}
	none := &stubCode{}
	if _, err := askNative(t, queries.Sources{Code: none}, "code_repos", map[string]any{"repo_facts": "yes"}); !errors.Is(err, queries.ErrBadParams) || len(none.asked) != 0 {
		t.Errorf("repo_facts=yes answered %v having read %d, want bad_params", err, len(none.asked))
	}
}

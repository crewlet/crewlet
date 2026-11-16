package gitlet_test

import (
	"encoding/base64"
	"errors"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/gitlet"
)

// A directory's last commits name a revision and a directory, the root or a clean path, once.
// Mutations: skip the rev or the path check; leave GitTreeCommits out of Unstampable or GitReadKinds.
func TestATreeCommitsQueryValidates(t *testing.T) {
	t.Parallel()
	asker := gitlet.Principal{Kind: gitlet.PrincipalOperator, OperatorID: "token:ci"}
	repo := "01920000-0000-7000-8000-000000000001"
	id := strings.Repeat("ab", 20)
	q := func(tc gitlet.TreeCommitsQuery) gitlet.GitQuery {
		return gitlet.GitQuery{Kind: gitlet.GitTreeCommits, RepoID: repo, Asker: asker, TreeCommits: &tc}
	}
	for _, tc := range []struct {
		name, field string
		q           gitlet.GitQuery
	}{
		{"a ref", "tree_commits.rev", q(gitlet.TreeCommitsQuery{Rev: "main"})},
		{"no rev", "tree_commits.rev", q(gitlet.TreeCommitsQuery{})},
		{"both path and path_b64", "tree_commits.path",
			q(gitlet.TreeCommitsQuery{Rev: id, Path: "d", PathB64: base64.StdEncoding.EncodeToString([]byte("d"))})},
		{"an unclean path", "tree_commits.path", q(gitlet.TreeCommitsQuery{Rev: id, Path: "d/../e"})},
		{"a leading slash", "tree_commits.path", q(gitlet.TreeCommitsQuery{Rev: id, Path: "/d"})},
		{"bad base64", "tree_commits.path_b64", q(gitlet.TreeCommitsQuery{Rev: id, PathB64: "!!"})},
		{"none of its own", "tree_commits", gitlet.GitQuery{Kind: gitlet.GitTreeCommits, RepoID: repo, Asker: asker}},
		{"another kind's query", "tree", func() gitlet.GitQuery {
			r := q(gitlet.TreeCommitsQuery{Rev: id})
			r.Tree = &gitlet.TreeQuery{Rev: id}
			return r
		}()},
	} {
		err := tc.q.Validate()
		if !errors.Is(err, gitlet.ErrInvalid) || !strings.Contains(err.Error(), tc.field+":") {
			t.Errorf("%s: Validate = %v, want ErrInvalid naming %s", tc.name, err, tc.field)
		}
	}
	for _, ok := range []gitlet.TreeCommitsQuery{{Rev: id[:7]}, {Rev: id, Path: "a/b"},
		{Rev: id, PathB64: base64.StdEncoding.EncodeToString([]byte("caf\xe9"))}} {
		r := q(ok)
		r.Unstamped = true
		if err := r.Validate(); err != nil {
			t.Errorf("%+v, unstamped: %v, want a read", ok, err)
		}
	}
	if !gitlet.GitTreeCommits.Valid() {
		t.Error("tree_commits is not a kind this build answers")
	}
}

// A page is cut where its budget ended it, nil and an unmarked page are not.
// Mutation: read an entry's mark in Cut, or drop the nil guard.
func TestATreeCommitsPageIsCutOnlyWhereMarked(t *testing.T) {
	t.Parallel()
	var none *gitlet.TreeCommitsAnswer
	entryOnly := &gitlet.TreeCommitsAnswer{Entries: []gitlet.TreeCommit{{Path: "a", OutOfTime: true}}}
	for _, tc := range []struct {
		name string
		a    *gitlet.TreeCommitsAnswer
		want bool
	}{
		{"nil", none, false},
		{"unmarked", &gitlet.TreeCommitsAnswer{Entries: []gitlet.TreeCommit{{Path: "a", Commit: "c"}}}, false},
		{"an entry marked, the page not", entryOnly, false},
		{"marked", &gitlet.TreeCommitsAnswer{OutOfTime: true}, true},
	} {
		if got := tc.a.Cut(); got != tc.want {
			t.Errorf("%s: Cut = %t, want %t", tc.name, got, tc.want)
		}
	}
}

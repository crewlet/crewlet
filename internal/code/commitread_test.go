package code_test

import (
	"errors"
	"reflect"
	"testing"

	"github.com/crewlet/crewlet/internal/code"
)

// A COMMIT NAMES THE CHANGE IT BELONGS TO. A commit's page reads, beside the
// repository's detail and in its snapshot, every revision the commit heads —
// "revision 2 of web!1" — and every change whose merge it is, each by the
// repository's name, its reference, its title and its state: here a change
// opened at feature's B, moved to C by a push to its source and merged by a
// push setting main to C. B heads the first revision, C the second and is the
// change's merge, and a commit no change names — or one another repository's
// change heads — belongs to none of this repository's. A commit is named by
// its whole id, and the read is not paged.
//
// Mutation: drop the repository from commitHeadsSQL's join — api's change at
// B is named in web's answer; read Merged by the head index rather than the
// merge's — C's merge is never named.
func TestACommitNamesTheChangeItBelongsTo(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	c, _ := committerOn(t, n, n.pub)
	ctx := t.Context()
	web := changeRepo(t, n, "web")
	api := changeRepo(t, n, "api")
	opened, err := c.ProposeChange(ctx, opening(web, featureRef, mainRef, dev))
	requireChanged(t, opened, err)
	other, err := c.ProposeChange(ctx, opening(api, featureRef, mainRef, dev))
	requireChanged(t, other, err)
	pushedBy(t, c, web, ada, update(featureRef, oidOf("B"), oidOf("C")))
	pushedBy(t, c, web, ada, update(mainRef, oidOf("A"), oidOf("C")))

	read := func(commit string) code.ReadAnswer {
		t.Helper()
		a, err := n.reader.Read(ctx, code.ReadQuery{Kind: code.ReadCommit, Asker: asker, Repo: "web",
			Commit: commit})
		if err != nil {
			t.Fatalf("read commit %s of web: %v", commit, err)
		}
		if a.Repo == nil || a.Repo.ID != web || a.Commit == nil || a.Commit.Commit != commit {
			t.Fatalf("a commit read of web = %+v, want web's detail and the commit's changes", a)
		}
		return a
	}
	merged := code.ChangeName{Repo: "web", Ref: "web!1", Title: "Retry the upload", State: code.ChangeStateMerged}
	for _, tc := range []struct {
		commit    string
		merged    []code.ChangeName
		revisions []code.CommitRevision
	}{
		{oidOf("B"), nil, []code.CommitRevision{{Change: merged, N: 1}}},
		{oidOf("C"), []code.ChangeName{merged}, []code.CommitRevision{{Change: merged, N: 2}}},
		{oidOf("Z"), nil, nil},
	} {
		got := read(tc.commit).Commit
		if !reflect.DeepEqual(got.Merged, tc.merged) || !reflect.DeepEqual(got.Revisions, tc.revisions) ||
			got.More {
			t.Errorf("commit %s of web belongs to %+v, want merged %+v and revisions %+v", tc.commit, got,
				tc.merged, tc.revisions)
		}
	}

	for _, q := range []code.ReadQuery{
		{Kind: code.ReadCommit, Asker: asker, Repo: "web", Commit: oidOf("B")[:12]},
		{Kind: code.ReadCommit, Asker: asker, Repo: "web", Commit: oidOf("B"), Limit: 1},
		{Kind: code.ReadRepo, Asker: asker, Repo: "web", Commit: oidOf("B")},
	} {
		if _, err := n.reader.Read(ctx, q); !errors.Is(err, code.ErrInvalid) {
			t.Errorf("a %s read naming commit %q, limit %d = %v, want ErrInvalid", q.Kind, q.Commit, q.Limit, err)
		}
	}
}

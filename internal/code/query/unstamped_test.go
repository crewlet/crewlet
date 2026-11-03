package query_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"testing"

	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/code/cache"
	"github.com/crewlet/crewlet/internal/code/codetest"
	"github.com/crewlet/crewlet/internal/code/query"
	"github.com/crewlet/crewlet/internal/statelog"
)

// unindexed is a node's cache whose index of pushes it cannot read yet: what
// a node that has not built it answers, so a stamped read names no push.
type unindexed struct{ query.Cache }

func (unindexed) Provenance(cache.Synced) (cache.Provenance, error) {
	return cache.Provenance{}, errors.New("unindexed: this copy's index is not built yet")
}

// AN UNSTAMPED PAGE IS A FUNCTION OF WHAT IT NAMES. A stamped page is cut
// against the room its stamps leave, so the same history by the same whole id
// is a page of fewer commits where the node's index names the push behind each
// — a question could never answer it as one that never changes. Asked
// unstamped (code.GitQuery.Unstamped), it is one page, byte for byte, whoever
// cuts it: the read that cuts it and the one the memo answers with no git at
// all, a node whose index names every push and one that has none, and a copy
// the log has moved past since — and it carries no stamp, so nothing it
// answers moves either: no repository, no position, no memo, no push and no
// coverage.
//
// Mutation: read the index on an unstamped read (index's Unstamped arm) — the
// indexed node cuts a shorter page than the unindexed one; stamp an
// unstamped answer (stamp's Unstamped arm) — it carries a position and the
// pushes; key the two cuts alike (memoKey's "unstamped" part) — the stamped
// read is answered the unstamped page from the memo, and overruns its bound.
func TestAnUnstampedPageIsAFunctionOfWhatItNames(t *testing.T) {
	t.Parallel()
	x := newFixture(t, codetest.SoloOptions{}, codetest.Settings())
	var head string
	for i := range 24 {
		head = x.p.Commit(x.work, map[string]string{"history.txt": fmt.Sprintf("line %d\n", i)},
			fmt.Sprintf("commit %02d of a history read a page at a time", i))
	}
	x.push(codetest.RepoName, "main")
	repo := x.repo(codetest.RepoName)
	x.settle(0, repo)
	node := x.f.Nodes()[0]
	const limit = 4 << 10
	server := func(o query.Options) *query.Server {
		s := x.server(0, o)
		query.SetPageBytes(s, limit)
		return s
	}
	q := logQ(repo, code.LogQuery{Rev: head, Limit: code.MaxLogLimit})

	stamped := mustRead(t, server(query.Options{}), q)
	if len(stamped.PushedBy) == 0 || stamped.Position.IsZero() {
		t.Fatalf("a stamped page of a history the index holds = %+v, want the push behind each commit and the "+
			"copy's position", stamped)
	}
	bare := mustRead(t, server(query.Options{Cache: unindexed{memoless{node.Cache()}}}), q)
	if len(bare.Log.Commits) <= len(stamped.Log.Commits) {
		t.Fatalf("a stamped page lists %d commits where the index names their pushes and %d where it names "+
			"none: a case whose stamps move no page shows nothing", len(stamped.Log.Commits), len(bare.Log.Commits))
	}

	q.Unstamped = true
	rec := &recording{}
	memoized := server(query.Options{Git: rec.on(x, 0)})
	cut := mustRead(t, memoized, q)
	ran := len(rec.ran())
	recalled := mustRead(t, memoized, q)
	if n := len(rec.ran()) - ran; n != 0 {
		t.Errorf("the second read of the page ran %d processes, want it answered from the memo", n)
	}
	indexless := mustRead(t, server(query.Options{Cache: unindexed{memoless{node.Cache()}}}), q)
	x.commit(map[string]string{"later.txt": "a later push moves the copy\n"}, "a later commit")
	x.synced(0, repo)
	moved := mustRead(t, server(query.Options{Cache: memoless{node.Cache()}}), q)
	if moved.Position != (statelog.Position{}) {
		t.Errorf("an unstamped page carries the copy's position %v", moved.Position)
	}

	want, err := json.Marshal(cut)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		a    code.GitAnswer
	}{{"recalled from the memo", recalled}, {"cut by a node with no index", indexless},
		{"cut from a copy the log moved past", moved}} {
		got, err := json.Marshal(tc.a)
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != string(want) {
			t.Errorf("the unstamped page %s =\n%s\nwant the page cut\n%s", tc.name, got, want)
		}
	}
	if cut.RepoID != "" || cut.Memo || cut.PushedBy != nil || cut.Coverage != nil || !cut.Position.IsZero() {
		t.Errorf("an unstamped page carries a stamp: %+v", cut)
	}
	if len(cut.Log.Commits) != len(bare.Log.Commits) {
		t.Errorf("the unstamped page lists %d commits, want the %d a page no stamp takes room from holds",
			len(cut.Log.Commits), len(bare.Log.Commits))
	}
	again := mustRead(t, memoized, logQ(repo, code.LogQuery{Rev: head, Limit: code.MaxLogLimit}))
	if len(again.Log.Commits) != len(stamped.Log.Commits) {
		t.Errorf("the stamped read after the unstamped one lists %d commits, want its own cut's %d",
			len(again.Log.Commits), len(stamped.Log.Commits))
	}
}

// AN UNSTAMPED READ IS A PAGE'S. A landed-in read's answer is what the node's
// index says of the push that landed a commit, and a revision's facts, a
// re-anchoring and a checks file answer no page: none can be asked
// unstamped, and each is refused before any git node is.
//
// Mutation: drop Validate's unstamped arm — every one of them is accepted.
func TestOnlyAPageIsReadUnstamped(t *testing.T) {
	t.Parallel()
	const repo, oid = "0e8bd7a4-5a04-4e17-8f3c-7e7d4b2c9a11", "4b825dc642cb6eb9a060e54bf8d69288fbee4904"
	for _, q := range []code.GitQuery{
		{Kind: code.GitLandedIn, RepoID: repo, Asker: person, LandedIn: &code.LandedInQuery{Commit: oid,
			Target: "refs/heads/main", TargetOID: oid, At: statelog.Position{Stream: "s", Generation: 1, Seq: 1}}},
		{Kind: code.GitChecksFile, RepoID: repo, ChecksFile: &code.ChecksFileQuery{Commit: oid}},
	} {
		q.Unstamped = true
		if err := q.Validate(); !errors.Is(err, code.ErrInvalid) {
			t.Errorf("an unstamped %s read validated with %v, want ErrInvalid", q.Kind, err)
		}
	}
	for _, q := range []code.GitQuery{treeQ(repo, code.TreeQuery{Rev: oid}), logQ(repo, code.LogQuery{Rev: oid}),
		compareQ(repo, oid, oid)} {
		q.Unstamped = true
		if err := q.Validate(); err != nil {
			t.Errorf("an unstamped %s read refused: %v", q.Kind, err)
		}
	}
}

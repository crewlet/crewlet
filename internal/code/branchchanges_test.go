package code_test

import (
	"errors"
	"reflect"
	"testing"

	"github.com/crewlet/crewlet/internal/code"
)

// A BRANCH IS NAMED BY THE LIVE CHANGE IT HEADS: a `branch_changes` read — the
// engine's own, what it names a collected coding run's delivered refs by —
// answers each branch it names that heads a live change, a draft's included,
// in the order it names them, and leaves out one that heads none, one whose
// change is closed, and one that does not exist. It is the engine's, so it
// names no asker, and it names its repository by id and each branch once,
// whole, under refs/heads/.
//
// Mutation: answer the changes in the statement's order rather than the
// query's — release's draft before feature's change, red.
func TestABranchIsNamedByTheLiveChangeItHeads(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	c, _ := committerOn(t, n, n.pub)
	ctx := t.Context()
	repo := changeRepo(t, n, "web")
	feature, err := c.ProposeChange(ctx, opening(repo, featureRef, mainRef, dev))
	requireChanged(t, feature, err)
	draft := opening(repo, releaseRef, mainRef, dev)
	draft.Open.Draft = true
	release, err := c.ProposeChange(ctx, draft)
	requireChanged(t, release, err)

	read := func(refs ...string) []code.BranchChange {
		t.Helper()
		answer, err := n.reader.Read(ctx, code.ReadQuery{Kind: code.ReadBranchChanges,
			BranchChanges: &code.BranchChangesQuery{RepoID: repo, Refs: refs}, Fresh: stale})
		if err != nil {
			t.Fatalf("branch_changes: %v", err)
		}
		return answer.BranchChanges
	}
	want := []code.BranchChange{{Ref: featureRef, ChangeID: feature.ChangeID, Number: feature.Number},
		{Ref: releaseRef, ChangeID: release.ChangeID, Number: release.Number}}
	if got := read(featureRef, mainRef, "refs/heads/gone", releaseRef); !reflect.DeepEqual(got, want) {
		t.Fatalf("the branches' changes are %+v, want %+v", got, want)
	}
	if got := read(releaseRef, featureRef); !reflect.DeepEqual(got, []code.BranchChange{want[1], want[0]}) {
		t.Fatalf("named the other way round, the branches' changes are %+v, want them in the query's order", got)
	}

	closed, err := c.ProposeChange(ctx, writing(dev, feature.Version,
		func(p *code.ChangeProposal) {
			p.Close = &code.ChangeClose{RepoID: repo, ChangeID: feature.ChangeID}
		}))
	requireChanged(t, closed, err)
	if got := read(featureRef, releaseRef); !reflect.DeepEqual(got, want[1:]) {
		t.Fatalf("once feature's change is closed the branches' changes are %+v, want release's alone", got)
	}

	for name, q := range map[string]code.ReadQuery{
		"an asker": {Kind: code.ReadBranchChanges, Asker: dev,
			BranchChanges: &code.BranchChangesQuery{RepoID: repo, Refs: []string{featureRef}}, Fresh: stale},
		"no branches": {Kind: code.ReadBranchChanges, Fresh: stale},
		"no branch":   {Kind: code.ReadBranchChanges, BranchChanges: &code.BranchChangesQuery{RepoID: repo}, Fresh: stale},
		"a tag": {Kind: code.ReadBranchChanges,
			BranchChanges: &code.BranchChangesQuery{RepoID: repo, Refs: []string{"refs/tags/v1"}}, Fresh: stale},
		"a branch twice": {Kind: code.ReadBranchChanges,
			BranchChanges: &code.BranchChangesQuery{RepoID: repo, Refs: []string{featureRef, featureRef}}, Fresh: stale},
		"a repository by name": {Kind: code.ReadBranchChanges,
			BranchChanges: &code.BranchChangesQuery{RepoID: "web", Refs: []string{featureRef}}, Fresh: stale},
		"a cursor": {Kind: code.ReadBranchChanges, Cursor: "1",
			BranchChanges: &code.BranchChangesQuery{RepoID: repo, Refs: []string{featureRef}}, Fresh: stale},
		"no level": {Kind: code.ReadBranchChanges,
			BranchChanges: &code.BranchChangesQuery{RepoID: repo, Refs: []string{featureRef}}},
		"another kind's question": {Kind: code.ReadChanges, Asker: dev, Repo: "web",
			BranchChanges: &code.BranchChangesQuery{RepoID: repo, Refs: []string{featureRef}}, Fresh: stale},
	} {
		if _, err := n.reader.Read(ctx, q); !errors.Is(err, code.ErrInvalid) && !errors.Is(err, code.ErrBadLevel) {
			t.Errorf("a branch_changes read naming %s answered %v, want it refused", name, err)
		}
	}
}

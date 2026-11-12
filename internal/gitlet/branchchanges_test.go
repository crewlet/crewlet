package gitlet_test

import (
	"errors"
	"fmt"
	"reflect"
	"testing"

	"github.com/crewlet/crewlet/internal/gitlet"
)

// A BRANCH IS NAMED BY THE LIVE CHANGE IT HEADS: a `branch_changes` read — the
// engine's own, what it names a collected executor's delivered refs by —
// answers each branch it names that heads a live change, a draft's included,
// in the order it names the repositories and their branches, and leaves out
// one that heads none, one whose change is closed, and one that does not
// exist. EVERY REPOSITORY IN ONE READ: one answer names the branches of each
// repository of the run, each by its repository, so a collection pays one
// read for all of them. It is the engine's, so it names no asker, and it names
// each repository by id and once, each with its branches, each branch once,
// whole, under refs/heads/.
//
// Mutations: order a repository's changes against the query's order — feature's
// change after release's draft, red; serve the first repository alone — api's
// change missing, red. (Dropping the order's sort leaves this green: the
// statement's CROSS JOIN drives it from the query's list in its order, and the
// sort is what keeps the answer in that order should a plan ever stop.)
func TestABranchIsNamedByTheLiveChangeItHeads(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	c, _ := committerOn(t, n, n.pub)
	ctx := t.Context()
	repo := changeRepo(t, n, "web")
	api := changeRepo(t, n, "api")
	feature, err := c.ProposeChange(ctx, opening(repo, featureRef, mainRef, dev))
	requireChanged(t, feature, err)
	draft := opening(repo, releaseRef, mainRef, dev)
	draft.Open.Draft = true
	release, err := c.ProposeChange(ctx, draft)
	requireChanged(t, release, err)
	apiFeature, err := c.ProposeChange(ctx, opening(api, featureRef, mainRef, dev))
	requireChanged(t, apiFeature, err)

	read := func(repos ...gitlet.RepoBranches) []gitlet.BranchChange {
		t.Helper()
		answer, err := n.reader.Read(ctx, gitlet.ReadQuery{Kind: gitlet.ReadBranchChanges,
			BranchChanges: &gitlet.BranchChangesQuery{Repos: repos}, Fresh: stale})
		if err != nil {
			t.Fatalf("branch_changes: %v", err)
		}
		return answer.BranchChanges
	}
	of := func(id string, refs ...string) gitlet.RepoBranches {
		return gitlet.RepoBranches{RepoID: id, Refs: refs}
	}
	want := []gitlet.BranchChange{
		{RepoID: repo, Ref: featureRef, ChangeID: feature.ChangeID, Number: feature.Number},
		{RepoID: repo, Ref: releaseRef, ChangeID: release.ChangeID, Number: release.Number},
		{RepoID: api, Ref: featureRef, ChangeID: apiFeature.ChangeID, Number: apiFeature.Number}}
	if got := read(of(repo, featureRef, mainRef, "refs/heads/gone", releaseRef),
		of(api, mainRef, featureRef)); !reflect.DeepEqual(got, want) {
		t.Fatalf("the branches' changes are %+v, want %+v", got, want)
	}
	if got := read(of(api, featureRef), of(repo, releaseRef, featureRef)); !reflect.DeepEqual(got,
		[]gitlet.BranchChange{want[2], want[1], want[0]}) {
		t.Fatalf("named the other way round, the branches' changes are %+v, want them in the query's order", got)
	}

	closed, err := c.ProposeChange(ctx, writing(dev, feature.Version,
		func(p *gitlet.ChangeProposal) {
			p.Close = &gitlet.ChangeClose{RepoID: repo, ChangeID: feature.ChangeID}
		}))
	requireChanged(t, closed, err)
	if got := read(of(repo, featureRef, releaseRef)); !reflect.DeepEqual(got, want[1:2]) {
		t.Fatalf("once feature's change is closed the branches' changes are %+v, want release's alone", got)
	}

	nine := make([]gitlet.RepoBranches, gitlet.MaxRunRepos+1)
	for i := range nine {
		nine[i] = of(fmt.Sprintf("0190d8f0-0000-7000-8000-%012d", i), featureRef)
	}
	many := make([]string, gitlet.MaxRefsPerQuery+1)
	for i := range many {
		many[i] = fmt.Sprintf("refs/heads/b%d", i)
	}
	for name, q := range map[string]*gitlet.BranchChangesQuery{
		"no repository":                         {},
		"no branch":                             {Repos: []gitlet.RepoBranches{{RepoID: repo}}},
		"a tag":                                 {Repos: []gitlet.RepoBranches{of(repo, "refs/tags/v1")}},
		"a branch twice":                        {Repos: []gitlet.RepoBranches{of(repo, featureRef, featureRef)}},
		"a repository twice":                    {Repos: []gitlet.RepoBranches{of(repo, featureRef), of(repo, releaseRef)}},
		"a repository by name":                  {Repos: []gitlet.RepoBranches{of("web", featureRef)}},
		"more repositories than a run is given": {Repos: nine},
		"more branches than a read answers": {Repos: []gitlet.RepoBranches{of(repo, many[:gitlet.MaxRefsPerQuery]...),
			of(api, many[gitlet.MaxRefsPerQuery:]...)}},
	} {
		if _, err := n.reader.Read(ctx, gitlet.ReadQuery{Kind: gitlet.ReadBranchChanges, BranchChanges: q,
			Fresh: stale}); !errors.Is(err, gitlet.ErrInvalid) {
			t.Errorf("a branch_changes read naming %s answered %v, want it refused", name, err)
		}
	}
	one := &gitlet.BranchChangesQuery{Repos: []gitlet.RepoBranches{of(repo, featureRef)}}
	for name, q := range map[string]gitlet.ReadQuery{
		"an asker": {Kind: gitlet.ReadBranchChanges, Asker: dev, BranchChanges: one, Fresh: stale},
		"no query": {Kind: gitlet.ReadBranchChanges, Fresh: stale},
		"a cursor": {Kind: gitlet.ReadBranchChanges, Cursor: "1", BranchChanges: one, Fresh: stale},
		"no level": {Kind: gitlet.ReadBranchChanges, BranchChanges: one},
		"another kind's question": {Kind: gitlet.ReadChanges, Asker: dev, Repo: "web", BranchChanges: one,
			Fresh: stale},
	} {
		if _, err := n.reader.Read(ctx, q); !errors.Is(err, gitlet.ErrInvalid) && !errors.Is(err, gitlet.ErrBadLevel) {
			t.Errorf("a branch_changes read naming %s answered %v, want it refused", name, err)
		}
	}
}

package query_test

import (
	"errors"
	"slices"
	"testing"

	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/gitlet"
	"github.com/crewlet/crewlet/internal/gitlet/gitlettest"
	"github.com/crewlet/crewlet/internal/gitlet/query"
)

// A UNIT REPOSITORY'S TOOL READ IS RECORDED — every read the node answered of a
// `visibility: unit` repository, as its asker, by its kind, its bytes the
// answer's encoding — and a memo hit as a miss: the memo is the copy's, shared
// by every asker, so a second seat's read of a page the first one's read cut is
// a read of the repository too. A refused read records nothing, and neither
// does any read of a `visibility: company` repository, which every seat reads
// by declaration, nor the engine's own read, which nobody made.
//
// Mutation: drop the visibility check, and the company repository's read is
// recorded; answer a memo hit at once, as an early return, and the second
// seat's read is missing; record the engine's own read, and the home's facts
// read is recorded as somebody's.
func TestAUnitRepositorysToolReadIsRecorded(t *testing.T) {
	t.Parallel()
	settings := gitlettest.Settings()
	settings.Visibility = gitlet.VisibilityUnit
	x := newFixture(t, gitlettest.SoloOptions{}, settings)
	head := x.commit(map[string]string{"f": "1\n"}, "one")
	repo := x.repo(gitlettest.RepoName)
	rec := &fetches{}
	runs := &recording{}
	s := x.server(0, query.Options{Git: runs.on(x, 0), Roster: company, Fetched: rec.record})

	read := func(who gitlet.Principal) gitlet.GitAnswer {
		t.Helper()
		q := treeQ(repo, gitlet.TreeQuery{Rev: head})
		q.Asker = who
		return mustRead(t, s, q)
	}
	first := read(seat("eng-dev"))
	// THE SAME PAGE again, for another seat: the memo answers it, and no git
	// runs for it.
	ran := len(runs.ran())
	again := read(seat("eng-lead"))
	if more := runs.ran()[ran:]; len(more) != 0 {
		t.Fatalf("the premise: the second seat's read of the same page ran git %v, want the memo's answer", more)
	}
	theirs := read(person)
	outside := treeQ(repo, gitlet.TreeQuery{Rev: head})
	outside.Asker = seat("sales-rep")
	if _, err := s.Read(t.Context(), outside); err == nil {
		t.Fatal("the premise: a seat outside the owning unit was served")
	}
	got := rec.all()
	want := []types.CodeFetched{
		{RepoID: repo, RepoName: gitlettest.RepoName, PrincipalKind: types.CodeReaderAgent, Principal: "eng-dev",
			Seat: "eng-dev", Via: types.FetchedViaTool, Kind: string(gitlet.GitTree), Reads: 1,
			Bytes: int64(encoded(t, first))},
		{RepoID: repo, RepoName: gitlettest.RepoName, PrincipalKind: types.CodeReaderAgent, Principal: "eng-lead",
			Seat: "eng-lead", Via: types.FetchedViaTool, Kind: string(gitlet.GitTree), Reads: 1,
			Bytes: int64(encoded(t, again))},
		{RepoID: repo, RepoName: gitlettest.RepoName, PrincipalKind: types.CodeReaderOperator,
			Principal: person.OperatorID, Seat: person.Seat, Via: types.FetchedViaTool, Kind: string(gitlet.GitTree),
			Reads: 1, Bytes: int64(encoded(t, theirs))},
	}
	if len(got) != len(want) {
		t.Fatalf("the reads recorded are %+v, want the two seats' and the person's", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("read %d is recorded as %+v, want %+v", i, got[i], want[i])
		}
	}

	// A COMPANY REPOSITORY'S READS ARE NOBODY'S AUDIT.
	x.f.Declare(t, gitlet.Declared{Name: "open", Settings: gitlettest.Settings()})
	x.push("open", "main")
	mustRead(t, s, treeQ(x.repo("open"), gitlet.TreeQuery{Rev: head}))
	if after := rec.all(); len(after) != len(want) {
		t.Errorf("a read of a visibility: company repository was recorded: %+v", after[len(want):])
	}

	// THE ENGINE'S OWN READ IS NOBODY'S: the home's read of a revision's
	// facts names no asker, and a read of the unit repository it answers is
	// recorded as nobody's read of it.
	mustRead(t, s, factsQ(repo, head, head))
	if after := rec.all(); len(after) != len(want) {
		t.Errorf("the engine's own read of a visibility: unit repository was recorded: %+v", after[len(want):])
	}
}

// A READ MADE FOR A CHECK IS THE ENGINE'S OWN: a check's box's read of its
// commit through the API names no asker — the box is no principal, and the
// attempt's row that admitted it is its authority — so the Gitlet node judges no
// grant on the `visibility: unit` repository it reads, which a seat outside
// the owning unit is refused, and records it as nobody's read; and it reads
// the attempt's commit alone, a read at another commit refused as it is
// validated, having read nothing.
//
// Mutations: judge a read made for a check by its asker's grant (the kind's
// rule alone in the server's grant step) — refused as nobody, red; record it
// as a read — red; hold no read made for a check to its commit — the later
// commit's tree is answered, red.
func TestAReadMadeForACheckIsTheEnginesOwn(t *testing.T) {
	t.Parallel()
	settings := gitlettest.Settings()
	settings.Visibility = gitlet.VisibilityUnit
	x := newFixture(t, gitlettest.SoloOptions{}, settings)
	head := x.commit(map[string]string{"f": "1\n"}, "one")
	later := x.commit(map[string]string{"f": "2\n"}, "two")
	repo := x.repo(gitlettest.RepoName)
	rec := &fetches{}
	s := x.server(0, query.Options{Roster: company, Fetched: rec.record})
	attempt := gitlet.CheckKey{RepoID: repo, OID: head, Target: "refs/heads/main", Name: "unit", Attempt: 1}
	forCheck := func(rev string) gitlet.GitQuery {
		return gitlet.GitQuery{Kind: gitlet.GitTree, RepoID: repo, Home: true, Check: &attempt,
			Tree: &gitlet.TreeQuery{Rev: rev}}
	}
	outside := treeQ(repo, gitlet.TreeQuery{Rev: head})
	outside.Asker = seat("sales-rep")
	if _, err := s.Read(t.Context(), outside); !errors.Is(err, gitlet.ErrNotPermitted) {
		t.Fatalf("the premise: a seat outside the owning unit read the repository: %v", err)
	}
	a := mustRead(t, s, forCheck(head))
	if a.Tree == nil || !slices.ContainsFunc(a.Tree.Entries, func(e gitlet.TreeEntry) bool { return e.Path == "f" }) {
		t.Fatalf("a read made for the check answered %+v, want its commit's tree", a.Tree)
	}
	if got := rec.all(); len(got) != 0 {
		t.Errorf("a read made for a check was recorded as somebody's: %+v", got)
	}
	if _, err := s.Read(t.Context(), forCheck(later)); !errors.Is(err, gitlet.ErrInvalid) {
		t.Errorf("a read made for the check at another commit answered %v, want gitlet.ErrInvalid", err)
	}
}

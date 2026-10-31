package builtin_test

import (
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/tools"
)

// maintains is a precheck grant that passes every command: the cases here are
// about whom a launch reads for, not what it may push.
func maintains(asker code.Principal, repo code.RepoRef) code.Grant {
	return code.GrantOf(code.Principal{Kind: code.PrincipalOperator, OperatorID: "token:ops", Seat: "founder"},
		repo, nil)
}

// launchTurn is agent-cto's turn as a launch is made in one: a run id that is
// a UUID, as every turn's is, which the run's own branch is named by, and the
// holds every engine-built turn keeps — a launch holds the run's own branch
// on them before anything durable of it exists.
func launchTurn(t *testing.T) *turnctx.Turn {
	t.Helper()
	turn := codeTurn(t)
	turn.RunID = "0190d8f0-1111-7000-8000-0123456789ab"
	turn.Holds = turnctx.NewHolds(nil)
	return turn
}

// A RUN'S REPOSITORIES ARE READ FOR WHOEVER WOKE ITS TURN (M3-decisions U40
// a): a launch's resolution reads as the code tools read — every read of the
// repository and its refs carries the colleague seat the turn acts for, for
// the node that answers to judge that seat's grant too — so a box is never
// given a repository its requester could not read; and a turn a stranger woke
// is refused before anything is read, holding nothing.
//
// Mutation: resolve the repositories as the seat alone (callerOn skipped) →
// the reads carry no ceiling and the stranger's turn reads, red.
func TestARunsRepositoriesAreReadForWhoeverWokeItsTurn(t *testing.T) {
	t.Parallel()
	host := newCodeHost()
	turn := launchTurn(t)
	turn.Requester = "agent-ceo"
	got, err := codeDeps(host).ResolveRunRepos(t.Context(), turn,
		[]builtin.RepoRequest{{Repo: "site", Access: "read"}}, maintains)
	if err != nil {
		t.Fatalf("ResolveRunRepos: %v", err)
	}
	if len(got.Repos) != 1 || got.Repos[0].RepoID != siteID || got.Repos[0].Access != sandbox.RepoRead {
		t.Fatalf("the run is given %+v, want site to read", got.Repos)
	}
	reads, resolves, _ := host.asked()
	if len(reads) == 0 || len(resolves) == 0 {
		t.Fatalf("the launch read %d repositories and resolved %d ref sets, want both", len(reads), len(resolves))
	}
	for _, q := range reads {
		if q.CeilingSeat != "agent-ceo" {
			t.Errorf("a read of %s carried ceiling %q, want the requester agent-ceo", q.Repo, q.CeilingSeat)
		}
	}
	for _, q := range resolves {
		if q.CeilingSeat != "agent-ceo" {
			t.Errorf("a ref resolve of %s carried ceiling %q, want the requester agent-ceo", q.Repo, q.CeilingSeat)
		}
	}

	stranger := newCodeHost()
	woken := launchTurn(t)
	woken.Senders = []types.CanonicalIdentity{{}}
	_, err = codeDeps(stranger).ResolveRunRepos(t.Context(), woken,
		[]builtin.RepoRequest{{Repo: "site", Access: "read"}}, maintains)
	var refused *builtin.RefusedLaunch
	if !errors.As(err, &refused) || tools.RefusalOf(refused.Result) != tools.RefusalForbidden {
		t.Fatalf("a turn a stranger woke answered %v, want a forbidden refusal", err)
	}
	if reads, resolves, gits := stranger.asked(); len(reads)+len(resolves)+len(gits) != 0 {
		t.Errorf("a turn a stranger woke read the code host %d times", len(reads)+len(resolves)+len(gits))
	}
	if ids := woken.Holds.IDs(); len(ids) != 0 {
		t.Errorf("a turn a stranger woke holds %v", ids)
	}
}

// launchRoster is the chart a launch's precheck judges in these cases:
// agent-cto, full_access in eng — site's owning unit — writes site; agent-ceo,
// full_access in another unit, reads it (visibility company) and owns its own
// namespace there; neither leads anything.
type launchRoster struct{}

func (launchRoster) Seat(handle string) (code.RosterSeat, bool) {
	switch handle {
	case "agent-cto":
		return code.RosterSeat{Handle: handle, Tier: code.TierFullAccess, Unit: "eng"}, true
	case "agent-ceo":
		return code.RosterSeat{Handle: handle, Tier: code.TierFullAccess, Unit: "exec"}, true
	}
	return code.RosterSeat{}, false
}
func (launchRoster) Lead(string) string                { return "" }
func (launchRoster) Leads(string) []string             { return nil }
func (launchRoster) Within(unit, ancestor string) bool { return unit == ancestor }

// charted is a launch's precheck grant from launchRoster.
func charted(asker code.Principal, repo code.RepoRef) code.Grant {
	return code.GrantOf(asker, repo, launchRoster{})
}

// A RUN'S PUSHES ARE DECIDED FOR WHOEVER WOKE ITS TURN TOO (M3-decisions U40
// a, its write half for a coding run): in a turn a colleague seat woke, the
// run's grants name that seat for the code host's box admission to hold the
// box to, and the launch prechecks the same rule — `access: write` on a branch
// the colleague could not push is refused naming why, an access left out
// there is read, and the seat's own branch stays write, its own work. A turn
// a person woke, or nobody, names no ceiling and pushes what the seat may.
//
// Mutations: the precheck judged on the seat's grant alone (no Under) → the
// colleague's write of main is given write, red; RunRepos.Ceiling left empty →
// the colleague's turn names nobody, red.
func TestARunsPushesAreDecidedForWhoeverWokeItsTurn(t *testing.T) {
	t.Parallel()
	resolve := func(t *testing.T, requester string, req builtin.RepoRequest) (builtin.RunRepos, error) {
		t.Helper()
		turn := launchTurn(t)
		turn.Requester = requester
		return codeDeps(newCodeHost()).ResolveRunRepos(t.Context(), turn, []builtin.RepoRequest{req}, charted)
	}

	_, err := resolve(t, "agent-ceo", builtin.RepoRequest{Repo: "site", Branch: "main", Access: "write"})
	var refused *builtin.RefusedLaunch
	if !errors.As(err, &refused) || tools.RefusalOf(refused.Result) != tools.RefusalForbidden ||
		!strings.Contains(refused.Result.Output, "this work is for the seat whose ask started its turn") {
		t.Fatalf("a colleague who reads site asked for a write of main: %v, want a forbidden refusal naming "+
			"whom the run works for", err)
	}
	for _, c := range []struct {
		name, requester string
		req             builtin.RepoRequest
		access          sandbox.RepoAccess
		ceiling         string
	}{
		{"main left to the default, for a colleague who reads it", "agent-ceo",
			builtin.RepoRequest{Repo: "site", Branch: "main"}, sandbox.RepoRead, "agent-ceo"},
		{"the seat's own branch, for that colleague", "agent-ceo",
			builtin.RepoRequest{Repo: "site", Access: "write"}, sandbox.RepoWrite, "agent-ceo"},
		{"main, for a person", "founder",
			builtin.RepoRequest{Repo: "site", Branch: "main", Access: "write"}, sandbox.RepoWrite, ""},
		{"main, for nobody", "",
			builtin.RepoRequest{Repo: "site", Branch: "main", Access: "write"}, sandbox.RepoWrite, ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			got, err := resolve(t, c.requester, c.req)
			if err != nil {
				t.Fatalf("ResolveRunRepos: %v", err)
			}
			if len(got.Repos) != 1 || got.Repos[0].Access != c.access || got.Ceiling != c.ceiling {
				t.Errorf("the run is given %+v under ceiling %q, want %s under %q", got.Repos, got.Ceiling,
					c.access, c.ceiling)
			}
		})
	}
}

// A LAUNCH READS EVERY REF OF A REPOSITORY IN ONE QUERY — the run's branch, a
// base named as a ref and the default branch — at the seat's level, one
// barrier a repository; a base given as an id is expanded by one read by
// object id, floored at what the launch had read.
//
// Mutation: a query a ref → two resolves for one repository, red.
func TestALaunchReadsARepositorysRefsInOneQuery(t *testing.T) {
	t.Parallel()
	host := newCodeHost()
	turn := launchTurn(t)
	if _, err := codeDeps(host).ResolveRunRepos(t.Context(), turn,
		[]builtin.RepoRequest{{Repo: "site", Base: "v1", Access: "read"}}, maintains); err != nil {
		t.Fatalf("ResolveRunRepos: %v", err)
	}
	_, resolves, gits := host.asked()
	if len(resolves) != 1 || len(gits) != 0 || !slices.Contains(resolves[0].Refs, "v1") {
		t.Errorf("a launch naming a tag made %d ref queries (%+v) and %d reads by object id, want one query "+
			"naming the tag", len(resolves), resolves, len(gits))
	}

	byID := newCodeHost()
	if _, err := codeDeps(byID).ResolveRunRepos(t.Context(), launchTurn(t),
		[]builtin.RepoRequest{{Repo: "site", Base: mainOID[:9], Access: "read"}}, maintains); err != nil {
		t.Fatalf("ResolveRunRepos: %v", err)
	}
	if _, resolves, gits := byID.asked(); len(resolves) != 1 || len(gits) != 1 || gits[0].Tree == nil ||
		gits[0].Tree.Rev != mainOID[:9] {
		t.Errorf("a launch naming an abbreviated id made %d ref queries and reads by object id %+v, want one "+
			"of each, the read asking the id", len(resolves), gits)
	}
}

// changeAt is site's change number 3, from feature into main, in state, as a
// read of it answers.
func changeAt(state code.ChangeState) *code.ChangeDetail {
	return &code.ChangeDetail{ChangeSummary: code.ChangeSummary{
		ChangeRow: code.ChangeRow{ID: "0190d8f0-4444-7000-8000-0123456789ab", RepoID: siteID, Number: 3,
			Title: "Retry the upload", State: state, SourceRef: "refs/heads/feature", TargetRef: "refs/heads/main",
			Head: featureOID, Revision: 2, Owner: "agent-cto", OwnerKind: code.PartyAgent, Version: 4},
		Repo: "site", Ref: "site!3"}}
}

// hostWithChange is the code host holding site and its change number 3 in
// state, answering a read of any other number not found.
func hostWithChange(state code.ChangeState) *codeHost {
	host := newCodeHost()
	host.changes = func(q code.ReadQuery) (code.ReadAnswer, error) {
		if q.Kind != code.ReadChange || q.Change != 3 {
			return code.ReadAnswer{}, fmt.Errorf("%w: site!%d", code.ErrChangeNotFound, q.Change)
		}
		return code.ReadAnswer{Position: codeAt(12), Level: q.Fresh.Level, Change: changeAt(state)}, nil
	}
	return host
}

// A RUN ON A CHANGE STARTS FROM ITS HEAD (M4-decisions D33): a repository named
// with a `change` — by its number, or its reference — is given the run on the
// change's own source branch, from the head that branch holds, and names the
// change on the run's grant, so its pushes revise it and its collection names
// it; the change is read once, LINEARIZABLE — a run must not start from a
// change a node behind still shows open — and a closed or a merged one is
// refused before a box costs anything, as is a change of another repository
// and one the repository does not hold.
//
// Mutations: start from the default branch rather than the change's — the
// base is main's, red; read the change stale — red; let a merged change
// through — red.
func TestARunOnAChangeStartsFromItsHead(t *testing.T) {
	t.Parallel()
	for _, named := range []string{"3", "site!3"} {
		host := hostWithChange(code.ChangeStateOpen)
		got, err := codeDeps(host).ResolveRunRepos(t.Context(), launchTurn(t),
			[]builtin.RepoRequest{{Repo: "site", Change: named}}, maintains)
		if err != nil {
			t.Fatalf("change %q: ResolveRunRepos: %v", named, err)
		}
		want := sandbox.RunRepo{Repo: "site", RepoID: siteID, Base: featureOID, Branch: "feature",
			Access: sandbox.RepoWrite, Paths: []string{},
			Change: &sandbox.RunChange{ID: changeAt(code.ChangeStateOpen).ID, Number: 3}}
		if len(got.Repos) != 1 || !reflect.DeepEqual(got.Repos[0], want) {
			t.Fatalf("change %q: the run is given %+v, want %+v", named, got.Repos, want)
		}
		reads, resolves, _ := host.asked()
		if n := readsOf(reads, code.ReadChange); n != 1 {
			t.Fatalf("change %q: the launch read the change %d times, want once", named, n)
		}
		for _, q := range reads {
			if q.Kind == code.ReadChange && q.Fresh.Level != statelog.ReadLinearizable {
				t.Errorf("change %q: the change was read %s, want linearizable", named, q.Fresh.Level)
			}
		}
		if len(resolves) != 1 || resolves[0].Refs[0] != "refs/heads/feature" {
			t.Errorf("change %q: the launch resolved %+v, want the change's branch first", named, resolves)
		}
	}

	for name, tc := range map[string]struct {
		host    *codeHost
		req     builtin.RepoRequest
		refusal tools.Refusal
		says    string
	}{
		"a closed change": {hostWithChange(code.ChangeStateClosed),
			builtin.RepoRequest{Repo: "site", Change: "3"}, tools.RefusalConflict, "site!3 is closed"},
		"a merged change": {hostWithChange(code.ChangeStateMerged),
			builtin.RepoRequest{Repo: "site", Change: "3"}, tools.RefusalConflict, "site!3 is merged"},
		"a change it does not hold": {hostWithChange(code.ChangeStateOpen),
			builtin.RepoRequest{Repo: "site", Change: "4"}, tools.RefusalNotFound, ""},
		"another repository's change": {hostWithChange(code.ChangeStateOpen),
			builtin.RepoRequest{Repo: "site", Change: "web!3"}, "", "names a change of repository"},
		"no change": {hostWithChange(code.ChangeStateOpen),
			builtin.RepoRequest{Repo: "site", Change: "three"}, "", "is no change"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			_, err := codeDeps(tc.host).ResolveRunRepos(t.Context(), launchTurn(t), []builtin.RepoRequest{tc.req},
				maintains)
			var refused *builtin.RefusedLaunch
			if !errors.As(err, &refused) || !strings.Contains(refused.Result.Output, tc.says) ||
				(tc.refusal != "" && tools.RefusalOf(refused.Result) != tc.refusal) {
				t.Fatalf("ResolveRunRepos = %v, want a %s refusal saying %q", err, tc.refusal, tc.says)
			}
		})
	}
}

// A LAUNCH RESOLVES A RUN'S ASK TO OPEN A CHANGE (M4-decisions D33): the
// branch it proposes into is the one it names, else the repository's default
// branch, whole on the run's grant beside its title and whether it is a
// draft — resolved in the launch's one ref query, so a target that does not
// exist is refused before a box costs anything — and an ask that proposes the
// run's branch into itself, names a title no change may carry, or comes to a
// run that may not push its branch is refused.
//
// Mutation: leave the target unresolved when none is named — the grant's
// target is empty, red.
func TestALaunchResolvesARunsAskToOpenAChange(t *testing.T) {
	t.Parallel()
	host := newCodeHost()
	got, err := codeDeps(host).ResolveRunRepos(t.Context(), launchTurn(t), []builtin.RepoRequest{
		{Repo: "site", Branch: "feature", OpenChange: &builtin.OpenChangeRequest{Title: "Retry the upload",
			Draft: true}}}, maintains)
	if err != nil {
		t.Fatalf("ResolveRunRepos: %v", err)
	}
	want := &sandbox.OpenChange{Target: "refs/heads/main", Title: "Retry the upload", Draft: true}
	if len(got.Repos) != 1 || !reflect.DeepEqual(got.Repos[0].Open, want) || got.Repos[0].Access != sandbox.RepoWrite {
		t.Fatalf("the run is given %+v, want write and an ask to open %+v", got.Repos, want)
	}
	if _, resolves, _ := host.asked(); len(resolves) != 1 || !slices.Contains(resolves[0].Refs, "refs/heads/main") {
		t.Errorf("the launch resolved %+v, want the target in its one query", resolves)
	}

	for name, tc := range map[string]struct {
		req   builtin.RepoRequest
		grant builtin.GrantFunc
		says  string
	}{
		"a target that does not exist": {builtin.RepoRequest{Repo: "site",
			OpenChange: &builtin.OpenChangeRequest{Target: "release"}}, maintains, "release"},
		"the run's own branch": {builtin.RepoRequest{Repo: "site", Branch: "feature",
			OpenChange: &builtin.OpenChangeRequest{Target: "refs/heads/feature"}}, maintains, "into itself"},
		"a title on two lines": {builtin.RepoRequest{Repo: "site",
			OpenChange: &builtin.OpenChangeRequest{Title: "Retry\nthe upload"}}, maintains, "`title`"},
		"a branch it may not push": {builtin.RepoRequest{Repo: "site", Branch: "main",
			OpenChange: &builtin.OpenChangeRequest{Target: "feature"}},
			func(code.Principal, code.RepoRef) code.Grant { return code.Grant{} }, "may not push"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			_, err := codeDeps(newCodeHost()).ResolveRunRepos(t.Context(), launchTurn(t),
				[]builtin.RepoRequest{tc.req}, tc.grant)
			var refused *builtin.RefusedLaunch
			if !errors.As(err, &refused) || !strings.Contains(refused.Result.Output, tc.says) {
				t.Fatalf("ResolveRunRepos = %v, want a refusal saying %q", err, tc.says)
			}
		})
	}
}

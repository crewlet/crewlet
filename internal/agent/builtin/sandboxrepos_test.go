package builtin_test

import (
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/sandbox"
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

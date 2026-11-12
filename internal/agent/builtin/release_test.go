package builtin_test

import (
	"context"
	"errors"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/delegated"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/gitlet"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/tools"
)

// The release requests' commits: a pull request's head, the merge commits of
// its changes, and the commit a deploy names.
var (
	pullHead     = strings.Repeat("a1", 20)
	movedHead    = strings.Repeat("b2", 20)
	mergeOf41    = strings.Repeat("c3", 20)
	mergeOf42    = strings.Repeat("c4", 20)
	deployCommit = strings.Repeat("d4", 20)
)

// mergeAsked is one merge the fake GitHub was asked for.
type mergeAsked struct {
	seat         string
	ref          builtin.PullRef
	head, method string
}

// fakePulls is the company's GitHub as the release requests read it: one
// repository, acme/web, its pull requests' facts by number, which commits
// hold which, the deploy's checks, how each check run named in a rule stands,
// and every merge it was asked for.
type fakePulls struct {
	mu        sync.Mutex
	facts     map[int]builtin.PullFacts
	factsErr  error
	contains  map[[2]string]bool
	offBranch bool
	checks    []builtin.CheckFact
	// runs are the results of the checks a rule declares, by name; one not
	// here reads missing.
	runs     map[string]string
	merged   []mergeAsked
	mergeErr error
}

// withDeclared is required with every check declared for acme/web's branch
// added as GitHub's wiring judges it, from p's runs.
func (p *fakePulls) withDeclared(required []builtin.CheckFact, branch string,
	declared []delegated.RequiredChecks) []builtin.CheckFact {
	out := slices.Clone(required)
	for _, name := range delegated.DeclaredFor(declared, "acme/web", branch) {
		result := p.runs[name]
		if result == "" {
			result = builtin.CheckMissing
		}
		out = append(out, builtin.CheckFact{Name: name, Result: result, Declared: true})
	}
	return out
}

func (p *fakePulls) Ref(address string) (builtin.PullRef, bool) {
	rest, ok := strings.CutPrefix(address, "https://github.com/acme/web/pull/")
	n, err := strconv.Atoi(rest)
	if !ok || err != nil {
		return builtin.PullRef{}, false
	}
	return builtin.PullRef{Owner: "acme", Repo: "web", Number: n}, true
}

func (p *fakePulls) Repo(address string) (builtin.PullRef, bool) {
	return builtin.PullRef{Owner: "acme", Repo: "web"}, address == "https://github.com/acme/web"
}

func (p *fakePulls) Facts(_ context.Context, _ string, ref builtin.PullRef,
	declared []delegated.RequiredChecks) (builtin.PullFacts, error) {
	if p.factsErr != nil {
		return builtin.PullFacts{}, p.factsErr
	}
	f := p.facts[ref.Number]
	f.Required = p.withDeclared(f.Required, f.Base, declared)
	return f, nil
}

func (p *fakePulls) Contains(_ context.Context, _ string, _ builtin.PullRef, commit, ancestor string) (bool, error) {
	return p.contains[[2]string{commit, ancestor}], nil
}

func (p *fakePulls) OnBranch(context.Context, string, builtin.PullRef, string, string) (bool, error) {
	return !p.offBranch, nil
}

func (p *fakePulls) Checks(_ context.Context, _ string, _ builtin.PullRef, branch, _ string,
	declared []delegated.RequiredChecks) ([]builtin.CheckFact, bool, error) {
	return p.withDeclared(p.checks, branch, declared), true, nil
}

func (p *fakePulls) Merge(_ context.Context, seat string, ref builtin.PullRef, head, method string) (string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.mergeErr != nil {
		return "", p.mergeErr
	}
	p.merged = append(p.merged, mergeAsked{seat: seat, ref: ref, head: head, method: method})
	return mergeOf42, nil
}

// readyPull is acme/web#42 as every condition of a merge holds: open at
// pullHead, its one required check passed, mergeable, approved by a reviewer
// who is neither its author nor the requester, unlabelled, touching ordinary
// source.
func readyPull() builtin.PullFacts {
	yes := true
	return builtin.PullFacts{Title: "Retry the upload", Author: "dev", Open: true, Head: pullHead, Base: "main",
		Mergeable: &yes, MergeableState: "clean", Approvers: []string{"reviewer"},
		Required: []builtin.CheckFact{{Name: "ci", Result: builtin.CheckPassed}}, RequiredRead: true,
		Paths: []string{"web/src/upload.ts"}, PathsComplete: true}
}

// mergedPull is change n merged into main as merge.
func mergedPull(merge string) builtin.PullFacts {
	return builtin.PullFacts{Title: "A change", Author: "dev", Merged: true, MergeCommit: merge, Base: "main",
		Paths: []string{"web/src/upload.ts"}, PathsComplete: true}
}

// releaseDesk is a desk holding sre's call of tool (of act kind) with args,
// put to delegates under a live rule.
func releaseDesk(t *testing.T, tool string, kind delegated.Kind, args map[string]any, delegates ...string) *fakeDesk {
	t.Helper()
	digest, err := sandbox.HeldArgumentsDigest(args)
	if err != nil {
		t.Fatal(err)
	}
	held := &sandbox.HeldCall{ID: "0190f3a8-0000-7000-8000-0000000000f1", LaunchID: "launch-7",
		Server: "crewlet", Tool: tool, Raw: tool, Fixed: true, Approvers: sandbox.HeldApproversRelease,
		Act: string(kind), HeldAt: delegatedAt,
		Delegation: &sandbox.HeldDelegation{Rule: 1, Delegates: delegates, Digest: digest}}
	return &fakeDesk{
		run: sandbox.PendingRun{TurnID: "run-sre", LaunchID: "launch-7", AgentHandle: "sre",
			Status: sandbox.StatusAwaiting, HeldCall: held,
			WorkItem: &types.WorkItem{Backend: types.WorkWorklet, ID: "id-4", Key: "OPS-4"}},
		args: args, delegates: delegates, live: true, full: map[string]bool{},
	}
}

// reviewOver is one call of review_delegated by turn's seat over desk, with
// deps' code host and GitHub.
func reviewOver(t *testing.T, deps builtin.Deps, desk *fakeDesk, turn *turnctx.Turn,
	args map[string]any) tools.Result {
	t.Helper()
	deps.Delegated = builtin.DelegatedDeps{Desk: desk, Now: func() time.Time { return delegatedAt }}
	reg := tools.NewRegistry()
	if _, err := builtin.Register(reg, deps); err != nil {
		t.Fatal(err)
	}
	entry, ok := reg.Lookup(builtin.ReviewDelegatedTool)
	if !ok {
		t.Fatal("review_delegated is not registered")
	}
	res, err := asSeatCallable(t, tools.Innermost(entry.Tool)).CallForTurn(asExecutor(turn.Handle()), turn, args)
	if err != nil {
		t.Fatal(err)
	}
	return res
}

// pullMergeArgs is ask_to_merge's arguments for acme/web#42 at head.
func pullMergeArgs(head string) map[string]any {
	return map[string]any{"change": "https://github.com/acme/web/pull/42", "head": head,
		"reason": "the release needs it"}
}

// A delegate's allow of a pull request's merge stands only where GitHub shows,
// when the verdict is given, every condition the engine holds a merge to: the
// head asked about, open and no draft, every required check passed on it,
// mergeable and not held back by its branch's rules, approved by somebody
// other than its author and the requester, no sensitive label, and no path no
// delegate allows. Anything else, a fact it cannot read included, goes to
// people with the condition named.
//
// Mutation: drop any one condition, read an unread check as passed, or let an
// unreadable pull request through, and a merge nobody should have allowed is
// carried out by a delegate alone.
func TestAMergeIsAllowedOnlyWhereGitHubShowsEveryCondition(t *testing.T) {
	t.Parallel()
	_, o, _ := sshFixture(t)
	for _, c := range []struct {
		name  string
		set   func(*builtin.PullFacts, *fakePulls, *fakeDesk)
		unmet string
	}{
		{"every condition holds", func(*builtin.PullFacts, *fakePulls, *fakeDesk) {}, ""},
		{"the head moved", func(f *builtin.PullFacts, _ *fakePulls, _ *fakeDesk) { f.Head = movedHead },
			delegated.UnmetHeadMoved},
		{"closed", func(f *builtin.PullFacts, _ *fakePulls, _ *fakeDesk) { f.Open = false }, delegated.UnmetNotOpen},
		{"a draft", func(f *builtin.PullFacts, _ *fakePulls, _ *fakeDesk) { f.Draft = true }, delegated.UnmetNotReady},
		{"a required check failed", func(f *builtin.PullFacts, _ *fakePulls, _ *fakeDesk) {
			f.Required = []builtin.CheckFact{{Name: "ci", Result: builtin.CheckFailed}}
		}, delegated.UnmetChecks},
		{"a required check nobody could read", func(f *builtin.PullFacts, _ *fakePulls, _ *fakeDesk) {
			f.Required = []builtin.CheckFact{{Name: "ci", Result: "unread"}}
		}, delegated.UnmetChecks},
		{"no check required", func(f *builtin.PullFacts, _ *fakePulls, _ *fakeDesk) { f.Required = nil },
			delegated.UnmetNoChecks},
		{"the required checks unread", func(f *builtin.PullFacts, _ *fakePulls, _ *fakeDesk) {
			f.RequiredRead = false
		}, delegated.UnmetUnread},
		{"mergeability still computed", func(f *builtin.PullFacts, _ *fakePulls, _ *fakeDesk) { f.Mergeable = nil },
			delegated.UnmetMergeable},
		{"a conflict", func(f *builtin.PullFacts, _ *fakePulls, _ *fakeDesk) {
			no := false
			f.Mergeable, f.MergeableState = &no, "dirty"
		}, delegated.UnmetMergeable},
		{"held back by its branch's rules", func(f *builtin.PullFacts, _ *fakePulls, _ *fakeDesk) {
			f.MergeableState = "blocked"
		}, delegated.UnmetNotReady},
		{"approved only by the requester", func(f *builtin.PullFacts, _ *fakePulls, _ *fakeDesk) {
			f.Approvers = []string{"sre"}
		}, delegated.UnmetApproval},
		{"labelled security", func(f *builtin.PullFacts, _ *fakePulls, _ *fakeDesk) {
			f.Labels = []string{"Security"}
		}, delegated.OverruledLabel},
		{"a workflow", func(f *builtin.PullFacts, _ *fakePulls, _ *fakeDesk) {
			f.Paths = append(f.Paths, ".github/workflows/ci.yml")
		}, delegated.PathCIPipelines},
		{"a path the rule adds", func(f *builtin.PullFacts, _ *fakePulls, d *fakeDesk) {
			f.Paths, d.release.EscalatePaths = append(f.Paths, "billing/rates.go"), []string{"billing/**"}
		}, delegated.PathRule},
		{"more paths than GitHub lists", func(f *builtin.PullFacts, _ *fakePulls, _ *fakeDesk) {
			f.PathsComplete = false
		}, delegated.UnmetUnclassified},
		{"GitHub cannot be read", func(_ *builtin.PullFacts, p *fakePulls, _ *fakeDesk) {
			p.factsErr = errors.New("github: 502")
		}, delegated.UnmetUnread},
	} {
		facts := readyPull()
		pulls := &fakePulls{}
		desk := releaseDesk(t, builtin.AskToMergeTool, delegated.KindPRMerge, pullMergeArgs(pullHead), "dev")
		c.set(&facts, pulls, desk)
		pulls.facts = map[int]builtin.PullFacts{42: facts}
		res := reviewOver(t, builtin.Deps{Pulls: pulls}, desk, sshTurn(o, "dev", "", nil),
			verdict(desk, delegated.Allow, delegated.High))
		switch {
		case c.unmet == "" && (len(desk.answers) != 1 || desk.answers[0].choice != sandbox.HeldOnce):
			t.Errorf("%s: answered %q and escalated %+v, want the merge allowed", c.name, res.Output, desk.escalated)
		case c.unmet != "" && !putToPeople(desk, c.unmet):
			t.Errorf("%s: answered %q, carried out %+v and escalated %+v, want it put to people for %s",
				c.name, res.Output, desk.answers, desk.escalated, c.unmet)
		}
	}
}

// A merge or deploy on a branch GitHub requires no check of, a private
// repository on a free plan, stands on the checks its rule declares for that
// repository and branch: every one passed on the commit decided, or it goes
// to people, saying which and that the rule declared it. A rule declaring
// none for the branch leaves it requiring nothing, which still goes to people.
//
// Mutation: drop the rule's checks from the GitHub reads, and every such
// merge and deploy goes to people as requiring nothing; judge them as passed
// without reading them, and a check CI never ran lets a delegate merge.
func TestAFreePlanReleaseStandsOnTheChecksItsRuleDeclares(t *testing.T) {
	t.Parallel()
	_, o, _ := sshFixture(t)
	main := []delegated.RequiredChecks{{Repo: "Acme/Web", Branch: "main", Checks: []string{"test", "lint"}}}
	for _, c := range []struct {
		name     string
		declared []delegated.RequiredChecks
		runs     map[string]string
		unmet    string
	}{
		{"every declared check passed", main,
			map[string]string{"test": builtin.CheckPassed, "lint": builtin.CheckPassed}, ""},
		{"a declared check missing", main, map[string]string{"test": builtin.CheckPassed}, delegated.UnmetChecks},
		{"a declared check skipped", main,
			map[string]string{"test": builtin.CheckPassed, "lint": "inconclusive"}, delegated.UnmetChecks},
		{"checks declared for another branch", []delegated.RequiredChecks{{Repo: "acme/web", Branch: "release",
			Checks: []string{"test"}}}, map[string]string{"test": builtin.CheckPassed}, delegated.UnmetNoChecks},
		{"no check declared", nil, nil, delegated.UnmetNoChecks},
	} {
		for _, kind := range []delegated.Kind{delegated.KindPRMerge, delegated.KindDeploy} {
			name := c.name + " (" + string(kind) + ")"
			facts := readyPull()
			facts.Required = nil
			pulls := &fakePulls{runs: c.runs, facts: map[int]builtin.PullFacts{42: facts,
				41: mergedPull(mergeOf41)}}
			tool, args := builtin.AskToMergeTool, pullMergeArgs(pullHead)
			if kind == delegated.KindDeploy {
				pulls.facts[42] = mergedPull(mergeOf42)
				pulls.contains = map[[2]string]bool{{deployCommit, mergeOf41}: true, {deployCommit, mergeOf42}: true}
				tool, args = builtin.AskToDeployTool, deployArgs()
			}
			desk := releaseDesk(t, tool, kind, args, "dev")
			desk.release.RequiredChecks = c.declared
			res := reviewOver(t, builtin.Deps{Pulls: pulls}, desk, sshTurn(o, "dev", "", nil),
				verdict(desk, delegated.Allow, delegated.High))
			switch {
			case c.unmet == "" && (len(desk.answers) != 1 || desk.answers[0].choice != sandbox.HeldOnce):
				t.Errorf("%s: answered %q and escalated %+v, want it allowed", name, res.Output, desk.escalated)
			case c.unmet != "" && !putToPeople(desk, c.unmet):
				t.Errorf("%s: answered %q, carried out %+v and escalated %+v, want it put to people for %s",
					name, res.Output, desk.answers, desk.escalated, c.unmet)
			case c.unmet == delegated.UnmetChecks:
				if said := strings.Join(desk.escalated[0].Engine, " "); !strings.Contains(said, "lint (") ||
					!strings.Contains(said, "declared by the rule") {
					t.Errorf("%s: escalated saying %q, want lint named as declared by the rule", name, said)
				}
			}
		}
	}
}

// deployArgs is ask_to_deploy's arguments for deployCommit of acme/web,
// shipping 41 and 42.
func deployArgs() map[string]any {
	return map[string]any{"repo": "https://github.com/acme/web", "commit": deployCommit,
		"changes": []any{"41", "42"}, "environment": "production",
		"rollback": "Redeploy the previous release tag.", "reason": "the release train is green"}
}

// A delegate's allow of a deploy stands only where every change it names has
// merged into one branch, the commit holds each and is on that branch, every
// check the branch requires passed on the commit, and no change touches a path
// no delegate allows.
//
// Mutation: drop any one condition and a deploy of unmerged or untested code
// is approved by a delegate alone.
func TestADeployIsAllowedOnlyWhereItsChangesMergedAndItsCommitIsGreen(t *testing.T) {
	t.Parallel()
	_, o, _ := sshFixture(t)
	for _, c := range []struct {
		name  string
		set   func(*fakePulls)
		unmet string
	}{
		{"every condition holds", func(*fakePulls) {}, ""},
		{"a change not merged", func(p *fakePulls) {
			f := p.facts[41]
			f.Merged = false
			p.facts[41] = f
		}, delegated.UnmetNotMerged},
		{"a change the commit does not hold", func(p *fakePulls) {
			delete(p.contains, [2]string{deployCommit, mergeOf41})
		}, delegated.UnmetNotContained},
		{"changes merged into two branches", func(p *fakePulls) {
			f := p.facts[42]
			f.Base = "release"
			p.facts[42] = f
		}, delegated.UnmetNotContained},
		{"a commit off the branch", func(p *fakePulls) { p.offBranch = true }, delegated.UnmetNotContained},
		{"a required check failed on the commit", func(p *fakePulls) {
			p.checks = []builtin.CheckFact{{Name: "ci", Result: builtin.CheckFailed}}
		}, delegated.UnmetChecks},
		{"no check required", func(p *fakePulls) { p.checks = nil }, delegated.UnmetNoChecks},
		{"a migration", func(p *fakePulls) {
			f := p.facts[42]
			f.Paths = []string{"db/migrations/0042_drop.sql"}
			p.facts[42] = f
		}, delegated.PathMigrations},
	} {
		pulls := &fakePulls{
			facts:    map[int]builtin.PullFacts{41: mergedPull(mergeOf41), 42: mergedPull(mergeOf42)},
			contains: map[[2]string]bool{{deployCommit, mergeOf41}: true, {deployCommit, mergeOf42}: true},
			checks:   []builtin.CheckFact{{Name: "ci", Result: builtin.CheckPassed}},
		}
		c.set(pulls)
		desk := releaseDesk(t, builtin.AskToDeployTool, delegated.KindDeploy, deployArgs(), "dev")
		res := reviewOver(t, builtin.Deps{Pulls: pulls}, desk, sshTurn(o, "dev", "", nil),
			verdict(desk, delegated.Allow, delegated.High))
		switch {
		case c.unmet == "" && (len(desk.answers) != 1 || desk.answers[0].choice != sandbox.HeldOnce):
			t.Errorf("%s: answered %q and escalated %+v, want the deploy allowed", c.name, res.Output, desk.escalated)
		case c.unmet != "" && !putToPeople(desk, c.unmet):
			t.Errorf("%s: answered %q, carried out %+v and escalated %+v, want it put to people for %s",
				c.name, res.Output, desk.answers, desk.escalated, c.unmet)
		}
	}
}

// releaseTool is the release tool name over deps, as a seat's surface holds it.
func releaseTool(t *testing.T, deps builtin.Deps, name string) tools.SeatCallable {
	t.Helper()
	reg := tools.NewRegistry()
	if _, err := builtin.Register(reg, deps); err != nil {
		t.Fatal(err)
	}
	entry, ok := reg.Lookup(name)
	if !ok {
		t.Fatalf("%s is not registered", name)
	}
	if !entry.ApprovalFixed {
		t.Fatalf("%s does not ask before every call", name)
	}
	return asSeatCallable(t, tools.Innermost(entry.Tool))
}

// An approved merge merges exactly the head it was asked about, as the
// requesting seat, once; a call carrying no approval merges nothing, and a
// head GitHub says moved is refused, not merged.
//
// Mutation: merge without the approval, at a head other than the one asked
// about, or as another seat, and an approval carries out something nobody
// approved.
func TestAnApprovedMergeMergesExactlyItsHeadAsTheSeat(t *testing.T) {
	t.Parallel()
	_, o, _ := sshFixture(t)
	pulls := &fakePulls{facts: map[int]builtin.PullFacts{42: readyPull()}}
	merge := releaseTool(t, builtin.Deps{Pulls: pulls}, builtin.AskToMergeTool)
	turn := sshTurn(o, "sre", "", nil)

	bare, err := merge.CallForTurn(asSeat("sre"), turn, pullMergeArgs(pullHead))
	if err != nil || !bare.Failed || len(pulls.merged) != 0 {
		t.Fatalf("an unapproved call answered %q (%v) and merged %+v, want a refusal and nothing merged",
			bare.Output, err, pulls.merged)
	}
	approved := tools.WithApproval(asSeat("sre"), tools.Approval{By: "ops-lead", HeldID: "held-9"})
	got, err := merge.CallForTurn(approved, turn, pullMergeArgs(pullHead))
	want := mergeAsked{seat: "sre", ref: builtin.PullRef{Owner: "acme", Repo: "web", Number: 42}, head: pullHead,
		method: "merge"}
	if err != nil || got.Failed || len(pulls.merged) != 1 || pulls.merged[0] != want {
		t.Fatalf("an approved call answered %q (%v) and merged %+v, want %+v", got.Output, err, pulls.merged, want)
	}
	pulls.mergeErr = builtin.ErrPullMoved
	moved, _ := merge.CallForTurn(approved, turn, pullMergeArgs(pullHead))
	if !moved.Failed || tools.RefusalOf(moved) != tools.RefusalConflict {
		t.Fatalf("a head GitHub says moved answered %q, want a conflict", moved.Output)
	}
}

// A deploy's approval is exactly what was asked: the commit, the environment
// and the changes it was approved for, said back to the run; and a call whose
// environment, rollback or changes are not what a deploy names is refused
// before anybody is asked.
//
// Mutation: accept a call with no rollback, or more changes than a deploy
// names, and a person is asked to approve a deploy nobody can undo or read.
func TestADeployApprovalIsExactlyWhatWasAsked(t *testing.T) {
	t.Parallel()
	_, o, _ := sshFixture(t)
	deploy := releaseTool(t, builtin.Deps{Pulls: &fakePulls{}}, builtin.AskToDeployTool)
	turn := sshTurn(o, "sre", "", nil)
	approved := tools.WithApproval(asSeat("sre"), tools.Approval{By: "ops-lead", HeldID: "held-10"})
	got, err := deploy.CallForTurn(approved, turn, deployArgs())
	if err != nil || got.Failed || !strings.Contains(got.Output, deployCommit) ||
		!strings.Contains(got.Output, "production") || !strings.Contains(got.Output, "ops-lead") {
		t.Fatalf("an approved deploy answered %q (%v), want the commit, the environment and the approver", got.Output,
			err)
	}
	many := make([]any, 21)
	for i := range many {
		many[i] = strconv.Itoa(i + 1)
	}
	for name, set := range map[string]func(map[string]any){
		"an environment with spaces": func(a map[string]any) { a["environment"] = "prod and staging" },
		"no rollback":                func(a map[string]any) { a["rollback"] = " " },
		"twenty-one changes":         func(a map[string]any) { a["changes"] = many },
		"a commit by branch name":    func(a map[string]any) { a["commit"] = "main" },
		"another repository":         func(a map[string]any) { a["repo"] = "https://github.com/other/web" },
	} {
		args := deployArgs()
		set(args)
		if res, _ := deploy.CallForTurn(approved, turn, args); !res.Failed {
			t.Errorf("%s: answered %q, want it refused", name, res.Output)
		}
	}
}

// A Gitlet change's merge is held to its target's own gate as Gitlet judges it:
// an unmet precondition, a target that requires no check, and a head nobody
// but its authors approved each put a delegate's allow to people; a change
// whose gate is met and whose head another reviewer approved is allowed.
//
// Mutation: read the readiness as met whatever it says, count an author's own
// approval, or take a target that requires no check as passed, and a delegate
// merges what Gitlet's own gate holds back.
func TestAGitletMergeIsHeldToItsTargetsGate(t *testing.T) {
	t.Parallel()
	args := map[string]any{"change": "site!7", "head": revHead(3), "reason": "ready to ship"}

	held := siteChange()
	host := newChangeHost(held)
	host.git = diffOf(siteChangeFiles()...)
	turn := codeTurn(t)
	desk := releaseDesk(t, builtin.AskToMergeTool, delegated.KindPRMerge, args, turn.Handle())
	reviewOver(t, builtin.Deps{Code: writeDeps(host, &codeWrites{})}, desk, turn,
		verdict(desk, delegated.Allow, delegated.High))
	for _, word := range []string{delegated.UnmetNotReady, delegated.UnmetNoChecks, delegated.UnmetApproval} {
		if !putToPeople(desk, word) {
			t.Errorf("site!7 held back by its gate: carried out %+v and escalated %+v, want it put to people "+
				"for %s", desk.answers, desk.escalated, word)
		}
	}

	ready := siteChange()
	ready.detail.Readiness = &gitlet.Readiness{Met: 2, Total: 2, Preconditions: []gitlet.Precondition{
		{Kind: gitlet.PreconditionCheck, State: gitlet.PreconditionMet, Detail: "unit passed"},
		{Kind: gitlet.PreconditionMergeable, State: gitlet.PreconditionMet, Detail: "merges cleanly"}}}
	ready.reviews = []gitlet.ReviewRow{
		{ID: "r-own", Revision: 3, Reviewer: "agent-ceo", ReviewerKind: gitlet.PartyAgent, Verdict: gitlet.VerdictApprove},
		{ID: "r-qa", Revision: 3, Reviewer: "agent-qa", ReviewerKind: gitlet.PartyAgent, Verdict: gitlet.VerdictApprove}}
	host = newChangeHost(ready)
	host.git = diffOf(siteChangeFiles()...)
	desk = releaseDesk(t, builtin.AskToMergeTool, delegated.KindPRMerge, args, turn.Handle())
	res := reviewOver(t, builtin.Deps{Code: writeDeps(host, &codeWrites{})}, desk, turn,
		verdict(desk, delegated.Allow, delegated.High))
	if len(desk.answers) != 1 || desk.answers[0].choice != sandbox.HeldOnce {
		t.Fatalf("a ready site!7 answered %q and escalated %+v, want the merge allowed", res.Output, desk.escalated)
	}

	ownOnly := siteChange()
	ownOnly.detail.Readiness = ready.detail.Readiness
	ownOnly.reviews = ready.reviews[:1]
	host = newChangeHost(ownOnly)
	host.git = diffOf(siteChangeFiles()...)
	desk = releaseDesk(t, builtin.AskToMergeTool, delegated.KindPRMerge, args, turn.Handle())
	reviewOver(t, builtin.Deps{Code: writeDeps(host, &codeWrites{})}, desk, turn,
		verdict(desk, delegated.Allow, delegated.High))
	if !putToPeople(desk, delegated.UnmetApproval) || slices.ContainsFunc(desk.escalated[0].Overruled,
		func(w string) bool { return w == delegated.UnmetNotReady }) {
		t.Fatalf("site!7 approved only by its author escalated %+v, want it put to people for no_approval alone",
			desk.escalated)
	}
}

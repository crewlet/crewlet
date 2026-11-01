package engine

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/tools"
)

// THE REF LIFECYCLE THROUGH A NODE'S OWN DUTY (M3-boxes §10.6.6, C9b's engine
// cases): what a live turn holds from the code host's idle-branch expiry,
// asked of the real code-retention pass a node builds — its listing, its
// proposals to the repository's committer through the estate router, and the
// committer's decide — over the real code host's rows. The turns are the
// engine's own: its launcher resolving a run's repositories, and the seat's
// code tools, each through the node's code seam (CodeToolDeps); and what the
// pass asks of the live turns is the engine's own holds reader over the run
// store the launches wrote and the node's registry of running turns.
//
// THE BOXES ARE THE LAUNCH RIG'S, never the node's own sandbox runtime: the
// rig's coordinator is never started, so a parked run stays parked until a
// case ends it, where a node's waiter would poll a box its provider never
// made and end the run under the case. So the run store the pass reads is the
// rig's, through the reader a node builds ([holdsReader]); that the node's
// duty reads its own store through [Engine.codeHolds] is
// TestTheRetentionDutyExpiresAnIdleSeatBranchNoRunHolds's.
//
// THE CLOCK IS THE PASS'S, and where a case needs it the tools': a branch's
// broker instant cannot be moved, so a fortnight passes on the clocks that
// judge it (code.Retention.Now, builtin.CodeDeps.Now).

// refLifecycleDoc is a company hosting web and api, both owned by the
// Engineering unit, whose two seats write code at full_access and run it in
// a fake remote cell.
const refLifecycleDoc = `
name: Acme
providers:
  sandbox:
    fake: true
  llm:
    zulu:
      model: claude-sonnet-5
      auth: {mode: api-key, api_key: "sk-ant-fake-ref-lifecycle-key"}
units:
  - name: Engineering
    project: ENG
    roles:
      - name: SWE
        handle: swe
        llm: zulu
        code: {access: full_access}
        sandbox: {enabled: true}
      - name: QA
        handle: qa
        llm: zulu
        code: {access: full_access}
        sandbox: {enabled: true}
code:
  native: true
  repos:
    - name: web
      owner: {unit: Engineering}
    - name: api
      owner: {unit: Engineering}
`

// refLifecycle is one node hosting the company's code and the launch rig its
// seats' runs are launched on.
type refLifecycle struct {
	node     *Engine
	rig      *reposRig
	web, api *code.RepoDetail
	// turns is the node's registry of running turns, as its dispatch keeps
	// it: a turn's frame registers its holds while it runs.
	turns *runningTurns
}

func newRefLifecycle(t *testing.T) *refLifecycle {
	t.Helper()
	node := newSandboxNode(t, parseCompany(t, refLifecycleDoc))
	servesCodeReads(t, node)
	reconciled(t, node)
	return &refLifecycle{node: node, rig: newReposRig(t, refLifecycleDoc, "https://git.example.com/box/git"),
		web: mustRepo(t, node, "web"), api: mustRepo(t, node, "api"), turns: &node.turnHolds}
}

// deps are the seat tools' seams on the node, their clock skew ahead of the
// wall clock.
func (lc *refLifecycle) deps(skew time.Duration) builtin.CodeDeps {
	deps := CodeToolDeps(lc.node)
	if skew != 0 {
		deps.Now = func() time.Time { return time.Now().Add(skew) }
	}
	return deps
}

// launch launches turn's run on the node's code seam with repos, and answers
// its row.
func (lc *refLifecycle) launch(t *testing.T, turn *turnctx.Turn, repos ...builtin.RepoRequest) sandbox.PendingRun {
	t.Helper()
	l := &launcher{engine: lc.rig.e, code: lc.deps(0)}
	if _, err := l.Launch(t.Context(), turn, builtin.SandboxRequest{Brief: "fix the failing test",
		Repos: repos}); err != nil {
		t.Fatalf("Launch: %v", err)
	}
	row, found, err := lc.rig.pending.Get(t.Context(), turn.RunID)
	if err != nil || !found {
		t.Fatalf("the launched run's row: found %v, %v", found, err)
	}
	return row
}

// seatPush records seat's earlier turn's write of ref on repo: an engine-built
// push of the seat's, creating ref at its broker instant.
func (lc *refLifecycle) seatPush(t *testing.T, seat string, repo *code.RepoDetail, ref, oid string) {
	t.Helper()
	push := code.Proposal{PushID: uuid.Must(uuid.NewV7()).String(), RepoID: repo.ID,
		Principal: code.Principal{Kind: code.PrincipalAgent, Seat: seat,
			Run: code.RunProvenance{TurnID: statelog.NewOpID(time.Now(), "")}},
		Via: code.ViaEngine, Node: lc.node.id, ReceivedAt: time.Now().UTC(),
		Commands: []code.Command{{Ref: ref, Old: code.ZeroOID, New: oid}}}
	res, err := lc.node.router.Code().Propose(t.Context(), push)
	if err != nil || res.Outcome != statelog.OutcomeApplied || res.Answer.Commands[0].Result != code.ResultOK {
		t.Fatalf("%s's earlier write of %s = %+v, %v", seat, ref, res, err)
	}
}

// pass is the node's code-retention duty's pass on the clock at, its holds
// read as the node reads them, over the rig's run store and the node's
// running turns — or by holds, where a case wraps that reader.
func (lc *refLifecycle) pass(t *testing.T, at time.Time,
	holds func(context.Context, []string) (map[string]bool, error)) code.RetentionReport {
	t.Helper()
	d := lc.node.newCodeRetentionDuty(lc.node.writeSide().code, lc.node.native.Load().codeReader,
		lc.node.core.Load().nodeID)
	d.pass.Now = func() time.Time { return at }
	d.pass.Holds = lc.liveHolds
	if holds != nil {
		d.pass.Holds = holds
	}
	report, err := d.pass.Pass(t.Context())
	if err != nil || len(report.ExpiryFailed) != 0 || report.ExpiryUnread != "" {
		t.Fatalf("the pass = %+v, %v", report, err)
	}
	return report
}

// liveHolds is the node's reader of the live turns' holds over the rig's run
// store and the node's running turns.
func (lc *refLifecycle) liveHolds(ctx context.Context, keys []string) (map[string]bool, error) {
	return holdsReader{runs: lc.rig.pending, leases: listedLeases{}, self: lc.turns, budget: holdsAskBudget}.
		holds(ctx, keys)
}

// exists reports whether repo still holds ref, read linearizably.
func (lc *refLifecycle) exists(t *testing.T, repo *code.RepoDetail, ref string) bool {
	t.Helper()
	answer, err := lc.node.router.Code().ResolveRef(t.Context(), code.RefQuery{Repo: repo.Name,
		Refs: []string{ref}, Fresh: statelog.Freshness{Level: statelog.ReadLinearizable},
		Asker: code.Principal{Kind: code.PrincipalOperator, OperatorID: "op-1", Seat: "swe"}})
	if err != nil || len(answer.Refs) != 1 {
		t.Fatalf("resolve %s of %s: %+v, %v", ref, repo.Name, answer, err)
	}
	return answer.Refs[0].Missing == ""
}

// eng42 is the seat branch a turn on ENG-42 works on.
const eng42 = "refs/heads/seats/swe/ENG-42"

// parkRelaunched drives the relaunch scenario: swe's eng42 on web was last
// pushed by an earlier turn; a native executor on ENG-42 calls run_sandbox
// with web, its box checking the branch out and pushing nothing, and parks;
// answered, the resumed executor relaunches with api alone — the row's grants
// no longer name web — and parks again, its frame returning. It answers the
// run's turn id.
func (lc *refLifecycle) parkRelaunched(t *testing.T) string {
	t.Helper()
	lc.seatPush(t, "swe", lc.web, eng42, oidAt(42))
	item := &types.WorkItem{Backend: types.WorkNative, ID: "task-42", Key: "ENG-42"}
	turn := lc.rig.turn(t, "swe", item)
	frame := lc.turns.running(turn.Holds)
	first := lc.launch(t, turn, builtin.RepoRequest{Repo: lc.web.Name})
	frame() // the turn parks, and its frame returns
	resumed := &turnctx.Turn{RunID: turn.RunID, Seat: turn.Seat, Org: lc.rig.c.Org,
		Lookups: turnctx.NewLookups(), Holds: lc.rig.e.rowHolds(first), WorkItem: item}
	done := lc.turns.running(resumed.Holds)
	again := lc.launch(t, resumed, builtin.RepoRequest{Repo: lc.api.Name})
	done() // and parks again
	if len(again.Repos) != 1 || again.Repos[0].RepoID != lc.api.ID {
		t.Fatalf("the relaunch's row grants %+v, want api alone", again.Repos)
	}
	return turn.RunID
}

// A BRANCH AN EARLIER LAUNCH GRANTED IS HELD, through the node's own pass: a
// run's first launch was given web on swe's seats/swe/ENG-42, which an earlier
// turn pushed; its relaunch was given api alone, so the row's grants no
// longer name web; a fortnight and an hour after that push, with no turn
// running, the pass keeps the branch — held from the row, which kept every
// ref either launch resolved.
//
// Mutation: resolveRunRepos skipping HoldRef → the branch expired under the
// second park, red.
func TestABranchAnEarlierLaunchGrantedIsHeld(t *testing.T) {
	t.Parallel()
	lc := newRefLifecycle(t)
	lc.parkRelaunched(t)
	report := lc.pass(t, time.Now().Add(code.RefExpiry+time.Hour), nil)
	if report.RefsExpired != 0 || report.RefsHeld != 1 {
		t.Errorf("the pass under the second park = %+v, want %s held and nothing expired", report, eng42)
	}
	if !lc.exists(t, lc.web, eng42) {
		t.Errorf("web's %s was expired under a parked run whose first launch was granted it", eng42)
	}
}

// A BRANCH IS EXPIRED ON TIME ONCE ITS RUN ENDS: the run of the relaunch case
// ends — its ending decided and its row deleted — and nothing holds the
// branch any more; a pass on a clock short of the expiry window keeps it (not
// yet due), and the next pass past the window expires it. So a branch is
// never expired before RefExpiry, and is expired by the first pass after its
// run ends that finds it due: within one RetentionInterval, the duty's tick.
//
// Mutation: the registry never forgetting a turn that returned → the resumed
// turn's frame still reads the branch held, never expired, red.
func TestABranchIsExpiredOnTimeOnceItsRunEnds(t *testing.T) {
	t.Parallel()
	lc := newRefLifecycle(t)
	turnID := lc.parkRelaunched(t)
	ending, decided, err := lc.rig.pending.DecideEnding(t.Context(), turnID, sandbox.Decision{
		License: sandbox.License{WhileIn: sandbox.Active, Launch: sandbox.EveryLaunch}})
	if err != nil || !decided || ending.Ending == nil {
		t.Fatalf("decide the run's ending: %v, %v", decided, err)
	}
	if _, finished, err := lc.rig.pending.Finish(t.Context(), turnID, ending.Ending.ID); err != nil || !finished {
		t.Fatalf("finish the run: %v, %v", finished, err)
	}
	if report := lc.pass(t, time.Now().Add(code.RefExpiry-time.Hour), nil); report.RefsExpired != 0 ||
		!lc.exists(t, lc.web, eng42) {
		t.Fatalf("a pass an hour short of the window = %+v; want %s kept, not yet due", report, eng42)
	}
	report := lc.pass(t, time.Now().Add(code.RefExpiry+time.Hour), nil)
	if report.RefsExpired != 1 || report.RefsHeld != 0 {
		t.Errorf("the first pass past the window after the run ended = %+v, want %s expired", report, eng42)
	}
	if lc.exists(t, lc.web, eng42) {
		t.Errorf("web's %s outlived the run that held it by a pass past its window", eng42)
	}
}

// A BRANCH A PARKED RUN ONLY READ IS HELD: qa's seats/qa/fixtures on web was
// last pushed 337 hours ago by the pass's clock — due, and older than
// code.RefHoldRecordAfter. The pass reads its page and then the live holds,
// and only once that read has answered (nothing held) does a native executor
// of another seat, swe, resolve the branch with code_resolve — the one hold
// the pass cannot have seen, which the read records on the code log because
// the branch is that old. The pass proposes, and the committer keeps the
// branch: the expiry is answered stale, the hold recorded after the page
// listed it (refs_kept 1). The executor then calls run_sandbox naming no
// repository and parks, its frame returning: every later pass keeps the
// branch from the row (refs_held 1).
//
// code_resolve rather than code_read: the case's commits are rows with no
// objects, and a read of a file would fail its git read after the hold it
// took; the hold is resolveRef's either way.
//
// Mutations: skip the hold record → the first pass deletes the branch, red;
// drop ExpiryRule's held term → the same, red; read the holds before the page
// → the record lands below the page's Seen and the branch is deleted, red.
func TestABranchAParkedRunOnlyReadIsHeld(t *testing.T) {
	t.Parallel()
	lc := newRefLifecycle(t)
	const fixtures = "refs/heads/seats/qa/fixtures"
	lc.seatPush(t, "qa", lc.web, fixtures, oidAt(7))
	skew := 337 * time.Hour
	at := time.Now().Add(skew)

	turn := lc.rig.turn(t, "swe", nil)
	var resolve tools.SeatCallable
	for _, tool := range builtin.CodeTools(lc.deps(skew)) {
		if tool.Name() == builtin.CodeResolveTool {
			resolve, _ = tool.(tools.SeatCallable)
		}
	}
	if resolve == nil {
		t.Fatalf("the node's code tools serve no seat-callable %s", builtin.CodeResolveTool)
	}
	frame := lc.turns.running(turn.Holds)
	read := false
	report := lc.pass(t, at, func(ctx context.Context, keys []string) (map[string]bool, error) {
		held, err := lc.liveHolds(ctx, keys)
		if !read {
			read = true
			got, callErr := resolve.CallForTurn(t.Context(), turn, map[string]any{"repo": lc.web.Name,
				"ref": "seats/qa/fixtures"})
			if callErr != nil || got.Failed {
				t.Fatalf("swe's code_resolve of %s answered %v %q", fixtures, callErr, got.Output)
			}
		}
		return held, err
	})
	if !read {
		t.Fatal("the pass read no holds")
	}
	if report.RefsExpired != 0 || report.RefsKept != 1 {
		t.Errorf("the pass whose holds missed the read = %+v, want %s kept by the committer", report, fixtures)
	}
	if !lc.exists(t, lc.web, fixtures) {
		t.Fatalf("web's %s was expired although a live turn read it while the pass ran", fixtures)
	}

	row := lc.launch(t, turn)
	frame() // the executor parks, and its frame returns
	if len(row.Repos) != 0 {
		t.Fatalf("a launch naming no repository was granted %+v", row.Repos)
	}
	report = lc.pass(t, at, nil)
	if report.RefsExpired != 0 || report.RefsHeld != 1 {
		t.Errorf("a pass under the parked run = %+v, want %s held from its row", report, fixtures)
	}
	if !lc.exists(t, lc.web, fixtures) {
		t.Errorf("web's %s was expired under a parked run whose turn read it", fixtures)
	}
}

// A BRIDGED RUN HOLDS THE BRANCH IT WROTE: an agent-mode run — the launch
// rig's agent-mode box driving the MCP bridge — makes a bridged
// code_commit_files creating seats/swe/notes on the project repository web, a
// branch no launch granted, through the node's code seam, so the write lands
// on the node's code host; its hold is on the run's row before the write is
// sent. The run parks on a question and the node restarts: a fresh registry
// of running turns, every hold its memory kept gone, the row alone. A
// fortnight and an hour after the write, the node's pass keeps the branch,
// held from the row, which names the branch's hold key.
//
// Mutation: CodeDeps.branch skipping HoldRef → the branch is expired after
// the restart, red.
func TestABridgedRunHoldsTheBranchItWrote(t *testing.T) {
	t.Parallel()
	lc := newRefLifecycle(t)
	const notes = "refs/heads/seats/swe/notes"
	c, seat := splitLoginCompany(t, "api", "codex")
	var commit tools.Callable
	for _, tool := range builtin.CodeTools(lc.deps(0)) {
		if tool.Name() == builtin.CodeCommitFilesTool {
			commit = tool
		}
	}
	if commit == nil {
		t.Fatalf("the node's code tools serve no %s", builtin.CodeCommitFilesTool)
	}
	turn := &turnctx.Turn{RunID: statelog.NewOpID(time.Now(), ""), Seat: seat,
		Lookups: turnctx.NewLookups(), Holds: turnctx.NewHolds(nil)}
	run := launchBridgedOn(t, c, turn, commit)
	run.bridgedCall(t, builtin.CodeCommitFilesTool, commitOn(lc.web.Name, "seats/swe/notes", ""))
	if !lc.exists(t, lc.web, notes) {
		t.Fatalf("the bridged write of %s did not land on the node's code host", notes)
	}
	row, found, err := run.pending.Get(t.Context(), turn.RunID)
	if err != nil || !found {
		t.Fatalf("the bridged run's row: found %v, %v", found, err)
	}
	if key := code.RefHoldKey(lc.web.ID, notes); !slices.Contains(row.CodeHolds, key) {
		t.Errorf("the bridged run's row holds %v, want %s's key %s", row.CodeHolds, notes, key)
	}

	// THE NODE RESTARTS: no running turn, no bridge session — the row alone.
	restarted := func(ctx context.Context, keys []string) (map[string]bool, error) {
		return holdsReader{runs: run.pending, leases: listedLeases{}, self: &runningTurns{},
			budget: holdsAskBudget}.holds(ctx, keys)
	}
	report := lc.pass(t, time.Now().Add(code.RefExpiry+time.Hour), restarted)
	if report.RefsExpired != 0 || report.RefsHeld != 1 {
		t.Errorf("the pass after the restart = %+v, want %s held from the parked run's row", report, notes)
	}
	if !lc.exists(t, lc.web, notes) {
		t.Errorf("web's %s, which a parked bridged run wrote, was expired after its node restarted", notes)
	}
}

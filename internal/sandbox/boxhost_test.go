package sandbox

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/crewlet/crewlet/internal/events/types"
)

// boxAsks is a node's way of asking a box's host, as a case stages the host's
// answer.
type boxAsks struct {
	mu     sync.Mutex
	asked  []BoxRead
	result Result
	ended  bool
	err    error
}

func (b *boxAsks) ReadBox(_ context.Context, _ PendingRun, read BoxRead) (Result, bool, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.asked = append(b.asked, read)
	return b.result, b.ended, b.err
}

func (b *boxAsks) reads() []BoxRead {
	b.mu.Lock()
	defer b.mu.Unlock()
	return slices.Clone(b.asked)
}

// nudges records every nudge a coordinator sends.
type nudges struct {
	mu   sync.Mutex
	sent []Nudge
}

func (n *nudges) send(_ context.Context, nudge Nudge) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.sent = append(n.sent, nudge)
}

func (n *nudges) all() []Nudge {
	n.mu.Lock()
	defer n.mu.Unlock()
	return slices.Clone(n.sent)
}

// onNode makes the rig's coordinator a node's of the fleet: node, asking a
// box's host through asks, and telling hosts through told.
func (r *coordRig) onNode(t *testing.T, node string, asks BoxReader, told *nudges) {
	t.Helper()
	r.coordinator.node = node
	r.coordinator.boxes = asks
	if told != nil {
		r.coordinator.nudge = told.send
	}
}

// launchedBy seeds a running job in the direct cell as node launched it — its
// box on that node's host — and returns its row.
func (r *coordRig) launchedBy(turnID, node string) PendingRun {
	r.t.Helper()
	ctx := r.t.Context()
	box, err := r.provider.Create(ctx, Spec{TurnID: turnID, Placement: Direct})
	if err != nil {
		r.t.Fatalf("Create: %v", err)
	}
	if _, err := r.pending.BeginLaunch(ctx, PendingRun{
		TurnID: turnID, AgentHandle: "swe", AgentID: sweID, Role: "SWE",
		CodingAgent: "claude-code", TraceID: "tr-1", CreatedAt: r.now, LaunchNode: node,
	}, rigLease); err != nil {
		r.t.Fatalf("BeginLaunch: %v", err)
	}
	if err := r.pending.AttachSandbox(ctx, turnID, BoxRef{
		SandboxID: box.ID(), CommandID: "cmd-1", Placement: string(Direct),
		CodingAgent: "claude-code", PauseTTLSec: DefaultPauseTTL.Seconds(),
	}, rigLease); err != nil {
		r.t.Fatalf("AttachSandbox: %v", err)
	}
	r.suspend(turnID)
	return r.get(turnID)
}

// readHere records whether this node's own runner read a box back.
func readHere(rig *coordRig) *int {
	var n int
	var mu sync.Mutex
	rig.runner.CollectFunc = func(context.Context) error {
		mu.Lock()
		defer mu.Unlock()
		n++
		return nil
	}
	return &n
}

// A COLLECTION OF A BOX ON ANOTHER NODE'S HOST IS ASKED OF THAT HOST: the seat
// moved while the job worked, and the box is a process tree there that no
// other node can read. What the host read is the run's account here — fitted
// here, under the turn — and this node's own runner reads nothing.
//
// Mutation: read every box locally, and the collection reads a box this node
// cannot reach (in production: one that is not there).
func TestACollectionOfABoxOnAnotherHostIsAskedOfThatHost(t *testing.T) {
	t.Parallel()
	rig := newCoordRig(t)
	asks := &boxAsks{result: Result{Success: true, Text: "read on node-a's host"}}
	rig.onNode(t, "node-b", asks, nil)
	local := readHere(rig)
	rig.launchedBy("t1", "node-a")

	payload, ev := rig.completion("t1")
	if err := rig.coordinator.OnCompleted(t.Context(), payload, ev); err != nil {
		t.Fatalf("OnCompleted: %v", err)
	}
	if got := asks.reads(); !slices.Equal(got, []BoxRead{BoxReadCollect}) {
		t.Fatalf("the box's host was asked %v; want one collection", got)
	}
	if *local != 0 {
		t.Errorf("this node read %d box(es) itself", *local)
	}
	if len(rig.resumer.requests) != 1 || rig.resumer.requests[0].Ended.Text != "read on node-a's host" {
		t.Errorf("the turn resumed with %+v; want the host's account", rig.resumer.requests)
	}
}

// A HOST THAT DOES NOT ANSWER HANDS THE CLAIM BACK UNCOUNTED, and the
// completion is acknowledged: the host announces it again from its own next
// poll once it is back. Counted, a host that restarted for a minute would have
// its runs given up as unreadable.
//
// Mutation: count a silent host as a box that could not be read, and the
// row's run of failed collections starts.
func TestAHostThatDoesNotAnswerHandsTheClaimBackUncounted(t *testing.T) {
	t.Parallel()
	rig := newCoordRig(t)
	asks := &boxAsks{err: fmt.Errorf("%w: node-a, within a minute", ErrBoxHostSilent)}
	rig.onNode(t, "node-b", asks, nil)
	rig.launchedBy("t1", "node-a")

	payload, ev := rig.completion("t1")
	if err := rig.coordinator.OnCompleted(t.Context(), payload, ev); err != nil {
		t.Errorf("OnCompleted = %v; want the completion acknowledged — the host announces it again", err)
	}
	got := rig.get("t1")
	if got.Status != StatusRunning {
		t.Errorf("the run is %q; want it handed back to running", got.Status)
	}
	if got.Launch.CollectFailures != 0 || !got.Launch.CollectFailingSince.IsZero() {
		t.Errorf("a silent host was counted as a failed collection: %d since %v",
			got.Launch.CollectFailures, got.Launch.CollectFailingSince)
	}
	if len(rig.failures()) != 0 || len(rig.resumer.requests) != 0 {
		t.Errorf("a silent host ended or resumed the run: %v, %v", rig.failures(), rig.resumer.requests)
	}
}

// A BOX ITS HOST FOUND GONE IS SETTLED AT ONCE, as one this node's own backend
// found gone is: no attempt can read it.
func TestABoxItsHostFoundGoneIsSettledAtOnce(t *testing.T) {
	t.Parallel()
	rig := newCoordRig(t)
	asks := &boxAsks{err: fmt.Errorf("sandbox: node node-a found the box gone: %w", ErrBoxGone)}
	rig.onNode(t, "node-b", asks, nil)
	rig.launchedBy("t1", "node-a")

	payload, ev := rig.completion("t1")
	_ = rig.coordinator.OnCompleted(t.Context(), payload, ev)
	rig.finished("t1")
	if failed := rig.failures(); len(failed) != 1 || failed[0].Reason != types.SandboxFailureCollect {
		t.Errorf("announced %+v; want one %s", failed, types.SandboxFailureCollect)
	}
}

// A COMPLETION AFTER THE FLEET FOUND THE BOX'S HOST GONE CARRIES THE ROUND ON,
// with no read asked of anybody: there is no box, anywhere, and what the job did
// went with it. The turn resumes told what was lost and that no box was kept,
// and nothing is announced as a failure — the turn goes on.
//
// Mutation: collect a run whose row carries the verdict, and the host that is
// gone is asked — and, silent, the run is handed back for ever; settle it as
// lost, and the turn the park acknowledged is never started again.
func TestACompletionAfterTheHostWasFoundGoneCarriesTheRoundOn(t *testing.T) {
	t.Parallel()
	rig := newCoordRig(t)
	asks := &boxAsks{err: ErrBoxHostSilent}
	rig.onNode(t, "node-b", asks, nil)
	run := rig.launchedBy("t1", "node-a")
	if marked, err := rig.pending.MarkLost(t.Context(), "t1", run.LaunchID,
		JobLoss{Cause: LossLaunchNodeGone, Node: "node-a", At: rig.now}); err != nil || !marked {
		t.Fatalf("MarkLost = %v, %v", marked, err)
	}

	payload, ev := rig.completion("t1")
	if err := rig.coordinator.OnCompleted(t.Context(), payload, ev); err != nil {
		t.Fatalf("OnCompleted: %v", err)
	}
	if got := asks.reads(); len(got) != 0 {
		t.Errorf("asked the gone host for %v", got)
	}
	if failed := rig.failures(); len(failed) != 0 {
		t.Errorf("announced %+v; a lost job's round is carried on, not failed", failed)
	}
	requests := rig.resumer.calls()
	if len(requests) != 1 {
		t.Fatalf("the turn was resumed %d times, want once", len(requests))
	}
	lost := requests[0].Lost
	if lost == nil || lost.Cause != LossLaunchNodeGone || lost.BoxKept ||
		lost.Losses != 1 || lost.Limit != MaxRoundLosses || lost.Why != LossLaunchNodeGone.Why() {
		t.Errorf("resumed with %+v; want the gone node's loss, no box kept, the round's first", lost)
	}
}

// AN ENDING MADE ON ANOTHER NODE LEAVES THE BOX TO ITS HOST, and tells the host
// to look now: killed by id here, the local backend would find no such box and
// call the kill done, leaving the job running there.
//
// Mutation: kill a box whatever its host, and this node's backend is asked to.
func TestAnEndingOnAnotherNodeLeavesTheBoxToItsHost(t *testing.T) {
	t.Parallel()
	rig := newCoordRig(t)
	told := &nudges{}
	rig.onNode(t, "node-b", &boxAsks{}, told)
	run := rig.launchedBy("t1", "node-a")

	if err := rig.pending.SetStatus(t.Context(), "t1", StatusLaunching, rigLease); err != nil {
		t.Fatalf("SetStatus: %v", err)
	}
	if err := rig.coordinator.FailRun(t.Context(), "t1", types.SandboxFailureSuspensionUnrecorded,
		"the suspended conversation could not be written"); err != nil {
		t.Fatalf("FailRun: %v", err)
	}
	rig.finished("t1")
	if killed := rig.provider.KilledIDs(); len(killed) != 0 {
		t.Errorf("killed %v on this node for a box on another node's host", killed)
	}
	if got := told.all(); len(got) == 0 || got[0].TurnID != "t1" || got[0].Why != NudgeReleased ||
		got[0].LaunchID != run.LaunchID {
		t.Errorf("the box's host was told %+v; want the run's box released to it", got)
	}
}

// A RELAUNCH AWAY FROM THE PREVIOUS BOX'S HOST STARTS FRESH HERE, and leaves
// that box to its host: this node can neither continue in it nor kill it. The
// row names this node and its new box, and the old host is told once it does.
//
// Mutation: continue in a box whatever its host, and the relaunch is started
// in a box this node cannot reach.
func TestARelaunchAwayFromTheBoxsHostStartsFresh(t *testing.T) {
	t.Parallel()
	rig := newCoordRig(t)
	told := &nudges{}
	rig.onNode(t, "node-b", &boxAsks{}, told)
	prior := rig.launchedBy("t1", "node-a")
	// THE RESUMED TURN'S OWN RELAUNCH, the one shape a box is continued in.
	if _, won, err := rig.pending.ClaimForResume(t.Context(), "t1",
		CompletionTail(prior.LaunchID), rigLease); err != nil || !won {
		t.Fatalf("ClaimForResume = %v, %v", won, err)
	}

	res, err := rig.coordinator.Launch(t.Context(), rig.manager, launchReq("t1"))
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}
	if res.Reused || res.SandboxID == prior.SandboxID {
		t.Fatalf("the relaunch continued in %s, on another node's host", prior.SandboxID)
	}
	if killed := rig.provider.KilledIDs(); slices.Contains(killed, prior.SandboxID) {
		t.Errorf("killed %s from a node that is not its host", prior.SandboxID)
	}
	got := rig.get("t1")
	if got.LaunchNode != "node-b" || got.SandboxID != res.SandboxID {
		t.Errorf("the row names %s's box %s; want node-b's %s", got.LaunchNode, got.SandboxID, res.SandboxID)
	}
	sent := told.all()
	if len(sent) != 1 || sent[0].Why != NudgeReleased || sent[0].LaunchID != got.LaunchID {
		t.Errorf("told %+v; want the old host told once the row names the new box", sent)
	}
}

// A RELAUNCH ON THE BOX'S OWN HOST CONTINUES IN IT: the node that launched the
// previous job is the one node that can reach its direct box, so a relaunch
// there keeps the checkout — the expensive half of a run — rather than
// cloning, installing and setting up again from the pushed branch. Its twin
// above is the relaunch on any other node, which starts fresh.
//
// Mutation: drop the previous job's node from the box the relaunch is handed
// (coordstore.go's resetLaunch), and every relaunch on a real node starts
// fresh.
func TestARelaunchOnTheBoxsHostContinuesInIt(t *testing.T) {
	t.Parallel()
	rig := newCoordRig(t)
	told := &nudges{}
	rig.onNode(t, "node-a", &boxAsks{}, told)
	prior := rig.launchedBy("t1", "node-a")
	if _, won, err := rig.pending.ClaimForResume(t.Context(), "t1",
		CompletionTail(prior.LaunchID), rigLease); err != nil || !won {
		t.Fatalf("ClaimForResume = %v, %v", won, err)
	}
	req := launchReq("t1")
	req.Spec.Placement = Direct

	res, err := rig.coordinator.Launch(t.Context(), rig.manager, req)
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}
	if !res.Reused || res.SandboxID != prior.SandboxID {
		t.Fatalf("the relaunch on its box's host ran in %s (reused=%v), want it to continue in %s",
			res.SandboxID, res.Reused, prior.SandboxID)
	}
	if killed := rig.provider.KilledIDs(); len(killed) != 0 {
		t.Errorf("killed %v; the relaunch continues in its box", killed)
	}
	if sent := told.all(); len(sent) != 0 {
		t.Errorf("told %+v; no host is left a box", sent)
	}
	if got := rig.get("t1"); got.LaunchNode != "node-a" || got.SandboxID != prior.SandboxID {
		t.Errorf("the row names %s's box %s; want node-a's %s", got.LaunchNode, got.SandboxID, prior.SandboxID)
	}
}

// A LAUNCH RECORDS ITS OWN NODE AS THE BOX'S HOST, and its box records the turn
// it was made for — which is how that host tells, later, whether a run still
// names it.
func TestALaunchRecordsItsNodeAndItsBoxRecordsItsTurn(t *testing.T) {
	t.Parallel()
	rig := newCoordRig(t)
	rig.onNode(t, "node-a", nil, nil)
	req := launchReq("t1")
	// THE CELL, as every spec a launch is handed names it ([Manager.BuildSpec]).
	req.Spec.Placement = Direct
	res, err := rig.coordinator.Launch(t.Context(), rig.manager, req)
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}
	if got := rig.get("t1"); got.LaunchNode != "node-a" {
		t.Errorf("the row names %q as the box's host; want the launching node-a", got.LaunchNode)
	}
	boxes, err := rig.manager.HostBoxes()
	if err != nil {
		t.Fatalf("HostBoxes: %v", err)
	}
	if !slices.Contains(boxes, HostBox{SandboxID: res.SandboxID, TurnID: "t1", Placement: Direct}) {
		t.Errorf("this host lists %+v; want the launch's box made for t1", boxes)
	}
}

// A HOST READS A BOX FOR A PEER ONLY WHERE THE RUN'S ROW LICENSES IT: its own
// box, the job and the box the asker named, and the state the read is made in
// — so a request, whatever it is, makes no read the run's own protocol is not
// making, and never pauses a running job's box.
//
// Mutation: drop any of the checks, and its case reads (and, for a
// collection, pauses) the box.
func TestAHostReadsABoxOnlyWhereTheRowLicensesIt(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		arrange func(t *testing.T, rig *coordRig, run PendingRun) (launch, box string, read BoxRead)
		read    bool
	}{
		{"a running job's box, for a collection", func(_ *testing.T, _ *coordRig, run PendingRun) (string, string, BoxRead) {
			return run.LaunchID, run.SandboxID, BoxReadCollect
		}, false},
		{"a claimed job's box, for its collection", func(t *testing.T, rig *coordRig, run PendingRun) (string, string, BoxRead) {
			if _, won, err := rig.pending.ClaimForResume(t.Context(), run.TurnID,
				CompletionTail(run.LaunchID), rigLease); err != nil || !won {
				t.Fatalf("ClaimForResume = %v, %v", won, err)
			}
			return run.LaunchID, run.SandboxID, BoxReadCollect
		}, true},
		{"another job than the row holds", func(t *testing.T, rig *coordRig, run PendingRun) (string, string, BoxRead) {
			if _, won, err := rig.pending.ClaimForResume(t.Context(), run.TurnID,
				CompletionTail(run.LaunchID), rigLease); err != nil || !won {
				t.Fatalf("ClaimForResume = %v, %v", won, err)
			}
			return "not-" + run.LaunchID, run.SandboxID, BoxReadCollect
		}, false},
		{"another box than the row names", func(t *testing.T, rig *coordRig, run PendingRun) (string, string, BoxRead) {
			if _, won, err := rig.pending.ClaimForResume(t.Context(), run.TurnID,
				CompletionTail(run.LaunchID), rigLease); err != nil || !won {
				t.Fatalf("ClaimForResume = %v, %v", won, err)
			}
			return run.LaunchID, "not-" + run.SandboxID, BoxReadCollect
		}, false},
		{"a stop's read before it is decided", func(_ *testing.T, _ *coordRig, run PendingRun) (string, string, BoxRead) {
			return run.LaunchID, run.SandboxID, BoxReadStop
		}, false},
		{"a decided stop's read", func(t *testing.T, rig *coordRig, run PendingRun) (string, string, BoxRead) {
			if _, _, err := rig.pending.DecideEnding(t.Context(), run.TurnID, Decision{
				License: License{WhileIn: Active, Launch: EveryLaunch}, Reclaim: true,
				Stop: &RunStop{By: "founder", ByKind: "human"},
			}); err != nil {
				t.Fatalf("DecideEnding: %v", err)
			}
			return run.LaunchID, run.SandboxID, BoxReadStop
		}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rig := newCoordRig(t)
			rig.onNode(t, "node-a", nil, nil)
			local := readHere(rig)
			rig.runner.Finish(Result{Success: true, Text: "done"})
			run := rig.launchedBy("t1", "node-a")
			launch, box, read := tc.arrange(t, rig, run)

			_, _, err := rig.coordinator.ReadHostedBox(t.Context(), "t1", launch, box, read)
			switch {
			case tc.read && (err != nil || *local != 1):
				t.Errorf("ReadHostedBox = %v after %d read(s); want the box read once", err, *local)
			case !tc.read && (!errors.Is(err, ErrBoxNotHosted) || *local != 0):
				t.Errorf("ReadHostedBox = %v after %d read(s); want it refused unread", err, *local)
			}
			paused := rig.provider.Box(run.SandboxID).Paused()
			if want := tc.read && read == BoxReadCollect; paused != want {
				t.Errorf("the box is paused: %v; want %v", paused, want)
			}
		})
	}

	// AND ONLY ON ITS OWN HOST: a run another node launched is that node's.
	rig := newCoordRig(t)
	rig.onNode(t, "node-b", nil, nil)
	run := rig.launchedBy("t1", "node-a")
	if _, won, err := rig.pending.ClaimForResume(t.Context(), "t1",
		CompletionTail(run.LaunchID), rigLease); err != nil || !won {
		t.Fatalf("ClaimForResume = %v, %v", won, err)
	}
	if _, _, err := rig.coordinator.ReadHostedBox(t.Context(), "t1", run.LaunchID, run.SandboxID,
		BoxReadCollect); !errors.Is(err, ErrBoxNotHosted) {
		t.Errorf("a node read a box another node launched: %v", err)
	}
}

// A REPLY CARRIES THE RUN'S TEXT HELD TO ITS BOUND, the note inside it: whole
// lines, a report from its start, a failure from both ends — and text within
// it untouched.
func TestACarriedResultHoldsEachPieceToItsBound(t *testing.T) {
	t.Parallel()
	const budget = 4 << 10
	long := strings.Repeat("a line of the run's own words\n", 1000)
	got := CarriedResult(Result{Text: long, Error: long, Question: long, Transcript: long}, budget)
	for name, piece := range map[string]string{"report": got.Text, "failure": got.Error,
		"question": got.Question} {
		if len(piece) > budget || !strings.Contains(piece, "not shown") {
			t.Errorf("the %s is %d bytes, saying %q; want it inside %d with its note", name,
				len(piece), piece[max(0, len(piece)-80):], budget)
		}
	}
	if !strings.HasPrefix(got.Text, "a line of") || !strings.HasSuffix(got.Error, "words\n") {
		t.Errorf("the report did not keep its start or the failure its end")
	}
	if got.Transcript != long {
		t.Error("the transcript, bounded where the box is read, was cut again")
	}
	short := CarriedResult(Result{Text: "done"}, budget)
	if short.Text != "done" {
		t.Errorf("text inside the bound became %q", short.Text)
	}
}

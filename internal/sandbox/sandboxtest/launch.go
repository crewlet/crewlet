package sandboxtest

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"testing/synctest"
	"time"

	"github.com/crewlet/crewlet/internal/sandbox"
)

func testASecondLaunchKeepsTheBoxItWillReattachTo(t *testing.T, s sandbox.PendingStore) {
	// IN A BUBBLE, so the store's clock is the case's: the box is dated the
	// instant it is attached, and the relaunch comes a second later.
	synctest.Test(t, func(t *testing.T) {
		// A second launch in one turn presents the same turn id, and the box
		// on that row is the paused checkout it may reattach to. A launch
		// that erased it would re-clone the work, and one that did not name
		// the box could not reclaim it when the next job runs elsewhere.
		//
		// AND WHOSE HOST IT IS ON, with it: a box is a process tree or a
		// container on the launching node's host, so the relaunch may
		// continue in it only there, and until this launch attaches a box of
		// its own the row's box is still that one. What was the previous
		// JOB's and not its box's (command, session) is not the relaunch's:
		// a reader finding it would read the finished job back as this one.
		first := run("t1")
		first.Placement, first.LaunchNode = "direct", "node-a"
		// AND THE ENTRY ITS JOB RAN UNDER, which a relaunch is handed with
		// the box: its copy of the session continues only under it.
		first.Launch.Entry = "entry-primary"
		mustLaunched(t, s, first)
		attached := time.Now().UTC()
		if err := s.AttachSandbox(t.Context(), "t1",
			sandbox.BoxRef{SandboxID: "box-9", CommandID: "c-1", SessionID: "s-1", Harness: "claude-code"},
			sandbox.Fence{}); err != nil {
			t.Fatalf("attach: %v", err)
		}
		// AND WHEN IT WAS FIRST ATTACHED, which is how old its checkouts can be:
		// a relaunch continues in it only while it is young enough
		// ([sandbox.KeptBoxMaxAge]), so a box the attach left undated, or a reset
		// that handed the launch no date, would never be continued in. And a
		// relaunch that dated it again would date it a second later, where on
		// the machine's clock the two read equal inside one tick.
		if dated := mustGet(t, s, "t1").BoxSince; !dated.Equal(attached) {
			t.Fatalf("attaching a box to a run that named none dated it %v, want %v, the instant it "+
				"was attached", dated, attached)
		}
		time.Sleep(time.Second)
		next := run("t1")
		next.LaunchNode = "node-b"
		next.Launch.Entry = "entry-secondary"
		opened, err := s.BeginLaunch(t.Context(), next, sandbox.Fence{})
		if err != nil {
			t.Fatalf("relaunch: %v", err)
		}

		got := mustGet(t, s, "t1")
		if got.SandboxID != "box-9" || got.Placement != "direct" || got.LaunchNode != "node-a" {
			t.Errorf("a second launch erased the attached box, its cell or its host: box %q in %q on %q",
				got.SandboxID, got.Placement, got.LaunchNode)
		}
		if got.CommandID != "" || got.SessionID != "" {
			t.Errorf("the relaunch's row still names the previous job: command %q, session %q",
				got.CommandID, got.SessionID)
		}
		if got.Launch.Entry != "entry-secondary" {
			t.Errorf("the relaunch's record names entry %q, want its own", got.Launch.Entry)
		}
		want := sandbox.PriorBox{SandboxID: "box-9", Placement: "direct",
			Harness: "claude-code", Status: sandbox.StatusRunning, LaunchNode: "node-a", Entry: "entry-primary"}
		prior := opened.Prior
		since := prior.Since
		prior.Since = time.Time{}
		if !reflect.DeepEqual(prior, want) || !since.Equal(attached) {
			t.Errorf("the relaunch was handed %+v, dated %v, as the box it found, want %+v dated %v",
				prior, since, want, attached)
		}
		if !got.BoxSince.Equal(attached) {
			t.Errorf("the relaunch's row dates its box %v, want the attach's %v", got.BoxSince, attached)
		}
		// THE ATTACH THAT NAMES THIS JOB'S BOX NAMES ITS HOST WITH IT.
		if err := s.AttachSandbox(t.Context(), "t1",
			sandbox.BoxRef{SandboxID: "box-10", Placement: "direct", Node: "node-b", Harness: "claude-code"},
			sandbox.Fence{}); err != nil {
			t.Fatalf("attach the relaunch's box: %v", err)
		}
		if got := mustGet(t, s, "t1"); got.SandboxID != "box-10" || got.LaunchNode != "node-b" {
			t.Errorf("the relaunch's box %q is named on %q, want box-10 on node-b", got.SandboxID, got.LaunchNode)
		}
	})
}

// A RELAUNCH'S BOX MOVES THE ROW TO ITS CELL. The next job can land in
// another cell than the last — a fallback onto a member with a run_in of its
// own — and every reader reaches the box through the row's cell, so the box,
// its cell and its harness move as one write: a row that took the new box and kept
// the old cell sent every poll, collection and reclaim of it to a backend that
// never made it.
func testARelaunchsBoxMovesTheRowToItsCell(t *testing.T, s sandbox.PendingStore) {
	first := run("t-cells")
	first.Placement = "direct"
	mustLaunched(t, s, first)
	if err := s.AttachSandbox(t.Context(), "t-cells", sandbox.BoxRef{
		SandboxID: "box-1", Placement: "direct", Harness: "claude-code",
	}, sandbox.Fence{}); err != nil {
		t.Fatalf("attach: %v", err)
	}
	next := run("t-cells")
	next.Placement, next.Harness = "container", "opencode"
	mustBeginLaunch(t, s, next)
	if err := s.AttachSandbox(t.Context(), "t-cells", sandbox.BoxRef{
		SandboxID: "box-2", Placement: "container", Harness: "opencode",
	}, sandbox.Fence{}); err != nil {
		t.Fatalf("attach: %v", err)
	}

	got := mustGet(t, s, "t-cells")
	if got.SandboxID != "box-2" || got.Placement != "container" || got.Harness != "opencode" {
		t.Errorf("row = box %q in %q on %q, want the relaunch's box-2 in container on opencode",
			got.SandboxID, got.Placement, got.Harness)
	}
}

// EVERY LAUNCH RECORDS THE NODE THAT LAUNCHED IT — the relaunch's, not the
// first job's: a relaunch may land on another node, and a box is on the host of
// the node that started it, never on the one that started the job before it.
func testEveryLaunchRecordsTheNodeThatLaunchedIt(t *testing.T, s sandbox.PendingStore) {
	first := run("t-node")
	first.LaunchNode = "node-a"
	mustLaunched(t, s, first)
	if got := mustGet(t, s, "t-node"); got.LaunchNode != "node-a" {
		t.Fatalf("the launch recorded node %q, want node-a", got.LaunchNode)
	}
	next := run("t-node")
	next.LaunchNode = "node-b"
	mustBeginLaunch(t, s, next)
	if got := mustGet(t, s, "t-node"); got.LaunchNode != "node-b" {
		t.Errorf("the relaunch left node %q, want its own node-b", got.LaunchNode)
	}
}

func testASecondLaunchDropsTheFirstSuspension(t *testing.T, s sandbox.PendingStore) {
	// THE RELAUNCH RESET. The previous job's parked turn state is not this
	// job's, and leaving it in place is worse than absent: a completion
	// claimed before the new suspension lands would resume the turn from the
	// park the LAST job left, which has already moved on. The status is what
	// says so — back to launching, so the poll leaves it alone until the new
	// park is written.
	mustLaunched(t, s, run("t1"))
	mustBeginLaunch(t, s, run("t1"))

	got := mustGet(t, s, "t1")
	if got.Status != sandbox.StatusLaunching {
		t.Errorf("status = %q, want %q", got.Status, sandbox.StatusLaunching)
	}
	if len(got.ExecuteState) != 0 {
		t.Errorf("the first call's suspension survived the relaunch: %+v", got.ExecuteState)
	}
}

// A SECOND LAUNCH UNDER ONE TURN ID IS A SECOND ROUND, and its tool log starts
// empty.
//
// The bridged log is the whole record a run's resume rebuilds its phase
// from. A reviewer's self_iterate launches another executor run under the same
// turn id, and the first round's log left in place is not merely stale: a
// round that in fact submitted nothing would report the PREVIOUS round's
// outcome instead of being rescued as incomplete, and the previous round's
// deliveries would satisfy this round's delivery check. The same reset the
// suspension beside it gets, for the same reason.
func testASecondLaunchDropsTheFirstRunsBridgedCalls(t *testing.T, s sandbox.PendingStore) {
	first := run("t-relaunch-bridge")
	launch := mustBeginLaunch(t, s, first)
	if _, err := s.AppendBridgeCall(context.Background(), first.TurnID, sandbox.BridgeAppend{
		Launch: launch,
		Call:   sandbox.BridgeCall{Name: "submit_work", Args: `{"outcome":"delivered"}`, At: base},
	}); err != nil {
		t.Fatalf("AppendBridgeCall: %v", err)
	}
	if got := mustLogged(t, s, first.TurnID); len(got.Log.Calls) != 1 {
		t.Fatalf("the first round recorded %d calls, want 1", len(got.Log.Calls))
	}

	mustBeginLaunch(t, s, run("t-relaunch-bridge"))
	got := mustLogged(t, s, first.TurnID)
	if len(got.Log.Calls) != 0 {
		t.Errorf("the second round inherited %d calls from the first: %+v",
			len(got.Log.Calls), got.Log.Calls)
	}
	if got.Log.Elided != 0 {
		t.Errorf("the elision count survived the relaunch: %d", got.Log.Elided)
	}
}

func testALaunchNeedsATurnID(t *testing.T, s sandbox.PendingStore) {
	// The turn id is the identity. A row without one collides with every
	// other row that forgot the same field, and nothing could ever find it.
	if _, err := s.BeginLaunch(t.Context(), run(""), sandbox.Fence{}); err == nil {
		t.Error("a run with no turn id was persisted")
	}
}

// A LAUNCH ANSWERS THE JOB IT OPENED, on the first launch and on a relaunch
// alike: its name and the instant it began, exactly as the row records them.
// The answer is what a run's bridge session is bound to before its
// box exists, so an answer that differed from the row — a stale name, or the
// previous job's — would file every call the box makes under a job that is not
// its own.
//
// IN A BUBBLE, so the store's clock is the case's: each launch is held to the
// instant it was made, a second after the one before, where a relaunch that
// answered the previous job's start read equal to its own inside one tick of
// the machine's clock.
func testALaunchAnswersTheJobItOpened(t *testing.T, s sandbox.PendingStore) {
	synctest.Test(t, func(t *testing.T) {
		ctx := t.Context()
		var names []string
		for i := range 2 {
			launched := time.Now().UTC()
			opened, err := s.BeginLaunch(ctx, run("t-answered"), sandbox.Fence{})
			if err != nil {
				t.Fatalf("launch %d: %v", i+1, err)
			}
			row := mustGet(t, s, "t-answered")
			if opened.LaunchID == "" || opened.LaunchID != row.LaunchID {
				t.Fatalf("launch %d answered job %q, and the row holds %q", i+1, opened.LaunchID, row.LaunchID)
			}
			if facts := row.Launch; !facts.StartedAt.Equal(launched) || !opened.Launch.StartedAt.Equal(launched) {
				t.Errorf("launch %d answered a start of %v, and the row records %v; want both %v, the "+
					"instant it launched", i+1, opened.Launch.StartedAt, facts.StartedAt, launched)
			}
			names = append(names, opened.LaunchID)
			time.Sleep(time.Second)
		}
		if names[0] == names[1] {
			t.Error("a relaunch answered the first job's name — the case exercises nothing")
		}
	})
}

// A LAUNCH ON A ROW A NEWER LEASE HOLDS IS REFUSED, not reported open. The
// launch that went on from a silent no-op started a job on a row that never
// named it, in a box nothing would ever reclaim.
func testALaunchARowsNewerLeaseOutranksIsRefused(t *testing.T, s sandbox.PendingStore) {
	ctx := t.Context()
	first := mustLaunched(t, s, run("t-outranked"))
	if ok, err := s.ClaimOwnership(ctx, "t-outranked", "node-b:2", 7); err != nil || !ok {
		t.Fatalf("ClaimOwnership = %v, %v", ok, err)
	}
	opened, err := s.BeginLaunch(ctx, run("t-outranked"), sandbox.Fence{Owner: "node-a:1", Epoch: 3})
	if err == nil {
		t.Fatalf("a launch under a lease a newer one outranks was opened as job %q", opened.LaunchID)
	}
	// THE REFUSAL SAYS THE SEAT MOVED, the one fact a launch's caller acts on:
	// this node no longer holds the seat, so nothing it starts is anybody's.
	if !errors.Is(err, sandbox.ErrSeatNotHeld) {
		t.Errorf("a launch under an outgrown lease = %v, want ErrSeatNotHeld", err)
	}
	if got := mustGet(t, s, "t-outranked"); got.LaunchID != first || got.Status != sandbox.StatusRunning {
		t.Errorf("the refused launch moved the row: job %q status %q, want %q %q",
			got.LaunchID, got.Status, first, sandbox.StatusRunning)
	}
}

func testALaunchingRunIsNotClaimable(t *testing.T, s sandbox.PendingStore) {
	// THE WINDOW THIS STATE EXISTS TO CLOSE. The job is started and can
	// finish at any moment, but the turn has not yet written the
	// conversation a resume re-enters. A completion claimed here would find
	// nothing to resume into and fail the whole turn.
	mustBeginLaunch(t, s, run("t1"))

	if _, won, err := s.ClaimForResume(t.Context(), "t1", completionOf(t, s, "t1"), sandbox.Fence{}); err != nil || won {
		t.Fatalf("a launching run was claimed: won=%v err=%v", won, err)
	}
	// Nor by a tail that names the status outright: the closed set is the
	// store's to keep, not the caller's to widen.
	widened := sandbox.Tail{
		Launch: mustGet(t, s, "t1").LaunchID, From: []string{sandbox.StatusLaunching},
	}
	if _, won, err := s.ClaimForResume(t.Context(), "t1", widened, sandbox.Fence{}); err != nil || won {
		t.Fatalf("a tail naming launching claimed it: won=%v err=%v", won, err)
	}
	if got := mustGet(t, s, "t1"); got.Status != sandbox.StatusLaunching {
		t.Errorf("a refused claim moved the row to %q", got.Status)
	}
}

func testSuspendingOpensTheRunToTheTail(t *testing.T, s sandbox.PendingStore) {
	// One write, both facts: the conversation lands and the run becomes
	// pollable at the same instant. Two writes would leave a running row
	// with nothing to resume into, which is the state the poll fires on.
	mustBeginLaunch(t, s, run("t1"))
	suspended, err := s.MarkSuspended(t.Context(), "t1", suspension())
	if err != nil || !suspended {
		t.Fatalf("mark suspended: suspended=%v err=%v", suspended, err)
	}

	got := mustGet(t, s, "t1")
	if got.Status != sandbox.StatusRunning {
		t.Errorf("status = %q, want %q", got.Status, sandbox.StatusRunning)
	}
	if len(got.ExecuteState) == 0 {
		t.Error("the run opened to the poll with no conversation on it")
	}
	if _, won, err := s.ClaimForResume(t.Context(), "t1", completionOf(t, s, "t1"), sandbox.Fence{}); err != nil || !won {
		t.Errorf("a suspended run was not claimable: won=%v err=%v", won, err)
	}
}

func testOnlyALaunchingRunCanSuspend(t *testing.T, s sandbox.PendingStore) {
	// A run whose tail has already been claimed has nowhere to put a
	// suspension, and re-arming it would hand a redelivered completion a
	// second resume of a turn that is over. Reported rather than written,
	// so the caller fails the run instead of stranding it.
	mustLaunched(t, s, run("t1"))
	mustClaim(t, s, "t1")

	suspended, err := s.MarkSuspended(t.Context(), "t1", suspension())
	if err != nil {
		t.Fatalf("mark suspended: %v", err)
	}
	if suspended {
		t.Error("a claimed run was re-armed by a late suspension")
	}
	if got := mustGet(t, s, "t1"); got.Status != sandbox.StatusResumed {
		t.Errorf("status = %q, want the claim to stand", got.Status)
	}
}

func testAttachingABoxClearsThePauseStamp(t *testing.T, s sandbox.PendingStore) {
	// paused_at is half of what an operator board draws a HELD box from, so
	// it has to move with the box. A reused box is attached while the row
	// still carries the stamp from the collect that paused it — and a live
	// second run then rendered as a paused one for the rest of the turn.
	mustLaunched(t, s, run("t1"))
	if err := s.AttachSandbox(t.Context(), "t1",
		sandbox.BoxRef{SandboxID: "box-1", CommandID: "c-1"}, sandbox.Fence{}); err != nil {
		t.Fatalf("attach: %v", err)
	}
	if err := s.MarkBoxPaused(t.Context(), "t1", base); err != nil {
		t.Fatalf("pause: %v", err)
	}
	if err := s.AttachSandbox(t.Context(), "t1",
		sandbox.BoxRef{SandboxID: "box-1", CommandID: "c-2"}, sandbox.Fence{}); err != nil {
		t.Fatalf("reattach: %v", err)
	}

	if got := mustGet(t, s, "t1"); got.Paused() {
		t.Errorf("a reattached box still reads as a held paused box: paused_at=%s", got.PausedAt)
	}
}

// A TAIL REQUEST ASKS ABOUT ONE JOB, and the store's record is what says
// whether that job is running: while it runs it is, a later launch on the same
// turn is a different job, a parked run is not running, and a run whose
// record is gone — settled and reclaimed — says so rather than answering with
// the empty output of a box nobody drives any more.
func testLiveLaunchSaysWhetherAJobIsRunning(t *testing.T, s sandbox.PendingStore) {
	ctx := t.Context()
	mustLaunched(t, s, run("t1"))
	first := mustGet(t, s, "t1").LaunchID

	if _, running, status, err := sandbox.LiveLaunch(ctx, s, "t1", first); err != nil ||
		!running || status != "" {
		t.Fatalf("a running launch read running=%v status=%q err=%v; want running", running, status, err)
	}
	if _, running, status, err := sandbox.LiveLaunch(ctx, s, "t1", "some-other-job"); err != nil ||
		running || status != sandbox.StatusReplaced {
		t.Errorf("another job's launch id read running=%v status=%q err=%v; want %q",
			running, status, err, sandbox.StatusReplaced)
	}

	park(t, s, "t1")
	if _, running, status, err := sandbox.LiveLaunch(ctx, s, "t1", first); err != nil ||
		running || status != sandbox.StatusAwaiting {
		t.Errorf("a parked launch read running=%v status=%q err=%v; want %q",
			running, status, err, sandbox.StatusAwaiting)
	}

	if _, finished, err := end(ctx, s, "t1", everyJob(sandbox.Fence{}, sandbox.Active)); err != nil || !finished {
		t.Fatalf("Finish = %v, %v", finished, err)
	}
	if _, running, status, err := sandbox.LiveLaunch(ctx, s, "t1", first); err != nil ||
		running || status != "" {
		t.Errorf("a finished run read running=%v status=%q err=%v; want not running, no record",
			running, status, err)
	}
}

func testEveryLaunchIsNamedAnew(t *testing.T, s sandbox.PendingStore) {
	// The name is what tells a completion's job from the one that replaced
	// it, so it has to change on every launch under one turn id, and it is
	// the store's to mint: a caller that could choose it could reuse one.
	chosen := run("t1")
	chosen.LaunchID = "chosen-by-the-caller"
	mustBeginLaunch(t, s, chosen)
	first := mustGet(t, s, "t1").LaunchID
	if first == "" || first == chosen.LaunchID {
		t.Fatalf("launch id = %q, want one the store minted", first)
	}
	mustBeginLaunch(t, s, run("t1"))
	if second := mustGet(t, s, "t1").LaunchID; second == "" || second == first {
		t.Errorf("a second launch under one turn id kept the name %q", second)
	}
}

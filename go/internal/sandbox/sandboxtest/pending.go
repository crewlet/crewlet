// Package sandboxtest is the pending-run store's contract suite.
//
// ONE SUITE, BOTH IMPLEMENTATIONS. The properties that matter here — the
// at-most-once tail claim, the epoch fence, the box record's two halves moving
// together — are properties of the STATEMENTS, not of the code around them, so
// a suite that ran only against a fake would assert the author's intent and
// nothing about the store. And a memory twin nobody holds to the contract is a
// twin that models the store wrongly and certifies the bug.
package sandboxtest

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/sandbox"
)

var base = time.Date(2026, 8, 23, 12, 0, 0, 0, time.UTC)

// Run drives every case against one store.
func Run(t *testing.T, newStore func(t *testing.T) sandbox.PendingStore) {
	t.Helper()
	cases := []struct {
		name string
		fn   func(*testing.T, sandbox.PendingStore)
	}{
		{"CreateIsIdempotent", testCreateIsIdempotent},
		{"CreateNeedsATurnID", testCreateNeedsATurnID},
		{"TheTailIsClaimedExactlyOnce", testTheTailIsClaimedExactlyOnce},
		{"AClaimIsExclusiveUnderContention", testAClaimIsExclusiveUnderContention},
		{"AClaimReportsWhereItCameFrom", testAClaimReportsWhereItCameFrom},
		{"AReseedIsStillClaimable", testAReseedIsStillClaimable},
		{"ATerminalRunIsNotClaimable", testATerminalRunIsNotClaimable},
		{"ParkingCarriesTheBranch", testParkingCarriesTheBranch},
		{"OwnershipIsNotStolenByAnOlderLease", testOwnershipIsNotStolenByAnOlderLease},
		{"AStaleFenceCannotWrite", testAStaleFenceCannotWrite},
		{"ReleasingABoxClearsBothHalves", testReleasingABoxClearsBothHalves},
		{"ExecuteStateRoundTrips", testExecuteStateRoundTrips},
		{"ActiveIncludesResumed", testActiveIncludesResumed},
		{"AnAnswerFindsTheRunThatAsked", testAnAnswerFindsTheRunThatAsked},
		{"AnAnswerWithNoConversationMatchesNothing", testAnAnswerWithNoConversationMatchesNothing},
		{"TheReaperSeesOnlyOldSnapshots", testTheReaperSeesOnlyOldSnapshots},
		{"ListingsAreStable", testListingsAreStable},
		{"APauseExpiresExactlyOnce", testAPauseExpiresExactlyOnce},
		{"OnlyAParkedRunCanExpire", testOnlyAParkedRunCanExpire},
		{"AnAnsweredRunCannotBeExpiredUnderTheResume", testAnAnsweredRunCannotBeExpiredUnderTheResume},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			tc.fn(t, newStore(t))
		})
	}
}

func run(turnID string) sandbox.PendingRun {
	return sandbox.PendingRun{
		TurnID: turnID, AgentHandle: "swe", AgentID: "a-1", Role: "SWE",
		CodingAgent: "claude-code", TaskDescription: "fix the flake",
		SuccessCriteria: []string{"CI green"}, ConversationKey: "slack:C1",
		TraceID: "tr-1", CreatedAt: base,
	}
}

func mustCreate(t *testing.T, s sandbox.PendingStore, r sandbox.PendingRun) {
	t.Helper()
	if err := s.Create(context.Background(), r); err != nil {
		t.Fatalf("create %s: %v", r.TurnID, err)
	}
}

func mustGet(t *testing.T, s sandbox.PendingStore, turnID string) sandbox.PendingRun {
	t.Helper()
	got, ok, err := s.Get(context.Background(), turnID)
	if err != nil {
		t.Fatalf("get %s: %v", turnID, err)
	}
	if !ok {
		t.Fatalf("no run %s", turnID)
	}
	return got
}

func testCreateIsIdempotent(t *testing.T, s sandbox.PendingStore) {
	// A kick-off turn redelivered after its ack was lost presents the same
	// turn id. Raising would turn a recoverable redelivery into a failed
	// turn, while the row already there is the correct one — possibly with
	// a box attached that the second create would erase.
	mustCreate(t, s, run("t1"))
	if err := s.AttachSandbox(context.Background(), "t1",
		sandbox.BoxRef{SandboxID: "box-9", CommandID: "c-1"}, sandbox.Fence{}); err != nil {
		t.Fatalf("attach: %v", err)
	}
	mustCreate(t, s, run("t1"))

	if got := mustGet(t, s, "t1"); got.SandboxID != "box-9" {
		t.Errorf("a repeat create erased the attached box: %+v", got)
	}
}

func testCreateNeedsATurnID(t *testing.T, s sandbox.PendingStore) {
	// The turn id is the identity. A row without one collides with every
	// other row that forgot the same field, and nothing could ever find it.
	r := run("")
	if err := s.Create(context.Background(), r); err == nil {
		t.Error("a run with no turn id was persisted")
	}
}

func testTheTailIsClaimedExactlyOnce(t *testing.T, s sandbox.PendingStore) {
	// THE AT-MOST-ONCE GATE. Two nodes both splicing a result into one
	// suspended loop produce two turns from one job — which the seat sees
	// as its own work arriving twice.
	mustCreate(t, s, run("t1"))
	if _, won, err := s.ClaimForResume(context.Background(), "t1"); err != nil || !won {
		t.Fatalf("first claim: won=%v err=%v", won, err)
	}
	if _, won, err := s.ClaimForResume(context.Background(), "t1"); err != nil || won {
		t.Errorf("a second claim won: won=%v err=%v", won, err)
	}
}

func testAClaimIsExclusiveUnderContention(t *testing.T, s sandbox.PendingStore) {
	// The property a fake cannot have. Ten goroutines racing one tail:
	// exactly one may win.
	mustCreate(t, s, run("t1"))
	var (
		wg   sync.WaitGroup
		mu   sync.Mutex
		wins int
	)
	for range 10 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, won, err := s.ClaimForResume(context.Background(), "t1"); err == nil && won {
				mu.Lock()
				wins++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if wins != 1 {
		t.Errorf("%d claims won; exactly one may", wins)
	}
}

func testAClaimReportsWhereItCameFrom(t *testing.T, s sandbox.PendingStore) {
	// A failed resume dispatch reverts to EXACTLY the prior status so the
	// NAK'd trigger can re-claim on redelivery. Inferring it afterwards is
	// unsound: a reused run keeps its old question, so "has a question"
	// does not mean "was parked".
	mustCreate(t, s, run("t1"))
	if err := s.MarkAwaiting(context.Background(), "t1",
		sandbox.Clarification{Question: "which branch?", Audience: "requester"}); err != nil {
		t.Fatalf("park: %v", err)
	}
	got, won, err := s.ClaimForResume(context.Background(), "t1")
	if err != nil || !won {
		t.Fatalf("claim: won=%v err=%v", won, err)
	}
	if got.ClaimedFrom != sandbox.StatusAwaiting {
		t.Errorf("claimed_from = %q, want the parked status", got.ClaimedFrom)
	}
	if got.Status != sandbox.StatusResumed {
		t.Errorf("the returned row carries %q, want the POST-flip status", got.Status)
	}
}

func testAReseedIsStillClaimable(t *testing.T, s sandbox.PendingStore) {
	// Reaping the box does NOT end the run — the answer can still arrive,
	// and the work re-seeds from the pushed branch. That is the whole
	// reason reseed is a state rather than a deletion.
	mustCreate(t, s, run("t1"))
	if err := s.SetStatus(context.Background(), "t1", sandbox.StatusReseed, sandbox.Fence{}); err != nil {
		t.Fatalf("set reseed: %v", err)
	}
	if _, won, err := s.ClaimForResume(context.Background(), "t1"); err != nil || !won {
		t.Errorf("a reseeded run could not be claimed: won=%v err=%v", won, err)
	}
}

func testATerminalRunIsNotClaimable(t *testing.T, s sandbox.PendingStore) {
	for _, status := range []string{sandbox.StatusDone, sandbox.StatusFailed} {
		mustCreate(t, s, run(status))
		if err := s.SetStatus(context.Background(), status, status, sandbox.Fence{}); err != nil {
			t.Fatalf("set %s: %v", status, err)
		}
		if _, won, _ := s.ClaimForResume(context.Background(), status); won {
			t.Errorf("a %s run was claimed", status)
		}
	}
}

func testParkingCarriesTheBranch(t *testing.T, s sandbox.PendingStore) {
	// The WIP is pushed BEFORE the question is asked, so a snapshot reaped
	// days later loses nothing a re-seed cannot recover. A question parked
	// over unpushed work is a question whose answer arrives to an empty box.
	mustCreate(t, s, run("t1"))
	if err := s.MarkAwaiting(context.Background(), "t1", sandbox.Clarification{
		Question: "which base branch?", Audience: "requester",
		Branch: "wip/swe/t1", SessionID: "sess-1",
	}); err != nil {
		t.Fatalf("park: %v", err)
	}
	got := mustGet(t, s, "t1")
	if got.Status != sandbox.StatusAwaiting || got.Branch != "wip/swe/t1" ||
		got.Question != "which base branch?" || got.Audience != "requester" {
		t.Errorf("parked run = %+v", got)
	}
}

func testOwnershipIsNotStolenByAnOlderLease(t *testing.T, s sandbox.PendingStore) {
	// A run whose epoch is HIGHER belongs to a newer lease. Taking it would
	// put two engines on one box, both collecting, both resuming.
	mustCreate(t, s, run("t1"))
	if won, err := s.ClaimOwnership(context.Background(), "t1", "node-b:2", 5); err != nil || !won {
		t.Fatalf("claim: won=%v err=%v", won, err)
	}
	if won, err := s.ClaimOwnership(context.Background(), "t1", "node-a:1", 3); err != nil || won {
		t.Errorf("an older lease stole the run: won=%v err=%v", won, err)
	}
	if got := mustGet(t, s, "t1"); got.Owner != "node-b:2" || got.OwnerEpoch != 5 {
		t.Errorf("owner = %+v", got)
	}
	// Equal epochs pass: a node re-claiming its OWN run after a restart
	// within one lease must not be locked out of it.
	if won, err := s.ClaimOwnership(context.Background(), "t1", "node-b:3", 5); err != nil || !won {
		t.Errorf("a node could not re-claim its own run: won=%v err=%v", won, err)
	}
}

func testAStaleFenceCannotWrite(t *testing.T, s sandbox.PendingStore) {
	// THE FENCE IS THE GUARANTEE, the ownership check only an optimisation:
	// a node whose lease moved cannot write even if it has not noticed yet.
	mustCreate(t, s, run("t1"))
	if _, err := s.ClaimOwnership(context.Background(), "t1", "node-b:2", 7); err != nil {
		t.Fatalf("claim: %v", err)
	}
	stale := sandbox.Fence{Owner: "node-a:1", Epoch: 3}
	if err := s.SetStatus(context.Background(), "t1", sandbox.StatusFailed, stale); err != nil {
		t.Fatalf("set status: %v", err)
	}
	if got := mustGet(t, s, "t1"); got.Status != sandbox.StatusRunning {
		t.Errorf("a stale fence wrote %q", got.Status)
	}
	if err := s.AttachSandbox(context.Background(), "t1",
		sandbox.BoxRef{SandboxID: "ghost"}, stale); err != nil {
		t.Fatalf("attach: %v", err)
	}
	if got := mustGet(t, s, "t1"); got.SandboxID != "" {
		t.Errorf("a stale fence attached a box: %q", got.SandboxID)
	}
}

func testReleasingABoxClearsBothHalves(t *testing.T, s sandbox.PendingStore) {
	// A paused_at pointing at no box is a snapshot the reaper looks for
	// every tick and never finds — a warning per tick, for ever.
	mustCreate(t, s, run("t1"))
	if err := s.AttachSandbox(context.Background(), "t1",
		sandbox.BoxRef{SandboxID: "box-1", CommandID: "c-1"}, sandbox.Fence{}); err != nil {
		t.Fatalf("attach: %v", err)
	}
	if err := s.MarkBoxPaused(context.Background(), "t1", base); err != nil {
		t.Fatalf("pause: %v", err)
	}
	if got := mustGet(t, s, "t1"); !got.Paused() || !got.HasBox() {
		t.Fatalf("run = %+v", got)
	}
	if err := s.ReleaseBox(context.Background(), "t1"); err != nil {
		t.Fatalf("release: %v", err)
	}
	got := mustGet(t, s, "t1")
	if got.Paused() || got.HasBox() || got.CommandID != "" {
		t.Errorf("release left %+v", got)
	}
}

func testExecuteStateRoundTrips(t *testing.T, s sandbox.PendingStore) {
	// THE SUSPENDED CONVERSATION. Everything the tool loop needs to re-enter
	// where it stopped; a lossy round trip here resumes into a conversation
	// that is not the one that was suspended.
	mustCreate(t, s, run("t1"))
	state := map[string]any{
		"messages":             []any{map[string]any{"role": "assistant", "content": "working"}},
		"pending_tool_call_id": "call_1",
		"pending_tool_name":    "run_sandbox",
		"active_tool_names":    []any{"run_sandbox", "activate_tool"},
		"iteration":            float64(2),
	}
	if err := s.SaveExecuteState(context.Background(), "t1", state); err != nil {
		t.Fatalf("save: %v", err)
	}
	got := mustGet(t, s, "t1")
	if got.ExecuteState["pending_tool_call_id"] != "call_1" ||
		got.ExecuteState["pending_tool_name"] != "run_sandbox" {
		t.Errorf("execute state = %+v", got.ExecuteState)
	}
	msgs, ok := got.ExecuteState["messages"].([]any)
	if !ok || len(msgs) != 1 {
		t.Errorf("the suspended conversation did not survive: %+v", got.ExecuteState["messages"])
	}
}

func testActiveIncludesResumed(t *testing.T, s sandbox.PendingStore) {
	// Boot recovery has to SEE a tail that died mid-flight with the previous
	// engine. Nothing else would ever look at that row again, and its paused
	// box would leak for ever.
	mustCreate(t, s, run("t1"))
	if _, _, err := s.ClaimForResume(context.Background(), "t1"); err != nil {
		t.Fatalf("claim: %v", err)
	}
	got, err := s.ListActive(context.Background())
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(got) != 1 || got[0].Status != sandbox.StatusResumed {
		t.Errorf("active = %+v; a resumed tail must stay visible to recovery", got)
	}

	// A terminal run does not.
	if err := s.SetStatus(context.Background(), "t1", sandbox.StatusDone, sandbox.Fence{}); err != nil {
		t.Fatalf("set done: %v", err)
	}
	if got, _ := s.ListActive(context.Background()); len(got) != 0 {
		t.Errorf("a done run is still active: %+v", got)
	}
}

func testAnAnswerFindsTheRunThatAsked(t *testing.T, s sandbox.PendingStore) {
	// A seat can have parked more than one question on one thread, and the
	// answer belongs to the most recent — the person is replying to what
	// they were just asked.
	first, second := run("t1"), run("t2")
	second.CreatedAt = base.Add(time.Minute)
	mustCreate(t, s, first)
	mustCreate(t, s, second)
	for _, id := range []string{"t1", "t2"} {
		if err := s.MarkAwaiting(context.Background(), id,
			sandbox.Clarification{Question: "?" + id}); err != nil {
			t.Fatalf("park %s: %v", id, err)
		}
	}
	got, ok, err := s.FindAwaitingByConversation(context.Background(), "swe", "slack:C1")
	if err != nil || !ok {
		t.Fatalf("find: ok=%v err=%v", ok, err)
	}
	if got.TurnID != "t2" {
		t.Errorf("matched %s, want the most recently parked question", got.TurnID)
	}
	// And a different seat's thread is not this seat's.
	if _, ok, _ := s.FindAwaitingByConversation(context.Background(), "other", "slack:C1"); ok {
		t.Error("another seat's answer matched this seat's run")
	}
}

func testAnAnswerWithNoConversationMatchesNothing(t *testing.T, s sandbox.PendingStore) {
	// Matching by seat alone would hand an unrelated message to whichever
	// run happened to be waiting — and that run would treat it as the
	// answer to its question.
	mustCreate(t, s, run("t1"))
	if err := s.MarkAwaiting(context.Background(), "t1",
		sandbox.Clarification{Question: "?"}); err != nil {
		t.Fatalf("park: %v", err)
	}
	if _, ok, _ := s.FindAwaitingByConversation(context.Background(), "swe", ""); ok {
		t.Error("a message with no conversation matched a parked run")
	}
}

func testTheReaperSeesOnlyOldSnapshots(t *testing.T, s sandbox.PendingStore) {
	// A provider holds a paused box indefinitely and bills for the snapshot,
	// so nothing else would ever reclaim it. What the reaper must NOT see is
	// a fresh pause or a run with no box at all.
	for _, tc := range []struct {
		id     string
		paused time.Time
		hasBox bool
	}{
		{"old", base.Add(-2 * time.Hour), true},
		{"fresh", base.Add(-time.Minute), true},
		{"boxless", base.Add(-2 * time.Hour), false},
	} {
		mustCreate(t, s, run(tc.id))
		if tc.hasBox {
			if err := s.AttachSandbox(context.Background(), tc.id,
				sandbox.BoxRef{SandboxID: "box-" + tc.id}, sandbox.Fence{}); err != nil {
				t.Fatalf("attach %s: %v", tc.id, err)
			}
		}
		if err := s.MarkBoxPaused(context.Background(), tc.id, tc.paused); err != nil {
			t.Fatalf("pause %s: %v", tc.id, err)
		}
	}
	got, err := s.ListPausedBefore(context.Background(), base.Add(-time.Hour))
	if err != nil {
		t.Fatalf("list paused: %v", err)
	}
	if len(got) != 1 || got[0].TurnID != "old" {
		var ids []string
		for _, r := range got {
			ids = append(ids, r.TurnID)
		}
		t.Errorf("reapable = %v, want only the old snapshot with a box", ids)
	}
}

func testListingsAreStable(t *testing.T, s sandbox.PendingStore) {
	// A recovery pass that reordered its work every boot would make two runs
	// of the same failure look like different failures.
	for i, id := range []string{"c", "a", "b"} {
		r := run(id)
		r.CreatedAt = base.Add(time.Duration(i) * time.Second)
		mustCreate(t, s, r)
	}
	var first []string
	for range 5 {
		got, err := s.ListActive(context.Background())
		if err != nil {
			t.Fatalf("list: %v", err)
		}
		var ids []string
		for _, r := range got {
			ids = append(ids, r.TurnID)
		}
		if first == nil {
			first = ids
			continue
		}
		if len(ids) != len(first) {
			t.Fatalf("listing length changed: %v then %v", first, ids)
		}
		for i := range ids {
			if ids[i] != first[i] {
				t.Fatalf("listing order changed: %v then %v", first, ids)
			}
		}
	}
}

// The reaper's flip is the AUTHORITY for the whole reap — it decides whether
// the box gets destroyed — so two reapers racing must produce exactly one
// destruction.
func testAPauseExpiresExactlyOnce(t *testing.T, s sandbox.PendingStore) {
	mustCreate(t, s, run("t1"))
	park(t, s, "t1")

	const racers = 10
	var wins atomic.Int32
	var wg sync.WaitGroup
	start := make(chan struct{})
	for range racers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			won, err := s.ExpirePause(context.Background(), "t1")
			if err != nil {
				t.Errorf("ExpirePause: %v", err)
				return
			}
			if won {
				wins.Add(1)
			}
		}()
	}
	close(start)
	wg.Wait()

	if got := wins.Load(); got != 1 {
		t.Fatalf("%d of %d reapers won the flip, want exactly 1 — each winner kills a box", got, racers)
	}
	got, _, err := s.Get(context.Background(), "t1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Status != sandbox.StatusReseed {
		t.Fatalf("status = %q, want %q", got.Status, sandbox.StatusReseed)
	}
}

// Every other paused box belongs to a tail that is actively being driven, and
// expiring one from the reaper would kill it out from under live work.
func testOnlyAParkedRunCanExpire(t *testing.T, s sandbox.PendingStore) {
	for _, status := range []string{
		sandbox.StatusRunning, sandbox.StatusResumed,
		sandbox.StatusDone, sandbox.StatusFailed, sandbox.StatusReseed,
	} {
		mustCreate(t, s, run(status))
		if err := s.SetStatus(context.Background(), status, status, sandbox.Fence{}); err != nil {
			t.Fatalf("SetStatus: %v", err)
		}
		won, err := s.ExpirePause(context.Background(), status)
		if err != nil {
			t.Fatalf("ExpirePause(%s): %v", status, err)
		}
		if won {
			t.Fatalf("a run in %q was expired by the pause reaper", status)
		}
	}
}

// The answer that un-parks a run and the reaper that expires it are the same
// race, from the two sides: whichever lands first, the other must lose.
func testAnAnsweredRunCannotBeExpiredUnderTheResume(t *testing.T, s sandbox.PendingStore) {
	mustCreate(t, s, run("t1"))
	park(t, s, "t1")

	if _, won, err := s.ClaimForResume(context.Background(), "t1"); err != nil || !won {
		t.Fatalf("ClaimForResume = %v, %v", won, err)
	}
	won, err := s.ExpirePause(context.Background(), "t1")
	if err != nil {
		t.Fatalf("ExpirePause: %v", err)
	}
	if won {
		t.Fatal("the reaper expired a run whose answer had already claimed it — it would destroy the box the resume is reconnecting to")
	}
}

// park moves a seeded run to awaiting_clarification with a box attached.
func park(t *testing.T, s sandbox.PendingStore, turnID string) {
	t.Helper()
	ctx := context.Background()
	if err := s.AttachSandbox(ctx, turnID, sandbox.BoxRef{
		SandboxID: "box-" + turnID, CodingAgent: "claude-code", PauseTTLSec: 1800,
	}, sandbox.Fence{}); err != nil {
		t.Fatalf("AttachSandbox: %v", err)
	}
	if err := s.MarkAwaiting(ctx, turnID, sandbox.Clarification{
		Question: "which branch?", Audience: "requester",
	}); err != nil {
		t.Fatalf("MarkAwaiting: %v", err)
	}
}

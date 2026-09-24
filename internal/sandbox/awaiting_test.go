package sandbox_test

import (
	"context"
	"encoding/json"
	"sync/atomic"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/sandbox"
)

// --- the index an answer is matched through --------------------------------

// countedRuns counts the reads of every run record a store makes, so a case can
// say which questions take one.
type countedRuns struct {
	*memory.Fleet
	listings atomic.Int32
}

func (c *countedRuns) SandboxRuns(ctx context.Context) ([]coord.Record, error) {
	c.listings.Add(1)
	return c.Fleet.SandboxRuns(ctx)
}

// parked is a run of seat swe parked on a question on conversation chat:C1.
func parked(t *testing.T, store *sandbox.CoordStore, turnID string) sandbox.PendingRun {
	t.Helper()
	ctx := t.Context()
	if err := store.BeginLaunch(ctx, sandbox.PendingRun{
		TurnID: turnID, AgentHandle: "swe", ConversationKey: "chat:C1",
	}, sandbox.Fence{}); err != nil {
		t.Fatalf("BeginLaunch: %v", err)
	}
	if ok, err := store.MarkSuspended(ctx, turnID, map[string]any{"version": 2}); err != nil || !ok {
		t.Fatalf("MarkSuspended = %v, %v", ok, err)
	}
	if err := store.MarkAwaiting(ctx, turnID, sandbox.Clarification{Question: "which branch?"}); err != nil {
		t.Fatalf("MarkAwaiting: %v", err)
	}
	run, found, err := store.Get(ctx, turnID)
	if err != nil || !found {
		t.Fatalf("Get = %v, %v", found, err)
	}
	return run
}

// entries is every entry of the index.
func entries(t *testing.T, fleet coord.AwaitingRuns) []coord.AwaitingRun {
	t.Helper()
	all, err := fleet.AllAwaitingRuns(t.Context())
	if err != nil {
		t.Fatalf("AllAwaitingRuns: %v", err)
	}
	return all
}

// AN ANSWER IS ONE KEYED READ, NOT A READ OF EVERY RUN. Every delivery a seat
// receives on a conversation is offered to the run parked on it, so the lookup
// is on the path of every message — and a listing of the fleet's runs, each up
// to a record's ceiling, is the one read it must not take.
func TestAnAnswerIsMatchedWithoutReadingEveryRun(t *testing.T) {
	t.Parallel()
	fleet := &countedRuns{Fleet: memory.NewFleet()}
	store := sandbox.NewCoordStore(fleet)
	parked(t, store, "t1")

	got, found, err := store.FindAwaitingByConversation(t.Context(), "swe", "chat:C1")
	if err != nil || !found || got.TurnID != "t1" {
		t.Fatalf("FindAwaitingByConversation = %+v, %v, %v, want the parked run", got, found, err)
	}
	if _, found, _ := store.FindAwaitingByConversation(t.Context(), "swe", "chat:C2"); found {
		t.Error("a delivery on another conversation was matched to the run")
	}
	if n := fleet.listings.Load(); n != 0 {
		t.Errorf("matching an answer read every run %d times, want none", n)
	}
}

// A STALE ENTRY RESOLVES TO NO RUN, NEVER TO A WRONG ONE. An entry is filed
// before the park and dropped after the finish, so it can name a run that has
// moved on: one claimed by its answer is not waiting, and must not be found
// again, until a failed resume hands it back.
func TestAnEntryWhoseRunMovedOnIsNotAnAnswer(t *testing.T) {
	t.Parallel()
	fleet := memory.NewFleet()
	store := sandbox.NewCoordStore(fleet)
	run := parked(t, store, "t1")

	claimed, won, err := store.ClaimForResume(t.Context(), "t1", sandbox.AnswerTail(run.LaunchID))
	if err != nil || !won {
		t.Fatalf("ClaimForResume = %v, %v", won, err)
	}
	if _, found, err := store.FindAwaitingByConversation(t.Context(), "swe", "chat:C1"); err != nil || found {
		t.Fatalf("a claimed run was found as waiting (%v, %v): a second answer would be handed to it",
			found, err)
	}
	released, err := store.ReleaseClaim(t.Context(), "t1", sandbox.Release{
		Launch: claimed.LaunchID, To: claimed.ClaimedFrom,
	})
	if err != nil || !released {
		t.Fatalf("ReleaseClaim = %v, %v", released, err)
	}
	if _, found, err := store.FindAwaitingByConversation(t.Context(), "swe", "chat:C1"); err != nil || !found {
		t.Errorf("a run handed back to its question was not found (%v, %v): its entry went with "+
			"the claim", found, err)
	}
}

// THE INDEX ENDS WITH THE RUN, and an entry that outlives its run — a finish
// whose drop failed — is dropped by the next answer that reads it.
func TestAnEndedRunLeavesNoEntry(t *testing.T) {
	t.Parallel()
	fleet := memory.NewFleet()
	store := sandbox.NewCoordStore(fleet)
	parked(t, store, "t1")
	if ended, err := store.Finish(t.Context(), "t1", sandbox.Fence{}); err != nil || !ended {
		t.Fatalf("Finish = %v, %v", ended, err)
	}
	if left := entries(t, fleet); len(left) != 0 {
		t.Errorf("the ended run's entry is still filed: %+v", left)
	}

	stale := coord.AwaitingRun{Handle: "swe", Conversation: "chat:C1", TurnID: "t-gone"}
	if err := fleet.FileAwaitingRun(t.Context(), stale); err != nil {
		t.Fatalf("FileAwaitingRun: %v", err)
	}
	if _, found, err := store.FindAwaitingByConversation(t.Context(), "swe", "chat:C1"); err != nil || found {
		t.Fatalf("an entry naming no run was answered as one (%v, %v)", found, err)
	}
	if left := entries(t, fleet); len(left) != 0 {
		t.Errorf("an entry naming no run survived the answer that read it: %+v", left)
	}
}

// A RUN AN OLDER BUILD PARKED HAS NO ENTRY: that build flips the row and files
// nothing. A build with the index files it when it lists the run — the
// completion poll's every tick, and the recovery pass of a node taking the seat
// — and from then an answer finds it.
func TestARunParkedWithoutAnEntryIsFoundOnceAListingIndexesIt(t *testing.T) {
	t.Parallel()
	for _, seat := range []string{"", "swe"} {
		fleet := memory.NewFleet()
		store := sandbox.NewCoordStore(fleet)
		older := sandbox.PendingRun{
			TurnID: "t1", AgentHandle: "swe", ConversationKey: "chat:C1",
			Status: sandbox.StatusAwaiting, LaunchID: "launch-1",
			ExecuteState: map[string]any{"version": 2}, CreatedAt: time.Now().UTC(),
		}
		raw, err := json.Marshal(older)
		if err != nil {
			t.Fatalf("Marshal: %v", err)
		}
		if created, err := fleet.CreateSandboxRun(t.Context(), "t1", raw); err != nil || !created {
			t.Fatalf("CreateSandboxRun = %v, %v", created, err)
		}
		if _, found, _ := store.FindAwaitingByConversation(t.Context(), "swe", "chat:C1"); found {
			t.Fatal("the premise: a run parked with no entry is found without one")
		}

		listing, err := store.ListActive(t.Context())
		if seat != "" {
			listing, err = store.ListActiveForSeat(t.Context(), seat)
		}
		if err != nil {
			t.Fatalf("listing: %v", err)
		}
		if err := store.IndexAwaiting(t.Context(), seat, listing); err != nil {
			t.Fatalf("IndexAwaiting(%q): %v", seat, err)
		}
		if got, found, err := store.FindAwaitingByConversation(t.Context(), "swe", "chat:C1"); err != nil ||
			!found || got.TurnID != "t1" {
			t.Errorf("seat %q: after the listing indexed it, the answer found %+v, %v, %v", seat, got, found, err)
		}
	}
}

// A LISTING IS A SNAPSHOT: an entry whose run it does not hold is dropped only
// once the run, read again, is gone — a run launched and parked after the
// listing was taken keeps its entry.
func TestAnEntryTheListingDoesNotHoldIsKeptWhileItsRunExists(t *testing.T) {
	t.Parallel()
	fleet := memory.NewFleet()
	store := sandbox.NewCoordStore(fleet)
	parked(t, store, "t1")
	gone := coord.AwaitingRun{Handle: "swe", Conversation: "chat:C1", TurnID: "t-gone"}
	if err := fleet.FileAwaitingRun(t.Context(), gone); err != nil {
		t.Fatalf("FileAwaitingRun: %v", err)
	}

	if err := store.IndexAwaiting(t.Context(), "", nil); err != nil {
		t.Fatalf("IndexAwaiting: %v", err)
	}
	left := entries(t, fleet)
	if len(left) != 1 || left[0].TurnID != "t1" {
		t.Errorf("entries = %+v, want the parked run's kept and the ended one's dropped", left)
	}
}

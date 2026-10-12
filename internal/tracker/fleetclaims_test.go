package tracker_test

import (
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/coord/kv"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/tracker"
)

// productionSeatTTL is the seat lease TTL a fleet's coordination store is
// opened at unless coordination.lease_ttl_seconds says otherwise
// (seat.SeatLeaseTTL, restated rather than imported: what matters here is the
// number the store is held to, not the package that chose it).
const productionSeatTTL = 45 * time.Second

// THE WALKS AND THE BULK ADMISSION RUN ON THE FLEET'S OWN COORDINATION STORE,
// at the seat lease TTL a fleet really opens it with.
//
// Every other case takes its claims from the in-memory twin, which keeps any
// deadline it is asked for. The embedded KV store a fleet runs held every
// lease outside the duties to its seat lease bucket's age, so at the shipped
// 45 seconds the walks' 60-second claim was refused: every cross-project move
// and every merge on every `embedded-kv` fleet failed before its first append
// with an error naming a TTL, and a bulk edit's admission failed OPEN, so the
// fleet-wide bound on bulk edits was silently gone. Nothing single-node could
// see it.
//
// The store is the engine's own construction — the coordination KV over the
// broker's own JetStream client — so a claim lands where a fleet's does.
func TestCrossProjectMoveAndMergeRunOnEmbeddedKVCoordination(t *testing.T) {
	t.Parallel()
	r := moveFixture(t, "m-kid")
	claims, err := kv.Open(t.Context(), r.broker.JetStream(), kv.Config{TTL: productionSeatTTL})
	if err != nil {
		t.Fatalf("open the coordination store: %v", err)
	}
	r.claimOn(claims)

	// THE MOVE: the whole subtree re-homed, under a claim the store took
	// and gave back.
	if _, err := r.writer.MoveTaskToProject(t.Context(),
		statelog.NewOpID(time.Now(), "move"), "m-root", "OPS", nil); err != nil {
		t.Fatalf("a cross-project move on embedded-kv coordination: %v", err)
	}
	r.drain()
	for _, id := range []string{"m-root", "m-kid"} {
		if got := oneTask(t, r, id).Project; got != "OPS" {
			t.Errorf("%s is in %q after the move, want OPS", id, got)
		}
	}
	if lease := claimOf(t, r, tracker.MoveClaim("m-root")); lease != nil {
		t.Errorf("the finished move still holds %s: %+v", tracker.MoveClaim("m-root"), lease)
	}

	// THE MERGE: a duplicate in the target folded into the moved root, its
	// subtask carried across.
	dup := newTask("dup")
	dup.Project = "OPS"
	if _, err := r.writer.CreateTask(t.Context(), "op-dup", dup, nil); err != nil {
		t.Fatalf("file the duplicate: %v", err)
	}
	r.drain()
	parent := "dup"
	kid := newTask("dup-kid")
	kid.Project, kid.Parent, kid.Depth = "OPS", &parent, 1
	if _, err := r.writer.CreateTask(t.Context(), "op-dup-kid", kid, nil); err != nil {
		t.Fatalf("file the duplicate's subtask: %v", err)
	}
	r.drain()
	if _, err := r.writer.MergeDuplicates(t.Context(),
		statelog.NewOpID(time.Now(), "merge"), "dup", "m-root", true, nil); err != nil {
		t.Fatalf("a merge on embedded-kv coordination: %v", err)
	}
	r.drain()
	if got := oneTask(t, r, "dup").Status; got != tracker.StatusCancelled {
		t.Errorf("the duplicate is %q after the merge, want cancelled", got)
	}
	if got := parentOf(r.task(t, "dup-kid")); got != "m-root" {
		t.Errorf("the duplicate's subtask is under %q after the merge, want m-root", got)
	}
	if lease := claimOf(t, r, tracker.MergeClaim("dup")); lease != nil {
		t.Errorf("the finished merge still holds %s: %+v", tracker.MergeClaim("dup"), lease)
	}

	// THE BULK ADMISSION, at the largest bulk a call carries before the
	// applier's drain is measured: twice its projected apply time at the
	// one-row-a-second floor, which is well past the seat TTL. It must be a
	// lease the store HOLDS, because one it refused is admitted anyway —
	// the admission fails open — and a fleet with no bulk bound looks
	// exactly like one whose bulks never collide.
	release, err := r.writer.Admit(t.Context(), tracker.MaxBulkTasks)
	if err != nil {
		t.Fatalf("admit a bulk of %d tasks: %v", tracker.MaxBulkTasks, err)
	}
	if lease := claimOf(t, r, tracker.BulkClaim()); lease == nil {
		t.Errorf("a bulk of %d tasks was admitted with no lease held on %s: the store "+
			"refused the admission's TTL and the bulk was waved through unbounded",
			tracker.MaxBulkTasks, tracker.BulkClaim())
	}
	release()
	if lease := claimOf(t, r, tracker.BulkClaim()); lease != nil {
		t.Errorf("the released bulk admission is still held: %+v", lease)
	}
}

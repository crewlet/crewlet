package tracker_test

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/tracker"
)

// THE DUTY LEAVES A MERGE ITS OWN NODE IS WALKING, and releases nothing.
//
// On a single-node deployment — the default — a live merge and the duty's
// completion of it run on the same node, and both took the walk's claim under
// that node's id. Coordination reads an owner that already holds a lease as the
// holder RENEWING it, so the duty was told yes beside the live walk: it
// re-parented and closed the duplicate under its own operation, and then
// released the claim in the middle of the walk it had just finished for it.
func TestTheDutyLeavesAMergeItsOwnNodeIsWalking(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	for _, id := range []string{"keep", "held"} {
		filedTask(t, r, id)
	}
	markMerge(t, r, "held", "keep")
	release, err := r.writer.Hold(t.Context(), tracker.MergeClaim("held"))
	if err != nil {
		t.Fatalf("hold the live walk's claim: %v", err)
	}
	before := claimOf(t, r, tracker.MergeClaim("held"))

	swept, err := trackerWorker(t, r).Tick(t.Context())
	if err != nil {
		t.Fatalf("Tick: %v", err)
	}
	if got := swept["tracker_abandoned_merges"]; got != 0 {
		t.Errorf("the duty finished %d merge(s) beside the live walk on its own "+
			"node, want none", got)
	}
	r.drain()
	if held := r.task(t, "held"); !held.Task.Merging ||
		held.Task.Status == tracker.StatusCancelled {
		t.Errorf("the duty finished the merge its own node is walking — it is "+
			"%q, merging %v", held.Task.Status, held.Task.Merging)
	}
	if after := claimOf(t, r, tracker.MergeClaim("held")); after == nil ||
		after.Owner != before.Owner || after.Epoch != before.Epoch {
		t.Fatalf("the live walk's claim is %+v after the tick, want its own %+v "+
			"— the duty released a claim it never held", after, before)
	}

	release()
	if swept, err = trackerWorker(t, r).Tick(t.Context()); err != nil {
		t.Fatalf("Tick: %v", err)
	}
	if got := swept["tracker_abandoned_merges"]; got != 1 {
		t.Errorf("the duty finished %d merge(s) once the walk let go, want 1", got)
	}
}

// THE DUTY LEAVES A MOVE ITS OWN NODE IS WALKING, for the same reason.
func TestTheDutyLeavesAMoveItsOwnNodeIsWalking(t *testing.T) {
	t.Parallel()
	r := moveFixture(t, "m-kid-a", "m-kid-b")
	stopMoveAt(t, r, "m-kid-b")
	release, err := r.writer.Hold(t.Context(), tracker.MoveClaim("m-root"))
	if err != nil {
		t.Fatalf("hold the live walk's claim: %v", err)
	}
	before := claimOf(t, r, tracker.MoveClaim("m-root"))

	swept, err := trackerWorker(t, r).Tick(t.Context())
	if err != nil {
		t.Fatalf("Tick: %v", err)
	}
	if got := swept["tracker_abandoned_moves"]; got != 0 {
		t.Errorf("the duty finished %d move(s) beside the live walk on its own "+
			"node, want none", got)
	}
	r.drain()
	if !oneTask(t, r, "m-root").Moving || oneTask(t, r, "m-kid-b").Project != "ENG" {
		t.Error("the duty finished the move its own node is walking")
	}
	if after := claimOf(t, r, tracker.MoveClaim("m-root")); after == nil ||
		after.Owner != before.Owner || after.Epoch != before.Epoch {
		t.Fatalf("the live walk's claim is %+v after the tick, want its own %+v",
			after, before)
	}
	release()
}

// TWO WALKS OF ONE THING ON ONE NODE: the second is refused, as a peer's is,
// and each claim names the node it runs on.
func TestASecondWalkOnTheSameNodeIsRefused(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	for _, resource := range []string{tracker.MoveClaim("t-1"), tracker.MergeClaim("t-1")} {
		release, err := r.writer.Hold(t.Context(), resource)
		if err != nil {
			t.Fatalf("take %s: %v", resource, err)
		}
		if _, err := r.writer.Hold(t.Context(), resource); !errors.Is(err, statelog.ErrUnavailable) {
			t.Errorf("a second walk of %s on the node already walking it got %v, "+
				"want a refusal wrapping statelog.ErrUnavailable", resource, err)
		}
		if lease := claimOf(t, r, resource); lease == nil ||
			!strings.HasPrefix(lease.Owner, "node-a/") {
			t.Errorf("%s is held by %+v, want an owner naming this node", resource, lease)
		}
		release()
		again, err := r.writer.Hold(t.Context(), resource)
		if err != nil {
			t.Fatalf("take %s once the first walk let go: %v", resource, err)
		}
		again()
	}
}

// A SECOND BULK ON THE SAME NODE IS REFUSED rather than renewing the first
// one's admission — and releasing either names only its own.
func TestASecondBulkOnTheSameNodeIsRefused(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	release, err := r.writer.Admit(t.Context(), 10)
	if err != nil {
		t.Fatalf("admit the first bulk: %v", err)
	}
	if _, err := r.writer.Admit(t.Context(), 10); !errors.Is(err, tracker.ErrBulkInFlight) {
		t.Fatalf("a second bulk on the node already running one got %v, want "+
			"ErrBulkInFlight", err)
	}
	release()
	again, err := r.writer.Admit(t.Context(), 10)
	if err != nil {
		t.Fatalf("admit a bulk once the first finished: %v", err)
	}
	again()
}

// A CLAIM THE MIXED-VERSION GATE REFUSED IS NOT ANOTHER WALK OR ANOTHER BULK.
//
// The gate refuses every claim this build makes while a node of an older one
// is live, and both tracker claims read every refusal as a peer's hold: a bulk
// was told a colleague's edit was in flight and to retry "in about a second" —
// the hint read the remaining time off a holder there was none of — and a walk
// was told it was already running. For a whole rolling upgrade. So an older
// build's lease is staged, and a bulk is admitted (it bounds a rate, and fails
// open exactly as it does on an unknown) while a walk fails closed naming the
// upgrade rather than a walk.
func TestAClaimTheGateRefusedIsNotAnotherWalk(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	if lease, _, err := r.claims.TryAcquire(t.Context(), coord.NodeResource("old"), coord.AcquireOptions{
		Owner: "old:1", TTL: time.Hour, Protocol: coord.ProtocolVersion - 1, Ungated: true,
	}); err != nil || lease == nil {
		t.Fatalf("stage an older build: (%v, %v)", lease, err)
	}

	release, err := r.writer.Admit(t.Context(), 10)
	if err != nil {
		t.Fatalf("a bulk beside an older build got %v, want it admitted — the gate's "+
			"refusal names no colleague editing", err)
	}
	release()

	_, err = r.writer.Hold(t.Context(), tracker.MoveClaim("t-1"))
	switch {
	case !errors.Is(err, statelog.ErrUnavailable):
		t.Fatalf("a walk beside an older build got %v, want a refusal wrapping "+
			"statelog.ErrUnavailable", err)
	case strings.Contains(err.Error(), "another walk"):
		t.Fatalf("a walk the gate refused was told another walk holds it: %v", err)
	case !strings.Contains(err.Error(), "older build"):
		t.Fatalf("a walk the gate refused does not say an older build is live: %v", err)
	}
}

func claimOf(t *testing.T, r *roundTrip, resource string) *coord.Lease {
	t.Helper()
	lease, err := r.claims.Get(t.Context(), resource)
	if err != nil {
		t.Fatalf("read %s: %v", resource, err)
	}
	return lease
}

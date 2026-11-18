package worklet_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/worklet"
)

// THE DUTY LEAVES A MERGE ITS OWN NODE IS WALKING, and releases nothing.
//
// On a single node (the default) a live merge and the duty's completion of it
// run side by side, and coordination reads an owner already holding a lease as
// RENEWING it. A claim under the bare node id would tell the duty yes beside
// the live walk: it would finish the merge under its own operation and release
// the claim mid-walk.
func TestTheDutyLeavesAMergeItsOwnNodeIsWalking(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	for _, id := range []string{"keep", "held"} {
		filedTask(t, r, id)
	}
	markMerge(t, r, "held", "keep")
	release, err := r.writer.Hold(t.Context(), worklet.MergeClaim("held"))
	if err != nil {
		t.Fatalf("hold the live walk's claim: %v", err)
	}
	before := claimOf(t, r, worklet.MergeClaim("held"))

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
		held.Task.Status == worklet.StatusCancelled {
		t.Errorf("the duty finished the merge its own node is walking — it is "+
			"%q, merging %v", held.Task.Status, held.Task.Merging)
	}
	if after := claimOf(t, r, worklet.MergeClaim("held")); after == nil ||
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
	release, err := r.writer.Hold(t.Context(), worklet.MoveClaim("m-root"))
	if err != nil {
		t.Fatalf("hold the live walk's claim: %v", err)
	}
	before := claimOf(t, r, worklet.MoveClaim("m-root"))

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
	if after := claimOf(t, r, worklet.MoveClaim("m-root")); after == nil ||
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
	for _, resource := range []string{worklet.MoveClaim("t-1"), worklet.MergeClaim("t-1")} {
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

// A CLAIM THE MIXED-VERSION GATE REFUSED IS NOT ANOTHER WALK.
//
// The gate refuses every claim while a node on a lower lease protocol is live;
// read as a peer's hold, the caller would wait for a walk nobody started for
// as long as that node stayed. The walk fails closed naming the lower-protocol
// node.
func TestAClaimTheGateRefusedIsNotAnotherWalk(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	stageAnOlderNode(t, r)

	_, err := r.writer.Hold(t.Context(), worklet.MoveClaim("t-1"))
	switch {
	case !errors.Is(err, statelog.ErrUnavailable):
		t.Fatalf("a walk beside a lower-protocol node got %v, want a refusal wrapping "+
			"statelog.ErrUnavailable", err)
	case strings.Contains(err.Error(), "another walk"):
		t.Fatalf("a walk the gate refused was told another walk holds it: %v", err)
	case !strings.Contains(err.Error(), "lower lease protocol"):
		t.Fatalf("a walk the gate refused does not say a lower-protocol node is live: %v", err)
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

// stageAnOlderNode puts this rig's writer one lease protocol AHEAD of a live
// node, where the mixed-version gate refuses every claim: a node on this
// build's protocol holds its presence lease, and the writer claims as the
// build after it.
//
// THE WRITER MOVES UP rather than the peer down: this build may speak the
// lowest protocol, and a protocol of zero or less means "this build"
// ([coord.AcquireOptions.Protocol]), a peer the gate never sees.
func stageAnOlderNode(t *testing.T, r *roundTrip) {
	t.Helper()
	if lease, _, err := r.claims.TryAcquire(t.Context(), coord.NodeResource("old"), coord.AcquireOptions{
		Owner: "old:1", TTL: time.Hour, Protocol: coord.ProtocolVersion, Ungated: true,
	}); err != nil || lease == nil {
		t.Fatalf("stage a node on this build's protocol: (%v, %v)", lease, err)
	}
	r.claimOn(newerClaims{Backend: r.claims})
}

// newerClaims is a coordination store this rig's writer reaches as the build
// after this one: every claim it makes states the next lease protocol.
type newerClaims struct{ coord.Backend }

func (n newerClaims) TryAcquire(ctx context.Context, resource string,
	opts coord.AcquireOptions) (*coord.Lease, coord.Refusal, error) {

	opts.Protocol = coord.ProtocolVersion + 1
	return n.Backend.TryAcquire(ctx, resource, opts)
}

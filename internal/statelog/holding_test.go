package statelog_test

import (
	"database/sql"
	"errors"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/statelog"
)

// GATE 3: A NODE THAT DOES NOT SERVE THE LOG'S PARTITION IS REFUSED BEFORE IT
// DECIDES ANYTHING.
//
// Only a partition's serving holders write its logs, because they are the
// writers the trim counts there: a decision taken from the rows of a partition
// this node is not counted on is the one the floor theorem's premise excludes.
// So the write authority asks before it takes a snapshot — the decision never
// runs, the broker never hears of it — and refuses `not_holder`, which names
// another node as the remedy and is counted as such.
func TestAWriteOnAPartitionThisNodeDoesNotServeIsRefusedBeforeItDecides(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.holding.Stop(logOf(probeDomain{}).Partition)
	decided := 0
	subject := probeSubject("a")
	_, err := h.pub.Publish(t.Context(), statelog.Request{
		Subject: subject, Scope: statelog.ScopeSet{Paths: []string{subject.String()}},
		OpID: "op-not-holder", Pattern: statelog.PatternArbitrated,
		Decide: func(_ *sql.Tx, stamp statelog.Stamp) (statelog.Decision, error) {
			decided++
			return statelog.Decision{Payload: probeRecord(stamp, "op-not-holder", "x"), Version: 1}, nil
		},
	})
	var refusal *statelog.Unavailable
	switch {
	case !errors.Is(err, statelog.ErrNotHolder) || !errors.As(err, &refusal):
		t.Fatalf("a write on a partition this node does not serve was answered %v, "+
			"want a refusal carrying %v", err, statelog.ErrNotHolder)
	case refusal.Reason != statelog.ReasonNotHolder || refusal.OpID != "op-not-holder":
		t.Fatalf("the refusal is %+v, want reason %q under the write's own operation "+
			"id, so the node that serves the partition finishes it", refusal,
			statelog.ReasonNotHolder)
	case decided != 0 || h.rows.snapshots() != 0:
		t.Fatalf("the refused write took %d snapshot(s) and decided %d time(s) — a node "+
			"that does not serve the partition never decides from its rows",
			h.rows.snapshots(), decided)
	case h.appends.appends.Load() != 0:
		t.Fatalf("the refused write reached the broker %d time(s)", h.appends.appends.Load())
	}
	if n := refusals(h.metrics)[string(statelog.ReasonNotHolder)]; n != 1 {
		t.Errorf("the refusal counter holds %d %q refusal(s), want 1", n, statelog.ReasonNotHolder)
	}
}

// GATE 3 AGAIN, BEFORE THE APPEND: A WRITE WHOSE NODE STOPS SERVING WHILE IT
// DECIDES IS NEVER APPENDED.
//
// A node that stops serving a partition stops deciding for it, and a write
// already past the first question when that happened must not reach the log.
// The decision here is where the node stops serving.
func TestAWriteWhoseNodeStopsServingWhileItDecidesIsNotAppended(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	subject := probeSubject("a")
	_, err := h.pub.Publish(t.Context(), statelog.Request{
		Subject: subject, Scope: statelog.ScopeSet{Paths: []string{subject.String()}},
		OpID: "op-leaving", Pattern: statelog.PatternArbitrated,
		Decide: func(_ *sql.Tx, stamp statelog.Stamp) (statelog.Decision, error) {
			h.holding.Stop(logOf(probeDomain{}).Partition)
			return statelog.Decision{Payload: probeRecord(stamp, "op-leaving", "x"), Version: 1}, nil
		},
	})
	if !errors.Is(err, statelog.ErrNotHolder) {
		t.Fatalf("a write whose node stopped serving while it decided was answered "+
			"%v, want %v", err, statelog.ErrNotHolder)
	}
	if n := h.appends.appends.Load(); n != 0 {
		t.Fatalf("the write reached the broker %d time(s) after its node stopped "+
			"serving the partition", n)
	}
}

// GATE 3'S THIRD VALUE: A NODE THAT CANNOT TELL WHETHER IT SERVES THE PARTITION
// WRITES NOTHING, and says it could not tell rather than that it does not.
//
// Failing open would be the uncounted writer the gate exists to exclude, and
// reading "cannot tell" as "does not" would send an operator's remedy to
// another node when this one only needs its view back.
func TestAWriteOnANodeThatCannotTellWhetherItServesIsRefused(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	unreadable := errors.New("the node's view of the partitions it serves is unreadable")
	h.holding.Fail(unreadable)
	_, err := h.write(probeSubject("a"), "op-unknown", "x")
	var refusal *statelog.Unavailable
	switch {
	case !errors.As(err, &refusal) || refusal.Reason != statelog.ReasonHoldingUnknown:
		t.Fatalf("a write on a node that cannot tell whether it serves was answered "+
			"%v, want a %q refusal", err, statelog.ReasonHoldingUnknown)
	case !errors.Is(err, unreadable):
		t.Fatalf("the refusal %v does not carry why the node could not tell", err)
	case errors.Is(err, statelog.ErrNotHolder):
		t.Fatalf("the refusal %v says the node does not serve the partition, which "+
			"nothing established", err)
	case h.rows.snapshots() != 0 || h.appends.appends.Load() != 0:
		t.Fatalf("the refused write took %d snapshot(s) and %d append(s)",
			h.rows.snapshots(), h.appends.appends.Load())
	}
}

// A PUBLISHER WITH NO ANSWER TO WHO MAY WRITE ITS LOG IS NEVER BUILT.
//
// Gate 3 is asked of the holding the publisher is built with, so one built
// without would either crash at its first write or — read as "serves
// everything" — write any log it is handed, the uncounted writer the gate
// exists to exclude.
func TestAPublisherIsNotBuiltWithoutAHolding(t *testing.T) {
	t.Parallel()
	deps := statelog.Deps{
		Domain: probeDomain{}, Spec: specOf(probeDomain{}),
		Layout: layoutOf(probeDomain{}), LogID: logOf(probeDomain{}),
		Log: struct{ statelog.Appender }{}, Records: struct{ statelog.LogReader }{},
		Rows:  struct{ statelog.Rows }{},
		Fence: struct{ statelog.Fence }{}, Gates: struct{ statelog.Gates }{},
		Waiter: struct{ statelog.Waiter }{}, Voids: struct{ statelog.Voids }{},
		Identity:  struct{ statelog.Identity }{},
		Admission: noCeiling(t), NodeID: "node-a",
		Generation: func() uint32 { return 1 },
	}
	if _, err := statelog.NewPublisher(deps); err == nil ||
		!strings.Contains(err.Error(), "holding") {
		t.Fatalf("a publisher with no holding was built (%v)", err)
	}
	deps.Holding = statelog.ServesOnly(logOf(probeDomain{}).Partition)
	if _, err := statelog.NewPublisher(deps); err != nil {
		t.Fatalf("a publisher with every dependency was refused: %v", err)
	}
}

// A FIXED SET SERVES WHAT IT NAMES AND NOTHING ELSE, and the empty set serves
// nothing — the answer a node without data gives.
func TestAFixedHoldingServesExactlyItsPartitions(t *testing.T) {
	t.Parallel()
	tracker0 := statelog.PartitionID{Space: statelog.SpaceTracker}
	tracker1 := statelog.PartitionID{Space: statelog.SpaceTracker, Index: 1}
	for _, tc := range []struct {
		holding statelog.Holding
		p       statelog.PartitionID
		want    bool
	}{
		{statelog.ServesOnly(statelog.EstatePartition), statelog.EstatePartition, true},
		{statelog.ServesOnly(statelog.EstatePartition), tracker0, false},
		{statelog.ServesOnly(tracker0, tracker1), tracker1, true},
		{statelog.ServesOnly(), statelog.EstatePartition, false},
	} {
		got, err := tc.holding.Serving(tc.p)
		if err != nil || got != tc.want {
			t.Errorf("Serving(%s) = (%v, %v), want (%v, nil)", tc.p, got, err, tc.want)
		}
	}
}

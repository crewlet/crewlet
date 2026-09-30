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
// A leave stops its node deciding for the partition, and a write already past
// the first question when that happened must not reach the log: the release
// the leave publishes later is what keeps out a write that raced it to the
// broker, and this is what keeps that to the writes truly in flight. The
// decision here is where the node stops serving.
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

// A RELEASE IS THE ONE WRITE A NODE MAKES ON A LOG IT NO LONGER SERVES, AND IT
// MAKES IT ONLY THEN.
//
// A node leaves a partition by ceasing to decide there first and releasing the
// partition's logs second, so everything it decided lies below its release. A
// release while it still serves is one its own later writes land above — each
// dropped on every holder, each caller told the node left when it had not —
// so the write authority refuses it, a programming error in the leave. And the
// release is held to its flag both ways: a write flagged a release that is not
// one would be an ordinary record from a node that stopped serving, and a
// release not flagged as one would be refused as exactly that.
func TestOnlyAReleaseIsWrittenByANodeThatStoppedServing(t *testing.T) {
	t.Parallel()
	partition := logOf(releasingDomain{}).Partition
	release := func(h *harness, opID string, flagged, nodeGate bool,
		payload func(statelog.Stamp) []byte) (statelog.Result, error) {
		subject := statelog.Subject{Kind: releasingGateKind, ID: "node-a"}
		return h.pub.Publish(t.Context(), statelog.Request{
			Subject: subject, Scope: statelog.ScopeSet{Paths: []string{subject.String()}},
			OpID: opID, Pattern: statelog.PatternArbitrated,
			NodeGate: nodeGate, Release: flagged,
			Decide: func(_ *sql.Tx, stamp statelog.Stamp) (statelog.Decision, error) {
				return statelog.Decision{Payload: payload(stamp), Version: 1}, nil
			},
		})
	}
	releaseRecord := func(opID string) func(statelog.Stamp) []byte {
		return func(stamp statelog.Stamp) []byte {
			return releasingRecord(stamp, opID, "node-a", releasingOpRelease)
		}
	}
	for _, tc := range []struct {
		name     string
		serving  bool
		flagged  bool
		nodeGate bool
		payload  func(opID string) func(statelog.Stamp) []byte
		refused  func(error) bool
	}{
		{name: "a release once the node has stopped serving",
			flagged: true, nodeGate: true, payload: releaseRecord},
		{name: "a release while the node still serves", serving: true,
			flagged: true, nodeGate: true, payload: releaseRecord,
			refused: func(err error) bool { return errors.Is(err, statelog.ErrReleaseWhileServing) }},
		{name: "a release not flagged a node gate",
			flagged: true, payload: releaseRecord,
			refused: func(err error) bool { return err != nil && strings.Contains(err.Error(), "not a node gate") }},
		{name: "an eviction flagged a release",
			flagged: true, nodeGate: true,
			payload: func(opID string) func(statelog.Stamp) []byte {
				return func(stamp statelog.Stamp) []byte {
					return releasingRecord(stamp, opID, "node-a", "evict")
				}
			},
			refused: func(err error) bool { return err != nil && strings.Contains(err.Error(), "only a release is") }},
		{name: "a release not flagged one", serving: true, nodeGate: true, payload: releaseRecord,
			refused: func(err error) bool { return err != nil && strings.Contains(err.Error(), "only a release is") }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := newHarnessFor(t, releasingDomain{})
			if !tc.serving {
				h.holding.Stop(partition)
			}
			res, err := release(h, "op-"+strings.ReplaceAll(tc.name, " ", "-"),
				tc.flagged, tc.nodeGate, tc.payload("op-"+strings.ReplaceAll(tc.name, " ", "-")))
			switch {
			case tc.refused == nil && err != nil:
				t.Fatalf("%s was refused: %v", tc.name, err)
			case tc.refused == nil && h.appends.appends.Load() != 1:
				t.Fatalf("%s reached the broker %d time(s), want once (%+v)", tc.name,
					h.appends.appends.Load(), res)
			case tc.refused != nil && !tc.refused(err):
				t.Fatalf("%s was answered (%+v, %v), want it refused", tc.name, res, err)
			case tc.refused != nil && h.appends.appends.Load() != 0:
				t.Fatalf("the refused %s reached the broker %d time(s)", tc.name,
					h.appends.appends.Load())
			}
		})
	}
}

// releasingGateKind and releasingOpRelease are the releasing domain's node
// gate — its eviction subject's kind — and the op that makes one a release.
const (
	releasingGateKind  = "eviction"
	releasingOpRelease = "release"
)

// releasingDomain is the probe domain — its name, its log and its partition —
// with a node gate it can read off its log: an eviction, and the release that
// is the node's own.
type releasingDomain struct{ probeDomain }

func (releasingDomain) InstallsGate(env statelog.Envelope) bool {
	return env.Kind == releasingGateKind
}

func (releasingDomain) NodeGate(env statelog.Envelope) bool {
	return env.Kind == releasingGateKind
}

func (releasingDomain) EvictionSubject(node string) statelog.Subject {
	return statelog.Subject{Kind: releasingGateKind, ID: node}
}

func (releasingDomain) Evicts([]byte) (bool, error) { return true, nil }

func (releasingDomain) Releases(env statelog.Envelope) bool {
	return env.Kind == releasingGateKind && env.Op == releasingOpRelease
}

// releasingRecord is one gate record of the releasing domain about node,
// under op, carrying the stamp its decision was handed.
func releasingRecord(stamp statelog.Stamp, opID, node, op string) []byte {
	subject := statelog.Subject{Kind: releasingGateKind, ID: node}
	return encodeProbe(statelog.Envelope{
		V: 1, Kind: releasingGateKind, Op: op, Subject: subject, OpID: opID,
		Gen: stamp.Gen, Writer: stamp.Writer,
		Scope: statelog.ScopeSet{Paths: []string{subject.String()}},
	})
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

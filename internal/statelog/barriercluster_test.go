package statelog_test

import (
	"context"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/jsprovision"
	js "github.com/crewlet/crewlet/internal/queue/jetstream"
	"github.com/crewlet/crewlet/internal/queue/jetstream/jetstreamtest"
	"github.com/crewlet/crewlet/internal/statelog"
)

// AN ISOLATED FORMER LEADER MUST NOT SERVE A LINEARIZABLE READ.
//
// Why the read index is an APPEND, not a leader check ([statelog.ReadIndex]):
// a cut-off member's own view names a nonempty leader and a stale last
// sequence until the cluster notices, while a barrier commits only with a
// quorum, so a member without one gets no sequence at all.
func TestLinearizableRefusesWhenTheOldLeaderIsIsolated(t *testing.T) {
	t.Parallel()
	c := jetstreamtest.StartPartitionableCluster(t, 3, js.Config{})
	spec := specOf(probeDomain{})

	q := c.Client(t, 0)
	if err := q.EnsureDomainStream(t.Context(), js.DomainStream{
		Name:       spec.Name,
		Subjects:   spec.Subjects,
		MaxBytes:   spec.MaxBytes,
		Duplicates: spec.Duplicates,
	}); err != nil {
		t.Fatalf("provision the log: %v", err)
	}

	// The isolated member's own client: its read index is on the wrong side.
	isolated := c.Client(t, 2)
	log, err := isolated.DomainLog(t.Context(), spec.Name)
	if err != nil {
		t.Fatalf("open the log on the member about to be cut: %v", err)
	}
	gen := func() uint32 { return 1 }
	idx, err := statelog.NewReadIndex(probeDomain{}, specOf(probeDomain{}), log, log.Members, testSigner(t, probeDomain{}), noCeiling(t), probeEncode, gen, nil)
	if err != nil {
		t.Fatalf("NewReadIndex: %v", err)
	}
	// It works while connected, so the refusal below is the partition's,
	// not the wiring's.
	if _, err := idx.Read(t.Context()); err != nil {
		t.Fatalf("a barrier from a connected member: %v", err)
	}

	c.Partition(t, 2)

	// EVERY ATTEMPT FOR TWENTY SECONDS: an isolated former leader was
	// measured reporting itself leader, with a stale last sequence, for up
	// to 15.4 s, so a shorter window would pass the leader check this level
	// replaces.
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
		at, err := idx.Read(ctx)
		cancel()
		if err == nil {
			t.Fatalf("the isolated member served a read index at %s — a "+
				"linearizable read from the wrong side of a partition is the "+
				"exact answer this level exists to refuse", at)
		}
		time.Sleep(250 * time.Millisecond)
	}
}

// AND THE MAJORITY GOES ON SERVING THEM: proving only the isolated member's
// refusal would pass a read index that never works.
func TestLinearizableKeepsWorkingOnTheSurvivingMajority(t *testing.T) {
	t.Parallel()
	c := jetstreamtest.StartPartitionableCluster(t, 3, js.Config{})
	spec := specOf(probeDomain{})

	q := c.Client(t, 0)
	if err := q.EnsureDomainStream(t.Context(), js.DomainStream{
		Name:       spec.Name,
		Subjects:   spec.Subjects,
		MaxBytes:   spec.MaxBytes,
		Duplicates: spec.Duplicates,
	}); err != nil {
		t.Fatalf("provision the log: %v", err)
	}
	log, err := q.DomainLog(t.Context(), spec.Name)
	if err != nil {
		t.Fatalf("open the log: %v", err)
	}
	// THE COUNT AN UNCONFIRMED BARRIER IS JUDGED BY is the stream's own,
	// read at open: three, so a cut is a missed quorum, never a stalled write.
	if got := log.Members(); got != 3 {
		t.Fatalf("the log of a three-member cluster is stored by %d members, want 3", got)
	}
	idx, err := statelog.NewReadIndex(probeDomain{}, specOf(probeDomain{}), log, log.Members, testSigner(t, probeDomain{}), noCeiling(t), probeEncode,
		func() uint32 { return 1 }, nil)
	if err != nil {
		t.Fatalf("NewReadIndex: %v", err)
	}

	c.Partition(t, 2)

	// Two of three is a quorum, so the read index must come back once the
	// pair elects, on NATS's own timeout.
	var last error
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
		at, err := idx.Read(ctx)
		cancel()
		if err == nil {
			if at.Seq == 0 {
				t.Fatalf("the majority answered sequence 0")
			}
			return
		}
		last = err
		time.Sleep(250 * time.Millisecond)
	}
	t.Fatalf("the surviving majority never served a read index: %v — two of "+
		"three replicas is a quorum, so a cluster that cannot commit a barrier "+
		"here has lost more than the member that was cut", last)
}

// A BARRIER IS SERVED ACROSS AN ELECTION OF ITS LOG'S LEADER, on a real
// cluster: the leader's member is cut off (a member shut down hands its
// leadership over at once), and the two left elect one on the broker's own
// timers, four to nine seconds after the last heartbeat. Meanwhile they swallow
// a barrier's append, so a read whose deadline is shorter than the election
// fails at that deadline, and one with time to wait commits a barrier sent again
// once the new leader leads, in one read inside the barrier's budget
// (crewlet/crewlet#2096). A read told the log has no leader: election_test.go.
//
// Mutant: no barrier is sent again while nobody answers — the read runs out its
// budget on the append the survivors swallowed.
func TestABarrierIsServedAcrossAnElectionOfItsLogsLeader(t *testing.T) {
	t.Parallel()
	c := jetstreamtest.StartPartitionableCluster(t, 3, js.Config{})
	spec := specOf(probeDomain{})
	queues := make([]*js.Queue, 3)
	for i := range queues {
		queues[i] = c.Client(t, i)
	}
	if err := queues[0].EnsureDomainStream(t.Context(), js.DomainStream{
		Name:       spec.Name,
		Subjects:   spec.Subjects,
		MaxBytes:   spec.MaxBytes,
		Duplicates: spec.Duplicates,
	}); err != nil {
		t.Fatalf("provision the log: %v", err)
	}
	info, err := jsprovision.LookupStream(t.Context(), queues[0].JetStream(), spec.Name)
	if err != nil {
		t.Fatalf("read who leads the log: %v", err)
	}
	leader := -1
	for i, cfg := range c.Configs {
		if cfg.ServerName == info.Cluster.Leader {
			leader = i
		}
	}
	if leader < 0 {
		t.Fatalf("the log is led by %q, no member of the cluster", info.Cluster.Leader)
	}
	reader := (leader + 1) % len(queues)
	log, err := queues[reader].DomainLog(t.Context(), spec.Name)
	if err != nil {
		t.Fatalf("open the log on member %d: %v", reader, err)
	}
	idx, err := statelog.NewReadIndex(probeDomain{}, spec, log, log.Members,
		testSigner(t, probeDomain{}), noCeiling(t), probeEncode, func() uint32 { return 1 }, nil)
	if err != nil {
		t.Fatalf("NewReadIndex: %v", err)
	}
	if _, err := idx.Read(t.Context()); err != nil {
		t.Fatalf("the premise: a barrier before the election: %v", err)
	}

	c.Partition(t, leader)

	short, cancel := context.WithTimeout(t.Context(), time.Second)
	_, err = idx.Read(short)
	cancel()
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("a barrier whose deadline ended a second into the election answered %v, want "+
			"its deadline's end", err)
	}
	at, err := idx.Read(t.Context())
	if err != nil {
		t.Fatalf("a barrier across the election of the log's leader: %v", err)
	}
	if at.Seq == 0 {
		t.Fatalf("the new leader answered sequence 0")
	}
}

// THE STREAM'S OWN INFO IS NOT ON THE READ PATH, held by a static walk because
// the temptation is permanent: a cluster's reported leader is the member's own
// in-memory value, nonempty and wrong in exactly the case a linearizable read
// refuses, and the obvious saving on the barrier's round trip.
func TestStreamInfoIsNotOnTheReadPath(t *testing.T) {
	t.Parallel()
	// ONE PARSE PER FILE: ParseDir is deprecated for grouping files without
	// their build tags, and go/packages type-checks the world, which a walk
	// over SELECTOR NAMES does not need.
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("list the package directory: %v", err)
	}
	fset := token.NewFileSet()
	var scanned int
	for _, entry := range entries {
		path := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(path, ".go") || isTestFile(path) {
			continue
		}
		file, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		scanned++
		ast.Inspect(file, func(n ast.Node) bool {
			sel, ok := n.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			switch sel.Sel.Name {
			case "CachedInfo", "Leader":
				t.Errorf("%s reads %s — the read index is a quorum-committed "+
					"append precisely because a member's own view of its "+
					"cluster answers with a nonempty leader and a stale last "+
					"sequence in the case this level exists to refuse",
					path, sel.Sel.Name)
			}
			return true
		})
	}
	// A GUARD ASSERTING AN ABSENCE MUST PROVE IT SCANNED SOMETHING, or
	// "found no violations" and "scanned nothing" are the same green.
	if scanned < 5 {
		t.Fatalf("the walk scanned %d file(s) — an absence proved over nothing "+
			"is not a proof", scanned)
	}
}

func isTestFile(path string) bool {
	return len(path) > 8 && path[len(path)-8:] == "_test.go"
}

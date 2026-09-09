package statelog_test

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"testing"
	"time"

	js "github.com/crewlet/crewlet/internal/queue/jetstream"
	"github.com/crewlet/crewlet/internal/queue/jetstream/jetstreamtest"
	"github.com/crewlet/crewlet/internal/statelog"
)

// AN ISOLATED FORMER LEADER MUST NOT SERVE A LINEARIZABLE READ.
//
// This is why the read index is an APPEND and not a leader check. Asking the
// broker for its cluster leader answers from the member's own in-memory state
// with no peer contacted, so the dangerous case — a member that has been cut
// off and does not know it — answers with a NONEMPTY leader name and its own
// stale last sequence, for as long as it takes the rest of the cluster to
// notice.
//
// A barrier cannot lie about it: the server proposes rather than stores, the
// commit returns false until the acknowledgements reach a quorum, and the
// acknowledgement is written from the apply path. A member with no quorum
// therefore gets no sequence at all.
func TestLinearizableRefusesWhenTheOldLeaderIsIsolated(t *testing.T) {
	t.Parallel()
	c := jetstreamtest.StartPartitionableCluster(t, 3, js.Config{})
	spec := probeDomain{}.Stream()

	q := c.Client(t, 0)
	if err := q.EnsureDomainStream(t.Context(), js.DomainStream{
		Name:       spec.Name,
		Subjects:   spec.Subjects,
		MaxBytes:   spec.MaxBytes,
		Duplicates: spec.Duplicates,
	}); err != nil {
		t.Fatalf("provision the log: %v", err)
	}

	// The isolated member's own client, so the read index it drives is the
	// one on the wrong side of the cut.
	isolated := c.Client(t, 2)
	log, err := isolated.DomainLog(t.Context(), spec.Name)
	if err != nil {
		t.Fatalf("open the log on the member about to be cut: %v", err)
	}
	gen := func() uint32 { return 1 }
	idx, err := statelog.NewReadIndex(probeDomain{}, log, probeEncode, gen, nil)
	if err != nil {
		t.Fatalf("NewReadIndex: %v", err)
	}
	// It works while the member is connected, which is what makes the
	// refusal below a statement about the partition rather than about the
	// wiring.
	if _, err := idx.Read(t.Context()); err != nil {
		t.Fatalf("a barrier from a connected member: %v", err)
	}

	c.Partition(t, 2)

	// EVERY ATTEMPT FOR TWENTY SECONDS, and the number is the point. The
	// design this replaces refused on the broker's reported leader, and
	// an isolated former leader was measured answering with its own name
	// and its own stale last sequence for 15.4, 13.1 and 15.4 seconds
	// after the majority had committed a newer write. A window shorter
	// than that would pass against the very check this level exists to
	// replace.
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

// AND THE MAJORITY GOES ON SERVING THEM.
//
// A test that only proved the isolated member refuses would be satisfied by a
// read index that never works at all.
func TestLinearizableKeepsWorkingOnTheSurvivingMajority(t *testing.T) {
	t.Parallel()
	c := jetstreamtest.StartPartitionableCluster(t, 3, js.Config{})
	spec := probeDomain{}.Stream()

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
	idx, err := statelog.NewReadIndex(probeDomain{}, log, probeEncode,
		func() uint32 { return 1 }, nil)
	if err != nil {
		t.Fatalf("NewReadIndex: %v", err)
	}

	c.Partition(t, 2)

	// Two of three is a quorum, so the read index must come back — after
	// the surviving pair elects, which is NATS's own timeout rather than
	// anything this engine sets.
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

// THE STREAM'S OWN INFO IS NOT ON THE READ PATH, and this is a static walk
// because the temptation is permanent.
//
// A cluster's reported leader is the member's own in-memory value, read under
// its own lock with no peer contacted — so it is nonempty and wrong in exactly
// the case a linearizable read exists to refuse. Reaching for it is the
// obvious optimisation the moment somebody notices the barrier costs a round
// trip, and the check it appears to be is one that cannot fire.
func TestStreamInfoIsNotOnTheReadPath(t *testing.T) {
	t.Parallel()
	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, ".", nil, 0)
	if err != nil {
		t.Fatalf("parse the package: %v", err)
	}
	var scanned int
	for _, pkg := range pkgs {
		for path, file := range pkg.Files {
			if isTestFile(path) {
				continue
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

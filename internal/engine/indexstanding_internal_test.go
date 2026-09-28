package engine

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/search"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/store"
)

// THE SEMANTIC INDEX'S DUTY ASKS EVERY NODE THE VECTOR LOG COUNTS which
// records it reads.
//
// A record kind a build's envelope refuses stops that build's applier rather
// than being deferred — which every build before the vector envelope stopped
// validating kinds does with the index's records. So the duty publishes
// nothing about the index until every node applying the log advertises a
// build that reads it: a row from a build that predates the advertisement
// (no version at all), or an offline node's old row, holds it back, exactly as
// either would pin the log's trim — and an operator's eviction releases it.
func TestTheIndexDutyAsksEveryNodeTheVectorLogCounts(t *testing.T) {
	t.Parallel()
	e, _ := aRunningNode(t)
	s := e.native.Load().log
	s.publishPositions(t.Context())

	vectors := s.Domain(search.Domain{}.Name())
	duty := &embedDuty{engine: e, log: vectors, register: s.positions,
		leases: e.backends.Coord, identity: s.identityDomains(), db: e.backends.Store}
	standing, err := duty.standing(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if got := standing.Readers[e.id]; got != search.RecordVersion {
		t.Fatalf("the duty reads this node as reading version %d, want %d",
			got, search.RecordVersion)
	}
	if !standing.Current {
		t.Fatal("a node with nothing on its vector log to apply is read as behind it")
	}

	// A PEER ON A BUILD THAT PREDATES THE ADVERTISEMENT: its row names the
	// vector log and says nothing about what it reads.
	if err := e.backends.Fleet.PutPositions(t.Context(), coord.NodePositions{
		NodeID: "node-old", At: time.Now().UTC(),
		Domains: map[string]coord.DomainPosition{
			search.Domain{}.Name(): {Seq: 1, Generation: vectors.runner.Committed().Generation},
		},
	}); err != nil {
		t.Fatal(err)
	}
	if standing, err = duty.standing(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got, counted := standing.Readers["node-old"]; !counted || got != 0 {
		t.Fatalf("a peer that advertises nothing reads as %d (counted %v) — it "+
			"is a build that cannot read the index's records", got, counted)
	}

	// AND AN OPERATOR'S EVICTION RELEASES IT. The vector log carries no
	// eviction of its own, so the fleet's — every identity log's — is what
	// says the old machine is not coming back; once its fence window has
	// passed, the node leaves the set the index waits for.
	res, err := e.native.Load().gate.Evict(t.Context(), GateRequest{
		Node: "node-old", OpID: "op-evict-old", By: "operator"})
	if err != nil || !res.Complete() {
		t.Fatalf("evict node-old: %v (%+v)", err, res)
	}
	for _, running := range s.identityDomains() {
		waitApplied(t, running)
	}
	tombs, err := duty.evicted(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(tombs) != 1 || tombs[0].NodeID != "node-old" {
		t.Fatalf("the fleet's evictions read as %+v, want node-old's", tombs)
	}
	// AN EVICTION STILL GOING ROUND THE LOGS is not yet the fleet's word: one
	// identity log that has not applied it keeps the node counted. And a log
	// whose evictions cannot be read is an error, never "evicted nowhere".
	lagging := &embedDuty{engine: e, db: e.backends.Store, identity: append(
		slices.Clone(duty.identity),
		&runningLog{domain: noEvictions{s.identityDomains()[0].domain}})}
	if partial, err := lagging.evicted(t.Context()); err != nil || len(partial) != 0 {
		t.Fatalf("an eviction one identity log has not applied read as %+v (%v), "+
			"want none yet", partial, err)
	}
	unreadable := &embedDuty{engine: e, db: e.backends.Store, identity: append(
		slices.Clone(duty.identity), &runningLog{domain: search.Domain{}})}
	if _, err := unreadable.evicted(t.Context()); err == nil {
		t.Fatal("a log that lists no evictions read as one that holds none")
	}
	rows, err := e.backends.Fleet.Positions(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	past := tombs[0].At.Add(statelog.EvictionFenceWindow + time.Second)
	readers := statelog.Readers(statelog.CountedSet(past,
		reportedPositions(rows, search.Domain{}.Name()), nil, tombs))
	if _, counted := readers["node-old"]; counted {
		t.Fatalf("an evicted node still holds the index back: %v", readers)
	}
	if readers[e.id] != search.RecordVersion {
		t.Fatalf("the eviction took this node out of the readers too: %v", readers)
	}
}

// A TICK KEEPS ITS LEASE WHILE IT RUNS, AND STOPS THE MOMENT IT CANNOT.
//
// A training at a large partition runs longer than the interval the lease is
// claimed on, so the tick renews it as it goes; a renewal that is refused —
// or that cannot be answered, since a node that cannot say it holds the duty
// must not publish as its holder — cancels the tick, so the singleton never has
// two writers. And the renewals stop with the tick: their goroutine is the
// tick's, and outliving it would renew a lease nobody is using.
func TestATickStopsWhenItsLeaseCannotBeRenewed(t *testing.T) {
	t.Parallel()
	for name, answer := range map[string]func(int) (bool, error){
		"refused":    func(int) (bool, error) { return false, nil },
		"unanswered": func(int) (bool, error) { return false, coord.ErrUnavailable },
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			var mu sync.Mutex
			calls := 0
			d := &embedDuty{renewEvery: 5 * time.Millisecond,
				claim: func(context.Context) (bool, error) {
					mu.Lock()
					defer mu.Unlock()
					calls++
					if calls < 3 {
						return true, nil
					}
					return answer(calls)
				}}
			tick, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			stop := d.keepClaimed(tick, cancel)
			<-tick.Done()
			stop()
			if !errors.Is(tick.Err(), context.Canceled) {
				t.Fatalf("the tick ended with %v, want cancelled by the lost lease",
					tick.Err())
			}
			mu.Lock()
			held := calls
			mu.Unlock()
			time.Sleep(20 * time.Millisecond)
			mu.Lock()
			defer mu.Unlock()
			if calls != held || held != 3 {
				t.Fatalf("the lease was claimed %d times, then %d — renewals "+
					"continued after the tick ended", held, calls)
			}
		})
	}
}

// noEvictions is an identity log that has not applied any eviction yet.
type noEvictions struct{ statelog.Domain }

func (noEvictions) Evictions(context.Context, *store.DB) ([]statelog.EvictionRow, error) {
	return nil, nil
}

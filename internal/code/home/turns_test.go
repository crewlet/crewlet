package home_test

import (
	"context"
	"slices"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/code/home"
	"github.com/crewlet/crewlet/internal/queue/topics"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/statelog/metrics"
)

// A TURN HOLDS A WHOLE CHAIN: the queue builds its train in its turn, one
// Backstop, so the git a chain is given at the most (code.ChainBuildDeadline:
// its merges and its pack) leaves a third of the turn — twenty seconds — for
// what the turn does around it: its due read before it, and the bundle's
// store and the candidate record after it, each well under a second in the
// common case. A chain given more would be cut by its turn's end with its
// work thrown away, and every turn after it would build it to the same cut.
//
// Mutation: give a chain's pack as long as its merges (code.ChainPackDeadline
// thirty seconds) → sixty of a sixty-second turn, red.
func TestATurnHoldsAWholeChain(t *testing.T) {
	t.Parallel()
	if room := home.Backstop - code.ChainBuildDeadline; room < home.Backstop/3 {
		t.Errorf("a chain is given %s of git in a turn of %s, leaving %s for the turn's reads, store and record; "+
			"want a third of the turn at least", code.ChainBuildDeadline, home.Backstop, room)
	}
}

// busyRepo is an estate whose first read of one repository's due changes
// waits for the case to let it go, keeping the freshness of every read of
// them and how many ran at once.
type busyRepo struct {
	*estate
	repo     string
	mu       sync.Mutex
	reads    []statelog.Freshness
	inFlight int
	most     int
	first    chan struct{}
	release  chan struct{}
}

func (b *busyRepo) Read(ctx context.Context, q code.ReadQuery) (code.ReadAnswer, error) {
	if q.Kind != code.ReadChangesDue || q.ChangesDue == nil || q.ChangesDue.RepoID != b.repo {
		return b.estate.Read(ctx, q)
	}
	b.mu.Lock()
	b.inFlight++
	b.most = max(b.most, b.inFlight)
	b.reads = append(b.reads, q.Fresh)
	n := len(b.reads)
	b.mu.Unlock()
	defer func() {
		b.mu.Lock()
		b.inFlight--
		b.mu.Unlock()
	}()
	if n == 1 {
		close(b.first)
		select {
		case <-b.release:
		case <-ctx.Done():
		}
	}
	return b.estate.Read(ctx, q)
}

// turned is the freshness of every read of the repository's due changes so
// far, and the most that ran at once.
func (b *busyRepo) turned() ([]statelog.Freshness, int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return slices.Clone(b.reads), b.most
}

// A BURST ON A BUSY REPOSITORY COSTS ONE MORE TURN: while a turn of the
// change work holds a repository, records told about it at positions out of
// order schedule ONE more turn of it — after the busy one ends, never beside
// it — read at the FURTHEST position the burst named, so the rows it reads
// hold every record of it; never a turn a record, and never two turns of one
// repository at once.
//
// In a bubble, where synctest.Wait returns once every goroutine but the
// case's is durably blocked: once the burst has been told, a turn it wrongly
// scheduled beside the busy one — the loop's second goroutine is idle — has
// begun its read by then, beside the held one; and once the busy turn is let
// go, every turn the burst scheduled has run by then, while the bubble's
// clock, which holds the loop's passes, has not moved.
//
// Mutations: schedule a busy pair as an idle one, queued at once — a second
// turn runs beside the first, red; keep the last floor asked rather than the
// furthest — the turn after reads at 53, red.
func TestABurstOnABusyRepositoryCostsOneMoreTurn(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		e := newEstate()
		e.hold(repoA, 1, oid("a"), oid("1"), code.RevisionFactsAnswer{MergeBase: oid("1"), PatchID: oid("e"),
			Touched: []string{"a.go"}})
		b := &busyRepo{estate: e, repo: repoA, first: make(chan struct{}), release: make(chan struct{})}
		p := &placement{homes: map[string]string{repoA: "node-a"}}
		n := newNudges()
		loopOver(t, home.Options{Chart: noChart, Estate: b, Placement: asNode{placement: p, node: "node-a"},
			Nudges: n, Cache: unmerged{t: t}, Git: unmerged{t: t}, Chains: unqueued{t: t}, Tasks: unqueued{t: t},
			NodeID: "node-a", Share: units(2)}, time.Hour)
		synctest.Wait()
		if reads, _ := b.turned(); len(reads) != 1 {
			t.Fatalf("the home's first pass read the repository's due changes %d times with the loop parked, "+
				"want its one turn, held", len(reads))
		}
		for _, seq := range []uint64{50, 59, 53} {
			n.subject(t, repoA, seq)
		}
		synctest.Wait()
		if reads, most := b.turned(); len(reads) != 1 || most != 1 {
			t.Fatalf("with the busy turn held the burst began %d reads of the repository's due changes, %d at "+
				"once: want the busy turn's alone, the burst's one turn waiting for it", len(reads), most)
		}

		close(b.release)
		synctest.Wait()
		reads, most := b.turned()
		if len(reads) != 2 {
			t.Fatalf("the repository's change work took %d turns, want the busy one and ONE more for the burst",
				len(reads))
		}
		if most != 1 {
			t.Errorf("%d turns of the repository's change work ran at once, want one", most)
		}
		furthest := statelog.Position{Stream: topics.CodeLogStream, Seq: 59}
		if f := reads[1]; f.Level != statelog.ReadSession || !slices.Equal(f.Floors, []statelog.Position{furthest}) {
			t.Errorf("the turn after the burst read at %+v, want the burst's furthest position %s", f, furthest)
		}
		if left := e.left(repoA); left != 0 {
			t.Errorf("the repository still holds %d due changes once the loop is parked again, want none", left)
		}
	})
}

// stalledReads is an estate whose git reads never answer before their turn's
// context ends.
type stalledReads struct{ *estate }

func (s stalledReads) GitRead(ctx context.Context, _ code.GitQuery) (code.GitAnswer, error) {
	<-ctx.Done()
	return code.GitAnswer{}, ctx.Err()
}

// A TURN PAST ITS BOUND IS CUT, AND LISTED AGAIN: a repository's turn whose
// facts read outlasts its one Backstop is ended there and counted `cut` —
// its goroutine yielded to the next repository — and the next pass lists the
// repository again, its work still due, and works it once more, cut again.
//
// Mutations: run a turn under the loop's own context, unbounded — the read
// never ends and nothing is cut, red; count a cut turn as failed — red.
func TestATurnPastItsBoundIsCutAndListedAgain(t *testing.T) {
	t.Parallel()
	e := newEstate()
	e.hold(repoA, 1, oid("a"), oid("1"), code.RevisionFactsAnswer{MergeBase: oid("1"), PatchID: oid("e"),
		Touched: []string{"a.go"}})
	p := &placement{homes: map[string]string{repoA: "node-a"}}
	_, rec := loopOver(t, home.Options{Chart: noChart,
		Estate: stalledReads{e}, Placement: asNode{placement: p, node: "node-a"},
		Nudges: newNudges(), Cache: unmerged{t: t}, Git: unmerged{t: t}, Chains: unqueued{t: t},
		Tasks: unqueued{t: t}, NodeID: "node-a", Share: units(2)}, 50*time.Millisecond)
	cut := metrics.Attrs{"worker": home.WorkerChanges, "outcome": "cut"}
	deadline := time.Now().Add(10 * time.Second)
	for counted(rec, metrics.CodeHomePasses, cut) < 2 {
		if time.Now().After(deadline) { //fixedwait:measure a poll's own deadline, whose firing only fails the case
			t.Fatalf("the home counted %d of the repository's turns cut in 10s, want the turn past its bound "+
				"cut, and the next pass's turn again", counted(rec, metrics.CodeHomePasses, cut))
		}
		time.Sleep(5 * time.Millisecond) //fixedwait:liveness
	}
	if got := e.countAsked(repoA); got < 2 {
		t.Errorf("the repository's due changes were read %d times, want once a turn: listed again after the cut",
			got)
	}
	if got := counted(rec, metrics.CodeHomePasses, metrics.Attrs{"worker": home.WorkerChanges,
		"outcome": "failed"}); got != 0 {
		t.Errorf("the home counted %d of the repository's turns failed, want the turns past their bound cut", got)
	}
}

// lateReads is an estate whose git reads answer only once their turn's context
// has ended, and then answer as the estate does.
type lateReads struct{ *estate }

func (l lateReads) GitRead(ctx context.Context, q code.GitQuery) (code.GitAnswer, error) {
	<-ctx.Done()
	return l.estate.GitRead(context.WithoutCancel(ctx), q)
}

// A TURN WHOSE WORK FINISHES PAST ITS BOUND DID IT: a repository's turn whose
// facts read answers only once its one Backstop has ended, and whose work then
// finishes, the facts recorded, is counted `worked`. Counted `cut`, it would
// say the rest of its work is listed again by the next pass when none is left,
// and a home whose turns all ran a little long would count none worked (#826).
//
// Mutation: count a turn cut whenever its bound has ended, whatever its work
// answered, and this turn is counted cut, red.
func TestATurnWhoseWorkFinishesPastItsBoundDidIt(t *testing.T) {
	t.Parallel()
	e := newEstate()
	e.hold(repoA, 1, oid("a"), oid("1"), code.RevisionFactsAnswer{MergeBase: oid("1"), PatchID: oid("e"),
		Touched: []string{"a.go"}})
	p := &placement{homes: map[string]string{repoA: "node-a"}}
	_, rec := loopOver(t, home.Options{Chart: noChart,
		Estate: lateReads{e}, Placement: asNode{placement: p, node: "node-a"},
		Nudges: newNudges(), Cache: unmerged{t: t}, Git: unmerged{t: t}, Chains: unqueued{t: t},
		Tasks: unqueued{t: t}, NodeID: "node-a", Share: units(2)}, 50*time.Millisecond)
	e.waitDone(t, repoA, 10*time.Second)
	// COUNTED AS THE TURN ENDS, after the write that emptied its list.
	waitTurns(t, rec, 1)
	if got := counted(rec, metrics.CodeHomePasses, metrics.Attrs{"worker": home.WorkerChanges,
		"outcome": "cut"}); got != 0 {
		t.Errorf("the home counted %d of the repository's turns cut, want the turn whose work finished past "+
			"its bound worked", got)
	}
	if got := counted(rec, metrics.CodeHomePasses, metrics.Attrs{"worker": home.WorkerChanges,
		"outcome": "worked"}); got == 0 {
		t.Errorf("the home counted none of the repository's turns worked, want the turn whose work finished " +
			"past its bound")
	}
}

package home_test

import (
	"context"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/code/home"
	"github.com/crewlet/crewlet/internal/queue/topics"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/statelog/metrics"
)

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
// Mutations: schedule a busy pair as an idle one, queued at once — a second
// turn runs beside the first, red; keep the last floor asked rather than the
// furthest — the turn after reads at 53, red.
func TestABurstOnABusyRepositoryCostsOneMoreTurn(t *testing.T) {
	t.Parallel()
	e := newEstate()
	e.hold(repoA, 1, oid("a"), oid("1"), code.RevisionFactsAnswer{MergeBase: oid("1"), PatchID: oid("e"),
		Touched: []string{"a.go"}})
	b := &busyRepo{estate: e, repo: repoA, first: make(chan struct{}), release: make(chan struct{})}
	p := &placement{homes: map[string]string{repoA: "node-a"}}
	n := newNudges()
	loopOver(t, home.Options{Estate: b, Placement: asNode{placement: p, node: "node-a"}, Nudges: n,
		Cache: unmerged{t: t}, Git: unmerged{t: t}, Chains: unqueued{t: t}, Tasks: unqueued{t: t}, NodeID: "node-a",
		Share: units(2)}, time.Hour)
	select {
	case <-b.first:
	case <-time.After(10 * time.Second):
		t.Fatal("the home's first pass worked no turn of the repository in 10s")
	}
	for _, seq := range []uint64{50, 59, 53} {
		n.subject(t, repoA, seq)
	}
	close(b.release)
	e.waitDone(t, repoA, 10*time.Second)
	deadline := time.Now().Add(10 * time.Second)
	for {
		if reads, _ := b.turned(); len(reads) >= 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the burst on the busy repository scheduled no turn after it in 10s")
		}
		time.Sleep(5 * time.Millisecond)
	}
	// ANY FURTHER TURN the burst scheduled would begin within moments.
	time.Sleep(200 * time.Millisecond)
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
	_, rec := loopOver(t, home.Options{Estate: stalledReads{e}, Placement: asNode{placement: p, node: "node-a"},
		Nudges: newNudges(), Cache: unmerged{t: t}, Git: unmerged{t: t}, Chains: unqueued{t: t},
		Tasks: unqueued{t: t}, NodeID: "node-a", Share: units(2)}, 50*time.Millisecond)
	cut := metrics.Attrs{"worker": home.WorkerChanges, "outcome": "cut"}
	deadline := time.Now().Add(10 * time.Second)
	for counted(rec, metrics.CodeHomePasses, cut) < 2 {
		if time.Now().After(deadline) {
			t.Fatalf("the home counted %d of the repository's turns cut in 10s, want the turn past its bound "+
				"cut, and the next pass's turn again", counted(rec, metrics.CodeHomePasses, cut))
		}
		time.Sleep(5 * time.Millisecond)
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

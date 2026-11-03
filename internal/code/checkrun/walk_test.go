package checkrun_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/code/checkrun"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/statelog"
)

// queueReads is a data node's code domain, recording the cell every read of
// the queue a runner made named.
type queueReads struct {
	checkrun.Estate
	mu    sync.Mutex
	cells []code.CheckCell
}

func (e *queueReads) Read(ctx context.Context, q code.ReadQuery) (code.ReadAnswer, error) {
	if q.Kind == code.ReadChecksQueued {
		e.mu.Lock()
		e.cells = append(e.cells, q.ChecksQueued.RunIn)
		e.mu.Unlock()
	}
	return e.Estate.Read(ctx, q)
}

// count is how many reads of the queue the runner has made.
func (e *queueReads) count() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return len(e.cells)
}

// A NODE READS NO QUEUE IT CANNOT CLAIM FROM: its catalogue holds an e2b cell
// — its seats' — but its company runs checks only in containers, and with its
// one container slot held a nudged walk reads nothing of the queue, however
// much of it waits; once the slot frees, the walk reads again and claims the
// next attempt.
//
// Mutation: count a cell the catalogue configures as room whatever the
// company allows — the e2b cell keeps every walk reading the queue, red.
func TestAFullNodeReadsNoQueueItCannotClaimFrom(t *testing.T) {
	t.Parallel()
	f := newFleet(t)
	f.company.Code.Checks.AllowRunIn = []config.Placement{config.PlacementContainer}
	var keys []code.CheckKey
	for _, oid := range []string{"1", "2", "3"} {
		keys = append(keys, f.queue(oidOf(oid), def("unit", time.Minute)))
	}
	reads := &queueReads{Estate: estate{f.d}}
	o := f.options("git-a", 1, &order{nodes: []string{"git-a"}})
	o.Estate = reads
	// NO BACKSTOP WITHIN THE CASE: a walk at the start, at each nudge and at
	// each check's ending, and nothing else.
	o.Timing.Backstop = time.Hour
	r := f.runner(o)
	first := f.container.box(t)
	f.await(keys[0], "named its box", func(row code.CheckRow) bool { return row.Box == first.ID() })
	// The first walk has run its course once the slot is held and no read
	// has followed for many walks' gaps.
	settled := reads.count()
	for {
		time.Sleep(40 * fast.Walk)
		if n := reads.count(); n == settled {
			break
		} else {
			settled = n
		}
	}
	for range 5 {
		r.Nudge()
		time.Sleep(10 * fast.Walk)
	}
	if n := reads.count(); n != settled {
		t.Fatalf("a node whose one container slot is held read the queue %d times over five nudged walks, "+
			"want none: it claims nothing it reads", n-settled)
	}
	f.container.end(first, 0, "")
	second := f.container.box(t)
	f.await(keys[1], "named its box", func(row code.CheckRow) bool { return row.Box == second.ID() })
	if n := reads.count(); n == settled {
		t.Fatal("a freed slot was claimed with no read of the queue")
	}
	f.container.end(second, 0, "")
	f.container.end(f.container.box(t), 0, "")
	for _, key := range keys {
		f.ended(key)
	}
}

// A CELL'S QUEUE IS TAKEN OLDEST FIRST WHATEVER ITS CHECKS NAME: an attempt
// naming the container cell, queued first, and one naming no cell — the
// company's default, the container cell too — compete for one slot, and the
// older is claimed first though the two are read as two ranges of the queue.
//
// Mutation: take the first range's attempts before the next's — the younger
// attempt, naming no cell, is claimed first, red.
func TestACellsQueueIsTakenOldestFirstWhateverItsChecksName(t *testing.T) {
	t.Parallel()
	f := newFleet(t)
	named := def("unit", time.Minute)
	named.RunIn = code.CellContainer
	older := f.queue(oidOf("1"), named)
	younger := f.queue(oidOf("2"), def("unit", time.Minute))
	f.runner(f.options("git-a", 1, &order{nodes: []string{"git-a"}}))
	box := f.container.box(t)
	if row := f.await(older, "named a box", func(r code.CheckRow) bool { return r.Box != "" }); row.Box != box.ID() {
		t.Fatalf("the older attempt holds box %q, want the first box made, %q", row.Box, box.ID())
	}
	if row, _ := f.row(younger); row.State != code.CheckStateQueued {
		t.Fatalf("the younger attempt is %s while the older holds the one slot, want queued", row.State)
	}
	f.container.end(box, 0, "")
	f.container.end(f.container.box(t), 0, "")
	f.ended(older)
	f.ended(younger)
}

// THE CLAIM WALK'S READ IS ONE CELL'S QUEUE: a `checks_queued` read naming a
// cell lists the queued attempts whose definitions name it, and one naming
// none the attempts that name none — each attempt in exactly one — and a read
// that names no question, or a cell no check runs in, is refused.
//
// Mutation: list every queued attempt whatever its cell — each read lists all
// three, red.
func TestAClaimWalkReadIsOneCellsQueue(t *testing.T) {
	t.Parallel()
	f := newFleet(t)
	inContainer := def("unit", time.Minute)
	inContainer.RunIn = code.CellContainer
	want := map[code.CheckCell]code.CheckKey{
		code.CellContainer: f.queue(oidOf("1"), inContainer),
		code.CellE2B:       f.queue(oidOf("2"), remote("lint", time.Minute)),
		"":                 f.queue(oidOf("3"), def("vet", time.Minute)),
	}
	fresh := statelog.Freshness{Level: statelog.ReadLinearizable}
	for cell, key := range want {
		a, err := f.d.Served().Read(t.Context(), code.ReadQuery{Kind: code.ReadChecksQueued,
			ChecksQueued: &code.ChecksQueuedQuery{RunIn: cell}, Fresh: fresh})
		if err != nil {
			t.Fatalf("read the queue of %q: %v", cell, err)
		}
		if len(a.Runnable) != 1 || a.Runnable[0].CheckKey != key {
			t.Errorf("the queue of %q lists %d attempts (%+v), want only %s", cell, len(a.Runnable), a.Runnable,
				key.Name)
		}
	}
	for name, q := range map[string]*code.ChecksQueuedQuery{
		"no question": nil, "a cell no check runs in": {RunIn: code.CheckCell(config.PlacementDirect)},
	} {
		_, err := f.d.Served().Read(t.Context(), code.ReadQuery{Kind: code.ReadChecksQueued, ChecksQueued: q,
			Fresh: fresh})
		if !errors.Is(err, code.ErrInvalid) {
			t.Errorf("a queue read naming %s = %v, want ErrInvalid", name, err)
		}
	}
}

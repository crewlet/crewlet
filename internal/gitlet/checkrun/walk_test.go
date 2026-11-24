package checkrun_test

import (
	"context"
	"errors"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/gitlet"
	"github.com/crewlet/crewlet/internal/gitlet/checkrun"
	"github.com/crewlet/crewlet/internal/gitlet/gitlettest"
	"github.com/crewlet/crewlet/internal/statelog"
)

// queueReads is a data node's code domain, counting the reads of the queue a
// runner made.
type queueReads struct {
	checkrun.Estate
	reads atomic.Int64
}

func (e *queueReads) Read(ctx context.Context, q gitlet.ReadQuery) (gitlet.ReadAnswer, error) {
	if q.Kind == gitlet.ReadChecksQueued {
		e.reads.Add(1)
	}
	return e.Estate.Read(ctx, q)
}

// count is how many reads of the queue the runner has made.
func (e *queueReads) count() int { return int(e.reads.Load()) }

// walks counts a runner's walks by the read each begins with: the epoch it
// runs under (Runner.walk reads its Company first). A run reads it too, as it
// starts, so a count read while no check starts counts walks alone.
type walks struct {
	company func() (checkrun.Company, bool)
	begun   atomic.Int64
}

// counting has the runner built from o count its walks on w.
func (w *walks) counting(o *checkrun.Options) {
	w.company = o.Company
	o.Company = w.read
}

func (w *walks) read() (checkrun.Company, bool) {
	w.begun.Add(1)
	return w.company()
}

// walked waits until n walks of r begun after the call have ended: r is
// nudged into each, and into one more, which begins only once those before it
// have ended — a runner walks one walk at a time.
func (w *walks) walked(t *testing.T, r *checkrun.Runner, n int) {
	t.Helper()
	from := w.begun.Load()
	for i := range int64(n) + 1 {
		r.Nudge()
		eventually(t, "a nudged walk", func() bool { return w.begun.Load() > from+i })
	}
}

// A NODE READS NO QUEUE IT CANNOT CLAIM FROM: with its one container held, a
// nudged walk reads nothing of the queue, however much of it waits; once the
// container frees, the walk reads again and claims the next attempt.
//
// The walk that claimed the first attempt read the queue before its claim, so
// its reads are all counted once the box is named; the five nudged walks are
// counted once each has ended (walks.walked).
//
// Mutation: read the queue whatever the room — every nudged walk reads it,
// red.
func TestAFullNodeReadsNoQueueItCannotClaimFrom(t *testing.T) {
	t.Parallel()
	f := newFleet(t)
	var keys []gitlet.CheckKey
	for _, oid := range []string{"1", "2", "3"} {
		keys = append(keys, f.queue(oidOf(oid), def("unit", time.Minute)))
	}
	reads := &queueReads{Estate: estate{f.d}}
	o := f.options("git-a", 1, &order{nodes: []string{"git-a"}})
	o.Estate = reads
	// NO BACKSTOP WITHIN THE CASE: a walk at the start, at each nudge and at
	// each check's ending, and nothing else.
	o.Timing.Backstop = time.Hour
	var w walks
	w.counting(&o)
	r := f.runner(o)
	first := f.container.box(t)
	f.await(keys[0], "named its box", func(row gitlet.CheckRow) bool { return row.Box == first.ID() })
	settled := reads.count()
	w.walked(t, r, 5)
	if n := reads.count(); n != settled {
		t.Fatalf("a node whose one container is held read the queue %d times over five nudged walks, "+
			"want none: it claims nothing it reads", n-settled)
	}
	f.container.end(first, 0, "")
	second := f.container.box(t)
	f.await(keys[1], "named its box", func(row gitlet.CheckRow) bool { return row.Box == second.ID() })
	if n := reads.count(); n == settled {
		t.Fatal("a freed container was claimed with no read of the queue")
	}
	f.container.end(second, 0, "")
	f.container.end(f.container.box(t), 0, "")
	for _, key := range keys {
		f.ended(key)
	}
}

// A NODE THAT MAKES NO CONTAINER READS NO QUEUE: its company runs checks, but
// its catalogue has no container backend (a node behind its company's apply),
// so its walks read nothing and claim nothing, and the attempt waits queued
// for a node that makes one.
//
// Mutation: walk a catalogue with no container backend as one with — the walk
// reads the queue and claims the attempt, which its run ends box_failed, red.
func TestANodeThatMakesNoContainerReadsNoQueue(t *testing.T) {
	t.Parallel()
	f := newFleet(t)
	key := f.queue(oidOf("1"), def("unit", time.Minute))
	reads := &queueReads{Estate: estate{f.d}}
	o := f.options("git-a", 1, &order{nodes: []string{"git-a"}})
	o.Estate = reads
	o.Boxes = func() (checkrun.Boxes, bool) { return nil, false }
	o.Timing.Backstop = time.Hour
	var w walks
	w.counting(&o)
	w.walked(t, f.runner(o), 3)
	if n := reads.count(); n != 0 {
		t.Errorf("a node with no container backend read the queue %d times, want none", n)
	}
	if row, _ := f.row(key); row.State != gitlet.CheckStateQueued {
		t.Errorf("a node with no container backend left the attempt %s, want it queued for a node that "+
			"makes one", row.State)
	}
}

// THE CLAIM WALK'S READ IS THE QUEUE, OLDEST QUEUED FIRST: a `checks_queued`
// read lists every attempt waiting for a runner and none a runner holds, in
// the order they were queued, a page resuming exactly where the last ended;
// and it is the engine's own, refused where a principal asks it.
//
// Mutation: page the queue from its start again after a cursor — the second
// page repeats the first attempt, red.
func TestTheClaimWalksReadIsTheQueueOldestFirst(t *testing.T) {
	t.Parallel()
	f := newFleet(t)
	// QUEUED AT INSTANTS THE CASE STATES, a minute apart, in an order their
	// commits' ids do not share: the order read is the queue's, never the
	// wall clock's or the key's.
	at := time.Now().UTC().Truncate(time.Second).Add(-time.Hour)
	var want []gitlet.CheckKey
	for i, oid := range []string{"3", "1", "2"} {
		d := def("unit", time.Minute)
		key := f.key(oidOf(oid), d.Name, 1)
		res, err := f.d.Served().PublishChecks(t.Context(), gitlet.ChecksRequest{OpID: newOpID("code_check_queue"),
			Queued: &gitlet.CheckQueued{CheckKey: key, Purpose: gitlet.PurposeTip, Def: &d,
				Reason: gitlet.QueueDispatch, QueuedAt: at.Add(time.Duration(i) * time.Minute)}})
		if err != nil || res.Refusal != "" {
			t.Fatalf("queue %s: (%+v, %v)", key.OID, res, err)
		}
		want = append(want, key)
	}
	held := f.queue(oidOf("4"), def("lint", time.Minute))
	f.claim(held, "git-a", gitlet.PlacementContainer, "", time.Now().UTC())
	fresh := statelog.Freshness{Level: statelog.ReadLinearizable}
	var (
		got    []gitlet.CheckKey
		cursor string
	)
	for page := 0; ; page++ {
		if page > len(want) {
			t.Fatalf("a page of one attempt each read %d pages of a queue of %d: %v", page, len(want), got)
		}
		a, err := f.d.Served().Read(t.Context(), gitlet.ReadQuery{Kind: gitlet.ReadChecksQueued, Cursor: cursor,
			Limit: 1, Fresh: fresh})
		if err != nil {
			t.Fatalf("read the queue after %q: %v", cursor, err)
		}
		for _, c := range a.Runnable {
			got = append(got, c.CheckKey)
		}
		if cursor = a.Next; cursor == "" {
			break
		}
	}
	if !slices.Equal(got, want) {
		t.Errorf("the queue read as %v, want the three waiting in the order they were queued, %v", got, want)
	}
	_, err := f.d.Served().Read(t.Context(), gitlet.ReadQuery{Kind: gitlet.ReadChecksQueued, Fresh: fresh,
		Asker: gitlet.Principal{Kind: gitlet.PrincipalOperator, OperatorID: "op-pat", Seat: gitlettest.FixturePat}})
	if !errors.Is(err, gitlet.ErrInvalid) {
		t.Errorf("a queue read a principal asks = %v, want ErrInvalid", err)
	}
}

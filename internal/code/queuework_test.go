package code_test

import (
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/events/types"
)

// THE HOME'S QUEUE WORK IS ONE SNAPSHOT OF ONE TARGET (M5-D12, M5-D15): a
// `queue_due` read of one repository answers its home a target a page, every
// fact the queue worker decides its next step from read in one transaction by
// the shipped reader over the shipped applier's rows — main's tip, the gate its
// queue lands under (its depth and batch, the required check), the
// live entry with who enqueued it, whether it closes its work item, the change
// its merge commit is written from and that work item, and the live train with
// the latest attempt of the required check on each candidate commit, for main.
// The page's cursor is the target, and the page after it is empty.
//
// Mutation: answer the candidate commits' checks from the train's base rather
// than each commit → the passed attempt on M is missing, red; drop the gate's
// batch from the answer → zero, red; answer the enqueuer as the queue's own
// principal → red.
func TestTheHomesQueueWorkIsOneSnapshotOfOneTarget(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	n.chart = testChart()
	c, _ := committerOn(t, n, n.pub)
	w, _ := writerOn(t, n, n.pub)
	s := suiteSettings()
	s.Owner = code.Owner{Kind: code.OwnerUnit, Ref: "Engineering"}
	s.Protections[0].Gate = queueGate()
	s.Protections[0].Gate.Landing.Batch = 3
	repo := n.ensure("web", s, t1).RepoID
	n.push(repo, entry(uuid.Must(uuid.NewV7()).String(), set(mainRef, code.ZeroOID, oidOf("A")),
		set(featureRef, code.ZeroOID, oidOf("B"))))
	open := opening(repo, featureRef, mainRef, dev)
	item := types.WorkItem{Backend: types.WorkNative, ID: "task-9", Key: "ENG-412", Project: "ENG"}
	open.Open.WorkItem = &item
	opened, err := c.ProposeChange(t.Context(), open)
	requireChanged(t, opened, err)
	ready(t, n, w, repo, opened.ChangeID)
	enqueue := enqueueing(repo, opened.ChangeID, ada, oidOf("B"))
	enqueue.Enqueue.CloseTask = true
	res, err := c.ProposeQueue(t.Context(), enqueue)
	requireQueued(t, res, err, code.QueueWaiting, 1)
	entryID := enqueue.Enqueue.EntryID
	train := training(repo, oidOf("A"), entryID, oidOf("M"))
	res, err = c.ProposeQueue(t.Context(), train)
	requireRecorded(t, res, err)
	n.checked(repo, oidOf("M"), mainRef, "unit", 2, code.CheckStatePassed)
	n.checked(repo, oidOf("A"), mainRef, "unit", 1, code.CheckStateFailed)

	q := code.ReadQuery{Kind: code.ReadQueueDue, QueueDue: &code.QueueDueQuery{RepoID: repo}}
	answer, err := n.reader.Read(t.Context(), q)
	if err != nil || answer.DueQueue == nil {
		t.Fatalf("the home's queue work of web reads %+v (%v), want main's", answer.DueQueue, err)
	}
	d := *answer.DueQueue
	if d.TargetRef != mainRef || d.Tip != oidOf("A") || d.Depth != 4 ||
		d.Batch != 3 || len(d.Required) != 1 || d.Required[0] != "unit" || d.More || answer.Next != mainRef {
		t.Fatalf("main's queue work reads %+v (next %q), want main at A, a queue at depth 4 and batch 3 "+
			"requiring unit, its cursor main", d, answer.Next)
	}
	if len(d.Entries) != 1 {
		t.Fatalf("main's queue work holds %d entries, want the one enqueued", len(d.Entries))
	}
	e := d.Entries[0]
	if e.EntryID != entryID || e.State != code.QueueTesting || e.HeadOID != oidOf("B") || !e.CloseTask ||
		e.EnqueuedBy != ada || e.Train != train.Candidate.Train || e.Change.ID != opened.ChangeID ||
		e.Change.Number != 1 || e.Change.SourceRef != featureRef || e.WorkItem == nil || *e.WorkItem != item {
		t.Fatalf("the entry reads %+v, want ada's entry of web!1 testing at B in its train, closing %s", e,
			item.Key)
	}
	if len(d.Train) != 1 || d.Train[0].Train != train.Candidate.Train || d.Train[0].BaseOID != oidOf("A") ||
		d.Train[0].TipOID != oidOf("M") || len(d.Train[0].Commits) != 1 ||
		d.Train[0].Commits[0].EntryID != entryID || d.Train[0].Commits[0].Commit != oidOf("M") {
		t.Fatalf("main's train reads %+v, want one candidate of the entry built onto A as M", d.Train)
	}
	held, ok := d.Train[0].Commits[0].Check("unit")
	if !ok || held.Attempt != 2 || held.State != code.CheckStatePassed {
		t.Fatalf("unit on M reads %+v (%t), want its latest attempt, 2, passed", held, ok)
	}

	q.Cursor = answer.Next
	if after, err := n.reader.Read(t.Context(), q); err != nil || after.DueQueue != nil || after.Next != "" {
		t.Fatalf("the page after main reads %+v (next %q, %v), want none", after.DueQueue, after.Next, err)
	}
}

// THE HOME'S QUEUE WORK COUNTS WHAT ITS TARGET WAS ASKED TO LAND (M5-D28):
// asked to, a `queue_due` read counts the entries enqueued on main at or
// after an instant — the live ones and the ended ones alike, each by its
// ENQUEUE's broker instant — and names the repository, which the merge-rate
// alarm names the target by. An entry enqueued an hour before the window and
// dequeued inside it is no enqueue of the window; the entry enqueued in it is;
// a read that asks for no count gets none.
//
// Mutation: count the live entries alone → the second dequeued entry is
// missed, red; judge an ended entry by when it ended rather than when it was
// enqueued → the first entry, dequeued inside the window, is counted, red.
func TestTheHomesQueueWorkCountsItsTargetsEnqueues(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	n.chart = testChart()
	c, _ := committerOn(t, n, n.pub)
	w, _ := writerOn(t, n, n.pub)
	repo, change := queueRepo(t, n, c, "web")
	ready(t, n, w, repo, change)
	// stamp publishes what follows at the broker instant at.
	stamp := func(at time.Time) {
		n.storedAt = func(uint64, time.Time) time.Time { return at }
	}
	window := harnessAt.Add(-code.MergeRateWindow)
	stamp(window.Add(-time.Hour))
	first := enqueueing(repo, change, ada, oidOf("B"))
	res, err := c.ProposeQueue(t.Context(), first)
	requireQueued(t, res, err, code.QueueWaiting, 1)
	stamp(window.Add(10 * time.Minute))
	res, err = c.ProposeQueue(t.Context(), dequeueing(repo, first.Enqueue.EntryID, ada))
	requireQueued(t, res, err, code.QueueDequeued, 0)
	second := enqueueing(repo, change, ada, oidOf("B"))
	res, err = c.ProposeQueue(t.Context(), second)
	requireQueued(t, res, err, code.QueueWaiting, 1)
	stamp(window.Add(20 * time.Minute))
	res, err = c.ProposeQueue(t.Context(), dequeueing(repo, second.Enqueue.EntryID, ada))
	requireQueued(t, res, err, code.QueueDequeued, 0)
	res, err = c.ProposeQueue(t.Context(), enqueueing(repo, change, ada, oidOf("B")))
	requireQueued(t, res, err, code.QueueWaiting, 1)

	read := func(since time.Time) code.QueueDue {
		t.Helper()
		answer, err := n.reader.Read(t.Context(), code.ReadQuery{Kind: code.ReadQueueDue,
			QueueDue: &code.QueueDueQuery{RepoID: repo, EnqueuedSince: since}})
		if err != nil || answer.DueQueue == nil {
			t.Fatalf("the home's queue work of web reads %+v (%v), want main's", answer.DueQueue, err)
		}
		return *answer.DueQueue
	}
	if d := read(window); d.Enqueued != 2 || d.Repo != "web" {
		t.Fatalf("main's queue work counts %d enqueue(s) since the window opened, of %q; want 2 of web — "+
			"the one ended inside it and the one live, never the one enqueued before it", d.Enqueued, d.Repo)
	}
	if d := read(time.Time{}); d.Enqueued != 0 {
		t.Fatalf("a read asking no count counts %d enqueue(s), want none", d.Enqueued)
	}
	if _, err := n.reader.Read(t.Context(), code.ReadQuery{Kind: code.ReadQueueDue,
		QueueDue: &code.QueueDueQuery{EnqueuedSince: window}}); !errors.Is(err, code.ErrInvalid) {
		t.Fatalf("the listing asked to count enqueues answers %v, want invalid: the count is a repository's", err)
	}
}

// THE HOME'S QUEUE WORK READS THE REPOSITORY'S DROP EPOCH (M6-D4): the
// landing its home decides from the snapshot states it, so the snapshot
// answers the epoch the repository's row holds in the same transaction — 0
// before any expunge, the begin's once one has begun.
//
// Mutation: leave the epoch out of the queue work → 0 after the begin, red.
func TestTheHomesQueueWorkReadsTheDropEpoch(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	n.chart = testChart()
	c, _ := committerOn(t, n, n.pub)
	w, _ := writerOn(t, n, n.pub)
	repo, change := queueRepo(t, n, c, "web")
	ready(t, n, w, repo, change)
	res, err := c.ProposeQueue(t.Context(), enqueueing(repo, change, ada, oidOf("B")))
	requireQueued(t, res, err, code.QueueWaiting, 1)

	q := code.ReadQuery{Kind: code.ReadQueueDue, QueueDue: &code.QueueDueQuery{RepoID: repo}}
	read := func() int64 {
		t.Helper()
		answer, err := n.reader.Read(t.Context(), q)
		if err != nil || answer.DueQueue == nil {
			t.Fatalf("the home's queue work of web reads %+v (%v), want main's", answer.DueQueue, err)
		}
		return answer.DueQueue.DropEpoch
	}
	if epoch := read(); epoch != 0 {
		t.Fatalf("the queue work of a repository never expunged reads drop epoch %d, want 0", epoch)
	}
	b := beginFor(n, repo, 1, n.livePacks(repo), 1, 0)
	began, err := c.ProposeExpunge(t.Context(), code.ExpungeProposal{Begin: &b})
	requireExpunged(t, began, err)
	if epoch := read(); epoch != 1 {
		t.Fatalf("the queue work after an expunge's begin reads drop epoch %d, want the begin's, 1", epoch)
	}
}

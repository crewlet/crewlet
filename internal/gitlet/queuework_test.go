package gitlet_test

import (
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/gitlet"
	"github.com/crewlet/crewlet/internal/store"
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
	s.Owner = gitlet.Owner{Kind: gitlet.OwnerUnit, Ref: "Engineering"}
	s.Protections[0].Gate = queueGate()
	s.Protections[0].Gate.Landing.Batch = 3
	repo := n.ensure("web", s, t1).RepoID
	n.push(repo, entry(uuid.Must(uuid.NewV7()).String(), set(mainRef, gitlet.ZeroOID, oidOf("A")),
		set(featureRef, gitlet.ZeroOID, oidOf("B"))))
	open := opening(repo, featureRef, mainRef, dev)
	item := types.WorkItem{Backend: types.WorkWorklet, ID: "task-9", Key: "ENG-412", Project: "ENG"}
	open.Open.WorkItem = &item
	opened, err := c.ProposeChange(t.Context(), open)
	requireChanged(t, opened, err)
	ready(t, n, w, repo, opened.ChangeID)
	enqueue := enqueueing(repo, opened.ChangeID, ada, oidOf("B"))
	enqueue.Enqueue.CloseTask = true
	res, err := c.ProposeQueue(t.Context(), enqueue)
	requireQueued(t, res, err, gitlet.QueueWaiting, 1)
	entryID := enqueue.Enqueue.EntryID
	train := training(repo, oidOf("A"), entryID, oidOf("M"))
	res, err = c.ProposeQueue(t.Context(), train)
	requireRecorded(t, res, err)
	n.checked(repo, oidOf("M"), mainRef, "unit", 2, gitlet.CheckStatePassed)
	n.checked(repo, oidOf("A"), mainRef, "unit", 1, gitlet.CheckStateFailed)

	q := gitlet.ReadQuery{Kind: gitlet.ReadQueueDue, QueueDue: &gitlet.QueueDueQuery{RepoID: repo}}
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
	if e.EntryID != entryID || e.State != gitlet.QueueTesting || e.HeadOID != oidOf("B") || !e.CloseTask ||
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
	if !ok || held.Attempt != 2 || held.State != gitlet.CheckStatePassed {
		t.Fatalf("unit on M reads %+v (%t), want its latest attempt, 2, passed", held, ok)
	}

	q.Cursor = answer.Next
	if after, err := n.reader.Read(t.Context(), q); err != nil || after.DueQueue != nil || after.Next != "" {
		t.Fatalf("the page after main reads %+v (next %q, %v), want none", after.DueQueue, after.Next, err)
	}
}

// THE HOME'S QUEUE WORK READS WHAT ITS STEP NEEDS: a `queue_due` page names
// as many of its target's live entries as one train under the gate takes —
// depth × batch, here one of two, and More for the other — and their changes'
// descriptions, the body and the authors a squash's message is written from,
// only on a snapshot its home builds a train from (QueueDue.Builds): with no
// train, the first entry's change with its body and its author; with a train
// standing on the tip, the same change with neither, since testing, landing
// and bisecting write no commit message.
//
// Mutation: scan MaxCandidateEntries rather than depth × batch → both
// entries are named, red; read the descriptions on every snapshot → the
// standing train's page carries the body, red; never read them → the build's
// page has no body, red; leave the authors unread → the build's page names
// none, red.
func TestTheHomesQueueWorkReadsWhatItsStepNeeds(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	n.chart = testChart()
	c, _ := committerOn(t, n, n.pub)
	w, _ := writerOn(t, n, n.pub)
	s := suiteSettings()
	s.Owner = gitlet.Owner{Kind: gitlet.OwnerUnit, Ref: "Engineering"}
	s.Protections[0].Gate = queueGate()
	s.Protections[0].Gate.Landing.Depth, s.Protections[0].Gate.Landing.Batch = 1, 1
	repo := n.ensure("web", s, t1).RepoID
	n.push(repo, entry(uuid.Must(uuid.NewV7()).String(), set(mainRef, gitlet.ZeroOID, oidOf("A")),
		set(featureRef, gitlet.ZeroOID, oidOf("B")), set(releaseRef, gitlet.ZeroOID, oidOf("R"))))
	var entries []string
	for _, src := range []struct{ ref, head string }{{featureRef, "B"}, {releaseRef, "R"}} {
		open := opening(repo, src.ref, mainRef, dev)
		open.Open.Body = "Retries the upload, from " + src.ref
		opened, err := c.ProposeChange(t.Context(), open)
		requireChanged(t, opened, err)
		converse(t, w, n.revising(repo, opened.ChangeID, 1, oidOf(src.head), mainRef, nil))
		converse(t, w, reviewing(repo, opened.ChangeID, platLead, 1, gitlet.VerdictApprove, nil))
		n.passed(repo, oidOf(src.head), mainRef, "unit")
		enqueue := enqueueing(repo, opened.ChangeID, ada, oidOf(src.head))
		res, err := c.ProposeQueue(t.Context(), enqueue)
		requireQueued(t, res, err, gitlet.QueueWaiting, len(entries)+1)
		entries = append(entries, enqueue.Enqueue.EntryID)
	}
	work := func() gitlet.QueueDue {
		t.Helper()
		answer, err := n.reader.Read(t.Context(), gitlet.ReadQuery{Kind: gitlet.ReadQueueDue,
			QueueDue: &gitlet.QueueDueQuery{RepoID: repo}})
		if err != nil || answer.DueQueue == nil {
			t.Fatalf("the home's queue work of web reads %+v (%v), want main's", answer.DueQueue, err)
		}
		return *answer.DueQueue
	}

	d := work()
	if len(d.Entries) != 1 || !d.More || d.Entries[0].EntryID != entries[0] {
		t.Fatalf("main's queue work at depth 1 and batch 1 names %+v (more %t), want the first entry alone "+
			"and more behind it", d.Entries, d.More)
	}
	built := d.Entries[0].Change
	if !d.Builds() || built.Body != "Retries the upload, from "+featureRef || len(built.Authors) == 0 {
		t.Fatalf("the snapshot a train is built from reads the change %+v (builds %t), want its body and its "+
			"authors", built, d.Builds())
	}

	train := training(repo, oidOf("A"), entries[0], oidOf("M"))
	res, err := c.ProposeQueue(t.Context(), train)
	requireRecorded(t, res, err)
	d = work()
	standing := d.Entries[0].Change
	if d.Builds() || !d.TrainStands() || standing.ID != built.ID || standing.Number != built.Number ||
		standing.Body != "" || standing.Authors != nil {
		t.Fatalf("the snapshot of a standing train reads the change %+v (builds %t), want its row without its "+
			"description", standing, d.Builds())
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
// missed, red; count the target's entries whenever enqueued → the first
// entry, enqueued before the window, is counted, red.
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
	window := harnessAt.Add(-gitlet.MergeRateWindow)
	stamp(window.Add(-time.Hour))
	first := enqueueing(repo, change, ada, oidOf("B"))
	res, err := c.ProposeQueue(t.Context(), first)
	requireQueued(t, res, err, gitlet.QueueWaiting, 1)
	stamp(window.Add(10 * time.Minute))
	res, err = c.ProposeQueue(t.Context(), dequeueing(repo, first.Enqueue.EntryID, ada))
	requireQueued(t, res, err, gitlet.QueueDequeued, 0)
	second := enqueueing(repo, change, ada, oidOf("B"))
	res, err = c.ProposeQueue(t.Context(), second)
	requireQueued(t, res, err, gitlet.QueueWaiting, 1)
	stamp(window.Add(20 * time.Minute))
	res, err = c.ProposeQueue(t.Context(), dequeueing(repo, second.Enqueue.EntryID, ada))
	requireQueued(t, res, err, gitlet.QueueDequeued, 0)
	res, err = c.ProposeQueue(t.Context(), enqueueing(repo, change, ada, oidOf("B")))
	requireQueued(t, res, err, gitlet.QueueWaiting, 1)

	read := func(since time.Time) gitlet.QueueDue {
		t.Helper()
		answer, err := n.reader.Read(t.Context(), gitlet.ReadQuery{Kind: gitlet.ReadQueueDue,
			QueueDue: &gitlet.QueueDueQuery{RepoID: repo, EnqueuedSince: since}})
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
	if _, err := n.reader.Read(t.Context(), gitlet.ReadQuery{Kind: gitlet.ReadQueueDue,
		QueueDue: &gitlet.QueueDueQuery{EnqueuedSince: window}}); !errors.Is(err, gitlet.ErrInvalid) {
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
	requireQueued(t, res, err, gitlet.QueueWaiting, 1)

	q := gitlet.ReadQuery{Kind: gitlet.ReadQueueDue, QueueDue: &gitlet.QueueDueQuery{RepoID: repo}}
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
	began, err := c.ProposeExpunge(t.Context(), gitlet.ExpungeProposal{Begin: &b})
	requireExpunged(t, began, err)
	if epoch := read(); epoch != 1 {
		t.Fatalf("the queue work after an expunge's begin reads drop epoch %d, want the begin's, 1", epoch)
	}
}

// trainAttempt writes attempt 1 of check name on commit oid of repoID, for
// main, in state — queued, running or ended — as a merge queue queues one for
// purpose, naming candidate ("" for a tip's bisect, which names none),
// straight into n's estate.
func (n *node) trainAttempt(repoID, oid, name string, state gitlet.CheckState, purpose gitlet.CheckPurpose,
	candidate string) {

	n.t.Helper()
	at := store.EncodeTime(harnessAt)
	started, finished := int64(0), int64(0)
	if state != gitlet.CheckStateQueued {
		started = at
	}
	if state.Ended() {
		finished = at
	}
	n.exec(`INSERT INTO code_checks (repo_id, commit_oid, target_ref, name, attempt, state, purpose, change_id,
		revision, candidate_id, entry_id, base_oid, defs_oid, required, run_in, timeout_seconds, reason,
		runner_node, placement, box_handle, reporter, queued_at, started_at, finished_at,
		duration_ms, exit_code, failures_omitted, queued_position, finished_position, row_bytes, overdue_at,
		url, summary, def, failures, tail)
		VALUES (?, ?, ?, ?, 1, ?, ?, '', 0, ?, '', '', '', 1, '', 600, '', '', '', '', '', ?, ?, ?, 0, NULL,
			0, 1, 0, 4, ?, '', '', '', '', '')`,
		repoID, oid, mainRef, name, string(state), string(purpose), candidate, at, started, finished,
		store.EncodeTime(harnessAt.Add(600*time.Second)))
}

// THE HOME'S QUEUE WORK ANSWERS A GIVEN-UP TRAIN'S CHECKS: the first page of
// a repository's `queue_due` read answers every attempt its queue queued that
// has not ended — queued or running — on a commit no train of its target
// tests or landed, `dequeued` where a change its candidate merged left its
// queue and `abandoned` where its train was given up otherwise, and never an
// attempt on a commit a live train tests — even one naming a candidate a
// rebuilt train dropped, since the rebuilt train's dispatch of the same
// commit is answered from that attempt's row — one that ended, or a tip's
// bisect, which names no candidate. It answers them with no live entry left
// to page — the dequeue of the last one — and the listing of repositories
// names the repository then too, after those holding a live entry, a page at
// a time.
//
// Mutation: drop the test of the commit → the live train's attempts are
// answered, red; test the candidate rather than the commit → the rebuilt
// train's own measurement is answered, red; drop the dequeued test → the
// dequeued candidate's attempts read abandoned, red; read the given-up
// attempts only beside a live entry → none after the last dequeue, red; list
// only the repositories holding a live entry → the drained repository is
// never listed, red.
func TestTheHomesQueueWorkAnswersAGivenUpTrainsChecks(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	n.chart = testChart()
	c, _ := committerOn(t, n, n.pub)
	w, _ := writerOn(t, n, n.pub)
	repo, change := queueRepo(t, n, c, "web")
	ready(t, n, w, repo, change)
	other, otherChange := queueRepo(t, n, c, "api")
	ready(t, n, w, other, otherChange)

	first := enqueueing(repo, change, ada, oidOf("B"))
	res, err := c.ProposeQueue(t.Context(), first)
	requireQueued(t, res, err, gitlet.QueueWaiting, 1)
	one := training(repo, oidOf("A"), first.Enqueue.EntryID, oidOf("M"))
	recorded, err := c.ProposeQueue(t.Context(), one)
	requireRecorded(t, recorded, err)
	cand := one.Candidate.Candidates[0].CandidateID
	n.trainAttempt(repo, oidOf("M"), "unit", gitlet.CheckStateRunning, gitlet.PurposeCandidate, cand)
	n.trainAttempt(repo, oidOf("M"), "lint", gitlet.CheckStateQueued, gitlet.PurposeCandidate, cand)
	n.trainAttempt(repo, oidOf("M"), "e2e", gitlet.CheckStatePassed, gitlet.PurposeCandidate, cand)
	n.trainAttempt(repo, oidOf("T"), "unit", gitlet.CheckStateQueued, gitlet.PurposeBisect, "")
	res, err = c.ProposeQueue(t.Context(), enqueueing(other, otherChange, ada, oidOf("B")))
	requireQueued(t, res, err, gitlet.QueueWaiting, 1)

	firstPage := func() gitlet.ReadAnswer {
		t.Helper()
		answer, err := n.reader.Read(t.Context(), gitlet.ReadQuery{Kind: gitlet.ReadQueueDue,
			QueueDue: &gitlet.QueueDueQuery{RepoID: repo}})
		if err != nil {
			t.Fatalf("the home's queue work of web: %v", err)
		}
		return answer
	}
	if a := firstPage(); len(a.QueueAbandoned) != 0 || a.DueQueue == nil {
		t.Fatalf("a live train's checks read given up: %+v (work %+v)", a.QueueAbandoned, a.DueQueue)
	}

	res, err = c.ProposeQueue(t.Context(), dequeueing(repo, first.Enqueue.EntryID, ada))
	requireQueued(t, res, err, gitlet.QueueDequeued, 0)
	key := func(name string) gitlet.CheckKey {
		return gitlet.CheckKey{RepoID: repo, OID: oidOf("M"), Target: mainRef, Name: name, Attempt: 1}
	}
	a := firstPage()
	want := []gitlet.AbandonedCheck{{CheckKey: key("lint"), CandidateID: cand, Reason: gitlet.CheckReasonDequeued},
		{CheckKey: key("unit"), CandidateID: cand, Reason: gitlet.CheckReasonDequeued}}
	if a.DueQueue != nil || !slices.Equal(a.QueueAbandoned, want) {
		t.Fatalf("with its last entry dequeued, web's first page answers %+v (work %+v), want the running "+
			"unit and the queued lint of the dequeued change's candidate, dequeued, alone", a.QueueAbandoned,
			a.DueQueue)
	}

	// A TRAIN GIVEN UP BY A NEW ONE: its candidate merged no change that
	// left the queue, so its attempts are abandoned.
	second := enqueueing(repo, change, ada, oidOf("B"))
	res, err = c.ProposeQueue(t.Context(), second)
	requireQueued(t, res, err, gitlet.QueueWaiting, 1)
	two := training(repo, oidOf("A"), second.Enqueue.EntryID, oidOf("N"))
	recorded, err = c.ProposeQueue(t.Context(), two)
	requireRecorded(t, recorded, err)
	n.trainAttempt(repo, oidOf("N"), "unit", gitlet.CheckStateQueued, gitlet.PurposeBisect,
		two.Candidate.Candidates[0].CandidateID)
	three := training(repo, oidOf("A"), second.Enqueue.EntryID, oidOf("P"))
	recorded, err = c.ProposeQueue(t.Context(), three)
	requireRecorded(t, recorded, err)
	// A TRAIN BUILT AGAIN AT THE SAME COMMIT: its dispatch of P would be
	// answered from the third train's attempt, which names a candidate the
	// fourth dropped — and which measures what the fourth tests.
	n.trainAttempt(repo, oidOf("P"), "unit", gitlet.CheckStateRunning, gitlet.PurposeCandidate,
		three.Candidate.Candidates[0].CandidateID)
	recorded, err = c.ProposeQueue(t.Context(), training(repo, oidOf("A"), second.Enqueue.EntryID, oidOf("P")))
	requireRecorded(t, recorded, err)
	a = firstPage()
	abandoned := gitlet.AbandonedCheck{CheckKey: gitlet.CheckKey{RepoID: repo, OID: oidOf("N"), Target: mainRef,
		Name: "unit", Attempt: 1}, CandidateID: two.Candidate.Candidates[0].CandidateID,
		Reason: gitlet.CheckReasonAbandoned}
	if a.DueQueue == nil || len(a.QueueAbandoned) != 3 || !slices.Contains(a.QueueAbandoned, abandoned) ||
		!slices.Contains(a.QueueAbandoned, want[0]) {
		t.Fatalf("with the second train given up by a third and the third rebuilt at its commit, web's first "+
			"page answers %+v, want the first train's two dequeued and the second's bisect probe abandoned, "+
			"and never the run on the commit the live train tests", a.QueueAbandoned)
	}
	if b, err := n.reader.Read(t.Context(), gitlet.ReadQuery{Kind: gitlet.ReadQueueDue,
		QueueDue: &gitlet.QueueDueQuery{RepoID: repo}, Cursor: a.Next}); err != nil || len(b.QueueAbandoned) != 0 {
		t.Fatalf("web's second page answers %+v (%v), want the given-up attempts on the first alone",
			b.QueueAbandoned, err)
	}

	// THE LISTING: web holds a live entry again, so dequeue it to leave
	// web holding given-up attempts alone, and page the listing one
	// repository at a time.
	res, err = c.ProposeQueue(t.Context(), dequeueing(repo, second.Enqueue.EntryID, ada))
	requireQueued(t, res, err, gitlet.QueueDequeued, 0)
	gitlet.SetPageLimits(n.reader, 1, 1<<20)
	var listed []string
	cursor := ""
	for range 4 {
		page, err := n.reader.Read(t.Context(), gitlet.ReadQuery{Kind: gitlet.ReadQueueDue,
			QueueDue: &gitlet.QueueDueQuery{}, Cursor: cursor})
		if err != nil {
			t.Fatalf("the listing of repositories owing queue work: %v", err)
		}
		listed = append(listed, page.DueRepos...)
		if cursor = page.Next; cursor == "" {
			break
		}
	}
	if !slices.Equal(listed, []string{other, repo}) || cursor != "" {
		t.Fatalf("the listing names %v (next %q), want api, holding a live entry, then web, holding a given-up "+
			"train's attempts alone", listed, cursor)
	}
}

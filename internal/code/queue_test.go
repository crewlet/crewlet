package code_test

import (
	"database/sql"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/store"
)

// A PROTECTED BRANCH'S MERGE QUEUE, decided by the shipped committer from one
// snapshot on a real embedded broker, applied by the shipped applier and read
// through the shipped reader.

// queueGate is main's gate on a queue repository: one counted approval, and
// the check unit — a queue target, pre_land.
func queueGate() *code.MergeGate {
	return &code.MergeGate{MergeBy: code.MergeByWriters, Approvals: code.ApprovalsOne, Checks: []string{"unit"},
		Landing: code.Landing{Policy: code.LandingPreLand, Depth: 4, Batch: 4}, DeleteSource: code.DeleteSeats}
}

// queueRepo is a fresh repository named name — the one Engineering owns, its
// main a queue target ([queueGate]) — holding main at A, feature at B and
// release at R, and dev's change from feature into main on it, as discussed
// opens one.
func queueRepo(t *testing.T, n *node, c *code.Committer, name string) (repo, change string) {
	t.Helper()
	s := suiteSettings()
	s.Owner = code.Owner{Kind: code.OwnerUnit, Ref: "Engineering"}
	s.Protections[0].Gate = queueGate()
	repo = n.ensure(name, s, t1).RepoID
	n.push(repo, entry(uuid.Must(uuid.NewV7()).String(), set(mainRef, code.ZeroOID, oidOf("A")),
		set(featureRef, code.ZeroOID, oidOf("B")), set(releaseRef, code.ZeroOID, oidOf("R"))))
	opened, err := c.ProposeChange(t.Context(), opening(repo, featureRef, mainRef, dev))
	requireChanged(t, opened, err)
	return repo, opened.ChangeID
}

// ready makes change's revision 1 — head B — meet main's gate: its facts
// against main recorded, plat-lead's approval, and unit passed on B.
func ready(t *testing.T, n *node, w *code.ChangeWriter, repo, change string) {
	t.Helper()
	converse(t, w, n.revising(repo, change, 1, oidOf("B"), mainRef, nil))
	converse(t, w, reviewing(repo, change, platLead, 1, code.VerdictApprove, nil))
	n.passed(repo, oidOf("B"), mainRef, "unit")
}

// passed writes check name passed on commit oid of repoID, checked for the
// branch target, straight into n's estate — the ending no runner of this build
// reports yet. Every column is written, overdue_at its queued instant plus its
// timeout, as the applier writes it.
func (n *node) passed(repoID, oid, target, name string) {
	n.t.Helper()
	at := store.EncodeTime(harnessAt)
	n.exec(`INSERT INTO code_checks (repo_id, commit_oid, target_ref, name, attempt, state, purpose, change_id,
		revision, candidate_id, entry_id, base_oid, defs_oid, required, run_in, timeout_seconds, reason,
		runner_node, placement, box_handle, borrowed, reporter, queued_at, started_at, finished_at,
		duration_ms, exit_code, failures_omitted, queued_position, finished_position, row_bytes, overdue_at,
		url, summary, def, failures, tail)
		VALUES (?, ?, ?, ?, 1, 'passed', 'head', '', 0, '', '', '', '', 1, '', 600, 'exit',
			'node-a', 'container', '', 0, '', ?, ?, ?, 1000, 0, 0, 1, 2, 4, ?, '', 'ok', '', '', '')`,
		repoID, oid, target, name, at, at, at, store.EncodeTime(harnessAt.Add(600*time.Second)))
}

// enqueueing is asker's enqueue of change, at head and on main, under an
// operation id of its own.
func enqueueing(repo, change string, asker code.Principal, head string) code.QueueProposal {
	opID := statelog.NewOpID(time.Now(), "code_merge_change")
	return code.QueueProposal{OpID: opID, Asker: asker, Enqueue: &code.Enqueue{RepoID: repo,
		EntryID: code.QueueEntryID(opID), ChangeID: change, TargetRef: mainRef, HeadOID: head,
		Method: code.MergeMethodMerge}}
}

// dequeueing is asker's dequeue of entry.
func dequeueing(repo, entry string, asker code.Principal) code.QueueProposal {
	return code.QueueProposal{OpID: statelog.NewOpID(time.Now(), "code_dequeue_change"), Asker: asker,
		Dequeue: &code.Dequeue{RepoID: repo, EntryID: entry}}
}

// requireQueued fails t unless res is a write applied at a position, leaving
// its entry in state at place.
func requireQueued(t *testing.T, res code.QueueResult, err error, state code.QueueState, place int) {
	t.Helper()
	if err != nil {
		t.Fatalf("ProposeQueue: %v", err)
	}
	if res.Outcome != statelog.OutcomeApplied || res.Position.IsZero() || res.Refusal != "" ||
		res.State != state || res.Place != place {
		t.Fatalf("the queue's write answered %+v, want applied, %s at place %d", res, state, place)
	}
}

// requireQueueRefused fails t unless res is refused under refusal with a
// sentence holding says, and nothing was appended to n's log since before.
func requireQueueRefused(t *testing.T, n *node, before uint64, res code.QueueResult, err error,
	refusal code.MergeRefusal, says string) {

	t.Helper()
	if err != nil {
		t.Fatalf("ProposeQueue: %v", err)
	}
	if res.Refusal != refusal || !strings.Contains(res.Detail, says) {
		t.Fatalf("the queue's write answered %+v, want refused %s saying %q", res, refusal, says)
	}
	if after := n.end(); after != before {
		t.Fatalf("a refused write appended: the log moved from %d to %d", before, after)
	}
}

// entryState is entry's state and end reason as n's rows hold them.
func (n *node) entryState(repo, entry string) (code.QueueState, string) {
	n.t.Helper()
	var state, reason string
	if err := n.db.Reader().Read(n.t.Context(), func(tx *sql.Tx) error {
		return tx.QueryRowContext(n.t.Context(), `SELECT state, end_reason FROM code_queue
			WHERE repo_id = ? AND entry_id = ?`, repo, entry).Scan(&state, &reason)
	}); err != nil {
		n.t.Fatalf("read entry %s: %v", entry, err)
	}
	return code.QueueState(state), reason
}

// AN ENQUEUE IS DECIDED BY THE GATE: a change that meets no term of main's
// gate yet — no approval, its required check never run — is refused
// gate_unmet, naming every term it misses and whom each waits on, and nothing
// is appended; the same change once approved and checked joins the queue,
// first, its readiness then saying the queue lands it — computing, waiting on
// nobody — and a read of the queue listing it with its change's number. An
// enqueue formed against another target is refused stale, and one of a
// change into a branch whose gate requires no check no_queue. The enqueuer
// is a person, whose enqueue the chart judges all the same: it is what says
// the seat plat-lead's approval counts. And an attempt of the required check
// that passed on the change's head CHECKED FOR ANOTHER BRANCH counts for
// nothing: it ran that branch's checks file, and the gate reads the attempts
// of the change's own target alone.
//
// Mutations: decide an enqueue without its gate — the first enqueue applies,
// red; judge the chart for a seat's enqueue alone (QueueProposal.JudgesChart)
// — the person's enqueue is decided with no chart, plat-lead's approval
// counts for nothing, and the second is refused gate_unmet, red; read the
// gate's attempts by (repository, commit, check), whatever their target — the
// first refusal names no check, since release's passed unit is read for main,
// red.
func TestAnEnqueueIsDecidedByTheGate(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	n.chart = testChart()
	c, _ := committerOn(t, n, n.pub)
	w, _ := writerOn(t, n, n.pub)
	repo, change := queueRepo(t, n, c, "web")
	n.passed(repo, oidOf("B"), releaseRef, "unit")

	before := n.end()
	res, err := c.ProposeQueue(t.Context(), enqueueing(repo, change, ada, oidOf("B")))
	requireQueueRefused(t, n, before, res, err, code.MergeRefusalGateUnmet, "does not meet")
	var missed []code.PreconditionKind
	for _, p := range res.Unmet {
		missed = append(missed, p.Kind)
	}
	if want := []code.PreconditionKind{code.PreconditionFacts, code.PreconditionApproval,
		code.PreconditionCheck}; !slices.Equal(missed, want) {
		t.Fatalf("the refusal names %v, want %v", missed, want)
	}

	ready(t, n, w, repo, change)
	enqueue := enqueueing(repo, change, ada, oidOf("B"))
	res, err = c.ProposeQueue(t.Context(), enqueue)
	requireQueued(t, res, err, code.QueueWaiting, 1)
	p := requireState(t, readinessOf(t, n, 1), code.PreconditionQueue, code.PreconditionComputing,
		"place 1 in refs/heads/main's queue")
	if len(p.WaitsOn) > 0 {
		t.Errorf("a queued change waits on %+v, want nobody: the queue lands it", p.WaitsOn)
	}
	answer, err := n.reader.Read(t.Context(), code.ReadQuery{Kind: code.ReadQueue, Asker: ada, Repo: "web"})
	if err != nil || len(answer.Queue) != 1 || answer.Queue[0].EntryID != enqueue.Enqueue.EntryID ||
		answer.Queue[0].Number != 1 || answer.Queue[0].Place != 1 {
		t.Fatalf("the queue reads %+v (%v), want web!1 first", answer.Queue, err)
	}

	open := enqueueing(repo, change, ada, oidOf("B"))
	open.Enqueue.TargetRef = releaseRef
	before = n.end()
	res, err = c.ProposeQueue(t.Context(), open)
	requireQueueRefused(t, n, before, res, err, code.MergeRefusalStale, "merges into refs/heads/main")

	other, err := c.ProposeChange(t.Context(), opening(repo, releaseRef, featureRef, dev))
	requireChanged(t, other, err)
	direct := enqueueing(repo, other.ChangeID, ada, oidOf("R"))
	direct.Enqueue.TargetRef = featureRef
	before = n.end()
	res, err = c.ProposeQueue(t.Context(), direct)
	requireQueueRefused(t, n, before, res, err, code.MergeRefusalNoQueue, "has no merge queue")
}

// A QUEUE HOLDS AT MOST ITS ENTRIES: with room for one waiting entry, a second
// change's enqueue onto the same target is refused queue_full, appending
// nothing, before any of its gate is judged.
//
// Mutation: drop the count — the second enqueue is judged by its gate and
// refused gate_unmet instead, red.
func TestAQueueHoldsAtMostItsEntries(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	n.chart = testChart()
	c, _ := committerOn(t, n, n.pub)
	code.SetQueueEntries(c, 1)
	w, _ := writerOn(t, n, n.pub)
	repo, change := queueRepo(t, n, c, "web")
	ready(t, n, w, repo, change)
	res, err := c.ProposeQueue(t.Context(), enqueueing(repo, change, ada, oidOf("B")))
	requireQueued(t, res, err, code.QueueWaiting, 1)

	second, err := c.ProposeChange(t.Context(), opening(repo, releaseRef, mainRef, dev))
	requireChanged(t, second, err)
	before := n.end()
	res, err = c.ProposeQueue(t.Context(), enqueueing(repo, second.ChangeID, ada, oidOf("R")))
	requireQueueRefused(t, n, before, res, err, code.MergeRefusalQueueFull, "holds 1 waiting")
}

// A PUSH TO A QUEUED HEAD EJECTS IT: the queue holds what was enqueued — a
// change at one head — so a push moving the change's source branch ejects its
// entry in the push's own record, head_moved, and the change's readiness no
// longer says the queue lands it; enqueued again at its new head, it is a new
// entry.
//
// Mutation: leave a moved head's entry waiting — the queue would test a head
// nobody enqueued — and the entry reads waiting, red.
func TestAPushToAQueuedHeadEjectsIt(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	n.chart = testChart()
	c, _ := committerOn(t, n, n.pub)
	w, _ := writerOn(t, n, n.pub)
	repo, change := queueRepo(t, n, c, "web")
	ready(t, n, w, repo, change)
	enqueue := enqueueing(repo, change, ada, oidOf("B"))
	res, err := c.ProposeQueue(t.Context(), enqueue)
	requireQueued(t, res, err, code.QueueWaiting, 1)

	n.revise(repo, "B", "B2")
	if state, reason := n.entryState(repo, enqueue.Enqueue.EntryID); state != code.QueueEjected ||
		reason != string(code.EjectHeadMoved) {
		t.Fatalf("a queued change whose head a push moved holds an entry %s (%s), want ejected, head_moved",
			state, reason)
	}
	if slices.ContainsFunc(readinessOf(t, n, 1).Preconditions, func(p code.Precondition) bool {
		return p.Kind == code.PreconditionQueue
	}) {
		t.Error("an ejected change's readiness still says the queue lands it")
	}
	before := n.end()
	res, err = c.ProposeQueue(t.Context(), enqueueing(repo, change, ada, oidOf("B")))
	requireQueueRefused(t, n, before, res, err, code.MergeRefusalHeadMoved, "read it again")
}

// A CLOSED CHANGE LEAVES THE QUEUE — and so does one made a draft or
// retargeted: each record ends its change's live entry in its own apply,
// dequeued, naming what ended it; and an entry its enqueuer dequeues leaves
// too, a seat that only reads the repository refused.
//
// Mutation: leave the entry of a closed change waiting — the queue would land
// a change nobody wants merged — and the closed case reads waiting, red.
func TestAClosedChangeLeavesTheQueue(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		end    func(repo, change string, version int64) code.ChangeProposal
		reason code.QueueEndReason
	}{
		{"closed", func(repo, change string, version int64) code.ChangeProposal {
			return closing(repo, change, dev, version)
		}, code.EndClosed},
		{"drafted", func(repo, change string, version int64) code.ChangeProposal {
			return writing(dev, version, func(p *code.ChangeProposal) {
				p.Draft = &code.ChangeDraft{RepoID: repo, ChangeID: change, Draft: true}
			})
		}, code.EndDrafted},
		{"retargeted", func(repo, change string, version int64) code.ChangeProposal {
			return writing(dev, version, func(p *code.ChangeProposal) {
				p.Retarget = &code.ChangeRetarget{RepoID: repo, ChangeID: change, TargetRef: releaseRef}
			})
		}, code.EndRetargeted},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			n := newNode(t)
			n.chart = testChart()
			c, _ := committerOn(t, n, n.pub)
			w, _ := writerOn(t, n, n.pub)
			repo, change := queueRepo(t, n, c, "web")
			ready(t, n, w, repo, change)
			enqueue := enqueueing(repo, change, ada, oidOf("B"))
			res, err := c.ProposeQueue(t.Context(), enqueue)
			requireQueued(t, res, err, code.QueueWaiting, 1)
			ended, err := c.ProposeChange(t.Context(), tc.end(repo, change, 1))
			requireChanged(t, ended, err)
			if state, reason := n.entryState(repo, enqueue.Enqueue.EntryID); state != code.QueueDequeued ||
				reason != string(tc.reason) {
				t.Fatalf("a %s change's entry is %s (%s), want dequeued, %s", tc.name, state, reason, tc.reason)
			}
		})
	}

	t.Run("dequeued", func(t *testing.T) {
		t.Parallel()
		n := newNode(t)
		n.chart = testChart()
		c, _ := committerOn(t, n, n.pub)
		w, _ := writerOn(t, n, n.pub)
		repo, change := queueRepo(t, n, c, "web")
		ready(t, n, w, repo, change)
		enqueue := enqueueing(repo, change, dev, oidOf("B"))
		res, err := c.ProposeQueue(t.Context(), enqueue)
		requireQueued(t, res, err, code.QueueWaiting, 1)
		before := n.end()
		res, err = c.ProposeQueue(t.Context(), dequeueing(repo, enqueue.Enqueue.EntryID, agent("reader")))
		requireQueueRefused(t, n, before, res, err, code.MergeRefusalNotPermitted, "holds read")
		res, err = c.ProposeQueue(t.Context(), dequeueing(repo, enqueue.Enqueue.EntryID, dev))
		requireQueued(t, res, err, code.QueueDequeued, 0)
		if state, reason := n.entryState(repo, enqueue.Enqueue.EntryID); state != code.QueueDequeued ||
			reason != string(code.EndAsked) {
			t.Fatalf("a dequeued entry is %s (%s), want dequeued, asked", state, reason)
		}
	})
}

// A SEAT'S DEQUEUE IS HELD TO WHOEVER ITS TURN WORKS FOR (QueueProposal.
// Ceiling, M3-decisions U40 (a)): cto, who maintains the repository and so
// may take dev's entry out itself, does not for nobody — a stranger, or a
// person who may not act — nor for reader, a colleague who is neither the
// entry's enqueuer, its change's owner nor a maintainer, each refused
// not_permitted in words saying whom the work is for, appending nothing; and
// dev takes its own entry out for nobody, since its own work is steered by
// whoever woke it. A ceiling rides a seat's dequeue alone.
//
// Mutations: decide a dequeue without its ceiling — cto's dequeues for nobody
// and reader apply, red; refuse the seat's own entry under a ceiling — dev's
// dequeue is refused, red.
func TestASeatsDequeueIsHeldToWhoeverItsTurnWorksFor(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	n.chart = testChart()
	c, _ := committerOn(t, n, n.pub)
	w, _ := writerOn(t, n, n.pub)
	repo, change := queueRepo(t, n, c, "web")
	ready(t, n, w, repo, change)
	enqueue := enqueueing(repo, change, dev, oidOf("B"))
	res, err := c.ProposeQueue(t.Context(), enqueue)
	requireQueued(t, res, err, code.QueueWaiting, 1)
	entry := enqueue.Enqueue.EntryID
	nobody := &code.WriteCeiling{Kind: code.WriteCeilingNobody}
	for _, tc := range []struct {
		name    string
		ceiling *code.WriteCeiling
		says    string
	}{
		{"nobody", nobody, "may change nothing of this company's code"},
		{"a colleague who only reads", &code.WriteCeiling{Kind: code.WriteCeilingSeat, Seat: "reader"},
			"this work is for seat reader"},
	} {
		p := dequeueing(repo, entry, agent("cto"))
		p.Ceiling = tc.ceiling
		before := n.end()
		res, err := c.ProposeQueue(t.Context(), p)
		requireQueueRefused(t, n, before, res, err, code.MergeRefusalNotPermitted, tc.says)
	}

	for name, p := range map[string]code.QueueProposal{
		"an enqueue's":    enqueueing(repo, change, dev, oidOf("B")),
		"a person's":      dequeueing(repo, entry, ada),
		"for the asker's": dequeueing(repo, entry, dev),
	} {
		p.Ceiling = nobody
		if name == "for the asker's" {
			p.Ceiling = &code.WriteCeiling{Kind: code.WriteCeilingSeat, Seat: "dev"}
		}
		if err := p.Validate(); err == nil {
			t.Errorf("%s ceiling validated: only a seat's dequeue, made for somebody else, carries one", name)
		}
	}

	own := dequeueing(repo, entry, dev)
	own.Ceiling = nobody
	res, err = c.ProposeQueue(t.Context(), own)
	requireQueued(t, res, err, code.QueueDequeued, 0)
}

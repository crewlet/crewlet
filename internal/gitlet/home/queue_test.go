package home_test

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/gitlet"
	"github.com/crewlet/crewlet/internal/gitlet/home"
	"github.com/crewlet/crewlet/internal/queue/topics"
	"github.com/crewlet/crewlet/internal/statelog"
)

// THE MERGE QUEUE OVER ITS ROWS: one repository's main, its live entries and
// its live train as the `queue_due` read answers them (queueRows), each record
// the worker asks of the repository's committer applied to those rows as the
// code domain's applier applies it — a candidate puts its entries in its train,
// testing; an eject takes its entries out and abandons the train it ends; a
// land moves main to its tip, its entries out — every check it queues held as
// that commit's latest attempt, and the checks file every commit holds, and
// every attempt the worker cancels ended cancelled — the read answering, as
// given up, each attempt the worker queued that has not ended on a commit no
// train tests or landed; the chains a Gitlet node builds (queueChains), each entry's merge a commit derived
// from the commit it was built onto; and the work items closed (queueTasks).

const queueMain = "refs/heads/main"

// queueRows is a target's queue as its rows hold it.
type queueRows struct {
	mu       sync.Mutex
	tip      string
	depth    int
	batch    int
	required []string
	entries  []gitlet.QueueWorkEntry
	train    []gitlet.QueueWorkCandidate
	checks   map[string]map[string]gitlet.TipCheck
	files    map[string]string
	file     string
	queued   []gitlet.CheckQueued
	records  []gitlet.QueueProposal
	seq      uint64
	// refuse, where set, answers a land instead of the rows: the decide's
	// refusal of entries it would not land.
	refuse func(gitlet.Land) *gitlet.QueueResult
	// epoch is the repository's drop epoch as the rows hold it: a candidate
	// or a landing stating another is refused expunging, as the decide
	// refuses it.
	epoch int64
	// enqueued is what the rows count of main's enqueues since the instant a
	// read asks, and since every instant a read asked from.
	enqueued int
	since    []time.Time
	// mirrored is the repository a pull mirror, as the read answers it.
	mirrored bool
	// landed is every commit a landing put on main; cancelled every
	// attempt the worker ended, in order.
	landed    map[string]bool
	cancelled []gitlet.CheckFinished
	// owed is every landed entry still owing its work item's close, as a
	// landing's apply leaves it (code_queue.close_op), until a settle
	// clears it; settles every close a close_settled applied settled, in
	// order, and settleRecords how many records applied them — kept apart
	// from records, which are the queue's trains and landings.
	owed          []gitlet.QueueWorkEntry
	settles       []gitlet.SettledClose
	settleRecords int
	// pending, where set, answers a land pending: its rows applied, as the
	// committer's would be, but not yet on the asker's.
	pending bool
}

func newQueueRows(tip string, depth, batch int) *queueRows {
	return &queueRows{tip: tip, depth: depth, batch: batch, required: []string{"unit"},
		checks: map[string]map[string]gitlet.TipCheck{}, files: map[string]string{}, file: checksFile("unit"),
		seq: 100, landed: map[string]bool{}}
}

// enqueue puts n changes at the back of main's queue, every other one asking
// its work item closed, each enqueued by a seat of its own.
func (r *queueRows) enqueue(t *testing.T, n int) []gitlet.QueueWorkEntry {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	var added []gitlet.QueueWorkEntry
	for range n {
		number := int64(len(r.entries) + len(r.records) + 1)
		r.seq++
		at := time.Date(2026, 10, 1, 12, 0, int(r.seq), 0, time.UTC)
		e := gitlet.QueueWorkEntry{EntryID: gitlet.QueueEntryID(statelog.NewOpID(at, "")),
			Seq: statelog.Position{Stream: topics.CodeLogStream, Seq: r.seq}, State: gitlet.QueueWaiting,
			HeadOID: hexOf(fmt.Sprintf("head-%d-%d", r.seq, number)), Method: gitlet.MergeMethodMerge,
			EnqueuedBy: gitlet.Principal{Kind: gitlet.PrincipalAgent, Seat: fmt.Sprintf("dev-%d", r.seq)},
			Change: gitlet.ChainChange{ID: uuid.Must(uuid.NewV7()).String(), Number: int64(r.seq),
				Title: fmt.Sprintf("change %d", r.seq), SourceRef: fmt.Sprintf("refs/heads/c%d", r.seq),
				Owner: "dev", Authors: []string{"dev"}}}
		if r.seq%2 == 0 {
			e.CloseTask = true
			e.WorkItem = &types.WorkItem{Backend: types.WorkWorklet, ID: fmt.Sprintf("task-%d", r.seq),
				Key: fmt.Sprintf("ENG-%d", r.seq)}
		}
		r.entries = append(r.entries, e)
		added = append(added, e)
	}
	return added
}

// ended records name's latest attempt on commit as ended in state.
func (r *queueRows) ended(commit, name string, state gitlet.CheckState) {
	r.mu.Lock()
	defer r.mu.Unlock()
	held := r.checks[commit][name]
	if r.checks[commit] == nil {
		r.checks[commit] = map[string]gitlet.TipCheck{}
	}
	r.checks[commit][name] = gitlet.TipCheck{Name: name, Attempt: max(1, held.Attempt), State: state}
}

// running records name's latest attempt on commit as claimed by a runner.
func (r *queueRows) running(commit, name string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	held := r.checks[commit][name]
	held.State = gitlet.CheckStateRunning
	r.checks[commit][name] = held
}

// ends is every attempt the worker cancelled, in order.
func (r *queueRows) ends() []gitlet.CheckFinished {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.cancelled)
}

// givenUp is every attempt the worker queued that has not ended, on a commit
// no train tests or landed, as the read answers it. Under mu.
func (r *queueRows) givenUp() []gitlet.AbandonedCheck {
	tested := map[string]bool{}
	for _, c := range r.train {
		for _, commit := range c.Commits {
			tested[commit.Commit] = true
		}
	}
	var out []gitlet.AbandonedCheck
	for _, q := range r.queued {
		held := r.checks[q.OID][q.Name]
		if held.Attempt != q.Attempt || held.State.Ended() || tested[q.OID] || r.landed[q.OID] {
			continue
		}
		out = append(out, gitlet.AbandonedCheck{CheckKey: q.CheckKey, CandidateID: q.CandidateID,
			Reason: gitlet.CheckReasonAbandoned})
	}
	return out
}

// snapshot is the rows' train and live entries, as they stand.
func (r *queueRows) snapshot() ([]gitlet.QueueWorkCandidate, []gitlet.QueueWorkEntry) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.train), slices.Clone(r.entries)
}

// asked is every record and every check the worker asked, in order.
func (r *queueRows) asked() ([]gitlet.QueueProposal, []gitlet.CheckQueued) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.records), slices.Clone(r.queued)
}

// position is the rows' log position, past every record they applied.
func (r *queueRows) position() statelog.Position {
	return statelog.Position{Stream: topics.CodeLogStream, Seq: r.seq}
}

func (r *queueRows) Read(_ context.Context, q gitlet.ReadQuery) (gitlet.ReadAnswer, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	switch {
	case q.Kind == gitlet.ReadChangesDue || q.Kind == gitlet.ReadChecksDue:
		return gitlet.ReadAnswer{Position: r.position()}, nil
	case q.Kind != gitlet.ReadQueueDue || q.QueueDue == nil:
		return gitlet.ReadAnswer{}, errors.New("the queue asked a read that is not its listing")
	case q.QueueDue.RepoID == "":
		if len(r.entries) == 0 && len(r.givenUp()) == 0 && len(r.owed) == 0 {
			return gitlet.ReadAnswer{Position: r.position()}, nil
		}
		return gitlet.ReadAnswer{DueRepos: []string{repoA}, Position: r.position()}, nil
	}
	abandoned, owed := r.givenUp(), slices.Clone(r.owed)
	if len(r.entries) == 0 {
		return gitlet.ReadAnswer{QueueAbandoned: abandoned, QueueOwedCloses: owed, Position: r.position()}, nil
	}
	d := &gitlet.QueueDue{TargetRef: queueMain, Repo: "web", Tip: r.tip, Depth: r.depth,
		Batch: r.batch, Required: slices.Clone(r.required), Entries: slices.Clone(r.entries),
		DropEpoch: r.epoch, Mirrored: r.mirrored}
	if since := q.QueueDue.EnqueuedSince; !since.IsZero() {
		r.since = append(r.since, since)
		d.Enqueued = r.enqueued
	}
	for _, c := range r.train {
		c.Commits = slices.Clone(c.Commits)
		for i := range c.Commits {
			c.Commits[i].Checks = nil
			for _, name := range r.required {
				if held, ok := r.checks[c.Commits[i].Commit][name]; ok {
					c.Commits[i].Checks = append(c.Commits[i].Checks, held)
				}
			}
		}
		d.Train = append(d.Train, c)
	}
	return gitlet.ReadAnswer{DueQueue: d, QueueAbandoned: abandoned, QueueOwedCloses: owed,
		Position: r.position()}, nil
}

func (r *queueRows) GitRead(_ context.Context, q gitlet.GitQuery) (gitlet.GitAnswer, error) {
	if err := q.Validate(); err != nil {
		return gitlet.GitAnswer{}, err
	}
	if q.Kind != gitlet.GitChecksFile {
		return gitlet.GitAnswer{}, errors.New("the queue asked a git read that is not a checks file")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	raw, ok := r.files[q.ChecksFile.Commit]
	if !ok {
		raw = r.file
	}
	a := &gitlet.ChecksFileAnswer{Commit: q.ChecksFile.Commit, OID: blobOf(raw), Type: "blob",
		Size: int64(len(raw)), Raw: []byte(raw)}
	return gitlet.GitAnswer{Kind: gitlet.GitChecksFile, RepoID: q.RepoID, ChecksFile: a}, nil
}

func (r *queueRows) Checks(_ context.Context, req gitlet.ChecksRequest) (gitlet.ChecksResult, error) {
	if err := req.Validate(); err != nil {
		return gitlet.ChecksResult{}, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if f := req.Finished; f != nil && f.State == gitlet.CheckStateCancelled {
		held := r.checks[f.OID][f.Name]
		if held.Attempt != f.Attempt || held.State.Ended() {
			return gitlet.ChecksResult{Refusal: gitlet.CheckRefusalNoSuchCheck, Detail: "it ended"}, nil
		}
		held.State, held.Reason = f.State, f.Reason
		r.checks[f.OID][f.Name] = held
		r.cancelled = append(r.cancelled, *f)
		r.seq++
		return gitlet.ChecksResult{Outcome: statelog.OutcomeApplied, Position: r.position()}, nil
	}
	if req.Queued == nil {
		return gitlet.ChecksResult{}, errors.New("the queue asked a checks write that neither queues nor cancels")
	}
	q := *req.Queued
	r.queued = append(r.queued, q)
	if r.checks[q.OID] == nil {
		r.checks[q.OID] = map[string]gitlet.TipCheck{}
	}
	r.checks[q.OID][q.Name] = gitlet.TipCheck{Name: q.Name, Attempt: q.Attempt, State: gitlet.CheckStateQueued}
	r.seq++
	return gitlet.ChecksResult{Outcome: statelog.OutcomeApplied, Position: r.position()}, nil
}

func (r *queueRows) Queue(_ context.Context, p gitlet.QueueProposal) (gitlet.QueueResult, error) {
	if err := p.Validate(); err != nil {
		return gitlet.QueueResult{}, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if s := p.CloseSettled; s != nil {
		// EACH CLOSE STILL OWED SETTLED, every other skipped, and a record
		// settling none refused stale, as the decide does.
		var settled []gitlet.SettledClose
		for _, c := range s.Settled {
			i := slices.IndexFunc(r.owed, func(e gitlet.QueueWorkEntry) bool { return e.EntryID == c.EntryID })
			if i >= 0 {
				r.owed = slices.Delete(r.owed, i, i+1)
				settled = append(settled, c)
			}
		}
		if len(settled) == 0 {
			return gitlet.QueueResult{Refusal: gitlet.MergeRefusalStale, Detail: "none owes its work item a close"},
				nil
		}
		r.settles = append(r.settles, settled...)
		r.settleRecords++
		r.seq++
		return gitlet.QueueResult{Outcome: statelog.OutcomeApplied, Position: r.position()}, nil
	}
	r.records = append(r.records, p)
	switch {
	case (p.Candidate != nil || p.Land != nil) && p.DropEpoch != r.epoch:
		return gitlet.QueueResult{Refusal: gitlet.MergeRefusalExpunging,
			Detail: gitlet.ExpungingBuiltDetail("web", "train", "its home", p.DropEpoch, r.epoch)}, nil
	case p.Candidate != nil:
		c := p.Candidate
		if c.BaseOID != r.tip {
			return gitlet.QueueResult{Refusal: gitlet.MergeRefusalTargetMoved, Detail: "main moved"}, nil
		}
		r.abandon()
		for _, tc := range c.Candidates {
			wc := gitlet.QueueWorkCandidate{CandidateID: tc.CandidateID, Train: c.Train, Ordinal: tc.Ordinal,
				BaseOID: c.BaseOID, TipOID: tc.TipOID}
			for _, e := range tc.Entries {
				wc.Commits = append(wc.Commits, gitlet.QueueWorkCommit{EntryID: e.EntryID, Commit: e.Commit})
				i := r.entry(e.EntryID)
				r.entries[i].State, r.entries[i].Train, r.entries[i].CandidateID =
					gitlet.QueueTesting, c.Train, tc.CandidateID
			}
			r.train = append(r.train, wc)
		}
	case p.Eject != nil:
		for _, x := range p.Eject.Entries {
			if i := r.entry(x.EntryID); i >= 0 {
				if r.entries[i].Train != "" {
					r.abandon()
				}
				r.entries = slices.Delete(r.entries, i, i+1)
			}
		}
	case p.Land != nil:
		if r.refuse != nil {
			if res := r.refuse(*p.Land); res != nil {
				return *res, nil
			}
		}
		for _, e := range p.Land.Entries {
			// THE CLOSE IT OWES, as the land's apply leaves it.
			if landed := r.entries[r.entry(e.EntryID)]; landed.CloseTask && landed.WorkItem != nil {
				landed.State, landed.CloseOp = gitlet.QueueLanded, gitlet.CloseWorkItemOpID(p.OpID, e.EntryID)
				r.owed = append(r.owed, landed)
			}
			r.entries = slices.Delete(r.entries, r.entry(e.EntryID), r.entry(e.EntryID)+1)
			r.landed[e.Commit] = true
		}
		r.tip = p.Land.TipOID
		r.abandon()
		if r.pending {
			r.seq++
			return gitlet.QueueResult{Outcome: statelog.OutcomePending, Position: r.position()}, nil
		}
	}
	r.seq++
	return gitlet.QueueResult{Outcome: statelog.OutcomeApplied, Position: r.position()}, nil
}

// entry is where entry id stands among the live entries, -1 for nowhere.
// Under mu.
func (r *queueRows) entry(id string) int {
	return slices.IndexFunc(r.entries, func(e gitlet.QueueWorkEntry) bool { return e.EntryID == id })
}

// abandon ends the live train: its entries wait again. Under mu.
func (r *queueRows) abandon() {
	for i := range r.entries {
		if r.entries[i].Train != "" {
			r.entries[i].State, r.entries[i].Train, r.entries[i].CandidateID = gitlet.QueueWaiting, "", ""
		}
	}
	r.train = nil
}

func (r *queueRows) Converse(context.Context, gitlet.ConversationWrite) (gitlet.ConversationResult, error) {
	return gitlet.ConversationResult{}, errors.New("the queue asked a write of a change's conversation")
}

func (r *queueRows) Change(context.Context, gitlet.ChangeProposal) (gitlet.ChangeResult, error) {
	return gitlet.ChangeResult{}, errors.New("the queue asked a write of a change's lifecycle")
}

// queueChains is a Gitlet node's chain builds: each entry's merge a commit
// derived from the commit before it, an entry in conflicts skipped as a
// conflict on the path it names, one in rejected skipped rejected_object, the
// chain answering its first reach entries alone where reach is set — the
// rest not reached — and every build and every settle counted.
type queueChains struct {
	mu        sync.Mutex
	conflicts map[string]string
	rejected  map[string]bool
	reach     int
	builds    []gitlet.ChainBuild
	settled   []bool
	// epoch is the drop epoch of the copy every chain is built from.
	epoch int64
}

func (c *queueChains) BuildChain(_ context.Context, b gitlet.ChainBuild) (gitlet.ChainResult, error) {
	if err := b.Validate(); err != nil {
		return gitlet.ChainResult{}, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.builds = append(c.builds, b)
	res := gitlet.ChainResult{Tip: b.Base, DropEpoch: c.epoch}
	for i, e := range b.Entries {
		if c.reach > 0 && i == c.reach {
			break
		}
		if c.rejected[e.EntryID] {
			res.Commits = append(res.Commits, gitlet.ChainCommit{EntryID: e.EntryID,
				Skipped: gitlet.WriteRejectedObject, Detail: "an object this change makes is one git's check refuses"})
			continue
		}
		if path, ok := c.conflicts[e.EntryID]; ok {
			res.Commits = append(res.Commits, gitlet.ChainCommit{EntryID: e.EntryID, Skipped: gitlet.WriteConflict,
				Conflicts: []gitlet.Conflict{{Path: path}}})
			continue
		}
		res.Tip = hexOf(res.Tip + e.EntryID)
		res.Commits = append(res.Commits, gitlet.ChainCommit{EntryID: e.EntryID, Commit: res.Tip})
	}
	res.Settle = func(context.Context, bool) error { return nil }
	settle := res.Settle
	res.Settle = func(ctx context.Context, applied bool) error {
		c.mu.Lock()
		c.settled = append(c.settled, applied)
		c.mu.Unlock()
		return settle(ctx, applied)
	}
	return res, nil
}

// built is how many chains were built.
func (c *queueChains) built() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.builds)
}

// queueTasks is every work item a landing asked closed, in order: closed
// those it closed, and asked every close asked, fail's included.
type queueTasks struct {
	mu     sync.Mutex
	closed []home.TaskClose
	asked  []home.TaskClose
	// fail, where set, answers a close it returns an error for with that
	// error, closing nothing.
	fail func(home.TaskClose) error
}

func (q *queueTasks) CloseWorkItem(_ context.Context, c home.TaskClose) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.asked = append(q.asked, c)
	if q.fail != nil {
		if err := q.fail(c); err != nil {
			return err
		}
	}
	q.closed = append(q.closed, c)
	return nil
}

// closes is what q closed and what it was asked, in order.
func (q *queueTasks) closes() (closed, asked []home.TaskClose) {
	q.mu.Lock()
	defer q.mu.Unlock()
	return slices.Clone(q.closed), slices.Clone(q.asked)
}

// settled is every close a close_settled the rows applied settled, and the
// closes still owed.
func (r *queueRows) settled() ([]gitlet.SettledClose, []gitlet.QueueWorkEntry) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.settles), slices.Clone(r.owed)
}

// queueLoop is a loop over rows, building with chains and closing with tasks,
// never started: a case runs its queue's turns itself (home.QueueWork).
func queueLoop(t *testing.T, rows *queueRows, chains home.Chains, tasks home.Tasks) *home.Loop {
	t.Helper()
	l, err := home.New(home.Options{Chart: noChart,
		Estate: rows, Placement: asNode{placement: &placement{}, node: "git-a"},
		Nudges: newNudges(), Cache: unmerged{t: t}, Git: unmerged{t: t}, Chains: chains, Tasks: tasks,
		Share: units(2), NodeID: "git-a"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return l
}

// turn runs one turn of l's queue over rows, failing t on its error.
func turn(t *testing.T, l *home.Loop, rows *queueRows) {
	t.Helper()
	rows.mu.Lock()
	at := rows.position()
	rows.mu.Unlock()
	if err := home.QueueWork(t.Context(), l, repoA, at); err != nil {
		t.Fatalf("the queue's turn: %v", err)
	}
}

// hexOf is a whole object id derived from s.
func hexOf(s string) string {
	sum := sha1.Sum([]byte(s))
	return hex.EncodeToString(sum[:])
}

// THE QUEUE LANDS THE LONGEST GREEN PREFIX (M5-D12, M5-D13, M5-D15, M5-D27):
// six changes waiting at depth 3 and batch 2 are ONE train of three
// candidates of min(2, ⌈6/3⌉) = 2 changes each, built onto main's tip as one
// chain and recorded; each candidate's tip is queued the required check under
// the checks file of the train's BASE — never the one a candidate's own commit
// holds — for the candidate; and with the first candidate's tip still running,
// the second's passed and the third's failed, the first two candidates land in
// ONE land record — the second's tip holds the first's changes, so its pass is
// theirs — main moving to the second's tip, and every landed entry that asked
// it has its work item closed as its enqueuer under the landing's step id.
//
// Mutation: take ⌊W/D⌋ rather than ⌈W/D⌉ in BatchOf → candidates of one, red;
// stop the prefix at the first candidate that passed → only the first, red
// (it has not passed: nothing lands, red); read the checks file at a
// candidate's tip → the queued defs are the tip's, red; close a work item as
// the queue's node rather than the enqueuer → red.
func TestTheQueueLandsTheLongestGreenPrefix(t *testing.T) {
	t.Parallel()
	base := oid("a")
	rows := newQueueRows(base, 3, 2)
	entries := rows.enqueue(t, 6)
	chains, tasks := &queueChains{}, &queueTasks{}
	l := queueLoop(t, rows, chains, tasks)

	turn(t, l, rows)
	train, _ := rows.snapshot()
	if len(train) != 3 {
		t.Fatalf("six changes at depth 3 and batch 2 built a train of %d candidates, want 3", len(train))
	}
	for i, c := range train {
		if len(c.Commits) != 2 || c.Commits[0].EntryID != entries[2*i].EntryID ||
			c.Commits[1].EntryID != entries[2*i+1].EntryID || c.BaseOID != base {
			t.Fatalf("candidate %d holds %+v onto %s, want entries %d and %d onto main's tip", i+1, c.Commits,
				c.BaseOID, 2*i+1, 2*i+2)
		}
	}
	if chains.built() != 1 {
		t.Fatalf("the train took %d chain builds, want one", chains.built())
	}

	// THE TIPS' OWN FILES DEFINE THE CHECK OTHERWISE: a candidate could
	// narrow its own check, so the base's file is what each tip is held to.
	rows.mu.Lock()
	for _, c := range train {
		rows.files[c.TipOID] = checksFile("unit", "lint")
	}
	rows.mu.Unlock()
	turn(t, l, rows)
	_, queued := rows.asked()
	if len(queued) != 3 {
		t.Fatalf("the train's tips were queued %d checks, want one on each of 3: %+v", len(queued), queued)
	}
	for i, q := range queued {
		if q.OID != train[i].TipOID || q.Name != "unit" || q.Purpose != gitlet.PurposeCandidate ||
			q.CandidateID != train[i].CandidateID || q.Base != base || q.DefsOID != blobOf(checksFile("unit")) ||
			!q.Required {
			t.Fatalf("check %d queued %+v, want unit on candidate %d's tip, for the candidate, under the "+
				"base's checks file %s", i, q, i+1, blobOf(checksFile("unit")))
		}
	}

	rows.ended(train[1].TipOID, "unit", gitlet.CheckStatePassed)
	rows.ended(train[2].TipOID, "unit", gitlet.CheckStateFailed)
	turn(t, l, rows)
	records, _ := rows.asked()
	land := records[len(records)-1].Land
	if land == nil {
		t.Fatalf("the turn after the second candidate passed recorded %+v, want a landing", records[len(records)-1])
	}
	if land.BaseOID != base || land.TipOID != train[1].TipOID ||
		land.Train != train[0].Train || len(land.Entries) != 4 {
		t.Fatalf("the landing is %+v, want the first four entries landed from main's tip onto the second "+
			"candidate's tip", land)
	}
	for i, e := range land.Entries {
		want := train[i/2].Commits[i%2]
		if e.EntryID != want.EntryID || e.Commit != want.Commit || e.ChangeID != entries[i].Change.ID {
			t.Fatalf("landed entry %d is %+v, want %+v of change %s", i, e, want, entries[i].Change.ID)
		}
	}
	if rows.tip != train[1].TipOID {
		t.Fatalf("main is at %s after the landing, want the second candidate's tip", rows.tip)
	}
	var want []string
	for _, e := range entries[:4] {
		if e.CloseTask {
			want = append(want, e.EntryID)
		}
	}
	tasks.mu.Lock()
	closed := slices.Clone(tasks.closed)
	tasks.mu.Unlock()
	if len(closed) != len(want) {
		t.Fatalf("the landing closed %d work items, want the %d its entries asked: %+v", len(closed), len(want),
			closed)
	}
	landOp := records[len(records)-1].OpID
	for i, c := range closed {
		e := entries[slices.IndexFunc(entries, func(e gitlet.QueueWorkEntry) bool { return e.EntryID == want[i] })]
		if c.As != e.EnqueuedBy || c.WorkItem != *e.WorkItem || c.Change.ID != e.Change.ID ||
			c.OpID != statelog.StepOpID(landOp, "close-work-item", e.EntryID) || c.RepoID != repoA {
			t.Fatalf("close %d is %+v, want %s's work item closed as its enqueuer %+v under the landing's step",
				i, c, e.EntryID, e.EnqueuedBy)
		}
	}
}

// A RED BATCH IS BISECTED (M5-D13): a train of one candidate of four changes
// — depth 1, batch 4 — whose tip fails is bisected over its entries' own
// commits: the commit midway between the base (good) and the tip (red) is
// queued the failed check for the bisect; it passing, the next probe is
// midway between it and the tip; that one failing, it is the first red
// commit after the last good one, and its entry — the culprit, alone — is
// ejected check_failed naming the check. The eject abandons the train, and the
// next turn builds the three others onto main again, the culprit out.
//
// Mutation: probe the tip rather than midway → the first probe is the failed
// tip, red; eject the red commit's successor (or the whole batch) → red;
// keep the ejected entry in the rebuilt chain → red.
func TestARedBatchIsBisected(t *testing.T) {
	t.Parallel()
	base := oid("b")
	rows := newQueueRows(base, 1, 4)
	entries := rows.enqueue(t, 4)
	chains := &queueChains{}
	l := queueLoop(t, rows, chains, &queueTasks{})

	turn(t, l, rows)
	train, _ := rows.snapshot()
	if len(train) != 1 || len(train[0].Commits) != 4 {
		t.Fatalf("four changes at depth 1 and batch 4 are %+v, want one candidate of four", train)
	}
	commits := train[0].Commits
	rows.ended(commits[3].Commit, "unit", gitlet.CheckStateFailed)

	probe := func(want int) {
		t.Helper()
		_, queued := rows.asked()
		q := queued[len(queued)-1]
		if q.OID != commits[want].Commit || q.Purpose != gitlet.PurposeBisect || q.EntryID != commits[want].EntryID ||
			q.Name != "unit" || q.DefsOID != blobOf(checksFile("unit")) {
			t.Fatalf("the bisect probed %+v, want unit on entry %d's commit %s", q, want+1, commits[want].Commit)
		}
	}
	turn(t, l, rows)
	probe(1)
	rows.ended(commits[1].Commit, "unit", gitlet.CheckStatePassed)
	turn(t, l, rows)
	probe(2)
	rows.ended(commits[2].Commit, "unit", gitlet.CheckStateFailed)
	turn(t, l, rows)

	records, _ := rows.asked()
	eject := records[len(records)-1].Eject
	if eject == nil || len(eject.Entries) != 1 || eject.Entries[0].EntryID != entries[2].EntryID ||
		eject.Entries[0].Reason != gitlet.EjectCheckFailed || !slices.Equal(eject.Entries[0].Checks, []string{"unit"}) {
		t.Fatalf("the bisect ended in %+v, want entry 3 alone ejected check_failed naming unit",
			records[len(records)-1])
	}
	if train, _ := rows.snapshot(); len(train) != 0 {
		t.Fatalf("the culprit's eject left the train %+v standing", train)
	}

	turn(t, l, rows)
	train, live := rows.snapshot()
	if len(live) != 3 || len(train) != 1 || len(train[0].Commits) != 3 || train[0].BaseOID != base ||
		slices.ContainsFunc(train[0].Commits, func(c gitlet.QueueWorkCommit) bool {
			return c.EntryID == entries[2].EntryID
		}) {
		t.Fatalf("the train rebuilt after the eject is %+v, want the three others onto main", train)
	}
}

// A NEW HOME RESUMES THE QUEUE FROM ROWS: what a train has reached is its
// rows' — its candidates and the checks recorded on them — and nothing held by
// the home that built it, so a second loop over the same rows, as a home that
// took the repository over has, TESTS the standing train (its checks queued)
// and lands it, building nothing; and a target a person's push moved under
// the train abandons it — the next turn builds a new train onto the new tip.
//
// Mutation: build whenever the worker holds no train of its own (a memory of
// the last build) → the new home builds, red; drop QueueDue.TrainStands' base
// check → the train on the old tip is tested on, red.
func TestANewHomeResumesTheQueueFromRows(t *testing.T) {
	t.Parallel()
	base := oid("c")
	rows := newQueueRows(base, 2, 1)
	rows.enqueue(t, 2)
	first := &queueChains{}
	turn(t, queueLoop(t, rows, first, &queueTasks{}), rows)
	train, _ := rows.snapshot()
	if first.built() != 1 || len(train) != 2 {
		t.Fatalf("the first home built %d chains into %d candidates, want one of 2", first.built(), len(train))
	}

	next := &queueChains{}
	l := queueLoop(t, rows, next, &queueTasks{})
	turn(t, l, rows)
	if next.built() != 0 {
		t.Fatalf("the home that took the queue over built %d chains, want none: the train stands in its rows",
			next.built())
	}
	if _, queued := rows.asked(); len(queued) != 2 {
		t.Fatalf("the new home queued %d checks on the standing train, want one on each of its 2 tips",
			len(queued))
	}
	rows.ended(train[0].TipOID, "unit", gitlet.CheckStatePassed)
	turn(t, l, rows)
	records, _ := rows.asked()
	if land := records[len(records)-1].Land; land == nil || land.TipOID != train[0].TipOID || land.Train != train[0].Train {
		t.Fatalf("the new home recorded %+v, want the standing train's first candidate landed",
			records[len(records)-1])
	}

	// A PERSON'S PUSH MOVES MAIN under the train still testing what is left.
	rows.enqueue(t, 1)
	turn(t, l, rows)
	standing, _ := rows.snapshot()
	if len(standing) == 0 {
		t.Fatal("the queue built no train of the entries left")
	}
	rows.mu.Lock()
	rows.tip = oid("d")
	rows.mu.Unlock()
	built := next.built()
	turn(t, l, rows)
	train, _ = rows.snapshot()
	if next.built() != built+1 || len(train) == 0 || train[0].BaseOID != oid("d") ||
		train[0].Train == standing[0].Train {
		t.Fatalf("a train whose target moved is %+v after the turn (%d builds), want a new train onto the new "+
			"tip", train, next.built()-built)
	}
}

// EJECTING A CULPRIT CANCELS ITS TRAIN'S CHECKS: a train of two candidates
// (depth 2, batch 1) whose first candidate fails its unit check has its
// culprit ejected, which gives the train up — and the second candidate's
// attempts, its unit running and its lint still queued, measure a commit
// nothing will land while they hold a runner's cells. The turn that builds
// the next train cancels nothing, since a train built again may test a
// given-up attempt's commit (its snapshot is read before its candidate is
// recorded); the turn after cancels exactly those two attempts, `abandoned`,
// and none of the live train's.
//
// Mutation: drop the cancel → the given-up attempts run on, red; cancel in a
// turn that recorded a train → the build turn cancels, red.
func TestEjectingACulpritCancelsItsTrainsChecks(t *testing.T) {
	t.Parallel()
	base := oid("c")
	rows := newQueueRows(base, 2, 1)
	rows.required, rows.file = []string{"lint", "unit"}, checksFile("lint", "unit")
	entries := rows.enqueue(t, 3)
	l := queueLoop(t, rows, &queueChains{}, &queueTasks{})

	turn(t, l, rows)
	turn(t, l, rows)
	train, _ := rows.snapshot()
	if len(train) != 2 {
		t.Fatalf("the queue built %+v, want a train of two candidates of one change each", train)
	}
	first, second := train[0].Tip().Commit, train[1].Tip().Commit
	if _, queued := rows.asked(); len(queued) != 4 {
		t.Fatalf("the train's test queued %+v, want lint and unit on each candidate's tip", queued)
	}
	rows.ended(first, "unit", gitlet.CheckStateFailed)
	rows.ended(first, "lint", gitlet.CheckStatePassed)
	rows.running(second, "unit")

	turn(t, l, rows)
	if _, live := rows.snapshot(); len(live) != 2 || slices.ContainsFunc(live, func(e gitlet.QueueWorkEntry) bool {
		return e.EntryID == entries[0].EntryID
	}) {
		t.Fatalf("the red first candidate left %+v live, want its culprit ejected", live)
	}
	built := len(rows.ends())
	turn(t, l, rows)
	if ends := rows.ends(); len(ends) != built {
		t.Fatalf("the turn that built the next train cancelled %+v, want nothing: a train built again may "+
			"test what a given-up attempt measures", ends[built:])
	}
	turn(t, l, rows)
	train, _ = rows.snapshot()
	var got []string
	for _, f := range rows.ends() {
		if f.State != gitlet.CheckStateCancelled || f.Reason != gitlet.CheckReasonAbandoned ||
			slices.ContainsFunc(train, func(c gitlet.QueueWorkCandidate) bool { return c.Tip().Commit == f.OID }) {
			t.Fatalf("the queue ended %+v, want a given-up attempt cancelled abandoned", f)
		}
		got = append(got, f.OID+" "+f.Name)
	}
	slices.Sort(got)
	if want := []string{second + " lint", second + " unit"}; !slices.Equal(got, want) {
		t.Fatalf("the queue cancelled %v, want the given-up second candidate's queued lint and running unit, "+
			"%v", got, want)
	}
}

// A GIVEN-UP RUN IS QUEUED AGAIN FOR THE TRAIN THAT TESTS ITS COMMIT: a
// train built again from the same rows is the same commits, so the live train
// may test a commit whose latest attempt a given-up train's cancellation
// ended — `abandoned`, having measured nothing — and its test queues the next
// attempt as a requeue, as it does one its runner's infrastructure ended;
// one cancelled for any other reason is left, as stuck.
//
// Mutation: drop the given-up arm of the landing states → the cancelled
// attempt reads stuck and nothing is queued, red; requeue any cancellation →
// the superseded one is queued again, red.
func TestAGivenUpRunIsQueuedAgainForTheTrainThatTestsIt(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		reason  gitlet.CheckReason
		requeue bool
	}{{gitlet.CheckReasonAbandoned, true}, {gitlet.CheckReasonDequeued, true}, {gitlet.CheckReasonSuperseded, false}} {
		t.Run(string(c.reason), func(t *testing.T) {
			t.Parallel()
			rows := newQueueRows(oid("c"), 1, 1)
			rows.enqueue(t, 1)
			l := queueLoop(t, rows, &queueChains{}, &queueTasks{})
			turn(t, l, rows)
			turn(t, l, rows)
			train, _ := rows.snapshot()
			tip := train[0].Tip().Commit
			rows.mu.Lock()
			rows.checks[tip]["unit"] = gitlet.TipCheck{Name: "unit", Attempt: 1, State: gitlet.CheckStateCancelled,
				Reason: c.reason}
			rows.mu.Unlock()
			_, before := rows.asked()
			turn(t, l, rows)
			_, after := rows.asked()
			switch queued := after[len(before):]; {
			case c.requeue && (len(queued) != 1 || queued[0].OID != tip || queued[0].Attempt != 2 ||
				queued[0].Reason != gitlet.QueueRequeue):
				t.Fatalf("the train testing a commit whose run was cancelled %s queued %+v, want attempt 2 "+
					"requeued", c.reason, queued)
			case !c.requeue && len(queued) != 0:
				t.Fatalf("the train testing a commit whose run was cancelled %s queued %+v, want nothing",
					c.reason, queued)
			}
		})
	}
}

// THE QUEUE STATES THE DROP EPOCH IT BUILT AND READ AT (M6-D4): on a
// repository an expunge has begun on (epoch 1), a chain built from a copy
// still at the epoch before is recorded at that epoch and refused expunging,
// so no train stands; built again from a copy past the begin, its candidate
// states the copy's epoch and is recorded. And the landing of that train
// states the epoch its snapshot read: with another expunge begun after the
// train stood (epoch 2) — which holds a candidate's tip live — the landing
// decided from the read at epoch 2 states it and lands main.
//
// Mutation: propose the candidate at the snapshot's epoch rather than its
// chain's → the train built from the stale copy is recorded, red; propose the
// landing at no epoch → it is refused, and main stays, red.
func TestTheQueueStatesTheDropEpochItBuiltAndReadAt(t *testing.T) {
	t.Parallel()
	base := oid("e")
	rows := newQueueRows(base, 1, 1)
	rows.epoch = 1
	rows.enqueue(t, 1)
	chains := &queueChains{}
	l := queueLoop(t, rows, chains, &queueTasks{})

	turn(t, l, rows)
	records, _ := rows.asked()
	if train, _ := rows.snapshot(); len(train) != 0 || len(records) != 1 || records[0].Candidate == nil ||
		records[0].DropEpoch != 0 {
		t.Fatalf("a chain built from a copy before the begin recorded %+v and stands %+v, want its candidate "+
			"proposed at the copy's epoch 0 and refused", records, train)
	}

	chains.mu.Lock()
	chains.epoch = 1
	chains.mu.Unlock()
	turn(t, l, rows)
	records, _ = rows.asked()
	train, _ := rows.snapshot()
	if len(train) != 1 || records[len(records)-1].Candidate == nil || records[len(records)-1].DropEpoch != 1 {
		t.Fatalf("the chain built again from a copy past the begin recorded %+v, want its candidate at "+
			"epoch 1 standing", records[len(records)-1])
	}

	rows.ended(train[0].TipOID, "unit", gitlet.CheckStatePassed)
	rows.mu.Lock()
	rows.epoch = 2
	rows.mu.Unlock()
	turn(t, l, rows)
	records, _ = rows.asked()
	land := records[len(records)-1]
	if land.Land == nil || land.DropEpoch != 2 || rows.tip != train[0].TipOID {
		t.Fatalf("the landing recorded %+v with main at %s, want the train landed at the epoch its "+
			"snapshot read, 2, and main on its tip", land, rows.tip)
	}
}

// A CHAIN'S UNBUILDABLE ENTRY IS EJECTED, ITS UNREACHED ONES WAIT: three
// changes at depth 3 and batch 1, built by a chain that reached only two of
// them — the second skipped rejected_object — eject the second `conflict`,
// with no paths, as an entry no train can build; record a train of the first
// alone; and leave the third waiting in no train, neither ejected nor tested,
// for the train after this one.
//
// Mutation: leave a rejected_object entry unejected → no eject, red; eject
// the entries a chain did not reach → the third is gone, red.
func TestTheQueueEjectsAnUnbuildableEntryAndKeepsTheUnreached(t *testing.T) {
	t.Parallel()
	rows := newQueueRows(oid("9"), 3, 1)
	entries := rows.enqueue(t, 3)
	chains := &queueChains{rejected: map[string]bool{entries[1].EntryID: true}, reach: 2}
	l := queueLoop(t, rows, chains, &queueTasks{})

	turn(t, l, rows)
	records, _ := rows.asked()
	if len(records) != 2 || records[0].Eject == nil || len(records[0].Eject.Entries) != 1 ||
		records[0].Eject.Entries[0].EntryID != entries[1].EntryID ||
		records[0].Eject.Entries[0].Reason != gitlet.EjectConflict || len(records[0].Eject.Entries[0].Paths) != 0 ||
		records[1].Candidate == nil {
		t.Fatalf("the build recorded %+v, want the second entry ejected conflict, then its train", records)
	}
	train, live := rows.snapshot()
	if len(train) != 1 || len(train[0].Commits) != 1 || train[0].Commits[0].EntryID != entries[0].EntryID {
		t.Fatalf("the train is %+v, want the first entry alone", train)
	}
	third := slices.IndexFunc(live, func(e gitlet.QueueWorkEntry) bool { return e.EntryID == entries[2].EntryID })
	if third < 0 || live[third].State != gitlet.QueueWaiting || live[third].Train != "" {
		t.Fatalf("the entries are %+v, want the third the chain did not reach still waiting in no train", live)
	}
}

// THE QUEUE EJECTS WHAT IT CANNOT LAND, each for its own reason and naming
// what a person reads to mend it: three changes at depth 3 and batch 1, the
// second's merge conflicting on go.mod, build a train of the two others — the
// second ejected `conflict` naming the path, in its own record ahead of the
// train's — and the chain settled applied once its candidate record applied;
// both candidates green, the landing its decide refuses — the first entry's
// gate no longer met, the third's head moved — ejects each for its own
// refusal. And a train whose BASE's checks file defines the required check in
// no form that runs can never be green, so its entries are ejected
// `check_failed` naming the check, and nothing is queued on any tip.
//
// Mutation: leave a conflicting merge unejected → no eject, red; eject
// nothing a landing refuses head_moved → red; answer an undefined required
// check without the eject → red.
// A PULL MIRROR'S QUEUE IS SHED, NEVER BUILT: a repository that became a pull
// mirror with entries queued — nothing merges into it, so its candidates and
// its landings are refused repo_mirrored — has every live entry ejected
// repo_mirrored in one eject record, and no chain is built for it: not for a
// queue that never had a train, and not for a standing train whose candidates
// all passed, which would otherwise land.
//
// Mutation: drop the lane's mirrored arm (target) — the first turn builds a
// chain and records a candidate, the second lands the green train, red; eject
// with another reason — the record names it, red.
func TestAPullMirrorsQueueIsShedNeverBuilt(t *testing.T) {
	t.Parallel()
	shed := func(t *testing.T, rows *queueRows, chains *queueChains, live []gitlet.QueueWorkEntry, builds int) {
		t.Helper()
		records, _ := rows.asked()
		eject := records[len(records)-1].Eject
		if eject == nil || len(eject.Entries) != len(live) || slices.ContainsFunc(eject.Entries,
			func(x gitlet.Ejection) bool { return x.Reason != gitlet.EjectRepoMirrored }) {
			t.Fatalf("the pull mirror's turn recorded %+v, want its %d live entries ejected repo_mirrored",
				records[len(records)-1], len(live))
		}
		for i, e := range live {
			if eject.Entries[i].EntryID != e.EntryID {
				t.Errorf("the eject names %s at %d, want %s", eject.Entries[i].EntryID, i, e.EntryID)
			}
		}
		if got := chains.built(); got != builds {
			t.Errorf("%d chains were built, want %d: a pull mirror's queue is never built", got, builds)
		}
		if _, entries := rows.snapshot(); len(entries) != 0 {
			t.Errorf("%d entries are still live after the shed", len(entries))
		}
	}

	t.Run("waiting", func(t *testing.T) {
		t.Parallel()
		rows := newQueueRows(oid("c"), 2, 2)
		live := rows.enqueue(t, 3)
		rows.mirrored = true
		chains := &queueChains{}
		turn(t, queueLoop(t, rows, chains, &queueTasks{}), rows)
		shed(t, rows, chains, live, 0)
	})

	t.Run("a green train", func(t *testing.T) {
		t.Parallel()
		rows := newQueueRows(oid("d"), 2, 1)
		live := rows.enqueue(t, 2)
		chains := &queueChains{}
		l := queueLoop(t, rows, chains, &queueTasks{})
		turn(t, l, rows)
		train, _ := rows.snapshot()
		if len(train) != 2 {
			t.Fatalf("the train is %+v, want two candidates", train)
		}
		for _, c := range train {
			rows.ended(c.TipOID, "unit", gitlet.CheckStatePassed)
		}
		rows.mu.Lock()
		rows.mirrored = true
		rows.mu.Unlock()
		tip := rows.tip
		turn(t, l, rows)
		shed(t, rows, chains, live, 1)
		if records, _ := rows.asked(); slices.ContainsFunc(records, func(p gitlet.QueueProposal) bool {
			return p.Land != nil
		}) || rows.tip != tip {
			t.Errorf("the pull mirror's green train was landed: main at %s, want %s", rows.tip, tip)
		}
	})
}

func TestTheQueueEjectsWhatItCannotLand(t *testing.T) {
	t.Parallel()
	rows := newQueueRows(oid("e"), 3, 1)
	entries := rows.enqueue(t, 3)
	chains := &queueChains{conflicts: map[string]string{entries[1].EntryID: "go.mod"}}
	l := queueLoop(t, rows, chains, &queueTasks{})

	turn(t, l, rows)
	records, _ := rows.asked()
	if len(records) != 2 || records[0].Eject == nil || len(records[0].Eject.Entries) != 1 ||
		records[0].Eject.Entries[0].EntryID != entries[1].EntryID ||
		records[0].Eject.Entries[0].Reason != gitlet.EjectConflict ||
		!slices.Equal(records[0].Eject.Entries[0].Paths, []string{"go.mod"}) || records[1].Candidate == nil {
		t.Fatalf("the build recorded %+v, want the second entry ejected conflict on go.mod, then its train",
			records)
	}
	train, _ := rows.snapshot()
	if len(train) != 2 || train[0].Commits[0].EntryID != entries[0].EntryID ||
		train[1].Commits[0].EntryID != entries[2].EntryID {
		t.Fatalf("the train is %+v, want the first and the third entries a candidate each", train)
	}
	chains.mu.Lock()
	settled := slices.Clone(chains.settled)
	chains.mu.Unlock()
	if !slices.Equal(settled, []bool{true}) {
		t.Fatalf("the chain was settled %v, want once, applied", settled)
	}

	rows.ended(train[0].TipOID, "unit", gitlet.CheckStatePassed)
	rows.ended(train[1].TipOID, "unit", gitlet.CheckStatePassed)
	rows.mu.Lock()
	rows.refuse = func(gitlet.Land) *gitlet.QueueResult {
		return &gitlet.QueueResult{Refusal: gitlet.MergeRefusalGateUnmet, Detail: "would not land", Refused: []gitlet.RefusedEntry{
			{EntryID: entries[0].EntryID, Refusal: gitlet.MergeRefusalGateUnmet},
			{EntryID: entries[2].EntryID, Refusal: gitlet.MergeRefusalHeadMoved},
		}}
	}
	rows.mu.Unlock()
	turn(t, l, rows)
	records, _ = rows.asked()
	eject := records[len(records)-1].Eject
	if records[len(records)-2].Land == nil || eject == nil || len(eject.Entries) != 2 ||
		eject.Entries[0].EntryID != entries[0].EntryID || eject.Entries[0].Reason != gitlet.EjectGateUnmet ||
		eject.Entries[1].EntryID != entries[2].EntryID || eject.Entries[1].Reason != gitlet.EjectHeadMoved {
		t.Fatalf("the refused landing was followed by %+v, want the first entry ejected gate_unmet and the "+
			"third head_moved", records[len(records)-2:])
	}

	undefined := newQueueRows(oid("f"), 2, 1)
	undefined.file = checksFile("lint")
	left := undefined.enqueue(t, 2)
	l = queueLoop(t, undefined, &queueChains{}, &queueTasks{})
	turn(t, l, undefined)
	turn(t, l, undefined)
	records, queued := undefined.asked()
	eject = records[len(records)-1].Eject
	if len(queued) != 0 || eject == nil || len(eject.Entries) != 2 || eject.Entries[0].EntryID != left[0].EntryID ||
		eject.Entries[1].EntryID != left[1].EntryID ||
		slices.ContainsFunc(eject.Entries, func(x gitlet.Ejection) bool {
			return x.Reason != gitlet.EjectCheckFailed || !slices.Equal(x.Checks, []string{"unit"})
		}) {
		t.Fatalf("a train whose base defines no unit queued %+v and recorded %+v, want nothing queued and "+
			"both entries ejected check_failed naming unit", queued, records[len(records)-1])
	}
}

// landsOne queues one change on rows asking its work item closed, builds its
// train in a turn of l, passes its tip's check and lands it in the next: the
// entry, the landing's operation id and the landing turn's error, which a
// close left owed makes.
func landsOne(t *testing.T, rows *queueRows, l *home.Loop) (gitlet.QueueWorkEntry, string, error) {
	t.Helper()
	e := rows.enqueue(t, 1)[0]
	rows.mu.Lock()
	i := rows.entry(e.EntryID)
	rows.entries[i].CloseTask = true
	rows.entries[i].WorkItem = &types.WorkItem{Backend: types.WorkWorklet, ID: "task-" + e.EntryID[:8],
		Key: "ENG-" + e.EntryID[:4]}
	e = rows.entries[i]
	rows.mu.Unlock()
	turn(t, l, rows)
	train, _ := rows.snapshot()
	if len(train) != 1 {
		t.Fatalf("one change waiting built a train of %d candidates, want 1", len(train))
	}
	rows.ended(train[0].TipOID, "unit", gitlet.CheckStatePassed)
	rows.mu.Lock()
	at := rows.position()
	rows.mu.Unlock()
	err := home.QueueWork(t.Context(), l, repoA, at)
	records, _ := rows.asked()
	land := records[len(records)-1]
	if land.Land == nil {
		t.Fatalf("the turn after its tip passed recorded %+v, want its landing", land)
	}
	return e, land.OpID, err
}

// A LAND ANSWERED PENDING CLOSES ON THE NEXT TURN (M5-D27): the home closes a
// landed change's work item only once its landing has APPLIED, so a landing
// its committer answered pending closes nothing in the turn that asked it —
// but the landing's apply leaves the close owed on the entry's row, and the
// next turn reads it there and closes the item as the enqueuer, under the
// landing's own step (gitlet.CloseWorkItemOpID), and settles it closed.
//
// Mutation: drop the owed closes a queue_due read's first page answers
// (Work keeps no QueueOwedCloses) — the pending landing's item is never
// closed, red.
func TestALandAnsweredPendingClosesOnTheNextTurn(t *testing.T) {
	t.Parallel()
	rows := newQueueRows(oid("a"), 1, 1)
	rows.pending = true
	tasks := &queueTasks{}
	l := queueLoop(t, rows, &queueChains{}, tasks)
	e, landOp, err := landsOne(t, rows, l)
	if err != nil {
		t.Fatalf("the turn whose landing was answered pending: %v", err)
	}
	if closed, _ := tasks.closes(); len(closed) != 0 {
		t.Fatalf("a landing answered pending closed %+v, want nothing closed on its word", closed)
	}
	if _, owed := rows.settled(); len(owed) != 1 {
		t.Fatalf("the pending landing left %d closes owed, want its entry's", len(owed))
	}

	rows.mu.Lock()
	rows.pending = false
	rows.mu.Unlock()
	turn(t, l, rows)
	closed, _ := tasks.closes()
	if len(closed) != 1 || closed[0].OpID != gitlet.CloseWorkItemOpID(landOp, e.EntryID) ||
		closed[0].As != e.EnqueuedBy || closed[0].WorkItem != *e.WorkItem || closed[0].RepoID != repoA {
		t.Fatalf("the next turn closed %+v, want %s's work item closed as %+v under its landing's step", closed,
			e.EntryID, e.EnqueuedBy)
	}
	if settles, owed := rows.settled(); len(settles) != 1 || settles[0].EntryID != e.EntryID ||
		settles[0].Outcome != gitlet.CloseClosed || len(owed) != 0 {
		t.Fatalf("the close settled %+v with %d still owed, want the entry settled closed", settles, len(owed))
	}
}

// A FAILED CLOSE IS RETRIED (M5-D27): a close whose end the tracker cannot
// state — unreachable, its answer unknown — fails the turn and is NOT
// settled, so the next turn asks the same close again, under the same
// operation, and settles it once it lands; a close the tracker will never
// make (home.NotClosed: a vendor's item, an enqueuer who may not) is settled
// not_closed at once, its sentence on the record, and never asked again.
//
// Mutations: settle a close whose error is not a NotClosed — it is never
// asked again, red; settle a NotClosed one closed — its outcome is wrong,
// red.
func TestAFailedCloseIsRetried(t *testing.T) {
	t.Parallel()
	rows := newQueueRows(oid("a"), 1, 1)
	unreachable := errors.New("the tracker did not answer")
	tasks := &queueTasks{fail: func(home.TaskClose) error { return unreachable }}
	l := queueLoop(t, rows, &queueChains{}, tasks)
	e, landOp, err := landsOne(t, rows, l)
	if !errors.Is(err, unreachable) {
		t.Fatalf("a turn whose close failed answered %v, want the close's failure", err)
	}
	if settles, owed := rows.settled(); len(settles) != 0 || len(owed) != 1 {
		t.Fatalf("a close the tracker did not answer settled %+v with %d owed, want it left owed", settles,
			len(owed))
	}
	rows.mu.Lock()
	at := rows.position()
	rows.mu.Unlock()
	if err := home.QueueWork(t.Context(), l, repoA, at); !errors.Is(err, unreachable) {
		t.Fatalf("the next turn, its close failing again, answered %v, want the close's failure", err)
	}

	tasks.mu.Lock()
	tasks.fail = nil
	tasks.mu.Unlock()
	turn(t, l, rows)
	closed, asked := tasks.closes()
	want := gitlet.CloseWorkItemOpID(landOp, e.EntryID)
	if len(asked) != 3 || len(closed) != 1 || closed[0].OpID != want || asked[0].OpID != want ||
		asked[1].OpID != want {
		t.Fatalf("the close was asked %+v and made %+v, want the same close under %s asked on every turn "+
			"until it landed", asked, closed, want)
	}
	if settles, owed := rows.settled(); len(settles) != 1 || settles[0].Outcome != gitlet.CloseClosed ||
		len(owed) != 0 {
		t.Fatalf("the retried close settled %+v with %d owed, want it settled closed", settles, len(owed))
	}

	why := "ENG-9 is a jira work item, which is closed where it lives"
	tasks.mu.Lock()
	tasks.fail = func(home.TaskClose) error { return &home.NotClosed{Why: why} }
	tasks.mu.Unlock()
	refused, _, err := landsOne(t, rows, l)
	if err != nil {
		t.Fatalf("a turn whose close was refused for good answered %v, want it settled, not failed", err)
	}
	settles, owed := rows.settled()
	if len(settles) != 2 || settles[1].EntryID != refused.EntryID || settles[1].Outcome != gitlet.CloseNotClosed ||
		settles[1].Detail != why || len(owed) != 0 {
		t.Fatalf("a close the tracker will not make settled %+v with %d owed, want it settled not_closed "+
			"saying %q", settles, len(owed), why)
	}
	_, before := tasks.closes()
	turn(t, l, rows)
	if _, after := tasks.closes(); len(after) != len(before) {
		t.Fatalf("a settled close was asked again: %+v", after[len(before):])
	}
}

// A LANDING'S CLOSES ARE SETTLED IN ONE RECORD (M5-D27): a landing of three
// changes, each asking its work item closed, is three tracker writes, each
// as its enqueuer under its own step of the landing — and ONE close_settled
// record of how all three ended, in the landing's order, the one the tracker
// will not make settled not_closed with its sentence beside the two closed:
// never a code-log append a change, one after another inside the queue turn.
//
// Mutation: settle each close in a record of its own → three records, red.
func TestALandingsClosesAreSettledInOneRecord(t *testing.T) {
	t.Parallel()
	rows := newQueueRows(oid("a"), 1, 4)
	why := "ENG-2 is a jira work item, which is closed where it lives"
	entries := rows.enqueue(t, 3)
	rows.mu.Lock()
	for i := range rows.entries {
		rows.entries[i].CloseTask = true
		rows.entries[i].WorkItem = &types.WorkItem{Backend: types.WorkWorklet, ID: fmt.Sprintf("task-%d", i),
			Key: fmt.Sprintf("ENG-%d", i+1)}
	}
	rows.mu.Unlock()
	tasks := &queueTasks{fail: func(c home.TaskClose) error {
		if c.WorkItem.Key == "ENG-2" {
			return &home.NotClosed{Why: why}
		}
		return nil
	}}
	l := queueLoop(t, rows, &queueChains{}, tasks)
	turn(t, l, rows)
	train, _ := rows.snapshot()
	if len(train) != 1 || len(train[0].Commits) != 3 {
		t.Fatalf("the premise: three changes at depth 1 and batch 4 built %+v, want one candidate of three", train)
	}
	rows.ended(train[0].TipOID, "unit", gitlet.CheckStatePassed)
	turn(t, l, rows)

	if _, asked := tasks.closes(); len(asked) != 3 {
		t.Fatalf("the landing asked %d closes, want its three changes'", len(asked))
	}
	rows.mu.Lock()
	records := rows.settleRecords
	rows.mu.Unlock()
	settles, owed := rows.settled()
	if records != 1 || len(settles) != 3 || len(owed) != 0 {
		t.Fatalf("the landing's closes were settled by %d records as %+v with %d still owed, want one record "+
			"of all three", records, settles, len(owed))
	}
	for i, s := range settles {
		want := gitlet.SettledClose{EntryID: entries[i].EntryID, Outcome: gitlet.CloseClosed}
		if i == 1 {
			want.Outcome, want.Detail = gitlet.CloseNotClosed, why
		}
		if s != want {
			t.Errorf("close %d was settled %+v, want %+v", i, s, want)
		}
	}
}

// A NEW HOME CLOSES WHAT THE OLD ONE OWED (M5-D27): a home that stops between
// its landing applying and its close — here, one whose tracker could not
// answer — leaves the close on the rows, not in itself, so the home that
// takes the repository over closes the item in its first turn, under the
// landing's step the row names, as the enqueuer, and settles it.
//
// Mutation: close an owed item under an operation of the turn's own rather
// than the row's (TaskClose.OpID not CloseOp) — the new home's close is
// another write than the old one's, red.
func TestANewHomeClosesWhatTheOldOneOwed(t *testing.T) {
	t.Parallel()
	rows := newQueueRows(oid("a"), 1, 1)
	gone := &queueTasks{fail: func(home.TaskClose) error { return errors.New("the home stopped") }}
	e, landOp, err := landsOne(t, rows, queueLoop(t, rows, &queueChains{}, gone))
	if err == nil {
		t.Fatal("the old home's turn answered no failure for a close it could not make")
	}

	tasks := &queueTasks{}
	turn(t, queueLoop(t, rows, &queueChains{}, tasks), rows)
	closed, _ := tasks.closes()
	if len(closed) != 1 || closed[0].OpID != gitlet.CloseWorkItemOpID(landOp, e.EntryID) ||
		closed[0].As != e.EnqueuedBy {
		t.Fatalf("the new home closed %+v, want %s's work item closed as its enqueuer under its landing's step",
			closed, e.EntryID)
	}
	if settles, owed := rows.settled(); len(settles) != 1 || settles[0].EntryID != e.EntryID || len(owed) != 0 {
		t.Fatalf("the new home settled %+v with %d owed, want the old home's close settled", settles, len(owed))
	}
}

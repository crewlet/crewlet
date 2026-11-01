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

	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/code/home"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/queue/topics"
	"github.com/crewlet/crewlet/internal/statelog"
)

// THE MERGE QUEUE OVER ITS ROWS: one repository's main, its live entries and
// its live train as the `queue_due` read answers them (queueRows), each record
// the worker asks of the repository's committer applied to those rows as the
// code domain's applier applies it — a candidate puts its entries in its train,
// testing; an eject takes its entries out and abandons the train it ends; a
// land moves main to its tip, its entries out — every check it queues held as
// that commit's latest attempt, and the checks file every commit holds; the
// chains a git node builds (queueChains), each entry's merge a commit derived
// from the commit it was built onto; and the work items closed (queueTasks).

const queueMain = "refs/heads/main"

// queueRows is a target's queue as its rows hold it.
type queueRows struct {
	mu       sync.Mutex
	tip      string
	depth    int
	batch    int
	required []string
	entries  []code.QueueWorkEntry
	train    []code.QueueWorkCandidate
	checks   map[string]map[string]code.TipCheck
	files    map[string]string
	file     string
	queued   []code.CheckQueued
	records  []code.QueueProposal
	seq      uint64
	// refuse, where set, answers a land instead of the rows: the decide's
	// refusal of entries it would not land.
	refuse func(code.Land) *code.QueueResult
	// epoch is the repository's drop epoch as the rows hold it: a candidate
	// or a landing stating another is refused expunging, as the decide
	// refuses it.
	epoch int64
	// enqueued is what the rows count of main's enqueues since the instant a
	// read asks, and since every instant a read asked from.
	enqueued int
	since    []time.Time
}

func newQueueRows(tip string, depth, batch int) *queueRows {
	return &queueRows{tip: tip, depth: depth, batch: batch, required: []string{"unit"},
		checks: map[string]map[string]code.TipCheck{}, files: map[string]string{}, file: checksFile("unit"),
		seq: 100}
}

// enqueue puts n changes at the back of main's queue, every other one asking
// its work item closed, each enqueued by a seat of its own.
func (r *queueRows) enqueue(t *testing.T, n int) []code.QueueWorkEntry {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	var added []code.QueueWorkEntry
	for range n {
		number := int64(len(r.entries) + len(r.records) + 1)
		r.seq++
		at := time.Date(2026, 10, 1, 12, 0, int(r.seq), 0, time.UTC)
		e := code.QueueWorkEntry{EntryID: code.QueueEntryID(statelog.NewOpID(at, "")),
			Seq: statelog.Position{Stream: topics.CodeLogStream, Seq: r.seq}, State: code.QueueWaiting,
			HeadOID: hexOf(fmt.Sprintf("head-%d-%d", r.seq, number)), Method: code.MergeMethodMerge,
			EnqueuedBy: code.Principal{Kind: code.PrincipalAgent, Seat: fmt.Sprintf("dev-%d", r.seq)},
			Change: code.ChainChange{ID: uuid.Must(uuid.NewV7()).String(), Number: int64(r.seq),
				Title: fmt.Sprintf("change %d", r.seq), SourceRef: fmt.Sprintf("refs/heads/c%d", r.seq),
				Owner: "dev", Authors: []string{"dev"}}}
		if r.seq%2 == 0 {
			e.CloseTask = true
			e.WorkItem = &types.WorkItem{Backend: types.WorkNative, ID: fmt.Sprintf("task-%d", r.seq),
				Key: fmt.Sprintf("ENG-%d", r.seq)}
		}
		r.entries = append(r.entries, e)
		added = append(added, e)
	}
	return added
}

// ended records name's latest attempt on commit as ended in state.
func (r *queueRows) ended(commit, name string, state code.CheckState) {
	r.mu.Lock()
	defer r.mu.Unlock()
	held := r.checks[commit][name]
	if r.checks[commit] == nil {
		r.checks[commit] = map[string]code.TipCheck{}
	}
	r.checks[commit][name] = code.TipCheck{Name: name, Attempt: max(1, held.Attempt), State: state}
}

// snapshot is the rows' train and live entries, as they stand.
func (r *queueRows) snapshot() ([]code.QueueWorkCandidate, []code.QueueWorkEntry) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.train), slices.Clone(r.entries)
}

// asked is every record and every check the worker asked, in order.
func (r *queueRows) asked() ([]code.QueueProposal, []code.CheckQueued) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.records), slices.Clone(r.queued)
}

// position is the rows' log position, past every record they applied.
func (r *queueRows) position() statelog.Position {
	return statelog.Position{Stream: topics.CodeLogStream, Seq: r.seq}
}

func (r *queueRows) Read(_ context.Context, q code.ReadQuery) (code.ReadAnswer, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	switch {
	case q.Kind == code.ReadChangesDue || q.Kind == code.ReadChecksDue:
		return code.ReadAnswer{Position: r.position()}, nil
	case q.Kind != code.ReadQueueDue || q.QueueDue == nil:
		return code.ReadAnswer{}, errors.New("the queue asked a read that is not its listing")
	case q.QueueDue.RepoID == "":
		if len(r.entries) == 0 {
			return code.ReadAnswer{Position: r.position()}, nil
		}
		return code.ReadAnswer{DueRepos: []string{repoA}, Position: r.position()}, nil
	}
	if len(r.entries) == 0 {
		return code.ReadAnswer{Position: r.position()}, nil
	}
	d := &code.QueueDue{TargetRef: queueMain, Repo: "web", Tip: r.tip, Depth: r.depth,
		Batch: r.batch, Required: slices.Clone(r.required), Entries: slices.Clone(r.entries),
		DropEpoch: r.epoch}
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
	return code.ReadAnswer{DueQueue: d, Position: r.position()}, nil
}

func (r *queueRows) GitRead(_ context.Context, q code.GitQuery) (code.GitAnswer, error) {
	if err := q.Validate(); err != nil {
		return code.GitAnswer{}, err
	}
	if q.Kind != code.GitChecksFile {
		return code.GitAnswer{}, errors.New("the queue asked a git read that is not a checks file")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	raw, ok := r.files[q.ChecksFile.Commit]
	if !ok {
		raw = r.file
	}
	a := &code.ChecksFileAnswer{Commit: q.ChecksFile.Commit, OID: blobOf(raw), Type: "blob",
		Size: int64(len(raw)), Raw: []byte(raw)}
	return code.GitAnswer{Kind: code.GitChecksFile, RepoID: q.RepoID, ChecksFile: a}, nil
}

func (r *queueRows) Checks(_ context.Context, req code.ChecksRequest) (code.ChecksResult, error) {
	if err := req.Validate(); err != nil {
		return code.ChecksResult{}, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if req.Queued == nil {
		return code.ChecksResult{}, errors.New("the queue asked a checks write that queues nothing")
	}
	q := *req.Queued
	r.queued = append(r.queued, q)
	if r.checks[q.OID] == nil {
		r.checks[q.OID] = map[string]code.TipCheck{}
	}
	r.checks[q.OID][q.Name] = code.TipCheck{Name: q.Name, Attempt: q.Attempt, State: code.CheckStateQueued}
	r.seq++
	return code.ChecksResult{Outcome: statelog.OutcomeApplied, Position: r.position()}, nil
}

func (r *queueRows) Queue(_ context.Context, p code.QueueProposal) (code.QueueResult, error) {
	if err := p.Validate(); err != nil {
		return code.QueueResult{}, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.records = append(r.records, p)
	switch {
	case (p.Candidate != nil || p.Land != nil) && p.DropEpoch != r.epoch:
		return code.QueueResult{Refusal: code.MergeRefusalExpunging,
			Detail: code.ExpungingBuiltDetail("web", "train", "its home", p.DropEpoch, r.epoch)}, nil
	case p.Candidate != nil:
		c := p.Candidate
		if c.BaseOID != r.tip {
			return code.QueueResult{Refusal: code.MergeRefusalTargetMoved, Detail: "main moved"}, nil
		}
		r.abandon()
		for _, tc := range c.Candidates {
			wc := code.QueueWorkCandidate{CandidateID: tc.CandidateID, Train: c.Train, Ordinal: tc.Ordinal,
				BaseOID: c.BaseOID, TipOID: tc.TipOID}
			for _, e := range tc.Entries {
				wc.Commits = append(wc.Commits, code.QueueWorkCommit{EntryID: e.EntryID, Commit: e.Commit})
				i := r.entry(e.EntryID)
				r.entries[i].State, r.entries[i].Train, r.entries[i].CandidateID =
					code.QueueTesting, c.Train, tc.CandidateID
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
			r.entries = slices.Delete(r.entries, r.entry(e.EntryID), r.entry(e.EntryID)+1)
		}
		r.tip = p.Land.TipOID
		r.abandon()
	}
	r.seq++
	return code.QueueResult{Outcome: statelog.OutcomeApplied, Position: r.position()}, nil
}

// entry is where entry id stands among the live entries, -1 for nowhere.
// Under mu.
func (r *queueRows) entry(id string) int {
	return slices.IndexFunc(r.entries, func(e code.QueueWorkEntry) bool { return e.EntryID == id })
}

// abandon ends the live train: its entries wait again. Under mu.
func (r *queueRows) abandon() {
	for i := range r.entries {
		if r.entries[i].Train != "" {
			r.entries[i].State, r.entries[i].Train, r.entries[i].CandidateID = code.QueueWaiting, "", ""
		}
	}
	r.train = nil
}

func (r *queueRows) Converse(context.Context, code.ConversationWrite) (code.ConversationResult, error) {
	return code.ConversationResult{}, errors.New("the queue asked a write of a change's conversation")
}

func (r *queueRows) Change(context.Context, code.ChangeProposal) (code.ChangeResult, error) {
	return code.ChangeResult{}, errors.New("the queue asked a write of a change's lifecycle")
}

// queueChains is a git node's chain builds: each entry's merge a commit
// derived from the commit before it, an entry in conflicts skipped as a
// conflict on the path it names, and every build and every settle counted.
type queueChains struct {
	mu        sync.Mutex
	conflicts map[string]string
	builds    []code.ChainBuild
	settled   []bool
	// epoch is the drop epoch of the copy every chain is built from.
	epoch int64
}

func (c *queueChains) BuildChain(_ context.Context, b code.ChainBuild) (code.ChainResult, error) {
	if err := b.Validate(); err != nil {
		return code.ChainResult{}, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.builds = append(c.builds, b)
	res := code.ChainResult{Tip: b.Base, DropEpoch: c.epoch}
	for _, e := range b.Entries {
		if path, ok := c.conflicts[e.EntryID]; ok {
			res.Commits = append(res.Commits, code.ChainCommit{EntryID: e.EntryID, Skipped: code.WriteConflict,
				Conflicts: []code.Conflict{{Path: path}}})
			continue
		}
		res.Tip = hexOf(res.Tip + e.EntryID)
		res.Commits = append(res.Commits, code.ChainCommit{EntryID: e.EntryID, Commit: res.Tip})
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

// queueTasks is every work item a landing asked closed, in order.
type queueTasks struct {
	mu     sync.Mutex
	closed []home.TaskClose
}

func (q *queueTasks) CloseWorkItem(_ context.Context, c home.TaskClose) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.closed = append(q.closed, c)
	return nil
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
		if q.OID != train[i].TipOID || q.Name != "unit" || q.Purpose != code.PurposeCandidate ||
			q.CandidateID != train[i].CandidateID || q.Base != base || q.DefsOID != blobOf(checksFile("unit")) ||
			!q.Required {
			t.Fatalf("check %d queued %+v, want unit on candidate %d's tip, for the candidate, under the "+
				"base's checks file %s", i, q, i+1, blobOf(checksFile("unit")))
		}
	}

	rows.ended(train[1].TipOID, "unit", code.CheckStatePassed)
	rows.ended(train[2].TipOID, "unit", code.CheckStateFailed)
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
		e := entries[slices.IndexFunc(entries, func(e code.QueueWorkEntry) bool { return e.EntryID == want[i] })]
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
	rows.ended(commits[3].Commit, "unit", code.CheckStateFailed)

	probe := func(want int) {
		t.Helper()
		_, queued := rows.asked()
		q := queued[len(queued)-1]
		if q.OID != commits[want].Commit || q.Purpose != code.PurposeBisect || q.EntryID != commits[want].EntryID ||
			q.Name != "unit" || q.DefsOID != blobOf(checksFile("unit")) {
			t.Fatalf("the bisect probed %+v, want unit on entry %d's commit %s", q, want+1, commits[want].Commit)
		}
	}
	turn(t, l, rows)
	probe(1)
	rows.ended(commits[1].Commit, "unit", code.CheckStatePassed)
	turn(t, l, rows)
	probe(2)
	rows.ended(commits[2].Commit, "unit", code.CheckStateFailed)
	turn(t, l, rows)

	records, _ := rows.asked()
	eject := records[len(records)-1].Eject
	if eject == nil || len(eject.Entries) != 1 || eject.Entries[0].EntryID != entries[2].EntryID ||
		eject.Entries[0].Reason != code.EjectCheckFailed || !slices.Equal(eject.Entries[0].Checks, []string{"unit"}) {
		t.Fatalf("the bisect ended in %+v, want entry 3 alone ejected check_failed naming unit",
			records[len(records)-1])
	}
	if train, _ := rows.snapshot(); len(train) != 0 {
		t.Fatalf("the culprit's eject left the train %+v standing", train)
	}

	turn(t, l, rows)
	train, live := rows.snapshot()
	if len(live) != 3 || len(train) != 1 || len(train[0].Commits) != 3 || train[0].BaseOID != base ||
		slices.ContainsFunc(train[0].Commits, func(c code.QueueWorkCommit) bool {
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
// the last build) → the new home builds, red; drop trainStands' base check →
// the train on the old tip is tested on, red.
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
	rows.ended(train[0].TipOID, "unit", code.CheckStatePassed)
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

	rows.ended(train[0].TipOID, "unit", code.CheckStatePassed)
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
		records[0].Eject.Entries[0].Reason != code.EjectConflict ||
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

	rows.ended(train[0].TipOID, "unit", code.CheckStatePassed)
	rows.ended(train[1].TipOID, "unit", code.CheckStatePassed)
	rows.mu.Lock()
	rows.refuse = func(code.Land) *code.QueueResult {
		return &code.QueueResult{Refusal: code.MergeRefusalGateUnmet, Detail: "would not land", Refused: []code.RefusedEntry{
			{EntryID: entries[0].EntryID, Refusal: code.MergeRefusalGateUnmet},
			{EntryID: entries[2].EntryID, Refusal: code.MergeRefusalHeadMoved},
		}}
	}
	rows.mu.Unlock()
	turn(t, l, rows)
	records, _ = rows.asked()
	eject := records[len(records)-1].Eject
	if records[len(records)-2].Land == nil || eject == nil || len(eject.Entries) != 2 ||
		eject.Entries[0].EntryID != entries[0].EntryID || eject.Entries[0].Reason != code.EjectGateUnmet ||
		eject.Entries[1].EntryID != entries[2].EntryID || eject.Entries[1].Reason != code.EjectHeadMoved {
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
		slices.ContainsFunc(eject.Entries, func(x code.Ejection) bool {
			return x.Reason != code.EjectCheckFailed || !slices.Equal(x.Checks, []string{"unit"})
		}) {
		t.Fatalf("a train whose base defines no unit queued %+v and recorded %+v, want nothing queued and "+
			"both entries ejected check_failed naming unit", queued, records[len(records)-1])
	}
}

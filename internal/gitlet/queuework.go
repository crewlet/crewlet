package gitlet

import (
	"context"
	"database/sql"
	"fmt"
	"math"
	"slices"
	"time"

	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/store"
)

// A TARGET'S QUEUE WORK, READ FROM THE ROWS (M5-D12, M5-D13): a `queue_due`
// read of ONE repository answers its home one target's [QueueDue] a page —
// everything the home's queue worker (internal/gitlet/home's queue) decides its
// next step from, in ONE snapshot: the target's tip, the gate its queue lands
// under (its depth, batch and required checks), its live entries in
// queue order — as many as one train under that gate can take, each with its
// change and who enqueued it — and its live train, each candidate's commits
// with the latest attempt of each required check on each, for the target —
// and the repository's drop epoch, which a landing decided from the snapshot
// carries ([QueueProposal.DropEpoch]).
//
// ONE TARGET A PAGE, its cursor the target, and A PAGE READS WHAT ITS STEP
// NEEDS, by batches, never an entry at a time: the target's first depth ×
// batch live entries and one more for [QueueDue.More] — every entry a train
// can take, and enough to size its candidates ([QueueDue.Entries]) — their
// changes in ONE statement over the changes' key ([changeRowsIn]), and the
// changes' DESCRIPTIONS, a body and the authors each, only where the snapshot
// owes a build ([QueueDue.Builds]): a squash's commit message is written from
// them, and no other step reads them. They are two more statements over the
// bodies' and the authors' keys, driven from the list ([descriptionsOf]). A
// turn that tests, lands or bisects a standing train — every turn the
// train's own check records nudge, the busiest a queue sees — reads none of
// them: at the default gate, 8 entries and one statement of their changes,
// where it read 64 entries, 192 statements of them and up to two MiB of
// bodies at MaxChangeBodyBytes (internal/gitlet's package doc has the figures).
//
// A fast-forward's commit is its change's head, so its candidate reads the
// head's checks: a check is a measurement of a commit for its target, whatever
// purpose queued it (ADR-0098), and a head its gate saw pass needs no run
// again.
//
// A GIVEN-UP TRAIN'S CHECKS, ON THE FIRST PAGE: a repository's first page
// answers, beside its first target's work, the attempts its queues queued
// that have not ended, on a commit no train of their target tests or landed
// ([AbandonedCheck]) — every target's, since an eject or a dequeue of a
// target's last live entry leaves it no page of its own — which the home
// cancels once its targets have taken their steps. An eject, a dequeue, a
// target a person moved and a new candidate record each give a train up in
// their apply and touch no check row, which is the checks writer's alone: so
// the queue that gave the train up ends its attempts, as the check work ends a
// head's its change moved on from. Left running, every one measures a commit
// nothing will land while it holds a runner's cell ahead of the live train's,
// since a runner claims oldest first — up to depth times the required checks
// a red first candidate, an eject, a dequeue or a person's push. The listing
// of repositories names, after those holding a live entry, those owing such a
// cancellation, so a turn that failed to cancel is listed again.

// QueueDue is one target's queue work as one snapshot holds it
// ([ReadAnswer.DueQueue]).
type QueueDue struct {
	TargetRef string `json:"target_ref"`
	// Repo is the repository's name, as an operator knows it: what the
	// merge-rate alarm names the target by.
	Repo string `json:"repo,omitempty"`
	// Mirrored is the repository a PULL MIRROR ([RepoRef.Mirrored]), which
	// nothing merges into — a candidate and a landing on it are refused
	// repo_mirrored — so its home builds no train and puts every live entry
	// out ([EjectRepoMirrored]): an answer between nodes, so an older
	// reader drops it and reads the queue as it did.
	Mirrored bool `json:"mirrored,omitempty"`
	// Tip is the target's tip, what a train is built onto.
	Tip string `json:"tip"`
	// Depth, Batch and Required are the target's gate as its queue lands
	// under it ([GateRules]): how many candidates it tests at once, how many
	// changes one takes, and the checks every candidate's tip passes before
	// it lands, sorted.
	Depth    int      `json:"depth"`
	Batch    int      `json:"batch"`
	Required []string `json:"required,omitempty"`
	// Entries are the target's live entries in queue order: the first
	// Depth × Batch — every entry one train can take, since a candidate
	// takes at most Batch and a train at most Depth of them, and as many as
	// sizing its candidates needs ([BatchOf] in internal/gitlet/home takes
	// min(Batch, ⌈W/Depth⌉), which More answers past Depth × (Batch − 1)) —
	// and More where more wait behind them. A train its gate's depth or
	// batch has since narrowed may test an entry past them, and so stands no
	// more ([QueueDue.TrainStands]): it is built again under the gate.
	Entries []QueueWorkEntry `json:"entries"`
	More    bool             `json:"more,omitempty"`
	// Train is the target's live train's candidates in ordinal order, none
	// where it holds no train.
	Train []QueueWorkCandidate `json:"train,omitempty"`
	// DropEpoch is the repository's drop epoch as the snapshot holds it: the
	// epoch a landing decided from this snapshot states
	// ([QueueProposal.DropEpoch]). A standing train's candidates were each
	// recorded at the epoch of the copy their chain was built from, and an
	// expunge begun since holds every candidate's tip live (its
	// `expunge_tips`), so a landing of one is judged at the epoch its home
	// read the train at: one begun between the read and the decide is
	// refused `expunging`, and the next turn reads the train again.
	DropEpoch int64 `json:"drop_epoch,omitempty"`
	// Enqueued is how many entries were enqueued on the target at or after
	// the read's [QueueDueQuery.EnqueuedSince] — live, landed, ejected or
	// dequeued alike, since each was a change asked to land — up to
	// [MergeRateScan]; zero where the read asked no count, or the target's
	// gate requires no check, so it has no queue whose merge rate anything
	// prices (M5-D28).
	Enqueued int `json:"enqueued,omitempty"`
}

// QueueWorkEntry is one live entry as the home's queue reads it: its row, who
// enqueued it — whom its merge commit names and its landing lands it as, and
// whom its work item is closed as — and its change, as its chain commit is
// written from. The change's Body and Authors are read only where the
// snapshot owes a build ([QueueDue.Builds]), the one step that writes a
// commit message from them; every other snapshot answers them empty.
type QueueWorkEntry struct {
	EntryID     string            `json:"entry_id"`
	Seq         statelog.Position `json:"seq"`
	State       QueueState        `json:"state"`
	HeadOID     string            `json:"head_oid"`
	Method      MergeMethod       `json:"method"`
	CloseTask   bool              `json:"close_task,omitempty"`
	EnqueuedBy  Principal         `json:"enqueued_by"`
	Train       string            `json:"train,omitempty"`
	CandidateID string            `json:"candidate_id,omitempty"`
	Change      ChainChange       `json:"change"`
	// WorkItem is the change's work item, nil for none: what a landing
	// closes where its entry asked it to.
	WorkItem *types.WorkItem `json:"work_item,omitempty"`
	// CloseOp is, on an owed close ([ReadAnswer.QueueOwedCloses]), the
	// operation its work item is closed under ([CloseWorkItemOpID] of the
	// landing's), "" on every other entry. An answer between nodes, so an
	// older reader drops it and leaves the close owed for a newer home.
	CloseOp string `json:"close_op,omitempty"`
}

// QueueWorkCandidate is one candidate of a target's live train: its id, its
// ordinal, the commit it was built onto and its tip, and each entry's commit
// with the checks recorded on it.
type QueueWorkCandidate struct {
	CandidateID string            `json:"candidate_id"`
	Train       string            `json:"train"`
	Ordinal     int               `json:"ordinal"`
	BaseOID     string            `json:"base_oid"`
	TipOID      string            `json:"tip_oid"`
	Commits     []QueueWorkCommit `json:"commits"`
}

// Tip is the candidate's last commit: its tip, what its checks run on.
func (c QueueWorkCandidate) Tip() QueueWorkCommit { return c.Commits[len(c.Commits)-1] }

// QueueWorkCommit is one entry's commit in its candidate, and the latest
// attempt of each required check recorded on it for the target — none for a
// name never queued there.
type QueueWorkCommit struct {
	EntryID string     `json:"entry_id"`
	Commit  string     `json:"commit"`
	Checks  []TipCheck `json:"checks,omitempty"`
}

// TrainStands reports d's train still the one to test: built onto the
// target's tip, every entry it tests live in it and among d's entries. A
// train its target moved under — a person's push or override merge — or that
// tests an entry no longer live does not stand, and is built again.
func (d QueueDue) TrainStands() bool {
	if len(d.Train) == 0 || d.Train[0].BaseOID != d.Tip {
		return false
	}
	train := d.Train[0].Train
	for _, c := range d.Train {
		for _, commit := range c.Commits {
			i := slices.IndexFunc(d.Entries, func(e QueueWorkEntry) bool { return e.EntryID == commit.EntryID })
			if i < 0 || d.Entries[i].Train != train || d.Entries[i].State != QueueTesting {
				return false
			}
		}
	}
	return true
}

// Builds reports d a snapshot its home builds a train from: a target of a
// repository it may merge into, holding live entries, gated on a required
// check, with a tip to build onto and no train standing on it. The reader
// answers the entries' descriptions on such a snapshot alone, and the home's
// queue worker builds on it alone, both by this one predicate, so neither can
// build from a snapshot the other read without them.
func (d QueueDue) Builds() bool {
	return len(d.Entries) > 0 && !d.Mirrored && len(d.Required) > 0 && d.Tip != ZeroOID && !d.TrainStands()
}

// Check is the latest attempt of name recorded on the commit, and false for
// none.
func (c QueueWorkCommit) Check(name string) (TipCheck, bool) {
	for _, held := range c.Checks {
		if held.Name == name {
			return held, true
		}
	}
	return TipCheck{}, false
}

// AbandonedCheck is an attempt a merge queue queued — a candidate's tip's or
// a bisect probe's — that has not ended, on a commit its target's queue no
// longer tests: its key, the candidate it was queued for and the reason the
// home cancels it for — [CheckReasonDequeued] where a change that candidate
// merged left its queue, [CheckReasonAbandoned] for any other giving up.
//
// AN ANSWER'S ROW, NOT A RECORD'S, so it takes no versionedFields row. AS A
// STATED CONDITION (M3-O84 §2 item 5): no build that carries the code domain
// has run without it — none of the domain is on main — so a build answering a
// `queue_due` read with none is one nobody ran; were one ever to run first,
// its home would cancel nothing, which is what such a build knows.
type AbandonedCheck struct {
	CheckKey
	CandidateID string      `json:"candidate_id"`
	Reason      CheckReason `json:"reason"`
}

// MaxAbandonedChecks is the most given-up attempts one `queue_due` page
// answers: a whole train's at the deepest queue and the widest gate,
// MaxQueueDepth × MaxRequiredChecks — so the attempts one giving up leaves
// are cancelled in one turn — and the rest on the next, which each
// cancellation's record nudges.
const MaxAbandonedChecks = MaxQueueDepth * MaxRequiredChecks

// abandonedChecksIn is repository repoID's given-up attempts, at most
// [MaxAbandonedChecks] in the order of code_checks_train_idx: one seek of that
// index, each attempt's commit sought in code_queue_commit_idx and its
// candidate in code_queue_dequeued_idx (replicated 0140).
func abandonedChecksIn(ctx context.Context, tx *sql.Tx, repoID string) ([]AbandonedCheck, error) {
	rows, err := tx.QueryContext(ctx, abandonedChecksSQL, repoID, MaxAbandonedChecks)
	if err != nil {
		return nil, fmt.Errorf("gitlet: read the checks of %s's given-up trains: %w", repoID, err)
	}
	defer func() { _ = rows.Close() }()
	var out []AbandonedCheck
	for rows.Next() {
		x := AbandonedCheck{CheckKey: CheckKey{RepoID: repoID}, Reason: CheckReasonAbandoned}
		var dequeued bool
		if err := rows.Scan(&x.Target, &x.CandidateID, &x.OID, &x.Name, &x.Attempt, &dequeued); err != nil {
			return nil, fmt.Errorf("gitlet: read the checks of %s's given-up trains: %w", repoID, err)
		}
		if dequeued {
			x.Reason = CheckReasonDequeued
		}
		out = append(out, x)
	}
	return out, rows.Err()
}

// The given-up trains' reads (registered in plan_test.go): a repository's
// attempts over code_checks_train_idx, and the repositories holding one,
// grouped on its leading column — each index's partial predicate spelled as
// it declares it. An attempt is GIVEN UP where no entry of its target's queue
// a train tests or landed was built at its commit (code_queue_commit_idx), and
// NOT where its candidate is gone: a train built again from the same rows is
// the same commits byte for byte (gitlet/host's BuildChain), and its dispatch of
// a commit an attempt is already measuring is answered from that attempt's row
// (decideQueued), which keeps naming the candidate it was first queued for —
// so the live train's own measurement may name a dropped candidate, and only
// the commit says whether anything still tests it. A landed commit's checks
// are its target's tip work's to read.
const (
	abandonedChecksSQL = `SELECT k.target_ref, k.candidate_id, k.commit_oid, k.name, k.attempt,
			EXISTS (SELECT 1 FROM code_queue AS d INDEXED BY code_queue_dequeued_idx
				WHERE d.repo_id = k.repo_id AND d.candidate_id = k.candidate_id
					AND d.state = 'dequeued' AND d.candidate_id != '')
		FROM code_checks AS k INDEXED BY code_checks_train_idx
		WHERE k.repo_id = ?1 AND k.state IN ('queued', 'running') AND k.candidate_id != ''
			AND ` + untested + `
		ORDER BY k.target_ref, k.candidate_id, k.commit_oid, k.name, k.attempt LIMIT ?2`
	abandonedReposSQL = `SELECT k.repo_id FROM code_checks AS k INDEXED BY code_checks_train_idx
		WHERE k.repo_id > ?1 AND k.state IN ('queued', 'running') AND k.candidate_id != ''
			AND ` + untested + `
			AND EXISTS (SELECT 1 FROM code_repos AS r WHERE r.id = k.repo_id AND r.status = 'active')
		GROUP BY k.repo_id ORDER BY k.repo_id LIMIT ?2`
	// untested is an attempt k's commit tested by no train of its target and
	// landed by none.
	untested = `NOT EXISTS (SELECT 1 FROM code_queue AS q INDEXED BY code_queue_commit_idx
				WHERE q.repo_id = k.repo_id AND q.target_ref = k.target_ref AND q.commit_oid = k.commit_oid
					AND q.state IN ('testing', 'landed') AND q.commit_oid != '')`
)

// dueQueue files repository repoID's next target's queue work after cursor —
// "" for its first — into out, with that target as the next page's cursor.
// The target is the first holding a live entry past cursor: the first page's
// one row of queueOfRepoSQL, a later page's of queueOfRepoAfterSQL past every
// seq of the cursor's target. A non-zero since counts the target's enqueues
// from it ([enqueuedSince]) where its gate requires a check. The first page
// answers the repository's given-up attempts too ([abandonedChecksIn]),
// whether or not a target holds a live entry.
func dueQueue(ctx context.Context, tx *sql.Tx, repoID, cursor string, since time.Time, out *ReadAnswer) error {
	if cursor == "" {
		abandoned, err := abandonedChecksIn(ctx, tx, repoID)
		if err != nil {
			return err
		}
		out.QueueAbandoned = abandoned
		if out.QueueOwedCloses, err = owedClosesIn(ctx, tx, repoID); err != nil {
			return err
		}
	}
	stmt, args := queueOfRepoSQL, []any{repoID, 1}
	if cursor != "" {
		stmt, args = queueOfRepoAfterSQL, []any{repoID, cursor, cursor, cursor, int64(math.MaxInt64), 1}
	}
	first, err := entriesBy(ctx, tx, stmt, args...)
	if err != nil || len(first) == 0 {
		return err
	}
	target := first[0].TargetRef
	gate, err := gateOf(ctx, tx, repoID, target)
	if err != nil {
		return err
	}
	// AS MANY ENTRIES AS ONE TRAIN UNDER THE GATE TAKES, and one more to
	// say whether more wait ([QueueDue.Entries]).
	scan := max(1, gate.QueueDepth*gate.QueueBatch)
	scanned, err := entriesBy(ctx, tx, queueOfTargetSQL, repoID, target, 0, scan+1)
	if err != nil {
		return err
	}
	due := &QueueDue{TargetRef: target, More: len(scanned) > scan}
	scanned = scanned[:min(len(scanned), scan)]
	due.Depth, due.Batch, due.Required = gate.QueueDepth, gate.QueueBatch, gate.Checks
	if due.Tip, err = targetTip(ctx, tx, repoID, target); err != nil {
		return err
	}
	// THE DROP EPOCH, by the decide's own read of the repository's row
	// (decideRepoSQL, a seek of its key), so the epoch the home states is
	// the one its committer compares.
	repo, ok, err := pushRepoIn(ctx, tx, repoID)
	switch {
	case err != nil:
		return err
	case ok:
		due.DropEpoch, due.Repo, due.Mirrored = repo.dropEpoch, repo.name, repo.mirrored
	}
	if !since.IsZero() && len(gate.Checks) > 0 {
		if due.Enqueued, err = enqueuedSince(ctx, tx, repoID, target, since); err != nil {
			return err
		}
	}
	if due.Entries, err = workEntriesOf(ctx, tx, scanned); err != nil {
		return err
	}
	trains, err := trainsOf(ctx, tx, repoID, target)
	if err != nil {
		return err
	}
	if due.Train, err = trainChecks(ctx, tx, repoID, target, trains, gate.Checks); err != nil {
		return err
	}
	if due.Builds() {
		if err = descriptionsOf(ctx, tx, due.Entries); err != nil {
			return err
		}
	}
	out.DueQueue, out.Next = due, target
	return nil
}

// enqueuedSince is how many entries were enqueued on repoID's target at or
// after since, up to [MergeRateScan] — what a queue was asked to land within
// its merge rate's window (M5-D28), live and ended alike. ONE SEEK of
// code_queue_enqueued_idx, the target's entries by their enqueue's instant,
// answered from the index and stopped at the scan: the walk is the count,
// never another target's endings nor a row of the table, so a page costs the
// same whatever the repository's other queues land. Measured by
// BenchmarkQueueDuePage (-benchtime 3x -count 1, the runner's two E-cores) on
// four targets each with an hour of 3 840 landings: 8.6 ms a page with the
// two seeks this replaced — the target's live entries over
// code_queue_live_idx and the repository's endings over code_queue_ended_idx,
// each row fetched to filter — and 2.8 ms with this one, which walks main's
// own 3 841 entries and nothing of the other targets'.
func enqueuedSince(ctx context.Context, tx *sql.Tx, repoID, target string, since time.Time) (int, error) {
	var n int
	if err := tx.QueryRowContext(ctx, enqueuedSQL, repoID, target, store.EncodeTime(since), MergeRateScan).Scan(
		&n); err != nil {
		return 0, fmt.Errorf("gitlet: count %s's entries enqueued since %s: %w", target, since, err)
	}
	return n, nil
}

// enqueuedSQL is the merge rate's count (registered in plan_test.go): a
// target's entries enqueued from an instant over code_queue_enqueued_idx, up
// to a limit.
const enqueuedSQL = `SELECT COUNT(*) FROM (SELECT 1 FROM code_queue INDEXED BY code_queue_enqueued_idx
	WHERE repo_id = ? AND target_ref = ? AND enqueued_at >= ? LIMIT ?)`

// workEntriesOf is entries as the home's queue reads them, their changes read
// in ONE statement by the changes' key ([changeRowsIn]) — their descriptions
// left to [descriptionsOf].
func workEntriesOf(ctx context.Context, tx *sql.Tx, entries []queueEntry) ([]QueueWorkEntry, error) {
	ids := make([]string, len(entries))
	for i, e := range entries {
		ids[i] = e.ChangeID
	}
	rows, err := changeRowsIn(ctx, tx, ids)
	if err != nil {
		return nil, err
	}
	out := make([]QueueWorkEntry, len(entries))
	for i, e := range entries {
		row, found := rows[e.ChangeID]
		if !found {
			// A LIVE ENTRY NAMES A CHANGE ITS REPOSITORY HOLDS: every record
			// that removes a change ends its entry in the same apply.
			return nil, fmt.Errorf("gitlet: entry %s of %s's queue names change %s, which the repository does "+
				"not hold", e.EntryID, e.TargetRef, e.ChangeID)
		}
		out[i] = QueueWorkEntry{EntryID: e.EntryID, Seq: unpacked(e.Seq), State: e.State, HeadOID: e.HeadOID,
			Method: e.Method, CloseTask: e.CloseTask, EnqueuedBy: e.enqueuer(), Train: e.Train,
			CandidateID: e.CandidateID, Change: ChainChange{ID: row.id, Number: row.number, Title: row.title,
				SourceRef: row.source, Owner: row.owner}}
		if row.work.ID != "" {
			item := row.work
			out[i].WorkItem = &item
		}
	}
	return out, nil
}

// MaxOwedCloses is the most owed closes one `queue_due` page answers:
// [MaxLandEntries], the most one landing leaves owing — so a home settles a
// whole landing's closes in one turn and one close_settled record (which
// holds as many), and a repository owing more (several landings no home
// reached) settles the rest on the turns after, each close one tracker write
// of its enqueuer's.
const MaxOwedCloses = MaxLandEntries

// owedClosesIn is repository repoID's landed entries still owing their work
// item's close (code_queue.close_op), at most [MaxOwedCloses] in entry id
// order — one seek of code_queue_close_owed_idx, the operation read beside
// each row — each with its enqueuer, its change and the change's work item.
// An entry whose change the repository holds no more is answered with its
// change's id alone and no work item, which its home settles as not closed.
func owedClosesIn(ctx context.Context, tx *sql.Tx, repoID string) ([]QueueWorkEntry, error) {
	owed, ops, err := owedEntriesIn(ctx, tx, repoID)
	if err != nil || len(owed) == 0 {
		return nil, err
	}
	ids := make([]string, len(owed))
	for i, e := range owed {
		ids[i] = e.ChangeID
	}
	rows, err := changeRowsIn(ctx, tx, ids)
	if err != nil {
		return nil, err
	}
	out := make([]QueueWorkEntry, len(owed))
	for i, e := range owed {
		out[i] = QueueWorkEntry{EntryID: e.EntryID, Seq: unpacked(e.Seq), State: e.State, HeadOID: e.HeadOID,
			Method: e.Method, CloseTask: e.CloseTask, EnqueuedBy: e.enqueuer(), CloseOp: ops[i],
			Change: ChainChange{ID: e.ChangeID}}
		row, found := rows[e.ChangeID]
		if !found {
			continue
		}
		out[i].Change = ChainChange{ID: row.id, Number: row.number, Title: row.title, SourceRef: row.source,
			Owner: row.owner}
		if row.work.ID != "" {
			item := row.work
			out[i].WorkItem = &item
		}
	}
	return out, nil
}

// owedEntriesIn is repository repoID's entries owing a close and the
// operation each owes it under, in entry id order, at most [MaxOwedCloses]:
// [owedClosesSQL]'s one seek.
func owedEntriesIn(ctx context.Context, tx *sql.Tx, repoID string) ([]queueEntry, []string, error) {
	rows, err := tx.QueryContext(ctx, owedClosesSQL, repoID, MaxOwedCloses)
	if err != nil {
		return nil, nil, fmt.Errorf("gitlet: read %s's owed closes: %w", repoID, err)
	}
	defer func() { _ = rows.Close() }()
	var (
		owed []queueEntry
		ops  []string
	)
	for rows.Next() {
		var (
			op string
			e  queueEntry
		)
		if e, err = scanEntry(func(dest ...any) error { return rows.Scan(append(dest, &op)...) }); err != nil {
			return nil, nil, fmt.Errorf("gitlet: read %s's owed closes: %w", repoID, err)
		}
		owed, ops = append(owed, e), append(ops, op)
	}
	if err = rows.Err(); err != nil {
		return nil, nil, fmt.Errorf("gitlet: read %s's owed closes: %w", repoID, err)
	}
	return owed, ops, nil
}

// The owed closes' reads, by the index partial on exactly them (replicated
// 0146), its predicate spelled as declared: a repository's, and the active
// repositories holding one, grouped on the index's leading column.
const (
	owedClosesSQL = `SELECT ` + entryColumns + `, close_op FROM code_queue INDEXED BY code_queue_close_owed_idx
		WHERE repo_id = ? AND close_op != '' ORDER BY entry_id LIMIT ?`
	closeOwedReposSQL = `SELECT q.repo_id FROM code_queue AS q INDEXED BY code_queue_close_owed_idx
		WHERE q.close_op != '' AND q.repo_id > ?1
			AND EXISTS (SELECT 1 FROM code_repos AS r WHERE r.id = q.repo_id AND r.status = 'active')
		GROUP BY q.repo_id ORDER BY q.repo_id LIMIT ?2`
)

// descriptionsOf fills in each of entries' change's body and authors, by
// handle: two statements driven from the list of the changes, one over the
// bodies' key and one over the authors', whatever the number of entries.
func descriptionsOf(ctx context.Context, tx *sql.Tx, entries []QueueWorkEntry) error {
	ids := make([]string, len(entries))
	for i, e := range entries {
		ids[i] = e.Change.ID
	}
	list, err := jsonList(ids)
	if err != nil {
		return err
	}
	bodies := make(map[string]string, len(entries))
	authors := make(map[string][]string, len(entries))
	rows, err := tx.QueryContext(ctx, changeBodiesNamedSQL, list)
	if err == nil {
		err = eachRow(rows, func(row scanner) (err error) {
			var id, body string
			if err = row.Scan(&id, &body); err != nil {
				return err
			}
			bodies[id] = body
			return nil
		})
	}
	if err != nil {
		return fmt.Errorf("gitlet: read the bodies of %d queued changes: %w", len(ids), err)
	}
	rows, err = tx.QueryContext(ctx, changeAuthorsNamedSQL, list)
	if err == nil {
		err = eachRow(rows, func(row scanner) (err error) {
			var id, author string
			if err = row.Scan(&id, &author); err != nil {
				return err
			}
			authors[id] = append(authors[id], author)
			return nil
		})
	}
	if err != nil {
		return fmt.Errorf("gitlet: read the authors of %d queued changes: %w", len(ids), err)
	}
	for i := range entries {
		id := entries[i].Change.ID
		names := authors[id]
		// BY HANDLE, as a change's authors are read ([authorsIn]): the key
		// orders one change's, but nothing orders a join's output.
		slices.Sort(names)
		entries[i].Change.Body, entries[i].Change.Authors = bodies[id], names
	}
	return nil
}

// A queue build's descriptions (registered in plan_test.go): its changes'
// bodies and authors, each list driven from json_each and each change sought
// by its table's key.
const (
	changeBodiesNamedSQL = `SELECT b.change_id, b.body FROM json_each(?) AS j
		CROSS JOIN code_change_bodies AS b WHERE b.change_id = CAST(j.value AS TEXT)`
	changeAuthorsNamedSQL = `SELECT a.change_id, a.author FROM json_each(?) AS j
		CROSS JOIN code_change_authors AS a WHERE a.change_id = CAST(j.value AS TEXT)`
)

// trainChecks is trains with the latest attempt of each of required recorded
// on each of their commits for target, read in ONE statement over the checks'
// key, the gate's own ([gateChecksIn]).
func trainChecks(ctx context.Context, tx *sql.Tx, repoID, target string, trains []CandidateRow,
	required []string) ([]QueueWorkCandidate, error) {

	out := make([]QueueWorkCandidate, len(trains))
	var commits []string
	for i, c := range trains {
		out[i] = QueueWorkCandidate{CandidateID: c.CandidateID, Train: c.Train, Ordinal: c.Ordinal,
			BaseOID: c.BaseOID, TipOID: c.TipOID}
		for _, e := range c.Entries {
			out[i].Commits = append(out[i].Commits, QueueWorkCommit{EntryID: e.EntryID, Commit: e.Commit})
			commits = appendOnce(commits, e.Commit)
		}
	}
	if len(required) == 0 || len(commits) == 0 {
		return out, nil
	}
	var keys [][4]any
	facts := make([]GateFacts, len(commits))
	byHead := make(map[[3]string][]int, len(commits))
	names := make([][]string, len(commits))
	for i, commit := range commits {
		byHead[[3]string{repoID, commit, target}] = []int{i}
		names[i] = required
		for _, name := range required {
			keys = append(keys, [4]any{repoID, commit, target, name})
		}
	}
	if err := gateChecksIn(ctx, tx, keys, byHead, names, facts); err != nil {
		return nil, err
	}
	held := make(map[string][]TipCheck, len(commits))
	for i, commit := range commits {
		for _, c := range facts[i].Checks {
			held[commit] = append(held[commit], TipCheck{Name: c.Name, Attempt: c.Attempt, State: c.State,
				Reason: c.Reason})
		}
	}
	for i := range out {
		for j := range out[i].Commits {
			out[i].Commits[j].Checks = held[out[i].Commits[j].Commit]
		}
	}
	return out, nil
}

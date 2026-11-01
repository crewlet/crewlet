package code

import (
	"context"
	"database/sql"
	"fmt"
	"math"

	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/statelog"
)

// A TARGET'S QUEUE WORK, READ FROM THE ROWS (M5-D12, M5-D13): a `queue_due`
// read of ONE repository answers its home one target's [QueueDue] a page —
// everything the home's queue worker (internal/code/home's queue) decides its
// next step from, in ONE snapshot: the target's tip, the gate its queue lands
// under (its policy, depth, batch and required checks), its live entries in
// queue order — as many as one train can take, each with the change its chain
// commit is written from and who enqueued it — and its live train, each
// candidate's commits with the latest attempt of each required check on each,
// for the target — and the repository's drop epoch, which a landing decided
// from the snapshot carries ([QueueProposal.DropEpoch]).
//
// ONE TARGET A PAGE, its cursor the target: a target's entries carry their
// changes' descriptions (a squash's commit message is written from one), so a
// page is at most [MaxCandidateEntries] of them — about two MiB at
// MaxChangeBodyBytes each — however many targets a repository queues on.
//
// A fast-forward's commit is its change's head, so its candidate reads the
// head's checks: a check is a measurement of a commit for its target, whatever
// purpose queued it (ADR-0068), and a head its gate saw pass needs no run
// again.

// QueueDue is one target's queue work as one snapshot holds it
// ([ReadAnswer.DueQueue]).
type QueueDue struct {
	TargetRef string `json:"target_ref"`
	// Tip is the target's tip, what a train is built onto.
	Tip string `json:"tip"`
	// Policy, Depth, Batch and Required are the target's gate as its queue
	// lands under it ([GateRules]): its landing policy, how many candidates
	// it tests at once, how many changes one takes, and the checks every
	// candidate's tip passes before it lands, sorted.
	Policy   LandingPolicy `json:"policy"`
	Depth    int           `json:"depth"`
	Batch    int           `json:"batch"`
	Required []string      `json:"required,omitempty"`
	// Entries are the target's live entries in queue order: the first
	// [MaxCandidateEntries] — every entry one train can take — and More
	// where more wait behind them.
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
}

// QueueWorkEntry is one live entry as the home's queue reads it: its row, who
// enqueued it — whom its merge commit names and its landing lands it as, and
// whom its work item is closed as — and its change, as its chain commit is
// written from.
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

// dueQueue files repository repoID's next target's queue work after cursor —
// "" for its first — into out, with that target as the next page's cursor.
// The target is the first holding a live entry past cursor: the first page's
// one row of queueOfRepoSQL, a later page's of queueOfRepoAfterSQL past every
// seq of the cursor's target.
func dueQueue(ctx context.Context, tx *sql.Tx, repoID, cursor string, out *ReadAnswer) error {
	stmt, args := queueOfRepoSQL, []any{repoID, 1}
	if cursor != "" {
		stmt, args = queueOfRepoAfterSQL, []any{repoID, cursor, cursor, cursor, int64(math.MaxInt64), 1}
	}
	first, err := entriesBy(ctx, tx, stmt, args...)
	if err != nil || len(first) == 0 {
		return err
	}
	target := first[0].TargetRef
	scanned, err := entriesBy(ctx, tx, queueOfTargetSQL, repoID, target, 0, MaxCandidateEntries+1)
	if err != nil {
		return err
	}
	due := &QueueDue{TargetRef: target, More: len(scanned) > MaxCandidateEntries}
	scanned = scanned[:min(len(scanned), MaxCandidateEntries)]
	gate, err := gateOf(ctx, tx, repoID, target)
	if err != nil {
		return err
	}
	due.Policy, due.Depth, due.Batch, due.Required = gate.Landing, gate.QueueDepth, gate.QueueBatch, gate.Checks
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
		due.DropEpoch = repo.dropEpoch
	}
	for _, e := range scanned {
		w, err := workEntryOf(ctx, tx, e)
		if err != nil {
			return err
		}
		due.Entries = append(due.Entries, w)
	}
	trains, err := trainsOf(ctx, tx, repoID, target)
	if err != nil {
		return err
	}
	if due.Train, err = trainChecks(ctx, tx, repoID, target, trains, gate.Checks); err != nil {
		return err
	}
	out.DueQueue, out.Next = due, target
	return nil
}

// workEntryOf is e as the home's queue reads it, its change read by its key:
// its row, its body and its authors.
func workEntryOf(ctx context.Context, tx *sql.Tx, e queueEntry) (QueueWorkEntry, error) {
	w := QueueWorkEntry{EntryID: e.EntryID, Seq: unpacked(e.Seq), State: e.State, HeadOID: e.HeadOID,
		Method: e.Method, CloseTask: e.CloseTask, EnqueuedBy: e.enqueuer(), Train: e.Train,
		CandidateID: e.CandidateID}
	row, found, err := changeRowIn(ctx, tx, e.ChangeID)
	if err != nil || !found {
		// A LIVE ENTRY NAMES A CHANGE ITS REPOSITORY HOLDS: every record
		// that removes a change ends its entry in the same apply.
		if err == nil {
			err = fmt.Errorf("code: entry %s of %s's queue names change %s, which the repository does not "+
				"hold", e.EntryID, e.TargetRef, e.ChangeID)
		}
		return QueueWorkEntry{}, err
	}
	body, err := bodyOf(ctx, tx, row.id)
	if err != nil {
		return QueueWorkEntry{}, err
	}
	authors, err := authorsIn(ctx, tx, row.id)
	if err != nil {
		return QueueWorkEntry{}, err
	}
	names := make([]string, len(authors))
	for i, a := range authors {
		names[i] = a.Author
	}
	w.Change = ChainChange{ID: row.id, Number: row.number, Title: row.title, Body: body, SourceRef: row.source,
		Owner: row.owner, Authors: names}
	if row.work.ID != "" {
		item := row.work
		w.WorkItem = &item
	}
	return w, nil
}

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

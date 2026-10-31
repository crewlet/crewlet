package code

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/store"
)

// THE QUEUE SWEEP: what removes an IDLE repository's queue history — its ended
// entries and its landings below their target's green row — once it outlives
// [QueueHistory] (M5-D34).
//
// A repository's history past the horizon is removed by its own queue's
// records — each candidate, eject and land removes the oldest past its broker
// instant ([sweepQueueHistory]) — so a repository whose queues keep working
// keeps its thirty days by itself. One whose queues stopped, or whose landings
// are a person's pushes and its merges' alone, keeps whatever it held for
// ever, since nothing of its own would remove it; so the code-retention duty
// lists the repositories holding history past the horizon by
// [QueueSweepGrace] — which no repository whose queues keep working holds —
// and publishes a queue_sweep record on each one's subject ([Retention.Pass]'s
// queue step), which every node applies as a queue record's horizon is
// applied.
//
// ON THE REPOSITORY'S SUBJECT, ARBITRATED, as a retired-pack sweep is
// ([Store.RetireSweep]): the rows are the repository's own records', and the
// broker orders the sweep among them, so the decide's count is what the apply
// removes. BY RECORDS, NEVER BY A NODE-LOCAL JOB: the rows are replicated, and
// a delete each node made on its own clock would leave every data node's copy
// different.

// QueueSweep takes ONE step of sweeping repoID's queue history past
// [QueueHistory]: a queue_sweep record removing, oldest first, the ended
// entries and the landings below their green row from before now less the
// history — at most [QueueSweepRows] — and reports what it did. A caller
// steps a repository while the result says More.
//
// now is the DUTY HOLDER'S clock, and the record carries the cutoff it
// judged — never its broker instant's — so the apply removes what this decide
// saw past the history, and a step that read rows past it never appends a
// record that removes none of them.
func (s *Store) QueueSweep(ctx context.Context, repoID string, now time.Time) (SweepResult, error) {
	if now.IsZero() {
		return SweepResult{}, fmt.Errorf("%w: now: a queue sweep judges the history by the duty holder's "+
			"clock, and none was given", ErrInvalid)
	}
	through := now.Add(-QueueHistory)
	cutoff := store.EncodeTime(through)
	subject := RepoSubject(repoID)
	scope := []string{RepoPath(repoID)}
	// THE WALL CLOCK, as every op id of this domain is minted: its instant is
	// judged against the ledger's watermark, never against the decide's
	// clock, which a caller may have set anywhere.
	opID := statelog.NewOpID(time.Now(), "code.queue_sweep")
	at := s.now()
	var swept SweepResult
	result, err := s.publisher.Publish(ctx, statelog.Request{
		Subject: subject.statelog(), Scope: statelog.ScopeSet{Paths: scope},
		OpID: opID, Pattern: statelog.PatternArbitrated,
		Decide: func(tx *sql.Tx, stamp statelog.Stamp) (statelog.Decision, error) {
			swept = SweepResult{}
			// NO ROW, NO RECORD: a purged repository's queues went with its
			// purge, and a record about a repository this node's rows do not
			// hold is one the applier refuses.
			var name string
			switch err := tx.QueryRowContext(ctx, repoNameSQL, repoID).Scan(&name); {
			case errors.Is(err, sql.ErrNoRows):
				return statelog.Decision{}, nil
			case err != nil:
				return statelog.Decision{}, fmt.Errorf("code: read repository %s: %w", repoID, err)
			}
			cut, more, err := queueHistoryIn(ctx, tx, repoID, cutoff, QueueSweepRows)
			if err != nil || cut.rows() == 0 {
				return statelog.Decision{}, err
			}
			swept = SweepResult{Rows: cut.rows(), More: more}
			return recordFor(stamp, subject, OpQueueSweep, scope, opID, Actor{ID: s.nodeID, Kind: ActorSystem},
				QueueSweep{RepoID: repoID, Through: through}, at)
		},
	})
	switch {
	case err != nil:
		return SweepResult{}, fmt.Errorf("code: sweep the queues of repository %s: %w", repoID, err)
	case swept.Rows == 0:
		return SweepResult{}, nil
	case result.Outcome == statelog.OutcomeUnknown:
		return SweepResult{}, unknownOutcome(ctx, "code: whether the sweep of repository %s's queues landed "+
			"is unknown (operation %s); the next pass decides it again", repoID, opID)
	}
	swept.Outcome, swept.Position = result.Outcome, result.Position
	return swept, nil
}

// queueSweepQuery asks for one page of the queue sweep's listing.
type queueSweepQuery struct {
	// cutoff is now less QueueHistory less QueueSweepGrace: a repository is
	// listed only once some of its history lies before it, and its step then
	// sweeps everything past the history.
	cutoff time.Time

	// after is the last repository of the page before; "" for the first.
	after string

	// limit is the most repositories the page names.
	limit int
}

// queueSweepDue lists one page of the repositories holding queue history from
// before q.cutoff — an ended entry, or a landing below its target's green row
// — by id, after q.after, and whether the listing goes on past it.
//
// A WALK OF THE REPOSITORIES BY THEIR KEY, two seeks each: a repository's
// oldest ended entry over code_queue_ended_idx, and each of its targets' oldest
// landing below its green row by the landings' key — the oldest, since a
// target's landings below its green are removed oldest first. No index orders
// either by instant across repositories, and a walk of at most CodeMaxRepos
// rows a pass is a fraction of the pass's budget. A STALE READ of this node's
// rows, as the purge listing is: every step decides again in its own snapshot.
func (r *Reader) queueSweepDue(ctx context.Context, q queueSweepQuery) ([]string, bool, error) {
	if q.limit <= 0 {
		return nil, false, fmt.Errorf("%w: a page of the queue sweep's listing names at least one repository",
			ErrInvalid)
	}
	var page []string
	query := statelog.Freshness{Level: statelog.ReadStale}.Query(
		statelog.ScopeSet{Paths: []string{DomainPath()}}, true)
	_, err := r.log.Read(ctx, query, func(tx *sql.Tx) error {
		page = nil
		rows, err := tx.QueryContext(ctx, queueSweepDueSQL, q.after, store.EncodeTime(q.cutoff), q.limit)
		if err != nil {
			return err
		}
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				return err
			}
			page = append(page, id)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, false, fmt.Errorf("code: list the repositories holding queue history from before %s: %w",
			q.cutoff.UTC().Format(time.RFC3339), err)
	}
	return page, len(page) == q.limit, nil
}

// queueSweepDueSQL is the code-retention pass's listing of the repositories
// holding queue history past the history by the grace: the repositories by
// their key after the cursor, each kept where an ended entry ended before the
// cutoff — one seek of code_queue_ended_idx, its predicate repeated as the
// index declares it — or where a target's oldest landing below its green row
// landed before it, one seek of the landings' key a green row.
const queueSweepDueSQL = `SELECT r.id FROM code_repos AS r WHERE r.id > ?1
	AND (EXISTS (SELECT 1 FROM code_queue AS q INDEXED BY code_queue_ended_idx
			WHERE q.repo_id = r.id AND q.state IN ('landed', 'ejected', 'dequeued') AND q.ended_at < ?2)
		OR EXISTS (SELECT 1 FROM code_green AS g WHERE g.repo_id = r.id
			AND (SELECT l.at FROM code_landings AS l WHERE l.repo_id = g.repo_id AND l.target_ref = g.target_ref
				AND l.position < g.position ORDER BY l.position, l.ordinal LIMIT 1) < ?2))
	ORDER BY r.id LIMIT ?3`

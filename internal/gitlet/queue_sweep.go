package gitlet

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/store"
)

// THE QUEUE SWEEP removes an IDLE repository's queue history (ended entries,
// and landings below their target's green row) once it outlives
// [QueueHistory] (M5-D34). A working queue's own records trim it
// ([sweepQueueHistory]); one whose queues stopped, or whose landings are only
// pushes and merges, would keep it for ever. So the code-retention duty lists
// the repositories past the horizon by [QueueSweepGrace], which no working
// queue reaches, and publishes a queue_sweep on each one's subject
// ([Retention.Pass]'s queue step), applied as a queue record's horizon is.
//
// ARBITRATED ON THE REPOSITORY'S SUBJECT, as [Store.RetireSweep], ordered among
// its own records so the decide's count is what the apply removes; BY RECORDS,
// never a node-local job, whose deletes would leave the replicas differing.

// QueueSweep takes ONE step of sweeping repoID's queue history past
// [QueueHistory]: a queue_sweep record removing, oldest first, the ended
// entries and the landings below their green row from before now less the
// history, at most [QueueSweepRows] with ejections within [QueueSweepBytes].
// A caller steps a repository while the result says More.
//
// now is the DUTY HOLDER'S clock, and the record carries the cutoff it judged,
// never its broker instant's, so the apply removes what this decide saw and a
// step never appends a record that removes none of the rows it read.
func (s *Store) QueueSweep(ctx context.Context, repoID string, now time.Time) (SweepResult, error) {
	if now.IsZero() {
		return SweepResult{}, refusedf("%w: now: a queue sweep judges the history by the duty holder's "+
			"clock, and none was given", ErrInvalid)
	}
	through := now.Add(-QueueHistory)
	cutoff := store.EncodeTime(through)
	subject := RepoSubject(repoID)
	scope := []string{RepoPath(repoID)}
	// THE WALL CLOCK, as every op id of this domain is minted: its instant is
	// judged against the ledger's watermark, not the decide's clock, which a
	// caller may set anywhere.
	opID := statelog.NewOpID(time.Now(), "code_queue_sweep")
	at := s.now()
	var swept SweepResult
	result, err := s.publisher.Publish(ctx, statelog.Request{
		Subject: subject.statelog(), Scope: statelog.ScopeSet{Paths: scope},
		OpID: opID, Pattern: statelog.PatternArbitrated,
		Decide: func(tx *sql.Tx, stamp statelog.Stamp) (statelog.Decision, error) {
			swept = SweepResult{}
			// NO ROW, NO RECORD: a purged repository's queues went with it, and
			// the applier refuses a record about a repository its rows lack.
			var name string
			switch err := tx.QueryRowContext(ctx, repoNameSQL, repoID).Scan(&name); {
			case errors.Is(err, sql.ErrNoRows):
				return statelog.Decision{}, nil
			case err != nil:
				return statelog.Decision{}, fmt.Errorf("gitlet: read repository %s: %w", repoID, err)
			}
			cut, more, err := queueHistoryIn(ctx, tx, repoID, cutoff, QueueSweepRows, QueueSweepBytes)
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
		return SweepResult{}, fmt.Errorf("gitlet: sweep the queues of repository %s: %w", repoID, err)
	case swept.Rows == 0:
		return SweepResult{}, nil
	case result.Outcome == statelog.OutcomeUnknown:
		return SweepResult{}, unknownOutcome(ctx, "gitlet: whether the sweep of repository %s's queues landed "+
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
// before q.cutoff (an ended entry, or a landing below its target's green row),
// by id after q.after, and whether the listing goes on past it.
//
// A WALK OF THE REPOSITORIES BY KEY, two seeks each: the oldest ended entry
// over code_queue_ended_idx, and each target's oldest landing below green by
// the landings' key (removed oldest first). No index orders either by instant
// across repositories, so the cost follows the repositories: 80 ms a pass at
// config.CodeMaxRepos, nothing due (BenchmarkQueueSweepListing, -benchtime 3x,
// two E-cores of a Core Ultra 7 265U); past a tenth of [RetentionPassBudget]
// (6 s), the ref expiry listing's rule, it moves to seeks by instant. A STALE
// READ holding no write lane: every step decides again in its own snapshot.
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
		return nil, false, fmt.Errorf("gitlet: list the repositories holding queue history from before %s: %w",
			q.cutoff.UTC().Format(time.RFC3339), err)
	}
	return page, len(page) == q.limit, nil
}

// queueSweepDueSQL lists, by key after the cursor, the repositories with an
// entry ended before the cutoff (one seek of code_queue_ended_idx, its
// predicate repeated as the index declares it) or a target whose oldest
// landing below its green row landed before it (one seek of the landings' key).
const queueSweepDueSQL = `SELECT r.id FROM code_repos AS r WHERE r.id > ?1
	AND (EXISTS (SELECT 1 FROM code_queue AS q INDEXED BY code_queue_ended_idx
			WHERE q.repo_id = r.id AND q.state IN ('landed', 'ejected', 'dequeued') AND q.ended_at < ?2)
		OR EXISTS (SELECT 1 FROM code_green AS g WHERE g.repo_id = r.id
			AND (SELECT l.at FROM code_landings AS l WHERE l.repo_id = g.repo_id AND l.target_ref = g.target_ref
				AND l.position < g.position ORDER BY l.position, l.ordinal LIMIT 1) < ?2))
	ORDER BY r.id LIMIT ?3`

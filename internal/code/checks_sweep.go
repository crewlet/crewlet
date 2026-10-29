package code

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/store"
)

// THE CHECKS SWEEP: what removes an IDLE repository's check rows once they
// outlive [CheckHistory].
//
// A repository's rows past the history are removed by its own endings — each
// check_finished record removes its repository's oldest past its broker
// instant — so a repository whose checks keep ending keeps its history at
// thirty days by itself. One whose checks stopped ending keeps whatever it
// held for ever, since nothing of its own would ever remove it; so the
// code-retention duty lists the repositories holding rows past the history
// and publishes a checks_sweep record on each one's checks subject
// ([Retention.Pass]'s checks step), which every node applies as the horizon an
// ending applies.
//
// BY RECORDS, NEVER BY A NODE-LOCAL JOB, as the retired packs' sweep: the rows
// are replicated, and a delete each node made on its own clock would leave
// every data node's copy different.

// ChecksSweep takes ONE step of sweeping repoID's check rows past
// [CheckHistory]: a checks_sweep record removing, oldest first, the rows that
// ended before now less the history — at most what one record's horizon
// removes ([CheckHorizonRows], [CheckHorizonBytes]) — and reports what it
// did. A caller steps a repository while the result says More.
//
// now is the DUTY HOLDER'S clock, and the record carries the cutoff it
// judged — never its broker instant's — so the apply removes what this decide
// saw past the history, and a step that read rows past it never appends a
// record that removes none of them (a holder whose clock runs ahead by d
// removes rows up to d early, which a thirty-day history does not notice).
//
// ADDITIVE, as every record on the subject: the apply is the horizon's join,
// which commutes with every other record there. A repository with no row
// appends nothing: its checks went with its purge.
func (s *Store) ChecksSweep(ctx context.Context, repoID string, now time.Time) (SweepResult, error) {
	if now.IsZero() {
		return SweepResult{}, fmt.Errorf("%w: now: a checks sweep judges the history by the duty holder's "+
			"clock, and none was given", ErrInvalid)
	}
	through := now.Add(-CheckHistory)
	cutoff := store.EncodeTime(through)
	subject := ChecksSubject(repoID)
	scope := []string{ChecksPath(repoID)}
	// THE WALL CLOCK, as every op id of this domain is minted: its instant is
	// judged against the ledger's watermark, never the decide's clock, which a
	// caller may have set anywhere.
	opID := statelog.NewOpID(time.Now(), "code.checks_sweep")
	at := s.now()
	var swept SweepResult
	result, err := s.publisher.Publish(ctx, statelog.Request{
		Subject: subject.statelog(), Scope: statelog.ScopeSet{Paths: scope},
		OpID: opID, Pattern: statelog.PatternAdditive,
		Decide: func(tx *sql.Tx, stamp statelog.Stamp) (statelog.Decision, error) {
			swept = SweepResult{}
			// NO ROW, NO RECORD: a purged repository's checks went with its
			// purge, and the deletion gate drops a record about one.
			var name string
			switch err := tx.QueryRowContext(ctx, repoNameSQL, repoID).Scan(&name); {
			case errors.Is(err, sql.ErrNoRows):
				return statelog.Decision{}, nil
			case err != nil:
				return statelog.Decision{}, fmt.Errorf("code: read repository %s: %w", repoID, err)
			}
			rows, _, more, err := checksEndedBefore(ctx, tx, repoID, 1, cutoff, CheckHorizonRows,
				CheckHorizonBytes)
			if err != nil {
				return statelog.Decision{}, fmt.Errorf("code: read %s's checks past their history: %w", repoID, err)
			}
			if rows == 0 {
				return statelog.Decision{}, nil
			}
			swept = SweepResult{Rows: rows, More: more}
			return recordFor(stamp, subject, OpChecksSweep, scope, opID, Actor{ID: s.nodeID, Kind: ActorSystem},
				ChecksSweep{RepoID: repoID, Through: through}, at)
		},
	})
	switch {
	case err != nil:
		return SweepResult{}, fmt.Errorf("code: sweep the checks of repository %s: %w", repoID, err)
	case swept.Rows == 0:
		return SweepResult{}, nil
	case result.Outcome == statelog.OutcomeUnknown:
		return SweepResult{}, unknownOutcome(ctx, "code: whether the sweep of repository %s's checks landed "+
			"is unknown (operation %s); the next pass decides it again", repoID, opID)
	}
	swept.Outcome, swept.Position = result.Outcome, result.Position
	return swept, nil
}

// checksCursor is where a page of the checks listing ended: its last entry's
// finished_at, as stored, and its repository.
type checksCursor struct {
	at   int64
	repo string
}

// checksQuery asks for one page of the checks listing.
type checksQuery struct {
	// cutoff is now less CheckHistory: an ending is listed only once its
	// finished_at is before it.
	cutoff time.Time

	// after is the last entry of the page before; nil for the first.
	after *checksCursor

	// limit is the most entries the page names.
	limit int
}

// checksPage is one page of the checks listing.
type checksPage struct {
	// repos is the repository of every entry the page names, in their order —
	// a repository once for each instant its rows ended at on the page, which
	// the pass steps once.
	repos []string

	// next is where the next page resumes; nil when this page ended the
	// listing.
	next *checksCursor
}

// checksDue lists one page of the repositories holding check rows past the
// history, oldest ending first, resuming after q.after.
//
// ONE ENTRY AN (INSTANT, REPOSITORY), never a row: an ending's rows of one
// repository at one instant are swept together, so the statement groups them
// by the pair, in code_checks_recent_idx's own order, and answers one row for
// each; the keyset on the same pair resumes after the last a page named. A
// STALE READ of this node's rows, as the purge listing is: every step decides
// again in its own snapshot.
func (r *Reader) checksDue(ctx context.Context, q checksQuery) (checksPage, error) {
	if q.limit <= 0 {
		return checksPage{}, fmt.Errorf("%w: a page of the checks listing names at least one entry", ErrInvalid)
	}
	after := checksCursor{at: math.MinInt64}
	if q.after != nil {
		after = *q.after
	}
	var page checksPage
	query := statelog.Freshness{Level: statelog.ReadStale}.Query(
		statelog.ScopeSet{Paths: []string{DomainPath()}}, true)
	_, err := r.log.Read(ctx, query, func(tx *sql.Tx) error {
		page = checksPage{}
		rows, err := tx.QueryContext(ctx, checksDueSQL, store.EncodeTime(q.cutoff), after.at, after.at,
			after.repo, q.limit)
		if err != nil {
			return err
		}
		defer func() { _ = rows.Close() }()
		var last checksCursor
		for rows.Next() {
			if err := rows.Scan(&last.repo, &last.at); err != nil {
				return err
			}
			page.repos = append(page.repos, last.repo)
		}
		if err := rows.Err(); err != nil {
			return err
		}
		if len(page.repos) == q.limit {
			page.next = &last
		}
		return nil
	})
	if err != nil {
		return checksPage{}, fmt.Errorf("code: list the repositories holding checks past %v: %w", CheckHistory, err)
	}
	return page, nil
}

// checksDueSQL is the code-retention pass's listing of the repositories
// holding checks past the history, one row an (instant, repository): grouped
// by code_checks_recent_idx's own two leading columns, so the plan reads the
// groups in its order and sorts nothing, the index's predicate repeated as
// written so the planner may use it at all, and the >= beside the OR what
// keeps the index's range seek (planner.go).
const checksDueSQL = `SELECT repo_id, finished_at FROM code_checks INDEXED BY code_checks_recent_idx
	WHERE finished_at > 0 AND finished_at < ? AND finished_at >= ? AND (finished_at > ? OR repo_id > ?)
	GROUP BY finished_at, repo_id
	ORDER BY finished_at, repo_id LIMIT ?`

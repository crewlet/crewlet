package gitlet

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

// THE CHECKS SWEEP removes an IDLE repository's check rows past
// [CheckHistory]. Each check_finished record removes its own repository's
// rows past the history, so only a repository whose checks stopped ending
// keeps them for ever. The code-retention duty lists the repositories holding
// a row past the history by [CheckSweepGrace], which no repository whose
// checks keep ending holds, and publishes a checks_sweep on each one's checks
// subject ([Retention.Pass]'s checks step), applied as an ending's horizon.
// BY RECORDS, NEVER A NODE-LOCAL JOB: the rows are replicated, and a delete on
// each node's own clock would leave the data nodes' copies different.

// ChecksSweep takes ONE step of sweeping repoID's check rows past
// [CheckHistory]: a checks_sweep record removing, oldest first, the rows that
// ended before now less the history, at most one horizon's worth
// ([CheckHorizonRows], [CheckHorizonBytes]). A caller steps a repository while
// the result says More.
//
// now is the DUTY HOLDER'S clock, and the record carries the cutoff it judged,
// never its broker instant's, so a step that saw rows past it never appends
// one removing none (a clock ahead by d removes rows up to d early, which a
// thirty-day history does not notice). ADDITIVE: the apply is the horizon's
// join, commuting with every record on the subject. A repository with no row
// appends nothing: its checks went with its purge.
func (s *Store) ChecksSweep(ctx context.Context, repoID string, now time.Time) (SweepResult, error) {
	if now.IsZero() {
		return SweepResult{}, refusedf("%w: now: a checks sweep judges the history by the duty holder's "+
			"clock, and none was given", ErrInvalid)
	}
	through := now.Add(-CheckHistory)
	cutoff := store.EncodeTime(through)
	subject := ChecksSubject(repoID)
	scope := []string{ChecksPath(repoID)}
	// THE WALL CLOCK, never now: an op id's instant is judged against the
	// ledger's watermark, and a caller may set now anywhere.
	opID := statelog.NewOpID(time.Now(), "code_checks_sweep")
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
				return statelog.Decision{}, fmt.Errorf("gitlet: read repository %s: %w", repoID, err)
			}
			rows, more, err := checksEndedBefore(ctx, tx, repoID, 1, cutoff, CheckHorizonRows,
				CheckHorizonBytes)
			if err != nil {
				return statelog.Decision{}, fmt.Errorf("gitlet: read %s's checks past their history: %w", repoID, err)
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
		return SweepResult{}, fmt.Errorf("gitlet: sweep the checks of repository %s: %w", repoID, err)
	case swept.Rows == 0:
		return SweepResult{}, nil
	case result.Outcome == statelog.OutcomeUnknown:
		return SweepResult{}, unknownOutcome(ctx, "gitlet: whether the sweep of repository %s's checks landed "+
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
	// cutoff is now less CheckHistory less CheckSweepGrace: an ending is
	// listed once it finished before it, and its repository's step then sweeps
	// every row past the history.
	cutoff time.Time

	// after is the last entry of the page before; nil for the first.
	after *checksCursor

	// limit is the most entries the page names.
	limit int
}

// checksPage is one page of the checks listing.
type checksPage struct {
	// repos is each entry's repository, in order: a repository once for each
	// instant its rows ended at on the page, which the pass steps once.
	repos []string

	// next is where the next page resumes; nil when this page ended the
	// listing.
	next *checksCursor
}

// checksDue lists one page of the repositories holding check rows that ended
// before q.cutoff, oldest ending first, resuming after q.after.
//
// ONE ENTRY AN (INSTANT, REPOSITORY), never a row: one repository's rows
// ending at one instant are swept together, so checksDueSQL groups by the pair
// and the keyset resumes on it. A STALE READ, as the purge listing's: every
// step decides again in its own snapshot.
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
		return checksPage{}, fmt.Errorf("gitlet: list the repositories holding checks that ended before %s: %w",
			q.cutoff.UTC().Format(time.RFC3339), err)
	}
	return page, nil
}

// checksDueSQL is checksDue's listing, grouped by code_checks_recent_idx's two
// leading columns so the plan reads the groups in index order and sorts
// nothing. The index's predicate is repeated as written so the planner may
// use it at all, and the >= beside the OR keeps its range seek (planner.go).
const checksDueSQL = `SELECT repo_id, finished_at FROM code_checks INDEXED BY code_checks_recent_idx
	WHERE finished_at > 0 AND finished_at < ? AND finished_at >= ? AND (finished_at > ? OR repo_id > ?)
	GROUP BY finished_at, repo_id
	ORDER BY finished_at, repo_id LIMIT ?`

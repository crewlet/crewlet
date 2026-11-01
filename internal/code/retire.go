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

// RETIRED PACKS, AND THE SWEEP THAT REMOVES THEM.
//
// A pack a record retires leaves code_packs for code_packs_retired, a RETIRED
// reference ([RetiredPackReferences], ADR-0074): its row keeps its object from
// the collector for [RetireGrace] and does nothing else, so a reader that
// began on the pack before the retirement — a cache that has not synced past
// it, a clone of the largest bundle a row can name — finishes its read.
// The grace is the row's lifetime, and ending it is this domain's: a
// retire_sweep record on the repository's subject, which the code-retention
// duty publishes ([Retention.Pass]) and every node applies, removes the rows
// the duty's clock puts past the grace, and the collector's next pass deletes
// their objects as it deletes anything no row names.
//
// BY RECORDS, NEVER BY A NODE-LOCAL JOB: code_packs_retired is replicated, so
// a delete each node made on its own clock would leave every data node's copy
// different, and a backup or a snapshot of one would then disagree with
// another's. The duty decides; the apply removes exactly what the record
// names, reading no clock (apply.go's first rule).

// SweepResult is what one [Store.RetireSweep] did.
type SweepResult struct {
	// Rows is how many retired rows the step's record removes — its decide's
	// own count of the rows at or below its Through. Zero when the step
	// appended nothing: no row of the repository was past RetireGrace.
	Rows int

	// More reports rows past the grace left above what this record removes,
	// because one record removes at most [RetireSweepRows]: the caller
	// steps the repository again.
	More bool

	// Outcome is the outcome of the record the step appended: applied, or
	// pending — durable at Position and not yet applied on THIS node, whose
	// rows then still hold what it removes. Never unknown, which is the
	// step's error. Empty when the step appended nothing.
	Outcome statelog.Outcome

	// Position is where that record landed; zero when the step appended
	// nothing.
	Position statelog.Position
}

// Held reports that this node's rows already hold what the step did: its
// record applied here, or it appended none.
func (r SweepResult) Held() bool { return r.Outcome != statelog.OutcomePending }

// RetireSweep takes ONE step of sweeping repoID's retired pack rows: a
// retire_sweep record removing, oldest first, every row whose retired_at is at
// or before now less [RetireGrace] — at most [RetireSweepRows] of them, and
// always every row of a retiring record or none — and reports what it did. A
// caller steps a repository while the result says More.
//
// now is the DUTY HOLDER'S clock, and the grace is judged by it against an
// instant the broker stamped: a holder whose clock runs ahead by d sweeps up
// to d early, which [RetireGrace] is sized to outlast. The record carries the
// position it cut at rather than the instant, so the apply reads no clock.
//
// A repository with no row is not an error and appends nothing: its rows went
// with its purge.
func (s *Store) RetireSweep(ctx context.Context, repoID string, now time.Time) (SweepResult, error) {
	if now.IsZero() {
		return SweepResult{}, fmt.Errorf("%w: now: a sweep judges the grace by the "+
			"duty holder's clock, and none was given", ErrInvalid)
	}
	cutoff := store.EncodeTime(now.Add(-RetireGrace))
	subject := RepoSubject(repoID)
	scope := []string{RepoPath(repoID)}
	// THE WALL CLOCK, as every op id of this domain is minted: its instant is
	// judged against the ledger's watermark, never against the decide's
	// clock, which a caller may have set anywhere.
	opID := statelog.NewOpID(time.Now(), "code.retire_sweep")
	at := s.now()
	var swept SweepResult
	result, err := s.publisher.Publish(ctx, statelog.Request{
		Subject: subject.statelog(), Scope: statelog.ScopeSet{Paths: scope},
		OpID: opID, Pattern: statelog.PatternArbitrated,
		Decide: func(tx *sql.Tx, stamp statelog.Stamp) (statelog.Decision, error) {
			swept = SweepResult{}
			// NO ROW, NO RECORD: a purged repository's retired rows went
			// with its purge, and a record about a repository this node's
			// rows do not hold is one the applier refuses.
			var name string
			switch err := tx.QueryRowContext(ctx, repoNameSQL, repoID).Scan(&name); {
			case errors.Is(err, sql.ErrNoRows):
				return statelog.Decision{}, nil
			case err != nil:
				return statelog.Decision{}, fmt.Errorf("code: read repository %s: %w", repoID, err)
			}
			through, rows, more, err := sweepThrough(ctx, tx, repoID, cutoff)
			if err != nil || rows == 0 {
				return statelog.Decision{}, err
			}
			swept = SweepResult{Rows: rows, More: more}
			return recordFor(stamp, subject, OpRetireSweep, scope, opID,
				Actor{ID: s.nodeID, Kind: ActorSystem},
				RetireSweep{RepoID: repoID, Through: unpacked(through)}, at)
		},
	})
	switch {
	case err != nil:
		return SweepResult{}, fmt.Errorf("code: sweep the retired packs of repository %s: %w", repoID, err)
	case swept.Rows == 0:
		return SweepResult{}, nil
	case result.Outcome == statelog.OutcomeUnknown:
		return SweepResult{}, unknownOutcome(ctx, "code: whether the sweep of repository %s's "+
			"retired packs landed is unknown (operation %s); the next pass decides it again",
			repoID, opID)
	}
	swept.Outcome, swept.Position = result.Outcome, result.Position
	return swept, nil
}

// sweepThrough is a sweep's decide over one repository's retired rows, walked
// oldest first: the highest retired_position whose rows are all past cutoff
// and fit one record, how many rows lie at or below it, and whether rows past
// cutoff remain above it.
//
// THE WALK STOPS AT THE FIRST ROW INSIDE THE GRACE, and that stop is SAFE,
// never exact. A position's rows are one record's retirements and share its
// broker instant, but positions are only NEARLY monotone in instants: a
// leader change can step the broker's clock back by the skew between members,
// so a later record may carry an earlier instant. Every row at or below the
// answer was read and found past the cutoff, so nothing inside the grace is
// swept; a due row behind a later-stamped one waits for the next pass — at
// most the members' skew past its grace.
//
// ONE ROW PAST THE RECORD'S BOUND IS READ, so a position the bound would cut
// is seen whole or not at all: its rows go together in a later record, and
// the repository's mark only ever names a position whose every retirement is
// removed. A first position alone past the bound is an error, since no record
// that retires packs writes that many rows.
func sweepThrough(ctx context.Context, tx *sql.Tx, repoID string, cutoff int64) (through int64, rows int,
	more bool, err error) {

	walk, err := tx.QueryContext(ctx, retiredOldestSQL, repoID, RetireSweepRows+1)
	if err != nil {
		return 0, 0, false, fmt.Errorf("code: read %s's retired packs: %w", repoID, err)
	}
	defer func() { _ = walk.Close() }()
	var due []int64
	for walk.Next() {
		var position, retiredAt int64
		if err := walk.Scan(&position, &retiredAt); err != nil {
			return 0, 0, false, fmt.Errorf("code: read %s's retired packs: %w", repoID, err)
		}
		if retiredAt > cutoff {
			break
		}
		due = append(due, position)
	}
	if err := walk.Err(); err != nil {
		return 0, 0, false, fmt.Errorf("code: read %s's retired packs: %w", repoID, err)
	}
	if len(due) <= RetireSweepRows {
		if len(due) == 0 {
			return 0, 0, false, nil
		}
		return due[len(due)-1], len(due), false, nil
	}
	// PAST THE BOUND: the record removes the whole positions among the first
	// RetireSweepRows rows, which ends where the row past them begins.
	cut := due[RetireSweepRows]
	whole := RetireSweepRows
	for whole > 0 && due[whole-1] == cut {
		whole--
	}
	if whole == 0 {
		return 0, 0, false, fmt.Errorf("code: repository %s holds more than %d retired pack rows "+
			"at position %s, more than one sweep record removes — no record that retires packs "+
			"writes that many, so these rows are not ones a record of this domain wrote",
			repoID, RetireSweepRows, unpacked(cut))
	}
	return due[whole-1], whole, true, nil
}

// retiredCursor is where a page of the retired listing ended: its last
// entry's retired_at, as stored, and its repository.
type retiredCursor struct {
	at   int64
	repo string
}

// retiredQuery asks for one page of the retired listing.
type retiredQuery struct {
	// cutoff is now less RetireGrace: a retirement is listed only once its
	// retired_at is at or before it.
	cutoff time.Time

	// after is the last entry of the page before; nil for the first.
	after *retiredCursor

	// limit is the most entries — retiring records — the page names.
	limit int
}

// retiredPage is one page of the retired listing.
type retiredPage struct {
	// repos is the repository of every retiring record the page names, in
	// their order — a repository once for each of its records on the page,
	// which the pass steps once.
	repos []string

	// next is where the next page resumes; nil when this page ended the
	// listing.
	next *retiredCursor
}

// retiredDue lists one page of the records whose retired pack rows are past
// the grace, oldest retirement first, resuming after q.after: the repository
// of each.
//
// ONE ENTRY A RETIRING RECORD, never a row: a record's rows share its broker
// instant and its repository, so the statement groups them by the pair, in
// code_packs_retired_due_idx's own order, and answers one row for each. A
// sweep step removes a record's rows whole, so an entry is at most one step
// of the pass's and a page is a page of its work — where a page of ROWS was
// one record's alone once a record retired a page of packs, and every other
// row of it a repository the pass had already stepped. The keyset on the same
// pair resumes after the last record a page named.
//
// A STALE READ of this node's rows, as the purge listing is: every step
// decides again in its own snapshot, so a row a sweep removed since the
// listing is answered there.
func (r *Reader) retiredDue(ctx context.Context, q retiredQuery) (retiredPage, error) {
	if q.limit <= 0 {
		return retiredPage{}, fmt.Errorf("%w: a page of the retired listing names at least one record", ErrInvalid)
	}
	after := retiredCursor{at: math.MinInt64}
	if q.after != nil {
		after = *q.after
	}
	var page retiredPage
	query := statelog.Freshness{Level: statelog.ReadStale}.Query(
		statelog.ScopeSet{Paths: []string{DomainPath()}}, true)
	_, err := r.log.Read(ctx, query, func(tx *sql.Tx) error {
		page = retiredPage{}
		rows, err := tx.QueryContext(ctx, retiredDueSQL, store.EncodeTime(q.cutoff),
			after.at, after.at, after.repo, q.limit)
		if err != nil {
			return err
		}
		defer func() { _ = rows.Close() }()
		var last retiredCursor
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
		return retiredPage{}, fmt.Errorf("code: list the repositories holding packs retired "+
			"past %v: %w", RetireGrace, err)
	}
	return page, nil
}

// The sweep's reads, each registered in plan_test.go so the pinned driver's
// planner is held to the index each is written for. The decide's walk names
// code_packs_retired_repo_idx, whose order — position, then pack and part — is
// the one every node removes rows in (apply_retire.go).
const (
	// retiredOldestSQL is a sweep decide's walk: a repository's retired rows
	// oldest first, their positions and broker instants alone.
	retiredOldestSQL = `SELECT retired_position, retired_at FROM code_packs_retired
		INDEXED BY code_packs_retired_repo_idx WHERE repo_id = ?
		ORDER BY retired_position, pack_id, part LIMIT ?`
	// retiredDueSQL is the code-retention pass's listing, one row a retiring
	// record: grouped by the index's own two columns, so the plan reads the
	// groups in its order and sorts nothing (plan_test.go holds it to that),
	// and the >= beside the OR is what keeps the index's range seek, which a
	// bare OR of the two terms would give up (planner.go).
	retiredDueSQL = `SELECT repo_id, retired_at FROM code_packs_retired
		INDEXED BY code_packs_retired_due_idx
		WHERE retired_at <= ? AND retired_at >= ? AND (retired_at > ? OR repo_id > ?)
		GROUP BY retired_at, repo_id
		ORDER BY retired_at, repo_id LIMIT ?`
)

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

// RETIRED PACKS AND THEIR SWEEP. A retired pack's row in code_packs_retired
// ([RetiredPackReferences], ADR-0081) keeps its object from the collector for
// [RetireGrace], so a reader that began on the pack before the retirement (an
// unsynced cache, a clone of the largest bundle) finishes. A retire_sweep
// record on the repository's subject, published by the code-retention duty
// ([Retention.Pass]) and applied on every node, removes the rows past the
// grace; the collector then deletes their objects as unnamed.
//
// BY RECORDS, NEVER A NODE-LOCAL JOB: the table is replicated, so a delete on
// each node's own clock would leave the data nodes' copies (and snapshots)
// disagreeing. The apply removes exactly what the record names, reading no
// clock (apply.go's first rule).

// SweepResult is what one [Store.RetireSweep] did.
type SweepResult struct {
	// Rows is how many retired rows the step's record removes (those at or
	// below its Through); zero when no row was past RetireGrace and the step
	// appended nothing.
	Rows int

	// More reports due rows left above this record, which removes at most
	// [RetireSweepRows]: the caller steps the repository again.
	More bool

	// Outcome is the appended record's outcome: applied, or pending (durable
	// at Position, not yet applied on THIS node, whose rows still hold what it
	// removes). Never unknown, which is the step's error. Empty when the step
	// appended nothing.
	Outcome statelog.Outcome

	// Position is where that record landed; zero when the step appended
	// nothing.
	Position statelog.Position
}

// Held reports that this node's rows already hold what the step did: its
// record applied here, or it appended none.
func (r SweepResult) Held() bool { return r.Outcome != statelog.OutcomePending }

// RetireSweep takes ONE step of sweeping repoID's retired pack rows: a
// retire_sweep record removing, oldest first, every row retired at or before
// now less [RetireGrace], at most [RetireSweepRows] and a retiring record's
// rows whole or not at all. A caller steps a repository while the result says
// More. A repository with no row (purged) appends nothing and is no error.
//
// now is the DUTY HOLDER'S clock, judged against a broker-stamped instant: a
// holder whose clock runs ahead by d sweeps up to d early, which [RetireGrace]
// is sized to outlast. The record carries the position it cut at, not the
// instant, so the apply reads no clock.
func (s *Store) RetireSweep(ctx context.Context, repoID string, now time.Time) (SweepResult, error) {
	if now.IsZero() {
		return SweepResult{}, fmt.Errorf("%w: now: a sweep judges the grace by the "+
			"duty holder's clock, and none was given", ErrInvalid)
	}
	cutoff := store.EncodeTime(now.Add(-RetireGrace))
	subject := RepoSubject(repoID)
	scope := []string{RepoPath(repoID)}
	// THE WALL CLOCK, as for every op id of this domain: the ledger's
	// watermark judges it, and a caller may set now anywhere.
	opID := statelog.NewOpID(time.Now(), "code_retire_sweep")
	at := s.now()
	var swept SweepResult
	result, err := s.publisher.Publish(ctx, statelog.Request{
		Subject: subject.statelog(), Scope: statelog.ScopeSet{Paths: scope},
		OpID: opID, Pattern: statelog.PatternArbitrated,
		Decide: func(tx *sql.Tx, stamp statelog.Stamp) (statelog.Decision, error) {
			swept = SweepResult{}
			// NO ROW, NO RECORD: a purge took the retired rows too, and the
			// applier refuses a record on a repository its rows lack.
			var name string
			switch err := tx.QueryRowContext(ctx, repoNameSQL, repoID).Scan(&name); {
			case errors.Is(err, sql.ErrNoRows):
				return statelog.Decision{}, nil
			case err != nil:
				return statelog.Decision{}, fmt.Errorf("gitlet: read repository %s: %w", repoID, err)
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
		return SweepResult{}, fmt.Errorf("gitlet: sweep the retired packs of repository %s: %w", repoID, err)
	case swept.Rows == 0:
		return SweepResult{}, nil
	case result.Outcome == statelog.OutcomeUnknown:
		return SweepResult{}, unknownOutcome(ctx, "gitlet: whether the sweep of repository %s's "+
			"retired packs landed is unknown (operation %s); the next pass decides it again",
			repoID, opID)
	}
	swept.Outcome, swept.Position = result.Outcome, result.Position
	return swept, nil
}

// sweepThrough is a sweep's decide over repoID's retired rows, oldest first:
// the highest retired_position whose rows are all past cutoff and fit one
// record, how many rows lie at or below it, and whether due rows remain above.
//
// THE WALK STOPS AT THE FIRST ROW INSIDE THE GRACE: safe, not exact. A leader
// change can step the broker's clock back by the members' skew, so a later
// position may carry an earlier instant; every row swept was read past cutoff,
// and a due row behind a later-stamped one waits at most that skew longer.
//
// ONE ROW PAST THE BOUND IS READ, so a position the bound would cut waits whole
// for a later record: the repository's mark names only a fully removed one.
func sweepThrough(ctx context.Context, tx *sql.Tx, repoID string, cutoff int64) (through int64, rows int,
	more bool, err error) {

	walk, err := tx.QueryContext(ctx, retiredOldestSQL, repoID, RetireSweepRows+1)
	if err != nil {
		return 0, 0, false, fmt.Errorf("gitlet: read %s's retired packs: %w", repoID, err)
	}
	defer func() { _ = walk.Close() }()
	var due []int64
	for walk.Next() {
		var position, retiredAt int64
		if err := walk.Scan(&position, &retiredAt); err != nil {
			return 0, 0, false, fmt.Errorf("gitlet: read %s's retired packs: %w", repoID, err)
		}
		if retiredAt > cutoff {
			break
		}
		due = append(due, position)
	}
	if err := walk.Err(); err != nil {
		return 0, 0, false, fmt.Errorf("gitlet: read %s's retired packs: %w", repoID, err)
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
		return 0, 0, false, fmt.Errorf("gitlet: repository %s holds more than %d retired pack rows "+
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
	// repos is the repository of each retiring record on the page, in order:
	// once per record, each one step of the pass.
	repos []string

	// next is where the next page resumes; nil when this page ended the
	// listing.
	next *retiredCursor
}

// retiredDue lists one page of the records whose retired pack rows are past
// the grace, oldest retirement first, resuming after q.after: the repository
// of each.
//
// ONE ENTRY PER RETIRING RECORD, never per row: a record's rows share its
// broker instant and repository, so the statement groups by that pair in
// code_packs_retired_due_idx's order and the keyset resumes on it. A sweep
// step removes a record's rows whole, so an entry is one step of the pass and
// a page a page of its work, where a page of rows could be one record's alone.
//
// A STALE READ, as the purge listing is: each step decides again in its own
// snapshot, which sees a row swept since the listing.
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
		return retiredPage{}, fmt.Errorf("gitlet: list the repositories holding packs retired "+
			"past %v: %w", RetireGrace, err)
	}
	return page, nil
}

// The sweep's reads, each held to its index in plan_test.go. The decide's walk
// follows code_packs_retired_repo_idx's order (position, pack, part), the one
// every node removes rows in (apply_retire.go).
const (
	// retiredOldestSQL is a sweep decide's walk: a repository's retired rows
	// oldest first, their positions and broker instants alone.
	retiredOldestSQL = `SELECT retired_position, retired_at FROM code_packs_retired
		INDEXED BY code_packs_retired_repo_idx WHERE repo_id = ?
		ORDER BY retired_position, pack_id, part LIMIT ?`
	// retiredDueSQL is the code-retention pass's listing, one row per retiring
	// record: grouped by the index's two columns, so the plan sorts nothing
	// (plan_test.go), and the >= beside the OR keeps the index's range seek,
	// which a bare OR would give up (planner.go).
	retiredDueSQL = `SELECT repo_id, retired_at FROM code_packs_retired
		INDEXED BY code_packs_retired_due_idx
		WHERE retired_at <= ? AND retired_at >= ? AND (retired_at > ? OR repo_id > ?)
		GROUP BY retired_at, repo_id
		ORDER BY retired_at, repo_id LIMIT ?`
)

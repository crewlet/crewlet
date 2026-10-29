package code

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/gitref"
	"github.com/crewlet/crewlet/internal/objstore"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/store"
)

// A REPOSITORY'S CHECKS, READ: what became of each attempt of each check of
// each commit, as its row on the code log holds it — three kinds of read.
//
//   - `checks` ([ReadChecks]) lists attempts, held to the asker's grant: one
//     repository's, newest ended first and every attempt not yet ended after
//     them (code_checks_finished_idx, read backwards); one change's of it, by
//     commit, name and attempt (code_checks_change_idx), or one commit's
//     (code_checks' key); or the WINDOW — no repository named — every
//     repository's attempts that ended at or after a moment, newest ended
//     first (code_checks_recent_idx), each kept only where its repository is
//     one the asker reads, so a page is the rows it READ and may be short, as
//     a listing of changes across repositories is.
//   - `check` ([ReadCheck]) is one attempt whole: the row, the definition it
//     ran under, its tail, the failures its ending named, and the log a
//     failed run kept.
//   - `checks_due` ([ReadChecksDue]) is the home's: the live changes whose head
//     commit no attempt of any check has been queued at (dueCheckWork's doc).
//
// EVERY LISTING IS A KEYSET over its index's own order, its cursor the last
// row's place in it, so a page resumes exactly where the one before ended
// whatever lands meanwhile, and every page is one seek and one walk.

// checkColumns is a check row as a listing reads it: every column but the
// three wide ones only a `check` read carries (def, failures, tail) — url and
// summary, declared last of the rest, are each a line.
const checkColumns = `repo_id, commit_oid, name, attempt, state, purpose, change_id, revision, candidate_id,
	entry_id, target_ref, base_oid, defs_oid, required, run_in, timeout_seconds, reason, runner_node, placement,
	box_handle, borrowed, reporter, queued_at, started_at, finished_at, duration_ms, exit_code,
	failures_omitted, queued_position, finished_position, url, summary`

// scanCheck reads one row of [checkColumns] — followed, where more is given,
// by those columns.
func scanCheck(scan func(...any) error, more ...any) (CheckRow, error) {
	var (
		r                                        CheckRow
		state, purpose, runIn, reason, placement string
		queuedAt, startedAt, finishedAt          int64
		queuedPosition, finishedPosition         int64
		exitCode                                 sql.NullInt64
	)
	dest := []any{&r.RepoID, &r.OID, &r.Name, &r.Attempt, &state, &purpose, &r.ChangeID, &r.Revision,
		&r.CandidateID, &r.EntryID, &r.Target, &r.Base, &r.DefsOID, &r.Required, &runIn, &r.TimeoutSeconds,
		&reason, &r.Runner, &placement, &r.Box, &r.Borrowed, &r.Reporter, &queuedAt, &startedAt,
		&finishedAt, &r.DurationMs, &exitCode, &r.FailuresOmitted, &queuedPosition, &finishedPosition, &r.URL,
		&r.Summary}
	if err := scan(append(dest, more...)...); err != nil {
		return CheckRow{}, err
	}
	r.State, r.Purpose, r.RunIn = CheckState(state), CheckPurpose(purpose), CheckCell(runIn)
	r.Reason, r.Placement = CheckReason(reason), CheckPlacement(placement)
	r.QueuedAt, r.StartedAt, r.FinishedAt = activityAt(queuedAt), activityAt(startedAt), activityAt(finishedAt)
	if exitCode.Valid {
		code := int(exitCode.Int64)
		r.ExitCode = &code
	}
	if queuedPosition > 0 {
		r.Queued = unpacked(queuedPosition)
	}
	if finishedPosition > 0 {
		r.Finished = unpacked(finishedPosition)
	}
	return r, nil
}

// checkRowIn is the attempt at key in tx, as a listing reads it; false where
// the repository holds none.
func checkRowIn(ctx context.Context, tx *sql.Tx, key CheckKey) (CheckRow, bool, error) {
	r, err := scanCheck(tx.QueryRowContext(ctx, checkRowSQL, key.RepoID, key.OID, key.Name, key.Attempt).Scan)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return CheckRow{}, false, nil
	case err != nil:
		return CheckRow{}, false, fmt.Errorf("code: read check %s of %s: %w", key.Name, key.OID, err)
	}
	return r, true, nil
}

// latestCheckIn is the latest attempt of check name of commit oid in tx — the
// one the next attempt follows — and false where the repository holds none.
func latestCheckIn(ctx context.Context, tx *sql.Tx, repoID, oid, name string) (CheckRow, bool, error) {
	r, err := scanCheck(tx.QueryRowContext(ctx, latestCheckSQL, repoID, oid, name).Scan)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return CheckRow{}, false, nil
	case err != nil:
		return CheckRow{}, false, fmt.Errorf("code: read the latest attempt of check %s of %s: %w", name, oid, err)
	}
	return r, true, nil
}

// checksResolver is the scope a read of a repository's checks naming it by
// NAME is about, read in the read's own transaction: its name's claim and,
// where the claim names a repository, its row — which its grant is judged
// from — and its checks.
func checksResolver(name string) statelog.Resolver {
	token := NameToken(name)
	return func(ctx context.Context, tx *sql.Tx) (statelog.ScopeSet, error) {
		id, found, err := claimIn(ctx, tx, name)
		if err != nil || !found {
			return statelog.ScopeSet{Paths: []string{NamePath(token)}}, err
		}
		return statelog.ScopeSet{Paths: []string{NamePath(token), RepoPath(id), ChecksPath(id)}}, nil
	}
}

// checksQuery is the framework read a `checks`, `check` or `checks_due` read
// resolves to. A listing is a SET READ — of the repository it names, of the
// domain for the window — so an attempt a deferred record holds back is
// listed as its row stands; a `check` read is a POINT READ, which a record
// this node deferred on the repository's checks refuses, as a change's detail
// is refused, rather than answer an attempt around it; and the home's listing
// is a set read of the domain, or of the one repository's rows and checks.
func (q ReadQuery) checksQuery(fresh statelog.Freshness) statelog.Query {
	switch q.Kind {
	case ReadCheck:
		return fresh.Resolved(checksResolver(q.Repo), false)
	case ReadChecksDue:
		if q.ChecksDue == nil {
			return fresh.Query(statelog.ScopeSet{Paths: []string{DomainPath()}}, true)
		}
		id := q.ChecksDue.RepoID
		return fresh.Query(statelog.ScopeSet{Paths: []string{RepoPath(id), ChecksPath(id)}}, true)
	}
	if q.Repo == "" {
		return fresh.Query(statelog.ScopeSet{Paths: []string{DomainPath()}}, true)
	}
	return fresh.Resolved(checksResolver(q.Repo), true)
}

// validateChecks is [ReadQuery.validate]'s rules for a `checks` and a `check`
// read, beyond every principal read's: a `checks` read narrows a repository's
// listing to a change or a commit, or bounds the window — never both, and
// never an attempt's name; a `check` read names its repository and its one
// attempt whole, and is neither paged nor bounded.
func (q ReadQuery) validateChecks() error {
	refused := func(field, why string) error { return fmt.Errorf("%w: %s: %s", ErrInvalid, field, why) }
	c := q.Checks
	if q.Kind == ReadCheck {
		switch {
		case c == nil:
			return refused("checks", "a check read names the commit, the check and the attempt it reads")
		case c.Change != 0:
			return refused("checks.change", "a check read names its attempt by its commit, never a change")
		case !c.Since.IsZero():
			return refused("checks.since", "only a window of checks is bounded by a moment")
		case q.Cursor != "" || q.Limit != 0:
			return refused("limit", "one attempt is read whole, never paged")
		case !gitref.ValidOID(c.OID) || c.OID == ZeroOID:
			return refused("checks.oid", fmt.Sprintf("%q is not a commit's whole id", c.OID))
		case !config.ValidCodeCheckName(c.Name):
			return refused("checks.name", fmt.Sprintf("%q is not a check name", c.Name))
		case c.Attempt < 1 || c.Attempt > MaxCheckAttempts:
			return refused("checks.attempt", fmt.Sprintf("%d is not an attempt: 1 to %d (code.MaxCheckAttempts)",
				c.Attempt, MaxCheckAttempts))
		}
		return nil
	}
	if c == nil {
		return nil
	}
	switch {
	case c.Name != "" || c.Attempt != 0:
		return refused("checks.name", "a listing of checks names no attempt; a check read reads one")
	case c.Change < 0:
		return refused("checks.change", fmt.Sprintf("%d is no change's number", c.Change))
	case c.OID != "" && (!gitref.ValidOID(c.OID) || c.OID == ZeroOID):
		return refused("checks.oid", fmt.Sprintf("%q is not a commit's whole id", c.OID))
	case q.Repo == "" && (c.Change != 0 || c.OID != ""):
		return refused("checks", "a change's or a commit's checks are read on the repository a read names")
	case q.Repo != "" && !c.Since.IsZero():
		return refused("checks.since", "only the window of every repository's checks is bounded by a moment; "+
			"a repository's lists its own attempts newest ended first")
	}
	return nil
}

// validateChecksDue is [ReadQuery.validate] for a `checks_due` read: the
// engine's own, so it names NO asker and no seat it reads for, for the
// `compaction_due` read's reason; its one repository, where it names one, by
// id in its canonical spelling; and nothing else a listing names, since a page
// is MaxSnapshotItems rows.
func (q ReadQuery) validateChecksDue() error {
	refused := func(field, why string) error { return fmt.Errorf("%w: %s: %s", ErrInvalid, field, why) }
	switch {
	case q.Asker != (Principal{}):
		return refused("asker", "a checks_due read is the engine's own, which no principal makes")
	case q.CeilingSeat != "":
		return refused("ceiling_seat", "a checks_due read is the engine's own, which no seat makes for another")
	case q.ChecksDue != nil && !canonicalUUID(q.ChecksDue.RepoID):
		return refused("checks_due.repo_id", fmt.Sprintf("%q is not a repository's id in its canonical "+
			"lowercase spelling", q.ChecksDue.RepoID))
	}
	for _, f := range []struct {
		field string
		set   bool
	}{
		{"repo", q.Repo != ""}, {"status", len(q.Status) > 0}, {"prefix", q.Prefix != ""},
		{"introduced", q.Introduced != nil}, {"run_pushes", q.RunPushes != nil}, {"min_packs", q.MinPacks != 0},
		{"limit", q.Limit != 0}, {"brief", q.Brief}, {"push", q.Push != ""}, {"change", q.Change != 0},
		{"state", q.State != ""}, {"owner", q.Owner != ""}, {"work", q.Work != ""},
		{"reviewer", q.Reviewer != ""}, {"needs", q.Needs != ""}, {"part", q.Part != ""},
		{"changes_due", q.ChangesDue != nil}, {"checks", q.Checks != nil},
		{"path_owners", q.PathOwners},
	} {
		if f.set {
			return refused(f.field, "a checks_due read lists the repositories a home owes check work in, or "+
				"one repository's due changes by its id, a page of MaxSnapshotItems at a time, and names "+
				"nothing else")
		}
	}
	return nil
}

// checks serves one page of a `checks` read: of the repository it names —
// one change's attempts, one commit's, or all of them newest ended first — or
// of the window, every repository the asker reads.
func (r *Reader) checks(ctx context.Context, tx *sql.Tx, q ReadQuery, chart Roster, out *ReadAnswer) error {
	limit := min(limitOr(q.Limit, defaultCheckLimit), r.pageRows)
	c := CheckQuery{}
	if q.Checks != nil {
		c = *q.Checks
	}
	if q.Repo == "" {
		return r.checksWindow(ctx, tx, q, c, chart, limit, out)
	}
	repo, err := readableIn(ctx, tx, q.Repo, q.Asker, q.CeilingSeat, chart)
	if err != nil {
		return err
	}
	var (
		rows   *sql.Rows
		cursor func(CheckRow) string
	)
	switch {
	case c.Change != 0:
		// ONE err FOR EVERY BRANCH, never one declared here: a statement's
		// failure below is read after the switch.
		var (
			change changeRow
			found  bool
		)
		if change, found, err = changeByNumberIn(ctx, tx, repo.ref.ID, c.Change); err != nil {
			return err
		}
		if !found {
			return fmt.Errorf("%w: %s", ErrChangeNotFound, ChangeRef(repo.ref.Name, c.Change))
		}
		if c.OID != "" {
			name, attempt, ok := cutNameAttempt(q.Cursor)
			if !ok {
				return fmt.Errorf("%w: %q", ErrBadCursor, q.Cursor)
			}
			rows, err = tx.QueryContext(ctx, checksOfChangeCommitSQL, change.id, c.OID, name, attempt, limit)
			cursor = func(row CheckRow) string { return nameAttemptCursor(row) }
		} else {
			oid, name, attempt, ok := cutCommitCursor(q.Cursor)
			if !ok {
				return fmt.Errorf("%w: %q", ErrBadCursor, q.Cursor)
			}
			rows, err = tx.QueryContext(ctx, checksOfChangeSQL, change.id, oid, name, attempt, limit)
			cursor = func(row CheckRow) string { return row.OID + "/" + nameAttemptCursor(row) }
		}
	case c.OID != "":
		name, attempt, ok := cutNameAttempt(q.Cursor)
		if !ok {
			return fmt.Errorf("%w: %q", ErrBadCursor, q.Cursor)
		}
		rows, err = tx.QueryContext(ctx, checksOfCommitSQL, repo.ref.ID, c.OID, name, attempt, limit)
		cursor = func(row CheckRow) string { return nameAttemptCursor(row) }
	case q.Cursor == "":
		rows, err = tx.QueryContext(ctx, checksOfRepoSQL, repo.ref.ID, limit)
		cursor = endedCursor
	default:
		at, oid, name, attempt, ok := cutEndedCursor(q.Cursor)
		if !ok {
			return fmt.Errorf("%w: %q", ErrBadCursor, q.Cursor)
		}
		rows, err = tx.QueryContext(ctx, checksOfRepoAfterSQL, repo.ref.ID, at, oid, name, attempt, limit)
		cursor = endedCursor
	}
	if err != nil {
		return fmt.Errorf("code: list repository %s's checks: %w", repo.ref.Name, err)
	}
	defer func() { _ = rows.Close() }()
	scanned, err := scanChecks(rows)
	if err != nil {
		return fmt.Errorf("code: list repository %s's checks: %w", repo.ref.Name, err)
	}
	r.pageChecks(scanned, nil, limit, cursor, out)
	return nil
}

// checksWindow serves one page of the window: every repository's attempts
// that ended at or after c.Since, newest ended first, each kept where its
// repository is one the asker — and the seat it reads for — may read.
func (r *Reader) checksWindow(ctx context.Context, tx *sql.Tx, q ReadQuery, c CheckQuery, chart Roster,
	limit int, out *ReadAnswer) error {

	since := int64(1)
	if !c.Since.IsZero() {
		since = max(store.EncodeTime(c.Since), 1)
	}
	var (
		rows *sql.Rows
		err  error
	)
	if q.Cursor == "" {
		rows, err = tx.QueryContext(ctx, checksWindowSQL, since, limit)
	} else {
		at, repo, oid, name, attempt, ok := cutWindowCursor(q.Cursor)
		if !ok {
			return fmt.Errorf("%w: %q", ErrBadCursor, q.Cursor)
		}
		rows, err = tx.QueryContext(ctx, checksWindowAfterSQL, since, at, repo, oid, name, attempt, limit)
	}
	if err != nil {
		return fmt.Errorf("code: list the checks of every repository: %w", err)
	}
	defer func() { _ = rows.Close() }()
	scanned, err := scanChecks(rows)
	if err != nil {
		return fmt.Errorf("code: list the checks of every repository: %w", err)
	}
	ids := make([]string, len(scanned))
	for i, row := range scanned {
		ids[i] = row.RepoID
	}
	listed, err := listedReposAmong(ctx, tx, ids, q.Asker, q.CeilingSeat, chart, "a page of checks")
	if err != nil {
		return err
	}
	r.pageChecks(scanned, func(row CheckRow) bool { _, ok := listed[row.RepoID]; return ok }, limit,
		windowCursor, out)
	return nil
}

// scanChecks reads every row of a listing's statement.
func scanChecks(rows *sql.Rows) ([]CheckRow, error) {
	var out []CheckRow
	for rows.Next() {
		row, err := scanCheck(rows.Scan)
		if err != nil {
			return nil, err
		}
		out = append(out, row)
	}
	return out, rows.Err()
}

// pageChecks files the rows a listing READ into out — those keep keeps, every
// one where it is nil — until the page's byte budget, and names where the
// next page resumes: after the last row it served where the budget ended it,
// and after the last row it READ where it read a whole page, so a page the
// grant kept little of still advances.
func (r *Reader) pageChecks(scanned []CheckRow, keep func(CheckRow) bool, limit int,
	cursor func(CheckRow) string, out *ReadAnswer) {

	pg := page{maxRows: limit, maxB: r.pageBytes}
	for _, row := range scanned {
		if keep != nil && !keep(row) {
			continue
		}
		out.Checks = append(out.Checks, row)
		if pg.add(row.encodedBytes()) {
			out.Next = cursor(row)
			return
		}
	}
	if len(scanned) == limit {
		out.Next = cursor(scanned[len(scanned)-1])
	}
}

// check serves a `check` read: the one attempt it names, whole, held to the
// asker's grant on its repository.
func (r *Reader) check(ctx context.Context, tx *sql.Tx, q ReadQuery, chart Roster, out *ReadAnswer) error {
	repo, err := readableIn(ctx, tx, q.Repo, q.Asker, q.CeilingSeat, chart)
	if err != nil {
		return err
	}
	key := CheckKey{RepoID: repo.ref.ID, OID: q.Checks.OID, Name: q.Checks.Name, Attempt: q.Checks.Attempt}
	var (
		d                   CheckDetail
		def, failures, tail string
	)
	d.CheckRow, err = scanCheck(tx.QueryRowContext(ctx, checkWholeSQL, key.RepoID, key.OID, key.Name,
		key.Attempt).Scan, &def, &failures, &tail)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return fmt.Errorf("%w: %s holds no attempt %d of check %s of %s", ErrCheckNotFound, repo.ref.Name,
			key.Attempt, key.Name, key.OID)
	case err != nil:
		return fmt.Errorf("code: read check %s of %s: %w", key.Name, key.OID, err)
	}
	d.Tail = tail
	if def != "" {
		d.Def = &CheckDef{}
		if err := json.Unmarshal([]byte(def), d.Def); err != nil {
			return fmt.Errorf("code: read check %s of %s's definition: %w", key.Name, key.OID, err)
		}
	}
	if failures != "" {
		if err := json.Unmarshal([]byte(failures), &d.Failures); err != nil {
			return fmt.Errorf("code: read check %s of %s's failures: %w", key.Name, key.OID, err)
		}
	}
	var (
		object, hash string
		size         int64
	)
	switch err := tx.QueryRowContext(ctx, checkLogSQL, key.RepoID, key.OID, key.Name, key.Attempt).Scan(&object,
		&hash, &size); {
	case errors.Is(err, sql.ErrNoRows):
	case err != nil:
		return fmt.Errorf("code: read check %s of %s's log: %w", key.Name, key.OID, err)
	default:
		k, err := objstore.ParseKey(object)
		if err != nil {
			return fmt.Errorf("code: read check %s of %s's log: %w", key.Name, key.OID, err)
		}
		d.Log = &CheckLog{Object: k, Hash: objstore.Hash(hash), Size: size}
	}
	out.Check = &d
	return nil
}

// THE HOME'S DUE CHECK WORK, READ FROM THE ROWS: a `checks_due` read
// ([ReadChecksDue]) is what a repository's home git node polls for, and is
// nudged into reading, to learn which change heads it owes a dispatch of their
// checks: a LIVE change on an ACTIVE repository, its target a live branch,
// whose head commit holds NO attempt of any check — every one it dispatches
// queues at least one attempt there (or records one skipped), and drops out of
// the listing by its own row, so the rows are the work list. Judged BY COMMIT,
// as a check's row is keyed: a change whose head another change's checks were
// queued at shares those attempts, and is no dispatch of its own.
//
// TWO SHAPES, as `changes_due`'s, and over the same index: with no repository
// it pages the repositories holding such a change, by id, walking
// code_changes_open_idx grouped on its leading column; with one it pages that
// repository's such changes by source branch, each with its target's tip and
// the position that moved it there — the tip whose checks file the home reads
// the definitions from. Every change is checked against code_checks' key, one
// seek a change.
//
// READ STALE BY DEFAULT, as every duty listing is: a head listed a moment late
// is dispatched at the next pass, and one listed after its dispatch landed is
// answered from the attempt's row by the writer, which queues nothing twice.
func (r *Reader) dueCheckWork(ctx context.Context, tx *sql.Tx, q ReadQuery, out *ReadAnswer) error {
	if q.ChecksDue == nil {
		if q.Cursor != "" && !canonicalUUID(q.Cursor) {
			return fmt.Errorf("%w: %q", ErrBadCursor, q.Cursor)
		}
		rows, err := tx.QueryContext(ctx, dueCheckReposSQL, q.Cursor, r.pageRows)
		if err != nil {
			return fmt.Errorf("code: list the repositories check work is due in: %w", err)
		}
		defer func() { _ = rows.Close() }()
		pg := page{maxRows: r.pageRows, maxB: r.pageBytes}
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				return fmt.Errorf("code: list the repositories check work is due in: %w", err)
			}
			out.DueRepos = append(out.DueRepos, id)
			if pg.add(stringBytes(id) + 1) {
				out.Next = id
				break
			}
		}
		return rows.Err()
	}
	repoID := q.ChecksDue.RepoID
	if q.Cursor != "" && !ValidRef(q.Cursor) {
		return fmt.Errorf("%w: %q", ErrBadCursor, q.Cursor)
	}
	var status string
	switch err := tx.QueryRowContext(ctx, dueRepoSQL, repoID).Scan(&status); {
	case errors.Is(err, sql.ErrNoRows):
		return nil
	case err != nil:
		return fmt.Errorf("code: read repository %s's state: %w", repoID, err)
	case RepoStatus(status) != RepoStatusActive:
		return nil
	}
	rows, err := tx.QueryContext(ctx, dueCheckChangesSQL, repoID, q.Cursor, r.pageRows)
	if err != nil {
		return fmt.Errorf("code: list repository %s's heads due their checks: %w", repoID, err)
	}
	defer func() { _ = rows.Close() }()
	pg := page{maxRows: r.pageRows, maxB: r.pageBytes}
	for rows.Next() {
		var (
			c  ChangeDue
			at int64
		)
		if err := rows.Scan(&c.ID, &c.Number, &c.Revision, &c.Head, &c.SourceRef, &c.TargetRef, &c.Tip,
			&at); err != nil {
			return fmt.Errorf("code: list repository %s's heads due their checks: %w", repoID, err)
		}
		c.TipPosition = unpacked(at)
		out.DueChanges = append(out.DueChanges, c)
		if pg.add(c.encodedBytes()) {
			out.Next = c.SourceRef
			break
		}
	}
	return rows.Err()
}

// BacklogScan is the most queued attempts one reading of the checks' backlog
// walks: 1 024, oldest queued first — the oldest are the ones past their
// timeouts first, and a backlog of more is one the reading already counts in
// the thousand it read; a reading every AlarmInterval on every data node is a
// thousand point reads of rows at most, each its timeout beside the index.
const BacklogScan = 1024

// ChecksBacklog is what a node's alarm reads of the queued checks
// ([Reader.ChecksBacklog]): how many of the oldest queued attempts have waited
// longer than their own timeout, the longest wait among them, and that
// attempt, named for an operator.
type ChecksBacklog struct {
	Behind int
	Oldest time.Duration
	Check  string
}

// ChecksBacklog reads the queued attempts this node's rows hold, oldest
// queued first and at most [BacklogScan] of them, and counts each that has
// waited longer than its OWN timeout — the timeout_seconds of the definition
// it was queued under — by now: the threshold of the code_checks_backlog
// alarm is every check's own (ADR-0015), never one of the alarm's. An attempt
// queued with no definition — one a person's CI reports, which no runner of
// the engine claims — is never behind. A STALE read of this node's rows, as
// every alarm's reading is: the alarm says what this node holds.
func (r *Reader) ChecksBacklog(ctx context.Context, now time.Time) (ChecksBacklog, error) {
	query := statelog.Freshness{Level: statelog.ReadStale}.Query(
		statelog.ScopeSet{Paths: []string{DomainPath()}}, true)
	var out ChecksBacklog
	_, err := r.log.Read(ctx, query, func(tx *sql.Tx) error {
		out = ChecksBacklog{}
		rows, err := tx.QueryContext(ctx, checksBacklogSQL, math.MinInt64, BacklogScan)
		if err != nil {
			return err
		}
		defer func() { _ = rows.Close() }()
		var oldest CheckKey
		for rows.Next() {
			var (
				key               CheckKey
				queuedAt, timeout int64
			)
			if err := rows.Scan(&key.RepoID, &key.OID, &key.Name, &key.Attempt, &queuedAt, &timeout); err != nil {
				return err
			}
			waited := now.Sub(store.DecodeTime(queuedAt))
			if timeout <= 0 || waited <= time.Duration(timeout)*time.Second {
				continue
			}
			out.Behind++
			if waited > out.Oldest {
				out.Oldest, oldest = waited, key
			}
		}
		if err := rows.Err(); err != nil || out.Behind == 0 {
			return err
		}
		name := oldest.RepoID
		switch err := tx.QueryRowContext(ctx, repoNameSQL, oldest.RepoID).Scan(&name); {
		case errors.Is(err, sql.ErrNoRows):
		case err != nil:
			return err
		}
		commit := oldest.OID
		if len(commit) > 12 {
			commit = commit[:12]
		}
		out.Check = fmt.Sprintf("%s@%s: %s (attempt %d)", name, commit, oldest.Name, oldest.Attempt)
		return nil
	})
	if err != nil {
		return ChecksBacklog{}, fmt.Errorf("code: read the queued checks' backlog: %w", err)
	}
	return out, nil
}

// THE CURSORS, each a row's place in its listing's order, "/"-separated — no
// part of one holds a "/": a commit is hex, a check's name is
// [config.ValidCodeCheckName]'s alphabet, a repository's id a UUID and an
// instant digits.

// nameAttemptCursor is a row's place among one commit's attempts.
func nameAttemptCursor(row CheckRow) string { return row.Name + "/" + strconv.Itoa(row.Attempt) }

// cutNameAttempt reads a [nameAttemptCursor]; "" is a first page's.
func cutNameAttempt(s string) (string, int, bool) {
	if s == "" {
		return "", 0, true
	}
	name, rest, ok := strings.Cut(s, "/")
	attempt, err := strconv.Atoi(rest)
	if !ok || err != nil || !config.ValidCodeCheckName(name) || attempt < 1 {
		return "", 0, false
	}
	return name, attempt, true
}

// cutCommitCursor reads a change's listing's cursor, "<commit>/<name>/<attempt>";
// "" is a first page's.
func cutCommitCursor(s string) (string, string, int, bool) {
	if s == "" {
		return "", "", 0, true
	}
	oid, rest, ok := strings.Cut(s, "/")
	if !ok || !gitref.ValidOID(oid) {
		return "", "", 0, false
	}
	name, attempt, ok := cutNameAttempt(rest)
	return oid, name, attempt, ok && rest != ""
}

// endedCursor is a row's place in a repository's listing, newest ended first:
// "<finished_at>/<commit>/<name>/<attempt>", an attempt not yet ended at 0.
func endedCursor(row CheckRow) string {
	at := int64(0)
	if !row.FinishedAt.IsZero() {
		at = store.EncodeTime(row.FinishedAt)
	}
	return strconv.FormatInt(at, 10) + "/" + row.OID + "/" + nameAttemptCursor(row)
}

// cutEndedCursor reads an [endedCursor].
func cutEndedCursor(s string) (int64, string, string, int, bool) {
	head, rest, ok := strings.Cut(s, "/")
	at, err := strconv.ParseInt(head, 10, 64)
	if !ok || err != nil || at < 0 {
		return 0, "", "", 0, false
	}
	oid, name, attempt, ok := cutCommitCursor(rest)
	return at, oid, name, attempt, ok && rest != ""
}

// windowCursor is a row's place in the window, newest ended first:
// "<finished_at>/<repo>/<commit>/<name>/<attempt>".
func windowCursor(row CheckRow) string {
	return strconv.FormatInt(store.EncodeTime(row.FinishedAt), 10) + "/" + row.RepoID + "/" + row.OID + "/" +
		nameAttemptCursor(row)
}

// cutWindowCursor reads a [windowCursor].
func cutWindowCursor(s string) (int64, string, string, string, int, bool) {
	head, rest, ok := strings.Cut(s, "/")
	at, err := strconv.ParseInt(head, 10, 64)
	if !ok || err != nil || at < 1 || at == math.MaxInt64 {
		return 0, "", "", "", 0, false
	}
	repo, rest, ok := strings.Cut(rest, "/")
	if !ok || !canonicalUUID(repo) {
		return 0, "", "", "", 0, false
	}
	oid, name, attempt, ok := cutCommitCursor(rest)
	return at, repo, oid, name, attempt, ok && rest != ""
}

// The reads of a repository's checks, each registered in plan_test.go, so the
// pinned driver's planner is held to the key each is written for: one attempt
// by code_checks' key, whole or as a listing reads it; the latest attempt of
// one check of one commit, the key read backwards from its (repository,
// commit, name) prefix; one commit's attempts after a (name, attempt) on the
// key; one change's after a (commit, name, attempt), or one commit of its
// after a (name, attempt), on code_checks_change_idx; a repository's newest
// ended first, from the top or after a place, on code_checks_finished_idx
// read backwards; and the window's, on code_checks_recent_idx read backwards
// above its moment. Each keyset's >= beside its OR is what keeps the index's
// range seek (planner.go).
const (
	checkRowSQL = `SELECT ` + checkColumns + ` FROM code_checks INDEXED BY ` + checksKey + `
		WHERE repo_id = ? AND commit_oid = ? AND name = ? AND attempt = ?`
	checkWholeSQL = `SELECT ` + checkColumns + `, def, failures, tail FROM code_checks INDEXED BY ` + checksKey + `
		WHERE repo_id = ? AND commit_oid = ? AND name = ? AND attempt = ?`
	checkLogSQL = `SELECT object, hash, size FROM code_check_logs
		WHERE repo_id = ? AND commit_oid = ? AND name = ? AND attempt = ?`
	latestCheckSQL = `SELECT ` + checkColumns + ` FROM code_checks INDEXED BY ` + checksKey + `
		WHERE repo_id = ? AND commit_oid = ? AND name = ? ORDER BY attempt DESC LIMIT 1`
	checksOfCommitSQL = `SELECT ` + checkColumns + ` FROM code_checks INDEXED BY ` + checksKey + `
		WHERE repo_id = ?1 AND commit_oid = ?2 AND name >= ?3 AND (name > ?3 OR attempt > ?4)
		ORDER BY name, attempt LIMIT ?5`
	checksOfChangeSQL = `SELECT ` + checkColumns + ` FROM code_checks INDEXED BY code_checks_change_idx
		WHERE change_id = ?1 AND commit_oid >= ?2
			AND (commit_oid > ?2 OR name > ?3 OR (name = ?3 AND attempt > ?4))
		ORDER BY commit_oid, name, attempt LIMIT ?5`
	checksOfChangeCommitSQL = `SELECT ` + checkColumns + ` FROM code_checks INDEXED BY code_checks_change_idx
		WHERE change_id = ?1 AND commit_oid = ?2 AND name >= ?3 AND (name > ?3 OR attempt > ?4)
		ORDER BY name, attempt LIMIT ?5`
	checksOfRepoSQL = `SELECT ` + checkColumns + ` FROM code_checks INDEXED BY ` + checksEndedKey + `
		WHERE repo_id = ?1
		ORDER BY finished_at DESC, commit_oid DESC, name DESC, attempt DESC LIMIT ?2`
	checksOfRepoAfterSQL = `SELECT ` + checkColumns + ` FROM code_checks INDEXED BY ` + checksEndedKey + `
		WHERE repo_id = ?1 AND finished_at <= ?2
			AND (finished_at < ?2 OR commit_oid < ?3 OR (commit_oid = ?3 AND (name < ?4
				OR (name = ?4 AND attempt < ?5))))
		ORDER BY finished_at DESC, commit_oid DESC, name DESC, attempt DESC LIMIT ?6`
	checksWindowSQL = `SELECT ` + checkColumns + ` FROM code_checks INDEXED BY code_checks_recent_idx
		WHERE finished_at > 0 AND finished_at >= ?1
		ORDER BY finished_at DESC, repo_id DESC, commit_oid DESC, name DESC, attempt DESC LIMIT ?2`
	checksWindowAfterSQL = `SELECT ` + checkColumns + ` FROM code_checks INDEXED BY code_checks_recent_idx
		WHERE finished_at > 0 AND finished_at >= ?1 AND finished_at <= ?2
			AND (finished_at < ?2 OR repo_id < ?3 OR (repo_id = ?3 AND (commit_oid < ?4
				OR (commit_oid = ?4 AND (name < ?5 OR (name = ?5 AND attempt < ?6))))))
		ORDER BY finished_at DESC, repo_id DESC, commit_oid DESC, name DESC, attempt DESC LIMIT ?7`
	// THE BACKLOG'S WALK: the queued attempts oldest first, the queued state
	// repeated as code_checks_queued_idx is partial on it and the >= what
	// keeps its range seek, each with its timeout read beside the index.
	checksBacklogSQL = `SELECT repo_id, commit_oid, name, attempt, queued_at, timeout_seconds
		FROM code_checks INDEXED BY code_checks_queued_idx
		WHERE state = 'queued' AND queued_at >= ?1
		ORDER BY queued_at, repo_id, commit_oid, name, attempt LIMIT ?2`
	// THE HOME'S LISTING, changes_due's two statements with the facts' test
	// replaced by the checks': a head with no attempt at its commit, one seek
	// of code_checks' key.
	dueCheckReposSQL = `SELECT c.repo_id FROM code_changes AS c INDEXED BY code_changes_open_idx
		WHERE c.repo_id > ?1 AND ` + liveChanges + `
			AND NOT EXISTS (SELECT 1 FROM code_checks AS k INDEXED BY ` + checksKey + `
				WHERE k.repo_id = c.repo_id AND k.commit_oid = c.head_oid)
			AND EXISTS (SELECT 1 FROM code_refs AS t INDEXED BY ` + refsKey + `
				WHERE t.repo_id = c.repo_id AND t.ref = c.target_ref AND t.target != '')
			AND EXISTS (SELECT 1 FROM code_repos AS r WHERE r.id = c.repo_id AND r.status = 'active')
		GROUP BY c.repo_id ORDER BY c.repo_id LIMIT ?2`
	dueCheckChangesSQL = `SELECT c.id, c.number, c.revision, c.head_oid, c.source_ref, c.target_ref, t.target,
			t.position
		FROM code_changes AS c INDEXED BY code_changes_open_idx
		CROSS JOIN code_refs AS t INDEXED BY ` + refsKey + `
		WHERE c.repo_id = ?1 AND c.source_ref > ?2 AND ` + liveChanges + `
			AND t.repo_id = c.repo_id AND t.ref = c.target_ref AND t.target != ''
			AND NOT EXISTS (SELECT 1 FROM code_checks AS k INDEXED BY ` + checksKey + `
				WHERE k.repo_id = c.repo_id AND k.commit_oid = c.head_oid)
		ORDER BY c.source_ref LIMIT ?3`
)

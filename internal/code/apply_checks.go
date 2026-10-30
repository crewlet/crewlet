package code

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/crewlet/crewlet/internal/store"
)

// THE CHECKS SUBJECT'S APPLIES: every record on a repository's checks subject
// ([KindChecks]), each PURE IN THE RECORD, ITS POSITION AND ITS INSTANT like
// every apply here — and each a JOIN ([joinCheck]), so the records commute, as
// PatternAdditive requires of them (checks.go states the join).
//
// THE SUBJECT GUARD: a record on one repository's checks subject may write
// only that repository's checks — its payload names the subject's repository —
// or it is one no correct writer forms, an error rather than a skip: a record
// about one repository's checks that named another would escape that
// repository's deletion gate, which [Applier.Gated] reads off the subject.
//
// IT READS NO ROW BUT ITS OWN. Not the repository's: a check's row says
// nothing that depends on the repository's, and the repository a purge
// removed is held off by its marker at the gate, before any apply. So its
// scope names its checks alone (ChecksPath), and a record here waits behind
// none of the repository's own records but its purge's steps, which delete
// its rows and name the same path.

// applyChecks is every record on a repository's checks subject.
func (a *Applier) applyChecks(ctx context.Context, tx *sql.Tx, at applyContext) (int, error) {
	payload, err := DecodeMutation(at.record)
	if err != nil {
		return 0, err
	}
	var repoID string
	switch p := payload.(type) {
	case CheckQueued:
		repoID = p.RepoID
	case CheckStarted:
		repoID = p.RepoID
	case CheckFinished:
		repoID = p.RepoID
	case ChecksSweep:
		repoID = p.RepoID
	case CheckDispatched:
		repoID = p.RepoID
	default:
		return 0, fmt.Errorf("code: %s is not an op this build applies to a repository's checks (the record "+
			"at %s)", at.record.Op, at.position)
	}
	if repoID != at.record.Subject.ID {
		return 0, fmt.Errorf("code: the record at %s is on checks.%s and its payload names repository %s — a "+
			"record on one repository's checks subject may write only that repository's checks", at.position,
			at.record.Subject.ID, repoID)
	}
	switch p := payload.(type) {
	case CheckQueued:
		return joinCheck(ctx, tx, at, p.CheckKey, queuedPart(p, at))
	case CheckStarted:
		return joinCheck(ctx, tx, at, p.CheckKey, startedPart(p))
	case CheckFinished:
		return applyCheckFinished(ctx, tx, at, p)
	case ChecksSweep:
		return applyChecksSweep(ctx, tx, at, p)
	case CheckDispatched:
		return applyCheckDispatched(ctx, tx, at, p)
	}
	return 0, fmt.Errorf("code: the record at %s carries a %T", at.position, payload)
}

// applyCheckFinished joins an ending into its attempt's row, and then removes
// ITS OWN REPOSITORY'S HISTORY past the record's broker instant less
// [CheckHistory]: an ending is what keeps the rows' removal in step with the
// checks that end.
func applyCheckFinished(ctx context.Context, tx *sql.Tx, at applyContext, f CheckFinished) (int, error) {
	part, err := finishedPart(f, at)
	if err != nil {
		return 0, err
	}
	joined, err := joinCheck(ctx, tx, at, f.CheckKey, part)
	if err != nil {
		return 0, err
	}
	removed, err := checksHorizon(ctx, tx, at, f.RepoID, store.EncodeTime(at.brokerAt.Add(-CheckHistory)))
	return joined + removed, err
}

// applyChecksSweep removes the repository's check rows that ended before the
// sweep's Through, as an ending's horizon does.
//
// A THROUGH NO WRITER FORMS IS AN ERROR, not a skip, as a retire sweep's: a
// cutoff at or past the record's own broker instant would remove checks that
// ended after the sweep landed — a history no duty holder's clock, held within
// an hour of the fleet's by `clock_skew`, can name.
func applyChecksSweep(ctx context.Context, tx *sql.Tx, at applyContext, s ChecksSweep) (int, error) {
	if s.Through.IsZero() || !s.Through.Before(at.brokerAt) {
		return 0, fmt.Errorf("code: the checks sweep at %s removes the checks that ended before %s, not before "+
			"its own instant %s — no writer forms it", at.position, s.Through.Format(time.RFC3339Nano),
			at.brokerAt.Format(time.RFC3339Nano))
	}
	return checksHorizon(ctx, tx, at, s.RepoID, store.EncodeTime(s.Through))
}

// checkQueue is a row's QUEUE, as the queuing record with the lowest position
// set it ([checkRow]).
type checkQueue struct {
	purpose, changeID, candidateID, entryID, base, defsOID, runIn, def string
	revision, timeoutSeconds                                           int64
	required                                                           bool
	queuedAt, position                                                 int64
}

// overdueAt is the instant, in the store's microseconds, the queue's attempt
// falls overdue — waits for a runner longer than its own definition's
// timeout, which the code_checks_backlog alarm counts — or 0 for an attempt
// with no timeout: one a person's CI reports, which no runner of the engine
// claims, and so is never overdue. code_checks_overdue_idx holds the queued
// attempts by it, so the backlog's walk reads them in the order they fell
// overdue (replicated 0083).
func (q checkQueue) overdueAt() int64 {
	if q.timeoutSeconds <= 0 {
		return 0
	}
	return q.queuedAt + q.timeoutSeconds*int64(time.Second/time.Microsecond)
}

// checkRun is a row's RUN, as the claim with the lower (startedAt, runner)
// set it, its holder's later record naming the box.
type checkRun struct {
	runner, placement, box string
	borrowed               bool
	startedAt              int64
}

// checkEnding is a row's ENDING, as the ending a join keeps set it, with the
// log it named.
type checkEnding struct {
	state, reason, reporter, url, summary, tail, failures string
	exitCode                                              sql.NullInt64
	durationMs, failuresOmitted, finishedAt, position     int64
	log                                                   *CheckLog
}

// checkPart is what one record says of a row: each of its three parts, nil
// where the record carries none of it.
type checkPart struct {
	queue  *checkQueue
	run    *checkRun
	ending *checkEnding
}

// queuedPart is a queued attempt's part — and, for one recorded skipped, its
// ending too, at the queuing instant and position.
func queuedPart(q CheckQueued, at applyContext) checkPart {
	part := checkPart{queue: &checkQueue{
		purpose: string(q.Purpose), changeID: q.ChangeID, revision: int64(q.Revision),
		candidateID: q.CandidateID, entryID: q.EntryID, base: q.Base, defsOID: q.DefsOID,
		required: q.Required, queuedAt: store.EncodeTime(q.QueuedAt), position: at.packed,
	}}
	if q.Def != nil {
		// THE DEFINITION AS THE RECORD CARRIES IT, re-encoded by the one
		// encoder every node runs, so every node stores it byte for byte.
		def, _ := json.Marshal(q.Def)
		part.queue.def, part.queue.runIn, part.queue.timeoutSeconds = string(def), string(q.Def.RunIn),
			int64(q.Def.TimeoutSeconds)
	}
	if q.Skipped {
		part.ending = &checkEnding{state: string(CheckStateSkipped), reason: string(CheckReasonPaths),
			finishedAt: part.queue.queuedAt, position: at.packed}
	}
	return part
}

// startedPart is a started attempt's part: its run. A function of its own, as
// each record's part is, so the record is a typed parameter wherever its fields
// are converted — inline, a type switch's variable handed to a conversion is
// a value the code cache's writers gate cannot type, and refuses
// (TestNothingButSyncWritesACache).
func startedPart(s CheckStarted) checkPart {
	return checkPart{run: &checkRun{
		runner: s.Runner, placement: string(s.Placement), box: s.Box, borrowed: s.Borrowed,
		startedAt: store.EncodeTime(s.StartedAt),
	}}
}

// finishedPart is an ending's part.
func finishedPart(f CheckFinished, at applyContext) (checkPart, error) {
	e := &checkEnding{
		state: string(f.State), reason: string(f.Reason), url: f.URL, summary: f.Summary, tail: f.Tail,
		durationMs: f.DurationMs, failuresOmitted: int64(f.FailuresOmitted),
		finishedAt: store.EncodeTime(f.FinishedAt), position: at.packed, log: f.Log,
	}
	if f.ExitCode != nil {
		e.exitCode = sql.NullInt64{Int64: int64(*f.ExitCode), Valid: true}
	}
	if f.Reporter != nil {
		// A PERSON, by the human seat they hold — who reported it, as every
		// row names a person; the credential is the record's.
		e.reporter = f.Reporter.Seat
	}
	if len(f.Failures) > 0 {
		failures, err := json.Marshal(f.Failures)
		if err != nil {
			return checkPart{}, fmt.Errorf("code: encode the failures the ending at %s names: %w", at.position, err)
		}
		e.failures = string(failures)
	}
	return checkPart{ending: e}, nil
}

// checkRow is one code_checks row as the join reads and writes it: its key and
// its three parts, each present by a column only that part sets — the queue by
// its position, the run by its placement, the ending by its position.
type checkRow struct {
	key    CheckKey
	queue  checkQueue
	run    checkRun
	ending checkEnding
}

// state is the row's state, DERIVED from its parts and never joined on its
// own: an ending's once one arrived, running once a claim did, queued
// otherwise — so it only ever rises.
func (r checkRow) state() CheckState {
	switch {
	case r.ending.position > 0:
		return CheckState(r.ending.state)
	case r.run.placement != "":
		return CheckStateRunning
	}
	return CheckStateQueued
}

// rowBytes is what the row's wide columns weigh, as code_checks.row_bytes
// records it: its definition, its link, its summary, its tail and its
// failures — the columns that make one row wider than another, whose overflow
// pages a row's deletion frees and the horizon and a purge part are bounded
// by. Every other column is a few dozen bytes on every row.
func (r checkRow) rowBytes() int64 {
	return int64(len(r.queue.def) + len(r.ending.url) + len(r.ending.summary) + len(r.ending.tail) +
		len(r.ending.failures))
}

// joinQueue keeps the queue of the lower position: one dispatch's, whichever
// of two a failover published lands first, and a row's first queue whatever
// made the row. It reports whether the row changed.
func (r *checkRow) joinQueue(q checkQueue) bool {
	if r.queue.position != 0 && r.queue.position <= q.position {
		return false
	}
	r.queue = q
	return true
}

// joinRun keeps the claim with the lower (startedAt, runner); and of ONE
// claim — its holder's later record repeats its StartedAt and Runner — the
// box it names, set once (two different, which no holder names, keep the
// lesser, so either order holds one), the placement likewise, and borrowed
// once either said so. It reports whether the row changed.
func (r *checkRow) joinRun(s checkRun) bool {
	held := r.run
	switch {
	case held.placement == "", s.startedAt < held.startedAt,
		s.startedAt == held.startedAt && s.runner < held.runner:
		r.run = s
	case s.startedAt == held.startedAt && s.runner == held.runner:
		r.run.placement = lesser(held.placement, s.placement)
		r.run.box = lesser(held.box, s.box)
		r.run.borrowed = held.borrowed || s.borrowed
	}
	return r.run != held
}

// lesser is the lesser of two values a join keeps one of, an empty one being
// no value: the one set, or the lower of two set.
func lesser(a, b string) string {
	switch {
	case a == "":
		return b
	case b == "" || a <= b:
		return a
	}
	return b
}

// joinEnding keeps the ending [endsBefore] puts first. It reports whether the
// row changed, and so whether its log follows.
func (r *checkRow) joinEnding(e checkEnding) bool {
	if r.ending.position != 0 && !endsBefore(e, r.ending) {
		return false
	}
	r.ending = e
	return true
}

// endsBefore orders two endings of one attempt, TOTALLY, so a join of any of
// them in any order keeps one: a measurement before infrastructure's — a run
// that passed or failed measured the commit, and one that errored, was
// cancelled or skipped did not — then the earlier finishedAt, then the lower
// reporter, then the lower position, which no two records share.
func endsBefore(a, b checkEnding) bool {
	if am, bm := CheckState(a.state).Measured(), CheckState(b.state).Measured(); am != bm {
		return am
	}
	if a.finishedAt != b.finishedAt {
		return a.finishedAt < b.finishedAt
	}
	if a.reporter != b.reporter {
		return a.reporter < b.reporter
	}
	return a.position < b.position
}

// joinCheck joins one record's part into the row at key — making the row from
// the part where none is held — writes the row back where the join changed
// it, and the log of the ending it keeps where that changed: the rows it
// wrote and deleted.
func joinCheck(ctx context.Context, tx *sql.Tx, at applyContext, key CheckKey, part checkPart) (int, error) {
	row, held, err := readCheckRow(ctx, tx, key)
	if err != nil {
		return 0, fmt.Errorf("code: read check %s of %s at %s: %w", key.Name, key.OID, at.position, err)
	}
	if !held {
		row = checkRow{key: key}
	}
	changed, ended := false, false
	if part.queue != nil {
		changed = row.joinQueue(*part.queue) || changed
	}
	if part.run != nil {
		changed = row.joinRun(*part.run) || changed
	}
	if part.ending != nil {
		ended = row.joinEnding(*part.ending)
		changed = ended || changed
	}
	if !changed {
		return 0, nil
	}
	q, r, e := row.queue, row.run, row.ending
	if _, err = tx.ExecContext(ctx, writeCheckSQL, key.RepoID, key.OID, key.Name, key.Attempt,
		string(row.state()), q.purpose, q.changeID, q.revision, q.candidateID, q.entryID, key.Target, q.base,
		q.defsOID, q.required, q.runIn, q.timeoutSeconds, e.reason, r.runner, r.placement, r.box, r.borrowed,
		e.reporter, q.queuedAt, r.startedAt, e.finishedAt, e.durationMs, e.exitCode, e.failuresOmitted,
		q.position, e.position, row.rowBytes(), q.overdueAt(), e.url, e.summary, q.def, e.failures,
		e.tail); err != nil {
		return 0, fmt.Errorf("code: write check %s of %s at %s: %w", key.Name, key.OID, at.position, err)
	}
	if !ended {
		return 1, nil
	}
	logs, err := writeEndingLog(ctx, tx, at, key, row.ending.log)
	return 1 + logs, err
}

// writeEndingLog makes the attempt's log row the KEPT ENDING'S: the ending it
// replaced named its own log or none, and the one it keeps names its own or
// none, so the row is dropped and, for an ending that kept a log, written. It
// answers the rows it deleted and wrote.
func writeEndingLog(ctx context.Context, tx *sql.Tx, at applyContext, key CheckKey, l *CheckLog) (int, error) {
	res, err := tx.ExecContext(ctx, dropCheckLogSQL, key.RepoID, key.OID, key.Target, key.Name, key.Attempt)
	if err != nil {
		return 0, fmt.Errorf("code: drop check %s of %s's log at %s: %w", key.Name, key.OID, at.position, err)
	}
	dropped, _ := res.RowsAffected()
	if l == nil {
		return int(dropped), nil
	}
	if _, err = tx.ExecContext(ctx, writeCheckLogSQL, key.RepoID, key.OID, key.Target, key.Name, key.Attempt,
		l.Object.String(), string(l.Hash), l.Size); err != nil {
		return 0, fmt.Errorf("code: write check %s of %s's log at %s: %w", key.Name, key.OID, at.position, err)
	}
	return int(dropped) + 1, nil
}

// readCheckRow reads the row at key, whole; false where none is held. The log
// is not read: a join that keeps a new ending writes the log that ending names
// over whatever was there.
func readCheckRow(ctx context.Context, tx *sql.Tx, key CheckKey) (checkRow, bool, error) {
	r := checkRow{key: key}
	q, run, e := &r.queue, &r.run, &r.ending
	err := tx.QueryRowContext(ctx, readCheckSQL, key.RepoID, key.OID, key.Target, key.Name, key.Attempt).Scan(
		&e.state, &q.purpose, &q.changeID, &q.revision, &q.candidateID, &q.entryID, &q.base,
		&q.defsOID, &q.required, &q.runIn, &q.timeoutSeconds, &e.reason, &run.runner, &run.placement, &run.box,
		&run.borrowed, &e.reporter, &q.queuedAt, &run.startedAt, &e.finishedAt, &e.durationMs, &e.exitCode,
		&e.failuresOmitted, &q.position, &e.position, &e.url, &e.summary, &q.def, &e.failures, &e.tail)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return checkRow{}, false, nil
	case err != nil:
		return checkRow{}, false, err
	}
	if e.position == 0 {
		// THE STATE COLUMN IS DERIVED: an ending's state is the ending's
		// only once there is one.
		e.state = ""
	}
	return r, true, nil
}

// checksHorizon removes repoID's check rows that ended before cutoff — each
// with its log — oldest first, at most [CheckHorizonRows] of them and only
// while what they weigh is under [CheckHorizonBytes], the first whatever it
// weighs, so a record always advances the history; it answers the rows it
// deleted. Every node removes the same rows: the order is the index's own, its
// key the row's whole key.
func checksHorizon(ctx context.Context, tx *sql.Tx, at applyContext, repoID string, cutoff int64) (int, error) {
	take, _, _, err := checksEndedBefore(ctx, tx, repoID, 1, cutoff, CheckHorizonRows, CheckHorizonBytes)
	if err != nil {
		return 0, fmt.Errorf("code: read %s's checks past their history at %s: %w", repoID, at.position, err)
	}
	return deleteOldestChecks(ctx, tx, repoID, 1, cutoff, take)
}

// checksEndedBefore reads repoID's check rows whose finished_at is in [lo,
// hi), oldest first, and answers how many of them one removal takes — at most
// maxRows, and only while their row_bytes are under maxBytes, the first
// whatever it weighs — what those weigh, and whether rows in the range remain
// past them. The horizon and a checks sweep's decide ask it of the ended rows
// before a cutoff (lo 1: an ending's instant; 0 is no ending), a purge part of
// every row a repository holds.
func checksEndedBefore(ctx context.Context, tx *sql.Tx, repoID string, lo, hi int64, maxRows int,
	maxBytes int64) (take int, weighed int64, more bool, err error) {

	rows, err := tx.QueryContext(ctx, checksOldestSQL, repoID, lo, hi, maxRows+1)
	if err != nil {
		return 0, 0, false, err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		if take == maxRows || (take > 0 && weighed >= maxBytes) {
			return take, weighed, true, nil
		}
		var commit, target, name string
		var attempt, width int64
		if err := rows.Scan(&commit, &target, &name, &attempt, &width); err != nil {
			return 0, 0, false, err
		}
		take++
		weighed += width
	}
	return take, weighed, false, rows.Err()
}

// deleteOldestChecks deletes the first n of repoID's check rows whose
// finished_at is in [lo, hi) in [checksOldestSQL]'s order — their logs first,
// found through them — and answers the rows it deleted.
func deleteOldestChecks(ctx context.Context, tx *sql.Tx, repoID string, lo, hi int64, n int) (int, error) {
	if n == 0 {
		return 0, nil
	}
	deleted := 0
	for _, stmt := range []string{deleteOldestCheckLogsSQL, deleteOldestChecksSQL} {
		res, err := tx.ExecContext(ctx, stmt, repoID, lo, hi, n)
		if err != nil {
			return 0, fmt.Errorf("code: delete %s's oldest checks: %w", repoID, err)
		}
		affected, _ := res.RowsAffected()
		deleted += int(affected)
	}
	return deleted, nil
}

// purgeChecks deletes, for a purge part, a deleted repository's check rows —
// each with its log — in the horizon's order, at most half of budget of them,
// since each may take its log with it, and only while the part weighs under
// PurgePartBytes with weighed already taken, the first whatever it weighs; it
// answers the rows it deleted. A check's rows are the last a part reaches, so
// a repository's pushes, refs, packs and changes go before them.
func purgeChecks(ctx context.Context, tx *sql.Tx, repoID string, budget int, weighed int64) (int, error) {
	if budget < 2 || weighed >= PurgePartBytes {
		return 0, nil
	}
	take, _, _, err := checksEndedBefore(ctx, tx, repoID, math.MinInt64, math.MaxInt64, budget/2,
		PurgePartBytes-weighed)
	if err != nil {
		return 0, fmt.Errorf("code: read %s's checks to purge: %w", repoID, err)
	}
	return deleteOldestChecks(ctx, tx, repoID, math.MinInt64, math.MaxInt64, take)
}

// The checks subject's statements — the applier's, the horizon's and a purge
// part's — each registered in plan_test.go, so the pinned driver's planner is
// held to the key each is written for.
const (
	// checksKey is code_checks' PRIMARY KEY index, (repo_id, commit_oid,
	// target_ref, name, attempt), which shares its leading column with
	// code_checks_finished_idx and code_checks_running_idx: a read of one row
	// NAMES it (planner.go).
	checksKey = "sqlite_autoindex_code_checks_1"
	// checksEndedKey is code_checks_finished_idx, (repo_id, finished_at,
	// commit_oid, target_ref, name, attempt, row_bytes): a repository's rows
	// in the one order every node removes them in, carrying what each weighs.
	checksEndedKey = "code_checks_finished_idx"

	readCheckSQL = `SELECT state, purpose, change_id, revision, candidate_id, entry_id, base_oid,
		defs_oid, required, run_in, timeout_seconds, reason, runner_node, placement, box_handle, borrowed,
		reporter, queued_at, started_at, finished_at, duration_ms, exit_code, failures_omitted,
		queued_position, finished_position, url, summary, def, failures, tail
		FROM code_checks INDEXED BY ` + checksKey + `
		WHERE repo_id = ? AND commit_oid = ? AND target_ref = ? AND name = ? AND attempt = ?`
	// writeCheckSQL writes a row whole, the join's: every column the join
	// computed, over whatever the row held — overdue_at derived from its
	// queue (checkQueue.overdueAt).
	writeCheckSQL = `INSERT INTO code_checks (repo_id, commit_oid, name, attempt, state, purpose,
			change_id, revision, candidate_id, entry_id, target_ref, base_oid, defs_oid, required, run_in,
			timeout_seconds, reason, runner_node, placement, box_handle, borrowed, reporter, queued_at,
			started_at, finished_at, duration_ms, exit_code, failures_omitted, queued_position,
			finished_position, row_bytes, overdue_at, url, summary, def, failures, tail)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?,
			?, ?, ?, ?, ?, ?)
		ON CONFLICT (repo_id, commit_oid, target_ref, name, attempt) DO UPDATE SET
			state = excluded.state, purpose = excluded.purpose, change_id = excluded.change_id,
			revision = excluded.revision, candidate_id = excluded.candidate_id, entry_id = excluded.entry_id,
			base_oid = excluded.base_oid, defs_oid = excluded.defs_oid,
			required = excluded.required, run_in = excluded.run_in, timeout_seconds = excluded.timeout_seconds,
			reason = excluded.reason, runner_node = excluded.runner_node, placement = excluded.placement,
			box_handle = excluded.box_handle, borrowed = excluded.borrowed, reporter = excluded.reporter,
			queued_at = excluded.queued_at, started_at = excluded.started_at,
			finished_at = excluded.finished_at, duration_ms = excluded.duration_ms,
			exit_code = excluded.exit_code, failures_omitted = excluded.failures_omitted,
			queued_position = excluded.queued_position, finished_position = excluded.finished_position,
			row_bytes = excluded.row_bytes, overdue_at = excluded.overdue_at, url = excluded.url,
			summary = excluded.summary,
			def = excluded.def, failures = excluded.failures, tail = excluded.tail`
	dropCheckLogSQL = `DELETE FROM code_check_logs
		WHERE repo_id = ? AND commit_oid = ? AND target_ref = ? AND name = ? AND attempt = ?`
	writeCheckLogSQL = `INSERT INTO code_check_logs (repo_id, commit_oid, target_ref, name, attempt, object, hash,
			size)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`
	// checksOldestSQL is a repository's rows whose finished_at is in [?2, ?3)
	// in the removal's order, with what each weighs — a covering read of
	// checksEndedKey, so weighing a row never reads it.
	checksOldestSQL = `SELECT commit_oid, target_ref, name, attempt, row_bytes
		FROM code_checks INDEXED BY ` + checksEndedKey + `
		WHERE repo_id = ?1 AND finished_at >= ?2 AND finished_at < ?3
		ORDER BY finished_at, commit_oid, target_ref, name, attempt LIMIT ?4`
	// deleteOldestCheckLogsSQL and deleteOldestChecksSQL delete the first ?4
	// of those rows: the logs through the rows they belong to, each by its
	// key, then the rows — the rowid idiom, so they are the rows the order
	// reaches first and no more.
	deleteOldestCheckLogsSQL = `DELETE FROM code_check_logs WHERE rowid IN (SELECT l.rowid
		FROM (SELECT commit_oid, target_ref, name, attempt FROM code_checks INDEXED BY ` + checksEndedKey + `
			WHERE repo_id = ?1 AND finished_at >= ?2 AND finished_at < ?3
			ORDER BY finished_at, commit_oid, target_ref, name, attempt LIMIT ?4) AS c
		CROSS JOIN code_check_logs AS l
		WHERE l.repo_id = ?1 AND l.commit_oid = c.commit_oid AND l.target_ref = c.target_ref
			AND l.name = c.name AND l.attempt = c.attempt)`
	deleteOldestChecksSQL = `DELETE FROM code_checks WHERE rowid IN (SELECT rowid
		FROM code_checks INDEXED BY ` + checksEndedKey + `
		WHERE repo_id = ?1 AND finished_at >= ?2 AND finished_at < ?3
		ORDER BY finished_at, commit_oid, target_ref, name, attempt LIMIT ?4)`
	// THE FINAL PURGE'S, each of a repository's whole set: its logs, then
	// its rows.
	purgeAllCheckLogsSQL = `DELETE FROM code_check_logs WHERE repo_id = ?`
	purgeAllChecksSQL    = `DELETE FROM code_checks WHERE repo_id = ?`
)

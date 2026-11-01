package code

import (
	"context"
	"database/sql"
	"encoding/base64"
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
// each commit, as its row on the code log holds it — six kinds of read.
//
//   - `checks` ([ReadChecks]) lists attempts, held to the asker's grant: one
//     repository's, newest ended first and every attempt not yet ended after
//     them (code_checks_finished_idx, read backwards); one change's of it —
//     its head's, or a commit it names, CHECKED FOR ITS TARGET — by name and
//     attempt, or one commit's for every target (both code_checks' key); or
//     the WINDOW — no repository named — every repository's attempts that
//     ended at or after a moment, newest ended first (code_checks_recent_idx),
//     each kept only where its repository is one the asker reads, so a page
//     is the rows it READ and may be short, as a listing of changes across
//     repositories is. Every shape takes a check's name and a state as
//     NARROWINGS, tested on the rows a page read beside the grant
//     ([CheckQuery.State]): the dashboard's Checks section's facets.
//   - `check` ([ReadCheck]) is one attempt whole: the row, the definition it
//     ran under, its tail, the failures its ending named, and the log a
//     failed run kept.
//   - `checks_due` ([ReadChecksDue]) is the home's: the live changes whose head
//     no dispatch for its target names its target's checks file, or whose
//     head's attempt its infrastructure ended is due a requeue, each with
//     what it owes, the targets due a reading of their checks file again,
//     and the protected targets owing tip work (dueCheckWork's doc).
//   - `check_box` ([ReadCheckBox]) is a check's box's admission's: the one
//     attempt its token names, by its key (code_checks' own), as its row
//     stands — the engine's own read, held to no grant, since the box is no
//     principal and the row is what admits it.
//   - `checks_queued` ([ReadChecksQueued]) is a git node's claim walk: the
//     attempts waiting for a runner whose definitions name one cell, oldest
//     queued first, each with its definition and its repository's name —
//     the engine's own, as `check_box` is (queuedChecks' doc).
//   - `checks_running` ([ReadChecksRunning]) is a repository's home's sweep:
//     the repositories holding a running attempt, or one repository's
//     running attempts — the engine's own too (runningChecks' doc).
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
	r, err := scanCheck(tx.QueryRowContext(ctx, checkRowSQL, key.RepoID, key.OID, key.Target, key.Name,
		key.Attempt).Scan)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return CheckRow{}, false, nil
	case err != nil:
		return CheckRow{}, false, fmt.Errorf("code: read check %s of %s: %w", key.Name, key.OID, err)
	}
	return r, true, nil
}

// latestCheckIn is the latest attempt in tx of key's check of its commit for
// its target — the one the next attempt follows, whatever key's own attempt —
// and false where the repository holds none.
func latestCheckIn(ctx context.Context, tx *sql.Tx, key CheckKey) (CheckRow, bool, error) {
	r, err := scanCheck(tx.QueryRowContext(ctx, latestCheckSQL, key.RepoID, key.OID, key.Target, key.Name).Scan)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return CheckRow{}, false, nil
	case err != nil:
		return CheckRow{}, false, fmt.Errorf("code: read the latest attempt of check %s of %s for %s: %w",
			key.Name, key.OID, key.Target, err)
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

// checksQuery is the framework read a `checks`, `check`, `checks_due`,
// `check_box`, `checks_queued` or `checks_running` read resolves to. A listing
// is a SET READ — of the repository it names, of the domain for the window —
// so an attempt a deferred record holds back is listed as its row stands; a
// `check` read is a POINT READ, which a record this node deferred on the
// repository's checks refuses, as a change's detail is refused, rather than
// answer an attempt around it, and so is a `check_box` read, of the checks of
// the repository its key names by id; the home's listing is a set read of the
// domain, or of the one repository's rows and checks; and a claim walk and the
// home's sweep are set reads too — of the domain, or of the one repository's
// checks — each claim and each ending they lead to decided again by the
// repository's checks writer from its own snapshot.
func (q ReadQuery) checksQuery(fresh statelog.Freshness) statelog.Query {
	switch q.Kind {
	case ReadCheck:
		return fresh.Resolved(checksResolver(q.Repo), false)
	case ReadCheckBox:
		return fresh.Query(statelog.ScopeSet{Paths: []string{ChecksPath(q.CheckBox.RepoID)}}, false)
	case ReadChecksQueued:
		return fresh.Query(statelog.ScopeSet{Paths: []string{DomainPath()}}, true)
	case ReadChecksRunning:
		if id := q.ChecksRunning.RepoID; id != "" {
			return fresh.Query(statelog.ScopeSet{Paths: []string{ChecksPath(id)}}, true)
		}
		return fresh.Query(statelog.ScopeSet{Paths: []string{DomainPath()}}, true)
	case ReadChecksDue:
		if q.ChecksDue == nil || q.ChecksDue.RepoID == "" {
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
		case c.State != "":
			return refused("checks.state", "a check read names one attempt, whatever its state; a listing of "+
				"checks is narrowed by one")
		case q.Cursor != "" || q.Limit != 0:
			return refused("limit", "one attempt is read whole, never paged")
		case !gitref.ValidOID(c.OID) || c.OID == ZeroOID:
			return refused("checks.oid", fmt.Sprintf("%q is not a commit's whole id", c.OID))
		case !strings.HasPrefix(c.Target, "refs/heads/") || !ValidRef(c.Target):
			return refused("checks.target", fmt.Sprintf("%q is not a branch's ref: an attempt is of a commit "+
				"checked for a branch", c.Target))
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
	case c.Target != "":
		return refused("checks.target", "a listing of checks names no branch: a change's are those checked "+
			"for its own target, and a commit's those checked for every branch it was; a check read names one")
	case c.Attempt != 0:
		return refused("checks.attempt", "a listing of checks names no attempt; a check read reads one")
	case c.Name != "" && !config.ValidCodeCheckName(c.Name):
		return refused("checks.name", fmt.Sprintf("%q is not a check name", c.Name))
	case c.State != "" && !c.State.Valid():
		return refused("checks.state", fmt.Sprintf("%q is not one of %v", c.State, CheckStates()))
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
// `compaction_due` read's reason; its own question, REQUIRED, with the instant
// a target's reading is due again by, and its one repository, where it names
// one, by id in its canonical spelling; and nothing else a listing names,
// since a page is MaxSnapshotItems rows.
func (q ReadQuery) validateChecksDue() error {
	refused := func(field, why string) error { return fmt.Errorf("%w: %s: %s", ErrInvalid, field, why) }
	switch {
	case q.Asker != (Principal{}):
		return refused("asker", "a checks_due read is the engine's own, which no principal makes")
	case q.CeilingSeat != "":
		return refused("ceiling_seat", "a checks_due read is the engine's own, which no seat makes for another")
	case q.ChecksDue == nil:
		return refused("checks_due", "a checks_due read names the instant its home's reading of a target's "+
			"checks file is due again by, and, for one repository's due check work, the repository")
	case q.ChecksDue.RefreshBefore.IsZero():
		return refused("checks_due.refresh_before", "a checks_due read names the instant a target's reading "+
			"recorded at or before is due again: its home's clock less its refresh")
	case q.ChecksDue.RepoID != "" && !canonicalUUID(q.ChecksDue.RepoID):
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
		{"changes_due", q.ChangesDue != nil}, {"checks", q.Checks != nil}, {"check_box", q.CheckBox != nil},
		{"path_owners", q.PathOwners}, {"gates", q.Gates}, {"expunge_tips", q.ExpungeTips != nil},
		{"expunged_before", !q.ExpungedBefore.IsZero()},
		{"branch_changes", q.BranchChanges != nil},
		{"target", q.Target != ""}, {"queue_due", q.QueueDue != nil},
		{"checks_running", q.ChecksRunning != nil}, {"mirror_push", q.MirrorPush != nil},
		{"checks_queued", q.ChecksQueued != nil},
	} {
		if f.set {
			return refused(f.field, "a checks_due read lists the repositories a home owes check work in, or "+
				"one repository's due changes by its id, a page of MaxSnapshotItems at a time, and names "+
				"nothing else")
		}
	}
	return nil
}

// validateCheckBox is [ReadQuery.validate] for a `check_box` read: the
// engine's own — a check's box's admission — so it names NO asker and no seat
// it reads for, since the box is no principal and judged as one it would be
// refused as one nobody named; its attempt by its key, whole, as the box's
// token names it; and nothing else a read names, since it reads one row.
func (q ReadQuery) validateCheckBox() error {
	refused := func(field, why string) error { return fmt.Errorf("%w: %s: %s", ErrInvalid, field, why) }
	switch {
	case q.Asker != (Principal{}):
		return refused("asker", "a check_box read is the engine's own — a check's box's admission — which no "+
			"principal makes")
	case q.CeilingSeat != "":
		return refused("ceiling_seat", "a check_box read is the engine's own, which no seat makes for another")
	case q.CheckBox == nil:
		return refused("check_box", "a check_box read names the attempt it reads by its key")
	}
	if err := q.CheckBox.validateAs("check_box."); err != nil {
		return err
	}
	for _, f := range []struct {
		field string
		set   bool
	}{
		{"repo", q.Repo != ""}, {"status", len(q.Status) > 0}, {"prefix", q.Prefix != ""},
		{"introduced", q.Introduced != nil}, {"run_pushes", q.RunPushes != nil}, {"min_packs", q.MinPacks != 0},
		{"limit", q.Limit != 0}, {"cursor", q.Cursor != ""}, {"brief", q.Brief}, {"push", q.Push != ""},
		{"change", q.Change != 0}, {"state", q.State != ""}, {"owner", q.Owner != ""}, {"work", q.Work != ""},
		{"reviewer", q.Reviewer != ""}, {"needs", q.Needs != ""}, {"part", q.Part != ""},
		{"changes_due", q.ChangesDue != nil}, {"checks", q.Checks != nil}, {"checks_due", q.ChecksDue != nil},
		{"path_owners", q.PathOwners}, {"gates", q.Gates}, {"expunge_tips", q.ExpungeTips != nil},
		{"expunged_before", !q.ExpungedBefore.IsZero()},
		{"branch_changes", q.BranchChanges != nil},
		{"target", q.Target != ""}, {"queue_due", q.QueueDue != nil},
		{"checks_running", q.ChecksRunning != nil}, {"mirror_push", q.MirrorPush != nil},
		{"checks_queued", q.ChecksQueued != nil},
	} {
		if f.set {
			return refused(f.field, "a check_box read reads the one attempt its key names, whole, and names "+
				"nothing else")
		}
	}
	return nil
}

// validateChecksQueued is [ReadQuery.validate] for a `checks_queued` read: the
// engine's own — a runner's claim walk — so it names NO asker and no seat it
// reads for, for `check_box`'s reason; its question, REQUIRED, naming the
// cell its attempts' definitions name, or none; a page of at most
// MaxSnapshotItems attempts and the cursor it resumes after; and nothing else
// a read names, since it lists every repository's queued attempts.
func (q ReadQuery) validateChecksQueued() error {
	refused := func(field, why string) error { return fmt.Errorf("%w: %s: %s", ErrInvalid, field, why) }
	switch r := q.ChecksQueued; {
	case r == nil:
		return refused("checks_queued", `a checks_queued read names its question: the cell its attempts' `+
			`definitions name, or "" for the ones naming none`)
	case r.RunIn != "" && !r.RunIn.Valid():
		return refused("checks_queued.run_in", fmt.Sprintf(`%q is not one of %v, or "" for none`, r.RunIn,
			CheckCells()))
	case q.Asker != (Principal{}):
		return refused("asker", "a checks_queued read is the engine's own — a runner's claim walk — which no "+
			"principal makes")
	case q.CeilingSeat != "":
		return refused("ceiling_seat", "a checks_queued read is the engine's own, which no seat makes for "+
			"another")
	case q.Limit < 0 || q.Limit > MaxSnapshotItems:
		return refused("limit", fmt.Sprintf("%d is outside 1..%d (code.MaxSnapshotItems), or 0 for the most",
			q.Limit, MaxSnapshotItems))
	}
	for _, f := range []struct {
		field string
		set   bool
	}{
		{"repo", q.Repo != ""}, {"status", len(q.Status) > 0}, {"prefix", q.Prefix != ""},
		{"introduced", q.Introduced != nil}, {"run_pushes", q.RunPushes != nil}, {"min_packs", q.MinPacks != 0},
		{"brief", q.Brief}, {"push", q.Push != ""}, {"change", q.Change != 0}, {"state", q.State != ""},
		{"owner", q.Owner != ""}, {"work", q.Work != ""}, {"reviewer", q.Reviewer != ""},
		{"needs", q.Needs != ""}, {"part", q.Part != ""}, {"changes_due", q.ChangesDue != nil},
		{"checks", q.Checks != nil}, {"checks_due", q.ChecksDue != nil}, {"check_box", q.CheckBox != nil},
		{"checks_running", q.ChecksRunning != nil}, {"mirror_push", q.MirrorPush != nil}, {"path_owners", q.PathOwners}, {"gates", q.Gates},
		{"expunge_tips", q.ExpungeTips != nil},
		{"expunged_before", !q.ExpungedBefore.IsZero()},
		{"branch_changes", q.BranchChanges != nil}, {"target", q.Target != ""}, {"queue_due", q.QueueDue != nil},
	} {
		if f.set {
			return refused(f.field, "a checks_queued read lists every repository's queued attempts, a page at "+
				"a time, and names nothing else")
		}
	}
	return nil
}

// validateChecksRunning is [ReadQuery.validate] for a `checks_running` read:
// the engine's own — a repository's home's sweep — naming no asker and no
// seat; its question, REQUIRED, with its one repository, where it names one,
// by id in its canonical spelling; a cursor only beside none, since a
// repository's running attempts are read in one page; and nothing else.
func (q ReadQuery) validateChecksRunning() error {
	refused := func(field, why string) error { return fmt.Errorf("%w: %s: %s", ErrInvalid, field, why) }
	switch r := q.ChecksRunning; {
	case q.Asker != (Principal{}):
		return refused("asker", "a checks_running read is the engine's own — a home's sweep — which no "+
			"principal makes")
	case q.CeilingSeat != "":
		return refused("ceiling_seat", "a checks_running read is the engine's own, which no seat makes for "+
			"another")
	case r == nil:
		return refused("checks_running", "a checks_running read names its question: the repositories holding "+
			"a running attempt, or one repository's by its id")
	case r.RepoID != "" && !canonicalUUID(r.RepoID):
		return refused("checks_running.repo_id", fmt.Sprintf("%q is not a repository's id in its canonical "+
			"lowercase spelling", r.RepoID))
	case r.RepoID != "" && q.Cursor != "":
		return refused("cursor", "a repository's running attempts are read whole, in one page; only the "+
			"listing of repositories is paged")
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
		{"changes_due", q.ChangesDue != nil}, {"checks", q.Checks != nil}, {"checks_due", q.ChecksDue != nil},
		{"check_box", q.CheckBox != nil}, {"path_owners", q.PathOwners}, {"gates", q.Gates},
		{"expunge_tips", q.ExpungeTips != nil},
		{"expunged_before", !q.ExpungedBefore.IsZero()},
		{"branch_changes", q.BranchChanges != nil}, {"target", q.Target != ""}, {"queue_due", q.QueueDue != nil},
		{"checks_queued", q.ChecksQueued != nil}, {"mirror_push", q.MirrorPush != nil},
	} {
		if f.set {
			return refused(f.field, "a checks_running read lists the repositories holding a running attempt, or "+
				"one repository's running attempts by its id, and names nothing else")
		}
	}
	return nil
}

// queuedChecks serves one page of a `checks_queued` read: the attempts waiting
// for a runner whose definitions name the read's cell (ChecksQueuedQuery's
// RunIn, "" for the ones naming none), OLDEST QUEUED FIRST, on
// code_checks_queued_idx — each with the definition it was queued under and
// its repository's name, after the cursor (queuedCursor) — of every repository
// a runner may still claim in, active or archived: a deleted repository
// refuses every claim (repo_deleted), so its queued attempts wait for its
// purge and are read and skipped, as the backlog's walk skips them.
//
// BY CELL, because a runner reads only what it can claim now: the cells it
// has room in (internal/code/checkrun's walk), each one seek, so one cell's
// backlog never costs a node's walk in another (replicated 0102). OLDEST
// FIRST within it, because that is the order a fleet's runners take the queue
// in: every git node walks it from its head, and the one a repository's git
// order ranks first claims each attempt first, so the attempts waiting longest
// are the ones claimed first wherever they fall in the order. A page is a
// limit's rows at most and the page's byte budget — a definition is the widest
// field, some hundreds of bytes for a usual check — and names where the next
// resumes.
func (r *Reader) queuedChecks(ctx context.Context, tx *sql.Tx, q ReadQuery, out *ReadAnswer) error {
	limit := min(limitOr(q.Limit, r.pageRows), r.pageRows)
	var (
		at                       int64
		repo, oid, target, check string
		attempt                  int
	)
	if q.Cursor != "" {
		var ok bool
		if at, repo, oid, target, check, attempt, ok = cutWindowCursor(q.Cursor); !ok {
			return fmt.Errorf("%w: %q", ErrBadCursor, q.Cursor)
		}
	}
	rows, err := tx.QueryContext(ctx, queuedChecksSQL, string(q.ChecksQueued.RunIn), at, repo, oid, target, check,
		attempt, limit)
	if err != nil {
		return fmt.Errorf("code: list the queued checks: %w", err)
	}
	defer func() { _ = rows.Close() }()
	pg := page{maxRows: limit, maxB: r.pageBytes}
	var last CheckRow
	for rows.Next() {
		var (
			c   RunnableCheck
			def string
		)
		if c.CheckRow, err = scanCheck(rows.Scan, &def, &c.Repo); err != nil {
			return fmt.Errorf("code: list the queued checks: %w", err)
		}
		if def != "" {
			c.Def = &CheckDef{}
			if err := json.Unmarshal([]byte(def), c.Def); err != nil {
				return fmt.Errorf("code: read check %s of %s's definition: %w", c.Name, c.OID, err)
			}
		}
		out.Runnable = append(out.Runnable, c)
		last = c.CheckRow
		if pg.add(c.encodedBytes() + stringBytes(c.Repo) + len(def)) {
			break
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("code: list the queued checks: %w", err)
	}
	if pg.full() {
		out.Next = queuedCursor(last)
	}
	return nil
}

// queuedCursor is an attempt's place in the claim walk's order —
// "<queued_at>/<repo>/<commit>/<target>/<name>/<attempt>", the window's shape
// with the queue's instant in its ending's place — read back by
// cutWindowCursor.
func queuedCursor(row CheckRow) string {
	return strconv.FormatInt(store.EncodeTime(row.QueuedAt), 10) + "/" + row.RepoID + "/" + row.OID + "/" +
		targetCursor(row)
}

// runningChecks serves a `checks_running` read: with no repository, a page of
// the repositories holding a running attempt, by id after the cursor — the
// running index's leading column, grouped, so a page is one range of a
// partial index holding only what runs now; with one, that repository's
// running attempts, every one in ONE page: they are bounded by the boxes the
// fleet's runners hold at once — git.max_checks on each git node and the
// remote cells — far under a page's MaxSnapshotItems, and the home that
// sweeps them ends what it finds and reads the rest again at its next pass.
func (r *Reader) runningChecks(ctx context.Context, tx *sql.Tx, q ReadQuery, out *ReadAnswer) error {
	if id := q.ChecksRunning.RepoID; id != "" {
		rows, err := tx.QueryContext(ctx, runningChecksSQL, id, r.pageRows)
		if err != nil {
			return fmt.Errorf("code: list repository %s's running checks: %w", id, err)
		}
		defer func() { _ = rows.Close() }()
		if out.Checks, err = scanChecks(rows); err != nil {
			return fmt.Errorf("code: list repository %s's running checks: %w", id, err)
		}
		return nil
	}
	if q.Cursor != "" && !canonicalUUID(q.Cursor) {
		return fmt.Errorf("%w: %q", ErrBadCursor, q.Cursor)
	}
	rows, err := tx.QueryContext(ctx, runningReposSQL, q.Cursor, r.pageRows)
	if err != nil {
		return fmt.Errorf("code: list the repositories running checks: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return fmt.Errorf("code: list the repositories running checks: %w", err)
		}
		out.DueRepos = append(out.DueRepos, id)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if len(out.DueRepos) == r.pageRows {
		out.Next = out.DueRepos[len(out.DueRepos)-1]
	}
	return nil
}

// checkBox serves a `check_box` read: the attempt its key names as its row
// stands, the answer's one check — or none, where the rows hold no such
// attempt: an answer the box's admission reads again through the barrier
// before it refuses (internal/code/host's checkAdmitted), since a copy behind
// the log may not have applied the attempt's queue yet.
func (r *Reader) checkBox(ctx context.Context, tx *sql.Tx, q ReadQuery, out *ReadAnswer) error {
	row, found, err := checkRowIn(ctx, tx, *q.CheckBox)
	if err != nil || !found {
		return err
	}
	out.Checks = []CheckRow{row}
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
	var rows *sql.Rows
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
		// THE ATTEMPTS THAT JUDGE THE CHANGE: its head's — or the commit it
		// names — checked for ITS target, read on code_checks' key, whichever
		// change's dispatch queued them; never a listing by the change, which
		// a head another change into the same branch shares would read empty.
		oid := change.head
		if c.OID != "" {
			oid = c.OID
		}
		name, attempt, ok := cutNameAttempt(q.Cursor)
		if !ok {
			return fmt.Errorf("%w: %q", ErrBadCursor, q.Cursor)
		}
		rows, err = tx.QueryContext(ctx, checksOfTargetSQL, repo.ref.ID, oid, change.target, name, attempt, limit)
	case c.OID != "":
		target, name, attempt, ok := cutTargetCursor(q.Cursor)
		if !ok {
			return fmt.Errorf("%w: %q", ErrBadCursor, q.Cursor)
		}
		rows, err = tx.QueryContext(ctx, checksOfCommitSQL, repo.ref.ID, c.OID, target, name, attempt, limit)
	case q.Cursor == "":
		rows, err = tx.QueryContext(ctx, checksOfRepoSQL, repo.ref.ID, limit)
	default:
		at, oid, target, name, attempt, ok := cutEndedCursor(q.Cursor)
		if !ok {
			return fmt.Errorf("%w: %q", ErrBadCursor, q.Cursor)
		}
		rows, err = tx.QueryContext(ctx, checksOfRepoAfterSQL, repo.ref.ID, at, oid, target, name, attempt, limit)
	}
	if err != nil {
		return fmt.Errorf("code: list repository %s's checks: %w", repo.ref.Name, err)
	}
	defer func() { _ = rows.Close() }()
	scanned, err := scanChecks(rows)
	if err != nil {
		return fmt.Errorf("code: list repository %s's checks: %w", repo.ref.Name, err)
	}
	for i := range scanned {
		scanned[i].Repo = repo.ref.Name
	}
	r.pageChecks(scanned, c.keeps, limit, checksWalk(q), out)
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
		at, repo, oid, target, name, attempt, ok := cutWindowCursor(q.Cursor)
		if !ok {
			return fmt.Errorf("%w: %q", ErrBadCursor, q.Cursor)
		}
		rows, err = tx.QueryContext(ctx, checksWindowAfterSQL, since, at, repo, oid, target, name, attempt, limit)
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
	for i, row := range scanned {
		if repo, ok := listed[row.RepoID]; ok {
			scanned[i].Repo = repo.ref.Name
		}
	}
	r.pageChecks(scanned, func(row CheckRow) bool { _, ok := listed[row.RepoID]; return ok && c.keeps(row) },
		limit, checksWalk(q), out)
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

// pageChecks files the rows a listing READ into out — those keep keeps: the
// listing's narrowing ([CheckQuery.keeps]) and, on the window, the asker's
// grant — until the page's byte budget, and names where the
// next page resumes: after the last row it served where the budget ended it,
// and after the last row it READ where it read a whole page, so a page the
// grant kept little of still advances.
func (r *Reader) pageChecks(scanned []CheckRow, keep func(CheckRow) bool, limit int,
	cursor func(CheckRow) string, out *ReadAnswer) {

	pg := page{maxRows: limit, maxB: r.pageBytes}
	for _, row := range scanned {
		if !keep(row) {
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
	key := CheckKey{RepoID: repo.ref.ID, OID: q.Checks.OID, Target: q.Checks.Target, Name: q.Checks.Name,
		Attempt: q.Checks.Attempt}
	var (
		d                   CheckDetail
		def, failures, tail string
	)
	d.CheckRow, err = scanCheck(tx.QueryRowContext(ctx, checkWholeSQL, key.RepoID, key.OID, key.Target, key.Name,
		key.Attempt).Scan, &def, &failures, &tail)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return fmt.Errorf("%w: %s holds no attempt %d of check %s of %s for %s", ErrCheckNotFound,
			repo.ref.Name, key.Attempt, key.Name, key.OID, key.Target)
	case err != nil:
		return fmt.Errorf("code: read check %s of %s: %w", key.Name, key.OID, err)
	}
	d.Repo = repo.ref.Name
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
	switch err := tx.QueryRowContext(ctx, checkLogSQL, key.RepoID, key.OID, key.Target, key.Name,
		key.Attempt).Scan(&object,
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
// nudged into reading, to learn what it owes the checks of the changes it is
// home to. A LIVE change on an ACTIVE repository, its target a live branch,
// owes its home a DISPATCH of its head's checks where no dispatch of its head
// for its target names the checks file its target's row holds — a head never
// dispatched, a change retargeted since, a target whose checks file changed
// since (checksdispatch.go). The home reads the file at the target's tip,
// queues what it defines at the head for the target, and records the
// dispatch, which takes the head off the list by its own row whether the file
// defined many checks or none: the rows are the work list, and it converges
// on what the home decided, never on whether an attempt exists — a target with
// no checks file, every repository that has not adopted checks, has none for
// ever. A live change owes its home a REQUEUE too, where its head's latest
// attempt of a check for its target ended errored for its infrastructure
// below [AutoRequeueAttempts], until the next attempt's row is there. And a
// target branch of a live change owes its home a READING of its checks file
// again where its branch moved past the tip its row was read at and that
// reading was recorded at or before [ChecksDueQuery.RefreshBefore] — the
// home's clock less its refresh — so a target pushed to on every landing is
// read at the home's pace, and only a file that changed makes its heads due.
// And a repository owes its home the TIP WORK of each protected target holding
// a landing its tip checks have not judged (tipwork.go, ADR-0068).
//
// TWO SHAPES, as `changes_due`'s, and over the same index: with no repository
// it pages the repositories holding such a change or such a target, by id,
// walking code_changes_open_idx grouped on its leading column; with one it
// pages that repository's due heads by source branch, each with its target's
// tip and the position that moved it there — the tip whose checks file the
// home reads — and what it owes ([CheckWork], read in the same snapshot), and
// on its FIRST PAGE its targets due a reading ([ReadAnswer.DueTargets]),
// walking its targets' rows by their key, and its tip work
// ([ReadAnswer.DueTips]), walking code_landings_unjudged_idx. The listing of
// repositories is the union of the two listings, one keyset page of each
// after the cursor merged into one. Every change is tested by its
// dispatch's key and its target's, one seek of each, and by
// code_checks_requeue_idx, which holds only the attempts a requeue reads.
//
// READ STALE BY DEFAULT, as every duty listing is: a head listed a moment late
// is dispatched at the next pass, and one listed after its dispatch landed is
// answered from the rows by the writer, which records nothing twice.
func (r *Reader) dueCheckWork(ctx context.Context, tx *sql.Tx, q ReadQuery, out *ReadAnswer) error {
	refresh := store.EncodeTime(q.ChecksDue.RefreshBefore)
	if q.ChecksDue.RepoID == "" {
		if q.Cursor != "" && !canonicalUUID(q.Cursor) {
			return fmt.Errorf("%w: %q", ErrBadCursor, q.Cursor)
		}
		changes, err := repoIDsIn(ctx, tx, dueCheckReposSQL, q.Cursor, r.pageRows, refresh, AutoRequeueAttempts)
		if err != nil {
			return fmt.Errorf("code: list the repositories check work is due in: %w", err)
		}
		tips, err := repoIDsIn(ctx, tx, dueTipReposSQL, q.Cursor, r.pageRows)
		if err != nil {
			return fmt.Errorf("code: list the repositories tip work is due in: %w", err)
		}
		ids, next := mergeRepoPages(changes, tips, r.pageRows)
		pg := page{maxRows: r.pageRows, maxB: r.pageBytes}
		for _, id := range ids {
			out.DueRepos = append(out.DueRepos, id)
			if pg.add(stringBytes(id) + 1) {
				next = id
				break
			}
		}
		out.Next = next
		return nil
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
	required, err := requiredIn(ctx, tx, repoID)
	if err != nil {
		return err
	}
	pg := page{maxRows: r.pageRows, maxB: r.pageBytes}
	if q.Cursor == "" {
		// THE TARGETS DUE A READING, on the first page alone: at most one a
		// branch the repository's changes target, so bounded by its refs.
		if err := r.dueCheckTargets(ctx, tx, repoID, refresh, &pg, out); err != nil {
			return err
		}
		// ITS PROTECTED TARGETS' TIP WORK (tipwork.go), on the first page
		// alone too: one a target holding a landing its tip checks owe.
		if err := r.dueTips(ctx, tx, repoID, required, &pg, out); err != nil {
			return err
		}
	}
	heads, err := dueCheckHeadsIn(ctx, tx, repoID, q.Cursor, r.pageRows)
	if err != nil {
		return err
	}
	for _, h := range heads {
		work, err := checkWorkOf(ctx, tx, repoID, h, required)
		if err != nil {
			return err
		}
		c := h.due
		c.Checks = &work
		out.DueChanges = append(out.DueChanges, c)
		if pg.add(c.encodedBytes()) {
			out.Next = c.SourceRef
			break
		}
	}
	return nil
}

// dueCheckTargets files a repository's targets due a reading of their checks
// file into out — each a branch a live change targets, which moved past the
// tip its reading was recorded at, at or before refresh — within pg.
func (r *Reader) dueCheckTargets(ctx context.Context, tx *sql.Tx, repoID string, refresh int64, pg *page,
	out *ReadAnswer) error {

	rows, err := tx.QueryContext(ctx, dueCheckTargetsSQL, repoID, refresh, r.pageRows)
	if err != nil {
		return fmt.Errorf("code: list repository %s's targets due a reading: %w", repoID, err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var (
			d  CheckTargetDue
			at int64
		)
		if err := rows.Scan(&d.TargetRef, &d.Tip, &at); err != nil {
			return fmt.Errorf("code: list repository %s's targets due a reading: %w", repoID, err)
		}
		d.TipPosition = unpacked(at)
		out.DueTargets = append(out.DueTargets, d)
		if pg.add(d.encodedBytes()) {
			break
		}
	}
	return rows.Err()
}

// repoIDsIn is the repository ids query answers with args, in its order.
func repoIDsIn(ctx context.Context, tx *sql.Tx, query string, args ...any) ([]string, error) {
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// BacklogScan is the most overdue attempts one reading of the checks' backlog
// counts: 1 024. The reading walks the queued attempts IN THE ORDER THEY FELL
// OVERDUE (code_checks_overdue_idx: each its queued instant plus its own
// definition's timeout), so every row it reads is one past its own timeout
// and its count is exact up to the bound — never the oldest QUEUED, which are
// not the first overdue when timeouts differ: a thousand attempts of a
// fifty-minute check queued a minute before a one-minute lint filled a walk
// by queued instant with none of them overdue, and the lint stuck behind them
// was never read. Past the bound the reading says there are more
// ([ChecksBacklog.More]) rather than count them: a thousand attempts past
// their own timeouts is a fleet with no runner at all, which the alarm names
// either way. So a reading every AlarmInterval on every data node is one
// range of one partial index, a thousand entries at most, each read with its
// row for the instant it was queued and its repository's key for whether a
// runner may still claim it — and the entries of a deleted repository's
// attempts, until its purge removes them, are read and skipped the same way.
const BacklogScan = 1024

// ChecksBacklog is what a node's alarm reads of the queued checks
// ([Reader.ChecksBacklog]): how many queued attempts have waited longer than
// their own timeout — [BacklogScan] of them at most, with More where the rows
// hold more than it — the longest wait among those it counted, and that
// attempt, named for an operator.
type ChecksBacklog struct {
	Behind int
	More   bool
	Oldest time.Duration
	Check  string
}

// ChecksBacklog reads the queued attempts this node's rows hold that have
// waited longer than their OWN timeout — the timeout_seconds of the
// definition each was queued under — by now, in the order they fell overdue
// and at most [BacklogScan] of them: the threshold of the code_checks_backlog
// alarm is every check's own (ADR-0015), never one of the alarm's. An attempt
// queued with no definition — one a person's CI reports, which no runner of
// the engine claims — is never overdue.
//
// AN ATTEMPT NO RUNNER CAN CLAIM IS NOBODY'S BACKLOG: one of a repository that
// is deleted, being purged or gone is not counted. Every write of such a
// repository's checks is refused repo_deleted — a runner's claim and the
// home's cancel alike — so its queued attempts stay queued until the purge
// removes them, up to code.deleted_retention_days later; counted, they raised
// the alarm on every data node for that long, its remedy sending an operator
// to git nodes that were healthy. Each is tested by its repository's key as
// the walk reads it, so it takes no place in the count.
//
// A STALE read of this node's rows, as every alarm's reading is: the alarm
// says what this node holds.
func (r *Reader) ChecksBacklog(ctx context.Context, now time.Time) (ChecksBacklog, error) {
	query := statelog.Freshness{Level: statelog.ReadStale}.Query(
		statelog.ScopeSet{Paths: []string{DomainPath()}}, true)
	var out ChecksBacklog
	_, err := r.log.Read(ctx, query, func(tx *sql.Tx) error {
		var err error
		out, err = checksBacklogIn(ctx, tx, now)
		return err
	})
	if err != nil {
		return ChecksBacklog{}, fmt.Errorf("code: read the queued checks' backlog: %w", err)
	}
	return out, nil
}

// checksBacklogIn is [Reader.ChecksBacklog]'s reading in tx: one more row
// than [BacklogScan] read, so a full count tells a backlog of exactly the
// bound from one past it.
func checksBacklogIn(ctx context.Context, tx *sql.Tx, now time.Time) (ChecksBacklog, error) {
	rows, err := tx.QueryContext(ctx, checksBacklogSQL, store.EncodeTime(now), BacklogScan+1)
	if err != nil {
		return ChecksBacklog{}, err
	}
	defer func() { _ = rows.Close() }()
	var (
		out    ChecksBacklog
		oldest CheckKey
	)
	for rows.Next() {
		if out.Behind == BacklogScan {
			out.More = true
			break
		}
		var (
			key      CheckKey
			queuedAt int64
		)
		if err := rows.Scan(&key.RepoID, &key.OID, &key.Target, &key.Name, &key.Attempt, &queuedAt); err != nil {
			return ChecksBacklog{}, err
		}
		out.Behind++
		if waited := now.Sub(store.DecodeTime(queuedAt)); waited > out.Oldest {
			out.Oldest, oldest = waited, key
		}
	}
	if err := rows.Err(); err != nil || out.Behind == 0 {
		return out, err
	}
	name := oldest.RepoID
	switch err := tx.QueryRowContext(ctx, repoNameSQL, oldest.RepoID).Scan(&name); {
	case errors.Is(err, sql.ErrNoRows):
	case err != nil:
		return ChecksBacklog{}, err
	}
	commit := oldest.OID
	if len(commit) > 12 {
		commit = commit[:12]
	}
	// THE BRANCH IT IS CHECKED FOR is part of the attempt's name, as of its
	// key: one commit checked for two branches is two attempts.
	out.Check = fmt.Sprintf("%s@%s for %s: %s (attempt %d)", name, commit,
		strings.TrimPrefix(oldest.Target, "refs/heads/"), oldest.Name, oldest.Attempt)
	return out, nil
}

// THE CURSORS, each a row's place in its listing's order, "/"-separated — no
// part of one holds a "/": a commit is hex, a check's name is
// [config.ValidCodeCheckName]'s alphabet, a repository's id a UUID, an instant
// digits, and a target branch — a ref, which holds "/" — is written in
// unpadded base64url ([cursorTarget]), whose alphabet holds none.

// cursorTarget spells a target branch in a cursor.
var cursorTarget = base64.RawURLEncoding

// checksWalk is how a `checks` listing q spells the cursor that resumes it
// after a row it served, by the walk its question takes: the window's where it
// names no repository ([windowCursor]); one change's attempts at its head, by
// name and attempt ([nameAttemptCursor]); one commit's for every target
// ([targetCursor]); and a repository's, newest ended first ([endedCursor]).
// THE ONE CHOICE: the reader pages by it and [ReadQuery.ChecksCursor] answers
// from it, so a page's own cursor and one a caller asks for after any of its
// rows are spelled alike.
func checksWalk(q ReadQuery) func(CheckRow) string {
	c := CheckQuery{}
	if q.Checks != nil {
		c = *q.Checks
	}
	switch {
	case q.Repo == "":
		return windowCursor
	case c.Change != 0:
		return nameAttemptCursor
	case c.OID != "":
		return targetCursor
	}
	return endedCursor
}

// ChecksCursor is the cursor that resumes q — a `checks` listing — after row,
// a row a page of it served: what a caller that keeps fewer of a page's rows
// than it was served hands back as the next page's cursor. It is spelled by
// the walk q's question takes ([checksWalk]), as the page's own cursor is, so
// a caller never learns how a walk spells one.
func (q ReadQuery) ChecksCursor(row CheckRow) string { return checksWalk(q)(row) }

// nameAttemptCursor is a row's place among one commit's attempts for one
// target.
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

// targetCursor is a row's place among one commit's attempts for every target:
// "<target>/<name>/<attempt>".
func targetCursor(row CheckRow) string {
	return cursorTarget.EncodeToString([]byte(row.Target)) + "/" + nameAttemptCursor(row)
}

// cutTargetCursor reads a [targetCursor]; "" is a first page's.
func cutTargetCursor(s string) (string, string, int, bool) {
	if s == "" {
		return "", "", 0, true
	}
	head, rest, ok := strings.Cut(s, "/")
	target, err := cursorTarget.DecodeString(head)
	if !ok || err != nil || !ValidRef(string(target)) {
		return "", "", 0, false
	}
	name, attempt, ok := cutNameAttempt(rest)
	return string(target), name, attempt, ok && rest != ""
}

// endedCursor is a row's place in a repository's listing, newest ended first:
// "<finished_at>/<commit>/<target>/<name>/<attempt>", an attempt not yet ended
// at 0.
func endedCursor(row CheckRow) string {
	at := int64(0)
	if !row.FinishedAt.IsZero() {
		at = store.EncodeTime(row.FinishedAt)
	}
	return strconv.FormatInt(at, 10) + "/" + row.OID + "/" + targetCursor(row)
}

// cutEndedCursor reads an [endedCursor].
func cutEndedCursor(s string) (int64, string, string, string, int, bool) {
	head, rest, ok := strings.Cut(s, "/")
	at, err := strconv.ParseInt(head, 10, 64)
	if !ok || err != nil || at < 0 {
		return 0, "", "", "", 0, false
	}
	oid, rest, ok := strings.Cut(rest, "/")
	if !ok || !gitref.ValidOID(oid) {
		return 0, "", "", "", 0, false
	}
	target, name, attempt, ok := cutTargetCursor(rest)
	return at, oid, target, name, attempt, ok && rest != ""
}

// windowCursor is a row's place in the window, newest ended first:
// "<finished_at>/<repo>/<commit>/<target>/<name>/<attempt>".
func windowCursor(row CheckRow) string {
	return strconv.FormatInt(store.EncodeTime(row.FinishedAt), 10) + "/" + row.RepoID + "/" + row.OID + "/" +
		targetCursor(row)
}

// cutWindowCursor reads a [windowCursor].
func cutWindowCursor(s string) (int64, string, string, string, string, int, bool) {
	head, rest, ok := strings.Cut(s, "/")
	at, err := strconv.ParseInt(head, 10, 64)
	if !ok || err != nil || at < 1 || at == math.MaxInt64 {
		return 0, "", "", "", "", 0, false
	}
	repo, rest, ok := strings.Cut(rest, "/")
	if !ok || !canonicalUUID(repo) {
		return 0, "", "", "", "", 0, false
	}
	oid, rest, ok := strings.Cut(rest, "/")
	if !ok || !gitref.ValidOID(oid) {
		return 0, "", "", "", "", 0, false
	}
	target, name, attempt, ok := cutTargetCursor(rest)
	return at, repo, oid, target, name, attempt, ok && rest != ""
}

// The reads of a repository's checks, each registered in plan_test.go, so the
// pinned driver's planner is held to the key each is written for: one attempt
// by code_checks' key, whole or as a listing reads it; the latest attempt of
// one check of one commit for one target, the key read backwards from its
// (repository, commit, target, name) prefix; one commit's attempts for every
// target after a (target, name, attempt) on the key, or for one target after
// a (name, attempt) — what a change's attempts are; a repository's newest
// ended first, from the top or after a place, on code_checks_finished_idx read
// backwards; and the window's, on code_checks_recent_idx read backwards above
// its moment. Each keyset's >= beside its OR is what keeps the index's range
// seek (planner.go).
const (
	checkRowSQL = `SELECT ` + checkColumns + ` FROM code_checks INDEXED BY ` + checksKey + `
		WHERE repo_id = ? AND commit_oid = ? AND target_ref = ? AND name = ? AND attempt = ?`
	checkWholeSQL = `SELECT ` + checkColumns + `, def, failures, tail FROM code_checks INDEXED BY ` + checksKey + `
		WHERE repo_id = ? AND commit_oid = ? AND target_ref = ? AND name = ? AND attempt = ?`
	checkLogSQL = `SELECT object, hash, size FROM code_check_logs
		WHERE repo_id = ? AND commit_oid = ? AND target_ref = ? AND name = ? AND attempt = ?`
	latestCheckSQL = `SELECT ` + checkColumns + ` FROM code_checks INDEXED BY ` + checksKey + `
		WHERE repo_id = ? AND commit_oid = ? AND target_ref = ? AND name = ? ORDER BY attempt DESC LIMIT 1`
	checksOfCommitSQL = `SELECT ` + checkColumns + ` FROM code_checks INDEXED BY ` + checksKey + `
		WHERE repo_id = ?1 AND commit_oid = ?2 AND target_ref >= ?3
			AND (target_ref > ?3 OR name > ?4 OR (name = ?4 AND attempt > ?5))
		ORDER BY target_ref, name, attempt LIMIT ?6`
	checksOfTargetSQL = `SELECT ` + checkColumns + ` FROM code_checks INDEXED BY ` + checksKey + `
		WHERE repo_id = ?1 AND commit_oid = ?2 AND target_ref = ?3 AND name >= ?4 AND (name > ?4 OR attempt > ?5)
		ORDER BY name, attempt LIMIT ?6`
	checksOfRepoSQL = `SELECT ` + checkColumns + ` FROM code_checks INDEXED BY ` + checksEndedKey + `
		WHERE repo_id = ?1
		ORDER BY finished_at DESC, commit_oid DESC, target_ref DESC, name DESC, attempt DESC LIMIT ?2`
	checksOfRepoAfterSQL = `SELECT ` + checkColumns + ` FROM code_checks INDEXED BY ` + checksEndedKey + `
		WHERE repo_id = ?1 AND finished_at <= ?2
			AND (finished_at < ?2 OR commit_oid < ?3 OR (commit_oid = ?3 AND (target_ref < ?4
				OR (target_ref = ?4 AND (name < ?5 OR (name = ?5 AND attempt < ?6))))))
		ORDER BY finished_at DESC, commit_oid DESC, target_ref DESC, name DESC, attempt DESC LIMIT ?7`
	checksWindowSQL = `SELECT ` + checkColumns + ` FROM code_checks INDEXED BY code_checks_recent_idx
		WHERE finished_at > 0 AND finished_at >= ?1
		ORDER BY finished_at DESC, repo_id DESC, commit_oid DESC, target_ref DESC, name DESC, attempt DESC
		LIMIT ?2`
	checksWindowAfterSQL = `SELECT ` + checkColumns + ` FROM code_checks INDEXED BY code_checks_recent_idx
		WHERE finished_at > 0 AND finished_at >= ?1 AND finished_at <= ?2
			AND (finished_at < ?2 OR repo_id < ?3 OR (repo_id = ?3 AND (commit_oid < ?4
				OR (commit_oid = ?4 AND (target_ref < ?5 OR (target_ref = ?5 AND (name < ?6
					OR (name = ?6 AND attempt < ?7))))))))
		ORDER BY finished_at DESC, repo_id DESC, commit_oid DESC, target_ref DESC, name DESC, attempt DESC
		LIMIT ?8`
	// THE BACKLOG'S WALK: the queued attempts overdue by ?1, in the order
	// they fell overdue, on code_checks_overdue_idx — its partial predicate
	// repeated verbatim, and its range what the seek reads; each kept only
	// where its repository is one a runner may still claim it in (active or
	// archived), tested by the repository's key.
	checksBacklogSQL = `SELECT k.repo_id, k.commit_oid, k.target_ref, k.name, k.attempt, k.queued_at
		FROM code_checks AS k INDEXED BY code_checks_overdue_idx
		WHERE k.state = 'queued' AND k.overdue_at > 0 AND k.overdue_at < ?1
			AND EXISTS (SELECT 1 FROM code_repos AS r WHERE r.id = k.repo_id
				AND r.status IN ('active', 'archived'))
		ORDER BY k.overdue_at, k.repo_id, k.commit_oid, k.target_ref, k.name, k.attempt LIMIT ?2`
	// THE CLAIM WALK: the queued attempts naming one cell (?1), oldest
	// queued first after a place, on code_checks_queued_idx — its partial
	// predicate repeated verbatim, the cell its leading column's equality,
	// and the >= beside the keyset's OR what keeps its range seek — each with
	// its definition and its repository's name, and kept only where its
	// repository is one a runner may still claim it in, by the repository's
	// key. A first page is the place before every attempt: an instant of 0,
	// which no queued attempt has.
	queuedChecksSQL = `SELECT ` + checkColumns + `, def,
			(SELECT r.name FROM code_repos AS r WHERE r.id = code_checks.repo_id)
		FROM code_checks INDEXED BY code_checks_queued_idx
		WHERE state = 'queued' AND run_in = ?1 AND queued_at >= ?2
			AND (queued_at > ?2 OR repo_id > ?3 OR (repo_id = ?3 AND (commit_oid > ?4
				OR (commit_oid = ?4 AND (target_ref > ?5 OR (target_ref = ?5 AND (name > ?6
					OR (name = ?6 AND attempt > ?7))))))))
			AND EXISTS (SELECT 1 FROM code_repos AS r WHERE r.id = code_checks.repo_id
				AND r.status IN ('active', 'archived'))
		ORDER BY queued_at, repo_id, commit_oid, target_ref, name, attempt LIMIT ?8`
	// THE HOME'S SWEEP: the repositories holding a running attempt after a
	// place, code_checks_running_idx's leading column grouped — the index
	// holds only what runs now — and one repository's running attempts by
	// its key's prefix of the same index, its partial predicate repeated
	// verbatim.
	runningReposSQL = `SELECT repo_id FROM code_checks INDEXED BY code_checks_running_idx
		WHERE state = 'running' AND repo_id > ?1 GROUP BY repo_id ORDER BY repo_id LIMIT ?2`
	runningChecksSQL = `SELECT ` + checkColumns + ` FROM code_checks INDEXED BY code_checks_running_idx
		WHERE repo_id = ?1 AND state = 'running' LIMIT ?2`
	// THE HOME'S LISTING, changes_due's two statements with the facts' test
	// replaced by the checks' work: a head is due while no dispatch of it for
	// its target names the checks file its target's row holds — one seek of
	// each key — or while its latest attempt of a check for its target is
	// one its infrastructure ended below ?4 (AutoRequeueAttempts) and no next
	// attempt follows it — code_checks_requeue_idx, then code_checks' key —
	// and, in the listing of repositories, a target due a reading beside it,
	// by its row's key, where its branch moved past the tip it was read at
	// and the reading is no later than ?3. A repository's page answers both
	// tests of each change it lists, which say what its home owes it. The
	// targets due a reading of one repository walk its targets' rows by their
	// key, each tested against its branch and a live change into it, which
	// code_changes_target_idx holds the live changes of by target.
	headUndispatched = `NOT EXISTS (SELECT 1 FROM code_check_dispatches AS d INDEXED BY ` +
		checkDispatchesKey + `
		CROSS JOIN code_check_targets AS g INDEXED BY ` + checkTargetsKey + `
		WHERE d.repo_id = c.repo_id AND d.commit_oid = c.head_oid AND d.target_ref = c.target_ref
			AND g.repo_id = c.repo_id AND g.target_ref = c.target_ref AND g.defs_oid = d.defs_oid)`
	headRequeueDue = `EXISTS (SELECT 1 FROM code_checks AS k INDEXED BY code_checks_requeue_idx
		WHERE k.repo_id = c.repo_id AND k.commit_oid = c.head_oid AND k.target_ref = c.target_ref
			AND ` + requeuedRows + ` AND k.attempt < ?4
			AND NOT EXISTS (SELECT 1 FROM code_checks AS n INDEXED BY ` + checksKey + `
				WHERE n.repo_id = k.repo_id AND n.commit_oid = k.commit_oid AND n.target_ref = k.target_ref
					AND n.name = k.name AND n.attempt = k.attempt + 1))`
	dueCheckReposSQL = `SELECT c.repo_id FROM code_changes AS c INDEXED BY code_changes_open_idx
		CROSS JOIN code_refs AS t INDEXED BY ` + refsKey + `
		WHERE c.repo_id > ?1 AND ` + liveChanges + `
			AND t.repo_id = c.repo_id AND t.ref = c.target_ref AND t.target != ''
			AND (` + headUndispatched + ` OR ` + headRequeueDue + `
				OR EXISTS (SELECT 1 FROM code_check_targets AS g INDEXED BY ` + checkTargetsKey + `
					WHERE g.repo_id = c.repo_id AND g.target_ref = c.target_ref AND g.tip_position < t.position
						AND g.at <= ?3))
			AND EXISTS (SELECT 1 FROM code_repos AS r WHERE r.id = c.repo_id AND r.status = 'active')
		GROUP BY c.repo_id ORDER BY c.repo_id LIMIT ?2`
	dueCheckChangesSQL = `SELECT c.id, c.number, c.revision, c.head_oid, c.source_ref, c.target_ref, t.target,
			t.position, ` + headUndispatched + `, ` + headRequeueDue + `
		FROM code_changes AS c INDEXED BY code_changes_open_idx
		CROSS JOIN code_refs AS t INDEXED BY ` + refsKey + `
		WHERE c.repo_id = ?1 AND c.source_ref > ?2 AND ` + liveChanges + `
			AND t.repo_id = c.repo_id AND t.ref = c.target_ref AND t.target != ''
			AND (` + headUndispatched + ` OR ` + headRequeueDue + `)
		ORDER BY c.source_ref LIMIT ?3`
	dueCheckTargetsSQL = `SELECT g.target_ref, t.target, t.position
		FROM code_check_targets AS g INDEXED BY ` + checkTargetsKey + `
		CROSS JOIN code_refs AS t INDEXED BY ` + refsKey + `
		WHERE g.repo_id = ?1 AND g.at <= ?2
			AND t.repo_id = g.repo_id AND t.ref = g.target_ref AND t.target != '' AND t.position > g.tip_position
			AND EXISTS (SELECT 1 FROM code_changes AS c INDEXED BY code_changes_target_idx
				WHERE c.repo_id = g.repo_id AND c.target_ref = g.target_ref AND ` + liveChanges + `)
		ORDER BY g.target_ref LIMIT ?3`
)

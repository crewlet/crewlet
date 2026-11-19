package advisordomain

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/store"
)

// Reader answers from this node's applied rows, through the domain's read
// authority, at each caller's level.
type Reader struct {
	db  store.ReplicatedReader
	log *statelog.Reader
}

// ReaderOptions configure a reader.
type ReaderOptions struct {
	DB store.ReplicatedReader

	// Log is this domain's read authority, required: without it a read level
	// is a label, not a guarantee.
	Log *statelog.Reader
}

// NewReader builds the advisor read side.
func NewReader(opts ReaderOptions) (*Reader, error) {
	switch {
	case opts.DB.IsZero():
		return nil, errors.New("advisordomain: a reader needs the replicated estate")
	case opts.Log == nil:
		return nil, errors.New("advisordomain: a reader needs its domain's read authority")
	}
	return &Reader{db: opts.DB, log: opts.Log}, nil
}

// DefaultRead is the level a person's read of the advisor takes: their own
// writes, wherever they made them.
func DefaultRead() statelog.Freshness { return statelog.Freshness{Level: statelog.ReadSession} }

// ---- findings ----------------------------------------------------------- //

// FindingRow is one episode as the log keeps it. An instant that was never
// set is the zero time; KeyVersion is the key's version, a write's ifMatch.
type FindingRow struct {
	ID, Key, KeyID, Class string
	Source                Source
	Actor                 Actor
	Area                  string
	Severity              Severity
	SubjectKind           string
	SubjectID             string
	State                 State
	CloseReason           CloseReason

	Revision                     int
	VerdictDigest, FixDigest     string
	ImpactMinutes, ImpactTokens  *int64
	OpenedAt, RevisedAt, FactsAt time.Time

	ClosedAt, SnoozedUntil, EffectDueAt, AppliedAt, IneffectiveAt, UsefulAt time.Time
	AppliedFix, AppliedBy, IneffectiveFix, UsefulBy                         string

	// SeenBefore is how many episodes of the key opened before this one.
	SeenBefore int

	// Explanation is the model's words for the verdict ExplainedDigest names:
	// shown only while that is the row's VerdictDigest.
	Explanation, ExplainedDigest string

	Body       json.RawMessage
	KeyVersion uint64
}

// findingColumns are a [FindingRow]'s columns, in [scanFinding]'s order.
const findingColumns = `f.id, f.key, f.key_id, f.class, f.source, f.actor, f.area, f.severity,
	f.subject_kind, f.subject_id, f.state, f.close_reason, f.revision, f.verdict_digest, f.fix_digest,
	f.impact_minutes, f.impact_tokens, f.opened_at, f.revised_at, f.facts_at, f.closed_at,
	f.snoozed_until, f.effect_due_at, f.applied_at, f.ineffective_at, f.useful_at, f.applied_fix,
	f.applied_by, f.ineffective_fix, f.useful_by, f.seen_before, f.explanation, f.explained_digest,
	f.body, COALESCE(k.version, 0)
	FROM advisor_findings f LEFT JOIN advisor_keys k ON k.key_id = f.key_id`

// scanFinding reads one row of [findingColumns].
func scanFinding(rows interface{ Scan(...any) error }) (FindingRow, error) {
	var (
		f                                            FindingRow
		source, actor, severity, state, reason, body string
		minutes, tokens                              sql.NullInt64
		opened, revised, facts, closed, snoozed, due int64
		applied, ineffective, useful, version        int64
	)
	if err := rows.Scan(&f.ID, &f.Key, &f.KeyID, &f.Class, &source, &actor, &f.Area, &severity,
		&f.SubjectKind, &f.SubjectID, &state, &reason, &f.Revision, &f.VerdictDigest, &f.FixDigest,
		&minutes, &tokens, &opened, &revised, &facts, &closed, &snoozed, &due, &applied,
		&ineffective, &useful, &f.AppliedFix, &f.AppliedBy, &f.IneffectiveFix, &f.UsefulBy,
		&f.SeenBefore, &f.Explanation, &f.ExplainedDigest, &body, &version); err != nil {
		return FindingRow{}, err
	}
	f.Source, f.Actor, f.Severity = Source(source), Actor(actor), Severity(severity)
	f.State, f.CloseReason = State(state), CloseReason(reason)
	if minutes.Valid {
		f.ImpactMinutes = &minutes.Int64
	}
	if tokens.Valid {
		f.ImpactTokens = &tokens.Int64
	}
	f.OpenedAt, f.RevisedAt, f.FactsAt = store.DecodeTime(opened), store.DecodeTime(revised), store.DecodeTime(facts)
	f.ClosedAt, f.SnoozedUntil, f.EffectDueAt = decodeOptional(closed), decodeOptional(snoozed), decodeOptional(due)
	f.AppliedAt, f.IneffectiveAt, f.UsefulAt = decodeOptional(applied), decodeOptional(ineffective),
		decodeOptional(useful)
	f.Body, f.KeyVersion = json.RawMessage(body), uint64(version)
	return f, nil
}

// scanFindings reads every row of a [findingColumns] query, and closes them.
func scanFindings(rows *sql.Rows) ([]FindingRow, error) {
	var out []FindingRow
	for rows.Next() {
		f, err := scanFinding(rows)
		if err != nil {
			_ = rows.Close()
			return nil, err
		}
		out = append(out, f)
	}
	return out, errors.Join(rows.Err(), rows.Close())
}

// Filter narrows a page of findings: the non-closed ones (Open), the closed
// ones (Closed), both or neither for every one, and by severity, area and
// class where set.
type Filter struct {
	Open, Closed bool
	Severity     Severity
	Area, Class  string

	// Cursor is where the page continues, empty for the newest; Limit how
	// many rows it holds, 1 to [MaxPageSize], DefaultPageSize where zero.
	Cursor string
	Limit  int
}

// cursorSeparator parts a cursor's instant and id; neither holds it.
const cursorSeparator = "~"

// findingCursor is where a page continues below f.
func findingCursor(f FindingRow) string {
	return strconv.FormatInt(store.EncodeTime(f.OpenedAt), 10) + cursorSeparator + f.ID
}

// parseFindingCursor reads a cursor back.
func parseFindingCursor(cursor string) (int64, string, error) {
	at, id, ok := strings.Cut(cursor, cursorSeparator)
	opened, err := strconv.ParseInt(at, 10, 64)
	if !ok || err != nil || !IsUUID(id) {
		return 0, "", invalid("cursor %q is not one a page of findings answered", cursor)
	}
	return opened, id, nil
}

// limitOf is a page's limit, refused outside 1..MaxPageSize.
func limitOf(limit int) (int, error) {
	switch {
	case limit == 0:
		return DefaultPageSize, nil
	case limit < 0 || limit > MaxPageSize:
		return 0, invalid("limit is 1 to %d, and %d is not", MaxPageSize, limit)
	}
	return limit, nil
}

// Findings reads one page of episodes, newest first by when they opened, and
// the cursor of the page after it, empty on the last.
func (r *Reader) Findings(ctx context.Context, f Filter, fresh statelog.Freshness) ([]FindingRow, string, error) {
	limit, err := limitOf(f.Limit)
	if err != nil {
		return nil, "", err
	}
	if f.Severity != "" && !f.Severity.Valid() {
		return nil, "", invalid("severity %q is not one of blocks, costs or note", f.Severity)
	}
	where, args := []string{"1 = 1"}, []any{}
	switch {
	case f.Open && !f.Closed:
		where = append(where, "f.closed_at = 0")
	case f.Closed && !f.Open:
		where = append(where, "f.closed_at != 0")
	}
	for _, eq := range []struct{ column, value string }{
		{"f.severity", string(f.Severity)}, {"f.area", f.Area}, {"f.class", f.Class},
	} {
		if eq.value != "" {
			where, args = append(where, eq.column+" = ?"), append(args, eq.value)
		}
	}
	if f.Cursor != "" {
		opened, id, cursorErr := parseFindingCursor(f.Cursor)
		if cursorErr != nil {
			return nil, "", cursorErr
		}
		where = append(where, "(f.opened_at < ? OR (f.opened_at = ? AND f.id < ?))")
		args = append(args, opened, opened, id)
	}
	var out []FindingRow
	_, err = r.log.Read(ctx, fresh.Query(DomainScope(), true), func(tx *sql.Tx) error {
		rows, queryErr := tx.QueryContext(ctx, `SELECT `+findingColumns+` WHERE `+strings.Join(where, " AND ")+`
			ORDER BY f.opened_at DESC, f.id DESC LIMIT ?`, append(args, limit+1)...)
		if queryErr != nil {
			return queryErr
		}
		var scanErr error
		out, scanErr = scanFindings(rows)
		return scanErr
	})
	if err != nil {
		return nil, "", fmt.Errorf("advisordomain: read a page of findings: %w", err)
	}
	if len(out) <= limit {
		return out, "", nil
	}
	return out[:limit], findingCursor(out[limit-1]), nil
}

// NonClosed reads every open and applied episode, for the duty, refusing past
// [MaxNonClosed] rather than answering a slice of them.
func (r *Reader) NonClosed(ctx context.Context, fresh statelog.Freshness) ([]FindingRow, error) {
	var out []FindingRow
	_, err := r.log.Read(ctx, fresh.Query(DomainScope(), true), func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, `SELECT `+findingColumns+` WHERE f.closed_at = 0
			ORDER BY f.opened_at, f.id LIMIT ?`, MaxNonClosed+1)
		if err != nil {
			return err
		}
		if out, err = scanFindings(rows); err != nil {
			return err
		}
		if len(out) > MaxNonClosed {
			return fmt.Errorf("advisordomain: more than %d findings are open or applied, past what one "+
				"read answers", MaxNonClosed)
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("advisordomain: read the non-closed findings: %w", err)
	}
	return out, nil
}

// HistoryRow is one line of an episode's history: when, what, who (the
// engine's actor or a person's), and the facts that moved.
type HistoryRow struct {
	At     time.Time
	Event  Event
	ByKind iam.ActorKind
	ByName string
	Moved  json.RawMessage
}

// Finding reads one episode by its id, with its history oldest first, or
// [ErrNotFound].
func (r *Reader) Finding(ctx context.Context, id string, fresh statelog.Freshness) (FindingRow, []HistoryRow, error) {
	if !IsUUID(id) {
		return FindingRow{}, nil, invalid("%q is not an episode id", id)
	}
	var (
		out     FindingRow
		lines   []HistoryRow
		missing bool
	)
	resolve := func(ctx context.Context, tx *sql.Tx) (statelog.ScopeSet, error) {
		var keyID string
		err := tx.QueryRowContext(ctx, `SELECT key_id FROM advisor_findings WHERE id = ?`, id).Scan(&keyID)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			return DomainScope(), nil
		case err != nil:
			return statelog.ScopeSet{}, err
		}
		return statelog.ScopeSet{Paths: []string{ScopeTerm{Kind: TermFinding, ID: keyID}.Path()}}, nil
	}
	_, err := r.log.Read(ctx, fresh.Resolved(resolve, false), func(tx *sql.Tx) error {
		row, err := scanFinding(tx.QueryRowContext(ctx, `SELECT `+findingColumns+` WHERE f.id = ?`, id))
		switch {
		case errors.Is(err, sql.ErrNoRows):
			missing = true
			return nil
		case err != nil:
			return err
		}
		out, missing = row, false
		lines, err = historyOf(ctx, tx, id)
		return err
	})
	if err != nil {
		return FindingRow{}, nil, fmt.Errorf("advisordomain: read finding %s: %w", id, err)
	}
	if missing {
		return FindingRow{}, nil, fmt.Errorf("%w: the log holds no finding %s", ErrNotFound, id)
	}
	return out, lines, nil
}

// historyOf is one episode's history, oldest first.
func historyOf(ctx context.Context, tx *sql.Tx, id string) ([]HistoryRow, error) {
	rows, err := tx.QueryContext(ctx, `
		SELECT at, event, by_kind, by_name, moved FROM advisor_finding_history
		WHERE finding_id = ? ORDER BY position`, id)
	if err != nil {
		return nil, err
	}
	var out []HistoryRow
	for rows.Next() {
		var (
			h                  HistoryRow
			at                 int64
			event, kind, moved string
		)
		if err := rows.Scan(&at, &event, &kind, &h.ByName, &moved); err != nil {
			_ = rows.Close()
			return nil, err
		}
		h.At, h.Event, h.ByKind, h.Moved = store.DecodeTime(at), Event(event), iam.ActorKind(kind),
			json.RawMessage(moved)
		out = append(out, h)
	}
	return out, errors.Join(rows.Err(), rows.Close())
}

// Episodes reads every kept episode of a key, newest first.
func (r *Reader) Episodes(ctx context.Context, key string, fresh statelog.Freshness) ([]FindingRow, error) {
	if err := validKey(key); err != nil {
		return nil, err
	}
	var out []FindingRow
	_, err := r.log.Read(ctx, fresh.Query(KeyScope(key), true), func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, `SELECT `+findingColumns+` WHERE f.key_id = ?
			ORDER BY f.opened_at DESC, f.id DESC LIMIT ?`, KeyID(key), MaxEpisodes)
		if err != nil {
			return err
		}
		out, err = scanFindings(rows)
		return err
	})
	if err != nil {
		return nil, fmt.Errorf("advisordomain: read the episodes of %s: %w", key, err)
	}
	return out, nil
}

// KeyRow is one finding key: its version (a write's ifMatch), how many
// episodes it opened, its non-closed one, its hold and the fixes that did not
// take.
type KeyRow struct {
	KeyID, Key string
	Version    uint64
	Episodes   int
	Current    string

	// HoldUntil is when the hold lapses, zero for none or for one that holds
	// for good (HoldForGood); HoldEpisode is the episode whose dismissal set
	// it.
	HoldUntil               time.Time
	HoldForGood             bool
	HoldReason, HoldEpisode string
	HoldSeverity            Severity
	Ineffective             []string
}

// Held reports whether the key holds a hold, lapsed or not.
func (k KeyRow) Held() bool { return k.HoldForGood || !k.HoldUntil.IsZero() }

// Key reads one key, and whether the log holds it.
func (r *Reader) Key(ctx context.Context, key string, fresh statelog.Freshness) (KeyRow, bool, error) {
	if err := validKey(key); err != nil {
		return KeyRow{}, false, err
	}
	var (
		out   KeyRow
		found bool
	)
	_, err := r.log.Read(ctx, fresh.Query(KeyScope(key), false), func(tx *sql.Tx) error {
		var (
			current, reason, severity, episode sql.NullString
			hold                               sql.NullInt64
			version                            int64
			ineffective                        string
		)
		err := tx.QueryRowContext(ctx, `
			SELECT key, version, episodes, current_episode, hold_until, hold_reason, hold_severity,
				hold_episode, ineffective
			FROM advisor_keys WHERE key_id = ?`, KeyID(key)).Scan(&out.Key, &version, &out.Episodes,
			&current, &hold, &reason, &severity, &episode, &ineffective)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			out, found = KeyRow{}, false
			return nil
		case err != nil:
			return err
		}
		out.KeyID, out.Version, out.Current = KeyID(key), uint64(version), current.String
		out.HoldReason, out.HoldSeverity, out.HoldEpisode = reason.String, Severity(severity.String), episode.String
		if hold.Valid {
			out.HoldForGood = hold.Int64 == 0
			out.HoldUntil = decodeOptional(hold.Int64)
		}
		found = true
		return json.Unmarshal([]byte(ineffective), &out.Ineffective)
	})
	if err != nil {
		return KeyRow{}, false, fmt.Errorf("advisordomain: read key %s: %w", key, err)
	}
	return out, found, nil
}

// Counts reads how many episodes are open or applied, by severity.
func (r *Reader) Counts(ctx context.Context, fresh statelog.Freshness) (map[Severity]int, error) {
	out := map[Severity]int{}
	_, err := r.log.Read(ctx, fresh.Query(DomainScope(), true), func(tx *sql.Tx) error {
		clear(out)
		rows, err := tx.QueryContext(ctx, `
			SELECT severity, COUNT(*) FROM advisor_findings WHERE closed_at = 0 GROUP BY severity`)
		if err != nil {
			return err
		}
		for rows.Next() {
			var (
				severity string
				n        int
			)
			if err := rows.Scan(&severity, &n); err != nil {
				_ = rows.Close()
				return err
			}
			out[Severity(severity)] = n
		}
		return errors.Join(rows.Err(), rows.Close())
	})
	if err != nil {
		return nil, fmt.Errorf("advisordomain: count the open findings: %w", err)
	}
	return out, nil
}

// ---- runs --------------------------------------------------------------- //

// RunRow is one run: requested by a person, recorded when it ended, or both.
// Status is empty while it is only requested.
type RunRow struct {
	ID      string
	Trigger Trigger
	By      string

	RequestedAt, StartedAt, EndedAt time.Time
	Status                          RunStatus

	Classes json.RawMessage
	Model   json.RawMessage
}

// runColumns are a [RunRow]'s columns, in [scanRun]'s order.
const runColumns = `run_id, trigger_kind, by_name, requested_at, started_at, ended_at, status, classes, model
	FROM advisor_runs`

// scanRun reads one row of [runColumns].
func scanRun(rows interface{ Scan(...any) error }) (RunRow, error) {
	var (
		r                               RunRow
		trigger, status, classes, model string
		requested, started, ended       int64
	)
	if err := rows.Scan(&r.ID, &trigger, &r.By, &requested, &started, &ended, &status, &classes,
		&model); err != nil {
		return RunRow{}, err
	}
	r.Trigger, r.Status = Trigger(trigger), RunStatus(status)
	r.RequestedAt, r.StartedAt, r.EndedAt = decodeOptional(requested), decodeOptional(started),
		decodeOptional(ended)
	r.Classes, r.Model = json.RawMessage(classes), json.RawMessage(model)
	return r, nil
}

// oneRun reads the run a statement selects, and whether there is one.
func (r *Reader) oneRun(ctx context.Context, q statelog.Query, statement string, args ...any) (RunRow, bool, error) {
	var (
		out   RunRow
		found bool
	)
	_, err := r.log.Read(ctx, q, func(tx *sql.Tx) error {
		row, err := scanRun(tx.QueryRowContext(ctx, `SELECT `+runColumns+` `+statement, args...))
		switch {
		case errors.Is(err, sql.ErrNoRows):
			out, found = RunRow{}, false
			return nil
		case err != nil:
			return err
		}
		out, found = row, true
		return nil
	})
	return out, found, err
}

// LatestRun reads the run that ended last, and whether one has.
func (r *Reader) LatestRun(ctx context.Context, fresh statelog.Freshness) (RunRow, bool, error) {
	out, found, err := r.oneRun(ctx, fresh.Query(RunsScope(), true),
		`INDEXED BY advisor_runs_ended_idx WHERE status != '' ORDER BY ended_at DESC, run_id DESC LIMIT 1`)
	if err != nil {
		return RunRow{}, false, fmt.Errorf("advisordomain: read the latest run: %w", err)
	}
	return out, found, nil
}

// Run reads one run by its id, and whether the log holds it.
func (r *Reader) Run(ctx context.Context, id string, fresh statelog.Freshness) (RunRow, bool, error) {
	if !IsUUID(id) {
		return RunRow{}, false, invalid("%q is not a run id", id)
	}
	out, found, err := r.oneRun(ctx, fresh.Query(RunsScope(), false), `WHERE run_id = ?`, id)
	if err != nil {
		return RunRow{}, false, fmt.Errorf("advisordomain: read run %s: %w", id, err)
	}
	return out, found, nil
}

// PendingRequests reads every run a person requested that no record has
// ended yet, oldest first.
func (r *Reader) PendingRequests(ctx context.Context, fresh statelog.Freshness) ([]RunRow, error) {
	var out []RunRow
	_, err := r.log.Read(ctx, fresh.Query(RunsScope(), true), func(tx *sql.Tx) error {
		out = nil
		rows, err := tx.QueryContext(ctx, `SELECT `+runColumns+` INDEXED BY advisor_runs_pending_idx
			WHERE status = '' ORDER BY requested_at, run_id LIMIT ?`, MaxPending)
		if err != nil {
			return err
		}
		for rows.Next() {
			run, err := scanRun(rows)
			if err != nil {
				_ = rows.Close()
				return err
			}
			out = append(out, run)
		}
		return errors.Join(rows.Err(), rows.Close())
	})
	if err != nil {
		return nil, fmt.Errorf("advisordomain: read the pending run requests: %w", err)
	}
	return out, nil
}

// LastRunOfClasses reads, per class, the end of its newest run whose class
// status was done: what a finding's freshness is judged by.
func (r *Reader) LastRunOfClasses(ctx context.Context, fresh statelog.Freshness) (map[string]time.Time, error) {
	out := map[string]time.Time{}
	_, err := r.log.Read(ctx, fresh.Query(RunsScope(), true), func(tx *sql.Tx) error {
		clear(out)
		rows, err := tx.QueryContext(ctx, `SELECT class, ended_at FROM advisor_class_runs`)
		if err != nil {
			return err
		}
		for rows.Next() {
			var (
				class string
				ended int64
			)
			if err := rows.Scan(&class, &ended); err != nil {
				_ = rows.Close()
				return err
			}
			out[class] = store.DecodeTime(ended)
		}
		return errors.Join(rows.Err(), rows.Close())
	})
	if err != nil {
		return nil, fmt.Errorf("advisordomain: read each class's last run: %w", err)
	}
	return out, nil
}

// ---- reports ------------------------------------------------------------ //

// ReportRow is one week's report.
type ReportRow struct {
	Week      string
	From, To  time.Time
	WrittenAt time.Time
	Body      json.RawMessage
}

// scanReport reads one report row.
func scanReport(rows interface{ Scan(...any) error }) (ReportRow, error) {
	var (
		r                 ReportRow
		from, to, written int64
		body              string
	)
	if err := rows.Scan(&r.Week, &from, &to, &written, &body); err != nil {
		return ReportRow{}, err
	}
	r.From, r.To, r.WrittenAt, r.Body = store.DecodeTime(from), store.DecodeTime(to),
		store.DecodeTime(written), json.RawMessage(body)
	return r, nil
}

// Reports reads one page of reports, newest week first, and the cursor of the
// page after it, empty on the last. limit is 1 to [MaxPageSize],
// DefaultPageSize where zero.
func (r *Reader) Reports(ctx context.Context, cursor string, limit int,
	fresh statelog.Freshness) ([]ReportRow, string, error) {

	limit, limitErr := limitOf(limit)
	if limitErr != nil {
		return nil, "", limitErr
	}
	below := "9999-W99"
	if cursor != "" {
		if !IsWeek(cursor) {
			return nil, "", invalid("cursor %q is not one a page of reports answered", cursor)
		}
		below = cursor
	}
	var out []ReportRow
	_, err := r.log.Read(ctx, fresh.Query(DomainScope(), true), func(tx *sql.Tx) error {
		out = nil
		rows, err := tx.QueryContext(ctx, `
			SELECT week, period_from, period_to, written_at, body FROM advisor_reports
			WHERE week < ? ORDER BY week DESC LIMIT ?`, below, limit+1)
		if err != nil {
			return err
		}
		for rows.Next() {
			report, err := scanReport(rows)
			if err != nil {
				_ = rows.Close()
				return err
			}
			out = append(out, report)
		}
		return errors.Join(rows.Err(), rows.Close())
	})
	if err != nil {
		return nil, "", fmt.Errorf("advisordomain: read a page of reports: %w", err)
	}
	if len(out) <= limit {
		return out, "", nil
	}
	return out[:limit], out[limit-1].Week, nil
}

// Report reads one week's report, and whether it was written.
func (r *Reader) Report(ctx context.Context, week string, fresh statelog.Freshness) (ReportRow, bool, error) {
	if !IsWeek(week) {
		return ReportRow{}, false, invalid("week %q is not an ISO week such as 2026-W47", week)
	}
	var (
		out   ReportRow
		found bool
	)
	scope := statelog.ScopeSet{Paths: []string{ScopeTerm{Kind: TermReport, ID: week}.Path()}}
	_, err := r.log.Read(ctx, fresh.Query(scope, false), func(tx *sql.Tx) error {
		row, err := scanReport(tx.QueryRowContext(ctx, `
			SELECT week, period_from, period_to, written_at, body FROM advisor_reports WHERE week = ?`, week))
		switch {
		case errors.Is(err, sql.ErrNoRows):
			out, found = ReportRow{}, false
			return nil
		case err != nil:
			return err
		}
		out, found = row, true
		return nil
	})
	if err != nil {
		return ReportRow{}, false, fmt.Errorf("advisordomain: read the report of %s: %w", week, err)
	}
	return out, found, nil
}

// ---- alarm episodes ----------------------------------------------------- //

// AlarmEpisodeRow is one episode of an alarm series: raised, and cleared or
// still open (ClearedAt zero).
type AlarmEpisodeRow struct {
	SeriesID             string
	Node, Kind, Instance string
	RaisedAt, ClearedAt  time.Time
}

// AlarmEpisodes reads every episode open at any instant since since: those
// still open and those cleared at or after it, oldest raised first, refusing
// past [MaxAlarmEpisodes].
func (r *Reader) AlarmEpisodes(ctx context.Context, since time.Time,
	fresh statelog.Freshness) ([]AlarmEpisodeRow, error) {

	var out []AlarmEpisodeRow
	_, err := r.log.Read(ctx, fresh.Query(DomainScope(), true), func(tx *sql.Tx) error {
		out = nil
		rows, err := tx.QueryContext(ctx, `
			SELECT series_id, node, kind, instance, raised_at, cleared_at FROM advisor_alarm_episodes
			WHERE cleared_at IS NULL OR cleared_at >= ?
			ORDER BY raised_at, series_id, raised_position LIMIT ?`,
			store.EncodeTime(since), MaxAlarmEpisodes+1)
		if err != nil {
			return err
		}
		for rows.Next() {
			var (
				e       AlarmEpisodeRow
				raised  int64
				cleared sql.NullInt64
			)
			if err := rows.Scan(&e.SeriesID, &e.Node, &e.Kind, &e.Instance, &raised, &cleared); err != nil {
				_ = rows.Close()
				return err
			}
			e.RaisedAt = store.DecodeTime(raised)
			if cleared.Valid {
				e.ClearedAt = store.DecodeTime(cleared.Int64)
			}
			out = append(out, e)
		}
		if err := errors.Join(rows.Err(), rows.Close()); err != nil {
			return err
		}
		if len(out) > MaxAlarmEpisodes {
			return fmt.Errorf("advisordomain: more than %d alarm episodes since %s, past what one read "+
				"answers", MaxAlarmEpisodes, since.UTC())
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("advisordomain: read the alarm episodes: %w", err)
	}
	return out, nil
}

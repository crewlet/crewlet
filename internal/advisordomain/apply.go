package advisordomain

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/store"
)

// The applier is a PURE FUNCTION of the log: it reads no config, clock,
// keyring or other domain; every instant it writes is the broker's or the
// record's own; it judges no text (package doc, "The applier judges no text");
// a record the rows no longer admit writes nothing rather than failing; and
// every write is guarded so a redelivery writes nothing new.

// Applier writes this node's copy of the advisor estate. It holds nothing.
type Applier struct{}

// NewApplier builds the advisor applier.
func NewApplier() *Applier { return &Applier{} }

// Committed is the post-commit half, and there is nothing to tell: a reader
// asks again.
func (*Applier) Committed(context.Context) {}

// Gated reports a record its writer published inside that writer's eviction
// window, this log's one gate: it produces no row.
func (a *Applier) Gated(ctx context.Context, tx *sql.Tx, rec statelog.Record) (
	statelog.Reason, bool, error) {

	if rec.Writer == "" {
		return "", false, nil
	}
	evicted, err := evictedAt(ctx, tx, rec.Writer, rec.Position.Packed())
	if err != nil {
		return "", false, err
	}
	if evicted {
		return statelog.ReasonEvicted, true, nil
	}
	return "", false, nil
}

// applyContext is what every case needs, gathered once.
type applyContext struct {
	record   MutationRecord
	position statelog.Position
	packed   int64

	// brokerAt is the broker's own instant for this record, the only instant
	// a row records of its own.
	brokerAt time.Time
}

// at is the broker instant as it is stored.
func (c applyContext) at() int64 { return store.EncodeTime(c.brokerAt) }

// Apply writes one record's rows, dispatched on the subject kind.
func (a *Applier) Apply(ctx context.Context, tx *sql.Tx, rec statelog.Record,
	opts statelog.ApplyOptions) (int, error) {

	record, err := Decode(rec.Payload)
	if err != nil {
		return 0, fmt.Errorf("advisordomain: decode the record at %s: %w", rec.Position, err)
	}
	c := applyContext{record: record, position: rec.Position, packed: rec.Position.Packed(),
		brokerAt: opts.StoredAt}
	payload, err := DecodeMutation(record)
	if err != nil {
		return 0, fmt.Errorf("advisordomain: the record at %s: %w", rec.Position, err)
	}
	switch record.Subject.Kind {
	case KindBarrier:
		return 0, nil
	case KindTrimmed:
		// The framework removes this log's anchors below the floor in this
		// same transaction (ADR-0080); this domain writes nothing for it.
		return 0, nil
	case KindEviction:
		return applyEviction(ctx, tx, c, payload)
	case KindGeneration:
		return applyGeneration(ctx, tx, c, payload)
	case KindFinding:
		return applyFinding(ctx, tx, c, payload)
	case KindRun:
		return applyRun(ctx, tx, c, payload)
	case KindReport:
		return applyReport(ctx, tx, c, payload)
	case KindAlarm:
		return applyAlarm(ctx, tx, c, payload)
	case KindRetention:
		return applyExpire(ctx, tx, c, payload)
	}
	// A kind this build does not know at a version it reads is a writer
	// publishing a kind it never declared; failing makes that visible.
	return 0, fmt.Errorf("advisordomain: %s is not a kind this build applies, and the record "+
		"at %s claims version %d", record.Subject.Kind, rec.Position, record.V)
}

// ---- a finding ---------------------------------------------------------- //

// applyFinding writes one record on a finding key. A key at or past this
// record's position has taken it (a redelivery) and writes nothing; otherwise
// the op writes what the rows admit and the key's version moves to it.
func applyFinding(ctx context.Context, tx *sql.Tx, c applyContext, payload any) (int, error) {
	keyID := c.record.Subject.ID
	k, err := loadKey(ctx, tx, keyID)
	if err != nil {
		return 0, err
	}
	if k.found && k.version >= c.packed {
		return 0, nil
	}
	var written int
	switch p := payload.(type) {
	case Opening:
		// THE KEY ROW IS WRITTEN BY THE OPENING ITSELF, so it returns whole.
		return applyOpen(ctx, tx, c, k, p)
	case Revision:
		written, err = applyRevise(ctx, tx, c, k, p)
	case Refresh:
		written, err = applyRefresh(ctx, tx, c, k, p)
	case Move:
		written, err = applyMove(ctx, tx, c, k, p)
	case Explanation:
		written, err = applyExplain(ctx, tx, c, k, p)
	case Release:
		written, err = applyRelease(ctx, tx, c, k)
	default:
		return 0, fmt.Errorf("advisordomain: the finding record at %s carries a %T", c.position, payload)
	}
	if err != nil || !k.found {
		return written, err
	}
	res, err := tx.ExecContext(ctx, `UPDATE advisor_keys SET version = ? WHERE key_id = ? AND version < ?`,
		c.packed, keyID, c.packed)
	if err != nil {
		return 0, fmt.Errorf("advisordomain: move key %s to %s: %w", keyID, c.position, err)
	}
	return written + affected(res), nil
}

// history writes one line of an episode's history at this record.
func history(ctx context.Context, tx *sql.Tx, c applyContext, finding string, event Event,
	moved []byte) (int, error) {

	if len(strings.TrimSpace(string(moved))) == 0 {
		moved = []byte("[]")
	}
	res, err := tx.ExecContext(ctx, `
		INSERT INTO advisor_finding_history (finding_id, position, at, event, by_kind, by_name, moved)
		VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (finding_id, position) DO NOTHING`,
		finding, c.packed, c.at(), string(event), string(c.record.ActorKind), c.record.Actor, string(moved))
	if err != nil {
		return 0, fmt.Errorf("advisordomain: record %s's %s at %s: %w", finding, event, c.position, err)
	}
	return affected(res), nil
}

// applyOpen writes a new episode where the key has no non-closed one: its
// row, the key's row created or moved, and its first history line. The hold
// is the writer's to judge, at its own clock: the applier reads none.
func applyOpen(ctx context.Context, tx *sql.Tx, c applyContext, k keyState, o Opening) (int, error) {
	if o.Episode == "" || o.Key == "" {
		return 0, fmt.Errorf("advisordomain: the opening at %s names no episode or key", c.position)
	}
	if k.current != "" {
		return 0, nil
	}
	if e, err := loadEpisode(ctx, tx, o.Episode); err != nil || e.found {
		return 0, err
	}
	res, err := tx.ExecContext(ctx, `
		INSERT INTO advisor_findings (id, key_id, key, class, source, actor, area, severity,
			subject_kind, subject_id, state, revision, verdict_digest, fix_digest, impact_minutes,
			impact_tokens, opened_at, revised_at, facts_at, seen_before, body, position)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		o.Episode, c.record.Subject.ID, o.Key, o.Class, string(o.Source), string(o.Actor), o.Area,
		string(o.Severity), o.SubjectKind, o.SubjectID, string(StateOpen), o.Revision, o.VerdictDigest,
		o.FixDigest, nullable(o.ImpactMinutes), nullable(o.ImpactTokens), c.at(), c.at(), c.at(),
		k.episodes, string(o.Body), c.packed)
	if err != nil {
		return 0, fmt.Errorf("advisordomain: open episode %s at %s: %w", o.Episode, c.position, err)
	}
	written := affected(res)
	res, err = tx.ExecContext(ctx, `
		INSERT INTO advisor_keys (key_id, key, version, episodes, current_episode)
		VALUES (?, ?, ?, 1, ?)
		ON CONFLICT (key_id) DO UPDATE SET version = excluded.version,
			episodes = advisor_keys.episodes + 1, current_episode = excluded.current_episode
		WHERE excluded.version > advisor_keys.version`,
		c.record.Subject.ID, o.Key, c.packed, o.Episode)
	if err != nil {
		return 0, fmt.Errorf("advisordomain: open key %s at %s: %w", o.Key, c.position, err)
	}
	written += affected(res)
	n, err := history(ctx, tx, c, o.Episode, EventOpened, nil)
	return written + n, err
}

// nullable is an optional count as a row stores it.
func nullable(v *int64) any {
	if v == nil {
		return nil
	}
	return *v
}

// current is the key's non-closed episode named id, false where the rows no
// longer hold it.
func current(ctx context.Context, tx *sql.Tx, k keyState, id string) (episodeState, bool, error) {
	if !k.found || k.current == "" || k.current != id {
		return episodeState{}, false, nil
	}
	e, err := loadEpisode(ctx, tx, id)
	if err != nil || !e.found || e.state.Closed() {
		return episodeState{}, false, err
	}
	return e, true, nil
}

// applyRevise writes a moved verdict or fix: the next revision only.
func applyRevise(ctx context.Context, tx *sql.Tx, c applyContext, k keyState, r Revision) (int, error) {
	e, ok, err := current(ctx, tx, k, r.Episode)
	if err != nil || !ok || r.Revision != e.revision+1 {
		return 0, err
	}
	res, err := tx.ExecContext(ctx, `
		UPDATE advisor_findings SET revision = ?, severity = ?, verdict_digest = ?, fix_digest = ?,
			impact_minutes = ?, impact_tokens = ?, body = ?, revised_at = ?, facts_at = ?, position = ?
		WHERE id = ?`,
		r.Revision, string(r.Severity), r.VerdictDigest, r.FixDigest, nullable(r.ImpactMinutes),
		nullable(r.ImpactTokens), string(r.Body), c.at(), c.at(), c.packed, r.Episode)
	if err != nil {
		return 0, fmt.Errorf("advisordomain: revise episode %s at %s: %w", r.Episode, c.position, err)
	}
	n, err := history(ctx, tx, c, r.Episode, EventRevised, r.Moved)
	return affected(res) + n, err
}

// applyRefresh writes refreshed facts on the revision they were read under;
// a newer stored revision takes nothing.
func applyRefresh(ctx context.Context, tx *sql.Tx, c applyContext, k keyState, r Refresh) (int, error) {
	e, ok, err := current(ctx, tx, k, r.Episode)
	if err != nil || !ok || r.Revision != e.revision {
		return 0, err
	}
	res, err := tx.ExecContext(ctx, `
		UPDATE advisor_findings SET impact_minutes = ?, impact_tokens = ?, body = ?, facts_at = ?,
			position = ?
		WHERE id = ?`,
		nullable(r.ImpactMinutes), nullable(r.ImpactTokens), string(r.Body), c.at(), c.packed, r.Episode)
	if err != nil {
		return 0, fmt.Errorf("advisordomain: refresh episode %s at %s: %w", r.Episode, c.position, err)
	}
	return affected(res), nil
}

// applyMove writes one legal transition (package doc, "Legal moves"), and its
// history line; an illegal one takes nothing.
func applyMove(ctx context.Context, tx *sql.Tx, c applyContext, k keyState, m Move) (int, error) {
	e, ok, err := current(ctx, tx, k, m.Episode)
	if err != nil || !ok || !legalFrom(m.Event, e.state) || (m.Event == EventClosed) != m.State.Closed() {
		return 0, err
	}
	var (
		statement string
		args      []any
	)
	switch m.Event {
	case EventApplied:
		statement = `state = ?, applied_fix = ?, applied_at = ?, applied_by = ?, effect_due_at = ?,
			snoozed_until = 0`
		args = []any{string(StateApplied), m.FixDigest, c.at(), c.record.Actor, encodeOptional(m.EffectDueAt)}
	case EventIneffective:
		statement = `state = ?, ineffective_fix = ?, ineffective_at = ?, effect_due_at = 0`
		args = []any{string(StateOpen), m.FixDigest, c.at()}
	case EventSnoozed:
		statement, args = `snoozed_until = ?`, []any{encodeOptional(m.SnoozedUntil)}
	case EventUnsnoozed:
		statement, args = `snoozed_until = 0`, nil
	case EventUseful:
		statement, args = `useful_by = ?, useful_at = ?`, []any{c.record.Actor, c.at()}
	case EventClosed:
		statement = `state = ?, close_reason = ?, closed_at = ?`
		args = []any{string(m.State), string(m.Reason), c.at()}
	default:
		return 0, nil
	}
	res, err := tx.ExecContext(ctx, `UPDATE advisor_findings SET `+statement+`, position = ? WHERE id = ?`,
		append(args, c.packed, m.Episode)...)
	if err != nil {
		return 0, fmt.Errorf("advisordomain: move episode %s %s at %s: %w", m.Episode, m.Event,
			c.position, err)
	}
	written := affected(res)
	keyID := c.record.Subject.ID
	switch {
	case m.Event == EventIneffective:
		res, err = tx.ExecContext(ctx, `UPDATE advisor_keys SET ineffective = ? WHERE key_id = ?`,
			k.withIneffective(m.FixDigest), keyID)
	case m.Event == EventClosed && m.Hold != nil:
		res, err = tx.ExecContext(ctx, `
			UPDATE advisor_keys SET current_episode = NULL, hold_until = ?, hold_reason = ?,
				hold_severity = ?, hold_episode = ?
			WHERE key_id = ?`,
			encodeOptional(m.Hold.Until), m.Hold.Reason, string(m.Hold.Severity), m.Episode, keyID)
	case m.Event == EventClosed:
		res, err = tx.ExecContext(ctx, `UPDATE advisor_keys SET current_episode = NULL WHERE key_id = ?`,
			keyID)
	default:
		res = nil
	}
	if err != nil {
		return 0, fmt.Errorf("advisordomain: move key %s at %s: %w", keyID, c.position, err)
	}
	if res != nil {
		written += affected(res)
	}
	n, err := history(ctx, tx, c, m.Episode, m.Event, m.Moved)
	return written + n, err
}

// applyExplain writes the model's words while the stored verdict is the one
// they explain.
func applyExplain(ctx context.Context, tx *sql.Tx, c applyContext, k keyState, x Explanation) (int, error) {
	e, ok, err := current(ctx, tx, k, x.Episode)
	if err != nil || !ok || e.verdictDigest != x.VerdictDigest {
		return 0, err
	}
	res, err := tx.ExecContext(ctx, `
		UPDATE advisor_findings SET explanation = ?, explained_digest = ?, position = ? WHERE id = ?`,
		x.Text, x.VerdictDigest, c.packed, x.Episode)
	if err != nil {
		return 0, fmt.Errorf("advisordomain: explain episode %s at %s: %w", x.Episode, c.position, err)
	}
	n, err := history(ctx, tx, c, x.Episode, EventExplained, nil)
	return affected(res) + n, err
}

// applyRelease lifts the key's hold, with a line in the history of the
// episode whose dismissal set it, where that episode is still kept.
func applyRelease(ctx context.Context, tx *sql.Tx, c applyContext, k keyState) (int, error) {
	if !k.found || !k.hold.Valid {
		return 0, nil
	}
	res, err := tx.ExecContext(ctx, `
		UPDATE advisor_keys SET hold_until = NULL, hold_reason = NULL, hold_severity = NULL,
			hold_episode = NULL
		WHERE key_id = ?`, c.record.Subject.ID)
	if err != nil {
		return 0, fmt.Errorf("advisordomain: release key %s at %s: %w", c.record.Subject.ID, c.position, err)
	}
	written := affected(res)
	e, err := loadEpisode(ctx, tx, k.holdEpisode)
	if err != nil || !e.found {
		return written, err
	}
	n, err := history(ctx, tx, c, k.holdEpisode, EventReleased, nil)
	return written + n, err
}

// ---- runs, reports, alarms ---------------------------------------------- //

// applyRun writes a run's request or its record. A request creates the run's
// row once; a record fills it once, and moves the newest run of each class it
// ran to done.
func applyRun(ctx context.Context, tx *sql.Tx, c applyContext, payload any) (int, error) {
	run := c.record.Subject.ID
	switch p := payload.(type) {
	case RunRequest:
		res, err := tx.ExecContext(ctx, `
			INSERT INTO advisor_runs (run_id, trigger_kind, by_name, requested_at, position)
			VALUES (?, ?, ?, ?, ?)
			ON CONFLICT (run_id) DO NOTHING`, run, string(TriggerPerson), p.By, c.at(), c.packed)
		if err != nil {
			return 0, fmt.Errorf("advisordomain: request run %s at %s: %w", run, c.position, err)
		}
		return affected(res), nil
	case RunRecord:
		return applyRecord(ctx, tx, c, run, p)
	}
	return 0, fmt.Errorf("advisordomain: the run record at %s carries a %T", c.position, payload)
}

// applyRecord fills a run's row once, and the newest end of each class whose
// status was done.
func applyRecord(ctx context.Context, tx *sql.Tx, c applyContext, run string, r RunRecord) (int, error) {
	lines, err := runClasses(r.Classes)
	if err != nil {
		return 0, fmt.Errorf("advisordomain: the run record at %s: %w", c.position, err)
	}
	model := strings.TrimSpace(string(r.Model))
	if model == "" {
		model = "null"
	}
	res, err := tx.ExecContext(ctx, `
		INSERT INTO advisor_runs (run_id, trigger_kind, by_name, started_at, ended_at, status, classes,
			model, position)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (run_id) DO UPDATE SET trigger_kind = excluded.trigger_kind,
			by_name = excluded.by_name, started_at = excluded.started_at, ended_at = excluded.ended_at,
			status = excluded.status, classes = excluded.classes, model = excluded.model,
			position = excluded.position
		WHERE advisor_runs.status = ''`,
		run, string(r.Trigger), r.By, store.EncodeTime(r.StartedAt), store.EncodeTime(r.EndedAt),
		string(r.Status), string(r.Classes), model, c.packed)
	if err != nil {
		return 0, fmt.Errorf("advisordomain: record run %s at %s: %w", run, c.position, err)
	}
	written := affected(res)
	if written == 0 {
		return 0, nil
	}
	ended := store.EncodeTime(r.EndedAt)
	for _, l := range lines {
		if l.Status != ClassDone {
			continue
		}
		res, err := tx.ExecContext(ctx, `
			INSERT INTO advisor_class_runs (class, ended_at, position) VALUES (?, ?, ?)
			ON CONFLICT (class) DO UPDATE SET ended_at = excluded.ended_at, position = excluded.position
			WHERE excluded.ended_at > advisor_class_runs.ended_at`, l.Class, ended, c.packed)
		if err != nil {
			return 0, fmt.Errorf("advisordomain: record class %s's run at %s: %w", l.Class, c.position, err)
		}
		written += affected(res)
	}
	return written, nil
}

// applyReport writes a week's report once.
func applyReport(ctx context.Context, tx *sql.Tx, c applyContext, payload any) (int, error) {
	r, ok := payload.(Report)
	if !ok {
		return 0, fmt.Errorf("advisordomain: the report record at %s carries a %T", c.position, payload)
	}
	res, err := tx.ExecContext(ctx, `
		INSERT INTO advisor_reports (week, period_from, period_to, written_at, body, position)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT (week) DO NOTHING`,
		c.record.Subject.ID, store.EncodeTime(r.From), store.EncodeTime(r.To), c.at(), string(r.Body), c.packed)
	if err != nil {
		return 0, fmt.Errorf("advisordomain: write the report of %s at %s: %w", c.record.Subject.ID,
			c.position, err)
	}
	return affected(res), nil
}

// applyAlarm opens an episode of the series where none is open, or closes the
// open one. A series that holds this record's position or a later one has
// taken it (a redelivery) and writes nothing.
func applyAlarm(ctx context.Context, tx *sql.Tx, c applyContext, payload any) (int, error) {
	series := c.record.Subject.ID
	var raised, cleared sql.NullInt64
	if err := tx.QueryRowContext(ctx, `
		SELECT MAX(raised_position), MAX(cleared_position) FROM advisor_alarm_episodes
		WHERE series_id = ?`, series).Scan(&raised, &cleared); err != nil {
		return 0, fmt.Errorf("advisordomain: read alarm series %s at %s: %w", series, c.position, err)
	}
	if raised.Int64 >= c.packed || cleared.Int64 >= c.packed {
		return 0, nil
	}
	open, err := openEpisode(ctx, tx, series)
	if err != nil {
		return 0, err
	}
	var res sql.Result
	switch p := payload.(type) {
	case AlarmRaise:
		if open {
			return 0, nil
		}
		res, err = tx.ExecContext(ctx, `
			INSERT INTO advisor_alarm_episodes (series_id, raised_position, node, kind, instance, raised_at)
			VALUES (?, ?, ?, ?, ?, ?)`,
			series, c.packed, p.Node, p.Kind, p.Instance, store.EncodeTime(p.RaisedAt))
	case AlarmClear:
		if !open {
			return 0, nil
		}
		res, err = tx.ExecContext(ctx, `
			UPDATE advisor_alarm_episodes SET cleared_at = ?, cleared_position = ?
			WHERE series_id = ? AND cleared_position IS NULL`,
			store.EncodeTime(p.ClearedAt), c.packed, series)
	default:
		return 0, fmt.Errorf("advisordomain: the alarm record at %s carries a %T", c.position, payload)
	}
	if err != nil {
		return 0, fmt.Errorf("advisordomain: apply alarm series %s at %s: %w", series, c.position, err)
	}
	return affected(res), nil
}

// ---- retention ---------------------------------------------------------- //

// expiryStatements are what a day's expiry deletes, each with the cutoff it is
// held to: closed episodes and their history; a key with nothing left on it
// (no episode, no hold standing at the expiry's broker instant, no ineffective
// fix); runs, reports and cleared alarm episodes past theirs; and the record
// of older expiries.
var expiryStatements = []struct {
	statement string
	cutoff    func(e Expiry, brokerAt int64) []any
}{
	{`DELETE FROM advisor_finding_history WHERE finding_id IN (SELECT id FROM advisor_findings
		INDEXED BY advisor_findings_closed_idx WHERE closed_at != 0 AND closed_at < ?)`,
		func(e Expiry, _ int64) []any { return []any{store.EncodeTime(e.ClosedBefore)} }},
	{`DELETE FROM advisor_findings WHERE closed_at != 0 AND closed_at < ?`,
		func(e Expiry, _ int64) []any { return []any{store.EncodeTime(e.ClosedBefore)} }},
	{`DELETE FROM advisor_keys WHERE current_episode IS NULL AND ineffective = '[]'
		AND (hold_until IS NULL OR (hold_until != 0 AND hold_until <= ?))
		AND NOT EXISTS (SELECT 1 FROM advisor_findings WHERE advisor_findings.key_id = advisor_keys.key_id)`,
		func(_ Expiry, at int64) []any { return []any{at} }},
	{`DELETE FROM advisor_runs WHERE (status != '' AND ended_at < ?) OR (status = '' AND requested_at < ?)`,
		func(e Expiry, _ int64) []any {
			return []any{store.EncodeTime(e.RunsBefore), store.EncodeTime(e.RunsBefore)}
		}},
	{`DELETE FROM advisor_reports WHERE written_at < ?`,
		func(e Expiry, _ int64) []any { return []any{store.EncodeTime(e.ReportsBefore)} }},
	{`DELETE FROM advisor_alarm_episodes WHERE cleared_at IS NOT NULL AND cleared_at < ?`,
		func(e Expiry, _ int64) []any { return []any{store.EncodeTime(e.AlarmsBefore)} }},
	{`DELETE FROM advisor_expiries WHERE at < ?`,
		func(e Expiry, _ int64) []any { return []any{store.EncodeTime(e.RunsBefore)} }},
}

// applyExpire records the day's expiry once, and removes what is past each
// retention it names.
func applyExpire(ctx context.Context, tx *sql.Tx, c applyContext, payload any) (int, error) {
	e, ok := payload.(Expiry)
	if !ok || e.ClosedBefore.IsZero() || e.RunsBefore.IsZero() || e.ReportsBefore.IsZero() ||
		e.AlarmsBefore.IsZero() {
		return 0, fmt.Errorf("advisordomain: the expiry at %s names no cutoff", c.position)
	}
	res, err := tx.ExecContext(ctx, `
		INSERT INTO advisor_expiries (day, at, position) VALUES (?, ?, ?)
		ON CONFLICT (day) DO NOTHING`, c.record.Subject.ID, c.at(), c.packed)
	if err != nil {
		return 0, fmt.Errorf("advisordomain: record the expiry of %s at %s: %w", c.record.Subject.ID,
			c.position, err)
	}
	written := affected(res)
	if written == 0 {
		return 0, nil
	}
	for _, s := range expiryStatements {
		res, err := tx.ExecContext(ctx, s.statement, s.cutoff(e, c.at())...)
		if err != nil {
			return 0, fmt.Errorf("advisordomain: expire at %s: %w", c.position, err)
		}
		written += affected(res)
	}
	return written, nil
}

// ---- the framework's ---------------------------------------------------- //

// applyEviction records a node's removal from this log, or its readmission, as
// an INVERSE COMMIT rather than a delete.
func applyEviction(ctx context.Context, tx *sql.Tx, c applyContext, payload any) (int, error) {
	e, ok := payload.(Eviction)
	if !ok || e.NodeID == "" {
		return 0, fmt.Errorf("advisordomain: the eviction at %s names no node", c.position)
	}
	var (
		res sql.Result
		err error
	)
	if e.Readmitted {
		res, err = tx.ExecContext(ctx, `
			UPDATE advisor_evictions SET readmitted_position = ?, version = ?
			WHERE node_id = ? AND version < ?`, c.packed, c.packed, e.NodeID, c.packed)
	} else {
		// A SECOND EVICTION CLEARS THE READMISSION, so the evicted,
		// readmitted, evicted sequence reads as three facts.
		res, err = tx.ExecContext(ctx, `
			INSERT INTO advisor_evictions
				(node_id, at, by, from_position, readmitted_position, version)
			VALUES (?, ?, ?, ?, NULL, ?)
			ON CONFLICT (node_id) DO UPDATE SET
				at = excluded.at, by = excluded.by,
				from_position = excluded.from_position,
				readmitted_position = NULL, version = excluded.version
			WHERE excluded.version > advisor_evictions.version`,
			e.NodeID, c.at(), e.EvictedBy, c.packed, c.packed)
	}
	if err != nil {
		return 0, fmt.Errorf("advisordomain: apply the eviction of %s at %s: %w",
			e.NodeID, c.position, err)
	}
	return affected(res), nil
}

// applyGeneration writes a reanchor's audit row, create-only.
func applyGeneration(ctx context.Context, tx *sql.Tx, c applyContext, payload any) (int, error) {
	g, ok := payload.(Generation)
	if !ok {
		return 0, fmt.Errorf("advisordomain: the generation record at %s carries a %T", c.position, payload)
	}
	res, err := tx.ExecContext(ctx, `
		INSERT INTO advisor_log_generations
			(generation, at, by, new_stream_created_at, prev_last_seq_seen, record_id)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT (generation) DO NOTHING`,
		g.Generation, c.at(), g.By, store.EncodeTime(g.StreamAt),
		int64(g.PrevHighest), c.record.OpID)
	if err != nil {
		return 0, fmt.Errorf("advisordomain: record generation %d at %s: %w", g.Generation, c.position, err)
	}
	return affected(res), nil
}

// affected is how many rows a statement wrote, the row budget's input.
func affected(res sql.Result) int {
	n, _ := res.RowsAffected()
	return int(n)
}

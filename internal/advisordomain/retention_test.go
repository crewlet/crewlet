package advisordomain_test

import (
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/advisordomain"
	"github.com/crewlet/crewlet/internal/iam"
)

// The instant the apply cases start at.
var t0 = time.Date(2030, 1, 1, 9, 0, 0, 0, time.UTC)

// THE RETENTION TAKES ONLY WHAT IS PAST IT: closed episodes with their history
// after ninety days, a key only once nothing holds it, runs and cleared alarm
// episodes after thirty days, reports after twenty-six weeks; and a day is
// swept once.
//
// Mutation: drop `ineffective = '[]'` from the key's expiry and the key whose
// fix did not take loses that memory.
func TestTheRetentionTakesOnlyWhatIsPastIt(t *testing.T) {
	t.Parallel()
	e := newEstate(t)
	day := t0.Add(24 * time.Hour)
	// Four keys, each episode opened at t0 and moved a day later: one
	// resolved, one dismissed for good, one whose fix did not take, one open.
	resolved, held := opening(idleKey, advisordomain.SeverityCosts, t0), opening(starvedKey, advisordomain.SeverityCosts, t0)
	failed, open := opening(poolKey, advisordomain.SeverityCosts, t0), opening("wake.alarm_noop:company", advisordomain.SeverityNote, t0)
	for _, o := range []advisordomain.Opening{resolved, held, failed, open} {
		e.apply(record(t, o.Key, advisordomain.OpOpen, "open-"+o.Key, o), t0)
	}
	e.apply(record(t, resolved.Key, advisordomain.OpMove, "close-resolved", closing(resolved.Episode)), day)
	e.apply(record(t, held.Key, advisordomain.OpMove, "dismiss-held",
		dismissal(held.Episode, advisordomain.SeverityCosts, time.Time{})), day)
	fix := failed.FixDigest
	for i, m := range []advisordomain.Move{
		{Episode: failed.Episode, Event: advisordomain.EventApplied, State: advisordomain.StateApplied,
			FixDigest: fix, EffectDueAt: day},
		{Episode: failed.Episode, Event: advisordomain.EventIneffective, State: advisordomain.StateOpen, FixDigest: fix},
		closing(failed.Episode),
	} {
		e.apply(record(t, failed.Key, advisordomain.OpMove, "failed-"+string(rune('a'+i)), m), day)
	}
	run := advisordomain.RunRecord{Run: "4c7d2e91-3b5a-4f60-8e1d-9a2b7c6d5e4f", Trigger: advisordomain.TriggerSchedule,
		StartedAt: t0, EndedAt: t0.Add(time.Minute), Status: advisordomain.RunDone, Classes: json.RawMessage(`[]`)}
	e.apply(recordBy(t, advisordomain.RunSubject(run.Run), advisordomain.OpRecord, "run", run, engine), t0)
	report := advisordomain.Report{Week: advisordomain.Week(t0), From: t0.Add(-7 * 24 * time.Hour), To: t0,
		Body: json.RawMessage(`{}`)}
	e.apply(recordBy(t, advisordomain.ReportSubject(report.Week), advisordomain.OpWrite, "report", report, engine), t0)
	raise := advisordomain.AlarmRaise{Node: "data-1", Kind: "pool_starved", RaisedAt: t0}
	series := advisordomain.AlarmSubject(raise.Node, raise.Kind, raise.Instance)
	e.apply(recordBy(t, series, advisordomain.OpRaise, "raise", raise, engine), t0)
	e.apply(recordBy(t, series, advisordomain.OpClear, "clear", advisordomain.AlarmClear{ClearedAt: t0.Add(time.Hour)},
		engine), t0)

	now := t0.Add(100 * 24 * time.Hour)
	today, expiry := advisordomain.ExpiryFor(now)
	expire := recordBy(t, advisordomain.RetentionSubject(today), advisordomain.OpExpire, "expire", expiry, engine)
	e.apply(expire, now)

	for _, c := range []struct {
		label     string
		statement string
		want      int
	}{
		{"closed episodes", `SELECT COUNT(*) FROM advisor_findings WHERE closed_at != 0`, 0},
		{"the open episode", `SELECT COUNT(*) FROM advisor_findings WHERE closed_at = 0`, 1},
		{"history of closed episodes", `SELECT COUNT(*) FROM advisor_finding_history WHERE finding_id != ?`, 0},
		{"the resolved key", `SELECT COUNT(*) FROM advisor_keys WHERE key = ?`, 0},
		{"the held key", `SELECT COUNT(*) FROM advisor_keys WHERE key = ? AND hold_until = 0`, 1},
		{"the key whose fix did not take", `SELECT COUNT(*) FROM advisor_keys WHERE key = ? AND ineffective != '[]'`, 1},
		{"the run", `SELECT COUNT(*) FROM advisor_runs`, 0},
		{"the report, inside its 26 weeks", `SELECT COUNT(*) FROM advisor_reports`, 1},
		{"the cleared alarm episode", `SELECT COUNT(*) FROM advisor_alarm_episodes`, 0},
	} {
		var args []any
		switch c.label {
		case "history of closed episodes":
			args = []any{open.Episode}
		case "the resolved key":
			args = []any{resolved.Key}
		case "the held key":
			args = []any{held.Key}
		case "the key whose fix did not take":
			args = []any{failed.Key}
		}
		if got := e.count(c.statement, args...); got != c.want {
			t.Errorf("%s: %d rows, want %d", c.label, got, c.want)
		}
	}
	// A DAY IS SWEPT ONCE: its record again sweeps nothing, not even a run
	// recorded since that its cutoff would take.
	late := run
	late.Run = "9e8d7c6b-5a49-4382-a1b0-c9d8e7f6a5b4"
	e.apply(recordBy(t, advisordomain.RunSubject(late.Run), advisordomain.OpRecord, "late-run", late, engine), now)
	e.applyAt(e.seq+1, expire, now.Add(time.Hour))
	if got := e.count(`SELECT COUNT(*) FROM advisor_runs`); got != 1 {
		t.Errorf("a day's expiry applied twice swept again: %d runs, want the late one kept", got)
	}
}

// engine is the actor of every record the duty writes.
var engine = iam.Actor{Name: "engine", Kind: iam.ActorSystem}

// A RECORD THE ROWS NO LONGER ADMIT WRITES NOTHING, and a redelivered record
// writes nothing new: the applier holds every record to the rows again.
func TestARecordTheRowsNoLongerAdmitWritesNothing(t *testing.T) {
	t.Parallel()
	e := newEstate(t)
	o := opening(idleKey, advisordomain.SeverityCosts, t0)
	e.apply(record(t, idleKey, advisordomain.OpOpen, "open", o), t0)
	snooze := record(t, idleKey, advisordomain.OpMove, "snooze", advisordomain.Move{Episode: o.Episode,
		Event: advisordomain.EventSnoozed, State: advisordomain.StateOpen, SnoozedUntil: t0.Add(time.Hour)})
	e.apply(snooze, t0)
	snoozedAt := e.seq
	e.apply(record(t, idleKey, advisordomain.OpMove, "unsnooze", advisordomain.Move{Episode: o.Episode,
		Event: advisordomain.EventUnsnoozed, State: advisordomain.StateOpen}), t0)
	e.applyAt(snoozedAt, snooze, t0)
	if got := e.count(`SELECT COUNT(*) FROM advisor_findings WHERE snoozed_until != 0`); got != 0 {
		t.Errorf("a redelivered snooze snoozed the episode again")
	}
	e.apply(record(t, idleKey, advisordomain.OpMove, "close", closing(o.Episode)), t0)
	lines := e.count(`SELECT COUNT(*) FROM advisor_finding_history`)
	for _, late := range []struct {
		op      advisordomain.OpKind
		payload any
	}{
		{advisordomain.OpMove, advisordomain.Move{Episode: o.Episode, Event: advisordomain.EventApplied,
			State: advisordomain.StateApplied, FixDigest: o.FixDigest, EffectDueAt: t0}},
		{advisordomain.OpRevise, advisordomain.Revision{Episode: o.Episode, Revision: 2,
			Severity: advisordomain.SeverityBlocks, VerdictDigest: sha("verdict 2"), FixDigest: o.FixDigest,
			Body: json.RawMessage(`{}`)}},
		{advisordomain.OpExplain, advisordomain.Explanation{Episode: o.Episode, VerdictDigest: o.VerdictDigest,
			Text: "Late words."}},
	} {
		e.apply(record(t, idleKey, late.op, "late-"+string(late.op), late.payload), t0)
	}
	if got := e.count(`SELECT COUNT(*) FROM advisor_finding_history`); got != lines {
		t.Errorf("records about a closed episode wrote %d history lines", got-lines)
	}
	if got := e.count(`SELECT COUNT(*) FROM advisor_findings WHERE state = 'resolved' AND revision = 1
		AND explanation = ''`); got != 1 {
		t.Errorf("records about a closed episode moved it")
	}
	row, _, err := e.reader().Finding(t.Context(), o.Episode, stale)
	if err != nil || row.KeyVersion != uint64(position(e.seq).Packed()) {
		t.Errorf("the key is at %d (%v), want the newest record's position: every record on a key moves "+
			"its version", row.KeyVersion, err)
	}
	if _, _, err := e.reader().Finding(t.Context(), advisordomain.EpisodeID(idleKey, t0.Add(time.Hour)), stale); !errors.Is(err, advisordomain.ErrNotFound) {
		t.Errorf("an episode never opened: %v, want ErrNotFound", err)
	}
	// AN ALARM'S CLEAR REDELIVERED after the next raise leaves that one open.
	raise := advisordomain.AlarmRaise{Node: "data-1", Kind: "pool_starved", RaisedAt: t0}
	series := advisordomain.AlarmSubject(raise.Node, raise.Kind, raise.Instance)
	e.apply(recordBy(t, series, advisordomain.OpRaise, "raise-1", raise, engine), t0)
	clearing := recordBy(t, series, advisordomain.OpClear, "clear-1", advisordomain.AlarmClear{ClearedAt: t0}, engine)
	e.apply(clearing, t0)
	clearedAt := e.seq
	e.apply(recordBy(t, series, advisordomain.OpRaise, "raise-2", raise, engine), t0)
	e.applyAt(clearedAt, clearing, t0)
	if got := e.count(`SELECT COUNT(*) FROM advisor_alarm_episodes WHERE cleared_at IS NULL`); got != 1 {
		t.Errorf("a redelivered clear closed the series' next episode")
	}
}

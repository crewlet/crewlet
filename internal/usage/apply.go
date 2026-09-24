package usage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/crewlet/crewlet/internal/period"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/store"
)

// Applier is the usage domain's deterministic state machine: the ONLY writer of
// `usage_turns`, `usage_tokens`, `usage_reads` and `usage_schedule_runs`, on
// every node.
//
// # An apply REPLACES
//
// A record is its object's whole current value, so applying it deletes every
// row the object held and writes the record's. Inserting beside what was there
// would count a day's morning again every time its afternoon was published —
// the one mistake this shape cannot survive, because nothing downstream could
// tell a double count from a busy day.
//
// # The guard is the POSITION
//
// For the vector domain's reason: a record's position on this stream is
// monotone and a pure function of the record, so a redelivery, or an older
// message replayed after a newer one was applied, is a no-op by arithmetic.
// A seat's guard lives on its head row in `usage_turns`, which every seat
// record writes; a schedule has no head row, so its guard is the newest version
// among its own rows.
//
// # The horizon is applied here, by the record
//
// Every apply also deletes the rows older than its own day minus [History].
// That is a pure function of the record — the day it carries — so every node
// that applies it deletes the same rows, and no local sweep ever writes this
// estate. A company that goes quiet keeps its old rows until the next record
// arrives, which is harmless: the stream has already forgotten them, and a
// read asks for a window rather than for everything.
type Applier struct{}

// NewApplier builds the usage applier. It takes nothing, for the vector
// applier's reason: an applier that needed a node id would write rows that
// depend on which node ran it.
func NewApplier() Applier { return Applier{} }

// HorizonDays is [History] in whole days, which is what the apply subtracts
// from a record's day.
var HorizonDays = int(History / (24 * time.Hour))

// Apply writes this record's rows and reports how many it wrote.
func (a Applier) Apply(ctx context.Context, tx *sql.Tx, rec statelog.Record, opts statelog.ApplyOptions) (int, error) {
	r, err := Decode(rec.Payload)
	if err != nil {
		var future *ErrFutureVersion
		if errors.As(err, &future) {
			return 0, err
		}
		return 0, fmt.Errorf("usage: apply at %s: %w", rec.Position, err)
	}
	expired, err := a.expire(ctx, tx, r.Subject.Day)
	if err != nil {
		return 0, err
	}
	var wrote int
	switch r.Subject.Kind {
	case KindSeat:
		wrote, err = a.seat(ctx, tx, r, rec.Position, opts.MaxVariables)
	case KindSchedule:
		wrote, err = a.schedule(ctx, tx, r, rec.Position, opts.MaxVariables)
	default:
		// UNREACHABLE past Decode, which refuses an unknown kind — kept so
		// a kind added to the enum without an apply is a failure here
		// rather than a record that writes nothing.
		err = fmt.Errorf("usage: the record on %s at %s is a kind this build "+
			"has no apply for", r.Subject, rec.Position)
	}
	return expired + wrote, err
}

// expire deletes every row older than the record's day minus the history.
func (a Applier) expire(ctx context.Context, tx *sql.Tx, day string) (int, error) {
	w, err := period.Parse(period.Day, day, nil)
	if err != nil {
		return 0, fmt.Errorf("usage: the record's day: %w", err)
	}
	cutoff := w.Shift(-HorizonDays).Label
	var n int
	for _, table := range []string{"usage_tokens", "usage_reads", "usage_turns", "usage_schedule_runs"} {
		res, err := tx.ExecContext(ctx, `DELETE FROM `+table+` WHERE day < ?`, cutoff)
		if err != nil {
			return 0, fmt.Errorf("usage: expire %s below %s: %w", table, cutoff, err)
		}
		moved, err := res.RowsAffected()
		if err != nil {
			return 0, err
		}
		n += int(moved)
	}
	return n, nil
}

// seat replaces one seat-day.
func (a Applier) seat(ctx context.Context, tx *sql.Tx, r Record, at statelog.Position, maxVars int) (int, error) {
	s := r.Subject
	version := at.Packed()
	var held int64
	switch err := tx.QueryRowContext(ctx, `
		SELECT version FROM usage_turns WHERE day = ? AND node = ? AND agent_id = ?`,
		s.Day, s.Node, s.Seat).Scan(&held); {
	case errors.Is(err, sql.ErrNoRows):
	case err != nil:
		return 0, fmt.Errorf("usage: read the head of %s: %w", s, err)
	case held >= version:
		// A REDELIVERY, or an older message than this node already holds.
		return 0, nil
	}

	for _, table := range []string{"usage_tokens", "usage_reads"} {
		if _, err := tx.ExecContext(ctx, `DELETE FROM `+table+
			` WHERE day = ? AND node = ? AND agent_id = ?`, s.Day, s.Node, s.Seat); err != nil {
			return 0, fmt.Errorf("usage: clear %s for %s: %w", table, s, err)
		}
	}

	hist, err := r.Turns.Durations.MarshalJSON()
	if err != nil {
		return 0, fmt.Errorf("usage: encode the histogram for %s: %w", s, err)
	}
	var lastEnded int64
	if !r.Turns.LastEndedAt.IsZero() {
		lastEnded = store.EncodeTime(r.Turns.LastEndedAt)
	}
	if _, err = tx.ExecContext(ctx, `
		INSERT INTO usage_turns
			(day, node, agent_id, handle, role, turns, failed, reviewed,
			 first_pass, sent_back, duration_hist, last_ended_at, reads_elided,
			 version)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (day, node, agent_id) DO UPDATE SET
			handle        = excluded.handle,
			role          = excluded.role,
			turns         = excluded.turns,
			failed        = excluded.failed,
			reviewed      = excluded.reviewed,
			first_pass    = excluded.first_pass,
			sent_back     = excluded.sent_back,
			duration_hist = excluded.duration_hist,
			last_ended_at = excluded.last_ended_at,
			reads_elided  = excluded.reads_elided,
			version       = excluded.version`,
		s.Day, s.Node, s.Seat, r.Handle, r.Role, r.Turns.Count, r.Turns.Failed,
		r.Turns.Reviewed, r.Turns.FirstPass, r.Turns.SentBack, string(hist),
		lastEnded, r.ReadsElided, version); err != nil {
		return 0, fmt.Errorf("usage: write the head of %s: %w", s, err)
	}
	rows := 1

	n, err := store.InsertRows(ctx, tx, maxVars, `
		INSERT INTO usage_tokens
			(day, node, agent_id, phase, worker, model, provider_key, input,
			 output, cache_read, cache_write, total, calls) VALUES `,
		`(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, ``, len(r.Tokens),
		func(i int) []any {
			t := r.Tokens[i]
			return []any{s.Day, s.Node, s.Seat, t.Phase, t.Worker, t.Model,
				t.ProviderKey, t.Input, t.Output, t.CacheRead, t.CacheWrite,
				t.Total, t.Calls}
		})
	if err != nil {
		return 0, fmt.Errorf("usage: write the spend of %s: %w", s, err)
	}
	rows += n

	n, err = store.InsertRows(ctx, tx, maxVars, `
		INSERT INTO usage_reads
			(day, node, agent_id, backend, page_id, via, reads, last_at,
			 last_turn_id, last_work_key, last_query) VALUES `,
		`(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, ``, len(r.Reads),
		func(i int) []any {
			rd := r.Reads[i]
			var last int64
			if !rd.LastAt.IsZero() {
				last = store.EncodeTime(rd.LastAt)
			}
			return []any{s.Day, s.Node, s.Seat, rd.Backend, rd.PageID, rd.Via,
				rd.Count, last, rd.LastTurnID, rd.LastWorkKey, rd.LastQuery}
		})
	if err != nil {
		return 0, fmt.Errorf("usage: write the reads of %s: %w", s, err)
	}
	return rows + n, nil
}

// schedule replaces one schedule-day.
func (a Applier) schedule(ctx context.Context, tx *sql.Tx, r Record, at statelog.Position, maxVars int) (int, error) {
	s := r.Subject
	version := at.Packed()
	var held sql.NullInt64
	if err := tx.QueryRowContext(ctx, `
		SELECT MAX(version) FROM usage_schedule_runs
		 WHERE day = ? AND node = ? AND scope_type = ? AND scope_id = ? AND name = ?`,
		s.Day, s.Node, s.ScopeType, s.ScopeID, s.Schedule).Scan(&held); err != nil {
		return 0, fmt.Errorf("usage: read the newest version of %s: %w", s, err)
	}
	if held.Valid && held.Int64 >= version {
		return 0, nil
	}
	if _, err := tx.ExecContext(ctx, `
		DELETE FROM usage_schedule_runs
		 WHERE day = ? AND node = ? AND scope_type = ? AND scope_id = ? AND name = ?`,
		s.Day, s.Node, s.ScopeType, s.ScopeID, s.Schedule); err != nil {
		return 0, fmt.Errorf("usage: clear the fires of %s: %w", s, err)
	}
	n, err := store.InsertRows(ctx, tx, maxVars, `
		INSERT INTO usage_schedule_runs
			(day, node, scope_type, scope_id, name, fired_at, target, outcome,
			 trace_id, turn_id, version) VALUES `,
		`(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, ``, len(r.Fires),
		func(i int) []any {
			f := r.Fires[i]
			return []any{s.Day, s.Node, s.ScopeType, s.ScopeID, s.Schedule,
				store.EncodeTime(f.At), f.Target, f.Outcome, f.TraceID, f.TurnID,
				version}
		})
	if err != nil {
		return 0, fmt.Errorf("usage: write the fires of %s: %w", s, err)
	}
	return n, nil
}

// Gated reports whether this record must produce no rows. NOTHING GATES A
// USAGE RECORD: there is no deletion marker and no eviction that could make a
// node's own day untrue.
func (Applier) Gated(context.Context, *sql.Tx, statelog.Record) (statelog.Reason, bool, error) {
	return "", false, nil
}

// Committed runs after the transaction commits. There is nothing to do:
// nothing waits on a usage record, which is the same fact the absent operation
// ledger states.
func (Applier) Committed(context.Context) {}

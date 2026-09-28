package usage

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/period"
	"github.com/crewlet/crewlet/internal/store"
)

// Estate is the replicated estate as a reader of this domain's rows needs it:
// a read transaction and nothing else. Satisfied by the replicated handle of a
// [store.DB].
type Estate interface {
	Read(ctx context.Context, fn func(*sql.Tx) error) error
}

// SpendRow is one spend cell of one node's day, with the seat named as that
// day's record named it.
type SpendRow struct {
	Day, Node, AgentID string
	Handle, Role       string
	Tokens
}

// SpendQuery is a range of company days, both inclusive, and optionally one
// seat.
type SpendQuery struct {
	// From and To are company day labels (`2026-09-23`).
	From, To string

	// AgentID narrows the read to one seat, by the id every node derives
	// alike; empty reads every seat.
	AgentID string
}

func (q SpendQuery) check() error {
	for _, label := range []string{q.From, q.To} {
		if _, err := period.Parse(period.Day, label, nil); err != nil {
			return fmt.Errorf("usage: a spend range is two company days: %w", err)
		}
	}
	if q.From > q.To {
		return fmt.Errorf("usage: the spend range %s..%s ends before it starts", q.From, q.To)
	}
	return nil
}

// Spend reads every node's spend for the company days q.From..q.To,
// inclusive, in (day, node, seat, cell) order.
//
// EVERY NODE'S ROWS, which is the whole point of the domain: the answer is the
// same on whichever node is asked, and it still includes a node that has left
// the fleet. The labels are the company's own (`2026-09-23`), which sort as
// strings — so the range is a seek on the leading column of every key.
func Spend(ctx context.Context, estate Estate, q SpendQuery) ([]SpendRow, error) {
	if err := q.check(); err != nil {
		return nil, err
	}
	var out []SpendRow
	err := estate.Read(ctx, func(tx *sql.Tx) error {
		// The seat filter is an `= ?` OR an empty argument, rather than
		// two query texts: one statement, and a narrowed read seeks the
		// same day range and drops the other seats' rows on the way.
		rows, err := tx.QueryContext(ctx, `
			SELECT t.day, t.node, t.agent_id,
			       COALESCE(h.handle, ''), COALESCE(h.role, ''),
			       t.phase, t.worker, t.model, t.provider_key,
			       t.input, t.output, t.cache_read, t.cache_write, t.total, t.calls
			  FROM usage_tokens t
			  LEFT JOIN usage_turns h
			    ON h.day = t.day AND h.node = t.node AND h.agent_id = t.agent_id
			 WHERE t.day >= ? AND t.day <= ? AND (? = '' OR t.agent_id = ?)
			 ORDER BY t.day, t.node, t.agent_id, t.phase, t.worker, t.model, t.provider_key`,
			q.From, q.To, q.AgentID, q.AgentID)
		if err != nil {
			return err
		}
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			var r SpendRow
			if err := rows.Scan(&r.Day, &r.Node, &r.AgentID, &r.Handle, &r.Role,
				&r.Phase, &r.Worker, &r.Model, &r.ProviderKey, &r.Input, &r.Output,
				&r.CacheRead, &r.CacheWrite, &r.Total, &r.Calls); err != nil {
				return err
			}
			out = append(out, r)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, fmt.Errorf("usage: read the spend for %s..%s: %w", q.From, q.To, err)
	}
	return out, nil
}

// TurnsRow is one node's head row for one seat on one day: its name as that
// day's record named it, and how many of its turns ended and failed.
type TurnsRow struct {
	Day, Node, AgentID string
	Handle, Role       string
	Turns, Failed      int64
}

// SeatTurns reads every node's turn counts for the company days
// q.From..q.To, inclusive, in (day, node, seat) order — the half of a spend
// answer that says how many turns the tokens were spent over.
//
// A HEAD ROW WITH NO SPEND IS STILL READ: a seat that ended a turn without a
// model call (a turn that failed before its first round) ended a turn, and a
// per-seat table that dropped it would say the seat did nothing.
func SeatTurns(ctx context.Context, estate Estate, q SpendQuery) ([]TurnsRow, error) {
	if err := q.check(); err != nil {
		return nil, err
	}
	var out []TurnsRow
	err := estate.Read(ctx, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, `
			SELECT day, node, agent_id, handle, role, turns, failed
			  FROM usage_turns
			 WHERE day >= ? AND day <= ? AND (? = '' OR agent_id = ?)
			 ORDER BY day, node, agent_id`,
			q.From, q.To, q.AgentID, q.AgentID)
		if err != nil {
			return err
		}
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			var r TurnsRow
			if err := rows.Scan(&r.Day, &r.Node, &r.AgentID, &r.Handle, &r.Role,
				&r.Turns, &r.Failed); err != nil {
				return err
			}
			out = append(out, r)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, fmt.Errorf("usage: read the turns for %s..%s: %w", q.From, q.To, err)
	}
	return out, nil
}

// SeatDay is one node's whole head row for one seat on one day, with the
// tokens that day's spend cells add up to — every number a seat's activity
// answer folds, from one read.
type SeatDay struct {
	Day, Node, AgentID string
	Handle, Role       string

	Turns, Failed, Reviewed, FirstPass, SentBack int64

	// Durations is the day's turn-duration histogram on this node. Merged
	// across days and nodes by adding counts, which is the only way a
	// quantile over both stays exact to the bin (see [Hist]).
	Durations Hist

	// LastEndedAt is when the newest turn this node ended for the seat that
	// day finished; zero when none did.
	LastEndedAt time.Time

	// Tokens is the day's `total` over every spend cell of the seat on this
	// node — input plus output, the cache counts being a breakdown of input.
	Tokens int64
}

// SeatDays reads every node's head row for the company days q.From..q.To,
// inclusive, in (day, node, seat) order, each with its day's token total.
//
// TWO STATEMENTS IN ONE TRANSACTION rather than a join: the head row is the
// record's guard and every seat record writes one, so a token total without a
// head row cannot exist, and summing the spend cells per (day, node, seat) in
// its own GROUP BY keeps the head read a plain seek on `usage_turns`' key. Both
// read the same snapshot, so the pair is one state of the estate.
func SeatDays(ctx context.Context, estate Estate, q SpendQuery) ([]SeatDay, error) {
	if err := q.check(); err != nil {
		return nil, err
	}
	var out []SeatDay
	err := estate.Read(ctx, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, `
			SELECT day, node, agent_id, handle, role, turns, failed, reviewed,
			       first_pass, sent_back, duration_hist, last_ended_at
			  FROM usage_turns
			 WHERE day >= ? AND day <= ? AND (? = '' OR agent_id = ?)
			 ORDER BY day, node, agent_id`,
			q.From, q.To, q.AgentID, q.AgentID)
		if err != nil {
			return err
		}
		at := map[[3]string]int{}
		err = func() error {
			defer func() { _ = rows.Close() }()
			for rows.Next() {
				var r SeatDay
				var hist string
				var lastEnded int64
				if scanErr := rows.Scan(&r.Day, &r.Node, &r.AgentID, &r.Handle, &r.Role,
					&r.Turns, &r.Failed, &r.Reviewed, &r.FirstPass, &r.SentBack,
					&hist, &lastEnded); scanErr != nil {
					return scanErr
				}
				if histErr := r.Durations.UnmarshalJSON([]byte(hist)); histErr != nil {
					return fmt.Errorf("the histogram of %s/%s/%s: %w", r.Day, r.Node, r.AgentID, histErr)
				}
				if lastEnded != 0 {
					r.LastEndedAt = store.DecodeTime(lastEnded)
				}
				at[[3]string{r.Day, r.Node, r.AgentID}] = len(out)
				out = append(out, r)
			}
			return rows.Err()
		}()
		if err != nil {
			return err
		}
		sums, err := tx.QueryContext(ctx, `
			SELECT day, node, agent_id, SUM(total)
			  FROM usage_tokens
			 WHERE day >= ? AND day <= ? AND (? = '' OR agent_id = ?)
			 GROUP BY day, node, agent_id`,
			q.From, q.To, q.AgentID, q.AgentID)
		if err != nil {
			return err
		}
		defer func() { _ = sums.Close() }()
		for sums.Next() {
			var day, node, agent string
			var total int64
			if err := sums.Scan(&day, &node, &agent, &total); err != nil {
				return err
			}
			if i, ok := at[[3]string{day, node, agent}]; ok {
				out[i].Tokens = total
			}
		}
		return sums.Err()
	})
	if err != nil {
		return nil, fmt.Errorf("usage: read the seat days for %s..%s: %w", q.From, q.To, err)
	}
	return out, nil
}

// ScheduleRun is one schedule fire some node dispatched, as the company feed
// reads it.
type ScheduleRun struct {
	Day, Node          string
	ScopeType, ScopeID string
	Name               string
	FiredAt            time.Time
	Target, Outcome    string
	TraceID, TurnID    string
}

// Key is the run's tiebreak among fires at one instant, and with FiredAt its
// keyset position: the primary key's columns other than the day, which
// `fired_at` already implies.
func (r ScheduleRun) Key() string {
	return strings.Join([]string{r.Node, r.ScopeType, r.ScopeID, r.Name, r.Target}, "\x1f")
}

// ScheduleRunsQuery is one page of every node's schedule fires, newest first.
type ScheduleRunsQuery struct {
	// BeforeAt and BeforeKey resume strictly after the last row a reader
	// was handed — see [ScheduleRun.Key]. A zero BeforeAt is the newest page.
	BeforeAt  time.Time
	BeforeKey string

	// Target narrows to the fires addressed to one seat.
	Target string

	// Outcome narrows to the fires the ledger recorded under one outcome
	// (`fired`, `skipped_catchup`, `skipped_paused`); empty is every one.
	Outcome string

	// Limit is how many rows; one more is read to say whether there are
	// more.
	Limit int
}

// ScheduleRuns reads one page of the schedule fires every node recorded,
// newest first, through `usage_schedule_runs_fired_idx` (replicated 0029).
//
// EVERY NODE'S FIRES, which is the domain's point: a schedule fires on
// whichever node holds its duty, and the company's account of its schedules
// is the union — including a node that has since left. The horizon is the
// applier's (181 days); nothing older is here to page to.
func ScheduleRuns(ctx context.Context, estate Estate, q ScheduleRunsQuery) (
	[]ScheduleRun, bool, error) {

	if q.Limit < 1 {
		return nil, false, fmt.Errorf("usage: a schedule-run page is at least one row, not %d", q.Limit)
	}
	where := "1 = 1"
	var args []any
	if !q.BeforeAt.IsZero() {
		at := store.EncodeTime(q.BeforeAt)
		where += ` AND fired_at <= ? AND NOT (fired_at = ? AND
			(node || char(31) || scope_type || char(31) || scope_id || char(31) ||
			 name || char(31) || target) >= ?)`
		args = append(args, at, at, q.BeforeKey)
	}
	if q.Target != "" {
		where += " AND target = ?"
		args = append(args, q.Target)
	}
	if q.Outcome != "" {
		where += " AND outcome = ?"
		args = append(args, q.Outcome)
	}
	args = append(args, q.Limit+1)
	var out []ScheduleRun
	err := estate.Read(ctx, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, `
			SELECT day, node, scope_type, scope_id, name, fired_at, target,
			       outcome, trace_id, turn_id
			  FROM usage_schedule_runs
			 WHERE `+where+`
			 ORDER BY fired_at DESC, node DESC, scope_type DESC, scope_id DESC,
			          name DESC, target DESC
			 LIMIT ?`, args...)
		if err != nil {
			return err
		}
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			var r ScheduleRun
			var fired int64
			if err := rows.Scan(&r.Day, &r.Node, &r.ScopeType, &r.ScopeID, &r.Name,
				&fired, &r.Target, &r.Outcome, &r.TraceID, &r.TurnID); err != nil {
				return err
			}
			r.FiredAt = store.DecodeTime(fired)
			out = append(out, r)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, false, fmt.Errorf("usage: read the schedule runs: %w", err)
	}
	more := len(out) > q.Limit
	if more {
		out = out[:q.Limit]
	}
	return out, more, nil
}

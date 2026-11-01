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

// Spend reads every data node's spend for the company days q.From..q.To,
// inclusive, in (day, node, seat, cell) order.
//
// EVERY DATA NODE'S ROWS, which is the whole point of the domain: the answer is
// the same on whichever node is asked, and it still includes a node that has
// left the fleet. The labels are the company's own (`2026-09-23`), which sort
// as strings — so the range is a seek on the leading column of every key.
func Spend(ctx context.Context, estate Estate, q SpendQuery) ([]SpendRow, error) {
	page, err := SpendPage(ctx, estate, q, nil, 0)
	return page.Rows, err
}

// TurnsRow is one node's head row for one seat on one day: its name as that
// day's record named it, and how many of its turns ended and failed.
type TurnsRow struct {
	Day, Node, AgentID string
	Handle, Role       string
	Turns, Failed      int64
}

// SeatTurns reads every data node's turn counts for the company days
// q.From..q.To, inclusive, in (day, node, seat) order — the half of a spend
// answer that says how many turns the tokens were spent over.
//
// A HEAD ROW WITH NO SPEND IS STILL READ: a seat that ended a turn without a
// model call (a turn that failed before its first round) ended a turn, and a
// per-seat table that dropped it would say the seat did nothing.
func SeatTurns(ctx context.Context, estate Estate, q SpendQuery) ([]TurnsRow, error) {
	page, err := SeatTurnsPage(ctx, estate, q, nil, 0)
	return page.Rows, err
}

// PersonRow is one spend cell of one node's day for one PERSON: the human seat
// a person's questions (or a unit they lead's passes) were spent for, named by
// its handle and its role as the record named them.
type PersonRow struct {
	Day, Node, Person, Role string
	Tokens
}

// PersonQuery is a range of company days, both inclusive, and optionally one
// person by their seat's handle.
type PersonQuery struct {
	From, To string
	Person   string
}

// PersonSpend reads every data node's spend for people over the company days
// q.From..q.To, inclusive, in (day, node, person, cell) order — the half of a
// named spend window no seat's row carries, since a person has no agent id.
func PersonSpend(ctx context.Context, estate Estate, q PersonQuery) ([]PersonRow, error) {
	page, err := PersonSpendPage(ctx, estate, q, nil, 0)
	return page.Rows, err
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

	// Code is the seat's changes opened, reviews given and findings
	// received on the code host that day on this node; zeros for none.
	Code CodeDay
}

// SeatDays reads every data node's head row for the company days q.From..q.To,
// inclusive, in (day, node, seat) order, each with its day's token total.
//
// TWO STATEMENTS IN ONE TRANSACTION rather than a join, per page
// ([SeatDaysPage]): the head row is the record's guard and every seat record
// writes one, so a token total without a head row cannot exist, and summing
// the spend cells per (day, node, seat) in its own GROUP BY keeps the head read
// a plain seek on `usage_turns`' key. Both read the same snapshot, so the pair
// is one state of the estate.
func SeatDays(ctx context.Context, estate Estate, q SpendQuery) ([]SeatDay, error) {
	page, err := SeatDaysPage(ctx, estate, q, nil, 0)
	return page.Rows, err
}

// scanSeatDay reads one head row of company day day, as [SeatDaysPage]'s
// statement selects it.
func scanSeatDay(rows *sql.Rows, day string) (SeatDay, error) {
	r := SeatDay{Day: day}
	var hist string
	var lastEnded int64
	if err := rows.Scan(&r.Node, &r.AgentID, &r.Handle, &r.Role,
		&r.Turns, &r.Failed, &r.Reviewed, &r.FirstPass, &r.SentBack,
		&hist, &lastEnded, &r.Code.ChangesOpened, &r.Code.ReviewsGiven, &r.Code.FindingsReceived); err != nil {
		return r, err
	}
	if err := r.Durations.UnmarshalJSON([]byte(hist)); err != nil {
		return r, fmt.Errorf("the histogram of %s/%s/%s: %w", r.Day, r.Node, r.AgentID, err)
	}
	if lastEnded != 0 {
		r.LastEndedAt = store.DecodeTime(lastEnded)
	}
	return r, nil
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

// ScheduleRunsQuery is one page of every data node's schedule fires, newest
// first.
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

// ScheduleRuns reads one page of the schedule fires every data node recorded,
// newest first, through `usage_schedule_runs_fired_idx` (replicated 0029).
//
// EVERY DATA NODE'S FIRES, which is the domain's point: a schedule fires on
// whichever node holds its duty (a duty takes `workers`, which takes `data`),
// and the company's account of its schedules is the union — including a node
// that has since left. The horizon is the applier's (181 days); nothing older
// is here to page to.
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

package usage

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"iter"
	"slices"
	"strings"
)

// A READ IN PAGES, AND WHERE A PAGE MAY END.
//
// A window of this domain's rows is large: ninety days of a three-node,
// forty-seat company is some ten thousand (day, node, seat) groups of several
// spend cells each, tens of megabytes of rows. Read in-process that is one
// transaction and no wire; answered by ANOTHER data node — the case a node
// whose usage copy is out of service routes to (ADR-0049) — it crosses one
// message, which carries at most [queue.MaxPayloadBytes]. So every read that
// can grow with a window has a PAGED form ([SpendPage], [SeatTurnsPage],
// [PersonSpendPage], [SeatDaysPage], [PageReadsPage]), and the whole read is
// that form asked for one unbounded page: one implementation, one order, one
// rule for where a page ends ([Fill]).
//
// A PAGE PAYS FOR ITS OWN ROWS. Every paged read walks its window one LEADING
// KEY at a time — one company day for the day-keyed reads ([walkDays]), one
// page id for a page's readers — each by a statement of its own that SEEKS
// that key and streams in the read's order, inside the page's one transaction.
// [Fill] stops the walk where the page ends, so nothing past it is read, and a
// page that resumes inside a key re-reads at most that one key's rows ahead of
// its own. One statement over the whole window was a filter, never a seek: the
// keyset a page resumed after was applied to rows the statement had already
// read, joined and SORTED from the window's first day, so every page paid for
// the window from its resumption to the end, and the cost of a walk grew with
// the square of the window. The package doc has the measurement.

// Group is the key of the rows ONE RECORD writes, in a read's order — the
// columns a page may be cut between and nowhere else.
//
// A PAGE IS CUT ONLY BETWEEN TWO GROUPS, because pages are separate reads —
// separate transactions, possibly on separate nodes — and an apply REPLACES a
// record's rows whole: a cut inside a group could hand a reader the first
// cells of one version of a seat's day and the rest of the next, a total
// neither version ever had.
type Group []string

// ErrBadCursor is a page's resumption no read of this domain issued: a cursor
// that does not decode, or a group of another shape than the read it resumes
// is cut by.
var ErrBadCursor = errors.New("usage: the cursor is not one this read issued")

// Cursor is g as a surface hands its reader the next page's resumption:
// opaque and URL-safe, so a client carries it back as it came.
func (g Group) Cursor() string {
	raw, err := json.Marshal([]string(g))
	if err != nil {
		// A slice of strings always encodes.
		panic(err)
	}
	return base64.RawURLEncoding.EncodeToString(raw)
}

// ParseCursor is the group a [Group.Cursor] encodes, and the start of a read
// for "". Whether its shape is the read's is the read's to judge
// ([resumeAfter]).
func ParseCursor(s string) (Group, error) {
	if s == "" {
		return nil, nil
	}
	// EACH REFUSAL A SENTENCE FOR THE CALLER past the sentinel, which a
	// surface quotes alone: what is wrong with the cursor they handed back.
	raw, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return nil, fmt.Errorf("%w: the cursor does not decode (%v) — hand back a page's `next` unchanged",
			ErrBadCursor, err)
	}
	var g []string
	if err := json.Unmarshal(raw, &g); err != nil || len(g) == 0 {
		return nil, fmt.Errorf("%w: the cursor names no page's end — hand back a page's `next` unchanged",
			ErrBadCursor)
	}
	return Group(g), nil
}

// Before reports whether g sorts before h, column by column, each column
// compared byte-wise — the order the statements' BINARY collation sorts the
// same TEXT columns in. An empty group (the start) sorts before every other.
func (g Group) Before(h Group) bool {
	if len(g) == 0 || len(h) == 0 {
		return len(g) == 0 && len(h) > 0
	}
	for i := range min(len(g), len(h)) {
		if g[i] != h[i] {
			return g[i] < h[i]
		}
	}
	return len(g) < len(h)
}

// Page is one page of a paged read: its rows, the group it ended on — the
// after of the page that follows — and whether the read is done.
type Page[R any] struct {
	Rows []R
	Next Group
	Done bool

	// measured holds each row's encoding where [Fill] made one to measure
	// the page, which [Page.AppendJSON] writes rather than encoding the row
	// a second time.
	measured
}

// measured is the encodings [Fill] measured a bounded page's rows by, in the
// rows' order — none on a page that was not bounded, since nothing measured
// it. Embedded rather than named so a page's own fields stay exactly Rows,
// Next and Done, which is what crosses the wire.
type measured struct {
	encoded []json.RawMessage
}

// AppendJSON appends p's JSON encoding to dst — exactly the bytes
// [json.Marshal] writes for p — reusing each row's encoding [Fill] made to
// measure the page, so a page bound for the wire is encoded ONCE. Encoding it
// with json.Marshal instead encoded every row twice: once to measure it, once
// to send it. A page nothing measured encodes its rows here.
func (p Page[R]) AppendJSON(dst []byte) ([]byte, error) {
	dst = append(dst, `{"Rows":`...)
	switch {
	case p.Rows == nil:
		dst = append(dst, "null"...)
	case len(p.encoded) == len(p.Rows):
		dst = append(dst, '[')
		for i, row := range p.encoded {
			if i > 0 {
				dst = append(dst, ',')
			}
			dst = append(dst, row...)
		}
		dst = append(dst, ']')
	default:
		rows, err := json.Marshal(p.Rows)
		if err != nil {
			return dst, fmt.Errorf("usage: encode a page's rows: %w", err)
		}
		dst = append(dst, rows...)
	}
	next, err := json.Marshal(p.Next)
	if err != nil {
		return dst, fmt.Errorf("usage: encode where a page ended: %w", err)
	}
	dst = append(dst, `,"Next":`...)
	dst = append(dst, next...)
	if p.Done {
		return append(dst, `,"Done":true}`...), nil
	}
	return append(dst, `,"Done":false}`...), nil
}

// Fill is the ONE cut rule every paged read here uses: it appends rows,
// summing each one's JSON encoding (the encoding a page crosses the wire in,
// kept for [Page.AppendJSON]), and stops at the FIRST group boundary once the
// sum has reached budget. A budget of zero or less is unbounded: one page
// holds every row and is done, and nothing is encoded.
//
// A page that stopped early ends on its last row's group (Next) and is not
// done; one that ran out of rows is done, with no Next. A row error ends the
// fill with that error. A GROUP IS NEVER SPLIT, so a page may run past budget
// by at most one group — one record's rows, which a record being one message
// bounds ([ReadsPerSeatDay] is what keeps a seat-day's under 1 MiB).
//
// Exported because the transport that carries a page holds its own budget to
// this same rule (internal/estate).
func Fill[R any](budget int, groupOf func(R) Group, rows iter.Seq2[R, error]) (Page[R], error) {
	var page Page[R]
	var last Group
	size := 0
	for row, err := range rows {
		if err != nil {
			return Page[R]{}, err
		}
		group := groupOf(row)
		if budget > 0 && size >= budget && len(page.Rows) > 0 && !slices.Equal(group, last) {
			page.Next = last
			return page, nil
		}
		if budget > 0 {
			encoded, err := json.Marshal(row)
			if err != nil {
				return Page[R]{}, fmt.Errorf("usage: measure a row of a page: %w", err)
			}
			size += len(encoded)
			page.encoded = append(page.encoded, encoded)
		}
		page.Rows = append(page.Rows, row)
		last = group
	}
	page.Done = true
	return page, nil
}

// Group is the key of the record that wrote r: a seat's day on one node.
func (r SpendRow) Group() Group { return Group{r.Day, r.Node, r.AgentID} }

// Group is the key of the record that wrote r: a seat's day on one node.
func (r TurnsRow) Group() Group { return Group{r.Day, r.Node, r.AgentID} }

// Group is the key of the record that wrote r: a person's day on one node.
func (r PersonRow) Group() Group { return Group{r.Day, r.Node, r.Person} }

// Group is the key of the record that wrote r: a seat's day on one node.
func (r SeatDay) Group() Group { return Group{r.Day, r.Node, r.AgentID} }

// Group is one page's entries in the record that wrote r — a seat's day on
// one node — which is what the read's (page, day, node, seat) order keeps
// together: every way that seat reached that page that day.
func (r PageRead) Group() Group { return Group{r.PageID, r.Day, r.Node, r.AgentID} }

// resumption is where a paged read picks its walk up: the LEADING KEY of the
// group the page before it ended on, and the keyset over the rest of that
// group — which holds on that one key alone, since every later key is read
// whole.
type resumption struct {
	// after is the group the page before ended on; empty for a first page,
	// which starts at the window's first key with no keyset.
	after Group

	// past is the predicate over the group's columns after the leading key
	// that holds for every row past after, and args its arguments.
	past string
	args []any
}

// key is the leading key the walk resumes at, or "" for a first page.
func (r resumption) key() string {
	if len(r.after) == 0 {
		return ""
	}
	return r.after[0]
}

// on is the keyset a statement over leading key k takes: past on the key the
// page resumes in, and `1 = 1` on every other.
func (r resumption) on(k string) (string, []any) {
	if len(r.after) == 0 || k != r.after[0] {
		return "1 = 1", nil
	}
	return r.past, r.args
}

// resumeAfter is the resumption of a read keyed on a leading column followed
// by rest, after the group after.
//
// A group of the wrong arity is refused rather than compared on its prefix: a
// resumption that names fewer columns than a record's key resumes inside a
// record, which is the one place a page may not begin.
func resumeAfter(read string, rest []string, after Group) (resumption, error) {
	if len(after) == 0 {
		return resumption{}, nil
	}
	if len(after) != 1+len(rest) {
		return resumption{}, fmt.Errorf("%w: a %s page resumes after a group of %d columns, not %d",
			ErrBadCursor, read, 1+len(rest), len(after))
	}
	past, args := keyset(rest, after[1:])
	return resumption{after: after, past: past, args: args}, nil
}

// keyset is the predicate over cols that holds for every row whose values sort
// past g, written as nested comparisons — `(c1 > ? OR (c1 = ? AND c2 > ?))` —
// beside a statement's own WHERE.
func keyset(cols []string, g Group) (string, []any) {
	last := len(cols) - 1
	clause := cols[last] + " > ?"
	args := []any{g[last]}
	for i := last - 1; i >= 0; i-- {
		clause = "(" + cols[i] + " > ? OR (" + cols[i] + " = ? AND " + clause + "))"
		args = append([]any{g[i], g[i]}, args...)
	}
	return clause, args
}

// walkDays is a day-keyed read's rows over the company days from..to, one
// day per statement, in day order: each day the table holds a row on is found
// by one seek on the table's day index ([nextDay]) and read by day, which
// hands back that day's rows — a statement that seeks `day = ?` — under the
// keyset its resumption puts on it.
//
// FOUND, NEVER COUNTED OUT: a day nobody recorded anything on costs nothing
// past its one seek, so the walk's cost is the days that hold rows however
// long a range a caller names — and the applier keeps at most [History] of
// them.
func walkDays[R any](ctx context.Context, tx *sql.Tx, table, from, to string, resume resumption,
	day func(day, past string, pastArgs []any) iter.Seq2[R, error]) iter.Seq2[R, error] {

	return func(yield func(R, error) bool) {
		var zero R
		cursor, inclusive := from, true
		if k := resume.key(); k > from {
			cursor = k
		}
		for {
			next, found, err := nextDay(ctx, tx, table, cursor, inclusive, to)
			if err != nil {
				yield(zero, err)
				return
			}
			if !found {
				return
			}
			past, pastArgs := resume.on(next)
			for r, err := range day(next, past, pastArgs) {
				if !yield(r, err) || err != nil {
					return
				}
			}
			cursor, inclusive = next, false
		}
	}
}

// nextDay is the first company day after from — or at it, when inclusive —
// and at or before to on which table holds a row, and whether there is one.
// table is one of this package's own, never a caller's.
func nextDay(ctx context.Context, tx *sql.Tx, table, from string, inclusive bool, to string) (
	string, bool, error) {

	var day sql.NullString
	if err := tx.QueryRowContext(ctx, nextDayStatement(table, inclusive), from, to).Scan(&day); err != nil {
		return "", false, fmt.Errorf("find the next day of %s after %s: %w", table, from, err)
	}
	return day.String, day.Valid, nil
}

// nextDayStatement is [nextDay]'s seek: the least day of table in a range,
// read off the head of an index whose first column is the day.
func nextDayStatement(table string, inclusive bool) string {
	op := ">"
	if inclusive {
		op = ">="
	}
	return `SELECT MIN(day) FROM ` + table + ` WHERE day ` + op + ` ? AND day <= ?`
}

// scanned is a statement's rows, each read by scan, as a sequence that closes
// the cursor when its reader stops early — which is what [Fill] does at a
// page's end.
func scanned[R any](ctx context.Context, tx *sql.Tx, query string, args []any,
	scan func(*sql.Rows) (R, error)) iter.Seq2[R, error] {

	return func(yield func(R, error) bool) {
		var zero R
		rows, err := tx.QueryContext(ctx, query, args...)
		if err != nil {
			yield(zero, err)
			return
		}
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			r, err := scan(rows)
			if !yield(r, err) || err != nil {
				return
			}
		}
		if err := rows.Err(); err != nil {
			yield(zero, err)
		}
	}
}

// The statements each paged read issues, one leading key at a time. NAMED so
// the plan gate (TestEveryPagedStatementSeeksItsKeyAndStreams) explains the
// very text a page runs: each must SEEK its leading key and need no sorter,
// or a page's cost is the window's again.
const (
	// spendDayStatement is one company day's spend cells, in the read's
	// order — the table's own key, so the day streams as stored.
	spendDayStatement = `
		SELECT node, agent_id, phase, worker, model, provider_key,
		       input, output, cache_read, cache_write, total, calls
		  FROM usage_tokens
		 WHERE day = ? AND (? = '' OR agent_id = ?) AND %s
		 ORDER BY day, node, agent_id, phase, worker, model, provider_key`

	// dayNamesStatement is one company day's head rows' names, which
	// the spend cells are named by.
	dayNamesStatement = `
		SELECT node, agent_id, handle, role
		  FROM usage_turns
		 WHERE day = ? AND (? = '' OR agent_id = ?) AND %s`

	// turnsDayStatement is one company day's head rows' turn counts.
	turnsDayStatement = `
		SELECT node, agent_id, handle, role, turns, failed
		  FROM usage_turns
		 WHERE day = ? AND (? = '' OR agent_id = ?) AND %s
		 ORDER BY day, node, agent_id`

	// personDayStatement is one company day's spend cells for people.
	personDayStatement = `
		SELECT node, person, role, phase, worker, model, provider_key,
		       input, output, cache_read, cache_write, total, calls
		  FROM usage_person_tokens
		 WHERE day = ? AND (? = '' OR person = ?) AND %s
		 ORDER BY day, node, person, phase, worker, model, provider_key`

	// seatDayStatement is one company day's whole head rows.
	seatDayStatement = `
		SELECT node, agent_id, handle, role, turns, failed, reviewed,
		       first_pass, sent_back, duration_hist, last_ended_at,
		       changes_opened, reviews_given, findings_received
		  FROM usage_turns
		 WHERE day = ? AND (? = '' OR agent_id = ?) AND %s
		 ORDER BY day, node, agent_id`

	// seatDayTokensStatement is one company day's token total per seat
	// head, which a seat day carries beside its head row.
	seatDayTokensStatement = `
		SELECT node, agent_id, SUM(total)
		  FROM usage_tokens
		 WHERE day = ? AND (? = '' OR agent_id = ?) AND %s
		 GROUP BY node, agent_id`

	// pageReadsStatement is one page id's reads from a day on, in the
	// read's order — usage_reads_page_idx's own (replicated 0050) — each
	// named by its seat's head row of that day, and narrowed to the ways
	// of reaching it by the %s it is formatted with.
	pageReadsStatement = `
		SELECT r.page_id, r.day, r.node, r.agent_id,
		       COALESCE(h.handle, ''), COALESCE(h.role, ''),
		       r.backend, r.via, r.reads, r.last_at,
		       r.last_turn_id, r.last_work_key, r.last_query
		  FROM usage_reads r
		  LEFT JOIN usage_turns h
		    ON h.day = r.day AND h.node = r.node AND h.agent_id = r.agent_id
		 WHERE r.page_id = ? AND r.day >= ? AND r.day <= ? AND %s
		 ORDER BY r.page_id, r.day, r.node, r.agent_id, r.via`
)

// The tables each day-keyed read walks the days of: the rows the read is of,
// never the rows that only name them.
const (
	tokensTable       = "usage_tokens"
	turnsTable        = "usage_turns"
	personTokensTable = "usage_person_tokens"
)

// SpendPage is one page of [Spend], resuming after the group after (empty for
// the first) and cut by [Fill] at budget bytes.
func SpendPage(ctx context.Context, estate Estate, q SpendQuery, after Group,
	budget int) (Page[SpendRow], error) {

	if err := q.check(); err != nil {
		return Page[SpendRow]{}, err
	}
	resume, err := resumeAfter("spend", []string{"node", "agent_id"}, after)
	if err != nil {
		return Page[SpendRow]{}, err
	}
	var page Page[SpendRow]
	err = estate.Read(ctx, func(tx *sql.Tx) error {
		var fillErr error
		page, fillErr = Fill(budget, SpendRow.Group, walkDays(ctx, tx, tokensTable, q.From, q.To, resume,
			func(day, past string, pastArgs []any) iter.Seq2[SpendRow, error] {
				return spendDay(ctx, tx, day, q.AgentID, past, pastArgs)
			}))
		return fillErr
	})
	if err != nil {
		return Page[SpendRow]{}, fmt.Errorf("usage: read the spend for %s..%s: %w", q.From, q.To, err)
	}
	return page, nil
}

// spendDay is one company day's spend cells, each named as its seat's head
// row of that day named it, and named empty where the day wrote no head for
// the cells — which is what the reader's fold expects of a seat with no name.
//
// THE NAMES ARE READ BESIDE THE CELLS, one statement for the day's heads and
// one for its cells, joined here: a day's heads are one per (node, seat), a
// few hundred, while a join probed the head table once per CELL, and that
// probe cost the read half as much again as reading the cells did.
func spendDay(ctx context.Context, tx *sql.Tx, day, agent, past string,
	pastArgs []any) iter.Seq2[SpendRow, error] {

	return func(yield func(SpendRow, error) bool) {
		names, err := dayNames(ctx, tx, day, agent, past, pastArgs)
		if err != nil {
			yield(SpendRow{}, err)
			return
		}
		args := append([]any{day, agent, agent}, pastArgs...)
		for r, err := range scanned(ctx, tx, fmt.Sprintf(spendDayStatement, past), args,
			func(rows *sql.Rows) (SpendRow, error) {
				r := SpendRow{Day: day}
				scanErr := rows.Scan(&r.Node, &r.AgentID, &r.Phase, &r.Worker, &r.Model,
					&r.ProviderKey, &r.Input, &r.Output, &r.CacheRead, &r.CacheWrite,
					&r.Total, &r.Calls)
				named := names[seatOn{r.Node, r.AgentID}]
				r.Handle, r.Role = named.handle, named.role
				return r, scanErr
			}) {
			if !yield(r, err) || err != nil {
				return
			}
		}
	}
}

// seatOn is one seat on one node, within one company day.
type seatOn struct{ node, agent string }

// seatName is a seat as one day's head row named it.
type seatName struct{ handle, role string }

// dayNames is every seat's name on one company day, as that day's head rows
// named them, under the keyset its walk puts on the day.
func dayNames(ctx context.Context, tx *sql.Tx, day, agent, past string,
	pastArgs []any) (map[seatOn]seatName, error) {

	type named struct {
		seatOn
		seatName
	}
	names := map[seatOn]seatName{}
	args := append([]any{day, agent, agent}, pastArgs...)
	for named, err := range scanned(ctx, tx, fmt.Sprintf(dayNamesStatement, past), args,
		func(rows *sql.Rows) (named, error) {
			var r named
			return r, rows.Scan(&r.node, &r.agent, &r.handle, &r.role)
		}) {
		if err != nil {
			return nil, err
		}
		names[named.seatOn] = named.seatName
	}
	return names, nil
}

// SeatTurnsPage is one page of [SeatTurns], as [SpendPage] is of [Spend].
func SeatTurnsPage(ctx context.Context, estate Estate, q SpendQuery, after Group,
	budget int) (Page[TurnsRow], error) {

	if err := q.check(); err != nil {
		return Page[TurnsRow]{}, err
	}
	resume, err := resumeAfter("turns", []string{"node", "agent_id"}, after)
	if err != nil {
		return Page[TurnsRow]{}, err
	}
	var page Page[TurnsRow]
	err = estate.Read(ctx, func(tx *sql.Tx) error {
		var fillErr error
		page, fillErr = Fill(budget, TurnsRow.Group, walkDays(ctx, tx, turnsTable, q.From, q.To, resume,
			func(day, past string, pastArgs []any) iter.Seq2[TurnsRow, error] {
				return scanned(ctx, tx, fmt.Sprintf(turnsDayStatement, past),
					append([]any{day, q.AgentID, q.AgentID}, pastArgs...),
					func(rows *sql.Rows) (TurnsRow, error) {
						r := TurnsRow{Day: day}
						return r, rows.Scan(&r.Node, &r.AgentID, &r.Handle, &r.Role,
							&r.Turns, &r.Failed)
					})
			}))
		return fillErr
	})
	if err != nil {
		return Page[TurnsRow]{}, fmt.Errorf("usage: read the turns for %s..%s: %w", q.From, q.To, err)
	}
	return page, nil
}

// PersonSpendPage is one page of [PersonSpend], as [SpendPage] is of [Spend].
func PersonSpendPage(ctx context.Context, estate Estate, q PersonQuery, after Group,
	budget int) (Page[PersonRow], error) {

	if err := (SpendQuery{From: q.From, To: q.To}).check(); err != nil {
		return Page[PersonRow]{}, err
	}
	resume, err := resumeAfter("people's spend", []string{"node", "person"}, after)
	if err != nil {
		return Page[PersonRow]{}, err
	}
	var page Page[PersonRow]
	err = estate.Read(ctx, func(tx *sql.Tx) error {
		var fillErr error
		page, fillErr = Fill(budget, PersonRow.Group, walkDays(ctx, tx, personTokensTable, q.From, q.To, resume,
			func(day, past string, pastArgs []any) iter.Seq2[PersonRow, error] {
				return scanned(ctx, tx, fmt.Sprintf(personDayStatement, past),
					append([]any{day, q.Person, q.Person}, pastArgs...),
					func(rows *sql.Rows) (PersonRow, error) {
						r := PersonRow{Day: day}
						return r, rows.Scan(&r.Node, &r.Person, &r.Role, &r.Phase, &r.Worker,
							&r.Model, &r.ProviderKey, &r.Input, &r.Output, &r.CacheRead,
							&r.CacheWrite, &r.Total, &r.Calls)
					})
			}))
		return fillErr
	})
	if err != nil {
		return Page[PersonRow]{}, fmt.Errorf("usage: read the people's spend for %s..%s: %w",
			q.From, q.To, err)
	}
	return page, nil
}

// SeatDaysPage is one page of [SeatDays], as [SpendPage] is of [Spend].
//
// EACH DAY'S TOKEN TOTALS are summed by their own statement, over that day
// and under the keyset the walk puts on it, in the page's one transaction —
// so a seat day's total is of the same state of the estate as its head row,
// and a page sums no day it does not read. A total for a seat past where the
// page ends is summed with the rest of its day and never handed back.
func SeatDaysPage(ctx context.Context, estate Estate, q SpendQuery, after Group,
	budget int) (Page[SeatDay], error) {

	if err := q.check(); err != nil {
		return Page[SeatDay]{}, err
	}
	resume, err := resumeAfter("seat-day", []string{"node", "agent_id"}, after)
	if err != nil {
		return Page[SeatDay]{}, err
	}
	var page Page[SeatDay]
	err = estate.Read(ctx, func(tx *sql.Tx) error {
		var fillErr error
		page, fillErr = Fill(budget, SeatDay.Group, walkDays(ctx, tx, turnsTable, q.From, q.To, resume,
			func(day, past string, pastArgs []any) iter.Seq2[SeatDay, error] {
				return seatDaysOn(ctx, tx, day, q.AgentID, past, pastArgs)
			}))
		return fillErr
	})
	if err != nil {
		return Page[SeatDay]{}, fmt.Errorf("usage: read the seat days for %s..%s: %w", q.From, q.To, err)
	}
	return page, nil
}

// seatDaysOn is one company day's head rows, each with that day's token
// total for its seat on its node.
func seatDaysOn(ctx context.Context, tx *sql.Tx, day, agent, past string,
	pastArgs []any) iter.Seq2[SeatDay, error] {

	return func(yield func(SeatDay, error) bool) {
		type summed struct {
			seatOn
			total int64
		}
		args := append([]any{day, agent, agent}, pastArgs...)
		totals := map[seatOn]int64{}
		for sum, err := range scanned(ctx, tx, fmt.Sprintf(seatDayTokensStatement, past), args,
			func(rows *sql.Rows) (summed, error) {
				var r summed
				return r, rows.Scan(&r.node, &r.agent, &r.total)
			}) {
			if err != nil {
				yield(SeatDay{}, err)
				return
			}
			totals[sum.seatOn] = sum.total
		}
		for r, err := range scanned(ctx, tx, fmt.Sprintf(seatDayStatement, past), args,
			func(rows *sql.Rows) (SeatDay, error) {
				r, scanErr := scanSeatDay(rows, day)
				r.Tokens = totals[seatOn{r.Node, r.AgentID}]
				return r, scanErr
			}) {
			if !yield(r, err) || err != nil {
				return
			}
		}
	}
}

// PageReadsPage is one page of [PageReads], as [SpendPage] is of [Spend].
//
// ONE PAGE ID PER STATEMENT, the ids sorted and de-duplicated once, so the
// statements' answers are the read's order end to end: an id before the one
// the page resumes in is never read, that one is read from the day the page
// before ended on — a seek on usage_reads_page_idx, under the keyset over the
// rest of its group — and every later id is read whole. A statement over a
// CHUNK of ids sorted every row of the chunk before handing back its first,
// so every page re-read the chunk it resumed in from the start.
func PageReadsPage(ctx context.Context, estate Estate, q PageReadsQuery, after Group,
	budget int) (Page[PageRead], error) {

	if err := (SpendQuery{From: q.From, To: q.To}).check(); err != nil {
		return Page[PageRead]{}, err
	}
	ids := slices.Compact(slices.Sorted(slices.Values(slices.DeleteFunc(slices.Clone(q.PageIDs),
		func(id string) bool { return strings.TrimSpace(id) == "" }))))
	if len(ids) == 0 {
		return Page[PageRead]{}, fmt.Errorf("usage: a page's reads name the page")
	}
	resume, err := resumeAfter("page-reads", []string{"r.day", "r.node", "r.agent_id"}, after)
	if err != nil {
		return Page[PageRead]{}, err
	}
	var page Page[PageRead]
	err = estate.Read(ctx, func(tx *sql.Tx) error {
		walk := func(yield func(PageRead, error) bool) {
			first, _ := slices.BinarySearch(ids, resume.key())
			for _, id := range ids[first:] {
				from := q.From
				if id == resume.key() {
					from = max(from, resume.after[1])
				}
				past, pastArgs := resume.on(id)
				for r, err := range pageReadRows(ctx, tx, id, from, q, past, pastArgs) {
					if !yield(r, err) || err != nil {
						return
					}
				}
			}
		}
		var fillErr error
		page, fillErr = Fill(budget, PageRead.Group, walk)
		return fillErr
	})
	if err != nil {
		return Page[PageRead]{}, fmt.Errorf("usage: read the reads of %d pages for %s..%s: %w",
			len(ids), q.From, q.To, err)
	}
	return page, nil
}

// pageReadsWhere is the narrowing [pageReadsStatement] is formatted with:
// past, and the ways of reaching a page q names, if it names any.
func pageReadsWhere(q PageReadsQuery, past string) string {
	if len(q.Vias) == 0 {
		return past
	}
	return past + " AND r.via IN (" + strings.TrimSuffix(strings.Repeat("?,", len(q.Vias)), ",") + ")"
}

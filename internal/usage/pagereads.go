package usage

import (
	"context"
	"database/sql"
	"fmt"
	"iter"
	"time"

	"github.com/crewlet/crewlet/internal/store"
)

// PageRead is one node's reads of one page by one seat on one day, through one
// way of reaching it (a `via`: the knowledge_read event's own vocabulary), with the seat
// named as that day's head row named it.
type PageRead struct {
	PageID             string
	Day, Node, AgentID string
	Handle, Role       string
	Backend, Via       string

	Count       int64
	LastAt      time.Time
	LastTurnID  string
	LastWorkKey string
	LastQuery   string
}

// PageReadsQuery is some pages' reads over a range of company days.
type PageReadsQuery struct {
	// PageIDs are the backend's own page ids — one for a page's readers,
	// a listing's worth for the skills screen's "loaded by" column.
	PageIDs []string

	// From and To are company day labels, both inclusive.
	From, To string

	// Vias narrows to the ways of reaching a page given; empty is every one.
	Vias []string
}

// PageReads reads every data node's reads of some pages over q's days, in
// (page, day, node, seat, via) order — through `usage_reads_page_idx`, which is
// exactly this question's shape and order (replicated 0073), one page id at a
// time ([PageReadsPage]).
//
// EVERY DATA NODE'S ROWS, which is why this is the domain's and not the event
// log's: a seat's reads are recorded in the day of the data node whose event
// log holds its turn — where it ran, or that node's custody keeper — and a
// page's readers are the union, including a node that has since left the
// fleet.
func PageReads(ctx context.Context, estate Estate, q PageReadsQuery) ([]PageRead, error) {
	page, err := PageReadsPage(ctx, estate, q, nil, 0)
	return page.Rows, err
}

// pageReadRows is one page id's reads over q's days from from on, in the
// read's order, where past — a keyset predicate over the read's group, or
// `1 = 1` — holds.
func pageReadRows(ctx context.Context, tx *sql.Tx, id, from string, q PageReadsQuery,
	past string, pastArgs []any) iter.Seq2[PageRead, error] {

	args := make([]any, 0, 3+len(pastArgs)+len(q.Vias))
	args = append(args, id, from, q.To)
	args = append(args, pastArgs...)
	for _, via := range q.Vias {
		args = append(args, via)
	}
	return scanned(ctx, tx, fmt.Sprintf(pageReadsStatement, pageReadsWhere(q, past)), args,
		func(rows *sql.Rows) (PageRead, error) {
			var r PageRead
			var last int64
			if err := rows.Scan(&r.PageID, &r.Day, &r.Node, &r.AgentID, &r.Handle, &r.Role,
				&r.Backend, &r.Via, &r.Count, &last,
				&r.LastTurnID, &r.LastWorkKey, &r.LastQuery); err != nil {
				return r, err
			}
			if last != 0 {
				r.LastAt = store.DecodeTime(last)
			}
			return r, nil
		})
}

// ReadsElided is how many (page, via) entries the per-seat-day cap
// ([ReadsPerSeatDay]) dropped over a range of company days, on every node.
//
// NOT PER PAGE, and it cannot be: a dropped entry is dropped whole, page id
// included, so all a reader of one page's reads can be told is how many
// entries the window lost of which some may have been this page's. Zero is
// the reading that says the answer is complete.
func ReadsElided(ctx context.Context, estate Estate, from, to string) (int64, error) {
	if err := (SpendQuery{From: from, To: to}).check(); err != nil {
		return 0, err
	}
	var total int64
	err := estate.Read(ctx, func(tx *sql.Tx) error {
		return tx.QueryRowContext(ctx, `
			SELECT COALESCE(SUM(reads_elided), 0) FROM usage_turns
			 WHERE day >= ? AND day <= ? AND reads_elided > 0`,
			from, to).Scan(&total)
	})
	if err != nil {
		return 0, fmt.Errorf("usage: count the elided reads for %s..%s: %w", from, to, err)
	}
	return total, nil
}

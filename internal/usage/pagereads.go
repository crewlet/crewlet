package usage

import (
	"context"
	"database/sql"
	"fmt"
	"slices"
	"strings"
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

// pageReadsChunk is how many page ids one statement binds: a listing's
// window is 500, and a chunk well inside every engine's parameter limit keeps
// the statement one plan however long the list.
const pageReadsChunk = 250

// PageReads reads every node's reads of some pages over q's days, in (page,
// day, node, seat, via) order — through `usage_reads_page_idx`, which is
// exactly this question's shape.
//
// EVERY NODE'S ROWS, which is why this is the domain's and not the event
// log's: a seat's reads are recorded where its turn ran, and a page's readers
// are the union — including a node that has since left the fleet.
func PageReads(ctx context.Context, estate Estate, q PageReadsQuery) ([]PageRead, error) {
	if err := (SpendQuery{From: q.From, To: q.To}).check(); err != nil {
		return nil, err
	}
	ids := slices.DeleteFunc(slices.Clone(q.PageIDs), func(id string) bool {
		return strings.TrimSpace(id) == ""
	})
	if len(ids) == 0 {
		return nil, fmt.Errorf("usage: a page's reads name the page")
	}
	var out []PageRead
	err := estate.Read(ctx, func(tx *sql.Tx) error {
		for chunk := range slices.Chunk(ids, pageReadsChunk) {
			if err := readPageReads(ctx, tx, chunk, q, &out); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("usage: read the reads of %d pages for %s..%s: %w",
			len(ids), q.From, q.To, err)
	}
	return out, nil
}

func readPageReads(ctx context.Context, tx *sql.Tx, ids []string, q PageReadsQuery,
	out *[]PageRead) error {

	where := "r.page_id IN (" + strings.TrimSuffix(strings.Repeat("?,", len(ids)), ",") +
		") AND r.day >= ? AND r.day <= ?"
	args := make([]any, 0, len(ids)+2+len(q.Vias))
	for _, id := range ids {
		args = append(args, id)
	}
	args = append(args, q.From, q.To)
	if len(q.Vias) > 0 {
		where += " AND r.via IN (" + strings.TrimSuffix(strings.Repeat("?,", len(q.Vias)), ",") + ")"
		for _, via := range q.Vias {
			args = append(args, via)
		}
	}
	rows, err := tx.QueryContext(ctx, `
		SELECT r.page_id, r.day, r.node, r.agent_id,
		       COALESCE(h.handle, ''), COALESCE(h.role, ''),
		       r.backend, r.via, r.reads, r.last_at,
		       r.last_turn_id, r.last_work_key, r.last_query
		  FROM usage_reads r
		  LEFT JOIN usage_turns h
		    ON h.day = r.day AND h.node = r.node AND h.agent_id = r.agent_id
		 WHERE `+where+`
		 ORDER BY r.page_id, r.day, r.node, r.agent_id, r.via`, args...)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var r PageRead
		var last int64
		if err := rows.Scan(&r.PageID, &r.Day, &r.Node, &r.AgentID, &r.Handle, &r.Role,
			&r.Backend, &r.Via, &r.Count, &last,
			&r.LastTurnID, &r.LastWorkKey, &r.LastQuery); err != nil {
			return err
		}
		if last != 0 {
			r.LastAt = store.DecodeTime(last)
		}
		*out = append(*out, r)
	}
	return rows.Err()
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

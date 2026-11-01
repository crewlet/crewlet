package code

import (
	"cmp"
	"context"
	"database/sql"
	"fmt"
	"slices"
	"strconv"

	"github.com/crewlet/crewlet/internal/routing"
	"github.com/crewlet/crewlet/internal/store"
)

// WHOM ONE CHANGE'S RECORDS REACHED — the `change_routing` read, the change
// page's Woke tab: the change by its number on its repository, as a `change`
// read names it and held to the same grant, and the records about it that
// reached anybody, newest first, each with every recipient its notices name
// (code_notifications, written by the applier per record and recipient,
// apply_notices.go). It is the OTHER axis of the rows a person's code inbox
// reads: an inbox is one recipient's notices across changes, this is one
// change's notices across recipients, sought by code_notifications_change_idx.
//
// # Whole records, a page at a time
//
// A page lists whole records: every notice of one record shares its position,
// the index's order after the change, so a record's recipients are read
// together and a page closes only between two records — a cursor naming a
// position never splits one. A record's recipients are bounded where it is
// written (its routing's own bounds), so a whole record always fits.
//
// # An empty answer is two facts, told apart by the horizon
//
// Every record about a change says whom it concerns — its routing
// ([ChangeRouting]), or none where it concerns nobody — so a change whose
// records list nobody is one of two things: its records concerned nobody, or
// only their own actors, or its notices were swept, or never written, past
// their horizon. They send a
// reader to two different places, and [routing.DeliveryOf] is the rule that
// tells them apart, dated by the change's opening — its oldest record —
// against [RoutedPage.RetainedFrom]: the instant from which every notice is
// kept, a seat's and a person's alike, which is the shorter horizon of the
// two (a seat's, [SeatNoticeHorizon]; a person's is the company's
// inbox.retention_days, at least as long). The serving node's clock dates
// it, since it is that node's sweeps that keep or take its rows.

// The page of a change's routing: a page of RECORDS, each whole — fifty by
// default, as a page of a change's reviews or comments, and at most two
// hundred, the bound every page of a change's conversation keeps; the bytes
// of a page are bounded by [MaxPageBytes] beside them.
const (
	defaultRoutedLimit      = 50
	MaxRoutedRecordsPerPage = 200
)

// changeRouting serves one page of whom change q.Change of repository q.Repo
// reached: the change's name and its opening, judged against the grant from
// the repository's row ([readableIn]) as a `change` read is, and the records
// about it that reached anybody, newest first, after the position a cursor
// names.
func (r *Reader) changeRouting(ctx context.Context, tx *sql.Tx, q ReadQuery, chart Roster, out *ReadAnswer) error {
	repo, err := readableIn(ctx, tx, q.Repo, q.Asker, q.CeilingSeat, chart)
	if err != nil {
		return err
	}
	row, found, err := changeByNumberIn(ctx, tx, repo.ref.ID, q.Change)
	if err != nil {
		return err
	}
	named := ChangeRef(repo.ref.Name, q.Change)
	if !found {
		return fmt.Errorf("%w: %s", ErrChangeNotFound, named)
	}
	query, args := changeNoticesSQL, []any{row.id}
	if q.Cursor != "" {
		position, parseErr := strconv.ParseInt(q.Cursor, 10, 64)
		if parseErr != nil || position < 1 {
			return fmt.Errorf("%w: %q is not a record's place on the code log", ErrBadCursor, q.Cursor)
		}
		query, args = changeNoticesAfterSQL, append(args, position)
	}
	routed := &RoutedPage{Change: ChangeName{Repo: repo.ref.Name, Ref: named, Title: row.title, State: row.state},
		OpenedAt: store.DecodeTime(row.openedAt), Records: []RoutedRecord{},
		RetainedFrom: r.now().UTC().Add(-SeatNoticeHorizon)}
	out.Routed = routed
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return fmt.Errorf("code: read whom %s reached: %w", named, err)
	}
	defer func() { _ = rows.Close() }()
	limit := min(limitOr(q.Limit, defaultRoutedLimit), r.pageRows)
	pg := page{maxRows: limit, maxB: r.pageBytes}
	var (
		record *RoutedRecord
		seq    int64
	)
	// finish counts the record read so far into the page, its recipients in
	// the order a reader wants them, and reports a page that takes no more.
	finish := func() bool {
		sortRecipients(record.Recipients)
		routed.Records = append(routed.Records, *record)
		return pg.add(record.encodedBytes())
	}
	for rows.Next() {
		var (
			n                   RoutedRecipient
			recordID, kind      string
			reason, excerpt     string
			addressed, fallback bool
			at, packed          int64
		)
		if scanErr := rows.Scan(&recordID, &n.Handle, &kind, &reason, &addressed, &fallback, &n.FallbackRank,
			&excerpt, &at, &packed); scanErr != nil {
			return fmt.Errorf("code: read whom %s reached: %w", named, scanErr)
		}
		if record != nil && packed != seq {
			if finish() {
				out.Next = strconv.FormatInt(seq, 10)
				break
			}
			record = nil
		}
		if record == nil {
			seq = packed
			record = &RoutedRecord{RecordID: recordID, Position: unpacked(packed), At: store.DecodeTime(at),
				Excerpt: excerpt}
		}
		n.Kind, n.Reason, n.Addressed, n.Fallback = PartyKind(kind), Reason(reason), addressed, fallback
		if !n.Fallback {
			// A RANK WITHOUT A FALLBACK IS NOISE: the column is zero on
			// every ordinary notice, and a surface reading it would render
			// "1st substitute" beside a reviewer.
			n.FallbackRank = 0
		}
		record.Recipients = append(record.Recipients, n)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("code: read whom %s reached: %w", named, err)
	}
	if out.Next == "" && record != nil {
		finish()
	}
	routed.Delivery = routedDelivery(routed, q.Cursor != "")
	return nil
}

// routedDelivery is what a page of a change's routing can say about whom its
// records reached ([RoutedPage.Delivery]): a page after a cursor continues a
// listing whose first page held records, and every other page is
// [routing.DeliveryOf] over the change — held, since the read found it;
// announced, since every record about a change says whom it concerns, a
// routing or none for nobody — dated by
// its opening against the page's horizon.
func routedDelivery(p *RoutedPage, later bool) routing.Delivery {
	if later {
		return routing.DeliveryReached
	}
	return routing.DeliveryOf(routing.Evidence{Held: true, Announced: true, Recipients: len(p.Records),
		At: p.OpenedAt, RetainedFrom: p.RetainedFrom})
}

// sortRecipients orders a record's recipients as a reader scans them:
// addressed first, a fallback last and the fallbacks by rank, each group by
// handle.
func sortRecipients(rs []RoutedRecipient) {
	slices.SortFunc(rs, func(a, b RoutedRecipient) int {
		switch {
		case a.Addressed != b.Addressed:
			if a.Addressed {
				return -1
			}
			return 1
		case a.Fallback != b.Fallback:
			if b.Fallback {
				return -1
			}
			return 1
		}
		return cmp.Or(cmp.Compare(a.FallbackRank, b.FallbackRank), cmp.Compare(a.Handle, b.Handle))
	})
}

// The change's notices, newest record first, each record's together: one
// range of code_notifications_change_idx, (change_id, log_seq), walked
// backward, and after a cursor the same range below the position it names.
// Unbounded in the statement and bounded by the page, which stops reading
// at the first record past it — a LIMIT counts rows, and a page counts whole
// records. Registered in plan_test.go.
const (
	changeNoticesSQL = `SELECT record_id, recipient, recipient_kind, reason, addressed, fallback_only,
			fallback_rank, excerpt, created_at, log_seq
		FROM code_notifications INDEXED BY code_notifications_change_idx WHERE change_id = ?
		ORDER BY log_seq DESC`
	changeNoticesAfterSQL = `SELECT record_id, recipient, recipient_kind, reason, addressed, fallback_only,
			fallback_rank, excerpt, created_at, log_seq
		FROM code_notifications INDEXED BY code_notifications_change_idx WHERE change_id = ? AND log_seq < ?
		ORDER BY log_seq DESC`
)

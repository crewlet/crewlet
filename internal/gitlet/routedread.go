package gitlet

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

// The change_routing read (the change page's Woke tab): the change named and
// granted as a `change` read, and the records about it that reached anybody,
// newest first, each with every recipient its code_notifications rows (one
// per record, change and recipient: apply_notices.go) name once — an inbox's
// other axis, sought by code_notifications_change_idx. A page holds whole records: a record's
// notices share one position, so a cursor never splits one, and its routing
// bounds its recipients, so one always fits. An empty answer is nobody
// concerned or notices swept past their horizon; [routing.DeliveryOf] tells
// them apart by the change's opening against [RoutedPage.RetainedFrom], the
// seat horizon ([SeatNoticeHorizon], no longer than a person's
// inbox.retention_days), dated by the serving node's clock, whose sweeps keep
// the rows.

// A page of a change's routing counts whole RECORDS: fifty by default and at
// most two hundred, as every page of a change's conversation; [MaxPageBytes]
// bounds its bytes.
const (
	defaultRoutedLimit      = 50
	MaxRoutedRecordsPerPage = 200
)

// changeRouting serves one page of whom change q.Change of q.Repo reached,
// judged by [readableIn] as a `change` read is.
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
		return fmt.Errorf("gitlet: read whom %s reached: %w", named, err)
	}
	defer func() { _ = rows.Close() }()
	limit := min(limitOr(q.Limit, defaultRoutedLimit), r.pageRows)
	pg := page{maxRows: limit, maxB: r.pageBytes}
	var (
		record *RoutedRecord
		seq    int64
	)
	// finish adds the record read so far to the page and reports a full page.
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
			return fmt.Errorf("gitlet: read whom %s reached: %w", named, scanErr)
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
			// A rank without a fallback is noise a surface would render as
			// "1st substitute" beside a reviewer.
			n.FallbackRank = 0
		}
		record.Recipients = append(record.Recipients, n)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("gitlet: read whom %s reached: %w", named, err)
	}
	if out.Next == "" && record != nil {
		finish()
	}
	routed.Delivery = routedDelivery(routed, q.Cursor != "")
	return nil
}

// routedDelivery is [RoutedPage.Delivery]: a page after a cursor continues a
// listing whose first page held records; any other is [routing.DeliveryOf]
// over the change — held, since the read found it, and announced, since every
// record says whom it concerns — dated by its opening against the horizon.
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

// The change's notices, newest record first: one backward range of
// code_notifications_change_idx (change_id, log_seq), below the cursor's
// position after one. No LIMIT: it counts rows, and the page stops at the
// first whole record past it. Registered in plan_test.go.
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

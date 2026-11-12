package atlasdomain

import (
	"cmp"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/atlas"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/store"
)

// THE LOG AS A READ: what was recorded, when and in whose voice, one line a
// claim. A claim an integration's read stopped holding is a second line at the
// instant the read withdrew it, so a fact that went away is as visible as one
// that arrived. Nothing here decides who may read a line: the caller holds
// every line to the atlas its reader is shown.

// LogLine is one line of the log: a claim recorded, or one its voice withdrew.
type LogLine struct {
	// ID is the claim's own id, and Entry the id of the entry it was recorded on.
	ID    string
	Entry string

	// Claim is what was said, in the vocabulary's own type.
	Claim atlas.Claim

	// At is the broker's instant for the record that wrote the line: the
	// claim's own record, or for a withdrawal the read that withdrew it.
	At time.Time

	// Withdrawn is true for the line a read left when it stopped holding the
	// claim, which no newer claim of its voice replaced.
	Withdrawn bool
}

// Cursor is where the log continues below this line.
func (l LogLine) Cursor() string {
	return strings.Join([]string{strconv.FormatInt(store.EncodeTime(l.At), 10),
		strconv.Itoa(boolInt(l.Withdrawn)), l.ID}, cursorSeparator)
}

// cursorSeparator parts a cursor's instant, stream and claim id; a claim id
// holds none.
const cursorSeparator = "~"

// logOrder is the log's order, newest first: by instant, a withdrawal before a
// claim of the same instant, then by claim id, so two lines never tie.
func logOrder(a, b LogLine) int {
	return cmp.Or(b.At.Compare(a.At), cmp.Compare(boolInt(b.Withdrawn), boolInt(a.Withdrawn)),
		cmp.Compare(b.ID, a.ID))
}

// LogQuery is one page of the log.
type LogQuery struct {
	// Entries narrows the page to the entries with these ids (one entry of the
	// atlas and every entry merged into it); none is the whole company's log.
	Entries []string

	// Cursor is the line the page starts below, empty for the newest, and
	// Limit how many lines it holds, DefaultPageSize where zero.
	Cursor string
	Limit  int

	Fresh statelog.Freshness
}

// LogPage is one page of the log, newest first.
type LogPage struct {
	Lines []LogLine

	// Next is the cursor of the page after this one, empty on the last.
	Next string

	Position statelog.Position
	Complete bool
}

// logBound is the line a page starts below.
type logBound struct {
	at        int64
	withdrawn bool
	id        string
	set       bool
}

// parseLogCursor reads a cursor a page answered.
func parseLogCursor(cursor string) (logBound, error) {
	if cursor == "" {
		return logBound{}, nil
	}
	parts := strings.SplitN(cursor, cursorSeparator, 3)
	bad := invalid("the cursor %q is not one a page answered", cursor)
	if len(parts) != 3 || parts[2] == "" || (parts[1] != "0" && parts[1] != "1") {
		return logBound{}, bad
	}
	at, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil || at <= 0 {
		return logBound{}, bad
	}
	return logBound{at: at, withdrawn: parts[1] == "1", id: parts[2], set: true}, nil
}

// below is the condition that keeps the rows of one stream (the withdrawals,
// or the claims) that sort after the bound, over the stream's instant column.
func (b logBound) below(column string, withdrawn bool) (string, []any) {
	switch {
	case !b.set:
		return "1 = 1", nil
	case withdrawn == b.withdrawn:
		return "(" + column + " < ? OR (" + column + " = ? AND claim_id < ?))", []any{b.at, b.at, b.id}
	case withdrawn:
		// A withdrawal sorts before a claim of its instant: all are above.
		return column + " < ?", []any{b.at}
	}
	return column + " <= ?", []any{b.at}
}

// noSuccessor keeps a replaced claim of `c` only where its voice recorded no
// claim on its slot in the record that replaced it: a read withdrew it. A
// claim a newer one replaced, and one that landed already replaced, have one.
const noSuccessor = `NOT EXISTS (SELECT 1 FROM atlas_claims n INDEXED BY atlas_claims_entry_idx
	WHERE n.entry_id = c.entry_id AND n.position = c.superseded_position AND n.slot = c.slot
		AND n.source_kind = c.source_kind AND n.source_name = c.source_name)`

// Log pages the log, newest first: the whole company's, or the lines of the
// entries the query names.
func (r *Reader) Log(ctx context.Context, q LogQuery) (LogPage, error) {
	for _, id := range q.Entries {
		if !IsEntryID(id) {
			return LogPage{}, invalid("%q is not an entry id", id)
		}
	}
	size := q.Limit
	switch {
	case size < 0 || size > MaxPageSize:
		return LogPage{}, invalid("a page holds 1 to %d lines", MaxPageSize)
	case size == 0:
		size = DefaultPageSize
	}
	bound, err := parseLogCursor(q.Cursor)
	if err != nil {
		return LogPage{}, err
	}
	scope := DomainScope()
	if len(q.Entries) == 1 {
		scope = EntryScope(q.Entries[0])
	}
	var out LogPage
	answer, err := r.log.Read(ctx, q.Fresh.Query(scope, true), func(tx *sql.Tx) error {
		out = LogPage{}
		// EACH STREAM AND EACH ENTRY IS ITS OWN INDEXED READ of one page and a
		// line, merged here: the page is the first lines of their union.
		entries := q.Entries
		if len(entries) == 0 {
			entries = []string{""}
		}
		for _, entry := range entries {
			for _, withdrawn := range []bool{false, true} {
				lines, readErr := logStream(ctx, tx, entry, withdrawn, bound, size+1)
				if readErr != nil {
					return readErr
				}
				out.Lines = append(out.Lines, lines...)
			}
		}
		return nil
	})
	if err != nil {
		return LogPage{}, fmt.Errorf("atlasdomain: read the atlas's log: %w", err)
	}
	slices.SortFunc(out.Lines, logOrder)
	if len(out.Lines) > size {
		out.Lines = out.Lines[:size]
		out.Next = out.Lines[size-1].Cursor()
	}
	out.Position, out.Complete = answer.Position, answer.Complete
	return out, nil
}

// logStream reads the first limit lines below bound of one stream: the claims
// recorded, or the claims withdrawn, of one entry or of every entry.
func logStream(ctx context.Context, tx *sql.Tx, entry string, withdrawn bool, bound logBound,
	limit int) ([]LogLine, error) {

	column, index, where := "c.at", "atlas_claims_at_idx", "1 = 1"
	if withdrawn {
		column, index, where = "c.superseded_at", "atlas_claims_expiry_idx", "c.replaced = 1 AND "+noSuccessor
	}
	var args []any
	if entry != "" {
		index, where = "atlas_claims_entry_idx", "c.entry_id = ? AND "+where
		args = append(args, entry)
	}
	below, boundArgs := bound.below(column, withdrawn)
	below = strings.ReplaceAll(below, "claim_id", "c.claim_id")
	args = append(append(args, boundArgs...), limit)
	rows, err := tx.QueryContext(ctx, `
		SELECT c.claim_id, c.entry_id, c.claim, c.observed_at, c.position, `+column+`
		FROM atlas_claims c INDEXED BY `+index+`
		WHERE `+where+` AND `+below+`
		ORDER BY `+column+` DESC, c.claim_id DESC LIMIT ?`, args...)
	if err != nil {
		return nil, err
	}
	return scanLines(rows, withdrawn)
}

// scanLines reads log lines off rows of (claim id, entry id, claim, observed,
// position, instant), and closes them.
func scanLines(rows *sql.Rows, withdrawn bool) ([]LogLine, error) {
	var out []LogLine
	for rows.Next() {
		var (
			l                      = LogLine{Withdrawn: withdrawn}
			body                   string
			observed, position, at int64
			wire                   Claim
		)
		if err := rows.Scan(&l.ID, &l.Entry, &body, &observed, &position, &at); err != nil {
			_ = rows.Close()
			return nil, err
		}
		if err := json.Unmarshal([]byte(body), &wire); err != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("claim %s does not decode: %w", l.ID, err)
		}
		l.Claim, l.At = wire.vocabulary(store.DecodeTime(observed), uint64(position)), store.DecodeTime(at)
		out = append(out, l)
	}
	return out, errors.Join(rows.Err(), rows.Close())
}

// ---- activity ----------------------------------------------------------- //

// ActivityDay is how many lines the log took on one entry in one UTC day,
// counted apart for each other entry they name, so a caller can leave out the
// lines its reader is not shown.
type ActivityDay struct {
	Entry string

	// Target is the id of the other entry the lines name, empty where they
	// name none.
	Target string

	// Day is the day's first instant, UTC.
	Day   time.Time
	Lines int
}

// Activity is how much the log took since an instant, and each entry's newest
// lines.
type Activity struct {
	Days []ActivityDay

	// Last is the lines of the newest record on each entry the log holds:
	// one or more an entry, in no order.
	Last []LogLine

	Position statelog.Position
	Complete bool
}

// dayMicros is one UTC day in the store's encoding of an instant.
const dayMicros = int64(24 * time.Hour / time.Microsecond)

// Activity counts the lines the log took at or after since, a day and an entry
// at a time, and reads each entry's newest lines however old they are.
func (r *Reader) Activity(ctx context.Context, since time.Time, fresh statelog.Freshness) (Activity, error) {
	var out Activity
	from := store.EncodeTime(since)
	answer, err := r.log.Read(ctx, fresh.Query(DomainScope(), true), func(tx *sql.Tx) error {
		out = Activity{}
		for _, withdrawn := range []bool{false, true} {
			days, err := activityDays(ctx, tx, withdrawn, from)
			if err != nil {
				return err
			}
			out.Days = append(out.Days, days...)
			last, err := lastLines(ctx, tx, withdrawn)
			if err != nil {
				return err
			}
			out.Last = append(out.Last, last...)
		}
		if len(out.Days) > MaxLiveClaims {
			return fmt.Errorf("atlasdomain: the log took more than %d lines since %s, past what one "+
				"read counts", MaxLiveClaims, since.UTC().Format(time.RFC3339))
		}
		return nil
	})
	if err != nil {
		return Activity{}, fmt.Errorf("atlasdomain: count the atlas's log: %w", err)
	}
	out.Position, out.Complete = answer.Position, answer.Complete
	return out, nil
}

// activityDays counts one stream's lines at or after from.
func activityDays(ctx context.Context, tx *sql.Tx, withdrawn bool, from int64) ([]ActivityDay, error) {
	column, index, where := "c.at", "atlas_claims_at_idx", "1 = 1"
	if withdrawn {
		column, index, where = "c.superseded_at", "atlas_claims_expiry_idx", "c.replaced = 1 AND "+noSuccessor
	}
	rows, err := tx.QueryContext(ctx, `
		SELECT c.entry_id, c.target_id, `+column+` / ? AS day, COUNT(*)
		FROM atlas_claims c INDEXED BY `+index+`
		WHERE `+where+` AND `+column+` >= ?
		GROUP BY c.entry_id, c.target_id, day LIMIT ?`, dayMicros, from, MaxLiveClaims+1)
	if err != nil {
		return nil, err
	}
	var out []ActivityDay
	for rows.Next() {
		var (
			d   ActivityDay
			day int64
		)
		if err := rows.Scan(&d.Entry, &d.Target, &day, &d.Lines); err != nil {
			_ = rows.Close()
			return nil, err
		}
		d.Day = store.DecodeTime(day * dayMicros)
		out = append(out, d)
	}
	return out, errors.Join(rows.Err(), rows.Close())
}

// lastLines reads one stream's lines of the newest record on every entry: the
// rows written at the entry's `last_at`, found by that instant.
func lastLines(ctx context.Context, tx *sql.Tx, withdrawn bool) ([]LogLine, error) {
	column, index, where := "c.at", "atlas_claims_at_idx", "1 = 1"
	if withdrawn {
		column, index, where = "c.superseded_at", "atlas_claims_expiry_idx", "c.replaced = 1 AND "+noSuccessor
	}
	rows, err := tx.QueryContext(ctx, `
		SELECT c.claim_id, c.entry_id, c.claim, c.observed_at, c.position, `+column+`
		FROM atlas_entries e JOIN atlas_claims c INDEXED BY `+index+`
			ON `+column+` = e.last_at AND c.entry_id = e.entry_id
		WHERE `+where)
	if err != nil {
		return nil, err
	}
	return scanLines(rows, withdrawn)
}

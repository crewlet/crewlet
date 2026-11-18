package gitlet

import (
	"context"
	"database/sql"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/store"
)

// A CHANGE'S MOVES: one row a move of its lifecycle (code_change_moves, replicated 0188), written
// by the apply of the record that made it, so a change's timeline says who closed, reopened,
// drafted, readied, edited or retargeted it and when, where its row says only where it stands now.
// A move is derived from the record and the row it moved, never from a field a record gained, so a
// record of every version writes the same moves on every node.

// ChangeMoveKind is what one move did to a change. A closed set ([ChangeMoveKinds]), with Valid.
type ChangeMoveKind string

const (
	// MoveClosed closed it without merging it: its Reason is the closer's, or [SourceDeleted]
	// where a push deleted its source.
	MoveClosed ChangeMoveKind = "closed"
	// MoveReopened opened a closed change again.
	MoveReopened ChangeMoveKind = "reopened"
	// MoveDrafted made a change asking to be merged a draft.
	MoveDrafted ChangeMoveKind = "drafted"
	// MoveReadied asked for a draft to be merged.
	MoveReadied ChangeMoveKind = "readied"
	// MoveRetitled set its title.
	MoveRetitled ChangeMoveKind = "retitled"
	// MoveDescribed set its description, or removed it.
	MoveDescribed ChangeMoveKind = "described"
	// MoveWorkItem set the work item it carries (Became its key, else its id) or cleared it
	// (Became "").
	MoveWorkItem ChangeMoveKind = "work_item"
	// MoveRetargeted moved its target from the branch Was to the branch Became.
	MoveRetargeted ChangeMoveKind = "retargeted"
)

// ChangeMoveKinds is the closed set, in this order.
func ChangeMoveKinds() []ChangeMoveKind {
	return []ChangeMoveKind{MoveClosed, MoveReopened, MoveDrafted, MoveReadied, MoveRetitled, MoveDescribed,
		MoveWorkItem, MoveRetargeted}
}

// Valid reports a kind this build knows.
func (k ChangeMoveKind) Valid() bool { return slices.Contains(ChangeMoveKinds(), k) }

// ChangeMove is one move of a change's lifecycle as a read serves it: what it did, who made it (a
// seat by its handle, a person by their human seat; "" for a push nobody made), a close's reason,
// what a retarget moved its target from and to and the work item an edit set, and the record's
// position and broker instant.
type ChangeMove struct {
	Kind   ChangeMoveKind `json:"kind"`
	By     string         `json:"by,omitempty"`
	ByKind PartyKind      `json:"by_kind,omitempty"`
	// Reason is the closer's words, as the change row's close_reason is.
	Reason   string            `json:"reason,omitempty" untrusted:"name"`
	Was      string            `json:"was,omitempty"`
	Became   string            `json:"became,omitempty"`
	Position statelog.Position `json:"position"`
	At       time.Time         `json:"at"`
}

// Cursor is the cursor that resumes a page of a change's moves after m: its position, packed,
// and its kind, which is the table's key after the change.
func (m ChangeMove) Cursor() string {
	return strconv.FormatInt(m.Position.Packed(), 10) + "/" + string(m.Kind)
}

// cutMoveCursor reads a [ChangeMove.Cursor].
func cutMoveCursor(s string) (int64, string, bool) {
	head, kind, ok := strings.Cut(s, "/")
	packed, err := strconv.ParseInt(head, 10, 64)
	if !ok || err != nil || packed <= 0 || !ChangeMoveKind(kind).Valid() {
		return 0, "", false
	}
	return packed, kind, true
}

// moveRow is one move as an apply writes it: the record's position and instant are the apply's.
type moveRow struct {
	change              string
	kind                ChangeMoveKind
	by                  string
	byKind              PartyKind
	reason, was, became string
}

// lifecycleMoves is what a lifecycle record (payload) does to a change whose row was row before
// it and next after it, as moves: each set field of an edit that is one, a draft's state where it
// moved, a retarget, a close, a reopen. A landing the home found makes none: the change's row says
// it merged, and how.
func lifecycleMoves(payload any, row, next changeRow) []moveRow {
	move := func(kind ChangeMoveKind, by Principal) moveRow {
		handle, party := by.party()
		return moveRow{change: row.id, kind: kind, by: handle, byKind: party}
	}
	var out []moveRow
	switch p := payload.(type) {
	case ChangeEdit:
		if p.Title != nil && next.title != row.title {
			out = append(out, move(MoveRetitled, p.By))
		}
		if p.Body != nil {
			out = append(out, move(MoveDescribed, p.By))
		}
		if p.WorkItem != nil || p.ClearWorkItem {
			m := move(MoveWorkItem, p.By)
			if m.became = next.work.Key; m.became == "" {
				m.became = next.work.ID
			}
			out = append(out, m)
		}
	case ChangeDraft:
		switch {
		case next.state == row.state:
		case p.Draft:
			out = append(out, move(MoveDrafted, p.By))
		default:
			out = append(out, move(MoveReadied, p.By))
		}
	case ChangeRetarget:
		m := move(MoveRetargeted, p.By)
		m.was, m.became = row.target, next.target
		out = append(out, m)
	case ChangeClose:
		m := move(MoveClosed, p.By)
		m.reason = p.Reason
		out = append(out, m)
	case ChangeReopen:
		out = append(out, move(MoveReopened, p.By))
	}
	return out
}

// parts is how many moves e makes at the most: one for each of its title, its description and
// its work item it sets.
func (e ChangeEdit) parts() int {
	n := 0
	for _, set := range []bool{e.Title != nil, e.Body != nil, e.WorkItem != nil || e.ClearWorkItem} {
		if set {
			n++
		}
	}
	return n
}

// changeMoves serves a page of change id's moves (named: its web!n), oldest first, after the
// move a cursor names ([ChangeMove.Cursor]).
func (r *Reader) changeMoves(ctx context.Context, tx *sql.Tx, q ReadQuery, named, id string,
	out *ReadAnswer) error {

	limit := min(limitOr(q.Limit, defaultMoveLimit), r.pageRows)
	query, args := movesOfSQL, []any{id}
	if q.Cursor != "" {
		packed, kind, ok := cutMoveCursor(q.Cursor)
		if !ok {
			return fmt.Errorf("%w: %q is not a move of a change", ErrBadCursor, q.Cursor)
		}
		query, args = movesOfAfterSQL, append(args, packed, kind)
	}
	rows, err := tx.QueryContext(ctx, query, append(args, limit)...)
	if err != nil {
		return fmt.Errorf("gitlet: read the moves of %s: %w", named, err)
	}
	defer func() { _ = rows.Close() }()
	pg := page{maxRows: limit, maxB: r.pageBytes}
	for rows.Next() {
		var (
			m              ChangeMove
			kind, byKind   string
			packed, atTime int64
		)
		if err = rows.Scan(&packed, &kind, &m.By, &byKind, &m.Reason, &m.Was, &m.Became, &atTime); err != nil {
			return fmt.Errorf("gitlet: read the moves of %s: %w", named, err)
		}
		m.Kind, m.ByKind, m.Position, m.At = ChangeMoveKind(kind), PartyKind(byKind), unpacked(packed),
			store.DecodeTime(atTime)
		out.Moves = append(out.Moves, m)
		if pg.add(m.encodedBytes()) {
			out.Next = m.Cursor()
			break
		}
	}
	if err = rows.Err(); err != nil {
		return fmt.Errorf("gitlet: read the moves of %s: %w", named, err)
	}
	return nil
}

// A change's moves are read on the table's key, oldest first; each registered in plan_test.go.
const (
	movesOfSQL = `SELECT position, kind, by_handle, by_kind, reason, was, became, at
		FROM code_change_moves INDEXED BY ` + movesKey + ` WHERE change_id = ? ORDER BY position, kind LIMIT ?`
	movesOfAfterSQL = `SELECT position, kind, by_handle, by_kind, reason, was, became, at
		FROM code_change_moves INDEXED BY ` + movesKey + ` WHERE change_id = ?1
			AND position >= ?2 AND (position > ?2 OR kind > ?3)
		ORDER BY position, kind LIMIT ?4`
	// movesKey is the table's key: the change, the position, the kind.
	movesKey = "sqlite_autoindex_code_change_moves_1"
)

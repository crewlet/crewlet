package gitlet

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/store"
)

// A CHANGE'S PAGE beside its rows: who takes part in its conversation, when the record behind
// each reviewer row landed, and its findings counted by path, each read with the change.

// ParticipantList is a change's participants, at most MaxDetailParticipants, and how many more.
type ParticipantList struct {
	Rows    []ParticipantRow `json:"rows"`
	Omitted int              `json:"omitted,omitempty"`
}

// ParticipantRow is one participant: a seat by its handle or a person by the human seat they
// hold, and their standing verdict and the revision it judged, where they have one.
type ParticipantRow struct {
	Handle          string    `json:"handle"`
	Kind            PartyKind `json:"kind"`
	Verdict         Verdict   `json:"verdict,omitempty"`
	VerdictRevision int64     `json:"verdict_revision,omitempty"`
}

// FindingPathRow is how many of a change's findings on one path were raised on one revision
// and stand in one state.
type FindingPathRow struct {
	Path     string       `json:"path"`
	State    FindingState `json:"state"`
	Revision int64        `json:"revision"`
	Findings int64        `json:"findings"`
}

// MaxDetailParticipants is the most participants a change's detail names, the first by
// handle: as many as asks a change can wait on at once. The reviewers part names every reviewer.
const MaxDetailParticipants = MaxReviewersPerChange

// The pages of a change's findings by path: groups are a few dozen bytes, so a page holds
// more than a page of findings does.
const (
	defaultFindingPathLimit = 500
	MaxFindingPathsPerPage  = 1_000
)

// widestInstant is the longest instant a row encodes, what a reviewer row is costed at
// before its instant is read.
var widestInstant = time.Date(9999, 12, 31, 23, 59, 59, 999_999_999, time.UTC)

// changePageMax is the most rows one page of a change read may ask for.
func (q ReadQuery) changePageMax() int {
	switch {
	case q.FindingPaths:
		return MaxFindingPathsPerPage
	case q.Viewed:
		return MaxViewedPerPage
	}
	return q.Part.maxPerPage()
}

// pagesDetail reports a change read whose first page carries the change's detail: its
// revisions, never its findings by path or its asker's viewed files.
func (q ReadQuery) pagesDetail() bool { return q.Part.detailed() && !q.FindingPaths && !q.Viewed }

// participantsIn is change id's participants by handle, at most MaxDetailParticipants, and how
// many more: one walk of the key, and a covering count only where the walk filled its bound.
func participantsIn(ctx context.Context, tx *sql.Tx, id string) (*ParticipantList, error) {
	rows, err := tx.QueryContext(ctx, participantsOfSQL, id, MaxDetailParticipants+1)
	if err != nil {
		return nil, fmt.Errorf("gitlet: read change %s's participants: %w", id, err)
	}
	defer func() { _ = rows.Close() }()
	out := &ParticipantList{Rows: make([]ParticipantRow, 0, 4)}
	read := 0
	for rows.Next() {
		var (
			p             ParticipantRow
			kind, verdict string
			revision      int64
		)
		if scanErr := rows.Scan(&p.Handle, &kind, &verdict, &revision); scanErr != nil {
			return nil, fmt.Errorf("gitlet: read change %s's participants: %w", id, scanErr)
		}
		if read++; read > MaxDetailParticipants {
			continue
		}
		p.Kind = PartyKind(kind)
		if verdict != "" {
			p.Verdict, p.VerdictRevision = Verdict(verdict), revision
		}
		out.Rows = append(out.Rows, p)
	}
	if err = rows.Err(); err != nil {
		return nil, fmt.Errorf("gitlet: read change %s's participants: %w", id, err)
	}
	if read <= MaxDetailParticipants {
		return out, nil
	}
	var count int
	if err = tx.QueryRowContext(ctx, participantsCountSQL, id).Scan(&count); err != nil {
		return nil, fmt.Errorf("gitlet: count change %s's participants: %w", id, err)
	}
	out.Omitted = count - MaxDetailParticipants
	return out, nil
}

// movedAtIn is the broker instant of the record at each packed position on change id, by
// position: one statement over the distinct positions, one notice of each record read.
func movedAtIn(ctx context.Context, tx *sql.Tx, id string, positions []int64) (map[int64]time.Time, error) {
	out := map[int64]time.Time{}
	if len(positions) == 0 {
		return out, nil
	}
	distinct := make([]int64, 0, len(positions))
	seen := make(map[int64]bool, len(positions))
	for _, p := range positions {
		if !seen[p] {
			seen[p] = true
			distinct = append(distinct, p)
		}
	}
	list, err := json.Marshal(distinct)
	if err != nil {
		return nil, err
	}
	rows, err := tx.QueryContext(ctx, reviewersMovedAtSQL, id, string(list))
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var (
			position int64
			created  sql.NullInt64
		)
		if scanErr := rows.Scan(&position, &created); scanErr != nil {
			return nil, scanErr
		}
		if created.Valid {
			out[position] = store.DecodeTime(created.Int64)
		}
	}
	return out, rows.Err()
}

// changeFindingPaths serves a page of change id's findings counted by path, state and revision,
// in that order, after the group a cursor names: one walk of code_findings_path_idx.
func (r *Reader) changeFindingPaths(ctx context.Context, tx *sql.Tx, q ReadQuery, named, id string,
	out *ReadAnswer) error {

	limit := min(limitOr(q.Limit, defaultFindingPathLimit), r.pageRows)
	query, args := findingPathsSQL, []any{id, limit}
	if q.Cursor != "" {
		after, err := findingPathCursor(q.Cursor)
		if err != nil {
			return err
		}
		query, args = findingPathsAfterSQL, []any{id, after.Path, string(after.State), after.Revision, limit}
	}
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return fmt.Errorf("gitlet: read the findings of %s by path: %w", named, err)
	}
	defer func() { _ = rows.Close() }()
	out.FindingPaths = []FindingPathRow{}
	pg := page{maxRows: limit, maxB: r.pageBytes}
	for rows.Next() {
		var (
			row   FindingPathRow
			state string
		)
		if scanErr := rows.Scan(&row.Path, &state, &row.Revision, &row.Findings); scanErr != nil {
			return fmt.Errorf("gitlet: read the findings of %s by path: %w", named, scanErr)
		}
		row.State = FindingState(state)
		out.FindingPaths = append(out.FindingPaths, row)
		if pg.add(row.encodedBytes()) {
			out.Next = row.Cursor()
			break
		}
	}
	return rows.Err()
}

// Cursor is the cursor that resumes a page of a change's findings by path after p: its
// revision, its state and its path, the path last since it may hold a slash.
func (p FindingPathRow) Cursor() string {
	return strconv.FormatInt(p.Revision, 10) + "/" + string(p.State) + "/" + p.Path
}

// findingPathCursor reads back a cursor [FindingPathRow.Cursor] issued.
func findingPathCursor(cursor string) (FindingPathRow, error) {
	revision, rest, cut := strings.Cut(cursor, "/")
	state, path, cutPath := strings.Cut(rest, "/")
	n, parseErr := strconv.ParseInt(revision, 10, 64)
	if !cut || !cutPath || parseErr != nil || n < 1 || !FindingState(state).Valid() || path == "" {
		return FindingPathRow{}, fmt.Errorf("%w: %q is not a group of a change's findings by path",
			ErrBadCursor, cursor)
	}
	return FindingPathRow{Path: path, State: FindingState(state), Revision: n}, nil
}

// A CHANGE'S PAGE'S READS, each registered in plan_test.go. The notice is a scalar subquery with
// LIMIT 1, so a record that wrote thousands of notices costs one seek; the groups walk an index
// holding exactly their columns, so they read no row and sort nothing.
const (
	findingPathsKey = "code_findings_path_idx"

	participantsOfSQL = `SELECT handle, kind, verdict, verdict_revision FROM code_participants
		INDEXED BY ` + participantsKey + ` WHERE change_id = ? ORDER BY handle LIMIT ?`
	participantsCountSQL = `SELECT COUNT(*) FROM code_participants INDEXED BY ` + participantsKey + `
		WHERE change_id = ?`
	reviewersMovedAtSQL = `SELECT j.value, (SELECT n.created_at FROM code_notifications AS n
			INDEXED BY code_notifications_change_idx WHERE n.change_id = ?1 AND n.log_seq = j.value LIMIT 1)
		FROM json_each(?2) AS j`
	findingPathsSQL = `SELECT path, state, revision, COUNT(*) FROM code_findings INDEXED BY ` +
		findingPathsKey + ` WHERE change_id = ? GROUP BY path, state, revision
		ORDER BY path, state, revision LIMIT ?`
	findingPathsAfterSQL = `SELECT path, state, revision, COUNT(*) FROM code_findings INDEXED BY ` +
		findingPathsKey + ` WHERE change_id = ?1 AND path >= ?2
			AND (path > ?2 OR state > ?3 OR (state = ?3 AND revision > ?4))
		GROUP BY path, state, revision ORDER BY path, state, revision LIMIT ?5`
)

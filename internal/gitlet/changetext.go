package gitlet

import (
	"cmp"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/knowledge"
	"github.com/crewlet/crewlet/internal/search"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/store"
	"github.com/crewlet/crewlet/internal/textindex"
)

const (
	// MaxChangeTextTerms is the most words one search of changes takes: a title is a sentence, and each
	// word is one bounded count and one more key read per document the index reads.
	MaxChangeTextTerms = 8
	// MaxChangeTextCandidates is the most matches one search reads from the index, newest first as the
	// index took them in: five of the largest page, each one key read of its change and its conversation.
	MaxChangeTextCandidates = 1_000
	// MaxChangeTextTail is the most changes moved past the index's watermark a read judges itself:
	// at the bound their descriptions weigh 3 MiB; a lag past it is the index building, which it says.
	MaxChangeTextTail = 100
)

// ChangesText is how wholly a listing narrowed by its words searched; an answer's field, no record's.
type ChangesText struct {
	// Building: this node's index of changes' words is behind the rows the read holds, is being rebuilt,
	// or was built from another copy, so a change holding the words may be missing.
	Building bool `json:"building,omitempty"`
	// IndexedBackTo: this node's first build of the index has reached back to the changes minted at this
	// instant and is still indexing older ones, so one minted at or before it may be missing.
	IndexedBackTo time.Time `json:"indexed_back_to,omitzero"`
	// Truncated: a search reads the newest MaxChangeTextScan of the narrowest of its scope and words and
	// judges the newest MaxChangeTextCandidates matches, and more may hold the words.
	Truncated bool `json:"truncated,omitempty"`
}

// ChangeWords is this node's own index of changes' words (search.ChangeIndexer), which a listing
// narrowed by its words takes its candidates from (ADR-0156).
type ChangeWords interface {
	Holding(ctx context.Context, q search.ChangeQuery) (search.ChangeHeld, error)
}

// changesPositionKey is code_changes_position_idx, (position) (replicated "A change is walked in the
// order its records moved it"): the changes moved past a position.
const changesPositionKey = "code_changes_position_idx"

// changesMovedPastSQL is the changes moved past a position, newest move first: what a listing
// narrowed by its words judges itself beside the index's candidates.
const changesMovedPastSQL = `SELECT ` + changeColumns + ` FROM code_changes INDEXED BY ` + changesPositionKey + `
		WHERE position > ? ORDER BY position DESC LIMIT ?`

// textLeads reports a listing whose words lead it: no reviewer, need, work item or readiness bucket
// names a narrower walk.
func (q ReadQuery) textLeads() bool {
	return q.Text != "" && q.Reviewer == "" && q.Needs == "" && q.Work == "" && q.Readiness == ""
}

// wordsProblem is why text is no search of changes' words, "" where it is one.
func wordsProblem(text string) string {
	if err := knowledge.CheckQuery(text); err != nil {
		var long *knowledge.QueryTooLongError
		if errors.As(err, &long) {
			return fmt.Sprintf("a search takes at most %d bytes, and this one holds %d: search on a few words",
				knowledge.MaxQueryBytes, long.Bytes)
		}
		return err.Error()
	}
	switch terms := textindex.Terms(text); {
	case len(terms) == 0:
		return "holds no word to search by: a word is two or more letters or digits"
	case len(terms) > MaxChangeTextTerms:
		return fmt.Sprintf("a search of changes takes at most %d words, and this one holds %d",
			MaxChangeTextTerms, len(terms))
	}
	return ""
}

// changesByWords serves a page of a listing its words lead: the index's candidates in the listing's
// scope and every change moved past its watermark, each held to the grant and judged in tx, in the
// order the listing walks without words, after q's cursor (ADR-0156).
func (r *Reader) changesByWords(ctx context.Context, tx *sql.Tx, q ReadQuery, chart Roster, cp statelog.Position,
	out *ReadAnswer) error {

	repos := map[string]listedRepo{}
	var repoID string
	if q.Repo != "" {
		named, err := resolvingIn(ctx, tx, q, chart, cp, out)
		if err != nil {
			return err
		}
		repoID = named.ref.ID
		repos[repoID] = named
	}
	terms := textindex.Terms(q.Text)
	held, err := r.words.Holding(ctx, search.ChangeQuery{Terms: terms, RepoID: repoID, State: string(q.State),
		Owner: q.Owner, Limit: r.textCandidates})
	if err != nil {
		return fmt.Errorf("%w: read this node's index of changes' words: %w", ErrNotServing, err)
	}
	// WHOSE, AND HOW COVERED: a candidate is trusted only where every record any index write was
	// decided from has produced its rows in this snapshot.
	instance, err := statelog.EstateInstance(ctx, tx)
	if err != nil {
		return err
	}
	prefix, err := statelog.PrefixIn(ctx, tx, Domain{})
	if err != nil {
		return err
	}
	ours := !held.Rebuilding && held.BuiltFrom == instance
	covered := ours && prefix.Applied().Packed() >= held.Seen
	var through int64
	candidates := map[string]bool{}
	if ours {
		through = held.Through
		for _, id := range held.IDs {
			candidates[id] = true
		}
	}

	rows, err := readChangesBy(ctx, tx, changesMovedPastSQL, []any{through, r.textTail + 1}, "a move past the index")
	if err != nil {
		return err
	}
	overflow := len(rows) > r.textTail
	if overflow {
		rows = rows[:r.textTail]
	}
	var unread []string
	for _, row := range rows {
		delete(candidates, row.id)
	}
	for id := range candidates {
		unread = append(unread, id)
	}
	named, err := changeRowsIn(ctx, tx, unread)
	if err != nil {
		return err
	}
	for _, row := range named {
		rows = append(rows, row)
	}

	kept, err := r.scopedWords(ctx, tx, q, chart, repoID, repos, rows)
	if err != nil {
		return err
	}
	var trusted, judged []changeRow
	for _, row := range kept {
		if covered && candidates[row.id] && heldAsItIs(held, row) {
			trusted = append(trusted, row)
		} else {
			judged = append(judged, row)
		}
	}
	if judged, err = holdingWords(ctx, tx, judged, terms); err != nil {
		return err
	}
	w := walkLedBy(q, repoID)
	ordered, err := w.inOrderAfter(append(trusted, judged...), q.Cursor)
	if err != nil {
		return err
	}

	limit := min(limitOr(q.Limit, defaultChangeLimit), r.pageRows)
	listed := ordered[:min(limit, len(ordered))]
	readiness, err := readinessIn(ctx, tx, listed, repos, chart, q.Asker, r.limits().MergeByFloor)
	if err != nil {
		return err
	}
	var cards map[string]*ChangeCard
	if q.Cards {
		if cards, err = r.changeCardsIn(ctx, tx, listed); err != nil {
			return err
		}
	}
	// HOW FAR BACK: while the first build walks back, the instant its floor's id was minted.
	backTo, dated := time.Time{}, true
	if ours && held.BackFloor != "" {
		backTo, dated = statelog.OpMintedAt(held.BackFloor)
	}
	out.ChangesText = &ChangesText{Building: !covered || overflow || !dated, IndexedBackTo: backTo,
		Truncated: ours && held.Truncated}
	pg := page{maxRows: limit, maxB: r.pageBytes}
	for i, row := range listed {
		s := repos[row.repoID].summary(row)
		s.Readiness, s.Card = readiness[row.id], cards[row.id]
		out.Changes = append(out.Changes, s)
		if pg.add(s.encodedBytes()) {
			// A page cut by its rows or its bytes resumes after its last row, where any follow it.
			if i < len(ordered)-1 {
				out.Next = w.cursor(row)
			}
			return nil
		}
	}
	return nil
}

// heldAsItIs reports a row one of the index's walks has reached at its position: at or below where
// the first build began with an id it has reached back to, or past that and no further than the
// watermark. Their zero, an index built whole, leaves the watermark alone.
func heldAsItIs(held search.ChangeHeld, row changeRow) bool {
	if row.position <= held.BackPos {
		return held.BackFloor == "" || row.id >= held.BackFloor
	}
	return row.position <= held.Through
}

// scopedWords keeps the rows of q's scope, its asker's grant and its window, each with what its
// conversation says of it; repos gains every repository a kept row names.
func (r *Reader) scopedWords(ctx context.Context, tx *sql.Tx, q ReadQuery, chart Roster, repoID string,
	repos map[string]listedRepo, rows []changeRow) ([]changeRow, error) {

	var scoped []changeRow
	for _, row := range rows {
		if (repoID == "" || row.repoID == repoID) && (q.State == "" || row.state == q.State) &&
			(q.Owner == "" || row.owner == q.Owner) {
			scoped = append(scoped, row)
		}
	}
	if repoID == "" {
		listed, err := listedReposIn(ctx, tx, scoped, q.Asker, q.CeilingSeat, chart)
		if err != nil {
			return nil, err
		}
		for id, repo := range listed {
			repos[id] = repo
		}
	}
	var kept []changeRow
	for _, row := range scoped {
		if _, listed := repos[row.repoID]; listed {
			kept = append(kept, row)
		}
	}
	if err := withConversations(ctx, tx, kept); err != nil {
		return nil, err
	}
	if q.Window == "" {
		return kept, nil
	}
	since := store.EncodeTime(r.now().Add(-q.Window.Duration()))
	return slices.DeleteFunc(kept, func(row changeRow) bool { return row.moved < since }), nil
}

// inOrderAfter is rows in the walk's order, newest first, after cursor where it is set, as the walk's
// own cursor spells it; a walk by id is led by a filter the words never lead.
func (w changeWalk) inOrderAfter(rows []changeRow, cursor string) ([]changeRow, error) {
	var key func(changeRow) (int64, string)
	switch w.order {
	case byNumber:
		key = func(row changeRow) (int64, string) { return row.number, "" }
	case byOpening:
		key = func(row changeRow) (int64, string) { return row.opened, row.id }
	case byMove:
		key = func(row changeRow) (int64, string) { return row.moved, row.id }
	default:
		return nil, errors.New("gitlet: a listing its words lead is never walked by id: a reviewer, a need " +
			"or the ready bucket leads that walk, and each leads the words too")
	}
	slices.SortFunc(rows, func(a, b changeRow) int {
		ak, aid := key(a)
		bk, bid := key(b)
		return cmp.Or(cmp.Compare(bk, ak), strings.Compare(bid, aid))
	})
	if cursor == "" {
		return rows, nil
	}
	var at int64
	var id string
	if w.order == byNumber {
		n, err := strconv.ParseInt(cursor, 10, 64)
		if err != nil || n <= 0 {
			return nil, fmt.Errorf("%w: %q", ErrBadCursor, cursor)
		}
		at = n
	} else {
		var ok bool
		if at, id, ok = cutLogCursor(cursor); !ok {
			return nil, fmt.Errorf("%w: %q", ErrBadCursor, cursor)
		}
	}
	return slices.DeleteFunc(rows, func(row changeRow) bool {
		k, rid := key(row)
		return k > at || k == at && rid >= id
	}), nil
}

// holdingWords keeps, in order, the rows in whose title or description every term appears: a row its
// title alone answers is kept without its description, and the rest are read by their keys.
func holdingWords(ctx context.Context, tx *sql.Tx, rows []changeRow, terms []string) ([]changeRow, error) {
	var described []string
	for _, row := range rows {
		if _, all := textindex.Plain.Find(row.title, terms); !all && row.bodyBytes > 0 {
			described = append(described, row.id)
		}
	}
	bodies := make(map[string]string, len(described))
	for chunk := range slices.Chunk(described, MaxChangesPerPage) {
		if err := bodiesNamedIn(ctx, tx, chunk, bodies); err != nil {
			return nil, err
		}
	}
	var kept []changeRow
	for _, row := range rows {
		title, body := row.title, bodies[row.id]
		_, all := textindex.Plain.FindIn(func(yield func(int, string) bool) {
			if yield(0, title) && body != "" {
				yield(len(title)+1, body)
			}
		}, terms)
		if all {
			kept = append(kept, row)
		}
	}
	return kept, nil
}

// bodiesNamedIn reads into bodies the description of each change ids names that has one.
func bodiesNamedIn(ctx context.Context, tx *sql.Tx, ids []string, bodies map[string]string) error {
	list, err := jsonList(ids)
	if err != nil {
		return err
	}
	rows, err := tx.QueryContext(ctx, changeBodiesNamedSQL, list)
	if err != nil {
		return fmt.Errorf("gitlet: read the descriptions of %d changes: %w", len(ids), err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var id, body string
		if err = rows.Scan(&id, &body); err != nil {
			return fmt.Errorf("gitlet: read the descriptions of %d changes: %w", len(ids), err)
		}
		bodies[id] = body
	}
	return rows.Err()
}

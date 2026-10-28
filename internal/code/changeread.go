package code

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/store"
)

// THE READS OF CHANGES: a listing of them ([ReadChanges]) and one in detail
// ([ReadChange]), each served inside the one transaction [Reader.Read] hands
// it, from the rows the records on the change's REPOSITORY'S subject wrote
// (replicated 0058 and 0066) — and each held to the asker's grant on that
// repository ([JudgeRead]), and to the grant of the seat it reads for, as a
// read of the repository's own rows is: a change is the repository's
// contents, and a `visibility: unit` repository's changes are no more a seat
// outside the unit's to read than its push history is.
//
// # A listing walks the one index its filters lead
//
// Every listing names a repository, a work item or a state ([ReadQuery.State]
// says why), and walks ONE index, chosen by its filters in this order
// ([walkOf]):
//
//   - a work item named: code_changes_work_idx (recreated in its order by
//     replicated 0069), the item's changes — the narrowest set a listing
//     names, and still paged, since a recurring item gathers a change every
//     time it is worked;
//   - an owner and a state: code_changes_owner_idx, the owner's changes in
//     that state;
//   - a repository and a state: code_changes_repo_state_idx (replicated
//     0069), the repository's changes in that state;
//   - a repository: code_changes_number_idx, the repository's changes;
//   - a state: code_changes_state_idx, the company's changes in that state.
//
// NEWEST FIRST, always: a repository's walks by number, which is the order the
// code log opened its changes in (a number is the apply's); every other by the
// position of the record that opened each change and then its id, since one
// record can open several. A filter the walked index does not seek — a
// repository beside a work item or an owner, a state beside a work item, an
// owner beside a repository's or a work item's walk — is tested on each row
// the page read, never written into the statement: a test inside a statement
// that no index serves reads every row it rejects before its LIMIT fills,
// without bound, where a page that reads LIMIT rows and keeps what passes reads
// what its limit says. So a page is the rows it READ, its cursor the last of
// them, and it may be short, or empty with a cursor, as a seat's listing of
// repositories is; a client pages until no cursor comes back.
//
// # Held to the grant
//
// A listing that NAMES its repository is that repository's: judged once,
// before any change is read, and refused whole where the asker — or the seat
// it reads for — may not read the repository ([ErrNotPermitted],
// [ErrAboveCeiling]), as its push history is; it lists the repository's
// changes whatever the repository's state, a deleted one's among them, since
// it was named. A listing that names NONE lists the changes of every
// repository both may read — the repositories its page's changes name read
// in one statement, each judged once a page by the one chart the read holds —
// and of an active or archived one only, as a listing of repositories leaves
// out a deleted one unless asked for it: a deleted repository's changes are no
// work anybody can do until it is restored. One change ([Reader.change]) is
// read only from a repository the asker may read: that a change of a
// repository the grant guards exists is never answered, and its number is not
// a way to learn it ([ErrChangeNotFound] is about a repository the asker
// reads).
//
// # One change
//
// A change is read by its NUMBER on its repository, web!42, which is how a
// person and a seat name one: its row, its description and its authors on a
// first page, beside its revisions newest first, a page at a time; a later
// page, after the cursor of the one before — a revision's number — carries
// its revisions alone, judged against the grant again from the repository's
// row, as every page of a refs listing is. The detail rides beside a page's
// rows in the half of the estate reply [MaxPageBytes] leaves free, as a refs
// listing's first page carries its repository's: its description is at most
// [MaxChangeBodyBytes], and its authors are whoever pushed its source branch
// as themselves, each a seat or a person of the company, some thirty
// thousand of whom would fit.

// changeOrder is the order a listing of changes walks in, which its cursor is
// spelled in.
type changeOrder int

const (
	// byNumber is one repository's changes, newest first by number; its
	// cursor is the last number a page read.
	byNumber changeOrder = iota
	// byOpening is changes across repositories, newest first by the position
	// of the record that opened each and then by id; its cursor is a
	// [logCursor] of the two.
	byOpening
)

// changeWalk is the one index a listing of changes walks, as its filters
// choose it ([walkOf]).
type changeWalk struct {
	// index is the index it walks, which its statements name and the plan
	// tests hold them to ([changeWalk.read] issues them).
	index string
	// keys are the values its statements' equalities bind, in their order,
	// before a page's cursor and its limit.
	keys  []any
	order changeOrder
	// repo, state and owner are the filters its index does not seek, which a
	// page tests on each row it read ([changeWalk.keeps]).
	repo, state, owner bool
}

// walkOf is the walk q's filters choose, repoID the id of the repository q
// names — "" where it names none. q is a listing [ReadQuery.validate] passed:
// it names a repository, a work item or a state.
func walkOf(q ReadQuery, repoID string) changeWalk {
	named := repoID != ""
	switch {
	case q.Work != "":
		backend, id, _ := cutWork(q.Work)
		return changeWalk{index: changesWorkKey, keys: []any{string(backend), id}, order: byOpening,
			repo: named, state: q.State != "", owner: q.Owner != ""}
	case q.Owner != "" && q.State != "":
		return changeWalk{index: changesOwnerKey, keys: []any{q.Owner, string(q.State)}, order: byOpening,
			repo: named}
	case named && q.State != "":
		return changeWalk{index: changesRepoStateKey, keys: []any{repoID, string(q.State)}, order: byNumber}
	case named:
		return changeWalk{index: changesNumberKey, keys: []any{repoID}, order: byNumber, owner: q.Owner != ""}
	}
	return changeWalk{index: changesStateKey, keys: []any{string(q.State)}, order: byOpening}
}

// cursor is where a page that read row last ends, in the walk's order.
func (w changeWalk) cursor(row changeRow) string {
	if w.order == byNumber {
		return strconv.FormatInt(row.number, 10)
	}
	return logCursor(row.opened, row.id)
}

// resume is what a page after cursor binds between the walk's keys and its
// limit, and false where cursor is no cursor of this walk's order.
func (w changeWalk) resume(cursor string) ([]any, bool) {
	if w.order == byNumber {
		n, err := strconv.ParseInt(cursor, 10, 64)
		return []any{n}, err == nil && n > 0
	}
	// THE >= BESIDE THE OR keeps the index's range seek, which a bare OR
	// of the two terms would give up (planner.go).
	position, id, ok := cutLogCursor(cursor)
	return []any{position, position, id}, ok
}

// keeps reports whether row passes every filter of q the walk's index does
// not seek, repoID the repository q names.
func (w changeWalk) keeps(row changeRow, q ReadQuery, repoID string) bool {
	return (!w.repo || row.repoID == repoID) && (!w.state || row.state == q.State) &&
		(!w.owner || row.owner == q.Owner)
}

// read is one page of the walk's rows: at most limit, after cursor where it
// is set — every row the statement answered, kept or not — by the walk's two
// statements, each handed on as the constant it is, so the read gate follows
// every text this issues to the plan it registers (readindex_test.go).
func (w changeWalk) read(ctx context.Context, tx *sql.Tx, cursor string, limit int) ([]changeRow, error) {
	switch w.index {
	case changesWorkKey:
		return w.readBy(ctx, tx, changesOfWorkSQL, changesOfWorkAfterSQL, cursor, limit)
	case changesOwnerKey:
		return w.readBy(ctx, tx, changesOfOwnerSQL, changesOfOwnerAfterSQL, cursor, limit)
	case changesRepoStateKey:
		return w.readBy(ctx, tx, changesInRepoStateSQL, changesInRepoStateAfterSQL, cursor, limit)
	case changesNumberKey:
		return w.readBy(ctx, tx, changesOfRepoSQL, changesOfRepoAfterSQL, cursor, limit)
	case changesStateKey:
		return w.readBy(ctx, tx, changesInStateSQL, changesInStateAfterSQL, cursor, limit)
	}
	return nil, fmt.Errorf("code: no listing of changes walks %s", w.index)
}

// readBy is [changeWalk.read] by first, a first page's statement, binding the
// walk's keys and the page's limit, and after, a page's after a cursor,
// binding its keys, the cursor's values ([changeWalk.resume]) and the limit.
func (w changeWalk) readBy(ctx context.Context, tx *sql.Tx, first, after, cursor string, limit int) (
	[]changeRow, error) {

	query, args := first, slices.Clone(w.keys)
	if cursor != "" {
		resumed, ok := w.resume(cursor)
		if !ok {
			return nil, fmt.Errorf("%w: %q", ErrBadCursor, cursor)
		}
		query, args = after, append(args, resumed...)
	}
	rows, err := tx.QueryContext(ctx, query, append(args, limit)...)
	if err != nil {
		return nil, fmt.Errorf("code: list changes by %s: %w", w.index, err)
	}
	defer func() { _ = rows.Close() }()
	var out []changeRow
	for rows.Next() {
		row, scanErr := scanChange(rows.Scan)
		if scanErr != nil {
			return nil, fmt.Errorf("code: list changes by %s: %w", w.index, scanErr)
		}
		out = append(out, row)
	}
	return out, rows.Err()
}

// listedRepo is a repository a read of changes serves changes of: what a
// grant reads of its row, and the asker's access to it.
type listedRepo struct {
	ref    RepoRef
	access Access
}

// summary is row as a listing of changes renders it, from repository l.
func (l listedRepo) summary(row changeRow) ChangeSummary {
	return ChangeSummary{ChangeRow: row.wire(), Repo: l.ref.Name, Ref: ChangeRef(l.ref.Name, row.number),
		Access: l.access.String()}
}

// readableIn is the repository name names in tx as a read of its changes
// serves it — refused [ErrRepoNotFound] where nothing holds the name, and the
// asker's or its ceiling's refusal where either may not read it ([JudgeRead]).
func readableIn(ctx context.Context, tx *sql.Tx, name string, asker Principal, ceilingSeat string,
	chart Roster) (listedRepo, error) {

	id, found, err := claimIn(ctx, tx, name)
	if err != nil {
		return listedRepo{}, err
	}
	if !found {
		return listedRepo{}, fmt.Errorf("%w: %s", ErrRepoNotFound, name)
	}
	ref, err := grantRefIn(ctx, tx, id)
	if err != nil {
		return listedRepo{}, err
	}
	g, err := JudgeRead(asker, ceilingSeat, ref, chart)
	if err != nil {
		return listedRepo{}, err
	}
	return listedRepo{ref: ref, access: g.Access}, nil
}

// changes serves one page of a listing of changes: the walk q's filters
// choose ([walkOf]), the rows it read tested against the filters its index
// does not seek and held to the grant — see the file's doc.
func (r *Reader) changes(ctx context.Context, tx *sql.Tx, q ReadQuery, chart Roster, out *ReadAnswer) error {
	repos := map[string]listedRepo{}
	var repoID string
	if q.Repo != "" {
		named, err := readableIn(ctx, tx, q.Repo, q.Asker, q.CeilingSeat, chart)
		if err != nil {
			return err
		}
		repoID = named.ref.ID
		repos[repoID] = named
	}
	walk := walkOf(q, repoID)
	limit := min(limitOr(q.Limit, defaultChangeLimit), r.pageRows)
	scanned, err := walk.read(ctx, tx, q.Cursor, limit)
	if err != nil {
		return err
	}
	if repoID == "" {
		if repos, err = listedReposIn(ctx, tx, scanned, q.Asker, q.CeilingSeat, chart); err != nil {
			return err
		}
	}
	pg := page{maxRows: limit, maxB: r.pageBytes}
	for _, row := range scanned {
		repo, listed := repos[row.repoID]
		if !listed || !walk.keeps(row, q, repoID) {
			continue
		}
		s := repo.summary(row)
		out.Changes = append(out.Changes, s)
		if pg.add(s.encodedBytes()) {
			out.Next = walk.cursor(row)
			return nil
		}
	}
	if len(scanned) == limit {
		out.Next = walk.cursor(scanned[len(scanned)-1])
	}
	return nil
}

// listedReposIn is every repository a page of changes across repositories
// names whose changes the listing serves, by id: active or archived, and read
// by both asker and the seat it reads for ([JudgeRead]) — each read in ONE
// statement over the page's repositories, and judged once a page.
func listedReposIn(ctx context.Context, tx *sql.Tx, scanned []changeRow, asker Principal, ceilingSeat string,
	chart Roster) (map[string]listedRepo, error) {

	listed := map[string]listedRepo{}
	named := map[string]bool{}
	var ids []string
	for _, row := range scanned {
		if !named[row.repoID] {
			named[row.repoID] = true
			ids = append(ids, row.repoID)
		}
	}
	if len(ids) == 0 {
		return listed, nil
	}
	list, err := jsonList(ids)
	if err != nil {
		return nil, err
	}
	rows, err := tx.QueryContext(ctx, listedReposSQL, list)
	if err != nil {
		return nil, fmt.Errorf("code: read the repositories a page of changes names: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var ref RepoRef
		var status, owners, visibility string
		if scanErr := rows.Scan(&ref.ID, &ref.Name, &status, &owners, &ref.Owner.Ref, &visibility); scanErr != nil {
			return nil, fmt.Errorf("code: read the repositories a page of changes names: %w", scanErr)
		}
		ref.Status, ref.Owner.Kind, ref.Visibility = RepoStatus(status), OwnerKind(owners), Visibility(visibility)
		if ref.Status != RepoStatusActive && ref.Status != RepoStatusArchived {
			continue
		}
		g, refused := JudgeRead(asker, ceilingSeat, ref, chart)
		if refused != nil {
			continue
		}
		listed[ref.ID] = listedRepo{ref: ref, access: g.Access}
	}
	return listed, rows.Err()
}

// change serves one change by its number: on a first page its detail and its
// first page of revisions, and on a later page, after the number of the last
// revision the page before served, its revisions alone — every page judged
// against the grant from the repository's row ([readableIn]).
func (r *Reader) change(ctx context.Context, tx *sql.Tx, q ReadQuery, chart Roster, out *ReadAnswer) error {
	repo, err := readableIn(ctx, tx, q.Repo, q.Asker, q.CeilingSeat, chart)
	if err != nil {
		return err
	}
	row, found, err := changeByNumberIn(ctx, tx, repo.ref.ID, q.Change)
	if err != nil {
		return err
	}
	if !found {
		return fmt.Errorf("%w: %s", ErrChangeNotFound, ChangeRef(repo.ref.Name, q.Change))
	}
	limit := min(limitOr(q.Limit, defaultRevisionLimit), r.pageRows)
	var rows *sql.Rows
	if q.Cursor == "" {
		body, bodyErr := bodyOf(ctx, tx, row.id)
		if bodyErr != nil {
			return bodyErr
		}
		authors, authorsErr := authorsIn(ctx, tx, row.id)
		if authorsErr != nil {
			return authorsErr
		}
		out.Change = &ChangeDetail{ChangeSummary: repo.summary(row), Body: body, Authors: authors}
		rows, err = tx.QueryContext(ctx, revisionsSQL, row.id, limit)
	} else {
		n, parseErr := strconv.ParseInt(q.Cursor, 10, 64)
		if parseErr != nil || n < 1 {
			return fmt.Errorf("%w: %q is not a revision's number", ErrBadCursor, q.Cursor)
		}
		rows, err = tx.QueryContext(ctx, revisionsAfterSQL, row.id, n, limit)
	}
	if err != nil {
		return fmt.Errorf("code: read the revisions of %s: %w", ChangeRef(repo.ref.Name, row.number), err)
	}
	defer func() { _ = rows.Close() }()
	pg := page{maxRows: limit, maxB: r.pageBytes}
	for rows.Next() {
		var (
			rev      RevisionRow
			kind     string
			position int64
			at       int64
		)
		if scanErr := rows.Scan(&rev.N, &rev.Head, &rev.Pusher, &kind, &rev.PushID, &position, &at); scanErr != nil {
			return fmt.Errorf("code: read the revisions of %s: %w", ChangeRef(repo.ref.Name, row.number), scanErr)
		}
		rev.PusherKind, rev.Position, rev.At = PartyKind(kind), unpacked(position), store.DecodeTime(at)
		out.Revisions = append(out.Revisions, rev)
		if pg.add(rev.encodedBytes()) {
			out.Next = strconv.FormatInt(rev.N, 10)
			break
		}
	}
	return rows.Err()
}

// changeByNumberIn is the change number names on repository repoID in tx, and
// false where it holds none — one seek of code_changes_number_idx.
func changeByNumberIn(ctx context.Context, tx *sql.Tx, repoID string, number int64) (changeRow, bool, error) {
	row, err := scanChange(tx.QueryRowContext(ctx, changeByNumberSQL, repoID, number).Scan)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return changeRow{}, false, nil
	case err != nil:
		return changeRow{}, false, fmt.Errorf("code: read change %d of %s: %w", number, repoID, err)
	}
	return row, true, nil
}

// authorsIn is every author of change id in tx, by handle — one range of the
// authors' key.
func authorsIn(ctx context.Context, tx *sql.Tx, id string) ([]ChangeAuthor, error) {
	rows, err := tx.QueryContext(ctx, changeAuthorsSQL, id)
	if err != nil {
		return nil, fmt.Errorf("code: read the authors of change %s: %w", id, err)
	}
	defer func() { _ = rows.Close() }()
	out := []ChangeAuthor{}
	for rows.Next() {
		var (
			a        ChangeAuthor
			kind     string
			position int64
		)
		if scanErr := rows.Scan(&a.Author, &kind, &a.FirstRevision, &position); scanErr != nil {
			return nil, fmt.Errorf("code: read the authors of change %s: %w", id, scanErr)
		}
		a.Kind, a.Position = PartyKind(kind), unpacked(position)
		out = append(out, a)
	}
	return out, rows.Err()
}

// wire is the change as a read serves it.
func (r changeRow) wire() ChangeRow {
	out := ChangeRow{ID: r.id, RepoID: r.repoID, Number: r.number, Title: r.title, State: r.state,
		SourceRef: r.source, TargetRef: r.target, Head: r.head, Revision: r.revision, Owner: r.owner,
		OwnerKind: r.ownerKind, OwnerOperatorID: r.operator, CreatedTurn: r.turn, CreatedRun: r.run,
		OpenedAt: store.DecodeTime(r.openedAt), OpenedPosition: unpacked(r.opened), CloseReason: r.closeReason,
		MergedVia: r.mergedVia, MergeOID: r.mergeOID, BodyBytes: r.bodyBytes, Version: r.version,
		Position: unpacked(r.position)}
	if r.work.ID != "" {
		work := r.work
		out.WorkItem = &work
	}
	if r.closedAt.Valid {
		out.ClosedAt = store.DecodeTime(r.closedAt.Int64)
	}
	if r.mergedAt.Valid {
		out.MergedAt = store.DecodeTime(r.mergedAt.Int64)
	}
	return out
}

// validateChanges is [ReadQuery.validate] for a `changes` listing: it names a
// repository, a work item or a state — what the indexes it walks are led by
// ([walkOf]) — each a value one could be, and an owner by a seat's handle.
func (q ReadQuery) validateChanges() error {
	refused := func(field, why string) error { return fmt.Errorf("%w: %s: %s", ErrInvalid, field, why) }
	switch {
	case q.Repo == "" && q.Work == "" && q.State == "":
		return refused("state", "a listing of changes names a repository, a work item or a state, which "+
			"the indexes it walks are each led by — no index holds every change the company has, and an "+
			"owner's changes are kept by state")
	case q.State != "" && !q.State.Valid():
		return refused("state", fmt.Sprintf("%q is not one of %v", q.State, ChangeStates()))
	case q.Owner != "" && !org.ValidHandle(q.Owner):
		return refused("owner", fmt.Sprintf("%q is not a seat's handle: a change's owner is a seat, or the "+
			"human seat a person holds", q.Owner))
	}
	if q.Work != "" {
		if _, _, why := cutWork(q.Work); why != "" {
			return refused("work", why)
		}
	}
	return nil
}

// cutWork reads a work item's identity across trackers, `<backend>:<id>`
// ([types.WorkItem.Ref]) — a backend this build knows and an id a change's row
// may hold ([workItemProblem]) — or says why it is none.
func cutWork(ref string) (types.WorkBackend, string, string) {
	backend, id, ok := strings.Cut(ref, ":")
	if !ok {
		return "", "", fmt.Sprintf("%q is not a work item's identity across trackers, <backend>:<id>", ref)
	}
	w := types.WorkItem{Backend: types.WorkBackend(backend), ID: id}
	if why := workItemProblem(w); why != "" {
		return "", "", why
	}
	return w.Backend, w.ID, ""
}

// The keys of the change listings' indexes, each named by its statements —
// a GUARD as planner.go says, which a schema change that renames one fails at
// its first run — and held by plan_test.go to the seek each is written for.
const (
	// changesRepoStateKey is code_changes_repo_state_idx, (repo_id, state,
	// number) (replicated 0069).
	changesRepoStateKey = "code_changes_repo_state_idx"
	// changesStateKey is code_changes_state_idx, (state, opened_position,
	// id) (replicated 0058 and 0066).
	changesStateKey = "code_changes_state_idx"
	// changesOwnerKey is code_changes_owner_idx, (owner, state,
	// opened_position, id) (replicated 0058 and 0066).
	changesOwnerKey = "code_changes_owner_idx"
	// changesWorkKey is code_changes_work_idx, (work_backend, work_id,
	// opened_position, id) WHERE work_id != '' (replicated 0069).
	changesWorkKey = "code_changes_work_idx"
)

// The reads of changes, each registered in plan_test.go so the pinned driver's
// planner is held to the index each is written for, and to reading in its
// order: every walk's first page and its page after a cursor, newest first —
// a repository's by number, below the cursor's; every other by the opening
// record's position and then the id, with the <= beside the OR that keeps the
// range seek (planner.go) — one change by its number, its authors by their
// key, its revisions newest first by theirs, and the repositories a page of
// changes across repositories names, driven from their list.
const (
	changesOfRepoSQL = `SELECT ` + changeColumns + ` FROM code_changes INDEXED BY ` + changesNumberKey + `
		WHERE repo_id = ? ORDER BY number DESC LIMIT ?`
	changesOfRepoAfterSQL = `SELECT ` + changeColumns + ` FROM code_changes INDEXED BY ` + changesNumberKey + `
		WHERE repo_id = ? AND number < ? ORDER BY number DESC LIMIT ?`
	changesInRepoStateSQL = `SELECT ` + changeColumns + ` FROM code_changes INDEXED BY ` +
		changesRepoStateKey + ` WHERE repo_id = ? AND state = ? ORDER BY number DESC LIMIT ?`
	changesInRepoStateAfterSQL = `SELECT ` + changeColumns + ` FROM code_changes INDEXED BY ` +
		changesRepoStateKey + ` WHERE repo_id = ? AND state = ? AND number < ? ORDER BY number DESC LIMIT ?`
	changesInStateSQL = `SELECT ` + changeColumns + ` FROM code_changes INDEXED BY ` + changesStateKey + `
		WHERE state = ? ORDER BY opened_position DESC, id DESC LIMIT ?`
	changesInStateAfterSQL = `SELECT ` + changeColumns + ` FROM code_changes INDEXED BY ` + changesStateKey + `
		WHERE state = ? AND opened_position <= ? AND (opened_position < ? OR id < ?)
		ORDER BY opened_position DESC, id DESC LIMIT ?`
	changesOfOwnerSQL = `SELECT ` + changeColumns + ` FROM code_changes INDEXED BY ` + changesOwnerKey + `
		WHERE owner = ? AND state = ? ORDER BY opened_position DESC, id DESC LIMIT ?`
	changesOfOwnerAfterSQL = `SELECT ` + changeColumns + ` FROM code_changes INDEXED BY ` + changesOwnerKey + `
		WHERE owner = ? AND state = ? AND opened_position <= ? AND (opened_position < ? OR id < ?)
		ORDER BY opened_position DESC, id DESC LIMIT ?`
	// A WORK ITEM'S, its index's partial predicate repeated verbatim: the
	// pinned planner uses a partial index only for a statement that implies
	// it in its own words.
	changesOfWorkSQL = `SELECT ` + changeColumns + ` FROM code_changes INDEXED BY ` + changesWorkKey + `
		WHERE work_backend = ? AND work_id = ? AND work_id != ''
		ORDER BY opened_position DESC, id DESC LIMIT ?`
	changesOfWorkAfterSQL = `SELECT ` + changeColumns + ` FROM code_changes INDEXED BY ` + changesWorkKey + `
		WHERE work_backend = ? AND work_id = ? AND work_id != ''
			AND opened_position <= ? AND (opened_position < ? OR id < ?)
		ORDER BY opened_position DESC, id DESC LIMIT ?`
	changeByNumberSQL = `SELECT ` + changeColumns + ` FROM code_changes INDEXED BY ` + changesNumberKey + `
		WHERE repo_id = ? AND number = ?`
	changeAuthorsSQL = `SELECT author, author_kind, first_revision, position FROM code_change_authors
		WHERE change_id = ? ORDER BY author`
	revisionsSQL = `SELECT n, head_oid, pusher, pusher_kind, push_id, position, at FROM code_revisions
		WHERE change_id = ? ORDER BY n DESC LIMIT ?`
	revisionsAfterSQL = `SELECT n, head_oid, pusher, pusher_kind, push_id, position, at FROM code_revisions
		WHERE change_id = ? AND n < ? ORDER BY n DESC LIMIT ?`
	listedReposSQL = `SELECT id, name, status, owner_kind, owner, visibility FROM code_repos
		WHERE id IN (SELECT CAST(value AS TEXT) FROM json_each(?))`
)

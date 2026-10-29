package code

import (
	"context"
	"database/sql"
	"encoding/json"
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
// Every listing names a repository, a work item, a state, a reviewer or what
// its changes wait on ([ReadQuery.State] says why), and walks ONE index,
// chosen by its filters in this order ([walkOf]):
//
//   - a reviewer, or a need of a review: code_reviewers_reviewer_idx
//     (replicated 0072), the changes whose ask of that reviewer stands —
//     the set a reviewer is asked to work through, newest change first
//     (conversationread.go);
//   - a work item named: code_changes_work_idx (recreated in its order by
//     replicated 0069), the item's changes — the narrowest set a listing
//     names, and still paged, since a recurring item gathers a change every
//     time it is worked;
//   - an owner and a state — or a need of a fix, an owner's in one live
//     state (conversationread.go): code_changes_owner_idx, the owner's
//     changes in that state;
//   - a repository and a state: code_changes_repo_state_idx (replicated
//     0069), the repository's changes in that state;
//   - a repository: code_changes_number_idx, the repository's changes;
//   - a state: code_changes_state_idx, the company's changes in that state.
//
// NEWEST FIRST, always: a repository's walks by number, which is the order the
// code log opened its changes in (a number is the apply's); a reviewer's by the
// change's id, a UUIDv7 minted when it was opened; every other by the position
// of the record that opened each change and then its id, since one record can
// open several. A filter the walked index does not seek — a repository beside
// a work item, an owner or a reviewer, a state beside a work item or a
// reviewer, an owner beside a repository's, a work item's or a reviewer's
// walk, a work item beside a reviewer's, and whether a change awaits its
// owner's fix, which its conversation's counters say — is tested on each row
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
	// byAsk is the changes a reviewer is asked to review, newest first by
	// the change's id; its cursor is the last id a page read.
	byAsk
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
	// repo, state, owner and work are the filters its index does not seek,
	// which a page tests on each row it read ([changeWalk.keeps]); live
	// keeps a draft or open change alone, for a listing of what a live
	// change waits on; fix keeps a change awaiting its owner's fix alone,
	// which its conversation's counters say ([needingFixIn]).
	repo, state, owner, work, live, fix bool
}

// walkOf is the walk q's filters choose, repoID the id of the repository q
// names — "" where it names none. q is a listing [ReadQuery.validate] passed,
// a need of a fix read as [ReadQuery.fixing] reads it: it names a repository,
// a work item, a state, a reviewer or a need.
func walkOf(q ReadQuery, repoID string) changeWalk {
	named, fix := repoID != "", q.Needs == NeedFix
	switch {
	case q.Reviewer != "" || q.Needs == NeedReview:
		return changeWalk{index: reviewerAsksKey, keys: []any{q.waitedOn()}, order: byAsk, repo: named,
			state: q.State != "", owner: q.Owner != "", work: q.Work != "", live: q.Needs == NeedReview}
	case q.Work != "":
		backend, id, _ := cutWork(q.Work)
		return changeWalk{index: changesWorkKey, keys: []any{string(backend), id}, order: byOpening,
			repo: named, state: q.State != "", owner: q.Owner != "", fix: fix}
	case q.Owner != "" && q.State != "":
		return changeWalk{index: changesOwnerKey, keys: []any{q.Owner, string(q.State)}, order: byOpening,
			repo: named, fix: fix}
	case named && q.State != "":
		return changeWalk{index: changesRepoStateKey, keys: []any{repoID, string(q.State)}, order: byNumber}
	case named:
		return changeWalk{index: changesNumberKey, keys: []any{repoID}, order: byNumber, owner: q.Owner != ""}
	}
	return changeWalk{index: changesStateKey, keys: []any{string(q.State)}, order: byOpening}
}

// cursor is where a page that read row last ends, in the walk's order.
func (w changeWalk) cursor(row changeRow) string {
	switch w.order {
	case byNumber:
		return strconv.FormatInt(row.number, 10)
	case byAsk:
		return row.id
	}
	return logCursor(row.opened, row.id)
}

// resume is what a page after cursor binds between the walk's keys and its
// limit, and false where cursor is no cursor of this walk's order.
func (w changeWalk) resume(cursor string) ([]any, bool) {
	switch w.order {
	case byNumber:
		n, err := strconv.ParseInt(cursor, 10, 64)
		return []any{n}, err == nil && n > 0
	case byAsk:
		return []any{cursor}, bareUUIDv7("cursor", cursor) == nil
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
		(!w.owner || row.owner == q.Owner) && (!w.work || row.work.Ref() == q.Work) &&
		(!w.live || row.state.Live())
}

// read is one page of the walk's rows: at most limit, after cursor where it
// is set — every row the statement answered, kept or not — by the walk's two
// statements, each handed on as the constant it is, so the read gate follows
// every text this issues to the plan it registers (readindex_test.go).
func (w changeWalk) read(ctx context.Context, tx *sql.Tx, cursor string, limit int) ([]changeRow, error) {
	switch w.index {
	case reviewerAsksKey:
		return w.readAsked(ctx, tx, cursor, limit)
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
// does not seek and held to the grant — see the file's doc — and a listing of
// what waits on the asker a page of one of its two halves
// ([Reader.changesWaitingOnMe]).
func (r *Reader) changes(ctx context.Context, tx *sql.Tx, q ReadQuery, chart Roster, out *ReadAnswer) error {
	switch q.Needs {
	case NeedMe:
		return r.changesWaitingOnMe(ctx, tx, q, chart, out)
	case NeedFix:
		q = q.fixing()
	}
	return r.listChanges(ctx, tx, q, chart, out, "")
}

// listChanges is [Reader.changes] over one walk, leaving out — of a walk of
// the changes awaiting a fix — those whose ask of asked stands, where asked
// names a reviewer: the first half of a listing of what waits on asked lists
// those.
func (r *Reader) listChanges(ctx context.Context, tx *sql.Tx, q ReadQuery, chart Roster, out *ReadAnswer,
	asked string) error {

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
	var fixing map[string]bool
	if walk.fix {
		if fixing, err = needingFixIn(ctx, tx, scanned, asked); err != nil {
			return err
		}
	}
	pg := page{maxRows: limit, maxB: r.pageBytes}
	for _, row := range scanned {
		repo, listed := repos[row.repoID]
		if !listed || !walk.keeps(row, q, repoID) || walk.fix && !fixing[row.id] {
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

	ids := make([]string, 0, len(scanned))
	for _, row := range scanned {
		ids = append(ids, row.repoID)
	}
	return listedReposAmong(ctx, tx, ids, asker, ceilingSeat, chart, "a page of changes")
}

// listedReposAmong is every repository of ids — each named once however often
// it is named, in any order — whose rows a listing across repositories serves,
// by id: active or archived, and read by both asker and the seat it reads for
// ([JudgeRead]) — each read in ONE statement over the page's repositories, and
// judged once a page. what names the page in a failure's sentence.
func listedReposAmong(ctx context.Context, tx *sql.Tx, ids []string, asker Principal, ceilingSeat string,
	chart Roster, what string) (map[string]listedRepo, error) {

	listed := map[string]listedRepo{}
	named := map[string]bool{}
	var distinct []string
	for _, id := range ids {
		if !named[id] {
			named[id] = true
			distinct = append(distinct, id)
		}
	}
	if len(distinct) == 0 {
		return listed, nil
	}
	list, err := jsonList(distinct)
	if err != nil {
		return nil, err
	}
	rows, err := tx.QueryContext(ctx, listedReposSQL, list)
	if err != nil {
		return nil, fmt.Errorf("code: read the repositories %s names: %w", what, err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var ref RepoRef
		var status, owners, visibility string
		if scanErr := rows.Scan(&ref.ID, &ref.Name, &status, &owners, &ref.Owner.Ref, &visibility); scanErr != nil {
			return nil, fmt.Errorf("code: read the repositories %s names: %w", what, scanErr)
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

// change serves one change by its number: on a first page of its revisions
// its detail beside them, and on a later page, after the number of the last
// revision the page before served, its revisions alone; and a page of the
// part of its conversation the read names ([ReadQuery.Part]) — every page
// judged against the grant from the repository's row ([readableIn]).
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
	switch q.Part {
	case PartReviewers:
		return r.changeReviewers(ctx, tx, q, ChangeRef(repo.ref.Name, row.number), row.id, out)
	case PartComments:
		return r.changeComments(ctx, tx, q, ChangeRef(repo.ref.Name, row.number), row.id, out)
	case PartReviews:
		return r.changeReviews(ctx, tx, q, ChangeRef(repo.ref.Name, row.number), row.id, out)
	case PartFindings:
		return r.changeFindings(ctx, tx, q, ChangeRef(repo.ref.Name, row.number), row.id, out)
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
		conversation, conversationErr := conversationRowIn(ctx, tx, row.id)
		if conversationErr != nil {
			return conversationErr
		}
		merge, mergeErr := mergeabilityRowIn(ctx, tx, row.id)
		if mergeErr != nil {
			return mergeErr
		}
		out.Change = &ChangeDetail{ChangeSummary: repo.summary(row), Body: body, Authors: authors,
			Conversation: conversation, Mergeability: merge}
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
			facts    factsScan
		)
		if scanErr := rows.Scan(append([]any{&rev.N, &rev.Head, &rev.Pusher, &kind, &rev.PushID, &position, &at},
			facts.targets()...)...); scanErr != nil {
			return fmt.Errorf("code: read the revisions of %s: %w", ChangeRef(repo.ref.Name, row.number), scanErr)
		}
		rev.PusherKind, rev.Position, rev.At = PartyKind(kind), unpacked(position), store.DecodeTime(at)
		if rev.Facts, err = facts.row(); err != nil {
			return fmt.Errorf("code: read revision %d of %s's facts: %w", rev.N, ChangeRef(repo.ref.Name,
				row.number), err)
		}
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

// factsScan is one revision's facts as a read joining the revisions to them
// scans them ([revisionFactsColumns]): every column nullable, since a revision
// the home has recorded no facts of joins none.
type factsScan struct {
	n                                               sql.NullInt64
	targetRef, targetOID, base, patch, node, digest sql.NullString
	owners, skipped, touched                        sql.NullString
	targetPosition, position                        sql.NullInt64
	omitted, touchesAll                             sql.NullBool
}

// targets is where a scan of [revisionFactsColumns] writes, in their order.
func (s *factsScan) targets() []any {
	return []any{&s.n, &s.targetRef, &s.targetOID, &s.targetPosition, &s.base, &s.patch, &s.omitted,
		&s.touchesAll, &s.node, &s.digest, &s.position, &s.owners, &s.skipped, &s.touched}
}

// row is the facts as a read serves them, and nil for a revision of none.
func (s *factsScan) row() (*RevisionFactsRow, error) {
	if !s.n.Valid {
		return nil, nil
	}
	f := &RevisionFactsRow{TargetRef: s.targetRef.String, TargetOID: s.targetOID.String,
		TargetPosition: unpacked(s.targetPosition.Int64), BaseOID: s.base.String, PatchID: s.patch.String,
		PatchIDOmitted: s.omitted.Bool, Touched: []string{}, TouchesAll: s.touchesAll.Bool, Node: s.node.String,
		OwnersDigest: s.digest.String, Position: unpacked(s.position.Int64)}
	if s.touched.String != "" {
		f.Touched = strings.Split(s.touched.String, "\x00")
	}
	if s.owners.String != "" {
		if err := json.Unmarshal([]byte(s.owners.String), &f.RequestedOwners); err != nil {
			return nil, fmt.Errorf("its matched owners: %w", err)
		}
	}
	if s.skipped.String != "" {
		if err := json.Unmarshal([]byte(s.skipped.String), &f.SkippedOwners); err != nil {
			return nil, fmt.Errorf("its skipped owners: %w", err)
		}
	}
	return f, nil
}

// validateChanges is [ReadQuery.validate] for a `changes` listing: it names a
// repository, a work item, a state, a reviewer or a need — what the indexes it
// walks are led by ([walkOf]) — each a value one could be, an owner and a
// reviewer by a seat's handle; and a need in a live state, whose one it waits
// on it names or its asker's seat is — a reviewer for a review, an owner for
// a fix, and for what waits on the asker neither, as that is the asker's own.
func (q ReadQuery) validateChanges() error {
	refused := func(field, why string) error { return fmt.Errorf("%w: %s: %s", ErrInvalid, field, why) }
	switch {
	case q.Repo == "" && q.Work == "" && q.State == "" && q.Reviewer == "" && q.Needs == "":
		return refused("state", "a listing of changes names a repository, a work item, a state, a reviewer "+
			"or what its changes wait on, which the indexes it walks are each led by — no index holds every "+
			"change the company has, and an owner's changes are kept by state")
	case q.State != "" && !q.State.Valid():
		return refused("state", fmt.Sprintf("%q is not one of %v", q.State, ChangeStates()))
	case q.Owner != "" && !org.ValidHandle(q.Owner):
		return refused("owner", fmt.Sprintf("%q is not a seat's handle: a change's owner is a seat, or the "+
			"human seat a person holds", q.Owner))
	case q.Reviewer != "" && !org.ValidHandle(q.Reviewer):
		return refused("reviewer", fmt.Sprintf("%q is not a seat's handle: a reviewer is a seat, or the "+
			"human seat a person holds", q.Reviewer))
	case q.Needs != "" && !q.Needs.Valid():
		return refused("needs", fmt.Sprintf("%q is not one of %v", q.Needs, Needs()))
	case q.Needs != "" && q.State != "" && !q.State.Live():
		return refused("state", fmt.Sprintf("needs=%s lists live changes, and a %s change waits on nobody",
			q.Needs, q.State))
	case q.Needs == NeedFix && q.Reviewer != "":
		return refused("reviewer", "needs=fix lists an owner's changes awaiting their fix; the changes awaiting "+
			"a reviewer's review are needs=review")
	case q.Needs == NeedMe && (q.Reviewer != "" || q.Owner != ""):
		return refused("needs", "needs=me is what waits on the asker, and names no reviewer and no owner")
	case q.Needs != "" && q.waitedOn() == "":
		whose := map[Need]string{NeedReview: "unless it names whose — a reviewer — ",
			NeedFix: "unless it names whose — an owner — "}[q.Needs]
		return refused("needs", fmt.Sprintf("needs=%s is the asker's own %sand this credential is bound to no "+
			"seat", q.Needs, whose))
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
	// A PAGE OF REVISIONS WITH THE FACTS RECORDED OF EACH, each revision's by
	// the facts' key beside its own — a revision of none reads a NULL key —
	// newest first in the revisions' key's order.
	revisionsSQL = `SELECT r.n, r.head_oid, r.pusher, r.pusher_kind, r.push_id, r.position, r.at, ` +
		revisionFactsColumns + ` FROM code_revisions AS r LEFT JOIN code_revision_facts AS f
			ON f.change_id = r.change_id AND f.n = r.n
		WHERE r.change_id = ? ORDER BY r.n DESC LIMIT ?`
	revisionsAfterSQL = `SELECT r.n, r.head_oid, r.pusher, r.pusher_kind, r.push_id, r.position, r.at, ` +
		revisionFactsColumns + ` FROM code_revisions AS r LEFT JOIN code_revision_facts AS f
			ON f.change_id = r.change_id AND f.n = r.n
		WHERE r.change_id = ? AND r.n < ? ORDER BY r.n DESC LIMIT ?`
	// revisionFactsColumns is every column of a revision's facts a read
	// serves, in [factsScan]'s order.
	revisionFactsColumns = `f.n, f.target_ref, f.target_oid, f.target_position, f.base_oid, f.patch_id,
		f.patch_id_omitted, f.touches_all, f.node, f.owners_digest, f.position, f.owners, f.skipped, f.touched`
	listedReposSQL = `SELECT id, name, status, owner_kind, owner, visibility FROM code_repos
		WHERE id IN (SELECT CAST(value AS TEXT) FROM json_each(?))`
)

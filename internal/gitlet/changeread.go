package gitlet

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
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/store"
	"github.com/crewlet/crewlet/internal/textindex"
	"github.com/crewlet/crewlet/internal/words"
)

// THE READS OF CHANGES: a listing ([ReadChanges]) and one in detail
// ([ReadChange]), each inside the one transaction [Reader.Read] hands it, from
// the rows the records on the change's REPOSITORY'S subject wrote, and each
// held to the grant of the asker and of the seat it reads for on that
// repository ([JudgeRead]): a change is the repository's contents, so a
// `visibility: unit` repository's changes are no more readable outside the
// unit than its push history.

// A listing walks ONE index, the one its filters lead ([walkOf]), newest first
// in the walk's order ([changeOrder]). A filter that index does not seek is
// tested on each row the page read ([changeWalk.keeps]), never written into
// the statement: a test no index serves reads every row it rejects before its
// LIMIT fills, without bound. So a page is the rows it READ, its cursor the
// last of them, and it may be short, or empty with a cursor; a client pages
// until no cursor comes back. The readiness bucket ([ReadQuery.Readiness]) is
// tested on each row the same way, once the page's readiness is judged.

// HELD TO THE GRANT: a listing that names its repository is judged once,
// before any change is read, and refused whole where the asker or its seat may
// not read it ([ErrNotPermitted], [ErrAboveCeiling]); it lists that
// repository's changes whatever its state, since it was named. A listing that
// names none serves only repositories both may read and that are in use, judged
// once a page ([listedReposIn]): a deleted repository's changes are no work
// anybody can do until it is restored. One change ([Reader.change]) is read
// only from a repository the asker may read, so its number never reveals that
// a guarded repository's change exists ([ErrChangeNotFound]).

// ONE CHANGE is read by its number on its repository (web!42): a first page
// carries its row, description and authors beside its newest revisions; a
// later page, after a revision's number, its revisions alone, judged against
// the grant again as every page of a refs listing is. The detail fits the half
// of the estate reply [MaxPageBytes] leaves free: its description is at most
// [MaxChangeBodyBytes], and some thirty thousand authors would fit.

// ITS READINESS ([ChangeSummary.Readiness]), on a live change's detail and
// every listed row, is its target's merge gate judged ([JudgeGate]) in the
// read's transaction against the read's one chart, a page's rows read a kind
// at a time ([gateFactsIn]), never the mergeability last stored: a read answers
// as of its snapshot. So a reader holding no chart refuses ([Reader.chartFor]).

// A PAGE KEPT SHORTER THAN SERVED: every row names the cursor that resumes
// after it ([RevisionRow.Cursor], [ReadQuery.ChangesCursor] and kin), and a
// page's own cursor is its last row's, by the same function. So a caller whose
// answer is smaller than the estate's reply (a code tool's) keeps a page's
// first rows and resumes after the last it kept without learning how a walk
// spells a cursor; rows read past it are read again by the next page.

// changeOrder is the order a listing of changes walks in, which its cursor is
// spelled in.
type changeOrder int

const (
	// byNumber is one repository's changes, newest first by number (the
	// apply's, so the code log's opening order); its cursor is the last number.
	byNumber changeOrder = iota
	// byOpening is changes across repositories, newest first by the position
	// of the record that opened each, then by id since one record can open
	// several; its cursor is a [logCursor] of the two.
	byOpening
	// byID is the changes a reviewer is asked to review, or whose stored
	// readiness waits on a person or is ready, newest first by id (a UUIDv7
	// minted at opening); its cursor is the last id.
	byID
	// byMove is the changes that moved within a window, or a live state's
	// asked by when they moved, newest first by last move, then id
	// ([windowWalk], [changeWalk.liveMovedWalk]); its cursor is a [logCursor].
	byMove
)

// changeWalk is the one index a listing of changes walks ([walkOf]).
type changeWalk struct {
	// index is the index its statements name, which the plan tests hold them to.
	index string
	// keys are what its statements' equalities bind, in order, before a page's
	// cursor and its limit.
	keys  []any
	order changeOrder
	// The filters its index does not seek, tested on each row a page read
	// ([changeWalk.keeps]): repo, state, owner and work; live keeps a draft or
	// open change; fix one awaiting its owner's fix ([needingFixIn]); person
	// one whose judged readiness waits on a person ([Readiness.WaitsPerson]);
	// window one that moved at or after since, a broker instant
	// (store.EncodeTime) — what a window's own walk seeks too.
	repo, state, owner, work, live, fix, person, window bool
	since                                               int64
	// scan bounds a live state's conversation side ([MaxSpokenScanned]).
	scan int
}

// walkOf is the walk q's filters choose, repoID the id of the repository q
// names ("" for none) and since the start of its window as a broker instant
// (store.EncodeTime; 0 for none). q passed [ReadQuery.validate], a need of a
// fix already read by [ReadQuery.fixing].
func walkOf(q ReadQuery, repoID string, since int64) changeWalk {
	w := walkLedBy(q, repoID)
	if q.Window != "" {
		// A walk by move bounds itself by the window rather than testing it.
		w.window, w.since = w.order != byMove, since
	}
	return w
}

// walkLedBy is [walkOf]'s walk before the window it tests: the one index
// q's filters lead, a window's own where nothing else leads it.
func walkLedBy(q ReadQuery, repoID string) changeWalk {
	named, fix := repoID != "", q.Needs == NeedFix
	switch {
	case q.Needs == NeedPerson:
		return changeWalk{index: waitsKey, order: byID, repo: named, state: q.State != "", owner: q.Owner != "",
			work: q.Work != "", live: true, person: true}
	case q.Reviewer != "" || q.Needs == NeedReview:
		return changeWalk{index: reviewerAsksKey, keys: []any{q.waitedOn()}, order: byID, repo: named,
			state: q.State != "", owner: q.Owner != "", work: q.Work != "", live: q.Needs == NeedReview}
	case q.Readiness == BucketReady && q.Needs == "" && q.ChangeSort != ChangeSortUpdated:
		return changeWalk{index: readyKey, order: byID, repo: named, state: q.State != "",
			owner: q.Owner != "", work: q.Work != "", live: true}
	case q.ChangeSort == ChangeSortUpdated && q.State.Live():
		if named {
			return changeWalk{index: changesRepoLiveMovedKey, keys: []any{repoID, string(q.State)}, order: byMove,
				owner: q.Owner != "", work: q.Work != ""}
		}
		return changeWalk{index: changesLiveMovedKey, keys: []any{string(q.State)}, order: byMove,
			owner: q.Owner != "", work: q.Work != ""}
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
	case q.Window != "" && !q.State.Live():
		// A WINDOW LEADS ahead of a terminal state, whose index holds every
		// change that ever merged or closed. A LIVE state leads instead (below)
		// and the window is tested on its rows: its index holds the bounded
		// live set, where a window's walk would read mostly merged changes to
		// keep the few open ones (TestAWindowLeadsNoLiveState).
		return changeWalk{index: changesMovedKey, order: byMove, state: q.State != "", owner: q.Owner != ""}
	}
	return changeWalk{index: changesStateKey, keys: []any{string(q.State)}, order: byOpening}
}

// ChangesCursor is the cursor that resumes q, a `changes` listing, after s, a
// change a page of it served: what a caller keeping fewer of a page's changes
// than it was served hands back. It is spelled by the walk the page was read
// by ([walkOf]) — a need of a fix's owner walk, or the half of what waits on
// the asker the page came from, the second behind [fixHalf].
func (q ReadQuery) ChangesCursor(s ChangeSummary) string {
	q = q.readying()
	prefix := ""
	switch q.Needs {
	case NeedMe:
		half, fixes := q.halfOfMe()
		if fixes {
			prefix = fixHalf
		}
		q = half
	case NeedFix:
		q = q.fixing()
	}
	// Every row of a listing naming a repository is of it, so the row's id stands in.
	repoID := ""
	if q.Repo != "" {
		repoID = s.RepoID
	}
	row := changeRow{id: s.ID, number: s.Number, opened: s.OpenedPosition.Packed(),
		moved: store.EncodeTime(s.UpdatedAt)}
	return prefix + walkOf(q, repoID, 0).cursor(row)
}

// cursor is where a page that read row last ends, in the walk's order.
func (w changeWalk) cursor(row changeRow) string {
	switch w.order {
	case byNumber:
		return strconv.FormatInt(row.number, 10)
	case byID:
		return row.id
	case byMove:
		return movedCursor(row)
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
	case byID:
		return []any{cursor}, bareUUIDv7("cursor", cursor) == nil
	}
	// THE <= BESIDE THE OR keeps the index's range seek, which a bare OR
	// of the two terms would give up (planner.go).
	position, id, ok := cutLogCursor(cursor)
	return []any{position, position, id}, ok
}

// keeps reports whether row passes every filter of q the walk's index does
// not seek, repoID the repository q names.
func (w changeWalk) keeps(row changeRow, q ReadQuery, repoID string) bool {
	return (!w.repo || row.repoID == repoID) && (!w.state || row.state == q.State) &&
		(!w.owner || row.owner == q.Owner) && (!w.work || row.work.Ref() == q.Work) &&
		(!w.live || row.state.Live()) && (!w.window || row.moved >= w.since)
}

// read is one page of the walk's rows, at most limit after cursor: every row
// the statements answered, kept or not, with its conversation's word on it
// ([withConversations]), and the next page's cursor, "" at the walk's end.
func (w changeWalk) read(ctx context.Context, tx *sql.Tx, cursor string, limit int) ([]changeRow, string,
	error) {

	switch w.index {
	case changesMovedKey:
		return windowWalk(ctx, tx, w.since, cursor, limit)
	case changesLiveMovedKey, changesRepoLiveMovedKey:
		return w.liveMovedWalk(ctx, tx, cursor, limit)
	}
	rows, err := w.readIndex(ctx, tx, cursor, limit)
	if err != nil {
		return nil, "", err
	}
	if err := withConversations(ctx, tx, rows); err != nil {
		return nil, "", err
	}
	if len(rows) == limit {
		return rows, w.cursor(rows[len(rows)-1]), nil
	}
	return rows, "", nil
}

// readIndex is [changeWalk.read] over the walk's one index, each statement
// passed as its constant so the read gate traces it to its registered plan
// (readindex_test.go).
func (w changeWalk) readIndex(ctx context.Context, tx *sql.Tx, cursor string, limit int) ([]changeRow, error) {
	switch w.index {
	case reviewerAsksKey:
		return w.readByID(ctx, tx, changesAskedSQL, changesAskedAfterSQL, cursor, limit,
			fmt.Sprintf("asking %v", w.keys[0]))
	case waitsKey:
		return w.readByID(ctx, tx, changesWaitingSQL, changesWaitingAfterSQL, cursor, limit, "waiting on a person")
	case readyKey:
		return w.readByID(ctx, tx, changesReadySQL, changesReadyAfterSQL, cursor, limit, "ready to merge")
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
	return nil, fmt.Errorf("gitlet: no listing of changes walks %s", w.index)
}

// readBy is [changeWalk.read] by first, a first page's statement binding the
// keys and the limit, or after, binding also the cursor's values
// ([changeWalk.resume]).
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
		return nil, fmt.Errorf("gitlet: list changes by %s: %w", w.index, err)
	}
	defer func() { _ = rows.Close() }()
	var out []changeRow
	for rows.Next() {
		row, scanErr := scanChange(rows.Scan)
		if scanErr != nil {
			return nil, fmt.Errorf("gitlet: list changes by %s: %w", w.index, scanErr)
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

// summary is row as a read renders it, from repository l, moved by its
// conversation where the read set that on the row ([withConversations], or the
// detail's conversation row), else by its lifecycle alone.
func (l listedRepo) summary(row changeRow) ChangeSummary {
	return ChangeSummary{ChangeRow: row.wire(), Repo: l.ref.Name, Ref: ChangeRef(l.ref.Name, row.number),
		Access: l.access.String(), OpenFindings: row.openFindings,
		UpdatedAt: store.DecodeTime(max(row.moved, row.updatedAt))}
}

// readableIn is the repository name names in tx, refused [ErrRepoNotFound]
// where nothing holds the name, or by [JudgeRead] for the asker or its ceiling.
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

// resolvingIn is the repository q names as a read of its changes or checks
// serves it ([readableIn]), or, where q resolves it ([ReadQuery.Resolve]), its
// whole row at checkpoint cp ([namedIn]), judged and answered as out.Resolved:
// one more statement, so a call's first read is its resolution and pays the
// code log one barrier, not a separate `repo` read's second.
func resolvingIn(ctx context.Context, tx *sql.Tx, q ReadQuery, chart Roster, cp statelog.Position,
	out *ReadAnswer) (listedRepo, error) {

	if !q.Resolve {
		return readableIn(ctx, tx, q.Repo, q.Asker, q.CeilingSeat, chart)
	}
	v, found, err := namedIn(ctx, tx, q.Repo, cp)
	if err != nil {
		return listedRepo{}, err
	}
	if !found {
		return listedRepo{}, fmt.Errorf("%w: %s", ErrRepoNotFound, q.Repo)
	}
	g, err := JudgeRead(q.Asker, q.CeilingSeat, v.ref, chart)
	if err != nil {
		return listedRepo{}, err
	}
	resolved := v.ref
	out.Resolved = &resolved
	return listedRepo{ref: v.ref, access: g.Access}, nil
}

// changes serves one page of a listing of changes (the file's doc): what waits
// on the asker a page of one of its two halves ([Reader.changesWaitingOnMe]),
// and the repository a listing resolves answered beside it at cp ([resolvingIn]).
func (r *Reader) changes(ctx context.Context, tx *sql.Tx, q ReadQuery, chart Roster, cp statelog.Position,
	out *ReadAnswer) error {

	q = q.readying()
	out.NarrowedTo = q.Readiness
	if q.textLeads() {
		return r.changesByWords(ctx, tx, q, chart, cp, out)
	}
	switch q.Needs {
	case NeedMe:
		return r.changesWaitingOnMe(ctx, tx, q, chart, cp, out)
	case NeedFix:
		q = q.fixing()
	}
	return r.listChanges(ctx, tx, q, chart, cp, out, "")
}

// listChanges is [Reader.changes] over one walk. Where asked names a reviewer,
// a walk of changes awaiting a fix leaves out those whose ask of asked stands:
// the first half of what waits on asked lists them.
func (r *Reader) listChanges(ctx context.Context, tx *sql.Tx, q ReadQuery, chart Roster, cp statelog.Position,
	out *ReadAnswer, asked string) error {

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
	var since int64
	if q.Window != "" {
		since = store.EncodeTime(r.now().Add(-q.Window.Duration()))
	}
	walk := walkOf(q, repoID, since)
	walk.scan = r.spokenScanned
	limit := min(limitOr(q.Limit, defaultChangeLimit), r.pageRows)
	scanned, next, err := walk.read(ctx, tx, q.Cursor, limit)
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
	var kept []changeRow
	for _, row := range scanned {
		if _, listed := repos[row.repoID]; listed && walk.keeps(row, q, repoID) && (!walk.fix || fixing[row.id]) {
			kept = append(kept, row)
		}
	}
	// A listing narrowed by words that another walk leads keeps the rows holding them.
	if q.Text != "" {
		if kept, err = holdingWords(ctx, tx, kept, textindex.Terms(q.Text)); err != nil {
			return err
		}
		out.ChangesText = &ChangesText{}
	}
	// Readiness is judged before the page is cut: waiting on a person and the
	// bucket are filters of what is judged.
	readiness, err := readinessIn(ctx, tx, kept, repos, chart, q.Asker, r.limits().MergeByFloor)
	if err != nil {
		return err
	}
	var listed []changeRow
	for _, row := range kept {
		judged := readiness[row.id]
		if (!walk.person || (judged != nil && judged.WaitsPerson())) && q.Readiness.holds(judged) {
			listed = append(listed, row)
		}
	}
	// The cards ([ReadQuery.Cards]) are read for the listed rows, and weigh in the cut.
	var cards map[string]*ChangeCard
	if q.Cards {
		if cards, err = r.changeCardsIn(ctx, tx, listed); err != nil {
			return err
		}
	}
	pg := page{maxRows: limit, maxB: r.pageBytes}
	for _, row := range listed {
		s := repos[row.repoID].summary(row)
		s.Readiness, s.Card = readiness[row.id], cards[row.id]
		out.Changes = append(out.Changes, s)
		if pg.add(s.encodedBytes()) {
			out.Next = walk.cursor(row)
			return nil
		}
	}
	out.Next = next
	return nil
}

// readinessIn is the readiness of each LIVE change of rows, by id: its
// target's merge gate judged ([JudgeGate]) in tx against chart, every row's
// repository in repos, the merge authority judged for asker under floor
// (code.merge_by_floor on the serving node's epoch). A change no longer live
// merges nothing, so has none.
func readinessIn(ctx context.Context, tx *sql.Tx, rows []changeRow, repos map[string]listedRepo,
	chart Roster, asker Principal, floor MergeBy) (map[string]*Readiness, error) {

	subjects := make([]readinessSubject, 0, len(rows))
	for _, row := range rows {
		if row.state.Live() {
			subjects = append(subjects, readinessSubject{repo: repos[row.repoID].ref, row: row})
		}
	}
	judged, err := gateFactsIn(ctx, tx, subjects)
	if err != nil {
		return nil, err
	}
	out := make(map[string]*Readiness, len(subjects))
	for i, s := range subjects {
		f := judged[i]
		f.Roster, f.Floor, f.Merger = chart, floor, asker
		gate, err := f.Gate()
		if err != nil {
			return nil, err
		}
		readiness := JudgeGate(f, gate)
		out[s.row.id] = &readiness
	}
	return out, nil
}

// listedReposIn is [listedReposAmong] over the repositories a page of changes
// names.
func listedReposIn(ctx context.Context, tx *sql.Tx, scanned []changeRow, asker Principal, ceilingSeat string,
	chart Roster) (map[string]listedRepo, error) {

	ids := make([]string, 0, len(scanned))
	for _, row := range scanned {
		ids = append(ids, row.repoID)
	}
	return listedReposAmong(ctx, tx, ids, asker, ceilingSeat, chart, "a page of changes")
}

// listedReposAmong is every repository of ids (in any order, repeats allowed)
// whose rows a listing across repositories serves, by id: in use (importing,
// active or archived) and readable by both asker and the seat it reads for
// ([JudgeRead]), read in ONE statement and judged once a page. what names the
// page in an error.
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
		return nil, fmt.Errorf("gitlet: read the repositories %s names: %w", what, err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var ref RepoRef
		var status, owners, visibility string
		if scanErr := rows.Scan(&ref.ID, &ref.Name, &status, &owners, &ref.Owner.Ref, &visibility); scanErr != nil {
			return nil, fmt.Errorf("gitlet: read the repositories %s names: %w", what, scanErr)
		}
		ref.Status, ref.Owner.Kind, ref.Visibility = RepoStatus(status), OwnerKind(owners), Visibility(visibility)
		if !ref.Status.live() {
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

// change serves one change by its number (the file's doc): a page of its
// revisions, its detail beside the first; its detail alone
// ([ReadQuery.DetailOnly]); its row alone in place of the detail
// ([ReadQuery.Brief]); or a page of the conversation part the read names
// ([ReadQuery.Part]) — every page judged against the grant from the
// repository's row ([resolvingIn]), at checkpoint cp.
func (r *Reader) change(ctx context.Context, tx *sql.Tx, q ReadQuery, chart Roster, cp statelog.Position,
	out *ReadAnswer) error {
	repo, err := resolvingIn(ctx, tx, q, chart, cp, out)
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
	if q.FindingPaths {
		return r.changeFindingPaths(ctx, tx, q, ChangeRef(repo.ref.Name, row.number), row.id, out)
	}
	if q.Viewed {
		return r.changeViewed(ctx, tx, q, repo.ref.ID, ChangeRef(repo.ref.Name, row.number), row, out)
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
	case PartStanding:
		return r.changeStanding(ctx, tx, q, ChangeRef(repo.ref.Name, row.number), row.id, out)
	case PartMoves:
		return r.changeMoves(ctx, tx, q, ChangeRef(repo.ref.Name, row.number), row.id, out)
	}
	limit := min(limitOr(q.Limit, defaultRevisionLimit), r.pageRows)
	var rows *sql.Rows
	switch {
	case q.Brief:
		// IN BRIEF: the row read above and nothing else — no body, authors,
		// conversation, mergeability or readiness.
		brief := repo.summary(row)
		out.ChangeBrief = &brief
		if q.DetailOnly {
			// No revision either: the read is the one seek of
			// code_changes_number_idx above.
			return nil
		}
		rows, err = tx.QueryContext(ctx, revisionsSQL, row.id, limit)
	case q.Cursor == "":
		body, bodyErr := bodyOf(ctx, tx, row.id)
		if bodyErr != nil {
			return bodyErr
		}
		authors, authorsErr := authorsIn(ctx, tx, row.id)
		if authorsErr != nil {
			return authorsErr
		}
		conversation, spoken, conversationErr := conversationRowIn(ctx, tx, row.id)
		if conversationErr != nil {
			return conversationErr
		}
		merge, mergeErr := mergeabilityRowIn(ctx, tx, row.id)
		if mergeErr != nil {
			return mergeErr
		}
		participants, participantsErr := participantsIn(ctx, tx, row.id)
		if participantsErr != nil {
			return participantsErr
		}
		tip, _, tipErr := liveRefIn(ctx, tx, row.repoID, row.target)
		if tipErr != nil {
			return fmt.Errorf("gitlet: read %s's target %s: %w", ChangeRef(repo.ref.Name, row.number), row.target,
				tipErr)
		}
		// From the conversation row just read: the detail is a change's most
		// re-read question, so it pays no second statement to [withConversations].
		row.moved = max(row.updatedAt, spoken)
		if conversation != nil {
			row.openFindings = conversation.OpenFindings
		}
		readiness, readinessErr := readinessIn(ctx, tx, []changeRow{row}, map[string]listedRepo{row.repoID: repo},
			chart, q.Asker, r.limits().MergeByFloor)
		if readinessErr != nil {
			return readinessErr
		}
		out.Change = &ChangeDetail{ChangeSummary: repo.summary(row), Body: body, Authors: authors,
			Conversation: conversation, Mergeability: merge, TargetTip: tip.OID, Participants: participants}
		out.Change.Readiness = readiness[row.id]
		if q.DetailOnly {
			// No revision is read, encoded or shipped ([ReadQuery.DetailOnly]).
			return nil
		}
		rows, err = tx.QueryContext(ctx, revisionsSQL, row.id, limit)
	default:
		n, parseErr := strconv.ParseInt(q.Cursor, 10, 64)
		if parseErr != nil || n < 1 {
			return fmt.Errorf("%w: %q is not a revision's number", ErrBadCursor, q.Cursor)
		}
		rows, err = tx.QueryContext(ctx, revisionsAfterSQL, row.id, n, limit)
	}
	if err != nil {
		return fmt.Errorf("gitlet: read the revisions of %s: %w", ChangeRef(repo.ref.Name, row.number), err)
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
			return fmt.Errorf("gitlet: read the revisions of %s: %w", ChangeRef(repo.ref.Name, row.number), scanErr)
		}
		rev.PusherKind, rev.Position, rev.At = PartyKind(kind), unpacked(position), store.DecodeTime(at)
		if rev.Facts, err = facts.row(); err != nil {
			return fmt.Errorf("gitlet: read revision %d of %s's facts: %w", rev.N, ChangeRef(repo.ref.Name,
				row.number), err)
		}
		out.Revisions = append(out.Revisions, rev)
		if pg.add(rev.encodedBytes()) {
			out.Next = rev.Cursor()
			break
		}
	}
	return rows.Err()
}

// Cursor is the cursor that resumes a page of a change's revisions after r:
// its number, below which the next page reads.
func (r RevisionRow) Cursor() string { return strconv.FormatInt(r.N, 10) }

// changeByNumberIn is the change number names on repository repoID in tx, and
// false where it holds none — one seek of code_changes_number_idx.
func changeByNumberIn(ctx context.Context, tx *sql.Tx, repoID string, number int64) (changeRow, bool, error) {
	row, err := scanChange(tx.QueryRowContext(ctx, changeByNumberSQL, repoID, number).Scan)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return changeRow{}, false, nil
	case err != nil:
		return changeRow{}, false, fmt.Errorf("gitlet: read change %d of %s: %w", number, repoID, err)
	}
	return row, true, nil
}

// authorsIn is every author of change id in tx, by handle — one range of the
// authors' key.
func authorsIn(ctx context.Context, tx *sql.Tx, id string) ([]ChangeAuthor, error) {
	rows, err := tx.QueryContext(ctx, changeAuthorsSQL, id)
	if err != nil {
		return nil, fmt.Errorf("gitlet: read the authors of change %s: %w", id, err)
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
			return nil, fmt.Errorf("gitlet: read the authors of change %s: %w", id, scanErr)
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
		ClosedBy: r.closedBy, ClosedByKind: r.closedByKind, MergedVia: r.mergedVia, MergeOID: r.mergeOID, MergedBy: r.mergedBy, MergedByKind: r.mergedByKind,
		MergedOperatorID: r.mergedOperator, OverrideReason: r.overrideReason, BodyBytes: r.bodyBytes,
		Version: r.version, Position: unpacked(r.position)}
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

// factsScan is one revision's facts ([revisionFactsColumns]) as a join scans
// them: every column nullable, since a revision with no facts recorded joins none.
type factsScan struct {
	n                                               sql.NullInt64
	targetRef, targetOID, base, patch, node, digest sql.NullString
	baseTree, headTree                              sql.NullString
	owners, skipped, classes, touched               sql.NullString
	targetPosition, position, measuredAt            sql.NullInt64
	omitted, touchesAll                             sql.NullBool
}

// targets is where a scan of [revisionFactsColumns] writes, in their order.
func (s *factsScan) targets() []any {
	return []any{&s.n, &s.targetRef, &s.targetOID, &s.targetPosition, &s.base, &s.baseTree, &s.headTree, &s.patch,
		&s.omitted, &s.touchesAll, &s.node, &s.digest, &s.position, &s.measuredAt, &s.owners, &s.skipped, &s.classes,
		&s.touched}
}

// row is the facts as a read serves them, and nil for a revision of none.
func (s *factsScan) row() (*RevisionFactsRow, error) {
	if !s.n.Valid {
		return nil, nil
	}
	f := &RevisionFactsRow{TargetRef: s.targetRef.String, TargetOID: s.targetOID.String,
		TargetPosition: unpacked(s.targetPosition.Int64), BaseOID: s.base.String, BaseTree: s.baseTree.String,
		HeadTree: s.headTree.String, PatchID: s.patch.String, PatchIDOmitted: s.omitted.Bool, Touched: []string{},
		TouchesAll: s.touchesAll.Bool, Node: s.node.String, OwnersDigest: s.digest.String,
		Position: unpacked(s.position.Int64)}
	if s.touched.String != "" {
		f.Touched = strings.Split(s.touched.String, "\x00")
	}
	if s.classes.String != "" {
		if err := json.Unmarshal([]byte(s.classes.String), &f.Classes); err != nil {
			return nil, fmt.Errorf("its classes: %w", err)
		}
	}
	if v := s.measuredAt.Int64; v > 0 {
		f.MeasuredAt = store.DecodeTime(v)
	}
	if s.owners.String != "" {
		var touched []TouchedOwner
		if err := json.Unmarshal([]byte(s.owners.String), &touched); err != nil {
			return nil, fmt.Errorf("its matched owners: %w", err)
		}
		// No answer names an owner's globs: the facts keep its first for the merge gate's sentence.
		f.RequestedOwners = make([]Owner, len(touched))
		for i, o := range touched {
			f.RequestedOwners[i] = o.Owner
		}
	}
	if s.skipped.String != "" {
		if err := json.Unmarshal([]byte(s.skipped.String), &f.SkippedOwners); err != nil {
			return nil, fmt.Errorf("its skipped owners: %w", err)
		}
	}
	return f, nil
}

// validateChanges is [ReadQuery.validate] for a `changes` listing: it names
// something an index it walks is led by ([walkOf]), each a valid value; a need
// is in a live state and names whom it waits on or is the asker seat's own; a
// bucket ([ReadQuery.Readiness]) is of the open state, empty read as open.
func (q ReadQuery) validateChanges() error {
	refused := func(field, why string) error { return fmt.Errorf("%w: %s: %s", ErrInvalid, field, why) }
	switch {
	case q.Repo == "" && q.Work == "" && q.State == "" && q.Reviewer == "" && q.Needs == "" && q.Window == "" &&
		q.Readiness == "" && q.Text == "":
		return refused("state", "a listing of changes names a repository, a work item, a state, a reviewer, "+
			"what its changes wait on, a window they moved in or the words they hold, which the indexes it walks are each led by — "+
			"no index holds every change the company has, and an owner's changes are kept by state")
	case q.Window != "" && !q.Window.Valid():
		return refused("window", fmt.Sprintf("%q is not one of %s", q.Window, words.Or(ChangeWindows())))
	case q.ChangeSort != "" && !q.ChangeSort.Valid():
		return refused("change_sort", fmt.Sprintf("%q is not one of %s", q.ChangeSort, words.Or(ChangeSorts())))
	case q.State != "" && !q.State.Valid():
		return refused("state", fmt.Sprintf("%q is not one of %s", q.State, words.Or(ChangeStates())))
	case q.Owner != "" && !org.ValidHandle(q.Owner):
		return refused("owner", fmt.Sprintf("%q is not a seat's handle: a change's owner is a seat, or the "+
			"human seat a person holds", q.Owner))
	case q.Reviewer != "" && !org.ValidHandle(q.Reviewer):
		return refused("reviewer", fmt.Sprintf("%q is not a seat's handle: a reviewer is a seat, or the "+
			"human seat a person holds", q.Reviewer))
	case q.Needs != "" && !q.Needs.Valid():
		return refused("needs", fmt.Sprintf("%q is not one of %s", q.Needs, words.Or(Needs())))
	case q.Needs != "" && q.State != "" && !q.State.Live():
		return refused("state", fmt.Sprintf("needs=%s lists live changes, and a %s change waits on nobody",
			q.Needs, q.State))
	case q.Readiness != "" && !q.Readiness.Valid():
		return refused("readiness", fmt.Sprintf("%q is not one of %s", q.Readiness, words.Or(ReadinessBuckets())))
	case q.Readiness != "" && q.State != "" && q.State != ChangeStateOpen:
		return refused("state", fmt.Sprintf("readiness=%s lists open changes by where their merge stands, "+
			"and a %s change stands in no bucket", q.Readiness, q.State))
	case q.Needs == NeedFix && q.Reviewer != "":
		return refused("reviewer", "needs=fix lists an owner's changes awaiting their fix; the changes awaiting "+
			"a reviewer's review are needs=review")
	case q.Needs == NeedMe && (q.Reviewer != "" || q.Owner != ""):
		return refused("needs", "needs=me is what waits on the asker, and names no reviewer and no owner")
	case q.Needs == NeedPerson && q.Reviewer != "":
		return refused("reviewer", "needs=person lists the changes waiting on any person; the changes asking "+
			"one reviewer's review are reviewer=")
	case q.Needs != "" && q.Needs != NeedPerson && q.waitedOn() == "":
		whose := map[Need]string{NeedReview: "unless it names whose — a reviewer — ",
			NeedFix: "unless it names whose — an owner — "}[q.Needs]
		return refused("needs", fmt.Sprintf("needs=%s is the asker's own %sand this credential is bound to no "+
			"seat", q.Needs, whose))
	case q.ChangeSort == ChangeSortUpdated && (q.Reviewer != "" || q.Needs != ""):
		return refused("change_sort", "updated lists a live state's changes by when they last moved; a listing "+
			"of a reviewer's asks or of what its changes wait on is read in the order of those asks")
	case q.ChangeSort == ChangeSortUpdated && walkLedBy(q.readying(), q.Repo).order != byMove:
		return refused("change_sort", "updated lists the changes of a live state, state=draft or state=open, "+
			"by when they last moved, or a window nothing else leads, which is listed so already")
	case q.ChangeSort == ChangeSortOpened && walkLedBy(q.readying(), q.Repo).order == byMove:
		return refused("change_sort", "a window that nothing else leads lists its changes by when they last "+
			"moved; name a live state, a repository, a work item or a reviewer to list them by when they were "+
			"opened")
	}
	if q.Work != "" {
		if _, _, why := cutWork(q.Work); why != "" {
			return refused("work", why)
		}
	}
	if q.Text != "" {
		if why := wordsProblem(q.Text); why != "" {
			return refused("text", why)
		}
	}
	return nil
}

// cutWork reads a work item's identity across trackers, `<backend>:<id>`
// ([types.WorkItem.Ref]), as [workItemProblem] admits it, or says why it is none.
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

// The keys of the change listings' indexes, named by their statements as a
// GUARD (planner.go) and held by plan_test.go to the seek each is written for.
const (
	// changesRepoStateKey is code_changes_repo_state_idx, (repo_id, state,
	// number) (replicated 0097).
	changesRepoStateKey = "code_changes_repo_state_idx"
	// changesStateKey is code_changes_state_idx, (state, opened_position,
	// id) (replicated 0086 and 0094).
	changesStateKey = "code_changes_state_idx"
	// changesOwnerKey is code_changes_owner_idx, (owner, state,
	// opened_position, id) (replicated 0086 and 0094).
	changesOwnerKey = "code_changes_owner_idx"
	// changesWorkKey is code_changes_work_idx, (work_backend, work_id,
	// opened_position, id) WHERE work_id != '' (replicated 0097).
	changesWorkKey = "code_changes_work_idx"
)

// The reads of changes, each registered in plan_test.go so the pinned planner
// is held to its index and its order. A page after a cursor by opening
// position puts <= beside the OR to keep the range seek (planner.go).
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
	// A work item's repeats its index's partial predicate verbatim: the pinned
	// planner uses a partial index only for a statement implying it in its own words.
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
	// Revisions with their facts, joined by key (a revision of none reads
	// NULLs), newest first in the revisions' key order.
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
	revisionFactsColumns = `f.n, f.target_ref, f.target_oid, f.target_position, f.base_oid, f.base_tree,
		f.head_tree, f.patch_id, f.patch_id_omitted, f.touches_all, f.node, f.owners_digest, f.position,
		f.measured_at, f.owners, f.skipped, f.classes, f.touched`
	listedReposSQL = `SELECT id, name, status, owner_kind, owner, visibility FROM code_repos
		WHERE id IN (SELECT CAST(value AS TEXT) FROM json_each(?))`
)

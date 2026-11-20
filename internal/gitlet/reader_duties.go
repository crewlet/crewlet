package gitlet

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/store"
)

// compactionDue serves one page of a `compaction_due` read: the active and
// archived repositories holding at least q.MinPacks live packs, with their
// row's version, one ranged walk of code_repos_packs_idx (replicated 0088,
// its predicate repeated verbatim), keyset on (live packs, id).
//
// MOST LIVE PACKS FIRST, so one near MaxPacksPerRepo (about to refuse pushes
// compaction_behind) never waits. ARCHIVED too: it is still read, each cold
// sync a GET a pack. THE VERSION keys the compactor's memory of a plan found
// not due. A FULL LISTING, never a delta, which would need an index on
// code_repos.version, rewritten by every record.
func (r *Reader) compactionDue(ctx context.Context, tx *sql.Tx, q ReadQuery, out *ReadAnswer) error {
	var (
		rows *sql.Rows
		err  error
	)
	switch packs, id, keyed := cutDueCursor(q.Cursor); {
	case q.Cursor != "" && !keyed:
		return fmt.Errorf("%w: %q", ErrBadCursor, q.Cursor)
	case q.Cursor == "":
		rows, err = tx.QueryContext(ctx, compactionDueFirstSQL, q.MinPacks, r.pageRows)
	default:
		rows, err = tx.QueryContext(ctx, compactionDueAfterSQL, q.MinPacks, packs, packs, id, r.pageRows)
	}
	if err != nil {
		return fmt.Errorf("gitlet: list the repositories a compaction may be due in: %w", err)
	}
	defer func() { _ = rows.Close() }()
	pg := page{maxRows: r.pageRows, maxB: r.pageBytes}
	for rows.Next() {
		var (
			c       CompactionCandidate
			version int64
		)
		if err := rows.Scan(&c.ID, &c.Name, &c.LivePacks, &c.LiveObjects, &version); err != nil {
			return fmt.Errorf("gitlet: list the repositories a compaction may be due in: %w", err)
		}
		c.Version = unpacked(version)
		out.Due = append(out.Due, c)
		if pg.add(c.encodedBytes()) {
			out.Next = dueCursor(c)
			break
		}
	}
	return rows.Err()
}

// dueCursor is "<live packs>/<id>": a repository's place in a `compaction_due`
// listing's order.
func dueCursor(c CompactionCandidate) string {
	return strconv.FormatInt(c.LivePacks, 10) + "/" + c.ID
}

// cutDueCursor reads a [dueCursor].
func cutDueCursor(s string) (int64, string, bool) {
	at, id, ok := strings.Cut(s, "/")
	if !ok || id == "" {
		return 0, "", false
	}
	packs, err := strconv.ParseInt(at, 10, 64)
	if err != nil || packs < 0 {
		return 0, "", false
	}
	return packs, id, true
}

// publicationDue serves one page of a `publication_due` read: the active and
// archived repositories holding a live pack no published layer covers, LEAST
// RECENTLY PUBLISHED FIRST (never published first), then by id, with what the
// due rule reads and the row's version: ONE WALK of code_repos_unpublished_idx
// (replicated 0090, its predicate repeated verbatim), a full listing. The
// published counts are the last publication's recount, so a listed repository
// may have nothing to publish, which only a plan can tell
// ([PublicationCandidate]).
//
// NONE WITH AN EXPUNGE OPEN: rule 6g refuses those publications
// (publication.go), and listed, its publisher would build a layer (perhaps a
// whole base) for the committer to refuse at every poll.
func (r *Reader) publicationDue(ctx context.Context, tx *sql.Tx, q ReadQuery, out *ReadAnswer) error {
	var (
		rows *sql.Rows
		err  error
	)
	switch at, id, keyed := cutPublishCursor(q.Cursor); {
	case q.Cursor != "" && !keyed:
		return fmt.Errorf("%w: %q", ErrBadCursor, q.Cursor)
	case q.Cursor == "":
		rows, err = tx.QueryContext(ctx, publicationDueFirstSQL, r.pageRows)
	default:
		rows, err = tx.QueryContext(ctx, publicationDueAfterSQL, at, at, id, r.pageRows)
	}
	if err != nil {
		return fmt.Errorf("gitlet: list the repositories a publication may be due in: %w", err)
	}
	defer func() { _ = rows.Close() }()
	pg := page{maxRows: r.pageRows, maxB: r.pageBytes}
	for rows.Next() {
		var (
			c                    PublicationCandidate
			lastPush             sql.NullInt64
			published, tookMs, v int64
		)
		if err := rows.Scan(&c.ID, &c.Name, &c.LivePacks, &c.PublishedPacks, &c.LiveObjects,
			&c.PublishedObjects, &lastPush, &published, &tookMs, &v); err != nil {
			return fmt.Errorf("gitlet: list the repositories a publication may be due in: %w", err)
		}
		if lastPush.Valid {
			c.LastPushAt = store.DecodeTime(lastPush.Int64)
		}
		// ZERO IS NEVER: the column's default, and no publication's broker
		// instant.
		if published != 0 {
			c.PublishedAt = store.DecodeTime(published)
		}
		c.PublishTook, c.Version = time.Duration(tookMs)*time.Millisecond, unpacked(v)
		out.Publish = append(out.Publish, c)
		if pg.add(c.encodedBytes()) {
			out.Next = publishCursor(published, c.ID)
			break
		}
	}
	return rows.Err()
}

// expungeDue serves one page of an `expunge_due` read in two ARMS, the first
// whole before the second, each a full listing with its partial predicate
// repeated verbatim:
//
//  1. OPEN: active and archived repositories with an expunge open or a purge
//     requested, BY ID, one walk of code_repos_expunging_idx (replicated 0092);
//  2. WEEKLY ([ReadQuery.ScannedBefore]): every OTHER active or archived
//     repository of any kind (repo_size counts every live byte) last JUDGED
//     ([ExpungeScan]) at or before the instant or never, LEAST RECENTLY FIRST,
//     one seek of code_repos_expunge_due_idx (replicated 0139).
//
// Deleted or purging: in neither (its expunge awaits a restore, or the purge).
func (r *Reader) expungeDue(ctx context.Context, tx *sql.Tx, q ReadQuery, out *ReadAnswer) error {
	weekly, after, ok := cutExpungeCursor(q.Cursor)
	if !ok || (weekly && q.ScannedBefore.IsZero()) {
		return fmt.Errorf("%w: %q", ErrBadCursor, q.Cursor)
	}
	pg := page{maxRows: r.pageRows, maxB: r.pageBytes}
	if !weekly {
		rows, err := tx.QueryContext(ctx, expungeDueSQL, q.Cursor, r.pageRows)
		if err != nil {
			return fmt.Errorf("gitlet: list the repositories an expunge is owed in: %w", err)
		}
		full, err := scanExpungeDue(rows, &pg, out, func(c ExpungeCandidate, _ int64) string { return c.ID })
		if err != nil || full || q.ScannedBefore.IsZero() {
			return err
		}
	}
	before := store.EncodeTime(q.ScannedBefore)
	var (
		rows *sql.Rows
		err  error
	)
	if after == nil {
		rows, err = tx.QueryContext(ctx, expungeWeeklySQL, before, r.pageRows-pg.rows)
	} else {
		rows, err = tx.QueryContext(ctx, expungeWeeklyAfterSQL, before, after.at, after.at, after.id,
			r.pageRows-pg.rows)
	}
	if err != nil {
		return fmt.Errorf("gitlet: list the repositories an expunge may be due in: %w", err)
	}
	_, err = scanExpungeDue(rows, &pg, out, func(c ExpungeCandidate, scanned int64) string {
		return weeklyCursor(scanned, c.ID)
	})
	return err
}

// scanExpungeDue reads one arm's rows of an `expunge_due` page into out,
// counting each against pg; full reports a page that took its last row, its
// cursor named by next from the row and its expunge_scanned_at.
func scanExpungeDue(rows *sql.Rows, pg *page, out *ReadAnswer,
	next func(c ExpungeCandidate, scanned int64) string) (full bool, err error) {

	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var (
			c                                     ExpungeCandidate
			requested, expunged, scanned, version int64
		)
		if err := rows.Scan(&c.ID, &c.Name, &c.Open, &c.Request, &requested, &expunged, &scanned, &c.LivePacks,
			&c.LiveBytes, &c.LiveObjects, &version); err != nil {
			return false, fmt.Errorf("gitlet: list the repositories an expunge is owed in: %w", err)
		}
		if c.Request != "" {
			c.Requested = unpacked(requested)
		}
		// ZERO IS NEVER: the column's default, and no commit's broker
		// instant.
		if expunged != 0 {
			c.ExpungedAt = store.DecodeTime(expunged)
		}
		c.Version = unpacked(version)
		out.Expunge = append(out.Expunge, c)
		if pg.add(c.encodedBytes()) {
			out.Next = next(c, scanned)
			return true, rows.Err()
		}
	}
	return false, rows.Err()
}

// weeklyPrefix opens an `expunge_due` cursor in its WEEKLY arm: the last
// row's expunge_scanned_at, encoded, and its id — "weekly:<at>:<id>". A cursor in
// the open arm is the last id served, a UUID, which never opens so.
const weeklyPrefix = "weekly:"

// weeklyCursor is the cursor after a weekly-arm row last judged at scanned
// (encoded) of id.
func weeklyCursor(scanned int64, id string) string {
	return weeklyPrefix + strconv.FormatInt(scanned, 10) + ":" + id
}

// expungeAfter is a weekly-arm cursor read back: the row it names.
type expungeAfter struct {
	at int64
	id string
}

// cutExpungeCursor reads an `expunge_due` cursor: "" or a repository id in the
// open arm (weekly false), or a weekly-arm cursor and the row it names; ok is
// false for a cursor no page answered.
func cutExpungeCursor(cursor string) (weekly bool, after *expungeAfter, ok bool) {
	rest, isWeekly := strings.CutPrefix(cursor, weeklyPrefix)
	if !isWeekly {
		return false, nil, cursor == "" || validRepoID("cursor", cursor) == nil
	}
	at, id, found := strings.Cut(rest, ":")
	n, err := strconv.ParseInt(at, 10, 64)
	if !found || err != nil || n < 0 || validRepoID("cursor", id) != nil {
		return true, nil, false
	}
	return true, &expungeAfter{at: n, id: id}, true
}

// indexed serves one page of an `indexed` read ([ReadIndexed]): the active
// and archived repositories searched by index, by name after the cursor, one
// seek of code_repos_search_idx (replicated 0107, its predicate repeated
// verbatim). A FULL LISTING, as [Reader.compactionDue]'s.
func (r *Reader) indexed(ctx context.Context, tx *sql.Tx, q ReadQuery, out *ReadAnswer) error {
	// THE CURSOR IS THE LAST NAME SERVED, and "" before the first: a name
	// is never empty, so one statement serves every page.
	if q.Cursor != "" && !ValidName(q.Cursor) {
		return fmt.Errorf("%w: %q", ErrBadCursor, q.Cursor)
	}
	rows, err := tx.QueryContext(ctx, indexedSQL, string(SearchIndexed), q.Cursor, r.pageRows)
	if err != nil {
		return fmt.Errorf("gitlet: list the repositories searched by index: %w", err)
	}
	defer func() { _ = rows.Close() }()
	pg := page{maxRows: r.pageRows, maxB: r.pageBytes}
	for rows.Next() {
		var c IndexedRepo
		if err := rows.Scan(&c.ID, &c.Name); err != nil {
			return fmt.Errorf("gitlet: list the repositories searched by index: %w", err)
		}
		out.Indexed = append(out.Indexed, c)
		if pg.add(2 + field("id", stringBytes(c.ID), true) + field("name", stringBytes(c.Name), false)) {
			out.Next = c.Name
			break
		}
	}
	return rows.Err()
}

// defaultTips serves one page of a `default_tips` read ([ReadDefaultTips]):
// the active project repositories by name after the cursor, each with its
// default branch's tip, one walk of code_repos_name_idx; a full listing.
func (r *Reader) defaultTips(ctx context.Context, tx *sql.Tx, q ReadQuery, out *ReadAnswer) error {
	if q.Cursor != "" && !ValidName(q.Cursor) {
		return fmt.Errorf("%w: %q", ErrBadCursor, q.Cursor)
	}
	query, args := defaultTipsSQL, []any{q.Cursor, string(RepoProject), r.pageRows}
	if q.DefaultTips != nil {
		query, args = defaultTipSQL, []any{q.DefaultTips.RepoID, string(RepoProject)}
	}
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return fmt.Errorf("gitlet: list the repositories' default tips: %w", err)
	}
	defer func() { _ = rows.Close() }()
	pg := page{maxRows: r.pageRows, maxB: r.pageBytes}
	for rows.Next() {
		var (
			c    DefaultTip
			head sql.NullString
		)
		if err := rows.Scan(&c.ID, &c.Name, &c.Visibility, &head); err != nil {
			return fmt.Errorf("gitlet: list the repositories' default tips: %w", err)
		}
		c.Head = head.String
		out.DefaultTips = append(out.DefaultTips, c)
		if pg.add(2 + field("id", stringBytes(c.ID), true) + field("name", stringBytes(c.Name), false) +
			field("visibility", stringBytes(string(c.Visibility)), false) +
			field("head", stringBytes(c.Head), false)) {
			out.Next = c.Name
			break
		}
	}
	return rows.Err()
}

// importsDue serves one page of an `imports_due` read: the repositories being
// imported, BY ID, with their source, chunks landed and row version, one walk
// of code_repos_importing_idx (replicated 0123, its predicate repeated
// verbatim); a full listing.
func (r *Reader) importsDue(ctx context.Context, tx *sql.Tx, q ReadQuery, out *ReadAnswer) error {
	if q.Cursor != "" && validRepoID("cursor", q.Cursor) != nil {
		return fmt.Errorf("%w: %q", ErrBadCursor, q.Cursor)
	}
	rows, err := tx.QueryContext(ctx, importsDueSQL, q.Cursor, r.pageRows)
	if err != nil {
		return fmt.Errorf("gitlet: list the repositories being imported: %w", err)
	}
	defer func() { _ = rows.Close() }()
	pg := page{maxRows: r.pageRows, maxB: r.pageBytes}
	for rows.Next() {
		var (
			c       ImportCandidate
			source  string
			version int64
		)
		if err := rows.Scan(&c.ID, &c.Name, &source, &c.Seq, &version); err != nil {
			return fmt.Errorf("gitlet: list the repositories being imported: %w", err)
		}
		// AN IMPORTING REPOSITORY NAMES ITS SOURCE, fixed at the create; one
		// naming none is listed with none, and its home ends it.
		if src, err := remoteOf[ImportSource](source); err != nil {
			return fmt.Errorf("gitlet: list the repositories being imported: %s: %w", c.ID, err)
		} else if src != nil {
			c.Source = *src
		}
		c.Version = unpacked(version)
		out.Imports = append(out.Imports, c)
		if pg.add(c.encodedBytes()) {
			out.Next = c.ID
			break
		}
	}
	return rows.Err()
}

// mirrorsDue serves one page of a `mirrors_due` read: every live push mirror
// BY ID (code_repos_mirror_to_idx), then every active pull mirror LEAST
// RECENTLY SYNCED FIRST (code_repos_mirrored_idx, replicated 0123), each
// predicate repeated verbatim; the cursor names its arm ([cutMirrorsCursor]),
// and a page ending the push arm goes on into the pull arm.
func (r *Reader) mirrorsDue(ctx context.Context, tx *sql.Tx, q ReadQuery, out *ReadAnswer) error {
	arm, after, at, ok := cutMirrorsCursor(q.Cursor)
	if !ok {
		return fmt.Errorf("%w: %q", ErrBadCursor, q.Cursor)
	}
	pg := page{maxRows: r.pageRows, maxB: r.pageBytes}
	if arm == "t" {
		full, err := r.pushMirrors(ctx, tx, after, &pg, out)
		if err != nil || full {
			return err
		}
		after, at = "", -1
	}
	rows, err := tx.QueryContext(ctx, pullMirrorsSQL, at, at, after, r.pageRows)
	if err != nil {
		return fmt.Errorf("gitlet: list the pull mirrors: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var (
			c       MirrorCandidate
			from    string
			synced  int64
			version int64
		)
		if err = rows.Scan(&c.ID, &c.Name, &from, &synced, &version); err != nil {
			return fmt.Errorf("gitlet: list the pull mirrors: %w", err)
		}
		if c.From, err = remoteOf[MirrorSource](from); err != nil {
			return fmt.Errorf("gitlet: list the pull mirrors: %s: %w", c.ID, err)
		}
		// ZERO IS NEVER: the column's default, and no record's instant.
		if synced != 0 {
			c.SyncedAt = store.DecodeTime(synced)
		}
		c.Version = unpacked(version)
		out.Mirrors = append(out.Mirrors, c)
		if pg.add(c.encodedBytes()) {
			out.Next = fmt.Sprintf("f:%d:%s", synced, c.ID)
			break
		}
	}
	return rows.Err()
}

// pushMirrors serves the push mirrors' arm of a `mirrors_due` page after the
// id after, and reports a page it filled — whose cursor it set.
func (r *Reader) pushMirrors(ctx context.Context, tx *sql.Tx, after string, pg *page, out *ReadAnswer) (
	bool, error) {

	rows, err := tx.QueryContext(ctx, pushMirrorsSQL, after, r.pageRows)
	if err != nil {
		return false, fmt.Errorf("gitlet: list the push mirrors: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var (
			c  MirrorCandidate
			to string
		)
		if err = rows.Scan(&c.ID, &c.Name, &to); err != nil {
			return false, fmt.Errorf("gitlet: list the push mirrors: %w", err)
		}
		if c.To, err = remoteOf[MirrorTarget](to); err != nil {
			return false, fmt.Errorf("gitlet: list the push mirrors: %s: %w", c.ID, err)
		}
		out.Mirrors = append(out.Mirrors, c)
		if pg.add(c.encodedBytes()) {
			out.Next = "t:" + c.ID
			return true, rows.Err()
		}
	}
	return false, rows.Err()
}

// cutMirrorsCursor reads a `mirrors_due` cursor: "" is the push arm's first
// row; `t:<id>` the push arm after id; `f:<instant>:<id>` the pull arm after
// the mirror synced at instant (store.EncodeTime, 0 for never) whose id is
// id. ok is false for anything else.
func cutMirrorsCursor(cursor string) (arm, after string, at int64, ok bool) {
	switch {
	case cursor == "":
		return "t", "", 0, true
	case strings.HasPrefix(cursor, "t:"):
		id := strings.TrimPrefix(cursor, "t:")
		return "t", id, 0, validRepoID("cursor", id) == nil
	case strings.HasPrefix(cursor, "f:"):
		instant, id, cut := strings.Cut(strings.TrimPrefix(cursor, "f:"), ":")
		n, err := strconv.ParseInt(instant, 10, 64)
		if !cut || err != nil || n < 0 || validRepoID("cursor", id) != nil {
			return "", "", 0, false
		}
		return "f", id, n, true
	}
	return "", "", 0, false
}

// expungeTips serves one page of an `expunge_tips` read ([ReadExpungeTips]):
// the objects an expunge keeps beside the refs (M6-D2), in the phases
// [tipsPhase] lists, each ONE SEEK of its index, keyset after the last row
// read. Drops are those at or after the query's instant (code_ref_drops' key,
// led by the instant, replicated 0089). The rest are IN FLIGHT and kept even
// by a purge: revision heads of draft or open changes (an interdiff reads
// force-pushed heads), live queue entries, candidates (rows and a pack, never
// a ref, M5-D12) and green rows; closed history keeps nothing. Each object is
// answered once a page. A purged repository is [ErrRepoNotFound]. NEVER HELD
// TO A GRANT: the objects go only to the maintainer.
func (r *Reader) expungeTips(ctx context.Context, tx *sql.Tx, q ReadQuery, out *ReadAnswer) error {
	in := q.ExpungeTips
	var name string
	switch err := tx.QueryRowContext(ctx, repoNameSQL, in.RepoID).Scan(&name); {
	case errors.Is(err, sql.ErrNoRows):
		return fmt.Errorf("%w: %s", ErrRepoNotFound, in.RepoID)
	case err != nil:
		return fmt.Errorf("gitlet: read repository %s: %w", in.RepoID, err)
	}
	since := store.EncodeTime(in.Since)
	from := tipsCursor{phase: tipsDrops}
	if q.Cursor != "" {
		c, ok := cutTipsCursor(q.Cursor)
		if !ok || (c.phase == tipsDrops && c.at < since) {
			return fmt.Errorf("%w: %q", ErrBadCursor, q.Cursor)
		}
		from = c
	}
	pg := page{maxRows: r.pageRows, maxB: r.pageBytes}
	answered := map[string]bool{}
	phases := tipsPhases()
	for i := slices.Index(phases, from.phase); i < len(phases); i++ {
		phase := phases[i]
		var after *tipsCursor
		if phase == from.phase && q.Cursor != "" {
			after = &from
		}
		full, err := r.tipsOf(ctx, tx, phase, in.RepoID, since, after, &pg, answered, out)
		if err != nil {
			return fmt.Errorf("gitlet: read the objects %s's expunge keeps (%s): %w", name, phase, err)
		}
		if full {
			return nil
		}
	}
	return nil
}

// tipsOf reads one phase of an `expunge_tips` page into out, from its start or
// after the row after names, counting every row read against pg
// ([tipAnswered]). full reports a page that took its last row, its cursor
// that row's place.
func (r *Reader) tipsOf(ctx context.Context, tx *sql.Tx, phase tipsPhase, repoID string, since int64,
	after *tipsCursor, pg *page, answered map[string]bool, out *ReadAnswer) (full bool, err error) {

	if phase == tipsRevisions {
		return r.revisionTips(ctx, tx, repoID, after, pg, answered, out)
	}
	var rows *sql.Rows
	switch phase {
	case tipsDrops:
		if after == nil {
			rows, err = tx.QueryContext(ctx, expungeTipsSQL, repoID, since, r.pageRows)
		} else {
			rows, err = tx.QueryContext(ctx, expungeTipsAfterSQL, repoID, after.at, after.position, after.ref,
				after.old, r.pageRows)
		}
	case tipsQueue:
		if after == nil {
			rows, err = tx.QueryContext(ctx, queueTipsSQL, repoID, r.pageRows)
		} else {
			rows, err = tx.QueryContext(ctx, queueTipsAfterSQL, repoID, after.ref, after.position, r.pageRows)
		}
	case tipsCandidates:
		if after == nil {
			rows, err = tx.QueryContext(ctx, candidateTipsSQL, repoID, r.pageRows)
		} else {
			rows, err = tx.QueryContext(ctx, candidateTipsAfterSQL, repoID, after.ref, after.id, after.position,
				r.pageRows)
		}
	case tipsGreen:
		if after == nil {
			rows, err = tx.QueryContext(ctx, greenTipsSQL, repoID, r.pageRows)
		} else {
			rows, err = tx.QueryContext(ctx, greenTipsAfterSQL, repoID, after.ref, r.pageRows)
		}
	}
	if err != nil {
		return false, err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		c := tipsCursor{phase: phase}
		var oid string
		switch phase {
		case tipsDrops:
			err = rows.Scan(&c.at, &c.position, &c.ref, &c.old)
			oid = c.old
		case tipsQueue:
			err = rows.Scan(&c.ref, &c.position, &oid)
		case tipsCandidates:
			err = rows.Scan(&c.ref, &c.id, &c.position, &oid)
		case tipsGreen:
			err = rows.Scan(&c.ref, &oid)
		}
		if err != nil {
			return false, err
		}
		if tipAnswered(c, oid, pg, answered, out) {
			return true, nil
		}
	}
	return false, rows.Err()
}

// revisionTips is the revisions phase of an `expunge_tips` page
// ([Reader.tipsOf]): live changes by source branch over code_changes_open_idx,
// then each one's revisions by key. TWO SEEKS, NOT A JOIN: the planner would
// sort every live revision by an inner column. A branch heads one live change
// (decideOpen's change_exists), but the cursor names the change too, since a
// new change may replace it between pages: only the cursor's own change
// resumes after its revision; any other starts from its first.
func (r *Reader) revisionTips(ctx context.Context, tx *sql.Tx, repoID string, after *tipsCursor, pg *page,
	answered map[string]bool, out *ReadAnswer) (full bool, err error) {

	q, args := liveChangeTipsSQL, []any{repoID, r.pageRows}
	if after != nil {
		q, args = liveChangeTipsFromSQL, []any{repoID, after.ref, r.pageRows}
	}
	for {
		batch, err := liveChangesOf(ctx, tx, q, args...)
		if err != nil {
			return false, err
		}
		for _, ch := range batch {
			from := int64(0)
			if after != nil && ch.id == after.id {
				from = after.position
			}
			if full, err := r.changeRevisionTips(ctx, tx, ch, from, pg, answered, out); full || err != nil {
				return full, err
			}
		}
		if len(batch) < r.pageRows {
			return false, nil
		}
		q, args = liveChangeTipsAfterSQL, []any{repoID, batch[len(batch)-1].ref, r.pageRows}
	}
}

// liveChangeTip is one live change a revisions phase reads: its id and its
// source branch.
type liveChangeTip struct{ id, ref string }

// liveChangesOf is the live changes q names, read whole and closed before
// their revisions are read, so no two statements are open at once on tx.
func liveChangesOf(ctx context.Context, tx *sql.Tx, q string, args ...any) ([]liveChangeTip, error) {
	rows, err := tx.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []liveChangeTip
	for rows.Next() {
		var c liveChangeTip
		if err := rows.Scan(&c.id, &c.ref); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// changeRevisionTips answers the heads of ch's revisions after revision from
// into out, in their number's order, as [Reader.tipsOf] answers a row.
func (r *Reader) changeRevisionTips(ctx context.Context, tx *sql.Tx, ch liveChangeTip, from int64, pg *page,
	answered map[string]bool, out *ReadAnswer) (full bool, err error) {

	rows, err := tx.QueryContext(ctx, changeRevisionTipsSQL, ch.id, from, r.pageRows)
	if err != nil {
		return false, err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		c := tipsCursor{phase: tipsRevisions, ref: ch.ref, id: ch.id}
		var oid string
		if err := rows.Scan(&c.position, &oid); err != nil {
			return false, err
		}
		if tipAnswered(c, oid, pg, answered, out) {
			return true, nil
		}
	}
	return false, rows.Err()
}

// tipAnswered answers oid into out unless the page answered it already, and
// counts its row against pg either way (a page is held to what it reads),
// reporting a page that took its last row, its cursor c.
func tipAnswered(c tipsCursor, oid string, pg *page, answered map[string]bool, out *ReadAnswer) bool {
	size := 0
	if !answered[oid] {
		answered[oid] = true
		out.Tips = append(out.Tips, oid)
		size = stringBytes(oid) + 1
	}
	if pg.add(size) {
		out.Next = c.String()
		return true
	}
	return false
}

// tipsPhase is one phase of an `expunge_tips` read ([Reader.expungeTips]): the
// table whose rows name what an expunge keeps.
type tipsPhase string

const (
	// tipsDrops is the ref drops since the read's instant (code_ref_drops).
	tipsDrops tipsPhase = "drops"
	// tipsRevisions is every revision head of a live change (code_revisions).
	tipsRevisions tipsPhase = "revisions"
	// tipsQueue is every live queue entry's head (code_queue).
	tipsQueue tipsPhase = "queue"
	// tipsCandidates is every candidate's tip (code_candidates).
	tipsCandidates tipsPhase = "candidates"
	// tipsGreen is every target's green row (code_green).
	tipsGreen tipsPhase = "green"
)

// tipsPhases is the phases in the order a read serves them.
func tipsPhases() []tipsPhase {
	return []tipsPhase{tipsDrops, tipsRevisions, tipsQueue, tipsCandidates, tipsGreen}
}

// tipsCursor is one row's place in its phase's index order, and an
// `expunge_tips` page's cursor, "<phase>:" then: drops
// "<at>:<position>:<old>:<ref>", revisions "<n>:<change>:<source_ref>", queue
// "<seq>:<target_ref>", candidates "<ordinal>:<train>:<target_ref>", green
// "<target_ref>". The ref is LAST, since it may hold every character but a
// colon (git's ref rules). position is a drop's record position, a revision's
// number, an entry's seq or a candidate's ordinal; id a revision's change or
// a candidate's train.
type tipsCursor struct {
	phase        tipsPhase
	at, position int64
	ref, old     string
	id           string
}

// String is c as a cursor.
func (c tipsCursor) String() string {
	p := string(c.phase) + ":"
	switch c.phase {
	case tipsDrops:
		return p + strconv.FormatInt(c.at, 10) + ":" + strconv.FormatInt(c.position, 10) + ":" + c.old + ":" + c.ref
	case tipsRevisions, tipsCandidates:
		return p + strconv.FormatInt(c.position, 10) + ":" + c.id + ":" + c.ref
	case tipsGreen:
		return p + c.ref
	}
	return p + strconv.FormatInt(c.position, 10) + ":" + c.ref
}

// cutTipsCursor reads a [tipsCursor].
func cutTipsCursor(s string) (tipsCursor, bool) {
	phase, rest, ok := strings.Cut(s, ":")
	c := tipsCursor{phase: tipsPhase(phase)}
	if !ok || !slices.Contains(tipsPhases(), c.phase) {
		return tipsCursor{}, false
	}
	fields := map[tipsPhase]int{tipsDrops: 4, tipsRevisions: 3, tipsQueue: 2, tipsCandidates: 3, tipsGreen: 1}
	parts := strings.SplitN(rest, ":", fields[c.phase])
	if len(parts) != fields[c.phase] || slices.Contains(parts, "") {
		return tipsCursor{}, false
	}
	c.ref = parts[len(parts)-1]
	if c.phase == tipsGreen {
		return c, true
	}
	n := 0
	if c.phase == tipsDrops {
		at, err := strconv.ParseInt(parts[0], 10, 64)
		if err != nil {
			return tipsCursor{}, false
		}
		c.at, n = at, 1
		c.old = parts[2]
	}
	position, err := strconv.ParseInt(parts[n], 10, 64)
	if err != nil || position < 0 {
		return tipsCursor{}, false
	}
	c.position = position
	if c.phase == tipsRevisions || c.phase == tipsCandidates {
		c.id = parts[1]
	}
	return c, true
}

// publishCursor is "<published_at>/<id>", the instant as the column stores it:
// a repository's place in a `publication_due` listing's order.
func publishCursor(publishedAt int64, id string) string {
	return strconv.FormatInt(publishedAt, 10) + "/" + id
}

// cutPublishCursor reads a [publishCursor].
func cutPublishCursor(s string) (int64, string, bool) {
	at, id, ok := strings.Cut(s, "/")
	if !ok || id == "" {
		return 0, "", false
	}
	published, err := strconv.ParseInt(at, 10, 64)
	if err != nil || published < 0 {
		return 0, "", false
	}
	return published, id, true
}

// RepoLife is one repository as a reconcile of code.repos reads it: its id and
// its name, whether it is live, deleted or purging, and who manages it — and
// nothing else.
type RepoLife struct {
	ID        string
	Name      string
	Status    RepoStatus
	ManagedBy ManagedBy
}

// lifePage is how many repositories one page of [Reader.WalkLives] reads:
// a thousand rows of four short columns, about 150 KB, so the ten thousand a
// document may declare are ten statements, each holding a reader-pool
// connection for milliseconds rather than the whole walk.
const lifePage = 1_000

// WalkLives hands visit every repository a reconcile of code.repos acts on
// (importing, active, archived or deleted; a purge is the code-retention
// duty's) in name order, a page of [lifePage] at a time, each one STALE read.
// A visit's error ends the walk.
//
// ITS OWN READ: [Reader.Read]'s listing joins the head, the rules and the
// counters a reconcile never uses, at hundreds of ms and MB over a full
// document. STALE IS ENOUGH: every decision is taken again in the reconcile's
// own snapshot after the activation's fence.
func (r *Reader) WalkLives(ctx context.Context, visit func(RepoLife) error) error {
	query := statelog.Freshness{Level: statelog.ReadStale}.Query(
		statelog.ScopeSet{Paths: []string{DomainPath()}}, true)
	page := make([]RepoLife, 0, lifePage)
	after := ""
	for {
		_, err := r.log.Read(ctx, query, func(tx *sql.Tx) error {
			page = page[:0]
			rows, err := tx.QueryContext(ctx, livesSQL, after, lifePage)
			if err != nil {
				return err
			}
			defer func() { _ = rows.Close() }()
			for rows.Next() {
				var (
					l               RepoLife
					status, managed string
				)
				if err := rows.Scan(&l.ID, &l.Name, &status, &managed); err != nil {
					return err
				}
				l.Status, l.ManagedBy = RepoStatus(status), ManagedBy(managed)
				page = append(page, l)
			}
			return rows.Err()
		})
		if err != nil {
			return fmt.Errorf("gitlet: list the repositories a reconcile acts on: %w", err)
		}
		for _, l := range page {
			if err := visit(l); err != nil {
				return err
			}
		}
		if len(page) < lifePage {
			return nil
		}
		after = page[len(page)-1].Name
	}
}

// dueRepo is one repository the code-retention pass steps: its id, its name
// to report, and its place in the listing's order.
type dueRepo struct {
	id, name string

	// purging says its purge has begun, so it is listed whatever the
	// retention.
	purging bool

	// deletedAt is its deletion's instant as stored: the listing's order,
	// with the id.
	deletedAt int64
}

// dueQuery asks for one page of what the code-retention pass steps.
type dueQuery struct {
	// cutoff is now less the retention: a DELETED repository is listed only
	// once its deletion is at or before it.
	cutoff time.Time

	// after is the last repository of the page before; nil for the first.
	after *dueRepo

	// limit is the most repositories the page lists.
	limit int
}

// due lists one page of what the code-retention pass steps, in its order:
// every PURGING repository first, whatever the retention (a begun purge is
// always finished: nothing restores partly gone rows), then every DELETED one
// at or before the cutoff; each phase oldest deletion first, resuming after
// q.after.
//
// A STALE READ of the three columns the pass uses, since every step decides
// again in its own snapshot; each phase ONE statement over its own partial
// index (replicated 0080), so a page reads only the rows it lists.
func (r *Reader) due(ctx context.Context, q dueQuery) ([]dueRepo, error) {
	if q.limit <= 0 {
		return nil, fmt.Errorf("%w: a page of the repositories due lists at least one", ErrInvalid)
	}
	var out []dueRepo
	query := statelog.Freshness{Level: statelog.ReadStale}.Query(
		statelog.ScopeSet{Paths: []string{DomainPath()}}, true)
	_, err := r.log.Read(ctx, query, func(tx *sql.Tx) error {
		out = out[:0]
		var (
			after   = int64(math.MinInt64)
			afterID string
			purging = true
		)
		if q.after != nil {
			after, afterID, purging = q.after.deletedAt, q.after.id, q.after.purging
		}
		if purging {
			var err error
			if out, err = dueIn(ctx, tx, out, true, duePurgingSQL, after, after, afterID,
				q.limit); err != nil {
				return fmt.Errorf("gitlet: read the repositories purging: %w", err)
			}
			if len(out) == q.limit {
				return nil
			}
			after, afterID = math.MinInt64, ""
		}
		var err error
		if out, err = dueIn(ctx, tx, out, false, dueDeletedSQL, store.EncodeTime(q.cutoff),
			after, after, afterID, q.limit-len(out)); err != nil {
			return fmt.Errorf("gitlet: read the repositories deleted past the retention: %w", err)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// dueIn appends to out every repository query lists, each marked purging or
// not.
func dueIn(ctx context.Context, tx *sql.Tx, out []dueRepo, purging bool, query string,
	args ...any) ([]dueRepo, error) {
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return out, err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		d := dueRepo{purging: purging}
		if err := rows.Scan(&d.id, &d.name, &d.deletedAt); err != nil {
			return out, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// purgedAt reports whether this node's rows hold repository id PURGED (row
// gone, deletion marker written), read at session floored at the purge's last
// record, so a node behind it refuses `behind` rather than answering early.
// False is definite: the record there removed nothing here.
func (r *Reader) purgedAt(ctx context.Context, id string, at statelog.Position) (bool, error) {
	if at.IsZero() {
		return false, refusedf("%w: at: a purge is confirmed at its last record's position", ErrInvalid)
	}
	var purged bool
	query := statelog.FloorAt(statelog.ReadSession, at).Query(
		statelog.ScopeSet{Paths: []string{RepoPath(id)}}, false)
	_, err := r.log.Read(ctx, query, func(tx *sql.Tx) error {
		var rows, marked int
		if err := tx.QueryRowContext(ctx, purgeStateSQL, id, id).Scan(&rows, &marked); err != nil {
			return fmt.Errorf("gitlet: read whether %s is purged: %w", id, err)
		}
		purged = rows == 0 && marked > 0
		return nil
	})
	return purged, err
}

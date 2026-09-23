package pages

import (
	"context"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/store"
)

// Reader answers questions about pages from this node's own applied rows.
//
// # What replaced the hydration flag
//
// The projection this reads in place of had a boolean: hydrated or not, with
// no way to say by how much it was behind. An applier's position is a PLACE ON
// A LOG, so a read here reports the level it answered at and a caller that
// needs its own write back waits for its own position — which is the whole
// reason the family moved off the bucket.
// EVERY READ GOES THROUGH [statelog.Reader], which is what turns the level
// from a word into a property of the answer: the refusal ladder first (an
// evicted node, one below the trim floor, one whose applier has stopped serves
// nothing), then the coverage probe over what the read is ABOUT, then the
// freshness target — a quorum-committed barrier for `linearizable`, the
// caller's own high-water mark for `session`, a declared bound for `stale` —
// and only then the rows.
//
// AND ONE TRANSACTION, which this reader also did not have. Every statement
// ran on `SQL()` directly, so a `Get` read the head, the comments, the
// history, the children and the ancestors at five different instants: a page
// answered with a comment thread from after the revision it reported, and a
// child list that could contain a page the head knew nothing about. D119 says
// every multi-statement answer runs inside one `BEGIN DEFERRED`, and the
// framework's read is where that transaction now comes from.
type Reader struct {
	db  *store.DB
	log *statelog.Reader

	// committed is this node's own applied position on the pages log, or
	// nil on a build that runs no applier. It is what a read's own answer
	// is stamped with.
	committed func() statelog.Position
}

// ReaderOptions configure a reader.
type ReaderOptions struct {
	DB *store.DB

	// Log is this domain's read authority. REQUIRED: without it every read
	// level is a label rather than a guarantee, which is silent at every
	// surface that renders one.
	Log *statelog.Reader

	// Committed is this node's applied position. Nil answers the zero
	// position, which is what a test with no runner has and what a read
	// then honestly reports.
	Committed func() statelog.Position
}

// NewReader builds the knowledge base's read side.
func NewReader(opts ReaderOptions) (*Reader, error) {
	if opts.DB == nil {
		return nil, errors.New("pages: a store is required")
	}
	if opts.Log == nil {
		return nil, errors.New("pages: a reader needs its domain's read " +
			"authority — without it every read level is a label rather than " +
			"a guarantee, and a degradation invisible in the answer is worse " +
			"than a refusal")
	}
	r := &Reader{db: opts.DB, log: opts.Log, committed: opts.Committed}
	if r.committed == nil {
		r.committed = func() statelog.Position { return statelog.Position{} }
	}
	return r, nil
}

// At is the position this node's rows were derived through, which every answer
// here is true as of.
func (r *Reader) At() statelog.Position { return r.committed() }

// Filter narrows a page listing.
type Filter struct {
	Container string
	ParentID  string
	Status    []Status
	Label     string
	Watcher   string
	Title     string

	// Skills narrows to tool-skill pages, or excludes them. A POINTER
	// because all three states are real: only skills (the sync walk),
	// everything but skills (an ordinary browse), and everything.
	Skills *bool

	// Onboarding narrows to the pages a seat's reading chain starts at.
	Onboarding bool

	Limit int

	// After resumes a listing strictly after the last page of a previous
	// one: it takes that listing's [Listing.NextCursor], and a value that
	// does not decode as one is refused naming the field.
	//
	// A KEYSET over the listing's own order — container, title, id — so
	// the next page starts where the last one ended whatever happened in
	// between: every page that matched on both reads and did not move in
	// that order is returned exactly once. A page renamed or trashed
	// between the two reads moves itself, and nothing else.
	After string

	// Offset skips that many matching pages, counted from After when it is
	// set and from the start when it is not. It shows a window at a
	// numbered position — rows 201 to 250 as of this read — and WALKING a
	// listing is After's job, not its: an offset counts rows rather than
	// naming one, so a page that leaves the rows it skips between two reads
	// (trashed out of a status filter, renamed past them, purged) moves
	// every later page up by one, and a walk by offset misses one.
	Offset int
}

// DefaultLimit is the page a listing that names no limit gets, and MaxLimit
// the largest page any listing gets — a larger limit is lowered to it, not
// refused.
//
// THEY BOUND A PAGE, NEVER WHAT A LISTING REACHES. [Reader.list] reads one row
// past the page and reports it as [Listing.Truncated] and
// [Listing.NextCursor], and [Filter.After] with that cursor reaches the rows
// after it; a page's children, which a detail read carries at DefaultLimit,
// are the same read with a parent and a status filter, and
// [Detail.ChildrenTruncated] and [Detail.ChildrenCursor] say when they went
// past it and where to resume.
//
// FIFTY is a judgement about the callers that did not choose: a seat's
// `list_pages` with no limit, and every page detail's children. A summary
// carries no body and encodes to about 300 bytes on a representative row, and
// to about 2.7 KB with a title at [MaxTitle] and [MaxLabels] labels at
// [MaxLabelLength] — so a default page is about 15 KB, and under 140 KB with
// every row at those caps.
//
// MaxLimit HAS A CEILING OF ITS OWN: [Reader.attachLabels] binds every id on
// the page in ONE statement, so a page has to stay under the parameter count
// internal/store falls back to when its probe of the engine cannot tell — 999.
// Past that, a full page on such an engine would be a refused statement rather
// than a slow one. Five hundred sits under it with room, and at the sizes
// above is at most about 1.4 MB.
const (
	DefaultLimit = 50
	MaxLimit     = 500
)

// Summary is one page as a listing renders it. The BODY IS ABSENT: fifty
// pages at 512 KiB each is twenty-five megabytes to draw a list of titles.
type Summary struct {
	ID         string    `json:"id"`
	Container  string    `json:"container"`
	ParentID   string    `json:"parent_id,omitempty"`
	Title      string    `json:"title"`
	Status     Status    `json:"status"`
	Author     string    `json:"author,omitempty"`
	Version    int       `json:"version"`
	Skill      bool      `json:"skill,omitempty"`
	Onboarding bool      `json:"onboarding,omitempty"`
	Labels     []string  `json:"labels,omitempty"`
	Updated    time.Time `json:"updated_at"`
	Revision   uint64    `json:"revision"`
}

// Listing is a page listing and what it is true as of.
//
// THE POSITION AND THE COVERAGE TRAVEL WITH THE ROWS, because a caller cannot
// reconstruct either afterwards: [Reader.At] answers about NOW rather than
// about the transaction this listing was read in, and a set read that could
// not account for everything is indistinguishable from a short one.
type Listing struct {
	Pages []Summary `json:"pages"`

	// Truncated says more pages matching the filter follow this listing's
	// last one, and [Listing.NextCursor] is how a caller reaches them. It is
	// read from one row past the limit, so a filter matching exactly the
	// limit is not truncated. A RENDERER THAT DRAWS [Listing.Pages] MUST READ
	// IT: the length of the list is the page, not the count of what matched.
	//
	// DISTINCT FROM [Listing.Complete], which covers the OTHER kind of
	// incompleteness — a deferred record's scope meeting this read — so a
	// caller checking that alone was told the answer was whole while half
	// the container was missing. The tool built on this goes out of its way
	// to surface Complete, on the reasoning that "a model that reads a
	// short list as the whole truth writes the duplicate"; a full page it
	// cannot see is full is the same mistake with nothing to check.
	Truncated bool `json:"truncated,omitempty"`

	// NextCursor is the [Filter.After] that resumes this listing strictly
	// after its last page, set exactly when Truncated is.
	NextCursor string `json:"next_cursor,omitempty"`

	// Level is what the read was SERVED at, which is the level asked for
	// or a refusal — never the level requested, which is how a level
	// becomes a label.
	Level statelog.ReadLevel `json:"read_level"`

	// Complete is false when a deferred record's scope meets this listing:
	// pages may be missing, pages that should have left may still be here.
	Complete bool `json:"complete"`

	// Position is the point on the log these rows were derived through.
	Position statelog.Position `json:"position"`

	// LogLag is ABSENT rather than zero when the broker could not be
	// reached: a read asks how far behind an answer may be, and an
	// unreachable broker answers "not at all".
	LogLag *uint64 `json:"log_lag,omitempty"`
}

// List answers a filtered listing at a read level.
//
// A SET READ, so a deferred scope makes it INCOMPLETE rather than refused: a
// listing cannot enumerate what would have ENTERED it — a deferred create is
// an absence with no local row — so it is served and says so. See
// [Listing.Complete].
func (r *Reader) List(ctx context.Context, f Filter, fresh statelog.Freshness) (Listing, error) {
	if fresh.Level == "" {
		return Listing{}, errors.New("pages: this read names no level — a " +
			"surface resolves an absent read_level to its own default (a seat " +
			"tool linearizable, a dashboard poll stale) before it reads")
	}
	where := []string{"1 = 1"}
	var args []any
	if f.Container != "" {
		where = append(where, "p.container = ?")
		args = append(args, strings.ToUpper(f.Container))
	}
	if f.ParentID != "" {
		where = append(where, "p.parent_id = ?")
		args = append(args, f.ParentID)
	}
	if len(f.Status) > 0 {
		marks := make([]string, len(f.Status))
		for i, s := range f.Status {
			marks[i] = "?"
			args = append(args, string(s))
		}
		where = append(where, "p.status IN ("+strings.Join(marks, ",")+")")
	}
	if f.Label != "" {
		where = append(where,
			"EXISTS (SELECT 1 FROM pages_labels l WHERE l.page_id = p.id AND l.label = ?)")
		args = append(args, f.Label)
	}
	if f.Watcher != "" {
		where = append(where,
			"EXISTS (SELECT 1 FROM pages_watchers w WHERE w.page_id = p.id "+
				"AND w.handle = ? AND w.muted = 0)")
		args = append(args, f.Watcher)
	}
	if title := strings.TrimSpace(f.Title); title != "" {
		// ESCAPED, and the ESCAPE clause says so. A title filter is text a
		// person typed, and `%` and `_` are LIKE's own wildcards: without
		// this, filtering for "100%" matches every page in the container
		// and nothing says the filter did not apply.
		where = append(where, `p.title LIKE ? ESCAPE '\'`)
		args = append(args, store.LikeContains(title))
	}
	if f.Skills != nil {
		if *f.Skills {
			where = append(where, "COALESCE(k.skill, 0) = 1")
		} else {
			where = append(where, "COALESCE(k.skill, 0) = 0")
		}
	}
	if f.Onboarding {
		where = append(where, "COALESCE(k.onboarding, 0) = 1")
	}

	if f.After != "" {
		after, err := decodeListCursor(f.After)
		if err != nil {
			return Listing{}, err
		}
		where, args = after.resume(where, args)
	}

	limit := f.Limit
	if limit <= 0 {
		limit = DefaultLimit
	}
	if limit > MaxLimit {
		limit = MaxLimit
	}
	var out []Summary
	var next string
	served, err := r.log.Read(ctx, fresh.Query(ReadScope(f.Container, ""), true), func(tx *sql.Tx) error {
		var err error
		out, next, err = r.list(ctx, tx, where, args, limit, max(f.Offset, 0))
		return err
	})
	if err != nil {
		return Listing{}, err
	}
	return Listing{
		Pages: out, Truncated: next != "", NextCursor: next,
		Level: served.Level, Complete: served.Complete,
		Position: served.Position, LogLag: served.Lag,
	}, nil
}

// listCursor is a listing's position in its own order: the container, title
// and id of the last page a listing returned.
//
// THE ID IS PART OF THE KEY although a title is its container's address,
// because nothing in `pages_heads` enforces that: the address is held in
// `pages_titles`, and a keyset over a pair two rows could share would skip
// the second of them. The id makes the order total whatever the table holds.
type listCursor struct {
	Container string
	Title     string
	ID        string
}

// encode renders the cursor a caller hands back as [Filter.After].
//
// OPAQUE — each value base64-encoded, joined by a dot the encoding's own
// alphabet never produces — because a title is prose carrying any character,
// and a cursor a caller could read as a title is one they would start
// composing by hand.
func (c listCursor) encode() string {
	return strings.Join([]string{
		base64.RawURLEncoding.EncodeToString([]byte(c.Container)),
		base64.RawURLEncoding.EncodeToString([]byte(c.Title)),
		base64.RawURLEncoding.EncodeToString([]byte(c.ID)),
	}, ".")
}

// decodeListCursor reads a [Filter.After] back, refusing a value that does not
// decode as one.
func decodeListCursor(after string) (listCursor, error) {
	parts := strings.Split(after, ".")
	var values [3][]byte
	ok := len(parts) == len(values)
	for i := 0; ok && i < len(values); i++ {
		var err error
		values[i], err = base64.RawURLEncoding.DecodeString(parts[i])
		ok = err == nil
	}
	// AN ID IS NEVER EMPTY, so a cursor without one was not minted by
	// [listCursor.encode] — and resuming after an empty id would repeat the
	// page the cursor names.
	if !ok || len(values[2]) == 0 {
		return listCursor{}, invalid("after", "%q is not a cursor a page "+
			"listing returned — pass a listing's `next_cursor` unchanged", after)
	}
	return listCursor{
		Container: string(values[0]), Title: string(values[1]), ID: string(values[2]),
	}, nil
}

// resume adds the predicate that starts a listing strictly after the cursor,
// in the listing's own ORDER BY.
//
// SPELLED OUT rather than as a row-value comparison, so it does not depend on
// the engine supporting `(a, b, c) > (?, ?, ?)`. The columns compare under
// the same collation the ORDER BY sorts them in, which is what makes
// "strictly after" and "the next row" the same row.
func (c listCursor) resume(where []string, args []any) ([]string, []any) {
	where = append(slices.Clip(where), `(p.container > ? OR (p.container = ? AND `+
		`(p.title > ? OR (p.title = ? AND p.id > ?))))`)
	args = append(slices.Clip(args), c.Container, c.Container, c.Title, c.Title, c.ID)
	return where, args
}

// list is the listing inside one transaction, so [Reader.Get] can take the
// children it reports from the same snapshot as the page itself.
//
// THE BOUND IS BOUND HERE, not by the caller. The placeholders are positional,
// so a caller that appended the page window to `args` itself would be one
// reordering away from paging the listing by a filter value — and the caller
// that got it right would still be stating the same two numbers twice.
// The second return is the cursor past this page, or empty when the page did
// not FILL — one row past the limit is read as evidence and dropped, the same
// shape [Reader.Activity] uses beside it, and the cursor is minted from the
// last row kept.
//
// Without the probe a listing of fifty pages and a container holding exactly
// fifty answered identically, and `Listing.Complete` covers only the OTHER
// kind of incompleteness (a deferred record's scope), so a caller checking it
// was told the answer was whole while half the container was missing.
func (r *Reader) list(ctx context.Context, tx *sql.Tx, where []string,
	args []any, limit, offset int) ([]Summary, string, error) {

	args = append(slices.Clip(args), limit+1, offset)
	rows, err := tx.QueryContext(ctx, `
		SELECT `+summaryColumns+`
		  FROM pages_heads p
		  LEFT JOIN pages_skills k ON k.page_id = p.id
		 WHERE `+strings.Join(where, " AND ")+`
		 ORDER BY p.container, p.title, p.id
		 LIMIT ? OFFSET ?`, args...)
	if err != nil {
		return nil, "", fmt.Errorf("pages: list pages: %w", err)
	}
	defer rows.Close()

	var out []Summary
	for rows.Next() {
		s, err := scanSummary(rows)
		if err != nil {
			return nil, "", err
		}
		out = append(out, s)
	}
	if err := rows.Err(); err != nil {
		return nil, "", fmt.Errorf("pages: list pages: %w", err)
	}
	// THE PROBE ROW IS EVIDENCE, never an answer: the page stays at the
	// bound and the caller is told there is more, and where it starts.
	var next string
	if len(out) > limit {
		out = out[:limit]
		last := out[limit-1]
		next = listCursor{Container: last.Container, Title: last.Title, ID: last.ID}.encode()
	}
	return out, next, r.attachLabels(ctx, tx, out)
}

// summaryColumns is what a [Summary] is read from, over `pages_heads p` LEFT
// JOINed to `pages_skills k`, in the order [scanSummary] takes them. A listing
// and a parent chain read the same row, so they read it with one list.
const summaryColumns = `p.id, p.container, p.parent_id, p.title, p.status,
	p.author, p.edit_version, COALESCE(k.skill, 0), COALESCE(k.onboarding, 0),
	p.updated_at, MAX(p.version, p.scoped_through)`

// scanSummary reads one row of [summaryColumns].
func scanSummary(row interface{ Scan(...any) error }) (Summary, error) {
	var (
		s                 Summary
		skill, onboarding int
		updated, revision int64
	)
	if err := row.Scan(&s.ID, &s.Container, &s.ParentID, &s.Title, &s.Status,
		&s.Author, &s.Version, &skill, &onboarding, &updated, &revision); err != nil {
		return Summary{}, fmt.Errorf("pages: scan page: %w", err)
	}
	s.Skill, s.Onboarding = skill != 0, onboarding != 0
	s.Updated = store.DecodeTime(updated)
	s.Revision = uint64(revision)
	return s, nil
}

func (r *Reader) attachLabels(ctx context.Context, tx *sql.Tx, items []Summary) error {
	if len(items) == 0 {
		return nil
	}
	ids := make([]any, len(items))
	at := make(map[string]int, len(items))
	for i, item := range items {
		ids[i] = item.ID
		at[item.ID] = i
	}
	rows, err := tx.QueryContext(ctx,
		`SELECT page_id, label FROM pages_labels WHERE page_id IN (`+
			placeholders(len(ids))+`) ORDER BY label`, ids...)
	if err != nil {
		return fmt.Errorf("pages: read labels: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id, label string
		if err := rows.Scan(&id, &label); err != nil {
			return fmt.Errorf("pages: scan label: %w", err)
		}
		if i, ok := at[id]; ok {
			items[i].Labels = append(items[i].Labels, label)
		}
	}
	return rows.Err()
}

// Detail is one page with everything a reader opening it wants.
type Detail struct {
	Page     Page              `json:"page"`
	Revision uint64            `json:"revision"`
	Comments []Comment         `json:"comments,omitempty"`
	History  []RevisionSummary `json:"history,omitempty"`

	// Children are this page's PUBLISHED children, the first [DefaultLimit]
	// of them in a listing's order. Published only for the reason
	// [Status.Readable] gives: a trashed page is deleted as far as any
	// reader is concerned and a draft is somebody's unfinished thought, and
	// a detail read is served to seats as well as to people.
	Children []Summary `json:"children,omitempty"`

	// ChildrenTruncated says this page has more published children than the
	// read carries. There is no paging parameter on a detail read, so the
	// rest are a listing's: [Filter] with this page as ParentID, Status
	// published and ChildrenCursor as After is the children after the last
	// one here — the same filter and the same order, so it neither repeats
	// nor skips a child that stayed where it was.
	ChildrenTruncated bool `json:"children_truncated,omitempty"`

	// ChildrenCursor is that [Filter.After], set exactly when
	// ChildrenTruncated is.
	ChildrenCursor string `json:"children_cursor,omitempty"`

	// Ancestors are the pages above this one, outermost first, and all of
	// them — see [parentChains.above] for why the walk has no depth cap and
	// what it returns when the chain loops.
	Ancestors []Summary `json:"ancestors,omitempty"`

	// Level is what the read was SERVED at, and Position the point on the
	// log every part above was read through — one instant, because they
	// are read in one transaction.
	Level    statelog.ReadLevel `json:"read_level"`
	Position statelog.Position  `json:"position"`

	// LogLag is ABSENT rather than zero when the broker could not be
	// reached.
	LogLag *uint64 `json:"log_lag,omitempty"`
}

// RevisionSummary is one past version as the history list renders it.
//
// METADATA ONLY, because a detail read that carried bodies would carry up to
// [RevisionsKept] of them. [Reader.Revision] reads one version's body.
type RevisionSummary struct {
	Version   int       `json:"version"`
	Author    string    `json:"author,omitempty"`
	Message   string    `json:"message,omitempty"`
	CreatedAt time.Time `json:"created_at"`
}

// Get reads one page by id, or by "CONTAINER/Title".
//
// A POINT READ, so a deferred scope REFUSES rather than answering incomplete:
// this answer is about one page, and if that page's own scope is stale there
// is nothing honest to return. That is the opposite of [Reader.List] and the
// difference is what the set flag means.
//
// EVERY PART IN ONE TRANSACTION — the head, the comments, the history, the
// children and the ancestors. They used to be five separate statements on the
// bare connection, so a page could answer with a comment thread from after the
// revision it reported.
func (r *Reader) Get(ctx context.Context, ref string, fresh statelog.Freshness) (Detail, error) {
	if fresh.Level == "" {
		return Detail{}, errors.New("pages: this read names no level — a " +
			"surface resolves an absent read_level to its own default before " +
			"it reads")
	}
	var detail Detail
	container, _, _ := strings.Cut(ref, "/")
	served, err := r.log.Read(ctx, fresh.Query(ReadScope(container, ref), false), func(tx *sql.Tx) error {
		document, revision, id, err := r.locate(ctx, tx, ref)
		if err != nil {
			return err
		}
		page, err := DecodePage([]byte(document))
		if err != nil {
			return err
		}
		detail = Detail{Page: page, Revision: revision}
		if detail.Comments, err = r.comments(ctx, tx, id); err != nil {
			return err
		}
		if detail.History, err = r.history(ctx, tx, id); err != nil {
			return err
		}
		// THIS READ HAS NO PAGING PARAMETER OF ITS OWN, so the marker and
		// the cursor are the whole of what a caller gets — see
		// [Detail.ChildrenTruncated] for where the rest is.
		if detail.Children, detail.ChildrenCursor, err = r.list(ctx, tx,
			[]string{"p.parent_id = ?", "p.status = ?"},
			[]any{id, string(StatusPublished)}, DefaultLimit, 0); err != nil {
			return err
		}
		detail.ChildrenTruncated = detail.ChildrenCursor != ""
		var looped bool
		detail.Ancestors, looped, err = newParentChains(tx).above(ctx, id, page.ParentID)
		if looped {
			// REPORTED RATHER THAN WALKED: nothing is missing from the
			// breadcrumb — every page the chain reaches is on it once —
			// and walking on would only repeat them.
			log.WarnContext(ctx, "pages_ancestor_cycle", "page", id,
				"detail", "this page's parent chain runs into a loop; the "+
					"breadcrumb lists every page the chain reaches once. Moving "+
					"any page on the loop to the top of its container breaks it")
		}
		return err
	})
	if err != nil {
		return Detail{}, err
	}
	detail.Level = served.Level
	detail.Position = served.Position
	detail.LogLag = served.Lag
	return detail, nil
}

// locate resolves a reference to a page row.
func (r *Reader) locate(ctx context.Context, tx *sql.Tx, ref string) (document string, revision uint64, id string, err error) {
	var rev int64
	query := `SELECT id, document, ` + HeadRevision + ` FROM pages_heads WHERE id = ?`
	args := []any{ref}
	if container, title, ok := strings.Cut(ref, "/"); ok {
		// "CONTAINER/Title", which is how a person and a model name a page
		// — the title is its address, and the container scopes it.
		// AGAINST title_norm, which is the value the fleet CLAIMED.
		//
		// The form this replaces compared LOWER(title) against the
		// normalised reference, and this engine's LOWER() folds ASCII
		// ONLY: a page titled "Qualité Étendue" was unreachable by its own
		// address, because the claim lowercased it with Go's Unicode case
		// tables and the lookup did not. It also could not use an index,
		// so every address lookup scanned the container.
		query = `SELECT id, document, ` + HeadRevision + ` FROM pages_heads
		          WHERE container = ? AND title_norm = ?`
		args = []any{strings.ToUpper(strings.TrimSpace(container)), NormalizeTitle(title)}
	}
	err = tx.QueryRowContext(ctx, query, args...).Scan(&id, &document, &rev)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return "", 0, "", fmt.Errorf("%w: page %s", ErrNotFound, ref)
	case err != nil:
		return "", 0, "", fmt.Errorf("pages: read %s: %w", ref, err)
	}
	return document, uint64(rev), id, nil
}

func (r *Reader) comments(ctx context.Context, tx *sql.Tx, pageID string) ([]Comment, error) {
	rows, err := tx.QueryContext(ctx,
		`SELECT document FROM pages_comments WHERE page_id = ? ORDER BY created_at, id`,
		pageID)
	if err != nil {
		return nil, fmt.Errorf("pages: read the thread on %s: %w", pageID, err)
	}
	defer rows.Close()
	var out []Comment
	for rows.Next() {
		var document string
		if err := rows.Scan(&document); err != nil {
			return nil, fmt.Errorf("pages: scan comment: %w", err)
		}
		comment, err := DecodeComment([]byte(document))
		if err != nil {
			log.WarnContext(ctx, "pages_comment_unreadable", "page", pageID,
				"error", err.Error())
			continue
		}
		out = append(out, comment)
	}
	return out, rows.Err()
}

// history is every revision the page still holds, newest first.
//
// NO LIMIT, because the table is already bounded: every save that writes a
// revision beyond the first [RevisionsKept] carries the one it retires
// ([retiredRevisions]), and the apply deletes it in the same transaction, so a
// page holds at most RevisionsKept rows here. A LIMIT of that size could only
// ever hide rows a correct writer never leaves, and it would hide them
// silently.
func (r *Reader) history(ctx context.Context, tx *sql.Tx, pageID string) ([]RevisionSummary, error) {
	rows, err := tx.QueryContext(ctx,
		`SELECT edit_version, author, message, created_at FROM pages_revisions
		  WHERE page_id = ? ORDER BY version DESC`, pageID)
	if err != nil {
		return nil, fmt.Errorf("pages: read the history of %s: %w", pageID, err)
	}
	defer rows.Close()
	var out []RevisionSummary
	for rows.Next() {
		var s RevisionSummary
		var at int64
		if err := rows.Scan(&s.Version, &s.Author, &s.Message, &at); err != nil {
			return nil, fmt.Errorf("pages: scan revision: %w", err)
		}
		s.CreatedAt = store.DecodeTime(at)
		out = append(out, s)
	}
	return out, rows.Err()
}

// parentChains walks parent chains inside one transaction, one primary-key read
// per page and each page read at most once however many chains pass through
// it. A page's breadcrumb ([Reader.Get]) and a search hit's ancestry
// ([Searcher.Search]) are both this walk, so the two cannot disagree about what
// is above a page.
type parentChains struct {
	tx   *sql.Tx
	read map[string]Summary
}

func newParentChains(tx *sql.Tx) *parentChains {
	return &parentChains{tx: tx, read: map[string]Summary{}}
}

// page reads one page as a listing renders it, and whether it is there.
func (c *parentChains) page(ctx context.Context, id string) (Summary, bool, error) {
	if s, ok := c.read[id]; ok {
		return s, true, nil
	}
	s, err := scanSummary(c.tx.QueryRowContext(ctx, `
		SELECT `+summaryColumns+`
		  FROM pages_heads p
		  LEFT JOIN pages_skills k ON k.page_id = p.id
		 WHERE p.id = ?`, id))
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return Summary{}, false, nil
	case err != nil:
		return Summary{}, false, fmt.Errorf("pages: walk the parent chain: %w", err)
	}
	c.read[id] = s
	return s, true, nil
}

// above is every page above one page, outermost first, and whether the chain
// runs into a loop.
//
// NO DEPTH CAP, because nothing at write bounds a tree's depth and a cap here
// would drop the outermost pages of a deep chain with nothing on the answer
// saying so. What ends the walk is its record of the pages it has visited,
// which starts with the page itself: a write refuses a parent that would close
// a loop ([checkParent]), but two moves of two different pages, each decided
// before the other applied, can still close one between them. On a loop the
// walk stops at the first page it has already visited, so it returns every
// page it reached once and never the page itself — for a page on the loop,
// every other page on it.
//
// A parent this node holds no page for ends the chain, since nothing is known
// above it.
func (c *parentChains) above(ctx context.Context, pageID, parentID string) (
	[]Summary, bool, error) {

	var chain []Summary
	looped := false
	seen := map[string]bool{pageID: true}
	for id := parentID; id != ""; {
		if seen[id] {
			looped = true
			break
		}
		seen[id] = true
		// A step through a page already read makes no call that would
		// notice a cancelled read, so the walk asks itself.
		if err := ctx.Err(); err != nil {
			return nil, false, err
		}
		s, held, err := c.page(ctx, id)
		if err != nil {
			return nil, false, err
		}
		if !held {
			break
		}
		chain = append(chain, s)
		id = s.ParentID
	}
	// Outermost first, which is breadcrumb order.
	slices.Reverse(chain)
	return chain, looped, nil
}

// ContainerListing is one container plus the figure a browser needs beside it.
//
// A DERIVED COUNT rather than a field on [Container], because the container
// document is what a writer wrote and this is a fact about OTHER rows: putting
// it on the document would mean every page create rewrites its container, and
// two writers adding a page to one space would contend on a counter neither of
// them touched.
type ContainerListing struct {
	Container

	// Pages is how many pages this container holds, live ones only.
	//
	// Counted in the SAME TRANSACTION as the containers, so a browser's
	// rail cannot show a count from one instant beside a list from
	// another — the same rule the tracker's own listings follow.
	Pages int `json:"pages"`
}

// Containers is every container this node knows about.
//
// THE DOMAIN IS ITS SCOPE, because a container list is about all of them —
// which is exactly what [ReadScope] returns for a read that names none.
func (r *Reader) Containers(ctx context.Context, fresh statelog.Freshness) (
	[]ContainerListing, error) {

	if fresh.Level == "" {
		return nil, errors.New("pages: this read names no level")
	}
	var out []ContainerListing
	_, err := r.log.Read(ctx, fresh.Query(ReadScope("", ""), true), func(tx *sql.Tx) error {
		containers, err := r.containers(ctx, tx)
		if err != nil {
			return err
		}
		counts, err := r.pageCounts(ctx, tx)
		if err != nil {
			return err
		}
		out = make([]ContainerListing, 0, len(containers))
		for _, c := range containers {
			out = append(out, ContainerListing{Container: c, Pages: counts[c.Key]})
		}
		return nil
	})
	return out, err
}

// pageCounts is how many pages each container holds.
//
// ONE GROUP BY rather than a count per container: a company with forty spaces
// would otherwise take forty round trips to draw one rail, and every one of
// them inside the read transaction the containers were listed in.
//
// TRASHED PAGES ARE NOT COUNTED: a trashed page is deleted as far as any
// reader is concerned (see [Status]), and this is how many pages a container
// holds. The predicate restates the status rather than reading `trashed_at`
// because the container index is on `(container, status, title)`: the query
// plan for this one is a scan of that index alone, where `trashed_at IS NULL`
// plans as a scan of another index that reads every row back from the table.
func (r *Reader) pageCounts(ctx context.Context, tx *sql.Tx) (map[string]int, error) {
	rows, err := tx.QueryContext(ctx,
		`SELECT container, COUNT(*) FROM pages_heads
		  WHERE status != ?
		  GROUP BY container`, string(StatusTrashed))
	if err != nil {
		return nil, fmt.Errorf("pages: count pages per container: %w", err)
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var key string
		var n int
		if err := rows.Scan(&key, &n); err != nil {
			return nil, fmt.Errorf("pages: scan a container's page count: %w", err)
		}
		out[key] = n
	}
	return out, rows.Err()
}

func (r *Reader) containers(ctx context.Context, tx *sql.Tx) ([]Container, error) {
	rows, err := tx.QueryContext(ctx,
		`SELECT document FROM pages_containers ORDER BY key`)
	if err != nil {
		return nil, fmt.Errorf("pages: list containers: %w", err)
	}
	defer rows.Close()
	var out []Container
	for rows.Next() {
		var document string
		if err := rows.Scan(&document); err != nil {
			return nil, fmt.Errorf("pages: scan container: %w", err)
		}
		container, err := DecodeContainer([]byte(document))
		if err != nil {
			continue
		}
		out = append(out, container)
	}
	return out, rows.Err()
}

// placeholders renders an IN list of n binds.
func placeholders(n int) string {
	if n <= 0 {
		return "NULL"
	}
	return strings.TrimSuffix(strings.Repeat("?,", n), ",")
}

// SkillPages are the tool-skill pages in one container, with their bodies.
//
// # Why this is its own read rather than a List plus a Get each
//
// A listing deliberately carries no body — fifty pages at 512 KiB each is
// twenty-five megabytes to draw a list of titles — so the caller that
// genuinely needs bodies would otherwise issue one query per page. The
// tool-skills container is the one place that is true, and it is small: a
// company with twenty MCP servers has twenty skills, not twenty thousand.
//
// TRASHED PAGES ARE EXCLUDED. A skill somebody removed must leave the
// registry, and a walk that returned it would put it straight back — the
// replace is wholesale, so what this returns IS the registry.
func (r *Reader) SkillPages(ctx context.Context, container string,
	fresh statelog.Freshness) ([]Page, error) {

	if fresh.Level == "" {
		return nil, errors.New("pages: this read names no level")
	}
	container = strings.ToUpper(strings.TrimSpace(container))
	if container == "" {
		return nil, nil
	}
	var out []Page
	_, err := r.log.Read(ctx, fresh.Query(ReadScope(container, ""), true), func(tx *sql.Tx) error {
		var err error
		out, err = r.skillPages(ctx, tx, container)
		return err
	})
	return out, err
}

func (r *Reader) skillPages(ctx context.Context, tx *sql.Tx,
	container string) ([]Page, error) {

	rows, err := tx.QueryContext(ctx, `
		SELECT p.document
		  FROM pages_heads p
		  JOIN pages_skills k ON k.page_id = p.id
		 WHERE p.container = ? AND k.skill = 1 AND p.status <> ?
		 ORDER BY p.title`, container, string(StatusTrashed))
	if err != nil {
		return nil, fmt.Errorf("pages: list the skills in %s: %w", container, err)
	}
	defer rows.Close()
	var out []Page
	for rows.Next() {
		var document string
		if err := rows.Scan(&document); err != nil {
			return nil, fmt.Errorf("pages: scan a skill page: %w", err)
		}
		page, err := DecodePage([]byte(document))
		if err != nil {
			// LEFT OUT rather than failing the walk: one page a newer
			// build wrote must not cost the company every other skill.
			log.WarnContext(ctx, "pages_skill_undecodable", "container", container,
				"error", err.Error())
			continue
		}
		out = append(out, page)
	}
	return out, rows.Err()
}

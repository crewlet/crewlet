package doclet

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/keyset"
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
	log *statelog.Reader
}

// ReaderOptions configure a reader.
//
// NO STORE HANDLE, and that is the point rather than an omission: every
// statement a page read issues runs inside the transaction the framework's
// read opens ([statelog.Reader]), which is what the read level, the snapshot
// and the coverage probe all rest on. A handle beside it is a door a
// statement can leave through, outside all three — one the reader carried
// and never read, which kept the door shut by luck rather than by design.
type ReaderOptions struct {
	// Log is this domain's read authority. REQUIRED: without it every read
	// level is a label rather than a guarantee, which is silent at every
	// surface that renders one.
	Log *statelog.Reader
}

// NewReader builds the knowledge base's read side.
func NewReader(opts ReaderOptions) (*Reader, error) {
	if opts.Log == nil {
		return nil, errors.New("doclet: a reader needs its domain's read " +
			"authority — without it every read level is a label rather than " +
			"a guarantee, and a degradation invisible in the answer is worse " +
			"than a refusal")
	}
	return &Reader{log: opts.Log}, nil
}

// Filter narrows a page listing.
type Filter struct {
	Container string
	ParentID  string

	// Roots narrows to the pages at the TOP of their container — the ones
	// with no parent. Its own field rather than an empty ParentID, because an
	// empty ParentID already means "any parent", and a tree loading its first
	// level asks the opposite question.
	Roots bool

	Status  []Status
	Label   string
	Watcher string
	Title   string

	// Skills narrows to tool-skill pages, or excludes them. A POINTER
	// because all three states are real: only skills (the sync walk),
	// everything but skills (an ordinary browse), and everything.
	Skills *bool

	// Onboarding narrows to the pages a seat's reading chain starts at.
	Onboarding bool

	Limit int

	// After is the [Listing.After] of the page before this one, or empty for
	// the first page. A CURSOR rather than an offset: a listing is read while
	// seats write, and an offset over a set a create or a rename just moved
	// repeats one row and skips another with nothing to say it did — which a
	// tree's "Load more" is exactly the reader to notice.
	After string
}

// ErrBadCursor refuses an [Filter.After] this reader did not mint.
var ErrBadCursor = errors.New("doclet: that cursor is not one this listing minted")

// DefaultLimit and MaxLimit bound a listing.
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

	// Children is how many pages sit directly under this one that the SAME
	// listing would show — its status and skill narrowing applied to them,
	// nothing else. A tree draws an expander off it, and a count that included
	// a trashed child would draw one that opens onto nothing.
	Children int `json:"children,omitempty"`

	// Folder marks a page with nothing written in it: its body is EMPTY. A
	// folder is not a kind of page the engine stores. It is what a page that
	// only groups pages IS, so the mark cannot disagree with the page: write a
	// paragraph into one and it is a page with children, empty one and it is
	// a folder again.
	Folder bool `json:"folder,omitempty"`

	// SavedBy and SavedAt are the last change to what the page SAYS: its
	// create, a save or a rename, each of which writes a revision.
	// [Summary.Updated] moves with every change, a label, a watch and a move
	// included, which is the wrong instant for "last saved". SavedBy is the
	// writer under the one name every row of theirs carries ([Actor.Name]): a
	// seat's handle, which is a person's seat too, or the login of a credential
	// nobody is bound through.
	SavedBy string    `json:"saved_by,omitempty"`
	SavedAt time.Time `json:"saved_at"`

	// savedID is the history row the two fields above are read from.
	savedID string
}

// savedKinds are the changes that write a revision: what "last saved" means.
const savedKinds = `'created', 'saved', 'renamed'`

// summarySaved is the read-time column every listing row carries beyond its
// head: the history row of its last save, which [Reader.attachSaves] reads in
// one statement for the whole page of rows. FROM THE HISTORY because the head
// keeps who CREATED the page and when anything last moved it, and neither is
// who last changed what it says.
const summarySaved = `
		       (SELECT h.id FROM pages_history h
		         WHERE h.page_id = p.id AND h.kind IN (` + savedKinds + `)
		         ORDER BY h.created_at DESC, h.id DESC LIMIT 1)`

// Listing is a page listing and what it is true as of.
//
// THE TOTAL AND THE CURSOR TRAVEL WITH THE ROWS, because a page of rows alone
// cannot say whether it is everything: fifty pages in a container of fifty and
// fifty in a container of four hundred were the same answer, and every screen
// that drew one said "50" about both.
//
// THE POSITION AND THE COVERAGE TRAVEL WITH THE ROWS, because a caller cannot
// reconstruct either afterwards: a position read after the answer is about NOW
// rather than about the transaction this listing was read in, and a set read
// that could not account for everything is indistinguishable from a short one.
type Listing struct {
	Pages []Summary `json:"pages"`

	// Total is how many pages the filter matches, ALL of them rather than
	// this page — counted in the same transaction as the rows, so the two
	// can never describe different instants.
	Total int `json:"total"`

	// After is what to pass as [Filter.After] for the next page, and ABSENT
	// on the last one.
	After string `json:"after,omitempty"`

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
		return Listing{}, errors.New("doclet: this read names no level; a " +
			"surface resolves an absent read_level to its own default (a seat " +
			"tool linearizable, a dashboard poll stale) before it reads")
	}
	spec, child := compileListing(f)
	limit := f.Limit
	if limit <= 0 {
		limit = DefaultLimit
	}
	if limit > MaxLimit {
		limit = MaxLimit
	}
	resume := listFromStart
	if f.After != "" {
		c, err := parseListCursor(f.After)
		if err != nil {
			return Listing{}, err
		}
		resume = listResumes(c, f.Container != "")
	}
	var out listPage
	served, err := r.log.Read(ctx, fresh.Query(ReadScope(f.Container), true), func(tx *sql.Tx) error {
		var err error
		out, err = r.list(ctx, tx, spec, child, limit, resume)
		return err
	})
	if err != nil {
		return Listing{}, err
	}
	return Listing{
		Pages: out.rows, Total: out.total, After: out.after,
		Level: served.Level, Complete: served.Complete,
		Position: served.Position, LogLag: served.Lag,
	}, nil
}

// compileListing is the predicate a filter's listing is, and the one each of
// its pages' children is counted under.
func compileListing(f Filter) (spec, child listSpec) {
	where := []string{"1 = 1"}
	var args []any
	if f.Container != "" {
		where = append(where, "p.container = ?")
		args = append(args, ContainerKey(f.Container))
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
	// WHAT A CHILD IS COUNTED UNDER: the listing's own status and skill
	// narrowing, on the child. Only those two — a label or a title filter
	// selects which pages are LISTED, and a child that does not share its
	// parent's label is still under it.
	child = listSpec{where: []string{"c.parent_id = p.id"}}
	if len(f.Status) > 0 {
		marks := make([]string, len(f.Status))
		for i, s := range f.Status {
			marks[i] = "?"
			child.args = append(child.args, string(s))
		}
		child.where = append(child.where, "c.status IN ("+strings.Join(marks, ",")+")")
	}
	if f.Skills != nil {
		if *f.Skills {
			where = append(where, "COALESCE(k.skill, 0) = 1")
			child.where = append(child.where, "COALESCE(ck.skill, 0) = 1")
		} else {
			where = append(where, "COALESCE(k.skill, 0) = 0")
			child.where = append(child.where, "COALESCE(ck.skill, 0) = 0")
		}
	}
	if f.Onboarding {
		where = append(where, "COALESCE(k.onboarding, 0) = 1")
	}
	if f.Roots {
		where = append(where, "p.parent_id = ''")
	}
	return listSpec{where: where, args: args}, child
}

// listSpec is one set of predicates and the arguments they bind, in order.
type listSpec struct {
	where []string
	args  []any
}

// listPage is one window of a listing: its rows, how many the predicate
// matches in all, and where the next window starts.
type listPage struct {
	rows  []Summary
	total int
	after string
}

// listCursor is where a listing window stopped: the ORDER's own columns
// ([listOrder]), so the next window is everything strictly after it in that
// order.
type listCursor struct {
	Container string `json:"c"`
	Title     string `json:"t"`
	ID        string `json:"i"`
}

func (c listCursor) String() string {
	body, err := json.Marshal(c)
	if err != nil {
		return ""
	}
	return base64.RawURLEncoding.EncodeToString(body)
}

func parseListCursor(raw string) (listCursor, error) {
	body, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil {
		return listCursor{}, fmt.Errorf("%w: %w", ErrBadCursor, err)
	}
	var c listCursor
	if err := json.Unmarshal(body, &c); err != nil || c.ID == "" {
		return listCursor{}, fmt.Errorf("%w: it decodes to no position", ErrBadCursor)
	}
	return c, nil
}

// childListing is the listing of one page's children a [Detail] carries —
// every one of them, whatever its status — and the predicate each of theirs
// is counted under.
func childListing(id string) (spec, child listSpec) {
	return listSpec{where: []string{"p.parent_id = ?"}, args: []any{id}},
		listSpec{where: []string{"c.parent_id = p.id"}}
}

// listOrder is the order every listing is read in, as the terms a later
// window resumes through: the container, the title, and the id, which keeps
// the order total. The knowledge base's own arbitration keeps one container's
// titles apart — a create claims its title's normalised form, and a trashed
// page keeps its claim — so two rows tied on both are rows that arbitration
// failed to keep out, a restore's residue, and a cursor with nothing to break
// the tie would skip one of them. Every column is NOT NULL, so no arm of a
// resume reads a NULL tail. Replicated 0062 holds the
// order in `pages_heads_container_idx` (container, title, id) and in
// `pages_heads_parent_idx` (parent_id, container, title, id), so a window is a
// walk of one of them that its LIMIT stops, never a sort of everything the
// listing matches.
var listOrder = []keyset.Term{
	{Column: "p.container", NeverNull: true},
	{Column: "p.title", NeverNull: true},
	{Column: "p.id", NeverNull: true},
}

// listFromStart is a listing's first window: the listing itself, from its
// first row.
var listFromStart = []listSpec{{}}

// listResumes is where the window after the row a cursor names is read: the
// arms of the order strictly after that row ([keyset.After]), nearest first,
// each an equality prefix and one bound — one seek of the index the order is
// served by — read in turn until the window is full.
//
// NOT ONE PREDICATE. Spelled as the nested disjunction `container > ? OR
// (container = ? AND (title > ? OR …))`, no conjunct bounds the leading
// column, and the planner seeks nothing: measured, a later window read every
// page the listing matched from the address index and sorted them all, so
// paging a knowledge base cost the square of its size. The keyset package's
// doc has the rest.
//
// A LISTING PINNED TO ONE CONTAINER resumes on its title and id alone. Its
// container is the same on every row, so the order's leading arm — every
// container after this one — holds nothing, and stated beside the listing's
// own `p.container = ?` it is a predicate no index range can express: the
// planner seeks the container and reads every page in it to find none past it,
// on exactly the last window of a container, where the arms before it ran
// out. A tree level under one parent keeps all three terms, because there
// the container is no equality of the listing's and its arm is an empty
// range of the parent index.
func listResumes(after listCursor, pinned bool) []listSpec {
	order, keys := listOrder, []any{after.Container, after.Title, after.ID}
	if pinned {
		order, keys = order[1:], keys[1:]
	}
	arms := keyset.After(order, keys)
	out := make([]listSpec, len(arms))
	for i, arm := range arms {
		clause, args := arm.SQL(order)
		out[i] = listSpec{where: []string{clause}, args: args}
	}
	return out
}

// list is the listing inside one transaction, so [Reader.Get] can take the
// children it reports from the same snapshot as the page itself: the total,
// and then the rows of one window, read through each of resume in turn until
// the window is full.
//
// THE TOTAL IS THE PREDICATE WITHOUT THE CURSOR, counted in the same
// transaction: it describes the whole listing rather than what is left of it.
func (r *Reader) list(ctx context.Context, tx *sql.Tx, spec, child listSpec,
	limit int, resume []listSpec) (listPage, error) {

	var page listPage
	statement, args := listTotalStatement(spec)
	if err := tx.QueryRowContext(ctx, statement, args...).Scan(&page.total); err != nil {
		return listPage{}, fmt.Errorf("doclet: count pages: %w", err)
	}

	var out []Summary
	for _, from := range resume {
		// ONE MORE THAN THE WINDOW, which is how the answer knows whether
		// there is a next one without a second statement — and what is
		// left of it, once an arm before this one has answered some of it.
		statement, args := listRowsStatement(spec, child, from, limit+1-len(out))
		read, err := listRows(ctx, tx, statement, args)
		if err != nil {
			return listPage{}, err
		}
		out = append(out, read...)
		if len(out) > limit {
			break
		}
	}
	if len(out) > limit {
		out = out[:limit]
		last := out[len(out)-1]
		page.after = listCursor{Container: last.Container, Title: last.Title, ID: last.ID}.String()
	}
	page.rows = out
	if err := r.attachLabels(ctx, tx, out); err != nil {
		return listPage{}, err
	}
	return page, r.attachSaves(ctx, tx, out)
}

// listTotalStatement counts what the listing matches, and its arguments.
func listTotalStatement(spec listSpec) (string, []any) {
	return `
		SELECT COUNT(*)
		  FROM pages_heads p
		  LEFT JOIN pages_skills k ON k.page_id = p.id
		 WHERE ` + strings.Join(spec.where, " AND "), spec.args
}

// listRowsStatement is one read of a window — the listing narrowed by from,
// which is nothing for a first window and one arm of a resume for a later one
// ([listResumes]) — in the listing's order, and its arguments.
//
// THE BOUND IS BOUND HERE, not by the caller. The placeholders are positional,
// so a caller that appended the window to its arguments itself would be one
// reordering away from paging the listing by a filter value — and the caller
// that got it right would still be stating the same numbers twice. The child
// count's arguments come FIRST because its subquery is in the SELECT list,
// which a statement binds before its WHERE.
func listRowsStatement(spec, child, from listSpec, limit int) (string, []any) {
	where := strings.Join(append(slices.Clip(spec.where), from.where...), " AND ")
	args := append(slices.Clip(child.args), spec.args...)
	args = append(args, from.args...)
	return `
		SELECT p.id, p.container, p.parent_id, p.title, p.status, p.author,
		       p.edit_version, COALESCE(k.skill, 0), COALESCE(k.onboarding, 0),
		       p.updated_at, MAX(p.version, p.scoped_through),
		       (SELECT COUNT(*) FROM pages_heads c
		          LEFT JOIN pages_skills ck ON ck.page_id = c.id
		         WHERE ` + strings.Join(child.where, " AND ") + `),
		       p.body = '',` + summarySaved + `
		  FROM pages_heads p
		  LEFT JOIN pages_skills k ON k.page_id = p.id
		 WHERE ` + where + `
		 ORDER BY p.container, p.title, p.id
		 LIMIT ?`, append(args, limit)
}

// listRows reads the rows of one window read.
func listRows(ctx context.Context, tx *sql.Tx, statement string, args []any) ([]Summary, error) {
	rows, err := tx.QueryContext(ctx, statement, args...)
	if err != nil {
		return nil, fmt.Errorf("doclet: list pages: %w", err)
	}
	defer rows.Close()

	var out []Summary
	for rows.Next() {
		var (
			s                         Summary
			skill, onboarding, folder int
			updated, revision         int64
			savedID                   sql.NullString
		)
		if err := rows.Scan(&s.ID, &s.Container, &s.ParentID, &s.Title, &s.Status,
			&s.Author, &s.Version, &skill, &onboarding, &updated, &revision,
			&s.Children, &folder, &savedID); err != nil {
			return nil, fmt.Errorf("doclet: scan page: %w", err)
		}
		s.Skill, s.Onboarding, s.Folder = skill != 0, onboarding != 0, folder != 0
		s.savedID = savedID.String
		s.Updated = store.DecodeTime(updated)
		s.Revision = uint64(revision)
		out = append(out, s)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("doclet: list pages: %w", err)
	}
	return out, nil
}

// attachSaves fills each row's last save from the history row the listing
// named for it, in one statement for the whole page of rows.
func (r *Reader) attachSaves(ctx context.Context, tx *sql.Tx, items []Summary) error {
	ids := make([]any, 0, len(items))
	at := make(map[string]int, len(items))
	for i, item := range items {
		if item.savedID == "" {
			continue
		}
		ids = append(ids, item.savedID)
		at[item.savedID] = i
	}
	if len(ids) == 0 {
		return nil
	}
	rows, err := tx.QueryContext(ctx,
		`SELECT id, actor, created_at FROM pages_history WHERE id IN (`+
			placeholders(len(ids))+`)`, ids...)
	if err != nil {
		return fmt.Errorf("doclet: read who last saved: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id, actor string
		var when int64
		if err := rows.Scan(&id, &actor, &when); err != nil {
			return fmt.Errorf("doclet: scan who last saved: %w", err)
		}
		if i, ok := at[id]; ok {
			items[i].SavedBy = actor
			items[i].SavedAt = store.DecodeTime(when)
		}
	}
	return rows.Err()
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
		return fmt.Errorf("doclet: read labels: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id, label string
		if err := rows.Scan(&id, &label); err != nil {
			return fmt.Errorf("doclet: scan label: %w", err)
		}
		if i, ok := at[id]; ok {
			items[i].Labels = append(items[i].Labels, label)
		}
	}
	return rows.Err()
}

// Detail is one page with everything a reader opening it wants.
type Detail struct {
	Page     Page   `json:"page"`
	Revision uint64 `json:"revision"`

	// Skill and Onboarding are what the applier DERIVED from the page —
	// whether its body parses as a tool skill, whether it is the onboarding
	// page — read from `pages_skills` in the same transaction. They sit
	// beside the page rather than on it because [Page] is the record's own
	// document and neither flag is anything a writer wrote. They were
	// missing from this answer altogether, so a tool-skill page's own screen
	// never said what it was: a listing row carried the mark and the page it
	// opened did not.
	Skill      bool `json:"skill"`
	Onboarding bool `json:"onboarding"`

	// Folder is [Summary.Folder]: the page's body is empty, so it is a page
	// that only groups pages.
	Folder bool `json:"folder"`

	Comments []Comment         `json:"comments,omitempty"`
	History  []RevisionSummary `json:"history,omitempty"`
	Children []Summary         `json:"children,omitempty"`

	// ChildrenTotal is how many children the page has, of which Children
	// is the first [DefaultLimit] by title — so a page with sixty children
	// says sixty rather than drawing fifty as all of them.
	ChildrenTotal int `json:"children_total"`

	// Ancestors are the parent chain, outermost first. Carried because
	// the auto-draft exclusion is by ancestor and a reader wants the
	// breadcrumb.
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
// METADATA ONLY, because the projection keeps only that — reading one
// revision's body is a coordination read, on demand.
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
		return Detail{}, errors.New("doclet: this read names no level; a " +
			"surface resolves an absent read_level to its own default before " +
			"it reads")
	}
	var detail Detail
	// THE SCOPE IS WHERE THE PAGE IS FILED, which only its rows can say: a
	// reference is an id or an address, and neither is a path — an id
	// names no space, and an address names a title rather than the page
	// holding it. So the framework resolves it in the answer's own
	// transaction ([pageReadScope]) and probes there.
	served, err := r.log.Read(ctx, fresh.Resolved(func(ctx context.Context,
		tx *sql.Tx) (statelog.ScopeSet, error) {
		return pageReadScope(ctx, tx, ref)
	}, false), func(tx *sql.Tx) error {
		document, revision, id, err := r.locate(ctx, tx, ref)
		if err != nil {
			return err
		}
		page, err := DecodePage([]byte(document))
		if err != nil {
			return err
		}
		detail = Detail{Page: page, Revision: revision}
		var skill, onboarding int
		switch serr := tx.QueryRowContext(ctx,
			`SELECT skill, onboarding FROM pages_skills WHERE page_id = ?`, id).
			Scan(&skill, &onboarding); {
		case errors.Is(serr, sql.ErrNoRows):
		case serr != nil:
			return fmt.Errorf("doclet: read what %s was derived as: %w", id, serr)
		}
		detail.Skill, detail.Onboarding = skill != 0, onboarding != 0
		detail.Folder = page.Body == ""
		if detail.Comments, err = r.comments(ctx, tx, id); err != nil {
			return err
		}
		if detail.History, err = r.history(ctx, tx, id); err != nil {
			return err
		}
		spec, child := childListing(id)
		children, err := r.list(ctx, tx, spec, child, DefaultLimit, listFromStart)
		if err != nil {
			return err
		}
		detail.Children, detail.ChildrenTotal = children.rows, children.total
		detail.Ancestors, err = r.ancestors(ctx, tx, page.ParentID)
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

// pageReadScope is what a point read of one page is about, resolved from the
// reference the caller named — inside the answer's own transaction, where the
// framework probes it ([statelog.Query.Resolve]).
//
// THE PAGE'S OWN PATH, under the space it is filed in. An address adds its
// title term as well, because a create or a rename arbitrates on the title and
// is filed under it — a deferred rename that took this address is about this
// read even while the page it resolved to still holds the name. The address's
// container is canonical ([ContainerKey]), as every scope a record is filed
// under is.
//
// A REFERENCE THAT RESOLVES TO NOTHING STILL BOUNDS A SCOPE, because "no such
// page" is a claim about rows a deferred create has not written: an address is
// exactly the term such a create is filed under, and an id is decided by
// [absentPageScope].
func pageReadScope(ctx context.Context, tx *sql.Tx, ref string) (statelog.ScopeSet, error) {
	query := `SELECT id, container FROM pages_heads WHERE id = ?`
	args := []any{ref}
	var address *ScopeTerm
	if container, title, ok := strings.Cut(ref, "/"); ok {
		container = ContainerKey(container)
		address = &ScopeTerm{Kind: TermTitle, Container: container, ID: TitleToken(title)}
		query = `SELECT id, container FROM pages_heads WHERE container = ? AND title_norm = ?`
		args = []any{container, NormalizeTitle(title)}
	}
	var id, container string
	err := tx.QueryRowContext(ctx, query, args...).Scan(&id, &container)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		if address == nil {
			return absentPageScope(ctx, tx, ref)
		}
		return statelog.ScopeSet{Paths: []string{address.Path()}}, nil
	case err != nil:
		return statelog.ScopeSet{}, fmt.Errorf("doclet: resolve %s: %w", ref, err)
	}
	paths := []string{ScopeTerm{Kind: TermObject, Container: container, ID: id}.Path()}
	if address != nil {
		paths = append(paths, address.Path())
	}
	return statelog.ScopeSet{Paths: paths}.Normalised(), nil
}

// absentPageScope is [pageReadScope] for an ID no row holds.
//
// A MINTED ID NAMES NO SPACE: the object path a create files it under names a
// container the absent row cannot supply. So the page is about its object path
// UNDER EVERY CONTAINER, and every term that contains such a path — the
// domain, the containers' root, and ANY container's own term, since a scope is
// the complete set of objects its apply may write and a container term covers
// every page that may come to be in it. That is not a finite list of paths, so
// it is answered as the retained records' own paths that fall inside it
// ([mayHoldPage]) beside the id's object path in the containerless space,
// whose every ancestor falls inside it too: the framework's probe then matches
// exactly the records that may create the page. The domain term, the obvious
// answer, refused an absent id under ANY retained record — one about another
// page in another space included — and an object path in one space alone
// served "no such page" past a record about a whole space that may be the one
// creating it.
//
// ANYTHING ELSE names no page a record could create, because every page id is
// minted: it is the containerless object path alone, which only a record about
// the whole domain covers.
//
// THE CONTAINER IS MATCHED IN GO, segment by segment, rather than by a LIKE
// pattern: `%` spans the separator, so a pattern for "any container's own
// term" also matches every page path under every container, and LIKE folds
// case where a path does not. The retained set it walks is this node's
// undecodable records, which a node holds few of and a node holding many has
// a louder problem than this read.
func absentPageScope(ctx context.Context, tx *sql.Tx, ref string) (statelog.ScopeSet, error) {
	id, minted := mintedID(ref)
	if !minted {
		return statelog.ScopeSet{Paths: []string{
			ScopeTerm{Kind: TermObject, ID: ref}.Path(),
		}}, nil
	}
	paths := []string{ScopeTerm{Kind: TermObject, ID: id.String()}.Path()}
	rows, err := tx.QueryContext(ctx, `
		SELECT DISTINCT s.path
		  FROM pages_log_deferred_scope s
		  JOIN pages_log_deferred d ON d.position = s.position`)
	if err != nil {
		return statelog.ScopeSet{}, fmt.Errorf("doclet: ask whether a retained "+
			"record creates page %s: %w", id, err)
	}
	defer rows.Close()
	for rows.Next() {
		var path string
		if err := rows.Scan(&path); err != nil {
			return statelog.ScopeSet{}, fmt.Errorf("doclet: read a retained "+
				"record's scope: %w", err)
		}
		if mayHoldPage(path, id.String()) {
			paths = append(paths, path)
		}
	}
	if err := rows.Err(); err != nil {
		return statelog.ScopeSet{}, fmt.Errorf("doclet: read the retained "+
			"records' scopes: %w", err)
	}
	return statelog.ScopeSet{Paths: paths}.Normalised(), nil
}

// mayHoldPage reports whether a retained record's scope path covers page id
// in SOME container: whether path is the page's object path, or any term
// above it, with the container segment matching whatever it names — see
// [absentPageScope].
func mayHoldPage(path, id string) bool {
	within := strings.Split(ScopeTerm{Kind: TermObject, Container: anyContainer,
		ID: id}.Path(), statelog.ScopeSeparator)
	held := strings.Split(strings.Trim(strings.TrimSpace(path),
		statelog.ScopeSeparator), statelog.ScopeSeparator)
	if len(held) > len(within) {
		return false
	}
	for i, segment := range held {
		if within[i] != anyContainer && segment != within[i] {
			return false
		}
	}
	return true
}

// anyContainer stands in the container segment of the path [mayHoldPage]
// compares against. A space key is upper-case and slug-shaped, so no real
// container is spelled with a byte a path never carries.
const anyContainer = "\x00"

// mintedID is ref as the uuid an id this engine mints is, and whether it is
// one — an address, or any other text, names no page a record could create.
func mintedID(ref string) (uuid.UUID, bool) {
	if strings.Contains(ref, "/") {
		return uuid.UUID{}, false
	}
	id, err := uuid.Parse(ref)
	return id, err == nil
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
		args = []any{ContainerKey(container), NormalizeTitle(title)}
	}
	err = tx.QueryRowContext(ctx, query, args...).Scan(&id, &document, &rev)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return "", 0, "", fmt.Errorf("%w: page %s", ErrNotFound, ref)
	case err != nil:
		return "", 0, "", fmt.Errorf("doclet: read %s: %w", ref, err)
	}
	return document, uint64(rev), id, nil
}

// comments reads a page's thread.
func (r *Reader) comments(ctx context.Context, tx *sql.Tx, pageID string) ([]Comment, error) {
	rows, err := tx.QueryContext(ctx,
		`SELECT document FROM pages_comments
		  WHERE page_id = ? ORDER BY created_at, id`,
		pageID)
	if err != nil {
		return nil, fmt.Errorf("doclet: read the thread on %s: %w", pageID, err)
	}
	defer rows.Close()
	var out []Comment
	for rows.Next() {
		var document string
		if err := rows.Scan(&document); err != nil {
			return nil, fmt.Errorf("doclet: scan comment: %w", err)
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

func (r *Reader) history(ctx context.Context, tx *sql.Tx, pageID string) ([]RevisionSummary, error) {
	rows, err := tx.QueryContext(ctx,
		`SELECT edit_version, author, message, created_at FROM pages_revisions
		  WHERE page_id = ? ORDER BY version DESC LIMIT ?`, pageID, RevisionsKept)
	if err != nil {
		return nil, fmt.Errorf("doclet: read the history of %s: %w", pageID, err)
	}
	defer rows.Close()
	var out []RevisionSummary
	for rows.Next() {
		var s RevisionSummary
		var at int64
		if err := rows.Scan(&s.Version, &s.Author, &s.Message, &at); err != nil {
			return nil, fmt.Errorf("doclet: scan revision: %w", err)
		}
		s.CreatedAt = store.DecodeTime(at)
		out = append(out, s)
	}
	return out, rows.Err()
}

// ancestorDepth bounds the parent walk.
//
// Sixteen. A page tree that deep is already unnavigable, so a breadcrumb stops
// there.
const ancestorDepth = 16

func (r *Reader) ancestors(ctx context.Context, tx *sql.Tx, parentID string) ([]Summary, error) {
	var chain []Summary
	for id := parentID; id != "" && len(chain) < ancestorDepth; {
		var s Summary
		var skill, onboarding int
		var updated, revision int64
		err := tx.QueryRowContext(ctx, `
			SELECT p.id, p.container, p.parent_id, p.title, p.status, p.author,
			       p.edit_version, COALESCE(k.skill, 0), COALESCE(k.onboarding, 0),
			       p.updated_at, MAX(p.version, p.scoped_through)
			  FROM pages_heads p
			  LEFT JOIN pages_skills k ON k.page_id = p.id
			 WHERE p.id = ?`, id).
			Scan(&s.ID, &s.Container, &s.ParentID, &s.Title, &s.Status, &s.Author,
				&s.Version, &skill, &onboarding, &updated, &revision)
		if errors.Is(err, sql.ErrNoRows) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("doclet: walk the parent chain: %w", err)
		}
		s.Skill, s.Onboarding = skill != 0, onboarding != 0
		s.Updated = store.DecodeTime(updated)
		s.Revision = uint64(revision)
		chain = append(chain, s)
		id = s.ParentID
	}
	// Outermost first, which is breadcrumb order.
	for i, j := 0, len(chain)-1; i < j; i, j = i+1, j-1 {
		chain[i], chain[j] = chain[j], chain[i]
	}
	return chain, nil
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
		return nil, errors.New("doclet: this read names no level")
	}
	var out []ContainerListing
	_, err := r.log.Read(ctx, fresh.Query(ReadScope(""), true), func(tx *sql.Tx) error {
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
// TRASHED PAGES ARE NOT COUNTED. A trashed page is invisible to every default
// listing this reader serves, so counting it would put a number on the rail
// that the list beside it cannot account for — a reader clicks "12" and finds
// nine. The predicate restates the status rather than reading `trashed_at`,
// because the container index carries the status after the listing's order
// (`(container, title, id, status)`, replicated 0062), so this walk reads the
// index alone and never a page's row.
func (r *Reader) pageCounts(ctx context.Context, tx *sql.Tx) (map[string]int, error) {
	rows, err := tx.QueryContext(ctx,
		`SELECT container, COUNT(*) FROM pages_heads
		  WHERE status != ?
		  GROUP BY container`, string(StatusTrashed))
	if err != nil {
		return nil, fmt.Errorf("doclet: count pages per container: %w", err)
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var key string
		var n int
		if err := rows.Scan(&key, &n); err != nil {
			return nil, fmt.Errorf("doclet: scan a container's page count: %w", err)
		}
		out[key] = n
	}
	return out, rows.Err()
}

func (r *Reader) containers(ctx context.Context, tx *sql.Tx) ([]Container, error) {
	rows, err := tx.QueryContext(ctx,
		`SELECT document FROM pages_containers ORDER BY key`)
	if err != nil {
		return nil, fmt.Errorf("doclet: list containers: %w", err)
	}
	defer rows.Close()
	var out []Container
	for rows.Next() {
		var document string
		if err := rows.Scan(&document); err != nil {
			return nil, fmt.Errorf("doclet: scan container: %w", err)
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
		return nil, errors.New("doclet: this read names no level")
	}
	container = ContainerKey(container)
	if container == "" {
		return nil, nil
	}
	var out []Page
	_, err := r.log.Read(ctx, fresh.Query(ReadScope(container), true), func(tx *sql.Tx) error {
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
		return nil, fmt.Errorf("doclet: list the skills in %s: %w", container, err)
	}
	defer rows.Close()
	var out []Page
	for rows.Next() {
		var document string
		if err := rows.Scan(&document); err != nil {
			return nil, fmt.Errorf("doclet: scan a skill page: %w", err)
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

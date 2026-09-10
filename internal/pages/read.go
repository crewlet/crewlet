package pages

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
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

	Limit  int
	Offset int
}

// DefaultLimit and MaxLimit bound a listing, on [work]'s reasoning.
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
func (r *Reader) List(ctx context.Context, f Filter, level statelog.ReadLevel) (Listing, error) {
	if level == "" {
		return Listing{}, errors.New("pages: this read names no level — a " +
			"surface resolves an absent read_level to its own default (a seat " +
			"tool session, a dashboard poll stale) before it reads")
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

	limit := f.Limit
	if limit <= 0 {
		limit = DefaultLimit
	}
	if limit > MaxLimit {
		limit = MaxLimit
	}
	args = append(args, limit, max(f.Offset, 0))

	var out []Summary
	served, err := r.log.Read(ctx, statelog.Query{
		Level: level, Scope: ReadScope(f.Container, ""), Set: true,
	}, func(tx *sql.Tx) error {
		var err error
		out, err = r.list(ctx, tx, where, args, limit)
		return err
	})
	if err != nil {
		return Listing{}, err
	}
	return Listing{
		Pages: out, Level: served.Level, Complete: served.Complete,
		Position: served.Position, LogLag: served.Lag,
	}, nil
}

// list is the listing inside one transaction, so [Reader.Get] can take the
// children it reports from the same snapshot as the page itself.
func (r *Reader) list(ctx context.Context, tx *sql.Tx, where []string,
	args []any, limit int) ([]Summary, error) {

	rows, err := tx.QueryContext(ctx, `
		SELECT p.id, p.container, p.parent_id, p.title, p.status, p.author,
		       p.edit_version, COALESCE(k.skill, 0), COALESCE(k.onboarding, 0),
		       p.updated_at, MAX(p.version, p.scoped_through)
		  FROM pages_heads p
		  LEFT JOIN pages_skills k ON k.page_id = p.id
		 WHERE `+strings.Join(where, " AND ")+`
		 ORDER BY p.container, p.title
		 LIMIT ? OFFSET ?`, args...)
	if err != nil {
		return nil, fmt.Errorf("pages: list pages: %w", err)
	}
	defer rows.Close()

	var out []Summary
	for rows.Next() {
		var (
			s                 Summary
			skill, onboarding int
			updated, revision int64
		)
		if err := rows.Scan(&s.ID, &s.Container, &s.ParentID, &s.Title, &s.Status,
			&s.Author, &s.Version, &skill, &onboarding, &updated, &revision); err != nil {
			return nil, fmt.Errorf("pages: scan page: %w", err)
		}
		s.Skill, s.Onboarding = skill != 0, onboarding != 0
		s.Updated = store.DecodeTime(updated)
		s.Revision = uint64(revision)
		out = append(out, s)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("pages: list pages: %w", err)
	}
	return out, r.attachLabels(ctx, tx, out)
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
	Children []Summary         `json:"children,omitempty"`

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
func (r *Reader) Get(ctx context.Context, ref string, level statelog.ReadLevel) (Detail, error) {
	if level == "" {
		return Detail{}, errors.New("pages: this read names no level — a " +
			"surface resolves an absent read_level to its own default before " +
			"it reads")
	}
	var detail Detail
	container, _, _ := strings.Cut(ref, "/")
	served, err := r.log.Read(ctx, statelog.Query{
		Level: level, Scope: ReadScope(container, ref),
	}, func(tx *sql.Tx) error {
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
		if detail.Children, err = r.list(ctx, tx,
			[]string{"1 = 1", "p.parent_id = ?"}, []any{id, DefaultLimit, 0},
			DefaultLimit); err != nil {
			return err
		}
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

// locate resolves a reference to a page row.
func (r *Reader) locate(ctx context.Context, tx *sql.Tx, ref string) (document string, revision uint64, id string, err error) {
	var rev int64
	query := `SELECT id, document, MAX(version, scoped_through) FROM pages_heads WHERE id = ?`
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
		query = `SELECT id, document, MAX(version, scoped_through) FROM pages_heads
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

func (r *Reader) history(ctx context.Context, tx *sql.Tx, pageID string) ([]RevisionSummary, error) {
	rows, err := tx.QueryContext(ctx,
		`SELECT edit_version, author, message, created_at FROM pages_revisions
		  WHERE page_id = ? ORDER BY version DESC LIMIT ?`, pageID, RevisionsKept)
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

// ancestorDepth bounds the parent walk.
//
// Sixteen. A page tree that deep is already unnavigable, and the cap is here
// so a cycle — which a save that set a page's parent to its own descendant
// would create — terminates rather than hanging the read that found it.
const ancestorDepth = 16

func (r *Reader) ancestors(ctx context.Context, tx *sql.Tx, parentID string) ([]Summary, error) {
	var chain []Summary
	seen := map[string]bool{}
	for id := parentID; id != "" && len(chain) < ancestorDepth; {
		if seen[id] {
			// A CYCLE. Reported rather than looped: the chain so far is
			// still useful for a breadcrumb, and hanging the read would
			// take the page down with the bad parent.
			log.WarnContext(ctx, "pages_ancestor_cycle", "page", id,
				"detail", "a page's parent chain reaches itself; the breadcrumb "+
					"is truncated rather than walked forever")
			break
		}
		seen[id] = true
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
			return nil, fmt.Errorf("pages: walk the parent chain: %w", err)
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

// Containers is every container this node knows about.
//
// THE DOMAIN IS ITS SCOPE, because a container list is about all of them —
// which is exactly what [ReadScope] returns for a read that names none.
func (r *Reader) Containers(ctx context.Context, level statelog.ReadLevel) ([]Container, error) {
	if level == "" {
		return nil, errors.New("pages: this read names no level")
	}
	var out []Container
	_, err := r.log.Read(ctx, statelog.Query{
		Level: level, Scope: ReadScope("", ""), Set: true,
	}, func(tx *sql.Tx) error {
		var err error
		out, err = r.containers(ctx, tx)
		return err
	})
	return out, err
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
	level statelog.ReadLevel) ([]Page, error) {

	if level == "" {
		return nil, errors.New("pages: this read names no level")
	}
	container = strings.ToUpper(strings.TrimSpace(container))
	if container == "" {
		return nil, nil
	}
	var out []Page
	_, err := r.log.Read(ctx, statelog.Query{
		Level: level, Scope: ReadScope(container, ""), Set: true,
	}, func(tx *sql.Tx) error {
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

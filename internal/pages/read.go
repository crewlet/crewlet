package pages

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/projection"
	"github.com/crewlet/crewlet/internal/store"
)

// Reader answers questions about pages from the node's own projection.
type Reader struct {
	db       *store.DB
	hydrated func() bool
}

// ReaderOptions configure a reader.
type ReaderOptions struct {
	DB       *store.DB
	Hydrated func() bool
}

// NewReader builds the knowledge base's read side.
func NewReader(opts ReaderOptions) (*Reader, error) {
	if opts.DB == nil {
		return nil, errors.New("pages: a store is required")
	}
	r := &Reader{db: opts.DB, hydrated: opts.Hydrated}
	if r.hydrated == nil {
		r.hydrated = func() bool { return true }
	}
	return r, nil
}

func (r *Reader) ready() error {
	if !r.hydrated() {
		return projection.ErrNotHydrated
	}
	return nil
}

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

// List answers a filtered listing.
func (r *Reader) List(ctx context.Context, f Filter) ([]Summary, error) {
	if err := r.ready(); err != nil {
		return nil, err
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
			"EXISTS (SELECT 1 FROM page_labels l WHERE l.page_id = p.id AND l.label = ?)")
		args = append(args, f.Label)
	}
	if f.Watcher != "" {
		where = append(where,
			"EXISTS (SELECT 1 FROM page_watchers w WHERE w.page_id = p.id "+
				"AND w.handle = ? AND w.muted = 0)")
		args = append(args, f.Watcher)
	}
	if title := strings.TrimSpace(f.Title); title != "" {
		where = append(where, "p.title LIKE ?")
		args = append(args, "%"+title+"%")
	}
	if f.Skills != nil {
		if *f.Skills {
			where = append(where, "p.skill = 1")
		} else {
			where = append(where, "p.skill = 0")
		}
	}
	if f.Onboarding {
		where = append(where, "p.onboarding = 1")
	}

	limit := f.Limit
	if limit <= 0 {
		limit = DefaultLimit
	}
	if limit > MaxLimit {
		limit = MaxLimit
	}
	args = append(args, limit, max(f.Offset, 0))

	rows, err := r.db.SQL().QueryContext(ctx, `
		SELECT p.id, p.container, p.parent_id, p.title, p.status, p.author,
		       p.version, p.skill, p.onboarding, p.updated_at, p.revision
		  FROM pages p
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
	return out, r.attachLabels(ctx, out)
}

func (r *Reader) attachLabels(ctx context.Context, items []Summary) error {
	if len(items) == 0 {
		return nil
	}
	ids := make([]any, len(items))
	at := make(map[string]int, len(items))
	for i, item := range items {
		ids[i] = item.ID
		at[item.ID] = i
	}
	rows, err := r.db.SQL().QueryContext(ctx,
		`SELECT page_id, label FROM page_labels WHERE page_id IN (`+
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
func (r *Reader) Get(ctx context.Context, ref string) (Detail, error) {
	if err := r.ready(); err != nil {
		return Detail{}, err
	}
	document, revision, id, err := r.locate(ctx, ref)
	if err != nil {
		return Detail{}, err
	}
	page, err := DecodePage([]byte(document))
	if err != nil {
		return Detail{}, err
	}
	detail := Detail{Page: page, Revision: revision}
	if detail.Comments, err = r.comments(ctx, id); err != nil {
		return Detail{}, err
	}
	if detail.History, err = r.history(ctx, id); err != nil {
		return Detail{}, err
	}
	if detail.Children, err = r.List(ctx, Filter{ParentID: id}); err != nil {
		return Detail{}, err
	}
	if detail.Ancestors, err = r.ancestors(ctx, page.ParentID); err != nil {
		return Detail{}, err
	}
	return detail, nil
}

// locate resolves a reference to a page row.
func (r *Reader) locate(ctx context.Context, ref string) (document string, revision uint64, id string, err error) {
	var rev int64
	query := `SELECT id, document, revision FROM pages WHERE id = ?`
	args := []any{ref}
	if container, title, ok := strings.Cut(ref, "/"); ok {
		// "CONTAINER/Title", which is how a person and a model name a page
		// — the title is its address, and the container scopes it.
		query = `SELECT id, document, revision FROM pages
		          WHERE container = ? AND LOWER(title) = ?`
		args = []any{strings.ToUpper(strings.TrimSpace(container)), NormalizeTitle(title)}
	}
	err = r.db.SQL().QueryRowContext(ctx, query, args...).Scan(&id, &document, &rev)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return "", 0, "", fmt.Errorf("%w: page %s", ErrNotFound, ref)
	case err != nil:
		return "", 0, "", fmt.Errorf("pages: read %s: %w", ref, err)
	}
	return document, uint64(rev), id, nil
}

func (r *Reader) comments(ctx context.Context, pageID string) ([]Comment, error) {
	rows, err := r.db.SQL().QueryContext(ctx,
		`SELECT document FROM page_comments WHERE page_id = ? ORDER BY created_at, id`,
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

func (r *Reader) history(ctx context.Context, pageID string) ([]RevisionSummary, error) {
	rows, err := r.db.SQL().QueryContext(ctx,
		`SELECT version, author, message, created_at FROM page_revisions
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

func (r *Reader) ancestors(ctx context.Context, parentID string) ([]Summary, error) {
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
		err := r.db.SQL().QueryRowContext(ctx, `
			SELECT id, container, parent_id, title, status, author, version,
			       skill, onboarding, updated_at, revision
			  FROM pages WHERE id = ?`, id).
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
func (r *Reader) Containers(ctx context.Context) ([]Container, error) {
	if err := r.ready(); err != nil {
		return nil, err
	}
	rows, err := r.db.SQL().QueryContext(ctx,
		`SELECT document FROM page_containers ORDER BY key`)
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

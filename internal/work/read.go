package work

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

// Reader answers questions about work from the node's own projection.
//
// SEPARATE FROM [Store], and the split is the design: a write goes to
// coordination and a read comes from the local copy. Putting both on one type
// would make it far too easy to read the projection inside a
// read-decide-write, which is how a compare-and-set gets conditioned on a
// revision the node has not caught up to yet.
type Reader struct {
	db       *store.DB
	hydrated func() bool
}

// ReaderOptions configure a reader.
type ReaderOptions struct {
	DB *store.DB

	// Hydrated reports whether the projection has caught up. Nil answers
	// yes, which is right only for a caller that has established it some
	// other way — a test, or a process that runs the projector itself and
	// gates on it.
	Hydrated func() bool
}

// NewReader builds the tracker's read side.
func NewReader(opts ReaderOptions) (*Reader, error) {
	if opts.DB == nil {
		return nil, errors.New("work: a store is required")
	}
	r := &Reader{db: opts.DB, hydrated: opts.Hydrated}
	if r.hydrated == nil {
		r.hydrated = func() bool { return true }
	}
	return r, nil
}

// ready refuses a read the projection cannot answer.
//
// RAISES RATHER THAN ANSWERING EMPTY, on the projection's own rule: "this
// company has no work" is an answer a seat acts on — it files a duplicate, it
// tells a person their link is dead — and a projection that has not caught up
// must never be able to say it.
func (r *Reader) ready() error {
	if !r.hydrated() {
		return projection.ErrNotHydrated
	}
	return nil
}

// Filter narrows a listing.
type Filter struct {
	Project  string
	Status   []Status
	Assignee string
	Reporter string
	Label    string
	Parent   string

	// Watcher lists what one seat is following, muted excluded.
	Watcher string

	// Open narrows to items that are not terminal. A POINTER because all
	// three states are real: open work, closed work, and everything.
	Open *bool

	// Text is a substring match on the key and title. Deliberately NOT the
	// knowledge search: this is the "find the item I half remember" box,
	// and a ranked semantic answer there is worse than an exact prefix.
	Text string

	Limit  int
	Offset int
}

// DefaultLimit is how many items a listing returns when the caller says
// nothing.
//
// Fifty: a board's first screen, and small enough that a tool result stays
// inside a turn's context beside everything else it carries.
const DefaultLimit = 50

// MaxLimit bounds a listing however much a caller asks for.
//
// Five hundred. A tool result at that size is already past what a model reads
// usefully, and a dashboard page that wanted more would be paging anyway.
const MaxLimit = 500

// Summary is one item as a listing renders it — everything a board draws and
// nothing it does not.
//
// The BODY IS ABSENT deliberately: fifty items at 64 KiB each is three
// megabytes to draw a list of titles, and the one item a reader opens is one
// more query.
type Summary struct {
	ID       string     `json:"id"`
	Key      string     `json:"key"`
	Project  string     `json:"project"`
	Type     Type       `json:"type"`
	Title    string     `json:"title"`
	Status   Status     `json:"status"`
	Priority Priority   `json:"priority"`
	Assignee string     `json:"assignee,omitempty"`
	Reporter string     `json:"reporter,omitempty"`
	ParentID string     `json:"parent_id,omitempty"`
	Labels   []string   `json:"labels,omitempty"`
	Due      *time.Time `json:"due,omitempty"`
	Updated  time.Time  `json:"updated_at"`
	Revision uint64     `json:"revision"`
}

// List answers a filtered listing.
func (r *Reader) List(ctx context.Context, f Filter) ([]Summary, error) {
	if err := r.ready(); err != nil {
		return nil, err
	}
	where := []string{"1 = 1"}
	var args []any
	if f.Project != "" {
		where = append(where, "i.project = ?")
		args = append(args, strings.ToUpper(f.Project))
	}
	if len(f.Status) > 0 {
		marks := make([]string, len(f.Status))
		for i, s := range f.Status {
			marks[i] = "?"
			args = append(args, string(s))
		}
		where = append(where, "i.status IN ("+strings.Join(marks, ",")+")")
	}
	if f.Open != nil {
		terminal := "(?, ?)"
		if *f.Open {
			where = append(where, "i.status NOT IN "+terminal)
		} else {
			where = append(where, "i.status IN "+terminal)
		}
		args = append(args, string(StatusDone), string(StatusCancelled))
	}
	if f.Assignee != "" {
		where = append(where, "i.assignee = ?")
		args = append(args, f.Assignee)
	}
	if f.Reporter != "" {
		where = append(where, "i.reporter = ?")
		args = append(args, f.Reporter)
	}
	if f.Parent != "" {
		where = append(where, "i.parent_id = ?")
		args = append(args, f.Parent)
	}
	if f.Label != "" {
		where = append(where,
			"EXISTS (SELECT 1 FROM work_labels l WHERE l.item_id = i.id AND l.label = ?)")
		args = append(args, f.Label)
	}
	if f.Watcher != "" {
		// MUTED EXCLUDED, because a mute is an explicit "stop showing me
		// this" and a list that included it would make the unwatch look
		// like it did nothing.
		where = append(where,
			"EXISTS (SELECT 1 FROM work_watchers w WHERE w.item_id = i.id "+
				"AND w.handle = ? AND w.muted = 0)")
		args = append(args, f.Watcher)
	}
	if text := strings.TrimSpace(f.Text); text != "" {
		where = append(where, "(i.item_key LIKE ? OR i.title LIKE ?)")
		like := "%" + text + "%"
		args = append(args, like, like)
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
		SELECT i.id, i.item_key, i.project, i.type, i.title, i.status,
		       i.priority, i.assignee, i.reporter, i.parent_id, i.due_at,
		       i.updated_at, i.revision
		  FROM work_items i
		 WHERE `+strings.Join(where, " AND ")+`
		 ORDER BY i.updated_at DESC, i.item_key
		 LIMIT ? OFFSET ?`, args...)
	if err != nil {
		return nil, fmt.Errorf("work: list items: %w", err)
	}
	defer rows.Close()

	var out []Summary
	for rows.Next() {
		var (
			s        Summary
			due      sql.NullInt64
			updated  int64
			revision int64
		)
		if err := rows.Scan(&s.ID, &s.Key, &s.Project, &s.Type, &s.Title, &s.Status,
			&s.Priority, &s.Assignee, &s.Reporter, &s.ParentID, &due,
			&updated, &revision); err != nil {
			return nil, fmt.Errorf("work: scan item: %w", err)
		}
		if due.Valid {
			at := store.DecodeTime(due.Int64)
			s.Due = &at
		}
		s.Updated = store.DecodeTime(updated)
		s.Revision = uint64(revision)
		out = append(out, s)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("work: list items: %w", err)
	}
	if err := r.attachLabels(ctx, out); err != nil {
		return nil, err
	}
	return out, nil
}

// attachLabels fills a listing's labels in ONE query.
//
// Rather than one per item: a fifty-item board would otherwise be fifty-one
// round trips to a single-writer database, which is the shape that makes a
// list feel slow for no reason a profile would obviously show.
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
		`SELECT item_id, label FROM work_labels WHERE item_id IN (`+
			placeholders(len(ids))+`) ORDER BY label`, ids...)
	if err != nil {
		return fmt.Errorf("work: read labels: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id, label string
		if err := rows.Scan(&id, &label); err != nil {
			return fmt.Errorf("work: scan label: %w", err)
		}
		if i, ok := at[id]; ok {
			items[i].Labels = append(items[i].Labels, label)
		}
	}
	return rows.Err()
}

// Detail is one item with everything a reader opening it wants.
type Detail struct {
	Item     Item      `json:"item"`
	Revision uint64    `json:"revision"`
	Comments []Comment `json:"comments,omitempty"`
	History  []Change  `json:"history,omitempty"`

	// Links are both directions, so a reader sees "blocks" and "blocked
	// by" without a second query and without knowing which end authored
	// which.
	Links []DetailLink `json:"links,omitempty"`
}

// DetailLink is one link as a reader sees it.
type DetailLink struct {
	Kind    LinkKind `json:"kind"`
	OtherID string   `json:"other_id"`
	Key     string   `json:"key,omitempty"`
	Title   string   `json:"title,omitempty"`
	Status  Status   `json:"status,omitempty"`

	// Derived marks the half nobody authored, so a UI can render it
	// differently and an editor knows which end to change.
	Derived bool `json:"derived,omitempty"`
}

// Get reads one item by id or by key.
//
// BY EITHER, because the two are used by different callers and neither should
// have to translate: a person and a model both hold the key, while every
// internal reference holds the id.
func (r *Reader) Get(ctx context.Context, idOrKey string) (Detail, error) {
	if err := r.ready(); err != nil {
		return Detail{}, err
	}
	var (
		document string
		revision int64
		id       string
	)
	err := r.db.SQL().QueryRowContext(ctx, `
		SELECT id, document, revision FROM work_items
		 WHERE id = ? OR item_key = ?`, idOrKey, strings.ToUpper(idOrKey)).
		Scan(&id, &document, &revision)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return Detail{}, fmt.Errorf("%w: item %s", ErrNotFound, idOrKey)
	case err != nil:
		return Detail{}, fmt.Errorf("work: read %s: %w", idOrKey, err)
	}
	item, err := DecodeItem([]byte(document))
	if err != nil {
		return Detail{}, err
	}
	detail := Detail{Item: item, Revision: uint64(revision)}
	if detail.Comments, err = r.comments(ctx, id); err != nil {
		return Detail{}, err
	}
	if detail.History, err = r.history(ctx, id); err != nil {
		return Detail{}, err
	}
	if detail.Links, err = r.links(ctx, id); err != nil {
		return Detail{}, err
	}
	return detail, nil
}

func (r *Reader) comments(ctx context.Context, itemID string) ([]Comment, error) {
	rows, err := r.db.SQL().QueryContext(ctx,
		`SELECT document FROM work_comments WHERE item_id = ? ORDER BY created_at, id`,
		itemID)
	if err != nil {
		return nil, fmt.Errorf("work: read the thread on %s: %w", itemID, err)
	}
	defer rows.Close()
	var out []Comment
	for rows.Next() {
		var document string
		if err := rows.Scan(&document); err != nil {
			return nil, fmt.Errorf("work: scan comment: %w", err)
		}
		comment, err := DecodeComment([]byte(document))
		if err != nil {
			// A comment a newer build wrote. SKIPPED rather than fatal: a
			// rolling upgrade must not make a whole thread unreadable on
			// the older half.
			log.WarnContext(ctx, "work_comment_unreadable", "item", itemID,
				"error", err.Error())
			continue
		}
		out = append(out, comment)
	}
	return out, rows.Err()
}

func (r *Reader) history(ctx context.Context, itemID string) ([]Change, error) {
	rows, err := r.db.SQL().QueryContext(ctx,
		`SELECT document FROM work_history WHERE item_id = ? ORDER BY created_at DESC, id DESC
		 LIMIT ?`, itemID, historyLimit)
	if err != nil {
		return nil, fmt.Errorf("work: read the history of %s: %w", itemID, err)
	}
	defer rows.Close()
	var out []Change
	for rows.Next() {
		var document string
		if err := rows.Scan(&document); err != nil {
			return nil, fmt.Errorf("work: scan change: %w", err)
		}
		change, err := DecodeChange([]byte(document))
		if err != nil {
			log.WarnContext(ctx, "work_change_unreadable", "item", itemID,
				"error", err.Error())
			continue
		}
		out = append(out, change)
	}
	return out, rows.Err()
}

// historyLimit bounds how much of an item's record one read returns.
//
// A hundred entries, newest first. An item worked for a year has thousands,
// and a tool result carrying all of them is a turn's whole context spent on
// an audit trail nobody asked for — the full record is still there for a
// caller that pages.
const historyLimit = 100

func (r *Reader) links(ctx context.Context, itemID string) ([]DetailLink, error) {
	rows, err := r.db.SQL().QueryContext(ctx, `
		SELECT l.other_id, l.kind, l.derived,
		       COALESCE(o.item_key, ''), COALESCE(o.title, ''), COALESCE(o.status, '')
		  FROM work_links l
		  LEFT JOIN work_items o ON o.id = l.other_id
		 WHERE l.item_id = ?
		 ORDER BY l.kind, o.item_key`, itemID)
	if err != nil {
		return nil, fmt.Errorf("work: read the links on %s: %w", itemID, err)
	}
	defer rows.Close()
	var out []DetailLink
	for rows.Next() {
		var link DetailLink
		var derived int
		if err := rows.Scan(&link.OtherID, &link.Kind, &derived,
			&link.Key, &link.Title, &link.Status); err != nil {
			return nil, fmt.Errorf("work: scan link: %w", err)
		}
		link.Derived = derived != 0
		out = append(out, link)
	}
	return out, rows.Err()
}

// Counters is the last number minted per project, for a board's header.
func (r *Reader) Counters(ctx context.Context) (map[string]int, error) {
	if err := r.ready(); err != nil {
		return nil, err
	}
	rows, err := r.db.SQL().QueryContext(ctx, `SELECT project, last FROM work_counters`)
	if err != nil {
		return nil, fmt.Errorf("work: read counters: %w", err)
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var project string
		var last int
		if err := rows.Scan(&project, &last); err != nil {
			return nil, fmt.Errorf("work: scan counter: %w", err)
		}
		out[project] = last
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

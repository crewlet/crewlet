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

// What happened to the company's pages, and what one revision actually said.
//
// # The rows were always here
//
// `pages_history` has one row per change since the domain landed — the kind,
// who made it, whether it was quiet, an excerpt, and the TURN that made it —
// and the schema ships two indexes naming readers that were never written:
// `pages_history_page_idx` "one page's activity" and `pages_history_created_idx`
// "the company-wide digest". Nothing read a single row of either. A wiki that
// recorded every edit, replicated it, snapshotted it, and offered no way to
// ask what had changed.
//
// # And a revision list without bodies is not a history
//
// The page detail carries revision SUMMARIES — a version, an author, a message
// and an instant — which says that a page was edited eleven times and not what
// any of those edits did. The bodies are in `pages_revisions` and were
// reachable only by reading the page at its head. So "what changed in version
// 7" had no answer, and neither did the diff every wiki is expected to show.
//
// # Why they are two reads and not one
//
// A body is the largest column this domain has, and an activity feed is a
// LIST. Returning bodies with the feed would ship every revision of every page
// somebody scrolled past; returning one on demand is a primary-key seek.

// PageChange is one thing that happened to a page.
type PageChange struct {
	ID     string `json:"id"`
	PageID string `json:"page_id"`

	// Title and Container are resolved by the read rather than stored on
	// the row, because a feed of uuids is a feed nobody reads — and they
	// are the page's CURRENT ones, which is the honest answer: a renamed
	// page's old entries belong to the page it is now.
	Title     string `json:"title,omitempty"`
	Container string `json:"container,omitempty"`

	Kind      ChangeKind `json:"kind"`
	Actor     string     `json:"actor,omitempty"`
	ActorKind string     `json:"actor_kind,omitempty"`

	// OperatorID names the person when the actor was an operator's own
	// assistant rather than a seat, which is the one case where the actor
	// alone does not say who.
	OperatorID string `json:"operator_id,omitempty"`

	CommentID string `json:"comment_id,omitempty"`
	Excerpt   string `json:"excerpt,omitempty"`

	// TurnID is what makes a page edit traceable to the work that caused
	// it — the field a wiki cannot have and this one records on every
	// change a seat makes.
	TurnID string `json:"turn_id,omitempty"`

	// Quiet marks a change that announced nothing. A fact about the change
	// rather than its importance: a label edit is quiet by construction.
	Quiet bool `json:"quiet,omitempty"`

	// At is the AUTHORED instant, which is what a card renders.
	At time.Time `json:"at"`

	// LogSeq is the composed position, and the cursor a page walks on.
	LogSeq uint64 `json:"log_seq"`
}

// PageActivity is a page of the change feed.
type PageActivity struct {
	Changes    []PageChange `json:"changes"`
	NextCursor string       `json:"next_cursor,omitempty"`

	Level          statelog.ReadLevel `json:"read_level"`
	LogSeq         uint64             `json:"log_seq"`
	AppliedThrough uint64             `json:"applied_through"`
	Complete       bool               `json:"complete"`
}

// MaxPageChanges is how many changes one page of the feed carries.
//
// ONE HUNDRED, half the tracker's activity feed and twice its inbox, because
// this list sits between them: it is SCROLLED like a feed rather than worked
// like an inbox, and a wiki generates far fewer changes than a tracker — a
// page is saved a handful of times where a task moves through six statuses,
// four assignees and a thread.
const MaxPageChanges = 100

// PageActivityQuery asks for a page of the feed.
type PageActivityQuery struct {
	// Page narrows to one page's own history. Empty is the company-wide
	// digest, which is the other index this table ships.
	Page string

	// Container narrows to one space. Composes with neither Page (which is
	// already in exactly one container) nor a broad scan: it is the middle
	// question, "what has this team been writing".
	Container string

	// Kinds narrows to particular changes. Empty is every kind.
	Kinds []ChangeKind

	// ActorKinds narrows to who was WRITING rather than to which handle —
	// every page change an operator token made, say. Empty is every kind.
	// The tracker's own feed carries the same filter for the same reason,
	// and one audit surface reads both.
	ActorKinds []AuthorKind

	// Since is a lower bound as a composed log position.
	Since uint64

	// Cursor resumes strictly before a position, which is what makes the
	// feed newest-first and pageable without an offset.
	Cursor uint64

	Limit int

	Freshness statelog.Freshness
}

// Activity answers a page of what has happened to the company's pages.
func (r *Reader) Activity(ctx context.Context, q PageActivityQuery) (PageActivity, error) {
	if q.Freshness.Level == "" {
		return PageActivity{}, fmt.Errorf("pages: this activity read names no " +
			"level — a surface resolves an absent read_level to its own " +
			"default before it reads")
	}
	for _, kind := range q.Kinds {
		if !kind.Valid() {
			return PageActivity{}, fmt.Errorf("pages: %q is not a change kind", kind)
		}
	}
	for _, kind := range q.ActorKinds {
		if !kind.Valid() {
			return PageActivity{}, fmt.Errorf("pages: %q is not an author kind — "+
				"the three are %v", kind, AuthorKinds())
		}
	}
	limit := q.Limit
	if limit <= 0 || limit > MaxPageChanges {
		limit = MaxPageChanges
	}

	out := PageActivity{Changes: []PageChange{}}
	served, err := r.log.Read(ctx,
		q.Freshness.Query(ReadScope(q.Container), true),
		func(tx *sql.Tx) error {
			changes, next, err := readPageActivity(ctx, tx, q, limit)
			if err != nil {
				return err
			}
			out.Changes, out.NextCursor = changes, next
			return nil
		})
	if err != nil {
		return PageActivity{}, err
	}
	out.Level = served.Level
	out.Complete = served.Complete
	return out, nil
}

// readPageActivity reads one page of the change feed.
//
// THE JOIN IS LEFT, for the reason the tracker's inbox gives about the same
// shape: a history entry outlives the page it names — a purge destroys the
// head and the entries are what say it ever existed — and a feed that dropped
// those rows would lose exactly the changes somebody is looking for.
func readPageActivity(ctx context.Context, tx *sql.Tx, q PageActivityQuery,
	limit int) ([]PageChange, string, error) {

	where := []string{"1 = 1"}
	var args []any
	if q.Page != "" {
		where = append(where, "h.page_id = ?")
		args = append(args, q.Page)
	}
	if q.Container != "" {
		where = append(where, "p.container = ?")
		args = append(args, strings.ToUpper(q.Container))
	}
	if len(q.Kinds) > 0 {
		marks := make([]string, len(q.Kinds))
		for i, kind := range q.Kinds {
			marks[i] = "?"
			args = append(args, string(kind))
		}
		where = append(where, "h.kind IN ("+strings.Join(marks, ",")+")")
	}
	if len(q.ActorKinds) > 0 {
		// `(actor_kind, version DESC)` — migration 0012 — which is the
		// feed's own order, so one kind is a range on the index rather
		// than a sort over every page change the company has made.
		marks := make([]string, len(q.ActorKinds))
		for i, kind := range q.ActorKinds {
			marks[i] = "?"
			args = append(args, string(kind))
		}
		where = append(where, "h.actor_kind IN ("+strings.Join(marks, ",")+")")
	}
	if q.Since > 0 {
		where = append(where, "h.version > ?")
		args = append(args, int64(q.Since))
	}
	if q.Cursor > 0 {
		// STRICTLY BEFORE: the feed is newest-first, so the cursor names
		// the last row of the previous page.
		where = append(where, "h.version < ?")
		args = append(args, int64(q.Cursor))
	}
	args = append(args, limit+1)

	rows, err := tx.QueryContext(ctx, `
		SELECT h.id, h.page_id, h.kind, h.actor, h.actor_kind, h.operator_id,
		       h.comment_id, h.excerpt, h.turn_id, h.quiet, h.created_at,
		       h.version, COALESCE(p.title, ''), COALESCE(p.container, '')
		  FROM pages_history h
		  LEFT JOIN pages_heads p ON p.id = h.page_id
		 WHERE `+strings.Join(where, " AND ")+`
		 ORDER BY h.version DESC
		 LIMIT ?`, args...)
	if err != nil {
		return nil, "", fmt.Errorf("pages: read the change feed: %w", err)
	}
	defer rows.Close()

	out := make([]PageChange, 0, limit)
	more := false
	for rows.Next() {
		if len(out) == limit {
			// THE EXTRA ROW IS THE CURSOR'S EVIDENCE and never an
			// answer: a page that returned it would overrun the limit.
			more = true
			break
		}
		var change PageChange
		var kind string
		var quiet int
		var at, version int64
		if err := rows.Scan(&change.ID, &change.PageID, &kind, &change.Actor,
			&change.ActorKind, &change.OperatorID, &change.CommentID,
			&change.Excerpt, &change.TurnID, &quiet, &at, &version,
			&change.Title, &change.Container); err != nil {

			return nil, "", fmt.Errorf("pages: scan a change: %w", err)
		}
		change.Kind = ChangeKind(kind)
		change.Quiet = quiet != 0
		change.At = store.DecodeTime(at)
		change.LogSeq = uint64(version)
		out = append(out, change)
	}
	if err := rows.Err(); err != nil {
		return nil, "", fmt.Errorf("pages: read the change feed: %w", err)
	}

	var next string
	if more && len(out) > 0 {
		next = fmt.Sprint(out[len(out)-1].LogSeq)
	}
	return out, next, nil
}

// Revision answers one saved version of one page, body included.
//
// THE BODY IS THE POINT. The page detail already carries revision SUMMARIES —
// a version, an author, a message, an instant — which says a page was edited
// eleven times and not what any of those edits did. The bodies were reachable
// only by reading the page at its head, so "what changed in version 7" had no
// answer and neither did the diff a wiki is expected to show.
//
// REVISION N IS THE BODY AT VERSION N, including the newest — see this
// package's own doc. The other reading, that a revision holds what was
// replaced, collides with itself on the first save.
//
// THREE-VALUED, in this tree's own shape for the question: the revision, or
// "no such version", or the read failed. A page keeps [RevisionsKept]
// versions, so an older one is an ORDINARY absence rather than a fault — and
// a caller has to be able to tell it from a page that never existed.
//
// [Revision.ID] and [Revision.V] are empty here, and deliberately: the row is
// COLUMNS rather than a stored document, so what this answers is what the
// applier wrote into them. A revision has no other identity than its page and
// its number.
func (r *Reader) Revision(ctx context.Context, pageID string, version int,
	fresh statelog.Freshness) (Revision, bool, error) {

	if fresh.Level == "" {
		return Revision{}, false, fmt.Errorf("pages: this revision read names no level")
	}
	pageID = strings.TrimSpace(pageID)
	switch {
	case pageID == "":
		return Revision{}, false, fmt.Errorf("pages: a revision read names no page")
	case version <= 0:
		return Revision{}, false, fmt.Errorf("pages: a revision is numbered from "+
			"1, and %d is not a version", version)
	}

	out := Revision{PageID: pageID, Version: version}
	held := false
	_, err := r.log.Read(ctx, fresh.Query(ReadScope(""), false), func(tx *sql.Tx) error {
		var at int64
		err := tx.QueryRowContext(ctx, `
			SELECT title, body, message, author, created_at
			  FROM pages_revisions
			 WHERE page_id = ? AND edit_version = ?`, pageID, version).
			Scan(&out.Title, &out.Body, &out.Message, &out.Author, &at)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			return nil
		case err != nil:
			return fmt.Errorf("pages: read %s at version %d: %w", pageID, version, err)
		}
		held = true
		out.CreatedAt = store.DecodeTime(at)
		return nil
	})
	if err != nil {
		return Revision{}, false, err
	}
	return out, held, nil
}

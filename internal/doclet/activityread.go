package doclet

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/store"
	"github.com/crewlet/crewlet/internal/words"
)

// What happened to the company's pages, and what one revision said.
//
// The feed reads `pages_history` (one row per change: kind, actor, quiet flag,
// excerpt, turn) in LOG order: the company's history through
// `pages_history_version_idx`, one page's through `pages_history_page_idx`.
//
// The page detail carries revision SUMMARIES only; the bodies in
// `pages_revisions` answer "what changed in version 7" and feed a diff. They
// are a second read, not part of the feed: a body is this domain's largest
// column and a feed is a LIST, so bodies with it would ship every revision of
// every page scrolled past, where one on demand is a primary-key seek.

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

	// Status is the page's CURRENT status, read with its title: a page in
	// the trash keeps its title, so without it a reader leading with the
	// newest change leads with a page in the trash.
	Status Status `json:"status,omitempty"`

	Kind      ChangeKind `json:"kind"`
	Actor     string     `json:"actor,omitempty"`
	ActorKind string     `json:"actor_kind,omitempty"`

	// OperatorID is the CREDENTIAL the change was made through, recorded
	// beside the actor rather than instead of it — a machine token
	// (`pat:<id>`), a browser session, or a credential's own login, as
	// [iam.ActorFor] records it — so "what did this credential do" is a
	// question the feed answers without reasoning about kinds. The actor
	// says WHO: for a person bound to a seat that is the seat, and the
	// credential is the one thing that tells their own write from their
	// assistant's.
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

	Level statelog.ReadLevel `json:"read_level"`

	// LogSeq is this node's checkpoint on the pages log and AppliedThrough
	// the prefix of it whose records produced their rows here, both
	// composed positions, read in the transaction that read the changes.
	//
	// TWO NUMBERS, for the tracker feed's reason: the checkpoint moves past
	// a record this node RETAINS — one this build cannot decode — so a node
	// holding one reads as caught up by its position alone, and only the
	// applied prefix stopping below it says otherwise. Both were declared
	// here and never filled, so every answer said zero and zero: a node
	// behind on the log was indistinguishable from one at its head, and the
	// audit, which names a source whose answer is behind, could not.
	LogSeq         uint64 `json:"log_seq"`
	AppliedThrough uint64 `json:"applied_through"`
	Complete       bool   `json:"complete"`
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

	// Actor narrows to one writer's changes — the company feed's `actor=`,
	// a seat by its handle.
	Actor string

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
		return PageActivity{}, fmt.Errorf("doclet: this activity read names no " +
			"level — a surface resolves an absent read_level to its own " +
			"default before it reads")
	}
	for _, kind := range q.Kinds {
		if !kind.Valid() {
			return PageActivity{}, fmt.Errorf("doclet: %q is not a change kind", kind)
		}
	}
	for _, kind := range q.ActorKinds {
		if !kind.Valid() {
			return PageActivity{}, fmt.Errorf("doclet: %q is not an author kind; "+
				"the three are %s", kind, words.And(AuthorKinds()))
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
			// IN THE SAME TRANSACTION as the changes, so the position
			// describes exactly the rows this page was read from.
			prefix, err := statelog.PrefixIn(ctx, tx, Domain{})
			if err != nil {
				return err
			}
			out.LogSeq = uint64(prefix.Settled.Packed())
			out.AppliedThrough = uint64(prefix.Applied().Packed())
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
func readPageActivity(ctx context.Context, tx *sql.Tx, q PageActivityQuery,
	limit int) ([]PageChange, string, error) {

	statement, args := pageActivityStatement(compilePageActivity(q), limit+1)
	rows, err := tx.QueryContext(ctx, statement, args...)
	if err != nil {
		return nil, "", fmt.Errorf("doclet: read the change feed: %w", err)
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
		var kind, status string
		var quiet int
		var at, version int64
		if err := rows.Scan(&change.ID, &change.PageID, &kind, &change.Actor,
			&change.ActorKind, &change.OperatorID, &change.CommentID,
			&change.Excerpt, &change.TurnID, &quiet, &at, &version,
			&change.Title, &change.Container, &status); err != nil {

			return nil, "", fmt.Errorf("doclet: scan a change: %w", err)
		}
		change.Kind = ChangeKind(kind)
		change.Status = Status(status)
		change.Quiet = quiet != 0
		change.At = store.DecodeTime(at)
		change.LogSeq = uint64(version)
		out = append(out, change)
	}
	if err := rows.Err(); err != nil {
		return nil, "", fmt.Errorf("doclet: read the change feed: %w", err)
	}

	var next string
	if more && len(out) > 0 {
		next = fmt.Sprint(out[len(out)-1].LogSeq)
	}
	return out, next, nil
}

// pageActivityPredicate is the feed's compiled filter: the clauses every
// change must meet with their arguments, and the author kinds it is narrowed
// to, which [pageActivityStatement] reads one arm at a time.
type pageActivityPredicate struct {
	where      []string
	args       []any
	actorKinds []any
}

// compilePageActivity turns the query into a predicate.
func compilePageActivity(q PageActivityQuery) pageActivityPredicate {
	var p pageActivityPredicate
	add := func(clause string, values ...any) {
		p.where = append(p.where, clause)
		p.args = append(p.args, values...)
	}
	if q.Page != "" {
		add("h.page_id = ?", q.Page)
	}
	if q.Container != "" {
		add("p.container = ?", ContainerKey(q.Container))
	}
	if len(q.Kinds) > 0 {
		marks := make([]string, len(q.Kinds))
		values := make([]any, len(q.Kinds))
		for i, kind := range q.Kinds {
			marks[i], values[i] = "?", string(kind)
		}
		add("h.kind IN ("+strings.Join(marks, ",")+")", values...)
	}
	// THE AUTHOR KINDS ARE THE STATEMENT'S ARMS, not a clause — see
	// [pageActivityStatement]. Each kind once, or its changes would be on
	// the page twice.
	for _, kind := range q.ActorKinds {
		if !slices.Contains(p.actorKinds, any(string(kind))) {
			p.actorKinds = append(p.actorKinds, string(kind))
		}
	}
	if actor := strings.TrimSpace(q.Actor); actor != "" {
		add("h.actor = ?", actor)
	}
	if q.Since > 0 {
		add("h.version > ?", int64(q.Since))
	}
	if q.Cursor > 0 {
		// STRICTLY BEFORE: the feed is newest-first, so the cursor names
		// the last row of the previous page.
		add("h.version < ?", int64(q.Cursor))
	}
	return p
}

// pageActivityStatement is the feed's one statement over a predicate, reading
// `rows` changes, and its arguments, named for the plan test.
//
// THE JOIN IS LEFT: an entry outlives its page (a purge destroys the head).
//
// ONE ARM PER AUTHOR KIND, merged: `pages_history_actor_kind_version_idx` is
// `(actor_kind, version DESC)`, the feed's order, so ONE kind is a range a
// LIMIT stops after a page. `actor_kind IN (…)`, even of one value, is seeked
// per value and sorted whole (every change any person or token ever made). The
// arms merge under the page's order and limit, each carrying the join because
// the container is a term of the head.
func pageActivityStatement(p pageActivityPredicate, rows int) (string, []any) {
	arm := func(extra ...string) string {
		where := append(append([]string{"1 = 1"}, p.where...), extra...)
		return `
		SELECT ` + pageChangeColumns + `
		  FROM pages_history h
		  LEFT JOIN pages_heads p ON p.id = h.page_id
		 WHERE ` + strings.Join(where, " AND ") + `
		 ORDER BY h.version DESC
		 LIMIT ?`
	}
	switch len(p.actorKinds) {
	case 0:
		return arm(), append(append([]any{}, p.args...), rows)
	case 1:
		return arm("h.actor_kind = ?"),
			append(append(append([]any{}, p.args...), p.actorKinds[0]), rows)
	}
	arms := make([]string, len(p.actorKinds))
	var args []any
	for i, kind := range p.actorKinds {
		arms[i] = "SELECT * FROM (" + arm("h.actor_kind = ?") + ")"
		args = append(append(append(args, p.args...), kind), rows)
	}
	// THE MERGED ROWS ARE NAMED `h`, and the head's three columns by the
	// names every arm gave them, so the merge reads the same columns an arm
	// returns — and orders by the change's version, as an arm does.
	return `
		SELECT h.id, h.page_id, h.kind, h.actor, h.actor_kind, h.operator_id,
		       h.comment_id, h.excerpt, h.turn_id, h.quiet, h.created_at,
		       h.version, h.title, h.container, h.status
		  FROM (` + strings.Join(arms, `
		 UNION ALL `) + `) h
		 ORDER BY h.version DESC
		 LIMIT ?`, append(args, rows)
}

// pageChangeColumns is every column a change is read with — the change's own
// under `h` and its head's title, container and status under `p`, absent for
// a page since purged.
const pageChangeColumns = `h.id, h.page_id, h.kind, h.actor, h.actor_kind, h.operator_id,
		       h.comment_id, h.excerpt, h.turn_id, h.quiet, h.created_at,
		       h.version, COALESCE(p.title, '') AS title,
		       COALESCE(p.container, '') AS container,
		       COALESCE(p.status, '') AS status`

// Revision answers one saved version of one page, body included (the page
// detail carries summaries only).
//
// REVISION N IS THE BODY AT VERSION N, including the newest (this package's
// doc); the other reading, what was replaced, collides on the first save.
//
// THREE-VALUED: the revision, "no such version", or the read failed. A page
// keeps [RevisionsKept] versions, so an older one is an ORDINARY absence, to be
// told from a page that never existed.
//
// [Revision.ID] and [Revision.V] stay empty: the row is COLUMNS, not a stored
// document, and a revision has no identity but its page and number.
func (r *Reader) Revision(ctx context.Context, pageID string, version int,
	fresh statelog.Freshness) (Revision, bool, error) {

	if fresh.Level == "" {
		return Revision{}, false, fmt.Errorf("doclet: this revision read names no level")
	}
	pageID = strings.TrimSpace(pageID)
	switch {
	case pageID == "":
		return Revision{}, false, fmt.Errorf("doclet: a revision read names no page")
	case version <= 0:
		return Revision{}, false, fmt.Errorf("doclet: a revision is numbered from "+
			"1, and %d is not a version", version)
	}

	out := Revision{PageID: pageID, Version: version}
	held := false
	_, err := r.log.Read(ctx, fresh.Query(ReadScope(""), false), func(tx *sql.Tx) error {
		var at int64
		err := tx.QueryRowContext(ctx, revisionStatement, pageID, version).
			Scan(&out.Title, &out.Body, &out.Message, &out.Author, &at)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			return nil
		case err != nil:
			return fmt.Errorf("doclet: read %s at version %d: %w", pageID, version, err)
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

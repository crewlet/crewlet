package search

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
)

// Backlinks is which pages and tasks link to one page: a page's "linked from".
// The links are rows of THIS NODE's index (`page_links`, node migration 0036)
// that [Indexer.Upsert] writes through `doclet.Links`' grammar, so they lag by
// a lap like a search. Until the first lap over both corpora finishes,
// [Indexer.LinkedFrom] refuses with [ErrIndexBuilding]: an empty list there
// means the linking body is unread, not that nothing links here.
//
// Titles, keys and statuses are NAMED from the replicated rows, never the
// index, which never re-reads a title for a rename: a second transaction, the
// one read here crossing the estate boundary, hence a batch from each side, not
// a join. A source the rows no longer publish (trashed, removed) is dropped.
type Backlinks struct {
	Pages []PageLink `json:"pages"`
	Tasks []TaskLink `json:"tasks"`

	// PagesTotal and TasksTotal are how many of each link here, of which the
	// lists carry the first [BacklinksLimit] by title and by key — a page
	// linked from four hundred tasks says four hundred rather than drawing
	// fifty as all of them.
	PagesTotal int `json:"pages_total"`
	TasksTotal int `json:"tasks_total"`
}

// PageLink is one page that links here.
type PageLink struct {
	ID        string `json:"id"`
	Container string `json:"container"`
	Title     string `json:"title"`
}

// TaskLink is one work item that links here, by one of two ways: its
// DESCRIPTION carries the address (the index finds it), or it names the page as
// a LINKED PAGE (`update_work_item{linked_pages}`, a `tracker_relations` row of
// kind `page`, read in the transaction that reads the names). A list knowing
// only one way would say nothing links here about a page a task was filed
// against. A task linked both ways is listed once, both named.
type TaskLink struct {
	ID  string `json:"id"`
	Key string `json:"key"`

	// KeyCollision marks a task holding a key ANOTHER task claimed first. The
	// tracker answers a key with the claimant, so a link built from
	// [TaskLink.Key] would open the wrong task; a reader uses [TaskLink.ID]
	// (the tracker's `ItemAddress` rule, mirrored by the dashboard).
	//
	// THE TRACKER'S OWN COLUMN, read, not re-derived: the applier derives
	// `key_collision` on every apply, so it is the same on every node. Deriving
	// it from `tracker_task_keys` here would be a second copy free to drift.
	KeyCollision bool `json:"key_collision,omitempty"`

	Title  string `json:"title"`
	Status string `json:"status"`

	// Via is how: [TaskLinkedPage] (the task names this page as one of its
	// linked pages), [TaskCitesPage] (its description carries the address),
	// or both.
	Via []string `json:"via"`
}

// The two ways a task links a page.
const (
	TaskLinkedPage = "linked_page"
	TaskCitesPage  = "description"
)

// BacklinksLimit is how many of each kind one answer names.
//
// Fifty: a page's rail lists them one line each, and a runbook cited by more
// than fifty tasks is one whose list is read by its count and its most recent
// few — the totals beside the lists say how many there are.
const BacklinksLimit = 50

// ErrIndexBuilding is [Indexer.LinkedFrom]'s answer on a node whose index has
// not finished its first lap over pages and tasks — a fresh or joined node for
// its first minutes, and a node that just upgraded past an [IndexDerivation]
// while its rows are re-derived.
//
// AN ERROR RATHER THAN AN EMPTY ANSWER, because the empty answer is a claim:
// "no page or task links here" is true only of an index that has read every
// body, and a caller handed empty lists with a flag beside them can drop the
// flag and keep the claim. The same reason [Slice.Building] exists for a
// search range.
var ErrIndexBuilding = errors.New("search: this node's index has not finished its first " +
	"build, so it cannot yet say what links to a page")

// linkedFromStatement is every document linking to one page, by the page's
// lowered id: named for the lexical plan gate, which holds every reader of
// kb_docs to the plan it runs under ([lexicalCorpusStatement]).
const linkedFromStatement = `
		SELECT d.source, d.source_id
		  FROM page_links l
		  JOIN kb_docs d ON d.id = l.doc_id
		 WHERE l.target_id = ?`

// LinkedFrom answers which pages and tasks link to one page, by the page's id,
// or [ErrIndexBuilding] while this node's index is still on its first lap.
func (x *Indexer) LinkedFrom(ctx context.Context, pageID string) (Backlinks, error) {
	out := Backlinks{Pages: []PageLink{}, Tasks: []TaskLink{}}
	// BOTH CORPORA: a page is linked from a page body and from a task's
	// description, so either one unread leaves a list short.
	if !x.ReadyFor(string(SourcePage), string(SourceTask)) {
		return out, ErrIndexBuilding
	}
	target := strings.ToLower(strings.TrimSpace(pageID))
	if target == "" {
		return out, nil
	}
	rows, err := x.db.SQL().QueryContext(ctx, linkedFromStatement, target)
	if err != nil {
		return out, fmt.Errorf("search: read the links to page %s: %w", pageID, err)
	}
	var pageIDs, taskIDs []string
	for rows.Next() {
		var source, id string
		if err = rows.Scan(&source, &id); err != nil {
			_ = rows.Close()
			return out, fmt.Errorf("search: scan a link to page %s: %w", pageID, err)
		}
		switch Source(source) {
		case SourcePage:
			pageIDs = append(pageIDs, id)
		case SourceTask:
			taskIDs = append(taskIDs, id)
		}
	}
	if err = rows.Err(); err != nil {
		_ = rows.Close()
		return out, fmt.Errorf("search: read the links to page %s: %w", pageID, err)
	}
	_ = rows.Close()
	// THROUGH THE HANDLE, for [Indexer.staleIn]'s reason: a replicated
	// estate that is not open answers [store.ErrNoEstate] rather than a nil
	// pool that panics.
	err = x.estate.Read(ctx, func(tx *sql.Tx) error {
		related, readErr := relatedTasks(ctx, tx, target)
		if readErr != nil {
			return readErr
		}
		if out.Pages, readErr = linkedPages(ctx, tx, pageIDs); readErr != nil {
			return readErr
		}
		out.Tasks, readErr = linkedTasks(ctx, tx, taskIDs, related)
		return readErr
	})
	if err != nil {
		return Backlinks{Pages: []PageLink{}, Tasks: []TaskLink{}},
			fmt.Errorf("search: name the pages and tasks linking to %s: %w", pageID, err)
	}
	out.PagesTotal, out.TasksTotal = len(out.Pages), len(out.Tasks)
	out.Pages = out.Pages[:min(len(out.Pages), BacklinksLimit)]
	out.Tasks = out.Tasks[:min(len(out.Tasks), BacklinksLimit)]
	return out, nil
}

// linkedPages names the published pages among ids, by title.
func linkedPages(ctx context.Context, tx *sql.Tx, ids []string) ([]PageLink, error) {
	out := []PageLink{}
	for chunk := range slices.Chunk(ids, ScanBatch) {
		statement, args := LinkedPagesRead(chunk)
		rows, err := tx.QueryContext(ctx, statement, args...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var p PageLink
			if err := rows.Scan(&p.ID, &p.Container, &p.Title); err != nil {
				_ = rows.Close()
				return nil, err
			}
			out = append(out, p)
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return nil, err
		}
		_ = rows.Close()
	}
	slices.SortFunc(out, func(a, b PageLink) int {
		if c := strings.Compare(strings.ToLower(a.Title), strings.ToLower(b.Title)); c != 0 {
			return c
		}
		return strings.Compare(a.ID, b.ID)
	})
	return out, nil
}

// LinkedPagesRead is [linkedPages]' statement over one batch of ids, and its
// arguments. See the package doc's "a read of another domain's table".
func LinkedPagesRead(ids []string) (string, []any) {
	return `
			SELECT id, container, title FROM pages_heads
			 WHERE status = 'published' AND id IN (` + binds(len(ids)) + `)`, anyOf(ids)
}

// relatedTasks is the tasks that name this page as a linked page — through
// `tracker_relations_other_idx`, the reverse edge the tracker keeps for
// exactly this question.
func relatedTasks(ctx context.Context, tx *sql.Tx, pageID string) ([]string, error) {
	statement, args := RelatedTasksRead(pageID)
	rows, err := tx.QueryContext(ctx, statement, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// RelatedTasksRead is [relatedTasks]' statement and its arguments. See the
// package doc's "a read of another domain's table".
func RelatedTasksRead(pageID string) (string, []any) {
	return `
		SELECT task_id FROM tracker_relations WHERE other_id = ? AND kind = 'page'`,
		[]any{pageID}
}

// linkedTasks names the live tasks among the cited and the related ids, by
// key, each once with the ways it links.
//
// TWO TASKS CAN HOLD ONE KEY ([TaskLink.KeyCollision]), so the key alone does
// not order the list: a tie is broken by the claimant first — the task the key
// actually opens — and then by id, because the rows arrive from batched `IN`
// reads in no order SQL promises and `slices.SortFunc` is not stable, so a tie
// left to the input drew the two in either order from one answer to the next.
func linkedTasks(ctx context.Context, tx *sql.Tx, cited, related []string) ([]TaskLink, error) {
	via := map[string][]string{}
	for _, id := range related {
		via[id] = append(via[id], TaskLinkedPage)
	}
	for _, id := range cited {
		if !slices.Contains(via[id], TaskCitesPage) {
			via[id] = append(via[id], TaskCitesPage)
		}
	}
	ids := slices.Sorted(maps.Keys(via))
	out := []TaskLink{}
	for chunk := range slices.Chunk(ids, ScanBatch) {
		statement, args := LinkedTasksRead(chunk)
		rows, err := tx.QueryContext(ctx, statement, args...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var t TaskLink
			var collision int
			if err := rows.Scan(&t.ID, &t.Key, &collision, &t.Title, &t.Status); err != nil {
				_ = rows.Close()
				return nil, err
			}
			t.KeyCollision = collision == 1
			t.Via = via[t.ID]
			out = append(out, t)
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return nil, err
		}
		_ = rows.Close()
	}
	slices.SortFunc(out, func(a, b TaskLink) int {
		if c := compareKeys(a.Key, b.Key); c != 0 {
			return c
		}
		if a.KeyCollision != b.KeyCollision {
			if b.KeyCollision {
				return -1
			}
			return 1
		}
		return strings.Compare(a.ID, b.ID)
	})
	return out, nil
}

// compareKeys orders work-item keys as a person reads them: by project, then
// by number — ENG-9 before ENG-10, which a string sort gets backwards.
func compareKeys(a, b string) int {
	pa, na := splitKey(a)
	pb, nb := splitKey(b)
	if c := strings.Compare(pa, pb); c != 0 {
		return c
	}
	if na != nb {
		if na < nb {
			return -1
		}
		return 1
	}
	return strings.Compare(a, b)
}

// splitKey is a key's project and its number, or the whole key and -1 when it
// is not `PROJECT-n`.
func splitKey(key string) (string, int) {
	project, number, ok := strings.Cut(key, "-")
	if !ok || number == "" {
		return key, -1
	}
	n := 0
	for _, r := range number {
		if r < '0' || r > '9' {
			return key, -1
		}
		n = n*10 + int(r-'0')
	}
	return project, n
}

// LinkedTasksRead is [linkedTasks]' statement over one batch of ids, and its
// arguments. See the package doc's "a read of another domain's table".
func LinkedTasksRead(ids []string) (string, []any) {
	return `
			SELECT id, key, key_collision, title, status FROM tracker_tasks
			 WHERE removed_at IS NULL AND id IN (` + binds(len(ids)) + `)`, anyOf(ids)
}

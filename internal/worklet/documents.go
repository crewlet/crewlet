package worklet

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/store"
)

// Reading one object's stored state INSIDE a write's own snapshot, unlike
// [Reader]'s many-row, completeness-reporting queries: the framework's
// snapshot already fixes the anchor and the deferral probe, and a row read in
// a second transaction would pair a decision with a different instant
// (contract 2).
//
// Each returns (value, held, error): "archived" and "not applied here yet" are
// different facts, and conflating them would create work in a project this
// node has never seen.

// documentSelect is the statement that reads one whole-document object, keyed
// per table as [documentTable] is: the key column differs, and one shared
// column would be the polymorphic document table this design refuses.
func documentSelect(s Subject) (query string, args []any, err error) {
	table, key, err := documentTable(s)
	if err != nil {
		return "", nil, err
	}
	switch table {
	case "tracker_projects", "tracker_tagsets", "tracker_catalogues":
		// THE COLUMN IS THE WRITE'S OWN: each table keys on the word its
		// subject means, and none of these has an `id` column.
		column := map[string]string{
			"tracker_projects":   "key",
			"tracker_tagsets":    "project_key",
			"tracker_catalogues": "name",
		}[table]
		return `SELECT document, version FROM ` + table +
			` WHERE ` + column + ` = ?`, []any{key}, nil
	case "tracker_views", "tracker_files":
		return `SELECT document, version FROM ` + table + ` WHERE id = ?`,
			[]any{key}, nil
	}
	// NOT tracker_persons: it has no `document` column, so [readPerson]
	// reassembles a person from its columns.
	return "", nil, fmt.Errorf("worklet: %s has no document read", s.Kind)
}

// readDocument reads one whole-document object and its stored version.
//
// The version is what `if_match` compares against, NEVER an expectation: that
// is the log's anchor, read by the framework in this same transaction (D153).
// Arbitrating on the row's version would judge against what this node has
// applied rather than what the log holds.
func readDocument[T any](ctx context.Context, tx *sql.Tx, s Subject,
	setVersion func(*T, uint64)) (T, bool, error) {

	var zero T
	query, args, err := documentSelect(s)
	if err != nil {
		return zero, false, err
	}
	var body []byte
	var version int64
	switch err := tx.QueryRowContext(ctx, query, args...).Scan(&body, &version); {
	case errors.Is(err, sql.ErrNoRows):
		return zero, false, nil
	case err != nil:
		return zero, false, fmt.Errorf("worklet: read %s: %w", s, err)
	}
	var document T
	if err := json.Unmarshal(body, &document); err != nil {
		return zero, false, fmt.Errorf("worklet: decode the stored %s: %w", s, err)
	}
	setVersion(&document, uint64(version))
	return document, true, nil
}

func readProject(ctx context.Context, tx *sql.Tx, key string) (Project, bool, error) {
	return readDocument(ctx, tx, ProjectSubject(key),
		func(p *Project, v uint64) { p.Version = v })
}

// readRoster reads the chart's seat order, absent until the first chart apply.
// Its own statement: the roster is machinery no generic write path reaches.
func readRoster(ctx context.Context, tx *sql.Tx) (Roster, bool, error) {
	var body []byte
	var version int64
	err := tx.QueryRowContext(ctx, `SELECT document, version FROM tracker_rosters
		WHERE name = ?`, RosterSubject().ID).Scan(&body, &version)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return Roster{}, false, nil
	case err != nil:
		return Roster{}, false, fmt.Errorf("worklet: read the seats' order: %w", err)
	}
	var roster Roster
	if err := json.Unmarshal(body, &roster); err != nil {
		return Roster{}, false, fmt.Errorf("worklet: decode the stored seats' order: %w", err)
	}
	roster.Version = uint64(version)
	return roster, true, nil
}

func readTagSet(ctx context.Context, tx *sql.Tx, project string) (TagSet, bool, error) {
	return readDocument(ctx, tx, TagsSubject(project),
		func(t *TagSet, v uint64) { t.Version = v })
}

// readPerson reads one person's own state. Not [readDocument]: the applier
// explodes the record into `tracker_persons` columns and keeps no `document`.
//
// AN ABSENT PERSON IS NOT AN ERROR: a row is written by a person's first
// gesture, so "no row" is ordinary for a new human and for every seat.
func readPerson(ctx context.Context, tx *sql.Tx, handle string) (Person, bool, error) {
	var person Person
	var seenSeq int64
	var setAt int64
	var version int64
	var read, unread, snoozed, reasons, priorities, pins, favorites []byte
	switch err := tx.QueryRowContext(ctx, `
		SELECT generation, seen_through, seen_through_stream, read_json,
		       unread_json, snoozed_json, primary_reasons_json, priorities_json,
		       pinned_views_json, favorites_json, priorities_set_by,
		       priorities_set_at, version
		FROM tracker_persons WHERE handle = ?`, handle).
		Scan(&person.Generation, &seenSeq, &person.SeenThrough.Stream, &read,
			&unread, &snoozed, &reasons, &priorities, &pins, &favorites,
			&person.PrioritiesSetBy, &setAt, &version); {
	case errors.Is(err, sql.ErrNoRows):
		return Person{V: DocumentVersion, Handle: handle}, false, nil
	case err != nil:
		return Person{}, false, fmt.Errorf("worklet: read %s's own state: %w",
			handle, err)
	}
	person.V, person.Handle = DocumentVersion, handle
	person.Version = uint64(version)
	// PACKED: the generation rides in the high bits, so a bare sequence
	// decodes as generation zero, which is what it was.
	person.SeenThrough.Generation = uint32(seenSeq / statelog.GenerationStride)
	person.SeenThrough.Seq = uint64(seenSeq % statelog.GenerationStride)
	if setAt != 0 {
		// ZERO IS UNSET, not the epoch: decoding it would put 1970 on
		// every screen that renders the date.
		person.PrioritiesSetAt = store.DecodeTime(setAt)
	}
	for _, part := range []struct {
		body []byte
		into any
		what string
	}{
		{read, &person.Read, "read"}, {unread, &person.Unread, "unread"},
		{snoozed, &person.Snoozed, "snoozed"},
		{reasons, &person.PrimaryReasons, "primary reasons"},
		{priorities, &person.Priorities, "priorities"},
		{pins, &person.PinnedViews, "pinned views"},
		{favorites, &person.Favorites, "favourites"},
	} {
		if len(part.body) == 0 {
			continue
		}
		if err := json.Unmarshal(part.body, part.into); err != nil {
			return Person{}, false, fmt.Errorf("worklet: decode %s's %s: %w",
				handle, part.what, err)
		}
	}
	return person, true, nil
}

// readCounter reads a project's key sequence. Not [readDocument]: the row
// stores `last` as a column and no document, since the number IS the object.
func readCounter(ctx context.Context, tx *sql.Tx, project string) (Counter, bool, error) {
	var last, version int64
	switch err := tx.QueryRowContext(ctx,
		`SELECT last, version FROM tracker_counters WHERE project_key = ?`,
		project).Scan(&last, &version); {
	case errors.Is(err, sql.ErrNoRows):
		// AN ABSENT COUNTER IS ZERO, not a refusal: a project's first
		// task creates it, so a create self-heals a project this node
		// has seen no task in yet.
		return Counter{V: DocumentVersion, Project: project}, false, nil
	case err != nil:
		return Counter{}, false, fmt.Errorf("worklet: read %s's key counter: %w",
			project, err)
	}
	return Counter{
		V: DocumentVersion, Version: uint64(version),
		Project: project, Last: int(last),
	}, true, nil
}

// readAlias reports whether a key is claimed, and by which task.
func readAlias(ctx context.Context, tx *sql.Tx, key string) (string, bool, error) {
	var task string
	switch err := tx.QueryRowContext(ctx,
		`SELECT task_id FROM tracker_task_keys WHERE key = ?`, key).Scan(&task); {
	case errors.Is(err, sql.ErrNoRows):
		return "", false, nil
	case err != nil:
		return "", false, fmt.Errorf("worklet: read the claim on key %s: %w", key, err)
	}
	return task, true, nil
}

// keyHeldByAnother reports whether opening key reaches a task OTHER than id
// ([resolveTaskID]'s answer), inside a write's own snapshot. It reads only the
// directory, as [keyOpensAnother] does: the resolver asks it first, the
// applier derives `key_collision` from it ([flagKeyCollisions]), and it holds
// a freshly minted key no row's column holds yet.
//
// It can be overtaken: two creates racing for a key a restored counter minted
// both read it unclaimed, and the second applied is flagged. Nothing depends
// on the answer being final (a wake's conversation is the task's id,
// [Prompt.PartitionKey]); the cost is one wake or receipt naming the key.
//
// An empty key or id is never a collision.
func keyHeldByAnother(ctx context.Context, tx *sql.Tx, key, id string) (bool, error) {
	if key == "" || id == "" {
		return false, nil
	}
	claimant, held, err := readAlias(ctx, tx, key)
	if err != nil {
		return false, err
	}
	return held && claimant != id, nil
}

// readChildBatch is one batch of a task's DIRECT children in project (see
// [Writer.reparentOnto]): `move_subtasks` moves the subtasks, whose own
// subtasks travel beneath them; re-parenting every descendant would flatten.
//
// A re-run reads only what is LEFT (a moved child is no longer a child), so
// the duty finishing a dead holder's walk sees the remainder. Within one run
// the cursor is `after`, an id: the applier lags the publish, so re-reading
// from the start would hand the walk the same batch until it caught up. An id
// cursor advances on what was PUBLISHED, which the walk knows.
func readChildBatch(ctx context.Context, tx *sql.Tx, parent, project, after string,
	limit int) ([]Task, error) {

	// NOT A REMOVED CHILD: a tombstoned task refuses every write, so
	// selecting one would stop every run and the merge could never finish.
	// It stays under its parent, where a restore finds it.
	rows, err := tx.QueryContext(ctx, childBatchStatement, parent, project, after, limit)
	if err != nil {
		return nil, fmt.Errorf("worklet: read the children of %s: %w", parent, err)
	}
	defer rows.Close()
	return scanSubtree(rows, parent)
}

// childBatchStatement is [readChildBatch]'s read, named for the plan test. It
// names the tree's index (replicated 0058): the planner otherwise seeks the
// project and sorts its whole board by id for one batch.
const childBatchStatement = `
		SELECT document, version, 0 FROM tracker_tasks INDEXED BY ` + parentIndex + `
		WHERE parent_id = ? AND project_key = ? AND id > ? AND removed_at IS NULL
		ORDER BY id LIMIT ?`

// readSubtree is every descendant of a root task, ORDERED BY (depth, id): a
// stable order for a cross-project move and a subtree removal, which walk it
// one append at a time; a rank or title would re-order under a concurrent
// edit. It is not a key assignment a re-run can reproduce (membership moves),
// so a move's re-run mints a fresh range for what is left.
//
// Depth first means a removal tombstones a parent before its child; the id
// breaks ties so every node and re-run walks one order.
func readSubtree(ctx context.Context, tx *sql.Tx, root string) ([]Task, error) {
	rows, err := tx.QueryContext(ctx, subtreeStatement, root, root, root)
	if err != nil {
		return nil, fmt.Errorf("worklet: read the subtree under %s: %w", root, err)
	}
	defer rows.Close()
	return scanSubtree(rows, root)
}

// subtreeStatement is [readSubtree]'s read, named for the plan test: each level
// of the tree is a seek on the parent (replicated 0058).
//
// The walk never steps onto its root, because the tree can be a CYCLE: two
// racing re-parents (A under B, B under A) are both applied and flagged
// (apply_closure.go), and an unbounded walk would loop past its context's
// deadline, holding its transaction. With ONE parent per task the root is the
// only task a walk can return to, so excluding it (at the first level too, for
// a task its own parent) returns each subtree task once.
const subtreeStatement = `
		WITH RECURSIVE descendants(id, depth) AS (
			SELECT id, 0 FROM tracker_tasks WHERE parent_id = ? AND id <> ?
			UNION ALL
			SELECT t.id, d.depth + 1
			FROM tracker_tasks t JOIN descendants d ON t.parent_id = d.id
			WHERE t.id <> ?
		)
		SELECT t.document, t.version, d.depth
		FROM descendants d JOIN tracker_tasks t ON t.id = d.id
		ORDER BY d.depth, t.id`

// scanSubtree decodes a subtree read's rows.
func scanSubtree(rows *sql.Rows, root string) ([]Task, error) {
	var subtree []Task
	for rows.Next() {
		var body []byte
		var version int64
		var depth int
		if err := rows.Scan(&body, &version, &depth); err != nil {
			return nil, fmt.Errorf("worklet: read a descendant of %s: %w", root, err)
		}
		var task Task
		if err := json.Unmarshal(body, &task); err != nil {
			return nil, fmt.Errorf("worklet: decode a descendant of %s: %w", root, err)
		}
		task.Version = uint64(version)
		subtree = append(subtree, task)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("worklet: read the subtree under %s: %w", root, err)
	}
	return subtree, nil
}

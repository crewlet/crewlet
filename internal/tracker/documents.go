package tracker

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/crewlet/crewlet/internal/store"
)

// Reading an object's stored state INSIDE a write's own snapshot.
//
// # Why these are not the query path
//
// [Reader] answers a question about many rows and reports whether the answer
// is complete. These answer one question about one row, inside the read
// transaction a decision is being made in, and completeness is not theirs to
// report: the framework's snapshot already establishes what the anchor and the
// deferral probe say about this subject, and a decision that read a row from a
// second transaction would be a decision paired with an expectation about a
// different instant — which is contract 2's whole point.
//
// Every one of them returns (value, held, error), never (value, error) with a
// zero value for absent: "this project is archived" and "this node has not
// applied this project yet" are different facts, and a caller that conflated
// them would create work in a project it has never seen.

// documentSelect is the statement that reads one whole-document object.
//
// KEYED PER TABLE, mirroring [documentTable], because the key column differs
// and a sprint's is two columns. A generic statement would mean every table
// carrying the same key column, which is the polymorphic document table this
// design deliberately does not have.
func documentSelect(s Subject) (query string, args []any, err error) {
	table, key, err := documentTable(s)
	if err != nil {
		return "", nil, err
	}
	switch table {
	case "tracker_sprints":
		project, number, err := splitSprintID(key)
		if err != nil {
			return "", nil, err
		}
		return `SELECT document, version FROM tracker_sprints
			WHERE project_key = ? AND number = ?`, []any{project, number}, nil
	case "tracker_projects", "tracker_tagsets", "tracker_catalogues":
		// THE COLUMN IS THE WRITE'S OWN. Each of these tables keys on
		// the word its subject means — a project on its `key`, a tag set
		// on the project it belongs to, a catalogue on its `name` — and
		// the catalogue's was spelled `id` here against a table that has
		// no such column, so the first read of one failed at runtime.
		column := map[string]string{
			"tracker_projects":   "key",
			"tracker_tagsets":    "project_key",
			"tracker_catalogues": "name",
		}[table]
		return `SELECT document, version FROM ` + table +
			` WHERE ` + column + ` = ?`, []any{key}, nil
	case "tracker_views", "tracker_goals":
		return `SELECT document, version FROM ` + table + ` WHERE id = ?`,
			[]any{key}, nil
	}
	// NOT tracker_persons, and its absence here is the point: that table
	// stores no `document` column at all — a person's record is exploded
	// into columns and nothing keeps the blob — so this statement would
	// have failed with "no such column: document" the first time anything
	// read one. [readPerson] reassembles it from the columns instead, for
	// the reason [readCounter] gives for the same shape.
	return "", nil, fmt.Errorf("tracker: %s has no document read", s.Kind)
}

// readDocument reads one whole-document object and its stored version.
//
// The version it fills in is what `if_match` compares against and NEVER an
// expectation: the expectation is the log's own anchor, read by the framework
// in this same transaction. D153's whole finding is that the two are different
// facts about different things, and a decision that used the row's version to
// arbitrate would be arbitrating against what this node has applied rather
// than against what the log holds.
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
		return zero, false, fmt.Errorf("tracker: read %s: %w", s, err)
	}
	var document T
	if err := json.Unmarshal(body, &document); err != nil {
		return zero, false, fmt.Errorf("tracker: decode the stored %s: %w", s, err)
	}
	setVersion(&document, uint64(version))
	return document, true, nil
}

func readProject(ctx context.Context, tx *sql.Tx, key string) (Project, bool, error) {
	return readDocument(ctx, tx, ProjectSubject(key),
		func(p *Project, v uint64) { p.Version = v })
}

func readSprint(ctx context.Context, tx *sql.Tx, project string, number int) (Sprint, bool, error) {
	return readDocument(ctx, tx, SprintSubject(project, number),
		func(s *Sprint, v uint64) { s.Version = v })
}

func readTagSet(ctx context.Context, tx *sql.Tx, project string) (TagSet, bool, error) {
	return readDocument(ctx, tx, TagsSubject(project),
		func(t *TagSet, v uint64) { t.Version = v })
}

// readPerson reads one person's own state.
//
// ITS OWN STATEMENT rather than [readDocument], because a person is not a
// whole-document object: `tracker_persons` carries no `document` column — the
// applier explodes the record into columns and keeps no blob — so the generic
// read would have failed on a column that has never existed. The counter is
// the other table of this shape and says the same thing.
//
// AN ABSENT PERSON IS NOT AN ERROR. Everyone starts without a row: the first
// thing that writes one is that person's first gesture, so "no row" is the
// ordinary state of every human on their first day and every seat for ever.
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
		return Person{}, false, fmt.Errorf("tracker: read %s's own state: %w",
			handle, err)
	}
	person.V, person.Handle = DocumentVersion, handle
	person.Version = uint64(version)
	person.SeenThrough.Seq = uint64(seenSeq)
	if setAt != 0 {
		// ZERO IS UNSET, not the epoch: the ordinary state is a
		// person's own list, and decoding a zero into 1970 would put a
		// date on every screen that renders one.
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
			return Person{}, false, fmt.Errorf("tracker: decode %s's %s: %w",
				handle, part.what, err)
		}
	}
	return person, true, nil
}

// readCounter reads a project's key sequence.
//
// ITS OWN STATEMENT rather than [readDocument], because the counter is not a
// whole-document object: its row carries `last` as a column and stores no
// document, since the one number IS the object.
func readCounter(ctx context.Context, tx *sql.Tx, project string) (Counter, bool, error) {
	var last, version int64
	switch err := tx.QueryRowContext(ctx,
		`SELECT last, version FROM tracker_counters WHERE project_key = ?`,
		project).Scan(&last, &version); {
	case errors.Is(err, sql.ErrNoRows):
		// AN ABSENT COUNTER IS ZERO, not a refusal. A project's first
		// task creates the counter with its own record, which is what
		// makes a create self-heal a project whose chart apply landed
		// before this node ever saw a task in it.
		return Counter{V: DocumentVersion, Project: project}, false, nil
	case err != nil:
		return Counter{}, false, fmt.Errorf("tracker: read %s's key counter: %w",
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
		return "", false, fmt.Errorf("tracker: read the claim on key %s: %w", key, err)
	}
	return task, true, nil
}

// readSubtree is every descendant of a root task, ORDERED BY (depth, id).
//
// # Why the order is part of the answer
//
// A cross-project move assigns each descendant a key from one minted range,
// and a duty completing an abandoned walk on another node has to assign the
// SAME key to the same descendant. The base rides the root's record; this
// ordering is the other half, and it is by (depth, id) because both are
// stable: a depth is a fact about the tree and an id never changes, while
// anything ordered by a rank or a title would re-order under an edit somebody
// made while the walk ran.
func readSubtree(ctx context.Context, tx *sql.Tx, root string) ([]Task, error) {
	rows, err := tx.QueryContext(ctx, `
		WITH RECURSIVE descendants(id, depth) AS (
			SELECT id, 0 FROM tracker_tasks WHERE parent = ?
			UNION ALL
			SELECT t.id, d.depth + 1
			FROM tracker_tasks t JOIN descendants d ON t.parent = d.id
		)
		SELECT t.document, t.version, d.depth
		FROM descendants d JOIN tracker_tasks t ON t.id = d.id
		ORDER BY d.depth, t.id`, root)
	if err != nil {
		return nil, fmt.Errorf("tracker: read the subtree under %s: %w", root, err)
	}
	defer rows.Close()

	var subtree []Task
	for rows.Next() {
		var body []byte
		var version int64
		var depth int
		if err := rows.Scan(&body, &version, &depth); err != nil {
			return nil, fmt.Errorf("tracker: read a descendant of %s: %w", root, err)
		}
		var task Task
		if err := json.Unmarshal(body, &task); err != nil {
			return nil, fmt.Errorf("tracker: decode a descendant of %s: %w", root, err)
		}
		task.Version = uint64(version)
		subtree = append(subtree, task)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("tracker: read the subtree under %s: %w", root, err)
	}
	return subtree, nil
}

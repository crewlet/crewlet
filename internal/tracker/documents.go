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
// per table. A generic statement would mean every table carrying the same key
// column, which is the polymorphic document table this design deliberately
// does not have.
func documentSelect(s Subject) (query string, args []any, err error) {
	table, key, err := documentTable(s)
	if err != nil {
		return "", nil, err
	}
	switch table {
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
	case "tracker_views":
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

// readPartyRecord reads the OWN-STATE document of a person who answers to more
// than one identity.
//
// # It is the first record that exists, never two merged
//
// A person's record is what THEY decided — the order they mean to work in,
// the views they pinned, which notices they have read. Two such records merged
// is an arrangement nobody made: two priority lists concatenated is an order
// no one chose, and two pin sets unioned is a strip whose "first" is a tie.
// So the identities are tried in order, the seat first, and the first that has
// a record is the one this person's state IS.
//
// # Why there can be a second record at all, and what the loop is for NOW
//
// The person tools used to address the write by the CREDENTIAL — `mark_inbox`
// and `set_pins` wrote on behalf of `actor.Handle`, which through the operator
// MCP is the token's own id — so a founder whose assistant had been marking
// their inbox read had one record, named `founder`, and a read of the seat
// alone showed them an inbox where nothing had ever been read.
//
// THAT IS FIXED AT THE WRITE, which is the only place it could be: a person
// write is keyed on [Writer.Record], the seat the credential is bound to, so
// every new record is the person's and there is at most one per person from
// here on. It was never attribution — a record's author stays the token, which
// is the audit trail — it is WHOSE STATE the document holds, and that is the
// person.
//
// So this loop is NOT a second opinion about the write: it is the READER of
// the records written BEFORE that fix, which are permanent (nothing rewrites a
// person's row, and a company that ran an earlier build has them). The seat
// leads, so the moment anything writes the person's own record it is the one
// that answers and the credential's is never read again. Delete the fallback
// and a founder loses every mark, pin and priority their assistant made before
// the upgrade; keep it and it costs one extra lookup for a person who has no
// old record, on the read path only.
func readPartyRecord(ctx context.Context, tx *sql.Tx, who Party) (
	Person, bool, error) {

	for _, handle := range who.Handles() {
		person, held, err := readPerson(ctx, tx, handle)
		if err != nil {
			return Person{}, false, err
		}
		if held {
			// THE SEAT'S NAME ON IT, whichever record answered: the
			// handle is what every surface renders and what a mark
			// names, and reporting the credential's would put a token
			// on a person's own screen.
			person.Handle = who.Handle
			return person, true, nil
		}
	}
	return Person{V: DocumentVersion, Handle: who.Handle}, false, nil
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
// readChildBatch is one batch of a task's DIRECT children.
//
// # Why direct children rather than the subtree
//
// Because that is what a merge moves. A grandchild re-parented onto the
// canonical task is a grandchild whose own parent is somewhere else entirely
// — the subtree flattened, which is what re-parenting every descendant did,
// and which no caller ever asked for: `move_subtasks` promises the
// duplicate's SUBTASKS move, and a subtask's own subtasks travel underneath
// it.
//
// # Why it is a batch, and why the batch is keyed rather than counted
//
// A re-run reads only what is LEFT: a moved child is no longer a child, so
// the tracker duty finishing a walk whose holder died starts from the
// beginning and sees exactly the remainder. That is what lets one walk serve
// both callers.
//
// Within ONE run the cursor is a KEY rather than that same shrinking
// selection, and the difference is the whole reason this signature has an
// `after`. A published record is arbitrated by the broker and applied by this
// node's own applier some time later, so the read that follows a batch of
// moves almost always still shows those children under the old parent. A walk
// that re-read from the start would hand the same batch to itself until the
// applier caught up — spinning, or, with a guard against that, stopping after
// sixty-four of two hundred children and leaving the rest to a duty that
// should never have been needed. An id cursor advances on what was PUBLISHED,
// which is the fact this walk actually knows.
func readChildBatch(ctx context.Context, tx *sql.Tx, parent, after string,
	limit int) ([]Task, error) {

	rows, err := tx.QueryContext(ctx, `
		SELECT document, version, 0 FROM tracker_tasks
		WHERE parent_id = ? AND id > ? ORDER BY id LIMIT ?`, parent, after, limit)
	if err != nil {
		return nil, fmt.Errorf("tracker: read the children of %s: %w", parent, err)
	}
	defer rows.Close()
	return scanSubtree(rows, parent)
}

func readSubtree(ctx context.Context, tx *sql.Tx, root string) ([]Task, error) {
	rows, err := tx.QueryContext(ctx, `
		WITH RECURSIVE descendants(id, depth) AS (
			SELECT id, 0 FROM tracker_tasks WHERE parent_id = ?
			UNION ALL
			SELECT t.id, d.depth + 1
			FROM tracker_tasks t JOIN descendants d ON t.parent_id = d.id
		)
		SELECT t.document, t.version, d.depth
		FROM descendants d JOIN tracker_tasks t ON t.id = d.id
		ORDER BY d.depth, t.id`, root)
	if err != nil {
		return nil, fmt.Errorf("tracker: read the subtree under %s: %w", root, err)
	}
	defer rows.Close()
	return scanSubtree(rows, root)
}

// scanSubtree decodes a subtree read's rows.
func scanSubtree(rows *sql.Rows, root string) ([]Task, error) {
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

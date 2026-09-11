package tracker

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"github.com/crewlet/crewlet/internal/store"
)

// The whole-document objects, and why they take one upsert each.
//
// A view, a goal, a sprint, a tag set, a person, a project and the two
// catalogues are small enough to travel as FULL POST-STATE — one document
// field, one upsert, and no patch semantics to get wrong. What that buys is
// that a replay of one of them is a single statement whose result cannot
// depend on what the row held before, which is the property a task's patch has
// to work for.
//
// EVERY ONE OF THEM CARRIES THE SAME VERSION GUARD, and the guard is a SKIP
// rather than an error: a redelivered record is ordinary traffic, and a
// constraint violation inside this transaction would abort it identically on
// every node and stall the whole fleet's log.

// applyDocument writes one whole-document object.
func (a *Applier) applyDocument(ctx context.Context, tx *sql.Tx, c applyContext) (int, error) {
	subject := c.subject()
	table, key, err := documentTable(subject)
	if err != nil {
		return 0, err
	}
	rows, err := a.upsertDocument(ctx, tx, table, key, c)
	if err != nil {
		return 0, err
	}
	// THE CHILDREN FOLLOW THE DOCUMENT'S OWN VERSION GUARD.
	//
	// Every upsert above carries `WHERE excluded.version > <table>.version`
	// and SKIPS a record it is not newer than — which is ordinary traffic:
	// a redelivery, or a record this build retained and reprocessed at its
	// original position after a newer one had already applied. The explode
	// below DELETES and re-inserts, and until this guard existed it did so
	// unconditionally: a stale reprocess left the goal's document saying
	// one thing and its targets saying another, with nothing to notice.
	//
	// Zero rows affected is exactly "this record did not write the
	// document", because an upsert that runs always affects one.
	extra := 0
	if rows > 0 {
		if extra, err = a.explode(ctx, tx, subject, c); err != nil {
			return 0, err
		}
	}
	history, err := a.writeHistory(ctx, tx, c, "")
	if err != nil {
		return 0, err
	}
	return rows + extra + history, nil
}

// documentTable maps a subject to the table and key its document lives at.
//
// A TABLE PER KIND rather than one polymorphic document table, because every
// one of them is FILTERED differently: a saved view is found by its container,
// a goal by its group, a person by their handle. One table would make every
// one of those a scan.
func documentTable(s Subject) (table, key string, err error) {
	switch s.Kind {
	case KindProject:
		return "tracker_projects", s.ID, nil
	case KindSprint:
		return "tracker_sprints", s.ID, nil
	case KindTags:
		return "tracker_tagsets", s.ID, nil
	case KindCatalogue:
		return "tracker_catalogues", s.ID, nil
	case KindView:
		return "tracker_views", s.ID, nil
	case KindGoal:
		return "tracker_goals", s.ID, nil
	case KindPerson:
		return "tracker_persons", s.ID, nil
	}
	return "", "", fmt.Errorf("tracker: %s is not a whole-document object", s.Kind)
}

// upsertDocument writes the object's own row.
//
// The statement is built per table rather than shared, because the key column
// and the extracted columns differ — and a single generic statement would mean
// every table carrying the same columns, which is the polymorphic document
// table this design does not have.
func (a *Applier) upsertDocument(ctx context.Context, tx *sql.Tx, table, key string,
	c applyContext) (int, error) {

	var res sql.Result
	var err error
	switch table {
	case "tracker_projects":
		var project Project
		if err := decodePayload(c.record.Mutation, &project); err != nil {
			return 0, fmt.Errorf("tracker: decode the project at %s: %w", c.position, err)
		}
		res, err = tx.ExecContext(ctx, `
			INSERT INTO tracker_projects
				(key, name, purpose, unit, chart_epoch, default_assignee,
				 sprint_policy_json, sprint_next, active_sprint, sprint_measure,
				 policy_version, archived, rank_respread_pending,
				 rank_duplicate_pending, open_count, done_count, closed_count,
				 created_at, updated_at, version, document)
			VALUES (?,?,?,?,?,?,?,?,?,?,?,?,0,0,0,0,0,?,?,?,?)
			ON CONFLICT (key) DO UPDATE SET
				name = excluded.name, purpose = excluded.purpose,
				unit = excluded.unit, chart_epoch = excluded.chart_epoch,
				default_assignee = excluded.default_assignee,
				sprint_policy_json = excluded.sprint_policy_json,
				sprint_next = excluded.sprint_next,
				active_sprint = excluded.active_sprint,
				sprint_measure = excluded.sprint_measure,
				policy_version = excluded.policy_version,
				archived = excluded.archived, updated_at = excluded.updated_at,
				version = excluded.version, document = excluded.document
			WHERE excluded.version > tracker_projects.version`,
			key, project.Name, project.Purpose, project.Unit, project.ChartEpoch,
			project.DefaultAssignee, jsonOf(project.Sprints), sprintNext(project),
			nullableInt(project.ActiveSprint), measureOf(project),
			project.PolicyVersion, boolInt(project.Archived),
			store.EncodeTime(project.CreatedAt), store.EncodeTime(project.UpdatedAt),
			c.packed, []byte(c.record.Mutation))
	case "tracker_sprints":
		var sprint Sprint
		if err := decodePayload(c.record.Mutation, &sprint); err != nil {
			return 0, fmt.Errorf("tracker: decode the sprint at %s: %w", c.position, err)
		}
		res, err = tx.ExecContext(ctx, `
			INSERT INTO tracker_sprints
				(project_key, number, name, goal, start_at, end_at, state,
				 closed_at, closed_by, open_at_close, rollover_to, rollover_done,
				 archived, created_at, updated_at, version, document)
			VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
			ON CONFLICT (project_key, number) DO UPDATE SET
				name = excluded.name, goal = excluded.goal,
				start_at = excluded.start_at, end_at = excluded.end_at,
				state = excluded.state, closed_at = excluded.closed_at,
				closed_by = excluded.closed_by,
				open_at_close = excluded.open_at_close,
				rollover_to = excluded.rollover_to,
				rollover_done = excluded.rollover_done,
				archived = excluded.archived, updated_at = excluded.updated_at,
				version = excluded.version, document = excluded.document
			WHERE excluded.version > tracker_sprints.version`,
			sprint.Project, sprint.Number, sprint.Name, sprint.Goal,
			store.EncodeTime(sprint.StartAt), store.EncodeTime(sprint.EndAt),
			string(sprint.State), nullableTime(sprint.ClosedAt), sprint.ClosedBy,
			sprint.OpenAtClose, nullableString(sprint.RolloverTo),
			boolInt(sprint.RolloverDone), boolInt(sprint.Archived),
			store.EncodeTime(sprint.CreatedAt), store.EncodeTime(sprint.UpdatedAt),
			c.packed, []byte(c.record.Mutation))
	case "tracker_tagsets":
		var set TagSet
		if err := decodePayload(c.record.Mutation, &set); err != nil {
			return 0, fmt.Errorf("tracker: decode the tag set at %s: %w", c.position, err)
		}
		res, err = tx.ExecContext(ctx, `
			INSERT INTO tracker_tagsets (project_key, tags_version, version, document)
			VALUES (?,?,?,?)
			ON CONFLICT (project_key) DO UPDATE SET
				tags_version = excluded.tags_version, version = excluded.version,
				document = excluded.document
			WHERE excluded.version > tracker_tagsets.version`,
			key, set.TagsVersion, c.packed, []byte(c.record.Mutation))
	case "tracker_catalogues":
		res, err = tx.ExecContext(ctx, `
			INSERT INTO tracker_catalogues (name, version, document)
			VALUES (?,?,?)
			ON CONFLICT (name) DO UPDATE SET
				version = excluded.version, document = excluded.document
			WHERE excluded.version > tracker_catalogues.version`,
			key, c.packed, []byte(c.record.Mutation))
	case "tracker_views":
		var view View
		if err := decodePayload(c.record.Mutation, &view); err != nil {
			return 0, fmt.Errorf("tracker: decode the view at %s: %w", c.position, err)
		}
		res, err = tx.ExecContext(ctx, `
			INSERT INTO tracker_views
				(id, container_kind, container_id, name, type, owner, protected,
				 is_default, rank, icon, params_json, version, document)
			VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?)
			ON CONFLICT (id) DO UPDATE SET
				container_kind = excluded.container_kind,
				container_id = excluded.container_id, name = excluded.name,
				type = excluded.type, owner = excluded.owner,
				protected = excluded.protected, is_default = excluded.is_default,
				rank = excluded.rank, icon = excluded.icon,
				params_json = excluded.params_json, version = excluded.version,
				document = excluded.document
			WHERE excluded.version > tracker_views.version`,
			key, view.Container.Kind, view.Container.ID, view.Name,
			string(view.Type), view.Owner, boolInt(view.Protected),
			boolInt(view.Default), string(view.Rank), view.Icon,
			jsonOf(view.Params), c.packed, []byte(c.record.Mutation))
	case "tracker_goals":
		var goal Goal
		if err := decodePayload(c.record.Mutation, &goal); err != nil {
			return 0, fmt.Errorf("tracker: decode the goal at %s: %w", c.position, err)
		}
		res, err = tx.ExecContext(ctx, `
			INSERT INTO tracker_goals
				(id, name, owners_json, group_label, start_at, due_at, health,
				 archived, created_at, updated_at, version, document)
			VALUES (?,?,?,?,?,?,?,?,?,?,?,?)
			ON CONFLICT (id) DO UPDATE SET
				name = excluded.name, owners_json = excluded.owners_json,
				group_label = excluded.group_label, start_at = excluded.start_at,
				due_at = excluded.due_at, health = excluded.health,
				archived = excluded.archived, updated_at = excluded.updated_at,
				version = excluded.version, document = excluded.document
			WHERE excluded.version > tracker_goals.version`,
			key, goal.Name, jsonOf(goal.Owners), goal.Group,
			nullableTime(goal.StartAt), nullableTime(goal.DueAt), goal.Health,
			boolInt(goal.Archived), store.EncodeTime(goal.CreatedAt),
			store.EncodeTime(goal.UpdatedAt), c.packed, []byte(c.record.Mutation))
	case "tracker_persons":
		var person Person
		if err := decodePayload(c.record.Mutation, &person); err != nil {
			return 0, fmt.Errorf("tracker: decode the person at %s: %w", c.position, err)
		}
		res, err = tx.ExecContext(ctx, `
			INSERT INTO tracker_persons
				(handle, generation, seen_through, seen_through_stream, read_json,
				 unread_json, snoozed_json, primary_reasons_json, priorities_json,
				 pinned_views_json, favorites_json, priorities_set_by,
				 priorities_set_at, version)
			VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?)
			ON CONFLICT (handle) DO UPDATE SET
				generation = excluded.generation,
				seen_through = excluded.seen_through,
				seen_through_stream = excluded.seen_through_stream,
				read_json = excluded.read_json, unread_json = excluded.unread_json,
				snoozed_json = excluded.snoozed_json,
				primary_reasons_json = excluded.primary_reasons_json,
				priorities_json = excluded.priorities_json,
				pinned_views_json = excluded.pinned_views_json,
				favorites_json = excluded.favorites_json,
				priorities_set_by = excluded.priorities_set_by,
				priorities_set_at = excluded.priorities_set_at,
				version = excluded.version
			WHERE excluded.version > tracker_persons.version`,
			key, person.Generation, int64(person.SeenThrough.Seq),
			person.SeenThrough.Stream, jsonOf(person.Read), jsonOf(person.Unread),
			jsonOf(person.Snoozed), jsonOf(person.PrimaryReasons),
			jsonOf(person.Priorities), jsonOf(person.PinnedViews),
			jsonOf(person.Favorites), person.PrioritiesSetBy,
			store.EncodeTime(person.PrioritiesSetAt), c.packed)
	default:
		return 0, fmt.Errorf("tracker: %s has no upsert", table)
	}
	if err != nil {
		return 0, fmt.Errorf("tracker: write %s at %s: %w", table, c.position, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("tracker: read the %s write's effect: %w", table, err)
	}
	return int(n), nil
}

// applyCounter writes a project's key sequence.
//
// ITS OWN SUBJECT, so a key mint never contends with an edit to that project's
// settings — which is what stops the busiest write in the company sharing an
// arbitration unit with the rarest.
func (a *Applier) applyCounter(ctx context.Context, tx *sql.Tx, c applyContext) (int, error) {
	var counter Counter
	if err := decodePayload(c.record.Mutation, &counter); err != nil {
		return 0, fmt.Errorf("tracker: decode the counter at %s: %w", c.position, err)
	}
	res, err := tx.ExecContext(ctx, `
		INSERT INTO tracker_counters (project_key, last, version) VALUES (?,?,?)
		ON CONFLICT (project_key) DO UPDATE SET
			last = excluded.last, version = excluded.version
		WHERE excluded.version > tracker_counters.version`,
		c.subject().ID, counter.Last, c.packed)
	if err != nil {
		return 0, fmt.Errorf("tracker: write the counter at %s: %w", c.position, err)
	}
	return affected(res)
}

// applyRankOrder writes a project's manual order.
//
// THE OBJECT IS THE ORDER, so the version lands on the order's own row and
// every moved task takes `scoped_through` instead — a rank move stamping a
// task's `version` would make that task's next write form an expectation the
// broker refuses, permanently, because the expectation is about the task's own
// subject and the record was published on another.
func (a *Applier) applyRankOrder(ctx context.Context, tx *sql.Tx, c applyContext) (int, error) {
	var order RankOrder
	if err := decodePayload(c.record.Mutation, &order); err != nil {
		return 0, fmt.Errorf("tracker: decode the rank order at %s: %w", c.position, err)
	}
	res, err := tx.ExecContext(ctx, `
		INSERT INTO tracker_rank_orders (project_key, version, scoped_through)
		VALUES (?,?,NULL)
		ON CONFLICT (project_key) DO UPDATE SET version = excluded.version
		WHERE excluded.version > tracker_rank_orders.version`,
		c.subject().ID, c.packed)
	if err != nil {
		return 0, fmt.Errorf("tracker: write the rank order at %s: %w", c.position, err)
	}
	rows, err := affected(res)
	if err != nil {
		return 0, err
	}
	if rows == 0 {
		// The order's own row refused the version, so every placement in
		// this record is already reflected. Applying them anyway would
		// move ranks backwards on a redelivery.
		return 0, nil
	}
	for _, placement := range order.Placements {
		moved, err := tx.ExecContext(ctx, `
			UPDATE tracker_tasks SET rank = ?, scoped_through = ?
			WHERE id = ? AND ? > MAX(version, scoped_through)`,
			string(placement.Rank), c.packed, placement.Task, c.packed)
		if err != nil {
			return 0, fmt.Errorf("tracker: move task %s at %s: %w",
				placement.Task, c.position, err)
		}
		n, err := affected(moved)
		if err != nil {
			return 0, err
		}
		rows += n
	}
	// A DUPLICATE RANK IS A REPAIRABLE OBSERVABLE, not a refusal, and the
	// probe is ONE INDEXED LOOKUP PER KEY THIS RECORD WROTE — never a
	// GROUP BY over the project.
	//
	// The distinction is the whole point of the decision that added the
	// column: a company-wide aggregate on every drag is the per-minute
	// scan no index answers, moved onto the apply path where it costs
	// every node rather than one. A duplicate can only involve a rank this
	// record just wrote, so those are the only ranks worth asking about,
	// and each is an equality seek on (project_key, rank, id).
	//
	// A unique index instead would turn a rare cosmetic anomaly into a
	// deterministic fleet-wide stalled log.
	for _, placement := range order.Placements {
		if _, err := tx.ExecContext(ctx, `
			UPDATE tracker_projects SET rank_duplicate_pending = 1
			WHERE key = ? AND EXISTS (
				SELECT 1 FROM tracker_tasks t
				WHERE t.project_key = ? AND t.rank = ? AND t.id <> ?)`,
			order.Project, order.Project, string(placement.Rank),
			placement.Task); err != nil {
			return 0, fmt.Errorf("tracker: probe for a duplicate of rank %s "+
				"in %s: %w", placement.Rank, order.Project, err)
		}
	}
	// AND THE RE-SPREAD HAND-OFF, SET AND CLEARED FROM THE SAME PROBE.
	//
	// A key past the renormalisation threshold is one the mint could not
	// shorten inline, so the project's order needs the duty's paced walk.
	// The applier owns the column in both directions — the walk's own last
	// batch is what clears it — because a RECORD clearing it would be a
	// second owner: the flag is derived from the rows this node holds, and
	// a node whose applier had not yet caught up would clear a flag its
	// own rows still justify.
	//
	// One indexed probe on the partial index over long keys, which is a
	// fraction of the project rather than a scan of it.
	if _, err := tx.ExecContext(ctx, `
		UPDATE tracker_projects SET rank_respread_pending = EXISTS (
			SELECT 1 FROM tracker_tasks t
			WHERE t.project_key = ? AND t.removed_at IS NULL
			  AND length(t.rank) > ?)
		WHERE key = ?`,
		order.Project, RankRenormaliseAt, order.Project); err != nil {
		return 0, fmt.Errorf("tracker: set %s's re-spread hand-off: %w",
			order.Project, err)
	}
	return rows, nil
}

// applyEviction writes the gate that drops a node's records.
//
// A READMISSION IS THE INVERSE COMMIT rather than a delete, so an eviction's
// whole history survives a replay — and a node that was evicted, readmitted
// and evicted again reads correctly rather than as one long absence.
func (a *Applier) applyEviction(ctx context.Context, tx *sql.Tx, c applyContext) (int, error) {
	var eviction Eviction
	if err := decodePayload(c.record.Mutation, &eviction); err != nil {
		return 0, fmt.Errorf("tracker: decode the eviction at %s: %w", c.position, err)
	}
	if eviction.Readmitted {
		res, err := tx.ExecContext(ctx, `
			UPDATE tracker_evictions
			SET readmitted_position = ?, readmitted_at = ?
			WHERE node_id = ? AND log_stream = ?`,
			c.packed, store.EncodeTime(c.brokerAt), eviction.NodeID,
			c.position.Stream)
		if err != nil {
			return 0, fmt.Errorf("tracker: readmit node %s at %s: %w",
				eviction.NodeID, c.position, err)
		}
		return affected(res)
	}
	res, err := tx.ExecContext(ctx, `
		INSERT INTO tracker_evictions
			(node_id, log_stream, from_position, at, by, readmitted_position,
			 readmitted_at)
		VALUES (?,?,?,?,?,NULL,NULL)
		ON CONFLICT (node_id, log_stream) DO UPDATE SET
			from_position = excluded.from_position, at = excluded.at,
			by = excluded.by, readmitted_position = NULL, readmitted_at = NULL
		WHERE excluded.from_position > tracker_evictions.from_position`,
		eviction.NodeID, c.position.Stream, c.packed,
		store.EncodeTime(c.brokerAt), eviction.EvictedBy)
	if err != nil {
		return 0, fmt.Errorf("tracker: evict node %s at %s: %w",
			eviction.NodeID, c.position, err)
	}
	return affected(res)
}

// applyGeneration records a reanchor.
//
// CREATE-ONLY: two operators deriving the same number race at the broker and
// exactly one wins, so the row is written once and never updated — which is
// what makes the table an audit trail rather than a mutable pointer.
func (a *Applier) applyGeneration(ctx context.Context, tx *sql.Tx, c applyContext) (int, error) {
	var gen Generation
	if err := decodePayload(c.record.Mutation, &gen); err != nil {
		return 0, fmt.Errorf("tracker: decode the generation at %s: %w", c.position, err)
	}
	res, err := tx.ExecContext(ctx, `
		INSERT INTO tracker_log_generations
			(generation, at, by, prev_stream_created_at, new_stream_created_at,
			 prev_last_seq_seen, reason, record_id)
		VALUES (?,?,?,?,?,?,?,?)
		ON CONFLICT (generation) DO NOTHING`,
		gen.Gen, store.EncodeTime(c.brokerAt), gen.ReanchoredBy,
		store.EncodeTime(gen.PrevStreamCreatedAt),
		store.EncodeTime(gen.NewStreamCreatedAt), int64(gen.PrevLastSeqSeen),
		gen.Reason, c.record.OpID)
	if err != nil {
		return 0, fmt.Errorf("tracker: record generation %d at %s: %w",
			gen.Gen, c.position, err)
	}
	return affected(res)
}

// applyAlias claims a key for a task.
//
// UPSERTED, NEVER DELETED BY A TASK APPLY, AND NEVER LOWERED FROM CURRENT: a
// former key must go on resolving for the life of the deployment, because it
// is pasted into chat and typed into tool calls. A key another task already
// holds is LEFT AS IT IS and the newer task carries the collision flag —
// because the alternative, taking the key, silently re-points every reference
// anybody ever wrote.
func (a *Applier) applyAlias(ctx context.Context, tx *sql.Tx, c applyContext) (int, error) {
	var alias KeyAlias
	if err := decodePayload(c.record.Mutation, &alias); err != nil {
		return 0, fmt.Errorf("tracker: decode the alias at %s: %w", c.position, err)
	}
	res, err := tx.ExecContext(ctx, `
		INSERT INTO tracker_task_keys (key, task_id, current) VALUES (?,?,?)
		ON CONFLICT (key) DO UPDATE SET current = MAX(current, excluded.current)
		WHERE tracker_task_keys.task_id = excluded.task_id`,
		alias.Key, alias.TaskID, boolInt(alias.Current))
	if err != nil {
		return 0, fmt.Errorf("tracker: claim key %s at %s: %w", alias.Key, c.position, err)
	}
	rows, err := affected(res)
	if err != nil {
		return 0, err
	}
	if rows == 0 {
		if _, err := tx.ExecContext(ctx,
			`UPDATE tracker_tasks SET key_collision = 1 WHERE id = ?`,
			alias.TaskID); err != nil {
			return 0, fmt.Errorf("tracker: flag the key collision on %s: %w",
				alias.TaskID, err)
		}
	}
	return rows, nil
}

// The small conversions, in one place so a nil pointer means the same thing at
// every call site.

func affected(res sql.Result) (int, error) {
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("tracker: read a statement's effect: %w", err)
	}
	return int(n), nil
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func nullableInt(v *int) any {
	if v == nil {
		return nil
	}
	return *v
}

func nullableString(v string) any {
	if v == "" {
		return nil
	}
	return v
}

func nullableTime(t *time.Time) any {
	if t == nil {
		return nil
	}
	return store.EncodeTime(*t)
}

// jsonOf encodes a value for a JSON column, answering "null" rather than
// failing: a column that could not be encoded is a row this node writes and
// its peer does not, and the identity claim would report the divergence
// without ever naming the value that caused it.
func jsonOf(v any) string {
	body, err := json.Marshal(v)
	if err != nil {
		return "null"
	}
	return string(body)
}

func jsonUnmarshal(raw []byte, into any) error { return json.Unmarshal(raw, into) }

func sprintNext(p Project) int {
	if p.Sprints == nil {
		return 1
	}
	return p.Sprints.Next
}

func measureOf(p Project) string {
	if p.Sprints == nil || p.Sprints.Measure == "" {
		return "points"
	}
	return p.Sprints.Measure
}

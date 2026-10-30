package worklet

// The statements the applier writes and reads a task's relation rows and the
// dependency mirror with, outside the query grammar, named so the plan gate
// plans the statement that runs rather than a copy of it — the relation and
// mirror tables are among the tables it holds every reader of to an index.
// They are the applier's own, which is why they live in an apply file: the
// replicated estate's write side is the appliers' alone, and the store's
// exclusivity gate admits a tracker write from the applier's files and from
// nowhere else in the package.

// The statements a task's relations and their mirror are written with, named
// for the plan test: each relation row one-sided unless its blocker's mirror
// row already lists the task — a probe of that one row by its key — and the
// mirror rows the blocker's own commit writes.
const (
	relationsInsertPrefix = `
				INSERT INTO tracker_relations
					(task_id, other_id, kind, derived, one_sided,
					 one_sided_final, note, created_at)
				VALUES`
	relationsInsertRow = `(?,?,?,0,
					CASE WHEN ? = 'waiting_on' AND NOT EXISTS (
						SELECT 1 FROM tracker_task_dependents d
						WHERE d.task_id = ? AND d.dependent_id = ?)
					THEN 1 ELSE 0 END, ?, ?, ?)`
	relationsInsertConflict = `ON CONFLICT (task_id, other_id, kind) DO UPDATE SET
					one_sided = excluded.one_sided,
					one_sided_final = excluded.one_sided_final,
					note = excluded.note`
	mirrorInsertPrefix   = `INSERT INTO tracker_task_dependents (task_id, dependent_id) VALUES`
	mirrorInsertRow      = `(?,?)`
	mirrorInsertConflict = `ON CONFLICT (task_id, dependent_id) DO NOTHING`
)

// rejudgeEdgesStatement is [Applier.maintainDeps]' re-judgement of every
// edge waiting on a blocker, named for the plan test: the edges found by
// their other end through the reverse edge's index, each against the
// blocker's mirror row for it, by that row's key.
const rejudgeEdgesStatement = `
		UPDATE tracker_relations SET one_sided =
			CASE WHEN EXISTS (SELECT 1 FROM tracker_task_dependents d
			                  WHERE d.task_id = ? AND d.dependent_id = tracker_relations.task_id)
			THEN 0 ELSE 1 END
		WHERE other_id = ? AND kind = 'waiting_on'`

// The statements [othersNaming] reads with, named for the plan test: the tasks
// whose relations name the purged task, through the reverse edge's index, and
// the blockers whose mirror lists it, through the mirror's reverse index.
const (
	relatingStatement = `
		SELECT DISTINCT task_id FROM tracker_relations
		WHERE other_id = ? AND task_id <> ?`
	mirroringStatement = `
		SELECT task_id FROM tracker_task_dependents
		WHERE dependent_id = ? AND task_id <> ?`
)

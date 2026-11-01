package worklet

import (
	"context"
	"database/sql"
	"fmt"
	"maps"
	"slices"

	"github.com/crewlet/crewlet/internal/statelog"
)

// purgeReach is every OTHER task a purge's apply writes a row of, mapped to
// the project it is filed in on this node — empty for one this node holds no
// row of.
//
// THE SAME TASKS THE APPLY WRITES, read from the same rows
// ([Applier.purgeTask]): the tasks whose dependency edge names the purged task
// as the blocker, the blockers whose mirror lists it, the tasks with a
// relation to it, the tasks whose body references it, and its descendants,
// whose ancestry is rebuilt — the direct children among them re-parented.
// Each is a write to a row keyed on that task, so a record deferred under it
// on some node must hold the purge back there, and it can only if the purge's
// scope names it.
//
// THE KEYS ARE NOT IN IT. The apply also releases every key the purged task
// held and hands each on ([handOnKey]), re-deriving its holders'
// `key_collision`; those writes are decided by the key directory, and every
// record that writes the directory is filed under the KEY ([keyTerm]), so the
// purge names its keys rather than the tasks holding them — [purgeScope]'s
// keys argument.
type purgeReach map[string]string

// purgeReachStatement is [readPurgeReach]'s one statement, named for the plan
// test: every arm a seek on the purged task's id, its children's through the
// index of the tree (replicated 0058).
const purgeReachStatement = `
		SELECT o.id, COALESCE(t.project_key, '') FROM (
			SELECT task_id AS id FROM tracker_task_deps WHERE blocker_id = ?
			UNION SELECT task_id FROM tracker_task_dependents WHERE dependent_id = ?
			UNION SELECT task_id FROM tracker_relations WHERE other_id = ?
			UNION SELECT from_task FROM tracker_references WHERE to_task = ?
			UNION SELECT descendant_id FROM tracker_task_closure WHERE ancestor_id = ?
			UNION SELECT id FROM tracker_tasks WHERE parent_id = ?
		) o LEFT JOIN tracker_tasks t ON t.id = o.id
		WHERE o.id <> ?`

// readPurgeReach reads a purge's reach inside tx.
//
// ONE STATEMENT, the task table joined for where each is filed, so the set is
// read from one snapshot. The parent pointers are read beside the closure
// because they are what the apply re-parents by; the closure alone would miss
// a child a cycle left without an ancestry row.
func readPurgeReach(ctx context.Context, tx *sql.Tx, id string) (purgeReach, error) {
	rows, err := tx.QueryContext(ctx, purgeReachStatement, id, id, id, id, id, id, id)
	if err != nil {
		return nil, fmt.Errorf("worklet: read what a purge of %s writes: %w", id, err)
	}
	defer func() { _ = rows.Close() }()
	reach := purgeReach{}
	for rows.Next() {
		var other, project string
		if err := rows.Scan(&other, &project); err != nil {
			return nil, fmt.Errorf("worklet: read what a purge of %s writes: %w", id, err)
		}
		reach[other] = project
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("worklet: read what a purge of %s writes: %w", id, err)
	}
	return reach, nil
}

// scopeForPurge states every task a purge of id writes and every key it hands
// on, read before the request for the reason [Writer.scopeForDependents]
// gives: the publisher probes the deferral index with the REQUEST's scope and
// the applier files a deferral under the ENVELOPE's, so the two have to be one
// set and the request's is fixed before the decide runs. The decide checks
// both again ([ScopeSet.coversReach], [ScopeSet.coversKeys]) against the rows
// it decides on.
//
// THE REACH AND THE KEYS ARE ONE READ, so they describe one snapshot of the
// task.
//
// A WRITER WITH NO REPLICATED ESTATE states the DOMAIN: it can read neither
// what the purge reaches nor which keys the task holds, and every task holds
// one, so any narrower scope would be one the decide refuses on every attempt.
// The domain is the one scope that certainly covers both, and what it costs —
// a deferral anywhere in the tracker holds the purge — is the safe direction.
func (w *Writer) scopeForPurge(ctx context.Context, id, project string) (ScopeSet, error) {
	if w.db.IsZero() {
		return ScopeSet{Terms: []ScopeTerm{{Kind: TermDomain}}}, nil
	}
	var (
		reach purgeReach
		keys  []string
	)
	if err := w.db.Read(ctx, func(tx *sql.Tx) error {
		var err error
		if reach, err = readPurgeReach(ctx, tx, id); err != nil {
			return err
		}
		current, held, err := readTask(ctx, tx, id)
		if err != nil || !held {
			// NOT HELD IS NOT AN ERROR HERE: the decide refuses a task
			// this node does not have, naming its purge if there was one.
			return err
		}
		keys = keysOf(current)
		return nil
	}); err != nil {
		return ScopeSet{}, err
	}
	return purgeScope(id, project, reach, keys), nil
}

// purgeScope is the scope naming a purge of id, filed in project, every task
// in reach and every key the purged task holds.
//
// ONE TERM A TASK OR A KEY while they fit under [MaxScopeTerms]; past that the
// cap's own rule folds the largest projects into their CONTAINER term
// ([ScopeSet.capped]) — a key is filed under its own project, so a former key
// minted in another project counts there. A task this node holds no row of
// has no path this build can form, so it is claimed by the DOMAIN, as
// [ScopeSet.withObjects] claims one: the safe direction.
func purgeScope(id, project string, reach purgeReach, keys []string) ScopeSet {
	if len(reach) == 0 && len(keys) == 0 {
		return ScopeSet{Subject: true, Container: project}
	}
	terms := make([]ScopeTerm, 0, 1+len(reach)+len(keys))
	terms = append(terms, ScopeTerm{Kind: TermObject, Container: project, ID: id})
	for _, other := range slices.Sorted(maps.Keys(reach)) {
		filed := reach[other]
		if filed == "" {
			return ScopeSet{Terms: []ScopeTerm{{Kind: TermDomain}}}
		}
		terms = append(terms, ScopeTerm{Kind: TermObject, Container: filed, ID: other})
	}
	return ScopeSet{Terms: terms}.withKeys(id, keys).capped()
}

// coversReach reports whether this scope names every task in reach where the
// reach says it is filed.
//
// WHERE, not only WHICH, for [ScopeSet.covers]' reason: a task that moved
// project between the read that built this scope and the snapshot deciding
// it is named at a path its records are no longer filed under.
func (s ScopeSet) coversReach(reach purgeReach) error {
	for other, filed := range reach {
		covered := slices.ContainsFunc(s.Terms, func(t ScopeTerm) bool {
			switch t.Kind {
			case TermDomain:
				return true
			case TermContainer:
				return filed != "" && t.named() == filed
			case TermObject:
				return filed != "" && t.ID == other && t.Container == filed
			}
			return false
		})
		if covered {
			continue
		}
		// NAMED, AND NOT SILENTLY WIDENED, for [ScopeSet.covers]' reason:
		// the request's scope is what the publisher probed with.
		return fmt.Errorf("worklet: task %s would be rewritten by this purge and "+
			"the purge's scope does not name it, so a record deferred under it "+
			"would not hold the purge back — re-run it, and the second attempt "+
			"names what the purge reaches now: %w", other, statelog.ErrConflict)
	}
	return nil
}

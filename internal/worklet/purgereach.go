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
// It must be the tasks [Applier.purgeTask] writes, read from the same rows:
// dependents, blockers, relations, referrers and descendants (direct children
// re-parented). A record deferred under one of them holds the purge back only
// if the purge's scope names it.
//
// Keys are not in it: the key hand-on ([handOnKey]) is decided by the key
// directory, whose records are filed under the KEY ([keyTerm]), so the purge
// names its keys — [purgeScope]'s keys argument — not the tasks holding them.
type purgeReach map[string]string

// purgeReachStatement is [readPurgeReach]'s one statement, named for the plan
// test: every arm a seek on the purged task's id, the children's through the
// tree index (replicated 0058).
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

// readPurgeReach reads a purge's reach inside tx, in one statement so the set
// is one snapshot. Parent pointers are read beside the closure because the
// apply re-parents by them: the closure alone misses a child a cycle left
// without an ancestry row.
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
// on, read in one snapshot before the request for the reason
// [Writer.scopeForDependents] gives; the decide checks both again
// ([ScopeSet.coversReach], [ScopeSet.coversKeys]).
//
// A writer with no replicated estate states the DOMAIN: it can read neither
// the reach nor the keys, and every task holds a key, so any narrower scope
// would be refused on every attempt. Over-holding the purge is the safe
// direction.
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
			// Not held is not an error here: the decide refuses it,
			// naming its purge if there was one.
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
// One term per task or key, folded by [ScopeSet.capped] past [MaxScopeTerms]
// (a key counts under its own project). A task this node holds no row of has
// no path to name, so it widens to the DOMAIN, as [ScopeSet.withObjects] does.
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
// reach says it is filed — WHERE, not only WHICH, for [ScopeSet.covers]'
// reason: a task that moved project since the scope was built is named at a
// path its records are no longer filed under.
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
		// Refused, not silently widened: the publisher probed with the
		// request's scope ([ScopeSet.covers]).
		return fmt.Errorf("worklet: task %s would be rewritten by this purge and "+
			"the purge's scope does not name it, so a record deferred under it "+
			"would not hold the purge back — re-run it, and the second attempt "+
			"names what the purge reaches now: %w", other, statelog.ErrConflict)
	}
	return nil
}

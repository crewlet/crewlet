package tracker

import (
	"cmp"
	"context"
	"database/sql"
	"fmt"
	"slices"

	"github.com/crewlet/crewlet/internal/statelog"
)

// purgeReach is every OTHER task a purge's apply writes a row of, mapped to
// the project it is filed in on this node — empty for one this node holds no
// row of.
//
// THE SAME SET THE APPLY WRITES, read from the same rows ([Applier.purgeTask]):
// the tasks whose dependency edge names the purged task as the blocker, the
// blockers whose mirror lists it, the tasks with a relation to it, the tasks
// whose body references it, and its descendants, whose ancestry is rebuilt —
// the direct children among them re-parented. Each is a write to a row keyed on
// that task, so a record deferred under it on some node must hold the purge
// back there, and it can only if the purge's scope names it.
type purgeReach map[string]string

// readPurgeReach reads a purge's reach inside tx.
//
// ONE STATEMENT, the task table joined for where each is filed, so the set is
// read from one snapshot. The parent pointers are read beside the closure
// because they are what the apply re-parents by; the closure alone would miss
// a child a cycle left without an ancestry row.
func readPurgeReach(ctx context.Context, tx *sql.Tx, id string) (purgeReach, error) {
	rows, err := tx.QueryContext(ctx, `
		SELECT o.id, COALESCE(t.project_key, '') FROM (
			SELECT task_id AS id FROM tracker_task_deps WHERE blocker_id = ?
			UNION SELECT task_id FROM tracker_task_dependents WHERE dependent_id = ?
			UNION SELECT task_id FROM tracker_relations WHERE other_id = ?
			UNION SELECT from_task FROM tracker_references WHERE to_task = ?
			UNION SELECT descendant_id FROM tracker_task_closure WHERE ancestor_id = ?
			UNION SELECT id FROM tracker_tasks WHERE parent_id = ?
		) o LEFT JOIN tracker_tasks t ON t.id = o.id
		WHERE o.id <> ?`, id, id, id, id, id, id, id)
	if err != nil {
		return nil, fmt.Errorf("tracker: read what a purge of %s writes: %w", id, err)
	}
	defer func() { _ = rows.Close() }()
	reach := purgeReach{}
	for rows.Next() {
		var other, project string
		if err := rows.Scan(&other, &project); err != nil {
			return nil, fmt.Errorf("tracker: read what a purge of %s writes: %w", id, err)
		}
		reach[other] = project
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("tracker: read what a purge of %s writes: %w", id, err)
	}
	return reach, nil
}

// scopeForPurge states every task a purge of id writes, read before the
// request for the reason [Writer.scopeForDependents] gives: the publisher
// probes the deferral index with the REQUEST's scope and the applier files a
// deferral under the ENVELOPE's, so the two have to be one set and the
// request's is fixed before the decide runs. The decide checks it again
// ([ScopeSet.coversReach]) against the rows it decides on.
//
// A WRITER WITH NO REPLICATED ESTATE states the task alone, as
// [Writer.scopeForDependents] does, and the decide refuses it wherever the
// purge reaches further — naming the task it is short by.
func (w *Writer) scopeForPurge(ctx context.Context, id, project string) (ScopeSet, error) {
	if w.db.IsZero() {
		return purgeScope(id, project, nil), nil
	}
	var reach purgeReach
	if err := w.db.Read(ctx, func(tx *sql.Tx) error {
		var err error
		reach, err = readPurgeReach(ctx, tx, id)
		return err
	}); err != nil {
		return ScopeSet{}, err
	}
	return purgeScope(id, project, reach), nil
}

// purgeScope is the scope naming a purge of id, filed in project, and every
// task in reach.
//
// ONE OBJECT TERM A TASK while they fit under [MaxScopeTerms]; past that, the
// largest projects are named by their CONTAINER term instead — the smallest
// covering term, which is the cap's own rule — until they fit, and a reach
// spread over more projects than that is the domain. A task this node holds
// no row of has no path this build can form, so it is claimed by the DOMAIN,
// as [ScopeSet.withObjects] claims one: the safe direction.
func purgeScope(id, project string, reach purgeReach) ScopeSet {
	if len(reach) == 0 {
		return ScopeSet{Subject: true, Container: project}
	}
	byProject := map[string][]string{project: {id}}
	for other, filed := range reach {
		if filed == "" {
			return ScopeSet{Terms: []ScopeTerm{{Kind: TermDomain}}}
		}
		byProject[filed] = append(byProject[filed], other)
	}
	projects := make([]string, 0, len(byProject))
	count := 0
	for p, ids := range byProject {
		projects = append(projects, p)
		slices.Sort(ids)
		count += len(ids)
	}
	if len(projects) > MaxScopeTerms {
		return ScopeSet{Terms: []ScopeTerm{{Kind: TermDomain}}}
	}
	// Largest first, the name breaking a tie, so the same reach always
	// states the same scope.
	slices.SortFunc(projects, func(a, b string) int {
		if c := cmp.Compare(len(byProject[b]), len(byProject[a])); c != 0 {
			return c
		}
		return cmp.Compare(a, b)
	})
	covered := map[string]bool{}
	for _, p := range projects {
		if count <= MaxScopeTerms {
			break
		}
		covered[p] = true
		count -= len(byProject[p]) - 1
	}
	terms := make([]ScopeTerm, 0, count)
	slices.Sort(projects)
	for _, p := range projects {
		if covered[p] {
			terms = append(terms, ScopeTerm{Kind: TermContainer, ID: p})
			continue
		}
		for _, other := range byProject[p] {
			terms = append(terms, ScopeTerm{Kind: TermObject, Container: p, ID: other})
		}
	}
	return ScopeSet{Terms: terms}
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
				return filed != "" && t.ID == filed
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
		return fmt.Errorf("tracker: task %s would be rewritten by this purge and "+
			"the purge's scope does not name it, so a record deferred under it "+
			"would not hold the purge back — re-run it, and the second attempt "+
			"names what the purge reaches now: %w", other, statelog.ErrConflict)
	}
	return nil
}

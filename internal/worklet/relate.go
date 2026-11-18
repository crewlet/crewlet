package worklet

import (
	"context"
	"database/sql"
	"fmt"
	"slices"
	"strings"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/statelog"
)

// A dependency is the one MIRRORED relation: `waiting_on` is authored on the
// dependent, and the blocker carries its id in [Task.Dependents] so a close
// names whom it unblocks without a reverse scan.
//
// Both halves are bounded collections a record carries WHOLE ([MaxWaitingOn],
// [MaxOtherRelations], [MaxDependents]), so a caller-formed set would discard
// edges added since the caller's read and count its cap against the wrong set.
// Both travel as an INTENT, settled inside the decide snapshot as
// [settleWatch] settles watchers.

// settleRelations resolves a relation gesture into the whole collection,
// inside the decide snapshot (see [settleWatch]). It returns a COPY: Decide
// re-runs on a retry, and a resolution folded into the captured patch would
// compound across attempts.
func (w *Writer) settleRelations(current Task, patch TaskPatch) (TaskPatch, error) {
	if patch.Relate == nil {
		// A WHOLE SET STILL HAS TO FIT: no caller states one today, but
		// the field is on the record, and a cap enforced on the gesture
		// alone is one a future writer walks past.
		if patch.Relations != nil {
			if err := checkRelations(current.ID, *patch.Relations); err != nil {
				return patch, err
			}
		}
		return patch, nil
	}
	if patch.Relations != nil {
		// BOTH SPELLINGS AT ONCE IS REFUSED, not ordered: whichever won
		// would silently discard the other.
		return patch, invalid("this patch on task %s carries both a "+
			"relation gesture and a whole relation set — a caller states one "+
			"or the other", current.ID)
	}
	// THE WRITER STAMPS THE EDGES IT AUTHORS. One already stamped keeps
	// it: the dependency sequence stamps every edge of a call with one
	// shared instant. ON A COPY, since the decide may re-run and a stamp
	// written onto the caller's intent would carry the first round's
	// instant into the next.
	stamped := *patch.Relate
	stamped.Add = slices.Clone(stamped.Add)
	for i, add := range stamped.Add {
		if add.CreatedBy == "" {
			stamped.Add[i].CreatedBy = w.Actor
		}
		if add.CreatedAt.IsZero() {
			stamped.Add[i].CreatedAt = w.Now()
		}
	}
	next, err := stamped.resolve(current)
	if err != nil {
		return patch, err
	}
	if err := checkRelations(current.ID, next); err != nil {
		return patch, err
	}
	patch.Relate = nil
	patch.Relations = &next
	return patch, nil
}

// resolve is the gesture against one consistent read of the set.
func (r *RelationIntent) resolve(current Task) ([]Relation, error) {
	if r.Set != nil {
		if len(r.Add) > 0 || len(r.Remove) > 0 || len(r.Final) > 0 {
			return nil, invalid("a relation gesture on task %s "+
				"states both a whole set and a delta of %d added and %d "+
				"removed — a caller states one or the other",
				current.ID, len(r.Add), len(r.Remove))
		}
		return dedupeRelations(r.Set), nil
	}
	next := slices.Clone(current.Relations)
	for _, drop := range r.Remove {
		next = slices.DeleteFunc(next, func(have Relation) bool {
			// ON (Kind, Other) ALONE: a note is not part of the
			// edge's identity, so removal never quotes it back.
			return have.Kind == drop.Kind && have.Other == drop.Other
		})
	}
	for _, mark := range r.Final {
		at := slices.IndexFunc(next, func(have Relation) bool {
			return have.Kind == mark.Kind && have.Other == mark.Other
		})
		if at < 0 {
			// THE EDGE WENT AWAY: the repair succeeded by another
			// route, and marking a missing relation would ADD one.
			continue
		}
		next[at].OneSidedFinal = true
	}
	for _, add := range r.Add {
		if slices.ContainsFunc(next, func(have Relation) bool {
			return have.Kind == add.Kind && have.Other == add.Other
		}) {
			// AN EDGE ALREADY THERE IS NOT AN ERROR: a retried turn
			// re-states its edges, and refusing the same (Kind, Other)
			// would fail an ordinary redelivery.
			continue
		}
		next = append(next, add)
	}
	return next, nil
}

// dedupeRelations keeps the FIRST of each (Kind, Other), order preserved, so
// every data node stores one row per edge whether it applied the record or
// re-derived the document.
func dedupeRelations(in []Relation) []Relation {
	type edge struct {
		kind  RelationKind
		other string
	}
	seen := make(map[edge]bool, len(in))
	out := make([]Relation, 0, len(in))
	for _, r := range in {
		e := edge{r.Kind, r.Other}
		if seen[e] {
			continue
		}
		seen[e] = true
		out = append(out, r)
	}
	return out
}

// checkRelations is the two caps, counted by kind, plus what an edge must be.
// TWO CAPS because a `waiting_on` edge costs a mirrored commit and a row every
// close scans, while other edges are inert bytes: [MaxWaitingOn] and
// [MaxOtherRelations].
func checkRelations(taskID string, relations []Relation) error {
	waiting, other, entries := 0, 0, 0
	for _, r := range relations {
		if r.Kind == RelationEntry {
			// AN ENTRY IS NAMED BY ITS ID IN ITS CANONICAL SPELLING, so one
			// entry is one edge on every node and no name is ever stored.
			if id, err := uuid.Parse(r.Other); err != nil || id.String() != r.Other {
				return invalid("task %s carries an %s relation whose other "+
					"end is not an atlas entry's id", taskID, r.Kind)
			}
			entries++
		}
		switch {
		case !r.Kind.Valid():
			return invalid("task %s carries a relation of kind %q, "+
				"and the kinds are: %s", taskID, r.Kind, relationKindList())
		case r.Other == "":
			return invalid("task %s carries a %s relation naming no "+
				"other task", taskID, r.Kind)
		case r.Other == taskID:
			// A SELF-EDGE IS REFUSED, not dropped: as `waiting_on` it
			// is a block no close could ever clear.
			return invalid("task %s carries a %s relation to itself",
				taskID, r.Kind)
		case len(r.Note) > MaxRelationNote:
			return invalid("the note on task %s's %s relation to %s "+
				"is %d bytes and the maximum is %d",
				taskID, r.Kind, r.Other, len(r.Note), MaxRelationNote)
		}
		if r.Kind == RelationWaitingOn {
			waiting++
			continue
		}
		other++
	}
	if waiting > MaxWaitingOn {
		return invalid("task %s would wait on %d tasks and the "+
			"maximum is %d — a task with more blockers than that is a "+
			"milestone, and the milestone is the thing to track",
			taskID, waiting, MaxWaitingOn)
	}
	if entries > MaxEntries {
		return invalid("task %s would name %d atlas entries and the "+
			"maximum is %d: work that touches more systems than that is "+
			"about the company at large, and its project is the thing to link",
			taskID, entries, MaxEntries)
	}
	if other > MaxOtherRelations {
		return invalid("task %s would carry %d links, duplicates, "+
			"page references and atlas entries together and the maximum is "+
			"%d: a set larger than that is a field rather than a relation",
			taskID, other, MaxOtherRelations)
	}
	return nil
}

// settleDependents resolves the BLOCKER's half against one consistent read.
func settleDependents(current Task, patch TaskPatch) (TaskPatch, error) {
	if patch.Depend == nil {
		if patch.Dependents != nil {
			if err := checkDependents(current.ID, *patch.Dependents); err != nil {
				return patch, err
			}
		}
		return patch, nil
	}
	if patch.Dependents != nil {
		return patch, invalid("this patch on task %s carries both a "+
			"dependent gesture and a whole dependent set — a caller states one "+
			"or the other", current.ID)
	}
	next := slices.Clone(current.Dependents)
	for _, drop := range patch.Depend.Remove {
		next = slices.DeleteFunc(next, func(have string) bool { return have == drop })
	}
	for _, add := range patch.Depend.Add {
		switch {
		case add == "":
			return patch, invalid("a dependent gesture on task %s "+
				"names no task", current.ID)
		case add == current.ID:
			return patch, invalid("task %s cannot wait on itself",
				current.ID)
		case slices.Contains(next, add):
			continue
		}
		next = append(next, add)
	}
	if err := checkDependents(current.ID, next); err != nil {
		return patch, err
	}
	patch.Depend = nil
	patch.Dependents = &next
	return patch, nil
}

// checkDependents is [MaxDependents], enforced where the set grows by one. It
// is a SCOPE bound: a blocker's close rewrites every dependent, which its
// record must enumerate in its declared scope.
func checkDependents(taskID string, dependents []string) error {
	if len(dependents) <= MaxDependents {
		return nil
	}
	return invalid("%d tasks would wait on task %s and the maximum "+
		"is %d — every one of them is an object a close of this task has to "+
		"name in its own scope", len(dependents), taskID, MaxDependents)
}

// relationKindList is the kinds, for a refusal a person reads.
func relationKindList() string {
	out := make([]string, 0, len(RelationKinds))
	for _, k := range RelationKinds {
		out = append(out, string(k))
	}
	return strings.Join(out, ", ")
}

// scopeForDependents widens a task write's scope to the dependents its apply
// will write, on EVERY patch: `maintainDeps` rewrites the dependents' rows on
// every task apply, whatever the patch changed.
//
// It reads BEFORE the request, not inside the decide: the publisher probes the
// deferral index with the request's scope and the applier files under the
// envelope's ([envelopeOf]), so the two must be one set. A dependent arriving
// in between is caught by [ScopeSet.covers] inside the decide.
func (w *Writer) scopeForDependents(ctx context.Context, id string,
	scope ScopeSet) (ScopeSet, error) {

	if w.db.IsZero() {
		// NO REPLICATED ESTATE, NOTHING TO ENUMERATE: the scope stays as
		// it is rather than refusing every write. Such a writer cannot
		// author a dependency ([Writer.Depend] refuses without one), and
		// one another node wrote is refused by [ScopeSet.covers] inside
		// the decide.
		return scope, nil
	}
	var (
		dependents []string
		filed      map[string]string
	)
	if err := w.db.Read(ctx, func(tx *sql.Tx) error {
		current, held, err := readTask(ctx, tx, id)
		if err != nil || !held {
			// NOT HELD IS NOT AN ERROR HERE: the decide refuses a
			// task this node lacks; this read only widens a scope.
			return err
		}
		dependents = current.Dependents
		filed, err = dependentsFiled(ctx, tx, dependents)
		return err
	}); err != nil {
		return scope, fmt.Errorf("worklet: read task %s's dependents to scope "+
			"its write: %w", id, err)
	}
	return scope.withObjects(id, dependents, filed), nil
}

// tasksFiledStatement is [dependentsFiled]' read of where n tasks are filed,
// named for the plan test.
func tasksFiledStatement(n int) string {
	return `SELECT id, project_key FROM tracker_tasks WHERE id IN (` + placeholders(n) + `)`
}

// dependentsFiled is where each of these tasks is filed (its project), for the
// ones this node holds. READ FROM THE DEPENDENTS' OWN ROWS: dependencies cross
// projects, and naming a dependent under the blocker's project names a path its
// deferred records are not filed under.
func dependentsFiled(ctx context.Context, tx *sql.Tx, ids []string) (map[string]string, error) {
	filed := make(map[string]string, len(ids))
	if len(ids) == 0 {
		return filed, nil
	}
	rows, err := tx.QueryContext(ctx, tasksFiledStatement(len(ids)), anyOf(ids)...)
	if err != nil {
		return nil, fmt.Errorf("worklet: read where %d dependent(s) are filed: %w",
			len(ids), err)
	}
	defer rows.Close()
	for rows.Next() {
		var id, project string
		if err := rows.Scan(&id, &project); err != nil {
			return nil, fmt.Errorf("worklet: read where a dependent is filed: %w", err)
		}
		filed[id] = project
	}
	return filed, rows.Err()
}

// withObjects turns a sentinel scope into the enumeration that also names
// these ids, keeping whatever the scope already stated. The subject's own term
// is kept, since an enumeration carries no sentinel.
//
// Each dependent is named under the project filed gives; one this node does not
// hold is claimed by the DOMAIN, the one term certain to contain it. That
// over-claims, the safe direction.
func (s ScopeSet) withObjects(subject string, ids []string, filed map[string]string) ScopeSet {
	if len(ids) == 0 {
		return s
	}
	terms := make([]ScopeTerm, 0, len(s.Terms)+len(ids)+1)
	if s.Subject {
		terms = append(terms, ScopeTerm{
			Kind: TermObject, Container: s.Container, ID: subject,
		})
	}
	terms = append(terms, s.Terms...)
	for _, id := range ids {
		if s.namesFiled(terms, id, filed) {
			continue
		}
		project, held := filed[id]
		if !held {
			terms = append(terms, ScopeTerm{Kind: TermDomain})
			continue
		}
		terms = append(terms, ScopeTerm{
			Kind: TermObject, Container: project, ID: id,
		})
	}
	return ScopeSet{Terms: terms}
}

// namesFiled reports whether terms already claim id where filed says it is:
// its object term under its own project, its project's container, or the
// domain. The container counts because [ScopeSet.capped] folds an enumeration
// past [MaxScopeTerms] into its largest project's container.
func (ScopeSet) namesFiled(terms []ScopeTerm, id string, filed map[string]string) bool {
	project, held := filed[id]
	return slices.ContainsFunc(terms, func(t ScopeTerm) bool {
		switch t.Kind {
		case TermDomain:
			return true
		case TermContainer:
			return held && t.named() == project
		case TermObject:
			return held && t.ID == id && t.Container == project
		}
		return false
	})
}

// covers reports whether this scope already names every one of these objects
// where filed says each one is: WHERE, not only WHICH, since a dependent may
// have moved project since the read that built the scope.
func (s ScopeSet) covers(ids []string, filed map[string]string) error {
	if len(ids) == 0 {
		return nil
	}
	for _, id := range ids {
		if s.namesFiled(s.Terms, id, filed) {
			continue
		}
		// REFUSED, NOT SILENTLY WIDENED: the publisher probed the
		// deferral index with the request's scope, never this one.
		return fmt.Errorf("worklet: task %s waits on this one and this write's "+
			"scope does not name it, so the record would not cover an object "+
			"its own apply writes — re-run it, and the second attempt "+
			"enumerates the dependents that are there now: %w",
			id, statelog.ErrConflict)
	}
	return nil
}

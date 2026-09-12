package tracker

import (
	"context"
	"database/sql"
	"fmt"
	"slices"
	"strings"

	"github.com/crewlet/crewlet/internal/statelog"
)

// The two halves of a dependency, and why each is a gesture rather than a set.
//
// A dependency is the one relation this design MIRRORS: `waiting_on` is
// authored on the dependent, and the blocker carries the dependent's id in
// [Task.Dependents] so a close can name who it unblocks without a reverse scan
// over every task in the company.
//
// Both halves are collections a record carries WHOLE, and both are bounded —
// [MaxWaitingOn], [MaxOtherRelations], [MaxDependents]. That combination is
// what makes a caller-formed set wrong twice over: it discards every edge that
// arrived between the caller's read and its write, and it has nothing to count
// the cap against, because the set a cap governs is the one the write lands
// on. So both travel as an INTENT and are settled here, inside the writer's
// decide snapshot, exactly as [settleWatch] settles the watchers.

// settleRelations resolves a relation gesture into the whole collection.
//
// INSIDE THE DECIDE SNAPSHOT, for the reason [settleWatch]'s doc gives: the
// set a gesture is resolved against has to be the set the record lands on.
// It returns a COPY rather than writing through the patch it was given,
// because Decide runs again on a retry and a resolution folded into the
// captured patch would compound across attempts.
func settleRelations(current Task, patch TaskPatch) (TaskPatch, error) {
	if patch.Relate == nil {
		// A WHOLE SET STILL HAS TO FIT. The gesture is how a tool
		// states relations, but the merge sequence writes its own
		// collection, and a cap enforced on one path only is a cap the
		// second writer walks straight past.
		if patch.Relations != nil {
			if err := checkRelations(current.ID, *patch.Relations); err != nil {
				return patch, err
			}
		}
		return patch, nil
	}
	if patch.Relations != nil {
		// BOTH SPELLINGS AT ONCE IS A PROGRAMMING ERROR, refused rather
		// than resolved in some order: one of them is a delta and the
		// other is the whole set, and whichever won would silently
		// discard the other.
		return patch, fmt.Errorf("tracker: this patch on task %s carries both a "+
			"relation gesture and a whole relation set — a caller states one "+
			"or the other", current.ID)
	}
	next, err := patch.Relate.resolve(current)
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
			return nil, fmt.Errorf("tracker: a relation gesture on task %s "+
				"states both a whole set and a delta of %d added and %d "+
				"removed — a caller states one or the other",
				current.ID, len(r.Add), len(r.Remove))
		}
		return dedupeRelations(r.Set), nil
	}
	next := slices.Clone(current.Relations)
	for _, drop := range r.Remove {
		next = slices.DeleteFunc(next, func(have Relation) bool {
			// ON (Kind, Other) ALONE. A note is something somebody
			// typed on an edge rather than part of the edge's
			// identity, so removing one does not require quoting it
			// back — and a caller that had to would be removing the
			// edge it last read rather than the edge that is there.
			return have.Kind == drop.Kind && have.Other == drop.Other
		})
	}
	for _, mark := range r.Final {
		at := slices.IndexFunc(next, func(have Relation) bool {
			return have.Kind == mark.Kind && have.Other == mark.Other
		})
		if at < 0 {
			// THE EDGE WENT AWAY, which is the repair succeeding by
			// another route: somebody removed the dependency the duty
			// was about to give up on. Marking a relation that is not
			// there would ADD one.
			continue
		}
		next[at].OneSidedFinal = true
	}
	for _, add := range r.Add {
		if slices.ContainsFunc(next, func(have Relation) bool {
			return have.Kind == add.Kind && have.Other == add.Other
		}) {
			// AN EDGE THAT IS ALREADY THERE IS NOT AN ERROR. A
			// retried turn re-states its own edges, and an edge's
			// durable identity is (Kind, Other) — so the second
			// statement is the same edge rather than a second one,
			// and refusing it would fail the retry the engine's own
			// redelivery guarantees make ordinary.
			continue
		}
		next = append(next, add)
	}
	return next, nil
}

// dedupeRelations keeps the FIRST of each (Kind, Other), order preserved.
//
// Deterministically, because every node stores what this record says: two
// spellings of one edge inside a `set` would otherwise be two rows on the node
// that applied the record and one on the node that re-derived the document.
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
//
// TWO NUMBERS RATHER THAN ONE, which is the design's own shape: a dependency
// edge costs a mirrored commit on the blocker and a row in the dependency
// table that every close scans, while a `linked` edge is inert and costs
// bytes. So `waiting_on` is bounded at [MaxWaitingOn] and everything else
// together at [MaxOtherRelations].
func checkRelations(taskID string, relations []Relation) error {
	waiting, other := 0, 0
	for _, r := range relations {
		switch {
		case !r.Kind.Valid():
			return fmt.Errorf("tracker: task %s carries a relation of kind %q, "+
				"and the kinds are: %s", taskID, r.Kind, relationKindList())
		case r.Other == "":
			return fmt.Errorf("tracker: task %s carries a %s relation naming no "+
				"other task", taskID, r.Kind)
		case r.Other == taskID:
			// A SELF-EDGE IS REFUSED RATHER THAN DROPPED. As a
			// `waiting_on` it is a task that blocks itself, which
			// the dependency table would read as permanently
			// blocked and no close could ever clear.
			return fmt.Errorf("tracker: task %s carries a %s relation to itself",
				taskID, r.Kind)
		case len(r.Note) > MaxRelationNote:
			return fmt.Errorf("tracker: the note on task %s's %s relation to %s "+
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
		return fmt.Errorf("tracker: task %s would wait on %d tasks and the "+
			"maximum is %d — a task with more blockers than that is a "+
			"milestone, and the milestone is the thing to track",
			taskID, waiting, MaxWaitingOn)
	}
	if other > MaxOtherRelations {
		return fmt.Errorf("tracker: task %s would carry %d links, duplicates "+
			"and page references together and the maximum is %d — a set "+
			"larger than that is a field rather than a relation",
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
		return patch, fmt.Errorf("tracker: this patch on task %s carries both a "+
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
			return patch, fmt.Errorf("tracker: a dependent gesture on task %s "+
				"names no task", current.ID)
		case add == current.ID:
			return patch, fmt.Errorf("tracker: task %s cannot wait on itself",
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

// checkDependents is [MaxDependents], enforced where the set grows by one.
//
// THE CAP IS A SCOPE BOUND BEFORE IT IS A FAN-OUT BOUND. A group change on a
// blocker rewrites two columns on every dependent, and the record has to
// ENUMERATE those objects in its declared scope — so a blocker past this many
// dependents is one whose own close could not state what it touches.
func checkDependents(taskID string, dependents []string) error {
	if len(dependents) <= MaxDependents {
		return nil
	}
	return fmt.Errorf("tracker: %d tasks would wait on task %s and the maximum "+
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

// scopeForStatus widens a status write's scope to the dependents its apply
// will write.
//
// # Why this read is BEFORE the request rather than inside its decide
//
// The publisher probes the deferral index with the REQUEST's scope and the
// applier files a deferral under the ENVELOPE's — see [envelopeOf] — so the
// two have to be one set, and the request's is fixed before the decide runs.
// A scope widened only inside the decide would be a record filed under objects
// its own writer never probed.
//
// Reading it early makes it possible for a dependent to arrive in between, so
// the decide re-checks the claim with [ScopeSet.covers] and refuses a write
// whose scope came up short. That is the honest pairing: an early read is safe
// exactly when something later proves it was still right.
//
// It returns the scope unchanged when the task has no dependents, which is the
// overwhelming majority of tasks — a create-and-close never reads a row here
// at all, because the enumeration is gated on [TaskPatch.Status] being present.
func (w *Writer) scopeForStatus(ctx context.Context, id, project string,
	scope ScopeSet) (ScopeSet, error) {

	if w.db == nil {
		// A WRITER WITH NO REPLICATED ESTATE CANNOT ENUMERATE, and the
		// honest move is to leave the scope alone rather than refuse
		// every status write it makes. Such a writer cannot have
		// AUTHORED a dependency either — [Writer.Depend] reads its
		// counterparties through this same estate and refuses without
		// one — so the ordinary case here is a task with no dependents
		// and a scope that is already right.
		//
		// The case it cannot rule out is a dependent another node wrote,
		// and that one is caught where it matters: [ScopeSet.covers]
		// runs inside the decide, against the rows this write actually
		// lands on, and refuses naming the object the claim is short by.
		return scope, nil
	}
	var dependents []string
	if err := w.db.Replicated().Read(ctx, func(tx *sql.Tx) error {
		current, held, err := readTask(ctx, tx, id)
		if err != nil || !held {
			// NOT HELD IS NOT AN ERROR HERE. The decide is what
			// refuses a task this node does not have, with the
			// message that says so; this read only widens a scope,
			// and widening nothing leaves that refusal to the one
			// place that states it properly.
			return err
		}
		dependents = current.Dependents
		return nil
	}); err != nil {
		return scope, fmt.Errorf("tracker: read task %s's dependents to scope "+
			"its status write: %w", id, err)
	}
	return scope.withObjects(project, id, dependents), nil
}

// withObjects turns a sentinel scope into the enumeration that also names
// these ids, keeping whatever the scope already stated.
//
// THE SUBJECT IS ALWAYS AMONG THEM. A scope built from terms carries no
// sentinel, so dropping the subject's own term would claim the dependents and
// not the task being written.
func (s ScopeSet) withObjects(container, subject string, ids []string) ScopeSet {
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
		if slices.ContainsFunc(terms, func(t ScopeTerm) bool {
			return t.Kind == TermObject && t.ID == id
		}) {
			continue
		}
		// THE BLOCKER'S OWN CONTAINER, because a dependent's project
		// is not on the blocker's row and cross-project dependencies
		// are allowed. A container term is the coarser claim of the
		// two, so naming this one over-claims rather than under-claims
		// — and an under-claim is the failure this whole path exists
		// to avoid.
		terms = append(terms, ScopeTerm{
			Kind: TermObject, Container: container, ID: id,
		})
	}
	return ScopeSet{Terms: terms}
}

// covers reports whether this scope already names every one of these objects.
func (s ScopeSet) covers(ids []string) error {
	if len(ids) == 0 {
		return nil
	}
	for _, id := range ids {
		if slices.ContainsFunc(s.Terms, func(t ScopeTerm) bool {
			return t.Kind == TermObject && t.ID == id
		}) {
			continue
		}
		// NAMED, AND NOT SILENTLY WIDENED. The request's scope is what
		// the publisher probed the deferral index with, and this record
		// is about to be filed under a different one — so a write that
		// went ahead would be a record whose own writer never looked
		// where it is filed.
		return fmt.Errorf("tracker: task %s waits on this one and this write's "+
			"scope does not name it, so the record would not cover an object "+
			"its own apply writes — re-run it, and the second attempt "+
			"enumerates the dependents that are there now: %w",
			id, statelog.ErrConflict)
	}
	return nil
}

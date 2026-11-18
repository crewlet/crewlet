package worklet

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/store"
)

// NewRows builds the publisher's read seam for this domain; spec is the log
// the publisher writes, whose checkpoint and anchors the seam reads. The
// framework implements it: the arbitration anchor, the deferred record and its
// scope index live in its tables, and a second copy of the containment probe
// over them, where a wrong clause is silent data loss, would be a second
// chance to get it wrong. Only the guards ([taskGuards]) are this domain's.
func NewRows(db store.ReplicatedReader, spec statelog.StreamSpec) (statelog.Rows, error) {
	return statelog.NewRows(db, Domain{}, spec, taskGuards)
}

// taskGuardsStatement is [taskGuards]' read, named for the plan test.
const taskGuardsStatement = `
		SELECT
			(SELECT COUNT(*) FROM tracker_deletions WHERE task_id = ?),
			(SELECT COUNT(*) FROM tracker_tasks WHERE id = ?)`

// taskGuards answers the two object-level facts a first write needs. The
// deletion marker is permanent where the guarding row is not: a task below the
// trim floor has only its row to prove it existed, while a purge's marker
// outlives the row and makes the removal irreversible. Only a task has either:
// every other object here is created by its first record and removed by
// nothing (a file's removal stamp is lifted by the next put at its path), so
// false for both is correct, not an omission.
func taskGuards(ctx context.Context, tx *sql.Tx, subj statelog.Subject) (
	deleted, guard bool, err error) {

	if ObjectKind(subj.Kind) != KindTask {
		return false, false, nil
	}
	var removed, present int
	err = tx.QueryRowContext(ctx, taskGuardsStatement,
		subj.ID, subj.ID).Scan(&removed, &present)
	if err != nil {
		return false, false, fmt.Errorf("worklet: read the guards on task %s: %w",
			subj.ID, err)
	}
	return removed > 0, present > 0, nil
}

// ReadScope is the closure a READ is about, in the alphabet a record's scope
// resolves into, so the coverage probe is one comparison. A query with no
// container is the DOMAIN: a read that cannot say what it is about is one
// every deferred record concerns, so "is this complete" honestly answers "no".
func ReadScope(q Query) statelog.ScopeSet {
	var paths []string
	switch {
	case q.Scope.Project != "":
		paths = append(paths, ScopeTerm{
			Kind: TermContainer, ID: q.Scope.Project,
		}.Path())
	case q.Scope.Workspace:
		// The Everything level is every container, not [WorkspaceContainer]
		// (where an object with no project lives): naming only that probes a
		// container no task is filed in, so a board with a deferred record
		// in any project would call itself complete. The container level is
		// the tightest term covering every project and nothing else; the
		// families sit beside it, and naming a field adds the catalogue below.
		paths = append(paths, statelog.ScopePath(pathDomain, pathContainer))
	default:
		paths = append(paths, pathDomain)
	}
	// A DISJUNCTION WIDENS THE CLOSURE, because every branch is part of
	// the same question: an answer is complete only if every branch's rows
	// were there to read.
	for _, branch := range q.Any {
		paths = append(paths, ReadScope(branch).Paths...)
	}
	// AND SO DOES A CUSTOM FIELD: `f.<slug>` resolves against the
	// declarations, so a node holding back an undecodable catalogue
	// record would resolve it against a stale one (wrong column, wrong
	// option id) and call the answer complete. The declarations live
	// under the catalogue FAMILY, which no container's closure reaches.
	if namesAField(q) || q.sortsBy("type") {
		paths = append(paths,
			ScopeTerm{Kind: TermFamily, ID: string(KindCatalogue)}.Path())
	}
	// AND AN ORDER BY THE SEATS' NAMES, which is read from the roster
	// ([assigneeOrder]), on the same reasoning: the order of a page drawn
	// from a roster this node holds back is not the fleet's.
	if q.sortsBy("assignee") {
		paths = append(paths,
			ScopeTerm{Kind: TermFamily, ID: string(KindRoster)}.Path())
	}
	return statelog.ScopeSet{Paths: paths}.Normalised()
}

// namesAField reports whether a query is answered from the catalogue too: a
// ref anywhere [collectFieldRefs] finds one (filter, sort, total, grouping
// axis), since a closure covering only the filter would let a board grouped on
// a stale declaration report itself complete.
func namesAField(q Query) bool {
	refs := map[string]bool{}
	collectFieldRefs(q, refs)
	return len(refs) > 0
}

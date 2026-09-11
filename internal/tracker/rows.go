package tracker

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/store"
)

// NewRows builds the publisher's read seam for this domain.
//
// THE FRAMEWORK IMPLEMENTS IT. Three of the four things a snapshot returns are
// the framework's own — the arbitration anchor, the deferred record and its
// scope index all live in tables it writes — and the two-clause containment
// probe over them is the one piece of SQL in this design where getting a clause
// wrong is silent data loss rather than a wrong answer. A second copy here
// would be a second chance to get it wrong, in the package least likely to be
// the one somebody re-reads.
//
// What is genuinely this domain's is the pair of GUARDS below.
func NewRows(db *store.DB) (statelog.Rows, error) {
	return statelog.NewRows(db, Domain{}, taskGuards)
}

// taskGuards answers the two object-level facts a first write needs.
//
// THE DELETION MARKER IS PERMANENT WHERE A GUARDING ROW IS NOT: a task below
// the trim floor has no record left on the log to prove it existed, and its own
// row is what still says so — while a purge's marker outlives the row itself
// and is what makes the removal irreversible.
//
// Only a task has either. Every other object in this domain is created by its
// first record and removed by nothing, so answering false for both is the
// correct answer rather than an omission.
func taskGuards(ctx context.Context, tx *sql.Tx, subj statelog.Subject) (
	deleted, guard bool, err error) {

	if ObjectKind(subj.Kind) != KindTask {
		return false, false, nil
	}
	var removed, present int
	err = tx.QueryRowContext(ctx, `
		SELECT
			(SELECT COUNT(*) FROM tracker_deletions WHERE task_id = ?),
			(SELECT COUNT(*) FROM tracker_tasks WHERE id = ?)`,
		subj.ID, subj.ID).Scan(&removed, &present)
	if err != nil {
		return false, false, fmt.Errorf("tracker: read the guards on task %s: %w",
			subj.ID, err)
	}
	return removed > 0, present > 0, nil
}

// ReadScope is the closure a READ is about.
//
// It is the same alphabet a record's own scope resolves into, which is what
// makes the coverage probe one comparison rather than a translation between
// two vocabularies. A query with no container is the DOMAIN — not because it
// touches everything, but because a read that cannot say what it is about is
// one every deferred record concerns, and the honest answer to "is this
// complete" is then "no".
func ReadScope(q Query) statelog.ScopeSet {
	var paths []string
	switch {
	case q.Scope.Project != "":
		paths = append(paths, ScopeTerm{
			Kind: TermContainer, ID: q.Scope.Project,
		}.Path())
	case q.Scope.Workspace:
		paths = append(paths, ScopeTerm{
			Kind: TermContainer, ID: WorkspaceContainer,
		}.Path())
	default:
		paths = append(paths, pathDomain)
	}
	// A DISJUNCTION WIDENS THE CLOSURE, because every branch is part of
	// the same question: an answer is complete only if every branch's rows
	// were there to read.
	for _, branch := range q.Any {
		paths = append(paths, ReadScope(branch).Paths...)
	}
	// AND SO DOES A CUSTOM FIELD, because such a query is answered from
	// the CATALOGUE as well as from the rows: `f.<slug>` resolves against
	// the declarations, and a node holding a catalogue record it cannot
	// decode would resolve the slug against a stale declaration — the
	// wrong column, the wrong option id — and report the answer complete.
	// The declarations live under the catalogue FAMILY, which no
	// container's closure reaches.
	if namesAField(q) {
		paths = append(paths,
			ScopeTerm{Kind: TermFamily, ID: string(KindCatalogue)}.Path())
	}
	return statelog.ScopeSet{Paths: paths}.Normalised()
}

// namesAField reports whether a query is answered from the catalogue too.
//
// EVERY PLACE A REF CAN APPEAR, which is [collectFieldRefs]' own set: a filter,
// a sort, a total and a grouping axis all resolve one, and a closure that
// covered only the filter would leave a board grouped on a stale declaration
// reporting itself complete.
func namesAField(q Query) bool {
	refs := map[string]bool{}
	collectFieldRefs(q, refs)
	return len(refs) > 0
}

package gitlet

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"

	"github.com/crewlet/crewlet/internal/gitref"
	"github.com/crewlet/crewlet/internal/queue/topics"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/store"
)

// THE HOME'S DUE CHANGE WORK, READ FROM THE ROWS: a `changes_due` read
// ([ReadChangesDue]) is what a repository's home Gitlet node polls, and is
// nudged into reading, to learn the changes it owes work. A LIVE change on an
// ACTIVE repository, its target a live branch, owes its home:
//   - its head revision's FACTS ([Revise], or [ChangeLanded] where the target
//     holds the head) where code_revision_facts holds none against that branch
//     at its repository's owners_digest and human_paths_digest;
//   - its head revision's MERGE ([Mergeability], or lands, as above) where
//     code_mergeability holds none against that branch.

// It owes too a REFRESH of its readiness ([Mergeability.Refresh], over the
// merge its row holds, running none) where the readiness was judged at a
// code_changes.version or code_conversations.readiness_version since moved, at
// a code_mergeability.checks_position a head check for its target was queued
// or ended past since (THE CHECK TERM), or by a chart earlier than
// [ChangesDueQuery.ChartEpoch] (THE CHART TERM: who may approve and who
// maintains are the chart's, which no record moves); and a RECHECK of a merge
// behind its target's tip or undecided, once recorded at or before
// [ChangesDueQuery.RecheckBefore] (the home's clock less its refresh), so a
// target moving on every push is rechecked at the home's pace, never its own.

// Any change on an active repository, LIVE OR ENDED, whatever its target, owes
// a SETTLING OF ITS ASKS ([AskSettle]) where they no longer match its state: an
// ended change whose asks still wait (code_conversations.asked), or a reopened
// live one holding asks its end left (code_conversations.ended). A change's
// lifecycle records write nothing of its conversation, and the listing of the
// changes waiting on a reviewer walks the waiting asks alone, so this is how
// an ask stops waiting when its change ends.

// Every record the home makes takes its change off the list by its own row (a
// merge and a refresh store the versions and chart they judged at; a settling
// moves the counts), so the rows and the home's chart are the whole work list.
// A chart apply makes every live change due ONE refresh, which the decide
// stores even where the answer did not move, since the row must say the later
// chart judged it; a committer behind that chart judges by its own, or refuses
// it superseded where nothing else moved, so the change stays due until it
// applies the activation too.

// A change on an ARCHIVED OR DELETED repository is nobody's work: every write
// would be refused (repo_archived, repo_deleted), and listed it would be read
// on every pass for ever. Nor is one whose target branch is gone, until a push
// makes it again or the change is retargeted: there is no tip to read against.
// A PULL MIRROR'S live changes stay its home's work though nothing merges into
// one (repo_mirrored): its source merges them, and the home's merge finds the
// head inside the target a sync moved, [ChangeLanded]. Facts and merges are
// records a pull mirror takes, so each takes the change off the list until a
// sync moves its target again.

// TWO SHAPES: with no repository it pages the repositories holding a due
// change, by id — a home's backstop asks about its whole placement, a
// repository at a time being no question a pass can afford — walking
// code_changes_open_idx (the live changes alone) grouped on its leading
// column, so a page is read in index order and never sorted. With one
// ([ChangesDueQuery]) it pages that repository's changes owing facts, a merge
// or a refresh by source branch, the same index's range, each with its
// target's tip by code_refs' key; the path owners only on a page naming a
// change owing its facts, in the same snapshot; and on its FIRST PAGE up to
// [ChangesDueQuery.Rechecks] rechecks, THE OLDEST MERGE FIRST. Each change is
// checked against its facts' key, its mergeability's and its conversation's.

// OLDEST FIRST, because a page cut at the home's budget in source-branch order
// would recheck the same first changes at every refresh and the last never. It
// is the one sort a listing here makes, of one repository's live changes
// behind a moved target — at most one a live branch (code_changes_open_idx),
// so MaxRefsPerRepo bounds it — since no index orders a repository's changes
// by when their merge was recorded without one written by the records of two
// subjects.

// MEASURED (BenchmarkChangesDue -benchtime 3x; two E-cores of a Core Ultra 7 265U, loaded,
// 2026-11-01): 1 000 repositories of 10 live changes with nothing due beside
// ONE of 10 000 behind a moved target. A backstop pass's listing of the
// repositories, every page, is 215 ms, some 10 µs a live change; that
// repository's due changes 168 ms; its first page with 64 rechecks 226 ms, the
// sort some 58 ms of it. EVERY GITLET NODE LISTS EVERY REPOSITORY once a
// Backstop (internal/gitlet/home), so G nodes ask G × 215 ms of reads a minute
// here, about 1 % of one core for three. Both grow with the live changes: a
// listing nearing a tenth of a Backstop (6 s, some 600 000 live changes) is
// what moves it off the walk of every live change.

// READ STALE BY DEFAULT, as every duty listing is: a change listed a moment
// behind costs a record the decide refuses superseded, and one a moment early
// is listed again at the next pass. A home nudged by a record reads at its
// position instead ([checkFreshness]: `session` beside a code log position),
// so the change it was nudged about is in the rows it reads.

// validateChangesDue is [ReadQuery.validate] for a `changes_due` read: the
// engine's own, so it names no asker or seat (the `compaction_due` read's
// reason), and nothing a listing names beyond its own query, since a page is
// MaxSnapshotItems rows.
func (q ReadQuery) validateChangesDue() error {
	refused := func(field, why string) error { return refusedf("%w: %s: %s", ErrInvalid, field, why) }
	switch d := q.ChangesDue; {
	case q.Asker != (Principal{}):
		return refused("asker", "a changes_due read is the engine's own, which no principal makes")
	case q.CeilingSeat != "":
		return refused("ceiling_seat", "a changes_due read is the engine's own, which no seat makes for another")
	case d == nil:
		return refused("changes_due", "a changes_due read names the instant its home's recheck of a merge is "+
			"due by, and, for one repository's due changes, the repository")
	case d.RecheckBefore.IsZero():
		return refused("changes_due.recheck_before", "a changes_due read names the instant a merge recorded at "+
			"or before is due again: its home's clock less its refresh")
	case d.RepoID != "" && !canonicalUUID(d.RepoID):
		return refused("changes_due.repo_id", fmt.Sprintf("%q is not a repository's id in its canonical "+
			"lowercase spelling", d.RepoID))
	case d.Rechecks < 0 || d.Rechecks > MaxSnapshotItems:
		return refused("changes_due.rechecks", fmt.Sprintf("%d; a page names 0 to %d rechecks (MaxSnapshotItems)",
			d.Rechecks, MaxSnapshotItems))
	case d.RepoID == "" && d.Rechecks != 0:
		return refused("changes_due.rechecks", "the listing of repositories names the repositories alone, and a "+
			"repository's read its rechecks")
	case d.ChartEpoch < 0:
		return refused("changes_due.chart_epoch", fmt.Sprintf("%d; a chart's activation is a Unix millisecond, "+
			"or 0 for a home holding none", d.ChartEpoch))
	}
	for _, f := range []struct {
		field string
		set   bool
	}{
		{"repo", q.Repo != ""}, {"status", len(q.Status) > 0}, {"prefix", q.Prefix != ""},
		{"introduced", q.Introduced != nil}, {"run_pushes", q.RunPushes != nil}, {"min_packs", q.MinPacks != 0},
		{"limit", q.Limit != 0}, {"brief", q.Brief}, {"push", q.Push != ""}, {"change", q.Change != 0},
		{"state", q.State != ""}, {"owner", q.Owner != ""}, {"work", q.Work != ""},
		{"reviewer", q.Reviewer != ""}, {"needs", q.Needs != ""}, {"window", q.Window != ""}, {"part", q.Part != ""},
		{"checks", q.Checks != nil}, {"checks_due", q.ChecksDue != nil}, {"check_box", q.CheckBox != nil},
		{"path_owners", q.PathOwners}, {"gates", q.Gates}, {"detail_only", q.DetailOnly},
		{"finding_ids", len(q.FindingIDs) > 0},
		{"expunge_tips", q.ExpungeTips != nil},
		{"scanned_before", !q.ScannedBefore.IsZero()},
		{"branch_changes", q.BranchChanges != nil},
		{"target", q.Target != ""},
		{"checks_running", q.ChecksRunning != nil}, {"mirror_push", q.MirrorPush != nil},
		{"default_tips", q.DefaultTips != nil},
	} {
		if f.set {
			return refused(f.field, "a changes_due read lists the repositories a home owes change work in, or "+
				"one repository's due changes by its id, a page of MaxSnapshotItems at a time, and names "+
				"nothing else")
		}
	}
	return nil
}

// changesDueQuery is a set read of the domain for the listing of repositories,
// as every duty listing is, and of the one repository for its due changes (its
// records' scope names its path, a conversation's among them), so a repository
// a deferred record holds back is read as its rows stand; the decide that
// follows judges again from its own snapshot.
func (q ReadQuery) changesDueQuery(fresh statelog.Freshness) statelog.Query {
	if q.ChangesDue.RepoID == "" {
		return fresh.Query(statelog.ScopeSet{Paths: []string{DomainPath()}}, true)
	}
	return fresh.Query(statelog.ScopeSet{Paths: []string{RepoPath(q.ChangesDue.RepoID)}}, true)
}

// changesDue serves one page of a `changes_due` read — see this file's head.
func (r *Reader) changesDue(ctx context.Context, tx *sql.Tx, q ReadQuery, out *ReadAnswer) error {
	if q.ChangesDue.RepoID == "" {
		return r.dueRepos(ctx, tx, q, out)
	}
	return r.dueChanges(ctx, tx, q, out)
}

// dueRepos serves a page of the repositories holding a due change, by id,
// after the cursor — the last id a page named.
func (r *Reader) dueRepos(ctx context.Context, tx *sql.Tx, q ReadQuery, out *ReadAnswer) error {
	if q.Cursor != "" && !canonicalUUID(q.Cursor) {
		return fmt.Errorf("%w: %q", ErrBadCursor, q.Cursor)
	}
	// TWO WALKS, MERGED: the live changes owing work or holding asks their end
	// left (code_changes_open_idx), and the ended changes whose asks still wait
	// (code_conversations_asked_idx), each at most a page of repositories after
	// the cursor in index order, so the first page of the two merged is the
	// first page of their union.
	live, err := reposOf(ctx, tx, dueReposSQL, q.Cursor, r.pageRows, store.EncodeTime(q.ChangesDue.RecheckBefore),
		q.ChangesDue.ChartEpoch)
	if err != nil {
		return err
	}
	ended, err := reposOf(ctx, tx, dueAskedReposSQL, q.Cursor, r.pageRows)
	if err != nil {
		return err
	}
	pg := page{maxRows: r.pageRows, maxB: r.pageBytes}
	for _, id := range mergeSorted(live, ended) {
		out.DueRepos = append(out.DueRepos, id)
		if pg.add(stringBytes(id) + 1) {
			out.Next = id
			break
		}
	}
	return nil
}

// reposOf is the repository ids query answers, bound to args.
func reposOf(ctx context.Context, tx *sql.Tx, query string, args ...any) ([]string, error) {
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("gitlet: list the repositories change work is due in: %w", err)
	}
	var ids []string
	if err := eachID(rows, func(id string) { ids = append(ids, id) }); err != nil {
		return nil, fmt.Errorf("gitlet: list the repositories change work is due in: %w", err)
	}
	return ids, nil
}

// mergeSorted is the union of two ascending lists of distinct ids, ascending
// and each id once.
func mergeSorted(a, b []string) []string {
	out := make([]string, 0, len(a)+len(b))
	for len(a) > 0 || len(b) > 0 {
		switch {
		case len(b) == 0 || (len(a) > 0 && a[0] < b[0]):
			out, a = append(out, a[0]), a[1:]
		case len(a) == 0 || b[0] < a[0]:
			out, b = append(out, b[0]), b[1:]
		default:
			out, a, b = append(out, a[0]), a[1:], b[1:]
		}
	}
	return out
}

// dueChanges serves a page of a repository's due changes by source branch
// after the cursor (the last source branch a page named), beside its path
// owners and gates where one owes its facts: none at all for a repository that
// is not active or not there.
func (r *Reader) dueChanges(ctx context.Context, tx *sql.Tx, q ReadQuery, out *ReadAnswer) error {
	repoID := q.ChangesDue.RepoID
	if q.Cursor != "" && !ValidRef(q.Cursor) {
		return fmt.Errorf("%w: %q", ErrBadCursor, q.Cursor)
	}
	var status string
	switch err := tx.QueryRowContext(ctx, dueRepoSQL, repoID).Scan(&status); {
	case errors.Is(err, sql.ErrNoRows):
		return nil
	case err != nil:
		return fmt.Errorf("gitlet: read repository %s's state: %w", repoID, err)
	case RepoStatus(status) != RepoStatusActive:
		return nil
	}
	// THE OWNERS AND THE GATES RIDE ONLY A PAGE NAMING A CHANGE OWING ITS
	// FACTS, read last in the same snapshot: most nudged reads name none, and
	// at their bounds they weigh three quarters of a mebibyte every read would
	// encode and ship for nothing. Their room ([MaxPathOwnersBytes],
	// [maxGatesBytes]) is reserved before the changes are read, so a page that
	// turns out to need them always fits them.
	pg := page{maxRows: r.pageRows, maxB: r.pageBytes - MaxPathOwnersBytes - maxGatesBytes}
	if err := dueChangesPage(ctx, tx, q, &pg, r.pageRows, out); err != nil {
		return err
	}
	if !slices.ContainsFunc(out.DueChanges, func(c ChangeDue) bool { return c.FactsDue }) {
		return nil
	}
	owners, err := pathOwnersIn(ctx, tx, repoID)
	if err != nil {
		return err
	}
	gates, err := gatesIn(ctx, tx, repoID)
	if err != nil {
		return err
	}
	out.PathOwners, out.Gates = owners, gates
	return nil
}

// maxGatesBytes is the most a repository's merge gates ([RuleGate]) weigh as
// JSON: the half mebibyte a due page reserves for them beside the owners.
const maxGatesBytes = MaxProtectionsPerRepo * (MaxGateBytes + gitref.MaxRefNameBytes + len(`{"pattern":"","gate":},`))

// dueChangesPage is a page of a repository's due changes into out — its asks
// due a settling and its rechecks first, on its first page — within pg.
func dueChangesPage(ctx context.Context, tx *sql.Tx, q ReadQuery, pg *page, pageRows int, out *ReadAnswer) error {
	repoID := q.ChangesDue.RepoID
	if q.Cursor == "" {
		if err := dueAskSettles(ctx, tx, repoID, pg, out); err != nil {
			return err
		}
	}
	if q.Cursor == "" && q.ChangesDue.Rechecks > 0 {
		if err := dueRechecks(ctx, tx, q, pg, out); err != nil {
			return err
		}
	}
	digests, err := matchDigestsIn(ctx, tx, repoID)
	if err != nil {
		return err
	}
	rows, err := tx.QueryContext(ctx, dueChangesSQL, repoID, q.Cursor, pageRows, q.ChangesDue.ChartEpoch,
		digests.owners, digests.humanPaths)
	if err != nil {
		return fmt.Errorf("gitlet: list repository %s's due changes: %w", repoID, err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var (
			c  ChangeDue
			at int64
		)
		if err := rows.Scan(&c.ID, &c.Number, &c.Revision, &c.Head, &c.SourceRef, &c.TargetRef, &c.Tip,
			&at, &c.FactsDue, &c.MergeDue, &c.RefreshDue); err != nil {
			return fmt.Errorf("gitlet: list repository %s's due changes: %w", repoID, err)
		}
		c.TipPosition = statelog.Unpack(topics.CodeLogStream, at)
		out.DueChanges = append(out.DueChanges, c)
		if pg.add(c.encodedBytes()) {
			out.Next = c.SourceRef
			break
		}
	}
	return rows.Err()
}

// dueAskSettles is a repository's changes whose asks its home owes a settling
// ([ReadAnswer.AskSettles]), at most [MaxAskSettlesPerRead], counted against
// the first page's budget: its ended changes with a waiting ask, by
// code_conversations_asked_idx (never the repository's history), then its live
// ones holding asks their end left, on the walk of its live changes.
func dueAskSettles(ctx context.Context, tx *sql.Tx, repoID string, pg *page, out *ReadAnswer) error {
	// EACH STATEMENT SPELLED AT ITS CALL, a constant the read-index gate
	// follows to its registered plan.
	due := func(rows *sql.Rows, err error, state ReviewerState) error {
		if err == nil {
			err = eachID(rows, func(id string) {
				d := AskDue{ChangeID: id, State: state}
				out.AskSettles = append(out.AskSettles, d)
				// COUNTED, NEVER A REASON TO STOP, as a recheck is: a few
				// dozen rows at most, and the page serves its changes after
				// them.
				pg.add(stringBytes(id) + len(d.State) + 32)
			})
		}
		if err != nil {
			return fmt.Errorf("gitlet: list repository %s's asks due a settling: %w", repoID, err)
		}
		return nil
	}
	rows, err := tx.QueryContext(ctx, endedAsksSQL, repoID, MaxAskSettlesPerRead)
	if err = due(rows, err, ReviewerEnded); err != nil {
		return err
	}
	left := MaxAskSettlesPerRead - len(out.AskSettles)
	if left <= 0 {
		return nil
	}
	rows, err = tx.QueryContext(ctx, reopenedAsksSQL, repoID, left)
	return due(rows, err, ReviewerRequested)
}

// dueRechecks is a repository's changes owing a recheck of their merge, the
// oldest first, at most as many as q asks, counted against the first page's
// budget.
func dueRechecks(ctx context.Context, tx *sql.Tx, q ReadQuery, pg *page, out *ReadAnswer) error {
	repoID := q.ChangesDue.RepoID
	rows, err := tx.QueryContext(ctx, dueRechecksSQL, repoID, store.EncodeTime(q.ChangesDue.RecheckBefore),
		q.ChangesDue.Rechecks)
	if err != nil {
		return fmt.Errorf("gitlet: list repository %s's due rechecks: %w", repoID, err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var (
			c           ChangeDue
			at, checked int64
		)
		if err := rows.Scan(&c.ID, &c.Number, &c.Revision, &c.Head, &c.SourceRef, &c.TargetRef, &c.Tip, &at,
			&checked); err != nil {
			return fmt.Errorf("gitlet: list repository %s's due rechecks: %w", repoID, err)
		}
		c.TipPosition, c.Checked = statelog.Unpack(topics.CodeLogStream, at), store.DecodeTime(checked)
		out.Rechecks = append(out.Rechecks, c)
		// COUNTED, NEVER A REASON TO STOP: the home asks a few dozen at most,
		// and a page always serves a change after them, so the next page
		// starts where it ends.
		pg.add(c.encodedBytes())
	}
	return rows.Err()
}

// The due listing's reads, each registered in plan_test.go: each walks
// code_changes_open_idx by [liveChanges], the predicate as the index declares
// it, or code_conversations_asked_idx for the asks still waiting, grouping the
// listing of repositories on the index's leading column, and reaches each
// row it checks by its key or, for a head's checks, code_checks' prefix.
// dueRechecksSQL alone sorts (this file's head says why).
//
// THE MERGEABILITY IS READ IN A SUBQUERY, NEVER JOINED: its `state` beside the
// change's would make [liveChanges], unqualified, an ambiguous column, which
// the driver refuses to plan. The listing of repositories asks [mergeHeld] of
// every live change, where a repository's own read asks its two halves apart.
const (
	dueReposSQL = `SELECT c.repo_id FROM code_changes AS c INDEXED BY code_changes_open_idx
		WHERE c.repo_id > ?1 AND ` + liveChanges + `
			AND (EXISTS (SELECT 1 FROM code_refs AS t INDEXED BY ` + refsKey + `
				WHERE t.repo_id = c.repo_id AND t.ref = c.target_ref AND t.target != ''
					AND (NOT EXISTS (SELECT 1 FROM code_revision_facts AS f CROSS JOIN code_repos AS o
							WHERE f.change_id = c.id AND f.n = c.revision AND f.target_ref = c.target_ref
								AND o.id = c.repo_id AND o.owners_digest = f.owners_digest
								AND o.human_paths_digest = f.human_paths_digest)
						OR NOT EXISTS (SELECT 1 FROM code_mergeability AS m
							WHERE ` + mergeHeld + `)
						OR EXISTS (SELECT 1 FROM code_mergeability AS m WHERE ` + refreshDue + `)))
				OR EXISTS (SELECT 1 FROM code_conversations AS v WHERE v.change_id = c.id AND v.ended > 0))
			AND EXISTS (SELECT 1 FROM code_repos AS r WHERE r.id = c.repo_id AND r.status = 'active')
		GROUP BY c.repo_id ORDER BY c.repo_id LIMIT ?2`
	dueAskedReposSQL = `SELECT v.repo_id FROM code_conversations AS v INDEXED BY ` + askedKey + `
		WHERE v.repo_id > ?1 AND asked > 0
			AND EXISTS (SELECT 1 FROM code_changes AS c WHERE c.id = v.change_id AND ` + endedChanges + `)
			AND EXISTS (SELECT 1 FROM code_repos AS r WHERE r.id = v.repo_id AND r.status = 'active')
		GROUP BY v.repo_id ORDER BY v.repo_id LIMIT ?2`
	// endedAsksSQL is one repository's ended changes whose asks still wait,
	// at most ?2, by the asked index, its partial predicate repeated
	// verbatim for changesWaitingSQL's reason; reopenedAsksSQL its live
	// changes holding asks their end left.
	endedAsksSQL = `SELECT v.change_id FROM code_conversations AS v INDEXED BY ` + askedKey + `
		WHERE v.repo_id = ?1 AND asked > 0
			AND EXISTS (SELECT 1 FROM code_changes AS c WHERE c.id = v.change_id AND ` + endedChanges + `)
		ORDER BY v.change_id LIMIT ?2`
	reopenedAsksSQL = `SELECT c.id FROM code_changes AS c INDEXED BY code_changes_open_idx
		WHERE c.repo_id = ?1 AND ` + liveChanges + `
			AND EXISTS (SELECT 1 FROM code_conversations AS v WHERE v.change_id = c.id AND v.ended > 0)
		LIMIT ?2`
	// askedKey is the index of a repository's changes holding an ask that
	// waits, partial on asked > 0; endedChanges a change, c, that closed or
	// merged.
	askedKey      = "code_conversations_asked_idx"
	endedChanges  = `c.state IN ('closed', 'merged')`
	dueRepoSQL    = `SELECT status FROM code_repos WHERE id = ?`
	dueChangesSQL = `SELECT c.id, c.number, c.revision, c.head_oid, c.source_ref, c.target_ref, t.target,
			t.position,
			NOT EXISTS (SELECT 1 FROM code_revision_facts AS f
				WHERE f.change_id = c.id AND f.n = c.revision AND f.target_ref = c.target_ref
					AND f.owners_digest = ?5 AND f.human_paths_digest = ?6),
			NOT EXISTS (SELECT 1 FROM code_mergeability AS m
				WHERE m.change_id = c.id AND m.revision = c.revision AND m.target_ref = c.target_ref),
			EXISTS (SELECT 1 FROM code_mergeability AS m WHERE ` + refreshDue + `)
		FROM code_changes AS c INDEXED BY code_changes_open_idx
		CROSS JOIN code_refs AS t INDEXED BY ` + refsKey + `
		WHERE c.repo_id = ?1 AND c.source_ref > ?2 AND ` + liveChanges + `
			AND t.repo_id = c.repo_id AND t.ref = c.target_ref AND t.target != ''
			AND (NOT EXISTS (SELECT 1 FROM code_revision_facts AS f
					WHERE f.change_id = c.id AND f.n = c.revision AND f.target_ref = c.target_ref
						AND f.owners_digest = ?5 AND f.human_paths_digest = ?6)
				OR NOT EXISTS (SELECT 1 FROM code_mergeability AS m
					WHERE m.change_id = c.id AND m.revision = c.revision AND m.target_ref = c.target_ref)
				OR EXISTS (SELECT 1 FROM code_mergeability AS m WHERE ` + refreshDue + `))
		ORDER BY c.source_ref LIMIT ?3`
	dueRechecksSQL = `SELECT c.id, c.number, c.revision, c.head_oid, c.source_ref, c.target_ref, t.target,
			t.position, (SELECT m.at FROM code_mergeability AS m WHERE m.change_id = c.id) AS checked
		FROM code_changes AS c INDEXED BY code_changes_open_idx
		CROSS JOIN code_refs AS t INDEXED BY ` + refsKey + `
		WHERE c.repo_id = ?1 AND ` + liveChanges + `
			AND t.repo_id = c.repo_id AND t.ref = c.target_ref AND t.target != ''
			AND EXISTS (SELECT 1 FROM code_mergeability AS m
				WHERE m.change_id = c.id AND m.revision = c.revision AND m.target_ref = c.target_ref
					AND (m.target_position < t.position OR m.state = 'unknown') AND m.at <= ?2)
		ORDER BY checked, c.id LIMIT ?3`
	// mergeHeld is a mergeability, m, its home owes no merge of: of the
	// change's head revision against its branch, and either of the target's
	// tip, t, and decided, or recorded after the recheck instant, ?3.
	mergeHeld = `m.change_id = c.id AND m.revision = c.revision AND m.target_ref = c.target_ref
		AND ((m.target_position >= t.position AND m.state != 'unknown') OR m.at > ?3)`
	// refreshDue is a mergeability, m, its home owes a refresh of: its
	// readiness judged at a version of the change's row, c, or of its
	// conversation's readiness inputs since moved, at a checks position a
	// head check for its target was queued or ended past since
	// (Mergeability.ChecksPosition), or by a chart earlier than the home's,
	// ?4 (Mergeability.ChartEpoch), which every statement using it binds.
	refreshDue = `m.change_id = c.id AND m.revision = c.revision AND m.target_ref = c.target_ref
		AND (m.change_version < c.version OR m.chart_epoch < ?4
			OR m.readiness_version < (SELECT v.readiness_version FROM code_conversations AS v WHERE v.change_id = c.id)
			OR EXISTS (SELECT 1 FROM code_checks AS k INDEXED BY ` + checksKey + `
				WHERE k.repo_id = c.repo_id AND k.commit_oid = c.head_oid AND k.target_ref = c.target_ref
					AND (k.queued_position > m.checks_position OR k.finished_position > m.checks_position)))`
)

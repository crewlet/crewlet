package gitlet

import (
	"context"
	"database/sql"
	"fmt"
	"slices"

	"github.com/crewlet/crewlet/internal/store"
)

// The merge's apply ([Applier.applyMerge]) and the two writes every record
// landing on a protected branch shares: its landing rows ([writeLandings]) and
// the branch's green row ([advanceGreen]). The decide held the change open at
// its head and into its target, the target at the merge's base and the source
// at the head, and arbitration on the repository's subject keeps that, so
// every guard is a SKIP, never a refusal that would stop the log on every
// node: a merge finding otherwise applies as NOTHING, whole, logged
// (code_merge_not_applied); only a replayed record meets one.

// LandingKind is what landed a commit on a protected branch
// (code_landings.kind): a queue's landing of its tested prefix, a change's
// direct merge, or a push. Each is spelled as the [MergedVia] of the same
// route, so a landing row and the change it merged name one route one way.
type LandingKind string

const (
	// LandedQueue is a queue's landing, each change tested merged on the
	// branch before it landed (the queue's `land` record).
	//
	// BASE FORMAT (queuerecords.go's head): only a land record's apply
	// writes it.
	LandedQueue LandingKind = "queue"
	// LandedDirect is a change merged by a merge record ([Merge]), its gate
	// held, or overridden by a person, from one snapshot.
	LandedDirect LandingKind = "direct"
	// LandedPush is a push that moved the branch: a person's under
	// direct_push persons, their recorded override.
	LandedPush LandingKind = "push"
)

// LandingKinds is the closed set: a branch's queue's, then a direct merge and
// a push.
func LandingKinds() []LandingKind { return []LandingKind{LandedQueue, LandedDirect, LandedPush} }

// Valid reports a kind this build knows.
func (k LandingKind) Valid() bool { return slices.Contains(LandingKinds(), k) }

// landing is one code_landings row a record writes: its ordinal among the
// record's landings on its branch (from 1), its kind, the change and queue
// entry ("" for a push), the branch before and after, who queued and who
// merged it, and whether its record proved it green, which moves the branch's
// green row onto it. A row holds no green of its own: a landing is green once
// it lies at or below its branch's green row.
type landing struct {
	target         string
	ordinal        int
	kind           LandingKind
	change, entry  string
	base, commit   string
	enqueuedBy, by string
	green          bool
}

// writeLandings writes a record's landings at its position and instant, in as
// few statements as the parameter limit allows ([store.InsertRows]), and the
// tip work (code_tips_due, one row a branch: tipwork.go) each landing its
// record did not prove green owes its branch; it answers the rows written. A
// key conflict is a skip, never an abort that would stall every node's log;
// none occurs, since every landing is keyed by its own record's position.
func writeLandings(ctx context.Context, tx store.Execer, at applyContext, repoID string,
	landings []landing) (int, error) {

	_, err := store.InsertRows(ctx, tx, at.maxVariables,
		`INSERT INTO code_landings (repo_id, target_ref, position, ordinal, kind, change_id, entry_id, base_oid,
			commit_oid, enqueued_by, merged_by, broken, at) VALUES `,
		`(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 0, ?)`,
		`ON CONFLICT (repo_id, target_ref, position, ordinal) DO NOTHING`, len(landings), func(i int) []any {
			l := landings[i]
			return []any{repoID, l.target, at.packed, l.ordinal, string(l.kind), l.change, l.entry, l.base,
				l.commit, l.enqueuedBy, l.by, at.at()}
		})
	if err != nil {
		return 0, fmt.Errorf("gitlet: write %d landings at %s: %w", len(landings), at.position, err)
	}
	rows := len(landings)
	owed := map[string]bool{}
	for _, l := range landings {
		if l.green || owed[l.target] {
			continue
		}
		owed[l.target] = true
		if _, err := tx.ExecContext(ctx, oweTipSQL, repoID, l.target); err != nil {
			return 0, fmt.Errorf("gitlet: owe %s's tip work at %s: %w", l.target, at.position, err)
		}
		rows++
	}
	return rows, nil
}

// advanceGreen moves target's green row to commit (the newest commit whose
// required checks passed, or needed none, which mirrors and deploys follow) at
// the record's position and instant: an upsert by key, so a branch's first
// green inserts it, and positions only grow, so it only moves forward. Every
// landing then lies at or below it, so the branch's tip work row goes. It
// answers the rows it wrote.
func advanceGreen(ctx context.Context, tx store.Execer, at applyContext, repoID, target, commit string) (int,
	error) {

	if _, err := tx.ExecContext(ctx, advanceGreenSQL, repoID, target, commit, at.packed, at.at()); err != nil {
		return 0, fmt.Errorf("gitlet: move %s's green row on %s to %s at %s: %w", target, repoID, short(commit),
			at.position, err)
	}
	res, err := tx.ExecContext(ctx, tipPaidSQL, repoID, target)
	if err != nil {
		return 0, fmt.Errorf("gitlet: clear %s's tip work at %s: %w", target, at.position, err)
	}
	cleared, _ := res.RowsAffected()
	return 1 + int(cleared), nil
}

// settleTipWork removes target's row of tip work once no landing above above
// (its green row's position, or -1 when no tip has passed) is left that no
// bisect found broken. It answers the rows it removed.
func settleTipWork(ctx context.Context, tx store.Execer, repoID, target string, above int64) (int, error) {
	res, err := tx.ExecContext(ctx, settleTipSQL, repoID, target, above)
	if err != nil {
		return 0, fmt.Errorf("gitlet: settle %s's tip work: %w", target, err)
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

// applyMerge applies a change's merge as one record (merge.go's head):
//
//  1. the change merged FIRST, so the push it lands does not take it for live
//     and merge it by push, or close it on a deleted source, again;
//  2. its live queue entry dequeued, where an override merged a queued change,
//     and its stored readiness cleared ([clearReadiness]);
//  3. the target's landing row where a rule protects it, and its green row
//     where no other check is required ([Merge.Protected], [Merge.Green]);
//  4. the push, through the apply every push takes ([Applier.landPushes]),
//     `via merge`, made for [keptCarrier], with the record's horizons;
//  5. its merge counted ([countChanges]), after the push, so the horizons get
//     what an older build's rows leave them.
func (a *Applier) applyMerge(ctx context.Context, tx *sql.Tx, at applyContext, m Merge) (int, error) {
	m.WorkingFor = keptCarrier(at, m.WorkingFor)
	row, found, err := changeRowIn(ctx, tx, m.ChangeID)
	if err != nil {
		return 0, err
	}
	by, kind := m.Principal.party()
	switch {
	case !found || row.repoID != m.RepoID:
		return mergeNotApplied(ctx, at, m, "the repository holds no such change")
	case row.state != ChangeStateOpen || row.head != m.HeadOID || row.target != m.TargetRef:
		return mergeNotApplied(ctx, at, m, "a merge takes the open change at the head and into the target its "+
			"decide read, and it is "+string(row.state)+" at "+short(row.head)+" into "+row.target)
	case by == "":
		return mergeNotApplied(ctx, at, m, "its principal names no seat, and only a seat or a person merges")
	case m.DeleteSource && m.SourceRef != row.source:
		return mergeNotApplied(ctx, at, m, "it deletes "+m.SourceRef+", and the change's source is "+row.source)
	}
	named := []string{m.TargetRef}
	if m.DeleteSource {
		named = append(named, m.SourceRef)
	}
	refs, err := refsNamed(ctx, tx, m.RepoID, named)
	if err != nil {
		return 0, fmt.Errorf("gitlet: read the refs the merge at %s moves: %w", at.position, err)
	}
	switch {
	case refTip(refs, m.TargetRef) != m.BaseOID:
		return mergeNotApplied(ctx, at, m, m.TargetRef+" is at "+short(refTip(refs, m.TargetRef))+", and the "+
			"merge was built onto "+short(m.BaseOID))
	case m.DeleteSource && refTip(refs, m.SourceRef) != m.HeadOID:
		return mergeNotApplied(ctx, at, m, m.SourceRef+" is at "+short(refTip(refs, m.SourceRef))+", and the "+
			"merge deletes it at "+short(m.HeadOID))
	}
	// ITS PUSH LANDS WHOLE WITH IT OR NOT AT ALL: a push id already in the
	// history is this merge's own record met again past the version guard,
	// and no record merges a change whose push the history skips.
	landed, err := pushesApplied(ctx, tx, []string{m.MergeID})
	if err != nil {
		return 0, fmt.Errorf("gitlet: read whether the merge at %s landed its push: %w", at.position, err)
	}
	if landed[m.MergeID] {
		return mergeNotApplied(ctx, at, m, "its push "+m.MergeID+" is in the repository's history already")
	}

	// 1 THE CHANGE MERGED FIRST, one version more at the record's position.
	next := row
	next.state, next.mergedVia, next.mergeOID = ChangeStateMerged, MergedViaDirect, m.CommitOID
	next.mergedAt = sql.NullInt64{Int64: at.at(), Valid: true}
	// Who merged it, as every record names them (iam.ActorFor): a seat by
	// handle, a person by human seat beside the credential they used.
	next.mergedBy, next.mergedByKind, next.overrideReason = by, kind, m.Override
	if kind == PartyPerson {
		next.mergedOperator = m.Principal.OperatorID
	}
	next.version, next.position, next.updatedAt = row.version+1, at.packed, at.at()
	if err = writeChanges(ctx, tx, at.maxVariables, changeMerged, []*changeRow{&next}); err != nil {
		return 0, fmt.Errorf("gitlet: merge change %s at %s: %w", m.ChangeID, at.position, err)
	}
	rows := 1

	// 2 ITS LIVE QUEUE ENTRY, where it holds one, and its stored readiness,
	// where it holds an answer: a merged change waits on nobody.
	ended, err := endLiveEntryOf(ctx, tx, at, m.ChangeID, QueueDequeued, EndMerged)
	if err != nil {
		return 0, err
	}
	rows += ended
	cleared, err := clearReadiness(ctx, tx, []string{m.ChangeID})
	if err != nil {
		return 0, fmt.Errorf("%w (the merge at %s)", err, at.position)
	}
	rows += cleared

	// 3 THE LANDING AND THE GREEN ROW, as the decide's snapshot judged the
	// target.
	if m.Protected {
		var landed int
		if landed, err = writeLandings(ctx, tx, at, m.RepoID, []landing{{target: m.TargetRef, ordinal: 1,
			kind: LandedDirect, change: m.ChangeID, base: m.BaseOID, commit: m.CommitOID, by: by,
			green: m.Green}}); err != nil {
			return 0, err
		}
		rows += landed
	}
	if m.Green {
		var moved int
		if moved, err = advanceGreen(ctx, tx, at, m.RepoID, m.TargetRef, m.CommitOID); err != nil {
			return 0, err
		}
		rows += moved
	}

	// 4 THE PUSH, with what the rows above leave the record's horizons.
	pushed, err := a.landPushes(ctx, tx, at, PushBatch{RepoID: m.RepoID, Pushes: []PushEntry{m.push()}}, recordPacks{},
		rows)
	if err != nil {
		return 0, err
	}

	// 5 ITS MERGE COUNTED.
	var merged ChangeCounts
	merged.move(row.state, ChangeStateMerged)
	tallied, err := countChanges(ctx, tx, m.RepoID, merged)
	if err != nil {
		return 0, fmt.Errorf("%w (the merge at %s)", err, at.position)
	}
	return rows + pushed + tallied, nil
}

// push is the push m lands, as its push row records it: m's id, principal,
// carrier and node, `via merge`; the target fast-forwarded from its base onto
// the commit (whose old object the commit still reaches, so no drop) and the
// source deleted where asked; its pack; and what it introduced: the commit a
// merge or squash built, none for a fast-forward, whose head the change's own
// pushes brought.
func (m Merge) push() PushEntry {
	e := PushEntry{PushID: m.MergeID, Principal: m.Principal, WorkingFor: m.WorkingFor, Via: ViaMerge, Node: m.Node,
		Pack: m.Pack, ReceivedAt: m.ReceivedAt,
		Commands: []Command{{Ref: m.TargetRef, Old: m.BaseOID, New: m.CommitOID, Result: ResultOK}}}
	if m.DeleteSource {
		e.Commands = append(e.Commands, Command{Ref: m.SourceRef, Old: m.HeadOID, New: ZeroOID, Result: ResultOK})
	}
	if m.Method != MergeMethodFastForward {
		e.Introduced, e.IntroducedCount = []string{m.CommitOID}, 1
	}
	return e
}

// mergeNotApplied logs a merge record that applies as nothing because what it
// is about is not as its decide held, and answers that it wrote no row.
func mergeNotApplied(ctx context.Context, at applyContext, m Merge, why string) (int, error) {
	applyLog.WarnContext(ctx, "code_merge_not_applied", "repo", m.RepoID, "change", m.ChangeID,
		"merge", m.MergeID, "op_id", at.record.OpID, "position", at.position.String(), "why", why)
	return 0, nil
}

// advanceGreenSQL and oweTipSQL are upserts, located by the key their VALUES
// carry and so registered nowhere. tipPaidSQL removes a branch's tip work by
// its key, and settleTipSQL where no unbroken landing lies above a position
// (the key's range, read to the first unbroken one), both registered in
// plan_test.go.
const (
	advanceGreenSQL = `INSERT INTO code_green (repo_id, target_ref, commit_oid, position, at)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT (repo_id, target_ref) DO UPDATE SET commit_oid = excluded.commit_oid,
			position = excluded.position, at = excluded.at`
	oweTipSQL = `INSERT INTO code_tips_due (repo_id, target_ref) VALUES (?, ?)
		ON CONFLICT (repo_id, target_ref) DO NOTHING`
	tipPaidSQL   = `DELETE FROM code_tips_due WHERE repo_id = ? AND target_ref = ?`
	settleTipSQL = `DELETE FROM code_tips_due WHERE repo_id = ?1 AND target_ref = ?2
		AND NOT EXISTS (SELECT 1 FROM code_landings WHERE repo_id = ?1 AND target_ref = ?2 AND position > ?3
			AND broken = 0)`
)

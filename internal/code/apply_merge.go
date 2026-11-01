package code

import (
	"context"
	"database/sql"
	"fmt"
	"slices"

	"github.com/crewlet/crewlet/internal/store"
)

// THE MERGE'S APPLY ([Applier.applyMerge]), and the two writes every record
// that lands on a protected branch shares — its landing rows
// ([writeLandings]) and the branch's green row ([advanceGreen]) — in the one
// transaction the framework holds a record's apply to, PURE IN THE RECORD, ITS
// POSITION AND ITS INSTANT. The decide held the change open at its head and
// into its target, the target at the merge's base and the source at the head;
// the broker's arbitration on the repository's subject guarantees the record
// applies against the rows that decide read. So every guard here is a SKIP,
// never a refusal, which would stop the log on every node: a merge that finds
// any of them otherwise applies as NOTHING, whole, logged
// (code_merge_not_applied) — no correct log carries one but a record the
// ledger would have collapsed, met again.

// LandingKind is what landed a commit on a protected branch, as its landing
// row says (code_landings.kind): a queue's landing of its tested prefix, a
// change's direct merge, or a push. A named string with a closed set, so a
// kind a later build records is a value.
type LandingKind string

const (
	// LandedPreLand is a queue's landing, each change tested merged on the
	// branch before it landed (m5-q2's `land` record).
	LandedPreLand LandingKind = "pre_land"
	// LandedDirect is a change merged by a merge record ([Merge]), its gate
	// held — or overridden by a person — from one snapshot.
	LandedDirect LandingKind = "direct"
	// LandedPush is a push that moved the branch: a person's under
	// direct_push persons, their recorded override (m5-q2's push landings).
	LandedPush LandingKind = "push"
)

// LandingKinds is the closed set: a branch's queue's, then a direct merge and
// a push.
func LandingKinds() []LandingKind { return []LandingKind{LandedPreLand, LandedDirect, LandedPush} }

// Valid reports a kind this build knows.
func (k LandingKind) Valid() bool { return slices.Contains(LandingKinds(), k) }

// landing is one code_landings row a record writes: its place among the
// record's landings on its branch (from 1), what kind of landing it is, what
// landed — the change and its queue entry, "" for a push that landed none —
// the branch before and after, who queued it and who merged it, and whether it
// is green as it lands.
type landing struct {
	target         string
	ordinal        int
	kind           LandingKind
	change, entry  string
	base, commit   string
	enqueuedBy, by string
	green          bool
}

// writeLandings writes landings, a record's landings on protected branches,
// at the record's position and instant — in as few statements as the
// engine's parameters allow ([store.InsertRows]). A conflict on the key is a
// skip, never an abort, which would stall the log on every node: no record
// this build applies meets one, since every landing a record writes is keyed
// by its own position.
func writeLandings(ctx context.Context, tx store.Execer, at applyContext, repoID string,
	landings []landing) error {

	_, err := store.InsertRows(ctx, tx, at.maxVariables,
		`INSERT INTO code_landings (repo_id, target_ref, position, ordinal, kind, change_id, entry_id, base_oid,
			commit_oid, enqueued_by, merged_by, green, broken, at) VALUES `,
		`(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 0, ?)`,
		`ON CONFLICT (repo_id, target_ref, position, ordinal) DO NOTHING`, len(landings), func(i int) []any {
			l := landings[i]
			return []any{repoID, l.target, at.packed, l.ordinal, string(l.kind), l.change, l.entry, l.base,
				l.commit, l.enqueuedBy, l.by, l.green, at.at()}
		})
	if err != nil {
		return fmt.Errorf("code: write %d landings at %s: %w", len(landings), at.position, err)
	}
	return nil
}

// advanceGreen moves target's green row on repoID to commit, at the record's
// position and instant: the branch's newest commit whose required checks
// passed — or that needed none — which mirrors and deploys follow. An upsert
// by the row's key, so the first green a branch reaches inserts its row: a
// record's position is past every earlier one's, and the version guard skips
// one met again, so the row only moves forward.
func advanceGreen(ctx context.Context, tx store.Execer, at applyContext, repoID, target, commit string) error {
	if _, err := tx.ExecContext(ctx, advanceGreenSQL, repoID, target, commit, at.packed, at.at()); err != nil {
		return fmt.Errorf("code: move %s's green row on %s to %s at %s: %w", target, repoID, short(commit),
			at.position, err)
	}
	return nil
}

// applyMerge applies a change's merge as one record — see merge.go's head:
//
//  1. THE CHANGE MERGED FIRST — `merged_via direct`, at the merge's commit,
//     who merged it and a person's override — so that the push this record
//     lands finds it merged: M4's merge by push (a target set to a live
//     change's head) and its close on a deleted source would otherwise take
//     the change for live, and merge or close it again.
//  2. Its live queue entry ended, where a person's override merged a queued
//     change directly: dequeued, as a landing of the change ends its entry.
//  3. The target's landing row, where a rule protects it, and its green row,
//     where its gate requires no check besides — what the decide stamped
//     ([Merge.Protected], [Merge.Green]).
//  4. THE PUSH, through the one apply every push takes ([Applier.landPushes]):
//     the target moved from its base onto the commit, the source deleted
//     where the merge asks it, the merge's pack, its push row `via merge`
//     naming the merger and what it introduced, the repository's counters,
//     and the horizons with what the record's rows leave them.
func (a *Applier) applyMerge(ctx context.Context, tx *sql.Tx, at applyContext, m Merge) (int, error) {
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
		return 0, fmt.Errorf("code: read the refs the merge at %s moves: %w", at.position, err)
	}
	switch {
	case refTip(refs, m.TargetRef) != m.BaseOID:
		return mergeNotApplied(ctx, at, m, m.TargetRef+" is at "+short(refTip(refs, m.TargetRef))+", and the "+
			"merge was built onto "+short(m.BaseOID))
	case m.DeleteSource && refTip(refs, m.SourceRef) != m.HeadOID:
		return mergeNotApplied(ctx, at, m, m.SourceRef+" is at "+short(refTip(refs, m.SourceRef))+", and the "+
			"merge deletes it at "+short(m.HeadOID))
	}
	// ITS PUSH LANDS WHOLE WITH IT OR NOT AT ALL: a push id the history holds
	// already is a push landed — which only this merge's own record, met
	// again past the version guard, could have landed — and the change is
	// never merged by a record whose push the history skips.
	landed, err := keySet(ctx, tx, pushesAppliedSQL, []string{m.MergeID})
	if err != nil {
		return 0, fmt.Errorf("code: read whether the merge at %s landed its push: %w", at.position, err)
	}
	if landed[m.MergeID] {
		return mergeNotApplied(ctx, at, m, "its push "+m.MergeID+" is in the repository's history already")
	}

	// 1 THE CHANGE MERGED FIRST, one version more at the record's position.
	next := row
	next.state, next.mergedVia, next.mergeOID = ChangeStateMerged, MergedViaDirect, m.CommitOID
	next.mergedAt = sql.NullInt64{Int64: at.at(), Valid: true}
	// WHO MERGED IT, as every record a write leaves names them
	// (iam.ActorFor): a seat by its handle, a person by their human seat
	// beside the credential they merged with.
	next.mergedBy, next.mergedByKind, next.overrideReason = by, kind, m.Override
	if kind == PartyPerson {
		next.mergedOperator = m.Principal.OperatorID
	}
	next.version, next.position = row.version+1, at.packed
	if err := writeChanges(ctx, tx, at.maxVariables, changeMerged, []*changeRow{&next}); err != nil {
		return 0, fmt.Errorf("code: merge change %s at %s: %w", m.ChangeID, at.position, err)
	}
	rows := 1

	// 2 ITS LIVE QUEUE ENTRY, where it holds one.
	ended, err := endLiveEntryOf(ctx, tx, at, m.ChangeID, QueueDequeued, EndMerged)
	if err != nil {
		return 0, err
	}
	rows += ended

	// 3 THE LANDING AND THE GREEN ROW, as the decide's snapshot judged the
	// target.
	if m.Protected {
		if err := writeLandings(ctx, tx, at, m.RepoID, []landing{{target: m.TargetRef, ordinal: 1,
			kind: LandedDirect, change: m.ChangeID, base: m.BaseOID, commit: m.CommitOID, by: by,
			green: m.Green}}); err != nil {
			return 0, err
		}
		rows++
	}
	if m.Green {
		if err := advanceGreen(ctx, tx, at, m.RepoID, m.TargetRef, m.CommitOID); err != nil {
			return 0, err
		}
		rows++
	}

	// 4 THE PUSH, with what the rows above leave the record's horizons.
	pushed, err := a.landPushes(ctx, tx, at, PushBatch{RepoID: m.RepoID, Pushes: []PushEntry{m.push()}}, recordPacks{},
		rows)
	if err != nil {
		return 0, err
	}
	return rows + pushed, nil
}

// push is the push m lands, as its push row records it: its push id the
// merge's, the merger its principal, `via merge`, the git node that built it;
// the target moved from its base onto the commit — a fast-forward of it,
// whose old object the commit still reaches, so no drop — and the source
// deleted where it asks; its pack; and what it introduced: the commit a merge
// or a squash built, and none for a fast-forward, whose head the change's own
// pushes brought.
func (m Merge) push() PushEntry {
	e := PushEntry{PushID: m.MergeID, Principal: m.Principal, Via: ViaMerge, Node: m.Node, Pack: m.Pack,
		ReceivedAt: m.ReceivedAt,
		Commands:   []Command{{Ref: m.TargetRef, Old: m.BaseOID, New: m.CommitOID, Result: ResultOK}}}
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

// advanceGreenSQL is a branch's green row moved by its key — an upsert, which
// locates the row by the key its VALUES carry and so is registered nowhere.
const advanceGreenSQL = `INSERT INTO code_green (repo_id, target_ref, commit_oid, position, at)
	VALUES (?, ?, ?, ?, ?)
	ON CONFLICT (repo_id, target_ref) DO UPDATE SET commit_oid = excluded.commit_oid,
		position = excluded.position, at = excluded.at`

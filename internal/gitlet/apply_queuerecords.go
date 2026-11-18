package gitlet

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"slices"

	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/store"
)

// The queue's own records' applies (queuerecords.go), each pure in the record,
// its position and its instant, as an enqueue's is (apply_queue.go). The
// decide held the entries live, the target at its base and every gate met, and
// every guard is a skip (apply.go's head, rule 3): a record finding its rows
// not as its decide held them applies as nothing, logged
// (code_queue_not_applied). Every record moves the repository's version, so
// the version guard skips it when it is met again; a landing's moves it
// through the counters of the pushes it lands.

// # A train is abandoned whole
//
// A target holds one train at most, valid only while every entry it tests is
// live in it: a candidate abandons the target's earlier train, and an eject, a
// dequeue or a landing of an entry a train tests ends that train's claim on
// the rest, its candidates removed (so a read of the target's trains names its
// live one alone) and its other entries waiting for the home's next candidate.
// A change's own records that end a testing entry (a close, a draft, a
// retarget, a push to its head) leave the train naming a dead entry, which no
// landing lands (its decide refuses a train whose entries are not live) and
// the home's next candidate abandons.

// # The queue's history keeps pace with its records
//
// Each candidate, eject and land removes its repository's queue history past
// [QueueHistory] (ended entries, then landings below their target's green row,
// oldest first): at most [queueHorizonRows] a record, as many as a landing
// ends and lands, and ejections only until they weigh [queueHorizonBytes], as
// much as one record adds, so a working queue keeps its own thirty days. An
// idle repository's history is the retention duty's ([Store.QueueSweep]).

// candidateTesting is a candidate's state while its train tests it — the one
// a candidate row holds, since a train that ends removes its rows.
const candidateTesting = "testing"

// applyCandidate records a train: the target's earlier train abandoned, every
// entry of this one testing in its candidate at the commit it became, the
// candidates' rows, the train's pack's row and the repository's counters, and
// the queue's history past its horizon.
func (a *Applier) applyCandidate(ctx context.Context, tx *sql.Tx, at applyContext, c Candidate) (int, error) {
	var named []string
	for _, cand := range c.Candidates {
		for _, e := range cand.Entries {
			named = append(named, e.EntryID)
		}
	}
	entries, err := entriesNamed(ctx, tx, c.RepoID, named)
	if err != nil {
		return 0, err
	}
	for _, id := range named {
		entry, found := entries[id]
		if why := whyNotLive(entry, found, c.TargetRef); why != "" {
			return queueNotApplied(ctx, at, c.RepoID, id, "a train tests live entries of its target, and it "+why)
		}
	}
	if tip, err := targetTip(ctx, tx, c.RepoID, c.TargetRef); err != nil { //nolint:govet // shadow: `x, err := f()` declares x too; see .golangci.yml
		return 0, err
	} else if tip != c.BaseOID {
		return queueNotApplied(ctx, at, c.RepoID, "", c.TargetRef+" is at "+short(tip)+", and the train was "+
			"built onto "+short(c.BaseOID))
	}
	rows, err := abandonTrainOn(ctx, tx, c.RepoID, c.TargetRef)
	if err != nil {
		return 0, fmt.Errorf("gitlet: abandon %s's train at %s: %w", c.TargetRef, at.position, err)
	}
	base := c.BaseOID
	bases := make([]string, len(c.Candidates))
	tested := make([]entryRow, 0, len(named))
	for i, cand := range c.Candidates {
		bases[i], base = base, cand.TipOID
		for _, e := range cand.Entries {
			next := entries[e.EntryID]
			next.State, next.Train, next.CandidateID, next.CommitOID = QueueTesting, c.Train, cand.CandidateID,
				e.Commit
			tested = append(tested, entryRow{queueEntry: next})
		}
	}
	if err = writeEntries(ctx, tx, at.maxVariables, entryTested, tested); err != nil {
		return 0, fmt.Errorf("gitlet: test train %s's entries at %s: %w", c.Train, at.position, err)
	}
	rows += len(tested)
	encoded := make([]string, len(c.Candidates))
	for i, cand := range c.Candidates {
		//nolint:govet // shadow: `x, err := f()` declares x too; see .golangci.yml
		entries, err := json.Marshal(cand.Entries)
		if err != nil {
			return 0, err
		}
		encoded[i] = string(entries)
	}
	packID := ""
	if c.Pack != nil {
		packID = c.Pack.PackID
	}
	if _, err = store.InsertRows(ctx, tx, at.maxVariables,
		`INSERT INTO code_candidates (repo_id, candidate_id, target_ref, train, ordinal, base_oid, tip_oid,
			pack_id, state, position, entries) VALUES `, `(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		`ON CONFLICT (repo_id, candidate_id) DO NOTHING`, len(c.Candidates), func(i int) []any {
			cand := c.Candidates[i]
			return []any{c.RepoID, cand.CandidateID, c.TargetRef, c.Train, cand.Ordinal, bases[i], cand.TipOID,
				packID, candidateTesting, at.packed, encoded[i]}
		}); err != nil {
		return 0, fmt.Errorf("gitlet: write train %s at %s: %w", c.Train, at.position, err)
	}
	rows += len(c.Candidates)
	added, err := addQueuePack(ctx, tx, at, c.RepoID, c.Pack, PackCandidate)
	if err != nil {
		return 0, err
	}
	rows += added
	swept, err := sweepQueueHistory(ctx, tx, c.RepoID, store.EncodeTime(at.brokerAt.Add(-QueueHistory)),
		queueHorizonRows, queueHorizonBytes)
	if err != nil {
		return 0, err
	}
	return rows + swept, nil
}

// applyEject puts the record's entries out of their queue — ejected, why and
// what of it a person reads first — abandons the train any of them was
// tested in, moves the repository's version, and removes the queue's history
// past its horizon.
func (a *Applier) applyEject(ctx context.Context, tx *sql.Tx, at applyContext, e Eject) (int, error) {
	named := make([]string, len(e.Entries))
	for i, x := range e.Entries {
		named[i] = x.EntryID
	}
	entries, err := entriesNamed(ctx, tx, e.RepoID, named)
	if err != nil {
		return 0, err
	}
	target, testing := "", false
	for _, x := range e.Entries {
		entry, found := entries[x.EntryID]
		if target == "" && found {
			target = entry.TargetRef
		}
		if why := whyNotLive(entry, found, target); why != "" {
			return queueNotApplied(ctx, at, e.RepoID, x.EntryID, "an eject puts one target's live entries out, "+
				"and it "+why)
		}
		testing = testing || entry.State == QueueTesting
	}
	ejected := make([]entryRow, len(e.Entries))
	for i, x := range e.Entries {
		//nolint:govet // shadow: `x, err := f()` declares x too; see .golangci.yml
		detail, err := ejectedDetail(x)
		if err != nil {
			return 0, err
		}
		next := entries[x.EntryID]
		next.State, next.EndedAt, next.EndedPosition, next.EndReason = QueueEjected, at.at(), at.packed, x.Reason
		ejected[i] = entryRow{queueEntry: next, ejected: detail}
	}
	if err = writeEntries(ctx, tx, at.maxVariables, entryEjected, ejected); err != nil {
		return 0, fmt.Errorf("gitlet: eject %d entries at %s: %w", len(ejected), at.position, err)
	}
	rows := len(ejected)
	if testing {
		//nolint:govet // shadow: `x, err := f()` declares x too; see .golangci.yml
		abandoned, err := abandonTrainOn(ctx, tx, e.RepoID, target)
		if err != nil {
			return 0, fmt.Errorf("gitlet: abandon %s's train at %s: %w", target, at.position, err)
		}
		rows += abandoned
	}
	if _, err = tx.ExecContext(ctx, touchedSQL, at.packed, e.RepoID); err != nil {
		return 0, fmt.Errorf("gitlet: move %s's version at %s: %w", e.RepoID, at.position, err)
	}
	swept, err := sweepQueueHistory(ctx, tx, e.RepoID, store.EncodeTime(at.brokerAt.Add(-QueueHistory)),
		queueHorizonRows, queueHorizonBytes)
	if err != nil {
		return 0, err
	}
	return rows + 1 + swept, nil
}

// applyLand lands a queue's entries as one record (queuerecords.go's head):
//  1. each change merged first by its enqueuer, `merged_via` queue at its
//     commit, so the pushes find it merged, as a merge's do
//     ([Applier.applyMerge]), rather than merging or closing it;
//  2. each entry landed at its commit, its train's landed candidates removed;
//  3. the landing rows in chain order, each green (the decide proved every
//     required check on the tip), and the green row moved to the tip;
//  4. the queue's history past its horizon, before the pushes' own horizons;
//  5. the pushes, one an entry, through [Applier.landPushes], `via queue` naming
//     the enqueuer and whom it was made for, read off the entry's row since
//     the record does not carry it ([Land.pushes]);
//  6. their merges counted ([countChanges]), after the pushes, as a merge's.
func (a *Applier) applyLand(ctx context.Context, tx *sql.Tx, at applyContext, l Land) (int, error) {
	named := make([]string, len(l.Entries))
	for i, e := range l.Entries {
		named[i] = e.EntryID
	}
	entries, err := entriesNamed(ctx, tx, l.RepoID, named)
	if err != nil {
		return 0, err
	}
	// Every change in one read, driven from the list.
	ids := make([]string, len(l.Entries))
	for i, e := range l.Entries {
		ids[i] = e.ChangeID
	}
	rowsOf, err := changeRowsIn(ctx, tx, ids)
	if err != nil {
		return 0, err
	}
	changes := make([]changeRow, len(l.Entries))
	for i, e := range l.Entries {
		entry, found := entries[e.EntryID]
		if why := whyNotLive(entry, found, l.TargetRef); why != "" {
			return queueNotApplied(ctx, at, l.RepoID, e.EntryID, "a landing lands live entries of its target, "+
				"and it "+why)
		}
		row, found := rowsOf[e.ChangeID]
		switch {
		case !found || row.repoID != l.RepoID || entry.ChangeID != e.ChangeID:
			return queueNotApplied(ctx, at, l.RepoID, e.EntryID, "the repository holds no such change of it")
		case row.state != ChangeStateOpen || row.head != entry.HeadOID || row.target != l.TargetRef:
			return queueNotApplied(ctx, at, l.RepoID, e.EntryID, "a landing lands an open change at the head "+
				"and into the target its entry queued, and it is "+string(row.state)+" at "+short(row.head)+
				" into "+row.target)
		}
		changes[i] = row
	}
	if tip, err := targetTip(ctx, tx, l.RepoID, l.TargetRef); err != nil { //nolint:govet // shadow: `x, err := f()` declares x too; see .golangci.yml
		return 0, err
	} else if tip != l.BaseOID {
		return queueNotApplied(ctx, at, l.RepoID, "", l.TargetRef+" is at "+short(tip)+", and the landing was "+
			"built onto "+short(l.BaseOID))
	}
	pushes := l.pushes(entries, changes)
	// Its pushes land whole with it or not at all, as a merge's do.
	landed, err := pushesApplied(ctx, tx, []string{pushes[0].PushID})
	if err != nil {
		return 0, fmt.Errorf("gitlet: read whether the landing at %s landed its pushes: %w", at.position, err)
	}
	if landed[pushes[0].PushID] {
		return queueNotApplied(ctx, at, l.RepoID, "", "its push "+pushes[0].PushID+" is in the repository's "+
			"history already")
	}

	// 1 Every change merged first, by its enqueuer: the queue never acts on
	// its own account.
	merged := make([]*changeRow, len(changes))
	for i, row := range changes {
		entry := entries[l.Entries[i].EntryID]
		next := row
		next.state, next.mergedVia, next.mergeOID = ChangeStateMerged, MergedViaQueue, l.Entries[i].Commit
		next.mergedAt = sql.NullInt64{Int64: at.at(), Valid: true}
		next.mergedBy, next.mergedByKind = entry.EnqueuedBy.Handle, entry.EnqueuedBy.Kind
		if entry.EnqueuedBy.Kind == PartyPerson {
			next.mergedOperator = entry.EnqueuedOperator
		}
		next.version, next.position, next.updatedAt = row.version+1, at.packed, at.at()
		merged[i] = &next
	}
	if err = writeChanges(ctx, tx, at.maxVariables, changeMerged, merged); err != nil {
		return 0, fmt.Errorf("gitlet: merge the changes the landing at %s lands: %w", at.position, err)
	}
	rows := len(merged)
	// Their stored readiness cleared in one statement: a merged change waits
	// on nobody ([clearReadiness]).
	cleared, err := clearReadiness(ctx, tx, ids)
	if err != nil {
		return 0, fmt.Errorf("%w (the landing at %s)", err, at.position)
	}
	rows += cleared

	// 2 Every entry landed, and the train's landed candidates removed.
	ended := make([]entryRow, len(l.Entries))
	for i, e := range l.Entries {
		next := entries[e.EntryID]
		next.State, next.EndedAt, next.EndedPosition, next.EndReason, next.CommitOID = QueueLanded, at.at(),
			at.packed, "", e.Commit
		// The close it owes where its enqueue asked one of a change carrying a
		// work item (M5-D27): whichever home and turn reaches it first closes
		// the item as its enqueuer and settles it (close_settled), never only
		// the landing's turn.
		ended[i] = entryRow{queueEntry: next, closeOp: closeOpOf(at, next, changes[i])}
	}
	if err = writeEntries(ctx, tx, at.maxVariables, entryLanded, ended); err != nil {
		return 0, fmt.Errorf("gitlet: land %d entries at %s: %w", len(ended), at.position, err)
	}
	rows += len(ended)
	dropped, err := dropLandedCandidates(ctx, tx, l)
	if err != nil {
		return 0, fmt.Errorf("gitlet: remove train %s's landed candidates at %s: %w", l.Train, at.position, err)
	}
	rows += dropped

	// 3 The landings, in chain order, and the green row.
	landings := make([]landing, len(l.Entries))
	from := l.BaseOID
	for i, e := range l.Entries {
		by := entries[e.EntryID].EnqueuedBy.Handle
		landings[i] = landing{target: l.TargetRef, ordinal: i + 1, kind: LandedQueue, change: e.ChangeID,
			entry: e.EntryID, base: from, commit: e.Commit, enqueuedBy: by, by: by, green: true}
		from = e.Commit
	}
	landingRowsWritten, err := writeLandings(ctx, tx, at, l.RepoID, landings)
	if err != nil {
		return 0, err
	}
	rows += landingRowsWritten
	moved, err := advanceGreen(ctx, tx, at, l.RepoID, l.TargetRef, l.TipOID)
	if err != nil {
		return 0, err
	}
	rows += moved

	// 4 The queue's history, before the pushes' own horizons.
	swept, err := sweepQueueHistory(ctx, tx, l.RepoID, store.EncodeTime(at.brokerAt.Add(-QueueHistory)),
		queueHorizonRows, queueHorizonBytes)
	if err != nil {
		return 0, err
	}
	rows += swept

	// 5 The pushes, their horizons given what the rows above leave.
	pushed, err := a.landPushes(ctx, tx, at, PushBatch{RepoID: l.RepoID, Pushes: pushes}, recordPacks{}, rows)
	if err != nil {
		return 0, err
	}

	// 6 Their merges counted.
	var landedCounts ChangeCounts
	for _, row := range changes {
		landedCounts.move(row.state, ChangeStateMerged)
	}
	tallied, err := countChanges(ctx, tx, l.RepoID, landedCounts)
	if err != nil {
		return 0, fmt.Errorf("%w (the landing at %s)", err, at.position)
	}
	return rows + pushed + tallied, nil
}

// closeOpOf is the operation a landed entry's work item is closed under
// (code_queue.close_op): [CloseWorkItemOpID] of the landing's operation where
// entry asked a close of a change carrying a work item, else "" (owing none).
func closeOpOf(at applyContext, entry queueEntry, change changeRow) string {
	if !entry.CloseTask || change.work.ID == "" {
		return ""
	}
	return CloseWorkItemOpID(at.record.OpID, entry.EntryID)
}

// CloseWorkItemOpID is the operation a landed entry's work item is closed
// under (M5-D27): a step of its landing's operation, as a direct merge's close
// is of the merge's, so whichever home asks it on whichever turn it is one
// tracker write, which the tracker's ledger answers once. It is recorded on
// the entry's row as the landing applies, since nothing else there names the
// landing's operation.
func CloseWorkItemOpID(landOpID, entryID string) string {
	return statelog.StepOpID(landOpID, "close-work-item", entryID)
}

// applyCloseSettled clears the closes landed entries owed their work items
// (code_queue.close_op), out of every home's queue work, in one statement: an
// entry owing none (another settle reached it first) is passed over, and a
// record that clears none writes nothing. The notices of its not_closed
// closes are the caller's ([Applier.noticed]).
func (a *Applier) applyCloseSettled(ctx context.Context, tx *sql.Tx, at applyContext, c CloseSettled) (int,
	error) {

	ids := c.entries()
	owed, err := owedClosesNamed(ctx, tx, c.RepoID, ids)
	if err != nil {
		return 0, err
	}
	settling := make([]string, 0, len(ids))
	for _, id := range ids {
		if o, found := owed[id]; found && o.op != "" {
			settling = append(settling, id)
			continue
		}
		if _, err = queueNotApplied(ctx, at, c.RepoID, id, "it owes its work item no close"); err != nil {
			return 0, err
		}
	}
	if len(settling) == 0 {
		return 0, nil
	}
	list, err := jsonList(settling)
	if err != nil {
		return 0, err
	}
	if _, err = tx.ExecContext(ctx, settleClosesSQL, list, c.RepoID); err != nil {
		return 0, fmt.Errorf("gitlet: settle %d entries' closes at %s: %w", len(settling), at.position, err)
	}
	if _, err = tx.ExecContext(ctx, touchedSQL, at.packed, c.RepoID); err != nil {
		return 0, fmt.Errorf("gitlet: move %s's version at %s: %w", c.RepoID, at.position, err)
	}
	return closeSettledRows(len(settling)), nil
}

// applyGreen moves the target's green row onto the landing record at Through,
// taking the landing's position so every landing up to it is green by
// position, and settles the target's tip work against what lies above it. It
// writes three rows whatever lies beneath (the green row, the tip work, the
// version), never one a landing, so no backlog of unproven landings makes a
// green outgrow a record.
func (a *Applier) applyGreen(ctx context.Context, tx *sql.Tx, at applyContext, g Green) (int, error) {
	green, held, err := greenIn(ctx, tx, g.RepoID, g.TargetRef)
	if err != nil {
		return 0, err
	}
	from := int64(0)
	if held {
		from = green.Position.Packed()
	}
	if from >= g.Through.Packed() {
		return queueNotApplied(ctx, at, g.RepoID, "", g.TargetRef+"'s green row is at or past "+
			g.Through.String()+" already")
	}
	tip, found, err := landingTipAt(ctx, tx, g.RepoID, g.TargetRef, g.Through.Packed())
	if err != nil {
		return 0, err
	}
	if !found || tip != g.OID {
		return queueNotApplied(ctx, at, g.RepoID, "", g.TargetRef+" holds no landing of "+short(g.OID)+" at "+
			g.Through.String())
	}
	if err = moveGreen(ctx, tx, at, g.RepoID, g.TargetRef, g.OID, g.Through.Packed()); err != nil {
		return 0, err
	}
	settled, err := settleTipWork(ctx, tx, g.RepoID, g.TargetRef, g.Through.Packed())
	if err != nil {
		return 0, fmt.Errorf("%w (the green at %s)", err, at.position)
	}
	if _, err := tx.ExecContext(ctx, touchedSQL, at.packed, g.RepoID); err != nil {
		return 0, fmt.Errorf("gitlet: move %s's version at %s: %w", g.RepoID, at.position, err)
	}
	return 2 + settled, nil
}

// applyBroken marks the landing the record names broken and settles its
// target's tip work: a target whose only landings above its green row are
// broken owes its home nothing until the next lands. The green row stays: it
// only ever advances on green.
func (a *Applier) applyBroken(ctx context.Context, tx *sql.Tx, at applyContext, b Broken) (int, error) {
	landing, found, err := landingAt(ctx, tx, b.RepoID, b.TargetRef, b.Position.Packed(), b.Ordinal)
	if err != nil {
		return 0, err
	}
	if !found || landing.commit != b.OID {
		return queueNotApplied(ctx, at, b.RepoID, "", b.TargetRef+" holds no landing of "+short(b.OID)+" at "+
			b.Position.String())
	}
	if _, err = tx.ExecContext(ctx, brokenLandingSQL, b.RepoID, b.TargetRef, b.Position.Packed(),
		b.Ordinal); err != nil {
		return 0, fmt.Errorf("gitlet: mark %s's landing at %s broken at %s: %w", b.TargetRef, b.Position,
			at.position, err)
	}
	green, held, err := greenIn(ctx, tx, b.RepoID, b.TargetRef)
	if err != nil {
		return 0, err
	}
	above := int64(-1)
	if held {
		above = green.Position.Packed()
	}
	settled, err := settleTipWork(ctx, tx, b.RepoID, b.TargetRef, above)
	if err != nil {
		return 0, fmt.Errorf("%w (the broken at %s)", err, at.position)
	}
	if _, err := tx.ExecContext(ctx, touchedSQL, at.packed, b.RepoID); err != nil {
		return 0, fmt.Errorf("gitlet: move %s's version at %s: %w", b.RepoID, at.position, err)
	}
	return 2 + settled, nil
}

// applyQueueSweep removes the repository's queue history from before Through,
// oldest first, within [QueueSweepRows] and [QueueSweepBytes] of ejections, as
// its decide counted from the same rows, and moves its version.
func (a *Applier) applyQueueSweep(ctx context.Context, tx *sql.Tx, at applyContext, s QueueSweep) (int, error) {
	swept, err := sweepQueueHistory(ctx, tx, s.RepoID, store.EncodeTime(s.Through), QueueSweepRows,
		QueueSweepBytes)
	if err != nil {
		return 0, err
	}
	if _, err := tx.ExecContext(ctx, touchedSQL, at.packed, s.RepoID); err != nil {
		return 0, fmt.Errorf("gitlet: move %s's version at %s: %w", s.RepoID, at.position, err)
	}
	return swept + 1, nil
}

// abandonTrainOn abandons target's live train on repoID, if any: its testing
// entries waiting again, naming no train, and its candidates removed. It
// answers the rows it wrote.
func abandonTrainOn(ctx context.Context, tx *sql.Tx, repoID, target string) (int, error) {
	reset, err := tx.ExecContext(ctx, resetTrainSQL, repoID, target)
	if err != nil {
		return 0, err
	}
	dropped, err := tx.ExecContext(ctx, dropTrainSQL, repoID, target)
	if err != nil {
		return 0, err
	}
	r, _ := reset.RowsAffected()
	d, _ := dropped.RowsAffected()
	return int(r + d), nil
}

// dropLandedCandidates removes l's train's candidates up to the one whose tip
// is the landing's, and answers how many. The later ones test on: each was
// built on the one before, whose tip the target now holds.
func dropLandedCandidates(ctx context.Context, tx *sql.Tx, l Land) (int, error) {
	train, err := trainsOf(ctx, tx, l.RepoID, l.TargetRef)
	if err != nil {
		return 0, err
	}
	i := slices.IndexFunc(train, func(c CandidateRow) bool { return c.Train == l.Train && c.TipOID == l.TipOID })
	if i < 0 {
		return 0, nil
	}
	res, err := tx.ExecContext(ctx, dropLandedSQL, l.RepoID, l.TargetRef, l.Train, train[i].Ordinal)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

// addQueuePack writes a queue record's pack (a train's, of kind candidate) as
// a live pack of the repository, moving its counters and its version; a record
// with no pack moves the version alone. It answers the rows it wrote.
func addQueuePack(ctx context.Context, tx *sql.Tx, at applyContext, repoID string, pack *PackRef,
	kind PackKind) (int, error) {

	if pack == nil {
		if _, err := tx.ExecContext(ctx, touchedSQL, at.packed, repoID); err != nil {
			return 0, fmt.Errorf("gitlet: move %s's version at %s: %w", repoID, at.position, err)
		}
		return 1, nil
	}
	// Its content dates from this record's position and broker instant, as a
	// push's pack's does: what it brings reaches the repository here.
	res, err := tx.ExecContext(ctx, addQueuePackSQL, repoID, pack.PackID, pack.Object.String(), string(pack.Hash),
		pack.Size, pack.IndexBytes, pack.ReverseBytes, pack.GitChecksum, pack.Objects, string(kind), at.packed,
		at.packed, at.at())
	if err != nil {
		return 0, fmt.Errorf("gitlet: write pack %s at %s: %w", pack.PackID, at.position, err)
	}
	added, _ := res.RowsAffected()
	if _, err := tx.ExecContext(ctx, queuePackCountersSQL, added, added*pack.Size, added*pack.Objects, at.packed,
		repoID); err != nil {
		return 0, fmt.Errorf("gitlet: count pack %s at %s: %w", pack.PackID, at.position, err)
	}
	return 2, nil
}

// sweepQueueHistory removes the cut [queueHistoryIn] takes of repoID's queue
// history before cutoff (a store.EncodeTime instant) within rows and bytes,
// each removal a key range up to the last row the walk took, so it removes
// exactly what that counts in the same rows. It answers how many it removed.
func sweepQueueHistory(ctx context.Context, tx *sql.Tx, repoID string, cutoff int64, rows int,
	bytes int64) (int, error) {

	cut, _, err := queueHistoryIn(ctx, tx, repoID, cutoff, rows, bytes)
	if err != nil {
		return 0, err
	}
	removed := 0
	if cut.entries > 0 {
		res, err := tx.ExecContext(ctx, sweepEndedSQL, repoID, cut.endedAt, cut.endedAt, cut.entryID)
		if err != nil {
			return 0, fmt.Errorf("gitlet: remove %s's ended queue entries: %w", repoID, err)
		}
		n, _ := res.RowsAffected()
		removed += int(n)
	}
	for _, l := range cut.landings {
		res, err := tx.ExecContext(ctx, sweepLandingsSQL, repoID, l.target, l.position, l.position, l.ordinal)
		if err != nil {
			return 0, fmt.Errorf("gitlet: remove %s's landings on %s: %w", repoID, l.target, err)
		}
		n, _ := res.RowsAffected()
		removed += int(n)
	}
	return removed, nil
}

// historyCut is what of a repository's queue history lies past a cutoff within
// a budget: how many ended entries, the key of the last (every entry before it
// in key order goes too) and what their ejections weigh; and for each target
// the last landing below its green row that goes, every one before it too.
type historyCut struct {
	entries  int
	endedAt  int64
	entryID  string
	weighed  int64
	landings []landingCut
}

// landingCut is the last landing of target to remove — its key — and how many
// it takes with it.
type landingCut struct {
	target   string
	position int64
	ordinal  int
	rows     int
}

// rows is how many rows the cut removes.
func (h historyCut) rows() int {
	n := h.entries
	for _, l := range h.landings {
		n += l.rows
	}
	return n
}

// queueHistoryIn is what of repoID's queue history lies before cutoff within
// budget rows, and whether more lies past it: ended entries oldest first over
// code_queue_ended_idx, weighed as a purge part is ([PurgePartBytes]) by
// ejected_bytes from the index alone, taken while those before weigh under
// bytes; then, only if neither bound stopped them, each target's landings
// below its green row, stopping at the first inside the history.
//
// The cut names its last entry by its key (ended_at, entry_id): a landing ends
// up to [MaxLandEntries] entries at one instant, and "the first n" of a tie
// would follow the engine's row order, which copies need not share. The
// landing stop is safe, not exact, as sweepThrough's is: positions are only
// nearly monotone in instants, so a landing may wait a record, by member skew.
func queueHistoryIn(ctx context.Context, tx *sql.Tx, repoID string, cutoff int64, budget int,
	bytes int64) (historyCut, bool, error) {

	var cut historyCut
	if budget <= 0 {
		return cut, false, nil
	}
	more, err := endedWithin(ctx, tx, repoID, cutoff, budget, bytes, &cut)
	if err != nil || more {
		return cut, more, err
	}
	greens, err := greenRowsIn(ctx, tx, repoID)
	if err != nil {
		return historyCut{}, false, err
	}
	left := budget - cut.entries
	for _, g := range greens {
		if left <= 0 {
			more = true
			break
		}
		rows, err := tx.QueryContext(ctx, landingsBelowSQL, repoID, g.TargetRef, g.Position.Packed(), left+1)
		if err != nil {
			return historyCut{}, false, fmt.Errorf("gitlet: read %s's landings below its green: %w", g.TargetRef, err)
		}
		l := landingCut{target: g.TargetRef}
		for rows.Next() {
			var (
				position, instant int64
				ordinal           int
			)
			if err := rows.Scan(&position, &ordinal, &instant); err != nil {
				_ = rows.Close()
				return historyCut{}, false, fmt.Errorf("gitlet: read %s's landings below its green: %w",
					g.TargetRef, err)
			}
			if instant >= cutoff {
				break
			}
			if l.rows == left {
				more = true
				break
			}
			l.position, l.ordinal = position, ordinal
			l.rows++
		}
		closeErr := rows.Close()
		if err := rows.Err(); err != nil {
			return historyCut{}, false, err
		}
		if closeErr != nil {
			return historyCut{}, false, closeErr
		}
		if l.rows > 0 {
			cut.landings = append(cut.landings, l)
			left -= l.rows
		}
	}
	return cut, more, nil
}

// endedWithin walks repoID's ended entries from before cutoff into cut, oldest
// first by key: at most budget, and none more once those taken weigh bytes. It
// answers whether an entry past the cut is left.
func endedWithin(ctx context.Context, tx *sql.Tx, repoID string, cutoff int64, budget int, bytes int64,
	cut *historyCut) (bool, error) {

	rows, err := tx.QueryContext(ctx, endedWithinSQL, repoID, cutoff, budget+1)
	if err != nil {
		return false, fmt.Errorf("gitlet: read %s's ended queue entries: %w", repoID, err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		if cut.entries == budget || (cut.entries > 0 && cut.weighed >= bytes) {
			return true, nil
		}
		var weighs int64
		if err := rows.Scan(&cut.endedAt, &cut.entryID, &weighs); err != nil {
			return false, fmt.Errorf("gitlet: read %s's ended queue entries: %w", repoID, err)
		}
		cut.entries++
		cut.weighed += weighs
	}
	if err := rows.Err(); err != nil {
		return false, fmt.Errorf("gitlet: read %s's ended queue entries: %w", repoID, err)
	}
	return false, nil
}

// greenRowsIn is repoID's green rows, by target.
func greenRowsIn(ctx context.Context, tx *sql.Tx, repoID string) ([]GreenRow, error) {
	rows, err := tx.QueryContext(ctx, greensSQL, repoID)
	if err != nil {
		return nil, fmt.Errorf("gitlet: read %s's green rows: %w", repoID, err)
	}
	defer func() { _ = rows.Close() }()
	var out []GreenRow
	for rows.Next() {
		var (
			g         = GreenRow{RepoID: repoID}
			packed, t int64
		)
		if err := rows.Scan(&g.TargetRef, &g.CommitOID, &packed, &t); err != nil {
			return nil, fmt.Errorf("gitlet: read %s's green rows: %w", repoID, err)
		}
		g.Position, g.At = unpacked(packed), store.DecodeTime(t)
		out = append(out, g)
	}
	return out, rows.Err()
}

// moveGreen moves target's green row on repoID to commit at position, the
// landing record whose tip it is, at the record's instant ([advanceGreen]'s
// upsert).
func moveGreen(ctx context.Context, tx store.Execer, at applyContext, repoID, target, commit string,
	position int64) error {

	if _, err := tx.ExecContext(ctx, advanceGreenSQL, repoID, target, commit, position, at.at()); err != nil {
		return fmt.Errorf("gitlet: move %s's green row on %s to %s at %s: %w", target, repoID, short(commit),
			at.position, err)
	}
	return nil
}

// entryRow is a queue entry as a queue record writes it back: its row, the
// ejection's JSON an eject keeps ("" otherwise), and the close a landing
// leaves it owing (code_queue.close_op: only a land sets it, only a
// close_settled clears it).
type entryRow struct {
	queueEntry
	ejected string
	closeOp string
}

// entryWrite is the conflict clause of a queue record's upsert of its entries,
// naming only the columns that write moves: the driver rewrites a row's entry
// in every index whose key or predicate holds a named column, whatever the
// value, so an entry put in a candidate keeps its live-index entry and only an
// ending moves it into the ended index.
type entryWrite string

// The writes of a queue record's entries.
const (
	entryTested entryWrite = `ON CONFLICT (repo_id, entry_id) DO UPDATE SET state = excluded.state,
		train = excluded.train, candidate_id = excluded.candidate_id, commit_oid = excluded.commit_oid`
	entryEjected entryWrite = `ON CONFLICT (repo_id, entry_id) DO UPDATE SET state = excluded.state,
		ended_at = excluded.ended_at, ended_position = excluded.ended_position, end_reason = excluded.end_reason,
		ejected = excluded.ejected, ejected_bytes = excluded.ejected_bytes`
	entryLanded entryWrite = `ON CONFLICT (repo_id, entry_id) DO UPDATE SET state = excluded.state,
		ended_at = excluded.ended_at, ended_position = excluded.ended_position, end_reason = excluded.end_reason,
		commit_oid = excluded.commit_oid, close_op = excluded.close_op`
)

// writeEntries writes a queue record's entries (up to [MaxLandEntries], the
// rows the record read) back over their rows by w, in as few upserts on the
// entry's key as the engine's parameters allow ([store.InsertRows]), as
// [writeChanges] does: a statement an entry would hold every data node's one
// write lane through the queue's hottest record (BenchmarkApplyALandRecord).
// Every entry is one the record read live, so the insert half never takes.
func writeEntries(ctx context.Context, tx store.Execer, maxVariables int, w entryWrite, rows []entryRow) error {
	_, err := store.InsertRows(ctx, tx, maxVariables, insertEntriesSQL, entryValuesSQL, string(w), len(rows),
		func(i int) []any {
			e := rows[i]
			return []any{e.RepoID, e.EntryID, e.TargetRef, e.ChangeID, e.Seq, e.HeadOID, string(e.Method),
				e.DeleteSource, e.CloseTask, e.EnqueuedBy.Handle, string(e.EnqueuedBy.Kind), e.EnqueuedOperator,
				string(e.State), e.Train, e.CandidateID, e.CommitOID, e.EnqueuedAt, e.EndedAt, e.EndedPosition,
				string(e.EndReason), e.WorkingFor, e.ejected, len(e.ejected), e.closeOp}
		})
	return err
}

// The queue's own records' writes, each registered in plan_test.go but the
// entries' upserts, whose conflict is found by their key. A partial index's
// predicate is spelled as the index declares it, and the history is walked
// oldest first in its key's order and removed over that key's range up to the
// last row walked.
const (
	// insertEntriesSQL and entryValuesSQL are entry rows as [writeEntries]
	// writes them: every column [entryColumns] reads (working_for written back
	// as read, since no upsert's conflict sets it), the ejection, and the
	// owed close.
	insertEntriesSQL = `INSERT INTO code_queue (` + entryColumns + `, ejected, ejected_bytes, close_op) VALUES `
	entryValuesSQL   = `(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`

	// settleClosesSQL clears the owed closes of a repository's entries named
	// by a JSON list, each by its key.
	settleClosesSQL = `UPDATE code_queue SET close_op = '' WHERE rowid IN (SELECT q.rowid
		FROM json_each(?1) AS j CROSS JOIN code_queue AS q INDEXED BY ` + queueKey + `
		WHERE q.repo_id = ?2 AND q.entry_id = CAST(j.value AS TEXT))`

	resetTrainSQL = `UPDATE code_queue SET state = 'waiting', train = '', candidate_id = '', commit_oid = ''
		WHERE rowid IN (SELECT rowid FROM code_queue INDEXED BY code_queue_live_idx
			WHERE repo_id = ? AND target_ref = ? AND state IN ('waiting', 'testing') AND state = 'testing')`
	dropTrainSQL = `DELETE FROM code_candidates WHERE rowid IN (SELECT rowid FROM code_candidates
		INDEXED BY code_candidates_train_idx WHERE repo_id = ? AND target_ref = ?)`
	dropLandedSQL = `DELETE FROM code_candidates WHERE rowid IN (SELECT rowid FROM code_candidates
		INDEXED BY code_candidates_train_idx WHERE repo_id = ? AND target_ref = ? AND train = ? AND ordinal <= ?)`
	addQueuePackSQL = `INSERT INTO code_packs (repo_id, pack_id, part, object, hash, size, index_bytes,
			reverse_bytes, git_checksum, objects, kind, state, added_position, content_position, arrived_at)
		VALUES (?, ?, 'pack', ?, ?, ?, ?, ?, ?, ?, ?, 'live', ?, ?, ?)
		ON CONFLICT (repo_id, pack_id, part) DO NOTHING`
	queuePackCountersSQL = `UPDATE code_repos SET live_packs = live_packs + ?, live_bytes = live_bytes + ?,
		live_objects = live_objects + ?, version = ? WHERE id = ?`
	brokenLandingSQL = `UPDATE code_landings SET broken = 1
		WHERE repo_id = ? AND target_ref = ? AND position = ? AND ordinal = ?`
	endedWithinSQL = `SELECT ended_at, entry_id, ejected_bytes FROM code_queue INDEXED BY code_queue_ended_idx
		WHERE repo_id = ? AND state IN ('landed', 'ejected', 'dequeued') AND ended_at < ?
		ORDER BY ended_at, entry_id LIMIT ?`
	sweepEndedSQL = `DELETE FROM code_queue WHERE rowid IN (SELECT rowid FROM code_queue
		INDEXED BY code_queue_ended_idx
		WHERE repo_id = ? AND state IN ('landed', 'ejected', 'dequeued') AND ended_at <= ?
			AND (ended_at < ? OR entry_id <= ?))`
	landingsBelowSQL = `SELECT position, ordinal, at FROM code_landings
		WHERE repo_id = ? AND target_ref = ? AND position < ? ORDER BY position, ordinal LIMIT ?`
	sweepLandingsSQL = `DELETE FROM code_landings WHERE rowid IN (SELECT rowid FROM code_landings
		WHERE repo_id = ? AND target_ref = ? AND position <= ? AND (position < ? OR ordinal <= ?))`
)

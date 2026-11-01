package code

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"slices"

	"github.com/crewlet/crewlet/internal/store"
)

// THE QUEUE'S OWN RECORDS' APPLIES (queuerecords.go) — each in the one
// transaction the framework holds a record's apply to, PURE IN THE RECORD, ITS
// POSITION AND ITS INSTANT, as an enqueue's is (apply_queue.go): the decide
// held its entries live, its target at its base and its landing's every gate
// met; the broker's arbitration on the repository's subject guarantees the
// record applies against the rows that decide read. So every guard here is a
// SKIP, never a refusal, which would stop the log on every node: a record that
// finds its rows not as its decide held them applies as NOTHING, whole,
// logged (code_queue_not_applied).
//
// EVERY RECORD MOVES THE REPOSITORY'S VERSION, so the version guard every
// record on its subject meets skips it when it is met again — a landing's
// through the counters of the pushes it lands.
//
// # A train is abandoned whole
//
// A target holds one train at most, and a train is valid only while every
// entry it tests is live in it: so a candidate ABANDONS the target's earlier
// train, and an eject, a dequeue and a landing of an entry a train tests end
// that train's claim on the rest — its candidates removed (a train that ends
// removes them, so a read of the target's trains names its live one alone)
// and its other entries waiting again, for its home's next candidate.
// Records of a change's own that end a testing entry — a close, a draft, a
// retarget, a push to its head — leave the train naming an entry no longer
// live, which no landing of it then lands (its decide refuses a train whose
// entries are not live) and the home's next candidate abandons.
//
// # The queue's history keeps pace with its records
//
// Each candidate, eject and land removes its repository's queue history past
// [QueueHistory] — its ended entries, then its targets' landings below their
// green rows, oldest first — at most [queueHorizonRows] a record, as many as a
// landing ends and lands, so a working queue keeps its own thirty days; an
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
	if tip, err := targetTip(ctx, tx, c.RepoID, c.TargetRef); err != nil {
		return 0, err
	} else if tip != c.BaseOID {
		return queueNotApplied(ctx, at, c.RepoID, "", c.TargetRef+" is at "+short(tip)+", and the train was "+
			"built onto "+short(c.BaseOID))
	}
	rows, err := abandonTrainOn(ctx, tx, c.RepoID, c.TargetRef)
	if err != nil {
		return 0, fmt.Errorf("code: abandon %s's train at %s: %w", c.TargetRef, at.position, err)
	}
	base := c.BaseOID
	bases := make([]string, len(c.Candidates))
	for i, cand := range c.Candidates {
		bases[i], base = base, cand.TipOID
		for _, e := range cand.Entries {
			if _, err := tx.ExecContext(ctx, testEntrySQL, c.Train, cand.CandidateID, e.Commit, c.RepoID,
				e.EntryID); err != nil {
				return 0, fmt.Errorf("code: test entry %s at %s: %w", e.EntryID, at.position, err)
			}
			rows++
		}
	}
	encoded := make([]string, len(c.Candidates))
	for i, cand := range c.Candidates {
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
	if _, err := store.InsertRows(ctx, tx, at.maxVariables,
		`INSERT INTO code_candidates (repo_id, candidate_id, target_ref, train, ordinal, base_oid, tip_oid,
			pack_id, state, position, entries) VALUES `, `(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		`ON CONFLICT (repo_id, candidate_id) DO NOTHING`, len(c.Candidates), func(i int) []any {
			cand := c.Candidates[i]
			return []any{c.RepoID, cand.CandidateID, c.TargetRef, c.Train, cand.Ordinal, bases[i], cand.TipOID,
				packID, candidateTesting, at.packed, encoded[i]}
		}); err != nil {
		return 0, fmt.Errorf("code: write train %s at %s: %w", c.Train, at.position, err)
	}
	rows += len(c.Candidates)
	added, err := addQueuePack(ctx, tx, at, c.RepoID, c.Pack, PackCandidate)
	if err != nil {
		return 0, err
	}
	rows += added
	swept, err := sweepQueueHistory(ctx, tx, c.RepoID, store.EncodeTime(at.brokerAt.Add(-QueueHistory)), queueHorizonRows)
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
	rows := 0
	for _, x := range e.Entries {
		detail, err := ejectedDetail(x)
		if err != nil {
			return 0, err
		}
		if _, err := tx.ExecContext(ctx, ejectEntrySQL, at.at(), at.packed, string(x.Reason), detail, e.RepoID,
			x.EntryID); err != nil {
			return 0, fmt.Errorf("code: eject entry %s at %s: %w", x.EntryID, at.position, err)
		}
		rows++
	}
	if testing {
		abandoned, err := abandonTrainOn(ctx, tx, e.RepoID, target)
		if err != nil {
			return 0, fmt.Errorf("code: abandon %s's train at %s: %w", target, at.position, err)
		}
		rows += abandoned
	}
	if _, err := tx.ExecContext(ctx, touchedSQL, at.packed, e.RepoID); err != nil {
		return 0, fmt.Errorf("code: move %s's version at %s: %w", e.RepoID, at.position, err)
	}
	swept, err := sweepQueueHistory(ctx, tx, e.RepoID, store.EncodeTime(at.brokerAt.Add(-QueueHistory)), queueHorizonRows)
	if err != nil {
		return 0, err
	}
	return rows + 1 + swept, nil
}

// applyLand lands a queue's entries as one record — see queuerecords.go's head:
//
//  1. EACH CHANGE MERGED FIRST — `merged_via` queue, at the commit
//     the queue built of it, merged by its ENQUEUER — so that the pushes this
//     record lands find it merged, as a merge's push finds its change
//     ([Applier.applyMerge]): a push setting a target to a live change's head
//     would otherwise merge it, and one deleting its source close it.
//  2. Each entry landed, at its commit; its train's landed candidates
//     removed, its candidates after them testing on.
//  3. The target's landing rows in chain order, one an entry, each GREEN —
//     the decide proved every required check on the tip — and its green row
//     moved to the tip.
//  4. The queue's history past its horizon — before the pushes, whose own
//     horizons take what the record's rows leave.
//  5. THE PUSHES, one an entry, through the one apply every push takes
//     ([Applier.landPushes]): the target moved from what the entry before it
//     left onto its commit, its source deleted where the decide stamped it,
//     a push row `via queue` naming its enqueuer and what it introduced, the
//     repository's counters, and the horizons.
func (a *Applier) applyLand(ctx context.Context, tx *sql.Tx, at applyContext, l Land) (int, error) {
	named := make([]string, len(l.Entries))
	for i, e := range l.Entries {
		named[i] = e.EntryID
	}
	entries, err := entriesNamed(ctx, tx, l.RepoID, named)
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
		row, found, err := changeRowIn(ctx, tx, e.ChangeID)
		if err != nil {
			return 0, err
		}
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
	if tip, err := targetTip(ctx, tx, l.RepoID, l.TargetRef); err != nil {
		return 0, err
	} else if tip != l.BaseOID {
		return queueNotApplied(ctx, at, l.RepoID, "", l.TargetRef+" is at "+short(tip)+", and the landing was "+
			"built onto "+short(l.BaseOID))
	}
	pushes := l.pushes(entries, changes)
	// ITS PUSHES LAND WHOLE WITH IT OR NOT AT ALL, as a merge's does.
	landed, err := keySet(ctx, tx, pushesAppliedSQL, []string{pushes[0].PushID})
	if err != nil {
		return 0, fmt.Errorf("code: read whether the landing at %s landed its pushes: %w", at.position, err)
	}
	if landed[pushes[0].PushID] {
		return queueNotApplied(ctx, at, l.RepoID, "", "its push "+pushes[0].PushID+" is in the repository's "+
			"history already")
	}

	// 1 EVERY CHANGE MERGED FIRST, by its enqueuer — the queue never acts on
	// its own account — one version more each at the record's position.
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
		next.version, next.position = row.version+1, at.packed
		merged[i] = &next
	}
	if err := writeChanges(ctx, tx, at.maxVariables, changeMerged, merged); err != nil {
		return 0, fmt.Errorf("code: merge the changes the landing at %s lands: %w", at.position, err)
	}
	rows := len(merged)

	// 2 EVERY ENTRY LANDED, and a train's landed candidates removed.
	for _, e := range l.Entries {
		if _, err := tx.ExecContext(ctx, landEntrySQL, at.at(), at.packed, e.Commit, l.RepoID,
			e.EntryID); err != nil {
			return 0, fmt.Errorf("code: land entry %s at %s: %w", e.EntryID, at.position, err)
		}
		rows++
	}
	dropped, err := dropLandedCandidates(ctx, tx, l)
	if err != nil {
		return 0, fmt.Errorf("code: remove train %s's landed candidates at %s: %w", l.Train, at.position, err)
	}
	rows += dropped

	// 3 THE LANDINGS, in chain order, and the green row.
	landings := make([]landing, len(l.Entries))
	from := l.BaseOID
	for i, e := range l.Entries {
		by := entries[e.EntryID].EnqueuedBy.Handle
		landings[i] = landing{target: l.TargetRef, ordinal: i + 1, kind: LandedPreLand, change: e.ChangeID,
			entry: e.EntryID, base: from, commit: e.Commit, enqueuedBy: by, by: by, green: true}
		from = e.Commit
	}
	if err := writeLandings(ctx, tx, at, l.RepoID, landings); err != nil {
		return 0, err
	}
	rows += len(landings)
	if err := advanceGreen(ctx, tx, at, l.RepoID, l.TargetRef, l.TipOID); err != nil {
		return 0, err
	}
	rows++

	// 4 THE QUEUE'S HISTORY, before the pushes' own horizons.
	swept, err := sweepQueueHistory(ctx, tx, l.RepoID, store.EncodeTime(at.brokerAt.Add(-QueueHistory)), queueHorizonRows)
	if err != nil {
		return 0, err
	}
	rows += swept

	// 5 THE PUSHES, with what the rows above leave the record's horizons.
	pushed, err := a.landPushes(ctx, tx, at, PushBatch{RepoID: l.RepoID, Pushes: pushes}, recordPacks{}, rows)
	if err != nil {
		return 0, err
	}
	return rows + pushed, nil
}

// applyGreen moves the target's green row onto the landing record at Through —
// its position the landing's, so a read of the target's landings above its
// green lists what came after it — and marks every landing up to it green.
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
	if err := moveGreen(ctx, tx, at, g.RepoID, g.TargetRef, g.OID, g.Through.Packed()); err != nil {
		return 0, err
	}
	res, err := tx.ExecContext(ctx, greenLandingsSQL, g.RepoID, g.TargetRef, from, g.Through.Packed())
	if err != nil {
		return 0, fmt.Errorf("code: mark %s's landings green up to %s at %s: %w", g.TargetRef, g.Through,
			at.position, err)
	}
	marked, _ := res.RowsAffected()
	if _, err := tx.ExecContext(ctx, touchedSQL, at.packed, g.RepoID); err != nil {
		return 0, fmt.Errorf("code: move %s's version at %s: %w", g.RepoID, at.position, err)
	}
	return 2 + int(marked), nil
}

// applyBroken marks the landing the record names broken. Its target's green
// row stays where it is: it only ever advances on green.
func (a *Applier) applyBroken(ctx context.Context, tx *sql.Tx, at applyContext, b Broken) (int, error) {
	landing, found, err := landingAt(ctx, tx, b.RepoID, b.TargetRef, b.Position.Packed(), b.Ordinal)
	if err != nil {
		return 0, err
	}
	if !found || landing.commit != b.OID {
		return queueNotApplied(ctx, at, b.RepoID, "", b.TargetRef+" holds no landing of "+short(b.OID)+" at "+
			b.Position.String())
	}
	if _, err := tx.ExecContext(ctx, brokenLandingSQL, b.RepoID, b.TargetRef, b.Position.Packed(),
		b.Ordinal); err != nil {
		return 0, fmt.Errorf("code: mark %s's landing at %s broken at %s: %w", b.TargetRef, b.Position,
			at.position, err)
	}
	if _, err := tx.ExecContext(ctx, touchedSQL, at.packed, b.RepoID); err != nil {
		return 0, fmt.Errorf("code: move %s's version at %s: %w", b.RepoID, at.position, err)
	}
	return 2, nil
}

// applyQueueSweep removes the repository's queue history from before Through,
// oldest first, at most [QueueSweepRows] — what its decide counted from the
// same rows — and moves its version.
func (a *Applier) applyQueueSweep(ctx context.Context, tx *sql.Tx, at applyContext, s QueueSweep) (int, error) {
	swept, err := sweepQueueHistory(ctx, tx, s.RepoID, store.EncodeTime(s.Through), QueueSweepRows)
	if err != nil {
		return 0, err
	}
	if _, err := tx.ExecContext(ctx, touchedSQL, at.packed, s.RepoID); err != nil {
		return 0, fmt.Errorf("code: move %s's version at %s: %w", s.RepoID, at.position, err)
	}
	return swept + 1, nil
}

// abandonTrainOn abandons target's live train on repoID, if it holds one: its
// testing entries waiting again, naming no train, and its candidates removed.
// It answers the rows it wrote.
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

// dropLandedCandidates removes the candidates of l's train the landing
// landed — those up to the one whose tip is the landing's — and answers how
// many. The train's later candidates test on: each was built on the one
// before, whose tip the target now holds.
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

// addQueuePack writes a queue record's pack — a train's, of kind candidate —
// as a live pack of the repository, its counters moved with it and its version
// to the record's position; a record with no pack moves the version alone. It
// answers the rows it wrote.
func addQueuePack(ctx context.Context, tx *sql.Tx, at applyContext, repoID string, pack *PackRef,
	kind PackKind) (int, error) {

	if pack == nil {
		if _, err := tx.ExecContext(ctx, touchedSQL, at.packed, repoID); err != nil {
			return 0, fmt.Errorf("code: move %s's version at %s: %w", repoID, at.position, err)
		}
		return 1, nil
	}
	// ITS CONTENT DATES FROM ITS OWN POSITION, and arrived at its own broker
	// instant, as a push's pack's does: what it brings reaches the repository
	// here.
	res, err := tx.ExecContext(ctx, addQueuePackSQL, repoID, pack.PackID, pack.Object.String(), string(pack.Hash),
		pack.Size, pack.IndexBytes, pack.ReverseBytes, pack.GitChecksum, pack.Objects, string(kind), at.packed,
		at.packed, at.at())
	if err != nil {
		return 0, fmt.Errorf("code: write pack %s at %s: %w", pack.PackID, at.position, err)
	}
	added, _ := res.RowsAffected()
	if _, err := tx.ExecContext(ctx, queuePackCountersSQL, added, added*pack.Size, added*pack.Objects, at.packed,
		repoID); err != nil {
		return 0, fmt.Errorf("code: count pack %s at %s: %w", pack.PackID, at.position, err)
	}
	return 2, nil
}

// sweepQueueHistory removes repoID's queue history from before cutoff (a
// store.EncodeTime instant), oldest first, at most budget rows — its ended
// entries first, then each target's landings below its green row — and
// answers how many it removed: exactly what [queueHistoryIn] counts in the
// same rows.
func sweepQueueHistory(ctx context.Context, tx *sql.Tx, repoID string, cutoff int64, budget int) (int, error) {
	cut, _, err := queueHistoryIn(ctx, tx, repoID, cutoff, budget)
	if err != nil {
		return 0, err
	}
	removed := 0
	if cut.entries > 0 {
		res, err := tx.ExecContext(ctx, sweepEndedSQL, repoID, cutoff, cut.entries)
		if err != nil {
			return 0, fmt.Errorf("code: remove %s's ended queue entries: %w", repoID, err)
		}
		n, _ := res.RowsAffected()
		removed += int(n)
	}
	for _, l := range cut.landings {
		res, err := tx.ExecContext(ctx, sweepLandingsSQL, repoID, l.target, l.position, l.position, l.ordinal)
		if err != nil {
			return 0, fmt.Errorf("code: remove %s's landings on %s: %w", repoID, l.target, err)
		}
		n, _ := res.RowsAffected()
		removed += int(n)
	}
	return removed, nil
}

// historyCut is what of a repository's queue history lies past a cutoff within
// a budget: how many of its ended entries, and for each target the last
// landing below its green row that does, every one before it doing too.
type historyCut struct {
	entries  int
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

// queueHistoryIn is what of repoID's queue history lies before cutoff — ended
// entries oldest first over code_queue_ended_idx, then each target's landings
// below its green row in the order they landed, stopping at the first that
// does not, since a later landing is no older — within budget rows, and
// whether more lies past it.
//
// THE WALK STOPS AT THE FIRST LANDING INSIDE THE HISTORY, and that stop is
// SAFE, never exact, as a retired-pack sweep's is (sweepThrough): positions
// are only nearly monotone in instants, so a landing behind a later-stamped
// one waits for the next record — at most the members' skew past its history.
func queueHistoryIn(ctx context.Context, tx *sql.Tx, repoID string, cutoff int64, budget int) (historyCut,
	bool, error) {

	var cut historyCut
	if budget <= 0 {
		return cut, false, nil
	}
	var ended int
	if err := tx.QueryRowContext(ctx, endedBeforeSQL, repoID, cutoff, budget+1).Scan(&ended); err != nil {
		return historyCut{}, false, fmt.Errorf("code: count %s's ended queue entries: %w", repoID, err)
	}
	if ended > budget {
		cut.entries = budget
		return cut, true, nil
	}
	cut.entries = ended
	left := budget - ended
	greens, err := greenRowsIn(ctx, tx, repoID)
	if err != nil {
		return historyCut{}, false, err
	}
	more := false
	for _, g := range greens {
		if left <= 0 {
			more = true
			break
		}
		rows, err := tx.QueryContext(ctx, landingsBelowSQL, repoID, g.TargetRef, g.Position.Packed(), left+1)
		if err != nil {
			return historyCut{}, false, fmt.Errorf("code: read %s's landings below its green: %w", g.TargetRef, err)
		}
		l := landingCut{target: g.TargetRef}
		for rows.Next() {
			var (
				position, instant int64
				ordinal           int
			)
			if err := rows.Scan(&position, &ordinal, &instant); err != nil {
				_ = rows.Close()
				return historyCut{}, false, fmt.Errorf("code: read %s's landings below its green: %w",
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

// greenRowsIn is repoID's green rows, by target.
func greenRowsIn(ctx context.Context, tx *sql.Tx, repoID string) ([]GreenRow, error) {
	rows, err := tx.QueryContext(ctx, greensSQL, repoID)
	if err != nil {
		return nil, fmt.Errorf("code: read %s's green rows: %w", repoID, err)
	}
	defer func() { _ = rows.Close() }()
	var out []GreenRow
	for rows.Next() {
		var (
			g         = GreenRow{RepoID: repoID}
			packed, t int64
		)
		if err := rows.Scan(&g.TargetRef, &g.CommitOID, &packed, &t); err != nil {
			return nil, fmt.Errorf("code: read %s's green rows: %w", repoID, err)
		}
		g.Position, g.At = unpacked(packed), store.DecodeTime(t)
		out = append(out, g)
	}
	return out, rows.Err()
}

// moveGreen moves target's green row on repoID to commit at position — the
// landing record whose tip it is — at the record's instant ([advanceGreen]'s
// upsert, its position the landing's).
func moveGreen(ctx context.Context, tx store.Execer, at applyContext, repoID, target, commit string,
	position int64) error {

	if _, err := tx.ExecContext(ctx, advanceGreenSQL, repoID, target, commit, position, at.at()); err != nil {
		return fmt.Errorf("code: move %s's green row on %s to %s at %s: %w", target, repoID, short(commit),
			at.position, err)
	}
	return nil
}

// The queue's own records' writes, each registered in plan_test.go: an entry
// put in a candidate, ejected and landed by its key; a target's train
// abandoned — its testing entries over code_queue_live_idx with its partial
// predicate spelled as the index declares it, and its candidates over
// code_candidates_train_idx — and a train's landed candidates; a train's pack
// and its counters; a landing broken by its key, and the landings a green
// marks over the key's range; and the queue's history — ended entries over
// code_queue_ended_idx, its predicate spelled as declared, oldest first, and a
// target's landings below its green row by the key's range, walked oldest
// first and removed up to the last one past the history.
const (
	testEntrySQL = `UPDATE code_queue SET state = 'testing', train = ?, candidate_id = ?, commit_oid = ?
		WHERE repo_id = ? AND entry_id = ?`
	ejectEntrySQL = `UPDATE code_queue SET state = 'ejected', ended_at = ?, ended_position = ?, end_reason = ?,
			ejected = ?
		WHERE repo_id = ? AND entry_id = ?`
	landEntrySQL = `UPDATE code_queue SET state = 'landed', ended_at = ?, ended_position = ?, end_reason = '',
			commit_oid = ?
		WHERE repo_id = ? AND entry_id = ?`
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
	greenLandingsSQL = `UPDATE code_landings SET green = 1 WHERE rowid IN (SELECT rowid FROM code_landings
		WHERE repo_id = ? AND target_ref = ? AND position > ? AND position <= ? AND green = 0)`
	endedBeforeSQL = `SELECT COUNT(*) FROM (SELECT 1 FROM code_queue INDEXED BY code_queue_ended_idx
		WHERE repo_id = ? AND state IN ('landed', 'ejected', 'dequeued') AND ended_at < ? LIMIT ?)`
	sweepEndedSQL = `DELETE FROM code_queue WHERE rowid IN (SELECT rowid FROM code_queue
		INDEXED BY code_queue_ended_idx
		WHERE repo_id = ? AND state IN ('landed', 'ejected', 'dequeued') AND ended_at < ?
		ORDER BY ended_at LIMIT ?)`
	landingsBelowSQL = `SELECT position, ordinal, at FROM code_landings
		WHERE repo_id = ? AND target_ref = ? AND position < ? ORDER BY position, ordinal LIMIT ?`
	sweepLandingsSQL = `DELETE FROM code_landings WHERE rowid IN (SELECT rowid FROM code_landings
		WHERE repo_id = ? AND target_ref = ? AND position <= ? AND (position < ? OR ordinal <= ?))`
)

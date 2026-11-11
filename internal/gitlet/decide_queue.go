package gitlet

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/statelog"
)

// THE QUEUE'S OWN RECORDS, DECIDED (queuerecords.go): each from ONE snapshot of
// the repository's rows on its committer, as an enqueue is — what its home
// built is proposed with what it was built from, and the decide holds the rows
// to it: the target AT THE TIP the train or the landing was built onto, read off
// the snapshot's refs (target_moved, which its home answers by building
// again); every entry LIVE in the target's queue (not_queued); and, for a
// landing, every entry's gate MET AGAIN with its enqueuer as the merger — the
// verdict race between an enqueue and its landing is the deliberate one, and
// the landing is the record that moves the branch.

// The rows each of the queue's own records makes its apply touch at most,
// beside a landing's pushes, which its decide counts as their apply writes
// them ([Committer.ProposeQueue] charges each; [TestALandRecordFitsItsCaps]
// holds them under MaxRecordRows).
const (
	// candidateDecidedRows: the target's earlier train abandoned
	// ([trainRows]), every entry of the new one testing, its candidates'
	// rows, its pack's row, the repository's and the queue's horizon.
	candidateDecidedRows = trainRows + MaxCandidateEntries + MaxQueueDepth + 1 + 1 + queueHorizonRows
	// brokenDecidedRows: the landing's row, the target's row of tip work, the
	// repository's, and the notices its routing makes.
	brokenDecidedRows = 3 + routingNoticeRows
	// greenDecidedRows: the target's green row, its row of tip work and the
	// repository's — whatever lies beneath the green: a landing is green by
	// its position, so no green writes one a landing ([Applier.applyGreen]).
	greenDecidedRows = 3
)

// closeSettledRows is the rows a close_settled of n closes makes its apply
// touch of its own: each entry's row and the repository's. Its routings'
// notices are kept within what [MaxRecordRows] leaves beside them
// ([keptRoutings]).
func closeSettledRows(n int) int { return n + 1 }

// closeSettledDecidedRows is what the committer charges a close_settled of n
// closes: its own rows and, every close not_closed, a routing's notices each
// ([routingNoticeRows]) — never past [MaxRecordRows], which its kept
// routings stay within.
func closeSettledDecidedRows(n int) int {
	return min(closeSettledRows(n)+n*routingNoticeRows, MaxRecordRows)
}

// owedNamedSQL is a repository's entries named by a JSON list, each by its
// key, with the close each owes its work item ("" for none): what a
// close_settled's decide, its apply and its answer read — one statement for
// every close the record settles.
const owedNamedSQL = `SELECT ` + qualifiedEntryColumns + `, q.close_op FROM json_each(?1) AS j
		CROSS JOIN code_queue AS q INDEXED BY ` + queueKey + `
		WHERE q.repo_id = ?2 AND q.entry_id = CAST(j.value AS TEXT)`

// ejectDecidedRows is the most rows an eject of n entries makes its apply
// touch: each entry ended, its target's train abandoned ([trainRows]), the
// repository's and the queue's horizon.
func ejectDecidedRows(n int) int { return n + trainRows + 1 + queueHorizonRows }

// landDecidedRows is the most rows a land of n entries makes its apply touch
// of its OWN, beside the pushes it lands: each entry's change merged, its
// stored readiness cleared ([clearReadiness]), its entry landed and its
// landing row; the target's green row and its row of tip work; its train's
// landed candidates; and the queue's horizon.
func landDecidedRows(n int) int { return 4*n + 2 + MaxQueueDepth + queueHorizonRows }

// landPushRows is the most rows a land's n pushes write before the changes
// they move ([entryRows]): each its push row, the target's move and its
// source's deletion — the ref, its tombstone and the object it leaves — and
// the first its pack.
func landPushRows(n int) int { return n*(1+1+3) + 1 }

// landRecordRows is a land record's rows but the changes its pushes move,
// which its decide counts ([decideLand]): what the committer charges it.
func landRecordRows(n int) int { return landDecidedRows(n) + landPushRows(n) }

// decideCandidate is a candidate's rules over the rows tx holds: the target at
// the train's base (target_moved), every entry live in the target's queue
// (not_queued), each candidate's commits what its entries' methods make (a
// fast-forward its head, a merge or a squash a commit of its own) and the pack
// carrying them (stale), and the pack held to a push's rules (upload_expired,
// repo_size, compaction_behind — m3-k5's rule 6c: a busy queue waits for
// compaction rather than outrunning it).
func decideCandidate(ctx context.Context, tx *sql.Tx, c Candidate, repo pushRepo, limits Limits, now time.Time,
	d queueDecision) (queueDecision, error) {

	if tip, err := targetTip(ctx, tx, c.RepoID, c.TargetRef); err != nil {
		return queueDecision{}, err
	} else if tip != c.BaseOID {
		return d.refuse(MergeRefusalTargetMoved, fmt.Sprintf("%s is at %s, and this train was built onto %s: "+
			"build it again onto the tip the branch holds", c.TargetRef, short(tip), short(c.BaseOID)))
	}
	var named []string
	for _, cand := range c.Candidates {
		for _, e := range cand.Entries {
			named = append(named, e.EntryID)
		}
	}
	entries, err := entriesNamed(ctx, tx, c.RepoID, named)
	if err != nil {
		return queueDecision{}, err
	}
	builds := false
	for _, cand := range c.Candidates {
		for _, e := range cand.Entries {
			entry, found := entries[e.EntryID]
			if why := whyNotLive(entry, found, c.TargetRef); why != "" {
				return d.refuse(MergeRefusalNotQueued, fmt.Sprintf("entry %s %s: its train is built again "+
					"without it", e.EntryID, why))
			}
			if why := builtAs(entry, e.Commit); why != "" {
				return d.refuse(MergeRefusalStale, fmt.Sprintf("candidate %d of train %s: %s", cand.Ordinal,
					c.Train, why))
			}
			builds = builds || entry.Method != MergeMethodFastForward
		}
	}
	if refusal, detail := queuePack(c.Pack, builds, repo, limits, now); refusal != "" {
		return d.refuse(refusal, detail)
	}
	d.payload = c
	return d, nil
}

// decideEject is an eject's rules over the rows tx holds: an ejection naming
// its repository a pull mirror on one that is not (stale), every entry live in
// its queue, and all of them one target's, whose train it abandons
// (not_queued).
func decideEject(ctx context.Context, tx *sql.Tx, e Eject, repo pushRepo, d queueDecision) (queueDecision,
	error) {

	// A REPOSITORY THAT IS A PULL MIRROR NO LONGER — its mirror_from removed
	// between the home's read and this decide — takes its queue's work again,
	// so its entries are not put out for a reason the rows no longer hold.
	if !repo.mirrored && slices.ContainsFunc(e.Entries, func(x Ejection) bool {
		return x.Reason == EjectRepoMirrored
	}) {
		return d.refuse(MergeRefusalStale, fmt.Sprintf("%s follows no source now: its queues' entries are put "+
			"out %s only while it is a pull mirror; read its queue again", repo.name, EjectRepoMirrored))
	}
	named := make([]string, len(e.Entries))
	for i, x := range e.Entries {
		named[i] = x.EntryID
	}
	entries, err := entriesNamed(ctx, tx, e.RepoID, named)
	if err != nil {
		return queueDecision{}, err
	}
	target := ""
	for _, x := range e.Entries {
		entry, found := entries[x.EntryID]
		if target == "" && found {
			target = entry.TargetRef
		}
		if why := whyNotLive(entry, found, target); why != "" {
			return d.refuse(MergeRefusalNotQueued, fmt.Sprintf("entry %s of %s's queues %s: an eject puts "+
				"one target's live entries out", x.EntryID, repo.name, why))
		}
	}
	// WHOM IT CONCERNS, a routing an entry's change: its authors and its
	// enqueuer told it left the queue — its authors that its merge
	// conflicted, where it did — the home's own record, so nobody's actor.
	ids := make([]string, len(e.Entries))
	for i, x := range e.Entries {
		ids[i] = entries[x.EntryID].ChangeID
	}
	changes, err := changeRowsIn(ctx, tx, ids)
	if err != nil {
		return queueDecision{}, err
	}
	routings := make([]ChangeRouting, 0, len(e.Entries))
	for _, x := range e.Entries {
		entry := entries[x.EntryID]
		row, found := changes[entry.ChangeID]
		if !found {
			continue
		}
		routed, routeErr := routingFor(ctx, tx, concerning{repoName: repo.name, change: row, authors: true,
			dequeued: true, conflicted: x.Reason == EjectConflict, enqueuer: entry.EnqueuedBy.reviewer()})
		if routeErr != nil {
			return queueDecision{}, routeErr
		}
		if routed != nil {
			routings = append(routings, *routed)
		}
	}
	room, err := routingRoomBeside(e)
	if err != nil {
		return queueDecision{}, err
	}
	if e.Routing, err = keptRoutings(routings, room, MaxRecordRows-ejectDecidedRows(len(e.Entries))); err != nil {
		return queueDecision{}, err
	}
	d.payload = e
	return d, nil
}

// decideLand is a landing's rules over the rows tx holds, in their order:
//
//  1. the target at the landing's base, read off the snapshot's refs
//     (target_moved);
//  2. the target a queue target (no_queue);
//  3. every entry live in the target's queue, of its change, at the head it
//     was enqueued at, its commit what its method makes (not_queued, stale);
//  4. the landing a PREFIX of the target's train — its candidates from the
//     first, in order, up to the one whose tip is the landing's — whose
//     every required check passed on its tip, by its latest attempt checked
//     for the target (stale, not_green);
//  5. EVERY ENTRY'S GATE MET AGAIN, from this snapshot, with its ENQUEUER as
//     the merger — every precondition but `mergeable`, which its candidate
//     proved on the branch, and the entry's own place in the queue
//     — its change open, not a draft, into the target at the entry's head:
//     each entry it would not land named in [QueueResult.Refused]
//     (gate_unmet, head_moved, change_not_open), for its home to eject;
//  6. its rows within a record — its own, its pushes', and the live changes
//     those pushes move, counted as their apply writes them (record_full).
//
// It STAMPS what its snapshot holds, so the apply matches no pattern: each
// entry's source deleted where the entry asked it and a push deleting it as
// its enqueuer would be taken. A landing lands GREEN, its required checks
// proved on its tip by rule 4, so its apply moves the target's green row.
func decideLand(ctx context.Context, tx *sql.Tx, l Land, repo pushRepo, seats SeatRules, limits Limits,
	d queueDecision) (queueDecision, error) {

	if tip, err := targetTip(ctx, tx, l.RepoID, l.TargetRef); err != nil {
		return queueDecision{}, err
	} else if tip != l.BaseOID {
		return d.refuse(MergeRefusalTargetMoved, fmt.Sprintf("%s is at %s, and this landing was built onto %s: "+
			"build it again onto the tip the branch holds", l.TargetRef, short(tip), short(l.BaseOID)))
	}
	rules, err := rulesOfRepos(ctx, tx, []readinessSubject{{repo: RepoRef{ID: l.RepoID}}})
	if err != nil {
		return queueDecision{}, err
	}
	gate, err := GateRulesOf(rules[l.RepoID], l.TargetRef, limits.MergeByFloor)
	if err != nil {
		return queueDecision{}, err
	}
	if !gate.QueueTarget() {
		return d.refuse(MergeRefusalNoQueue, fmt.Sprintf("%s has no merge queue — no rule matching it requires a "+
			"check — so nothing lands on it through one", l.TargetRef))
	}

	named := make([]string, len(l.Entries))
	for i, e := range l.Entries {
		named[i] = e.EntryID
	}
	entries, err := entriesNamed(ctx, tx, l.RepoID, named)
	if err != nil {
		return queueDecision{}, err
	}
	for _, e := range l.Entries {
		entry, found := entries[e.EntryID]
		if why := whyNotLive(entry, found, l.TargetRef); why != "" {
			d.refused = append(d.refused, RefusedEntry{EntryID: e.EntryID, Refusal: MergeRefusalNotQueued,
				Detail: fmt.Sprintf("entry %s %s", e.EntryID, why)})
			continue
		}
		if entry.ChangeID != e.ChangeID {
			return d.refuse(MergeRefusalStale, fmt.Sprintf("entry %s queues change %s, and this landing lands "+
				"it as change %s", e.EntryID, entry.ChangeID, e.ChangeID))
		}
		if why := builtAs(entry, e.Commit); why != "" {
			return d.refuse(MergeRefusalStale, why)
		}
	}
	if len(d.refused) > 0 {
		return d.refuse(MergeRefusalNotQueued, fmt.Sprintf("%d of the landing's %d entries are not live in %s's "+
			"queue: %s", len(d.refused), len(l.Entries), l.TargetRef, d.refused[0].Detail))
	}

	if detail, ok, err := prefixOfTrain(ctx, tx, l); err != nil || !ok { //nolint:govet // shadow: `x, err := f()` declares x too; see .golangci.yml
		if err != nil {
			return queueDecision{}, err
		}
		return d.refuse(MergeRefusalStale, detail)
	}
	checks, err := latestChecksOn(ctx, tx, l.RepoID, l.TipOID, l.TargetRef, gate.Checks)
	if err != nil {
		return queueDecision{}, err
	}
	if waiting := notPassed(gate.Checks, checks); len(waiting) > 0 {
		return d.refuse(MergeRefusalNotGreen, fmt.Sprintf("%s's required checks have not all passed on the "+
			"train's tip %s: %s — a queue lands only what it tested green", l.TargetRef, short(l.TipOID),
			strings.Join(waiting, ", ")))
	}

	// EVERY ENTRY'S GATE, AGAIN, its enqueuer the merger.
	ref := repo.grantRef(l.RepoID)
	ids := make([]string, len(l.Entries))
	for i, e := range l.Entries {
		ids[i] = e.ChangeID
	}
	changes, err := changeRowsIn(ctx, tx, ids)
	if err != nil {
		return queueDecision{}, err
	}
	subjects := make([]readinessSubject, 0, len(l.Entries))
	rows := make([]changeRow, len(l.Entries))
	for i, e := range l.Entries {
		row, found := changes[e.ChangeID]
		if !found || row.repoID != l.RepoID {
			return queueDecision{}, fmt.Errorf("code: entry %s of %s's queue names change %s, which the "+
				"repository does not hold", e.EntryID, repo.name, e.ChangeID)
		}
		rows[i] = row
		subjects = append(subjects, readinessSubject{repo: ref, row: row})
	}
	facts, err := gateFactsIn(ctx, tx, subjects)
	if err != nil {
		return queueDecision{}, err
	}
	for i, e := range l.Entries {
		entry, c := entries[e.EntryID], facts[i].Rows.Change
		named := ChangeRef(repo.name, c.Number)
		var refusal MergeRefusal
		var detail string
		switch {
		case c.State == ChangeStateClosed || c.State == ChangeStateMerged:
			refusal, detail = MergeRefusalChangeNotOpen, fmt.Sprintf("%s is %s", named, c.State)
		case c.State == ChangeStateDraft:
			refusal, detail = MergeRefusalChangeDraft, fmt.Sprintf("%s is a draft", named)
		case c.TargetRef != l.TargetRef:
			refusal, detail = MergeRefusalStale, fmt.Sprintf("%s merges into %s", named, c.TargetRef)
		case c.Head != entry.HeadOID:
			refusal, detail = MergeRefusalHeadMoved, fmt.Sprintf("%s's head is %s, and entry %s queued %s",
				named, short(c.Head), e.EntryID, short(entry.HeadOID))
		}
		if refusal != "" {
			d.refused = append(d.refused, RefusedEntry{EntryID: e.EntryID, Refusal: refusal, Detail: detail})
			continue
		}
		f := facts[i]
		f.Roster, f.Floor, f.Merger = seats.Roster, limits.MergeByFloor, entry.enqueuer()
		var unmet []Precondition
		for _, pre := range JudgeGate(f, gate).Preconditions {
			if pre.State != PreconditionMet && pre.Kind != PreconditionMergeable && pre.Kind != PreconditionQueue {
				unmet = append(unmet, pre)
			}
		}
		if len(unmet) > 0 {
			d.refused = append(d.refused, RefusedEntry{EntryID: e.EntryID, Refusal: MergeRefusalGateUnmet,
				Detail: fmt.Sprintf("%s no longer meets %s's merge gate with %s, who queued it, as the merger: %s",
					named, l.TargetRef, entry.EnqueuedBy.named(), unmet[0].Detail), Unmet: unmet})
		}
	}
	if len(d.refused) > 0 {
		first := d.refused[0]
		return d.refuse(first.Refusal, fmt.Sprintf("%d of the landing's %d entries would not land: %s",
			len(d.refused), len(l.Entries), first.Detail))
	}

	// THE SOURCES, each deleted where its entry asks it and a push deleting
	// it as its enqueuer would be taken — kept otherwise, as a merge asked
	// keeping its source; never a refusal, since nobody is asking to answer.
	sources := make([]string, 0, len(rows))
	for _, row := range rows {
		sources = append(sources, row.source)
	}
	refs, err := refsNamed(ctx, tx, l.RepoID, sources)
	if err != nil {
		return queueDecision{}, fmt.Errorf("code: read the sources the landing at %s deletes: %w", l.TargetRef, err)
	}
	stamped := l
	stamped.Entries = slices.Clone(l.Entries)
	for i, e := range stamped.Entries {
		entry, row := entries[e.EntryID], rows[i]
		if !entry.DeleteSource || refTip(refs, row.source) != row.head {
			continue
		}
		merger := entry.enqueuer()
		if r, _ := sourceDeletion(rules[l.RepoID], row.source, row.head, merger,
			GrantOf(merger, ref, seats.Roster)); r == "" {
			stamped.Entries[i].DeleteSource = true
		}
	}

	// ITS ROWS, the pushes' moves of the live changes among them, as the
	// apply writes them.
	pushes := stamped.pushes(entries, rows)
	book, err := readChangeBook(ctx, tx, l.RepoID, pushes, 0, 0)
	if err != nil {
		return queueDecision{}, err
	}
	own := landDecidedRows(len(pushes))
	for _, e := range pushes {
		book.move(e)
		own += entryRows(e)
	}
	total := recordRows(own + book.rowCount())
	if total > MaxRecordRows {
		return d.refuse(MergeRefusalRecordFull, fmt.Sprintf("landing %d entries on %s writes up to %d rows with "+
			"the live changes its pushes move, past MaxRecordRows (%d): land fewer at once", len(pushes),
			l.TargetRef, total, MaxRecordRows))
	}
	// WHOM IT CONCERNS, a routing an entry's change: its authors told it
	// merged — all but its enqueuer, who merged it as the landing's push
	// names them — within what the record's rows and bytes leave.
	routings := make([]ChangeRouting, 0, len(stamped.Entries))
	for i, e := range stamped.Entries {
		merger, mergerKind := entries[e.EntryID].enqueuer().party()
		//nolint:govet // shadow: `x, err := f()` declares x too; see .golangci.yml
		routed, err := routingFor(ctx, tx, concerning{repoName: repo.name, change: rows[i], actor: merger,
			actorKind: mergerKind, authors: true, merged: true})
		if err != nil {
			return queueDecision{}, err
		}
		if routed != nil {
			routings = append(routings, *routed)
		}
	}
	room, err := routingRoomBeside(stamped)
	if err != nil {
		return queueDecision{}, err
	}
	if stamped.Routing, err = keptRoutings(routings, room, MaxRecordRows-total); err != nil {
		return queueDecision{}, err
	}
	d.payload = stamped
	return d, nil
}

// decideGreen is a green's rules over the rows tx holds: a landing record at
// Through on the target, above its green row, whose last landing's commit is
// OID (no_landing); every check the target's gate requires passed on OID by
// its latest attempt checked for the target (not_green). Never record_full:
// it writes [greenDecidedRows] whatever lies beneath it.
func decideGreen(ctx context.Context, tx *sql.Tx, g Green, d queueDecision) (queueDecision,
	error) {

	green, held, err := greenIn(ctx, tx, g.RepoID, g.TargetRef)
	if err != nil {
		return queueDecision{}, err
	}
	if held && green.Position.Packed() >= g.Through.Packed() {
		return d.refuse(MergeRefusalNoLanding, fmt.Sprintf("%s's green row is at %s, at or past the landing at %s",
			g.TargetRef, green.Position, g.Through))
	}
	tip, found, err := landingTipAt(ctx, tx, g.RepoID, g.TargetRef, g.Through.Packed())
	switch {
	case err != nil:
		return queueDecision{}, err
	case !found:
		return d.refuse(MergeRefusalNoLanding, fmt.Sprintf("%s holds no landing at %s", g.TargetRef, g.Through))
	case tip != g.OID:
		return d.refuse(MergeRefusalNoLanding, fmt.Sprintf("the landing at %s moved %s to %s, and this green "+
			"names %s", g.Through, g.TargetRef, short(tip), short(g.OID)))
	}
	gate, err := gateOf(ctx, tx, g.RepoID, g.TargetRef)
	if err != nil {
		return queueDecision{}, err
	}
	checks, err := latestChecksOn(ctx, tx, g.RepoID, g.OID, g.TargetRef, gate.Checks)
	if err != nil {
		return queueDecision{}, err
	}
	if waiting := notPassed(gate.Checks, checks); len(waiting) > 0 {
		return d.refuse(MergeRefusalNotGreen, fmt.Sprintf("%s's required checks have not all passed on %s: %s — "+
			"a green row moves only on green", g.TargetRef, short(g.OID), strings.Join(waiting, ", ")))
	}
	d.payload = g
	return d, nil
}

// decideCloseSettled is a close_settled's rules over the rows tx holds: of
// its closes, those whose entry LANDED and still OWES its close are recorded
// and every other SKIPPED — another home's settle reached it first, which its
// asker reads as done — and a record none of whose closes is owed any more is
// refused stale. Each not_closed close recorded is stamped whom it tells: its
// change's authors and its enqueuer, under `merged`, its detail beside — the
// home's own record, so nobody's actor; nobody where the repository holds the
// change no more.
func decideCloseSettled(ctx context.Context, tx *sql.Tx, c CloseSettled, repo pushRepo, d queueDecision) (
	queueDecision, error) {

	owed, err := owedClosesNamed(ctx, tx, c.RepoID, c.entries())
	if err != nil {
		return queueDecision{}, err
	}
	stamped := CloseSettled{RepoID: c.RepoID}
	var notClosed []SettledClose
	for _, s := range c.Settled {
		if o, found := owed[s.EntryID]; found && o.entry.State == QueueLanded && o.op != "" {
			stamped.Settled = append(stamped.Settled, s)
			if s.Outcome == CloseNotClosed {
				notClosed = append(notClosed, s)
			}
		}
	}
	if len(stamped.Settled) == 0 {
		return d.refuse(MergeRefusalStale, fmt.Sprintf("none of the %d entries named owes its work item a "+
			"close: another settle reached each first, or no landing left it owing one", len(c.Settled)))
	}
	ids := make([]string, len(notClosed))
	for i, s := range notClosed {
		ids[i] = owed[s.EntryID].entry.ChangeID
	}
	changes, err := changeRowsIn(ctx, tx, ids)
	if err != nil {
		return queueDecision{}, err
	}
	routings := make([]ChangeRouting, 0, len(notClosed))
	for _, s := range notClosed {
		entry := owed[s.EntryID].entry
		row, found := changes[entry.ChangeID]
		if !found {
			continue
		}
		routed, routeErr := routingFor(ctx, tx, concerning{repoName: repo.name, change: row, authors: true,
			merged: true, enqueuer: entry.EnqueuedBy.reviewer(), notClosed: s.Detail})
		if routeErr != nil {
			return queueDecision{}, routeErr
		}
		if routed != nil {
			routings = append(routings, *routed)
		}
	}
	room, err := routingRoomBeside(stamped)
	if err != nil {
		return queueDecision{}, err
	}
	if stamped.Routing, err = keptRoutings(routings, room,
		MaxRecordRows-closeSettledRows(len(stamped.Settled))); err != nil {
		return queueDecision{}, err
	}
	d.payload = stamped
	return d, nil
}

// owedClose is a queue entry and the close it owes its work item
// (code_queue.close_op), "" for none.
type owedClose struct {
	entry queueEntry
	op    string
}

// owedClosesNamed reads repository repoID's entries ids by their key, each
// with the close it owes its work item, in one statement; an entry its
// queues hold no such row of is absent.
func owedClosesNamed(ctx context.Context, tx *sql.Tx, repoID string, ids []string) (map[string]owedClose, error) {
	out := make(map[string]owedClose, len(ids))
	list, err := jsonList(ids)
	if err != nil {
		return nil, err
	}
	rows, err := tx.QueryContext(ctx, owedNamedSQL, list, repoID)
	if err != nil {
		return nil, fmt.Errorf("code: read %d entries of %s's queues: %w", len(ids), repoID, err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var op string
		e, scanErr := scanEntry(func(dest ...any) error { return rows.Scan(append(dest, &op)...) })
		if scanErr != nil {
			return nil, fmt.Errorf("code: read %d entries of %s's queues: %w", len(ids), repoID, scanErr)
		}
		out[e.EntryID] = owedClose{entry: e, op: op}
	}
	return out, rows.Err()
}

// decideBroken is a broken's rules over the rows tx holds: the landing at its
// key, at its commit, above its target's green row (no_landing), and every
// check it names FAILED on the commit by its latest attempt checked for the
// target — a measurement, never an infrastructure's error (not_red).
func decideBroken(ctx context.Context, tx *sql.Tx, b Broken, repo pushRepo, seats SeatRules,
	d queueDecision) (queueDecision, error) {

	landing, found, err := landingAt(ctx, tx, b.RepoID, b.TargetRef, b.Position.Packed(), b.Ordinal)
	switch {
	case err != nil:
		return queueDecision{}, err
	case !found || landing.commit != b.OID:
		return d.refuse(MergeRefusalNoLanding, fmt.Sprintf("%s holds no landing of %s at %s, place %d",
			b.TargetRef, short(b.OID), b.Position, b.Ordinal))
	}
	green, held, err := greenIn(ctx, tx, b.RepoID, b.TargetRef)
	if err != nil {
		return queueDecision{}, err
	}
	if held && green.Position.Packed() >= b.Position.Packed() {
		return d.refuse(MergeRefusalNoLanding, fmt.Sprintf("the landing at %s is at or below %s's green row at "+
			"%s: green, and so not broken", b.Position, b.TargetRef, green.Position))
	}
	checks, err := latestChecksOn(ctx, tx, b.RepoID, b.OID, b.TargetRef, b.Checks)
	if err != nil {
		return queueDecision{}, err
	}
	for _, name := range b.Checks {
		i := slices.IndexFunc(checks, func(c GateCheck) bool { return c.Name == name })
		if i < 0 || checks[i].State != CheckStateFailed {
			state := "no attempt"
			if i >= 0 {
				state = string(checks[i].State)
			}
			return d.refuse(MergeRefusalNotRed, fmt.Sprintf("%s did not fail on %s (%s): a landing is broken by "+
				"a check that failed on it", name, short(b.OID), state))
		}
	}
	// WHOM IT CONCERNS: the authors of the change the landing merged and the
	// effective lead of the repository's owner — the home's own record, so
	// nobody's actor; nobody for a landing no change made. NEVER AN
	// ENQUEUER: a queue's landing lands green, its green row at its own
	// position, so the green row refuses every broken naming one above; what
	// a broken can name landed ahead of its checks — a person's override
	// merge or push — and no queue landed it.
	if landing.changeID != "" {
		row, found, err := changeRowIn(ctx, tx, landing.changeID)
		if err != nil {
			return queueDecision{}, err
		}
		if found {
			c := concerning{repoName: repo.name, change: row, authors: true, broken: true,
				ownerLead: ownersLead(repo.grantRef(b.RepoID), seats.Roster)}
			if b.Routing, err = routingFor(ctx, tx, c); err != nil {
				return queueDecision{}, err
			}
		}
	}
	d.payload = b
	return d, nil
}

// enqueuer is who enqueued e, as the principal its landing lands it as: a seat
// by its handle, or a person by the human seat they hold and the credential
// they enqueued it with (iam.ActorFor's attribution).
func (e queueEntry) enqueuer() Principal {
	if e.EnqueuedBy.Kind == PartyAgent {
		return Principal{Kind: PrincipalAgent, Seat: e.EnqueuedBy.Handle}
	}
	return Principal{Kind: PrincipalOperator, OperatorID: e.EnqueuedOperator, Seat: e.EnqueuedBy.Handle}
}

// whyNotLive is why entry, found or not, is no live entry of target's queue — ""
// where it is one.
func whyNotLive(entry queueEntry, found bool, target string) string {
	switch {
	case !found:
		return "is in no queue of the repository"
	case !entry.State.Live():
		return "is " + string(entry.State)
	case entry.TargetRef != target:
		return "is in " + entry.TargetRef + "'s queue"
	}
	return ""
}

// builtAs is why commit is not what entry's method makes of its head — "" where
// it is: a fast-forward its head itself, a merge or a squash a commit of its
// own.
func builtAs(entry queueEntry, commit string) string {
	switch {
	case entry.Method == MergeMethodFastForward && commit != entry.HeadOID:
		return fmt.Sprintf("entry %s fast-forwards to its head %s, and was built as %s", entry.EntryID,
			short(entry.HeadOID), short(commit))
	case entry.Method != MergeMethodFastForward && commit == entry.HeadOID:
		return fmt.Sprintf("entry %s lands as a %s, which builds a commit of its own, and was built as its "+
			"head %s", entry.EntryID, entry.Method, short(commit))
	}
	return ""
}

// queuePack is a train's pack held to a push's rules — or why it may not be:
// carried exactly where the train built a commit (stale), uploaded recently
// enough for a record to name it (upload_expired), within the repository's
// size (repo_size) and into a repository whose compaction keeps pace with its
// packs (compaction_behind, m3-k5's rule 6c).
func queuePack(pack *PackRef, builds bool, repo pushRepo, limits Limits, now time.Time) (MergeRefusal, string) {
	switch {
	case pack == nil && builds:
		return MergeRefusalStale, "the train builds commits of its own and carries no pack of them"
	case pack != nil && !builds:
		return MergeRefusalStale, "the train fast-forwards alone, building nothing, and carries a pack"
	case pack == nil:
		return "", ""
	}
	if detail, expired := uploadExpired("the train's pack", pack.Object, now, "its home builds it again"); expired {
		return MergeRefusalUploadExpired, detail
	}
	// THE ENGINE'S OWN BUILD, never a seat's outside share: the whole bound.
	if would := repo.liveBytes + pack.Size; would > limits.RepoMaxBytes {
		return MergeRefusalRepoSize, sizeDetail(repo.name, would, limits.RepoMaxBytes, limits, Grant{})
	}
	if repo.livePacks >= MaxPacksPerRepo {
		return MergeRefusalCompactionBehind, BehindDetail(repo.name, repo.livePacks)
	}
	return "", ""
}

// prefixOfTrain reports whether l's entries are a PREFIX of its train: the
// target's candidates of l.Train from its lowest ordinal, in order, each
// entry at the commit its candidate built of it, up to the candidate whose
// tip is l's — the first built onto l's base. Its sentence says why not.
func prefixOfTrain(ctx context.Context, tx *sql.Tx, l Land) (string, bool, error) {
	all, err := trainsOf(ctx, tx, l.RepoID, l.TargetRef)
	if err != nil {
		return "", false, err
	}
	var train []CandidateRow
	for _, c := range all {
		if c.Train == l.Train {
			train = append(train, c)
		}
	}
	switch {
	case len(train) == 0:
		return fmt.Sprintf("train %s is not %s's: its queue abandoned it, or another replaced it", l.Train,
			l.TargetRef), false, nil
	case train[0].BaseOID != l.BaseOID:
		return fmt.Sprintf("train %s's first candidate was built onto %s, and the landing onto %s", l.Train,
			short(train[0].BaseOID), short(l.BaseOID)), false, nil
	}
	i := 0
	for _, c := range train {
		for _, e := range c.Entries {
			if i >= len(l.Entries) || l.Entries[i].EntryID != e.EntryID || l.Entries[i].Commit != e.Commit {
				return fmt.Sprintf("the landing's entries are not train %s's candidates in order from its first: "+
					"its %d-th entry is not candidate %d's", l.Train, i+1, c.Ordinal), false, nil
			}
			i++
		}
		if i == len(l.Entries) {
			if c.TipOID != l.TipOID {
				return fmt.Sprintf("candidate %d of train %s tips at %s, and the landing at %s", c.Ordinal, l.Train,
					short(c.TipOID), short(l.TipOID)), false, nil
			}
			return "", true, nil
		}
	}
	return fmt.Sprintf("the landing names %d entries, past train %s's %d", len(l.Entries), l.Train, i), false, nil
}

// latestChecksOn is the latest attempt of each of names checked on commit for
// target, by name — none for a name no attempt of which is recorded there —
// in ONE statement over the checks' key, the gate's own ([gateChecksIn]).
func latestChecksOn(ctx context.Context, tx *sql.Tx, repoID, commit, target string, names []string) ([]GateCheck,
	error) {

	if len(names) == 0 {
		return nil, nil
	}
	keys := make([][4]any, len(names))
	for i, name := range names {
		keys[i] = [4]any{repoID, commit, target, name}
	}
	out := []GateFacts{{}}
	byHead := map[[3]string][]int{{repoID, commit, target}: {0}}
	if err := gateChecksIn(ctx, tx, keys, byHead, [][]string{names}, out); err != nil {
		return nil, err
	}
	return out[0].Checks, nil
}

// notPassed is every name of names whose latest attempt of checks did not pass
// — or was skipped, its paths untouched — each with where it stands.
func notPassed(names []string, checks []GateCheck) []string {
	var out []string
	for _, name := range names {
		i := slices.IndexFunc(checks, func(c GateCheck) bool { return c.Name == name })
		switch {
		case i < 0:
			out = append(out, name+" (no attempt)")
		case checks[i].State != CheckStatePassed && checks[i].State != CheckStateSkipped:
			out = append(out, fmt.Sprintf("%s (%s, attempt %d)", name, checks[i].State, checks[i].Attempt))
		}
	}
	return out
}

// gateOf is the merge gate target is held to on repoID by its rules as tx
// holds them — the checks it requires its tip to pass.
func gateOf(ctx context.Context, tx *sql.Tx, repoID, target string) (GateRules, error) {
	rules, err := rulesOfRepos(ctx, tx, []readinessSubject{{repo: RepoRef{ID: repoID}}})
	if err != nil {
		return GateRules{}, err
	}
	return GateRulesOf(rules[repoID], target, "")
}

// targetTip is target's tip on repoID as tx holds it, the zero id for none.
func targetTip(ctx context.Context, tx *sql.Tx, repoID, target string) (string, error) {
	refs, err := refsNamed(ctx, tx, repoID, []string{target})
	if err != nil {
		return "", fmt.Errorf("code: read %s's tip on %s: %w", target, repoID, err)
	}
	return refTip(refs, target), nil
}

// entriesNamed reads repository repoID's entries of ids by their key, by id —
// one statement driven from the list, a seek of the key an id.
func entriesNamed(ctx context.Context, tx *sql.Tx, repoID string, ids []string) (map[string]queueEntry, error) {
	out := map[string]queueEntry{}
	if len(ids) == 0 {
		return out, nil
	}
	list, err := jsonList(ids)
	if err != nil {
		return nil, err
	}
	rows, err := tx.QueryContext(ctx, entriesNamedSQL, list, repoID)
	if err != nil {
		return nil, fmt.Errorf("code: read %d entries of %s's queues: %w", len(ids), repoID, err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		e, err := scanEntry(rows.Scan)
		if err != nil {
			return nil, fmt.Errorf("code: read %d entries of %s's queues: %w", len(ids), repoID, err)
		}
		out[e.EntryID] = e
	}
	return out, rows.Err()
}

// landingTipAt is the commit of the LAST landing on target of the record at
// position — the record's tip — and false where it landed none there.
func landingTipAt(ctx context.Context, tx *sql.Tx, repoID, target string, position int64) (string, bool, error) {
	var commit string
	switch err := tx.QueryRowContext(ctx, landingTipSQL, repoID, target, position).Scan(&commit); {
	case errors.Is(err, sql.ErrNoRows):
		return "", false, nil
	case err != nil:
		return "", false, fmt.Errorf("code: read %s's landing at %d: %w", target, position, err)
	}
	return commit, true, nil
}

// landingRow is what a broken record reads of the landing it names: its
// commit and the change it landed ("" for a push that landed none).
type landingRow struct {
	commit, changeID string
}

// landingAt is target's landing at (position, ordinal), and false where there
// is none.
func landingAt(ctx context.Context, tx *sql.Tx, repoID, target string, position int64, ordinal int) (landingRow,
	bool, error) {

	var l landingRow
	switch err := tx.QueryRowContext(ctx, landingSQL, repoID, target, position, ordinal).Scan(&l.commit,
		&l.changeID); {
	case errors.Is(err, sql.ErrNoRows):
		return landingRow{}, false, nil
	case err != nil:
		return landingRow{}, false, fmt.Errorf("code: read %s's landing at %d, place %d: %w", target, position,
			ordinal, err)
	}
	return l, true, nil
}

// pushes is the push each of l's entries lands as, in order — its push id
// derived from the landing's, its enqueuer its principal, `via queue`, the
// target moved from what the entry before it left onto its commit (a
// fast-forward, which leaves nothing behind), the change's source deleted
// where the decide stamped it, and what it introduced: the commit a merge or
// a squash built, none for a fast-forward, whose head its change's own pushes
// brought. entries are the entries' rows and rows their changes', in l's
// order.
func (l Land) pushes(entries map[string]queueEntry, rows []changeRow) []PushEntry {
	out := make([]PushEntry, len(l.Entries))
	from := l.BaseOID
	for i, e := range l.Entries {
		entry := entries[e.EntryID]
		p := PushEntry{PushID: LandPushID(l.LandID, e.EntryID), Principal: entry.enqueuer(), Via: ViaQueue,
			Node: l.Node, ReceivedAt: l.ReceivedAt,
			Commands: []Command{{Ref: l.TargetRef, Old: from, New: e.Commit, Result: ResultOK}}}
		if e.DeleteSource {
			p.Commands = append(p.Commands, Command{Ref: rows[i].source, Old: rows[i].head, New: ZeroOID,
				Result: ResultOK})
		}
		if entry.Method != MergeMethodFastForward {
			p.Introduced, p.IntroducedCount = []string{e.Commit}, 1
		}
		out[i] = p
		from = e.Commit
	}
	return out
}

// appliedQueueRecord answers one of the queue's own records whose record
// applied at at, FROM THE ROWS IT WROTE — rows no later record rewrites,
// wherever they can be: a landing's first push row, an eject's ended entries,
// a green row at or past its landing, a broken landing; and a candidate's
// train at the record's position, which a later record may abandon. Rows that
// do not carry it are a record that applied and wrote what this node does not
// hold — or, for a train, one abandoned since — logged and answered unknown.
func (c *Committer) appliedQueueRecord(ctx context.Context, p QueueProposal, at statelog.Position) (QueueResult,
	error) {

	var held bool
	if err := c.db.Read(ctx, func(tx *sql.Tx) error {
		var err error
		held, err = queueRecordHeld(ctx, tx, p, at)
		return err
	}); err != nil {
		failed := fmt.Errorf("code: the record of operation %s applied at %s and its rows could not be read: %w",
			p.OpID, at, err)
		c.log.WarnContext(ctx, "code_commit_failed", "kind", kindQueue, "repo", p.RepoID(), "op_id", p.OpID,
			"error", failed)
		return QueueResult{}, failed
	}
	if !held {
		op, _ := p.record()
		c.log.WarnContext(ctx, "code_queue_unanswerable", "repo", p.RepoID(), "op", string(op), "op_id", p.OpID,
			"position", at.String())
		return QueueResult{Outcome: statelog.OutcomeUnknown, Detail: fmt.Sprintf("the %s's record applied at %s "+
			"and the rows this node holds do not carry it", op, at)}, nil
	}
	return QueueResult{Outcome: statelog.OutcomeApplied, Position: at}, nil
}

// queueRecordHeld reports whether tx's rows carry p's record, applied at at.
func queueRecordHeld(ctx context.Context, tx *sql.Tx, p QueueProposal, at statelog.Position) (bool, error) {
	repoID := p.RepoID()
	switch {
	case p.Candidate != nil:
		var n int
		err := tx.QueryRowContext(ctx, trainAtSQL, repoID, p.Candidate.TargetRef, p.Candidate.Train,
			at.Packed()).Scan(&n)
		return n > 0, err
	case p.Eject != nil:
		ids := make([]string, len(p.Eject.Entries))
		for i, x := range p.Eject.Entries {
			ids[i] = x.EntryID
		}
		entries, err := entriesNamed(ctx, tx, repoID, ids)
		if err != nil {
			return false, err
		}
		for _, id := range ids {
			if e := entries[id]; e.State != QueueEjected || e.EndedPosition != at.Packed() {
				return false, nil
			}
		}
		return true, nil
	case p.Land != nil:
		first := LandPushID(p.Land.LandID, p.Land.Entries[0].EntryID)
		landed, err := pushesApplied(ctx, tx, []string{first})
		return landed[first], err
	case p.Green != nil:
		green, held, err := greenIn(ctx, tx, repoID, p.Green.TargetRef)
		return held && green.Position.Packed() >= p.Green.Through.Packed(), err
	case p.Broken != nil:
		var broken bool
		err := tx.QueryRowContext(ctx, landingBrokenSQL, repoID, p.Broken.TargetRef, p.Broken.Position.Packed(),
			p.Broken.Ordinal).Scan(&broken)
		if errors.Is(err, sql.ErrNoRows) {
			return false, nil
		}
		return broken, err
	case p.CloseSettled != nil:
		// SETTLED, every entry it names owing no close any more: the
		// record's apply is the one write that clears one, and an entry its
		// decide skipped owed none already.
		ids := p.CloseSettled.entries()
		owed, err := owedClosesNamed(ctx, tx, repoID, ids)
		if err != nil {
			return false, err
		}
		for _, id := range ids {
			if o, found := owed[id]; !found || o.op != "" {
				return false, nil
			}
		}
		return true, nil
	}
	return false, fmt.Errorf("code: queue operation %s is none of the queue's own records", p.OpID)
}

// announceEjected hands the announcer a code_queue_ejected event for each
// entry the eject at at put out ([Committer.announceEjections]).
func (c *Committer) announceEjected(e Eject, at statelog.Position) {
	c.announceEjectionsLater(e.RepoID, at, func(context.Context, *sql.Tx) ([]Ejection, error) {
		return e.Entries, nil
	})
}

// announcePushEjections hands the announcer a code_queue_ejected event for each live
// queue entry of the changes the push record at at moved the heads of
// (ejected, [batchMoves]) — ended head_moved by the record's own apply, which
// no queue record names, so without this a seat's day counted none of the
// commonest ejection while the queue read listed every one. Each entry is the
// one the record ended — its change's entry ended at the record's position,
// ejected head_moved, read from this node's rows: one statement over the
// changes, a seek of code_queue_change_idx each ([pushEjectedSQL]).
func (c *Committer) announcePushEjections(repoID string, ejected []string, at statelog.Position) {
	c.announceEjectionsLater(repoID, at, func(ctx context.Context, tx *sql.Tx) ([]Ejection, error) {
		list, err := jsonList(ejected)
		if err != nil {
			return nil, err
		}
		rows, err := tx.QueryContext(ctx, pushEjectedSQL, list, at.Packed(), string(QueueEjected),
			string(EjectHeadMoved))
		if err != nil {
			return nil, fmt.Errorf("code: read the entries the push at %s ejected: %w", at, err)
		}
		defer func() { _ = rows.Close() }()
		var out []Ejection
		for rows.Next() {
			x := Ejection{Reason: EjectHeadMoved}
			if err := rows.Scan(&x.EntryID); err != nil {
				return nil, fmt.Errorf("code: scan an entry the push at %s ejected: %w", at, err)
			}
			out = append(out, x)
		}
		return out, rows.Err()
	})
}

// announceEjectionsLater hands the announcer the ejections read names
// ([Committer.announceEjections]), so the repository's next write waits on
// none of their read or publish (announce.go).
func (c *Committer) announceEjectionsLater(repoID string, at statelog.Position,
	read func(context.Context, *sql.Tx) ([]Ejection, error)) {

	c.announceLater(announcement{kind: "code_queue_ejected", repoID: repoID, id: at.String(),
		publish: func(ctx context.Context, when time.Time) { c.announceEjections(ctx, when, repoID, at, read) }})
}

// announceEjections publishes a code_queue_ejected event for each entry read
// puts out of repoID's queues at at, on this node's event stream, stamped
// when, under ctx — the announcer's one [checkEventBudget], as a merge's event
// is ([Committer.announceMerged]): each names the entry's change by its
// repository and number, read from this node's rows, and a stream that does
// not take one is logged — the entry's row still says it was ejected. read is
// asked inside the one read, so the entries it names and the rows that
// describe them are one snapshot.
func (c *Committer) announceEjections(ctx context.Context, when time.Time, repoID string, at statelog.Position,
	read func(context.Context, *sql.Tx) ([]Ejection, error)) {

	var out []types.CodeQueueEjected
	if err := c.db.Read(ctx, func(tx *sql.Tx) error {
		ejections, err := read(ctx, tx)
		if err != nil || len(ejections) == 0 {
			return err
		}
		e := Eject{RepoID: repoID, Entries: ejections}
		var repoName string
		switch err = tx.QueryRowContext(ctx, repoNameSQL, e.RepoID).Scan(&repoName); {
		case errors.Is(err, sql.ErrNoRows):
			repoName = e.RepoID
		case err != nil:
			return err
		}
		ids := make([]string, len(e.Entries))
		for i, x := range e.Entries {
			ids[i] = x.EntryID
		}
		entries, err := entriesNamed(ctx, tx, e.RepoID, ids)
		if err != nil {
			return err
		}
		listed := make([]queueEntry, 0, len(entries))
		for _, id := range ids {
			if entry, ok := entries[id]; ok {
				listed = append(listed, entry)
			}
		}
		numbers, err := numbersOf(ctx, tx, listed)
		if err != nil {
			return err
		}
		for _, x := range e.Entries {
			entry, ok := entries[x.EntryID]
			if !ok {
				continue
			}
			ev := types.CodeQueueEjected{RepoID: e.RepoID, RepoName: repoName, EntryID: x.EntryID,
				ChangeID: entry.ChangeID, Number: numbers[entry.ChangeID], Target: entry.TargetRef,
				Reason: string(x.Reason), Checks: x.Checks, Paths: x.Paths, EnqueuedBy: entry.EnqueuedBy.Handle,
				EnqueuedByKind: string(entry.EnqueuedBy.Kind)}
			// THE CHANGE'S OWNER, whose day counts the ejection
			// (types.CodeQueueEjected.Owner): one seek of the changes' key
			// an entry, of a record that ejects at most a queue's depth.
			change, found, err := changeRowIn(ctx, tx, entry.ChangeID)
			if err != nil {
				return err
			}
			if found {
				ev.Owner, ev.OwnerKind = change.owner, string(change.ownerKind)
				ev.OwnerAgent = agentOf(c.agentIDs, change.owner, change.ownerKind)
			}
			out = append(out, ev)
		}
		return nil
	}); err != nil {
		c.log.WarnContext(ctx, "code_queue_ejected_unpublished", "repo", repoID, "position", at.String(),
			"error", err.Error(), "detail", "the activity feed and the event log do not hold this ejection; the "+
				"entries' rows on the code log still do")
		return
	}
	for _, x := range out {
		if err := announceOn(ctx, c.events, c.nodeID, when, x); err != nil {
			c.log.WarnContext(ctx, "code_queue_ejected_unpublished", "repo", repoID, "entry", x.EntryID,
				"error", err.Error(), "detail", "the activity feed and the event log do not hold this ejection; "+
					"the entry's row on the code log still does")
		}
	}
}

// announceBroken hands the announcer the code_target_broken event of the
// broken at at ([Committer.publishBroken]), so the repository's next write
// waits on none of its read or publish (announce.go).
func (c *Committer) announceBroken(b Broken, at statelog.Position) {
	c.announceLater(announcement{kind: "code_target_broken", repoID: b.RepoID, id: at.String(),
		publish: func(ctx context.Context, when time.Time) { c.publishBroken(ctx, when, b, at) }})
}

// publishBroken publishes ONE code_target_broken event for the landing the
// broken at at named (ADR-0098), on this node's event stream, stamped when,
// under ctx — the announcer's one [checkEventBudget], as an ejection's is: the
// landing's kind, its change by its repository and number and who landed it,
// read from this node's rows, and a stream that does not take it logged — the
// landing's row still says it is broken.
func (c *Committer) publishBroken(ctx context.Context, when time.Time, b Broken, at statelog.Position) {
	ev := types.CodeTargetBroken{RepoID: b.RepoID, RepoName: b.RepoID, Target: b.TargetRef, Commit: b.OID,
		Checks: b.Checks}
	if err := c.db.Read(ctx, func(tx *sql.Tx) error {
		switch err := tx.QueryRowContext(ctx, repoNameSQL, b.RepoID).Scan(&ev.RepoName); {
		case errors.Is(err, sql.ErrNoRows):
			ev.RepoName = b.RepoID
		case err != nil:
			return err
		}
		if err := tx.QueryRowContext(ctx, landingNamedSQL, b.RepoID, b.TargetRef, b.Position.Packed(),
			b.Ordinal).Scan(&ev.Kind, &ev.ChangeID, &ev.LandedBy); err != nil {
			return err
		}
		if ev.ChangeID == "" {
			return nil
		}
		numbers, err := changeNumbersIn(ctx, tx, b.RepoID, []string{ev.ChangeID})
		ev.Number = numbers[ev.ChangeID]
		return err
	}); err != nil {
		c.log.WarnContext(ctx, "code_target_broken_unpublished", "repo", b.RepoID, "position", at.String(),
			"error", err.Error(), "detail", "the activity feed and the event log do not hold this broken "+
				"landing; its row on the code log still does")
		return
	}
	if err := announceOn(ctx, c.events, c.nodeID, when, ev); err != nil {
		c.log.WarnContext(ctx, "code_target_broken_unpublished", "repo", b.RepoID, "position", at.String(),
			"error", err.Error(), "detail", "the activity feed and the event log do not hold this broken "+
				"landing; its row on the code log still does")
	}
}

// ejectedDetail is an ejection's detail as code_queue.ejected keeps it: its
// checks and its paths, JSON, "" for an ejection naming neither.
func ejectedDetail(x Ejection) (string, error) {
	if len(x.Checks) == 0 && len(x.Paths) == 0 {
		return "", nil
	}
	encoded, err := json.Marshal(struct {
		Checks []string `json:"checks,omitempty"`
		Paths  []string `json:"paths,omitempty"`
	}{x.Checks, x.Paths})
	return string(encoded), err
}

// The queue's own records' reads, each registered in plan_test.go: a list of
// entries by their key, driven from the list; a train's candidates at a
// record's position, over code_candidates_train_idx; a landing record's tip,
// its last landing by the landings' key; a landing by its key, its commit and
// whether it is broken, and what a broken one announces; and the entries a
// push record ended by
// moving their changes' heads, driven from the list of the changes, one seek
// of code_queue_change_idx a change.
const (
	entriesNamedSQL = `SELECT ` + qualifiedEntryColumns + ` FROM json_each(?1) AS j
		CROSS JOIN code_queue AS q INDEXED BY ` + queueKey + `
		WHERE q.repo_id = ?2 AND q.entry_id = CAST(j.value AS TEXT)`
	pushEjectedSQL = `SELECT q.entry_id FROM json_each(?) AS j
		CROSS JOIN code_queue AS q INDEXED BY code_queue_change_idx
		WHERE q.change_id = CAST(j.value AS TEXT) AND q.ended_position = ? AND q.state = ? AND q.end_reason = ?`
	trainAtSQL = `SELECT COUNT(*) FROM code_candidates INDEXED BY code_candidates_train_idx
		WHERE repo_id = ? AND target_ref = ? AND train = ? AND position = ?`
	landingTipSQL = `SELECT commit_oid FROM code_landings WHERE repo_id = ? AND target_ref = ? AND position = ?
		ORDER BY ordinal DESC LIMIT 1`
	landingSQL = `SELECT commit_oid, change_id FROM code_landings
		WHERE repo_id = ? AND target_ref = ? AND position = ? AND ordinal = ?`
	landingBrokenSQL = `SELECT broken FROM code_landings
		WHERE repo_id = ? AND target_ref = ? AND position = ? AND ordinal = ?`
	landingNamedSQL = `SELECT kind, change_id, merged_by FROM code_landings
		WHERE repo_id = ? AND target_ref = ? AND position = ? AND ordinal = ?`
)

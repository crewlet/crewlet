package code

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/queue/topics"
	"github.com/crewlet/crewlet/internal/statelog"
)

// THE QUEUE'S OWN RECORDS, DECIDED (queuerecords.go): each from ONE snapshot of
// the repository's rows on its committer, as an enqueue is — what its home
// built is proposed with what it was built from, and the decide holds the rows
// to it: the target AT THE TIP the train or the chain was built onto, read off
// the snapshot's refs (target_moved, which its home answers by building
// again); every entry LIVE in the target's queue (not_queued); and, for a
// landing, every entry's gate MET AGAIN with its enqueuer as the merger — the
// verdict race between an enqueue and its landing is the deliberate one, and
// the landing is the record that moves the branch.

// The rows each of the queue's own records makes its apply touch at most,
// beside a landing's pushes, which its decide counts as their apply writes
// them ([Committer.ProposeQueue] charges each; [TestAPostLandRecordFitsItsCaps]
// holds them under MaxRecordRows).
const (
	// candidateDecidedRows: the target's earlier train abandoned
	// ([trainRows]), every entry of the new one testing, its candidates'
	// rows, its pack's row, the repository's and the queue's horizon.
	candidateDecidedRows = trainRows + MaxCandidateEntries + MaxQueueDepth + 1 + 1 + queueHorizonRows
	// brokenDecidedRows: the landing's row and the repository's, and the
	// notices its routing makes.
	brokenDecidedRows = 2 + routingNoticeRows
	// greenDecidedRows: the target's green row, the repository's, and the
	// landings it marks green ([greenLandingRows]).
	greenDecidedRows = 2 + greenLandingRows
	// greenLandingRows is the most landings one green record marks green:
	// half of MaxRecordRows, 2 000 — JUDGMENT: thirty-one post_land
	// landings of sixty-four changes each above a target's green row is a
	// target whose tip checks run that far behind its lane, and its next
	// green names an earlier landing first, refused record_full until it
	// does. A green past it would be a record past MaxRecordRows.
	greenLandingRows = MaxRecordRows / 2
)

// ejectDecidedRows is the most rows an eject of n entries makes its apply
// touch: each entry ended, its target's train abandoned ([trainRows]), the
// repository's and the queue's horizon.
func ejectDecidedRows(n int) int { return n + trainRows + 1 + queueHorizonRows }

// landDecidedRows is the most rows a land of n entries makes its apply touch
// of its OWN, beside the pushes it lands: each entry's change merged, its
// entry landed and its landing row; the target's green row; its train's
// landed candidates; and the queue's horizon.
func landDecidedRows(n int) int { return 3*n + 1 + MaxQueueDepth + queueHorizonRows }

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
	if refusal, detail := queuePack(c.Pack, builds, "train", repo, limits, now); refusal != "" {
		return d.refuse(refusal, detail)
	}
	d.payload = c
	return d, nil
}

// decideEject is an eject's rules over the rows tx holds: every entry live in
// its queue, and all of them one target's, whose train it abandons
// (not_queued).
func decideEject(ctx context.Context, tx *sql.Tx, e Eject, repo pushRepo, d queueDecision) (queueDecision,
	error) {

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
	routings := make([]ChangeRouting, 0, len(e.Entries))
	for _, x := range e.Entries {
		entry := entries[x.EntryID]
		row, found, err := changeRowIn(ctx, tx, entry.ChangeID)
		if err != nil {
			return queueDecision{}, err
		}
		if !found {
			continue
		}
		routed, err := routingFor(ctx, tx, concerning{repoName: repo.name, change: row, authors: true,
			dequeued: true, conflicted: x.Reason == EjectConflict, enqueuer: entry.EnqueuedBy.reviewer()})
		if err != nil {
			return queueDecision{}, err
		}
		routings = append(routings, *routed)
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
//  2. the target a queue target of the landing's policy (no_queue, stale);
//  3. every entry live in the target's queue, of its change, at the head it
//     was enqueued at, its commit what its method makes (not_queued, stale);
//  4. a pre_land landing a PREFIX of the target's train — its candidates
//     from the first, in order, up to the one whose tip is the landing's —
//     whose every required check passed on its tip, by its latest attempt
//     checked for the target (stale, not_green); a post_land chain its pack
//     carrying what it built, held to a push's rules (stale, upload_expired,
//     repo_size, compaction_behind);
//  5. EVERY ENTRY'S GATE MET AGAIN, from this snapshot, with its ENQUEUER as
//     the merger — every precondition but `mergeable`, which its candidate or
//     its chain proved on the branch, and the entry's own place in the queue
//     — its change open, not a draft, into the target at the entry's head:
//     each entry it would not land named in [QueueResult.Refused]
//     (gate_unmet, head_moved, change_not_open), for its home to eject;
//  6. its rows within a record — its own, its pushes', and the live changes
//     those pushes move, counted as their apply writes them (record_full).
//
// It STAMPS what its snapshot holds, so the apply matches no pattern: a
// pre_land landing green, and each entry's source deleted where the entry
// asked it and a push deleting it as its enqueuer would be taken.
func decideLand(ctx context.Context, tx *sql.Tx, l Land, repo pushRepo, seats SeatRules, limits Limits,
	now time.Time, d queueDecision) (queueDecision, error) {

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
	switch {
	case !gate.QueueTarget():
		return d.refuse(MergeRefusalNoQueue, fmt.Sprintf("%s has no merge queue — no rule matching it requires a "+
			"check — so nothing lands on it through one", l.TargetRef))
	case gate.Landing != l.Policy:
		return d.refuse(MergeRefusalStale, fmt.Sprintf("%s lands %s, and this landing was built %s: build it "+
			"again under the branch's policy", l.TargetRef, gate.Landing, l.Policy))
	}

	named := make([]string, len(l.Entries))
	for i, e := range l.Entries {
		named[i] = e.EntryID
	}
	entries, err := entriesNamed(ctx, tx, l.RepoID, named)
	if err != nil {
		return queueDecision{}, err
	}
	builds := false
	for _, e := range l.Entries {
		entry, found := entries[e.EntryID]
		if why := whyNotLive(entry, found, l.TargetRef); why != "" {
			d.refused = append(d.refused, RefusedEntry{EntryID: e.EntryID, Refusal: MergeRefusalNotQueued,
				Detail: fmt.Sprintf("entry %s %s", e.EntryID, why)})
			continue
		}
		switch {
		case entry.ChangeID != e.ChangeID:
			return d.refuse(MergeRefusalStale, fmt.Sprintf("entry %s queues change %s, and this landing lands "+
				"it as change %s", e.EntryID, entry.ChangeID, e.ChangeID))
		case l.Policy == LandingPostLand && entry.State != QueueWaiting:
			return d.refuse(MergeRefusalStale, fmt.Sprintf("entry %s is %s, and a post_land lane lands "+
				"waiting entries", e.EntryID, entry.State))
		}
		if why := builtAs(entry, e.Commit); why != "" {
			return d.refuse(MergeRefusalStale, why)
		}
		builds = builds || entry.Method != MergeMethodFastForward
	}
	if len(d.refused) > 0 {
		return d.refuse(MergeRefusalNotQueued, fmt.Sprintf("%d of the landing's %d entries are not live in %s's "+
			"queue: %s", len(d.refused), len(l.Entries), l.TargetRef, d.refused[0].Detail))
	}

	if l.Policy == LandingPreLand {
		if detail, ok, err := prefixOfTrain(ctx, tx, l); err != nil || !ok {
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
				"train's tip %s: %s — a pre_land queue lands only what it tested green", l.TargetRef,
				short(l.TipOID), strings.Join(waiting, ", ")))
		}
	} else if refusal, detail := queuePack(l.Pack, builds, "chain", repo, limits, now); refusal != "" {
		return d.refuse(refusal, detail)
	}

	// EVERY ENTRY'S GATE, AGAIN, its enqueuer the merger.
	ref := repo.grantRef(l.RepoID)
	subjects := make([]readinessSubject, 0, len(l.Entries))
	rows := make([]changeRow, len(l.Entries))
	for i, e := range l.Entries {
		row, found, err := changeRowIn(ctx, tx, e.ChangeID)
		if err != nil {
			return queueDecision{}, err
		}
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
	stamped.Green = l.Policy == LandingPreLand

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
		routed, err := routingFor(ctx, tx, concerning{repoName: repo.name, change: rows[i], actor: merger,
			actorKind: mergerKind, authors: true, merged: true})
		if err != nil {
			return queueDecision{}, err
		}
		routings = append(routings, *routed)
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
// its latest attempt checked for the target (not_green); and the landings it
// marks green within its record (record_full).
func decideGreen(ctx context.Context, tx *sql.Tx, g Green, repo pushRepo, d queueDecision) (queueDecision,
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
	from := int64(0)
	if held {
		from = green.Position.Packed()
	}
	var marks int
	if err := tx.QueryRowContext(ctx, landingsGreenedSQL, g.RepoID, g.TargetRef, from, g.Through.Packed(),
		greenLandingRows+1).Scan(&marks); err != nil {
		return queueDecision{}, fmt.Errorf("code: count %s's landings up to %s: %w", g.TargetRef, g.Through, err)
	}
	if marks > greenLandingRows {
		return d.refuse(MergeRefusalRecordFull, fmt.Sprintf("more than %d of %s's landings lie between its green "+
			"row and %s: a green names an earlier landing first", greenLandingRows, g.TargetRef, g.Through))
	}
	d.payload = g
	return d, nil
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
	// WHOM IT CONCERNS: the authors of the change the landing merged, its
	// enqueuer and the effective lead of the repository's owner — the home's
	// own record, so nobody's actor; nobody for a landing no change made.
	if landing.changeID != "" {
		row, found, err := changeRowIn(ctx, tx, landing.changeID)
		if err != nil {
			return queueDecision{}, err
		}
		if found {
			c := concerning{repoName: repo.name, change: row, authors: true, broken: true,
				ownerLead: ownersLead(repo.grantRef(b.RepoID), seats.Roster)}
			if landing.enqueuedBy != "" {
				c.enqueuer = Reviewer{Handle: landing.enqueuedBy, Kind: PartyAgent}
				if seat, ok := seats.Roster.Seat(landing.enqueuedBy); ok && seat.Human {
					c.enqueuer.Kind = PartyPerson
				}
			}
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

// queuePack is a queue record's pack held to a push's rules — or why it may
// not be: carried exactly where the record built a commit (stale), uploaded
// recently enough for a record to name it (upload_expired), within the
// repository's size (repo_size) and into a repository whose compaction keeps
// pace with its packs (compaction_behind, m3-k5's rule 6c). what names the
// record's build in a sentence.
func queuePack(pack *PackRef, builds bool, what string, repo pushRepo, limits Limits, now time.Time) (MergeRefusal,
	string) {

	switch {
	case pack == nil && builds:
		return MergeRefusalStale, fmt.Sprintf("the %s builds commits of its own and carries no pack of them", what)
	case pack != nil && !builds:
		return MergeRefusalStale, fmt.Sprintf("the %s fast-forwards alone, building nothing, and carries a pack",
			what)
	case pack == nil:
		return "", ""
	}
	if detail, expired := uploadExpired("the "+what+"'s pack", pack.Object, now, "its home builds it again"); expired {
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
// commit, the change it landed ("" for a push that landed none) and who
// enqueued that change ("" for a change no queue landed).
type landingRow struct {
	commit, changeID, enqueuedBy string
}

// landingAt is target's landing at (position, ordinal), and false where there
// is none.
func landingAt(ctx context.Context, tx *sql.Tx, repoID, target string, position int64, ordinal int) (landingRow,
	bool, error) {

	var l landingRow
	switch err := tx.QueryRowContext(ctx, landingSQL, repoID, target, position, ordinal).Scan(&l.commit,
		&l.changeID, &l.enqueuedBy); {
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
// where the decide stamped it, the chain's pack on the first, and what it
// introduced: the commit a merge or a squash built, none for a fast-forward,
// whose head its change's own pushes brought. entries are the entries' rows
// and rows their changes', in l's order.
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
		if i == 0 {
			p.Pack = l.Pack
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
		landed, err := keySet(ctx, tx, pushesAppliedSQL, []string{first})
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
	}
	return false, fmt.Errorf("code: queue operation %s is none of the queue's own records", p.OpID)
}

// announceEjected publishes a code_queue_ejected event for each entry the
// eject at at put out, on this node's event stream, bounded by
// [checkEventBudget] under the committer's own lifetime, as a merge's event is
// ([Committer.announce]): each names the entry's change by its repository and
// number, read from this node's rows, and a stream that does not take one is
// logged — the entry's row still says it was ejected.
func (c *Committer) announceEjected(e Eject, at statelog.Position) {
	if c.events == nil {
		return
	}
	ctx, cancel := context.WithTimeout(c.life, checkEventBudget)
	defer cancel()
	var out []types.CodeQueueEjected
	if err := c.db.Read(ctx, func(tx *sql.Tx) error {
		var repoName string
		switch err := tx.QueryRowContext(ctx, repoNameSQL, e.RepoID).Scan(&repoName); {
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
		c.log.WarnContext(ctx, "code_queue_ejected_unpublished", "repo", e.RepoID, "position", at.String(),
			"error", err.Error(), "detail", "the activity feed and the event log do not hold this ejection; the "+
				"entries' rows on the code log still do")
		return
	}
	for _, x := range out {
		ev := events.New(x, events.TraceContext{})
		ev.Source = c.nodeID
		if err := c.events.Publish(ctx, topics.Event(ev.Type), ev); err != nil {
			c.log.WarnContext(ctx, "code_queue_ejected_unpublished", "repo", e.RepoID, "entry", x.EntryID,
				"error", err.Error(), "detail", "the activity feed and the event log do not hold this ejection; "+
					"the entry's row on the code log still does")
		}
	}
}

// announceBroken publishes ONE code_target_broken event for the landing the
// broken at at named (ADR-0068), on this node's event stream, bounded by
// [checkEventBudget] under the committer's own lifetime, as an ejection's is:
// the landing's kind, its change by its repository and number and who landed
// it, read from this node's rows, and a stream that does not take it logged —
// the landing's row still says it is broken.
func (c *Committer) announceBroken(b Broken, at statelog.Position) {
	if c.events == nil {
		return
	}
	ctx, cancel := context.WithTimeout(c.life, checkEventBudget)
	defer cancel()
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
	e := events.New(ev, events.TraceContext{})
	e.Source = c.nodeID
	if err := c.events.Publish(ctx, topics.Event(e.Type), e); err != nil {
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
// whether it is broken, and what a broken one announces; and the landings a
// green marks, counted over the key's
// range above the target's green row.
const (
	entriesNamedSQL = `SELECT ` + qualifiedEntryColumns + ` FROM json_each(?1) AS j
		CROSS JOIN code_queue AS q WHERE q.repo_id = ?2 AND q.entry_id = CAST(j.value AS TEXT)`
	trainAtSQL = `SELECT COUNT(*) FROM code_candidates INDEXED BY code_candidates_train_idx
		WHERE repo_id = ? AND target_ref = ? AND train = ? AND position = ?`
	landingTipSQL = `SELECT commit_oid FROM code_landings WHERE repo_id = ? AND target_ref = ? AND position = ?
		ORDER BY ordinal DESC LIMIT 1`
	landingSQL = `SELECT commit_oid, change_id, enqueued_by FROM code_landings
		WHERE repo_id = ? AND target_ref = ? AND position = ? AND ordinal = ?`
	landingBrokenSQL = `SELECT broken FROM code_landings
		WHERE repo_id = ? AND target_ref = ? AND position = ? AND ordinal = ?`
	landingNamedSQL = `SELECT kind, change_id, merged_by FROM code_landings
		WHERE repo_id = ? AND target_ref = ? AND position = ? AND ordinal = ?`
	landingsGreenedSQL = `SELECT COUNT(*) FROM (SELECT 1 FROM code_landings
		WHERE repo_id = ? AND target_ref = ? AND position > ? AND position <= ? LIMIT ?)`
)

package gitlet

import (
	"cmp"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"math"
	"slices"
	"strings"
)

// A change's merge readiness: PRECONDITIONS the pure [ReadinessOf] judges over
// one snapshot's rows and one epoch's chart — never a workflow somebody must
// remember to run, never a tracker decision: it counts the change's own review
// objects, never ADR-0023 decisions. Each precondition, how an approval carries
// and whom each waits on are docs/concepts/gitlet.md, "Merge readiness"; the
// gate's kinds join them in [JudgeGate] (gatejudge.go), approval only if asked.
//
// Every read judges it again, so no reader trusts an answer older than its
// snapshot. A mergeability record's decide stamps it ([ReadinessRows.With]) for
// the wake and the readiness=ready and needs=person seeks; a home REFRESHes a
// stale stamp (changesdue.go), an ended change stores none (clearReadiness),
// and a stamp is as of its record, so no reader of a change's readiness reads it.

// PreconditionKind is one thing a live change must be before it merges
// ([ReadinessOf], [JudgeGate]): a closed set, ordered by [PreconditionKinds].
type PreconditionKind string

const (
	// PreconditionDraft is a change open for review, not a draft.
	PreconditionDraft PreconditionKind = "draft"
	// PreconditionFacts is the head revision's facts against the change's
	// target, recorded by its home.
	PreconditionFacts PreconditionKind = "facts"
	// PreconditionApproval is one counted approval of the head revision.
	PreconditionApproval PreconditionKind = "approval"
	// PreconditionHumanApproval is a PERSON'S counted approval, of a change
	// touching a path its target's gate holds to a person
	// ([MergeGate.HumanPaths]).
	PreconditionHumanApproval PreconditionKind = "human_approval"
	// PreconditionOwnersApproval is ONE counted approval from somebody
	// eligible for every path owner the change touches, where its target's
	// gate makes path owners gating ([MergeGate.OwnersApprove]).
	PreconditionOwnersApproval PreconditionKind = "owners_approval"
	// PreconditionBlocker is no standing blocker.
	PreconditionBlocker PreconditionKind = "blocker"
	// PreconditionCheck is every check its target's gate requires passed on
	// the head commit ([MergeGate.Checks]).
	PreconditionCheck PreconditionKind = "check"
	// PreconditionMergeable is the head revision merging cleanly into its
	// target, as its home merged it.
	PreconditionMergeable PreconditionKind = "mergeable"
	// PreconditionMergeAuthority is the authority of whoever would merge it
	// — a read's asker, a decide's principal — to merge into its target
	// ([MergeGate.MergeBy], code.merge_by_floor).
	PreconditionMergeAuthority PreconditionKind = "merge_authority"
	// PreconditionQueue is a change its target's merge queue holds: computing
	// while the queue lands it, its place named (queue.go).
	PreconditionQueue PreconditionKind = "queue"
)

// PreconditionKinds is the closed set in THE ORDER A CHANGE MOVES THROUGH —
// opened, measured, approved as its gate asks, cleared of blockers, checked,
// merged, merged by somebody — so a readiness's first unmet precondition is the
// earliest thing in its way, never whether this asker may merge a change still
// waiting on its review.
func PreconditionKinds() []PreconditionKind {
	return []PreconditionKind{PreconditionDraft, PreconditionFacts, PreconditionApproval,
		PreconditionHumanApproval, PreconditionOwnersApproval, PreconditionBlocker, PreconditionCheck,
		PreconditionMergeable, PreconditionMergeAuthority, PreconditionQueue}
}

// Valid reports a kind this build knows.
func (k PreconditionKind) Valid() bool { return slices.Contains(PreconditionKinds(), k) }

// PreconditionState is where a precondition stands.
type PreconditionState string

const (
	// PreconditionMet is a precondition the change meets.
	PreconditionMet PreconditionState = "met"
	// PreconditionUnmet is one it does not, which waits on somebody.
	PreconditionUnmet PreconditionState = "unmet"
	// PreconditionComputing is one the engine has not judged yet (facts or a
	// merge its home has not recorded), which waits on nobody.
	PreconditionComputing PreconditionState = "computing"
)

// PreconditionStates is the closed set, in this order.
func PreconditionStates() []PreconditionState {
	return []PreconditionState{PreconditionMet, PreconditionUnmet, PreconditionComputing}
}

// Valid reports a state this build knows.
func (s PreconditionState) Valid() bool { return slices.Contains(PreconditionStates(), s) }

// Party is one principal a readiness names: a seat by its handle, or a person
// by the human seat they hold, as a change's rows name one ([PartyKind]).
type Party struct {
	Handle string    `json:"handle"`
	Kind   PartyKind `json:"kind"`
}

// named is p in a sentence.
func (p Party) named() string {
	if p.Kind == PartyPerson {
		return fmt.Sprintf("person %q", p.Handle)
	}
	return fmt.Sprintf("seat %q", p.Handle)
}

// ClearedBy is who may clear one standing blocker (S40): its RAISER — by
// dismissing it, or by a review of a later revision that does not carry it —
// the seats the chart lets maintain the repository, and any person, each by
// dismissing it.
type ClearedBy struct {
	Raiser Party `json:"raiser"`
	// Maintainers is the seats that maintain the repository on the judging
	// chart: its owning unit's effective lead, or its owning seat, where its
	// tier lets it maintain — none where neither does.
	Maintainers []string `json:"maintainers,omitempty"`
	// Persons is whether any person may clear it: always, since a person
	// maintains every repository (ADR-0085).
	Persons bool `json:"persons,omitempty"`
}

// Blocker is one standing blocker a readiness names: the finding, where it
// stands — open, or answered by an author — and who may clear it.
type Blocker struct {
	FindingID string       `json:"finding_id"`
	State     FindingState `json:"state"`
	ClearedBy ClearedBy    `json:"cleared_by"`
}

// MaxReadinessBlockers is the most standing blockers one readiness names,
// oldest raised first; it counts the rest, which the `findings` part pages.
// Sixteen is a list somebody acts down, and at about 200 bytes a blocker keeps
// a listed change's readiness near 3 KB, so a page of changes is cut by its
// byte budget, never by one change's blockers.
const MaxReadinessBlockers = 16

// Precondition is one thing a change must be before it merges, judged: its
// kind, where it stands, whom it waits on while it is unmet, and one sentence
// saying why.
type Precondition struct {
	Kind  PreconditionKind  `json:"kind"`
	State PreconditionState `json:"state"`
	// WaitsOn is whom an unmet precondition waits on — none while it is met
	// or computing, which wait on the engine.
	WaitsOn []Party `json:"waits_on,omitempty"`
	// Blockers is a blocker precondition's standing blockers, the oldest
	// raised first, at most [MaxReadinessBlockers], each naming who may
	// clear it; BlockersOmitted is how many more stand.
	Blockers        []Blocker `json:"blockers,omitempty"`
	BlockersOmitted int64     `json:"blockers_omitted,omitempty"`
	// Queue is a queue precondition's LIVE entry in its target's merge queue,
	// as the read that judged the gate found it — nil on every other kind. A
	// dequeue names the entry from it, never walking the queue for its id.
	Queue *QueuedFacts `json:"queue,omitempty"`
	// Detail is one sentence of where it stands and, unmet, what clears it.
	Detail string `json:"detail"`
}

// Readiness is whether a live change may merge: every precondition judged
// ([Preconditions] one a kind, in [PreconditionKinds]' order), how many are
// met of how many, and the FIRST sentence — of the first unmet precondition,
// else the first computing one, else that the change is ready.
type Readiness struct {
	Met           int            `json:"met"`
	Total         int            `json:"total"`
	First         string         `json:"first"`
	Preconditions []Precondition `json:"preconditions"`
}

// Ready reports a readiness every precondition of which is met.
func (r Readiness) Ready() bool { return r.Total > 0 && r.Met == r.Total }

// WaitsPerson reports a readiness whose FIRST UNMET precondition waits on
// persons alone. One that waits on a seat too, on nobody named, or only on the
// engine does not.
func (r Readiness) WaitsPerson() bool {
	for _, p := range r.Preconditions {
		if p.State != PreconditionUnmet {
			continue
		}
		if len(p.WaitsOn) == 0 {
			return false
		}
		for _, w := range p.WaitsOn {
			if w.Kind != PartyPerson {
				return false
			}
		}
		return true
	}
	return false
}

// ReadinessRows is everything a change's readiness judges, as ONE snapshot
// holds it ([readinessRowsIn]): judged by [ReadinessOf] with no row read again.
type ReadinessRows struct {
	// Repo is the change's repository as a grant reads it: what an
	// approver's eligibility and the repository's maintainers are judged by.
	Repo RepoRef
	// Change is the change's row.
	Change ChangeRow
	// Authors is everyone who wrote it — its owner and every pusher of its
	// head (code_change_authors) — whose approval never counts.
	Authors []Party
	// Facts is the recorded facts of its head revision, and of each
	// revision an approval stands on, where its home recorded some.
	Facts []RevisionPatch
	// Approvals is each reviewer whose standing verdict approves, with the
	// revision it approved.
	Approvals []Approval
	// Asked is each reviewer whose ask stands (code_reviewers).
	Asked []Party
	// Blockers is its standing blockers, the oldest raised first, at most
	// [MaxReadinessBlockers]; Standing is how many stand in all
	// (code_conversations.open_blockers).
	Blockers []StandingBlocker
	Standing int64
	// Merge is its mergeability as its row holds it, nil for none.
	Merge *RecordedMerge
}

// RevisionPatch is what one revision's recorded facts say of its code and its
// diff: the branch they were read against, the trees of its base and its
// head, the diff's patch id ("" for a revision that changes nothing), and
// whether none was taken.
type RevisionPatch struct {
	N                  int64
	TargetRef          string
	BaseTree, HeadTree string
	PatchID            string
	PatchIDOmitted     bool
}

// identified reports a patch id that names the diff it was taken of: one
// taken, of a revision that changes something. An empty or an omitted id
// equals nothing, so no approval carries across it.
func (f RevisionPatch) identified() bool { return f.PatchID != "" && !f.PatchIDOmitted }

// sameCode reports two revisions holding the same code: both trees recorded,
// and equal — the base's, so the code a merge starts from is the same, and
// the head's, so the code it brings is ([ChangeClassNoCodeChange]). A tree
// not recorded equals nothing.
func (f RevisionPatch) sameCode(o RevisionPatch) bool {
	return f.BaseTree != "" && f.HeadTree != "" && f.BaseTree == o.BaseTree && f.HeadTree == o.HeadTree
}

// carries reports facts an approval of their revision can be carried from by
// either kind: trees recorded, or an identified patch id.
func (f RevisionPatch) carries() bool {
	return (f.BaseTree != "" && f.HeadTree != "") || f.identified()
}

// Approval is a reviewer's standing verdict approving a revision.
type Approval struct {
	Reviewer Party
	Revision int64
}

// StandingBlocker is one blocker standing on a change: the finding, who
// raised it, and where it stands.
type StandingBlocker struct {
	FindingID string
	Raiser    Party
	State     FindingState
}

// RecordedMerge is a change's mergeability as its readiness reads it: the
// revision merged, into which branch at which tip, what the merge did, and
// how many paths it left conflicted — named and counted alike.
type RecordedMerge struct {
	Revision  int64
	TargetRef string
	TargetOID string
	State     MergeState
	Conflicts int64
}

// With is r with the record's merge in place of the row's, so the readiness
// its decide stamps is the one its own apply makes true. A REFRESH carries no
// merge and leaves r as it is.
func (r ReadinessRows) With(m Mergeability) ReadinessRows {
	if m.Refresh {
		return r
	}
	r.Merge = &RecordedMerge{Revision: m.Revision, TargetRef: m.TargetRef, TargetOID: m.TargetOID, State: m.State,
		Conflicts: int64(len(m.Conflicts)) + m.ConflictsOmitted}
	return r
}

// ReadinessOf is a change's readiness (this file's head) over one snapshot's
// rows and one epoch's chart r: PURE, so a decide and a read holding the same
// rows and chart agree. Eligibility and maintainers are the chart's
// ([GrantOf]); a nil chart lets no seat review or maintain, so no caller whose
// judgment counts passes one.
func ReadinessOf(rows ReadinessRows, r Roster) Readiness {
	j := newReadinessJudge(rows, r)
	return j.readiness([]Precondition{j.draft(), j.facts(), j.approval(), j.blocker(), j.mergeable()})
}

// readinessJudge is one judgment of a change's readiness: its rows, the
// chart, the change as a sentence names it (web!42) and its owner.
type readinessJudge struct {
	rows   ReadinessRows
	roster Roster
	ref    string
	owner  Party
}

// newReadinessJudge is the judgment of rows' change by chart r.
func newReadinessJudge(rows ReadinessRows, r Roster) readinessJudge {
	return readinessJudge{rows: rows, roster: r, ref: ChangeRef(rows.Repo.Name, rows.Change.Number),
		owner: Party{Handle: rows.Change.Owner, Kind: rows.Change.OwnerKind}}
}

// readiness is pre, judged in [PreconditionKinds]' order, as a [Readiness].
func (j readinessJudge) readiness(pre []Precondition) Readiness {
	out := Readiness{Total: len(pre), Preconditions: pre}
	for _, p := range pre {
		if p.State == PreconditionMet {
			out.Met++
		}
	}
	out.First = fmt.Sprintf("%s is ready to merge: all %d of its preconditions are met", j.ref, out.Total)
	for _, state := range []PreconditionState{PreconditionUnmet, PreconditionComputing} {
		if i := slices.IndexFunc(pre, func(p Precondition) bool { return p.State == state }); i >= 0 {
			out.First = pre[i].Detail
			break
		}
	}
	return out
}

// draft is the draft precondition.
func (j readinessJudge) draft() Precondition {
	p := Precondition{Kind: PreconditionDraft}
	switch j.rows.Change.State {
	case ChangeStateOpen:
		p.State, p.Detail = PreconditionMet, j.ref+" is open for review, not a draft"
	case ChangeStateDraft:
		p.State, p.WaitsOn = PreconditionUnmet, []Party{j.owner}
		p.Detail = fmt.Sprintf("%s is a draft: its owner %s, a principal that maintains the repository or a "+
			"person marks it ready for review", j.ref, j.owner.named())
	default:
		p.State = PreconditionUnmet
		p.Detail = fmt.Sprintf("%s is %s, and only a live change merges", j.ref, j.rows.Change.State)
	}
	return p
}

// facts is the facts precondition.
func (j readinessJudge) facts() Precondition {
	n, target := j.rows.Change.Revision, j.rows.Change.TargetRef
	if _, ok := j.headFacts(); ok {
		return Precondition{Kind: PreconditionFacts, State: PreconditionMet,
			Detail: fmt.Sprintf("revision %d's facts against %s are recorded", n, target)}
	}
	return Precondition{Kind: PreconditionFacts, State: PreconditionComputing,
		Detail: fmt.Sprintf("revision %d's facts against %s are being read by the repository's home Gitlet node", n,
			target)}
}

// approvalTally is which approvals count on the head revision, by the one rule
// M4's approval and the gate's person's and owners' approvals share
// ([JudgeGate]): an ELIGIBLE reviewer's standing verdict, NEVER AN AUTHOR's,
// approving the head (onHead), or an earlier revision whose code (unchanged,
// latest unchangedFrom) or diff (carried, latest carriedFrom) the head holds
// unchanged. pending is an earlier approval waiting on the head's facts against
// its current target, which say whether it carries.
type approvalTally struct {
	onHead, unchanged, carried []Party
	unchangedFrom, carriedFrom int64
	pending                    bool
}

// counted is every reviewer whose approval counts on the head revision, each
// once, in the order the rows hold their verdicts.
func (t approvalTally) counted() []Party {
	var out []Party
	for _, list := range [][]Party{t.onHead, t.unchanged, t.carried} {
		for _, p := range list {
			if !slices.Contains(out, p) {
				out = append(out, p)
			}
		}
	}
	return out
}

// tally is the change's [approvalTally].
func (j readinessJudge) tally() approvalTally {
	n := j.rows.Change.Revision
	head, current := j.headFacts()
	var t approvalTally
	for _, a := range j.rows.Approvals {
		if j.author(a.Reviewer) || !j.eligible(a.Reviewer) {
			continue
		}
		if a.Revision == n {
			t.onHead = append(t.onHead, a.Reviewer)
			continue
		}
		from, recorded := j.factsOf(a.Revision)
		switch {
		case a.Revision > n, !recorded, !from.carries():
			// NEVER CARRIES: a revision past the head is none the change has,
			// and one with neither recorded trees nor an identified patch id
			// can never be shown the head's — its home measures the head alone.
		case !current:
			t.pending = true
		case head.sameCode(from):
			// NO CODE CHANGE: the head holds the code that was approved,
			// byte for byte, whatever its patch id says — or omits.
			t.unchanged = append(t.unchanged, a.Reviewer)
			t.unchangedFrom = max(t.unchangedFrom, a.Revision)
		case head.identified() && from.identified() && head.PatchID == from.PatchID:
			t.carried = append(t.carried, a.Reviewer)
			t.carriedFrom = max(t.carriedFrom, a.Revision)
		}
	}
	return t
}

// approval is the approval precondition.
func (j readinessJudge) approval() Precondition { return j.approvalBy(j.tally()) }

// approvalBy is the approval precondition, t the head revision's approvals.
func (j readinessJudge) approvalBy(t approvalTally) Precondition {
	n := j.rows.Change.Revision
	p := Precondition{Kind: PreconditionApproval}
	onHead, unchanged, carried := t.onHead, t.unchanged, t.carried
	switch {
	case len(onHead) > 0:
		p.State = PreconditionMet
		p.Detail = fmt.Sprintf("revision %d is approved by %s", n, namedAll(onHead))
	case len(unchanged) > 0 || len(carried) > 0:
		// EACH KIND THAT CARRIED IS NAMED, the code's first: an approval
		// carried by its trees needs no patch id, so one omitted reads as
		// no reason it carried.
		p.State = PreconditionMet
		var says []string
		if len(unchanged) > 0 {
			says = append(says, fmt.Sprintf("revision %d holds the code %s approved unchanged (%s: the base and "+
				"head trees of revision %d, the latest approved)", n, namedAll(unchanged), ChangeClassNoCodeChange,
				t.unchangedFrom))
		}
		if len(carried) > 0 {
			says = append(says, fmt.Sprintf("revision %d carries the diff %s approved unchanged (equal patch ids, "+
				"revision %d the latest approved)", n, namedAll(carried), t.carriedFrom))
		}
		p.Detail = strings.Join(says, "; ")
	case t.pending:
		p.State = PreconditionComputing
		p.Detail = fmt.Sprintf("an approval of an earlier revision waits for revision %d's facts against %s, "+
			"which say whether it carries that revision's diff unchanged", n, j.rows.Change.TargetRef)
	default:
		p.State = PreconditionUnmet
		for _, asked := range j.rows.Asked {
			if !j.author(asked) && j.eligible(asked) {
				p.WaitsOn = append(p.WaitsOn, asked)
			}
		}
		if len(p.WaitsOn) == 0 {
			p.WaitsOn = []Party{j.owner}
			p.Detail = fmt.Sprintf("no approval counts on revision %d, and nobody who may approve it is asked: "+
				"its owner %s asks a reviewer", n, j.owner.named())
			break
		}
		p.Detail = fmt.Sprintf("no approval counts on revision %d: it waits on %s", n, namedAll(p.WaitsOn))
	}
	return p
}

// blocker is the blocker precondition.
func (j readinessJudge) blocker() Precondition {
	standing := max(j.rows.Standing, int64(len(j.rows.Blockers)))
	if standing == 0 {
		return Precondition{Kind: PreconditionBlocker, State: PreconditionMet, Detail: "no blocker stands"}
	}
	p := Precondition{Kind: PreconditionBlocker, State: PreconditionUnmet}
	maintainers := maintainersOf(j.rows.Repo, j.roster)
	open := false
	var raisers []Party
	for _, b := range j.rows.Blockers {
		p.Blockers = append(p.Blockers, Blocker{FindingID: b.FindingID, State: b.State,
			ClearedBy: ClearedBy{Raiser: b.Raiser, Maintainers: maintainers, Persons: true}})
		if b.State == FindingOpen {
			open = true
		} else if !slices.Contains(raisers, b.Raiser) {
			raisers = append(raisers, b.Raiser)
		}
	}
	// AN OPEN BLOCKER WAITS ON THE CHANGE'S OWNER, to change the lines or
	// answer it; AN ANSWERED ONE ON ITS RAISER, who judges the answer —
	// resolved, won't fix and disputed all leave it standing (S40).
	if open {
		p.WaitsOn = append(p.WaitsOn, j.owner)
	}
	for _, r := range raisers {
		if !slices.Contains(p.WaitsOn, r) {
			p.WaitsOn = append(p.WaitsOn, r)
		}
	}
	p.BlockersOmitted = standing - int64(len(p.Blockers))
	who := "a seat that maintains the repository or a person"
	if len(maintainers) == 0 {
		who = "a person — no seat maintains the repository"
	}
	p.Detail = fmt.Sprintf("%d %s %s: each is cleared by its raiser's dismissal or review of a later "+
		"revision that does not carry it, or by the dismissal of %s", standing, plural(standing, "blocker",
		"blockers"), plural(standing, "stands", "stand"), who)
	return p
}

// mergeable is the mergeable precondition.
func (j readinessJudge) mergeable() Precondition {
	n, target := j.rows.Change.Revision, j.rows.Change.TargetRef
	p := Precondition{Kind: PreconditionMergeable}
	switch m := j.rows.Merge; {
	case m == nil || m.Revision != n || m.TargetRef != target:
		p.State = PreconditionComputing
		p.Detail = fmt.Sprintf("revision %d's merge into %s is being read by the repository's home Gitlet node", n,
			target)
	case m.State == MergeClean:
		p.State = PreconditionMet
		p.Detail = fmt.Sprintf("revision %d merges cleanly into %s as of %s", n, target, abbrev(m.TargetOID))
	case m.State == MergeConflict:
		p.State, p.WaitsOn = PreconditionUnmet, []Party{j.owner}
		p.Detail = fmt.Sprintf("revision %d conflicts with %s as of %s in %d %s: its owner %s merges the target "+
			"into it", n, target, abbrev(m.TargetOID), m.Conflicts, plural(m.Conflicts, "path", "paths"),
			j.owner.named())
	default:
		// UNKNOWN, or a later build's state: nothing this build reads as
		// decided, so the home merges again.
		p.State = PreconditionComputing
		p.Detail = fmt.Sprintf("revision %d's merge into %s as of %s decided nothing within its deadline: the "+
			"repository's home Gitlet node merges it again", n, target, abbrev(m.TargetOID))
	}
	return p
}

// headFacts is the head revision's recorded facts, and whether they are
// against the change's CURRENT target: facts of another branch — the change
// retargeted since — are its home's to read again.
func (j readinessJudge) headFacts() (RevisionPatch, bool) {
	f, ok := j.factsOf(j.rows.Change.Revision)
	return f, ok && f.TargetRef == j.rows.Change.TargetRef
}

// factsOf is revision n's recorded facts, and false for none.
func (j readinessJudge) factsOf(n int64) (RevisionPatch, bool) {
	i := slices.IndexFunc(j.rows.Facts, func(f RevisionPatch) bool { return f.N == n })
	if i < 0 {
		return RevisionPatch{}, false
	}
	return j.rows.Facts[i], true
}

// author reports p an author of the change, by handle and kind alike — the
// rule a review's decide refuses an author's review by.
func (j readinessJudge) author(p Party) bool { return slices.Contains(j.rows.Authors, p) }

// eligible reports p one whose approval counts, author or not: a person, who
// maintains every repository, or a seat the chart lets review it. A party of
// a kind this build does not name counts nothing.
func (j readinessJudge) eligible(p Party) bool {
	switch p.Kind {
	case PartyPerson:
		return true
	case PartyAgent:
		return GrantOf(Principal{Kind: PrincipalAgent, Seat: p.Handle}, j.rows.Repo, j.roster).Access >= AccessReview
	}
	return false
}

// maintainersOf is the seats chart r lets maintain repo: the ONE seat its
// relation's first row relates by maintain ([relation]) — its owning unit's
// effective lead, or its owning seat — where that seat's tier lets it
// maintain; none where it does not, or where no seat holds the place.
func maintainersOf(repo RepoRef, r Roster) []string {
	if r == nil {
		return nil
	}
	var seat string
	switch repo.Owner.Kind {
	case OwnerUnit:
		seat = r.Lead(repo.Owner.Ref)
	case OwnerSeat:
		seat = repo.Owner.Ref
	}
	if seat == "" || GrantOf(Principal{Kind: PrincipalAgent, Seat: seat}, repo, r).Access < AccessMaintain {
		return nil
	}
	return []string{seat}
}

// namedAll is parties in a sentence, each [Party.named], in their order.
func namedAll(parties []Party) string {
	names := make([]string, len(parties))
	for i, p := range parties {
		names[i] = p.named()
	}
	return strings.Join(names, ", ")
}

// plural is one where n is 1, many otherwise.
func plural(n int64, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

// abbrev is an object id as a sentence names it: its first seven digits.
func abbrev(oid string) string {
	if len(oid) > 7 {
		return oid[:7]
	}
	return oid
}

// readinessSubject is one change whose readiness a read or a decide judges:
// its row and its repository as a grant reads it.
type readinessSubject struct {
	repo RepoRef
	row  changeRow
}

// readinessRowsIn reads what each of subjects' readiness judges, in tx — ONE
// snapshot — one statement a kind of row over the whole page, so a page costs
// [readinessStatements] at most, never a handful a change: the counters,
// authors, approving verdicts, standing asks and mergeabilities by change id;
// the facts of each head and each revision an approval stands on, by (change,
// revision); and the standing blockers, oldest first and at most
// [MaxReadinessBlockers] a change, a range of their partial index up to a cut
// read first for the few holding more. The answer is in subjects' order.
func readinessRowsIn(ctx context.Context, tx queryer, subjects []readinessSubject) ([]ReadinessRows, error) {
	out := make([]ReadinessRows, len(subjects))
	if len(subjects) == 0 {
		return out, nil
	}
	at := make(map[string]int, len(subjects))
	ids := make([]string, len(subjects))
	for i, s := range subjects {
		out[i] = ReadinessRows{Repo: s.repo, Change: s.row.wire()}
		at[s.row.id], ids[i] = i, s.row.id
	}
	list, err := jsonList(ids)
	if err != nil {
		return nil, err
	}
	if err = readinessEach(ctx, tx, "counters", readinessCountersSQL, []any{list}, at,
		func(scan func(...any) (int, error)) (err error) {
			var standing int64
			i, err := scan(&standing)
			if err == nil {
				out[i].Standing = standing
			}
			return err
		}); err != nil {
		return nil, err
	}
	if err = readinessEach(ctx, tx, "authors", readinessAuthorsSQL, []any{list}, at,
		func(scan func(...any) (int, error)) (err error) {
			var handle, kind string
			i, err := scan(&handle, &kind)
			if err == nil {
				out[i].Authors = append(out[i].Authors, Party{Handle: handle, Kind: PartyKind(kind)})
			}
			return err
		}); err != nil {
		return nil, err
	}
	if err = readinessEach(ctx, tx, "approvals", readinessApprovalsSQL, []any{list}, at,
		func(scan func(...any) (int, error)) (err error) {
			var (
				handle, kind string
				revision     int64
			)
			i, err := scan(&handle, &kind, &revision)
			if err == nil {
				out[i].Approvals = append(out[i].Approvals, Approval{Reviewer: Party{Handle: handle,
					Kind: PartyKind(kind)}, Revision: revision})
			}
			return err
		}); err != nil {
		return nil, err
	}
	if err = readinessEach(ctx, tx, "asks", readinessAsksSQL, []any{list}, at,
		func(scan func(...any) (int, error)) (err error) {
			var handle, kind string
			i, err := scan(&handle, &kind)
			if err == nil {
				out[i].Asked = append(out[i].Asked, Party{Handle: handle, Kind: PartyKind(kind)})
			}
			return err
		}); err != nil {
		return nil, err
	}
	if err = readinessEach(ctx, tx, "mergeabilities", readinessMergesSQL, []any{list}, at,
		func(scan func(...any) (int, error)) (err error) {
			var (
				m       RecordedMerge
				state   string
				omitted int64
			)
			i, err := scan(&m.Revision, &m.TargetRef, &m.TargetOID, &state, &m.Conflicts, &omitted)
			if err == nil {
				m.State, m.Conflicts = MergeState(state), m.Conflicts+omitted
				out[i].Merge = &m
			}
			return err
		}); err != nil {
		return nil, err
	}
	// THE FACTS ASKED: each head revision's, and each earlier revision's an
	// approval stands on — the only ones an approval can be carried from.
	pairs := make([][2]any, 0, len(subjects))
	for i, s := range subjects {
		asked := []int64{s.row.revision}
		pairs = append(pairs, [2]any{s.row.id, s.row.revision})
		for _, a := range out[i].Approvals {
			if a.Revision < s.row.revision && !slices.Contains(asked, a.Revision) {
				asked = append(asked, a.Revision)
				pairs = append(pairs, [2]any{s.row.id, a.Revision})
			}
		}
	}
	encoded, err := json.Marshal(pairs)
	if err != nil {
		return nil, fmt.Errorf("gitlet: encode %d revisions' keys: %w", len(pairs), err)
	}
	if err = readinessEach(ctx, tx, "facts", readinessFactsSQL, []any{string(encoded)}, at,
		func(scan func(...any) (int, error)) (err error) {
			var f RevisionPatch
			i, err := scan(&f.N, &f.TargetRef, &f.BaseTree, &f.HeadTree, &f.PatchID, &f.PatchIDOmitted)
			if err == nil {
				out[i].Facts = append(out[i].Facts, f)
			}
			return err
		}); err != nil {
		return nil, err
	}
	// THE STANDING BLOCKERS of the changes whose counters hold any: the cut of
	// the few holding more than a readiness names first ([blockerCutsSQL]),
	// then every change's up to its cut in one statement ([standingBlockersSQL]).
	cuts := map[string]int64{}
	var blocked, crowded []string
	for i := range out {
		switch id := subjects[i].row.id; {
		case out[i].Standing > MaxReadinessBlockers:
			crowded = append(crowded, id)
			fallthrough
		case out[i].Standing > 0:
			blocked = append(blocked, id)
		}
	}
	if len(blocked) == 0 {
		return out, nil
	}
	if len(crowded) > 0 {
		if list, err = jsonList(crowded); err != nil {
			return nil, err
		}
		if err = readinessEach(ctx, tx, "blockers' cuts", blockerCutsSQL, []any{list}, at,
			func(scan func(...any) (int, error)) (err error) {
				var cut sql.NullInt64
				i, err := scan(&cut)
				if err == nil && cut.Valid {
					cuts[subjects[i].row.id] = cut.Int64
				}
				return err
			}); err != nil {
			return nil, err
		}
	}
	bounds := make([][2]any, len(blocked))
	for k, id := range blocked {
		// NO CUT: all its blockers fit, by its counter or by a cut walk that
		// found fewer standing than the counter says.
		cut, cutting := cuts[id]
		if !cutting {
			cut = math.MaxInt64
		}
		bounds[k] = [2]any{id, cut}
	}
	if encoded, err = json.Marshal(bounds); err != nil {
		return nil, fmt.Errorf("gitlet: encode %d changes' blockers' bounds: %w", len(bounds), err)
	}
	raised := map[string]int64{}
	if err := readinessEach(ctx, tx, "standing blockers", standingBlockersSQL, []any{string(encoded)}, at,
		func(scan func(...any) (int, error)) (err error) {
			var (
				b           StandingBlocker
				kind, state string
				position    int64
			)
			i, err := scan(&b.FindingID, &b.Raiser.Handle, &kind, &state, &position)
			if err == nil {
				b.Raiser.Kind, b.State = PartyKind(kind), FindingState(state)
				out[i].Blockers = append(out[i].Blockers, b)
				raised[b.FindingID] = position
			}
			return err
		}); err != nil {
		return nil, err
	}
	// THE OLDEST RAISED FIRST, AND NO MORE THAN A READINESS NAMES: the
	// statement names no order, and a cut is a position every blocker of one
	// review shares, so a crowded change's read may hold the rest of that review.
	for i := range out {
		slices.SortFunc(out[i].Blockers, func(a, b StandingBlocker) int {
			if c := cmp.Compare(raised[a.FindingID], raised[b.FindingID]); c != 0 {
				return c
			}
			return strings.Compare(a.FindingID, b.FindingID)
		})
		out[i].Blockers = out[i].Blockers[:min(len(out[i].Blockers), MaxReadinessBlockers)]
	}
	return out, nil
}

// readinessStatements is the most statements [readinessRowsIn] issues for a
// page, whatever it holds: one a kind of row, the blockers' cuts issued only
// where a change holds more than a readiness names.
const readinessStatements = 8

// lastBlockerOffset is MaxReadinessBlockers-1 SPELLED IN THE STATEMENT: the
// pinned driver applies a BOUND offset of a correlated scalar subquery only to
// the first row it evaluates, reading every later row's first entry, which a
// test holds this statement clear of; a test holds the two equal.
const lastBlockerOffset = "15"

// readinessEach runs one of [readinessRowsIn]'s statements over args, handing
// each row to each: its scan reads the columns after the change's id into dest
// and answers the change's index; a row of a change not asked is an error.
func readinessEach(ctx context.Context, tx queryer, what, query string, args []any, at map[string]int,
	each func(scan func(dest ...any) (int, error)) error) error {

	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return fmt.Errorf("gitlet: read the %s a readiness judges: %w", what, err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		if err := each(func(dest ...any) (int, error) {
			var id string
			if err := rows.Scan(append([]any{&id}, dest...)...); err != nil {
				return 0, err
			}
			i, ok := at[id]
			if !ok {
				return 0, fmt.Errorf("a row of change %s, which the read did not ask", id)
			}
			return i, nil
		}); err != nil {
			return fmt.Errorf("gitlet: read the %s a readiness judges: %w", what, err)
		}
	}
	return rows.Err()
}

// blockersKey is code_findings_blockers_idx, (change_id, position, id) WHERE
// [standingBlockers] (replicated 0106): a change's standing blockers alone,
// the oldest raised first.
const blockersKey = "code_findings_blockers_idx"

// standingBlockers is the predicate code_findings_blockers_idx is partial on,
// spelled as the index declares it — the pinned planner takes a partial index
// only for a statement implying its predicate in its own words — and the
// states [FindingState.Stands] names; a test holds the two equal.
const standingBlockers = `severity = 'blocker' AND state IN ('open', 'resolved', 'wont_fix', 'disputed')`

// A readiness's reads, each registered in plan_test.go and driven from a JSON
// list with a seek of a key an element: the per-change kinds by change id (the
// asks through [reviewersKey], NAMED, which says why), the facts by (change,
// revision). A cut is a correlated scalar subquery walking the blockers'
// partial index MaxReadinessBlockers entries in — the pinned planner reads
// every finding of the company for a `rowid IN` subquery naming the first
// sixteen — and the standing blockers are each change's range of that index up
// to its cut. So a change holding a thousand blockers costs sixteen entries for
// its cut and at most MaxReadinessBlockers + MaxFindingsPerReview - 1 rows.
const (
	readinessCountersSQL = `SELECT v.change_id, v.open_blockers FROM json_each(?) AS j
		CROSS JOIN code_conversations AS v WHERE v.change_id = CAST(j.value AS TEXT)`
	readinessAuthorsSQL = `SELECT a.change_id, a.author, a.author_kind FROM json_each(?) AS j
		CROSS JOIN code_change_authors AS a WHERE a.change_id = CAST(j.value AS TEXT)`
	readinessApprovalsSQL = `SELECT p.change_id, p.handle, p.kind, p.verdict_revision FROM json_each(?) AS j
		CROSS JOIN code_participants AS p WHERE p.change_id = CAST(j.value AS TEXT) AND p.verdict = 'approve'`
	readinessAsksSQL = `SELECT r.change_id, r.reviewer, r.reviewer_kind FROM json_each(?) AS j
		CROSS JOIN code_reviewers AS r INDEXED BY ` + reviewersKey + `
		WHERE r.change_id = CAST(j.value AS TEXT) AND r.state = 'requested'`
	readinessMergesSQL = `SELECT m.change_id, m.revision, m.target_ref, m.target_oid, m.state, m.conflicts_count,
			m.conflicts_omitted
		FROM json_each(?) AS j CROSS JOIN code_mergeability AS m WHERE m.change_id = CAST(j.value AS TEXT)`
	readinessFactsSQL = `SELECT f.change_id, f.n, f.target_ref, f.base_tree, f.head_tree, f.patch_id,
			f.patch_id_omitted
		FROM json_each(?) AS j CROSS JOIN code_revision_facts AS f
		WHERE f.change_id = CAST(j.value ->> 0 AS TEXT) AND f.n = CAST(j.value ->> 1 AS INTEGER)`
	blockerCutsSQL = `SELECT CAST(j.value AS TEXT), (SELECT position FROM code_findings INDEXED BY ` + blockersKey + `
			WHERE change_id = CAST(j.value AS TEXT) AND ` + standingBlockers + `
			ORDER BY position, id LIMIT 1 OFFSET ` + lastBlockerOffset + `)
		FROM json_each(?) AS j`
	standingBlockersSQL = `SELECT f.change_id, f.id, f.raiser, f.raiser_kind, f.state, f.position
		FROM json_each(?) AS j CROSS JOIN code_findings AS f INDEXED BY ` + blockersKey + `
		WHERE f.change_id = CAST(j.value ->> 0 AS TEXT) AND ` + standingBlockers + `
			AND f.position <= CAST(j.value ->> 1 AS INTEGER)`
)

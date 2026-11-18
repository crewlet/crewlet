package gitlet

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/notify"
	"github.com/crewlet/crewlet/internal/routing"
)

// WHOM A CHANGE RECORD CONCERNS: every record about a change carries a ROUTING
// SNAPSHOT ([ChangeRouting]): who made it, and the handles each reason it
// routes under reaches, as the deciding snapshot and chart held them. Every
// writer stamps it through ONE function ([routingFor]), and everything that
// tells somebody about the record reads the snapshot alone: [Concerns] is pure
// over it, through internal/routing's one precedence rule.
//
// A SNAPSHOT, NOT THE ROWS: who a record concerns is a fact of the moment it
// was decided. The rows move on (a reviewer withdrawn, a thread grown), and a
// reader deriving the audience later, on another node, would tell different
// people than the applier did; carrying the answer makes the applier's notice
// rows and the feed's wakes agree.

// # One role per reason, filled for the reasons the op routes by
//
// A record fills only the roles its op routes by (a close its watchers, a
// review its authors and mentions, a revise the path owners and reviewers it
// asks again): a comment carrying the change's sixty-four watchers would pay
// for a fan-out nobody reads. A push moving no change concerns nobody.
//
//   - mention: the handles an open's, edit's, review's or comment's text or a
//     push's options name, held to the deciding chart.
//   - review_requested: the reviewers an ask names that were not asked
//     already, and the path owners a revision's facts ask.

//   - changes_requested, review_submitted: the authors, by the review's verdict.
//   - checks_failed: the authors and the record's actor (the head's pusher, or
//     whoever's CI reported it), on a check of the current head that failed or
//     errored past the home's requeue: the one reason news to its actor
//     ([Reason.WakesActor]), since only this tells a pusher their commit fails.
//   - target_broken: the authors and the owner's effective lead, on the broken
//     record naming the landing that merged the change; never an enqueuer, as
//     only a landing ahead of its checks, which no queue made, can break.
//   - conflicted: the authors, on the mergeability that TURNED the merge into a
//     conflict (not every recheck finding it still one), and on a queue's
//     ejection of it for a conflicting merge.
//   - dequeued: the authors and the enqueuer, on an ejection or an asked dequeue.

//   - finding_replied: the raiser of the finding a comment replies to, and of
//     every finding an answer answers.
//   - ready_to_merge: the authors who may merge it into its target
//     ([PreconditionMergeAuthority] judged for each), else the owner's
//     effective lead, on the mergeability whose stored answer turns ready.
//   - merged: the authors, on the push that set the target to the head and on
//     its home's landing, on the merge record and on the queue's landing; each
//     but who merged it, which on the home's landing is whoever made the push
//     it found bringing the head in, never whoever moved the target last.
//   - re_review: the reviewers whose standing verdict asks for changes on an
//     earlier revision, on a revision's first facts.
//   - thread: the participants of the thread a comment is said in.

//   - watcher: the participants, on a close, a reopen, a retarget and a
//     draft's state.
//   - lead_fallback: the lead of the repository's owner, on a change's first
//     facts where nobody who may review it is asked.

// # A record with no audience carries no routing
//
// [routingFor] answers nil where no role the op routes by is filled (a comment
// mentioning nobody outside any thread, a thread's resolution, facts that ask
// nobody, a mergeability that turns nothing): else the change's context, some
// three hundred bytes, would ride every such record on every node's log for a
// fan-out of none. [RoutingsOf] names none for it, so the applier writes no
// notice and the feed acknowledges it without a wake. Where there IS an
// audience the routing stays whole, context included: the feed's node routes
// a record without reading a row it may lag on.

// # ready_to_merge is announced by ONE record, and why it is that one
//
// Readiness is judged from one snapshot of the change's rows ([ReadinessOf]),
// but its inputs live on TWO subjects: a draft's state and a revision on the
// repository's, an approval and a blocker's answer on the change's. A record
// on either computing it from its own snapshot is write skew: an approval and
// an undraft decided at once each see the other unapplied, and the transition
// both complete is announced by neither. So a mergeability record (the home's
// merge or its refresh, which follows any input that moved) stores readiness,
// and the record whose decide finds the stored `ready` going false to true is
// the one that announces it.

// # The actor
//
// Every record names the deciding NODE as its actor ([ActorSystem]), so the
// routing names who acted ([ChangeRouting.Actor]; [concerning]'s actor says
// who, record by record). On the home's landing that is the pusher of the push
// the home found bringing the head into the target ([landingPusher]), never
// the push that moved the target last, which may be somebody's on top. A
// reason is news to its actor only where [Reason.WakesActor] says: a reviewer
// is not told of their own review, nor a pusher of their own merge, but a
// pusher IS told the head they pushed fails its checks.

// # Whom the actor acted for
//
// A seat acts for whoever its turn works for, and a routing that may wake a
// seat says so beside its actor ([ChangeRouting.WorkingFor], ADR-0109): whom
// the asker's request was made for (writtenForInvalid, writtenForUnder), so
// the turn it wakes works for them, held to what they may use. Only beside an
// agent's actor, and only where the routing wakes a seat ([wakesASeat]): a
// person, a credential and the engine (save the exception below) write for
// nobody but themselves, and a routing telling only persons wakes no turn.

// THE ENGINE'S RECORDS THAT NAME AN EARLIER ACTOR carry whom that act was made
// for, as its row keeps it (code_revisions, code_pushes and code_queue; the
// replicated migration a_code_row_keeps_whom_an_agents_write_was_made_for),
// under the same two conditions, all read in the deciding snapshot: a
// revision's facts and a failed head check the revision pusher's
// ([revisionPusher]), the home's landing the merging push's ([landingPusher]),
// a queue's landing each entry's enqueue's ([decideLand]). Each wake is about
// that act, so it works for whom the act was made for, as the act's own wake
// did. A row that keeps nobody (made for nobody, a person's, or applied before
// the column was filled) carries nobody, and the turn works for the actor.

// THE ONE EXCEPTION, BESIDE NO ACTOR: the merge queue's own records about an
// entry's enqueue, its ejection ([decideEject]) and a close of the landed
// change's work item settled not_closed ([decideCloseSettled]), name no actor,
// tell the enqueuer and the authors about that enqueue, so each carries whom
// it was made for ([concerning.enqueuedFor]) where it wakes a seat. Worked for
// the told seat's own, the turn would re-queue the change or close the item
// its requester could not, with every app the seat holds (ADR-0109's
// amendment). Every other engine record about no earlier act (a mergeability,
// a broken landing) carries nobody: wake.go's writerOf hands the engine's
// carrier on for these two ops alone.

// # Who hears what the gate does
//
// The gate's records (a merge, a dequeue, a queue's ejection and landing, a
// broken landing, a head check's ending) are stamped through [routingFor] by
// their own writer: the committer, or the checks writer for an ending. A
// record of several changes carries one routing a change, KEPT WITHIN THE
// RECORD'S ROOM ([keptRoutings]): the record must land, so a change past the
// room is told to nobody. A landing no change made (a person's push onto a
// protected branch) carries no routing; the activity feed's
// `code_target_broken` event reports it.

// # The record format
//
// Every routing field but one is in the base format ([RecordVersion]);
// [ChangeRouting.WorkingFor] came with version 2, and a record whose routing
// carries it is stamped there ([versionedFields]), so an older build retains
// the record rather than read it short of whom its actor acted for. An open's,
// an edit's, a review's and a comment's mentions ride the routing alone
// ([withRouting]).

// Reason is why one handle hears about a change (code_notifications.reason) —
// a closed set, in precedence order ([Reasons]).
type Reason string

// The reasons, IN PRECEDENCE ORDER: the declarations are the order, and
// [Reasons] lists them as written.
const (
	ReasonMention          Reason = "mention"
	ReasonReviewRequested  Reason = "review_requested"
	ReasonChangesRequested Reason = "changes_requested"
	ReasonChecksFailed     Reason = "checks_failed"
	ReasonTargetBroken     Reason = "target_broken"
	ReasonConflicted       Reason = "conflicted"
	ReasonDequeued         Reason = "dequeued"
	ReasonFindingReplied   Reason = "finding_replied"
	ReasonReadyToMerge     Reason = "ready_to_merge"
	ReasonMerged           Reason = "merged"
	ReasonReviewSubmitted  Reason = "review_submitted"
	ReasonReReview         Reason = "re_review"
	ReasonThread           Reason = "thread"
	ReasonWatcher          Reason = "watcher"
	ReasonLeadFallback     Reason = "lead_fallback"
)

// Reasons is the fifteen, IN PRECEDENCE ORDER: the first reason naming a
// handle is the one it hears under, and [Concerns] offers them in this order,
// so the precedence is written once. WHAT THIS RECORD DID TO YOU OUTRANKS THE
// ROLE YOU HOLD: being named, then asked, then the change's state turning
// under you, then what you hold (a review, a thread, a watch), the lead's copy
// last. A failed check sits right after a request for changes (both say the
// change must change), a broken target next, before a conflict (it stops
// everybody merging), and a dequeue after a conflict, which says why.
//
// All fifteen are in the base format ([RecordVersion]), so every build's
// [Reason.Valid] and every reader of code_notifications knows them.
func Reasons() []Reason {
	return []Reason{ReasonMention, ReasonReviewRequested, ReasonChangesRequested, ReasonChecksFailed,
		ReasonTargetBroken, ReasonConflicted, ReasonDequeued, ReasonFindingReplied, ReasonReadyToMerge,
		ReasonMerged, ReasonReviewSubmitted, ReasonReReview, ReasonThread, ReasonWatcher, ReasonLeadFallback}
}

// Valid reports a reason this build knows.
func (r Reason) Valid() bool { return slices.Contains(Reasons(), r) }

// Addressed reports whether a notice under r ASKS its recipient something, a
// turn that must answer rather than absorb: every reason through
// ready_to_merge in [Reasons], and re_review. The rest is news to absorb; a
// PERSON's inbox leads with [DefaultPrimaryReasons].
func (r Reason) Addressed() bool {
	switch r {
	case ReasonMention, ReasonReviewRequested, ReasonChangesRequested, ReasonChecksFailed, ReasonTargetBroken,
		ReasonConflicted, ReasonDequeued, ReasonFindingReplied, ReasonReadyToMerge, ReasonReReview:
		return true
	}
	return false
}

// WakesActor reports whether a notice under r reaches the record's own actor
// too: only checks_failed, since nothing else tells a pusher their head fails
// its checks (as a failed pipeline tells its pusher on GitHub and GitLab: their
// prompts' WakesActor).
// Every other reason is about something the actor did and knows.
func (r Reason) WakesActor() bool { return r == ReasonChecksFailed }

// DefaultPrimaryReasons is the code reasons that lead the inbox of a person who
// has expressed no preference: the addressed ones plus your own change merged
// or reviewed, which changes what you do next as surely as an ask. A thread, a
// watch and a lead's copy are context, and context competing with action turns
// an inbox into a feed nobody reads. Deliberately NOT [Reason.Addressed]'s
// set: the two questions differ, as the tracker's own two lists do.
var DefaultPrimaryReasons = []Reason{
	ReasonMention, ReasonReviewRequested, ReasonChangesRequested, ReasonChecksFailed, ReasonTargetBroken,
	ReasonConflicted, ReasonDequeued, ReasonFindingReplied, ReasonReadyToMerge, ReasonMerged,
	ReasonReviewSubmitted, ReasonReReview,
}

// ChangeRouting is whom one record about a change concerns, as the writer's
// snapshot and chart held them, and who made the change it records, for whom
// — see this file's head. A handle is a seat's, or the human seat a person
// holds; [ChangeRouting.Persons] says which.
type ChangeRouting struct {
	// ChangeID is the change, and ChangeRef its name as the snapshot held it
	// (web!42) — an open's, the number its apply will give.
	ChangeID  string `json:"change_id"`
	ChangeRef string `json:"change_ref"`
	// RepoName and Title are the repository's name and the change's title as
	// the record leaves them.
	RepoName string `json:"repo_name"`
	Title    string `json:"title" untrusted:"name"`
	// Actor is who made the change the record records ([concerning]'s actor),
	// "" for the home's records no principal's act made. Nobody hears of a
	// change they made, save under [Reason.WakesActor].
	Actor string `json:"actor,omitempty"`
	// WorkingFor is whom Actor made the change for (ADR-0109): whoever the turn
	// that asked the write worked for (a person's seat, a colleague seat's, or
	// a credential acting as no seat with the apps it was held to), so the
	// woken turn works for them. Stamped by [routingFor] only on a routing that
	// wakes a seat ([wakesASeat]), beside an agent's Actor or, on the merge
	// queue's ejection and unclosed close, beside none; nil otherwise and for
	// the seat's own work (this file's head, "Whom the actor acted for").
	//
	// VERSION 2 ([versionedFields]): an older build retains the record rather
	// than wake a turn for the writing seat alone.
	WorkingFor *types.WorkingFor `json:"working_for,omitempty"`
	// Owner is the change's owner, and OwnerKind what it is.
	Owner     string    `json:"owner,omitempty"`
	OwnerKind PartyKind `json:"owner_kind,omitempty"`
	// WorkItem is the work item the change carries, nil for none: what a wake
	// names, so its turn is charged to the work it serves.
	WorkItem *types.WorkItem `json:"work_item,omitempty"`

	// Mentions is the handles the write's text names that the deciding chart
	// holds, at most [MaxMentions].
	Mentions []string `json:"mentions,omitempty"`
	// Requested is the reviewers an ask names that the change did not ask
	// already, and PathOwners the seats a revision's facts ask for its path
	// owners — each at most [MaxReviewersPerChange].
	Requested  []string `json:"requested,omitempty"`
	PathOwners []string `json:"path_owners,omitempty"`
	// Authors is the change's authors — its owner and everybody who moved its
	// head — at most [MaxRoutedParticipants], on the records that tell them:
	// a review, a merge, a conflict.
	Authors []string `json:"authors,omitempty"`
	// Verdict is the verdict a review gave, which tells its authors changes
	// were requested or that it was reviewed.
	Verdict Verdict `json:"verdict,omitempty"`
	// Conflicted is a mergeability that turned the change's merge into a
	// conflict, or a queue's ejection of it for one; Merged a record that
	// merged the change.
	Conflicted bool `json:"conflicted,omitempty"`
	Merged     bool `json:"merged,omitempty"`
	// NotClosed is, on a close_settled's routing of a change whose close
	// ended not_closed, why its work item was not closed (M5-D27): its authors
	// and Enqueuer hear it under `merged`, as their notice's excerpt.
	NotClosed string `json:"not_closed,omitempty"`
	// ChecksFailed is a check of the change's head that failed, or errored
	// past the home's own requeue: its authors and its actor hear.
	ChecksFailed bool `json:"checks_failed,omitempty"`
	// Broken is a broken record naming the landing that merged the change:
	// its authors and Lead hear. Dequeued is a record that put the
	// change's entry out of its queue: its authors and Enqueuer hear.
	Broken   bool `json:"broken,omitempty"`
	Dequeued bool `json:"dequeued,omitempty"`
	// Enqueuer is who put the change in its target's queue, on the records
	// about its entry; "" for none.
	Enqueuer string `json:"enqueuer,omitempty"`
	// Lead is the effective lead of the repository's owner, on a broken
	// record; "" for none.
	Lead string `json:"lead,omitempty"`
	// Raisers is the raisers of the findings a write replies to or answers,
	// at most [MaxRoutedParticipants].
	Raisers []string `json:"raisers,omitempty"`
	// ReadyFor is who a change that became ready to merge is announced to:
	// its authors who may merge it into its target, else the effective lead
	// of the repository's owner.
	ReadyFor []string `json:"ready_for,omitempty"`
	// ReReview is the reviewers a revision's first facts ask to look again,
	// at most [MaxReReviewers].
	ReReview []string `json:"re_review,omitempty"`
	// ThreadParticipants is everybody who said something in the thread a
	// comment is said in, at most [MaxRoutedParticipants].
	ThreadParticipants []string `json:"thread_participants,omitempty"`
	// Watchers is the change's participants — everybody asked, who reviewed,
	// commented or was mentioned in its conversation — at most
	// [MaxRoutedParticipants].
	Watchers []string `json:"watchers,omitempty"`
	// LeadFallback is the lead a change nobody who may review it is asked
	// reaches, "" for none.
	LeadFallback string `json:"lead_fallback,omitempty"`
	// Persons is which of the handles named above are persons — the actor
	// among them — sorted: what a notice row's recipient kind and its horizon
	// are read from, what keeps a person from ever being queue-woken, and
	// what tells a wake whether a person or a seat wrote what it announces.
	Persons []string `json:"persons,omitempty"`
}

// heard is who in r hears under reason, as its record's op filled it.
func (r *ChangeRouting) heard(reason Reason) []string {
	switch reason {
	case ReasonMention:
		return r.Mentions
	case ReasonReviewRequested:
		return slices.Concat(r.Requested, r.PathOwners)
	case ReasonChangesRequested:
		if r.Verdict == VerdictRequestChanges {
			return r.Authors
		}
	case ReasonChecksFailed:
		if r.ChecksFailed {
			return withOnce(r.Authors, r.Actor)
		}
	case ReasonTargetBroken:
		if r.Broken {
			return withOnce(r.Authors, r.Lead)
		}
	case ReasonConflicted:
		if r.Conflicted {
			return r.Authors
		}
	case ReasonDequeued:
		if r.Dequeued {
			return withOnce(r.Authors, r.Enqueuer)
		}
	case ReasonFindingReplied:
		return r.Raisers
	case ReasonReadyToMerge:
		return r.ReadyFor
	case ReasonMerged:
		switch {
		case r.Merged && r.NotClosed != "":
			// THE ENQUEUER TOO: the close was asked in their name, so the
			// item left open is theirs to close.
			return withOnce(r.Authors, r.Enqueuer)
		case r.Merged:
			return r.Authors
		}
	case ReasonReviewSubmitted:
		if r.Verdict == VerdictApprove || r.Verdict == VerdictComment {
			return r.Authors
		}
	case ReasonReReview:
		return r.ReReview
	case ReasonThread:
		return r.ThreadParticipants
	case ReasonWatcher:
		return r.Watchers
	case ReasonLeadFallback:
		if r.LeadFallback != "" {
			return []string{r.LeadFallback}
		}
	}
	return nil
}

// withOnce is list with each of more after it that it does not hold yet, and
// no "".
func withOnce(list []string, more ...string) []string {
	out := slices.Clip(list)
	for _, h := range more {
		if h != "" && !slices.Contains(out, h) {
			out = append(out, h)
		}
	}
	return out
}

// reviewer is p as the parties a routing names.
func (p Party) reviewer() Reviewer { return Reviewer(p) }

// Person reports whether handle is a person's, as r names it.
func (r *ChangeRouting) Person(handle string) bool {
	_, found := slices.BinarySearch(r.Persons, handle)
	return found
}

// Concern is one handle a record concerns: the candidate the routing rule
// keeps — its reason, whether it is addressed, its place in the fallback — and
// the change it hears about.
type Concern struct {
	routing.Candidate[Reason]
	// Change is the routing the candidate was offered from: the change it
	// hears about, and who made it.
	Change *ChangeRouting
}

// Concerns is every handle one record's routings concern, change by change in
// first-appearance order, each in precedence order. PURE OVER THE ROUTINGS (no
// row, chart or clock), so the applier's notices and the feed's wakes derive
// one set on any node at any time.
//
// THE ADD-ONCE RULE IS PER CHANGE: a handle one change's routings name under
// two reasons hears under the stronger (internal/routing's Builder), and a
// handle two of a record's changes name hears about each: the committer folds
// up to [MaxPushesPerRecord] unrelated pushes into one record, and keying on
// the record alone would lose all but the first. So a notice is keyed on record,
// change and recipient ([NoticeID], replicated 0133), as a wake's id is;
// routings naming one change twice (opened, then merged) share a builder.
func Concerns(routings []*ChangeRouting) []Concern {
	var (
		order  []string
		byID   = map[string][]*ChangeRouting{}
		out    []Concern
		reason = Reasons()
	)
	for _, r := range routings {
		if r == nil {
			continue
		}
		if _, seen := byID[r.ChangeID]; !seen {
			order = append(order, r.ChangeID)
		}
		byID[r.ChangeID] = append(byID[r.ChangeID], r)
	}
	for _, id := range order {
		var b routing.Builder[Reason]
		for _, why := range reason {
			for _, r := range byID[id] {
				handles := r.heard(why)
				if len(handles) == 0 {
					continue
				}
				before := len(b.Candidates())
				if why == ReasonLeadFallback {
					b.Fallback(why, handles...)
				} else {
					b.AddAll(handles, why, why.Addressed())
				}
				for _, c := range b.Candidates()[before:] {
					out = append(out, Concern{Candidate: c, Change: r})
				}
			}
		}
	}
	return out
}

// wakesASeat reports whether concerns tell a seat anything: some handle hears
// ([Concern.Hears]) that is not a person's, the lead's fallback included. THE
// ONE TEST the feed relays a record by (feed.go) and [routingFor] stamps
// WorkingFor by, so the two never disagree about a routing.
func wakesASeat(concerns []Concern) bool {
	for _, c := range concerns {
		if c.Hears() && !c.Change.Person(c.Handle) {
			return true
		}
	}
	return false
}

// NoticeID is one notice's identity among its recipient's: the record that
// reached them ([RecordID]) and the change it told them about. A person's mark
// names it, an inbox notice carries it as record_id, and a wake's id derives
// from it ([changefeed.WakeID]), so a record telling one handle about two
// changes leaves two notices and two wakes, and a redelivery collapses into
// them. "/" separates, which no change id (a bare UUID: [OpenChangeID],
// [PushChangeID]) holds, so [splitNoticeID] splits at the last one.
func NoticeID(recordID, changeID string) string { return recordID + "/" + changeID }

// splitNoticeID is id's record and change ([NoticeID]), and false for an id
// that names no change.
func splitNoticeID(id string) (record, change string, ok bool) {
	i := strings.LastIndexByte(id, '/')
	if i <= 0 || i == len(id)-1 {
		return "", "", false
	}
	return id[:i], id[i+1:], true
}

// Hears reports whether c is told: everybody but who made the change, except
// under the one reason that is news to its own actor ([Reason.WakesActor]).
func (c Concern) Hears() bool { return c.Candidate.Hears(c.Change.Actor, Reason.WakesActor) }

// changeNotices is how many handles one record tells about one change: the
// notices its apply may write about it, a (change, handle) each.
type changeNotices struct {
	changeID string
	notices  int
}

// noticesByChange is, for every change routings tell anybody about, how many
// handles hear of it ([Concern.Hears]), in [Concerns]' order of first hearers;
// a handle two changes concern counts under each. PURE OVER THE ROUTINGS, so
// code_changes.notices (replicated 0130) takes the same count on every node,
// whatever the horizon let each write.
func noticesByChange(routings ...*ChangeRouting) []changeNotices {
	var out []changeNotices
	for _, c := range Concerns(routings) {
		if !c.Hears() {
			continue
		}
		i := slices.IndexFunc(out, func(n changeNotices) bool { return n.changeID == c.Change.ChangeID })
		if i < 0 {
			out = append(out, changeNotices{changeID: c.Change.ChangeID})
			i = len(out) - 1
		}
		out[i].notices++
	}
	return out
}

// noticeWrites is the rows a record's apply writes for whom routings tell (a
// notice per (change, handle), [Applier.writeNotices], plus one count row per
// change told), which every decide counts with the record's rows. A push
// decide counts each entry's routings apart, so a change two entries route is
// counted twice and written once: an over-count, which only carries an entry
// to the next record early.
func noticeWrites(routings ...*ChangeRouting) int {
	by := noticesByChange(routings...)
	n := len(by)
	for _, c := range by {
		n += c.notices
	}
	return n
}

// Candidates is every handle rec concerns ([Concerns] over its routings) — none
// for a record of no change.
func Candidates(rec MutationRecord) ([]Concern, error) {
	payload, err := DecodeMutation(rec)
	if err != nil {
		return nil, err
	}
	return Concerns(RoutingsOf(payload)), nil
}

// RoutingsOf is the routings a decoded payload carries — one for a record about
// a change, one for each change a push record merges or opens — and none for
// any other.
func RoutingsOf(payload any) []*ChangeRouting {
	var r *ChangeRouting
	switch p := payload.(type) {
	case PushBatch:
		return pushRoutings(p)
	case ChangeOpen:
		r = p.Routing
	case ChangeEdit:
		r = p.Routing
	case ChangeDraft:
		r = p.Routing
	case ChangeRetarget:
		r = p.Routing
	case ChangeClose:
		r = p.Routing
	case ChangeReopen:
		r = p.Routing
	case ChangeLanded:
		r = p.Routing
	case ReviewerRequest:
		r = p.Routing
	case Comment:
		r = p.Routing
	case ThreadResolve:
		r = p.Routing
	case Review:
		r = p.Routing
	case FindingResolve:
		r = p.Routing
	case Revise:
		r = p.Routing
	case Mergeability:
		r = p.Routing
	case Merge:
		r = p.Routing
	case Dequeue:
		r = p.Routing
	case Broken:
		r = p.Routing
	case CheckFinished:
		r = p.Routing
	case CloseSettled:
		return pointersTo(p.Routing)
	case Eject:
		return pointersTo(p.Routing)
	case Land:
		return pointersTo(p.Routing)
	}
	if r == nil {
		return nil
	}
	return []*ChangeRouting{r}
}

// pointersTo is routings as the pointers [Concerns] reads, each into the
// record's own list.
func pointersTo(routings []ChangeRouting) []*ChangeRouting {
	var out []*ChangeRouting
	for i := range routings {
		out = append(out, &routings[i])
	}
	return out
}

// pushRoutings is the routings a push record carries, one per change its
// entries merge or open, each pointing into the record's entries. A function
// of its own so the batch is a typed parameter: internal/sourcetree/decltype
// cannot type a type switch's variable, and the cache-writers gate
// (TestNothingButSyncWritesACache) refuses one handed to append, which it
// cannot tell from a gitproc.Cmd.
func pushRoutings(b PushBatch) []*ChangeRouting {
	var out []*ChangeRouting
	for i := range b.Pushes {
		for j := range b.Pushes[i].Routing {
			out = append(out, &b.Pushes[i].Routing[j])
		}
	}
	return out
}

// routingNoticeRows is the most notice rows ONE routing of the gate's records
// makes its apply write, which each such record's rows are charged with: the
// change's authors ([MaxRoutedParticipants]) and the two the gate names beside
// them — an enqueuer and a lead, or an actor.
const routingNoticeRows = MaxRoutedParticipants + 2

// routingRoomBeside is the bytes a record whose payload is payload leaves its
// routings: what [MaxRecordBytes] leaves past the record's own overhead and the
// payload as it stands.
func routingRoomBeside(payload any) (int, error) {
	encoded, err := json.Marshal(payload)
	if err != nil {
		return 0, fmt.Errorf("gitlet: measure a record's payload: %w", err)
	}
	return MaxRecordBytes - recordOverheadBytes - len(encoded), nil
}

// keptRoutings is routings, in their order, as many as fit within room bytes
// and rows notice rows: the record lands whatever it would tell, and a change
// past the room is told to nobody (this file's head), as a push's leading
// entry keeps what fits ([routingRoom]).
func keptRoutings(routings []ChangeRouting, room, rows int) ([]ChangeRouting, error) {
	for len(routings) > 0 {
		size, err := routingBytes(routings)
		if err != nil {
			return nil, err
		}
		if size <= room && noticeRows(routings) <= rows {
			return routings, nil
		}
		routings = routings[:len(routings)-1]
	}
	return nil, nil
}

// withRouting is payload with r stamped on it (nil where the record concerns
// nobody), and an open's, an edit's, a comment's and a review's asker mentions
// taken off: [routingFor] holds them to the deciding chart into the routing,
// the copy every reader reads, so a copy beside it would be read by nobody and
// may name handles the chart does not hold, on every node's log.
func withRouting(payload any, r *ChangeRouting) any {
	switch p := payload.(type) {
	case ChangeOpen:
		p.Routing, p.Mentions = r, nil
		return p
	case ChangeEdit:
		p.Routing, p.Mentions = r, nil
		return p
	case ChangeDraft:
		p.Routing = r
		return p
	case ChangeRetarget:
		p.Routing = r
		return p
	case ChangeClose:
		p.Routing = r
		return p
	case ChangeReopen:
		p.Routing = r
		return p
	case ChangeLanded:
		p.Routing = r
		return p
	case ReviewerRequest:
		p.Routing = r
		return p
	case Comment:
		p.Routing, p.Mentions = r, nil
		return p
	case ThreadResolve:
		p.Routing = r
		return p
	case Review:
		p.Routing, p.Mentions = r, nil
		return p
	case FindingResolve:
		p.Routing = r
		return p
	case Revise:
		p.Routing = r
		return p
	case Mergeability:
		p.Routing = r
		return p
	case Merge:
		p.Routing = r
		return p
	case Dequeue:
		p.Routing = r
		return p
	case Broken:
		p.Routing = r
		return p
	case CheckFinished:
		p.Routing = r
		return p
	}
	panic(fmt.Sprintf("gitlet: a %T carries no routing — every record about a change does (RoutingsOf)", payload))
}

// concerning is what a record's decide hands [routingFor]: the change as the
// record leaves it, who made it, and the facts of the record's own op — each
// role filled only where the op routes by it (this file's head).
type concerning struct {
	repoName string
	// change is the change's row as the record leaves it: its id, number,
	// title, owner and work item.
	change changeRow
	// actor is who made the change: the asker or a push entry's pusher; for a
	// revision's facts and a failed head check its pusher, for a landing the
	// merging push's pusher, for a queue's landing the entry's enqueuer; "" for
	// the home's other records. actorKind names a person's actor among the persons, so a wake
	// says a person wrote what it announces.
	actor     string
	actorKind PartyKind
	// workingFor is whom actor made the change for: the asker's stated
	// carrier, or for an earlier actor the one its act left on its row
	// ([revisionPusher], [landingPusher], the entry's); [routingFor] stamps it
	// beside an agent's actor alone, on a routing that wakes a seat.
	workingFor *types.WorkingFor
	// enqueuedFor is whom an entry's enqueue was made for
	// (code_queue.working_for), set only by the merge queue's ejection and
	// unclosed close, which name no actor; [routingFor] stamps it beside no
	// actor alone, on a routing that wakes a seat, so no other engine record
	// carries a carrier beside no actor.
	enqueuedFor *types.WorkingFor
	// roster is the deciding chart: what a mention is held to and what says
	// whether a handle is a person's. Nil only for a record that names no
	// mention.
	roster Roster
	// mentions is the handles the write names — held to roster, at most
	// MaxMentions — and mentionText text a push's options name them in,
	// which the committer reads them out of.
	mentions    []string
	mentionText []string
	// requested, pathOwners, reReview, raisers and readyFor are the parties
	// the op names under their reasons; lead the lead's copy, a zero Reviewer
	// for none.
	requested, pathOwners, reReview, raisers, readyFor []Reviewer
	lead                                               Reviewer
	// verdict is a review's; conflicted, merged, checksFailed, broken and
	// dequeued the op's facts.
	verdict                                            Verdict
	conflicted, merged, checksFailed, broken, dequeued bool
	// notClosed is a close_settled's not_closed detail.
	notClosed string
	// enqueuer is who put the change in its queue, and ownerLead the
	// effective lead of the repository's owner, each a zero Reviewer for
	// none.
	enqueuer, ownerLead Reviewer
	// authors reads the change's authors, beside addedAuthors — the authors
	// a push record's earlier entries added, which no row holds yet; watchers
	// its participants; thread the participants of that thread, "" for none.
	authors      bool
	addedAuthors []Reviewer
	watchers     bool
	thread       string
}

// routingFor is the routing of one record about a change, read in the
// deciding snapshot tx; nil where the op fills no role (this file's head). THE
// ONE FUNCTION every writer of a change's records stamps by, so the committer
// and the change writer never disagree. Each role is read in its key's order
// and bounded ([MaxRoutedParticipants]); WorkingFor is stamped as
// [ChangeRouting.WorkingFor] says. The result is held to [MaxRoutingBytes],
// which those bounds keep it far inside: past it is a writer's bug, never a
// refusal.
func routingFor(ctx context.Context, tx *sql.Tx, c concerning) (*ChangeRouting, error) {
	row := c.change
	r := &ChangeRouting{ChangeID: row.id, ChangeRef: ChangeRef(c.repoName, row.number), RepoName: c.repoName,
		Title: row.title, Actor: c.actor, Owner: row.owner, OwnerKind: row.ownerKind, Verdict: c.verdict,
		Conflicted: c.conflicted, Merged: c.merged, ChecksFailed: c.checksFailed, Broken: c.broken,
		Dequeued: c.dequeued, NotClosed: c.notClosed}
	if row.work != (types.WorkItem{}) {
		work := row.work
		r.WorkItem = &work
	}
	persons := map[string]bool{}
	note := func(handle string, kind PartyKind) string {
		if kind == PartyPerson {
			persons[handle] = true
		}
		return handle
	}
	if row.owner != "" {
		note(row.owner, row.ownerKind)
	}
	if c.actor != "" {
		note(c.actor, c.actorKind)
	}
	mentions := c.mentions
	for _, text := range c.mentionText {
		mentions = append(mentions, notify.Mentions(text)...)
	}
	for _, m := range mentions {
		if c.roster == nil || slices.Contains(r.Mentions, m) || len(r.Mentions) >= MaxMentions {
			continue
		}
		// HELD TO THE DECIDING CHART: a name nobody holds would be a notice
		// nobody can receive.
		seat, ok := c.roster.Seat(m)
		if !ok {
			continue
		}
		kind := PartyAgent
		if seat.Human {
			kind = PartyPerson
		}
		r.Mentions = append(r.Mentions, note(m, kind))
	}
	parties := func(list []Reviewer) []string {
		var out []string
		for _, p := range list {
			if p.Handle != "" && !slices.Contains(out, p.Handle) {
				out = append(out, note(p.Handle, p.Kind))
			}
		}
		return out
	}
	r.Requested, r.PathOwners, r.ReReview = parties(c.requested), parties(c.pathOwners), parties(c.reReview)
	r.Raisers, r.ReadyFor = parties(c.raisers), parties(c.readyFor)
	if c.lead.Handle != "" {
		r.LeadFallback = note(c.lead.Handle, c.lead.Kind)
	}
	if c.enqueuer.Handle != "" {
		r.Enqueuer = note(c.enqueuer.Handle, c.enqueuer.Kind)
	}
	if c.ownerLead.Handle != "" {
		r.Lead = note(c.ownerLead.Handle, c.ownerLead.Kind)
	}
	if c.authors {
		authors, err := partiesOf(ctx, tx, routingAuthorsSQL, row.id)
		if err != nil {
			return nil, fmt.Errorf("gitlet: read change %s's authors: %w", row.id, err)
		}
		for _, a := range c.addedAuthors {
			if len(authors) < MaxRoutedParticipants && !slices.ContainsFunc(authors, func(p Reviewer) bool {
				return p.Handle == a.Handle
			}) {
				authors = append(authors, a)
			}
		}
		r.Authors = parties(authors)
	}
	if c.watchers {
		watchers, err := partiesOf(ctx, tx, routingWatchersSQL, row.id)
		if err != nil {
			return nil, fmt.Errorf("gitlet: read change %s's participants: %w", row.id, err)
		}
		r.Watchers = parties(watchers)
	}
	if c.thread != "" {
		said, err := threadParticipants(ctx, tx, row.id, c.thread)
		if err != nil {
			return nil, err
		}
		r.ThreadParticipants = parties(said)
	}
	if len(Concerns([]*ChangeRouting{r})) == 0 {
		// NOBODY TO TELL: no routing at all (this file's head).
		return nil, nil
	}
	for handle := range persons {
		r.Persons = append(r.Persons, handle)
	}
	slices.Sort(r.Persons)
	// WorkingFor ([ChangeRouting.WorkingFor]): after the persons are named,
	// since a person's handle wakes nobody, and before the routing is
	// measured, which counts it.
	carrier := c.workingFor
	switch {
	case c.actor == "":
		carrier = c.enqueuedFor
	case c.actorKind != PartyAgent:
		carrier = nil
	}
	if carrier != nil && wakesASeat(Concerns([]*ChangeRouting{r})) {
		r.WorkingFor = carrier.Clone()
	}
	encoded, err := json.Marshal(r)
	if err != nil {
		return nil, fmt.Errorf("gitlet: encode change %s's routing: %w", row.id, err)
	}
	if len(encoded) > MaxRoutingBytes {
		return nil, fmt.Errorf("gitlet: change %s's routing is %d bytes, past MaxRoutingBytes (%d) — the bounds of "+
			"its roles keep it from forming", row.id, len(encoded), MaxRoutingBytes)
	}
	return r, nil
}

// partiesOf is the parties one routing statement answers for a change — at
// most MaxRoutedParticipants, read in its key's order.
func partiesOf(ctx context.Context, tx *sql.Tx, query, changeID string) ([]Reviewer, error) {
	rows, err := tx.QueryContext(ctx, query, changeID, MaxRoutedParticipants)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []Reviewer
	for rows.Next() {
		var p Reviewer
		var kind string
		if err := rows.Scan(&p.Handle, &kind); err != nil {
			return nil, err
		}
		p.Kind = PartyKind(kind)
		out = append(out, p)
	}
	return out, rows.Err()
}

// threadParticipants is everybody who said something in thread on changeID —
// its root's author first, then the authors of its newest comments, each
// once, at most MaxRoutedParticipants. Only the newest [threadParticipantRows]
// are read, back from the thread's end by its index, so a comment's decide
// costs its audience, never its thread's history.
func threadParticipants(ctx context.Context, tx *sql.Tx, changeID, thread string) ([]Reviewer, error) {
	var out []Reviewer
	add := func(p Reviewer) {
		if len(out) < MaxRoutedParticipants && !slices.ContainsFunc(out, func(q Reviewer) bool {
			return q.Handle == p.Handle
		}) {
			out = append(out, p)
		}
	}
	root, found, err := commentIn(ctx, tx, thread)
	if err != nil {
		return nil, err
	}
	if found && root.changeID == changeID {
		add(Reviewer{Handle: root.author, Kind: root.authorKind})
	}
	rows, err := tx.QueryContext(ctx, threadParticipantsSQL, changeID, thread, threadParticipantRows)
	if err != nil {
		return nil, fmt.Errorf("gitlet: read who talks in thread %s of %s: %w", thread, changeID, err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var p Reviewer
		var kind string
		if err := rows.Scan(&p.Handle, &kind); err != nil {
			return nil, fmt.Errorf("gitlet: read who talks in thread %s of %s: %w", thread, changeID, err)
		}
		p.Kind = PartyKind(kind)
		add(p)
	}
	return out, rows.Err()
}

// threadParticipantRows is how many of a thread's newest comments name who
// takes part in it: FOUR PER PARTICIPANT a routing names
// ([MaxRoutedParticipants]), so a thread of sixty-four people each answering
// in turn names them all, and a thread two people ran to thousands of
// comments costs a comment's decide 256 index entries rather than its history.
const threadParticipantRows = 4 * MaxRoutedParticipants

// mentionsProblem is why mentions is no list a write may name, or "": at most
// [MaxMentions] handles, each a seat's handle, each once.
func mentionsProblem(mentions []string) string {
	if len(mentions) > MaxMentions {
		return fmt.Sprintf("%d handles, past the %d one write names (MaxMentions)", len(mentions), MaxMentions)
	}
	for i, m := range mentions {
		if why := handleProblem(m); why != "" {
			return fmt.Sprintf("[%d]: %s", i, why)
		}
		if slices.Contains(mentions[:i], m) {
			return fmt.Sprintf("[%d]: %q is named twice", i, m)
		}
	}
	return ""
}

// stampedRouting refuses a routing an asker set: the deciding node stamps it
// from its own snapshot and chart.
func stampedRouting(what string, r *ChangeRouting) error {
	if r != nil {
		return badField(what+".routing", "the deciding node stamps whom a record concerns from its snapshot and "+
			"its chart, and an asker sends none")
	}
	return nil
}

// revisionPusher is who pushed revision n of changeID, as a routing's actor
// names them — the opener for revision 1, whoever moved the head for every
// later one — and whom they pushed it for, as the revision's row keeps it
// (code_revisions.working_for, [storedCarrier]): nil for a revision made for
// nobody, a person's, and one applied before the carrier's build. "" and nil
// where tx holds no such revision.
func revisionPusher(ctx context.Context, tx *sql.Tx, changeID string, n int64) (string, PartyKind,
	*types.WorkingFor, error) {

	var pusher, kind string
	var madeFor sql.NullString
	switch err := tx.QueryRowContext(ctx, headPusherSQL, changeID, n).Scan(&pusher, &kind, &madeFor); {
	case errors.Is(err, sql.ErrNoRows):
		return "", "", nil, nil
	case err != nil:
		return "", "", nil, fmt.Errorf("gitlet: read who pushed revision %d of %s: %w", n, changeID, err)
	}
	return pusher, PartyKind(kind), storedCarrier(madeFor), nil
}

// landingPusher is who merged changeID of repoID, landed by its home (l), and
// whom they pushed for (code_pushes.working_for, [storedCarrier]): the
// principal of the push l names, held to the rows: an applied command setting
// l's target to a commit, at or after the record of l's revision and at or
// before moved, the target's last move. "" and nil where l names no such push,
// its row is gone, or its principal names no party.
//
// WHY THE PUSH THE HOME NAMES, never the target's last mover: the target may
// move again after the merge (a push on top, in the same group-commit record
// or a later one), and that pusher merged nothing. Telling which move merged
// takes git (one commit holding another), which the home runs and a decide
// never does.
func landingPusher(ctx context.Context, tx *sql.Tx, repoID, changeID string, l ChangeLanded, moved int64) (string,
	PartyKind, *types.WorkingFor, error) {

	pushed := l.PushPosition.Packed()
	if l.PushID == "" || pushed > moved {
		return "", "", nil, nil
	}
	var revised int64
	switch err := tx.QueryRowContext(ctx, revisionPositionSQL, changeID, l.Revision).Scan(&revised); {
	case errors.Is(err, sql.ErrNoRows):
		return "", "", nil, nil
	case err != nil:
		return "", "", nil, fmt.Errorf("gitlet: read where revision %d of %s was pushed: %w", l.Revision, changeID,
			err)
	}
	if pushed < revised {
		return "", "", nil, nil
	}
	var actorKind, seat, encoded string
	var madeFor sql.NullString
	switch err := tx.QueryRowContext(ctx, landingPushSQL, l.PushID, repoID, pushed).Scan(&actorKind, &seat,
		&encoded, &madeFor); {
	case errors.Is(err, sql.ErrNoRows):
		return "", "", nil, nil
	case err != nil:
		return "", "", nil, fmt.Errorf("gitlet: read push %s of repository %s, which merged %s: %w", l.PushID,
			repoID, changeID, err)
	}
	var cmds []Command
	if err := json.Unmarshal([]byte(encoded), &cmds); err != nil {
		return "", "", nil, fmt.Errorf("gitlet: decode push %s's commands of repository %s: %w", l.PushID, repoID,
			err)
	}
	if !slices.ContainsFunc(cmds, func(c Command) bool {
		return c.Ref == l.TargetRef && c.Result == ResultOK && c.New != ZeroOID
	}) {
		return "", "", nil, nil
	}
	who, kind := Principal{Kind: PrincipalKind(actorKind), Seat: seat}.party()
	if who == "" {
		return "", "", nil, nil
	}
	return who, kind, storedCarrier(madeFor), nil
}

// The routing's reads, each registered in plan_test.go: a change's authors and
// its participants, a range of each key in its order; and a thread's newest
// comments, a range of code_comments_thread_idx read back from its end.
const (
	routingAuthorsSQL = `SELECT author, author_kind FROM code_change_authors WHERE change_id = ?
		ORDER BY author LIMIT ?`
	routingWatchersSQL = `SELECT handle, kind FROM code_participants WHERE change_id = ?
		ORDER BY handle LIMIT ?`
	// headPusherSQL is who pushed a change's revision, and whom they pushed it
	// for — the actor of a failed check of its head and of the revision's
	// facts, and whom it acted for ([revisionPusher]) — one seek of the
	// revisions' key, the carrier read off the row the seek finds.
	headPusherSQL = `SELECT pusher, pusher_kind, working_for FROM code_revisions WHERE change_id = ? AND n = ?`
	// revisionPositionSQL is where a change's revision was pushed, the oldest
	// record a push merging it can be in ([landingPusher]); landingPushSQL is
	// the push a landing names as its merger, held to the repository and the
	// position named so another record's or repository's push is not it. Each
	// one seek of its table's key.
	revisionPositionSQL = `SELECT position FROM code_revisions WHERE change_id = ? AND n = ?`
	landingPushSQL      = `SELECT actor_kind, actor_seat, commands, working_for FROM code_pushes
		WHERE push_id = ? AND repo_id = ? AND position = ?`
	threadParticipantsSQL = `SELECT author, author_kind FROM code_comments INDEXED BY code_comments_thread_idx
		WHERE change_id = ? AND thread_id = ? ORDER BY position DESC LIMIT ?`
)

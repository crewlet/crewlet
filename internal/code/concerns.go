package code

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"slices"

	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/notify"
	"github.com/crewlet/crewlet/internal/routing"
)

// WHOM A CHANGE RECORD CONCERNS: every record about a change — a write of its
// lifecycle on the repository's subject, a push that merges or opens one, every
// write of its conversation on the change's own — carries a ROUTING SNAPSHOT
// ([ChangeRouting]): who made it, and the handles each reason it routes under
// reaches, as the deciding snapshot and chart held them. The writer stamps it,
// through ONE function ([routingFor]) whichever writer decides the record — the
// committer for the repository's records, the change writer for the change's —
// and everything that tells somebody about the record afterwards reads the
// snapshot and nothing else: [Concerns] is pure over it, through the one
// precedence rule every native domain routes by (internal/routing).
//
// # Why a snapshot, and not the rows
//
// Who a record concerns is a fact of the moment it was decided. The rows move
// on — a reviewer is withdrawn, an author joins by pushing, a thread grows —
// and a reader that derived the audience from the rows minutes later, on
// another node, would tell different people about the same record than the
// node that applied it. The record carries the answer, so every reader of it
// agrees: the applier that writes a notice row per recipient, and the change
// feed that wakes the seats among them.
//
// # One role per reason, filled for the reasons the op routes by
//
// A record fills only the roles its op routes by — a close its watchers, a
// review its authors and its mentions, a revise the path owners and the
// reviewers it asks to look again — never every role of the change on every
// record: a comment that carried the change's sixty-four watchers beside its
// own audience would pay for a fan-out nobody reads, on every node's log.
//
//   - mention: the handles the write's text names, held to the deciding chart
//     (an open, an edit, a review, a comment; a push's options).
//   - review_requested: the reviewers an ask names that were not asked
//     already, and the path owners a revision's facts ask.
//   - changes_requested, review_submitted: the change's authors, by the
//     review's verdict.
//   - checks_failed: the authors and the record's actor — the pusher of the
//     head the check measured — on the ending of a check of the change's
//     head that failed, or errored past the home's own requeue: the one
//     reason that is news to its actor too ([Reason.WakesActor]), since a
//     pusher learns that their commit fails its checks only from this.
//   - target_broken: the authors, the enqueuer and the effective lead of the
//     repository's owner, on the broken record naming the landing that
//     merged the change.
//   - conflicted: the authors, on the mergeability that TURNED the change's
//     merge into a conflict — not on every recheck that finds it still one —
//     and on a queue's ejection of it for a conflicting merge.
//   - dequeued: the authors and the enqueuer, on a record that put the
//     change's entry out of its target's queue — an ejection, or a dequeue
//     somebody asked.
//   - finding_replied: the raiser of the finding a comment replies to, and of
//     every finding an answer answers.
//   - ready_to_merge: the change's authors who may merge it into its target
//     ([PreconditionMergeAuthority] judged for each), else the effective
//     lead of the repository's owner, on the mergeability whose stored
//     answer goes from not ready to ready (below).
//   - merged: the authors, on the push that set the target to the change's
//     head and on its home's landing, on the merge record that merged it and
//     on the queue's landing of it — each but who merged it.
//   - re_review: the reviewers whose standing verdict asks for changes on an
//     earlier revision, on a revision's first facts.
//   - thread: the participants of the thread a comment is said in.
//   - watcher: the change's participants, on a close, a reopen, a retarget
//     and a draft's state.
//   - lead_fallback: the lead of the repository's owner, on a change's first
//     facts where nobody who may review it is asked.
//
// A push that moves no change concerns nobody: a head move, a close by a
// deleted source branch and a push to a ref no change uses are bookkeeping —
// what a change's page says, never what a wake is for.
//
// # ready_to_merge is announced by ONE record, and why it is that one
//
// Whether a change may merge is judged from one snapshot of the change's rows
// ([ReadinessOf]), and the inputs live on TWO subjects: a draft's state and a
// revision on the repository's, an approval and a blocker's answer on the
// change's. A record on either that computed readiness from its own snapshot
// would miss the other: an approval and an undraft decided at once each see
// the other not yet applied, each finds the change not ready, and the
// transition both complete is announced by neither — write skew across two
// subjects. So the transition is the stored answer's: a mergeability record —
// the home's merge or its refresh, which follows any readiness input that
// moved — stores the change's readiness, and the record whose decide finds
// the stored `ready` going false to true is the one that announces it.
//
// # The actor
//
// Every record of this domain names the deciding NODE as its actor
// ([ActorSystem]): the principal is in its payload. So the routing names who
// acted ([ChangeRouting.Actor]) — the asker, the pusher of a push's entry, the
// enqueuer of an entry a queue landed, the pusher of the head a check
// measured, nobody for the home's own records — and a code reason is news to
// whoever made the change only where [Reason.WakesActor] says so: a reviewer
// is not told of their own review, a pusher of their own merge, and a pusher
// IS told that the head they pushed fails its checks.
//
// # Who hears what the gate does
//
// The gate's records — a merge, a dequeue, a queue's ejection and landing, a
// broken landing and a head check's ending — carry a routing as every other
// record about a change does, each stamped by the record's own writer through
// [routingFor]: the committer for the repository's, the checks writer for an
// ending. A record of several changes (an ejection, a landing) carries one a
// change, KEPT WITHIN THE RECORD'S ROOM ([keptRoutings]) as a push's are: the
// record must land, and a change past the room is told to nobody. A landing
// no change made — a person's push onto a protected branch — concerns no
// change's audience, and its broken record carries no routing: the
// `code_target_broken` event on the activity feed is what reports it.
//
// # The record format
//
// Under the code domain's stated condition (no build that carries the domain
// has run without these fields — none of it is on main): every routing field
// below rides the one field each payload gains, and takes no row of its own.
// Once a build without them has run, each op's payload takes {Name:
// "<Payload>.Routing", Since: 2, Op: "<op>", Path: []string{"mutation",
// "routing"}} in versionedFields — a push's {Name: "PushEntry.Routing", Since:
// 2, Op: "push", Path: []string{"mutation", "pushes", "routing"}} — and an
// open's, an edit's, a review's and a comment's mentions {Name:
// "<Payload>.Mentions", Since: 2, Op: "<op>", Path: []string{"mutation",
// "mentions"}}, with RecordVersion at 2, its line under "What each version
// added" and a Carrying record in the statelogtest candidate: such a build
// would apply the record and tell nobody, where it must retain it.

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

// Reasons is the fifteen, IN PRECEDENCE ORDER: the first reason that names a
// handle is the one that handle hears under, and [Concerns] OFFERS them in
// exactly this order — the list is the offer order, so the precedence is
// written once. WHAT THIS RECORD DID TO YOU OUTRANKS THE ROLE YOU HOLD: being
// named comes first, then being asked, then the change's state turning under
// you, then what you hold (a review you gave, a thread you are in, a change you
// watch), and the lead's copy last.
//
// The gate's own sit among them by the same rule: a failed check of your head
// right after a request for changes — both say your change must change — and a
// target your landing broke right after it, before a conflict, since a broken
// branch stops everybody merging into it; a removal from the queue after a
// conflict, which says why the change left it.
//
// BASE FORMAT, AS A STATED CONDITION: a notice row's reason is read back by
// [Reason.Valid], and checks_failed, target_broken and dequeued are values an
// older reader of code_notifications does not know. None of the code domain is
// on main, so no build that carries it has run without them; once one has,
// each record that produces one takes its routing's versionedFields row
// (this file's head), which keeps such a build from applying the record.
func Reasons() []Reason {
	return []Reason{ReasonMention, ReasonReviewRequested, ReasonChangesRequested, ReasonChecksFailed,
		ReasonTargetBroken, ReasonConflicted, ReasonDequeued, ReasonFindingReplied, ReasonReadyToMerge,
		ReasonMerged, ReasonReviewSubmitted, ReasonReReview, ReasonThread, ReasonWatcher, ReasonLeadFallback}
}

// Valid reports a reason this build knows.
func (r Reason) Valid() bool { return slices.Contains(Reasons(), r) }

// Addressed reports whether a notice under r ASKS its recipient something — a
// turn that must answer rather than absorb: being named, being asked to
// review, a request for changes, a failed check of your head, a target your
// landing broke, a conflict, a removal from the queue, a reply to a finding
// raised, a change ready to merge, and a request to look again. Everything
// after them is news to absorb. For which reasons lead a PERSON's inbox, see
// [DefaultPrimaryReasons].
func (r Reason) Addressed() bool {
	switch r {
	case ReasonMention, ReasonReviewRequested, ReasonChangesRequested, ReasonChecksFailed, ReasonTargetBroken,
		ReasonConflicted, ReasonDequeued, ReasonFindingReplied, ReasonReadyToMerge, ReasonReReview:
		return true
	}
	return false
}

// WakesActor reports whether a notice under r reaches the record's own actor
// too: only checks_failed. A pushed head that fails its checks is news to its
// pusher above all — nothing else they did tells them — as a failed pipeline
// is to whoever pushed on GitHub and GitLab (their prompts' WakesActor).
// Every other reason is about something the actor did and knows.
func (r Reason) WakesActor() bool { return r == ReasonChecksFailed }

// DefaultPrimaryReasons is the code reasons that lead the inbox of a person who
// has expressed no preference.
//
// THE ADDRESSED ONES PLUS WHAT HAPPENED TO YOUR OWN CHANGE: a change of yours
// merged, or reviewed, changes what you do next as surely as an ask does, and
// asks nothing. A thread you are in, a change you watch and a lead's copy are
// context, and context that competes with action turns an inbox into a feed
// nobody reads. Deliberately NOT [Reason.Addressed]'s set — the two questions
// differ, as the tracker's own two lists do.
var DefaultPrimaryReasons = []Reason{
	ReasonMention, ReasonReviewRequested, ReasonChangesRequested, ReasonChecksFailed, ReasonTargetBroken,
	ReasonConflicted, ReasonDequeued, ReasonFindingReplied, ReasonReadyToMerge, ReasonMerged,
	ReasonReviewSubmitted, ReasonReReview,
}

// ChangeRouting is whom one record about a change concerns, as the writer's
// snapshot and chart held them — see this file's head. A handle is a seat's,
// or the human seat a person holds; [ChangeRouting.Persons] says which.
type ChangeRouting struct {
	// ChangeID is the change, and ChangeRef its name as the snapshot held it
	// (web!42) — an open's, the number its apply will give.
	ChangeID  string `json:"change_id"`
	ChangeRef string `json:"change_ref"`
	// RepoName and Title are the repository's name and the change's title as
	// the record leaves them.
	RepoName string `json:"repo_name"`
	Title    string `json:"title"`
	// Actor is who made the change the record records — its asker, a push
	// entry's pusher — and "" for the home's own records, which nobody
	// asked. Nobody hears of a change they made.
	Actor string `json:"actor,omitempty"`
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
	// ChecksFailed is a check of the change's head that failed, or errored
	// past the home's own requeue: its authors and its actor hear.
	ChecksFailed bool `json:"checks_failed,omitempty"`
	// Broken is a broken record naming the landing that merged the change:
	// its authors, Enqueuer and Lead hear. Dequeued is a record that put the
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
			return withOnce(r.Authors, r.Enqueuer, r.Lead)
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
		if r.Merged {
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
func (p Party) reviewer() Reviewer { return Reviewer{Handle: p.Handle, Kind: p.Kind} }

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

// Concerns is every handle routings concern, in precedence order — one record's
// routings, which a push record carries one of for every change it merges or
// opens, and every other record one of. PURE OVER THE ROUTINGS: it reads no
// row, no chart and no clock, so the applier that writes a notice row per
// concern and the feed that wakes the seats among them, on any node and at any
// time, derive one set.
//
// THE REASONS ARE OFFERED IN [Reasons]' ORDER, every routing's handles for one
// reason before any for the next, so a handle a record names under two
// reasons, about two changes, hears under the stronger — internal/routing's
// add-once rule across the whole record, which a notice row keyed on the
// record and the recipient needs: a record reaches one handle once.
func Concerns(routings []*ChangeRouting) []Concern {
	var (
		b   routing.Builder[Reason]
		out []Concern
	)
	for _, reason := range Reasons() {
		for _, r := range routings {
			if r == nil {
				continue
			}
			handles := r.heard(reason)
			if len(handles) == 0 {
				continue
			}
			before := len(b.Candidates())
			if reason == ReasonLeadFallback {
				b.Fallback(reason, handles...)
			} else {
				b.AddAll(handles, reason, reason.Addressed())
			}
			for _, c := range b.Candidates()[before:] {
				out = append(out, Concern{Candidate: c, Change: r})
			}
		}
	}
	return out
}

// Hears reports whether c is told: everybody but who made the change, except
// under the one reason that is news to its own actor ([Reason.WakesActor]).
func (c Concern) Hears() bool { return c.Candidate.Hears(c.Change.Actor, Reason.WakesActor) }

// heardCount is how many handles routings tell — the notice rows their
// record's apply may write ([Applier.writeNotices]), which every decide
// counts with the record's rows.
func heardCount(routings ...*ChangeRouting) int {
	n := 0
	for _, c := range Concerns(routings) {
		if c.Hears() {
			n++
		}
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

// pushRoutings is the routings a push record carries, one for each change its
// entries merge or open, each pointing into the record's own entries. A
// function of its own so the batch is a typed parameter where its routings
// are handed to append: inline, a type switch's variable is a value
// internal/sourcetree/decltype cannot type, and the code cache's writers gate
// (TestNothingButSyncWritesACache) refuses one handed to a call it cannot type
// either, append among them, since it cannot tell it from a gitproc.Cmd.
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
		return 0, fmt.Errorf("code: measure a record's payload: %w", err)
	}
	return MaxRecordBytes - recordOverheadBytes - len(encoded), nil
}

// keptRoutings is routings, in their order, as many as fit within room bytes
// of their record and rows notice rows — this file's head: a record of
// several changes lands whatever it would tell, and a change past the room is
// told to nobody, as a push's leading entry keeps what fits ([routingRoom]).
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

// withRouting is payload with r stamped on it — every payload of a change's
// records takes one ([RoutingsOf]).
func withRouting(payload any, r *ChangeRouting) any {
	switch p := payload.(type) {
	case ChangeOpen:
		p.Routing = r
		return p
	case ChangeEdit:
		p.Routing = r
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
		p.Routing = r
		return p
	case ThreadResolve:
		p.Routing = r
		return p
	case Review:
		p.Routing = r
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
	panic(fmt.Sprintf("code: a %T carries no routing — every record about a change does (RoutingsOf)", payload))
}

// concerning is what a record's decide hands [routingFor]: the change as the
// record leaves it, who made it, and the facts of the record's own op — each
// role filled only where the op routes by it (this file's head).
type concerning struct {
	repoName string
	// change is the change's row as the record leaves it: its id, number,
	// title, owner and work item.
	change changeRow
	// actor is who made the change, "" for the home's own records, and
	// actorKind what it is: a person's actor is named among the routing's
	// persons, so a wake says a person wrote what it announces.
	actor     string
	actorKind PartyKind
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

// routingFor is the routing of one record about a change, read in the deciding
// snapshot tx — THE ONE FUNCTION every writer of a change's records stamps its
// routing by, so the committer and the change writer never disagree about whom
// a record concerns. Each role is read in its key's order and bounded
// ([MaxRoutedParticipants]); the result is held to [MaxRoutingBytes], which
// those bounds keep it far inside — past it is a writer's bug, never a
// refusal.
func routingFor(ctx context.Context, tx *sql.Tx, c concerning) (*ChangeRouting, error) {
	row := c.change
	r := &ChangeRouting{ChangeID: row.id, ChangeRef: ChangeRef(c.repoName, row.number), RepoName: c.repoName,
		Title: row.title, Actor: c.actor, Owner: row.owner, OwnerKind: row.ownerKind, Verdict: c.verdict,
		Conflicted: c.conflicted, Merged: c.merged, ChecksFailed: c.checksFailed, Broken: c.broken,
		Dequeued: c.dequeued}
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
		// HELD TO THE DECIDING CHART: a name nobody holds is no seat to
		// tell, and a mention past the chart would be a notice nobody can
		// receive.
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
			return nil, fmt.Errorf("code: read change %s's authors: %w", row.id, err)
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
			return nil, fmt.Errorf("code: read change %s's participants: %w", row.id, err)
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
	for handle := range persons {
		r.Persons = append(r.Persons, handle)
	}
	slices.Sort(r.Persons)
	encoded, err := json.Marshal(r)
	if err != nil {
		return nil, fmt.Errorf("code: encode change %s's routing: %w", row.id, err)
	}
	if len(encoded) > MaxRoutingBytes {
		return nil, fmt.Errorf("code: change %s's routing is %d bytes, past MaxRoutingBytes (%d) — the bounds of "+
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
// once, at most MaxRoutedParticipants. THE NEWEST, read back from the end of
// the thread by its index, at most [threadParticipantRows] of them: a thread
// that ran to thousands of comments is read no deeper than the people who
// still talk in it, so a comment's decide costs what its audience is, never
// what its thread's history is.
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
		return nil, fmt.Errorf("code: read who talks in thread %s of %s: %w", thread, changeID, err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var p Reviewer
		var kind string
		if err := rows.Scan(&p.Handle, &kind); err != nil {
			return nil, fmt.Errorf("code: read who talks in thread %s of %s: %w", thread, changeID, err)
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

// The routing's reads, each registered in plan_test.go: a change's authors and
// its participants, a range of each key in its order; and a thread's newest
// comments, a range of code_comments_thread_idx read back from its end.
const (
	routingAuthorsSQL = `SELECT author, author_kind FROM code_change_authors WHERE change_id = ?
		ORDER BY author LIMIT ?`
	routingWatchersSQL = `SELECT handle, kind FROM code_participants WHERE change_id = ?
		ORDER BY handle LIMIT ?`
	// headPusherSQL is who pushed a change's revision — the actor of a failed
	// check of its head — one seek of the revisions' key.
	headPusherSQL         = `SELECT pusher, pusher_kind FROM code_revisions WHERE change_id = ? AND n = ?`
	threadParticipantsSQL = `SELECT author, author_kind FROM code_comments INDEXED BY code_comments_thread_idx
		WHERE change_id = ? AND thread_id = ? ORDER BY position DESC LIMIT ?`
)

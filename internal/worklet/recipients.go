package worklet

import (
	"slices"

	"github.com/crewlet/crewlet/internal/routing"
)

// Who hears about a change, and why the answer is TWO functions.
//
// [Candidates] is pure over the notification (kind, mentions, routing
// snapshot; no clock, org, registry or actor), so the applier can call it
// inside its transaction and the wake path on another node later, and get the
// same answer. [Route] is the post-filter needing what a record cannot carry:
// who the company employs NOW. The applier also takes Route's actor step
// ([routing.Candidate.Hears]), so `tracker_notifications` records everyone the
// change concerned but its author, the wake reaches only those still there, and
// the parity test can assert the two agree. The shared rule is
// internal/routing's; what is the tracker's own here is its reasons, their
// precedence, the one that reaches the actor, and [Reason.StartsTurn].

// Reason is why one handle hears about a change.
type Reason string

// The nineteen reasons, as they are written onto the log and into
// `tracker_notifications`.
//
// This order is NOT the precedence; [Reasons] is. Do not re-sort these to
// match it: two orderings in one file read as one.
const (
	ReasonMention        Reason = "mention"
	ReasonPrioritised    Reason = "prioritised"
	ReasonAssignee       Reason = "assignee"
	ReasonUnassigned     Reason = "unassigned"
	ReasonReporter       Reason = "reporter"
	ReasonAsked          Reason = "asked"
	ReasonAnswered       Reason = "answered"
	ReasonThread         Reason = "thread"
	ReasonUnblocked      Reason = "unblocked"
	ReasonBlocking       Reason = "blocking"
	ReasonRoutedTo       Reason = "routed_to"
	ReasonParentAssignee Reason = "parent_assignee"
	ReasonChecklist      Reason = "checklist"
	ReasonCollaborator   Reason = "collaborator"
	ReasonWatcher        Reason = "watcher"
	ReasonUnwatched      Reason = "unwatched"
	ReasonPurged         Reason = "purged"
	ReasonLeadFallback   Reason = "lead_fallback"
	// ReasonFlow is a notice that an item is not moving, to the one person
	// the flow duty tells (ADR-0162).
	ReasonFlow Reason = "flow"
)

// Reasons are the nineteen, IN PRECEDENCE ORDER: a handle hears under the
// first reason that names it, and later ones are dropped (mentioned AND
// watching is told "mentioned"). `blocking`, `asked` and `answered` precede
// `assignee` because each says what THIS COMMIT did to the recipient, and the
// assignee is the person they most often name; behind it `blocking` could name
// nobody. The offer order in [Candidates] must match;
// TestEveryKindOffersItsReasonsInTheDeclaredPrecedence holds the two together.
var Reasons = []Reason{
	ReasonMention, ReasonPrioritised, ReasonBlocking, ReasonAsked,
	ReasonAnswered, ReasonAssignee, ReasonUnassigned, ReasonReporter,
	ReasonThread, ReasonFlow, ReasonUnblocked, ReasonRoutedTo, ReasonParentAssignee,
	ReasonChecklist, ReasonCollaborator,
	ReasonWatcher, ReasonUnwatched, ReasonPurged, ReasonLeadFallback,
}

// Valid reports whether a reason off the wire is one this build knows.
func (r Reason) Valid() bool { return slices.Contains(Reasons, r) }

// addressedReasons is the eight under which a wake OBLIGES ITS SEAT TO ANSWER:
// somebody is waiting on you, not "important" (`routed_to` and a watcher are
// facts to absorb). It is NOT the inbox's first band, [DefaultPrimaryReasons],
// which differs on four reasons by design: `unassigned` and `unblocked` lead
// an inbox but ask no answer; `thread` and `blocking` ask one but should not
// lead an inbox.
var addressedReasons = []Reason{
	ReasonMention, ReasonAsked, ReasonAssignee, ReasonReporter,
	ReasonAnswered, ReasonPrioritised, ReasonThread, ReasonBlocking,
}

// Addressed reports whether a wake under this reason obliges its seat to
// answer. For which reasons lead a PERSON's inbox, see [DefaultPrimaryReasons].
func (r Reason) Addressed() bool { return slices.Contains(addressedReasons, r) }

// heldReasons are the reasons a seat hears under because it holds the task the
// wake is about, as its assignee: `assignee`; `blocking`, written on the
// blocker for its assignee; and `unblocked`, written on the dependent for its
// assignee. Such a wake asks nothing of a seat that no longer holds the task
// open, however late it arrives.
var heldReasons = []Reason{ReasonAssignee, ReasonBlocking, ReasonUnblocked}

// Held reports whether a wake under this reason reaches its seat because the
// seat holds the task the wake is about ([heldReasons]).
func (r Reason) Held() bool { return slices.Contains(heldReasons, r) }

// absorbableReasons are the reasons whose STATE NOTICE ([StateNotice]) asks
// its seat for nothing but the item it is about: the work the pickup owns, so
// a completed turn on the item discharges it (ADR-0162), and `flow`, whose
// lead a turn on the item answers. Its own question, distinct from
// [Reason.Addressed], which keeps its meaning for prompts and the working
// indicator: may a turn that read past the notice absorb it.
var absorbableReasons = []Reason{ReasonAssignee, ReasonBlocking, ReasonUnblocked, ReasonFlow}

// Absorbable reports whether a state notice under this reason is absorbed
// past its seat's seen-through mark for the item ([absorbableReasons]).
func (r Reason) Absorbable() bool { return slices.Contains(absorbableReasons, r) }

// turnReasons are the reasons whose notice starts a turn whatever the change
// was: each puts something to this seat (`mention`, `prioritised`, `asked`),
// replies to its ask (`answered`), moves work it owns (`unblocked`,
// `routed_to`, `parent_assignee`, `checklist`, `blocking`), says nobody is
// moving an item (`flow`), or takes work out of the lead's project
// (`purged`). A third question, distinct from
// [Reason.Addressed] and [DefaultPrimaryReasons]: is it worth a turn at all.
var turnReasons = []Reason{
	ReasonMention, ReasonPrioritised, ReasonAsked, ReasonAnswered,
	ReasonUnblocked, ReasonRoutedTo, ReasonParentAssignee,
	ReasonChecklist, ReasonBlocking, ReasonPurged, ReasonFlow,
}

// ownerReasons start a turn exactly when the change asks something of the
// task's OWNER ([Notify.ownerAsked]): assignee, collaborators, the lead heard
// in the owner's place ([ReasonLeadFallback], never for the owner's own change,
// [Notify.asks]), and the reporter, who verifies the outcome. One rule for all
// four, because each answers for the outcome;
// [Candidates] adds the reporter only for the changes that concern it, so
// another agent's note on work it handed over never wakes it.
var ownerReasons = []Reason{
	ReasonAssignee, ReasonCollaborator, ReasonLeadFallback, ReasonReporter,
}

// StartsTurn reports whether a notice under this reason wakes its seat into a
// turn, given whether the change asks something of that seat ([Notify.asks]).
// Every other notice is still delivered as an inbox row. The line is measured:
// a turn woken only by another agent's comment on a watched task changed the
// work in 4 of 1,011, while a status move by somebody else changed its owner's
// work 82% of the time. `thread` is decided per handle in [Candidates]
// (ADR-0113); `asked` is that answer, since agents in one thread otherwise
// wake each other to acknowledge acknowledgements.
func (r Reason) StartsTurn(asked bool) bool {
	switch {
	case slices.Contains(turnReasons, r):
		return true
	case slices.Contains(ownerReasons, r), r == ReasonWatcher, r == ReasonThread:
		return asked
	}
	return false
}

// WakesActor reports the one reason that reaches the person who made the
// change: that your task became workable is news whoever wrote it, today only
// the engine's [Writer.TellUnblocked]. Every other reason is dropped for the
// actor: telling somebody what they just did is noise a model spends a round on.
func (r Reason) WakesActor() bool { return r == ReasonUnblocked }

// Candidates is every handle a change concerns, in precedence order.
//
// It returns nothing for a nil notification, which keeps the two surfaces in
// step: the translator acks a quiet commit unrouted, and the applier calling
// this same function writes no notification row for it either.
func Candidates(n *Notify) []routing.Candidate[Reason] {
	if n == nil || n.reassertsHolder() {
		return nil
	}
	task := n.Kind.TaskCommit()
	// What the write moved, never its record's kind ([Notify.moved]): a
	// hand-off written with a status is a `status` record.
	handedOff := n.moved("assignee")
	finishedEdge := n.crossesFinished()

	// Told under the first reason, woken by any ([routing.Builder.StartsTurn]):
	// a stronger fact that starts no turn never swallows a weaker one that does.
	b := routing.Builder[Reason]{
		StartsTurn: func(reason Reason) bool { return reason.StartsTurn(n.asks(reason)) },
	}
	add := func(handle string, reason Reason, addressed bool) {
		b.Add(handle, reason, addressed)
	}
	addAll := func(handles []string, reason Reason, addressed bool) {
		b.AddAll(handles, reason, addressed)
	}

	// An agent's mention of an agent that asks nothing is a reference: told,
	// never woken or obliged (ADR-0113).
	for _, handle := range n.Mentions {
		named := !n.agentReference() || !slices.Contains(n.Snapshot.MentionedAgents, handle)
		b.AddWaking(handle, ReasonMention, named, named)
	}
	if n.Kind == ChangePrioritised {
		add(n.Snapshot.Person, ReasonPrioritised, true)
	}
	// What this commit did to you outranks the role you hold, so these three
	// precede `assignee` (see [Reasons]). Behind it, asking the assignee told
	// them only "a task you are assigned to changed", unaddressed when an agent
	// asked: the silence an ask exists to prevent.
	if n.Kind == ChangeRelations && len(n.Snapshot.Dependents) > 0 {
		add(n.Snapshot.Assignee, ReasonBlocking, false)
	}
	add(n.Snapshot.CommentAsk, ReasonAsked, true)
	add(n.Snapshot.AnsweredAuthor, ReasonAnswered, false)
	if task {
		add(n.Snapshot.Assignee, ReasonAssignee, n.assigneeAddressed(handedOff, finishedEdge))
	}
	if handedOff && n.Snapshot.LeftUnassigned {
		// Not the work's owner any more, so no audience for the fallback.
		b.InboxOnly(n.Snapshot.PrevAssignee, ReasonUnassigned)
	} else if handedOff {
		add(n.Snapshot.PrevAssignee, ReasonUnassigned, false)
	}
	switch {
	case finishedEdge, handedOff,
		n.Kind == ChangeComment,
		n.Kind == ChangeRemoved:
		add(n.Snapshot.Reporter, ReasonReporter, false)
	}
	if n.Kind == ChangeComment {
		// A stake in the thread wakes whether or not its holder spoke there.
		for _, handles := range [][]string{n.Snapshot.ThreadParticipants,
			n.Snapshot.ThreadOwing, n.Snapshot.ThreadAwaiting} {
			for _, handle := range handles {
				b.AddWaking(handle, ReasonThread, false,
					ReasonThread.StartsTurn(n.threadAsked(handle)))
			}
		}
	}
	// A flow notice tells the one person its duty chose and nobody else
	// ([RecordFlow]): it is no task commit and offers no fallback. Told,
	// never obliged: it asks the lead to look, not to answer.
	if n.Kind == ChangeFlow && n.Snapshot.Flow != nil {
		add(n.Snapshot.Flow.Told, ReasonFlow, false)
	}
	// [Writer.TellUnblocked], the only writer of `Snapshot.Unblocked`,
	// publishes a record on the DEPENDENT naming only its assignee, so no
	// earlier arm claims the handle; notice rows and wake ids key on record
	// and recipient, so a second notice on one record would collapse. Never
	// addressed: a turn that cannot start yet must not be forced to post.
	for _, party := range n.Snapshot.Unblocked {
		add(party.Assignee, ReasonUnblocked, false)
	}
	// Each is filled only where the write made that transition, whatever its
	// record's kind ([Notify.restated], [Wake.snapshot]): empty means it did
	// not happen.
	add(n.Snapshot.RoutedTo, ReasonRoutedTo, false)
	add(n.Snapshot.ParentAssignee, ReasonParentAssignee, false)
	addAll(n.Snapshot.ChecklistAssignees, ReasonChecklist, false)
	if task {
		addAll(n.Snapshot.Collaborators, ReasonCollaborator, false)
	}
	if task {
		addAll(n.Snapshot.Watchers, ReasonWatcher, false)
	}
	// An unwatch is a record, not a wake: the agent did not decide it.
	for _, h := range n.Snapshot.RemovedWatchers {
		b.InboxOnly(h, ReasonUnwatched)
	}
	if n.Kind == ChangePurged {
		add(n.Snapshot.ProjectLead, ReasonPurged, false)
	}
	// A departure tells each lead once, on one record ([Snapshot.DepartureLead]).
	add(n.Snapshot.DepartureLead, ReasonLeadFallback, false)
	// The fallback is always offered, as an ordered list; Route decides its
	// use. A list because a lead filing a stray into their own team is dropped
	// as the actor, and the project lead must still get the copy. It wakes
	// only where the work's owner did not make the change ([Notify.asks]).
	if n.fallsBack() {
		b.Fallback(ReasonLeadFallback, n.Snapshot.RoutingUnitLead, n.Snapshot.ProjectLead)
	}
	return b.Candidates()
}

// CandidateFor is how handle hears the change n describes, as [Candidates]
// decides it; false when the change does not concern it. A writer telling its
// caller who will act uses it, so it never keeps a second copy of the rule.
func CandidateFor(n *Notify, handle string) (routing.Candidate[Reason], bool) {
	for _, c := range Candidates(n) {
		if c.Handle == handle {
			return c, true
		}
	}
	return routing.Candidate[Reason]{}, false
}

// asks reports whether this change asks something of the seat that hears it
// under reason, for the reasons whose wake depends on it ([ownerReasons] and
// [ReasonWatcher]). A lead is asked to find work an owner, so a change its
// owner made ([Snapshot.ByOwner]) only tells the lead.
func (n *Notify) asks(reason Reason) bool {
	switch reason {
	case ReasonWatcher:
		return n.watcherAsked()
	case ReasonLeadFallback:
		return n.ownerAsked() && !n.Snapshot.ByOwner
	}
	return n.ownerAsked()
}

// assigneeAddressed decides whether the assignee is ASKED something (an
// addressed turn must answer; an unaddressed one may absorb), not whether it is
// woken ([Notify.ownerAsked]). A person's comment on your ticket is the ask;
// another agent's is not. A hand-off and the finished edge ask whatever kind
// carries them ([Notify.moved]); a reassertion to the holder hands nothing over.
func (n *Notify) assigneeAddressed(handedOff, finishedEdge bool) bool {
	if handedOff || finishedEdge {
		return true
	}
	switch n.Kind {
	case ChangeCreated:
		return true
	// A change the task waits on settled: its holder is to update it.
	case ChangeLinkSettled:
		return true
	case ChangeComment:
		return n.personComment()
	}
	return false
}

// ownerAsked decides whether the change wakes the seats that answer for the
// task ([ownerReasons]): every change but another agent's conversation and a
// change to who watches it alone ([Notify.watchOnly]). Another agent's comment
// wakes the owner only by naming it (@-mention, `ask`, or a reply in its
// thread), each a reason of its own: unaddressed agent comments mostly woke
// the owner into acknowledgement ping-pong. The comment tool warns through
// [CandidateFor]. A person's comment naming nobody wakes the owner; one naming
// somebody is theirs, and the owner hears it in its inbox (ADR-0113).
func (n *Notify) ownerAsked() bool {
	if n.Kind.Conversation() {
		return n.personComment() && !n.addressedToSomebody()
	}
	return !n.watchOnly()
}

// moved reports whether this change moved field on a task that already stood:
// whether its write recorded that field's delta ([Notify.Fields]).
//
// By the delta, never the kind: a write moving several fields is filed under
// one ([ChangeKind.MovedFields]), so "assign to ana and set todo" is a
// `status` record with an assignee delta. Never on a creation, whose deltas
// are every field from nothing ([Wake.Before]); a record built without a
// [Wake] moved nothing. [cardFields] keeps every field a route reads whole.
func (n *Notify) moved(field string) bool {
	if n.Kind == ChangeCreated {
		return false
	}
	_, ok := n.Fields[field]
	return ok
}

// movedKinds is what this change moved, as the kinds a recipient is told a
// change under: every kind whose field its write moved ([ChangeKind.MovedFields],
// [Notify.moved]), whatever kind its record ended with, in [ChangeKinds] order.
// Empty on a creation, and on a change that moved none of them. The parser
// stamps it for the prompt ([MetaMoved]). None on the conversation: the one
// field a comment moves is its writer's own watch ([WatchIntent.Auto]).
func (n *Notify) movedKinds() []ChangeKind {
	if n.Kind.Conversation() {
		return nil
	}
	var out []ChangeKind
	for _, kind := range ChangeKinds {
		if slices.ContainsFunc(kind.MovedFields(), n.moved) {
			out = append(out, kind)
		}
	}
	return out
}

// crossesFinished reports a change that moved the task's status into or out
// of a finished group, whatever kind its record ended with: the transition a
// reporter and a parent's assignee are told of. Requiring the status delta
// keeps a hand-built snapshot naming no group it left from reading as a crossing.
func (n *Notify) crossesFinished() bool {
	return n.moved("status") &&
		n.Snapshot.PrevStatusGroup.Finished() != n.Snapshot.StatusGroup.Finished()
}

// watchOnly reports a change to who watches the task and to nothing else:
// deltas naming only [watchFields], or a `watchers` record that moved nothing.
// It asks nothing of the owners and moves no work ([committedChange]). A watch
// written with another change is that change, whatever kind it was filed
// under; a field the card left off ([Notify.FieldsOmitted]) and a checklist
// item touched ([Snapshot.ChecklistAssignees]) count as other changes.
func (n *Notify) watchOnly() bool {
	if n.FieldsOmitted > 0 || len(n.Snapshot.ChecklistAssignees) > 0 {
		return false
	}
	for field := range n.Fields {
		if !slices.Contains(watchFields, field) {
			return false
		}
	}
	return len(n.Fields) > 0 || n.Kind == ChangeWatchers
}

// watchFields are the deltas that say who watches a task and who of them is
// muted: what a change to who watches it moves, and all it moves. The reads
// that decide on a history row ask the same of it in SQL ([movedBeyondWatch]).
var watchFields = []string{watchersField, mutedField}

// watchersField and mutedField name the two deltas of a change to who watches.
const (
	watchersField = "watchers"
	mutedField    = "muted"
)

// reassertsHolder reports an assignment to the seat that already holds the
// task with nothing else changed or named: it concerns nobody. "Nothing else"
// is everything the record says, not only the deltas: a checklist item handed
// to somebody moves no delta but must still reach its new holder.
func (n *Notify) reassertsHolder() bool {
	s := n.Snapshot
	return n.Kind == ChangeAssignee && s.Assignee == s.PrevAssignee &&
		len(n.Fields) == 0 && n.FieldsOmitted == 0 && len(n.Mentions) == 0 &&
		len(s.RemovedWatchers) == 0 && s.RoutedTo == "" && s.ParentAssignee == "" &&
		len(s.ChecklistAssignees) == 0
}

// threadAsked decides whether a reply wakes one thread participant: one with
// a stake in it (an open ask it made, or one made of it) whatever the reply
// names, and any other only for a person's comment that names nobody.
func (n *Notify) threadAsked(handle string) bool {
	if slices.Contains(n.Snapshot.ThreadOwing, handle) ||
		slices.Contains(n.Snapshot.ThreadAwaiting, handle) {
		return true
	}
	return n.personComment() && !n.addressedToSomebody()
}

// addressedToSomebody reports a comment that names whom it is for: an
// @-mention, an ask, or an answer to somebody's ask. Each of those reaches
// its handle under a reason of its own that starts a turn.
func (n *Notify) addressedToSomebody() bool {
	return len(n.Mentions) > 0 || n.Snapshot.CommentAsk != "" ||
		n.Snapshot.AnsweredAuthor != ""
}

// watcherAsked decides whether the change wakes a WATCHER, which follows the
// task rather than answering for it: a person's comment and nothing else. A
// person writing on a task speaks to whoever follows it, without naming
// anybody; every other change reaches a watcher's inbox, and a seat that
// needs a watcher to act names it.
func (n *Notify) watcherAsked() bool {
	return n.Kind.Conversation() && n.personComment() && !n.addressedToSomebody()
}

// MentionsWake reports whether the seats this change @-mentions are woken:
// every author's but an agent's comment that does not say it asks.
func (n *Notify) MentionsWake() bool { return n != nil && !n.agentReference() }

// agentReference reports an agent's comment that states it asks nothing, whose
// mentions of agents are references (ADR-0113). A record with no statement, a
// person's or an older build's, routes as it always did.
func (n *Notify) agentReference() bool {
	asks := n.Snapshot.CommentAsks
	return n.Kind.Conversation() && n.Snapshot.CommentAuthorKind == AuthorAgent &&
		asks != nil && !*asks
}

// personComment reports a comment a person wrote ([AuthorKind.Person]).
func (n *Notify) personComment() bool { return n.Snapshot.CommentAuthorKind.Person() }

// Conversation reports a change that is talk on the task rather than a change
// to it: a comment, and its edit, resolution or removal.
func (k ChangeKind) Conversation() bool {
	switch k {
	case ChangeComment, ChangeCommentEdited, ChangeCommentResolved, ChangeCommentRemoved:
		return true
	}
	return false
}

// TaskCommit reports a change about a task, which is the set that wakes an
// assignee, its collaborators and its watchers.
//
// Exported so a test can walk it: this allowlist has no compiler link to
// [ChangeKinds], and a kind missing from it routes to nobody silently. The
// classification test in wakes_test.go is that link.
func (k ChangeKind) TaskCommit() bool {
	switch k {
	case ChangeCreated, ChangeFields, ChangeStatus, ChangeAssignee,
		ChangeCollaborators, ChangeWatchers, ChangeTags, ChangeRelations,
		ChangeRouted, ChangeMoved, ChangeReparented,
		ChangeChecklist, ChangeArchived, ChangeComment, ChangeCommentEdited,
		ChangeCommentResolved, ChangeCommentRemoved, ChangeRemoved,
		ChangeRestored, ChangePurged, ChangeLinkSettled, ChangeDeadWait, ChangeDeadWaitEnded:
		return true
	}
	return false
}

// fallsBack reports a change that reaches a lead when nobody else is there to
// hear it: a creation, a move to another project, a removal, a comment, an
// update that moved the status or the team ([Notify.moved]), or one that left
// the task unassigned ([Snapshot.LeftUnassigned]). Not every task change: a tag edit
// on an unwatched task is not worth paging a lead, nor is a status or team a
// write only restated beside another edit.
func (n *Notify) fallsBack() bool {
	switch n.Kind {
	case ChangeCreated, ChangeMoved, ChangeRemoved, ChangeComment:
		return true
	}
	return n.moved("status") || n.moved("routing_unit") || n.Snapshot.LeftUnassigned
}

// ownership is n with who owns the task once a change actor made is done.
// [Snapshot.LeftUnassigned]: the change took the assignee away and named none.
// [Snapshot.ByOwner]: actor holds the task, as assignee or collaborator, never
// as reporter, and only the lead's fallback would hear it; never on a move into
// another project, which brings the work to a lead who never heard of it.
func (n *Notify) ownership(actor string) *Notify {
	if n == nil {
		return nil
	}
	out := *n
	s := &out.Snapshot
	s.LeftUnassigned = s.Assignee == "" && n.moved("assignee")
	holds := actor != "" && (s.Assignee == actor || slices.Contains(s.Collaborators, actor))
	s.ByOwner = false
	if holds && n.Kind != ChangeMoved {
		heard := Heard(Candidates(&out), nil, actor)
		s.ByOwner = len(heard) == 1 && heard[0].FallbackOnly
	}
	return &out
}

// Route is the post-filter — who is actually woken — with the tracker's one
// reason that reaches the person who made the change ([Reason.WakesActor]).
// Its five steps are [routing.Route]'s. Keeping only candidates whose notice
// starts a turn comes last, because a watcher an agent's quiet comment does
// not wake still heard it, and so keeps the lead's fallback copy quiet.
func Route(candidates []routing.Candidate[Reason], registry func(string) bool,
	actor string) []routing.Candidate[Reason] {

	return routing.Route(candidates, registry, actor, Reason.WakesActor)
}

// noticeWakes is, for the handles a change's notice rows go to, which it woke
// by the notifier's own rule, and whether it tells any of them without waking
// them. Every handle reads employed: the runtime registry is not the record's,
// so a seat it no longer holds can be marked woken where the parser falls back
// to its lead instead.
func noticeWakes(n *Notify, actor string) (map[string]bool, bool) {
	candidates := Candidates(n)
	woken := map[string]bool{}
	for _, c := range routing.Waking(Heard(candidates, nil, actor)) {
		woken[c.Handle] = true
	}
	for _, c := range candidates {
		if c.Hears(actor, Reason.WakesActor) && !woken[c.Handle] {
			return woken, true
		}
	}
	return woken, false
}

// mayWakeAnAgent reports whether a change actor made may wake an agent other
// than actor (ADR-0109), proved from its Notify alone ([Writer.carries]): some
// offered candidate, the fallback chain's included, that hears the change and
// whose notice starts a turn. The decide asks it of a Notify already stripped
// of credentials, since a credential's inbox is no candidate.
//
// Not [noticeWakes], [tellsWithoutWaking] or [Heard] with a nil registry: with
// none, a fallback is never reached while an ordinary candidate is heard, but
// the parser routes with the live registry and wakes the lead when a seat is gone.
func mayWakeAnAgent(n *Notify, actor string) bool {
	for _, c := range Candidates(n) {
		if !c.InboxOnly && c.Hears(actor, Reason.WakesActor) && c.Wakes {
			return true
		}
	}
	return false
}

// Heard is [Route] without its last step: everybody told, woken or not.
func Heard(candidates []routing.Candidate[Reason], registry func(string) bool,
	actor string) []routing.Candidate[Reason] {

	return routing.Heard(candidates, registry, actor, Reason.WakesActor)
}

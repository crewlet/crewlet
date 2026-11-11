package worklet

import (
	"slices"

	"github.com/crewlet/crewlet/internal/routing"
)

// Who hears about a change, and why the answer is TWO functions.
//
// [Candidates] is pure over the notification: it reads the kind, the mentions
// and the routing snapshot, and nothing else — no clock, no org, no registry,
// no actor. That is what lets the APPLIER call it inside its transaction (it
// reads no config and no clock, so the determinism rules stay literally true)
// and the wake path call it on a different node minutes later, and get the
// same answer.
//
// [Route] is the post-filter, and it needs the one fact a record cannot carry:
// who the company employs RIGHT NOW. Who made the change is on the record, so
// the applier takes Route's actor step too ([routing.Candidate.Hears]) and
// leaves only the roster to the wake. That split makes the applier's
// `tracker_notifications` rows a complete record of everyone the change
// concerned but its author, while the wake goes only to the people still
// there — and it is why the parity test can assert that the two surfaces
// agree.
//
// THE RULE BOTH APPLY IS internal/routing's, shared with every native domain:
// one reason per handle, the first offered; the ordered fallback; the
// post-filter's five steps. What stays here is what is the tracker's own — its
// eighteen reasons, the ORDER [Candidates] offers them in, which is their
// precedence, the one reason that reaches the actor, and WHICH NOTICES START A
// TURN ([Reason.StartsTurn]): everybody a change concerns is told, by a notice
// row, and only those it asks something of are woken.

// Reason is why one handle hears about a change.
type Reason string

// The eighteen reasons, as they are written onto the log and into
// `tracker_notifications`.
//
// THIS ORDER IS NOT THE PRECEDENCE — [Reasons] is, and it deliberately differs.
// Re-sorting these declarations to match it would buy nothing and leave two
// orderings in one file that a reader could mistake for one.
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
)

// Reasons are the eighteen, IN PRECEDENCE ORDER.
//
// THE ORDER IS THE RULE: the first reason that names a handle is the one that
// handle hears under, and every later one for the same handle is dropped. A
// person who is mentioned AND watching is told they were mentioned, which is
// the stronger fact and the one they will act on.
//
// It has to match the order [Candidates] adds them in, and the three reasons
// ahead of `assignee` are why that is worth saying. Each describes what THIS
// COMMIT did to the recipient, and each names exactly the handle the standing
// role was about to claim: behind `assignee` they were shadowed on the one
// person most likely to be the subject, and `blocking` — whose only handle IS
// the blocker's assignee — could never name anybody at all.
//
// THE PRECEDENCE IS WRITTEN TWICE, then — here for the surfaces that print and
// filter by reason, and as the offer order in [Candidates], which is the one
// the routing obeys — so a test holds the two together:
// TestEveryKindOffersItsReasonsInTheDeclaredPrecedence walks every kind with
// a different handle in every field and requires the candidates to come back
// in this order, so a reordered arm and a reordered list both fail it.
var Reasons = []Reason{
	ReasonMention, ReasonPrioritised, ReasonBlocking, ReasonAsked,
	ReasonAnswered, ReasonAssignee, ReasonUnassigned, ReasonReporter,
	ReasonThread, ReasonUnblocked, ReasonRoutedTo, ReasonParentAssignee,
	ReasonChecklist, ReasonCollaborator,
	ReasonWatcher, ReasonUnwatched, ReasonPurged, ReasonLeadFallback,
}

// Valid reports whether a reason off the wire is one this build knows.
func (r Reason) Valid() bool { return slices.Contains(Reasons, r) }

// addressedReasons is the eight under which a wake OBLIGES ITS SEAT TO ANSWER.
//
// ADDRESSED IS "SOMEBODY IS WAITING ON YOU", not "important": routed_to is
// deliberately not here, because "work routes to you now" is a fact to absorb
// rather than an obligation, and a watcher is following the task rather than
// being asked about it.
//
// IT IS NOT THE INBOX'S FIRST BAND, although it was called `primaryReasons`
// and documented as exactly that for as long as it has existed. That question
// has its own answer — [DefaultPrimaryReasons] — and the two lists differ on
// four reasons, correctly: `unassigned` and `unblocked` are changes to your
// own work that belong at the top of what you read, and neither asks you for
// an answer; `thread` and `blocking` do ask for one, and neither is what a
// person wants their inbox to open with. Two questions, two answers. The name
// was the bug: a reader who found this one first wrote its eight into a
// screen that needed the other eight, and nothing could have told them apart.
var addressedReasons = []Reason{
	ReasonMention, ReasonAsked, ReasonAssignee, ReasonReporter,
	ReasonAnswered, ReasonPrioritised, ReasonThread, ReasonBlocking,
}

// Addressed reports whether a wake under this reason obliges its seat to
// answer. For which reasons lead a PERSON's inbox, see [DefaultPrimaryReasons].
func (r Reason) Addressed() bool { return slices.Contains(addressedReasons, r) }

// turnReasons are the reasons whose notice starts a turn whatever the change
// was: each says the change was put to this seat, or moved work it is
// accountable for.
//
// A THIRD LIST, and not either of the two above, because it answers a third
// question: not "must the turn answer" ([Reason.Addressed]) and not "does it
// lead a person's inbox" ([DefaultPrimaryReasons]), but "is it worth a turn at
// all". `answered` is a reply to something the seat asked;
// `unblocked`, `routed_to`, `parent_assignee`, `checklist` and `blocking` move
// work the seat owns; `mention`, `prioritised` and `asked` put something to
// it; `purged` takes work out of the lead's project. None of these is
// something a seat should first learn about when it happens to read the task
// again.
var turnReasons = []Reason{
	ReasonMention, ReasonPrioritised, ReasonAsked, ReasonAnswered,
	ReasonUnblocked, ReasonRoutedTo, ReasonParentAssignee,
	ReasonChecklist, ReasonBlocking, ReasonPurged,
}

// ownerReasons start a turn exactly when the change asks something of the
// task's OWNER ([Notify.ownerAsked]): its assignee and collaborators, the lead
// who hears in the owner's place when the task has nobody else
// ([ReasonLeadFallback]), and the reporter, who verifies what the owner does.
//
// ONE RULE FOR ALL FOUR, because each answers for the outcome. The reporter is
// added only for the changes that concern it (the task finishing or reopening,
// a reassignment, a removal, a comment; see [Candidates]), so under the same
// rule it is woken by those and by a person's comment, and never by another
// agent's note on work it handed over.
var ownerReasons = []Reason{
	ReasonAssignee, ReasonCollaborator, ReasonLeadFallback, ReasonReporter,
}

// StartsTurn reports whether a notice under this reason wakes its seat into a
// turn, given whether the change asks something of the seat that holds the
// reason ([Notify.asks]).
//
// EVERY OTHER NOTICE IS STILL DELIVERED, as a row in the recipient's inbox
// that the seat reads when it next opens the task. The line is measured, on a
// live company's week of Worklet wakes: a turn woken only by another agent's
// comment on a task the seat watched changed the work in 4 of 1,011, and one
// woken only by a change to a watched task's status, relations, fields or
// checklist in 4 of 92; a change to the task woke its owner into a turn that
// changed the work far more often (a status move by somebody else 82%, a
// change to its dependencies 43%).
//
// `thread` IS DECIDED PER HANDLE, in [Candidates]: a reply in a thread wakes
// the participants an open ask there is addressed to whoever wrote it, and the
// rest only for a person's comment that names nobody (ADR-0113). Measured on a
// live company, two agents in one thread answered each other's
// acknowledgements six times in five minutes, each turn woken by `thread`.
// Here `asked` is that per-handle answer.
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
// change.
//
// That a task of yours became workable is news whoever wrote the record that
// says so: the notice is about the task, not about anything its writer did.
// Its one writer today is the unblocked repair ([Writer.TellUnblocked]),
// which the engine runs as its node, so the notice never meets its own
// author; the rule is the reason's, and it keeps the notice reaching its
// person through any writer that tells them, the person themself included.
// Every other reason is dropped for the actor, because telling somebody what
// they just did is noise a model spends a round on.
func (r Reason) WakesActor() bool { return r == ReasonUnblocked }

// Candidates is every handle a change concerns, in precedence order.
//
// IT RETURNS NOTHING FOR A NIL NOTIFICATION, and that single rule is what
// keeps the two surfaces in step: the translator acks a quiet commit so the
// feed never routes one, and the applier calls this same function, so a quiet
// commit leaves no notification row either. Without it a quiet walk would
// write rows nobody would ever be woken for, and an inbox would fill with
// changes the company deliberately did not announce.
func Candidates(n *Notify) []routing.Candidate[Reason] {
	if n == nil || n.reassertsHolder() {
		return nil
	}
	task := n.Kind.TaskCommit()
	// WHAT THE WRITE MOVED, never the one kind its record ended with
	// ([Notify.moved]): a hand-off written with a status is a `status`
	// record, and its old holder, its reporter and its new holder are told
	// as for one written alone.
	handedOff := n.moved("assignee")
	finishedEdge := n.crossesFinished()

	// TOLD UNDER THE FIRST REASON, WOKEN BY ANY: the builder keeps each
	// handle's first reason and adds every later ordinary reason's wake to
	// it ([routing.Builder.StartsTurn]), so a stronger fact that starts no
	// turn never swallows a weaker one that does — the reporter who also
	// wrote in the thread a reply lands in is told "a task you reported",
	// and is still woken, because the reply is to something they wrote.
	b := routing.Builder[Reason]{
		StartsTurn: func(reason Reason) bool { return reason.StartsTurn(n.asks(reason)) },
	}
	add := func(handle string, reason Reason, addressed bool) {
		b.Add(handle, reason, addressed)
	}
	addAll := func(handles []string, reason Reason, addressed bool) {
		b.AddAll(handles, reason, addressed)
	}

	// AN AGENT'S MENTION OF AN AGENT THAT ASKS NOTHING IS A REFERENCE: told,
	// never woken or obliged (ADR-0113); a person named is addressed as ever.
	for _, handle := range n.Mentions {
		named := !n.agentReference() || !slices.Contains(n.Snapshot.MentionedAgents, handle)
		b.AddWaking(handle, ReasonMention, named, named)
	}
	if n.Kind == ChangePrioritised {
		add(n.Snapshot.Person, ReasonPrioritised, true)
	}
	// WHAT THIS COMMIT DID TO YOU OUTRANKS THE ROLE YOU HOLD, and these
	// three arms are where that rule bites.
	//
	// The order here IS the precedence — the first reason that names a
	// handle is the one that handle hears under — and all three name
	// exactly the people the generic `assignee` arm was about to claim.
	// Behind it they were not merely outranked; `blocking` could never
	// name ANYBODY, because the only handle it ever adds is the blocker's
	// own assignee. `asked` and `answered` fared worse than they looked:
	// they fired for a colleague and fell silent for the one person most
	// likely to be asked, so asking the assignee a question told them "a
	// task you are assigned to changed" — and, when the asker was another
	// agent, told them so UNADDRESSED, which a turn is entitled to absorb
	// without replying. That is the exact silence an ask exists to prevent.
	//
	// It is the same rule that already puts `mention` first: being named
	// in the text outranks holding the task, and an ask is a mention with
	// an obligation attached.
	if n.Kind == ChangeRelations && len(n.Snapshot.Dependents) > 0 {
		add(n.Snapshot.Assignee, ReasonBlocking, false)
	}
	add(n.Snapshot.CommentAsk, ReasonAsked, true)
	add(n.Snapshot.AnsweredAuthor, ReasonAnswered, false)
	if task {
		add(n.Snapshot.Assignee, ReasonAssignee, n.assigneeAddressed(handedOff, finishedEdge))
	}
	if handedOff {
		add(n.Snapshot.PrevAssignee, ReasonUnassigned, false)
	}
	switch {
	case finishedEdge, handedOff,
		n.Kind == ChangeComment,
		n.Kind == ChangeRemoved:
		add(n.Snapshot.Reporter, ReasonReporter, false)
	}
	if n.Kind == ChangeComment {
		// A STAKE IN THE THREAD WAKES WHETHER OR NOT ITS HOLDER SPOKE
		// THERE: an asker, or a seat asked and not answered yet.
		for _, handles := range [][]string{n.Snapshot.ThreadParticipants,
			n.Snapshot.ThreadOwing, n.Snapshot.ThreadAwaiting} {
			for _, handle := range handles {
				b.AddWaking(handle, ReasonThread, false,
					ReasonThread.StartsTurn(n.threadAsked(handle)))
			}
		}
	}
	// THE UNBLOCKED NOTICE TAKES ITS PLACE LIKE ANY OTHER REASON, because its
	// record is one of its own: [Writer.TellUnblocked] — the repair, and the
	// only writer of `Snapshot.Unblocked` — publishes a relations commit on
	// the DEPENDENT naming its assignee and nobody else, so no earlier arm
	// can claim the handle first. A notice about another task on a record
	// about this one would be a second notice to one person on one record,
	// which neither the notice row (keyed on record and recipient) nor the
	// wake (whose id is derived from the two) can carry — the second collapses
	// into the first — so it is never written that way. And it is never
	// ADDRESSED: a turn that cannot start yet would otherwise be forced to
	// post something on every unblock.
	for _, party := range n.Snapshot.Unblocked {
		add(party.Assignee, ReasonUnblocked, false)
	}
	// THE TRANSITIONS A WRITE MADE, read wherever they are filled: the new
	// unit's lead, the parent's assignee and the holders of the checklist
	// items it touched are each filled where the write made that transition,
	// whatever kind its record ended with, and on nothing else — by an
	// update's decide ([Notify.restated]), or by [Wake.snapshot] for a record
	// no decide restates — so an empty field is a transition that did not
	// happen.
	add(n.Snapshot.RoutedTo, ReasonRoutedTo, false)
	add(n.Snapshot.ParentAssignee, ReasonParentAssignee, false)
	addAll(n.Snapshot.ChecklistAssignees, ReasonChecklist, false)
	if task {
		addAll(n.Snapshot.Collaborators, ReasonCollaborator, false)
	}
	if task {
		addAll(n.Snapshot.Watchers, ReasonWatcher, false)
	}
	// AN UNWATCH IS A RECORD, NOT A WAKE: a human learns that a lead removed
	// her watch, and an agent is not woken about a decision that was not its
	// own. Filled, like the transitions above, on any update that dropped a
	// watcher, whatever kind its record ended with.
	for _, h := range n.Snapshot.RemovedWatchers {
		b.InboxOnly(h, ReasonUnwatched)
	}
	if n.Kind == ChangePurged {
		add(n.Snapshot.ProjectLead, ReasonPurged, false)
	}
	// THE FALLBACK IS ALWAYS OFFERED, AS AN ORDERED LIST, and Route is
	// what decides whether it is used.
	//
	// A LIST RATHER THAN "unit lead, else project lead", because of one
	// common case: a lead filing an unassigned stray into their own team.
	// With one candidate, Route's actor drop removes it and the task
	// reaches NOBODY; with the list, the unit lead is dropped as the actor
	// and the project lead gets the copy.
	if n.fallsBack() {
		b.Fallback(ReasonLeadFallback, n.Snapshot.RoutingUnitLead, n.Snapshot.ProjectLead)
	}
	return b.Candidates()
}

// CandidateFor is how handle hears the change n describes, as [Candidates]
// decides it: its reason, whether its notice starts a turn and whether that
// turn is asked to answer; false when the change does not concern it. Every
// candidate is this change's: an unblocked notice rides a record of its own,
// on the task that became workable (see [Candidates]).
//
// FOR A WRITER THAT TELLS ITS CALLER WHO WILL ACT, so what it says is the
// routing's own answer for the record it is about to publish, never a second
// copy of the rule that drifts from it.
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
// [ReasonWatcher]).
func (n *Notify) asks(reason Reason) bool {
	if reason == ReasonWatcher {
		return n.watcherAsked()
	}
	return n.ownerAsked()
}

// assigneeAddressed decides whether the assignee is being ASKED something,
// which is what a woken turn owes: an addressed one must answer, and an
// unaddressed one may simply absorb. A comment from a person on your ticket
// IS the ask; the same comment from another agent is not.
//
// NOT WHETHER THE ASSIGNEE IS WOKEN, which is [Notify.ownerAsked]: a status
// move on your task asks you to look at it without obliging you to post.
//
// A HAND-OFF AND THE FINISHED EDGE ASK WHATEVER KIND CARRIES THEM, since both
// are read off what the write moved ([Notify.moved]): the new holder of work
// handed over with a status move owes it an answer as surely as the holder of
// work handed over alone, and a reassertion to the holder hands nothing over.
func (n *Notify) assigneeAddressed(handedOff, finishedEdge bool) bool {
	if handedOff || finishedEdge {
		return true
	}
	switch n.Kind {
	case ChangeCreated:
		return true
	// A CHANGE THE TASK WAITS ON SETTLED: whoever holds it is to update it.
	case ChangeLinkSettled:
		return true
	case ChangeComment:
		return n.personComment()
	}
	return false
}

// ownerAsked decides whether the change wakes the seats that answer for the
// task ([ownerReasons]): every change to it except another agent's
// conversation on it and a change to who watches it and nothing else
// ([Notify.watchOnly]).
//
// ANOTHER AGENT'S COMMENT WAKES THE OWNER ONLY BY NAMING IT, with an
// @-mention, an `ask` or a reply in a thread the owner wrote in, each a reason
// of its own that starts a turn whatever this answers. Measured before this
// rule, the owner's turns woken by an unaddressed agent comment ended in a
// reply comment 64% of the time, most of them an acknowledgement that woke
// the other agent to acknowledge it back, and changed the work in 14%; the
// comment tool tells an agent so, and its result warns when a comment reaches
// the assignee's inbox and no further, reading this rule through
// [CandidateFor]. A person's comment that names nobody is the ask, and wakes
// the owner.
//
// A PERSON'S COMMENT THAT NAMES SOMEBODY IS ADDRESSED TO THEM: an @-mention, an
// ask or an answer makes the comment somebody's in particular, and the owner
// hears it in its inbox unless it is the one named (ADR-0113).
func (n *Notify) ownerAsked() bool {
	if n.Kind.Conversation() {
		return n.personComment() && !n.addressedToSomebody()
	}
	return !n.watchOnly()
}

// moved reports whether this change moved field on a task that already stood:
// whether its write recorded that field's delta ([Notify.Fields]), whatever
// kind its record ended with.
//
// BY THE DELTA, NEVER BY THE KIND. A record carries one kind, and a write that
// moves several fields at once is filed under one of them
// ([ChangeKind.MovedFields]): "assign it to ana and set it to todo" is a
// `status` record with an assignee delta beside it. Decided by the kind, that
// hand-off told its old holder and its reporter nothing and woke ana as a
// status move she could absorb, the routing copy of what the work pickup's
// history read did until it read the assignee delta (#930, #953).
//
// NEVER ON A CREATION, whose deltas are every field it was filed with, from
// nothing ([Wake.Before]): filing work hands nobody anything, and the
// creation's own arms route it. And a record built without a [Wake] carries
// no deltas, so it moved nothing, which is what each of them is: a purge's
// notice, a person's priority list, a repair's late wake.
//
// THE WHOLE OF IT FOR EVERY FIELD A ROUTE READS, which [cardFields] keeps
// whatever it trims for a card.
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
// stamps it for the prompt ([MetaMoved]).
//
// NONE ON THE CONVERSATION, whose words are the change: the one field a
// comment moves is the watch its writer picks up by commenting
// ([WatchIntent.Auto]), which is the comment's own and nobody's news.
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
// reporter and a parent's assignee are told of, read off the record as the
// package's [crossesFinished] reads it off the two states. The status delta
// is what keeps a snapshot built by hand, which may name a group and none it
// left, from reading as an edge it never crossed.
func (n *Notify) crossesFinished() bool {
	return n.moved("status") &&
		n.Snapshot.PrevStatusGroup.Finished() != n.Snapshot.StatusGroup.Finished()
}

// watchOnly reports a change to who watches the task and to nothing else:
// deltas that name the watcher set and its mute alone ([watchFields]),
// whatever kind its record ended with, or a record of kind `watchers` that
// moved nothing at all. Such a change asks nothing of the seats that answer
// for the task, and moves no work ([committedChange]).
//
// A WATCH WRITTEN WITH ANOTHER CHANGE IS THAT CHANGE: `update_work_item` files
// a watch together with a due date, a priority, a title or a custom value
// under `watchers` wherever nothing else named a kind, and decided by the kind
// none of those reached the owners as more than a notice in their inbox. A
// move the card left off counts too ([Notify.FieldsOmitted]), and so does a
// checklist item the write touched, which moves no list's done count when it
// is renamed or handed to somebody ([Snapshot.ChecklistAssignees]).
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
// task with nothing else changed or named: no change, so it concerns nobody.
//
// NOTHING ELSE IS EVERYTHING THE RECORD SAYS THE WRITE DID: no delta, nothing
// the card left off, no transition its snapshot names and nobody named. Read
// off the deltas alone, a reassertion written beside a checklist item handed
// to somebody, which moves no list's done count, reached nobody, the item's
// new holder included.
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
// EXPORTED SO A TEST CAN WALK IT. The allowlist below has no compiler link to
// [ChangeKinds], and that gap is what let `prioritised` sit in the enum,
// answer Valid(), pass Validate(), ride a real record and route to nobody: a
// task's priority change woke no assignee, no collaborator and no watcher, and
// nothing in the build noticed. The classification test in wakes_test.go is
// that link, and it needs to be able to ask.
func (k ChangeKind) TaskCommit() bool {
	switch k {
	case ChangeCreated, ChangeFields, ChangeStatus, ChangeAssignee,
		ChangeCollaborators, ChangeWatchers, ChangeTags, ChangeRelations,
		ChangeRouted, ChangeMoved, ChangeReparented,
		ChangeChecklist, ChangeArchived, ChangeComment, ChangeCommentEdited,
		ChangeCommentResolved, ChangeCommentRemoved, ChangeRemoved,
		ChangeRestored, ChangePurged, ChangeLinkSettled:
		return true
	}
	return false
}

// fallsBack reports a change that reaches a lead when nobody else is there to
// hear it: a creation, a move to another project, a removal and a comment,
// each an event of its own, and an update that moved the task's status or its
// team, whatever kind its record ends with ([Notify.moved], #953).
//
// NOT EVERY TASK CHANGE, because a fallback is for a change nobody would
// otherwise learn about: a tag edit or a checklist tick on an unassigned,
// unwatched task is not something to page a lead for. And never a status or a
// team a write only restated beside another edit, which moved nothing a lead
// acts on: decided by the kind, a priority set beside the status the task
// already had paged the lead, while a re-route written beside labels paged
// nobody.
func (n *Notify) fallsBack() bool {
	switch n.Kind {
	case ChangeCreated, ChangeMoved, ChangeRemoved, ChangeComment:
		return true
	}
	return n.moved("status") || n.moved("routing_unit")
}

// Route is the post-filter — who is actually woken — with the tracker's one
// reason that reaches the person who made the change ([Reason.WakesActor]).
//
// Its five steps are [routing.Route]'s: drop the inbox-only candidates, drop
// the actor except under `unblocked`, drop handles the company no longer
// employs, keep the lowest-ranked fallback still employed only when no
// ordinary candidate survived — the step that is the whole reason the
// fallback [Candidates] offers is a list — and, last, keep only the
// candidates whose notice starts a turn ([Reason.StartsTurn]). Last, because
// a watcher an agent's quiet comment does not wake still heard it, and so
// keeps the lead's fallback copy quiet. The registry is asked only about a
// handle whose answer could change the result, never about the actor or
// about a chain somebody ordinary made unnecessary.
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
// than actor (ADR-0109), proved from its Notify alone, as a writer's decide
// holds it ([Writer.carries]): some candidate it offers, ordinary or the
// fallback chain's, that hears the change and whose notice starts a turn. A
// credential's inbox is no candidate, so the decide asks it of the Notify it
// has already taken credentials out of.
//
// NOT [noticeWakes], [tellsWithoutWaking] or [Heard] with a nil registry: those
// route with no registry, and with none a fallback is never reached while an
// ordinary candidate is heard. The parser routes with the live one, so when a
// watcher's or an assignee's seat is gone it wakes the lead instead — a wake a
// nil registry's answer misses, and a turn that would then work for the
// writing seat. So every offered candidate counts, the fallback's included.
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

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
// post-filter's four steps. What stays here is what is the tracker's own — its
// eighteen reasons, the ORDER [Candidates] offers them in, which is their
// precedence, and the one reason that reaches the actor.

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
	if n == nil {
		return nil
	}
	var b routing.Builder[Reason]
	add := func(handle string, reason Reason, addressed bool) {
		b.Add(handle, reason, addressed)
	}
	addAll := func(handles []string, reason Reason, addressed bool) {
		b.AddAll(handles, reason, addressed)
	}

	task := n.Kind.TaskCommit()
	finishedEdge := n.Snapshot.PrevStatusGroup.Finished() != n.Snapshot.StatusGroup.Finished()

	addAll(n.Mentions, ReasonMention, true)
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
		add(n.Snapshot.Assignee, ReasonAssignee, n.assigneeAddressed(finishedEdge))
	}
	if n.Kind == ChangeAssignee {
		add(n.Snapshot.PrevAssignee, ReasonUnassigned, false)
	}
	switch {
	case n.Kind == ChangeStatus && finishedEdge,
		n.Kind == ChangeComment,
		n.Kind == ChangeAssignee,
		n.Kind == ChangeRemoved:
		add(n.Snapshot.Reporter, ReasonReporter, false)
	}
	if n.Kind == ChangeComment {
		addAll(n.Snapshot.ThreadParticipants, ReasonThread, false)
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
	if n.Kind == ChangeRouted {
		add(n.Snapshot.RoutedTo, ReasonRoutedTo, false)
	}
	if n.Kind == ChangeStatus && finishedEdge {
		add(n.Snapshot.ParentAssignee, ReasonParentAssignee, false)
	}
	if n.Kind == ChangeChecklist {
		addAll(n.Snapshot.ChecklistAssignees, ReasonChecklist, false)
	}
	if task {
		addAll(n.Snapshot.Collaborators, ReasonCollaborator, false)
	}
	if task {
		addAll(n.Snapshot.Watchers, ReasonWatcher, false)
	}
	// AN UNWATCH IS A RECORD, NOT A WAKE: a human learns that a lead removed
	// her watch, and an agent is not woken about a decision that was not its
	// own.
	if n.Kind == ChangeWatchers {
		for _, h := range n.Snapshot.RemovedWatchers {
			b.InboxOnly(h, ReasonUnwatched)
		}
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
	if n.Kind.mayFallBack() {
		b.Fallback(ReasonLeadFallback, n.Snapshot.RoutingUnitLead, n.Snapshot.ProjectLead)
	}
	return b.Candidates()
}

// assigneeAddressed decides whether the assignee is being ASKED something.
//
// The distinction is what a turn does with the wake: an addressed one must
// answer, and an unaddressed one may simply absorb. A comment from a person on
// your ticket IS the ask; the same comment from another agent is not.
func (n *Notify) assigneeAddressed(finishedEdge bool) bool {
	switch n.Kind {
	case ChangeCreated, ChangeAssignee:
		return true
	case ChangeStatus:
		return finishedEdge
	case ChangeComment:
		return n.Snapshot.CommentAuthorKind == AuthorHuman ||
			n.Snapshot.CommentAuthorKind == AuthorOperator
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
		ChangeRestored, ChangePurged:
		return true
	}
	return false
}

// mayFallBack reports the six kinds that reach a lead when nobody else is
// there to hear them.
//
// SIX RATHER THAN EVERY TASK KIND, because a fallback is for a change nobody
// would otherwise learn about: a tag edit or a checklist tick on an
// unassigned, unwatched task is not something to page a lead for.
func (k ChangeKind) mayFallBack() bool {
	switch k {
	case ChangeCreated, ChangeStatus, ChangeMoved, ChangeRemoved,
		ChangeComment, ChangeRouted:
		return true
	}
	return false
}

// Route is the post-filter — who is actually woken — with the tracker's one
// reason that reaches the person who made the change ([Reason.WakesActor]).
//
// Its four steps are [routing.Route]'s: drop the inbox-only candidates, drop
// the actor except under `unblocked`, drop handles the company no longer
// employs, and keep the lowest-ranked fallback still employed only when no
// ordinary candidate survived — the step that is the whole reason the
// fallback [Candidates] offers is a list. The registry is asked only about a
// handle those steps could wake, never about the actor or about a chain
// somebody ordinary made unnecessary.
func Route(candidates []routing.Candidate[Reason], registry func(string) bool,
	actor string) []routing.Candidate[Reason] {

	return routing.Route(candidates, registry, actor, Reason.WakesActor)
}

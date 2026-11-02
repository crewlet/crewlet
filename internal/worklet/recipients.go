package worklet

import "slices"

// Who hears about a change, and why the answer is TWO functions.
//
// [Candidates] is pure over the notification: it reads the kind, the mentions
// and the routing snapshot, and nothing else — no clock, no org, no registry,
// no actor. That is what lets the APPLIER call it inside its transaction (it
// reads no config and no clock, so the determinism rules stay literally true)
// and the wake path call it on a different node minutes later, and get the
// same answer.
//
// [Route] is the post-filter, and it needs two facts a record cannot carry:
// who the company employs RIGHT NOW, and who made the change. Splitting them
// is what makes the applier's `tracker_notifications` row a complete record of
// everyone the change concerned, while the wake goes only to the people still
// there — and why the parity test can assert that the two surfaces agree.

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
// all". `answered` and `thread` are replies to something the seat wrote;
// `unblocked`, `routed_to`, `parent_assignee`, `checklist` and `blocking` move
// work the seat owns; `mention`, `prioritised` and `asked` put something to
// it; `purged` takes work out of the lead's project. None of these is
// something a seat should first learn about when it happens to read the task
// again.
var turnReasons = []Reason{
	ReasonMention, ReasonPrioritised, ReasonAsked, ReasonAnswered,
	ReasonThread, ReasonUnblocked, ReasonRoutedTo, ReasonParentAssignee,
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
func (r Reason) StartsTurn(asked bool) bool {
	switch {
	case slices.Contains(turnReasons, r):
		return true
	case slices.Contains(ownerReasons, r), r == ReasonWatcher:
		return asked
	}
	return false
}

// Candidate is one handle and why it is a candidate.
type Candidate struct {
	Handle string
	Reason Reason

	// Addressed marks a wake that asks something of its recipient — a
	// turn that must answer rather than absorb.
	Addressed bool

	// FallbackOnly marks a candidate kept only when no ordinary one
	// survives, and FallbackRank orders those among themselves.
	FallbackOnly bool
	FallbackRank int

	// InboxOnly marks a candidate written to the inbox and never woken.
	InboxOnly bool

	// Wakes marks a candidate whose notice starts a turn ([Reason.StartsTurn]).
	// One that does not is still written to the inbox, and still counts as
	// somebody who heard about the change when [Route] decides whether a
	// fallback lead is needed.
	Wakes bool

	// Task names the task this candidate is about when it is NOT the
	// subject of the change — which today is the unblocked notice alone.
	//
	// It is what makes that notice survive the one-reason-per-handle rule:
	// the person who closed ENG-1 and owns the ENG-2 it unblocked would
	// otherwise be deduped under `assignee` for ENG-1 and dropped as the
	// actor, and would never learn that ENG-2 became workable — which is
	// the entire feature.
	Task string
}

// WakesActor reports the one reason that reaches the person who made the
// change.
//
// An unblocked notice is about somebody ELSE's task becoming workable, so the
// actor closing their own blocker is exactly who needs to hear it. Every other
// reason is dropped for the actor, because telling somebody what they just did
// is noise a model spends a round on.
func (r Reason) WakesActor() bool { return r == ReasonUnblocked }

// Candidates is every handle a change concerns, in precedence order.
//
// IT RETURNS NOTHING FOR A NIL NOTIFICATION, and that single rule is what
// keeps the two surfaces in step: the translator acks a quiet commit so the
// feed never routes one, and the applier calls this same function, so a quiet
// commit leaves no notification row either. Without it a quiet walk would
// write rows nobody would ever be woken for, and an inbox would fill with
// changes the company deliberately did not announce.
func Candidates(n *Notify) []Candidate {
	if n == nil {
		return nil
	}
	task := n.Kind.TaskCommit()
	finishedEdge := n.Snapshot.PrevStatusGroup.Finished() != n.Snapshot.StatusGroup.Finished()
	wakes := func(reason Reason) bool { return reason.StartsTurn(n.asks(reason)) }

	var out []Candidate
	// seen is where each handle's one candidate sits, so a later reason
	// that names the same handle can still decide its wake.
	seen := map[string]int{}
	add := func(handle string, reason Reason, addressed bool) {
		if handle == "" {
			return
		}
		// THE FIRST REASON IS WHAT THE HANDLE IS TOLD, AND ANY REASON
		// WAKES IT. Precedence decides which fact a recipient reads, and
		// a stronger fact that starts no turn must not swallow a weaker
		// one that does: the reporter of a task who also wrote in the
		// thread a reply lands in is told "a task you reported", and is
		// still woken, because the reply is to something they wrote.
		if i, ok := seen[handle]; ok {
			out[i].Wakes = out[i].Wakes || wakes(reason)
			return
		}
		seen[handle] = len(out)
		out = append(out, Candidate{
			Handle: handle, Reason: reason, Addressed: addressed,
			Wakes: wakes(reason),
		})
	}
	addAll := func(handles []string, reason Reason, addressed bool) {
		for _, h := range handles {
			add(h, reason, addressed)
		}
	}

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
	// THE UNBLOCKED NOTICES ARE NOT DEDUPED AGAINST THE REST, because they
	// are about OTHER tasks: one commit legitimately tells one person both
	// "your task is done" and "this other one is now workable", and the
	// second is the only wake that reaches the person who made the change.
	for _, party := range n.Snapshot.Unblocked {
		if party.Assignee == "" {
			continue
		}
		out = append(out, Candidate{
			Handle: party.Assignee, Reason: ReasonUnblocked, Task: party.Task,
			Wakes: wakes(ReasonUnblocked),
		})
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
	if n.Kind == ChangeWatchers {
		for _, h := range n.Snapshot.RemovedWatchers {
			if _, ok := seen[h]; h == "" || ok {
				continue
			}
			seen[h] = len(out)
			out = append(out, Candidate{
				Handle: h, Reason: ReasonUnwatched, InboxOnly: true,
			})
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
		for rank, handle := range []string{
			n.Snapshot.RoutingUnitLead, n.Snapshot.ProjectLead,
		} {
			if _, ok := seen[handle]; handle == "" || ok {
				continue
			}
			seen[handle] = len(out)
			out = append(out, Candidate{
				Handle: handle, Reason: ReasonLeadFallback,
				FallbackOnly: true, FallbackRank: rank,
				Wakes: wakes(ReasonLeadFallback),
			})
		}
	}
	return out
}

// CandidateFor is how handle hears the change n describes, as [Candidates]
// decides it: its reason, whether its notice starts a turn and whether that
// turn is asked to answer; false when the change does not concern it. An
// unblocked notice, which is about another task, is not this change's.
//
// FOR A WRITER THAT TELLS ITS CALLER WHO WILL ACT, so what it says is the
// routing's own answer for the record it is about to publish, never a second
// copy of the rule that drifts from it.
func CandidateFor(n *Notify, handle string) (Candidate, bool) {
	for _, c := range Candidates(n) {
		if c.Handle == handle && c.Task == "" {
			return c, true
		}
	}
	return Candidate{}, false
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
func (n *Notify) assigneeAddressed(finishedEdge bool) bool {
	switch n.Kind {
	case ChangeCreated, ChangeAssignee:
		return true
	case ChangeStatus:
		return finishedEdge
	case ChangeComment:
		return n.personComment()
	}
	return false
}

// ownerAsked decides whether the change wakes the seats that answer for the
// task ([ownerReasons]): every change to it except another agent's
// conversation on it and a change to who watches it.
//
// ANOTHER AGENT'S COMMENT WAKES THE OWNER ONLY BY NAMING IT, with an
// @-mention, an `ask` or a reply in a thread the owner wrote in, each a reason
// of its own that starts a turn whatever this answers. Measured before this
// rule, the owner's turns woken by an unaddressed agent comment ended in a
// reply comment 64% of the time, most of them an acknowledgement that woke
// the other agent to acknowledge it back, and changed the work in 14%; the
// comment tool tells an agent so, and its result warns when a comment reaches
// the assignee's inbox and no further, reading this rule through
// [CandidateFor]. A person's comment is the ask, and wakes the owner whatever
// it names.
func (n *Notify) ownerAsked() bool {
	if n.Kind.Conversation() {
		return n.personComment()
	}
	return n.Kind != ChangeWatchers
}

// watcherAsked decides whether the change wakes a WATCHER, which follows the
// task rather than answering for it: a person's comment and nothing else. A
// person writing on a task speaks to whoever follows it, without naming
// anybody; every other change reaches a watcher's inbox, and a seat that
// needs a watcher to act names it.
func (n *Notify) watcherAsked() bool {
	return n.Kind.Conversation() && n.personComment()
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

// Route is the post-filter: who is actually woken.
//
// Five steps, in this order. The fourth is the whole reason the fallback is a
// list, and the fifth is why it comes last:
//
//  1. Drop the inbox-only candidates — they are a record, not a wake.
//  2. Drop handles the company no longer employs.
//  3. Drop the actor, except under the one reason that wakes them.
//  4. If no ordinary candidate survived, keep the LOWEST-RANKED surviving
//     fallback and only that one.
//  5. Keep only the candidates whose notice starts a turn ([Candidate.Wakes]).
//
// THE FIFTH STEP RUNS AFTER THE FALLBACK IS DECIDED, never before it. A task
// whose watchers heard about a change has an audience, and that is what the
// fallback asks; filtering the watchers out first would make every quiet
// comment on a watched task reach a lead instead, which is the noise this
// step exists to remove.
//
// ONE NAME IS THE ACTOR. A person bound to a seat writes AS that seat — the
// author iam.ActorFor records for them — so the handle their own gestures
// land on is the author itself, and a credential nobody is bound through
// writes, and is watched, under its own login.
func Route(candidates []Candidate, registry func(string) bool, actor string) []Candidate {
	var ordinary, fallback []Candidate
	for _, c := range candidates {
		if c.InboxOnly {
			continue
		}
		if registry != nil && !registry(c.Handle) {
			continue
		}
		if c.Handle == actor && !c.Reason.WakesActor() {
			continue
		}
		if c.FallbackOnly {
			fallback = append(fallback, c)
			continue
		}
		ordinary = append(ordinary, c)
	}
	if len(ordinary) > 0 {
		return waking(ordinary)
	}
	if len(fallback) == 0 {
		return nil
	}
	best := fallback[0]
	for _, c := range fallback[1:] {
		if c.FallbackRank < best.FallbackRank {
			best = c
		}
	}
	return waking([]Candidate{best})
}

// waking is the fifth step of [Route]: the candidates whose notice starts a
// turn, in the order given.
func waking(heard []Candidate) []Candidate {
	out := heard[:0:0]
	for _, c := range heard {
		if c.Wakes {
			out = append(out, c)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

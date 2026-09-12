package tracker

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
	ReasonGoalOwner      Reason = "goal_owner"
	ReasonSprint         Reason = "sprint"
	ReasonWatcher        Reason = "watcher"
	ReasonUnwatched      Reason = "unwatched"
	ReasonPurged         Reason = "purged"
	ReasonLeadFallback   Reason = "lead_fallback"
)

// Reasons are the twenty, IN PRECEDENCE ORDER.
//
// THE ORDER IS THE RULE: the first reason that names a handle is the one that
// handle hears under, and every later one for the same handle is dropped. A
// person who is mentioned AND watching is told they were mentioned, which is
// the stronger fact and the one they will act on.
var Reasons = []Reason{
	ReasonMention, ReasonPrioritised, ReasonAssignee, ReasonUnassigned,
	ReasonReporter, ReasonAsked, ReasonAnswered, ReasonThread,
	ReasonUnblocked, ReasonBlocking, ReasonRoutedTo, ReasonParentAssignee,
	ReasonChecklist, ReasonCollaborator, ReasonGoalOwner, ReasonSprint,
	ReasonWatcher, ReasonUnwatched, ReasonPurged, ReasonLeadFallback,
}

// Valid reports whether a reason off the wire is one this build knows.
func (r Reason) Valid() bool { return slices.Contains(Reasons, r) }

// primaryReasons is the eight an inbox shows first.
//
// PRIMARY IS "SOMETHING IS BEING ASKED OF YOU OR WAS DIRECTED AT YOU", not
// "important": routed_to is deliberately Other, because "work routes to you
// now" is a fact to absorb rather than an obligation.
var primaryReasons = []Reason{
	ReasonMention, ReasonAsked, ReasonAssignee, ReasonReporter,
	ReasonAnswered, ReasonPrioritised, ReasonThread, ReasonBlocking,
}

// Primary reports whether a reason belongs in an inbox's first section.
func (r Reason) Primary() bool { return slices.Contains(primaryReasons, r) }

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
//
// `batched` is an argument rather than a field of the notification because it
// is a property of the CALL that produced the record — a bulk sprint plan is
// thirty facts to absorb rather than thirty asks — and the notification is
// about one change.
func Candidates(n *Notify, batched bool) []Candidate {
	if n == nil {
		return nil
	}
	var out []Candidate
	seen := map[string]bool{}
	add := func(handle string, reason Reason, addressed bool) {
		if handle == "" || seen[handle] {
			return
		}
		seen[handle] = true
		out = append(out, Candidate{
			Handle: handle, Reason: reason, Addressed: addressed && !batched,
		})
	}
	addAll := func(handles []string, reason Reason, addressed bool) {
		for _, h := range handles {
			add(h, reason, addressed)
		}
	}

	task := n.Kind.TaskCommit()
	finishedEdge := n.Snapshot.PrevStatusGroup.Finished() != n.Snapshot.StatusGroup.Finished()

	addAll(n.Mentions, ReasonMention, true)
	if n.Kind == ChangePrioritised {
		add(n.Snapshot.Person, ReasonPrioritised, true)
	}
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
	add(n.Snapshot.CommentAsk, ReasonAsked, true)
	add(n.Snapshot.AnsweredAuthor, ReasonAnswered, false)
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
		})
	}
	if n.Kind == ChangeRelations && len(n.Snapshot.Dependents) > 0 {
		add(n.Snapshot.Assignee, ReasonBlocking, false)
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
	if n.Kind == ChangeGoalUpdated {
		addAll(n.Snapshot.GoalOwners, ReasonGoalOwner, false)
		addAll(n.Snapshot.GoalMembers, ReasonGoalOwner, false)
	}
	if n.Kind == ChangeSprintStarted || n.Kind == ChangeSprintClosed {
		addAll(n.Snapshot.SprintAssignees, ReasonSprint, false)
		add(n.Snapshot.ProjectLead, ReasonSprint, false)
	}
	if task {
		addAll(n.Snapshot.Watchers, ReasonWatcher, false)
	}
	if n.Kind == ChangeWatchers {
		for _, h := range n.Snapshot.RemovedWatchers {
			if h == "" || seen[h] {
				continue
			}
			seen[h] = true
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
			if handle == "" || seen[handle] {
				continue
			}
			seen[handle] = true
			out = append(out, Candidate{
				Handle: handle, Reason: ReasonLeadFallback,
				FallbackOnly: true, FallbackRank: rank,
			})
		}
	}
	return out
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
		ChangeRouted, ChangeMoved, ChangeReparented, ChangeSprint,
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
// Four steps, in this order, and the last one is the whole reason the fallback
// is a list:
//
//  1. Drop the inbox-only candidates — they are a record, not a wake.
//  2. Drop handles the company no longer employs.
//  3. Drop the actor, except under the one reason that wakes them.
//  4. If no ordinary candidate survived, keep the LOWEST-RANKED surviving
//     fallback and only that one.
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
		return ordinary
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
	return []Candidate{best}
}

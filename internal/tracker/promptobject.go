package tracker

import (
	"strings"

	"github.com/crewlet/crewlet/internal/notify"
)

// What a change to something that is NOT a task asks of the seat it reached.
//
// # Why these are separate frames rather than a branch in the task prompt
//
// Because almost none of the task prompt applies. Its openers all begin "A
// task…", its header is labelled **Task:**, its context block sends the reader
// to `get_work_item`, and its handling block is four steps about owning a work
// item — assignee, status, one substantive comment. A goal has no assignee and
// a sprint has no status, so a branch inside each of those would be four
// branches through one paragraph, and every later edit to the task wording
// would have to be read four ways.
//
// # Why they exist at all
//
// Three of the four routable object kinds are not tasks, and for a long time
// the parser simply dropped them — which is why [Writer.WritePriorities] came
// to argue, in its own doc comment, that a notification "attached to a person
// record renders no card and reaches nobody". That was TRUE of the code and
// false of the design: the routing rules for all three already existed in
// recipients.go, the reasons were already in the enum and already split
// Primary from Other, and the snapshot already carried GoalOwners,
// SprintAssignees and Person. Only the two ends were missing — nobody
// published, and nothing could render it if they had.
//
// # None of the three sends the reader to `get_work_item`
//
// A pointer at a task is exactly what these wakes do not have, and inventing
// one costs the seat a round and a failed tool call to discover. Each names
// the tool that actually answers its own question instead — which is also why
// `list_work_goals` had to stop being operator-only in the same change: a wake
// telling a seat its goal moved, reaching a seat with no way to read a goal,
// is not a feature.
func buildObjectPrompt(kind ObjectKind, n notify.Inbound, parties notify.Parties) string {
	var b strings.Builder
	switch kind {
	case KindGoal:
		promptGoal(&b, n, parties)
	case KindSprint:
		promptSprint(&b, n, parties)
	case KindPerson:
		promptPriorities(&b, n, parties)
	default:
		// UNREACHABLE BY CONSTRUCTION — [ObjectKind.Routable] is the
		// closed set the parser gates on, and this is its other half. A
		// kind added to one and not the other lands here, and an empty
		// prompt is the honest outcome: a wake with nothing to say is
		// better than a wake that says the wrong noun.
		return ""
	}
	return b.String()
}

// promptGoal: an outcome this seat owns or works toward has moved.
//
// NOT ADDRESSED, and the wording keeps it that way. A goal's owners are told
// so they can re-plan, not so they can reply — the plan puts `goal_owner` in
// the Other half of the inbox split for exactly that reason, and a prompt that
// asked for an answer would produce a "noted, thanks" on every health update.
func promptGoal(b *strings.Builder, n notify.Inbound, parties notify.Parties) {
	b.WriteString("A goal you are named on changed.")
	objectHeader(b, n, parties, "Goal")

	b.WriteString("\n## What this is" +
		"\nA goal is an outcome somebody committed the company to, with owners" +
		" who report on it. You are named on this one as an owner or a member," +
		" which is why you are hearing about it.\n")

	b.WriteString("\n## Get full context" +
		"\nRead it with `" + ListWorkGoalsTool + "` — its targets, their" +
		" current values and the health updates written against it.\n")

	b.WriteString("\n## How to handle this" +
		"\n1. **Check the targets that count YOUR work.** A goal's progress is" +
		" computed from the tasks its targets name, so a target that moved" +
		" says something about work somebody is doing — possibly yours." +
		"\n2. **If the health moved to at_risk or off_track**, look at what you" +
		" own under it and decide whether your plan still reaches the target." +
		" Re-prioritise your own work if it does not." +
		"\n3. **Do not reply to the goal itself.** There is nothing to answer" +
		" here — this is news to absorb and act on in the work, not a" +
		" question. If you conclude the target is unreachable, say so on the" +
		" task where the work actually is, with `" + CommentOnWorkTool +
		"`, and mention the person who raised it.\n")
}

// promptSprint: the window this seat plans into opened or closed.
func promptSprint(b *strings.Builder, n notify.Inbound, parties notify.Parties) {
	meta := n.Metadata
	closed := ChangeKind(meta[MetaChangeKind]) == ChangeSprintClosed
	if closed {
		b.WriteString("A sprint you had work in closed.")
	} else {
		b.WriteString("A sprint you have work in started.")
	}
	objectHeader(b, n, parties, "Sprint")

	project := meta[MetaProject]
	scope := ""
	if project != "" {
		scope = ", project: " + project
	}

	if closed {
		b.WriteString("\n## What this means" +
			"\nThe sprint is over. **No task's status changed** — a close is" +
			" about the sprint, not about the work in it. What was unfinished" +
			" is still yours; where it went next is in the line above.\n")
		b.WriteString("\n## Get full context" +
			"\nRead how it went with `" + SprintReportTool + "`(" +
			strings.TrimPrefix(scope, ", ") + ") — what was committed, what" +
			" was added mid-sprint, what landed and what was left.\n")
		b.WriteString("\n## How to handle this" +
			"\n1. **Find your own unfinished work** with `" + MyWorkTool +
			"` and confirm where it landed — the next sprint, the backlog, or" +
			" cancelled." +
			"\n2. **If something of yours was cancelled by the close** and" +
			" should not have been, say so on the task with `" +
			CommentOnWorkTool + "` rather than silently reopening it." +
			"\n3. **Do not re-plan the sprint.** Starting, closing and settling" +
			" a sprint's spillover are the project lead's decisions.\n")
		if lead := meta[MetaProjectLead]; lead != "" {
			b.WriteString("\nIf the rollover is still pending, **" + lead +
				"** settles it with `" + ManageSprintTool +
				"` — it is their call, not yours.\n")
		}
		return
	}

	b.WriteString("\n## Get full context" +
		"\nSee what is in it with `" + ListWorkItemsTool + "`(sprint: active" +
		scope + "), and your own share of it with `" + MyWorkTool + "`.\n")
	b.WriteString("\n## How to handle this" +
		"\n1. **Look at what you committed to.** The sprint's window is what" +
		" every figure in its report is measured over, so work that lands" +
		" outside it does not count toward it." +
		"\n2. **If you are over capacity**, say so now rather than at the" +
		" close — comment on the task you cannot reach with `" +
		CommentOnWorkTool + "` and name who should pick it up." +
		"\n3. **Do not add or remove work from the sprint on your own.** What" +
		" a team took on together is the lead's to change.\n")
}

// promptPriorities: somebody else wrote this seat's own priority list.
//
// THE ONE ADDRESSED WAKE of the three, and the only one of the four kinds the
// plan marks "a seat starts it, or says why not". Being told what to do next
// by somebody above you is an instruction, and a seat that absorbed it
// silently would leave the person who wrote the list unable to tell whether it
// was seen.
func promptPriorities(b *strings.Builder, n notify.Inbound, parties notify.Parties) {
	meta := n.Metadata
	who := promptSender(n, parties)
	b.WriteString(who + " set your priorities.")

	b.WriteString("\n\n**Your priorities:** changed by " + who)
	if body := strings.TrimSpace(n.Body); body != "" {
		b.WriteString("\n**What changed:** " + body)
	}
	if key := meta[MetaTaskKey]; key != "" {
		b.WriteString("\n**Top of your list:** " + key)
		if title := meta[MetaTitle]; title != "" {
			b.WriteString(" — " + title)
		}
	}
	b.WriteString("\n")

	b.WriteString("\n## What this is" +
		"\nYour personal priority list is an ordered list of work somebody" +
		" wants you to take up first. It is not the same thing as a task's" +
		" priority field, and it is not a board order — it is your own queue," +
		" and this change to it was made by somebody else.\n")

	if key := meta[MetaTaskKey]; key != "" {
		b.WriteString("\n## Get full context" +
			"\nRead **" + key + "** with `" + GetWorkItemTool + "` before" +
			" deciding anything, and see the whole list with `" + MyWorkTool +
			"` — its first block is your priorities, in the order " + who +
			" put them.\n")
	} else {
		b.WriteString("\n## Get full context" +
			"\nSee the list with `" + MyWorkTool + "` — its first block is" +
			" your priorities, in the order somebody put them.\n")
	}

	b.WriteString("\n## How to handle this" +
		"\nThis is now your first priority: **take it up, or say why you" +
		" cannot.**" +
		"\n1. **If you are taking it**, move it to an active status with `" +
		UpdateWorkItemTool + "` so " + who + " can see it started." +
		"\n2. **If you cannot** — it is blocked, it is not yours, you are" +
		" already committed elsewhere — comment with `" + CommentOnWorkTool +
		"` mentioning " + who + " and say which, naming what you are doing" +
		" instead." +
		"\n3. **Do not reorder the list back.** You may write your own" +
		" priorities, but quietly undoing somebody else's is how two people" +
		" end up believing different things about what you are working on." +
		"\n\nGoing silent is the one wrong answer here: an unacknowledged" +
		" priority looks exactly like a message that was lost.\n")
}

// objectHeader is the identifying block the goal and sprint frames share.
//
// It deliberately does NOT render a status or an assignee: neither a goal nor
// a sprint has one, and an empty line under a bold label reads as missing data
// rather than as data that does not exist.
func objectHeader(b *strings.Builder, n notify.Inbound, parties notify.Parties,
	label string) {

	meta := n.Metadata
	b.WriteString("\n\n**" + label + ":** " + n.Subject)
	if lead := objectLead(meta, promptSender(n, parties)); lead != "" {
		b.WriteString("\n**What happened:** " + lead)
	}
	b.WriteString("\n**By:** " + promptSender(n, parties))
	if project := meta[MetaProject]; project != "" {
		b.WriteString("\n**Project:** " + project)
	}
	if body := strings.TrimSpace(n.Body); body != "" {
		b.WriteString("\n**Detail:** " + body)
	}
	b.WriteString("\n")
}

// objectLead names what happened to a non-task object in one line.
func objectLead(meta map[string]string, actor string) string {
	by := ""
	if actor != "" {
		by = " by " + actor
	}
	switch ChangeKind(meta[MetaChangeKind]) {
	case ChangeGoalUpdated:
		return "The goal was updated" + by + "."
	case ChangeSprintStarted:
		return "The sprint was started" + by + "."
	case ChangeSprintClosed:
		return "The sprint was closed" + by + "."
	case ChangePrioritised:
		return "Your priority list was written" + by + "."
	}
	return ""
}

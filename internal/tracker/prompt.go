package tracker

import (
	"strings"

	"github.com/crewlet/crewlet/internal/notify"
)

// Prompt is what a change to a task asks of the seat it reached.
//
// # It tailors to the ROUTING REASON, not to the change kind
//
// One assignment reaches the new assignee, several watchers and possibly the
// unit lead, and it asks each of them for something different: the assignee
// owns the work, a watcher is following it, and the lead is there only because
// nobody else was named. A prompt keyed on the change kind would have to tell
// all three the same thing, which in practice means telling all three the
// weakest thing. The parser already decided which a copy is and stamped it, so
// this reads that back rather than re-deriving a judgement the router made
// with more information.
//
// # Twenty reasons, THREE FRAMES
//
// The router distinguishes twenty reasons because the routing rules genuinely
// differ — a blocker's dependent and a goal's owner are reached by different
// paths. What a RECIPIENT needs to be told collapses to three: somebody is
// asking you, this is your work, or this is activity you follow. A prompt with
// twenty openers would be twenty places for one sentence to drift, and the
// reason itself is rendered in the header either way.
//
// # This prompt DOES name tools, and that is the exception rather than a lapse
//
// The tool-capabilities doctrine says the engine must not name tools, because
// a deployed MCP server's names are not knowable by the engine. That reasoning
// does not reach here: these tools are shipped by this build, registered by
// this build under names this build chose, and present on every seat that can
// read this notification at all.
type Prompt struct{}

var _ notify.Prompt = Prompt{}

// Source implements [notify.Prompt].
func (Prompt) Source() string { return Source }

// RequiresRecon implements [notify.Prompt].
//
// TRUE FOR A POINTER, FALSE FOR PROSE, and the split matters more than it
// looks: the flag suppresses the turn-start relevance filtering AND the
// personal-memory and episode recall that go with it, so setting it on a wake
// that already carries what it means costs the seat its own context for
// nothing.
//
// A COMMENT CARRIES ITS TEXT — the excerpt is what somebody actually said — so
// the seat can begin reasoning from the trigger. A bare transition carries no
// prose at all and is a pointer at a task the seat must read first.
func (Prompt) RequiresRecon(n notify.Inbound) bool {
	if n.Metadata[MetaTaskKey] == "" {
		return false
	}
	switch ChangeKind(n.EventType) {
	case ChangeComment, ChangeCommentEdited:
		return strings.TrimSpace(n.Body) == ""
	case ChangePrioritised, ChangeSprintClosed:
		// NEITHER IS A POINTER. A `prioritised` wake names the task, the
		// person who put it there and the position, and a closed sprint
		// carries its own figures — so the seat can begin reasoning from
		// the trigger, and turning the recon on would cost it the
		// personal memory and episode recall that the flag suppresses,
		// for a fetch of something it was already told.
		//
		// `prioritised` reaches this switch at all only because it is the
		// one non-task wake that DOES carry a task key: the task at the
		// top of the list. Without this arm the key alone would have
		// turned it into a pointer.
		return false
	}
	return true
}

// Addressed implements [notify.Prompt]: somebody is waiting on THIS seat.
//
// The eight primary reasons and no others. A watcher is following the task
// rather than being asked about it, and a fallback is the task landing
// somewhere rather than on somebody — a seat obliged to answer either would
// comment on every field change in its unit's projects.
func (Prompt) Addressed(n notify.Inbound) bool {
	return Reason(n.Metadata[MetaVia]).Primary()
}

// ConversationKey implements [notify.Prompt]: the task is the conversation.
//
// THE KEY rather than the uuid, because the key is what a person pastes into
// chat and what a seat writes in a commit message — so a chat thread about
// ENG-42 and the tracker activity on it land in one ledger, which is the whole
// point of a conversation key.
func (Prompt) ConversationKey(metadata map[string]string, _ string) string {
	if key := metadata[MetaTaskKey]; key != "" {
		return key
	}
	// A NON-TASK WAKE KEYS ON ITS OWN OBJECT. Falling through to an empty
	// key would put every goal update in the company into ONE ledger
	// together with every sprint close — a conversation key is what
	// separates threads, and a shared empty one merges them all.
	if id := metadata[MetaObjectID]; id != "" {
		return metadata[MetaObject] + ":" + id
	}
	return ""
}

// WakesActor implements [notify.Prompt].
//
// ONE REASON DOES, and it is the one the router already names: a person who
// closed a blocker learns from this that the task it was blocking is now
// workable, which is a CONSEQUENCE of their own write rather than a repeat of
// it. Every other change here is something the actor already knows.
func (Prompt) WakesActor(via string) bool { return Reason(via).WakesActor() }

// DigestBody implements [notify.Prompt]: comments keep their text, field and
// status changes collapse to their lead.
//
// A comment is something a person SAID and each one is different. A field
// change's body is a delta line the digest's own lead already states, so five
// of them in a coalesced trigger is the same sentence five times, burying the
// comment underneath.
func (Prompt) DigestBody(eventType, body string) string {
	switch ChangeKind(eventType) {
	case ChangeComment, ChangeCommentEdited:
		return body
	}
	return ""
}

// Build implements [notify.Prompt].
func (Prompt) Build(n notify.Inbound, parties notify.Parties) string {
	meta := n.Metadata
	// THE OBJECT DECIDES THE FRAME, before the reason does. Every opener
	// below says "A task…", the header is labelled **Task:** and the
	// context block sends the reader to get_work_item — none of which is
	// true of a goal, a sprint or a person's priority list.
	//
	// AN ABSENT KEY IS A TASK, which is what keeps a record written by an
	// older build rendering exactly as it did: this metadata arrived with
	// the non-task wakes, and a rolling upgrade puts records without it on
	// the wire in both directions.
	if kind := ObjectKind(meta[MetaObject]); kind != "" && kind != KindTask {
		return buildObjectPrompt(kind, n, parties)
	}
	reason := Reason(meta[MetaVia])
	var b strings.Builder

	promptOpener(&b, n, parties, reason)

	promptChanged(&b, n)
	promptContext(&b, meta)
	if isFallback(reason) {
		promptWhyYou(&b, meta[MetaProject])
	}
	promptHandling(&b, meta, reason)
	return b.String()
}

// isFallback reports a reason that reached this seat because nobody else was
// named, rather than because of anything about them.
//
// ONE REASON, and it is named rather than derived from the candidate's own
// FallbackOnly flag: the flag is gone by the time a prompt runs — routing
// resolved it and stamped what survived — so the prompt reads the reason it
// was told, which is the only thing it has.
func isFallback(reason Reason) bool { return reason == ReasonLeadFallback }

// promptOpener writes the one sentence that says why this seat is reading
// this, and — for the reasons that are ABOUT something somebody said — the
// text itself.
//
// ONE SWITCH, and the shape is deliberate. This was three functions chosen by
// [Reason.Primary], and that split put six arms where their own reason could
// never reach them: `blocking` was written in the "following" half while
// Primary sends it to the "owned" one, and `unblocked` and `collaborator` the
// other way round — so a blocker's assignee read "a task you are named on
// changed" and somebody whose work had just become startable read "a task you
// are watching changed". Each arm looked right beside the others in its own
// function, and nothing could see the pairing was wrong.
//
// EVERY REASON HAS AN ARM, and the default is the honest sentence for one a
// newer build routed that this one does not know: a rolling upgrade puts such
// a wake on the wire, and "a task you are named on changed" is true of every
// reason there is.
func promptOpener(b *strings.Builder, n notify.Inbound, parties notify.Parties,
	reason Reason) {

	quoted := false
	switch reason {
	case ReasonMention:
		b.WriteString("You were @-mentioned in a comment on a task.")
		quoted = true
	case ReasonAsked:
		b.WriteString("You were asked a question on a task, and the person " +
			"who asked is waiting for an answer.")
		quoted = true
	case ReasonAnswered:
		b.WriteString("A question you asked on a task was answered.")
		quoted = true
	case ReasonThread:
		b.WriteString("A comment thread you are part of has a new reply.")
		quoted = true
	case ReasonPrioritised:
		b.WriteString("Somebody else set the order of your work queue.")
	case ReasonAssignee:
		switch ChangeKind(n.EventType) {
		case ChangeCreated:
			b.WriteString("A new task was filed with you as its assignee.")
		case ChangeRemoved:
			b.WriteString("A task you were assigned to was removed.")
		case ChangeAssignee:
			b.WriteString("A task was assigned to you.")
		default:
			b.WriteString("A task you are assigned to changed.")
		}
	case ReasonUnassigned:
		b.WriteString("A task you were assigned to went to somebody else.")
	case ReasonReporter:
		b.WriteString("A task you filed changed.")
	case ReasonUnblocked:
		b.WriteString("A task you are waiting on is now workable — every " +
			"blocker on it has finished.")
	case ReasonBlocking:
		// THE BLOCKER'S OWN ASSIGNEE, and the sentence has to say whose
		// problem it is: this seat holds the task somebody else is now
		// waiting for, which is a call on THEIR time rather than news
		// about somebody else's work.
		b.WriteString("Somebody's work now waits on a task of yours.")
	case ReasonRoutedTo:
		b.WriteString("Work routes to your team now: a task was pointed at it.")
	case ReasonParentAssignee:
		b.WriteString("A subtask of a task you hold finished or was reopened.")
	case ReasonChecklist:
		b.WriteString("A checklist item assigned to you changed.")
	case ReasonCollaborator:
		b.WriteString("A task you are collaborating on changed.")
	case ReasonGoalOwner:
		b.WriteString("A goal you own or are part of changed.")
	case ReasonSprint:
		b.WriteString("A sprint your work is in started or closed.")
	case ReasonWatcher:
		b.WriteString("A task you are watching changed.")
	case ReasonUnwatched:
		b.WriteString("Somebody removed your watch on a task.")
	case ReasonPurged:
		// NOT "changed". The task and everything on it were destroyed,
		// and a reader who opens this expecting an edit goes looking for
		// what moved — on a row that is gone.
		b.WriteString("A task in your project was permanently destroyed.")
	case ReasonLeadFallback:
		b.WriteString("A task in your team's project has activity and nobody " +
			"here is named on it.")
	default:
		b.WriteString("A task you are named on changed.")
	}
	promptHeader(b, n, parties)
	if !quoted {
		return
	}
	body := n.Body
	if body == "" {
		body = "(no text)"
	}
	b.WriteString("\n**Comment:**\n" + body + "\n")
}

// promptHeader is the identifying block every opener shares.
func promptHeader(b *strings.Builder, n notify.Inbound, parties notify.Parties) {
	meta := n.Metadata
	b.WriteString("\n\n**Task:** " + n.Subject)
	if lead := changeLead(meta, promptSender(n, parties)); lead != "" {
		b.WriteString("\n**What happened:** " + lead)
	}
	b.WriteString("\n**By:** " + promptSender(n, parties))
	if project := meta[MetaProject]; project != "" {
		b.WriteString("\n**Project:** " + project)
	}
	if status := meta[MetaStatus]; status != "" {
		b.WriteString("\n**Status:** " + status)
	}
	if assignee := meta[MetaAssignee]; assignee != "" {
		b.WriteString("\n**Assignee:** " + assignee)
	}
	if meta[MetaLate] == "true" {
		// WHY THEY ARE HEARING THIS NOW. A repair reaches somebody hours
		// after the change, and a reader who cannot tell that from a
		// fresh event re-reads a thread looking for what just moved.
		b.WriteString("\n**Note:** this notice is a repair — the change " +
			"itself happened earlier and the wake for it did not reach you.")
	}
	b.WriteString("\n")
}

// changeLead names what happened in one line.
//
// Worth stating plainly because the kind alone does not: a seat that had to
// infer "assigned to you" from a delta map would guess.
func changeLead(meta map[string]string, actor string) string {
	by := ""
	if actor != "" {
		by = " by " + actor
	}
	switch ChangeKind(meta[MetaChangeKind]) {
	case ChangeCreated:
		return "The task was filed" + by + "."
	case ChangeAssignee:
		return "The assignee changed" + by + "."
	case ChangeStatus:
		return "The status changed" + by + "."
	case ChangeComment:
		return "A comment was added" + by + "."
	case ChangeCommentEdited:
		return "A comment was edited" + by + "."
	case ChangeCommentResolved:
		return "A comment was resolved" + by + "."
	case ChangeCommentRemoved:
		return "A comment was removed" + by + "."
	case ChangeRelations:
		return "The task's relations changed" + by + "."
	case ChangeWatchers:
		return "The watchers changed" + by + "."
	case ChangeCollaborators:
		return "The collaborators changed" + by + "."
	case ChangeFields:
		return "Fields were edited" + by + "."
	case ChangeTags:
		return "The tags changed" + by + "."
	case ChangeSprint:
		return "The sprint changed" + by + "."
	case ChangeChecklist:
		return "A checklist changed" + by + "."
	case ChangeReparented:
		return "The task was re-parented" + by + "."
	case ChangeMoved:
		return "The task moved project" + by + "."
	case ChangeRouted:
		return "The task was routed to another unit" + by + "."
	case ChangeArchived:
		return "The task was archived" + by + "."
	case ChangeRemoved:
		return "The task was removed" + by + "."
	case ChangeRestored:
		return "The task was restored" + by + "."
	case ChangePurged:
		// THE ONE WITH NO INVERSE, and the line says so: every other kind
		// here describes something a reader could undo or answer.
		return "The task and everything on it were permanently destroyed" +
			by + ". This cannot be undone."
	}
	return ""
}

// promptChanged renders the delta the record already carries.
//
// NOT ON A COMMENT, whose body is the comment itself and is rendered by the
// asked opener — repeating it under a "What changed" heading reads as two
// different things having happened.
func promptChanged(b *strings.Builder, n notify.Inbound) {
	switch ChangeKind(n.EventType) {
	case ChangeComment, ChangeCommentEdited:
		return
	}
	if n.Body != "" {
		b.WriteString("\n## What changed\n" + n.Body + "\n")
	}
}

// promptContext is the recon pointer.
func promptContext(b *strings.Builder, meta map[string]string) {
	key := meta[MetaTaskKey]
	if key == "" {
		return
	}
	switch ChangeKind(meta[MetaChangeKind]) {
	case ChangeRemoved, ChangePurged:
		// NOTHING TO FETCH. Sending a seat to read a task that no longer
		// exists costs it a round and a failed tool call — and on a
		// purge the row is not in the trash either, so the tool answers
		// `not_found` rather than offering a restore.
		return
	}
	b.WriteString("\n## Get full context" +
		"\nRead **" + key + "** with `" + GetWorkItemTool + "` — its " +
		"type, status, priority, description, links and the whole comment " +
		"thread — before deciding on next steps. Do not act on partial " +
		"information.\n")
	if url := meta["url"]; url != "" {
		b.WriteString("Link: " + url + "\n")
	}
}

// promptWhyYou is the delegate / take it / escalate decision.
//
// REACHED ONLY FROM A FALLBACK ROUTING. A directed routing carries its own
// signal — being assigned or named says what is wanted — while a fallback says
// only that nobody else here was available, which is a fact about the org
// chart rather than about the work. Left unexplained, a lead reads it as "this
// is mine" and quietly absorbs every unowned task in their project.
func promptWhyYou(b *strings.Builder, project string) {
	where := "this project"
	if project != "" {
		where = "the **" + project + "** project"
	}
	b.WriteString("\n## Why you received this" +
		"\nThis reached you because you lead the team that owns " + where +
		", and the task names nobody here — no assignee on your team, no" +
		" colleague watching it, no @-mention. You are the default owner by" +
		" FALLBACK, not because the work is yours. This fires only when nobody" +
		" else here is involved, so nobody else is watching it — if you walk" +
		" away silently, it goes nowhere." +
		"\n\nDecide one of:" +
		"\n- **Delegate** — set the assignee to the right teammate with `" +
		UpdateWorkItemTool + "`. Future changes route to them rather" +
		" than back to you." +
		"\n- **Take it yourself** — only if the work clearly falls to you." +
		" Assign it to yourself so the routing reflects reality from now on." +
		"\n- **Escalate** — if it is out of scope or you cannot identify the" +
		" right owner, hand it to your own manager (named in your identity" +
		" prompt): comment mentioning them with `" + CommentOnWorkTool +
		"`, or reassign it to them. Either keeps the trail on the task.\n")
}

// promptHandling is the mechanics block.
//
// Tracker conventions only. Role-specific behaviour — ownership, tone, the
// quality bar — belongs in the role's behavioural guidelines, and repeating it
// here would put one team's standards on every seat.
func promptHandling(b *strings.Builder, meta map[string]string, reason Reason) {
	if ChangeKind(meta[MetaChangeKind]) == ChangePurged {
		// A DIFFERENT INSTRUCTION FROM A REMOVAL, because the remedies
		// differ: a removed task is in the trash and `restore_task`
		// brings it back, and a purged one is gone from every node with
		// no gesture that returns it. Telling a lead to "record any
		// progress worth keeping" is right in both; telling them the
		// task can come back is right in only one.
		b.WriteString("\n## How to handle this" +
			"\nThe task, its comments, its history and its turn records were" +
			" destroyed on every node, and nothing restores them. Stop any" +
			" in-flight work on it. If anybody on your team had progress worth" +
			" keeping, it now exists only where they wrote it down." +
			"\n\nYou are hearing this because you lead the project it was" +
			" filed in. No action is required of you beyond knowing.\n")
		return
	}
	if ChangeKind(meta[MetaChangeKind]) == ChangeRemoved {
		b.WriteString("\n## How to handle this" +
			"\nThe task no longer exists — stop any in-flight work on it. If" +
			" you had progress worth keeping, record it where your team tracks" +
			" such things; otherwise no action is needed.\n")
		return
	}
	if isFallback(reason) {
		// The delegate / take it / escalate block above already IS this
		// seat's instruction, and a second list under a second heading
		// makes a lead read two sets of steps and follow neither.
		return
	}

	b.WriteString("\n## How to handle this" +
		"\n1. **Are you the right owner?** If the task has an assignee who is" +
		" not you and not on your team, observe and stay silent — comment only" +
		" if you hold decision-blocking information they cannot see." +
		"\n2. **Has this already been addressed?** Read the existing comments" +
		" first. If your prior comment, or a teammate's, already covers the" +
		" question, do not restate it." +
		"\n3. **If you are acting**, move the task to an active status with `" +
		UpdateWorkItemTool + "`, make sure the assignee names you, and" +
		" post ONE substantive summary with `" + CommentOnWorkTool +
		"` when you are done." +
		` Avoid running commentary ("starting work", "still on it") — that is` +
		" noise on a surface other people are reading." +
		"\n4. **If anything is unclear**, comment mentioning the reporter and" +
		" ask. Do not guess.")

	if reason.Primary() {
		// A WATCHER IS NOT BEING ASKED. Watchers are on the task because
		// the participants rule put them there, so telling one they owe
		// an answer is precisely how a tracker fills up with "noted,
		// thanks" — the running-commentary rule two lines up, produced
		// by the prompt that forbade it.
		b.WriteString(
			"\n5. **If you were assigned or @-mentioned and have decided not" +
				" to act** — out of scope, wrong owner, already handled — do" +
				" NOT go quiet. Reassign it to the right teammate, or comment" +
				" naming who should own it. If you cannot identify them," +
				" mention your own manager so they can route it. An unanswered" +
				" assignment or mention looks exactly like a message that was" +
				" lost.")
	}
	b.WriteString("\n\n**Never** post internal thinking, status" +
		` acknowledgements, or "I agree with X" as comments. Substance only,` +
		" and stay on this task.\n")
}

// promptSender renders the actor as a colleague.
//
// Through the party registry first, because the actor is a HANDLE here and a
// handle is not what a person is called — a prompt rendering "eng" where a
// reader expects "Engineer (eng)" makes the roster and the notification look
// like two different companies.
func promptSender(n notify.Inbound, parties notify.Parties) string {
	actor := n.Metadata[notify.ActorField]
	if actor == "" {
		actor = n.Sender
	}
	if parties != nil && actor != "" {
		if party, ok := parties.ByHandle(actor); ok {
			if label := party.Label(); label != "" {
				return label
			}
		}
	}
	if actor == "" {
		return "someone"
	}
	return actor
}

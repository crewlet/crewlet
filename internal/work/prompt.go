package work

import (
	"strings"

	"github.com/crewlet/crewlet/internal/notify"
)

// Prompt is what a change to a work item asks of the seat it reached.
//
// # It tailors to the ROUTING REASON, not to the change kind
//
// One assignment reaches the new assignee, two watchers and possibly the unit
// lead, and it asks each of them for something different: the assignee owns
// the work, a watcher is following it, and the lead is there only because
// nobody else was named. A prompt keyed on the change kind would have to tell
// all three the same thing, which in practice means telling all three the
// weakest thing. [Parser] already decided which a copy is and stamped it, so
// this reads that back rather than re-deriving a judgement the router made
// with more information.
//
// # This prompt DOES name tools, and that is the exception rather than a lapse
//
// The tool-capabilities doctrine says the engine must not name tools, because
// a deployed MCP server's names are not knowable by the engine — a prompt
// saying "use jira_get_issue" is wrong on every company that named its server
// something else. That reasoning does not reach here: these tools are shipped
// by this build, registered by this build under names this build chose, and
// present on every seat that can read this notification at all. Describing
// them by capability instead would cost a round of catalogue-hunting on every
// wake to avoid a coupling that does not exist. See
// docs/concepts/tool-capabilities.md.
type Prompt struct{}

var _ notify.Prompt = Prompt{}

// Source implements [notify.Prompt].
func (Prompt) Source() string { return Source }

// RequiresRecon implements [notify.Prompt].
//
// TRUE FOR A POINTER, FALSE FOR PROSE, and the split matters more than it
// looks: the flag suppresses the turn-start relevance filtering AND the
// personal-memory and episode recall that go with it, so setting it on a
// wake that already carries what it means costs the seat its own context for
// nothing.
//
// A COMMENT CARRIES ITS TEXT — the excerpt is what somebody actually said —
// so the seat can begin reasoning from the trigger. A bare transition
// ("status todo → in_progress") carries no prose at all and is a pointer at
// an item the seat must read first.
func (Prompt) RequiresRecon(n notify.Inbound) bool {
	if n.Metadata[MetaItemKey] == "" {
		return false
	}
	switch ChangeKind(n.EventType) {
	case ChangeComment, ChangeCommentEdited:
		return strings.TrimSpace(n.Body) == ""
	}
	return true
}

// Addressed implements [notify.Prompt]: a mention or an assignment.
//
// The same two reasons the prompt frames as a directed ask. A watcher is
// following the item rather than being asked about it, and the lead fallback
// is the item landing somewhere rather than on somebody: a seat obliged to
// answer either would comment on every field change in its unit's projects.
func (Prompt) Addressed(n notify.Inbound) bool { return Addressed(n.Metadata) }

// ConversationKey implements [notify.Prompt]: the item is the conversation.
//
// The ITEM KEY rather than the uuid, because the key is what a person pastes
// into chat and what a seat writes in a commit message — so a chat thread
// about ENG-42 and the tracker activity on it land in one ledger, which is
// the whole point of a conversation key.
func (Prompt) ConversationKey(metadata map[string]string, _ string) string {
	return metadata[MetaItemKey]
}

// WakesActor implements [notify.Prompt]: never.
//
// Every change here is one the actor already knows about: their own comment,
// their own transition, their own assignment. The exception exists for a
// vendor reporting a consequence the actor could not have foreseen — a
// failing pipeline — and a tracker has none.
func (Prompt) WakesActor(string) bool { return false }

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
	var b strings.Builder

	switch meta[RoutedViaField] {
	case ViaAssignee:
		promptAssigned(&b, n, parties)
	case ViaMention:
		promptMentioned(&b, n, parties)
	default:
		// The watcher fan-out, the lead fallback, and any reason a later
		// parser adds. Evaluate-and-stay-silent is the right default for
		// activity the recipient merely follows, and the right default
		// for a reason this prompt has not been taught.
		promptWatching(&b, n, parties)
	}

	promptWhatChanged(&b, n)
	promptGetContext(&b, meta)
	if meta[RoutedViaField] == ViaLeadFallback {
		promptWhyRouted(&b, meta[MetaProject])
	}
	promptHandling(&b, meta)
	return b.String()
}

// promptAssigned: the work is theirs.
func promptAssigned(b *strings.Builder, n notify.Inbound, parties notify.Parties) {
	switch ChangeKind(n.EventType) {
	case ChangeCreated:
		b.WriteString("A new work item was filed with you as its assignee.")
	case ChangeRemoved:
		b.WriteString("A work item you were assigned to was removed.")
	case ChangeAssignee:
		b.WriteString("A work item was assigned to you.")
	default:
		b.WriteString("A work item you are assigned to changed.")
	}
	promptHeader(b, n, parties)
}

// promptMentioned: somebody named them.
func promptMentioned(b *strings.Builder, n notify.Inbound, parties notify.Parties) {
	b.WriteString("You were @-mentioned in a comment on a work item.")
	promptHeader(b, n, parties)
	body := n.Body
	if body == "" {
		body = "(no text)"
	}
	b.WriteString("\n**Comment:**\n" + body + "\n")
}

// promptWatching: activity on an item they follow, or that reached them as
// the lead of the owning team.
func promptWatching(b *strings.Builder, n notify.Inbound, parties notify.Parties) {
	if n.Metadata[RoutedViaField] == ViaLeadFallback {
		b.WriteString("A work item in your team's project has activity" +
			" and nobody here is named on it.")
	} else {
		b.WriteString("A work item you are watching changed.")
	}
	promptHeader(b, n, parties)
}

// promptHeader is the identifying block every opener shares.
func promptHeader(b *strings.Builder, n notify.Inbound, parties notify.Parties) {
	meta := n.Metadata
	b.WriteString("\n\n**Item:** " + n.Subject)
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
		return "The item was filed" + by + "."
	case ChangeAssignee:
		return "The assignee changed" + by + "."
	case ChangeStatus:
		return "The status changed" + by + "."
	case ChangeComment:
		return "A comment was added" + by + "."
	case ChangeCommentEdited:
		return "A comment was edited" + by + "."
	case ChangeCommentRemoved:
		return "A comment was removed" + by + "."
	case ChangeLinks:
		return "The item's links changed" + by + "."
	case ChangeWatchers:
		return "The watchers changed" + by + "."
	case ChangeFields:
		return "Fields were edited" + by + "."
	case ChangeRemoved:
		return "The item was removed" + by + "."
	}
	return ""
}

// promptWhatChanged renders the delta the record already carries.
//
// NOT ON A COMMENT, whose body is the comment itself and is rendered by the
// mention opener — repeating it under a "What changed" heading reads as two
// different things having happened.
func promptWhatChanged(b *strings.Builder, n notify.Inbound) {
	switch ChangeKind(n.EventType) {
	case ChangeComment, ChangeCommentEdited:
		return
	}
	if n.Body != "" {
		b.WriteString("\n## What changed\n" + n.Body + "\n")
	}
}

// promptGetContext is the recon pointer.
//
// It names the tools, for the reason the type doc gives: they are this
// build's own, registered under names this build chose, on every seat that
// can read this at all.
func promptGetContext(b *strings.Builder, meta map[string]string) {
	key := meta[MetaItemKey]
	if key == "" {
		return
	}
	if ChangeKind(meta[MetaChangeKind]) == ChangeRemoved {
		// Nothing to fetch. Sending a seat to read an item that no longer
		// exists costs it a round and a failed tool call.
		return
	}
	b.WriteString("\n## Get full context" +
		"\nRead **" + key + "** with `get_work_item` — its type, status," +
		" priority, description, links and the whole comment thread — before" +
		" deciding on next steps. Do not act on partial information.\n")
	if url := meta["url"]; url != "" {
		b.WriteString("Link: " + url + "\n")
	}
}

// promptWhyRouted is the delegate / take it / escalate decision.
//
// REACHED ONLY FROM THE LEAD ROUTING. A directed routing carries its own
// signal — being assigned or named says what is wanted — while a lead routing
// says only that nobody else here was available, which is a fact about the
// org chart rather than about the work. Left unexplained, a lead reads it as
// "this is mine" and quietly absorbs every unowned item in their project.
func promptWhyRouted(b *strings.Builder, project string) {
	where := "this project"
	if project != "" {
		where = "the **" + project + "** project"
	}
	b.WriteString("\n## Why you received this" +
		"\nThis reached you because you lead the team that owns " + where +
		", and the item names nobody here — no assignee on your team, no" +
		" colleague watching it, no @-mention. You are the default owner by" +
		" FALLBACK, not because the work is yours. This fires only when nobody" +
		" else here is involved, so nobody else is watching it — if you walk" +
		" away silently, it goes nowhere." +
		"\n\nDecide one of:" +
		"\n- **Delegate** — set the assignee to the right teammate with" +
		" `update_work_item`. Future changes route to them rather than back" +
		" to you." +
		"\n- **Take it yourself** — only if the work clearly falls to you." +
		" Assign it to yourself so the routing reflects reality from now on." +
		"\n- **Escalate** — if it is out of scope or you cannot identify the" +
		" right owner, hand it to your own manager (named in your identity" +
		" prompt): comment mentioning them with `comment_on_work_item`, or" +
		" reassign it to them. Either keeps the trail on the item.\n")
}

// promptHandling is the mechanics block.
//
// Tracker conventions only. Role-specific behaviour — ownership, tone, the
// quality bar — belongs in the role's behavioural guidelines, and repeating
// it here would put one team's standards on every seat.
func promptHandling(b *strings.Builder, meta map[string]string) {
	if ChangeKind(meta[MetaChangeKind]) == ChangeRemoved {
		b.WriteString("\n## How to handle this" +
			"\nThe item no longer exists — stop any in-flight work on it. If" +
			" you had progress worth keeping, record it where your team tracks" +
			" such things; otherwise no action is needed.\n")
		return
	}
	if meta[RoutedViaField] == ViaLeadFallback {
		// The delegate / take it / escalate block above already IS this
		// seat's instruction, and a second list under a second heading
		// makes a lead read two sets of steps and follow neither.
		return
	}

	b.WriteString("\n## How to handle this" +
		"\n1. **Are you the right owner?** If the item has an assignee who is" +
		" not you and not on your team, observe and stay silent — comment only" +
		" if you hold decision-blocking information they cannot see." +
		"\n2. **Has this already been addressed?** Read the existing comments" +
		" first. If your prior comment, or a teammate's, already covers the" +
		" question, do not restate it." +
		"\n3. **If you are acting**, move the item to an active status with" +
		" `update_work_item`, make sure the assignee names you, and post ONE" +
		" substantive summary with `comment_on_work_item` when you are done." +
		` Avoid running commentary ("starting work", "still on it") — that is` +
		" noise on a surface other people are reading." +
		"\n4. **If anything is unclear**, comment mentioning the reporter and" +
		" ask. Do not guess.")

	if promptDirectAsk(meta[RoutedViaField]) {
		// A WATCHER IS NOT BEING ASKED. Watchers are on the item because
		// the participants rule put them there, so telling one they owe
		// an answer is precisely how a tracker fills up with "noted,
		// thanks" — the running-commentary rule two lines up, produced by
		// the prompt that forbade it.
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
		" and stay on this item.\n")
}

// promptDirectAsk reports a routing that asks the recipient for something.
func promptDirectAsk(via string) bool {
	return via == "" || via == ViaAssignee || via == ViaMention
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
		// BY HANDLE, because the actor on a first-party change IS a
		// handle — nothing here authenticates as an account somewhere
		// else, so there is no external id to scope.
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

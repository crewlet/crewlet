package tracker

import (
	"strings"

	"github.com/crewlet/crewlet/internal/notify"
)

// What a change to something that is NOT a task asks of the seat it reached.
//
// # Why this is a separate frame rather than a branch in the task prompt
//
// Because almost none of the task prompt applies. Its openers all begin "A
// task…", its header is labelled **Task:**, its context block sends the reader
// to `get_work_item`, and its handling block is four steps about owning a work
// item — assignee, status, one substantive comment. A priority list has no
// assignee at all, so a branch inside the task frame would be branches through
// one paragraph, and every later edit to the task wording would have to be
// read twice.
//
// # Why it exists at all
//
// One of the two routable object kinds is not a task, and for a long time the
// parser simply dropped it — which is why [Writer.WritePriorities] came to
// argue, in its own doc comment, that a notification "attached to a person
// record renders no card and reaches nobody". That was TRUE of the code and
// false of the design: the routing rule already existed in recipients.go, the
// reason was already in the enum and already split Primary from Other, and the
// snapshot already carried Person. Only the two ends were missing — nobody
// published, and nothing could render it if they had.
//
// # It does not send the reader to `get_work_item` for the object itself
//
// A pointer at a task is exactly what this wake does not have, and inventing
// one costs the seat a round and a failed tool call to discover. It names the
// tool that actually answers its own question instead.
func buildObjectPrompt(kind ObjectKind, n notify.Inbound, parties notify.Parties) string {
	var b strings.Builder
	switch kind {
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

// promptPriorities: somebody else wrote this seat's own priority list.
//
// AN ADDRESSED WAKE, and the only non-task kind the plan marks "a seat starts
// it, or says why not". Being told what to do next by somebody above you is an
// instruction, and a seat that absorbed it silently would leave the person who
// wrote the list unable to tell whether it was seen.
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

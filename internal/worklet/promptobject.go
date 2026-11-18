package worklet

import (
	"strings"

	"github.com/crewlet/crewlet/internal/notify"
)

// buildObjectPrompt frames a change to something that is NOT a task for the
// seat it reached. Its own frame, not a branch of the task prompt: none of
// that applies (openers saying "A task…", a **Task:** header, handling steps
// that own a work item's assignee and status), and a priority list has no
// assignee. Nor does it send the reader to `get_work_item` for the object:
// there is no task to point at, and inventing one costs the seat a round and
// a failed tool call. It names the tool that answers its own question.
func buildObjectPrompt(kind ObjectKind, n notify.Inbound, parties notify.Parties) string {
	var b strings.Builder
	switch kind {
	case KindPerson:
		promptPriorities(&b, n, parties)
	default:
		// Unreachable by construction: [ObjectKind.Routable] is the closed set
		// the parser gates on. A kind added there and not here gets an empty
		// prompt — a wake with nothing to say beats one naming the wrong noun.
		return ""
	}
	return b.String()
}

// promptPriorities: somebody else wrote this seat's own priority list. An
// addressed wake, the one non-task kind a seat must start or say why not: an
// instruction absorbed silently leaves its writer unable to tell it was seen.
// The delivery check enforces that it does not end in silence; this says how
// each answer is given, which only the tracker knows.
func promptPriorities(b *strings.Builder, n notify.Inbound, parties notify.Parties) {
	meta := n.Metadata
	who := promptSender(n, parties)
	b.WriteString(who + " set your priorities.")

	b.WriteString("\n\n**Your priorities:** changed by " + who)
	if body := strings.TrimSpace(n.Body); body != "" {
		b.WriteString("\n**What changed:** " + body)
	}
	if key := metaAddress(meta); key != "" {
		b.WriteString("\n**Top of your list:** " + key)
		if title := meta[MetaTitle]; title != "" {
			b.WriteString(" — " + title)
		}
		promptCollision(b, meta)
	}
	b.WriteString("\n")

	b.WriteString("\n## What this is" +
		"\nYour personal priority list is an ordered list of work somebody" +
		" wants you to take up first. It is not the same thing as a task's" +
		" priority field, and it is not a board order — it is your own queue," +
		" and this change to it was made by somebody else.\n")

	if key := metaAddress(meta); key != "" {
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
		" end up believing different things about what you are working on.\n")
}

package plane

import (
	"strings"

	"github.com/crewlet/crewlet/internal/notify"
)

// The prompt: what a Plane webhook asks of the seat it reached.
//
// # It tailors to the ROUTING REASON, not to the event type
//
// The same "issue.updated" payload reaches an assignee, a thread subscriber
// and a project lead, and it asks each of them for something different: the
// assignee owns the work, the subscriber is watching it, and the lead is
// there only because nobody else was named. A prompt that keyed on the event
// would have to tell all three the same thing, which in practice means
// telling all three the weakest thing.
//
// The parser already decided which of those a copy is and stamped it as
// [RoutedViaField], so the prompt reads that back rather than re-deriving a
// judgement the router already made with more information than the prompt
// has.
//
// NOTHING HERE NAMES A TOOL. The prompt describes the capability — "fetch the
// work item", "set the assignee" — and lets the model find the matching tool
// in its own catalogue, because the deployed MCP server's tool names are not
// knowable by the engine. See docs/concepts/tool-capabilities.md.
type Prompt struct{}

var _ notify.Prompt = Prompt{}

// Source implements [notify.Prompt].
func (Prompt) Source() string { return Backend }

// RequiresRecon implements [notify.Prompt]: always, for an event that names
// an entity.
//
// A Plane webhook is a POINTER. It says a work item changed and carries one
// field's before and after; it does not carry the state, the priority, the
// other assignees or the comment thread that make the change mean anything.
// The seat has to fetch the entity before it can act, which is exactly what
// the "Get full context" block below tells it to do — so the two answer on
// the same condition, and an event naming neither entity is one the seat can
// read whole.
func (Prompt) RequiresRecon(n notify.Inbound) bool {
	return entityOf(n.Metadata) != ""
}

// ConversationKey implements [notify.Prompt]: the entity is the conversation.
//
// KEYED ON THE UUID, never the human "ENG-42". The UUID rides every issue AND
// comment payload; the sequence number rides only the issue one, and the
// project identifier in front of it needs a warm cache. Keying on the display
// key would split one work item's comments and its field updates into two
// coalescing partitions the moment either was missing — and the failure is
// silent, because two partitions each look like a perfectly ordinary
// conversation.
func (Prompt) ConversationKey(metadata map[string]string, _ string) string {
	return entityOf(metadata)
}

// WakesActor implements [notify.Prompt]: never.
//
// A tracker reports what somebody DID, not the outcome of what they did.
// Every event here is one the actor already knows about — their own comment,
// their own assignment, their own edit — and the parser drops the actor's
// copy at the door. The exception exists for a vendor that reports a
// consequence the actor could not have seen coming, which is a code host's
// failing pipeline, not a work item whose description was just saved.
func (Prompt) WakesActor(string) bool { return false }

// DigestBody implements [notify.Prompt]: comments keep their body, state
// snapshots collapse.
//
// On issue.created and issue.updated the body is the description AS IT WAS —
// a full snapshot re-sent on every field change. Five of them in a coalesced
// digest is the same paragraph five times, burying the one line that moved.
// Only the latest matters and it is rendered in full below the digest, so the
// intermediate copies collapse to their event lead. A COMMENT is a message
// rather than a snapshot: each one is something a person actually said.
func (Prompt) DigestBody(eventType, body string) string {
	if strings.HasPrefix(eventType, "issue_comment") {
		return body
	}
	return ""
}

// Build implements [notify.Prompt].
func (Prompt) Build(n notify.Inbound, parties notify.Parties) string {
	meta := n.Metadata
	var b strings.Builder
	switch meta[RoutedViaField] {
	case ViaAssignee, ViaAssigneeAdded:
		assigned(&b, n, parties)
	case ViaMention:
		mentioned(&b, n, parties)
	case ViaIntake:
		intake(&b, n, parties)
		whyRouted(&b, ViaIntake, meta["project"])
	case ViaPageLead:
		pageChanged(&b, n, parties)
	case ViaLeadFallback:
		watching(&b, n, parties)
		whyRouted(&b, ViaLeadFallback, meta["project"])
	default:
		// Subscriber fan-out, and anything a later parser adds. The
		// evaluate-and-stay-silent framing is the right default for
		// thread activity the recipient merely watches, and the right
		// default for a reason this prompt has not been taught.
		watching(&b, n, parties)
	}
	getFullContext(&b, meta)
	return b.String()
}

// assigned: the work is theirs.
func assigned(b *strings.Builder, n notify.Inbound, parties notify.Parties) {
	meta := n.Metadata
	deleted := meta["event_type"] == "issue.deleted"

	switch {
	case meta[RoutedViaField] == ViaAssigneeAdded:
		b.WriteString("You have been assigned a Plane work item.")
	case meta["event_type"] == "issue.created":
		b.WriteString("A new Plane work item was created with you as an assignee.")
	case deleted:
		b.WriteString("A Plane work item you were assigned to was deleted.")
	default:
		b.WriteString("A Plane work item you are assigned to was updated.")
	}
	b.WriteString("\n\n**Work item:** " + workItemRef(meta) + " — " + n.Subject +
		"\n**By:** " + senderLabel(n, parties) + "\n")
	if n.Body != "" {
		b.WriteString("\n**Description:**\n" + n.Body + "\n")
	}

	if deleted {
		// No fetch to send them on and no work to do: the entity is
		// gone, and the only thing worth saying is stop.
		b.WriteString("\n## How to handle this" +
			"\nThe work item no longer exists — stop any in-flight work on it." +
			" If you had progress worth keeping, record it where your team" +
			" tracks such things; otherwise no action is needed.\n")
		return
	}
	b.WriteString("\n## How to handle this" +
		"\n1. **Read the work item in full** before starting — state," +
		" priority, description, and the existing comments." +
		"\n2. **Do the work it describes.** Move it to an active state, and" +
		" post ONE substantive summary when you are done. Avoid running" +
		` commentary ("starting work", "still on it") — that is noise on a` +
		" surface other people are reading." +
		"\n3. **If you have decided not to act** — out of scope, wrong" +
		" owner, already handled — do NOT go quiet. Reassign it to the" +
		" right teammate (look them up to resolve their Plane user id), or" +
		" comment naming who should own it. An unanswered direct assignment" +
		" looks exactly like a message that was lost.\n")
}

// mentioned: somebody named them in a comment.
func mentioned(b *strings.Builder, n notify.Inbound, parties notify.Parties) {
	body := n.Body
	if body == "" {
		body = "(no text)"
	}
	b.WriteString("You were @-mentioned in a comment on a Plane work item.\n\n" +
		"**Work item:** " + workItemRef(n.Metadata) + " — " + n.Subject +
		"\n**By:** " + senderLabel(n, parties) +
		"\n\n**Comment:**\n" + body + "\n")
	b.WriteString(notify.EvaluationBlock())
}

// intake: Plane's unassigned-inbound surface.
func intake(b *strings.Builder, n notify.Inbound, parties notify.Parties) {
	b.WriteString("A Plane intake work item needs triage.\n\n" +
		"**Title:** " + n.Subject +
		"\n**Submitted by:** " + senderLabel(n, parties) + "\n")
	if n.Body != "" {
		b.WriteString("\n**Description:**\n" + n.Body + "\n")
	}
	b.WriteString("\nIntake is Plane's unassigned-inbound surface: the request" +
		" sits outside the normal backlog until somebody accepts, declines or" +
		" assigns it.\n")
}

// pageChanged: a knowledge page moved.
func pageChanged(b *strings.Builder, n notify.Inbound, parties notify.Parties) {
	meta := n.Metadata
	switch meta["event_type"] {
	case "page.created":
		b.WriteString("A page was created in your team's Plane project.")
	case "page.deleted":
		b.WriteString("A page was deleted from your team's Plane project.")
	default:
		b.WriteString("A page in your team's Plane project was updated.")
	}
	b.WriteString("\n\n**Page:** " + n.Subject)
	if project := meta["project"]; project != "" {
		b.WriteString("\n**Project:** " + project)
	}
	b.WriteString("\n**By:** " + senderLabel(n, parties) + "\n")
	// The routing reason is worth stating plainly here, because it is
	// unusual: this is not a page the seat subscribed to. Plane pages carry
	// no watchers, so page activity has exactly one place to go.
	b.WriteString("\nYou received this as the lead of the unit that owns the" +
		" project — Plane pages have no per-page watchers, so page activity" +
		" routes to you by default.\n" +
		"\n## How to handle this" +
		"\nStay silent if the page is unrelated to your team's work. If it is" +
		" relevant, read the change and decide whether your plans need" +
		" adjusting, or flag it to the teammate it concerns." +
		" Acknowledgement-only replies are noise.\n")
}

// watching: thread activity on something the recipient is merely following.
func watching(b *strings.Builder, n notify.Inbound, parties notify.Parties) {
	subject := n.Subject
	if subject == "" {
		subject = "Plane " + n.Metadata["event_type"]
	}
	b.WriteString("A Plane work item you follow was updated.\n\n" +
		"**Work item:** " + workItemRef(n.Metadata) + " — " + subject +
		"\n**By:** " + senderLabel(n, parties) + "\n")
	if n.Body != "" {
		b.WriteString("\n**Update:**\n" + n.Body + "\n")
	}
	b.WriteString(notify.EvaluationBlock())
}

// whyRouted is the delegate / take it / escalate decision.
//
// REACHED ONLY FROM THE TWO LEAD ROUTINGS, and that restriction is the point.
// A directed routing carries its own signal — being assigned or named says
// what is wanted — while a lead routing says only that nobody else was
// available, which is a fact about the org chart rather than about the work.
// Left unexplained, a lead reads it as "this is mine" and quietly absorbs
// every unowned ticket in their project.
//
// The reason is a PARAMETER rather than something read back out of the
// metadata, because [Prompt.Build]'s dispatch has already decided it. Reading
// it back would invite a guard here re-checking that decision — a second
// place the restriction lives, which cannot fire and so cannot be wrong in a
// way anything would catch, while quietly claiming the restriction is weaker
// than the dispatch makes it.
func whyRouted(b *strings.Builder, via, project string) {
	where := "this Plane project"
	if project != "" {
		where = "the **" + project + "** Plane project"
	}

	b.WriteString("\n## Why you received this\n")
	if via == ViaIntake {
		b.WriteString("This intake item was routed to you because you lead the" +
			" team that owns " + where + " — intake requests have no owner" +
			" until somebody triages them, so they reach the project lead by" +
			" default. If you walk away silently, the request goes nowhere.")
	} else {
		b.WriteString("This event was routed to you because you lead the team" +
			" that owns " + where + ", and the work item has no assignee. You" +
			" are the default owner by FALLBACK, not because the work is" +
			" yours. Lead-fallback fires only when nobody else is involved, so" +
			" nobody else is watching this item — if you walk away silently," +
			" it goes nowhere.")
	}
	b.WriteString("\n\nDecide one of:" +
		"\n- **Delegate** — assign the work item to the right teammate. Look" +
		" them up on your team to resolve their Plane user id, then set the" +
		" assignee. Future updates route to them rather than back to you." +
		"\n- **Take it yourself** — only if the work clearly falls to you." +
		" Assign the item to yourself so the routing reflects reality from" +
		" now on." +
		"\n- **Escalate** — if it is out of scope or you cannot identify the" +
		" right owner, hand it to your own manager (named in your identity" +
		" prompt): comment on the item mentioning them, or reassign it to" +
		" them. Either keeps the trail on the item, so the work is not lost.\n")
}

// getFullContext is the recon pointer, on the same condition
// [Prompt.RequiresRecon] answers.
//
// The two must agree: the flag tells the Plan phase not to bother filtering
// against a pointer, and this block is what makes the pointer followable. A
// flag set without the block sends a seat looking with nothing to look for.
func getFullContext(b *strings.Builder, meta map[string]string) {
	issue, page := meta["issue_id"], meta["page_id"]
	switch {
	case issue != "":
		ref := "work item `" + issue + "`"
		if key := meta["work_item_key"]; key != "" {
			ref = "**" + key + "**"
		}
		b.WriteString("\n## Get full context" +
			"\nFetch the full details for " + ref + " with your Plane tools —" +
			" state, priority, assignees, description, comments, linked items" +
			" — before deciding on next steps. Do not act on partial" +
			" information.\n")
	case page != "":
		b.WriteString("\n## Get full context" +
			"\nFetch the full content of page `" + page + "` with your Plane" +
			" tools before deciding on next steps.\n")
	default:
		return
	}
	if url := meta["url"]; url != "" {
		b.WriteString("Link: " + url + "\n")
	}
}

// entityOf is the thing this event is ABOUT — the one identity the
// conversation key and the recon flag both rest on.
//
// A work item first: a comment payload names both, and the conversation is
// the item rather than the comment, or every comment on one ticket would open
// its own coalescing partition.
func entityOf(metadata map[string]string) string {
	return firstOf(metadata["issue_id"], metadata["page_id"])
}

// workItemRef is the display reference: "ENG-42" where the key resolved, the
// UUID otherwise, and "?" where the event named neither.
//
// The UUID is worth rendering even though nobody can read it — it is what a
// seat pastes into a fetch, and a reference the model cannot use at all is
// strictly worse than an ugly one.
func workItemRef(metadata map[string]string) string {
	return firstOf(metadata["work_item_key"], metadata["issue_id"], "?")
}

// senderLabel renders the actor as a colleague.
//
// Through the party registry FIRST, because the payload's actor is a
// workspace UUID and the registry is the only thing that can turn it into
// "Ana Ruiz (ana, human colleague)" — which is what tells the seat whether it
// is being asked by a person who replies on their own time or by an agent it
// can reach directly. The display name is the fallback for a workspace member
// who is not a seat here, and it is a real answer rather than a degraded one:
// most people in a tracker are not in the org chart.
func senderLabel(n notify.Inbound, parties notify.Parties) string {
	meta := n.Metadata
	if parties != nil {
		if actor := meta[notify.ActorField]; actor != "" {
			if party, ok := parties.ByExternalID(Backend, actor); ok {
				if label := party.Label(); label != "" {
					return label
				}
			}
		}
	}
	return firstOf(meta["actor_name"], n.Sender, "someone")
}

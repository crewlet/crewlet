package confluence

import (
	"strings"

	"github.com/crewlet/crewlet/internal/notify"
)

// Prompt is what a Confluence event asks of the seat it reached.
//
// # A wiki change is rarely urgent, and the prompt has to say so
//
// Every other vendor's event is somebody asking for something. A page edit
// usually is not: it is documentation moving, and the right answer is
// almost always to read it, decide it changes nothing, and stay silent. A
// prompt that framed it as a request would produce a stream of "noted,
// thanks" comments on a wiki — which is worse than the noise, because it
// trains a reader to ignore the page's comments entirely.
//
// The one exception is a MENTION, which is somebody asking a question on a
// page, and that is the one routing this prompt frames as an ask.
type Prompt struct{}

var _ notify.Prompt = Prompt{}

// Source implements [notify.Prompt].
func (Prompt) Source() string { return Backend }

// RequiresRecon implements [notify.Prompt]: whenever the event names a page.
//
// The notification carries a SNIPPET, deliberately — a wiki page can be tens
// of kilobytes and putting one into every recipient's prompt for an event
// most of them will read and drop is the wrong trade. So the trigger is a
// pointer by construction, and the seat fetches the page if it means to act.
func (Prompt) RequiresRecon(n notify.Inbound) bool {
	return n.Metadata["page_id"] != ""
}

// ConversationKey implements [notify.Prompt]: the page is the conversation.
//
// Keyed on the page id, so a burst of edits and the comments about them
// coalesce into one trigger rather than one turn each — which is what a wiki
// produces when somebody restructures a space.
func (Prompt) ConversationKey(metadata map[string]string, _ string) string {
	return metadata["page_id"]
}

// WakesActor implements [notify.Prompt]: never. A page reports what somebody
// did, and the person who did it was there.
func (Prompt) WakesActor(string) bool { return false }

// DigestBody implements [notify.Prompt]: a comment keeps its text, a page
// snapshot collapses.
//
// On a page event the body is the page AS IT IS — re-sent on every save — so
// five saves in a coalesced digest is the same page five times. A COMMENT is
// something a person actually said.
func (Prompt) DigestBody(eventType, body string) string {
	if strings.HasPrefix(eventType, "comment_") {
		return body
	}
	return ""
}

// Build implements [notify.Prompt].
func (Prompt) Build(n notify.Inbound, parties notify.Parties) string {
	meta := n.Metadata
	var b strings.Builder
	if meta[RoutedViaField] == ViaMention {
		mentioned(&b, n, parties)
	} else {
		spaceActivity(&b, n, parties)
	}
	getFullContext(&b, meta)
	return b.String()
}

// mentioned: somebody named this seat on a page or in a comment on one.
func mentioned(b *strings.Builder, n notify.Inbound, parties notify.Parties) {
	b.WriteString("You were mentioned on a Confluence page.")
	header(b, n, parties)
	if n.Body != "" {
		b.WriteString("\n**What it says:**\n" + n.Body + "\n")
	}
	b.WriteString(notify.EvaluationBlock())
}

// spaceActivity: a page moved in a space this seat's team owns.
func spaceActivity(b *strings.Builder, n notify.Inbound, parties notify.Parties) {
	switch n.Metadata["event_type"] {
	case "page_created", "blog_created":
		b.WriteString("A page was created in your team's Confluence space.")
	case "page_trashed", "page_removed":
		b.WriteString("A page was deleted from your team's Confluence space.")
	case "comment_created", "comment_updated":
		b.WriteString("Somebody commented on a page in your team's Confluence space.")
	default:
		b.WriteString("A page in your team's Confluence space was updated.")
	}
	header(b, n, parties)
	if n.Body != "" {
		b.WriteString("\n**What changed:**\n" + n.Body + "\n")
	}
	// THE ROUTING REASON IS WORTH STATING, because it is unusual: this is
	// not a page the seat subscribed to. Confluence has no per-agent
	// watch the engine can read, so a space's activity has exactly one
	// place to go — and a lead who is not told that reads every edit as a
	// request.
	b.WriteString("\nYou received this as the lead of the team that owns the" +
		" space — Confluence has no per-agent page watchers the engine can" +
		" read, so space activity routes to you by default.\n" +
		"\n## How to handle this" +
		"\n**Staying silent is the ordinary outcome.** Documentation" +
		" changing is not a request. Read the change only if it plausibly" +
		" bears on work your team has in flight; if it does, adjust your" +
		" plans or tell the teammate it concerns. Do NOT comment to" +
		" acknowledge it — a wiki whose comments are acknowledgements is a" +
		" wiki whose comments nobody reads.\n")
}

// header is the identifying block both openers share.
func header(b *strings.Builder, n notify.Inbound, parties notify.Parties) {
	meta := n.Metadata
	b.WriteString("\n\n**Page:** " + n.Subject)
	if space := meta["space"]; space != "" {
		b.WriteString("\n**Space:** " + space)
	}
	b.WriteString("\n**By:** " + senderLabel(n, parties) + "\n")
}

// getFullContext is the recon pointer, on the same condition
// [Prompt.RequiresRecon] answers.
//
// The two must agree: the flag tells the Plan phase not to bother filtering
// against a pointer, and this block is what makes the pointer followable.
func getFullContext(b *strings.Builder, meta map[string]string) {
	id := meta["page_id"]
	if id == "" {
		return
	}
	b.WriteString("\n## Get full context" +
		"\nThe text above is an EXTRACT. Fetch page `" + id + "` with your" +
		" Confluence tools before acting on it — a page is usually far longer" +
		" than the part that changed, and the part that changed is rarely the" +
		" part that matters.\n")
	if url := meta["url"]; url != "" {
		b.WriteString("Link: " + url + "\n")
	}
}

// senderLabel renders the actor as a colleague.
//
// Through the party registry FIRST, because a Forge-relayed payload carries
// only an account id — no display name, ever — so a prompt reading the
// payload alone names every Cloud event's author "someone".
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

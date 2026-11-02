package confluence

import (
	"strings"

	"github.com/crewlet/crewlet/internal/notify"
	"github.com/crewlet/crewlet/internal/provenance"
)

// Prompt is what a Confluence event asks of the seat it reached.
//
// # A wiki change is rarely urgent, and the prompt has to say so
//
// Every other third-party app's event is somebody asking for something. A page edit
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

// SubjectIsLabel implements [notify.Prompt]: never — the subject is the
// page's title, which is what the change is about.
func (Prompt) SubjectIsLabel(notify.Inbound) bool { return false }

// Addressed implements [notify.Prompt]: a mention, and only a mention.
//
// The one routing this prompt frames as an ask — somebody named the seat in a
// page or a comment and is waiting for it. A watcher is subscribed to a page
// it once touched, and a space lead is being told its team's documentation
// moved; a seat obliged to answer either would comment on every edit in every
// space its unit owns.
func (Prompt) Addressed(n notify.Inbound) bool {
	return n.Metadata[RoutedViaField] == ViaMention
}

// PartitionKey implements [notify.Prompt]: the page is the conversation.
//
// Keyed on the page id, so a burst of edits and the comments about them
// coalesce into one trigger rather than one turn each — which is what a wiki
// produces when somebody restructures a space.
func (Prompt) PartitionKey(metadata map[string]string, _ string) string {
	return metadata["page_id"]
}

// ConversationIdentity implements [notify.Prompt]: the same page id.
//
// The two coincide because the page is one object that is both the merge unit
// and the durable thread. Confluence's own reply-to-comment case already keys
// a top-level comment and its reply to the PAGE, so there is no sub-thread
// grain the partition could cut finer on.
func (p Prompt) ConversationIdentity(metadata map[string]string, subject string) string {
	return p.PartitionKey(metadata, subject)
}

// WakesActor implements [notify.Prompt]: never. A page reports what somebody
// did, and the person who did it was there.
func (Prompt) WakesActor(notify.Inbound) bool { return false }

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

// Provenance implements [notify.Prompt]: what the parser stated — a remark, or
// the page's own text, by its writer only on the event that wrote every word —
// and, for a notification built without the parser, a remark for a comment's
// events and the page's text for every other.
func (Prompt) Provenance(n notify.Inbound, parties notify.Parties) notify.Provenance {
	fallback := provenance.SourcePageBody
	if strings.HasPrefix(n.Metadata["event_type"], "comment_") {
		fallback = provenance.SourcePageComment
	}
	return notify.Stated(n, parties, fallback)
}

// Build implements [notify.Prompt].
func (Prompt) Build(n notify.Inbound, parties notify.Parties) string {
	meta := n.Metadata
	var b strings.Builder
	switch meta[RoutedViaField] {
	case ViaMention:
		mentioned(&b, n, parties)
	case ViaSpaceLead:
		spaceActivity(&b, n, parties)
	default:
		// A SUBSCRIBER, and any reason a later parser adds: activity on a
		// page the seat has touched is the honest default, because it
		// claims nothing about the seat's role in the space.
		subscribed(&b, n, parties)
	}
	getFullContext(&b, meta)
	return b.String()
}

// mentioned: somebody named this seat on a page or in a comment on one.
//
// NO EVALUATION BLOCK. A mention is addressed ([Prompt.Addressed]) and the
// delivery check holds it to an answer, so telling the seat silence is fine
// when nothing was asked of it buys a corrective round and nothing else.
func mentioned(b *strings.Builder, n notify.Inbound, parties notify.Parties) {
	b.WriteString("You were mentioned on a Confluence page.")
	header(b, n, parties)
	if n.Body != "" {
		b.WriteString("\n**What it says:**\n" + n.Body + "\n")
	}
}

// subscribed: a page this seat has edited, or was mentioned on, moved.
//
// ITS OWN FRAME, never the lead's. A subscriber copy used to render the
// space-lead prompt, so a seat that had merely fixed a typo on a page was told
// it was the lead of the space, that nobody had worked on the page, and that
// mentioning a teammate would send the next change to them instead — three
// false sentences, the last because a mention subscribes the teammate and
// unsubscribes nobody, so the subscriber keeps hearing the page either way.
func subscribed(b *strings.Builder, n notify.Inbound, parties notify.Parties) {
	switch n.Metadata["event_type"] {
	case "page_trashed", "page_removed":
		b.WriteString("A Confluence page you follow was deleted.")
	case "comment_created", "comment_updated":
		b.WriteString("Somebody commented on a Confluence page you follow.")
	default:
		b.WriteString("A Confluence page you follow was updated.")
	}
	header(b, n, parties)
	if n.Body != "" {
		b.WriteString("\n**What changed:**\n" + n.Body + "\n")
	}
	b.WriteString("\nYou follow it because you edited it or were mentioned on it.\n")
	handlePageNews(b)
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
	// not a page the seat subscribed to, and nobody was named on it. A
	// lead who is not told that reads every edit as a request.
	b.WriteString("\nYou received this as the lead of the team that owns the" +
		" space: nobody was named on the page and no seat has worked on it, so" +
		" it routes to you by default.\n")
	handlePageNews(b)
	// THE DELEGATION SENTENCE IS LOAD-BEARING, not encouragement. Nothing
	// else subscribes a seat to a page it has not touched, so a lead who
	// answers by hand keeps receiving every later event on that page —
	// and a lead who mentions the right teammate stops, because the
	// mention subscribes them. Saying so is what makes the loop close.
	b.WriteString("\nIf this page belongs to a teammate, **@mention them in a" +
		" comment** rather than answering it yourself: the mention subscribes" +
		" them, so the next change reaches them instead of you. Editing the" +
		" page subscribes YOU.\n")
}

// handlePageNews is what a page change asks of a seat nobody named.
//
// SILENCE IS THE ORDINARY OUTCOME, and it is said rather than left to the
// evaluation block: documentation moving is not a request, and a seat that
// comments to acknowledge it trains every reader to ignore the page's
// comments. Shared by the subscriber and the lead, who differ only in why
// they were told.
func handlePageNews(b *strings.Builder) {
	b.WriteString("\n## How to handle this" +
		"\n**Staying silent is the ordinary outcome.** Documentation changing" +
		" is not a request: act only if it bears on work in flight, and do not" +
		" comment to acknowledge it.\n")
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
// The two must agree: the flag tells the prefetch not to bother filtering
// against a pointer, and this block is what makes the pointer followable.
func getFullContext(b *strings.Builder, meta map[string]string) {
	id := meta["page_id"]
	if id == "" {
		return
	}
	b.WriteString("\n## Get full context" +
		"\nThe text above is an EXTRACT: fetch page `" + id + "` with your" +
		" Confluence tools before acting on it.\n")
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

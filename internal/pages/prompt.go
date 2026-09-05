package pages

import (
	"strings"

	"github.com/crewlet/crewlet/internal/notify"
)

// Prompt is what a change to a page asks of the seat it reached.
//
// It tailors to the ROUTING REASON, as the tracker's does, and it names its
// tools for the same reason: these are shipped by this build, registered
// under names this build chose, and present on every seat that can read the
// notification at all.
type Prompt struct{}

var _ notify.Prompt = Prompt{}

// Source implements [notify.Prompt].
func (Prompt) Source() string { return Source }

// RequiresRecon implements [notify.Prompt].
//
// A COMMENT CARRIES ITS TEXT and needs no recon; every other change is a
// pointer at a page whose body the seat has to read. The distinction matters
// because the flag also suppresses personal-memory filtering and episode
// recall, so setting it on a wake that already carries what it means costs
// the seat its own context for nothing.
func (Prompt) RequiresRecon(n notify.Inbound) bool {
	if n.Metadata[MetaPageID] == "" {
		return false
	}
	switch ChangeKind(n.EventType) {
	case ChangeComment, ChangeCommentEdited:
		return strings.TrimSpace(n.Body) == ""
	}
	return true
}

// Addressed implements [notify.Prompt]: only a mention.
//
// A page has no assignee, so being named is the only thing that constitutes
// an ask. Marking a watcher copy addressed would oblige a seat to answer every
// save on every page it follows.
func (Prompt) Addressed(n notify.Inbound) bool { return Addressed(n.Metadata) }

// ConversationKey implements [notify.Prompt]: the page id.
//
// THE ID rather than the title, unlike a work item's human key: a title
// changes, and a conversation keyed on one would split in half at a rename —
// silently, each half looking like an ordinary conversation.
func (Prompt) ConversationKey(metadata map[string]string, _ string) string {
	if id := metadata[MetaPageID]; id != "" {
		return id
	}
	return ""
}

// WakesActor implements [notify.Prompt]: never. Every change here is one the
// actor already knows about.
func (Prompt) WakesActor(string) bool { return false }

// DigestBody implements [notify.Prompt]: comments keep their text, saves
// collapse.
//
// A page's save body is a delta line the digest's lead already states, and
// five saves in a coalesced trigger is the same sentence five times.
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
	case ViaMention:
		promptMentioned(&b, n, parties)
	case ViaLeadFallback:
		promptLead(&b, n, parties)
	default:
		promptWatching(&b, n, parties)
	}
	promptGetContext(&b, meta)
	promptHandling(&b, meta)
	return b.String()
}

func promptMentioned(b *strings.Builder, n notify.Inbound, parties notify.Parties) {
	if ChangeKind(n.EventType) == ChangeComment {
		b.WriteString("You were @-mentioned in a comment on a page.")
	} else {
		b.WriteString("You were @-mentioned on a page.")
	}
	promptHeader(b, n, parties)
	body := n.Body
	if body == "" {
		body = "(no text)"
	}
	b.WriteString("\n**What they wrote:**\n" + body + "\n")
}

func promptWatching(b *strings.Builder, n notify.Inbound, parties notify.Parties) {
	b.WriteString("A page you are watching changed.")
	promptHeader(b, n, parties)
	if n.Body != "" {
		b.WriteString("\n**What changed:**\n" + n.Body + "\n")
	}
}

func promptLead(b *strings.Builder, n notify.Inbound, parties notify.Parties) {
	switch ChangeKind(n.EventType) {
	case ChangeCreated:
		b.WriteString("A new page appeared in your team's knowledge container, " +
			"and nobody here is watching it.")
	case ChangeRemoved:
		b.WriteString("A page in your team's knowledge container was removed, " +
			"and nobody here was watching it.")
	default:
		b.WriteString("A page in your team's knowledge container changed status, " +
			"and nobody here is watching it.")
	}
	promptHeader(b, n, parties)
	if n.Body != "" {
		b.WriteString("\n**What changed:**\n" + n.Body + "\n")
	}
}

func promptHeader(b *strings.Builder, n notify.Inbound, parties notify.Parties) {
	meta := n.Metadata
	b.WriteString("\n\n**Page:** " + n.Subject)
	if lead := changeLead(meta, promptSender(n, parties)); lead != "" {
		b.WriteString("\n**What happened:** " + lead)
	}
	b.WriteString("\n**By:** " + promptSender(n, parties))
	if container := meta[MetaContainer]; container != "" {
		b.WriteString("\n**Container:** " + container)
	}
	if version := meta[MetaVersion]; version != "" && version != "0" {
		b.WriteString("\n**Version:** " + version)
	}
	b.WriteString("\n")
}

func changeLead(meta map[string]string, actor string) string {
	by := ""
	if actor != "" {
		by = " by " + actor
	}
	switch ChangeKind(meta[MetaChangeKind]) {
	case ChangeCreated:
		return "The page was written" + by + "."
	case ChangeSaved:
		return "The page was edited" + by + "."
	case ChangeRenamed:
		return "The page was renamed" + by + "."
	case ChangeMoved:
		return "The page was moved" + by + "."
	case ChangeStatus:
		return "The page's status changed" + by + "."
	case ChangeComment:
		return "A comment was added" + by + "."
	case ChangeCommentEdited:
		return "A comment was edited" + by + "."
	case ChangeLabels:
		return "The labels changed" + by + "."
	case ChangeRemoved:
		return "The page was removed" + by + "."
	}
	return ""
}

func promptGetContext(b *strings.Builder, meta map[string]string) {
	id := meta[MetaPageID]
	if id == "" || ChangeKind(meta[MetaChangeKind]) == ChangeRemoved {
		return
	}
	b.WriteString("\n## Get full context" +
		"\nRead the page with `get_page` before deciding whether anything is " +
		"needed of you. A change summary is not the page.\n")
	if url := meta["url"]; url != "" {
		b.WriteString("Link: " + url + "\n")
	}
}

func promptHandling(b *strings.Builder, meta map[string]string) {
	if ChangeKind(meta[MetaChangeKind]) == ChangeRemoved {
		b.WriteString("\n## How to handle this" +
			"\nThe page no longer exists — stop relying on it. If you had work " +
			"depending on what it said, say so where that work is tracked.\n")
		return
	}
	if meta[RoutedViaField] == ViaMention {
		b.WriteString("\n## How to handle this" +
			"\n1. **Read the page first.** Somebody named you on it, and the " +
			"question is almost always about what it says." +
			"\n2. **Answer where you were asked** — comment on the page with " +
			"`comment_on_page`, so the answer is beside the question for the " +
			"next person who reads it." +
			"\n3. **If the answer is a change to the page**, make it with " +
			"`save_page` and say in your comment what you changed. Pass the " +
			"`version` you read back as `base_version`, so an edit somebody " +
			"else made in the meantime is a refusal rather than a silent " +
			"overwrite." +
			"\n4. **If you have decided not to act**, say so in one comment " +
			"naming who should. An unanswered mention looks exactly like a " +
			"message that was lost.\n")
		return
	}
	b.WriteString("\n## How to handle this" +
		"\nThis is news, not a request. Read it if it bears on work you have " +
		"in hand, and otherwise do nothing — a page changing is not an " +
		"instruction. If it CONTRADICTS something you are doing, say so where " +
		"that work is tracked rather than commenting on the page.\n")
}

// promptSender renders the actor as a colleague, by HANDLE — the actor on a
// first-party change is a handle, and nothing here authenticates as an
// account somewhere else.
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

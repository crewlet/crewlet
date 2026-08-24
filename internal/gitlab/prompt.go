package gitlab

import (
	"strings"

	"github.com/crewlet/crewlet/internal/notify"
)

// The prompt: what a code-host event asks of the seat it reached.
//
// Like the tracker's, it dispatches on the ROUTING REASON the parser
// stamped, because one merge request event reaches a reviewer, an assignee
// and a watcher and asks each for something different. What is particular to
// a code host is the FOURTH shape: an event that reports the outcome of the
// recipient's own action. Nothing else in the company has one.
//
// NOTHING HERE NAMES A TOOL — the deployed MCP server's tool names are not
// knowable by the engine. See docs/concepts/tool-capabilities.md.
type Prompt struct{}

var _ notify.Prompt = Prompt{}

// Source implements [notify.Prompt].
func (Prompt) Source() string { return Backend }

// reconEvents are the reasons whose body is a POINTER rather than the
// context.
//
// A review request names a merge request and carries its description; the
// thing that has to be read is the DIFF. An assignment is the same shape. A
// failed pipeline carries a status and nothing else — the log is the whole
// content, and it is not in the payload.
//
// A COMMENT IS NOT IN HERE, and that is the distinction the set encodes: a
// note's body IS what was said, so the trigger is the context and the
// Plan-phase filters have something real to filter against.
var reconEvents = map[string]bool{
	MRReview:       true,
	MRAssigned:     true,
	IssueAssigned:  true,
	PipelineFailed: true,
}

// RequiresRecon implements [notify.Prompt].
func (Prompt) RequiresRecon(n notify.Inbound) bool {
	return reconEvents[n.Metadata["event_type"]]
}

// WakesActor implements [notify.Prompt]: only a failed pipeline.
//
// THE ONE REAL EXCEPTION TO THE SELF-ACTION RULE in the whole engine. A
// pipeline names the person who pushed as its actor, and when it goes red
// they are the one who has to fix it — suppressing it means the only person
// who can act never learns. Every other event here reports what somebody
// DID, which that somebody already knows: their own comment, their own
// assignment, their own merge.
//
// The test is "did something happen BECAUSE of me that I do not yet know
// about". A build is the answer because it runs asynchronously, minutes
// after the push, and reports a result nobody could have predicted.
func (Prompt) WakesActor(eventType string) bool { return eventType == PipelineFailed }

// DigestBody implements [notify.Prompt]: comments keep their body, item
// snapshots collapse.
//
// The issue and merge request hooks carry the DESCRIPTION AS IT WAS on every
// event, so five of them in a coalesced digest is one paragraph five times,
// burying the line that moved. Only the latest matters and it renders in
// full below the digest. A note is a message rather than a snapshot: each
// one is something a person actually said.
func (Prompt) DigestBody(eventType, body string) string {
	if strings.HasPrefix(eventType, "note.") {
		return body
	}
	return ""
}

// ConversationKey implements [notify.Prompt]: the item is the conversation.
//
// PROJECT-QUALIFIED, because an iid is unique only within its project — two
// repositories both have a !1, and a key that was just the number would
// merge a comment on one with a review request on the other. The separators
// are GitLab's own: `!` for a merge request, `#` for an issue, so the key
// reads as the reference a person would paste.
//
// A pipeline for a branch push names no item and so derives no key: it is
// never merged with anything, which is right — two unrelated builds failing
// are two problems.
func (Prompt) ConversationKey(metadata map[string]string, _ string) string {
	// THROUGH [ItemRef], never rebuilt here. The key and the reference
	// the prompt prints are the same string by construction, so they
	// cannot drift into disagreeing about which separator a merge request
	// uses — and a drift there would be invisible: two well-formed keys
	// that simply never merge what belongs together.
	return ItemRef(metadata)
}

// Build implements [notify.Prompt].
func (Prompt) Build(n notify.Inbound, parties notify.Parties) string {
	var b strings.Builder
	switch reason := n.Metadata["event_type"]; {
	case reason == MRReview:
		reviewRequested(&b, n, parties)
	case reason == MRAssigned || reason == IssueAssigned:
		assigned(&b, n, parties)
	case reason == PipelineFailed:
		pipelineFailed(&b, n, parties)
	case strings.HasSuffix(reason, ".mention"):
		// note.mention, plus the description pings issue.mention and
		// merge_request.mention: one directed "you were named" shape,
		// differing only in which surface the words are on.
		mentioned(&b, n, parties)
	case reason == IssueClosed:
		issueClosed(&b, n, parties)
	default:
		// Comments the recipient merely watches, approvals, merges,
		// closes — and any reason a later release adds. The
		// evaluate-and-stay-silent framing is the right default for
		// thread activity and the right default for the unknown.
		watching(&b, n, parties)
	}
	return b.String()
}

// reviewRequested: somebody wants this seat to look at a diff.
func reviewRequested(b *strings.Builder, n notify.Inbound, parties notify.Parties) {
	b.WriteString("You have been requested to review a merge request.\n\n")
	writeRef(b, "Merge request", n)
	b.WriteString("**Requested by:** " + senderLabel(n, parties) + "\n\n" +
		"**Description:**\n" + orElse(n.Body, "(no description)") + "\n")
	b.WriteString("\n## What you should do" +
		"\n1. **Read the diff**, not just the description — the changed" +
		" files are the review. Approve if the change is correct; leave" +
		" comments on the diff where it is not." +
		"\n2. **Tell the requester** on the conversation this came from" +
		" that it is approved or that it needs work. A review that lands" +
		" only on the merge request is a review they have to go looking for." +
		"\n3. **If you have decided not to review** — out of scope, wrong" +
		" reviewer, you wrote the code — do NOT go quiet. A review request" +
		" is a direct ask: re-request review from the right person, or" +
		" comment naming who should do it and take yourself off the" +
		" reviewer list so the requester knows to reassign.\n")
}

// assigned: the work is this seat's.
func assigned(b *strings.Builder, n notify.Inbound, parties notify.Parties) {
	mr := n.Metadata["event_type"] == MRAssigned
	kind := "issue"
	if mr {
		kind = "merge request"
	}
	b.WriteString("You have been assigned " + article(kind) + " " + kind + ".\n\n")
	writeRef(b, capitalize(kind), n)
	b.WriteString("**Assigned by:** " + senderLabel(n, parties) + "\n\n" +
		"**Description:**\n" + orElse(n.Body, "(no description)") + "\n")

	b.WriteString("\n## What you should do" +
		"\n1. **Read the " + kind + " in full** — the description and the" +
		" existing discussion — before starting.")
	if mr {
		b.WriteString("\n2. **Do the work it describes**, then push and update" +
			" the merge request.")
	} else {
		b.WriteString("\n2. **Do the work it describes.** Code changes go" +
			" through your sandboxed coding runtime, which opens a merge" +
			" request under your own identity on the code host.")
	}
	b.WriteString("\n3. **Report back** to whoever assigned it, on the" +
		" conversation it came from, when you are done — or say so if you" +
		" have decided not to act (out of scope, wrong person). An" +
		" unanswered direct assignment looks exactly like a message that" +
		" was lost.\n")
}

// pipelineFailed: something the recipient did has broken.
//
// The ONE prompt in the engine addressed to the person who caused the event,
// which is why it says so explicitly: a seat that has learned "I am not told
// about my own actions" needs to be told why this one is different, or it
// reads its own name as a routing mistake.
func pipelineFailed(b *strings.Builder, n notify.Inbound, parties notify.Parties) {
	b.WriteString("A pipeline you triggered has FAILED.\n\n")
	writeRef(b, "Merge request", n)
	b.WriteString("**Triggered by:** " + senderLabel(n, parties) + "\n")
	b.WriteString("\nYou are being told about your own action deliberately." +
		" You are not normally notified of what you did — but a build runs" +
		" minutes after the push and reports something nobody could have" +
		" predicted, and you are the one who can fix it.\n")
	b.WriteString("\n## What you should do" +
		"\n1. **Read the job log** for the failing job. The status alone" +
		" says nothing about the cause, and guessing at it from the diff" +
		" is how a red pipeline becomes three red pipelines." +
		"\n2. **Fix it and push.** A broken pipeline on your own branch is" +
		" yours to clear; it does not need reassigning or announcing first." +
		"\n3. **If the failure is not yours** — a flaky job, an outage in a" +
		" dependency, a failure that reproduces on the target branch too —" +
		" say so once on the merge request rather than pushing speculative" +
		" changes to see what happens.\n")
}

// mentioned: somebody wrote this seat's name.
func mentioned(b *strings.Builder, n notify.Inbound, parties notify.Parties) {
	note := n.Metadata["event_type"] == NoteMention
	surface, label := "issue or merge-request description", "Description"
	if note {
		surface, label = "comment", "Comment"
	}
	b.WriteString("You were mentioned in a " + surface + " on the code host.\n\n")
	writeRef(b, "On", n)
	b.WriteString("**By:** " + senderLabel(n, parties) + "\n\n" +
		"**" + label + ":**\n" + orElse(n.Body, "(no text)") + "\n")
	b.WriteString(notify.EvaluationBlock())
}

// issueClosed: somebody closed an issue this seat is working on.
//
// Its own shape rather than the watching default, because the message is
// STOP and the default's "decide whether you were asked" framing gets in the
// way of it. An agent that keeps working on a closed issue is spending a
// budget on a deliverable nobody will take.
func issueClosed(b *strings.Builder, n notify.Inbound, parties notify.Parties) {
	b.WriteString("An issue you are assigned to was CLOSED.\n\n")
	writeRef(b, "Issue", n)
	b.WriteString("**Closed by:** " + senderLabel(n, parties) + "\n")
	b.WriteString("\n## What you should do" +
		"\nStop any in-flight work on it. If you had progress worth keeping," +
		" say so on the issue — a close is often somebody deciding the work" +
		" is unnecessary, and they may not know how far it had got. If you" +
		" believe the close is a mistake, say that on the issue rather than" +
		" reopening it: whoever closed it had a reason you may not be able" +
		" to see.\n")
}

// watching: thread activity on something this seat merely follows.
func watching(b *strings.Builder, n notify.Inbound, parties notify.Parties) {
	reason := n.Metadata["event_type"]
	b.WriteString("Activity on the code host reached you: " +
		orElse(reason, "an update") + ".\n\n")
	writeRef(b, "On", n)
	b.WriteString("**By:** " + senderLabel(n, parties) + "\n")
	if n.Body != "" {
		b.WriteString("\n**Body:**\n" + n.Body + "\n")
	}
	b.WriteString("\nYou are receiving this because you take part in this" +
		" thread — you authored it, are assigned to it, review it, or have" +
		" commented on it before. That is a reason to be informed, not a" +
		" request to act.\n")
	b.WriteString(notify.EvaluationBlock())
}

// writeRef renders the item reference and its link.
//
// The reference is the one a person would paste — "nimbus/api!42" — because
// it is simultaneously what the model needs to fetch the item and what a
// human in the same conversation would recognise.
func writeRef(b *strings.Builder, label string, n notify.Inbound) {
	ref := ItemRef(n.Metadata)
	if ref == "" {
		ref = orElse(n.Metadata["project"], "(unknown project)")
	}
	line := "**" + label + ":** " + ref
	if subject := strings.TrimSpace(n.Subject); subject != "" {
		line += " — " + subject
	}
	b.WriteString(line + "\n")
	if url := n.Metadata["url"]; url != "" {
		b.WriteString("**Link:** " + url + "\n")
	}
}

// ItemRef is the human reference for whatever this event names — and the
// CONVERSATION KEY, which is the same question asked by a different caller.
//
// One rendering, exported, because it is simultaneously what a person would
// paste, what the model fetches with, and what decides whether two events
// coalesce. Three renderings of one reference is how two of them come to
// disagree about which separator a merge request uses.
//
// PROJECT-QUALIFIED, because an iid is unique only within its project: two
// repositories both have a !1, and a bare number would merge a comment on
// one with a review request on the other. An event naming no item — a
// pipeline for a branch push — yields "", so it is never merged with
// anything, which is right: two unrelated builds failing are two problems.
func ItemRef(metadata map[string]string) string {
	project := metadata["project"]
	if project == "" {
		return ""
	}
	if mr := metadata["mr_iid"]; mr != "" {
		return project + "!" + mr
	}
	if issue := metadata["issue_iid"]; issue != "" {
		return project + "#" + issue
	}
	return ""
}

// senderLabel renders the actor as a colleague where the registry knows one.
//
// THE RAW USERNAME IS KEPT ALONGSIDE the label, unlike the tracker's, and
// the difference is real: a tracker's actor is an opaque UUID that tells the
// model nothing, while a code host's is the handle the seat will type to
// mention them back. Dropping it would cost the reply.
func senderLabel(n notify.Inbound, parties notify.Parties) string {
	username := strings.TrimSpace(n.Sender)
	if parties != nil && username != "" {
		if party, ok := parties.ByExternalID(Backend, strings.ToLower(username)); ok {
			if label := party.Label(); label != "" {
				return label + " — `" + username + "`"
			}
		}
	}
	return orElse(username, "someone")
}

func orElse(value, fallback string) string {
	if strings.TrimSpace(value) == "" {
		return fallback
	}
	return value
}

func article(noun string) string {
	if noun == "" {
		return "a"
	}
	switch noun[0] {
	case 'a', 'e', 'i', 'o', 'u':
		return "an"
	}
	return "a"
}

func capitalize(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

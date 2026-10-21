package gitlab

import (
	"strings"
	"unicode/utf8"

	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/notify"
	"github.com/crewlet/crewlet/internal/provenance"
)

// Prompt is what a code-host event asks of the seat it reached.
//
// Like the tracker's, it dispatches on the ROUTING REASON the parser
// stamped, because one merge request event reaches a reviewer, an assignee
// and a watcher and asks each for something different. What is particular to
// a code host is the FOURTH shape: an event that reports the outcome of the
// recipient's own action. Only the hosted code host's failed run and the
// tracker's unblocked notice share it.
//
// WHAT IT SAYS IS WHAT ONLY THE ENGINE KNOWS: why the seat is reading this,
// the item by the reference a person would paste, the product facts a model
// would not assume (a close that means stop, where an issue's code changes
// go), and how each ask is declined on this host. How to review a diff or
// fix a red pipeline is the model's own, and that a direct ask may not end in
// silence is the executor's contract — neither is restated here.
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
// turn-start filters have something real to filter against.
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

// SubjectIsLabel implements [notify.Prompt]: never — the subject names the
// issue, merge request or pipeline the event is about.
func (Prompt) SubjectIsLabel(notify.Inbound) bool { return false }

// addressedEvents are the reasons that are an ASK of this seat rather than
// news about something it follows.
//
// An assignment, a review request and a mention each name the seat as the one
// who has to act; leaving any of them unanswered looks to the person who
// wrote it exactly like the webhook never arrived.
//
// A PLAIN NOTE IS NOT IN HERE, nor is a close or a failed pipeline. Those
// reach the seat because it participates in the thread, and a seat obliged to
// reply to every one of them would post on every state change of every merge
// request it has ever touched.
var addressedEvents = map[string]bool{
	IssueAssigned: true,
	IssueMention:  true,
	MRAssigned:    true,
	MRReview:      true,
	MRMention:     true,
	NoteMention:   true,
}

// Addressed implements [notify.Prompt].
func (Prompt) Addressed(n notify.Inbound) bool {
	return addressedEvents[n.Metadata["event_type"]]
}

// WakesActor implements [notify.Prompt]: only a failed pipeline.
//
// THIS HOST'S ONE EXCEPTION TO THE SELF-ACTION RULE, shared with the hosted
// host's failed run and for the same reason. A pipeline names the person who
// pushed as its actor, and when it goes red they are the one who has to fix
// it — suppressing it means the only person who can act never learns. Every
// other event here reports what somebody DID, which that somebody already
// knows: their own comment, their own assignment, their own merge.
//
// The test is "did something happen BECAUSE of me that I do not yet know
// about". A build is the answer because it runs asynchronously, minutes
// after the push, and reports a result nobody could have predicted.
func (Prompt) WakesActor(n notify.Inbound) bool { return n.EventType == PipelineFailed }

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

// PartitionKey implements [notify.Prompt]: the item is the conversation.
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
func (Prompt) PartitionKey(metadata map[string]string, _ string) string {
	// THROUGH [ItemRef], never rebuilt here. The key and the reference
	// the prompt prints are the same string by construction, so they
	// cannot drift into disagreeing about which separator a merge request
	// uses — and a drift there would be invisible: two well-formed keys
	// that simply never merge what belongs together.
	return ItemRef(metadata)
}

// ConversationIdentity implements [notify.Prompt]: the same item reference.
//
// The two coincide because the item is one object that is both the merge unit
// and the durable thread; a discussion on a merge request has no sub-thread
// grain here. DELEGATED rather than re-derived, for the reason the key itself
// goes through [ItemRef]: a second derivation of one reference drifts
// invisibly, into two well-formed keys that simply never merge what belongs
// together.
func (p Prompt) ConversationIdentity(metadata map[string]string, subject string) string {
	return p.PartitionKey(metadata, subject)
}

// Provenance implements [notify.Prompt]: what the parser stated — a note by
// its writer while it is fresh, an issue's or merge request's description by
// its opener on the hook that opened it and by nobody named after — and, for a
// notification built without the parser, a remark for a note's reasons and a
// description for every other.
func (Prompt) Provenance(n notify.Inbound, parties notify.Parties) notify.Provenance {
	fallback := provenance.SourceWorkDescription
	switch n.Metadata["event_type"] {
	case NoteMention, NoteComment:
		fallback = provenance.SourceWorkComment
	}
	return notify.Stated(n, parties, fallback)
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
	// THE DECLINE GESTURE AND NOTHING ELSE — see the hosted host's prompt:
	// reviewing is the model's own, and only the code host knows that a
	// review is declined by moving it.
	b.WriteString("\nIf you are not the right reviewer, request review from" +
		" whoever is and remove yourself from the reviewers.\n")
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
		"**Description:**\n" + orElse(n.Body, "(no description)") + "\n\n")
	if !mr {
		// A FACT ABOUT THIS ENGINE, not advice: where code changes go, and
		// whose name the merge request they produce carries.
		b.WriteString("Code changes go through your sandboxed coding runtime," +
			" which opens a merge request under your own identity on the code" +
			" host. ")
	}
	b.WriteString("If the work is not yours, reassign it to whoever it belongs to.\n")
}

// pipelineFailed: something the recipient did has broken.
//
// One of the few wakes in the engine addressed to the party who caused it
// (the hosted host's failed run and the tracker's unblocked notice are the
// others), which is why it says so explicitly: a seat that has learned "I am
// not told about my own actions" needs to be told why this one is different,
// or it reads its own name as a routing mistake. How to debug a red pipeline
// is the model's own, and is not restated.
//
// LABELLED "Pipeline", because the reference is the merge request the
// pipeline ran for or, on a branch push, the bare project — and "Merge
// request: nimbus/api" named a merge request that does not exist.
func pipelineFailed(b *strings.Builder, n notify.Inbound, parties notify.Parties) {
	b.WriteString("A pipeline you triggered has FAILED.\n\n")
	writeRef(b, "Pipeline", n)
	b.WriteString("**Triggered by:** " + senderLabel(n, parties) + "\n")
	b.WriteString("\nYou are being told about your own action deliberately." +
		" You are not normally notified of what you did — but a build runs" +
		" minutes after the push and reports something nobody could have" +
		" predicted, and you are the one who can fix it.\n")
}

// mentioned: somebody wrote this seat's name.
//
// NO EVALUATION BLOCK. A mention is addressed ([Prompt.Addressed]) and the
// delivery check holds it to an answer, so asking the seat whether it was
// really asked — and telling it silence is fine when it was not — buys a
// corrective round and nothing else.
func mentioned(b *strings.Builder, n notify.Inbound, parties notify.Parties) {
	note := n.Metadata["event_type"] == NoteMention
	surface, label := "issue or merge-request description", "Description"
	if note {
		surface, label = "comment", "Comment"
	}
	b.WriteString("You were mentioned in " + article(surface) + " " + surface +
		" on the code host.\n\n")
	writeRef(b, "On", n)
	b.WriteString("**By:** " + senderLabel(n, parties) + "\n\n" +
		"**" + label + ":**\n" + orElse(n.Body, "(no text)") + "\n")
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
	b.WriteString("\nStop any in-flight work on it.\n")
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

// ProjectIDField is the metadata key carrying the project's numeric id: the
// one part of an item's identity a rename or a transfer does not change.
const ProjectIDField = "project_id"

// WorkItem is the issue or merge request an event names, as the work item a
// turn it wakes is charged to.
//
// THE ID IS `<project id>!<iid>` FOR A MERGE REQUEST AND `<project id>#<iid>`
// FOR AN ISSUE — the reference [ItemRef] renders, with the project's numeric
// id in place of its path. The path is what a rename or a transfer changes; the
// id is not. The separator stays, because GitLab numbers issues and merge
// requests in two sequences, so `!1` and `#1` are two different items in one
// project. The key is [ItemRef] itself and the project is the path.
//
// An event naming no item — a pipeline for a branch push — names nothing, and
// so does one from a delivery that carried no project id.
func (Prompt) WorkItem(metadata map[string]string) (types.WorkItem, bool) {
	projectID, key := metadata[ProjectIDField], ItemRef(metadata)
	if projectID == "" || key == "" {
		return types.WorkItem{}, false
	}
	id := projectID + "#" + metadata["issue_iid"]
	if mr := metadata["mr_iid"]; mr != "" {
		id = projectID + "!" + mr
	}
	return types.WorkItem{
		Backend: types.WorkGitLab, ID: id, Key: key, Project: metadata["project"],
	}, true
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
	// The first RUNE: s[:1] on a multi-byte lead byte yields invalid UTF-8,
	// which ToUpper substitutes rather than passes through.
	first, size := utf8.DecodeRuneInString(s)
	return strings.ToUpper(string(first)) + s[size:]
}

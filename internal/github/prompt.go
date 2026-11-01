package github

import (
	"strings"
	"unicode/utf8"

	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/notify"
	"github.com/crewlet/crewlet/internal/provenance"
)

// Prompt is what a hosted code-host event asks of the seat it reached.
//
// It dispatches on the ROUTING REASON the parser stamped, because one pull
// request reaches a reviewer, an assignee, an author and a watcher and asks
// each for something different. Two shapes here are rare in the engine:
//
//   - AN EVENT THAT REPORTS THE OUTCOME OF THE RECIPIENT'S OWN ACTION — a
//     workflow run they triggered going red. Only the self-hosted host's
//     failed pipeline and the tracker's unblocked notice share it, and the
//     prompt says so out loud, because a seat that has learned "I am not told
//     about my own actions" reads its own name as a routing mistake otherwise.
//   - A REVIEW THAT ASKS FOR CHANGES. The self-hosted code host carries
//     approvals and nothing that says "this is not right yet, here is why",
//     so its prompt has no branch for it. It is the strongest directed ask a
//     code host makes, and rendering it as ordinary thread activity would
//     have an agent read a blocking review as news.
//
// WHAT IT SAYS IS WHAT ONLY THE ENGINE KNOWS: why the seat is reading this,
// the item by the reference a person would paste, the product facts a model
// would not assume (a block that holds the merge, a merge that answers the
// requester, a close that means stop, who makes an issue's code change and
// under whose name), and how each ask is declined on this host. How to review
// a diff or fix a red build is the model's own, and that a direct ask may not
// end in silence is the executor's contract — neither is restated here.
//
// NOTHING HERE NAMES A TOOL — the deployed MCP server's tool names are not
// knowable by the engine. See docs/concepts/tool-capabilities.md.
type Prompt struct{}

var _ notify.Prompt = Prompt{}

// Source implements [notify.Prompt].
func (Prompt) Source() string { return Backend }

// reconEvents are the reasons whose body is a POINTER rather than the
// content.
//
// A review request names a pull request and carries its description; the
// thing that has to be read is the DIFF. An assignment is the same shape. A
// failed workflow run carries a conclusion and nothing else — the log is the
// whole content, and it is not in the payload.
//
// A COMMENT IS NOT IN HERE, and that is the distinction the set encodes: a
// comment's body IS what was said, so the trigger is the content and the
// turn-start filters have something real to filter against. A
// CHANGES-REQUESTED REVIEW is in here even though it usually carries a body,
// because the body says WHAT is wrong and the diff comments say WHERE — and
// acting on the summary alone is how a review round produces a change that
// answers none of the line comments.
var reconEvents = map[string]bool{
	PRReviewRequested:  true,
	PRAssigned:         true,
	IssueAssigned:      true,
	PRChangesRequested: true,
	WorkflowFailed:     true,
}

// RequiresRecon implements [notify.Prompt].
func (Prompt) RequiresRecon(n notify.Inbound) bool {
	return reconEvents[n.Metadata["event_type"]]
}

// SubjectIsLabel implements [notify.Prompt]: never — the subject names the
// issue, pull request or workflow the event is about.
func (Prompt) SubjectIsLabel(notify.Inbound) bool { return false }

// addressedEvents are the reasons that are an ASK of this seat rather than
// news about something it follows.
//
// An assignment, a review request and a mention each name the seat as the
// one who has to act; leaving any of them unanswered looks to the person who
// wrote it exactly like the webhook never arrived.
//
// A COMMENT THAT DID NOT MENTION ANYBODY IS NOT IN HERE, nor is a merge, a
// close, an approval or a failed run. Those reach the seat because it
// subscribes to the thread, and a seat obliged to reply to every one of them
// would post on every state change of every pull request it has ever touched.
var addressedEvents = map[string]bool{
	IssueAssigned:      true,
	IssueMention:       true,
	PRAssigned:         true,
	PRReviewRequested:  true,
	PRMention:          true,
	PRChangesRequested: true,
	CommentMention:     true,
}

// Addressed implements [notify.Prompt].
func (Prompt) Addressed(n notify.Inbound) bool {
	return addressedEvents[n.Metadata["event_type"]]
}

// WakesActor implements [notify.Prompt]: only a failed workflow run.
//
// THIS HOST'S ONE EXCEPTION TO THE SELF-ACTION RULE, shared with the
// self-hosted host and for the same reason. A run names the person whose
// push triggered it, and when it goes red they are the one who has to fix it
// — suppressing it means the only person who can act never learns. Every
// other event here reports what somebody DID, which that somebody already
// knows: their own comment, their own assignment, their own merge.
//
// The test is "did something happen BECAUSE of me that I do not yet know
// about". A build is the answer because it runs asynchronously, minutes
// after the push, and reports a result nobody could have predicted.
func (Prompt) WakesActor(n notify.Inbound) bool { return n.EventType == WorkflowFailed }

// DigestBody implements [notify.Prompt]: comments and reviews keep their
// body, item snapshots collapse.
//
// The issue and pull request payloads carry the DESCRIPTION AS IT WAS on
// every event, so five of them in a coalesced digest is one paragraph five
// times, burying the line that moved. Only the latest matters and it renders
// in full below the digest. A comment or a review is a message rather than a
// snapshot: each one is something a person actually said.
func (Prompt) DigestBody(eventType, body string) string {
	switch {
	case strings.HasPrefix(eventType, "comment."):
		return body
	case eventType == PRChangesRequested || eventType == PRReviewed:
		return body
	default:
		return ""
	}
}

// PartitionKey implements [notify.Prompt]: the item is the conversation.
//
// REPOSITORY-QUALIFIED, because a number is unique only within its
// repository — two repositories both have a #1, and a key that was just the
// number would merge a comment on one with a review request on the other.
//
// A workflow run for a branch push names no item and so derives no key: it
// is never merged with anything, which is right — two unrelated builds
// failing are two problems.
func (Prompt) PartitionKey(metadata map[string]string, _ string) string {
	// THROUGH [ItemRef], never rebuilt here, so the key and the reference
	// the prompt prints are the same string by construction. A drift there
	// would be invisible: two well-formed keys that simply never merge
	// what belongs together.
	return ItemRef(metadata)
}

// ConversationIdentity implements [notify.Prompt]: the same item reference.
//
// The two coincide because the item is one object that is both the merge unit
// and the durable thread. DELEGATED rather than re-derived, for the reason the
// key itself goes through [ItemRef]: a second derivation of one reference
// drifts invisibly, into two well-formed keys that never merge what belongs
// together.
func (p Prompt) ConversationIdentity(metadata map[string]string, subject string) string {
	return p.PartitionKey(metadata, subject)
}

// Provenance implements [notify.Prompt]: what the parser stated — a remark by
// its writer while it is fresh, an issue's or pull request's description by
// its opener on the event that opened it and by nobody named after — and, for
// a notification built without the parser, a remark for a comment's or a
// review's reasons and a description for every other.
func (Prompt) Provenance(n notify.Inbound, parties notify.Parties) notify.Provenance {
	fallback := provenance.SourceWorkDescription
	switch reason := n.Metadata["event_type"]; reason {
	case CommentMention, CommentAdded, PRApproved, PRChangesRequested, PRReviewed:
		fallback = provenance.SourceWorkComment
	}
	return notify.Stated(n, parties, fallback)
}

// Build implements [notify.Prompt].
func (Prompt) Build(n notify.Inbound, parties notify.Parties) string {
	var b strings.Builder
	switch reason := n.Metadata["event_type"]; {
	case reason == PRReviewRequested:
		reviewRequested(&b, n, parties)
	case reason == PRAssigned || reason == IssueAssigned:
		assigned(&b, n, parties)
	case reason == PRChangesRequested:
		changesRequested(&b, n, parties)
	case reason == PRApproved:
		approved(&b, n, parties)
	case reason == PRMerged:
		merged(&b, n, parties)
	case reason == WorkflowFailed:
		workflowFailed(&b, n, parties)
	case strings.HasSuffix(reason, ".mention"):
		// comment.mention, plus the body pings issue.mention and
		// pull_request.mention: one directed "you were named" shape,
		// differing only in which surface the words are on.
		mentioned(&b, n, parties)
	case reason == IssueClosed:
		issueClosed(&b, n, parties)
	default:
		// Comments the recipient merely watches, a closed pull request,
		// a reopen, a review with no verdict — and any reason a later
		// API version adds. The evaluate-and-stay-silent framing is the
		// right default for thread activity and the right default for
		// the unknown.
		watching(&b, n, parties)
	}
	return b.String()
}

// reviewRequested: somebody wants this seat to look at a diff.
func reviewRequested(b *strings.Builder, n notify.Inbound, parties notify.Parties) {
	b.WriteString("You have been requested to review a pull request.\n\n")
	writeRef(b, "Pull request", n)
	b.WriteString("**Requested by:** " + senderLabel(n, parties) + "\n\n" +
		"**Description:**\n" + orElse(n.Body, "(no description)") + "\n")
	// THE DECLINE GESTURE AND NOTHING ELSE. How to review a diff is the
	// model's own, and that a direct ask may not end in silence is the
	// executor's contract; what only the code host knows is how a review
	// is declined — it moves to somebody else.
	b.WriteString("\nIf you are not the right reviewer, request review from" +
		" whoever is and remove yourself from the reviewers.\n")
}

// assigned: the work is this seat's.
func assigned(b *strings.Builder, n notify.Inbound, parties notify.Parties) {
	pr := n.Metadata["event_type"] == PRAssigned
	kind := "issue"
	if pr {
		kind = "pull request"
	}
	b.WriteString("You have been assigned " + article(kind) + " " + kind + ".\n\n")
	writeRef(b, capitalize(kind), n)
	b.WriteString("**Assigned by:** " + senderLabel(n, parties) + "\n\n" +
		"**Description:**\n" + orElse(n.Body, "(no description)") + "\n\n")
	if !pr {
		// A FACT ABOUT THIS ENGINE, not advice: who makes the change, and
		// whose name the pull request carries. The executor reading this IS
		// the seat's coding CLI's own run, with its own checkout, shell and
		// git, so the change is its own to make: there is no other runtime to
		// hand it to, and a prompt that names one has the seat look for
		// something that does not exist. The request opens under the seat's
		// own identity, so what the code host says about it afterwards — a
		// review, a merge — reaches the seat that wrote it.
		b.WriteString("You make any code change the issue needs yourself, in" +
			" your own checkout, and open the pull request under your own" +
			" identity on the code host. ")
	}
	b.WriteString("If the work is not yours, reassign it to whoever it belongs to.\n")
}

// changesRequested: a reviewer has blocked this seat's pull request.
//
// Its own shape rather than the watching default, and the framing is what
// matters: a changes-requested review is the one event on a code host that
// says the recipient's work is not finished. Rendered as thread activity, an
// agent reads it as information and moves on — leaving a blocked pull
// request open with nobody acting on it and a reviewer waiting. The opener is
// the whole instruction: how to answer a review is the model's own.
func changesRequested(b *strings.Builder, n notify.Inbound, parties notify.Parties) {
	b.WriteString("A reviewer has REQUESTED CHANGES on your pull request. " +
		"It cannot merge until this is resolved.\n\n")
	writeRef(b, "Pull request", n)
	b.WriteString("**Reviewer:** " + senderLabel(n, parties) + "\n\n" +
		"**Their summary:**\n" + orElse(n.Body, "(no summary — the review is "+
		"in the line comments)") + "\n")
}

// approved: a reviewer has cleared this seat's pull request.
//
// Short on purpose. The news is small and entirely actionable, and the one
// failure mode worth naming is an agent treating an approval as the end: an
// approved pull request that nobody merges is work that never shipped.
func approved(b *strings.Builder, n notify.Inbound, parties notify.Parties) {
	b.WriteString("Your pull request has been APPROVED.\n\n")
	writeRef(b, "Pull request", n)
	b.WriteString("**Approved by:** " + senderLabel(n, parties) + "\n")
	if strings.TrimSpace(n.Body) != "" {
		b.WriteString("\n**Their comment:**\n" + n.Body + "\n")
	}
	b.WriteString("\nMerge it, unless something else still blocks it — then" +
		" say on the pull request what does.\n")
}

// merged: this seat's pull request has landed.
//
// A merge is the moment the requester's question is answered, and nothing
// else tells them — so the wake says who is owed the news, and that the issue
// it was for is now this seat's to close.
func merged(b *strings.Builder, n notify.Inbound, parties notify.Parties) {
	b.WriteString("Your pull request has been MERGED.\n\n")
	writeRef(b, "Pull request", n)
	b.WriteString("**Merged by:** " + senderLabel(n, parties) + "\n")
	b.WriteString("\nTell whoever asked for it that it has landed, and close" +
		" the issue it was for.\n")
}

// workflowFailed: something the recipient did has broken.
//
// One of the few wakes in the engine addressed to the party who caused it
// (this host's, the self-hosted host's failed pipeline and the tracker's
// unblocked notice are the others), which is why it says so explicitly: a
// seat that has learned "I am not told about my own actions" needs to be told
// why this one is different, or it reads its own name as a routing mistake.
// How to debug a red run is the model's own, and is not restated.
func workflowFailed(b *strings.Builder, n notify.Inbound, parties notify.Parties) {
	b.WriteString("A workflow run you triggered has FAILED.\n\n")
	writeRef(b, "Run", n)
	b.WriteString("**Triggered by:** " + senderLabel(n, parties) + "\n")
	b.WriteString("\nYou are being told about your own action deliberately." +
		" You are not normally notified of what you did — but a workflow" +
		" runs minutes after the push and reports something nobody could" +
		" have predicted, and you are the one who can fix it.\n")
}

// mentioned: somebody wrote this seat's name.
//
// NO EVALUATION BLOCK. A mention is addressed ([Prompt.Addressed]) and the
// delivery check holds it to an answer, so asking the seat whether it was
// really asked — and telling it silence is fine when it was not — buys a
// corrective round and nothing else.
func mentioned(b *strings.Builder, n notify.Inbound, parties notify.Parties) {
	comment := n.Metadata["event_type"] == CommentMention
	surface, label := "issue or pull-request description", "Description"
	if comment {
		surface, label = "comment", "Comment"
	}
	b.WriteString("You were mentioned in " + article(surface) + " " + surface +
		" on the code host.\n\n")
	writeRef(b, "On", n)
	b.WriteString("**By:** " + senderLabel(n, parties) + "\n")
	if where := diffLocation(n); where != "" {
		b.WriteString("**On the diff:** " + where + "\n")
	}
	b.WriteString("\n**" + label + ":**\n" + orElse(n.Body, "(no text)") + "\n")
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
	if where := diffLocation(n); where != "" {
		b.WriteString("**On the diff:** " + where + "\n")
	}
	if n.Body != "" {
		b.WriteString("\n**Body:**\n" + n.Body + "\n")
	}
	b.WriteString("\nYou are receiving this because you take part in this" +
		" thread — you authored it, are assigned to it, review it, or have" +
		" commented on it before. That is a reason to be informed, not a" +
		" request to act.\n")
	b.WriteString(notify.EvaluationBlock())
}

// diffLocation is the file and line a review comment hangs off.
//
// Rendered wherever it exists rather than only on the review-comment
// prompts, because a line comment reaches a seat through several of them —
// a mention, a fan-out — and "somebody said this about your code" without
// saying which code is a message the recipient has to go and reconstruct.
func diffLocation(n notify.Inbound) string {
	path := n.Metadata["path"]
	if path == "" {
		return ""
	}
	if line := n.Metadata["line"]; line != "" {
		return path + ":" + line
	}
	return path
}

// writeRef renders the item reference and its link.
//
// The reference is the one a person would paste — "crewlet/crewlet#42" —
// because it is simultaneously what the model needs to fetch the item and
// what a human in the same conversation would recognise.
func writeRef(b *strings.Builder, label string, n notify.Inbound) {
	ref := ItemRef(n.Metadata)
	if ref == "" {
		ref = orElse(n.Metadata["repo"], "(unknown repository)")
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
// ONE SEPARATOR FOR BOTH KINDS, which is where this differs from the
// self-hosted host: GitHub numbers issues and pull requests in one sequence
// per repository and writes both as `#42`, so `owner/repo#42` names exactly
// one thing. Inventing a second separator to distinguish them would produce
// a reference that is not the one a person would paste, and would key the
// same thread two ways depending on which event arrived.
//
// An event naming no item — a workflow run on a branch push — yields "", so
// it is never merged with anything, which is right: two unrelated builds
// failing are two problems.
func ItemRef(metadata map[string]string) string {
	repo := metadata["repo"]
	if repo == "" {
		return ""
	}
	if pr := metadata["pr_number"]; pr != "" {
		return repo + "#" + pr
	}
	if issue := metadata["issue_number"]; issue != "" {
		return repo + "#" + issue
	}
	return ""
}

// RepoIDField is the metadata key carrying the repository's numeric id: the
// one part of an item's identity a rename or a transfer does not change.
const RepoIDField = "repo_id"

// WorkItem is the issue or pull request an event names, as the work item a
// turn it wakes is charged to.
//
// THE ID IS `<repository id>#<number>`, NOT GITHUB'S OWN GLOBAL ID, because a
// pull request has TWO of those: an issue comment on it carries the issue
// object's id and a review carries the pull request's, and keyed on either
// one pull request would be two items depending on which event woke the turn.
// Issues and pull requests share one number sequence per repository, which is
// what makes the number the single identity both faces agree on; the
// repository's numeric id rather than its name is what keeps it stable across
// a rename or a transfer. The key is the reference a person pastes, and the
// project is the repository's full name.
//
// An event naming no item — a workflow run on a branch push — names nothing,
// and so does one from a delivery that carried no repository id.
func (Prompt) WorkItem(metadata map[string]string) (types.WorkItem, bool) {
	repoID, key := metadata[RepoIDField], ItemRef(metadata)
	number := metadata["pr_number"]
	if number == "" {
		number = metadata["issue_number"]
	}
	if repoID == "" || key == "" || number == "" {
		return types.WorkItem{}, false
	}
	return types.WorkItem{
		Backend: types.WorkGitHub, ID: repoID + "#" + number,
		Key: key, Project: metadata["repo"],
	}, true
}

// senderLabel renders the actor as a colleague where the registry knows one.
//
// THE RAW LOGIN IS KEPT ALONGSIDE the label, because a code host's actor is
// the handle the seat will type to mention them back — dropping it would
// cost the reply.
func senderLabel(n notify.Inbound, parties notify.Parties) string {
	login := strings.TrimSpace(n.Sender)
	if parties != nil && login != "" {
		if party, ok := parties.ByExternalID(Backend, strings.ToLower(login)); ok {
			if label := party.Label(); label != "" {
				return label + " — `" + login + "`"
			}
		}
	}
	return orElse(login, "someone")
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

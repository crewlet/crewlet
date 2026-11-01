package code

import (
	"strings"

	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/notify"
	"github.com/crewlet/crewlet/internal/provenance"
	"github.com/crewlet/crewlet/internal/toolname"
)

// Prompt is what a code wake asks of the seat it reached: the change, why the
// seat hears about it, and the tool that answers it.
//
// It NAMES ITS TOOLS, as the tracker's and the knowledge base's do: they ship
// with this build under names this build chose, and every seat a code wake
// reaches is offered the code tools that read a change. The names are read
// from internal/toolname, because internal/agent/builtin, which registers
// them, imports this package; TestThePromptNamesOnlyRegisteredChangeTools
// holds the two together.
type Prompt struct{}

var _ notify.Prompt = Prompt{}

// Source implements [notify.Prompt]: [Source].
func (Prompt) Source() string { return Source }

// RequiresRecon implements [notify.Prompt]: ALWAYS. A code wake is a pointer
// at a change — its diff, its findings, its revisions — and even a comment or
// a review's summary reads against a diff the wake does not carry, so a seat
// filtering its context against the wake alone would filter against the least
// of what it is about.
func (Prompt) RequiresRecon(notify.Inbound) bool { return true }

// SubjectIsLabel implements [notify.Prompt]: never — the subject is the
// change's name and title, which is what the wake is about.
func (Prompt) SubjectIsLabel(notify.Inbound) bool { return false }

// Addressed implements [notify.Prompt]: the reasons that ask something of
// whoever hears them ([Reason.Addressed]) — a mention, an ask to review or to
// look again, a request for changes, a conflict, a reply to a finding, a
// change ready to merge. Everything else is news a seat may absorb in
// silence.
func (Prompt) Addressed(n notify.Inbound) bool { return Reason(n.Metadata[MetaVia]).Addressed() }

// PartitionKey implements [notify.Prompt]: the change, by its id.
//
// THE ID, never the reference: a change's number never moves, but its
// repository's name does (a rename), and a conversation keyed on `web!42`
// would split in two at the rename, each half looking ordinary.
func (p Prompt) PartitionKey(metadata map[string]string, subject string) string {
	return p.ConversationIdentity(metadata, subject)
}

// ConversationIdentity implements [notify.Prompt]: `change:<id>`, the same
// key as the partition. A change is both the merge unit and the durable
// thread — a review, a comment and a merge are activity ON the change — so
// the two coincide. Empty for a wake that names no change, which no parser
// writes and which then merges with nothing.
func (Prompt) ConversationIdentity(metadata map[string]string, _ string) string {
	if id := metadata[MetaChangeID]; id != "" {
		return string(KindChange) + ":" + id
	}
	return ""
}

// WorkItem is the work item a code wake is about — the one the change
// implements (ADR-0090) — which is the item the turn it wakes is charged to
// (ADR-0022's trigger rule). None for a change that names none.
func (Prompt) WorkItem(metadata map[string]string) (types.WorkItem, bool) {
	id := metadata[MetaWorkID]
	if id == "" {
		return types.WorkItem{}, false
	}
	return types.WorkItem{Backend: types.WorkBackend(metadata[MetaWorkBackend]), ID: id,
		Key: metadata[MetaWorkKey], Project: metadata[MetaWorkProject]}, true
}

// WakesActor implements [notify.Prompt]: never. No code reason is news to
// whoever made the change — a reviewer knows their review, a pusher their
// merge — which the routing already holds to ([Concern.Hears]).
func (Prompt) WakesActor(notify.Inbound) bool { return false }

// Provenance implements [notify.Prompt]: the change text the wake carries, on
// the engine's own code host, by the writer its record names (stamped by
// inboundOf); a wake whose stamp this build does not know reads as a remark in
// the change's thread.
func (Prompt) Provenance(n notify.Inbound, parties notify.Parties) notify.Provenance {
	return notify.StatedHosted(n, parties, provenance.SourceChangeComment)
}

// DigestBody implements [notify.Prompt]: what somebody SAID keeps its text —
// a comment, a review's summary, an ask's note, a description, a close's
// reason — and a record that says nothing of its own collapses to its lead.
func (Prompt) DigestBody(_, body string) string { return body }

// Build implements [notify.Prompt].
func (Prompt) Build(n notify.Inbound, parties notify.Parties) string {
	meta := n.Metadata
	reason := Reason(meta[MetaVia])
	var b strings.Builder
	b.WriteString(wakeOpener(reason, meta))
	b.WriteString("\n\n**Change:** " + n.Subject)
	if repo := meta[MetaRepo]; repo != "" {
		b.WriteString("\n**Repository:** " + repo)
	}
	if by := wakeSender(n, parties); by != "" {
		b.WriteString("\n**By:** " + by)
	}
	if key := meta[MetaWorkKey]; key != "" {
		b.WriteString("\n**Work item:** " + key)
	} else if id := meta[MetaWorkID]; id != "" {
		b.WriteString("\n**Work item:** " + id)
	}
	b.WriteString("\n")
	if n.Body != "" {
		b.WriteString("\n**What they wrote:**\n" + n.Body + "\n")
	}
	ref := meta[MetaChangeRef]
	b.WriteString("\n## Get full context\nRead the change with `" + toolname.CodeGetChange + "` (`change: " + ref +
		"`) before you act: this wake says what happened, not what the change is — its `summary`, its " +
		"`merge_readiness`, and with `files` and `diff` what it changes.\n")
	b.WriteString("\n## How to handle this\n" + wakeHandling(reason) + "\n")
	return b.String()
}

// wakeOpener is the first sentence a code wake says, by why the seat hears.
func wakeOpener(reason Reason, meta map[string]string) string {
	ref := meta[MetaChangeRef]
	switch reason {
	case ReasonMention:
		return "You were @-mentioned on change " + ref + "."
	case ReasonReviewRequested:
		return "You were asked to review change " + ref + "."
	case ReasonChangesRequested:
		return "A reviewer asked for changes on your change " + ref + "."
	case ReasonConflicted:
		return "Your change " + ref + " no longer merges cleanly into its target."
	case ReasonFindingReplied:
		return "Somebody answered a finding you raised on change " + ref + "."
	case ReasonReadyToMerge:
		return "Your change " + ref + " is ready to merge."
	case ReasonMerged:
		return "Your change " + ref + " was merged."
	case ReasonReviewSubmitted:
		if Verdict(meta[MetaVerdict]) == VerdictApprove {
			return "Your change " + ref + " was approved."
		}
		return "Your change " + ref + " was reviewed."
	case ReasonReReview:
		return "Change " + ref + ", on which you asked for changes, has a new revision to look at again."
	case ReasonThread:
		return "A thread you take part in on change " + ref + " has a new comment."
	case ReasonWatcher:
		return "Change " + ref + ", which you take part in, changed."
	case ReasonLeadFallback:
		return "Change " + ref + ", in a repository your unit owns, has nobody asked to review it who may."
	}
	return "Change " + ref + " changed."
}

// wakeHandling is what a code wake asks of its seat, by why it hears: the tool
// that answers, and where an answer goes — on the change, beside what it
// answers. That an addressed wake may not end in silence is the executor's
// contract, so it is not restated.
func wakeHandling(reason Reason) string {
	switch reason {
	case ReasonReviewRequested, ReasonReReview:
		return "Review it with `" + toolname.CodeReviewChange + "`, on the revision you read: approve, comment, or " +
			"request changes, each finding placed on the lines it is about — a blocker only for what " +
			"must not merge."
	case ReasonChangesRequested, ReasonConflicted:
		return "Fix it on the change's source branch, which makes its next revision, then answer each " +
			"finding with `" + toolname.CodeResolveFinding + "` and say what you did with `" +
			toolname.CodeCommentOnChange + "`. " +
			"Disagree in a comment rather than by pushing past it."
	case ReasonMention, ReasonThread, ReasonFindingReplied:
		return "Answer on the change with `" + toolname.CodeCommentOnChange + "`, in the thread you are answering, " +
			"so the answer sits beside the question."
	case ReasonReadyToMerge:
		return "Every precondition of its merge is met. Say so to whoever merges it — where the work " +
			"item it implements is tracked, or on the change with `" + toolname.CodeCommentOnChange + "`."
	case ReasonLeadFallback:
		return "Ask somebody who may review it with `" + toolname.CodeRequestReview + "`, or review it " +
			"yourself with `" + toolname.CodeReviewChange + "`."
	}
	return "This is news, not a request: act only if it bears on work you have in hand, and then say so " +
		"where that work is tracked."
}

// wakeSender renders who made the change as a colleague, by handle — the actor on
// a code wake is a handle, or nobody for the home's own records.
func wakeSender(n notify.Inbound, parties notify.Parties) string {
	actor := n.Metadata[notify.ActorField]
	if actor == "" {
		actor = n.Sender
	}
	if actor == "" {
		return ""
	}
	if parties != nil {
		if party, ok := parties.ByHandle(actor); ok {
			if label := party.Label(); label != "" {
				return label
			}
		}
	}
	return actor
}

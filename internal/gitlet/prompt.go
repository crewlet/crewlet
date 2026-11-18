package gitlet

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
// It names its tools, which ship with this build and are offered to every
// seat a code wake reaches. They come from internal/toolname because
// internal/agent/builtin, which registers them, imports this package;
// TestThePromptNamesOnlyRegisteredChangeTools holds the two together.
type Prompt struct{}

var _ notify.Prompt = Prompt{}

// Source implements [notify.Prompt]: [Source].
func (Prompt) Source() string { return Source }

// RequiresRecon implements [notify.Prompt]: always. A code wake points at a
// change, and even a comment reads against a diff the wake does not carry, so
// filtering context against the wake alone would miss most of what it is about.
func (Prompt) RequiresRecon(notify.Inbound) bool { return true }

// SubjectIsLabel implements [notify.Prompt]: never — the subject is the
// change's name and title, which is what the wake is about.
func (Prompt) SubjectIsLabel(notify.Inbound) bool { return false }

// Addressed implements [notify.Prompt]: the reasons that ask something of
// whoever hears them ([Reason.Addressed]); everything else is news a seat may
// absorb in silence.
func (Prompt) Addressed(n notify.Inbound) bool { return Reason(n.Metadata[MetaVia]).Addressed() }

// PartitionKey implements [notify.Prompt]: the change, by its id.
//
// The id, never the reference: a repository rename would split a
// conversation keyed on `web!42` in two, each half looking ordinary.
func (p Prompt) PartitionKey(metadata map[string]string, subject string) string {
	return p.ConversationIdentity(metadata, subject)
}

// ConversationIdentity implements [notify.Prompt]: `change:<id>`, the
// partition key too, since a change is both the merge unit and the durable
// thread. Empty for a wake naming no change, which no parser writes.
func (Prompt) ConversationIdentity(metadata map[string]string, _ string) string {
	if id := metadata[MetaChangeID]; id != "" {
		return string(KindChange) + ":" + id
	}
	return ""
}

// WorkItem is the work item a code wake is about — the one the change
// implements (ADR-0097) — which is the item the turn it wakes is charged to
// (ADR-0022's trigger rule). None for a change that names none.
func (Prompt) WorkItem(metadata map[string]string) (types.WorkItem, bool) {
	id := metadata[MetaWorkID]
	if id == "" {
		return types.WorkItem{}, false
	}
	return types.WorkItem{Backend: types.WorkBackend(metadata[MetaWorkBackend]), ID: id,
		Key: metadata[MetaWorkKey], Project: metadata[MetaWorkProject]}, true
}

// WakesActor implements [notify.Prompt]: only a check's ending, a failed
// check of a change's head, which is news to its pusher above all
// ([Reason.WakesActor]); no other code record is news to its actor
// ([Concern.Hears]).
func (Prompt) WakesActor(n notify.Inbound) bool { return n.EventType == string(OpCheckFinished) }

// Provenance implements [notify.Prompt]: the change text the wake carries, by
// the writer its record names (stamped by inboundOf); an unknown stamp reads
// as a remark in the change's thread.
func (Prompt) Provenance(n notify.Inbound, parties notify.Parties) notify.Provenance {
	return notify.StatedHosted(n, parties, provenance.SourceChangeComment)
}

// DigestBody implements [notify.Prompt]: what somebody said keeps its text,
// and a record that says nothing of its own collapses to its lead.
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
	case ReasonChecksFailed:
		return "A check of your change " + ref + "'s head failed."
	case ReasonTargetBroken:
		return "Change " + ref + " broke the branch it landed on: a check its target requires fails on it."
	case ReasonConflicted:
		return "Your change " + ref + " no longer merges cleanly into its target."
	case ReasonDequeued:
		return "Change " + ref + " left its target's merge queue without landing."
	case ReasonFindingReplied:
		return "Somebody answered a finding you raised on change " + ref + "."
	case ReasonReadyToMerge:
		// Not "your change": where no author may merge it, the wake reaches
		// the owning unit's lead.
		return "Change " + ref + " is ready to merge: every precondition its target's gate asks is met."
	case ReasonMerged:
		if why := meta[MetaNotClosed]; why != "" {
			// The queue's step two left undone (M5-D27): the change landed, and
			// the work item its enqueue asked closed was not.
			return "Change " + ref + " was merged through its queue, and its work item was not closed: " +
				strings.TrimSuffix(why, ".") + ". Close it where it lives, or say on it why it stays open."
		}
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
// that answers, and that the answer goes on the change. That an addressed wake
// may not end in silence is the executor's contract, not restated here.
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
	case ReasonChecksFailed:
		return "Read what failed with `" + toolname.CodeGetCheckFailures + "`, then fix it on the change's source branch, " +
			"which makes its next revision and runs its checks again. A check its infrastructure ended " +
			"(errored) runs again with `" + toolname.CodeRerunChecks + "`; one the code failed runs again only once the " +
			"code changes."
	case ReasonTargetBroken:
		return "Get the branch green again, whichever way is faster: read what failed with " +
			"`" + toolname.CodeGetCheckFailures + "`, then revert the change with `" + toolname.CodeRevertChange + "` — one write that " +
			"opens \"Revert <repo>!<n>\" into the same target, and with `merge` also merges it as its gate " +
			"allows — or open a change that fixes it forward. Say which on the change with " +
			"`" + toolname.CodeCommentOnChange + "`, so the others it woke do not do it twice."
	case ReasonDequeued:
		return "Read why with `" + toolname.CodeGetChange + "`: its `merge_readiness` names what no longer holds. Fix that " +
			"on the change, then queue it again with `" + toolname.CodeMergeChange + "` from the head you read " +
			"(`expected_head`) — or, where it is not yours to queue, say on it with `" + toolname.CodeCommentOnChange + "` " +
			"that it is ready to queue again."
	case ReasonMention, ReasonThread, ReasonFindingReplied:
		return "Answer on the change with `" + toolname.CodeCommentOnChange + "`, in the thread you are answering, " +
			"so the answer sits beside the question."
	case ReasonReadyToMerge:
		// The wake reaches whoever may merge it, else the owning unit's lead
		// (M5-D26), so it offers the merge and a way to hand it on.
		return "You hear it because you may merge it, or — where none of its authors may — because you " +
			"lead the unit that owns the repository. Merge it with `" + toolname.CodeMergeChange + "` from the head you " +
			"read (`expected_head`): on a target whose gate requires a check that puts it in the queue, " +
			"which merges it once its checks pass. Where the gate does not let you merge it, or it is " +
			"better somebody else's to merge, say who should on the change with `" + toolname.CodeCommentOnChange + "`."
	case ReasonLeadFallback:
		return "Ask somebody who may review it with `" + toolname.CodeRequestReview + "`, or review it " +
			"yourself with `" + toolname.CodeReviewChange + "`."
	}
	return "This is news, not a request: act only if it bears on work you have in hand, and then say so " +
		"where that work is tracked."
}

// wakeSender renders the wake's actor as a colleague: a handle, or nobody for
// the home's own records but a revision's facts and a landing, which name the
// pusher.
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

package artifactsdomain

import (
	"strings"

	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/notify"
	"github.com/crewlet/crewlet/internal/provenance"
)

// Prompt is what a comment on an artifact asks of the seat it woke: who wrote
// what, on which artifact and where on it, and the loop that answers it. The
// tools are named from their authority rows, which declare them.
type Prompt struct{}

var _ notify.Prompt = Prompt{}

// The tools the prompt names, each its authority row's action.
var (
	listCommentsTool   = string(authz.ActionArtifactCommentList)
	commentTool        = string(authz.ActionArtifactComment)
	resolveCommentTool = string(authz.ActionArtifactCommentResolve)
	publishTool        = string(authz.ActionArtifactPublish)
)

// Source implements [notify.Prompt]: [Source].
func (Prompt) Source() string { return Source }

// RequiresRecon implements [notify.Prompt]: a remark carries its words and
// needs none; a resolve or a reopen is a pointer at a thread.
func (Prompt) RequiresRecon(n notify.Inbound) bool { return strings.TrimSpace(n.Body) == "" }

// SubjectIsLabel implements [notify.Prompt]: never, the subject is the
// artifact's title.
func (Prompt) SubjectIsLabel(notify.Inbound) bool { return false }

// Addressed implements [notify.Prompt]: the copies the snapshot addressed, a
// person's remark to the artifact's owner or a seat a remark names.
func (Prompt) Addressed(n notify.Inbound) bool { return n.Metadata[MetaAddressed] == "true" }

// PartitionKey implements [notify.Prompt]: the artifact, so a burst of
// comments on one artifact is one turn.
func (p Prompt) PartitionKey(metadata map[string]string, subject string) string {
	return p.ConversationIdentity(metadata, subject)
}

// ConversationIdentity implements [notify.Prompt]: `artifact:<id>`, the same
// key as the partition, since every thread is activity on the artifact.
func (Prompt) ConversationIdentity(metadata map[string]string, _ string) string {
	if id := metadata[MetaArtifact]; id != "" {
		return Source + ":" + id
	}
	return ""
}

// WakesActor implements [notify.Prompt]: never, a remark is news to everyone
// but its author.
func (Prompt) WakesActor(notify.Inbound) bool { return false }

// Provenance implements [notify.Prompt]: a remark on the engine's own
// artifacts, by the author the parser stated.
func (Prompt) Provenance(n notify.Inbound, parties notify.Parties) notify.Provenance {
	return notify.StatedHosted(n, parties, provenance.SourceArtifactComment)
}

// DigestBody implements [notify.Prompt]: every remark keeps its text.
func (Prompt) DigestBody(_, body string) string { return body }

// Build implements [notify.Prompt].
func (Prompt) Build(n notify.Inbound, parties notify.Parties) string {
	meta := n.Metadata
	var b strings.Builder
	b.WriteString(opener(OpKind(n.EventType), NotifyReason(meta[MetaVia]), meta))
	b.WriteString("\n\n**Artifact:** " + n.Subject)
	if kind := meta[MetaKind]; kind != "" {
		b.WriteString(" (" + kind + ")")
	}
	b.WriteString("\n**By:** " + sender(n, parties))
	if version := meta[MetaVersion]; version != "" {
		b.WriteString("\n**Thread made on version:** " + version)
	}
	if head := meta[MetaHead]; head != "" {
		b.WriteString("\n**Current version:** " + head)
	}
	if label := meta[MetaLabel]; label != "" {
		b.WriteString("\n**Where:** " + label)
	}
	b.WriteString("\n")
	if n.Body != "" {
		b.WriteString("\n**What they wrote:**\n" + n.Body + "\n")
	}
	if quote := meta[MetaQuote]; quote != "" {
		// THE PAGE'S WORDS, which nobody the record names wrote: fenced here
		// under the wake's token as an artifact's file, as the body is by the
		// spine (ADR-0040).
		label := provenance.LabelForHosted(meta[notify.RecipientField], provenance.SourceArtifactFile, nil, false)
		b.WriteString("\n**What it quotes from the page:**\n" + label.Fence(n.Fence, quote) + "\n")
	}
	b.WriteString("\n## Get full context\nRead the thread with `" + listCommentsTool + "` (`artifact: " +
		meta[MetaArtifact] + "`, `thread: " + meta[MetaThread] + "`) before you act: it places the " +
		"thread on the current version, shows what it points at and the picture its writer " +
		"attached, if any.\n")
	b.WriteString("\n## How to handle this\n" + handling(OpKind(n.EventType), meta) + "\n")
	return b.String()
}

// opener is the first sentence a comment wake says, by what the record did and
// why the seat hears.
func opener(op OpKind, via NotifyReason, meta map[string]string) string {
	switch op {
	case OpCommentResolve:
		if v := meta[MetaResolvedVersion]; v != "" {
			return "A comment thread on your artifact was resolved, citing version " + v + "."
		}
		return "A comment thread on your artifact was resolved."
	case OpCommentReopen:
		return "A comment thread on your artifact was reopened: what it asks is not addressed yet."
	}
	reply := op == OpCommentReply
	switch via {
	case NotifyMention:
		return "You were @-mentioned in a comment on an artifact."
	case NotifyAssignee:
		if reply {
			return "A new reply in a comment thread on an artifact made for work assigned to you."
		}
		return "Somebody commented on an artifact made for work assigned to you."
	case NotifyParticipant:
		return "A new reply in a comment thread on an artifact where you remarked."
	}
	if reply {
		return "A new reply in a comment thread on your artifact."
	}
	return "Somebody commented on your artifact."
}

// handling is what a comment wake asks of its seat: the loop of reading the
// thread, changing the artifact on its current version, answering on the
// thread and resolving it citing the version that addressed it.
func handling(op OpKind, meta map[string]string) string {
	if op == OpCommentResolve {
		return "This is news, not a request: the thread is closed. Act only if the version it " +
			"cites leaves something of yours undone."
	}
	head := meta[MetaHead]
	publish := "`" + publishTool + "`"
	if head != "" {
		publish += " with `base_version: " + head + "`"
	}
	return "If the comment asks for a change, make it with " + publish + ", then resolve the " +
		"thread with `" + resolveCommentTool + "` citing the `version` that addressed it. Answer on " +
		"the thread with `" + commentTool + "` (`thread`), saying what changed or why nothing will, " +
		"so the answer sits beside the question."
}

// sender renders who wrote the record as a colleague, by handle.
func sender(n notify.Inbound, parties notify.Parties) string {
	actor := n.Metadata[notify.ActorField]
	if actor == "" {
		actor = n.Sender
	}
	if actor == "" {
		return "someone"
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

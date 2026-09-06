package jira

import (
	"strings"

	"github.com/crewlet/crewlet/internal/notify"
)

// Prompt is what a Jira webhook asks of the seat it reached.
//
// # It tailors to the ROUTING REASON, not to the event type
//
// One "jira:issue_updated" payload reaches an assignee, a watcher and a
// project lead, and it asks each of them for something different: the
// assignee owns the work, the watcher is following it, and the lead is there
// only because nobody else here was named. A prompt keyed on the event would
// have to tell all three the same thing, which in practice means telling all
// three the weakest thing.
//
// [Parser] already decided which of those a copy is and stamped it as
// [RoutedViaField], so the prompt reads that back rather than re-deriving a
// judgement the router made with more information than the prompt has.
//
// NOTHING HERE NAMES A TOOL. The prompt describes the capability — "fetch the
// issue", "set the assignee" — and lets the model find the matching tool in
// its own catalogue, because the deployed MCP server's tool names are not
// knowable by the engine. See docs/concepts/tool-capabilities.md.
type Prompt struct{}

var _ notify.Prompt = Prompt{}

// Source implements [notify.Prompt].
func (Prompt) Source() string { return Backend }

// RequiresRecon implements [notify.Prompt]: always, because a Jira webhook
// is a POINTER.
//
// It says an issue changed and carries one field's before and after; it does
// not carry the status, the priority, the acceptance criteria or the comment
// thread that make the change mean anything. The seat has to fetch the issue
// before it can act, which is what the "Get full context" block below tells
// it to do — so the two answer on the same condition, and a payload naming
// no issue is one the parser never routed.
func (Prompt) RequiresRecon(n notify.Inbound) bool {
	return n.Metadata["issue_key"] != ""
}

// Addressed implements [notify.Prompt]: a mention or an assignment.
//
// The SAME two reasons the prompt frames as a directed ask — see the routing
// reasons above, which are ordered strongest first for exactly this reason. A
// watcher is following the issue rather than being asked about it, and the
// lead fallback is the ticket landing somewhere rather than on somebody: a
// seat obliged to answer either would comment on every field change in its
// unit's projects.
func (Prompt) Addressed(n notify.Inbound) bool {
	switch n.Metadata[RoutedViaField] {
	case ViaMention, ViaAssignee:
		return true
	default:
		return false
	}
}

// ConversationKey implements [notify.Prompt]: the issue is the conversation.
//
// Keyed on the ISSUE KEY rather than the numeric id, because the key rides
// every payload Jira sends — issue and comment alike — while the id is
// absent from some webhook bridges' comment events. Keying on a field that
// is sometimes missing splits one issue's comments and its field updates
// into two coalescing partitions, silently: each half looks like a perfectly
// ordinary conversation.
func (Prompt) ConversationKey(metadata map[string]string, _ string) string {
	return metadata["issue_key"]
}

// WakesActor implements [notify.Prompt]: never.
//
// A tracker reports what somebody DID, not the outcome of what they did.
// Every event here is one the actor already knows about — their own comment,
// their own transition, their own edit — and the parser drops the actor's
// copy at the door. The exception exists for a vendor that reports a
// consequence the actor could not have seen coming, which is a code host's
// failing pipeline, not an issue whose description was just saved.
func (Prompt) WakesActor(string) bool { return false }

// DigestBody implements [notify.Prompt]: comments keep their body, issue
// snapshots collapse.
//
// On an issue event the body is the DESCRIPTION AS IT WAS — a full snapshot
// re-sent on every field change, and Jira re-emits it on changes that did
// not touch it. Five of those in a coalesced digest is the same page five
// times, burying the one line that moved; only the latest matters and it is
// rendered in full below the digest. A COMMENT is a message rather than a
// snapshot: each one is something a person actually said.
func (Prompt) DigestBody(eventType, body string) string {
	if commentEvents[eventType] {
		return body
	}
	return ""
}

// Build implements [notify.Prompt].
func (Prompt) Build(n notify.Inbound, parties notify.Parties) string {
	meta := n.Metadata
	var b strings.Builder

	switch meta[RoutedViaField] {
	case ViaAssignee:
		assigned(&b, n, parties)
	case ViaMention:
		mentioned(&b, n, parties)
	case ViaLeadFallback:
		watching(&b, n, parties)
	default:
		// The watcher fan-out, and anything a later parser adds. The
		// evaluate-and-stay-silent framing is the right default for
		// activity the recipient merely follows, and the right default
		// for a reason this prompt has not been taught.
		watching(&b, n, parties)
	}

	whatChanged(&b, meta)
	getFullContext(&b, meta)
	if meta[RoutedViaField] == ViaLeadFallback {
		whyRouted(&b, meta["project"])
	}
	handling(&b, meta)
	return b.String()
}

// eventLead names what happened in one line.
//
// Worth stating plainly because Jira's own event names do not: Cloud reports
// a comment, a description edit, a status transition and an assignee change
// all as "jira:issue_updated", so a seat that had to infer the kind from the
// changelog shape would guess.
func eventLead(meta map[string]string, actor string) string {
	by := ""
	if actor != "" {
		by = " by " + actor
	}
	switch event := meta["event_type"]; {
	case event == "comment_created":
		return "A comment was added" + by + "."
	case event == "comment_updated":
		return "A comment was edited" + by + "."
	case event == "comment_deleted":
		return "A comment was deleted" + by + "."
	case event == "jira:issue_created":
		return "The issue was created" + by + "."
	case event == "jira:issue_deleted":
		return "The issue was deleted" + by + "."
	case event == "jira:issue_updated":
		return "The issue was updated" + by + "."
	default:
		return ""
	}
}

// assigned: the work is theirs.
func assigned(b *strings.Builder, n notify.Inbound, parties notify.Parties) {
	meta := n.Metadata
	switch meta["event_type"] {
	case "jira:issue_created":
		b.WriteString("A new Jira issue was created with you as its assignee.")
	case "jira:issue_deleted":
		b.WriteString("A Jira issue you were assigned to was deleted.")
	default:
		b.WriteString("A Jira issue you are assigned to was updated.")
	}
	header(b, n, parties)
	if n.Body != "" {
		b.WriteString("\n**Details:**\n" + n.Body + "\n")
	}
}

// mentioned: somebody named them in a comment.
func mentioned(b *strings.Builder, n notify.Inbound, parties notify.Parties) {
	b.WriteString("You were @-mentioned in a comment on a Jira issue.")
	header(b, n, parties)
	body := n.Body
	if body == "" {
		body = "(no text)"
	}
	b.WriteString("\n**Comment:**\n" + body + "\n")
}

// watching: activity on an issue the recipient follows, or that reached them
// as the lead of the owning team.
func watching(b *strings.Builder, n notify.Inbound, parties notify.Parties) {
	if n.Metadata[RoutedViaField] == ViaLeadFallback {
		b.WriteString("A Jira issue in your team's project has activity" +
			" and nobody here is named on it.")
	} else {
		b.WriteString("A Jira issue you are watching was updated.")
	}
	header(b, n, parties)
	if n.Body != "" {
		b.WriteString("\n**Details:**\n" + n.Body + "\n")
	}
}

// header is the identifying block every opener shares.
func header(b *strings.Builder, n notify.Inbound, parties notify.Parties) {
	meta := n.Metadata
	actor := senderLabel(n, parties)
	b.WriteString("\n\n**Issue:** " + n.Subject)
	if lead := eventLead(meta, actor); lead != "" {
		b.WriteString("\n**What happened:** " + lead)
	}
	b.WriteString("\n**By:** " + actor)
	if project := meta["project"]; project != "" {
		b.WriteString("\n**Project:** " + project)
	}
	if at := meta["timestamp"]; at != "" {
		b.WriteString("\n**When:** " + at)
	}
	b.WriteString("\n")
}

// whatChanged renders the changelog the parser already resolved.
func whatChanged(b *strings.Builder, meta map[string]string) {
	if changes := meta["changes"]; changes != "" {
		b.WriteString("\n## What changed\n" + changes + "\n")
	}
}

// getFullContext is the recon pointer, on the same condition
// [Prompt.RequiresRecon] answers.
//
// The two must agree: the flag tells the turn-start prefetch not to bother filtering
// against a pointer, and this block is what makes the pointer followable. A
// flag set without the block sends a seat looking with nothing to look for.
func getFullContext(b *strings.Builder, meta map[string]string) {
	key := meta["issue_key"]
	if key == "" {
		return
	}
	b.WriteString("\n## Get full context" +
		"\nFetch the full details for **" + key + "** with your Jira tools —" +
		" type, status, priority, reporter, description, acceptance criteria," +
		" linked issues and the existing comments — before deciding on next" +
		" steps. Do not act on partial information.\n")
	if url := meta["url"]; url != "" {
		b.WriteString("Link: " + url + "\n")
	}
}

// whyRouted is the delegate / take it / escalate decision.
//
// REACHED ONLY FROM THE LEAD ROUTING, and that restriction is the point. A
// directed routing carries its own signal — being assigned or named says
// what is wanted — while a lead routing says only that nobody else here was
// available, which is a fact about the org chart rather than about the work.
// Left unexplained, a lead reads it as "this is mine" and quietly absorbs
// every unowned ticket in their project.
func whyRouted(b *strings.Builder, project string) {
	where := "this Jira project"
	if project != "" {
		where = "the **" + project + "** Jira project"
	}
	b.WriteString("\n## Why you received this" +
		"\nThis event was routed to you because you lead the team that owns " +
		where + ", and the issue names nobody here — no assignee on your team," +
		" no colleague watching it, no @-mention. You are the default owner by" +
		" FALLBACK, not because the work is yours. Lead-fallback fires only" +
		" when nobody else here is involved, so nobody else is watching this" +
		" issue — if you walk away silently, it goes nowhere." +
		"\n\nDecide one of:" +
		"\n- **Delegate** — assign the issue to the right teammate. Look them" +
		" up on your team to resolve their Jira account id, then set the" +
		" assignee. Future updates route to them rather than back to you." +
		"\n- **Take it yourself** — only if the work clearly falls to you." +
		" Assign the issue to yourself so the routing reflects reality from" +
		" now on." +
		"\n- **Escalate** — if it is out of scope or you cannot identify the" +
		" right owner, hand it to your own manager (named in your identity" +
		" prompt): comment on the issue mentioning them, or reassign it to" +
		" them. Either keeps the trail on the issue, so the work is not" +
		" lost.\n")
}

// handling is the platform-level mechanics block.
//
// Jira's own conventions only. Role-specific behaviour — ownership, tone,
// the quality bar — belongs in the role's behavioural guidelines, and
// repeating it here would put one team's standards on every seat.
func handling(b *strings.Builder, meta map[string]string) {
	if meta["event_type"] == "jira:issue_deleted" {
		// No fetch to send them on and no work to do: the issue is gone,
		// and the only thing worth saying is stop.
		b.WriteString("\n## How to handle this" +
			"\nThe issue no longer exists — stop any in-flight work on it. If" +
			" you had progress worth keeping, record it where your team tracks" +
			" such things; otherwise no action is needed.\n")
		return
	}
	if meta[RoutedViaField] == ViaLeadFallback {
		// The delegate / take it / escalate block above already IS this
		// seat's instruction, and a second list under a second heading
		// makes a lead read two sets of steps and follow neither.
		return
	}

	b.WriteString("\n## How to handle this" +
		"\n1. **Are you the right owner?** If the issue has an assignee who is" +
		" not you and not on your team, observe and stay silent — comment only" +
		" if you hold decision-blocking information they cannot see." +
		"\n2. **Has this already been addressed?** Read the existing comments" +
		" first. If your prior comment, or a teammate's, already covers the" +
		" question, do not restate it." +
		"\n3. **If you are acting**, move the issue to an active status, make" +
		" sure the assignee field names you, and post ONE substantive summary" +
		` when you are done. Avoid running commentary ("starting work", "still` +
		` on it") — that is noise on a surface other people are reading.` +
		"\n4. **If anything is unclear**, comment mentioning the reporter and" +
		" ask. Do not guess.")

	if directAsk(meta[RoutedViaField]) {
		// A WATCHER IS NOT BEING ASKED. Watchers receive events because
		// they once interacted, so telling one they owe an answer is
		// precisely how a tracker fills up with "noted, thanks" — the
		// running commentary rule two lines up, produced by the prompt
		// that forbade it.
		b.WriteString(
			"\n5. **If you were assigned or @-mentioned and have decided not" +
				" to act** — out of scope, wrong owner, already handled — do" +
				" NOT go quiet. Reassign it to the right teammate (look them" +
				" up to resolve their Jira account id), or comment naming who" +
				" should own it. If you cannot identify them, mention your own" +
				" manager so they can route it. An unanswered assignment or" +
				" mention looks exactly like a message that was lost.")
	}
	b.WriteString("\n\n**Never** post internal thinking, status" +
		` acknowledgements, or "I agree with X" as comments. Substance only,` +
		" and stay on this issue.\n")
}

// directAsk reports a routing that asks the recipient for something.
func directAsk(via string) bool {
	return via == "" || via == ViaAssignee || via == ViaMention
}

// senderLabel renders the actor as a colleague.
//
// Through the party registry FIRST, because a Forge-relayed payload carries
// ONLY an account id — no display name, ever — so a prompt reading the
// payload alone names every Cloud event's author "someone". The display name
// is the fallback for a Jira user who is not a seat here, and it is a real
// answer rather than a degraded one: most people in a tracker are not in the
// org chart.
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

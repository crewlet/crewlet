package jira_test

import (
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/jira"
	"github.com/crewlet/crewlet/internal/notify"
)

func inbound(via, event string, extra map[string]string) notify.Inbound {
	meta := map[string]string{
		jira.RoutedViaField: via,
		"event_type":        event,
		"issue_key":         "ENG-42",
		"project":           "ENG",
		"timestamp":         "2024-06-10T06:13:20Z",
		"actor_name":        "Ana Ruiz",
		notify.ActorField:   acctLead,
	}
	for k, v := range extra {
		meta[k] = v
	}
	return notify.Inbound{
		Source: jira.Backend, EventType: event,
		Subject: "[ENG-42] Fix the login redirect", Metadata: meta,
	}
}

// THE PROMPT TAILORS TO THE ROUTING REASON, not to the event type.
//
// One issue_updated payload reaches an assignee, a watcher and a lead, and
// it asks each of them for something different. A prompt keyed on the event
// would have to tell all three the weakest thing.
func TestThePromptAsksSomethingDifferentOfEachRouting(t *testing.T) {
	t.Parallel()
	openers := map[string]string{
		jira.ViaAssignee:     "you are assigned",
		jira.ViaMention:      "@-mentioned",
		jira.ViaWatcher:      "you are watching",
		jira.ViaLeadFallback: "nobody here is named",
	}
	seen := map[string]bool{}
	for via, want := range openers {
		got := (jira.Prompt{}).Build(inbound(via, "jira:issue_updated", nil), nil)
		if !strings.Contains(got, want) {
			t.Errorf("%s prompt does not say %q:\n%s", via, want, got)
		}
		first, _, _ := strings.Cut(got, "\n")
		if seen[first] {
			t.Errorf("%s opens with a line another routing already used: %q", via, first)
		}
		seen[first] = true
	}
}

// ONLY THE LEAD FALLBACK GETS THE DELEGATE / TAKE IT / ESCALATE BLOCK.
//
// A directed routing carries its own signal. A lead routing says only that
// nobody else was available — and left unexplained, a lead reads it as "this
// is mine" and quietly absorbs every unowned ticket in the project.
func TestOnlyTheLeadFallbackIsToldWhyItWasRouted(t *testing.T) {
	t.Parallel()
	lead := (jira.Prompt{}).Build(inbound(jira.ViaLeadFallback, "jira:issue_created", nil), nil)
	if !strings.Contains(lead, "## Why you received this") ||
		!strings.Contains(lead, "**Delegate**") {
		t.Fatalf("the lead is not told what to decide:\n%s", lead)
	}
	if !strings.Contains(lead, "**ENG** Jira project") {
		t.Errorf("the lead is not told which project:\n%s", lead)
	}
	for _, via := range []string{jira.ViaAssignee, jira.ViaMention, jira.ViaWatcher} {
		if got := (jira.Prompt{}).Build(inbound(via, "jira:issue_updated", nil), nil); strings.Contains(got, "Why you received this") {
			t.Errorf("%s was given the lead's delegate decision", via)
		}
	}
}

// A WATCHER IS NOT BEING ASKED FOR ANYTHING.
//
// Watchers receive events because they once interacted, so telling one they
// owe an answer is precisely how a tracker fills up with "noted, thanks" —
// the running-commentary noise the same prompt forbids two lines up.
func TestOnlyADirectAskCarriesTheDoNotGoQuietRule(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		via  string
		want bool
	}{
		{jira.ViaAssignee, true},
		{jira.ViaMention, true},
		{jira.ViaWatcher, false},
	} {
		got := (jira.Prompt{}).Build(inbound(tc.via, "jira:issue_updated", nil), nil)
		has := strings.Contains(got, "do\n NOT go quiet") ||
			strings.Contains(got, "do NOT go quiet")
		if has != tc.want {
			t.Errorf("%s: decline rule present = %v, want %v", tc.via, has, tc.want)
		}
	}
}

// A DELETED ISSUE GETS NO FETCH AND NO WORK. The entity is gone, and the
// only thing worth saying is stop.
func TestADeletedIssueIsToldToStop(t *testing.T) {
	t.Parallel()
	got := (jira.Prompt{}).Build(inbound(jira.ViaAssignee, "jira:issue_deleted", nil), nil)
	if !strings.Contains(got, "stop any in-flight work") {
		t.Fatalf("a deleted issue did not say stop:\n%s", got)
	}
	if strings.Contains(got, "move the issue to an active status") {
		t.Errorf("a deleted issue was told to work on it:\n%s", got)
	}
}

// THE RECON FLAG AND THE FETCH BLOCK ANSWER ON ONE CONDITION.
//
// The flag tells the Plan phase not to bother filtering against a pointer,
// and the block is what makes the pointer followable. A flag set without the
// block sends a seat looking with nothing to look for.
func TestReconAndTheFetchBlockAgree(t *testing.T) {
	t.Parallel()
	with := inbound(jira.ViaAssignee, "jira:issue_updated", nil)
	if !(jira.Prompt{}).RequiresRecon(with) ||
		!strings.Contains((jira.Prompt{}).Build(with, nil), "## Get full context") {
		t.Error("an event naming an issue is a pointer with no pointer block")
	}
	without := inbound(jira.ViaAssignee, "jira:issue_updated", map[string]string{"issue_key": ""})
	if (jira.Prompt{}).RequiresRecon(without) ||
		strings.Contains((jira.Prompt{}).Build(without, nil), "## Get full context") {
		t.Error("an event naming no issue was sent looking for one")
	}
}

// THE LINK IS RENDERED WHEN THERE IS ONE, and omitted when there is not.
func TestTheFetchBlockCarriesTheLinkWhenThereIsOne(t *testing.T) {
	t.Parallel()
	got := (jira.Prompt{}).Build(inbound(jira.ViaAssignee, "jira:issue_updated",
		map[string]string{"url": "https://jira.example.com/browse/ENG-42"}), nil)
	if !strings.Contains(got, "https://jira.example.com/browse/ENG-42") {
		t.Fatalf("the link was not rendered:\n%s", got)
	}
	if strings.Contains((jira.Prompt{}).Build(inbound(jira.ViaAssignee, "jira:issue_updated", nil), nil), "Link:") {
		t.Error("a Link: line was rendered with no link")
	}
}

// THE ISSUE IS THE CONVERSATION, keyed on the KEY rather than the numeric
// id: the key rides every payload Jira sends and the id does not ride some
// bridges' comment events. Keying on a sometimes-absent field splits one
// issue into two coalescing partitions, silently.
func TestTheIssueKeyIsTheConversation(t *testing.T) {
	t.Parallel()
	meta := map[string]string{"issue_key": "ENG-42", "issue_id": "10001"}
	if got := (jira.Prompt{}).ConversationKey(meta, "[ENG-42] anything"); got != "ENG-42" {
		t.Fatalf("conversation key = %q", got)
	}
	if got := (jira.Prompt{}).ConversationKey(map[string]string{"issue_id": "10001"}, "x"); got != "" {
		t.Fatalf("a payload naming no issue got the key %q", got)
	}
}

// COMMENTS KEEP THEIR BODY IN A DIGEST; ISSUE SNAPSHOTS COLLAPSE.
//
// On an issue event the body is the description AS IT WAS, re-sent on every
// field change — five of them in one digest is the same page five times,
// burying the line that moved.
func TestADigestKeepsCommentsAndCollapsesSnapshots(t *testing.T) {
	t.Parallel()
	for event, want := range map[string]string{
		"comment_created":    "what somebody said",
		"comment_deleted":    "what somebody said",
		"jira:issue_updated": "",
		"jira:issue_created": "",
	} {
		if got := (jira.Prompt{}).DigestBody(event, "what somebody said"); got != want {
			t.Errorf("%s digest body = %q, want %q", event, got, want)
		}
	}
}

// A TRACKER NEVER WAKES THE ACTOR. Every event here is one the actor already
// knows about — their own comment, their own transition, their own edit.
func TestTheTrackerNeverWakesTheActor(t *testing.T) {
	t.Parallel()
	for _, event := range []string{
		"jira:issue_created", "jira:issue_updated", "jira:issue_deleted",
		"comment_created", "comment_deleted",
	} {
		if (jira.Prompt{}).WakesActor(event) {
			t.Errorf("%s wakes its own actor", event)
		}
	}
}

// THE ACTOR IS NAMED THROUGH THE REGISTRY FIRST.
//
// A Forge-relayed payload carries ONLY an account id — no display name,
// ever — so a prompt reading the payload alone calls every Cloud event's
// author "someone".
func TestTheActorIsResolvedThroughTheRegistry(t *testing.T) {
	t.Parallel()
	reg := registry(t)
	n := inbound(jira.ViaAssignee, "comment_created",
		map[string]string{"actor_name": "", notify.ActorField: acctLead})
	got := (jira.Prompt{}).Build(n, reg)
	if !strings.Contains(got, "Eng Lead") {
		t.Fatalf("the actor was not named as a colleague:\n%s", got)
	}
	// And a stranger keeps whatever the payload said about them, which is a
	// real answer: most people in a tracker are not in the org chart.
	stranger := inbound(jira.ViaAssignee, "comment_created",
		map[string]string{notify.ActorField: acctStranger, "actor_name": "Someone Else"})
	if !strings.Contains((jira.Prompt{}).Build(stranger, reg), "Someone Else") {
		t.Error("a stranger's display name was dropped")
	}
}

// WHAT CHANGED IS RENDERED WHERE THE PARSER PUT IT.
func TestTheChangelogReachesThePrompt(t *testing.T) {
	t.Parallel()
	got := (jira.Prompt{}).Build(inbound(jira.ViaAssignee, "jira:issue_updated",
		map[string]string{"changes": "- **status**: To Do → In Progress"}), nil)
	if !strings.Contains(got, "## What changed") ||
		!strings.Contains(got, "To Do → In Progress") {
		t.Fatalf("the changelog is missing:\n%s", got)
	}
	if strings.Contains((jira.Prompt{}).Build(inbound(jira.ViaAssignee, "jira:issue_updated", nil), nil),
		"## What changed") {
		t.Error("an empty changelog produced a heading with nothing under it")
	}
}

// THE EVENT LEAD NAMES WHAT HAPPENED.
//
// Worth stating plainly because Jira's own event names do not: a comment, a
// description edit, a status transition and an assignee change all arrive as
// jira:issue_updated on Cloud.
func TestThePromptNamesWhatHappened(t *testing.T) {
	t.Parallel()
	for event, want := range map[string]string{
		"comment_created":    "A comment was added",
		"comment_deleted":    "A comment was deleted",
		"jira:issue_created": "The issue was created",
		"jira:issue_updated": "The issue was updated",
	} {
		got := (jira.Prompt{}).Build(inbound(jira.ViaWatcher, event, nil), nil)
		if !strings.Contains(got, want) {
			t.Errorf("%s: want %q in:\n%s", event, want, got)
		}
	}
}

// THE SPINE'S GENERIC PROMPT IS NOT WHAT A JIRA EVENT GETS.
func TestThePromptIsRegisteredForItsSource(t *testing.T) {
	t.Parallel()
	if got := (jira.Prompt{}).Source(); got != jira.Backend {
		t.Fatalf("source = %q", got)
	}
	prompts := notify.NewPrompts(jira.Prompt{})
	n := inbound(jira.ViaAssignee, "jira:issue_updated", nil)
	if !strings.Contains(prompts.For(jira.Backend).Build(n, nil), "you are assigned") {
		t.Error("the spine did not dispatch to the Jira prompt")
	}
}

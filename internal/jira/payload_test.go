package jira_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/jira"
)

func adf(t *testing.T, raw string) any {
	t.Helper()
	var out any
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		t.Fatalf("fixture: %v", err)
	}
	return out
}

// AN ADF TREE BECOMES PROSE, and the block boundaries survive.
//
// Without them a bulleted list becomes one run-on sentence, which is the
// difference between a model reading acceptance criteria as four items and
// reading them as one.
func TestFlatteningKeepsBlockBoundaries(t *testing.T) {
	t.Parallel()
	got := jira.Flatten(adf(t, `{"type":"doc","content":[
		{"type":"paragraph","content":[{"type":"text","text":"Steps:"}]},
		{"type":"bulletList","content":[
			{"type":"listItem","content":[{"type":"paragraph","content":[
				{"type":"text","text":"open the app"}]}]},
			{"type":"listItem","content":[{"type":"paragraph","content":[
				{"type":"text","text":"log in"}]}]}]}]}`))
	// Exact, because the shape is what a model reads: the lead-in on its
	// own line and one list item per line. What must never happen is the
	// two items running together into "open the applog in".
	want := "Steps:\nopen the app\n\nlog in"
	if got != want {
		t.Fatalf("flattened =\n%q\nwant\n%q", got, want)
	}
}

// A MENTION KEEPS ITS NAME. The sentence stops meaning anything without it:
// "can @ look at this" is not the message somebody sent.
func TestFlatteningRendersMentionsAsNames(t *testing.T) {
	t.Parallel()
	got := jira.Flatten(adf(t, `{"type":"paragraph","content":[
		{"type":"mention","attrs":{"id":"acct-1","text":"@Ana"}},
		{"type":"text","text":" please review"}]}`))
	if got != "@Ana please review" {
		t.Fatalf("flattened = %q", got)
	}
}

// A MENTION WITH ONLY AN ACCOUNT ID RENDERS AS ITSELF rather than vanishing:
// a mention that disappeared would change what the sentence means.
func TestAMentionWithNoNameRendersItsAccount(t *testing.T) {
	t.Parallel()
	got := jira.Flatten(adf(t, `{"type":"paragraph","content":[
		{"type":"mention","attrs":{"id":"acct-1"}},
		{"type":"text","text":" ping"}]}`))
	if !strings.Contains(got, "acct-1") {
		t.Fatalf("the mention was dropped: %q", got)
	}
}

// A PLAIN STRING PASSES THROUGH. Data Center sends wiki markup rather than
// ADF, and a caller must not have to branch on which it got.
func TestFlatteningPassesAPlainStringThrough(t *testing.T) {
	t.Parallel()
	if got := jira.Flatten("just text"); got != "just text" {
		t.Fatalf("flattened = %q", got)
	}
	if got := jira.Flatten(nil); got != "" {
		t.Fatalf("nil flattened to %q", got)
	}
}

// MENTION IDS COME BACK IN DOCUMENT ORDER, DEDUPED, from anywhere in the
// tree — Jira nests them inside list items and panels.
func TestMentionIDsWalkTheWholeTree(t *testing.T) {
	t.Parallel()
	got := jira.MentionIDs(adf(t, `{"type":"doc","content":[
		{"type":"paragraph","content":[{"type":"mention","attrs":{"id":"a"}}]},
		{"type":"panel","content":[{"type":"paragraph","content":[
			{"type":"mention","attrs":{"id":"b"}},
			{"type":"mention","attrs":{"id":"a"}}]}]}]}`))
	if strings.Join(got, ",") != "a,b" {
		t.Fatalf("mentions = %v", got)
	}
}

// A BODY THAT IS NOT A TREE YIELDS NO MENTIONS AND NO PANIC. Data Center
// comment bodies are strings.
func TestMentionIDsToleratesAnyBody(t *testing.T) {
	t.Parallel()
	for _, body := range []any{nil, "plain text", 42, []any{"a"}} {
		if got := jira.MentionIDs(body); len(got) != 0 {
			t.Errorf("%v yielded %v", body, got)
		}
	}
}

// THE CHANGELOG IS RENDERED AS LINES, and a prose field collapses.
//
// Jira re-emits the whole description on changes that did not touch it, so
// rendering the diff inline is a wall of text that usually says nothing.
func TestTheChangelogRendersOneLinePerField(t *testing.T) {
	t.Parallel()
	changes := changesFor(t, map[string]any{"changelog": map[string]any{
		"items": []any{
			map[string]any{"field": "status", "fromString": "To Do", "toString": "In Progress"},
			map[string]any{"field": "description", "fromString": "old", "toString": "new"},
			map[string]any{"field": "priority", "toString": "High"},
			map[string]any{"field": "labels"},
		},
	}})
	want := strings.Join([]string{
		"- **status**: To Do → In Progress",
		"- **description**: was updated",
		"- **priority**: (none) → High",
	}, "\n")
	if changes != want {
		t.Fatalf("changes =\n%q\nwant\n%q", changes, want)
	}
}

// AN ACCOUNT-ID CHANGE RESOLVES TO A COLLEAGUE.
//
// Jira omits fromString/toString on assignee transitions where one side is
// null, so the raw side is an account id — and a prompt line reading
// "712020:aaaa… → 712020:bbbb…" tells a seat nothing at all.
func TestAnAssigneeChangeResolvesThroughTheRegistry(t *testing.T) {
	t.Parallel()
	changes := changesFor(t, map[string]any{"changelog": map[string]any{
		"items": []any{
			map[string]any{"field": "assignee", "from": acctLead, "to": acctSWE},
		},
	}})
	if strings.Contains(changes, acctSWE) || !strings.Contains(changes, "swe") {
		t.Fatalf("the account ids were not resolved: %q", changes)
	}
}

// A DISPLAY STRING WINS OVER THE RAW ID, because it is what Jira itself
// shows and it needs no lookup.
func TestADisplayStringIsPreferredToAnAccountID(t *testing.T) {
	t.Parallel()
	changes := changesFor(t, map[string]any{"changelog": map[string]any{
		"items": []any{
			map[string]any{"field": "assignee", "to": acctSWE, "toString": "Sam Wu"},
		},
	}})
	if !strings.Contains(changes, "Sam Wu") {
		t.Fatalf("changes = %q", changes)
	}
}

// THE TIMESTAMP IS CONVERTED AT THE EDGE. Jira sends epoch milliseconds,
// which is a number no reader of a prompt can place.
func TestTheTimestampIsRenderedAsAnInstant(t *testing.T) {
	t.Parallel()
	meta := metadataFor(t, nil)
	if meta["timestamp"] != "2024-06-10T06:13:20Z" {
		t.Fatalf("timestamp = %q", meta["timestamp"])
	}
}

// THE PROJECT KEY IS UPPER-CASED, because Jira renders it upper and the
// lead map is keyed that way — a payload that arrived lower would match no
// lead and the ticket would reach nobody.
func TestTheProjectKeyIsNormalized(t *testing.T) {
	t.Parallel()
	meta := metadataFor(t, map[string]any{
		"project": map[string]any{"key": "eng", "name": "Engineering"},
	})
	if meta["project"] != "ENG" {
		t.Fatalf("project = %q", meta["project"])
	}
}

// changesFor routes one payload and returns the rendered changelog.
func changesFor(t *testing.T, extra map[string]any) string {
	t.Helper()
	return metadataForExtra(t, map[string]any{"assignee": person(acctSWE)}, extra)["changes"]
}

func metadataFor(t *testing.T, fields map[string]any) map[string]string {
	t.Helper()
	all := map[string]any{"assignee": person(acctSWE)}
	for k, v := range fields {
		all[k] = v
	}
	return metadataForExtra(t, all, nil)
}

// metadataForExtra is the metadata of the one copy a directed payload makes.
//
// Read through Parse rather than from an exported assembler, because the
// assembly IS internal: what a downstream reader can rely on is what a
// routed copy carries.
func metadataForExtra(t *testing.T, fields, extra map[string]any) map[string]string {
	t.Helper()
	reg := registry(t)
	// A project the lead map does not know, when the caller renamed it, is
	// still routed: the assignee is a seat here.
	w := issue("jira:issue_updated", acctLead, fields, extra)
	copies, err := parser(t, nil).Parse(context.Background(), w, reg)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(copies) != 1 {
		t.Fatalf("want one copy, got %d", len(copies))
	}
	return copies[0].Metadata
}

var _ = types.RawWebhook{}

package webhooks

import (
	"strings"
	"testing"
)

// EVERY SUMMARY QUOTES ITS FREE TEXT TO ONE BOUND, whichever route delivered it.
//
// The same Jira summary and the same Confluence title reach this package two
// ways — as the provider's own webhook and through the Forge relay — and each
// builder carried its own literal, so one title was cut at sixty characters on
// one route and fifty on the other. This holds every builder that quotes to
// [quoteRunes], cut on a character and marked, so a route that grows its own
// number fails here rather than in a reader's feed.
func TestEverySummaryQuotesItsFreeTextToOneBound(t *testing.T) {
	t.Parallel()
	// Multi-byte, so a builder that cut bytes rather than characters would
	// land somewhere else — and past the bound by a clear margin.
	long := strings.Repeat("é", quoteRunes+20)
	want := `"` + strings.Repeat("é", quoteRunes) + `…"`

	for _, tc := range []struct {
		name    string
		summary string
	}{
		{"slack message", slackSummary("ceo", map[string]any{"event": map[string]any{
			"type": "message", "user": "U1", "text": long}})},
		{"jira", jiraSummary(map[string]any{"webhookEvent": "jira:issue_created",
			"issue": map[string]any{"key": "OPS-1", "fields": map[string]any{"summary": long}}})},
		{"forge jira", forgeSummary("jira", "jira:issue_created", map[string]any{
			"issue": map[string]any{"key": "OPS-1", "fields": map[string]any{"summary": long}}})},
		{"github pull request", githubSummary("pull_request", map[string]any{"action": "opened",
			"pull_request": map[string]any{"number": 1.0, "title": long}})},
		{"github issue", githubSummary("issues", map[string]any{"action": "opened",
			"issue": map[string]any{"number": 1.0, "title": long}})},
		{"gitlab merge request", gitlabSummary("", map[string]any{"object_kind": "merge_request",
			"object_attributes": map[string]any{"iid": 1.0, "title": long}})},
		{"gitlab issue", gitlabSummary("", map[string]any{"object_kind": "issue",
			"object_attributes": map[string]any{"iid": 1.0, "title": long}})},
		{"confluence", confluenceSummary(map[string]any{"event": "page_updated",
			"page": map[string]any{"title": long}})},
		{"forge confluence", forgeSummary("confluence", "page_updated", map[string]any{
			"page": map[string]any{"title": long}})},
		{"forge confluence comment", forgeSummary("confluence", "comment_created", map[string]any{
			"page": map[string]any{"title": long}, "comment": map[string]any{"id": "c1"}})},
		{"datadog", datadogSummary(map[string]any{"title": long})},
	} {
		if !strings.Contains(tc.summary, want) {
			t.Errorf("%s: summary %q does not quote the text cut at %d characters",
				tc.name, tc.summary, quoteRunes)
		}
	}
}

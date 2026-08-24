package webhooks_test

import (
	"net/http"
	"strings"
	"testing"
	"unicode/utf8"
)

// The summary is the line the activity feed shows, and it is the only part of
// a delivery an operator reads without opening it. What these assert is not
// the exact wording — it is that the fields a reader scans for survive: who did
// it, what they did, and where.

func summaryOf(t *testing.T, e *edge) string {
	t.Helper()
	rows := e.rows(t)
	if len(rows) != 1 {
		t.Fatalf("%d rows in the log, want 1", len(rows))
	}
	return rows[0].Summary
}

func TestSummariesNameWhoWhatAndWhere(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		path    string
		body    string
		headers func(body []byte) map[string]string
		want    []string
	}{
		{
			name: "a GitHub pull request",
			path: "/webhooks/github",
			body: `{"action":"opened","pull_request":{"number":12,"title":"Add retries"},
			        "sender":{"login":"octocat"},"repository":{"full_name":"acme/widgets"}}`,
			headers: func(b []byte) map[string]string {
				h := githubDelivery(b, "gh-secret")
				h["X-GitHub-Event"] = "pull_request"
				return h
			},
			want: []string{"GitHub", "octocat", "PR #12", "Add retries", "acme/widgets"},
		},
		{
			name: "a GitHub push counts its commits",
			path: "/webhooks/github",
			body: `{"ref":"refs/heads/main","commits":[{"id":"a"},{"id":"b"}],
			        "sender":{"login":"octocat"}}`,
			headers: func(b []byte) map[string]string {
				h := githubDelivery(b, "gh-secret")
				h["X-GitHub-Event"] = "push"
				return h
			},
			want: []string{"pushed 2 commit(s) to main"},
		},
		{
			name: "a GitLab merge request",
			path: "/webhooks/gitlab",
			body: `{"object_kind":"merge_request","user":{"username":"jo"},
			        "object_attributes":{"iid":7,"action":"open","title":"Ship it"},
			        "project":{"path_with_namespace":"acme/api"}}`,
			headers: func(b []byte) map[string]string {
				return gitlabDelivery(b, gitlabSecret, "msg_s1", pinned)
			},
			want: []string{"GitLab", "jo", "open MR !7", "Ship it", "acme/api"},
		},
		{
			name: "a GitLab pipeline",
			path: "/webhooks/gitlab",
			body: `{"object_kind":"pipeline","object_attributes":{"status":"failed"},
			        "project":{"path_with_namespace":"acme/api"}}`,
			headers: func(b []byte) map[string]string {
				return gitlabDelivery(b, gitlabSecret, "msg_s2", pinned)
			},
			want: []string{"pipeline failed", "acme/api"},
		},
		{
			name: "a Jira issue",
			path: "/webhooks/jira",
			body: `{"webhookEvent":"jira:issue_created","user":{"displayName":"Sam"},
			        "issue":{"key":"OPS-4","fields":{"summary":"Disk full"}}}`,
			headers: func(b []byte) map[string]string { return atlassianDelivery(b, "jira-secret") },
			want:    []string{"Jira", "Sam", "issue created", "OPS-4", "Disk full"},
		},
		{
			name: "a Confluence page",
			path: "/webhooks/confluence",
			body: `{"event":"page_updated","user":{"displayName":"Ada"},
			        "page":{"title":"Runbook","space":{"key":"OPS"}}}`,
			headers: func(b []byte) map[string]string { return atlassianDelivery(b, "conf-secret") },
			want:    []string{"Confluence", "Ada", "page updated", "[OPS]", "Runbook"},
		},
		{
			name: "a Plane work item",
			path: "/webhooks/plane",
			body: `{"event":"issue","action":"created","data":{"name":"Ship it"},
			        "activity":{"actor":{"display_name":"Ash"}}}`,
			headers: func(b []byte) map[string]string { return planeDelivery(b, "pl-secret") },
			want:    []string{"Plane", "Ash", "issue", "created", "Ship it"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			e := newEdge(t)
			body := []byte(tc.body)
			if got := e.post(t, tc.path, body, tc.headers(body)).Code; got != http.StatusOK {
				t.Fatalf("got %d", got)
			}
			summary := summaryOf(t, e)
			for _, want := range tc.want {
				if !strings.Contains(summary, want) {
					t.Errorf("summary %q does not mention %q", summary, want)
				}
			}
		})
	}
}

func TestASlackMessagePreviewsItsText(t *testing.T) {
	t.Parallel()
	e := newEdge(t)
	body := []byte(`{"type":"event_callback","event_id":"E2",
	  "event":{"type":"message","user":"U1","text":"the build is broken","channel":"C9"}}`)
	if got := e.post(t, "/webhooks/slack/ceo", body, slackDelivery(body, "slack-secret", pinned)).Code; got != http.StatusOK {
		t.Fatalf("got %d", got)
	}
	summary := summaryOf(t, e)
	for _, want := range []string{"Slack → ceo", "U1", "the build is broken", "#C9"} {
		if !strings.Contains(summary, want) {
			t.Errorf("summary %q does not mention %q", summary, want)
		}
	}
}

func TestALongTitleIsTrimmedWithoutBreakingItsCharacters(t *testing.T) {
	t.Parallel()
	// Trimming by bytes through UTF-8 leaves a broken code point, which the
	// feed renders as a replacement character — and which is invalid JSON's
	// problem to explain rather than the trimming's.
	//
	// The leading "x" is load-bearing. The trim is at 60 characters, and
	// "é" is 2 bytes: without the offset a BYTE slice at 60 lands exactly
	// on a character boundary and produces valid UTF-8 anyway. Mutation
	// testing found that — byte-slicing survived this test until the
	// fixture stopped aligning with it.
	e := newEdge(t)
	title := "x" + strings.Repeat("é", 100)
	body := []byte(`{"webhookEvent":"jira:issue_created","issue":{"key":"OPS-1",
	  "fields":{"summary":"` + title + `"}}}`)
	if got := e.post(t, "/webhooks/jira", body, atlassianDelivery(body, "jira-secret")).Code; got != http.StatusOK {
		t.Fatalf("got %d", got)
	}
	// Read off the LIVE envelope rather than the stored row: whether a
	// database preserves an invalid byte sequence in a TEXT column is its
	// business, and asserting through it would test that instead of this.
	e.stream.mu.Lock()
	summary := e.stream.seen[0].Summary
	e.stream.mu.Unlock()

	if !strings.Contains(summary, "...") {
		t.Errorf("a 100-character title was not trimmed: %q", summary)
	}
	if !utf8.ValidString(summary) {
		t.Errorf("the trim broke a character: %q", summary)
	}
}

func TestASummaryOfAnUnfamiliarPayloadIsShortNotBroken(t *testing.T) {
	t.Parallel()
	// A webhook body is the one input here with no schema: a provider adds
	// a field, a self-hosted fork renames one. That must shorten the line,
	// never fail the delivery.
	e := newEdge(t)
	for _, body := range []string{
		`{}`,
		`{"action":null,"sender":"a string where an object was"}`,
		`{"issue":{"number":"12"},"pull_request":[]}`,
		`{"object_attributes":{"iid":3.5}}`,
	} {
		raw := []byte(body)
		res := e.post(t, "/webhooks/github", raw, githubDelivery(raw, "gh-secret"))
		if res.Code != http.StatusOK {
			t.Errorf("body %s got %d, want 200", body, res.Code)
		}
	}
	rows := e.rows(t)
	for _, row := range rows {
		if row.Summary == "" {
			t.Errorf("row %s has no summary at all", row.ID)
		}
		if strings.Contains(row.Summary, "%!") || strings.Contains(row.Summary, "<nil>") {
			t.Errorf("summary leaked a formatting artefact: %q", row.Summary)
		}
	}
}

func TestAnIdentifierIsNotPrintedAsAFloat(t *testing.T) {
	t.Parallel()
	// JSON decodes every number as a float64, so PR #42 reads "42.000000"
	// from anything that formats it naively.
	e := newEdge(t)
	body := []byte(`{"action":"opened","issue":{"number":42,"title":"x"}}`)
	if got := e.post(t, "/webhooks/github", body, githubDelivery(body, "gh-secret")).Code; got != http.StatusOK {
		t.Fatalf("got %d", got)
	}
	summary := summaryOf(t, e)
	// "#42" alone is not the assertion: "#42.000000" contains it, which is
	// exactly the defect. Mutation testing found that — a six-decimal
	// format survived this test until the check named the whole number.
	if !strings.Contains(summary, "issue #42 ") {
		t.Errorf("summary %q does not carry the issue number as a whole number", summary)
	}
	if strings.Contains(summary, "42.") {
		t.Errorf("summary %q rendered the id as a float", summary)
	}
}

func TestASummaryHasNoGapsWhereAFieldWasAbsent(t *testing.T) {
	t.Parallel()
	// A webhook body routinely omits half of what a summary would like.
	// Joining blindly produces "GitHub  opened PR #12" with a doubled
	// space, or a line that starts or ends with one — the reader sees it
	// on every row.
	e := newEdge(t)
	// No sender, no repository, no title: three of the five parts absent.
	body := []byte(`{"action":"opened","pull_request":{"number":3}}`)
	headers := githubDelivery(body, "gh-secret")
	headers["X-GitHub-Event"] = "pull_request"
	if got := e.post(t, "/webhooks/github", body, headers).Code; got != http.StatusOK {
		t.Fatalf("got %d", got)
	}
	summary := summaryOf(t, e)
	if strings.Contains(summary, "  ") {
		t.Errorf("summary %q has a gap where an absent field was", summary)
	}
	if summary != strings.TrimSpace(summary) {
		t.Errorf("summary %q is padded at one end", summary)
	}
}

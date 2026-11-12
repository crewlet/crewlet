package github_test

import (
	"errors"
	"net/http"
	"slices"
	"testing"

	"github.com/crewlet/crewlet/internal/github"
)

// moversPull is acme/web#41 open at factsHead, opened by author, with the
// reviewers, teams and auto-merge given in GitHub's own spelling.
func moversPull(author, reviewers, teams, autoMerge string) string {
	return `{"number":41,"state":"open","merged":false,"draft":false,"user":{"login":"` + author + `"},` +
		`"head":{"sha":"` + factsHead + `"},"requested_reviewers":` + reviewers +
		`,"requested_teams":` + teams + `,"auto_merge":` + autoMerge + `}`
}

// A PULL REQUEST NOBODY IS MOVING READS SO: no reviewer asked, no review of its
// head but its author's own or one of an older commit, and no merge armed.
func TestAPullRequestNobodyMovesReadsSo(t *testing.T) {
	t.Parallel()
	client, _ := newFactsAPI(t, map[string]http.HandlerFunc{
		factsPull: answer(moversPull("Builder[bot]", "[]", "[]", "null")),
		factsReviews: answer(jsonList(reviewJSON("builder[bot]", "COMMENTED", factsHead),
			reviewJSON("reviewer", "APPROVED", factsOlder), reviewJSON("reviewer", "PENDING", factsHead),
			reviewJSON("other", "DISMISSED", factsHead))),
	})
	got, err := client.PullMovers(t.Context(), "acme", "web", 41)
	if err != nil {
		t.Fatalf("PullMovers: %v", err)
	}
	if !got.Open() || got.Moving() || got.Head != factsHead || got.Author != "builder[bot]" {
		t.Errorf("an unreviewed pull request read as %+v, want open, at its head, moved by nobody", got)
	}
}

// EACH MOVER IS SEEN: a reviewer or a team asked for, a review submitted on
// the head, and a merge armed each mean somebody or something moves it.
func TestEveryMoverOfAPullRequestIsSeen(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, pull, reviews string
		check               func(github.PullMovers) bool
	}{
		{"a reviewer asked", moversPull("builder", `[{"login":"Reviewer"}]`, "[]", "null"), "[]",
			func(m github.PullMovers) bool { return slices.Equal(m.Requested, []string{"reviewer"}) }},
		{"a team asked", moversPull("builder", "[]", `[{"slug":"Web"}]`, "null"), "[]",
			func(m github.PullMovers) bool { return slices.Equal(m.Requested, []string{"team:web"}) }},
		{"a review of the head", moversPull("builder", "[]", "[]", "null"),
			jsonList(reviewJSON("Reviewer", "CHANGES_REQUESTED", factsHead)),
			func(m github.PullMovers) bool { return slices.Equal(m.Reviewed, []string{"reviewer"}) }},
		{"a merge armed", moversPull("builder", "[]", "[]", `{"merge_method":"squash"}`), "[]",
			func(m github.PullMovers) bool { return m.AutoMerge }},
	} {
		client, _ := newFactsAPI(t, map[string]http.HandlerFunc{
			factsPull: answer(tc.pull), factsReviews: answer(tc.reviews)})
		got, err := client.PullMovers(t.Context(), "acme", "web", 41)
		if err != nil {
			t.Fatalf("%s: PullMovers: %v", tc.name, err)
		}
		if !got.Moving() || !tc.check(got) {
			t.Errorf("%s: read as %+v, want it moving", tc.name, got)
		}
	}
}

// A SETTLED PULL REQUEST IS NOT ASKED FOR ITS REVIEWS, and a list it cannot
// read fails the whole read rather than answer nobody.
func TestASettledPullRequestReadsNoReviewsAndAnUnreadOneFails(t *testing.T) {
	t.Parallel()
	client, api := newFactsAPI(t, map[string]http.HandlerFunc{
		factsPull: answer(`{"state":"closed","merged":true,"merged_at":"2026-03-06T09:00:00Z",` +
			`"head":{"sha":"` + factsHead + `"},"user":{"login":"builder"}}`)})
	got, err := client.PullMovers(t.Context(), "acme", "web", 41)
	if err != nil || got.Open() || !got.Merged || got.MergedAt == nil {
		t.Fatalf("a merged pull request read as %+v (%v)", got, err)
	}
	if n := api.count(factsReviews); n != 0 {
		t.Errorf("a merged pull request's reviews were read %d times, want none", n)
	}
	client, _ = newFactsAPI(t, map[string]http.HandlerFunc{
		factsPull:    answer(moversPull("builder", "[]", "[]", "null")),
		factsReviews: refuse(http.StatusForbidden)})
	var refused *github.APIError
	if _, err := client.PullMovers(t.Context(), "acme", "web", 41); !errors.As(err, &refused) {
		t.Errorf("reviews the host refused read as %v, want the refusal", err)
	}
}

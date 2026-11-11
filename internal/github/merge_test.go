package github_test

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/github"
)

const factsMerge = factsPull + "/merge"

// THE MERGE NAMES THE HEAD IT WAS DECIDED ON. Left out, GitHub merges
// whatever the head is by then, so a push between the decision and the merge
// would land code nobody decided on.
func TestMergePullSendsTheDecidedHead(t *testing.T) {
	t.Parallel()
	var sent struct {
		SHA         string `json:"sha"`
		MergeMethod string `json:"merge_method"`
	}
	var method string
	client, _ := newFactsAPI(t, map[string]http.HandlerFunc{
		factsMerge: func(w http.ResponseWriter, r *http.Request) {
			method = r.Method
			if err := json.NewDecoder(r.Body).Decode(&sent); err != nil {
				t.Errorf("decode the merge body: %v", err)
			}
			_, _ = w.Write([]byte(`{"sha":"` + factsOlder + `","merged":true,` +
				`"message":"Pull Request successfully merged"}`))
		},
	})
	got, err := client.MergePull(t.Context(), "acme", "web", 41, factsHead, github.MergeMethodSquash)
	if err != nil {
		t.Fatalf("MergePull: %v", err)
	}
	if method != http.MethodPut || sent.SHA != factsHead || sent.MergeMethod != "squash" {
		t.Errorf("sent %s %+v, want PUT of the head by squash", method, sent)
	}
	if !got.Merged || got.SHA != factsOlder || got.Message == "" {
		t.Errorf("the merge read as %+v", got)
	}
}

// GITHUB'S REFUSALS OF A MERGE ARE TYPED, so a caller words each one without
// matching a message GitHub rewords freely, and GitHub's own detail (which
// names the missing check or review) is still reachable beneath. A rate limit
// is not the app being forbidden, and a 200 that did not merge is not a merge.
func TestGitHubsMergeRefusalsAreTyped(t *testing.T) {
	t.Parallel()
	sentinels := []error{
		github.ErrNotMergeable, github.ErrHeadMoved, github.ErrMergeInvalid, github.ErrMergeForbidden,
	}
	for name, tc := range map[string]struct {
		answer http.HandlerFunc
		want   error
	}{
		"405 not mergeable": {refuse(http.StatusMethodNotAllowed), github.ErrNotMergeable},
		"409 head moved":    {refuse(http.StatusConflict), github.ErrHeadMoved},
		"422 invalid":       {refuse(http.StatusUnprocessableEntity), github.ErrMergeInvalid},
		"403 forbidden":     {refuse(http.StatusForbidden), github.ErrMergeForbidden},
		"403 rate limited":  {refuse(http.StatusForbidden, "X-RateLimit-Remaining", "0"), nil},
		"404 not there":     {refuse(http.StatusNotFound), nil},
		"200 not merged": {
			answer(`{"merged":false,"message":"not this time"}`), github.ErrNotMergeable,
		},
	} {
		client, _ := newFactsAPI(t, map[string]http.HandlerFunc{factsMerge: tc.answer})
		_, err := client.MergePull(t.Context(), "acme", "web", 41, factsHead, github.MergeMethodMerge)
		if err == nil {
			t.Errorf("%s: merged", name)
			continue
		}
		for _, sentinel := range sentinels {
			if errors.Is(err, sentinel) != errors.Is(tc.want, sentinel) {
				t.Errorf("%s: errors.Is(%v, %v) = %v", name, err, sentinel, errors.Is(err, sentinel))
			}
		}
		var apiErr *github.APIError
		if strings.HasPrefix(name, "4") && (!errors.As(err, &apiErr) || !strings.Contains(apiErr.Detail, "refused")) {
			t.Errorf("%s: GitHub's own detail is lost: %v", name, err)
		}
	}
}

// A MERGE THAT NAMES NO EXACT HEAD, OR NO METHOD GITHUB OFFERS, IS NEVER SENT.
func TestAMergeWithoutAnExactHeadOrAKnownMethodIsNotSent(t *testing.T) {
	t.Parallel()
	client, api := newFactsAPI(t, map[string]http.HandlerFunc{factsMerge: answer(`{"merged":true}`)})
	for name, tc := range map[string]struct {
		number int
		sha    string
		method github.MergeMethod
	}{
		"no sha":       {41, "", github.MergeMethodMerge},
		"a branch":     {41, "main", github.MergeMethodMerge},
		"a short sha":  {41, factsHead[:7], github.MergeMethodMerge},
		"no method":    {41, factsHead, ""},
		"fast-forward": {41, factsHead, "fast-forward"},
		"no number":    {0, factsHead, github.MergeMethodMerge},
	} {
		if _, err := client.MergePull(t.Context(), "acme", "web", tc.number, tc.sha, tc.method); err == nil {
			t.Errorf("%s: merged", name)
		}
	}
	if n := api.count(factsMerge); n != 0 {
		t.Errorf("%d merges were sent", n)
	}
}

// A DEPLOY CARRIES A MERGE WHEN THE MERGE IS REACHABLE FROM IT: GitHub's
// comparison of the merge...the deploy says ahead or identical. Behind or
// diverged is a deploy that does not carry it.
func TestContainsReadsTheComparison(t *testing.T) {
	t.Parallel()
	for status, want := range map[string]bool{
		"ahead": true, "identical": true, "behind": false, "diverged": false,
	} {
		client, _ := newFactsAPI(t, map[string]http.HandlerFunc{
			"/repos/acme/web/compare/" + factsOlder + "..." + factsHead: answer(`{"status":"` + status + `"}`),
		})
		got, err := client.Contains(t.Context(), "acme", "web", factsHead, factsOlder)
		if err != nil || got != want {
			t.Errorf("%s: contains = %v, %v; want %v", status, got, err, want)
		}
	}
	client, _ := newFactsAPI(t, map[string]http.HandlerFunc{
		"/repos/acme/web/compare/" + factsOlder + "..." + factsHead: answer(`{"status":"sideways"}`),
	})
	if _, err := client.Contains(t.Context(), "acme", "web", factsHead, factsOlder); err == nil {
		t.Error("an unknown comparison read as an answer")
	}
	if _, err := client.Contains(t.Context(), "acme", "web", "main", factsOlder); err == nil {
		t.Error("a branch was compared as a commit")
	}
}

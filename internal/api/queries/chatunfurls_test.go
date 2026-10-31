package queries_test

import (
	"context"
	"errors"
	"testing"

	"github.com/crewlet/crewlet/internal/api/queries"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/github"
)

// unfurlSources is chat's sources on a deployment reached at
// https://crewlet.example.com, with the record questions a preview asks
// answered by stand-ins that say who asked them.
func unfurlSources(t *testing.T) (*queries.Registry, *[]string) {
	t.Helper()
	s := chatSources(t, newStubChat(), &stubChatSearch{}, nil)
	s.DashboardBase = func() string { return "https://crewlet.example.com" }
	r := queries.NewRegistry()
	queries.Register(r, s)
	var asked []string
	answer := func(what string, body map[string]any, refuse error) queries.Answer {
		return func(_ context.Context, p queries.Params) (any, error) {
			asked = append(asked, what+" "+p.String("id"))
			if refuse != nil {
				return nil, refuse
			}
			return body, nil
		}
	}
	r.Register("work_item", queries.Everybody(), answer("work_item", map[string]any{
		"task": map[string]any{"key": "ENG-12", "title": "Rotate keys", "status": "In progress",
			"status_group": "active", "assignee": "sre"}}, nil))
	r.Register("page", queries.Everybody(), answer("page", nil,
		errors.Join(queries.ErrUnauthorized)))
	r.Register("artifact", queries.Everybody(), answer("artifact", map[string]any{
		"title": "Launch plan", "kind": "doc", "owner": "pm", "owner_kind": "agent"}, nil))
	return r, &asked
}

// A LINK IS PREVIEWED AS ITS RECORD ONLY WHEN IT IS THIS DASHBOARD'S OWN, and
// only as the caller may read it: each record is asked of its own screen's
// question, the href is built from the record, and a link under another
// origin, or to a record the caller may not open, is drawn without one.
//
// Mutation: drop the origin check and the lookalike link is previewed as the
// company's task; swallow the refusal and the page is previewed.
func TestALinkIsPreviewedAsTheRecordItOpens(t *testing.T) {
	t.Parallel()
	r, asked := unfurlSources(t)
	got, err := r.Answer(asAna(t), "chat_unfurls", map[string]any{"urls": []any{
		"https://crewlet.example.com/#/worklet/ENG-12",
		"https://lookalike.example.net/#/worklet/ENG-12",
		"https://crewlet.example.com/#/doclet/pages/0b9d2a52-6d1c-4f0e-9a51-2a7f3c1d9e31",
		"https://crewlet.example.com/#/artifacts/0b9d2a52-6d1c-4f0e-9a51-2a7f3c1d9e32",
		"https://github.com/acme/web/pull/41",
	}})
	body := chatJSON(t, got, err)
	unfurls, _ := body["unfurls"].([]any)
	if len(unfurls) != 2 {
		t.Fatalf("previewed %d links, want the task and the artifact: %v", len(unfurls), unfurls)
	}
	task, _ := unfurls[0].(map[string]any)
	want := map[string]any{"kind": "task", "code": "ENG-12", "title": "Rotate keys",
		"status": "In progress", "tone": "info", "owner": "sre", "href": "#/worklet/ENG-12",
		"url": "https://crewlet.example.com/#/worklet/ENG-12"}
	for k, v := range want {
		if task[k] != v {
			t.Errorf("the task's %s is %v, want %v", k, task[k], v)
		}
	}
	artifact, _ := unfurls[1].(map[string]any)
	if artifact["kind"] != "artifact" || artifact["owner"] != "pm" ||
		artifact["href"] != "#/artifacts/0b9d2a52-6d1c-4f0e-9a51-2a7f3c1d9e32" {
		t.Errorf("the artifact previewed as %v", artifact)
	}
	if len(*asked) != 3 {
		t.Errorf("the record questions asked were %v, want one per own address", *asked)
	}

	// ONE MESSAGE'S LINKS AT A TIME.
	nine := make([]any, 9)
	for i := range nine {
		nine[i] = "https://crewlet.example.com/#/worklet/ENG-12"
	}
	if _, err := r.Answer(asAna(t), "chat_unfurls", map[string]any{"urls": nine}); !errors.Is(err, queries.ErrBadParams) {
		t.Errorf("nine links answered %v, want bad params", err)
	}
}

// pulls is a pull request reader that answers what it holds and says what it
// was asked.
type pulls struct {
	held  map[github.PullRef]github.PullRequest
	asked []github.PullRef
}

func (p *pulls) PullRequestPreview(_ context.Context, ref github.PullRef) (github.PullRequest, error) {
	p.asked = append(p.asked, ref)
	pr, ok := p.held[ref]
	if !ok {
		return github.PullRequest{}, errors.New("not found")
	}
	return pr, nil
}

// A PULL REQUEST IS PREVIEWED ONLY ON GITHUB.COM AND ONLY UNDER AN OWNER THE
// COMPANY'S GITHUB CONFIGURATION NAMES, with its repository, number, title,
// state and size and nothing else; any other address, and a pull request that
// cannot be read, is drawn as a plain link. Nothing is fetched for a link the
// rules leave out.
//
// Mutation: drop the owner check and the stranger's pull request is read.
func TestAPullRequestIsPreviewedOnlyUnderTheCompanysOwners(t *testing.T) {
	t.Parallel()
	cfg := company(t)
	cfg.Integrations.GitHub = &config.GitHub{Enabled: true, WebhookSecret: "hook",
		Provisioning: &config.GitHubProvisioning{Org: "acme"}}
	reader := &pulls{held: map[github.PullRef]github.PullRequest{
		{Owner: "acme", Repo: "web", Number: 41}: {Number: 41, Title: "Add a per-IP ceiling",
			State: "open", Draft: true, Additions: 120, Deletions: 14},
	}}
	s := chatSources(t, newStubChat(), &stubChatSearch{}, nil)
	s.Company, s.PullRequests = companySource(t, cfg), reader
	got, err := askChat(t, s, "chat_unfurls", map[string]any{"urls": []any{
		"https://github.com/acme/web/pull/41/files",
		"https://github.com/stranger/web/pull/7",
		"https://github.com/acme/web/pull/42",
		"https://github.example.com/acme/web/pull/41",
	}})
	body := chatJSON(t, got, err)
	unfurls, _ := body["unfurls"].([]any)
	if len(unfurls) != 1 {
		t.Fatalf("previewed %v, want the one readable pull request under acme", unfurls)
	}
	pr, _ := unfurls[0].(map[string]any)
	want := map[string]any{"kind": "pull_request", "code": "acme/web#41",
		"title": "Add a per-IP ceiling", "status": "Draft", "tone": "neutral",
		"meta": "+120 -14", "href": "https://github.com/acme/web/pull/41"}
	for k, v := range want {
		if pr[k] != v {
			t.Errorf("the preview's %s is %v, want %v", k, pr[k], v)
		}
	}
	if _, has := pr["owner"]; has {
		t.Errorf("the preview names its author: %v", pr)
	}
	for _, ref := range reader.asked {
		if ref.Owner != "acme" {
			t.Errorf("a pull request under %s was read", ref.Owner)
		}
	}
}

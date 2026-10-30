package atlassian_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/atlassian"
)

// tokenOrg is a fake organization answering the token routes.
type tokenOrg struct {
	server *httptest.Server
	mu     sync.Mutex
	calls  []string
	bodies []map[string]any
	answer map[string]struct {
		status int
		body   string
	}
}

func newTokenOrg(t *testing.T) *tokenOrg {
	t.Helper()
	o := &tokenOrg{answer: map[string]struct {
		status int
		body   string
	}{}}
	o.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		_ = json.Unmarshal(raw, &body)
		key := r.Method + " " + r.URL.Path
		o.mu.Lock()
		o.calls = append(o.calls, key)
		o.bodies = append(o.bodies, body)
		a, ok := o.answer[key]
		o.mu.Unlock()
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"message":"not found"}`))
			return
		}
		w.WriteHeader(a.status)
		_, _ = w.Write([]byte(a.body))
	}))
	t.Cleanup(o.server.Close)
	return o
}

func (o *tokenOrg) on(method, path string, status int, body string) {
	o.answer[method+" "+path] = struct {
		status int
		body   string
	}{status, body}
}

func (o *tokenOrg) client() *atlassian.Client {
	return atlassian.NewClient(atlassian.ClientOptions{BaseURL: o.server.URL})
}

// A LANE THAT IS OFF CONTRIBUTES NO SCOPE, so its product refuses the token
// even while the account keeps its product grant.
func TestScopesAreTheUnionOfTheLanesASeatIsOn(t *testing.T) {
	t.Parallel()
	if got := atlassian.ScopesFor(true, false); !slices.Equal(got, atlassian.JiraScopes) {
		t.Errorf("Jira only = %v", got)
	}
	if got := atlassian.ScopesFor(false, true); !slices.Equal(got, atlassian.ConfluenceScopes) {
		t.Errorf("Confluence only = %v", got)
	}
	if got := atlassian.ScopesFor(false, false); len(got) != 0 {
		t.Errorf("no lane = %v", got)
	}
	both := atlassian.ScopesFor(true, true)
	if !slices.Equal(both, atlassian.AgentScopes) || len(both) != len(atlassian.JiraScopes)+len(atlassian.ConfluenceScopes) {
		t.Errorf("both lanes = %v, want the union %v", both, atlassian.AgentScopes)
	}
}

func TestAGrantNamesEachProductItIsFor(t *testing.T) {
	t.Parallel()
	rules := atlassian.GrantsOf("cloud-1", atlassian.GrantConfluence)
	if len(rules) != 1 || rules[0].Resource != "ari:cloud:confluence::site/cloud-1" ||
		rules[0].Role != "ari:cloud:confluence::role/product/member" {
		t.Fatalf("rules = %+v", rules)
	}
	if got := atlassian.GrantsFor("cloud-1"); len(got) != len(atlassian.Products) {
		t.Fatalf("GrantsFor = %+v", got)
	}
}

// A SEAT ON NO LANE HOLDS NO TOKEN, and the refusal is made before anything is
// asked of Atlassian: a scopeless mint is a 400 that reads like a malformed
// request.
func TestAMintWithNoScopesIsRefusedBeforeTheCall(t *testing.T) {
	t.Parallel()
	org := newTokenOrg(t)
	_, err := org.client().MintToken(context.Background(), "key", "acc-1", "crewlet-sre", nil, time.Now())
	if err == nil {
		t.Fatal("a scopeless mint was accepted")
	}
	if len(org.calls) != 0 {
		t.Fatalf("calls = %v, want none", org.calls)
	}
}

func TestAMintCarriesTheScopesItWasAskedFor(t *testing.T) {
	t.Parallel()
	org := newTokenOrg(t)
	org.on(http.MethodPost, "/users/acc-1/manage/api-tokens", http.StatusOK,
		`{"id": "tok-1", "label": "x", "token": "fake-secret"}`)
	now := time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)
	minted, err := org.client().MintToken(context.Background(), "key", "acc-1", "crewlet-sre",
		atlassian.JiraScopes, now)
	if err != nil || minted.ID != "tok-1" {
		t.Fatalf("mint = %+v, %v", minted, err)
	}
	got, _ := org.bodies[0]["scopes"].([]any)
	if len(got) != len(atlassian.JiraScopes) || got[0] != atlassian.JiraScopes[0] {
		t.Fatalf("scopes sent = %v", org.bodies[0]["scopes"])
	}
}

func TestTokensAreListedAndRevokedByID(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	org := newTokenOrg(t)
	org.on(http.MethodGet, "/users/acc-1/manage/api-tokens", http.StatusOK, `[
		{"id": "tok-1", "label": "crewlet-sre-1", "createdAt": "2026-10-01T00:00:00Z", "expiry": "2027-07-28T00:00:00Z"},
		{"id": "tok-2", "label": "hand-made"}
	]`)
	org.on(http.MethodDelete, "/users/acc-1/manage/api-tokens/tok-1", http.StatusNoContent, ``)
	c := org.client()
	tokens, err := c.ListTokens(ctx, "key", "acc-1")
	if err != nil || len(tokens) != 2 || tokens[0].Label != "crewlet-sre-1" {
		t.Fatalf("tokens = %+v, %v", tokens, err)
	}
	if n, err := c.CountTokens(ctx, "key", "acc-1"); err != nil || n != 2 {
		t.Fatalf("CountTokens = %d, %v", n, err)
	}
	if err := c.RevokeToken(ctx, "key", "acc-1", "tok-1"); err != nil {
		t.Fatal(err)
	}
	if err := c.RevokeToken(ctx, "key", "acc-1", "tok-gone"); err != nil {
		t.Fatalf("a token already gone failed the revoke: %v", err)
	}
}

func TestARefusedRevokeKeepsItsStatus(t *testing.T) {
	t.Parallel()
	org := newTokenOrg(t)
	org.on(http.MethodDelete, "/users/acc-1/manage/api-tokens/tok-1", http.StatusForbidden,
		`{"message":"The API key is not permitted to manage this account"}`)
	err := org.client().RevokeToken(context.Background(), "key", "acc-1", "tok-1")
	if atlassian.Status(err) != http.StatusForbidden {
		t.Fatalf("err = %v, want the 403 kept", err)
	}
}

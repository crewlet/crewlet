package engine

import (
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/github"
	"github.com/crewlet/crewlet/internal/notify"
)

const appSeatsDoc = `
name: Acme
providers:
  sandbox: {fake: true}
  llm:
    primary: {model: claude-sonnet-5}
roles:
  - name: SRE Lead
    handle: sre-lead
    llm: primary
    integrations:
      github:
        enabled: false
        app_id: 41
        app_slug: Acme-SRE-Lead
        installation_id: 7
        private_key: pem
  - name: Reviewer
    handle: reviewer
    llm: primary
    integrations:
      github:
        enabled: false
  - name: Writer
    handle: writer
    llm: primary
`

// AN AGENT'S OWN APP IS ITS IDENTITY, AND IT COSTS NO REQUEST: its account
// is the slug the engine recorded, so a seat acting as its app is registered
// without a `GET /user`, or its deliveries reach no seat.
func TestASeatsOwnAppIsRegisteredUnderBothItsNames(t *testing.T) {
	t.Parallel()
	c := companyWith(t, appSeatsDoc)
	reg := notify.NewRegistry(c.Org, nil)

	ids := &githubIdentities{}
	if got := ids.register(reg, c, config.NewResolver(nil)); got != 1 {
		t.Fatalf("registered %d seats, want the one with an app", got)
	}

	// THE MENTION, what a person types. FOLDED: the slug is `Acme-SRE-Lead`
	// and [github.Mentions] lowercases a mention.
	party, ok := reg.ByExternalID(github.Backend, "acme-sre-lead")
	if !ok || party.Handle != "sre-lead" {
		t.Fatalf("a mention of the app resolved to %v (found=%v)", party.Handle, ok)
	}
	// AND THE ACCOUNT, which is how every payload names the same agent.
	party, ok = reg.ByExternalID(github.Backend, "acme-sre-lead[bot]")
	if !ok || party.Handle != "sre-lead" {
		t.Fatalf("the app's own account resolved to %v (found=%v)", party.Handle, ok)
	}

	// A SEAT WHOSE APP DOES NOT EXIST YET IS NOT REGISTERED: a bare "[bot]"
	// would map it and every seat like it onto one id.
	if _, ok := reg.ByExternalID(github.Backend, "[bot]"); ok {
		t.Error("a seat with no app was registered under a bare bot suffix")
	}
	if id := reg.ExternalID(github.Backend, "reviewer"); id != "" {
		t.Errorf("a seat whose app is not created yet is registered as %q", id)
	}
}

// A TOKEN THIS ENGINE RESOLVED IS FOLDED TOO: `GET /user` answers in the
// account's canonical casing (`SreLead`), and unfolded, a mention typed
// `@srelead` would never wake the seat.
func TestATokenResolvedAccountIsRegisteredFolded(t *testing.T) {
	t.Parallel()
	const doc = `
name: Acme
providers:
  sandbox: {fake: true}
  llm:
    primary: {model: claude-sonnet-5}
roles:
  - name: SRE Lead
    handle: sre-lead
    llm: primary
    mcp_env:
      github:
        GITHUB_TOKEN: ghp-seat
`
	c := companyWith(t, doc)
	reg := notify.NewRegistry(c.Org, nil)
	ids := &githubIdentities{identityCache[string]{known: map[string]string{"ghp-seat": "SreLead"}}}

	if got := ids.register(reg, c, config.NewResolver(nil)); got != 1 {
		t.Fatalf("registered %d seats", got)
	}
	if party, ok := reg.ByExternalID(github.Backend, "srelead"); !ok || party.Handle != "sre-lead" {
		t.Fatalf("a mention of the seat's account resolved to %v (found=%v)", party.Handle, ok)
	}
}

// AN APP OUTRANKS A LEFTOVER TOKEN, because the app is what the agent acts
// as: otherwise a stale credential in mcp_env overwrites the mapping and the
// app's own deliveries reach a stranger.
func TestAnAppOutranksALeftoverToken(t *testing.T) {
	t.Parallel()
	const doc = `
name: Acme
providers:
  sandbox: {fake: true}
  llm:
    primary: {model: claude-sonnet-5}
roles:
  - name: SRE Lead
    handle: sre-lead
    llm: primary
    mcp_env:
      github:
        GITHUB_TOKEN: ghp-old
    integrations:
      github:
        enabled: false
        app_id: 41
        app_slug: acme-sre-lead
        installation_id: 7
        private_key: pem
`
	c := companyWith(t, doc)
	reg := notify.NewRegistry(c.Org, nil)
	ids := &githubIdentities{identityCache[string]{known: map[string]string{"ghp-old": "somebody-else"}}}
	ids.register(reg, c, config.NewResolver(nil))

	if id := reg.ExternalID(github.Backend, "sre-lead"); id != "acme-sre-lead" {
		t.Fatalf("the seat is known as %q, want its own app", id)
	}
	if _, ok := reg.ByExternalID(github.Backend, "somebody-else"); ok {
		t.Error("a leftover token's account was registered over the seat's own app")
	}
}

// SEATS WITHOUT AN APP STILL USE THEIR TOKEN: a company mid-migration runs
// both shapes, and dropping the token path unroutes every seat without one.
func TestASeatWithNoAppKeepsItsToken(t *testing.T) {
	t.Parallel()
	const doc = `
name: Acme
providers:
  sandbox: {fake: true}
  llm:
    primary: {model: claude-sonnet-5}
roles:
  - name: SRE Lead
    handle: sre-lead
    llm: primary
    integrations:
      github:
        enabled: false
        app_id: 41
        app_slug: acme-sre-lead
        installation_id: 7
        private_key: pem
  - name: Reviewer
    handle: reviewer
    llm: primary
    mcp_env:
      github:
        GITHUB_TOKEN: ghp-reviewer
`
	c := companyWith(t, doc)
	reg := notify.NewRegistry(c.Org, nil)
	ids := &githubIdentities{identityCache[string]{known: map[string]string{"ghp-reviewer": "rev-account"}}}

	if got := ids.register(reg, c, config.NewResolver(nil)); got != 2 {
		t.Fatalf("registered %d seats, want both shapes", got)
	}
	if party, ok := reg.ByExternalID(github.Backend, "rev-account"); !ok || party.Handle != "reviewer" {
		t.Fatalf("the token seat resolved to %v (found=%v)", party.Handle, ok)
	}
}

// A SEAT WHOSE GITHUB CREDENTIAL IS ITS OWN APP'S TOKEN IS NEVER LOOKED UP.
// `Bearer ${GITHUB_APP_TOKEN_<HANDLE>}` is unissued before the app exists
// (401 on every wiring) and refused GET /user after. The identity is the
// app's slug: the seat with an app is registered under it, and the seat
// without one is named as waiting for it.
func TestASeatActingAsItsOwnAppIsNeverLookedUpWithGetUser(t *testing.T) {
	t.Parallel()
	const doc = `
name: Acme
providers:
  llm:
    primary: {model: claude-sonnet-5}
  sandbox: {fake: true}
integrations:
  github:
    enabled: true
    webhook_secret: "${GH_SECRET}"
roles:
  - name: SRE Lead
    handle: sre-lead
    llm: primary
    mcp_env:
      github:
        Authorization: "Bearer ${GITHUB_APP_TOKEN_SRE_LEAD}"
    integrations:
      github:
        enabled: false
        app_id: 41
        app_slug: acme-sre-lead
        installation_id: 7
        private_key: pem
  - name: Backend Engineer
    handle: backend-engineer
    llm: primary
    mcp_env:
      github:
        Authorization: "Bearer ${GITHUB_APP_TOKEN_BACKEND_ENGINEER}"
    integrations:
      github:
        enabled: true
        permissions: {contents: write}
`
	var lookups atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		lookups.Add(1)
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"message":"Bad credentials"}`))
	}))
	t.Cleanup(server.Close)

	c := companyWith(t, doc)
	env := config.NewResolver(nil)
	ids := &githubIdentities{}
	ids.resolve(t.Context(), server.URL, server.URL, github.SeatCredentials(c.Org, env.Value))
	if n := lookups.Load(); n != 0 {
		t.Fatalf("%d requests were made to look up seats whose credential is their own app's", n)
	}

	reg := notify.NewRegistry(c.Org, nil)
	if got := ids.register(reg, c, env); got != 1 {
		t.Fatalf("registered %d seats, want the one whose app exists", got)
	}
	if party, ok := reg.ByExternalID(github.Backend, "acme-sre-lead[bot]"); !ok || party.Handle != "sre-lead" {
		t.Errorf("the app's account resolved to %v (found=%v)", party.Handle, ok)
	}
	if got := githubAppsPending(c); len(got) != 1 || got[0] != "backend-engineer" {
		t.Errorf("seats waiting for their app = %v, want backend-engineer", got)
	}
	if got := ids.unresolved(c, env); len(got) != 0 {
		t.Errorf("seats reported unresolved = %v, want none: each acts as its own app", got)
	}
}

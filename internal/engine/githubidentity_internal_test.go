package engine

import (
	"testing"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/github"
	"github.com/crewlet/crewlet/internal/notify"
)

const appSeatsDoc = `
name: Acme
providers:
  llm:
    primary: {type: anthropic, model: claude-sonnet-5, api_keys: ["k"]}
roles:
  - name: SRE Lead
    handle: sre-lead
    llm: primary
    integrations:
      github:
        app_id: 41
        app_slug: Acme-SRE-Lead
        installation_id: 7
        private_key: pem
  - name: Reviewer
    handle: reviewer
    llm: primary
    integrations:
      github:
        tier: review
  - name: Writer
    handle: writer
    llm: primary
`

// AN AGENT'S OWN APP IS ITS IDENTITY, AND IT COSTS NO REQUEST.
//
// The old shape learned a seat's account by spending a `GET /user` per
// credential, because a personal access token says nothing about whose it is.
// An app's account is its slug, which the engine wrote down when it created
// the app, so a seat that acts as its own app was registrable all along and
// registered as nothing: a live company had an app created, installed, its
// deliveries verified and stored, and not one of them reaching a seat.
func TestASeatsOwnAppIsRegisteredUnderBothItsNames(t *testing.T) {
	t.Parallel()
	c := companyWith(t, appSeatsDoc)
	reg := notify.NewRegistry(c.Org)

	ids := &githubIdentities{}
	if got := ids.register(reg, c, config.NewResolver(nil)); got != 1 {
		t.Fatalf("registered %d seats, want the one with an app", got)
	}

	// THE MENTION, which is what a person types and what wakes the agent.
	// FOLDED: the slug is written as `Acme-SRE-Lead` and a mention arrives
	// from [github.Mentions] already lowercased.
	party, ok := reg.ByExternalID(github.Backend, "acme-sre-lead")
	if !ok || party.Handle != "sre-lead" {
		t.Fatalf("a mention of the app resolved to %v (found=%v)", party.Handle, ok)
	}
	// AND THE ACCOUNT, which is how every payload names the same agent.
	party, ok = reg.ByExternalID(github.Backend, "acme-sre-lead[bot]")
	if !ok || party.Handle != "sre-lead" {
		t.Fatalf("the app's own account resolved to %v (found=%v)", party.Handle, ok)
	}

	// A SEAT WHOSE APP DOES NOT EXIST YET IS NOT REGISTERED. It has a click
	// outstanding and acts as nobody until somebody makes it one; a bare
	// "[bot]" would map it and every seat like it onto one id.
	if _, ok := reg.ByExternalID(github.Backend, "[bot]"); ok {
		t.Error("a seat with no app was registered under a bare bot suffix")
	}
	if id := reg.ExternalID(github.Backend, "reviewer"); id != "" {
		t.Errorf("a seat whose app is not created yet is registered as %q", id)
	}
}

// A TOKEN THIS ENGINE RESOLVED IS FOLDED TOO.
//
// `GET /user` answers with the account's canonical casing, and a mention
// carries what a person typed. Registered unfolded, a seat whose account is
// `SreLead` was woken when a payload named it as an author and never when
// somebody wrote `@srelead`, which is the spelling most people type.
func TestATokenResolvedAccountIsRegisteredFolded(t *testing.T) {
	t.Parallel()
	const doc = `
name: Acme
providers:
  llm:
    primary: {type: anthropic, model: claude-sonnet-5, api_keys: ["k"]}
roles:
  - name: SRE Lead
    handle: sre-lead
    llm: primary
    mcp_env:
      github:
        GITHUB_TOKEN: ghp-seat
`
	c := companyWith(t, doc)
	reg := notify.NewRegistry(c.Org)
	ids := &githubIdentities{byToken: map[string]string{"ghp-seat": "SreLead"}}

	if got := ids.register(reg, c, config.NewResolver(nil)); got != 1 {
		t.Fatalf("registered %d seats", got)
	}
	if party, ok := reg.ByExternalID(github.Backend, "srelead"); !ok || party.Handle != "sre-lead" {
		t.Fatalf("a mention of the seat's account resolved to %v (found=%v)", party.Handle, ok)
	}
}

// AN APP OUTRANKS A LEFTOVER TOKEN, because the app is what the agent acts
// as. Registered the other way round, a credential nobody had cleaned out of
// mcp_env would overwrite the mapping with whatever account it authenticates
// as, and the app's own deliveries would reach a stranger.
func TestAnAppOutranksALeftoverToken(t *testing.T) {
	t.Parallel()
	const doc = `
name: Acme
providers:
  llm:
    primary: {type: anthropic, model: claude-sonnet-5, api_keys: ["k"]}
roles:
  - name: SRE Lead
    handle: sre-lead
    llm: primary
    mcp_env:
      github:
        GITHUB_TOKEN: ghp-old
    integrations:
      github:
        app_id: 41
        app_slug: acme-sre-lead
        installation_id: 7
        private_key: pem
`
	c := companyWith(t, doc)
	reg := notify.NewRegistry(c.Org)
	ids := &githubIdentities{byToken: map[string]string{"ghp-old": "somebody-else"}}
	ids.register(reg, c, config.NewResolver(nil))

	if id := reg.ExternalID(github.Backend, "sre-lead"); id != "acme-sre-lead" {
		t.Fatalf("the seat is known as %q, want its own app", id)
	}
	if _, ok := reg.ByExternalID(github.Backend, "somebody-else"); ok {
		t.Error("a leftover token's account was registered over the seat's own app")
	}
}

// SEATS WITHOUT AN APP STILL USE THEIR TOKEN, because a company mid-migration
// runs both shapes at once and dropping the token path would unroute every
// seat that has not been given an app yet.
func TestASeatWithNoAppKeepsItsToken(t *testing.T) {
	t.Parallel()
	const doc = `
name: Acme
providers:
  llm:
    primary: {type: anthropic, model: claude-sonnet-5, api_keys: ["k"]}
roles:
  - name: SRE Lead
    handle: sre-lead
    llm: primary
    integrations:
      github:
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
	reg := notify.NewRegistry(c.Org)
	ids := &githubIdentities{byToken: map[string]string{"ghp-reviewer": "rev-account"}}

	if got := ids.register(reg, c, config.NewResolver(nil)); got != 2 {
		t.Fatalf("registered %d seats, want both shapes", got)
	}
	if party, ok := reg.ByExternalID(github.Backend, "rev-account"); !ok || party.Handle != "reviewer" {
		t.Fatalf("the token seat resolved to %v (found=%v)", party.Handle, ok)
	}
}

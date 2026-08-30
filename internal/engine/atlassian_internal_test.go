package engine

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/crewlet/crewlet/internal/atlassian"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/confluence"
	"github.com/crewlet/crewlet/internal/jira"
	"github.com/crewlet/crewlet/internal/notify"
	"github.com/crewlet/crewlet/internal/org"
)

// Seat identity, for BOTH Atlassian products.
//
// # The bug these cover
//
// `confluence.Backend` was registered nowhere in this package, so the wiki's
// party namespace was permanently EMPTY for agent seats. Nothing failed: the
// deliveries arrived, the parser ran, and a page mentioning an agent resolved
// to nobody — so the subscription ledger was never written, an agent was
// never suppressed as the actor of its own edit, and every page event fell
// through to the space lead. The parser was correct the whole time; it was
// asking a registry nothing had ever written to.

// atlassianStub answers both products' identity endpoints, and records which
// were asked.
type atlassianStub struct {
	srv *httptest.Server

	mu sync.Mutex
	// asked is every path this stub served, in order.
	asked []string
	// jiraID and wikiID are what each product answers. They differ here on
	// purpose: on Data Center Jira answers `name` and Confluence answers
	// `userKey`, and registering one under the other's namespace is the
	// misroute the per-product walk exists to prevent.
	jiraID, wikiID string
	// refuse makes every identity call fail, for the degradation case.
	refuse bool
}

func newAtlassianStub(t *testing.T, jiraID, wikiID string) *atlassianStub {
	t.Helper()
	stub := &atlassianStub{jiraID: jiraID, wikiID: wikiID}
	stub.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		stub.mu.Lock()
		stub.asked = append(stub.asked, r.URL.Path)
		refuse := stub.refuse
		stub.mu.Unlock()
		if refuse {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/myself"):
			_ = json.NewEncoder(w).Encode(map[string]string{"accountId": stub.jiraID})
		case strings.HasSuffix(r.URL.Path, "/user/current"):
			_ = json.NewEncoder(w).Encode(map[string]string{"accountId": stub.wikiID})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(stub.srv.Close)
	return stub
}

func (s *atlassianStub) count(substr string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	var n int
	for _, path := range s.asked {
		if strings.Contains(path, substr) {
			n++
		}
	}
	return n
}

// atlassianCompany is one agent seat holding one Atlassian credential.
func atlassianCompany(t *testing.T) *Company {
	t.Helper()
	o := &org.Organization{Name: "Acme", Roles: []*org.Role{{
		Name: "Agent SWE",
		MCPEnv: org.MCPEnv{"atlassian": {
			"JIRA_API_TOKEN": "${TOKEN_SWE}",
			"JIRA_USERNAME":  "${EMAIL_SWE}",
		}},
	}}}
	o.Normalize()
	return &Company{Org: o, Config: &config.Company{Name: "Acme"}}
}

func atlassianEnv() *config.Resolver {
	return config.WithStore(config.MapSource(map[string]string{
		"TOKEN_SWE": "seat-token",
		"EMAIL_SWE": "swe@example.com",
	}))
}

func TestOneCredentialIsRegisteredUnderBothProductNamespaces(t *testing.T) {
	t.Parallel()
	// On Cloud both products answer the same account id, and BOTH
	// namespaces have to carry it: a Jira webhook and a Confluence page
	// event name the same agent by the same id on two different sources,
	// and a registry that held only one leaves the other inert.
	stub := newAtlassianStub(t, "5f0000000000000000000001", "5f0000000000000000000001")
	company, env := atlassianCompany(t), atlassianEnv()

	var ids atlassianIdentities
	where := productSite{base: stub.srv.URL, deploy: jira.Cloud}
	creds := atlassianSeatCredentials(company, env)
	ids.resolve(context.Background(), atlassian.ProductJira, where, creds)
	ids.resolve(context.Background(), atlassian.ProductConfluence, where, creds)

	reg := notify.NewRegistry(company.Org)
	registered := ids.register(reg, company, env)
	if registered[atlassian.ProductJira] != 1 || registered[atlassian.ProductConfluence] != 1 {
		t.Fatalf("registered = %v, want one seat per product", registered)
	}
	for _, backend := range []string{jira.Backend, confluence.Backend} {
		party, ok := reg.ByExternalID(backend, "5f0000000000000000000001")
		if !ok || party.Handle != "agent-swe" {
			t.Errorf("%s namespace resolved to %+v (ok=%v)", backend, party, ok)
		}
	}
}

func TestEachProductRegistersItsOwnAnswer(t *testing.T) {
	t.Parallel()
	// On Data Center the two endpoints answer DIFFERENT things — Jira's
	// `name` against Confluence's `userKey` — so one walk for both would
	// register one product's answer under the other's namespace, and every
	// event on that surface would name a stranger.
	stub := newAtlassianStub(t, "jira-name", "wiki-userkey")
	company, env := atlassianCompany(t), atlassianEnv()

	var ids atlassianIdentities
	where := productSite{base: stub.srv.URL, deploy: jira.DataCenter}
	creds := atlassianSeatCredentials(company, env)
	ids.resolve(context.Background(), atlassian.ProductJira, where, creds)
	ids.resolve(context.Background(), atlassian.ProductConfluence, where, creds)

	reg := notify.NewRegistry(company.Org)
	ids.register(reg, company, env)
	if party, ok := reg.ByExternalID(jira.Backend, "jira-name"); !ok || party.Handle != "agent-swe" {
		t.Errorf("the tracker namespace resolved to %+v (ok=%v)", party, ok)
	}
	if party, ok := reg.ByExternalID(confluence.Backend, "wiki-userkey"); !ok || party.Handle != "agent-swe" {
		t.Errorf("the wiki namespace resolved to %+v (ok=%v)", party, ok)
	}
	// AND THEY DO NOT LEAK INTO EACH OTHER.
	if _, ok := reg.ByExternalID(confluence.Backend, "jira-name"); ok {
		t.Error("the tracker's answer was registered in the wiki's namespace")
	}
	if _, ok := reg.ByExternalID(jira.Backend, "wiki-userkey"); ok {
		t.Error("the wiki's answer was registered in the tracker's namespace")
	}
}

func TestOnlyTheProductsThatResolvedAreRegistered(t *testing.T) {
	t.Parallel()
	// A company that runs Jira and no Confluence must not have its Jira
	// account ids answering wiki events — against a seat whose credential
	// was never checked there.
	stub := newAtlassianStub(t, "5f0000000000000000000001", "5f0000000000000000000001")
	company, env := atlassianCompany(t), atlassianEnv()

	var ids atlassianIdentities
	ids.resolve(context.Background(), atlassian.ProductJira,
		productSite{base: stub.srv.URL, deploy: jira.Cloud}, atlassianSeatCredentials(company, env))

	reg := notify.NewRegistry(company.Org)
	registered := ids.register(reg, company, env)
	if _, asked := registered[atlassian.ProductConfluence]; asked {
		t.Fatalf("registered = %v, want only the product that was resolved", registered)
	}
	if _, ok := reg.ByExternalID(confluence.Backend, "5f0000000000000000000001"); ok {
		t.Error("an unconfigured product's namespace was populated")
	}
}

func TestAResolvedIdentityIsNotAskedForTwice(t *testing.T) {
	t.Parallel()
	// Identity is a function of the CREDENTIAL, and credentials change
	// rarely, so a config apply that touched something else must not spend
	// one request per seat to re-learn what it already knows.
	stub := newAtlassianStub(t, "5f0000000000000000000001", "5f0000000000000000000001")
	company, env := atlassianCompany(t), atlassianEnv()

	var ids atlassianIdentities
	where := productSite{base: stub.srv.URL, deploy: jira.Cloud}
	creds := atlassianSeatCredentials(company, env)
	ids.resolve(context.Background(), atlassian.ProductJira, where, creds)
	first := stub.count("/myself")
	ids.resolve(context.Background(), atlassian.ProductJira, where, creds)
	if got := stub.count("/myself"); got != first {
		t.Fatalf("a second resolve spent %d more request(s)", got-first)
	}
}

func TestAnUnresolvedCredentialLeavesTheSeatUnregisteredRatherThanFailing(t *testing.T) {
	t.Parallel()
	// The instance may be briefly down, and the next apply retries. What
	// that costs is those seats' inbound routing until then, which is the
	// honest consequence — refusing to boot over it would be worse.
	stub := newAtlassianStub(t, "x", "y")
	stub.refuse = true
	company, env := atlassianCompany(t), atlassianEnv()

	var ids atlassianIdentities
	ids.resolve(context.Background(), atlassian.ProductJira,
		productSite{base: stub.srv.URL, deploy: jira.Cloud}, atlassianSeatCredentials(company, env))

	reg := notify.NewRegistry(company.Org)
	if got := ids.register(reg, company, env)[atlassian.ProductJira]; got != 0 {
		t.Fatalf("registered %d seat(s) from a refused lookup", got)
	}
}

func TestOnlyDistinctCredentialsAreResolved(t *testing.T) {
	t.Parallel()
	// Several seats may legitimately share one credential — a company
	// mid-migration, or one that has not provisioned per-seat accounts yet
	// — and resolving it once per seat would spend N requests to learn one
	// answer.
	shared := org.MCPEnv{"atlassian": {
		"JIRA_API_TOKEN": "${TOKEN_SWE}", "JIRA_USERNAME": "${EMAIL_SWE}",
	}}
	o := &org.Organization{Name: "Acme", Roles: []*org.Role{
		{Name: "Agent One", MCPEnv: shared},
		{Name: "Agent Two", MCPEnv: shared},
		{Name: "Agent Chat"},
	}}
	o.Normalize()
	company := &Company{Org: o, Config: &config.Company{Name: "Acme"}}

	creds := atlassianSeatCredentials(company, atlassianEnv())
	if len(creds) != 1 {
		t.Fatalf("%d credential(s), want one: %+v", len(creds), creds)
	}
	// AND IT CARRIES BOTH SEATS. A failed lookup logs these handles, and
	// without them an operator grepping for the seat that went quiet finds
	// nothing — which is the exact diagnosis the warning exists to give.
	if !slices.Equal(creds[0].handles, []string{"agent-one", "agent-two"}) {
		t.Fatalf("handles = %v, want every seat that shares the credential",
			creds[0].handles)
	}
}

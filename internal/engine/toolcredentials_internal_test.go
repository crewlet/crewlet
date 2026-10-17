package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/coord"
	coordmem "github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/integration"
	"github.com/crewlet/crewlet/internal/logging"
	"github.com/crewlet/crewlet/internal/mcp"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/secrets"
)

// The rule a tool server is started by when a value it is started with does
// not resolve (toolcredentials.go): what tells a credential on its way from
// one nobody will supply, what tells a wait from a wait that never ends, what
// a key declared optional leaves out, and what starts the server once the
// credential is there.

// mixedDoc is a company that provisions its agents' Atlassian accounts, whose
// per-role servers point at binaries that do not exist, and whose seats' blocks
// mix what the pass mints with what nothing does: the CEO's site address is a
// variable nobody sets, and the CTO names one token under two keys. Its
// servers' own env passes a host setting through by name that no host here
// sets, the guide's own example of a reference that may resolve to nothing,
// and declares it optional as the guide does.
const mixedDoc = `
name: Acme
providers:
  sandbox: {fake: true}
  llm:
    zulu:
      model: claude-sonnet-5
integrations:
  atlassian:
    org_id: "${ATLASSIAN_ORG_ID}"
    api_key: "${ATLASSIAN_ORG_KEY}"
mcp_servers:
  - name: atlassian
    shared: false
    command: "/nonexistent/definitely-not-an-atlassian-server"
    env:
      NPM_CONFIG_REGISTRY: "${HOST_SETTING_NOBODY_SETS}"
    optional_keys: [NPM_CONFIG_REGISTRY]
  - name: notes
    shared: true
    command: "/nonexistent/definitely-not-a-notes-server"
    env:
      NPM_CONFIG_REGISTRY: "${HOST_SETTING_NOBODY_SETS}"
    optional_keys: [NPM_CONFIG_REGISTRY]
  - name: tracker
    shared: false
    command: "/nonexistent/definitely-not-a-tracker-server"
roles:
  - name: CEO
    handle: ceo
    llm: zulu
    mcp_env:
      atlassian:
        JIRA_USERNAME: "${CEO_ATLASSIAN_EMAIL}"
        JIRA_API_TOKEN: "${CEO_ATLASSIAN_TOKEN}"
        JIRA_URL: "${CEO_JIRA_SITE}"
  - name: CTO
    handle: cto
    llm: zulu
    mcp_env:
      atlassian:
        JIRA_USERNAME: "${CTO_ATLASSIAN_EMAIL}"
        JIRA_API_TOKEN: "${CTO_ATLASSIAN_TOKEN}"
      tracker:
        SEAT_TOKEN: "${CTO_TRACKER_TOKEN}"
        OTHER_TOKEN: "${CTO_TRACKER_TOKEN}"
`

// configured is the company's entry for server.
func configured(t *testing.T, c *Company, server string) *config.MCPServer {
	t.Helper()
	for i := range c.Config.MCPServers {
		if c.Config.MCPServers[i].Name == server {
			return &c.Config.MCPServers[i]
		}
	}
	t.Fatalf("the company declares no server %q", server)
	return nil
}

// missingFor is what of the values one seat's child of a server is started
// with resolves to nothing under env, judged as a pass judges it before
// reading any integration.
func missingFor(t *testing.T, c *Company, handle, server string, env *config.Resolver) *credentialMissing {
	t.Helper()
	seat := c.Org.AgentSeatByHandle(handle)
	_, missing := resolveServer(configured(t, c, server), handle,
		mcp.InstanceName(server, seat.Name), seat.MCPEnv[server], env, &seatMint{kinds: (&Engine{}).mintedFor(c, handle)})
	return missing
}

// A BLOCK ONE OF WHOSE VARIABLES A PASS MINTS AND ANOTHER NOTHING DOES IS A
// FAILURE, and its sentence promises no start.
//
// The pass mints the CEO's Atlassian address and token, and nothing mints the
// site address beside them: waiting for the pass would wait for ever, since
// the pass never mints the other, behind a screen that says nothing is wrong.
func TestABlockMixingAMintedAndAnUnsetVariableIsAFailureNotAWait(t *testing.T) {
	t.Parallel()
	c := specCompany(t, mixedDoc)
	missing := missingFor(t, c, "ceo", "atlassian", config.NewResolver(config.MapSource{}))
	if missing == nil {
		t.Fatal("nothing reported missing")
	}
	if missing.waiting() {
		t.Errorf("a block with ${CEO_JIRA_SITE} unset reads as waiting: %s", missing.Error())
	}
	sentence := missing.Error()
	if strings.Contains(sentence, "starts once") {
		t.Errorf("a failure's sentence promises a start: %s", sentence)
	}
	for _, said := range []string{"${CEO_JIRA_SITE} is not set", "nothing this engine runs mints it",
		"waiting for the atlassian integration to mint ${CEO_ATLASSIAN_EMAIL} and ${CEO_ATLASSIAN_TOKEN}"} {
		if !strings.Contains(sentence, said) {
			t.Errorf("the sentence %q does not say %q", sentence, said)
		}
	}
}

// A VARIABLE TWO KEYS POINT AT IS NAMED ONCE, because a sentence telling an
// operator to set it twice reads as two different things to set.
func TestOneVariableTwoKeysPointAtIsNamedOnce(t *testing.T) {
	t.Parallel()
	c := specCompany(t, mixedDoc)
	missing := missingFor(t, c, "cto", "tracker", config.NewResolver(config.MapSource{}))
	if missing == nil {
		t.Fatal("nothing reported missing")
	}
	if n := strings.Count(missing.Error(), "${CTO_TRACKER_TOKEN}"); n != 1 {
		t.Errorf("the sentence names ${CTO_TRACKER_TOKEN} %d times: %s", n, missing.Error())
	}
}

// A VARIABLE SET TO NOTHING IS TOLD FROM ONE SET NOWHERE, because the remedies
// differ: "export it" sends an operator to do what they already did.
func TestAVariableSetToNothingIsToldFromOneSetNowhere(t *testing.T) {
	t.Parallel()
	c := specCompany(t, mixedDoc)
	empty := missingFor(t, c, "cto", "tracker",
		config.NewResolver(config.MapSource{"CTO_TRACKER_TOKEN": ""}))
	unset := missingFor(t, c, "cto", "tracker", config.NewResolver(config.MapSource{}))
	if empty == nil || unset == nil {
		t.Fatalf("missing = %v, %v: want both reported", empty, unset)
	}
	if got := empty.Error(); !strings.Contains(got, "is set to an empty value") ||
		strings.Contains(got, "is not set") {
		t.Errorf("a variable exported empty reads %q, want it called empty and not unset", got)
	}
	if got := unset.Error(); !strings.Contains(got, "is not set") ||
		strings.Contains(got, "empty value") {
		t.Errorf("a variable set nowhere reads %q, want it called unset and not empty", got)
	}
	if empty.waiting() || unset.waiting() {
		t.Error("a variable nothing mints reads as waiting")
	}
}

// A KEY DECLARED OPTIONAL WHOSE VALUE DOES NOT RESOLVE IS LEFT OUT, WHOLE, and
// holds nothing back — a seat's child of a per-role template, or a shared
// server.
//
// The guide passes NPM_CONFIG_REGISTRY through by name, unset on most hosts,
// and declares it optional. Left out is what Kubernetes does with an optional
// secret reference: the child starts without the variable. Passed on empty, it
// drew an `empty_env_vars` warning on every start; and a value built around a
// reference ("Bearer ${TOKEN}") would arrive as half of itself, a credential
// that looks set.
func TestADeclaredOptionalKeyThatDoesNotResolveIsLeftOutWhole(t *testing.T) {
	t.Parallel()
	c := specCompany(t, mixedDoc)
	env := config.NewResolver(config.MapSource{
		"CEO_ATLASSIAN_EMAIL": "agent-ceo@example.com", "CEO_ATLASSIAN_TOKEN": "ceo-minted",
		"CEO_JIRA_SITE": "https://acme.example.com",
	})
	specs, outcomes := seatSpecs(c, c.Org.AgentSeatByHandle("ceo"), env, nil)
	if len(specs) != 1 || len(outcomes) != 0 {
		t.Fatalf("per-role: %d spec(s), outcomes %+v: an optional host setting in the "+
			"template held the seat's child back", len(specs), outcomes)
	}
	if got, present := specs[0].Env["NPM_CONFIG_REGISTRY"]; present {
		t.Errorf("the optional host setting reached the child as %q, want it left out", got)
	}
	if specs[0].Env["JIRA_API_TOKEN"] != "ceo-minted" {
		t.Errorf("the seat's own credential did not reach the child: %v", specs[0].Env)
	}

	notes, missing := resolveServer(configured(t, c, "notes"), "", "notes", nil, env, nil)
	if missing != nil {
		t.Fatalf("shared: an optional host setting held the server back: %s", missing.Error())
	}
	if _, present := notes.values["NPM_CONFIG_REGISTRY"]; present {
		t.Errorf("shared: the optional host setting was passed on: %v", notes.values)
	}

	// WHOLE: one reference of two missing leaves no half of the value.
	server := &config.MCPServer{Name: "docs", Transport: config.TransportHTTP,
		URL: "https://docs.example.com/mcp", OptionalKeys: []string{"X-Trace"},
		Headers: map[string]string{"X-Trace": "${TRACE_PREFIX}-${TRACE_ID}"}}
	values, missing := resolveServer(server, "", "docs",
		nil, config.NewResolver(config.MapSource{"TRACE_PREFIX": "acme"}), nil)
	if missing != nil {
		t.Fatalf("an optional header held the server back: %s", missing.Error())
	}
	if got, present := values.values["X-Trace"]; present {
		t.Errorf("an optional header with one of its references unset was sent as %q", got)
	}
}

// A REFERENCE IN A SERVER'S OWN VALUES THAT IS NOT DECLARED OPTIONAL HOLDS THE
// SERVER BACK, as one in a seat's block does: the template's env, a shared
// server's key, an http server's url.
//
// They used to be passed on empty, so a shared server whose key was unset
// started, failed to authenticate and was retried by its bridge for a value
// nothing was going to supply. The sentence names the variable and both
// remedies, the declaration naming the KEY and where it goes — except for the
// url, which has no key and is never optional.
func TestAReferenceAServerDoesNotDeclareOptionalHoldsItBack(t *testing.T) {
	t.Parallel()
	c := specCompany(t, `
name: Acme
providers:
  sandbox: {fake: true}
  llm:
    zulu:
      model: claude-sonnet-5
mcp_servers:
  - name: search
    command: "/nonexistent/definitely-not-a-search-server"
    env:
      SEARCH_KEY: "${SEARCH_KEY}"
  - name: tracker
    shared: false
    command: "/nonexistent/definitely-not-a-tracker-server"
    env:
      TRACKER_SITE: "${TRACKER_SITE}"
  - name: docs
    transport: http
    url: "https://${DOCS_HOST}/mcp"
    headers:
      X-Docs-Host: "${DOCS_HOST}"
  - name: wiki
    transport: http
    url: "https://wiki.example.com/mcp"
    headers:
      Authorization: "Bearer ${WIKI_TOKEN}"
roles:
  - name: CEO
    handle: ceo
    llm: zulu
    mcp_env:
      tracker:
        SEAT_TOKEN: "${CEO_TRACKER_TOKEN}"
`)
	env := config.NewResolver(config.MapSource{"CEO_TRACKER_TOKEN": "tracker-ceo"})

	_, shared := resolveServer(configured(t, c, "search"), "", "search", nil, env, nil)
	if shared == nil {
		t.Fatal("a shared server whose key is unset was given values to start with")
	}
	for _, said := range []string{"${SEARCH_KEY} is not set", "crewlet secrets set",
		"list SEARCH_KEY under mcp_servers.search.optional_keys"} {
		if !strings.Contains(shared.Error(), said) {
			t.Errorf("the shared server's sentence %q does not say %q", shared.Error(), said)
		}
	}
	if shared.waiting() {
		t.Error("a shared server's unset key reads as waiting: no pass mints for nobody")
	}

	specs, outcomes := seatSpecs(c, c.Org.AgentSeatByHandle("ceo"), env, nil)
	var seat *credentialMissing
	if len(specs) != 0 || len(outcomes) != 1 || !errors.As(outcomes[0].err, &seat) {
		t.Fatalf("per-role: specs %+v, outcomes %+v: want the child held back for the "+
			"template's unset value, the seat's own block resolving", specs, outcomes)
	}
	if !strings.Contains(seat.Error(), "${TRACKER_SITE} is not set") {
		t.Errorf("the seat's child names %q, want the template's variable", seat.Error())
	}

	// A HEADER IS A KEY LIKE AN ENV VARIABLE: an http server's own
	// Authorization holds it back, and may be declared optional.
	_, header := resolveServer(configured(t, c, "wiki"), "", "wiki", nil, env, nil)
	if header == nil {
		t.Fatal("an http server whose own Authorization header does not resolve was given values")
	}
	for _, said := range []string{"${WIKI_TOKEN} is not set",
		"list Authorization under mcp_servers.wiki.optional_keys"} {
		if !strings.Contains(header.Error(), said) {
			t.Errorf("the http server's sentence %q does not say %q", header.Error(), said)
		}
	}

	// NOT OFFERED FOR A VARIABLE THE URL NEEDS, under the url or a header
	// beside it: declaring the header optional would leave the server held
	// back by its url, and the sentence would have sent somebody to do it.
	_, url := resolveServer(configured(t, c, "docs"), "", "docs", nil, env, nil)
	if url == nil || !strings.Contains(url.Error(), "${DOCS_HOST} is not set") {
		t.Fatalf("an http server whose url does not resolve: %v", url)
	}
	if strings.Contains(url.Error(), "optional_keys") {
		t.Errorf("a variable the url needs is offered as optional: %s", url.Error())
	}
}

// AN HTTP HEADER IS ONE HEADER WHICHEVER WAY IT IS SPELLED.
//
// HTTP does not tell `authorization` from `Authorization`. Merged by the key as
// written, a seat's `authorization` went out beside its template's
// `Authorization`, the connection set whichever came last out of a map, and
// the seat's child sent the template's identity on some of its requests; with
// the seat's key declared optional and unset, it sent the template's on all of
// them. The seat's key replaces the template's however either is spelled, and
// `optional_keys` names a header in any case too.
func TestAHeaderIsOneHeaderWhicheverWayItIsSpelled(t *testing.T) {
	t.Parallel()
	server := &config.MCPServer{Name: "code", Transport: config.TransportHTTP,
		URL:     "https://code.example.com/mcp",
		Headers: map[string]string{"Authorization": "Bearer ${SHARED_CODE_TOKEN}"}}
	block := map[string]string{"authorization": "Bearer ${CEO_CODE_TOKEN}"}

	both := config.NewResolver(config.MapSource{
		"SHARED_CODE_TOKEN": "the-template-identity", "CEO_CODE_TOKEN": "the-ceo-identity"})
	values, missing := resolveServer(server, "ceo", "code::CEO", block, both, nil)
	if missing != nil {
		t.Fatalf("held back: %s", missing.Error())
	}
	if len(values.values) != 1 || values.values["authorization"] != "Bearer the-ceo-identity" {
		t.Errorf("the seat's child is sent the headers %v: want the seat's Authorization "+
			"alone, as the seat spells it", values.values)
	}

	server.OptionalKeys = []string{"AUTHORIZATION"}
	template := config.NewResolver(config.MapSource{"SHARED_CODE_TOKEN": "the-template-identity"})
	values, missing = resolveServer(server, "ceo", "code::CEO", block, template, nil)
	if missing != nil {
		t.Fatalf("held back for a header declared optional in another case: %s", missing.Error())
	}
	if len(values.values) != 0 {
		t.Errorf("the seat's child is sent %v: its own Authorization did not resolve and "+
			"was declared optional, so none is sent, never the template's", values.values)
	}
}

// THE REMEDY OFFERS EACH VARIABLE WHAT CAN SUPPLY IT, OR LEAVE IT OUT.
//
// The integration's provision command seals a seat's bot or account — what a
// seat's own block holds — and nothing else; offered for a template's site
// address, it sent somebody to a command that cannot seal it. And the
// declaration that leaves a key out is offered for the server's own settings,
// where a host's passthrough is written, and never for a seat's block, which
// is the identity the seat acts as: started without it, the child acts as
// nobody, which is what holding it back exists to prevent.
func TestTheRemedyOffersEachVariableWhatCanSupplyItOrLeaveItOut(t *testing.T) {
	t.Parallel()
	server := &config.MCPServer{Name: "atlassian", Command: "mcp-atlassian",
		Env: map[string]string{"JIRA_URL": "${JIRA_URL}"}}
	nothing := config.NewResolver(config.MapSource{})
	block := map[string]string{"JIRA_API_TOKEN": "${CEO_ATLASSIAN_TOKEN}"}

	_, both := resolveServer(server, "ceo", "atlassian::CEO", block, nothing, nil)
	if both == nil {
		t.Fatal("nothing held back")
	}
	for _, said := range []string{
		"seal them (crewlet secrets set, or for ${CEO_ATLASSIAN_TOKEN} the integration's provision command)",
		"list JIRA_URL under mcp_servers.atlassian.optional_keys",
	} {
		if !strings.Contains(both.Error(), said) {
			t.Errorf("%q does not say %q", both.Error(), said)
		}
	}
	if strings.Contains(both.Error(), "list JIRA_API_TOKEN") {
		t.Errorf("the seat's own credential is offered as optional: %s", both.Error())
	}

	_, template := resolveServer(server, "ceo", "atlassian::CEO", nil, nothing, nil)
	if template == nil || strings.Contains(template.Error(), "provision command") {
		t.Errorf("a template's setting is offered the integration's provision command: %v", template)
	}

	_, seat := resolveServer(&config.MCPServer{Name: "atlassian", Command: "mcp-atlassian"}, "ceo", "atlassian::CEO", block, nothing, nil)
	if seat == nil || !strings.Contains(seat.Error(),
		"seal it (crewlet secrets set, or the integration's provision command)") {
		t.Errorf("a seat's own credential is not offered the provision command: %v", seat)
	}
}

// A KEY A PASS MINTS FOR THE SEAT IS WAITED FOR, WHATEVER ITS DECLARATION.
//
// `optional_keys` is for a value nothing will supply. A token the GitLab pass
// is about to mint is coming: left out, the seat's child started without its
// identity and acted as nobody until the seal — the failure the wait exists to
// prevent, reached by a declaration. A value nothing mints under the same
// declared key is still left out.
func TestAKeyAPassMintsIsWaitedForWhateverItsDeclaration(t *testing.T) {
	t.Parallel()
	c := specCompany(t, provisioningDoc(true)+`
mcp_servers:
  - name: gitlab
    shared: false
    command: "/nonexistent/definitely-not-glab"
    optional_keys: [GITLAB_TOKEN]
`)
	e := &Engine{}
	seat := c.Org.AgentSeatByHandle("swe")
	nothing := config.NewResolver(config.MapSource{})

	specs, held := seatSpecs(c, seat, nothing, &seatMint{kinds: e.mintedFor(c, "swe")})
	var missing *credentialMissing
	if len(specs) != 0 || len(held) != 1 || !errors.As(held[0].err, &missing) || !missing.waiting() {
		t.Fatalf("specs %+v, held %+v: want the child held back, waiting for the token "+
			"the pass mints, its key's declaration notwithstanding", specs, held)
	}

	specs, held = seatSpecs(c, seat, nothing, nil)
	if len(specs) != 1 || len(held) != 0 {
		t.Fatalf("specs %+v, held %+v: a declared key nothing mints holds nothing back", specs, held)
	}
	if value, present := specs[0].Env["GITLAB_TOKEN"]; present {
		t.Errorf("the declared key reached the child as %q, want it left out", value)
	}
}

// A HELD-BACK SERVER'S SENTENCE REACHES THE SCREEN WHOLE.
//
// The heartbeat bounds it ([coord.MaxMCPErrorBytes]) and Settings › Tools & MCP
// reads what survives. At 240 bytes the remedy the guide's own host-setting
// case needs — the key to declare optional — was always the part cut, and the
// row ended "…, l…". Asserted on the wire form, as a peer reads it.
func TestAHeldBackServersSentenceReachesTheScreenWhole(t *testing.T) {
	t.Parallel()
	guide := &config.MCPServer{Name: "tavily", Command: "npm",
		Args: []string{"exec", "--yes", "--", "tavily-mcp@0.2.22"},
		Env: map[string]string{
			"TAVILY_API_KEY":      "${TAVILY_API_KEY}",
			"NPM_CONFIG_REGISTRY": "${NPM_CONFIG_REGISTRY}",
		}}
	template := &config.MCPServer{Name: "atlassian", Command: "mcp-atlassian",
		Env: map[string]string{"JIRA_URL": "${JIRA_URL}"}}
	for _, tc := range []struct {
		name    string
		missing func() *credentialMissing
		says    string
	}{
		{"a host setting a shared server reads", func() *credentialMissing {
			_, m := resolveServer(guide, "", "tavily",
				nil, config.NewResolver(config.MapSource{"TAVILY_API_KEY": "tvly-example"}), nil)
			return m
		}, "list NPM_CONFIG_REGISTRY under mcp_servers.tavily.optional_keys"},
		{"a template's setting beside a seat's token being minted", func() *credentialMissing {
			_, m := resolveServer(template, "ceo", "atlassian::CEO", map[string]string{
				"JIRA_USERNAME":  "${CEO_ATLASSIAN_EMAIL}",
				"JIRA_API_TOKEN": "${CEO_ATLASSIAN_TOKEN}",
			}, config.NewResolver(config.MapSource{}), &seatMint{kinds: map[string]integration.Kind{
				"CEO_ATLASSIAN_EMAIL": integration.KindAtlassian,
				"CEO_ATLASSIAN_TOKEN": integration.KindAtlassian,
			}})
			return m
		}, "waiting for the atlassian integration to mint ${CEO_ATLASSIAN_EMAIL} and ${CEO_ATLASSIAN_TOKEN}"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			m := tc.missing()
			if m == nil || !strings.Contains(m.Error(), tc.says) {
				t.Fatalf("the sentence %v does not say %q", m, tc.says)
			}
			sent, ok := coord.StatusFromMeta(map[string]any{coord.StatusKey: coord.NodeStatus{
				MCP: []coord.MCPServerStatus{{Server: m.server, Failed: 1, Error: m.Error()}},
			}.Meta()})
			if !ok || len(sent.MCP) != 1 {
				t.Fatalf("the heartbeat's status did not read back: %+v", sent)
			}
			if got := sent.MCP[0].Error; got != m.Error() {
				t.Errorf("the heartbeat carried %q of the %d-byte sentence %q", got, len(m.Error()), m.Error())
			}
		})
	}
}

// A SEAT'S VALUE THAT RESOLVES TO NOTHING NEVER HANDS THE KEY BACK TO THE
// TEMPLATE'S.
//
// The layers are merged before anything is resolved, so a seat that writes a
// key owns it: declared optional and unset, the key is left out. Left out
// before the merge instead, the template's value would stand in — and for a
// key carrying an identity, the seat's child would act as whoever the template
// names.
func TestASeatsValueThatResolvesToNothingDoesNotHandBackTheTemplates(t *testing.T) {
	t.Parallel()
	server := &config.MCPServer{Name: "code", Transport: config.TransportHTTP,
		URL: "https://code.example.com/mcp", OptionalKeys: []string{"Authorization"},
		Headers: map[string]string{"Authorization": "Bearer ${SHARED_CODE_TOKEN}"}}
	env := config.NewResolver(config.MapSource{"SHARED_CODE_TOKEN": "the-template-identity"})
	values, missing := resolveServer(server, "ceo", "code::CEO",
		map[string]string{"Authorization": "Bearer ${CEO_CODE_TOKEN}"}, env, nil)
	if missing != nil {
		t.Fatalf("held back: %s", missing.Error())
	}
	if got, present := values.values["Authorization"]; present {
		t.Errorf("the seat's child sends Authorization %q: its own value did not resolve, "+
			"and the template's stood in", got)
	}
}

// statedAs is the fleet's record of an integration after one pass that found
// findings, as the reconcile loop writes it.
func statedAs(kind integration.Kind, findings ...integration.Finding) integration.State {
	state, _ := integration.Observe(integration.State{Kind: kind}, kind, findings, nil,
		time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC))
	return state
}

// WHETHER A PASS WILL MINT A SEAT'S CREDENTIAL IS READ OFF THE PASS'S OWN LAST
// CONCLUSION, by the vocabulary it reported in.
//
// A child held back for a credential the pass cannot mint until somebody acts
// waited for ever, reported as nothing being wrong, while the integration's own
// card said the opposite. Each case is a record a real pass writes.
func TestAMintIsStoppedOnlyWhereTheIntegrationsLastPassSaysAPersonMustAct(t *testing.T) {
	t.Parallel()
	kind := integration.KindAtlassian
	disconnecting := statedAs(kind)
	disconnecting.Disconnecting = true
	for name, tc := range map[string]struct {
		state   integration.State
		stopped bool
		says    string
	}{
		"no pass has run, which is every first boot": {state: integration.State{Kind: kind}},
		"the pass found everything fine":             {state: statedAs(kind)},
		"its own credential does not resolve": {
			state: statedAs(kind, integration.Finding{Kind: integration.FindingCredentialMissing,
				Detail: "the Atlassian organization id and its API key did not both resolve"}),
			stopped: true, says: "organization id and its API key",
		},
		"an administrator has an app to approve": {
			state: statedAs(kind, integration.Finding{Kind: integration.FindingApprovalRequired,
				Subject: "ServiceSettings.EnableBotAccountCreation", Detail: "bot creation is off"}),
			stopped: true, says: "bot creation is off",
		},
		"this seat's account was switched off by somebody else": {
			state: statedAs(kind, integration.Finding{Kind: integration.FindingIdentityFailed,
				Subject: "ceo", Detail: "ceo's bot is deactivated"}),
			stopped: true, says: "ceo's bot is deactivated",
		},
		"another seat's account failed": {
			state: statedAs(kind, integration.Finding{Kind: integration.FindingIdentityFailed,
				Subject: "cto", Detail: "cto's account could not be made"}),
		},
		"this seat's grant is landing": {
			state: statedAs(kind, integration.Finding{Kind: integration.FindingGrantPending,
				Subject: "ceo"}),
		},
		"a webhook cannot be registered, after the accounts were made": {
			state: statedAs(kind, integration.Finding{Kind: integration.FindingIngressBlocked,
				Detail: "no public base URL"}),
		},
		"a credential for something else is missing while this seat's account is being made": {
			state: statedAs(kind,
				integration.Finding{Kind: integration.FindingCredentialMissing,
					Subject: "integrations.datadog.webhook_token", Detail: "no webhook token"},
				integration.Finding{Kind: integration.FindingIdentityMissing, Subject: "ceo"}),
		},
		"one finding about this seat is the engine's and another a person's": {
			state: statedAs(kind,
				integration.Finding{Kind: integration.FindingIdentityMissing, Subject: "ceo"},
				integration.Finding{Kind: integration.FindingIdentityFailed, Subject: "ceo",
					Detail: "the account was refused"}),
			stopped: true, says: "the account was refused",
		},
		"the integration is being disconnected": {
			state: disconnecting, stopped: true, says: "being disconnected",
		},
	} {
		why, stopped := mintStopped(tc.state, "ceo")
		if stopped != tc.stopped {
			t.Errorf("%s: stopped = %t (%q), want %t", name, stopped, why, tc.stopped)
			continue
		}
		if tc.stopped && !strings.Contains(why, tc.says) {
			t.Errorf("%s: the reason %q does not say %q", name, why, tc.says)
		}
	}
}

// A CHILD WAITING FOR A PASS THAT CANNOT MINT IS REPORTED FAILED, from the beat
// after the integration says so, and waiting again once it no longer does.
//
// The claim's pass runs before the integration's first, so all it can say is
// waiting; only the heartbeat sees the integration's later report. Read once at
// the claim, a company whose Atlassian organization key nobody set showed every
// seat's server as waiting for credentials for ever, in a neutral tone, beside
// an integration card reporting the failure.
func TestAWaitingChildIsReportedFailedWhileThePassThatMintsItWaitsOnAPerson(t *testing.T) {
	t.Parallel()
	fleet := coordmem.NewFleet()
	e := &Engine{mcp: mcp.NewBridge(nil, mcp.Supervision{}), backends: &Backends{Fleet: fleet}}
	company := heldBackCompany(t)
	e.epoch.current.Store(company)
	ceo := heldSeatChildren(t, e, "ceo", func() bool { return true })
	e.convergeSeat(t.Context(), ceo, true)

	atlassianRow := func() coord.MCPServerStatus {
		t.Helper()
		for _, row := range e.mcpStatus(t.Context()) {
			if row.Server == "atlassian" {
				return row
			}
		}
		t.Fatal("no atlassian row")
		return coord.MCPServerStatus{}
	}
	if row := atlassianRow(); row.Waiting != 1 || row.Failed != 0 {
		t.Fatalf("before any pass, atlassian row = %+v: want the CEO's child waiting", row)
	}

	store, err := e.IntegrationStore()
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	save := func(state integration.State) {
		t.Helper()
		if err := store.SaveIntegration(t.Context(), state); err != nil {
			t.Fatalf("save: %v", err)
		}
	}
	save(statedAs(integration.KindAtlassian, integration.Finding{
		Kind:   integration.FindingCredentialMissing,
		Detail: "the Atlassian organization id and its API key did not both resolve",
	}))
	row := atlassianRow()
	if row.Failed != 1 || row.Waiting != 0 {
		t.Fatalf("with the pass refused its own credential, atlassian row = %+v: want "+
			"the CEO's child failed, its wait having no end", row)
	}
	for _, said := range []string{"the atlassian integration will not mint", "until a person acts",
		"organization id"} {
		if !strings.Contains(row.Error, said) {
			t.Errorf("the failure %q does not say %q", row.Error, said)
		}
	}

	save(statedAs(integration.KindAtlassian, integration.Finding{
		Kind: integration.FindingIdentityMissing, Subject: "ceo",
	}))
	if row := atlassianRow(); row.Waiting != 1 || row.Failed != 0 {
		t.Errorf("with the pass making the CEO's account, atlassian row = %+v: want "+
			"waiting again", row)
	}
}

// A SEAL THAT REFRESHES THIS NODE'S SNAPSHOT STARTS A CHILD WAITING FOR IT,
// WITH NO APPLY.
//
// The re-activation a seal asks for is a write the node may not get — here it
// has no config surface at all, as on a worker-only node — and a child held
// back arms no retry. On the node holding the value, the child stayed
// unstarted, its status saying the integration had yet to mint what the
// snapshot resolved.
func TestASealThatRefreshesThisNodeStartsTheChildItHeldBackWithNoApply(t *testing.T) {
	t.Parallel()
	cipher, err := secrets.NewCipher(secrets.Keyring{
		ActiveID: "k1",
		Keys:     map[string][]byte{"k1": []byte("crewlet-test-sealing-key-32bytes")},
	})
	if err != nil {
		t.Fatalf("keyring: %v", err)
	}
	e := &Engine{mcp: mcp.NewBridge(nil, mcp.Supervision{}),
		backends: &Backends{Fleet: coordmem.NewFleet()}, cipher: cipher}
	if e.configWriterOrNil() != nil {
		t.Fatal("precondition: this node must have no config surface, so no apply follows")
	}
	company := heldBackCompany(t)
	e.epoch.current.Store(company)
	ceo := heldSeatChildren(t, e, "ceo", func() bool { return true })
	e.convergeSeat(t.Context(), ceo, true)
	var missing *credentialMissing
	if outcomes := seatOutcomes(e, "ceo"); len(outcomes) != 1 || !errors.As(outcomes[0].err, &missing) {
		t.Fatalf("before the seal, outcomes = %+v: want the child held back", outcomes)
	}

	sink, err := e.SetupSink(testParty)
	if err != nil {
		t.Fatalf("sink: %v", err)
	}
	for name, value := range map[string]string{
		"CEO_ATLASSIAN_EMAIL": "agent-ceo@example.com", "CEO_ATLASSIAN_TOKEN": "ceo-minted",
	} {
		if err := sink.Record(t.Context(), name, value); err != nil {
			t.Fatalf("record %s: %v", name, err)
		}
	}
	if err := sink.Flush(t.Context()); err != nil {
		t.Fatalf("flush: %v", err)
	}
	pollUntil(t, 10*time.Second, "the held-back child's start to be attempted", func() bool {
		outcomes := seatOutcomes(e, "ceo")
		return len(outcomes) == 1 && outcomes[0].err != nil && !errors.As(outcomes[0].err, &missing)
	})
}

// lookupGate answers nothing, and holds the FIRST lookup made through it until
// released: a pass reading it is then mid-decision, its snapshot read.
type lookupGate struct {
	once     sync.Once
	entered  chan struct{}
	released chan struct{}
}

func (g *lookupGate) Lookup(string) (string, bool) {
	g.once.Do(func() {
		close(g.entered)
		<-g.released
	})
	return "", false
}

// THE SEAL'S APPLY LANDING WHILE THE CLAIM'S PASS IS DECIDING "WAITING" IS
// FOLDED INTO THE CLAIM.
//
// The claim's pass has read a snapshot without the credential; the apply that
// installs the sealed one asks the seat's runner for a pass while that pass is
// still deciding. A claim that dropped the ask would end with the child held
// back, no retry armed and nothing left to start it until some later apply.
func TestASealDuringTheClaimsWaitingDecisionStartsTheChild(t *testing.T) {
	t.Parallel()
	gate := &lookupGate{entered: make(chan struct{}), released: make(chan struct{})}
	e := &Engine{mcp: mcp.NewBridge(nil, mcp.Supervision{}), environ: gate}
	company := heldBackCompany(t)
	e.epoch.current.Store(company)
	ceo := heldSeatChildren(t, e, "ceo", func() bool { return true })

	// THE CLAIM, as claimSeatChildren leaves it: the caller is the runner,
	// with a pass asked for.
	ceo.mu.Lock()
	ceo.running, ceo.again = true, true
	ceo.mu.Unlock()
	done := make(chan struct{})
	go func() { defer close(done); e.runSeatPasses(t.Context(), ceo) }()

	<-gate.entered
	e.secretsMu.Lock()
	e.installSecrets(map[string]string{
		"CEO_ATLASSIAN_EMAIL": "agent-ceo@example.com", "CEO_ATLASSIAN_TOKEN": "ceo-minted",
	})
	e.secretsMu.Unlock()
	if ceo.ask(true) {
		t.Fatal("the apply became the runner while the claim held it")
	}
	close(gate.released)
	<-done

	outcomes := seatOutcomes(e, "ceo")
	var missing *credentialMissing
	if len(outcomes) != 1 || outcomes[0].err == nil || errors.As(outcomes[0].err, &missing) {
		t.Fatalf("outcomes after the claim = %+v: want the child's start attempted once the "+
			"apply that sealed its credential landed mid-pass", outcomes)
	}
}

// TWO SEATS WAITING ON ONE SERVER REPORT THE LOWER SEAT ID'S WAIT, whatever
// order their outcomes were filed in, so a beat that re-sends the same facts
// sends the same bytes.
func TestTheReportedWaitIsTheFirstBySeatID(t *testing.T) {
	t.Parallel()
	c := specCompany(t, mixedDoc)
	e := &Engine{mcp: mcp.NewBridge(nil, mcp.Supervision{})}
	e.epoch.current.Store(c)
	always := func() bool { return true }
	ceo := heldSeatChildren(t, e, "ceo", always)
	cto := heldSeatChildren(t, e, "cto", always)
	// Only what the pass mints left missing on atlassian, for both.
	e.secretsMu.Lock()
	e.installSecrets(map[string]string{"CEO_JIRA_SITE": "https://acme.example.com"})
	e.secretsMu.Unlock()
	e.convergeSeat(t.Context(), ceo, true)
	e.convergeSeat(t.Context(), cto, true)
	ceoID, _ := c.Org.AgentIDFor(c.Org.AgentSeatByHandle("ceo"))
	ctoID, _ := c.Org.AgentIDFor(c.Org.AgentSeatByHandle("cto"))
	first, firstHandle := ceoID, "CEO"
	if ctoID.String() < ceoID.String() {
		first, firstHandle = ctoID, "CTO"
	}
	for _, row := range e.mcpStatus(t.Context()) {
		if row.Server != "atlassian" {
			continue
		}
		if row.Waiting != 2 || row.WaitSeat != first ||
			!strings.Contains(row.Wait, "${"+firstHandle+"_ATLASSIAN_TOKEN}") {
			t.Errorf("atlassian row = %+v: want 2 waiting, reported as the first seat id %s's", row, first)
		}
		return
	}
	t.Fatal("no atlassian row")
}

// engineLogs is every line the process's logger writes while a test runs, as
// decoded JSON records.
type engineLogs struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (l *engineLogs) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.Write(p)
}

// records is every line written so far whose message is msg, about seat.
// About the seat because the logger is the process's: a line another case's
// seat writes on a goroutine of its own lands here too.
func (l *engineLogs) records(msg, seat string) []map[string]any {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(l.buf.String()), "\n") {
		var rec map[string]any
		if json.Unmarshal([]byte(line), &rec) == nil && rec["msg"] == msg && rec["seat"] == seat {
			out = append(out, rec)
		}
	}
	return out
}

// captureEngineLogs adds a JSON destination to the process's logger for the
// rest of the test. A PROCESS-WIDE CHANGE, so only a test that does not run in
// parallel may make it.
func captureEngineLogs(t *testing.T) *engineLogs {
	t.Helper()
	logs := &engineLogs{}
	debug := slog.LevelDebug
	logging.SetFile(logging.FileSink{Writer: logs, Format: logging.FormatJSON, Level: &debug})
	t.Cleanup(func() { logging.SetFile(logging.FileSink{}) })
	return logs
}

// WAITING IS WRITTEN AT INFO AND A CREDENTIAL NOTHING MINTS AT ERROR, and each
// line names only what applies to it.
//
// The level is the half of "this is not a failure" an operator watching the
// log sees: the claim of every first boot under provisioning used to write an
// error per seat and a retry line per attempt.
func TestWaitingIsLoggedAtInfoAndAMissingCredentialAtError(t *testing.T) {
	logs := captureEngineLogs(t)
	e := &Engine{mcp: mcp.NewBridge(nil, mcp.Supervision{})}
	e.epoch.current.Store(heldBackCompany(t))
	for _, handle := range []string{"ceo", "cto"} {
		e.convergeSeat(t.Context(), heldSeatChildren(t, e, handle, func() bool { return true }), true)
	}
	waiting := logs.records("mcp_server_awaiting_credential", "ceo")
	if len(waiting) != 1 || waiting[0]["level"] != "INFO" {
		t.Errorf("mcp_server_awaiting_credential = %v: want one line for the CEO, at INFO", waiting)
	}
	missing := logs.records("mcp_server_credential_missing", "cto")
	if len(missing) != 1 || missing[0]["level"] != "ERROR" {
		t.Fatalf("mcp_server_credential_missing = %v: want one line for the CTO, at ERROR", missing)
	}
	if others := logs.records("mcp_server_credential_missing", "ceo"); len(others) != 0 {
		t.Errorf("the CEO's wait was also written as an error: %v", others)
	}
	if _, said := missing[0]["awaiting"]; said {
		t.Errorf("a line about a variable nothing mints names what it awaits: %v", missing[0])
	}
}

// A CHILD WHOSE CREDENTIAL STOPS RESOLVING IS STOPPED, AND SAYS SO.
//
// A sealed token deleted — by a disconnect that removed the seats' accounts,
// or by hand — leaves a running child acting on the value it started with.
// Stopping it is the rule; it was logged as the seat no longer declaring the
// server, which sent the reader to a configuration that still declares it.
func TestAChildWhoseCredentialStopsResolvingIsRetiredSayingSo(t *testing.T) {
	logs := captureEngineLogs(t)
	company, _ := helperCompany(t, false, nil)
	company.Org.AgentSeatByHandle("ceo").MCPEnv["tracker"]["SEAT_TOKEN"] = "${CEO_TRACKER_TOKEN}"
	e := &Engine{mcp: mcp.NewBridge(nil, mcp.Supervision{})}
	e.epoch.current.Store(company)
	e.secretsMu.Lock()
	e.installSecrets(map[string]string{"CEO_TRACKER_TOKEN": "ceo-token"})
	e.secretsMu.Unlock()
	ceo := heldSeatChildren(t, e, "ceo", func() bool { return true })
	e.convergeSeat(t.Context(), ceo, true)
	if h := seatChild(ceo); !h.Serving {
		t.Fatalf("with its credential, the CEO's child is %+v: want it serving", h)
	}

	e.secretsMu.Lock()
	e.installSecrets(map[string]string{})
	e.secretsMu.Unlock()
	e.convergeSeat(t.Context(), ceo, true)
	if servers := ceo.bridge.Servers(); len(servers) != 0 {
		t.Errorf("the child whose credential went is still supervised: %v", servers)
	}
	retired := logs.records("mcp_server_retired", "ceo")
	if len(retired) != 1 {
		t.Fatalf("mcp_server_retired = %v: want one line", retired)
	}
	detail, _ := retired[0]["detail"].(string)
	if !strings.Contains(detail, "no longer resolves") || !strings.Contains(detail, "${CEO_TRACKER_TOKEN}") ||
		strings.Contains(detail, "no longer declares") {
		t.Errorf("the child was retired as %q: want the credential that went named, not "+
			"a server the seat still declares", detail)
	}
}

// A VENDOR'S PLAN IS READ IN THIS PACKAGE ONLY THROUGH A PASS'S seatPlan, or by
// a teardown.
//
// The plan is what a seat's child waits for ([Engine.mintedFor]), and a pass
// that minted by a plan of its own — another gate, a filter — would mint what
// no child waits for, or leave a child waiting for what it never mints, with
// every case of the plan's own still green. The Atlassian sealing clock read
// the vendor's plan directly and called a hand-made account one this engine
// provisions. A teardown reads the plan because what it removes is what may
// have been made, whether or not the company still asks for more.
func TestAVendorPlanIsReadOnlyThroughSeatPlan(t *testing.T) {
	t.Parallel()
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	fset := token.NewFileSet()
	read := 0
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		src, err := os.ReadFile(name)
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		file, err := parser.ParseFile(fset, name, src, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		read++
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			allowed := fn.Recv != nil && (fn.Name.Name == "seatPlan" || fn.Name.Name == "Teardown")
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				sel, ok := call.Fun.(*ast.SelectorExpr)
				if !ok || sel.Sel.Name != "PlanFor" {
					return true
				}
				if _, pkg := sel.X.(*ast.Ident); pkg && !allowed {
					t.Errorf("%s: %s reads a vendor's plan itself; take it from the "+
						"pass's seatPlan", fset.Position(call.Pos()), fn.Name.Name)
				}
				return true
			})
		}
	}
	if read < 50 {
		t.Fatalf("read %d source files: the walk did not find this package", read)
	}
}

// THE ATLASSIAN SEALING CLOCK COVERS ONLY THE SEATS THIS ENGINE PROVISIONS, the
// ones the pass's seat plan names.
//
// A company with no `integrations.atlassian` block makes no account, so a
// refusal minutes after a person sealed a token is the failure it looks like —
// not Atlassian still applying a grant this engine never made.
func TestTheAtlassianSealingClockCoversOnlySeatsThisEngineProvisions(t *testing.T) {
	t.Parallel()
	cipher, err := secrets.NewCipher(secrets.Keyring{
		ActiveID: "k1",
		Keys:     map[string][]byte{"k1": []byte("crewlet-test-sealing-key-32bytes")},
	})
	if err != nil {
		t.Fatalf("keyring: %v", err)
	}
	e := &Engine{backends: &Backends{Fleet: coordmem.NewFleet()}, cipher: cipher}
	if sealed := e.atlassianCredentialSealed(specCompany(t, provisioningDoc(false))); sealed != nil {
		t.Error("a company that provisions no Atlassian account was given a sealing clock, " +
			"so a refusal of a hand-made account's token reads as a grant still landing")
	}
	if sealed := e.atlassianCredentialSealed(specCompany(t, provisioningDoc(true))); sealed == nil {
		t.Error("a company that provisions its Atlassian accounts has no sealing clock")
	}
}

// A SERVER WITHHELD FROM A CODING BOX SAYS WHETHER A PASS IS MINTING ITS
// CREDENTIAL, by the rule a seat's own child is judged by.
//
// The run goes ahead without the server, so the line is all there is to say
// why — and a warning telling the operator to store a token the GitLab pass
// was about to mint into that very name sends them to do the pass's work. The
// run's own environment is judged by the same rule
// (TestARunWaitingForAMintedCredentialIsRefusedSayingWhenItCanRun).
func TestABoxServerWithheldForAMintedCredentialIsSaidAtInfo(t *testing.T) {
	logs := captureEngineLogs(t)
	// The seat's code-host server, which the box is scoped to below: a name
	// the company declares no server under is not withheld but unknown.
	c := specCompany(t, strings.Replace(provisioningDoc(true), "\nroles:\n",
		"\nmcp_servers:\n  - name: gitlab\n    shared: false\n"+
			"    command: \"/nonexistent/definitely-not-glab\"\nroles:\n", 1))
	seat := c.Org.AgentSeatByHandle("swe")
	e := &Engine{}

	e.boxServers(t.Context(), c, seat, &org.RoleSandbox{
		MCP: org.RoleSandboxMCP{Servers: []string{"gitlab"}}})
	withheld := logs.records("sandbox_mcp_server_withheld", "swe")
	if len(withheld) != 1 || withheld[0]["level"] != "INFO" ||
		!strings.Contains(withheld[0]["detail"].(string), "a coding run launched once the seal") {
		t.Errorf("sandbox_mcp_server_withheld = %v: want one line at INFO saying which run "+
			"is given the server", withheld)
	}
}

// serverRecords is records for a line about a server rather than a seat — a
// shared server's, which carries no seat at all.
func (l *engineLogs) serverRecords(msg, server string) []map[string]any {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(l.buf.String()), "\n") {
		var rec map[string]any
		if json.Unmarshal([]byte(line), &rec) == nil && rec["msg"] == msg && rec["server"] == server {
			out = append(out, rec)
		}
	}
	return out
}

// A SHARED SERVER WHOSE KEY DOES NOT RESOLVE IS NOT STARTED, IS NOT RETRIED,
// AND STARTS ON THE APPLY AFTER THE KEY ARRIVES — and one whose key stops
// resolving is stopped, saying why.
//
// It used to be handed to the shared bridge with the key empty: the vendor's
// server failed to authenticate (or started as nobody), and the bridge's
// supervision retried it on the restart schedule for a value nothing was going
// to supply, with an `empty_env_vars` warning naming the key. Nothing about it
// changes until the secret snapshot does, and every change to the snapshot is
// an apply, which reconciles the shared servers.
func TestASharedServerWhoseKeyDoesNotResolveIsNotStartedUntilItDoes(t *testing.T) {
	logs := captureEngineLogs(t)
	ctx := t.Context()
	e := &Engine{}
	e.mcp = mcp.NewBridge(nil, e.sharedSupervision())
	t.Cleanup(func() { _, _ = e.mcp.StopAll(context.Background()) })
	company := specCompany(t, `
name: Acme
providers:
  sandbox: {fake: true}
  llm:
    zulu:
      model: claude-sonnet-5
mcp_servers:
  - name: search
    command: "/nonexistent/definitely-not-a-search-server"
    env:
      SEARCH_KEY: "${SHARED_SEARCH_KEY}"
roles:
  - name: CEO
    handle: ceo
    llm: zulu
`)
	e.epoch.current.Store(company)

	e.startSharedServers(ctx, company)
	if held := e.mcp.Servers(); len(held) != 0 {
		t.Fatalf("the shared bridge holds %v: a server whose key is unset was handed to "+
			"the supervision that retries it", held)
	}
	// AS A PEER READS IT, off the heartbeat, which bounds the sentence.
	status, err := e.nodeStatus(ctx)
	if err != nil {
		t.Fatalf("node status: %v", err)
	}
	sent, _ := coord.StatusFromMeta(map[string]any{coord.StatusKey: status.Meta()})
	rows := sent.MCP
	if len(rows) != 1 || rows[0].Server != "search" || rows[0].Failed != 1 ||
		rows[0].Waiting != 0 || rows[0].Started != 0 {
		t.Fatalf("heartbeat = %+v: want the shared server failed, once", rows)
	}
	for _, said := range []string{"${SHARED_SEARCH_KEY} is not set",
		"list SEARCH_KEY under mcp_servers.search.optional_keys"} {
		if !strings.Contains(rows[0].Error, said) {
			t.Errorf("the heartbeat's sentence %q does not say %q", rows[0].Error, said)
		}
	}
	if missing := logs.serverRecords("mcp_server_credential_missing", "search"); len(missing) != 1 ||
		missing[0]["level"] != "ERROR" {
		t.Errorf("mcp_server_credential_missing = %v: want one error line for the server", missing)
	}

	// THE APPLY AFTER THE SEAL starts it: its binary is absent, so the
	// start fails, and the bridge keeps it to retry — which is what proves
	// it was handed over at all.
	e.secretsMu.Lock()
	e.installSecrets(map[string]string{"SHARED_SEARCH_KEY": "search-secret"})
	e.secretsMu.Unlock()
	e.startSharedServers(ctx, company)
	if held := e.mcp.Servers(); len(held) != 1 || held[0] != "search" {
		t.Fatalf("after the key was sealed the shared bridge holds %v, want the server", held)
	}

	// A KEY THAT STOPS RESOLVING STOPS THE SERVER, saying why.
	e.secretsMu.Lock()
	e.installSecrets(map[string]string{})
	e.secretsMu.Unlock()
	e.startSharedServers(ctx, company)
	if held := e.mcp.Servers(); len(held) != 0 {
		t.Errorf("after the key went the shared bridge still holds %v", held)
	}
	retired := logs.serverRecords("mcp_server_retired", "search")
	if len(retired) != 1 || retired[0]["level"] != "INFO" ||
		!strings.Contains(retired[0]["detail"].(string), "no longer resolves") {
		t.Errorf("mcp_server_retired = %v: want one line at INFO saying the key no longer "+
			"resolves, as a seat's child retired for its credential is", retired)
	}
}

// A BOX'S SERVER NOTHING WILL SUPPLY IS WITHHELD WITH A WARNING, ONCE, AND A
// NAME THE COMPANY DECLARES NO SERVER UNDER IS STILL SAID.
//
// The run goes ahead without either, so the line is all an operator gets: at
// info it reads as a wait that ends by itself. A scope may name a server twice,
// and the run is withheld it once. And a scope naming no server at all is the
// renderer's to report — a scope is not checked against `mcp_servers` when it
// is written — so it is left in the scope rather than dropped with nothing
// said.
func TestABoxServerNothingWillSupplyIsAWarningAndAnUnknownOneIsSaid(t *testing.T) {
	logs := captureEngineLogs(t)
	c := specCompany(t, `
name: Acme
providers:
  sandbox: {fake: true}
  llm:
    zulu:
      model: claude-sonnet-5
mcp_servers:
  - name: search
    command: search-server
    env:
      SEARCH_KEY: "${SEARCH_KEY_NOBODY_SETS}"
roles:
  - name: CEO
    handle: ceo
    llm: zulu
`)
	(&Engine{}).boxServers(t.Context(), c, c.Org.AgentSeatByHandle("ceo"), &org.RoleSandbox{
		MCP: org.RoleSandboxMCP{Servers: []string{"search", "search", "no-such-server"}}})
	withheld := logs.records("sandbox_mcp_server_withheld", "ceo")
	if len(withheld) != 1 || withheld[0]["level"] != "WARN" ||
		!strings.Contains(withheld[0]["detail"].(string), "${SEARCH_KEY_NOBODY_SETS} is not set") {
		t.Errorf("sandbox_mcp_server_withheld = %v: want one warning naming the variable", withheld)
	}
	if unknown := logs.serverRecords("sandbox_mcp_server_unknown", "no-such-server"); len(unknown) != 1 {
		t.Errorf("sandbox_mcp_server_unknown = %v: want the name no server answers to said", unknown)
	}
}

// A BOX IS GIVEN A SERVER BY THE RULE THE SERVER'S OWN CHILD IS STARTED BY: a
// shared server whose key does not resolve is withheld from it, and a key
// declared optional is left out of what it is given.
func TestABoxIsGivenAServerByTheRuleItsOwnChildIsStartedBy(t *testing.T) {
	t.Parallel()
	company := specCompany(t, `
name: Acme
providers:
  sandbox: {fake: true}
  llm:
    zulu:
      model: claude-sonnet-5
mcp_servers:
  - name: search
    command: "search-server"
    env:
      SEARCH_KEY: "${SEARCH_KEY}"
  - name: docs
    command: "docs-server"
    env:
      DOCS_TOKEN: "${DOCS_TOKEN}"
      NPM_CONFIG_REGISTRY: "${NPM_CONFIG_REGISTRY}"
    optional_keys: [NPM_CONFIG_REGISTRY]
roles:
  - name: CEO
    handle: ceo
    llm: zulu
`)
	seat := company.Org.AgentSeatByHandle("ceo")
	env := config.NewResolver(config.MapSource{"DOCS_TOKEN": "docs-secret"})
	gate := &org.RoleSandbox{MCP: org.RoleSandboxMCP{Servers: []string{"search", "docs"}}}

	got, withheld := sandboxMCP(env, company, seat, gate, nil)
	if _, offered := got["search"]; offered {
		t.Error("the box was offered a server whose key does not resolve")
	}
	if len(withheld) != 1 || withheld[0].server != "search" ||
		!strings.Contains(withheld[0].reason(), "${SEARCH_KEY} is not set") {
		t.Errorf("withheld = %+v: want the search server, naming its key", withheld)
	}
	docs, offered := got["docs"]
	if !offered {
		t.Fatalf("the box was not offered docs, whose required key resolves: %v", got)
	}
	if _, present := docs.Env["NPM_CONFIG_REGISTRY"]; present || docs.Env["DOCS_TOKEN"] != "docs-secret" {
		t.Errorf("the box's docs server is given %v: want its token, and the optional "+
			"host setting left out", docs.Env)
	}
}

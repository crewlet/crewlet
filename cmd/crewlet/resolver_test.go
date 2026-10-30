package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/engine"
	"github.com/crewlet/crewlet/internal/fleetsecrets"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/secrets"
	"github.com/crewlet/crewlet/internal/store"
)

// resolveWith is the resolver a `crewlet gitlab provision` run builds over
// reads, from the Tier A at bootstrapPath and through apiURL where one is
// given, saying on notes what it says.
func resolveWith(t *testing.T, environ config.Source, bootstrapPath, apiURL string,
	reads []string, notes io.Writer) (*config.Resolver, *companyStore, error) {

	t.Helper()
	companySecrets, err := openCompanyStore(environ,
		storeFlags{bootstrap: &bootstrapPath, api: &apiURL},
		"crewlet gitlab provision")
	if err != nil {
		return nil, nil, err
	}
	t.Cleanup(companySecrets.Close)
	resolver, err := companySecrets.resolver(t.Context(), reads, notes)
	return resolver, companySecrets, err
}

// A PROVISIONING RUN READS THE SECRET STORE, NOT JUST THE ENVIRONMENT.
//
// The chain used to be config.EnvOnly(), so every Tier B ${VAR} an operator
// had already put in the store resolved to the empty string. For a GitLab
// signing secret that is not merely a missing value. Empty is the signal to
// MINT, so the run replaced a working webhook secret at the third-party app
// with a fresh one and broke every delivery in flight until the config
// caught up. With the engine stopped the run reaches the fleet's store itself,
// and says so.
func TestAProvisioningRunResolvesThroughTheSecretStore(t *testing.T) {
	t.Parallel()
	cfg := bootstrapWithKeyring(t, "k1")
	if _, errs, err := secretsCmd(t, cfg, "set", "GITLAB_SIGNING_SECRET",
		"-value", "whsec_c3RvcmVkLXNpZ25pbmcta2V5LW9mLTMyLWJ5dGVzIQ=="); err != nil {
		t.Fatalf("seed the store: %v (%s)", err, errs)
	}

	var notes bytes.Buffer
	env, _, err := resolveWith(t, noEnviron, cfg, "",
		[]string{"GITLAB_SIGNING_SECRET"}, &notes)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}

	got := env.Value("${GITLAB_SIGNING_SECRET}")
	if got != "whsec_c3RvcmVkLXNpZ25pbmcta2V5LW9mLTMyLWJ5dGVzIQ==" {
		t.Fatalf("the stored secret did not reach the run: %q", got)
	}
	// AND IT SAYS WHICH CHAIN RAN, as every other chain does: the store,
	// reached with the engine stopped, and how many of the values the
	// command reads it held.
	for _, want := range []string{"the fleet's store", "engine is stopped", "1 of the stored values"} {
		if !strings.Contains(notes.String(), want) {
			t.Errorf("the note omits %q: %q", want, notes.String())
		}
	}
}

// STORE FIRST, ENVIRONMENT BEHIND — the same order the engine resolves in.
//
// A rotated secret must win over a stale export, which is the whole reason
// the store exists: rotation is an update of one row, and an environment
// that could shadow it would make the rotation appear to work and change
// nothing.
func TestAStoredSecretWinsOverAStaleExport(t *testing.T) {
	cfg := bootstrapWithKeyring(t, "k1")
	if _, errs, err := secretsCmd(t, cfg, "set", "CONFLUENCE_TOKEN",
		"-value", "the-rotated-one"); err != nil {
		t.Fatalf("seed the store: %v (%s)", err, errs)
	}
	t.Setenv("CONFLUENCE_TOKEN", "the-stale-export")

	env, _, err := resolveWith(t, noEnviron, cfg, "", []string{"CONFLUENCE_TOKEN"}, &bytes.Buffer{})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}

	if got := env.Lookup("CONFLUENCE_TOKEN"); got != "the-rotated-one" {
		t.Fatalf("a stale export shadowed the rotated secret: %q", got)
	}
}

// THE OPERATOR'S OWN CREDENTIAL IS NOT THE COMPANY'S, and does not come
// from the store — nor from the process's environment, but from the one the
// invocation is handed ([runIn]), which for the binary is the process's.
//
// A GitLab admin PAT carries `api` scope over the whole group, and the store
// is replicated to every node holding the keyring. Crewlet never persists
// one; reading one back would imply it may be kept there — which is how the
// most powerful credential in the deployment ends up in the shared table
// beside the seat tokens it exists to mint. And a case that read the process's
// environment would run on whatever the machine running the suite exports.
//
// Not parallel: it exports the credential on the process, to show a run
// handed an environment without it does not read it there.
//
// Mutation: read the store, or the process's environment, and the run finds an
// administrator token.
func TestAnOperatorCredentialIsNotReadFromTheStore(t *testing.T) {
	cfg := bootstrapWithKeyring(t, "k1")
	if _, errs, err := secretsCmd(t, cfg, "set", "GITLAB_ADMIN_TOKEN",
		"-value", "stored-admin-pat"); err != nil {
		t.Fatalf("seed the store: %v (%s)", err, errs)
	}
	t.Setenv("GITLAB_ADMIN_TOKEN", "process-admin-pat")
	company := companyAt(t, readerCompanyDoc, untouchedVendor(t))
	_, _, err := cliIn(t, noEnviron, "gitlab", "provision", company, "-print",
		"-config", cfg, "-public-url", "https://crewlet.example.com")
	if err == nil || !strings.Contains(err.Error(), "no administrator token") {
		t.Fatalf("err = %v, want the run to find no administrator token: one was read "+
			"out of the secret store or the process's environment", err)
	}
	exported := config.MapSource{"GITLAB_ADMIN_TOKEN": "  exported-admin-pat  "}
	if got := operatorCredential(exported, "GITLAB_ADMIN_TOKEN"); got != "exported-admin-pat" {
		t.Errorf("the exported credential did not reach the run: %q", got)
	}
}

// NO STORE IS A SUPPORTED DEPLOYMENT, AND IT SAYS SO.
//
// Environment-only resolution is the pre-store shape and must keep working.
// It must not be SILENT, though: a mistyped -config resolving nothing has
// exactly the destructive outcome above, and the only way an operator can
// tell the two apart is the run saying which chain it ran.
func TestWithoutAStoreTheRunSaysWhichChainItUsed(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "not-here.yaml")

	var notes bytes.Buffer
	env, _, err := resolveWith(t, noEnviron, missing, "", nil, &notes)
	if err != nil {
		t.Fatalf("a missing bootstrap is a supported deployment: %v", err)
	}

	t.Setenv("SOME_TOKEN", "from-the-environment")
	if got := env.Lookup("SOME_TOKEN"); got != "from-the-environment" {
		t.Errorf("environment-only resolution stopped working: %q", got)
	}
	if !strings.Contains(notes.String(), missing) {
		t.Errorf("the note does not name the path that was not there: %q", notes.String())
	}
	if !strings.Contains(notes.String(), "environment only") {
		t.Errorf("the note does not say which chain ran: %q", notes.String())
	}
}

// A KEYRINGLESS BOOTSTRAP IS REFUSED, NOT RESOLVED FROM THE ENVIRONMENT.
//
// It used to be the second environment-only case, beside a missing file: a
// node with a store and nothing to decrypt it with. Every node needs a keyring
// now, so a Tier A without one is a file no node can run, and resolving a
// company's values from whatever this shell holds on the strength of it would
// be the stale-export shadowing this chain exists to prevent. The refusal
// names the field, and the environment note is never written.
func TestABootstrapWithNoKeyringIsRefused(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("node:\n  id: cli-test\n"), 0o600); err != nil {
		t.Fatalf("write bootstrap: %v", err)
	}

	var notes bytes.Buffer
	_, _, err := resolveWith(t, noEnviron, path, "", nil, &notes)
	if err == nil {
		t.Fatal("a keyringless bootstrap was resolved rather than refused")
	}
	if !strings.Contains(err.Error(), "secrets.keys") {
		t.Errorf("the refusal does not name the keyring: %v", err)
	}
	if strings.Contains(notes.String(), "environment only") {
		t.Errorf("a refused bootstrap still announced an environment-only run: %q",
			notes.String())
	}
}

// A BROKEN BOOTSTRAP FAILS THE RUN RATHER THAN FALLING BACK.
//
// Falling back is the stale-export shadowing this chain exists to prevent,
// and it would happen at the worst moment: an operator who configured a
// store and did not get it would have every secret resolved from whatever
// their shell happened to hold.
func TestABrokenBootstrapRefusesRatherThanResolvingFromTheEnvironment(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("store:\n  path: [not, a, string]\n"), 0o600); err != nil {
		t.Fatalf("write bootstrap: %v", err)
	}

	var notes bytes.Buffer
	if _, _, err := resolveWith(t, noEnviron, path, "", nil, &notes); err == nil {
		t.Fatal("a bootstrap that cannot be read was treated as no bootstrap, " +
			"so a configured store silently became a stale environment")
	}
	if notes.Len() > 0 {
		t.Errorf("a failed run announced a fallback it did not take: %q", notes.String())
	}
}

// revealNode is a running node's /secrets surface as a run reading through it
// meets it: a listing of the names the fleet holds, POST /secrets/reveal
// answering the values of the names a batch asks for — refusing the whole
// batch over a name outside the grammar or a reserved one, as the node does —
// the single break-glass GET, and the write.
type revealNode struct {
	server *httptest.Server

	mu sync.Mutex

	// held is every value the node holds, by name: what it was made with,
	// and what [revealNode.hold] has added since, while it serves.
	held map[string]string

	// refuse, when set, is the status every reveal is answered with, and
	// its body the engine's own refusal of a machine token.
	refuse int

	// refuseListing, when set, is the status the listing is answered with.
	refuseListing int

	// gone is the names the listing still names and a reveal no longer
	// finds: removed between the two.
	gone map[string]bool

	// asked is each POST /secrets/reveal batch, and revealed every name a
	// reveal of either kind asked for; gets is the GET reveals alone.
	asked    [][]string
	revealed []string
	gets     []string

	tokens       []string
	contentTypes []string
	listings     int
	stored       map[string]string
}

func newRevealNode(t *testing.T, held map[string]string) *revealNode {
	t.Helper()
	n := &revealNode{held: held, gone: map[string]bool{}}
	n.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n.mu.Lock()
		n.tokens = append(n.tokens, r.Header.Get("Authorization"))
		n.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if name, ok := strings.CutPrefix(r.URL.Path, "/secrets/"); ok && r.Method == http.MethodPut {
			value, _ := io.ReadAll(r.Body)
			n.mu.Lock()
			if n.stored == nil {
				n.stored = map[string]string{}
			}
			n.stored[name] = string(value)
			n.mu.Unlock()
			// AND THE RE-ACTIVATION A NODE MAKES of a value its company
			// reads (ADR-0070), which a command prints.
			_, _ = fmt.Fprint(w, `{"updated_by":"token:ops","updated_by_kind":"operator",`+
				`"read_by":["providers.llm.default.auth.token"],`+
				`"reload":{"revision_id":"r-2","epoch":7}}`)
			return
		}
		// THE BATCH WRITE, which a provisioning sink records through.
		if r.Method == http.MethodPut && r.URL.Path == "/secrets" {
			var batch struct {
				Values map[string]string `json:"values"`
			}
			_ = json.NewDecoder(r.Body).Decode(&batch)
			n.mu.Lock()
			if n.stored == nil {
				n.stored = map[string]string{}
			}
			maps.Copy(n.stored, batch.Values)
			n.mu.Unlock()
			_, _ = fmt.Fprint(w, `{"read_by":{},"reload":null}`)
			return
		}
		if r.Method == http.MethodPost && r.URL.Path == "/secrets/reveal" {
			n.revealMany(w, r)
			return
		}
		if r.Method != http.MethodGet {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		if r.URL.Path == "/secrets" {
			n.mu.Lock()
			n.listings++
			if n.refuseListing != 0 {
				n.mu.Unlock()
				w.WriteHeader(n.refuseListing)
				_, _ = fmt.Fprint(w, `{"error":"forbidden","detail":"this token may not list secrets"}`)
				return
			}
			var rows []map[string]string
			for name := range n.held {
				rows = append(rows, map[string]string{"name": name, "key_id": "k1"})
			}
			n.mu.Unlock()
			_ = json.NewEncoder(w).Encode(map[string]any{"secrets": rows})
			return
		}
		name, ok := strings.CutPrefix(r.URL.Path, "/secrets/")
		if !ok || r.URL.Query().Get("reveal") != "true" {
			w.WriteHeader(http.StatusNotFound)
			_, _ = fmt.Fprint(w, `{}`)
			return
		}
		n.mu.Lock()
		n.revealed = append(n.revealed, name)
		n.gets = append(n.gets, name)
		refuse := n.refuse
		value, held := n.held[name]
		gone := n.gone[name]
		n.mu.Unlock()
		if refuse != 0 {
			w.WriteHeader(refuse)
			_, _ = fmt.Fprint(w, `{"error":"token_refused","detail":"a person must be present"}`)
			return
		}
		if !held || gone {
			w.WriteHeader(http.StatusNotFound)
			_, _ = fmt.Fprint(w, `{"error":"not_found"}`)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"name": name, "value": value})
	}))
	t.Cleanup(n.server.Close)
	return n
}

// revealMany answers POST /secrets/reveal as the node does: the whole batch
// refused over one name outside the grammar (400) or one reserved (403), and
// otherwise `values` for what it holds and `missing` for the rest.
func (n *revealNode) revealMany(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Names []string `json:"names"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = fmt.Fprint(w, `{"error":"invalid_body"}`)
		return
	}
	n.mu.Lock()
	n.asked = append(n.asked, slices.Clone(body.Names))
	n.revealed = append(n.revealed, body.Names...)
	n.contentTypes = append(n.contentTypes, r.Header.Get("Content-Type"))
	refuse := n.refuse
	n.mu.Unlock()
	if refuse != 0 {
		w.WriteHeader(refuse)
		_, _ = fmt.Fprint(w, `{"error":"token_refused","detail":"a person must be present"}`)
		return
	}
	for _, name := range body.Names {
		if secrets.Reserved(name) {
			w.WriteHeader(http.StatusForbidden)
			_, _ = fmt.Fprint(w, `{"error":"reserved_name"}`)
			return
		}
		if secrets.CheckName(name) != nil {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = fmt.Fprint(w, `{"error":"invalid_name"}`)
			return
		}
	}
	values, missing := map[string]string{}, []string{}
	n.mu.Lock()
	for _, name := range body.Names {
		if value, held := n.held[name]; held && !n.gone[name] {
			values[name] = value
		} else {
			missing = append(missing, name)
		}
	}
	n.mu.Unlock()
	_ = json.NewEncoder(w).Encode(map[string]any{"values": values, "missing": missing})
}

// listed is how many times the node has been asked for its listing.
func (n *revealNode) listed() int {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.listings
}

// requests is how many requests of any kind have reached the node.
func (n *revealNode) requests() int {
	n.mu.Lock()
	defer n.mu.Unlock()
	return len(n.tokens)
}

// reveals is every name a reveal of either kind asked for, sorted.
func (n *revealNode) reveals() []string {
	n.mu.Lock()
	defer n.mu.Unlock()
	out := slices.Clone(n.revealed)
	slices.Sort(out)
	return out
}

// batches is every POST /secrets/reveal batch, in order, and the GET reveals.
func (n *revealNode) batches() (asked [][]string, gets []string) {
	n.mu.Lock()
	defer n.mu.Unlock()
	return slices.Clone(n.asked), slices.Clone(n.gets)
}

// holds is the value the node holds under name, written to it or listed by it.
func (n *revealNode) holds(name string) (string, bool) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if value, ok := n.stored[name]; ok {
		return value, true
	}
	value, ok := n.held[name]
	return value, ok
}

// storedThroughNode is what a `crewlet gitlab provision` run, named -api, reads
// of the node for reads: its stored values, and what it said.
func storedThroughNode(t *testing.T, node *revealNode, environ config.Source,
	reads []string) (*storedValues, string, error) {

	t.Helper()
	var notes bytes.Buffer
	missing := filepath.Join(t.TempDir(), "crewlet.yaml")
	api := node.server.URL
	companySecrets, err := openCompanyStore(environ, storeFlags{bootstrap: &missing, api: &api},
		"crewlet gitlab provision")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(companySecrets.Close)
	values, err := companySecrets.stored(t.Context(), reads, &notes)
	return values, notes.String(), err
}

// readingCompany reads two variables: a ${VAR} its model's env names, and the
// headless token its Claude Code entry falls back to while it names none.
func readingCompany(t *testing.T) *config.Company {
	t.Helper()
	company, err := config.ParseCompanyDocument([]byte(`
name: Acme
roles:
  - name: Chief Executive
    handle: ceo
    goal: Run the company
providers:
  sandbox: {fake: true}
  llm:
    default:
      model: sonnet
      env:
        CREWLET_TEST_SETTING: "${PROXY_URL}"
`))
	if err != nil {
		t.Fatal(err)
	}
	return company
}

// scopeCompanyDoc reads variables at every kind of field a command reads, and
// beside each one a variable that command must not read: an entry whose key
// is another's and a dot; a vendor block's field the command never resolves;
// a unit's credential its direct member inherits, and one that no seat
// inherits; a key of a seat's vendor block that no reader consults; a tool
// server whose name starts with the vendor's; and a Confluence-only token in
// the block Jira shares with it.
const scopeCompanyDoc = `
name: Acme
providers:
  sandbox: {fake: true}
  llm:
    default:
      model: sonnet
      env:
        CREWLET_TEST_SETTING: "${PROXY_URL}"
    default.fast:
      model: haiku
      auth:
        mode: api-key
        api_key: "${FAST_KEY}"
    metered:
      model: sonnet
      auth:
        mode: api-key
        api_key: "${METERED_KEY}"
mcp_servers:
  - name: github.internal
    command: github-mcp-server
integrations:
  gitlab:
    enabled: true
    url: "${GITLAB_URL}"
    token: "${GITLAB_READ_TOKEN}"
    signing_secret: "${GITLAB_SIGNING_SECRET}"
  github:
    enabled: true
    token: "${GITHUB_ORG_TOKEN}"
    webhook_secret: "${GITHUB_WEBHOOK_SECRET}"
  mattermost:
    enabled: true
    url: "${MATTERMOST_URL}"
    team: acme
  confluence:
    url: "${CONFLUENCE_URL}"
    email: "${CONFLUENCE_EMAIL}"
    token: "${CONFLUENCE_TOKEN}"
roles:
  - name: Chief Executive
    handle: ceo
    llm: default
    integrations:
      mattermost:
        bot_token: "${MATTERMOST_BOT_CEO}"
units:
  - name: Engineering
    lead: ceo
    mcp_env:
      github:
        GITHUB_TOKEN: "${GITHUB_TOKEN_ENG}"
        GITHUB_HOST: "${GITHUB_HOST}"
    roles:
      - name: Architect
        handle: architect
        llm: default
        mcp_env:
          github.internal:
            GITHUB_TOKEN: "${GHE_TOKEN}"
    children:
      - name: Platform
        roles:
          - name: Engineer
            handle: swe
            llm: metered
            mcp_env:
              jira:
                JIRA_API_TOKEN: "${JIRA_TOKEN_SWE}"
              atlassian:
                CONFLUENCE_PERSONAL_TOKEN: "${CONFLUENCE_PAT_SWE}"
            integrations:
              mattermost:
                bot_token: "${MATTERMOST_BOT_SWE}"
  - name: Research
    lead: ceo
    mcp_env:
      github:
        GITHUB_TOKEN: "${GITHUB_TOKEN_RESEARCH}"
    children:
      - name: Lab
        roles:
          - name: Scientist
            handle: scientist
            llm: default
`

// scopeNode is a node holding every value scopeCompanyDoc names, and the ones
// it reads without naming them, and one it does not read, each stored as "v-"
// and its name.
func scopeNode(t *testing.T) *revealNode {
	t.Helper()
	held := map[string]string{}
	for _, name := range []string{
		"PROXY_URL", "CLAUDE_CODE_OAUTH_TOKEN", "FAST_KEY",
		"CREWLET_LLM_CLI_DEFAULT_FAST_CREDENTIALS", "METERED_KEY",
		"CREWLET_LLM_CLI_METERED_CREDENTIALS", "GITLAB_URL", "GITLAB_READ_TOKEN",
		"GITLAB_SIGNING_SECRET", "GITHUB_ORG_TOKEN", "GITHUB_WEBHOOK_SECRET",
		"GITHUB_TOKEN_ENG", "GITHUB_HOST", "GITHUB_TOKEN_RESEARCH", "GHE_TOKEN",
		"MATTERMOST_URL", "MATTERMOST_BOT_CEO", "MATTERMOST_BOT_SWE",
		"JIRA_TOKEN_SWE", "CONFLUENCE_PAT_SWE", "CONFLUENCE_URL",
		"CONFLUENCE_EMAIL", "CONFLUENCE_TOKEN", "READ_BY_NOTHING",
	} {
		held[name] = "v-" + name
	}
	return newRevealNode(t, held)
}

func scopeCompany(t *testing.T) (*config.Company, *org.Organization) {
	t.Helper()
	company, err := config.ParseCompanyDocument([]byte(scopeCompanyDoc))
	if err != nil {
		t.Fatal(err)
	}
	organization, err := company.Organization()
	if err != nil {
		t.Fatal(err)
	}
	return company, organization
}

// THROUGH A RUNNING NODE A COMMAND REVEALS ONLY WHAT IT READS, IN ONE BATCH.
//
// Each value comes back through a REVEAL, logged by name against the caller,
// so a command asks for what it reads and nothing else: one entry's sign-in
// for `crewlet llm login`, not its dotted neighbour's, nor every vendor's
// tokens; a GitLab provision's instance and signing key, not the engine's
// GitLab token; the seat credentials a reader consults, not the rest of the
// block they sit in, a unit's credential no seat inherits, or a tool server's
// that only shares the vendor's name. And it asks for them together, in one
// POST /secrets/reveal, where it once made one GET per name.
func TestARunningEngineRevealsOnlyWhatTheCommandReads(t *testing.T) {
	t.Parallel()
	company, organization := scopeCompany(t)
	integrations := company.Integrations
	for _, tc := range []struct {
		command string
		reads   []string
		want    []string
	}{
		{
			// The token it signs in from without naming it, and the env
			// its CLI runs with; the bundle is not stored.
			"crewlet llm login default", llmEntryReads(company, []string{"default"}),
			[]string{"CLAUDE_CODE_OAUTH_TOKEN", "PROXY_URL"},
		},
		{
			"crewlet llm status default.fast", llmEntryReads(company, []string{"default.fast"}),
			[]string{"CREWLET_LLM_CLI_DEFAULT_FAST_CREDENTIALS", "FAST_KEY"},
		},
		{
			"crewlet llm list", llmEntryReads(company, company.Providers.ProviderOrder()),
			[]string{"CLAUDE_CODE_OAUTH_TOKEN", "CREWLET_LLM_CLI_DEFAULT_FAST_CREDENTIALS",
				"CREWLET_LLM_CLI_METERED_CREDENTIALS", "FAST_KEY", "METERED_KEY", "PROXY_URL"},
		},
		{
			"crewlet gitlab provision", gitlabProvisionReads(integrations.GitLab),
			[]string{"GITLAB_SIGNING_SECRET", "GITLAB_URL"},
		},
		{
			// The org's, and the seat credential the Architect inherits
			// from Engineering.
			"crewlet github provision", githubProvisionReads(integrations.GitHub, organization),
			[]string{"GITHUB_ORG_TOKEN", "GITHUB_TOKEN_ENG", "GITHUB_WEBHOOK_SECRET"},
		},
		{
			"crewlet jira provision", jiraProvisionReads(&config.Jira{}, organization),
			[]string{"JIRA_TOKEN_SWE"},
		},
		{
			// A bot token at the root and two units down.
			"crewlet mattermost doctor", mattermostDoctorReads(integrations.Mattermost, organization),
			[]string{"MATTERMOST_BOT_CEO", "MATTERMOST_BOT_SWE", "MATTERMOST_URL"},
		},
		{
			"crewlet confluence import", confluenceReads(integrations.Confluence),
			[]string{"CONFLUENCE_EMAIL", "CONFLUENCE_TOKEN", "CONFLUENCE_URL"},
		},
	} {
		t.Run(tc.command, func(t *testing.T) {
			t.Parallel()
			node := scopeNode(t)
			values, notes, err := storedThroughNode(t, node, tokenEnviron("ops-token"), tc.reads)
			if err != nil {
				t.Fatalf("a running engine's store was refused rather than read through it: %v", err)
			}
			asked, gets := node.batches()
			if len(asked) != 1 || !slices.Equal(asked[0], tc.want) {
				t.Errorf("asked %v, want one batch of only what the command reads, %v",
					asked, tc.want)
			}
			if len(gets) != 0 {
				t.Errorf("revealed %v one GET at a time beside the batch", gets)
			}
			resolver := config.WithStore(values)
			for _, name := range tc.want {
				if got := resolver.Lookup(name); got != "v-"+name {
					t.Errorf("${%s} resolved to %q, not the fleet's value", name, got)
				}
			}
			for _, token := range node.tokens {
				if token != "Bearer ops-token" {
					t.Errorf("a request carried %q, not the invocation's own credential", token)
				}
			}
			for _, want := range []string{node.server.URL,
				fmt.Sprintf(": %d of the %d stored values", len(tc.want), len(tc.want)),
				"in 1 request,"} {
				if !strings.Contains(notes, want) {
					t.Errorf("the run did not say which chain it read, and how much (%q): %q",
						want, notes)
				}
			}
		})
	}
}

// ONLY NAMES THE NODE LISTS ARE ASKED FOR. A name the command reads and the
// node does not hold costs no request and is never named to the node, whose
// log records every name a batch asks; a name the listing gave and the node
// no longer finds — removed between the two — falls to the environment, and
// is not asked for again when it is read.
func TestOnlyNamesTheNodeListsAreAskedFor(t *testing.T) {
	t.Parallel()
	node := newRevealNode(t, map[string]string{"HELD": "v-HELD", "GONE": "v-GONE"})
	node.gone["GONE"] = true
	values, notes, err := storedThroughNode(t, node, tokenEnviron("ops-token"),
		[]string{"GONE", "HELD", "NEVER_STORED"})
	if err != nil {
		t.Fatal(err)
	}
	asked, _ := node.batches()
	if want := [][]string{{"GONE", "HELD"}}; len(asked) != 1 || !slices.Equal(asked[0], want[0]) {
		t.Fatalf("asked %v, want %v: a name the node does not list was sent", asked, want)
	}
	if _, ok := values.Lookup("GONE"); ok {
		t.Error("a name the node answered missing resolved as held")
	}
	if _, ok := values.Lookup("NEVER_STORED"); ok {
		t.Error("a name the node never listed resolved as held")
	}
	if asked, _ := node.batches(); len(asked) != 1 {
		t.Errorf("reading the missing names asked the node again: %v", asked)
	}
	if !strings.Contains(notes, ": 1 of the 2 stored values") {
		t.Errorf("the line does not count what came back of what was asked: %q", notes)
	}
}

// NO VALUE THE COMMAND READS IS HELD: the listing alone, which reveals
// nothing, and the line says so rather than counting zero reveals.
func TestANodeHoldingNoneOfTheReadsIsAskedNothing(t *testing.T) {
	t.Parallel()
	node := newRevealNode(t, map[string]string{"SOMETHING_ELSE": "x"})
	_, notes, err := storedThroughNode(t, node, tokenEnviron("ops-token"), []string{"GITLAB_URL"})
	if err != nil {
		t.Fatal(err)
	}
	if asked, gets := node.batches(); len(asked)+len(gets) != 0 {
		t.Errorf("a node holding none of the reads was asked %v and %v", asked, gets)
	}
	if !strings.Contains(notes, "it holds none of the values this command reads") {
		t.Errorf("the line does not say nothing was revealed: %q", notes)
	}
}

// A READ BEYOND WHAT THE COMMAND REVEALS FIRST IS STILL THE FLEET'S VALUE,
// revealed as it is made — one name, by the same POST — and said: a command
// whose list missed a field it reads resolves what the engine would, rather
// than reading the stored value as unset — which, for a key a provisioner
// mints when it resolves empty, replaces a working one. And a name the fleet
// does not hold costs no request.
func TestAReadBeyondTheScopeIsStillTheFleetsValue(t *testing.T) {
	t.Parallel()
	node := scopeNode(t)
	company, _ := scopeCompany(t)
	values, _, err := storedThroughNode(t, node, tokenEnviron("ops-token"),
		gitlabProvisionReads(company.Integrations.GitLab))
	if err != nil {
		t.Fatal(err)
	}
	var notes bytes.Buffer
	values.notes = &notes
	resolver := config.WithStore(values)
	if got := resolver.Value("${GITLAB_READ_TOKEN}"); got != "v-GITLAB_READ_TOKEN" {
		t.Errorf("a read beyond the scope resolved to %q, not the fleet's value", got)
	}
	if !strings.Contains(notes.String(), "${GITLAB_READ_TOKEN}") ||
		!strings.Contains(notes.String(), "as it was read") {
		t.Errorf("the reveal beyond the scope was not said: %q", notes.String())
	}
	// AS A REVEAL, logged where it was made: the same words the command's
	// first line used for its way in.
	if !strings.Contains(notes.String(), "revealed ${GITLAB_READ_TOKEN}") ||
		!strings.Contains(notes.String(), "logged there by name") {
		t.Errorf("a reveal through the node was said as a direct read: %q", notes.String())
	}
	resolver.Value("${GITLAB_READ_TOKEN}")
	resolver.Value("${NOT_HELD_BY_THE_FLEET}")
	asked, gets := node.batches()
	want := [][]string{{"GITLAB_SIGNING_SECRET", "GITLAB_URL"}, {"GITLAB_READ_TOKEN"}}
	if len(asked) != len(want) || !slices.Equal(asked[0], want[0]) || !slices.Equal(asked[1], want[1]) {
		t.Errorf("asked %v, want %v: once each, and nothing the fleet does not hold", asked, want)
	}
	if len(gets) != 0 {
		t.Errorf("a read beyond the scope revealed %v by GET", gets)
	}
}

// A FAILED READ BEYOND WHAT THE COMMAND REVEALS FIRST STOPS THE RUN, and says
// what to change. A lookup has no error to return, and the only other answer
// it has — unset — is the one that mints over a working secret; the command's
// top answers the stop as the error it is ([runIn]). The token is named only
// where the node refused it: a node failing answers the same for any token,
// and naming one then sends an operator to change a token that works.
func TestARefusedReadBeyondTheScopeStopsTheRun(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		status    int
		nameToken bool
	}{
		{http.StatusForbidden, true},
		{http.StatusServiceUnavailable, false},
	} {
		t.Run(strconv.Itoa(tc.status), func(t *testing.T) {
			t.Parallel()
			node := scopeNode(t)
			company, _ := scopeCompany(t)
			values, _, err := storedThroughNode(t, node, tokenEnviron("ops-token"),
				gitlabProvisionReads(company.Integrations.GitLab))
			if err != nil {
				t.Fatal(err)
			}
			node.mu.Lock()
			node.refuse = tc.status
			node.mu.Unlock()

			stopped := func() (stopped error) {
				defer func() {
					failed, ok := recover().(readFailed)
					if ok {
						stopped = failed
					}
				}()
				got, _ := values.Lookup("GITLAB_READ_TOKEN")
				t.Fatalf("a failed reveal resolved as %q, which a provisioner reads as unset", got)
				return nil
			}()
			if stopped == nil || !strings.Contains(stopped.Error(), "${GITLAB_READ_TOKEN}") ||
				!strings.Contains(stopped.Error(), strconv.Itoa(tc.status)) {
				t.Fatalf("the run stopped with %v, want an error naming the variable and "+
					"the node's answer", stopped)
			}
			if named := strings.Contains(stopped.Error(), apiTokenEnv); named != tc.nameToken {
				t.Errorf("a %d names the token: %v, want %v: %v", tc.status, named,
					tc.nameToken, stopped)
			}
		})
	}
}

// A GITLAB PROVISION THROUGH A NODE KEEPS THE KEY ITS HOOK SIGNS WITH, reading
// it through the node in one batch of the two fields it reads and nothing
// else, none of them as a read beyond its list — whether or not a Tier A is
// here, and creating no database where one is: -api names the node, possibly
// from a machine that is not one.
func TestAGitLabProvisionThroughANodeRevealsOnlyItsOwnFields(t *testing.T) {
	t.Parallel()
	cfg := bootstrapWithKeyring(t, "k1")
	for name, config := range map[string]string{
		"with a Tier A here": cfg,
		"with no Tier A":     filepath.Join(t.TempDir(), "absent.yaml"),
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			node := scopeNode(t)
			company := writeFile(t, t.TempDir(), "company.yaml", strings.NewReplacer(
				"url: https://gitlab.example.com", `url: "${GITLAB_URL}"`,
				"    enabled: true\n", "    enabled: true\n    token: \"${GITLAB_READ_TOKEN}\"\n",
			).Replace(gitlabCompanyDoc))
			var out, errs bytes.Buffer
			err := runGitLabProvision(invocation{environ: tokenEnviron("ops-token")}, []string{company, "-dry-run",
				"-config", config, "-api", node.server.URL,
				"-public-url", "https://crewlet.example.com"}, &out, &errs)
			if err != nil {
				t.Fatalf("gitlab provision -dry-run through a node: %v\n%s", err, errs.String())
			}
			if !strings.Contains(out.String(), "reused — the configured one already resolves") {
				t.Errorf("the stored signing key did not reach the plan, which mints over it:\n%s",
					out.String())
			}
			asked, gets := node.batches()
			if want := []string{"GITLAB_SIGNING_SECRET", "GITLAB_URL"}; len(asked) != 1 ||
				!slices.Equal(asked[0], want) || len(gets) != 0 {
				t.Errorf("asked %v and %v, want one batch of the two fields the command reads, %v",
					asked, gets, want)
			}
			if strings.Contains(out.String(), "as it was read") {
				t.Errorf("the command read beyond the fields it declares:\n%s", out.String())
			}
		})
	}
	db := storePathOf(t, cfg)
	for _, path := range []string{db, db + ".lock"} {
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("a run through a named node created %s here: %v", path, err)
		}
	}
}

// A NAMED NODE IS READ WHETHER OR NOT A TIER A IS HERE. A command run against
// a node from a machine that is not one has no Tier A at its default path,
// and used to resolve from the environment alone, so a GitLab provision run
// from a workstation with -api read the signing key as unset and minted a new
// one over the hook's.
func TestANamedNodeIsReadWithNoTierAHere(t *testing.T) {
	t.Parallel()
	node := scopeNode(t)
	missing := filepath.Join(t.TempDir(), "crewlet.yaml")
	var notes bytes.Buffer
	env, companySecrets, err := resolveWith(t, tokenEnviron("ops-token"), missing,
		node.server.URL, []string{"GITLAB_SIGNING_SECRET"}, &notes)
	if err != nil {
		t.Fatalf("a named node with no Tier A here: %v", err)
	}
	if got := env.Lookup("GITLAB_SIGNING_SECRET"); got != "v-GITLAB_SIGNING_SECRET" {
		t.Errorf("the signing key resolved to %q, not the named node's", got)
	}
	if companySecrets.boot != nil || strings.Contains(notes.String(), "environment only") {
		t.Errorf("the run read the environment alone (Tier A %v): %q",
			companySecrets.boot, notes.String())
	}
}

// A STOPPED NODE READS WHAT THE FLEET HOLDS — the resolver and the sink alike.
//
// The bug this whole route exists for: with the engine stopped, a command read
// the node's own table, which never held what was stored through a running
// node, so a provisioning run read a working signing key as unset and MINTED
// over it, and a -secret-store sink read a seat's working token as absent and
// replaced it. The value here is put on the fleet's store the way a running
// node puts it there, and both halves of one provisioning run must see it —
// at once, as a run holds them: the resolver it plans from and the sink it
// asks before minting, over the run's one route.
//
// Mutation: answer a stopped node from anywhere but the fleet's store, and
// both resolve unset.
func TestAStoppedNodeReadsWhatTheFleetHolds(t *testing.T) {
	t.Parallel()
	cfg := bootstrapWithKeyring(t, "k1")
	boot, err := loadBootstrapForStore(cfg)
	if err != nil {
		t.Fatalf("load Tier A: %v", err)
	}
	const key = "whsec_c3RvcmVkLXNpZ25pbmcta2V5LW9mLTMyLWJ5dGVzIQ=="
	onTheFleet, err := engine.OpenFleetStore(t.Context(), boot)
	if err != nil {
		t.Fatalf("open the fleet's store as a node does: %v", err)
	}
	cipher, err := boot.Secrets.Cipher()
	if err != nil {
		t.Fatal(err)
	}
	if err := fleetsecrets.New(onTheFleet.Fleet, cipher).Set(t.Context(),
		"GITLAB_SIGNING_SECRET", key, secrets.Author{Name: "token:ops", Kind: "operator"},
		secrets.SourceAPI, time.Now()); err != nil {
		t.Fatalf("store the key on the fleet: %v", err)
	}
	onTheFleet.Close()

	flags := flag.NewFlagSet("provision", flag.ContinueOnError)
	sinks := addSinkFlags(flags)
	if err := flags.Parse([]string{"-secret-store", "-config", cfg}); err != nil {
		t.Fatal(err)
	}
	companySecrets, err := openCompanyStore(noEnviron, sinks.store, "crewlet gitlab provision")
	if err != nil {
		t.Fatal(err)
	}
	defer companySecrets.Close()
	env, err := companySecrets.resolver(t.Context(), []string{"GITLAB_SIGNING_SECRET"},
		&bytes.Buffer{})
	if err != nil {
		t.Fatalf("resolve on a stopped node: %v", err)
	}
	if got := env.Value("${GITLAB_SIGNING_SECRET}"); got != key {
		t.Errorf("the resolver read the fleet's signing key as %q", got)
	}

	sink, err := sinks.open(t.Context(), companySecrets, io.Discard)
	if err != nil {
		t.Fatalf("open the -secret-store sink beside the resolver: %v", err)
	}
	held, ok, err := sink.Value(t.Context(), "GITLAB_SIGNING_SECRET")
	if err != nil || !ok || held != key {
		t.Errorf("the sink read the fleet's key as %q (held %v, err %v): a "+
			"provisioner asking it would mint over a working one", held, ok, err)
	}
}

// ONLY A LOCKED DATABASE IS READ THROUGH THE NODE. Any other failure to reach
// the store with the engine stopped is a broken node, and answering it through
// the node's API would replace its message with a connection refused — or
// with an answer from a node the run was never meant to ask.
//
// Mutation: route every failure of the stopped-engine way in through the
// node, and the node is asked.
func TestOnlyALockedStoreIsReadThroughTheNode(t *testing.T) {
	t.Parallel()
	node := newRevealNode(t, map[string]string{"PROXY_URL": "http://proxy.example.com:3128"})
	cfg := bootstrapForURL(t, node.server.URL)
	boot, err := loadBootstrapForStore(cfg)
	if err != nil {
		t.Fatal(err)
	}
	// A STORE DIRECTORY THAT IS A FILE: no broker starts on it, and nothing
	// holds the node's database.
	if err := os.WriteFile(boot.Stream.StoreDir, []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}

	_, _, err = resolveWith(t, tokenEnviron(cliFixtureToken), cfg, "",
		llmEntryReads(readingCompany(t), []string{"default"}), &bytes.Buffer{})
	if err == nil {
		t.Fatal("a store no broker could start on resolved anyway")
	}
	if errors.Is(err, store.ErrLocked) {
		t.Fatalf("an unrelated failure was read as a running engine: %v", err)
	}
	if n := node.requests(); n != 0 {
		t.Errorf("an unrelated failure was answered with %d requests to the node", n)
	}
}

// A NAMED NODE IS WHERE THE VALUES ARE READ, and nothing of this node's is
// touched: a vendor command's -api names the node its credentials are written
// through, possibly from a machine that is not one, where reaching the store
// the Tier A file describes would start a broker on a directory nobody meant —
// creating a fleet store there — and read every value the named node holds as
// unset.
func TestANamedNodeIsReadAndTheLocalStoreIsLeftAlone(t *testing.T) {
	t.Parallel()
	node := newRevealNode(t, map[string]string{"PROXY_URL": "http://proxy.example.com:3128"})
	cfg := bootstrapWithKeyring(t, "k1")
	// WHAT THE STOPPED-ENGINE ROUTE WOULD LEAVE: the database lock it asks,
	// and the broker's store directory.
	touched := []string{
		filepath.Join(filepath.Dir(cfg), "index.db.lock"),
		filepath.Join(filepath.Dir(cfg), "stream"),
	}

	env, _, err := resolveWith(t, tokenEnviron("ops-token"), cfg, node.server.URL,
		llmEntryReads(readingCompany(t), []string{"default"}), &bytes.Buffer{})
	if err != nil {
		t.Fatalf("resolve through a named node: %v", err)
	}
	if got := env.Value("${PROXY_URL}"); got != "http://proxy.example.com:3128" {
		t.Errorf("${PROXY_URL} = %q, not the named node's value", got)
	}
	for _, path := range touched {
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s was touched by a run that named a node: %v", path, err)
		}
	}
}

// A SLACK RUN WITH NO SEAT TO PROVISION OPENS NO STORE. What it reads of the
// deployment is Tier A's address alone, and it mints into no name a store
// could hold, so opening the store — or beside a running engine, asking it —
// would be a read for a run that uses none. A route it decided says so on the
// run's first line, whichever store it reached, so no such line is printed;
// and what a route with the engine stopped leaves — the database lock it asks,
// and the broker's store directory — is not there.
//
// Mutation: decide the route, or ask the store which names it holds, before
// the empty plan's return, and the route line is printed.
func TestASlackRunWithNoSeatToProvisionOpensNoStore(t *testing.T) {
	t.Parallel()
	cfg := bootstrapWithKeyring(t, "k1")
	boot, err := loadBootstrapForStore(cfg)
	if err != nil {
		t.Fatal(err)
	}
	companyPath := filepath.Join(t.TempDir(), "company.yaml")
	if err := os.WriteFile(companyPath, []byte(builderCompany), 0o600); err != nil {
		t.Fatal(err)
	}

	out, errs, err := cliIn(t, noEnviron, "slack", "provision", companyPath, "-print",
		"-config", cfg, "-ledger", filepath.Join(t.TempDir(), "ledger.json"))
	if err != nil {
		t.Fatalf("slack provision of a company with no Slack seat: %v (%s)", err, errs)
	}
	for _, line := range []string{"checking which names", "resolving ${VAR}"} {
		if strings.Contains(out, line) {
			t.Errorf("slack provision decided a route, though it reads nothing:\n%s", out)
		}
	}
	for _, path := range []string{boot.Store.Path + ".lock", boot.Stream.StoreDir} {
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("slack provision reached the store (%s), though it resolves "+
				"nothing: %v", path, err)
		}
	}
}

// seatsNodeTierA is the Tier A of a node that holds no data: a seats node,
// whose store is scratch and whose broker is a leaf of the data nodes'.
func seatsNodeTierA(t *testing.T, dir string) string {
	t.Helper()
	body := fmt.Sprintf("node:\n  id: agent-1\n  roles: [seats]\nstore:\n  path: %s\n"+
		"  scratch: true\nstream:\n  leaf:\n    urls: [\"nats-leaf://data-a.example.com:7422\"]\n"+
		"coordination:\n  type: embedded-kv\n"+fixtureKeyring,
		filepath.Join(dir, "index.db"))
	cfg := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(cfg, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return cfg
}

// A SEATS NODE SIGNS ITS SEATS IN THROUGH A NODE THAT HOLDS DATA.
//
// The logins `crewlet llm` makes are made on the node whose seats run on
// them, and a seats node holds no data: its store is scratch, so there is
// nothing of the company's to open there, and it used to refuse every `crewlet
// llm` command — as `crewlet secrets`, which writes. -api names the data node:
// doctor reads the entry's stored token through it, and a captured token is
// stored there. Without it the command says to name one, and opens nothing.
func TestASeatsNodeReadsAndStoresThroughTheDataNodeItNames(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	cfg := seatsNodeTierA(t, dir)
	node := newRevealNode(t, map[string]string{
		"FLEET_TOKEN": "fleet-headless-token",
		"OTHER_TOKEN": "another-entry's-token",
	})
	signsInFrom := func(key, name string) string {
		return strings.Replace(healthyDoctorEntry(t, key, "m"),
			"auth: {token: fake-headless-token}", `auth: {token: "${`+name+`}"}`, 1)
	}
	company := writeFile(t, dir, "company.yaml", "name: Acme\nproviders:\n  sandbox: {fake: true}\n  llm:\n"+
		signsInFrom("claude", "FLEET_TOKEN")+signsInFrom("other", "OTHER_TOKEN")+
		"roles:\n  - name: CEO\n    handle: ceo\n    kind: human\n")
	base := []string{"doctor", "claude", "-no-smoke", "-company", company, "-config", cfg}

	var out, notes bytes.Buffer
	if err := runLLM(tokenEnviron("ops-token"), base, &out, &notes); err == nil ||
		!strings.Contains(err.Error(), "store.scratch") || !strings.Contains(err.Error(), "-api") {
		t.Fatalf("a seats node with no node named = %v, want a refusal naming store.scratch and -api", err)
	}

	out.Reset()
	if err := runLLM(tokenEnviron("ops-token"), append(base, "-api", node.server.URL),
		&out, &notes); err != nil {
		t.Fatalf("doctor through the named data node: %v\n%s", err, out.String())
	}
	if !strings.Contains(out.String(), "problems      : none\n") {
		t.Errorf("the entry's stored token did not reach doctor through the named node:\n%s", out.String())
	}
	// THE ENTRY IT NAMES, and not the other one's token beside it.
	if !slices.Equal(node.reveals(), []string{"FLEET_TOKEN"}) {
		t.Errorf("revealed %v, want the token the entry it names reads", node.reveals())
	}

	target, _, err := storeLLMSecret(t.Context(),
		llmStoreAt(t, tokenEnviron("ops-token"), "login", "-config", cfg, "-api", node.server.URL),
		"CLAUDE_CODE_OAUTH_TOKEN", "sk-ant-oat01-captured")
	if err != nil {
		t.Fatalf("store a captured token through the named node: %v", err)
	}
	if stored, _ := node.holds("CLAUDE_CODE_OAUTH_TOKEN"); stored != "sk-ant-oat01-captured" ||
		!strings.Contains(target.where, node.server.URL) {
		t.Errorf("the captured token went to %q holding %q, want the named node", target.where, stored)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.Name() != "config.yaml" && e.Name() != "company.yaml" {
			t.Errorf("a seats node's run left %s behind: its scratch store was opened", e.Name())
		}
	}
}

// EVERY COMMAND THAT READS THE COMPANY TAKES -api. Each one's values are read
// through a node when its own cannot reach the company's store — a seats node,
// a machine that is not a node, or a cluster member whose engine is stopped —
// and both the scratch refusal and the cluster member's name -api as the way
// there, so a command without the flag was told to use one it would refuse.
func TestEveryCommandThatReadsTheCompanyTakesAPI(t *testing.T) {
	t.Parallel()
	for _, command := range [][]string{
		{"confluence", "import"}, {"confluence", "resync"}, {"confluence", "provision"},
		{"github", "provision"}, {"jira", "provision"}, {"gitlab", "provision"},
		{"mattermost", "doctor"}, {"slack", "provision"},
		{"llm", "list"},
	} {
		var out, errs bytes.Buffer
		err := run(append(command, "-api", "http://127.0.0.1:1", "-h"), &out, &errs)
		if !errors.Is(err, flag.ErrHelp) {
			t.Errorf("crewlet %s -api URL -h = %v, want the help: it does not take -api\n%s",
				strings.Join(command, " "), err, errs.String())
		}
	}
}

package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/store"
)

// A PROVISIONING RUN READS THE SECRET STORE, NOT JUST THE ENVIRONMENT.
//
// The chain used to be config.EnvOnly(), so every Tier B ${VAR} an operator
// had already put in the store resolved to the empty string. For a GitLab
// signing secret that is not merely a missing value. Empty is the signal to
// MINT, so the run replaced a working webhook secret at the third-party app
// with a fresh one and broke every delivery in flight until the config
// caught up.
func TestAProvisioningRunResolvesThroughTheSecretStore(t *testing.T) {
	t.Parallel()
	cfg := bootstrapWithKeyring(t, "k1")
	if _, errs, err := secretsCmd(t, cfg, "set", "GITLAB_SIGNING_SECRET",
		"-value", "whsec_c3RvcmVkLXNpZ25pbmcta2V5LW9mLTMyLWJ5dGVzIQ=="); err != nil {
		t.Fatalf("seed the store: %v (%s)", err, errs)
	}

	var notes bytes.Buffer
	env, _, closeEnv, err := companyResolver(t.Context(), noEnviron, nil, cfg, "", &notes)
	if err != nil {
		t.Fatalf("companyResolver: %v", err)
	}
	defer closeEnv()

	got := env.Value("${GITLAB_SIGNING_SECRET}")
	if got != "whsec_c3RvcmVkLXNpZ25pbmcta2V5LW9mLTMyLWJ5dGVzIQ==" {
		t.Fatalf("the stored secret did not reach the run: %q", got)
	}
	if notes.Len() > 0 {
		t.Errorf("a run that DID read the store announced that it did not: %q",
			notes.String())
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

	env, _, closeEnv, err := companyResolver(t.Context(), noEnviron, nil, cfg, "", &bytes.Buffer{})
	if err != nil {
		t.Fatalf("companyResolver: %v", err)
	}
	defer closeEnv()

	if got := env.Lookup("CONFLUENCE_TOKEN"); got != "the-rotated-one" {
		t.Fatalf("a stale export shadowed the rotated secret: %q", got)
	}
}

// THE OPERATOR'S OWN CREDENTIAL IS NOT THE COMPANY'S, and does not come
// from the store.
//
// A GitLab admin PAT carries `api` scope over the whole group, and the store
// is replicated to every node holding the keyring. Crewlet never persists
// one; reading one back would imply it may be kept there — which is how the
// most powerful credential in the deployment ends up in the shared table
// beside the seat tokens it exists to mint.
func TestAnOperatorCredentialIsNotReadFromTheStore(t *testing.T) {
	cfg := bootstrapWithKeyring(t, "k1")
	if _, errs, err := secretsCmd(t, cfg, "set", "GITLAB_ADMIN_TOKEN",
		"-value", "stored-admin-pat"); err != nil {
		t.Fatalf("seed the store: %v (%s)", err, errs)
	}
	if got := operatorCredential("GITLAB_ADMIN_TOKEN"); got != "" {
		t.Fatalf("an operator credential was read out of the secret store: %q", got)
	}
	t.Setenv("GITLAB_ADMIN_TOKEN", "  exported-admin-pat  ")
	if got := operatorCredential("GITLAB_ADMIN_TOKEN"); got != "exported-admin-pat" {
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
	env, _, closeEnv, err := companyResolver(t.Context(), noEnviron, nil, missing, "", &notes)
	if err != nil {
		t.Fatalf("a missing bootstrap is a supported deployment: %v", err)
	}
	defer closeEnv()

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
	_, _, _, err := companyResolver(t.Context(), noEnviron, nil, path, "", &notes)
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
	if _, _, _, err := companyResolver(t.Context(), noEnviron, nil, path, "", &notes); err == nil {
		t.Fatal("a bootstrap that cannot be read was treated as no bootstrap, " +
			"so a configured store silently became a stale environment")
	}
	if notes.Len() > 0 {
		t.Errorf("a failed run announced a fallback it did not take: %q", notes.String())
	}
}

// revealNode is a running node's /secrets surface as a read through it meets
// it: a listing of the names the fleet holds, and each value behind a reveal.
type revealNode struct {
	server *httptest.Server
	held   map[string]string

	mu sync.Mutex

	// refuse, when set, is the status every reveal is answered with, and
	// its body the engine's own refusal of a machine token.
	refuse   int
	revealed []string
	tokens   []string
	stored   map[string]string
}

func newRevealNode(t *testing.T, held map[string]string) *revealNode {
	t.Helper()
	n := &revealNode{held: held}
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
			_, _ = fmt.Fprint(w, `{"updated_by":"token:ops","updated_by_kind":"operator"}`)
			return
		}
		if r.Method != http.MethodGet {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		if r.URL.Path == "/secrets" {
			var rows []map[string]string
			for name := range n.held {
				rows = append(rows, map[string]string{"name": name, "key_id": "k1"})
			}
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
		refuse := n.refuse
		n.mu.Unlock()
		if refuse != 0 {
			w.WriteHeader(refuse)
			_, _ = fmt.Fprint(w, `{"error":"token_refused","detail":"a person must be present"}`)
			return
		}
		value, held := n.held[name]
		if !held {
			w.WriteHeader(http.StatusNotFound)
			_, _ = fmt.Fprint(w, `{"error":"not_found"}`)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"name": name, "value": value})
	}))
	t.Cleanup(n.server.Close)
	return n
}

// boot is a Tier A whose API address is this node's.
func (n *revealNode) boot(t *testing.T) *config.Bootstrap {
	t.Helper()
	host, port, err := net.SplitHostPort(strings.TrimPrefix(n.server.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	p, err := strconv.Atoi(port)
	if err != nil {
		t.Fatal(err)
	}
	return bootWithAPI(t, host, p, "")
}

func (n *revealNode) reveals() []string {
	n.mu.Lock()
	defer n.mu.Unlock()
	out := slices.Clone(n.revealed)
	slices.Sort(out)
	return out
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

// errLockedStore is the error a store a running engine holds opens with.
var errLockedStore = fmt.Errorf("open store: %w: /var/lib/crewlet/index.db is held by "+
	"pid 41 on host-a since 2026-01-01T00:00:00Z", store.ErrLocked)

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

// BESIDE A RUNNING ENGINE THE COMPANY'S VALUES ARE READ THROUGH IT — AND A
// COMMAND REVEALS ONLY WHAT IT READS.
//
// The engine holds its store for as long as it runs, and the company's
// stored values are the fleet's then, inside the engine's own process. Every
// command that reads the company — `crewlet llm` and the vendor commands —
// used to stop at the lock, which made `crewlet llm login` impossible beside
// the seats it signs in. And each value comes back through a REVEAL, logged
// by name against the caller, so a command asks for what it reads and
// nothing else: one entry's sign-in for `crewlet llm login`, not its dotted
// neighbour's, nor every vendor's tokens; a GitLab provision's instance and
// signing key, not the engine's GitLab token; the seat credentials a reader
// consults, not the rest of the block they sit in, a unit's credential no
// seat inherits, or a tool server's that only shares the vendor's name. A
// name the fleet does not hold is left to the environment without a request.
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
			"crewlet mattermost provision", mattermostProvisionReads(integrations.Mattermost),
			[]string{"MATTERMOST_URL"},
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
			var notes bytes.Buffer
			values, err := storedThroughTheRunningNode(t.Context(), tokenEnviron("ops-token"),
				node.boot(t), "crewlet.yaml", tc.reads, errLockedStore, &notes)
			if err != nil {
				t.Fatalf("a running engine's store was refused rather than read through it: %v", err)
			}
			if !slices.Equal(node.reveals(), tc.want) {
				t.Errorf("revealed %v, want only what the command reads, %v",
					node.reveals(), tc.want)
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
			if !strings.Contains(notes.String(), node.server.URL) ||
				!strings.Contains(notes.String(), fmt.Sprintf(": %d of", len(tc.want))) {
				t.Errorf("the run did not say which chain it read, and how much: %q", notes.String())
			}
		})
	}
}

// A READ BEYOND WHAT THE COMMAND REVEALS FIRST IS STILL THE FLEET'S VALUE,
// revealed as it is made and said: a command whose list missed a field it
// reads resolves what the engine would, rather than reading the stored value
// as unset — which, for a key a provisioner mints when it resolves empty,
// replaces a working one. And a name the fleet does not hold costs no
// request.
func TestAReadBeyondTheScopeIsStillTheFleetsValue(t *testing.T) {
	t.Parallel()
	node := scopeNode(t)
	company, _ := scopeCompany(t)
	var notes bytes.Buffer
	values, err := storedThroughTheRunningNode(t.Context(), tokenEnviron("ops-token"),
		node.boot(t), "crewlet.yaml", gitlabProvisionReads(company.Integrations.GitLab),
		errLockedStore, &notes)
	if err != nil {
		t.Fatal(err)
	}
	resolver := config.WithStore(values)
	if got := resolver.Value("${GITLAB_READ_TOKEN}"); got != "v-GITLAB_READ_TOKEN" {
		t.Errorf("a read beyond the scope resolved to %q, not the fleet's value", got)
	}
	if !strings.Contains(notes.String(), "${GITLAB_READ_TOKEN}") ||
		!strings.Contains(notes.String(), "as it was read") {
		t.Errorf("the reveal beyond the scope was not said: %q", notes.String())
	}
	resolver.Value("${GITLAB_READ_TOKEN}")
	resolver.Value("${NOT_HELD_BY_THE_FLEET}")
	want := []string{"GITLAB_READ_TOKEN", "GITLAB_SIGNING_SECRET", "GITLAB_URL"}
	if !slices.Equal(node.reveals(), want) {
		t.Errorf("revealed %v, want %v: once each, and nothing the fleet does not hold",
			node.reveals(), want)
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
			values, err := storedThroughTheRunningNode(t.Context(), tokenEnviron("ops-token"),
				node.boot(t), "crewlet.yaml", gitlabProvisionReads(company.Integrations.GitLab),
				errLockedStore, &bytes.Buffer{})
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

// A GITLAB PROVISION BESIDE A RUNNING ENGINE KEEPS THE KEY ITS HOOK SIGNS
// WITH, reading it through the node — and reveals the two fields it reads
// and nothing else, none of them as a read beyond its list.
func TestAGitLabProvisionThroughANodeRevealsOnlyItsOwnFields(t *testing.T) {
	t.Parallel()
	node := scopeNode(t)
	company := writeFile(t, t.TempDir(), "company.yaml", strings.NewReplacer(
		"url: https://gitlab.example.com", `url: "${GITLAB_URL}"`,
		"    enabled: true\n", "    enabled: true\n    token: \"${GITLAB_READ_TOKEN}\"\n",
	).Replace(gitlabCompanyDoc))
	var out, errs bytes.Buffer
	err := runGitLabProvision(tokenEnviron("ops-token"), []string{company, "-dry-run",
		"-config", bootstrapWithKeyring(t, "k1"), "-api", node.server.URL,
		"-public-url", "https://crewlet.example.com"}, &out, &errs)
	if err != nil {
		t.Fatalf("gitlab provision -dry-run through a node: %v\n%s", err, errs.String())
	}
	if !strings.Contains(out.String(), "reused — the configured one already resolves") {
		t.Errorf("the stored signing key did not reach the plan, which mints over it:\n%s", out.String())
	}
	if want := []string{"GITLAB_SIGNING_SECRET", "GITLAB_URL"}; !slices.Equal(node.reveals(), want) {
		t.Errorf("revealed %v, want the two fields the command reads, %v", node.reveals(), want)
	}
	if strings.Contains(out.String(), "as it was read") {
		t.Errorf("the command read beyond the fields it declares:\n%s", out.String())
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
	env, boot, closeEnv, err := companyResolver(t.Context(), tokenEnviron("ops-token"),
		[]string{"GITLAB_SIGNING_SECRET"}, missing, node.server.URL, &notes)
	if err != nil {
		t.Fatalf("a named node with no Tier A here: %v", err)
	}
	defer closeEnv()
	if got := env.Lookup("GITLAB_SIGNING_SECRET"); got != "v-GITLAB_SIGNING_SECRET" {
		t.Errorf("the signing key resolved to %q, not the named node's", got)
	}
	if boot != nil || strings.Contains(notes.String(), "environment only") {
		t.Errorf("the run read the environment alone (Tier A %v): %q", boot, notes.String())
	}
}

// A STOPPED NODE NAMES WHAT IT CANNOT READ. Its own table holds only what
// was stored while the engine was stopped — the engine moves those rows onto
// the fleet at its next start — so a value the command reads that neither
// the table nor the environment holds may be one the fleet has, and the run
// resolves it as unset: for a signing key, the signal to mint over a working
// one. Only what THIS command reads is named, and a run that has everything
// it reads says nothing.
func TestAStoppedNodeNamesTheValuesItCannotRead(t *testing.T) {
	cfg := bootstrapWithKeyring(t, "k1")
	if _, errs, err := secretsCmd(t, cfg, "set", "GITLAB_URL",
		"-value", "https://gitlab.example.com"); err != nil {
		t.Fatalf("seed the store: %v (%s)", err, errs)
	}
	t.Setenv("MATTERMOST_URL", "https://chat.example.com")
	company, organization := scopeCompany(t)
	integrations := company.Integrations

	for _, tc := range []struct {
		command      string
		reads        []string
		unread, read []string
	}{
		{
			"crewlet gitlab provision", gitlabProvisionReads(integrations.GitLab),
			[]string{"${GITLAB_SIGNING_SECRET}"},
			[]string{"${GITLAB_URL}", "${GITLAB_READ_TOKEN}"},
		},
		{
			"crewlet mattermost doctor", mattermostDoctorReads(integrations.Mattermost, organization),
			[]string{"${MATTERMOST_BOT_CEO}", "${MATTERMOST_BOT_SWE}"},
			[]string{"${MATTERMOST_URL}"},
		},
		{"crewlet mattermost provision", mattermostProvisionReads(integrations.Mattermost), nil, nil},
	} {
		var notes bytes.Buffer
		_, _, closeEnv, err := companyResolver(t.Context(), noEnviron, tc.reads, cfg, "", &notes)
		if err != nil {
			t.Fatalf("%s: companyResolver on a stopped node: %v", tc.command, err)
		}
		closeEnv()
		if len(tc.unread) == 0 {
			if notes.Len() > 0 {
				t.Errorf("%s reads nothing a stopped node lacks, and said %q",
					tc.command, notes.String())
			}
			continue
		}
		for _, want := range append([]string{"-api", "index.db"}, tc.unread...) {
			if !strings.Contains(notes.String(), want) {
				t.Errorf("%s: the note omits %q: %q", tc.command, want, notes.String())
			}
		}
		for _, answered := range tc.read {
			if strings.Contains(notes.String(), answered) {
				t.Errorf("%s: the note names %s, which the table, the environment or "+
					"another command answers: %q", tc.command, answered, notes.String())
			}
		}
	}
}

// A READ THE NODE REFUSES SAYS BOTH WHY, AND WHAT THE READ NEEDS.
//
// A machine token is refused every reveal, whatever it carries, and the
// command has no other way to the values while the engine runs. So the lock
// and the refusal are both in the error, with the token a reveal takes — and
// stopping the engine is NOT offered: a stopped node's own table is not where
// the fleet's values are, and reading it instead is the stale resolution that
// mints a webhook secret over a working one.
func TestARefusedReadThroughTheNodeSaysBothWhyAndWhatItTakes(t *testing.T) {
	t.Parallel()
	node := newRevealNode(t, map[string]string{"PROXY_URL": "http://proxy.example.com:3128"})
	node.refuse = http.StatusForbidden

	_, err := storedThroughTheRunningNode(t.Context(), tokenEnviron("pat-of-a-person"),
		node.boot(t), "crewlet.yaml", llmEntryReads(readingCompany(t), []string{"default"}),
		errLockedStore, &bytes.Buffer{})
	if err == nil {
		t.Fatal("a refused reveal resolved anyway, so a stored value would read as unset")
	}
	if !errors.Is(err, store.ErrLocked) {
		t.Fatalf("the lock's sentinel was lost: %v", err)
	}
	for _, want := range []string{"pid 41", "crewlet.yaml", "403", apiTokenEnv, "Tier A"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal omits %q: %v", want, err)
		}
	}
	if strings.Contains(err.Error(), "stop `crewlet run`") {
		t.Errorf("the refusal offers stopping the engine, whose own table does not "+
			"hold the fleet's values: %v", err)
	}
}

// ONLY A LOCKED STORE IS READ THROUGH THE NODE. Any other failure is a broken
// node, and an HTTP attempt would replace its message with a connection
// refused.
func TestOnlyALockedStoreIsReadThroughTheNode(t *testing.T) {
	t.Parallel()
	node := newRevealNode(t, map[string]string{"PROXY_URL": "http://proxy.example.com:3128"})
	other := errors.New("the keyring cannot open this store")

	_, err := storedThroughTheRunningNode(t.Context(), tokenEnviron("ops-token"),
		node.boot(t), "crewlet.yaml", llmEntryReads(readingCompany(t), []string{"default"}),
		other, &bytes.Buffer{})
	if !errors.Is(err, other) || err.Error() != other.Error() {
		t.Fatalf("an unrelated failure was not returned as it was: %v", err)
	}
	if len(node.tokens) != 0 {
		t.Errorf("an unrelated failure was answered with %d requests to the node", len(node.tokens))
	}
}

// A NAMED NODE IS WHERE THE VALUES ARE READ, and the local path is never
// opened: a vendor command's -api names the node its credentials are written
// through, possibly from a machine that is not one, where opening the Tier A
// store path would create an empty database and resolve every stored value as
// unset.
func TestANamedNodeIsReadAndTheLocalStoreIsLeftAlone(t *testing.T) {
	t.Parallel()
	node := newRevealNode(t, map[string]string{"PROXY_URL": "http://proxy.example.com:3128"})
	cfg := bootstrapWithKeyring(t, "k1")
	storePath := filepath.Join(filepath.Dir(cfg), "index.db")

	env, _, closeEnv, err := companyResolver(t.Context(), tokenEnviron("ops-token"),
		llmEntryReads(readingCompany(t), []string{"default"}), cfg, node.server.URL, &bytes.Buffer{})
	if err != nil {
		t.Fatalf("companyResolver through a named node: %v", err)
	}
	defer closeEnv()
	if got := env.Value("${PROXY_URL}"); got != "http://proxy.example.com:3128" {
		t.Errorf("${PROXY_URL} = %q, not the named node's value", got)
	}
	if _, err := os.Stat(storePath); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the local store at %s was opened for a run that named a node: %v",
			storePath, err)
	}
}

// SLACK RESOLVES NOTHING, so it opens no store for it. What it reads of the
// deployment is Tier A's address alone, and resolving the company's values
// there — opening the store, or beside a running engine revealing them
// through it — would be a credential read for a run that uses none.
func TestSlackProvisionReadsTierAAndNoStore(t *testing.T) {
	t.Parallel()
	cfg := bootstrapWithKeyring(t, "k1")
	storePath := filepath.Join(filepath.Dir(cfg), "index.db")
	companyPath := filepath.Join(t.TempDir(), "company.yaml")
	if err := os.WriteFile(companyPath, []byte(builderCompany), 0o600); err != nil {
		t.Fatal(err)
	}

	var out, errs bytes.Buffer
	err := run([]string{"slack", "provision", companyPath, "-print", "-config", cfg,
		"-ledger", filepath.Join(t.TempDir(), "ledger.json")}, &out, &errs)
	if err != nil {
		t.Fatalf("slack provision of a company with no Slack seat: %v (%s)", err, errs.String())
	}
	if _, err := os.Stat(storePath); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("slack provision opened the store at %s, though it resolves nothing: %v",
			storePath, err)
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
	company := writeFile(t, dir, "company.yaml", "name: Acme\nproviders:\n  llm:\n"+
		signsInFrom("claude", "FLEET_TOKEN")+signsInFrom("other", "OTHER_TOKEN")+
		"roles:\n  - name: CEO\n    handle: ceo\n    llm: claude\n")
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

	where, err := storeLLMSecret(t.Context(), tokenEnviron("ops-token"), cfg, node.server.URL,
		"CLAUDE_CODE_OAUTH_TOKEN", "sk-ant-oat01-captured")
	if err != nil {
		t.Fatalf("store a captured token through the named node: %v", err)
	}
	node.mu.Lock()
	stored := node.stored["CLAUDE_CODE_OAUTH_TOKEN"]
	node.mu.Unlock()
	if stored != "sk-ant-oat01-captured" || !strings.Contains(where, node.server.URL) {
		t.Errorf("the captured token went to %q holding %q, want the named node", where, stored)
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
// through a node when its own has no store of the company's — a seats node,
// or a machine that is not a node — and both the scratch refusal and the
// stopped-node note name -api as the way there, so a command without the
// flag was told to use one it would refuse.
func TestEveryCommandThatReadsTheCompanyTakesAPI(t *testing.T) {
	t.Parallel()
	for _, command := range [][]string{
		{"confluence", "import"}, {"confluence", "resync"}, {"confluence", "provision"},
		{"github", "provision"}, {"jira", "provision"}, {"gitlab", "provision"},
		{"mattermost", "provision"}, {"mattermost", "doctor"}, {"llm", "list"},
	} {
		var out, errs bytes.Buffer
		err := run(append(command, "-api", "http://127.0.0.1:1", "-h"), &out, &errs)
		if !errors.Is(err, flag.ErrHelp) {
			t.Errorf("crewlet %s -api URL -h = %v, want the help: it does not take -api\n%s",
				strings.Join(command, " "), err, errs.String())
		}
	}
}

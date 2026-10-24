package main

import (
	"bytes"
	"encoding/json"
	"errors"
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

	// refuse, when set, is the status every reveal is answered with, and
	// its body the engine's own refusal of a machine token.
	refuse int

	mu       sync.Mutex
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
		n.mu.Unlock()
		if n.refuse != 0 {
			w.WriteHeader(n.refuse)
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

// BESIDE A RUNNING ENGINE THE COMPANY'S VALUES ARE READ THROUGH IT.
//
// The engine holds its store for as long as it runs, and the company's
// stored values are the fleet's then, inside the engine's own process. Every
// command that reads the company — `crewlet llm` and the vendor commands —
// used to stop at the lock, which made `crewlet llm login` impossible beside
// the seats it signs in. And each value comes back through a REVEAL, so only
// what the company reads is asked for: a stored name it does not read is
// never revealed, and a name it reads that the fleet does not hold is left to
// the environment without a request.
func TestARunningEngineHandsOverOnlyTheValuesTheCompanyReads(t *testing.T) {
	t.Parallel()
	node := newRevealNode(t, map[string]string{
		"PROXY_URL":               "http://proxy.example.com:3128",
		"CLAUDE_CODE_OAUTH_TOKEN": "sk-ant-oat01-fixture",
		"GITLAB_TOKEN_SWE":        "glpat-not-read-by-this-company",
	})

	var notes bytes.Buffer
	values, err := storedThroughTheRunningNode(t.Context(), tokenEnviron("ops-token"),
		node.boot(t), "crewlet.yaml", readingCompany(t), errLockedStore, &notes)
	if err != nil {
		t.Fatalf("a running engine's store was refused rather than read through it: %v", err)
	}
	resolver := config.WithStore(values)
	if got := resolver.Value("${PROXY_URL}"); got != "http://proxy.example.com:3128" {
		t.Errorf("a ${VAR} the company names resolved to %q, not the fleet's value", got)
	}
	if got := resolver.Lookup("CLAUDE_CODE_OAUTH_TOKEN"); got != "sk-ant-oat01-fixture" {
		t.Errorf("the token the entry falls back to resolved to %q, not the fleet's value", got)
	}
	if want := []string{"CLAUDE_CODE_OAUTH_TOKEN", "PROXY_URL"}; !slices.Equal(node.reveals(), want) {
		t.Errorf("revealed %v, want only what the company reads and the fleet holds, %v",
			node.reveals(), want)
	}
	for _, token := range node.tokens {
		if token != "Bearer ops-token" {
			t.Errorf("a request carried %q, not the invocation's own credential", token)
		}
	}
	if !strings.Contains(notes.String(), node.server.URL) {
		t.Errorf("the run did not say which chain it read: %q", notes.String())
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
		node.boot(t), "crewlet.yaml", readingCompany(t), errLockedStore, &bytes.Buffer{})
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
		node.boot(t), "crewlet.yaml", readingCompany(t), other, &bytes.Buffer{})
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
		readingCompany(t), cfg, node.server.URL, &bytes.Buffer{})
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
	node := newRevealNode(t, map[string]string{"FLEET_TOKEN": "fleet-headless-token"})
	entry := strings.Replace(healthyDoctorEntry(t, "claude", "m"),
		"auth: {token: fake-headless-token}", `auth: {token: "${FLEET_TOKEN}"}`, 1)
	company := writeFile(t, dir, "company.yaml", "name: Acme\nproviders:\n  llm:\n"+entry+
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
	if !slices.Equal(node.reveals(), []string{"FLEET_TOKEN"}) {
		t.Errorf("revealed %v, want the token the entry reads", node.reveals())
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

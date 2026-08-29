package config

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/envref"
	"github.com/crewlet/crewlet/internal/org"
)

// repoRoot is where the shipped examples and the docs live, relative to
// this package.
const repoRoot = "../.."

// The shipped examples and the docs are a promise: someone copies them and
// runs them. Nothing else stops them drifting away from what the loader
// accepts, so this suite is what holds them to the models — the Python
// engine keeps the same test for the same reason.
func TestShippedCompanyExampleLoads(t *testing.T) {
	t.Parallel()
	data, err := os.ReadFile(filepath.Join(repoRoot, "examples", "nimbus.company.yaml"))
	if err != nil {
		t.Skipf("the example tree is not in this checkout: %v", err)
	}
	cfg, err := ParseCompany(data)
	if err != nil {
		t.Fatalf("examples/nimbus.company.yaml no longer loads:\n%v", err)
	}
	o, err := cfg.Organization()
	if err != nil {
		t.Fatalf("the example does not build an org:\n%v", err)
	}

	var agents, humans int
	for r := range o.AllRoles() {
		if r.IsHuman() {
			humans++
			continue
		}
		agents++
	}
	if agents == 0 || humans == 0 {
		t.Fatalf("the example should model both kinds of seat: %d agents, %d humans", agents, humans)
	}

	// Every credential in a committed example must be a reference. A
	// literal here would be published, permanently, in git history and in
	// every sdist built from it.
	for key, provider := range cfg.Providers.LLM {
		for i, raw := range provider.APIKeys {
			if _, whole := wholeRef(raw); !whole {
				t.Fatalf("providers.llm.%s.api_keys[%d] is a literal: %q", key, i, raw)
			}
		}
	}
}

// The quickstart's config is the canonical minimal company — the one a
// founder types out on their first day. It lives inline in markdown so it
// can be read in place, which means nothing but this stops it drifting.
func TestQuickstartCompanyLoads(t *testing.T) {
	t.Parallel()
	block := quickstartCompanyBlock(t)
	cfg, err := ParseCompany([]byte(block))
	if err != nil {
		t.Fatalf("the quickstart's company config no longer loads:\n%v", err)
	}
	o, err := cfg.Organization()
	if err != nil {
		t.Fatalf("the quickstart does not build an org:\n%v", err)
	}

	// Escalation has to terminate at a person: the page teaches a human
	// seat at the top of the chart, addressable and never executable.
	var humans []*org.Role
	for r := range o.AllRoles() {
		if r.IsHuman() {
			humans = append(humans, r)
		}
	}
	if len(humans) != 1 {
		t.Fatalf("the quickstart should model exactly one human seat, found %d", len(humans))
	}
	if humans[0].Contact.IsEmpty() {
		t.Fatal("a human seat with no contact identity is unreachable")
	}
	if len(humans[0].Manages) == 0 {
		t.Fatal("the founder seat should manage the top agent")
	}

	// With no integrations there is no inbound work, and the page promises
	// a turn within minutes — a schedule is the only thing that can
	// deliver one.
	var scheduled bool
	for r := range o.AllRoles() {
		for _, s := range r.Schedules {
			if s.IsEnabled() {
				scheduled = true
			}
		}
	}
	if !scheduled {
		t.Fatal("no seat carries an enabled schedule, so nothing would ever run")
	}

	// The page tells the reader handles are effectively permanent and to
	// set them explicitly; the example has to model that rather than rely
	// on derivation from a name they may rename.
	for r := range o.AllRoles() {
		if r.IsAgent() && r.DeclaredHandle == "" {
			t.Fatalf("agent seat %q has no explicit handle", r.Name)
		}
	}
}

// A reader follows the page top to bottom, so every ${VAR} the config
// references must appear in an export line somewhere on it. An unset one
// resolves to the empty string and fails much later, somewhere else.
func TestQuickstartExportsEveryVariableItReferences(t *testing.T) {
	t.Parallel()
	page := quickstartText(t)
	cfg, err := ParseCompany([]byte(quickstartCompanyBlock(t)))
	if err != nil {
		t.Fatal(err)
	}
	exported := map[string]bool{}
	for _, m := range regexp.MustCompile(`export\s+([A-Za-z_][A-Za-z0-9_]*)=`).FindAllStringSubmatch(page, -1) {
		exported[m[1]] = true
	}
	for _, name := range ReferencedNames(cfg) {
		if !exported[name] {
			t.Fatalf("the quickstart config references ${%s} but the page never exports it", name)
		}
	}
}

// THE SHIPPED EXAMPLE LOADS, which is the whole point of shipping one.
//
// It was the Python-era shape for the length of the rewrite — a `providers:`
// block naming a Pulsar broker and a PostgreSQL DSN, which this engine runs
// on neither — and the test here pinned the refusal so the break was a
// decision with a name on it. The file is now the Go shape, so what is
// pinned is that it stays loadable: it is what the quickstart, both
// bootstrap scripts and every integration walkthrough tell a founder to run,
// and nothing else notices when it stops working.
func TestBootstrapExampleLoads(t *testing.T) {
	t.Parallel()
	data, err := os.ReadFile(filepath.Join(repoRoot, "examples", "nimbus.config.yaml"))
	if err != nil {
		t.Skipf("the example tree is not in this checkout: %v", err)
	}
	// EVERY REFERENCE ANSWERED, so what is under test is the SHAPE rather
	// than whose shell happens to have the variables exported. A file that
	// only loads on a machine with the right environment is not a shipped
	// example.
	answers := MapSource{}
	for _, name := range envref.Names(string(data)) {
		answers[name] = "set-for-this-test"
	}
	cfg, err := ParseBootstrap(data, NewResolver(answers))
	if err != nil {
		t.Fatalf("the shipped Tier A example does not load: %v", err)
	}
	// The single-binary defaults it exists to demonstrate: no broker to
	// operate and no DSN to point anywhere.
	if cfg.Stream.Type != StreamEmbedded {
		t.Errorf("stream = %q, want the embedded default", cfg.Stream.Type)
	}
	if cfg.Stream.StoreDir == "" {
		t.Error("the embedded stream has no store_dir, so nothing published " +
			"survives a restart — not what a shipped example should show")
	}
	if cfg.Store.Path == "" {
		t.Error("no store path")
	}
	if cfg.Coordination.Type != CoordinationLocal {
		t.Errorf("coordination = %q, want local for a single node", cfg.Coordination.Type)
	}
}

// Tier A in its own shape, as the docs will carry it.
func TestBootstrapExampleShapeLoads(t *testing.T) {
	t.Parallel()
	cfg, err := ParseBootstrap([]byte(`
logging:
  level: info

node:
  id: "${CREWLET_NODE_ID}"
  roles: [ingress, seats, workers]
  labels:
    zone: eu

store:
  path: /var/lib/crewlet/crewlet.db

stream:
  type: embedded
  store_dir: /var/lib/crewlet/stream

coordination:
  type: local

api:
  host: "0.0.0.0"
  port: 8000
  auth:
    tokens:
      - id: founder
        token: "${CREWLET_API_TOKEN_FOUNDER}"

secrets:
  active_key_id: "2026-01"
  keys:
    - id: "2026-01"
      material: "${CREWLET_SECRET_KEY_2026_01}"
`), NewResolver(MapSource{
		"CREWLET_NODE_ID":             "node-eu-1",
		"CREWLET_API_TOKEN_FOUNDER":   "tok",
		"CREWLET_SECRET_KEY_2026_01":  "bWF0ZXJpYWw=",
		"CREWLET_STORE_UNUSED_MARKER": "",
	}))
	if err != nil {
		t.Fatalf("the documented Tier A shape must load:\n%v", err)
	}
	id, err := ResolveNodeID(cfg, EnvOnly())
	if err != nil {
		t.Fatal(err)
	}
	if id != "node-eu-1" {
		t.Fatalf("node id = %q", id)
	}
	if !cfg.Secrets.Enabled() {
		t.Fatal("the keyring should read as configured")
	}
}

// ---- quickstart plumbing --------------------------------------------- //

func quickstartText(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(repoRoot, "docs", "getting-started", "quickstart.md"))
	if err != nil {
		t.Skipf("the docs tree is not in this checkout: %v", err)
	}
	return string(data)
}

var yamlBlockRE = regexp.MustCompile("(?s)```yaml\n(.*?)```")

// quickstartCompanyBlock finds the Tier B block: the only one with a
// top-level name, which is exactly what distinguishes the tiers.
func quickstartCompanyBlock(t *testing.T) string {
	t.Helper()
	for _, m := range yamlBlockRE.FindAllStringSubmatch(quickstartText(t), -1) {
		if strings.HasPrefix(m[1], "name:") {
			return m[1]
		}
	}
	t.Fatal("the quickstart no longer contains a company config block")
	return ""
}

// wholeRef reports whether a value is exactly one ${VAR} reference, which
// is the only shape a credential may take in a committed example.
func wholeRef(value string) (string, bool) {
	m := regexp.MustCompile(`^\$\{([A-Za-z_][A-Za-z0-9_]*)\}$`).FindStringSubmatch(strings.TrimSpace(value))
	if m == nil {
		return "", false
	}
	return m[1], true
}

// THE SHIPPED GIT-AUTH RECIPE KEEPS ITS SECURITY PROPERTIES.
//
// examples/nimbus.company.yaml hands every sandboxed seat a credential
// helper carrying that seat's own code-host PAT. The recipe is CONFIG — the
// engine ships no setup steps of its own — so nothing in the engine
// constrains it, and the properties that keep the token from leaking are
// properties of this file and this file alone:
//
//   - The helper is scoped to the host at BOTH layers. The `credential.
//     "https://gitlab.com".helper` key is what git consults, and the script
//     re-checks `host=` itself. Either alone is a token offered to whatever
//     host asks — a malicious submodule URL is the cheap version of that
//     attack, and git will happily consult a helper for it.
//   - It stays silent with no token, so a public clone falls through to
//     anonymous instead of failing.
//   - insteadOf is added with `--add` on both forms. It is a multi-valued
//     key: a second plain `git config` REPLACES the first, which leaves
//     scp-style `git@host:` remotes on SSH — with no key in the box, so
//     they simply fail.
//   - The commit identity comes from the engine's generic agent facts, so
//     work attributes to the seat rather than to whoever built the image.
//
// A reader editing this recipe sees prose explaining each of those. This is
// what fails when the edit lands anyway.
func TestTheShippedGitAuthRecipeStaysScoped(t *testing.T) {
	t.Parallel()
	data, err := os.ReadFile(filepath.Join(repoRoot, "examples", "nimbus.company.yaml"))
	if err != nil {
		t.Skipf("the example tree is not in this checkout: %v", err)
	}
	cfg, err := ParseCompany(data)
	if err != nil {
		t.Fatalf("examples/nimbus.company.yaml no longer loads:\n%v", err)
	}
	if cfg.Providers.Sandbox == nil {
		t.Fatal("the example configures no sandbox provider, so this proves nothing")
	}

	var step *SandboxSetupStep
	for i := range cfg.Providers.Sandbox.Setup {
		if cfg.Providers.Sandbox.Setup[i].Name == "git-auth" {
			step = &cfg.Providers.Sandbox.Setup[i]
			break
		}
	}
	if step == nil {
		t.Fatal("the example ships no git-auth setup step; a sandboxed seat " +
			"would have no way to authenticate a headless clone")
	}

	var helper string
	for path, content := range step.Files {
		if strings.Contains(path, "git-credential-") {
			helper = content
		}
	}
	if helper == "" {
		t.Fatal("the git-auth step writes no credential helper")
	}

	// Layer one: the script refuses to answer for a host it does not know.
	if !strings.Contains(helper, "host=") {
		t.Error("the credential helper does not check the host it is being " +
			"asked about, so the seat's token is offered to any host that " +
			"asks — a malicious submodule URL is enough")
	}
	// And it declines rather than erroring when there is nothing to offer.
	if !strings.Contains(helper, "exit 0") {
		t.Error("the credential helper has no silent-decline path, so a " +
			"public clone fails instead of falling through to anonymous")
	}

	commands := strings.Join(step.Commands, "\n")

	// Layer two: git is told to consult it for that host only. A bare
	// `credential.helper` is the global one, consulted for everything.
	if !strings.Contains(commands, `credential."https://`) {
		t.Error("the helper is not registered under a host-scoped " +
			"credential key, so git consults it for every host")
	}
	for _, form := range []string{`insteadOf "git@`, `insteadOf "ssh://`} {
		if !strings.Contains(commands, form) {
			t.Errorf("no rewrite for %s remotes: they stay on SSH, and the "+
				"box has no key, so they fail", form)
		}
	}
	// Both rewrites must be --add. url.<base>.insteadOf is multi-valued.
	if adds := strings.Count(commands, "--add"); adds < 2 {
		t.Errorf("insteadOf is set with --add %d times, want at least 2: "+
			"without it the second rewrite replaces the first", adds)
	}
	for _, fact := range []string{"CREWLET_AGENT_HANDLE", "CREWLET_AGENT_EMAIL"} {
		if !strings.Contains(commands, fact) {
			t.Errorf("the commit identity does not come from $%s, so work "+
				"does not attribute to the seat", fact)
		}
	}
	// Headless git must never block on a prompt it cannot answer.
	if step.Env["GIT_TERMINAL_PROMPT"] != "0" {
		t.Error("GIT_TERMINAL_PROMPT is not 0, so an unauthenticated fetch " +
			"blocks on a username prompt until the run's TTL expires")
	}
}

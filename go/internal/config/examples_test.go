package config

import (
	"bytes"
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
const repoRoot = "../../.."

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

// THE COMMITTED SCHEMAS ARE WHAT THESE MODELS EMIT.
//
// `schema/*.json` is what an editor validates a config against, through the
// `# yaml-language-server:` modeline the examples carry — so a stale file
// flags a correct config as wrong, on the exact key the author just learned
// about. Nothing else compares them: the generator is a CLI command an
// author runs by hand, and forgetting is silent.
func TestTheCommittedSchemasMatchTheModels(t *testing.T) {
	t.Parallel()
	for _, tier := range []Tier{TierBootstrap, TierCompany} {
		name := filepath.Join(repoRoot, "schema", string(tier)+".schema.json")
		committed, err := os.ReadFile(name)
		if err != nil {
			t.Skipf("the schema tree is not in this checkout: %v", err)
		}
		generated, err := Schema(tier)
		if err != nil {
			t.Fatalf("generating the %s schema: %v", tier, err)
		}
		if !bytes.Equal(bytes.TrimSpace(committed), bytes.TrimSpace(generated)) {
			t.Errorf("schema/%s.schema.json is stale; regenerate it with "+
				"`crewlet schema %s -o schema/%s.schema.json`", tier, tier, tier)
		}
	}
}

// Tier A in its own shape, as the docs will carry it.
func TestBootstrapExampleShapeLoads(t *testing.T) {
	t.Parallel()
	cfg, err := ParseBootstrap([]byte(`
debug: false

node:
  id: "${CREWLET_NODE_ID}"
  roles: [ingress, seats, workers]
  labels:
    zone: eu

store:
  path: /var/lib/crewlet/crewlet.db
  driver: turso

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

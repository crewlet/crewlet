package config

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/crewlet/crewlet/internal/envref"
	"github.com/crewlet/crewlet/internal/hostbox"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/sourcetree"
)

// readRepoFile reads a file shipped in this repository, and FAILS when it
// cannot.
//
// Every caller here used to t.Skipf "the example tree is not in this
// checkout". CI does a full checkout and the module zip carries examples/ and
// docs/, so none of those skips has ever fired — which means the only event
// they could ever respond to is the one they must not hide: somebody renaming
// examples/nimbus.company.yaml, moving a docs page, or dropping an entry from
// the list. Every one of these cases certifies a SHIPPED artefact, so a file
// that is not there is a broken checkout or a rename nobody swept, and both
// are findings.
//
// This is not hypothetical. internal/api/queries/company_test.go carried the
// identical shape over a dashboard path, the React rewrite moved the path, and
// that gate certified nothing on every machine and in CI until somebody went
// looking.
func readRepoFile(t *testing.T, parts ...string) []byte {
	t.Helper()
	path := filepath.Join(append([]string{sourcetree.Root(t)}, parts...)...)
	data, err := os.ReadFile(path) //nolint:gosec // a path this repository ships
	if err != nil {
		t.Fatalf("read %s: %v — this case certifies a file this repository "+
			"ships, so a missing one is a rename that was not swept, not a "+
			"reason to certify nothing", path, err)
	}
	return data
}

// The shipped examples and the docs are a promise: someone copies them and
// runs them. Nothing else stops them drifting away from what the loader
// accepts, so this suite is what holds them to the models.
// shippedCompanies is EVERY company example in the tree.
//
// A list rather than one filename, because a second example is exactly the
// thing that rots: the one the docs open with gets read on every change and
// the other one does not. Both are a promise someone copies and runs.
var shippedCompanies = []string{
	// The reference: the full stack, on a metered key.
	"nimbus.company.yaml",
	// The short path: Mattermost alone, on a coding-CLI subscription.
	"nimbus-claude-cli.company.yaml",
}

func TestShippedCompanyExamplesLoad(t *testing.T) {
	t.Parallel()
	for _, name := range shippedCompanies {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			data := readRepoFile(t, "examples", name)
			cfg, err := ParseCompany(data)
			if err != nil {
				t.Fatalf("examples/%s no longer loads:\n%v", name, err)
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
				t.Fatalf("the example should model both kinds of seat: %d agents, %d humans",
					agents, humans)
			}

			// A FILE PEOPLE COPY AND KEEP states every identity. One
			// minted from a name is minted again from the corrected name
			// on the next import, and that is a different seat — a new
			// agent id, an empty mailbox, its memory left behind.
			if missing := undeclaredIdentities(t, data); len(missing) > 0 {
				t.Errorf("examples/%s leaves identities to be minted from names:\n  %s",
					name, strings.Join(missing, "\n  "))
			}

			// Every model credential in a committed example must be a
			// reference. A literal here would be published,
			// permanently, in git history and in every artifact
			// built from it. Only auth's fields: env holds settings
			// beside keys, and a literal "1" there is a setting.
			for key, provider := range cfg.Providers.LLM {
				for field, raw := range map[string]string{
					"auth.token":             provider.Auth.Token,
					"auth.api_key":           provider.Auth.APIKey,
					"auth.credential_bundle": provider.Auth.CredentialBundle,
				} {
					if _, whole := wholeRef(raw); raw != "" && !whole {
						t.Fatalf("providers.llm.%s.%s is a literal: %q", key, field, raw)
					}
				}
			}
		})
	}
}

// THE TWO EXAMPLES ARE THE SAME COMPANY, and that is the whole point of
// shipping two: a reader comparing them should see the stack change and
// nothing else. If the org charts drift apart, the smaller one stops being
// "the same company with less wired up" and becomes a second thing to
// maintain — which is how the one nobody opens goes stale.
func TestBothShippedExamplesModelTheSameCompany(t *testing.T) {
	t.Parallel()
	seats := func(name string) []string {
		t.Helper()
		data := readRepoFile(t, "examples", name)
		cfg, err := ParseCompany(data)
		if err != nil {
			t.Fatalf("examples/%s no longer loads:\n%v", name, err)
		}
		o, err := cfg.Organization()
		if err != nil {
			t.Fatalf("examples/%s does not build an org:\n%v", name, err)
		}
		var out []string
		for r := range o.AllRoles() {
			out = append(out, r.Handle())
		}
		slices.Sort(out)
		return out
	}
	full, cli := seats("nimbus.company.yaml"), seats("nimbus-claude-cli.company.yaml")
	if !slices.Equal(full, cli) {
		t.Errorf("the two examples no longer model the same seats:\n  full: %v\n  cli:  %v",
			full, cli)
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
	// VALIDATION NO LONGER ASKS FOR A CONTACT, so this is what keeps the page
	// teaching a founder the company's agents can actually @-mention: the
	// quickstart wires Mattermost further down, and a founder seat with no
	// identity there is one every escalation reaches and nobody reads.
	if humans[0].Contact.IsEmpty() {
		t.Fatal("the quickstart's founder seat carries no contact identity, " +
			"so no agent can @-mention them on the chat the page connects")
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

	// The page tells the reader handles are permanent and to set them; the
	// block has to model that rather than rely on derivation from a name
	// they are about to replace with their own.
	if missing := undeclaredIdentities(t, []byte(block)); len(missing) > 0 {
		t.Errorf("the quickstart's company leaves identities to be minted "+
			"from names, which a re-import of an edited file mints again:\n  %s",
			strings.Join(missing, "\n  "))
	}
}

// undeclaredIdentities names every seat in a company FILE that declares no
// `handle:` and every unit that declares no `id:`, read as the file is
// written.
//
// NOT FROM THE PARSED COMPANY: [ParseCompany] writes both in where they are
// missing ([MintIdentities]), so a check of what it returns can never see one
// missing — this case asked the parsed seats for one and passed whatever the
// page said. And the file is what matters: an edit of the STORED document
// keeps the identity minted for it, but a file is minted afresh from its names
// on every import, so a seat renamed in a file that left its handle out comes
// back as a different seat.
func undeclaredIdentities(t *testing.T, data []byte) []string {
	t.Helper()
	var written Company
	if err := yaml.Unmarshal(data, &written); err != nil {
		t.Fatalf("decode the company as written: %v", err)
	}
	var missing []string
	for role, at := range written.EachRole() {
		if strings.TrimSpace(role.Handle) == "" {
			missing = append(missing, fmt.Sprintf("seat %q (%s) declares no handle", role.Name, at))
		}
	}
	for unit, at := range written.EachUnit() {
		if strings.TrimSpace(unit.ID) == "" {
			missing = append(missing, fmt.Sprintf("unit %q (%s) declares no id", unit.Name, at))
		}
	}
	return missing
}

// THE README'S COMPANY LOADS. README.md is the repository's front page, and
// the company on it is the first YAML most readers copy and run — and no case
// read it. When providers.sandbox became required of every company with an
// agent seat and a model, that company was refused by `crewlet validate` and
// nothing went red: a sweep of the docs is what found it.
//
// EVERY yaml block on the page is held as a company document, so a second one
// is covered the day it is added, and a block of another kind fails here until
// this case is given its loader rather than being certified by nobody. A
// failure names the line of README.md the block starts on.
func TestTheReadmeCompanyLoads(t *testing.T) {
	t.Parallel()
	blocks := yamlBlocksOf(string(readRepoFile(t, "README.md")))
	if len(blocks) == 0 {
		t.Fatal("README.md shows no yaml block, so this case certifies nothing: " +
			"the company moved or its fence changed, and the case has to follow it")
	}
	for _, block := range blocks {
		at := fmt.Sprintf("README.md:%d", block.line)
		t.Run(at, func(t *testing.T) {
			t.Parallel()
			cfg, err := ParseCompany(block.doc)
			if err != nil {
				t.Fatalf("%s: the company here is refused — a reader copying it gets "+
					"a document `crewlet validate` rejects:\n%v", at, err)
			}
			o, err := cfg.Organization()
			if err != nil {
				t.Fatalf("%s: the company here does not build an org:\n%v", at, err)
			}

			// WHAT THE PAGE SAYS OF IT, which validation does not hold. "A
			// four-agent company, with a seat for you": a company with no
			// human seat is valid — one its agents run alone — and admits
			// nobody, the reader included, since every person joins on one.
			var agents, humans int
			for r := range o.AllRoles() {
				if r.IsHuman() {
					humans++
					continue
				}
				agents++
			}
			if agents == 0 || humans == 0 {
				t.Errorf("%s: the company should model both kinds of seat, an agent's "+
					"and the reader's own: %d agents, %d humans", at, agents, humans)
			}
			// "a handle is the seat's identity for good": a file is minted
			// afresh from its names on every import, so one a reader keeps
			// states every identity.
			if missing := undeclaredIdentities(t, block.doc); len(missing) > 0 {
				t.Errorf("%s: the company leaves identities to be minted from names, "+
					"which a re-import of an edited copy mints again:\n  %s",
					at, strings.Join(missing, "\n  "))
			}
			// "manages: and lead: name a seat by its handle and a unit by its
			// key": the block is the page's example of that rule, so every
			// reference in it resolves. Validation only warns of one that
			// does not, because a company assembled in pieces carries them.
			for _, w := range cfg.ReferenceWarnings() {
				t.Errorf("%s: %s: %s", at, w.Path, w.Message)
			}
		})
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
	for _, ref := range References(cfg) {
		if !exported[ref.Name] {
			t.Fatalf("the quickstart config references ${%s} at %s but the page never "+
				"exports it", ref.Name, ref.Path)
		}
	}
}

// THE SHIPPED EXAMPLE LOADS, which is the whole point of shipping one.
//
// What is pinned is that it stays loadable: it is what the quickstart, both
// bootstrap scripts and every integration walkthrough tell a founder to run,
// and nothing else notices when it stops working.
func TestBootstrapExampleLoads(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"nimbus.config.yaml", "nimbus-claude-cli.config.yaml"} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			bootstrapExampleLoads(t, name)
		})
	}
}

func bootstrapExampleLoads(t *testing.T, name string) {
	t.Helper()
	data := readRepoFile(t, "examples", name)
	// EVERY REFERENCE ANSWERED, so what is under test is the SHAPE rather
	// than whose shell happens to have the variables exported. A file that
	// only loads on a machine with the right environment is not a shipped
	// example.
	answers := MapSource{}
	for _, name := range envref.Names(string(data)) {
		// LONG ENOUGH TO BE A CREDENTIAL, because one of these
		// variables is one: `api.auth.tokens[].token` is checked on
		// the RESOLVED value against the 26-character floor, which is
		// the whole point of checking it there. A placeholder shorter
		// than that would make every shipped example fail on the
		// stand-in rather than on its own shape.
		answers[name] = "set-for-this-test-and-long-enough"
	}
	// AND A KEY WHERE A KEY GOES. Key material is checked on what its
	// reference RESOLVES to, as the cipher a node boots with reads it, so
	// the stand-in above is refused as a key — which says nothing about
	// the example's shape. The variables the keyring's material names are
	// read off the file itself rather than off a naming convention.
	var keyring struct {
		Secrets struct {
			Keys []struct {
				Material string `yaml:"material"`
			} `yaml:"keys"`
		} `yaml:"secrets"`
	}
	if err := yaml.Unmarshal(data, &keyring); err != nil {
		t.Fatalf("read the example's keyring: %v", err)
	}
	for _, key := range keyring.Secrets.Keys {
		for _, name := range envref.Names(key.Material) {
			answers[name] = TestKeyMaterial
		}
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
  roles: [data, ingress, seats, workers]
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
  external_url: "https://crewlet.example.com"
  auth:
    admin: serve
    tokens:
      - id: founder
        token: "${CREWLET_API_TOKEN_FOUNDER}"
        grants: [admin]

secrets:
  active_key_id: "2026-01"
  keys:
    - id: "2026-01"
      material: "${CREWLET_SECRET_KEY_2026_01}"
`), NewResolver(MapSource{
		"CREWLET_NODE_ID":             "node-eu-1",
		"CREWLET_API_TOKEN_FOUNDER":   "a-token-long-enough-to-pass",
		"CREWLET_SECRET_KEY_2026_01":  TestKeyMaterial,
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
	data := readRepoFile(t, "docs", "getting-started", "quickstart.md")
	return string(data)
}

var yamlBlockRE = regexp.MustCompile("(?s)```yaml\n(.*?)```")

// pageYAMLBlock is one ```yaml block of a markdown page, ready to parse.
type pageYAMLBlock struct {
	// line is the line of the page the block's first line is on.
	line int
	// doc is the block AT ITS PLACE ON THE PAGE: its text behind one empty
	// line for every page line above it, which YAML reads as nothing, so a
	// line the parser names (an unknown key, a value of the wrong type) is
	// the page's own rather than one counted from the fence.
	doc []byte
}

// yamlBlocksOf is every ```yaml block of a markdown page, in page order.
func yamlBlocksOf(page string) []pageYAMLBlock {
	var out []pageYAMLBlock
	for _, m := range yamlBlockRE.FindAllStringSubmatchIndex(page, -1) {
		line := strings.Count(page[:m[2]], "\n") + 1
		out = append(out, pageYAMLBlock{
			line: line,
			doc:  []byte(strings.Repeat("\n", line-1) + page[m[2]:m[3]]),
		})
	}
	return out
}

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

// THE PROVIDERS REFERENCE VALIDATES. The block under "Providers" in the
// configuration reference shows every field an entry takes, so it is the one
// operators copy whole — and nothing read it, so a rule added to the validator
// left the documented example refused by `crewlet validate` with no test going
// red. Checked
// through the whole company path: the reference block replaces the
// quickstart's providers, so the seats that select `default` still resolve.
func TestTheProvidersReferenceValidates(t *testing.T) {
	t.Parallel()
	page := string(readRepoFile(t, "docs", "getting-started", "configuration.md"))
	start := strings.Index(page, "\n## Providers\n")
	if start < 0 {
		t.Fatal("configuration.md has no Providers section")
	}
	m := yamlBlockRE.FindStringSubmatch(page[start:])
	if m == nil || !strings.HasPrefix(m[1], "providers:") {
		t.Fatal("the Providers section no longer opens with a providers block")
	}
	var reference, company yaml.Node
	if err := yaml.Unmarshal([]byte(m[1]), &reference); err != nil {
		t.Fatalf("the Providers reference is not YAML: %v", err)
	}
	if err := yaml.Unmarshal([]byte(quickstartCompanyBlock(t)), &company); err != nil {
		t.Fatalf("the quickstart's company is not YAML: %v", err)
	}
	root, providers := company.Content[0], mappingValue(reference.Content[0], "providers")
	replaced := false
	for i := 0; i+1 < len(root.Content); i += 2 {
		if root.Content[i].Value == "providers" {
			root.Content[i+1], replaced = providers, true
		}
	}
	if !replaced {
		t.Fatal("the quickstart's company has no providers to replace")
	}
	data, err := yaml.Marshal(&company)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if _, err := ParseCompany(data); err != nil {
		t.Fatalf("the Providers reference in configuration.md is refused by validation "+
			"— an operator copying it gets a document `crewlet validate` rejects:\n%v", err)
	}
}

// THE SANDBOX REFERENCE VALIDATES. The block under "Engine provider" in
// code-sandbox.md writes out every field of providers.sandbox, so it is the
// one an operator copies whole — and its container-only fields are held to the
// cells the company's executors reach, so a value that reads as a harmless
// default there (`runtime: auto` beside `default_run_in: e2b`) is a refusal.
// Checked through the whole company path: the reference replaces the
// quickstart's catalogue, so the quickstart's seats are the executors whose
// cells it is judged against.
func TestTheSandboxReferenceValidates(t *testing.T) {
	t.Parallel()
	page := string(readRepoFile(t, "docs", "concepts", "code-sandbox.md"))
	start := strings.Index(page, "\n### Engine provider — `providers.sandbox`\n")
	if start < 0 {
		t.Fatal("code-sandbox.md has no `Engine provider — providers.sandbox` section")
	}
	m := yamlBlockRE.FindStringSubmatch(page[start:])
	if m == nil || !strings.HasPrefix(m[1], "providers:") {
		t.Fatal("the providers.sandbox section no longer opens with a providers block")
	}
	var reference, company yaml.Node
	if err := yaml.Unmarshal([]byte(m[1]), &reference); err != nil {
		t.Fatalf("the providers.sandbox reference is not YAML: %v", err)
	}
	sandbox := mappingValue(mappingValue(reference.Content[0], "providers"), "sandbox")
	if sandbox == nil {
		t.Fatal("the providers.sandbox reference block carries no providers.sandbox")
	}
	if err := yaml.Unmarshal([]byte(quickstartCompanyBlock(t)), &company); err != nil {
		t.Fatalf("the quickstart's company is not YAML: %v", err)
	}
	providers := mappingValue(company.Content[0], "providers")
	replaced := false
	for i := 0; providers != nil && i+1 < len(providers.Content); i += 2 {
		if providers.Content[i].Value == "sandbox" {
			providers.Content[i+1], replaced = sandbox, true
		}
	}
	if !replaced {
		t.Fatal("the quickstart's company has no providers.sandbox to replace")
	}
	data, err := yaml.Marshal(&company)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if _, err := ParseCompany(data); err != nil {
		t.Fatalf("the providers.sandbox reference in code-sandbox.md is refused by "+
			"validation — an operator copying it gets a document `crewlet validate` "+
			"rejects:\n%v", err)
	}
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

// THE DOCUMENTED GIT-AUTH RECIPE KEEPS ITS SECURITY PROPERTIES.
//
// A sandboxed seat needs a credential helper carrying that seat's own
// code-host PAT, and the recipe is CONFIG — the engine ships no setup steps
// of its own — so nothing in the engine constrains it. It lives in
// docs/concepts/code-sandbox.md, which is where a reader copies it from, and
// the properties that keep the token from leaking are properties of that
// block and that block alone:
//
//   - The helper is scoped to the host at BOTH layers. The `credential.
//     "https://host".helper` key is what git consults, and the script
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
//   - The helper lands in the box's HOME. Neither local cell writes a setup
//     file outside it, and a direct box's home is minted per box, so no
//     absolute path a file entry holds is inside it: the recipe as it
//     stood, writing /usr/local/bin, was refused at setup in both local
//     cells, and a reader copying it got a seat that could never provision
//     a box. A file entry may still name the home RELATIVELY, which every
//     cell writes under it; the recipe writes the helper with a command
//     because git is given its absolute path, which only the box's shell
//     knows.
//   - That path is QUOTED where git is told it. git runs a helper's value
//     through the shell, so an unquoted "$HOME/…" splits at a space in the
//     home — a direct box's sits under state_dir: by default the engine
//     user's ~/.crewlet/sandboxes, and a relative one, like the examples',
//     resolved against wherever the engine was started — and every clone
//     dies with "could not read Username".
//
// The prose around the block explains each of those to a reader editing it.
// This is what fails when the edit lands anyway.
func TestTheDocumentedGitAuthRecipeStaysScoped(t *testing.T) {
	t.Parallel()
	cfg, err := ParseCompanyDocument([]byte(gitAuthRecipeBlock(t)))
	if err != nil {
		t.Fatalf("the documented git-auth recipe no longer parses:\n%v", err)
	}
	if cfg.Providers.Sandbox == nil {
		t.Fatal("the documented block configures no sandbox provider, so this proves nothing")
	}

	var step *SandboxSetupStep
	for i := range cfg.Providers.Sandbox.Setup {
		if cfg.Providers.Sandbox.Setup[i].Name == "git-auth" {
			step = &cfg.Providers.Sandbox.Setup[i]
			break
		}
	}
	if step == nil {
		t.Fatal("the documented recipe has no git-auth setup step; a sandboxed " +
			"seat following it would have no way to authenticate a headless clone")
	}

	var helper string
	for path, content := range step.Files {
		// The boxes' own rule, judged for a direct box's minted home, which
		// is the strictest any local cell holds a file to.
		if _, inside := hostbox.InHome("", path); !inside {
			t.Errorf("the git-auth step writes %s as a setup file, which no direct box "+
				"takes: its home is minted per box. Name the file relative to the box's "+
				"home, or write it under \"$HOME\" with a command", path)
		}
		if strings.Contains(path, "git-credential-") {
			helper = content
		}
	}
	for _, command := range step.Commands {
		if strings.Contains(command, "git-credential-") && strings.Contains(command, "<<") {
			helper = command
			if !strings.Contains(command, `"$HOME/`) {
				t.Errorf("the credential helper is not written under the box's home, the " +
					"one place every cell lets a setup step write it")
			}
		}
	}
	if helper == "" {
		t.Fatal("the git-auth step writes no credential helper")
	}
	for _, command := range step.Commands {
		if strings.Contains(command, "credential.") && strings.Contains(command, ".helper") &&
			strings.Contains(command, "$HOME") && !strings.Contains(command, `"!'`) {
			t.Errorf("the helper is registered by an unquoted path under $HOME, which a home "+
				"holding a space splits: register it as \"!'$HOME/…'\":\n%s", command)
		}
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

// gitAuthRecipeBlock finds the recipe on the code-sandbox page: the only
// yaml block that writes a credential helper.
func gitAuthRecipeBlock(t *testing.T) string {
	t.Helper()
	data := readRepoFile(t, "docs", "concepts", "code-sandbox.md")
	// The recipe proper is the block that declares the whole provider, so a
	// shorter snippet naming the helper elsewhere on the page is never
	// mistaken for it.
	for _, m := range yamlBlockRE.FindAllStringSubmatch(string(data), -1) {
		if strings.HasPrefix(m[1], "providers:") && strings.Contains(m[1], "git-credential-") {
			return m[1]
		}
	}
	t.Fatal("docs/concepts/code-sandbox.md no longer carries the git-auth recipe")
	return ""
}

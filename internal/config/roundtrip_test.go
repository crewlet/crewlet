package config

import (
	"encoding/json"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// A revision is stored, exported and served back to an operator, so a
// config has to survive the trip. The two things that must survive it are
// the ones a naive encoder loses: a ${VAR} POINTER (which must never be
// resolved on the way out) and the SHAPE its author wrote.
const roundTripDoc = `
name: Acme
mission: ship
providers:
  llm:
    default:
      type: anthropic
      model: claude-sonnet-5
      api_keys:
        - "${ANTHROPIC_API_KEY}"
    fast:
      type: anthropic
      model: claude-haiku-4-5
      api_keys:
        - "${ANTHROPIC_API_KEY}"
    big:
      type: anthropic
      model: claude-opus-5
      api_keys:
        - "${ANTHROPIC_API_KEY}"
    bigger:
      type: openai
      model: gpt-5
      api_keys:
        - "${OPENAI_API_KEY}"
integrations:
  confluence:
    url: "${CONFLUENCE_URL}"
    webhook_secret: "${CONFLUENCE_WEBHOOK_SECRET}"
    token: "${CONFLUENCE_TOKEN}"
  mattermost:
    enabled: true
    url: https://mm.example.com
    team: acme
    typing_status: always
roles:
  - name: Founder
    kind: human
    contact:
      mattermost_user_id: founder
  - name: CEO
    handle: ceo
    llm: fast
    learning_enabled: false
  - name: CTO
    handle: cto
    llm:
      default: fast
      review: [big, bigger]
mcp_servers:
  - name: gitlab
    shared: false
    command: glab
    args: [mcp, serve]
`

func TestCompanyRoundTripsThroughYAML(t *testing.T) {
	t.Parallel()
	original := mustCompany(t, roundTripDoc)

	encoded, err := yaml.Marshal(original)
	if err != nil {
		t.Fatalf("a company config must serialise: %v", err)
	}

	// A resolved secret in an export is the leak that keeping references
	// verbatim exists to prevent.
	for _, ref := range []string{
		"${ANTHROPIC_API_KEY}", "${CONFLUENCE_URL}",
		"${CONFLUENCE_WEBHOOK_SECRET}", "${CONFLUENCE_TOKEN}",
	} {
		if !strings.Contains(string(encoded), ref) {
			t.Fatalf("the export lost %s:\n%s", ref, encoded)
		}
	}

	reloaded, err := ParseCompany(encoded)
	if err != nil {
		t.Fatalf("an exported config no longer loads:\n%v\n\n%s", err, encoded)
	}

	if reloaded.Name != original.Name || reloaded.Mission != original.Mission {
		t.Fatalf("identity changed: %+v", reloaded)
	}
	if got := reloaded.Providers.LLM["default"].APIKeys[0]; got != "${ANTHROPIC_API_KEY}" {
		t.Fatalf("the key was rewritten as %q", got)
	}
	if reloaded.Integrations.Mattermost == nil || reloaded.Integrations.Mattermost.TypingStatus != StatusAlways {
		t.Fatalf("an enum did not survive: %+v", reloaded.Integrations.Mattermost)
	}
}

// A one-key chain is written back as the bare scalar it was authored as, so
// re-serialising a hand-written config does not rewrite every llm: line
// into a mapping and produce a diff nobody made.
func TestPhaseLLMKeepsItsAuthoredShape(t *testing.T) {
	t.Parallel()
	cfg := mustCompany(t, roundTripDoc)
	encoded, err := yaml.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(encoded), "llm: fast") {
		t.Fatalf("a scalar chain was expanded:\n%s", encoded)
	}
	reloaded := mustCompany(t, string(encoded))
	if got := reloaded.Roles[2].LLM.Review; len(got) != 2 || got[0] != "big" {
		t.Fatalf("the per-phase mapping did not survive: %v", got)
	}
}

// A toggle's third state has to survive too: an export that froze today's
// default into the document would make a future default change invisible to
// every company that had ever exported its config.
func TestUnsetToggleIsNotFrozenIntoAnExport(t *testing.T) {
	t.Parallel()
	cfg := mustCompany(t, roundTripDoc)
	encoded, err := yaml.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	reloaded := mustCompany(t, string(encoded))

	// The CEO said nothing about learning; the CTO... also said nothing.
	if reloaded.Roles[2].LearningEnabled.IsSet() {
		t.Fatal("an unset toggle came back set")
	}
	// The CEO opted out explicitly, and that must not read as unset.
	ceo := reloaded.Roles[1]
	if !ceo.LearningEnabled.IsSet() || ceo.LearningEnabled.Or(true) {
		t.Fatalf("an explicit false was lost: %+v", ceo.LearningEnabled)
	}
}

// The store keeps a revision as a document, so the JSON path has to hold
// the same two properties as the YAML one.
func TestCompanyRoundTripsThroughJSON(t *testing.T) {
	t.Parallel()
	original := mustCompany(t, roundTripDoc)

	encoded, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("a company config must serialise to JSON: %v", err)
	}
	if !strings.Contains(string(encoded), "${ANTHROPIC_API_KEY}") {
		t.Fatalf("the JSON export lost the reference:\n%s", encoded)
	}

	var reloaded Company
	if err := json.Unmarshal(encoded, &reloaded); err != nil {
		t.Fatalf("a JSON revision no longer decodes: %v", err)
	}
	if err := reloaded.Validate(); err != nil {
		t.Fatalf("a JSON round trip produced an invalid config:\n%v", err)
	}
	if !strings.EqualFold(reloaded.Roles[2].LLM.Review[0], "big") {
		t.Fatalf("the per-phase mapping did not survive JSON: %+v", reloaded.Roles[2].LLM)
	}
}

func TestBootstrapRoundTripsThroughYAML(t *testing.T) {
	t.Parallel()
	original, err := ParseBootstrap([]byte(`
node:
  id: node-eu-1
  roles: [seats, workers]
  labels: {zone: eu}
store: {path: /var/lib/crewlet/crewlet.db, max_open_conns: 6}
stream: {type: embedded, store_dir: /var/lib/crewlet/stream}
api: {host: 127.0.0.1, port: 8000, auth: {tokens: [{id: founder, token: tok}]}}
`), EnvOnly())
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := yaml.Marshal(original)
	if err != nil {
		t.Fatal(err)
	}
	reloaded, err := ParseBootstrap(encoded, EnvOnly())
	if err != nil {
		t.Fatalf("an exported bootstrap no longer loads:\n%v\n\n%s", err, encoded)
	}
	if reloaded.Node.ID != "node-eu-1" ||
		reloaded.Store.Path != "/var/lib/crewlet/crewlet.db" ||
		reloaded.Store.MaxOpenConns != 6 {
		t.Fatalf("identity changed: %+v", reloaded)
	}
	roles, err := reloaded.Node.RoleSet()
	if err != nil {
		t.Fatal(err)
	}
	// An exported node profile must not silently widen: a node that
	// declared two roles and came back running all three would start
	// claiming duties nobody assigned it.
	if roles.Has("ingress") {
		t.Fatalf("the declared role set widened on export: %v", roles.Names())
	}
}

package config

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/santhosh-tekuri/jsonschema/v6"
	"gopkg.in/yaml.v3"
)

func TestSchemaGenerates(t *testing.T) {
	t.Parallel()
	for _, tier := range SchemaTiers {
		data, err := Schema(tier)
		if err != nil {
			t.Fatalf("%s: %v", tier, err)
		}
		var doc map[string]any
		if err := json.Unmarshal(data, &doc); err != nil {
			t.Fatalf("%s: the schema is not valid JSON: %v", tier, err)
		}
		// The modeline in every shipped config points an editor at this
		// URL, so a missing id makes the whole artifact inert.
		if doc["$id"] == "" || doc["$schema"] == "" {
			t.Fatalf("%s: schema is missing its identity: %v", tier, doc)
		}
		if doc["additionalProperties"] != false {
			t.Fatalf("%s: the schema must refuse unknown keys, like the loader does", tier)
		}
	}
	if _, err := Schema("nonsense"); err == nil {
		t.Fatal("an unknown tier should be refused")
	}
}

// Generation must be deterministic: the schema is a committed artifact, and
// a generator that reordered its own output would show a diff on every run
// and teach reviewers to ignore it.
func TestSchemaGenerationIsStable(t *testing.T) {
	t.Parallel()
	for _, tier := range SchemaTiers {
		first, err := Schema(tier)
		if err != nil {
			t.Fatal(err)
		}
		second, err := Schema(tier)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(first, second) {
			t.Fatalf("%s: two generations differ", tier)
		}
	}
}

// Closed sets reach the schema from the SAME slices the validators read, so
// an editor's enum cannot drift from what the engine accepts. This walks
// the generated document and checks the ones that matter most.
func TestSchemaEnumsMatchTheValidators(t *testing.T) {
	t.Parallel()
	company := schemaDoc(t, TierCompany)
	defs, _ := company["$defs"].(map[string]any)

	cases := []struct {
		def, field string
		want       []string
	}{
		{"LLMProvider", "type", strs(LLMProviderTypes)},
		{"LLMProvider", "reasoning_effort", strs(ReasoningEfforts)},
		{"EmbeddingProvider", "type", strs(EmbeddingProviderTypes)},
		{"SandboxProvider", "type", strs(SandboxTypes)},
		{"SandboxProvider", "default_coding_agent", strs(CodingAgents)},
		{"LocalSandbox", "containment", strs(Containments)},
		{"LocalSandbox", "runtime", strs(ContainerRuntimes)},
		{"MCPServer", "transport", strs(MCPTransports)},
		{"Slack", "typing_status", strs(WorkingStatuses)},
		{"Mattermost", "typing_status", strs(WorkingStatuses)},
		{"PlaneProvisioning", "role", strs(PlaneRoles)},
		{"GitLabProvisioning", "access_level", strs(GitLabAccessLevels)},
		{"GitLabProvisioning", "group_webhook", strs(GroupWebhookModes)},
	}
	for _, tc := range cases {
		def, ok := defs[tc.def].(map[string]any)
		if !ok {
			t.Fatalf("$defs has no %s", tc.def)
		}
		props, _ := def["properties"].(map[string]any)
		field, ok := props[tc.field].(map[string]any)
		if !ok {
			t.Fatalf("%s has no %s", tc.def, tc.field)
		}
		raw, ok := field["enum"].([]any)
		if !ok {
			t.Fatalf("%s.%s carries no enum", tc.def, tc.field)
		}
		got := make([]string, len(raw))
		for i, v := range raw {
			got[i], _ = v.(string)
		}
		if strings.Join(got, ",") != strings.Join(tc.want, ",") {
			t.Fatalf("%s.%s enum = %v, validators accept %v", tc.def, tc.field, got, tc.want)
		}
	}
}

// parityCase is one document run through both layers.
type parityCase struct {
	name string
	tier Tier
	yaml string
	// editorCatches marks a mistake the schema is expected to flag on its
	// own — the reason the artifact exists.
	editorCatches bool
}

// THE INVARIANT: everything the schema rejects, the validator also rejects.
//
// The reverse does not hold and must not be asserted — a schema cannot
// express everything Validate checks. What it must never do is the other
// direction: an editor red-underlining a config the engine would happily
// run teaches an author to ignore it, and then it catches nothing at all.
func TestSchemaNeverRejectsWhatTheValidatorAccepts(t *testing.T) {
	t.Parallel()
	compiled := map[Tier]*jsonschema.Schema{
		TierBootstrap: compileSchema(t, TierBootstrap),
		TierCompany:   compileSchema(t, TierCompany),
	}

	for _, tc := range parityCases() {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			schemaErr := compiled[tc.tier].Validate(asJSON(t, tc.yaml))
			validatorErr := validateTier(tc.tier, tc.yaml)

			if schemaErr != nil && validatorErr == nil {
				t.Fatalf("the schema rejects a config the engine accepts — an "+
					"editor would flag a working file:\n%v", schemaErr)
			}
			if tc.editorCatches {
				if schemaErr == nil {
					t.Fatal("the schema should catch this while the author is still typing")
				}
				if validatorErr == nil {
					t.Fatal("the schema flags this but the engine accepts it")
				}
			}
		})
	}
}

func parityCases() []parityCase {
	return []parityCase{
		// Valid documents. Every one of these must survive BOTH layers, or
		// the schema is stricter than the engine.
		{name: "minimal company", tier: TierCompany, yaml: "name: Acme\n"},
		{name: "empty bootstrap", tier: TierBootstrap, yaml: "debug: false\n"},
		{
			name: "a full company",
			tier: TierCompany,
			yaml: `
name: Acme
mission: ship
policies: [write things down]
token_budget: 1000000
skill_variables:
  wiki_base_url: "${WIKI_URL}"
providers:
  llm:
    default:
      type: anthropic
      model: claude-sonnet-5
      api_keys: ["${ANTHROPIC_API_KEY}"]
      cooldowns: {rate_limit_seconds: 3600, auth_seconds: 300}
    sub:
      type: cli-agent
      model: sonnet
      cli: {agent: claude-code, max_concurrent: 2}
  embeddings: {type: openai, model: text-embedding-3-small, api_key: "${OPENAI_API_KEY}", dimensions: 1536}
  sandbox:
    type: local
    local: {containment: container, image: acme/box, runtime: auto}
    default_coding_agent: opencode
    setup:
      - name: git-auth
        commands: ["git config --global user.name \"$CREWLET_AGENT_HANDLE\""]
        env: {GIT_TERMINAL_PROMPT: "0"}
        brief: git is configured
turn_engine:
  max_iterations: 3
  extension_enabled: true
learning:
  enabled: true
  skill_synthesis: {scheduler_enabled: true}
scheduling: {enabled: true, tick_seconds: 10, default_timezone: UTC}
integrations:
  slack:
    typing_status: addressed
    status_phrases: {plan: ["is planning..."]}
  gitlab:
    enabled: true
    url: https://gitlab.com
    signing_secret: "${GITLAB_SIGNING_SECRET}"
    provisioning: {group: acme, access_level: maintainer, group_webhook: auto}
mcp_servers:
  - name: gitlab
    shared: false
    command: glab
    args: [mcp, serve]
  - name: gh
    transport: http
    shared: false
    url: https://api.githubcopilot.com/mcp/
    tool_annotations:
      gh_read: {read_only: true}
      gh_write: {readOnlyHint: false, openWorldHint: true}
roles:
  - name: Founder
    kind: human
    manages: [CEO]
    contact: {slack_user_id: U0FOUNDER}
  - name: CEO
    handle: ceo
    llm: {default: default, judge: sub}
    schedules:
      - {name: standup, cron: "0 9 * * 1-5", task: post a status note}
units:
  - name: Core
    lead: CTO
    mcp_env: {gitlab: {GITLAB_TOKEN: "${GL_SHARED}"}}
    integrations: {plane: {project: ENG}}
    roles:
      - name: CTO
        handle: cto
        sandbox:
          enabled: true
          env: {GITHUB_TOKEN: "${GH_CTO}"}
          mcp: {servers: [gitlab]}
`,
		},
		{
			name: "a three-node fleet",
			tier: TierBootstrap,
			yaml: "coordination:\n  type: embedded-kv\nstream:\n  replicas: 3\n  cluster:\n    name: acme\n    peers: [nats://b:6222, nats://c:6222]\n",
		},

		// Mistakes an editor should catch inline.
		{name: "unknown top-level key", tier: TierCompany, yaml: "name: Acme\nmisson: typo\n", editorCatches: true},
		{name: "missing company name", tier: TierCompany, yaml: "mission: ship\n", editorCatches: true},
		{
			name: "unknown provider type", tier: TierCompany, editorCatches: true,
			yaml: "name: Acme\nproviders:\n  llm:\n    default: {type: openai-compatable, model: m, base_url: https://x}\n",
		},
		{
			name: "unknown containment", tier: TierCompany, editorCatches: true,
			yaml: "name: Acme\nproviders:\n  sandbox: {type: local, local: {containment: chroot, image: x}}\n",
		},
		{
			name: "malformed handle", tier: TierCompany, editorCatches: true,
			yaml: "name: Acme\nroles:\n  - {name: CEO, handle: \"Chief Exec\"}\n",
		},
		{
			name: "coalesce window past the cap", tier: TierCompany, editorCatches: true,
			yaml: "name: Acme\nnotification_coalesce_window_seconds: 120\n",
		},
		{
			name: "an mcp server with no name", tier: TierCompany, editorCatches: true,
			yaml: "name: Acme\nmcp_servers:\n  - {command: uvx}\n",
		},
		{
			name: "both knowledge backends", tier: TierCompany, editorCatches: true,
			yaml: "name: Acme\nintegrations:\n  confluence: {url: https://x/wiki, token: t}\n  plane: {enabled: true, url: https://p, workspace: w, webhook_secret: s}\n",
		},
		{
			name: "a plane scope with no plane", tier: TierCompany, editorCatches: true,
			yaml: "name: Acme\nknowledge:\n  plane_projects: [ENG]\n",
		},
		{
			name: "a confluence scope while plane is the backend", tier: TierCompany, editorCatches: true,
			yaml: "name: Acme\nintegrations:\n  plane: {enabled: true, url: https://p, workspace: w, webhook_secret: s}\nknowledge:\n  confluence_spaces: [HANDBOOK]\n",
		},
		{
			name: "unknown bootstrap key", tier: TierBootstrap, editorCatches: true,
			yaml: "stroe:\n  path: x.db\n",
		},
		{
			name: "a Tier B key in Tier A", tier: TierBootstrap, editorCatches: true,
			yaml: "name: Acme\n",
		},
		{
			name: "an api port out of range", tier: TierBootstrap, editorCatches: true,
			yaml: "api:\n  port: 70000\n",
		},
		{
			name: "a fleet on local coordination", tier: TierBootstrap, editorCatches: true,
			yaml: "stream:\n  cluster:\n    name: acme\n    peers: [nats://b:6222, nats://c:6222]\n",
		},
		{
			name: "a two-node fleet", tier: TierBootstrap, editorCatches: true,
			yaml: "coordination:\n  type: embedded-kv\nstream:\n  cluster: {name: acme, peers: [nats://b:6222]}\n",
		},
		{
			name: "replicas with nobody to replicate to", tier: TierBootstrap, editorCatches: true,
			yaml: "coordination:\n  type: embedded-kv\nstream:\n  replicas: 3\n",
		},
		{
			name: "an external stream with no url", tier: TierBootstrap, editorCatches: true,
			yaml: "coordination:\n  type: embedded-kv\nstream:\n  type: nats\n",
		},

		// Rules the schema cannot express. They belong here to prove the
		// SOUNDNESS direction on documents the schema must let through and
		// the validator must not.
		{name: "a signing secret with no bot token", tier: TierCompany, yaml: "name: Acme\nroles:\n  - {name: CEO, integrations: {slack: {signing_secret: \"${S}\"}}}\n"},
		{name: "a stdio server with no command", tier: TierCompany, yaml: "name: Acme\nmcp_servers:\n  - {name: calc}\n"},
		{name: "duplicate handles", tier: TierCompany, yaml: "name: Acme\nroles:\n  - {name: \"Agent CEO\"}\n  - {name: \"agent ceo\"}\n"},
		{name: "a ceiling below its base", tier: TierCompany, yaml: "name: Acme\nturn_engine: {max_tool_rounds: 20, execute_max_tool_rounds_ceiling: 10}\n"},
	}
}

// The artifact has to work on the config it ships beside. The example
// carries the `# yaml-language-server: $schema=` modeline, so a schema that
// rejected it would put red underlines through the file a new operator
// copies first.
func TestShippedExampleValidatesAgainstTheSchema(t *testing.T) {
	t.Parallel()
	data, err := os.ReadFile(filepath.Join(repoRoot, "examples", "nimbus.company.yaml"))
	if err != nil {
		t.Skipf("the example tree is not in this checkout: %v", err)
	}
	if err := compileSchema(t, TierCompany).Validate(asJSON(t, string(data))); err != nil {
		t.Fatalf("the shipped example does not satisfy its own schema:\n%v", err)
	}
}

func compileSchema(t *testing.T, tier Tier) *jsonschema.Schema {
	t.Helper()
	data, err := Schema(tier)
	if err != nil {
		t.Fatal(err)
	}
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	c := jsonschema.NewCompiler()
	url := "https://crewlet.test/" + string(tier) + ".json"
	if err := c.AddResource(url, doc); err != nil {
		t.Fatal(err)
	}
	schema, err := c.Compile(url)
	if err != nil {
		t.Fatalf("the generated %s schema does not compile: %v", tier, err)
	}
	return schema
}

// asJSON converts a YAML document into the plain values a JSON Schema
// validator consumes.
func asJSON(t *testing.T, doc string) any {
	t.Helper()
	var raw any
	if err := yaml.Unmarshal([]byte(doc), &raw); err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(raw)
	if err != nil {
		t.Fatal(err)
	}
	value, err := jsonschema.UnmarshalJSON(bytes.NewReader(encoded))
	if err != nil {
		t.Fatal(err)
	}
	return value
}

func validateTier(tier Tier, doc string) error {
	if tier == TierBootstrap {
		_, err := ParseBootstrap([]byte(doc), EnvOnly())
		return err
	}
	_, err := ParseCompany([]byte(doc))
	return err
}

func schemaDoc(t *testing.T, tier Tier) map[string]any {
	t.Helper()
	data, err := Schema(tier)
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatal(err)
	}
	return doc
}

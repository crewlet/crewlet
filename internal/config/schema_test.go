package config

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/santhosh-tekuri/jsonschema/v6"
	"gopkg.in/yaml.v3"

	"github.com/crewlet/crewlet/internal/logging"
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
		// TWO DIFFERENT CLOSED SETS, deliberately. A seat may name `self`
		// — code work inside its own agent-mode executor run — and a
		// company default may not, because it would silently turn code
		// work off for every seat that is not in agent mode.
		{"SandboxProvider", "default_run_in", strs(BackendPlacements())},
		{"CLIAgent", "run_in", strs(BackendPlacements())},
		{"CLIAgent", "mode", strs(CLIAgentModes)},
		{"SandboxProvider", "default_coding_agent", strs(CodingAgents)},
		{"RoleSandbox", "run_in", strs(Placements)},
		{"LocalSandbox", "runtime", strs(ContainerRuntimes)},
		{"MCPServer", "transport", strs(MCPTransports)},
		{"Slack", "typing_status", strs(WorkingStatuses)},
		{"Mattermost", "typing_status", strs(WorkingStatuses)},
		{"GitLabProvisioning", "access_level", strs(GitLabAccessLevels)},
		{"GitLabProvisioning", "group_webhook", strs(ContainerWebhookModes)},
		// The same closed set on both code hosts, which is why it is one
		// type — and why both rows are here: a rename that updated one
		// field's tag and not the other would leave two enums that agree
		// today and drift on the next value.
		{"GitHubProvisioning", "org_webhook", strs(ContainerWebhookModes)},
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
		got := closedSet(t, field)
		if strings.Join(got, ",") != strings.Join(tc.want, ",") {
			t.Fatalf("%s.%s enum = %v, validators accept %v", tc.def, tc.field, got, tc.want)
		}
	}
}

// The Tier A enums, read from the SAME closed sets internal/logging exports
// and internal/config validates against. A hand-written `js:"enum=..."` tag
// is a second spelling of those slices: drift here is an editor offering
// `logging.format: pretty`, an operator writing it, and the engine refusing
// to boot on a value its own schema said was fine.
func TestBootstrapSchemaEnumsMatchTheValidators(t *testing.T) {
	t.Parallel()
	defs, _ := schemaDoc(t, TierBootstrap)["$defs"].(map[string]any)

	for _, tc := range []struct {
		def, field string
		want       []string
	}{
		{"Logging", "level", strs(logging.Levels)},
		{"Logging", "format", strs(logging.Formats)},
		// The file sink carries its own shape AND its own level — the
		// whole reason it is a second handler rather than a tee — so each
		// has its own enum to drift.
		{"LogFile", "format", strs(logging.Formats)},
		{"LogFile", "level", strs(logging.Levels)},
	} {
		def, ok := defs[tc.def].(map[string]any)
		if !ok {
			t.Fatalf("$defs has no %s", tc.def)
		}
		props, _ := def["properties"].(map[string]any)
		field, ok := props[tc.field].(map[string]any)
		if !ok {
			t.Fatalf("%s has no %s", tc.def, tc.field)
		}
		got := closedSet(t, field)
		if strings.Join(got, ",") != strings.Join(tc.want, ",") {
			t.Fatalf("%s.%s enum = %v, validators accept %v", tc.def, tc.field, got, tc.want)
		}
	}
}

// ONE NODE-ID RULE, in three places that cannot import each other: the
// validator's nodeIDPattern, node.id's js tag and role.placement.node's. A
// pin is compared with a node's id exactly, so the two tags are the same rule
// by construction — and a js tag is a second spelling of the regexp, which
// drifts the day one of them changes. Drift one way puts a red underline on a
// pin the engine accepts; the other way lets an editor pass a pin no node can
// ever satisfy, which the engine then refuses on apply.
func TestNodeIDPatternIsOneRule(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		tier       Tier
		def, field string
	}{
		{TierBootstrap, "Node", "id"},
		{TierCompany, "RolePlacement", "node"},
	} {
		defs, _ := schemaDoc(t, tc.tier)["$defs"].(map[string]any)
		def, ok := defs[tc.def].(map[string]any)
		if !ok {
			t.Fatalf("the %s schema's $defs has no %s", tc.tier, tc.def)
		}
		props, _ := def["properties"].(map[string]any)
		field, ok := props[tc.field].(map[string]any)
		if !ok {
			t.Fatalf("%s has no %s", tc.def, tc.field)
		}
		if got, want := ownPattern(t, field), nodeIDPattern.String(); got != want {
			t.Fatalf("%s.%s pattern = %v, the validator's nodeIDPattern is %q",
				tc.def, tc.field, got, want)
		}
	}
}

// closedSet reads a text field's closed set back out of its fragment: the
// STRING members of its enum, wherever the fragment puts it, less the empty
// string that spells "unset".
//
// Only the strings, because the enum also carries what YAML resolves a value
// to (`false` beside "false") and null. Those are forms of the set, not
// members of it, and TestEveryPositionAdmitsWhatTheDecoderReads is what holds
// them to the decoder.
func closedSet(t *testing.T, field map[string]any) []string {
	t.Helper()
	for _, candidate := range append([]any{field}, anyOf(field)...) {
		branch, _ := candidate.(map[string]any)
		raw, ok := branch["enum"].([]any)
		if !ok {
			continue
		}
		var out []string
		for _, v := range raw {
			if s, ok := v.(string); ok && s != "" {
				out = append(out, s)
			}
		}
		return out
	}
	t.Fatalf("the fragment carries no enum: %v", field)
	return nil
}

// ownPattern reads a text field's OWN pattern back out of its fragment — the
// one its js tag states, not the reference grammar a Tier A leaf also admits.
func ownPattern(t *testing.T, field map[string]any) string {
	t.Helper()
	if p, ok := field["pattern"].(string); ok {
		return p
	}
	for _, candidate := range anyOf(field) {
		branch, _ := candidate.(map[string]any)
		if p, ok := branch["pattern"].(string); ok && p != referencePattern {
			return p
		}
	}
	t.Fatalf("the fragment carries no pattern of its own: %v", field)
	return ""
}

func anyOf(field map[string]any) []any {
	branches, _ := field["anyOf"].([]any)
	return branches
}

// parityCase is one document run through both layers.
//
// Exactly one of the two flags may be set, and the pair is what makes a case
// able to FAIL: with neither, a document is asserted to survive both layers,
// so a fixture that quietly stops validating is caught here rather than
// sitting in the table proving nothing.
type parityCase struct {
	name string
	tier Tier
	yaml string
	// editorCatches marks a mistake the schema is expected to flag on its
	// own — the reason the artifact exists. The validator must refuse it too.
	editorCatches bool
	// validatorOnly marks a document the schema must LET THROUGH and the
	// validator must refuse: a rule a JSON Schema cannot express. Asserting
	// the schema's silence is the point — a case that drifts into being
	// schema-rejected stops covering the rule it names.
	validatorOnly bool
	// env is what a Tier A document's ${VAR}s resolve to, and nothing else
	// is consulted: a case reading the machine's own environment would
	// prove something different on every machine — `${HOSTNAME}` is a valid
	// node id on one host and not on the next. Tier B resolves nothing at
	// load, so a Tier B case carrying one is a mistake the test refuses.
	env map[string]string
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
			if tc.tier == TierCompany && tc.env != nil {
				t.Fatal("Tier B stores a ${VAR} verbatim and resolves nothing at load: an env on this case is never read")
			}
			schemaErr := compiled[tc.tier].Validate(asJSON(t, tc.yaml))
			validatorErr := validateTier(tc.tier, tc.yaml, tc.env)

			if schemaErr != nil && validatorErr == nil {
				t.Fatalf("the schema rejects a config the engine accepts — an "+
					"editor would flag a working file:\n%v", schemaErr)
			}
			switch {
			case tc.editorCatches:
				if schemaErr == nil {
					t.Fatal("the schema should catch this while the author is still typing")
				}
				if validatorErr == nil {
					t.Fatal("the schema flags this but the engine accepts it")
				}
			case tc.validatorOnly:
				if schemaErr != nil {
					t.Fatalf("this case is here to prove the schema LETS this "+
						"through — it now rejects it, so it no longer covers "+
						"the rule it names:\n%v", schemaErr)
				}
				if validatorErr == nil {
					t.Fatal("the engine must refuse this, or the case proves nothing")
				}
			default:
				if validatorErr != nil {
					t.Fatalf("this document is in the table as a WORKING config "+
						"and the engine refuses it:\n%v", validatorErr)
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
		// Genuinely empty: every Tier A field has a default, so a
		// document that sets nothing must survive both layers. It used to
		// carry one throwaway key, which meant the case proved that key
		// parsed rather than that the defaults stand on their own.
		//
		// `{}` rather than a zero-byte document because that decodes to
		// YAML null, which the schema refuses at the root ("got null,
		// want object") while the loader accepts it. That gap is real but
		// it is not this case's subject, and an empty MAPPING is what an
		// operator's "empty" crewlet.yaml actually looks like.
		{name: "empty bootstrap", tier: TierBootstrap, yaml: "{}\n"},
		{name: "bootstrap logging block", tier: TierBootstrap, yaml: "logging:\n  level: warn\n  format: json\n"},
		{
			name: "bootstrap log file block",
			tier: TierBootstrap,
			yaml: "logging:\n  level: warn\n  format: console\n  stderr: false\n" +
				"  file:\n    path: /var/log/crewlet/crewlet.log\n" +
				"    format: json\n    level: debug\n" +
				"    max_size_mb: 50\n    max_backups: 0\n",
		},
		// A NODE THAT LOGS NOWHERE — a cross-field implication the schema
		// is not asked to carry, so the validator is the only layer that
		// refuses it.
		{
			name:          "stderr off with no file",
			tier:          TierBootstrap,
			yaml:          "logging:\n  stderr: false\n",
			validatorOnly: true,
		},
		{
			name:          "an unknown level for the file",
			tier:          TierBootstrap,
			yaml:          "logging:\n  file:\n    path: /tmp/c.log\n    level: dbug\n",
			editorCatches: true,
		},
		// `max_backups: 0` above is the setting that must survive BOTH
		// layers: it is the zero value of its own type, and a schema or a
		// validator reading it as "unset" would hand a capped disk five
		// files it asked not to have.
		{
			name:          "a log file that rotates every zero bytes",
			tier:          TierBootstrap,
			yaml:          "logging:\n  file:\n    path: /tmp/crewlet.log\n    max_size_mb: 0\n",
			editorCatches: true,
		},
		{
			name:          "a negative backup count",
			tier:          TierBootstrap,
			yaml:          "logging:\n  file:\n    path: /tmp/crewlet.log\n    max_backups: -1\n",
			editorCatches: true,
		},
		// A SHAPE WITH NO FILE TO WRITE IT TO. A cross-field implication
		// the schema is not asked to carry, so the validator is the only
		// layer that refuses it.
		{
			name:          "a log file shape with no path",
			tier:          TierBootstrap,
			yaml:          "logging:\n  file:\n    format: json\n",
			validatorOnly: true,
		},
		{
			name: "a full company",
			tier: TierCompany,
			yaml: `
name: Acme
mission: ship
policies: [write things down]
timezone: Europe/Berlin
token_budget: {day: 1000000, week: 5000000, month: 20000000}
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
    local: {image: acme/box, runtime: auto}
    default_run_in: container
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
scheduling: {enabled: true, tick_seconds: 10}
integrations:
  mattermost:
    enabled: true
    url: https://mm.example.com
    team: acme
    typing_status: addressed
    provisioning: {username_prefix: agent-, channels: [town-square], display_name_suffix: " (AI)"}
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
    project: ENG
    roles:
      - name: CTO
        handle: cto
        integrations: {mattermost: {bot_token: "${MM_CTO}", username: cto-bot}}
        sandbox:
          enabled: true
          env: {GITHUB_TOKEN: "${GH_CTO}"}
          mcp: {servers: [gitlab]}
`,
		},
		{
			name: "a three-node fleet",
			tier: TierBootstrap,
			yaml: "coordination:\n  type: embedded-kv\nstream:\n  replicas: 3\n  store_dir: /var/lib/crewlet/stream\n" +
				"  cluster:\n    name: acme\n    peers: [nats://b:6222, nats://c:6222]\n",
		},
		// A LITERAL signing secret, not a ${VAR}. The full company above
		// carries the reference form, which validate() deliberately does not
		// inspect — so without this document nothing proves a correctly
		// shaped secret survives the whsec_<base64 of 32 bytes> check that
		// the rejection case further down relies on.
		{
			name: "a literal gitlab signing secret", tier: TierCompany,
			yaml: "name: Acme\nintegrations:\n  gitlab:\n    enabled: true\n    url: https://gitlab.example.com\n" +
				"    signing_secret: \"whsec_YS1maXh0dXJlLXNpZ25pbmcta2V5LW9mLTMyYnl0ZXM=\"\n",
		},

		{
			name: "a company's files in an S3 bucket", tier: TierBootstrap,
			yaml: "store:\n  objects:\n    backend: s3\n    s3:\n      bucket: files\n" +
				"      region: auto\n      endpoint: https://example.com\n      prefix: acme/\n",
		},
		{
			name: "a five-member fleet at the broker's ceiling", tier: TierBootstrap,
			yaml: "coordination:\n  type: embedded-kv\nstream:\n  replicas: 5\n  store_dir: /var/lib/crewlet/stream\n  cluster:\n" +
				"    name: acme\n    peers: [nats://b:6222, nats://c:6222, nats://d:6222, nats://e:6222]\n",
		},

		// A PIN IS A NODE ID, and both layers hold it to node.id's own
		// rule: a pin outside it names a node that cannot exist.
		{
			name: "a seat pinned to a node", tier: TierCompany,
			yaml: "name: Acme\nroles:\n  - {name: Builder, placement: {node: builder-1.eu_west}}\n",
		},

		// TIER A RESOLVES BEFORE IT DECODES, so a reference is judged by what
		// it resolves to. Every closed set and pattern Tier A states is here
		// once, resolving to a value the engine takes — the id the deployment
		// guide tells an operator to write first among them.
		{
			name: "a node id from the environment", tier: TierBootstrap,
			yaml: "node:\n  id: \"${HOSTNAME}\"\n",
			env:  map[string]string{"HOSTNAME": "builder-1"},
		},
		// Embedded, not whole: the resolver expands a reference anywhere in
		// a value, so a schema admitting only a whole one underlined this.
		{
			name: "a node id built around a reference", tier: TierBootstrap,
			yaml: "node:\n  id: \"edge-${ZONE}\"\n",
			env:  map[string]string{"ZONE": "eu"},
		},
		{
			name: "a log block from the environment", tier: TierBootstrap,
			yaml: "logging:\n  level: \"${LOG_LEVEL}\"\n  format: \"${LOG_FORMAT}\"\n" +
				"  file:\n    path: /tmp/crewlet.log\n    level: \"${FILE_LEVEL}\"\n    format: \"${FILE_FORMAT}\"\n",
			env: map[string]string{"LOG_LEVEL": "debug", "LOG_FORMAT": "json", "FILE_LEVEL": "warn", "FILE_FORMAT": "text"},
		},
		{
			name: "a backup floor from the environment", tier: TierBootstrap,
			yaml: "stream:\n  tracker_retention:\n    backup_floor: \"${BACKUP_FLOOR}\"\n",
			env:  map[string]string{"BACKUP_FLOOR": "operator"},
		},
		{
			name: "a keyring from the environment", tier: TierBootstrap,
			yaml: "secrets:\n  active_key_id: \"${KEY_ID}\"\n  keys:\n    - {id: \"${KEY_ID}\", material: \"${KEY}\"}\n",
			env:  map[string]string{"KEY_ID": "k1", "KEY": "YS1maXh0dXJlLWtleS1vZi10aGlydHktdHdvLWJ5dGU="},
		},
		// A REFERENCE READ AS "NOT EMBEDDED-KV" made the fleet rule fire:
		// the rule refuses a fleet on local coordination, and it took any
		// type it could not see to be local.
		{
			name: "a fleet whose coordination is the environment's", tier: TierBootstrap,
			yaml: "coordination:\n  type: \"${COORDINATION}\"\nstream:\n  store_dir: /var/lib/crewlet/stream\n  cluster:\n    name: acme\n" +
				"    peers: [nats://b:6222, nats://c:6222]\n",
			env: map[string]string{"COORDINATION": "embedded-kv"},
		},
		// And the same rule's stream half demanded a LITERAL embedded.
		{
			name: "an empty stream type on one node", tier: TierBootstrap,
			yaml: "stream:\n  type: \"\"\n",
		},
		{
			name: "a stream type from the environment on one node", tier: TierBootstrap,
			yaml: "stream:\n  type: \"${STREAM_TYPE}\"\n",
			env:  map[string]string{"STREAM_TYPE": "embedded"},
		},
		{
			name: "an external stream named by the environment", tier: TierBootstrap,
			yaml: "coordination:\n  type: embedded-kv\nstream:\n  type: \"${STREAM_TYPE}\"\n  url: \"${NATS_URL}\"\n",
			env:  map[string]string{"STREAM_TYPE": "nats", "NATS_URL": "nats://nats.example.com:4222"},
		},
		// A NUMBER OR A SWITCH TAKES A WHOLE REFERENCE, whose value is read
		// as the same characters written there would be: `8080` is a port,
		// `true` and `yes` are both a switch. Each used to be refused — a
		// substituted value was a string, which no number field decodes and
		// a bool field decodes only for the YAML 1.1 words.
		{
			name: "a flag from the environment", tier: TierBootstrap,
			yaml: "stream:\n  debug: \"${STREAM_DEBUG}\"\n",
			env:  map[string]string{"STREAM_DEBUG": "yes"},
		},
		{
			name: "a flag from the environment that says true", tier: TierBootstrap,
			yaml: "stream:\n  debug: \"${STREAM_DEBUG}\"\n",
			env:  map[string]string{"STREAM_DEBUG": "true"},
		},
		{
			name: "a port from the environment", tier: TierBootstrap,
			yaml: "api:\n  port: \"${API_PORT}\"\n",
			env:  map[string]string{"API_PORT": "8080"},
		},
		{
			name: "a lease TTL from the environment", tier: TierBootstrap,
			yaml: "coordination:\n  lease_ttl_seconds: \"${LEASE_TTL}\"\n",
			env:  map[string]string{"LEASE_TTL": "30"},
		},

		// COPIES LIVE ON MEMBERS THIS FILE DOES NOT NAME on an external
		// stream and on a leaf, and the validator counts neither — a rule
		// demanding peers of them refused every external fleet that kept
		// more than one copy, and demanded a list the validator refuses
		// under an external stream.
		{
			name: "an external fleet keeping three copies", tier: TierBootstrap,
			yaml: "coordination:\n  type: embedded-kv\nstream:\n  type: nats\n" +
				"  url: nats://nats.example.com:4222\n  replicas: 3\n",
		},
		{
			name: "a leaf keeping the fleet's three copies", tier: TierBootstrap,
			yaml: "coordination:\n  type: embedded-kv\nnode:\n  roles: [seats]\nstore:\n  scratch: true\n" +
				"stream:\n  replicas: 3\n  leaf:\n    urls: [nats://member.example.com:7422]\n",
		},

		// AN EMPTY VALUE IS UNSET, written either way, at every level — and
		// a rule built from `properties` alone matches a null block, which
		// is why each condition states the object it looks inside.
		{name: "a bootstrap that is YAML null", tier: TierBootstrap, yaml: "~\n"},
		{
			name: "an empty stream block on a solo fleet member", tier: TierBootstrap,
			yaml: "coordination:\n  type: embedded-kv\nstream:\n  cluster:\n",
		},
		{
			name: "empty log settings", tier: TierBootstrap,
			yaml: "logging:\n  level: \"\"\n  format:\n",
		},
		{
			name: "empty company settings", tier: TierCompany,
			yaml: "name: Acme\nmission:\nroles:\n  - {name: CEO, handle: \"\", llm: ~}\n",
		},

		// TEXT IS TEXT, whatever YAML resolved it as: the decoder hands a
		// string field the characters, so each of these is a string to the
		// engine and a boolean or a number to an editor. The first is the
		// spelling the GitHub guide itself uses.
		{
			name: "the documented org_webhook spelling", tier: TierCompany,
			yaml: "name: Acme\nintegrations:\n  github: {enabled: true, webhook_secret: s, provisioning: {org: acme, org_webhook: false}}\n",
		},
		{
			name: "a group webhook written as a boolean", tier: TierCompany,
			yaml: "name: Acme\nintegrations:\n  gitlab: {enabled: true, url: https://gitlab.example.com, " +
				"signing_secret: \"${GITLAB_SIGNING_SECRET}\", provisioning: {group: acme, group_webhook: true}}\n",
		},
		{name: "a company name YAML reads as a number", tier: TierCompany, yaml: "name: 2024\n"},
		{
			name: "node facts YAML reads as other types", tier: TierBootstrap,
			yaml: "node:\n  id: 7\n  labels: {gpu: true, rack: 12}\n",
		},

		// A BOOLEAN READS YAML 1.1's WORDS too — the decoder keeps them.
		{name: "a flag written as a word", tier: TierBootstrap, yaml: "stream:\n  debug: off\n"},
		{name: "a toggle written as a word", tier: TierCompany, yaml: "name: Acme\nlearning:\n  enabled: yes\n"},

		// Mistakes an editor should catch inline.
		{name: "unknown top-level key", tier: TierCompany, yaml: "name: Acme\nmisson: typo\n", editorCatches: true},
		{
			name: "a pin no node id can equal", tier: TierCompany, editorCatches: true,
			yaml: "name: Acme\nroles:\n  - {name: Builder, placement: {node: \"builder 1\"}}\n",
		},
		// Tier B keeps a ${VAR} verbatim and a pin is compared as written,
		// so a reference here is the literal node id "${…}".
		{
			name: "a pin written as a reference", tier: TierCompany, editorCatches: true,
			yaml: "name: Acme\nroles:\n  - {name: Builder, placement: {node: \"${BUILDER_NODE}\"}}\n",
		},
		{
			name: "a misspelt objects key", tier: TierBootstrap, editorCatches: true,
			yaml: "store:\n  objects:\n    backnd: s3\n",
		},
		{
			name: "more stream copies than JetStream keeps", tier: TierBootstrap, editorCatches: true,
			yaml: "stream:\n  replicas: 6\n",
		},
		// A BUCKET WITH NO REGION — a field the s3 backend needs and
		// every other backend refuses, which is a cross-field rule the
		// schema is not asked to carry.
		{
			name: "an s3 backend with no region", tier: TierBootstrap, validatorOnly: true,
			yaml: "store:\n  objects:\n    backend: s3\n    s3:\n      bucket: files\n",
		},
		// MORE COPIES THAN THE CLUSTER NAMES MEMBERS — a cross-field
		// bound the schema is not asked to carry.
		// THIS NODE'S OWN ROUTE IN ITS PEERS is not another member, and
		// recognising it takes a comparison with cluster.host and
		// cluster.port that a JSON Schema cannot make: two entries read as
		// three members to the schema and as two to the engine.
		{
			name: "a two-node fleet that lists itself", tier: TierBootstrap, validatorOnly: true,
			yaml: "coordination:\n  type: embedded-kv\nstream:\n  cluster:\n    name: acme\n" +
				"    port: 6222\n    host: 10.0.0.11\n    peers: [nats://10.0.0.11:6222, nats://b:6222]\n",
		},
		// A PEER NO SERVER CAN DIAL — the schema says a string.
		{
			name: "a peer with no port", tier: TierBootstrap, validatorOnly: true,
			yaml: "coordination:\n  type: embedded-kv\nstream:\n  cluster:\n    name: acme\n" +
				"    peers: [nats://b, nats://c:6222]\n",
		},
		{
			name: "four stream copies on three members", tier: TierBootstrap, validatorOnly: true,
			yaml: "coordination:\n  type: embedded-kv\nstream:\n  replicas: 4\n  cluster:\n" +
				"    name: acme\n    peers: [nats://b:6222, nats://c:6222]\n",
		},
		{name: "missing company name", tier: TierCompany, yaml: "mission: ship\n", editorCatches: true},
		// AN EMPTY TYPE IS THE DEFAULT ONE to the loader, and so to the
		// fleet rules here: an empty coordination type is local, and an
		// empty stream type is embedded and names no members for copies.
		{
			name: "a fleet on an empty coordination type", tier: TierBootstrap, editorCatches: true,
			yaml: "coordination:\n  type: \"\"\nstream:\n  cluster:\n    name: acme\n    peers: [nats://b:6222, nats://c:6222]\n",
		},
		{
			name: "copies on an empty stream type with no peers", tier: TierBootstrap, editorCatches: true,
			yaml: "stream:\n  type: \"\"\n  replicas: 3\n",
		},
		// BUT ONLY A WHOLE ONE. A number or a switch composed out of text
		// would have a type decided by how two strings concatenate, so the
		// resolver refuses a reference with anything around it there — and
		// the schema says so while the author types.
		{
			name: "a port built around a reference", tier: TierBootstrap, editorCatches: true,
			yaml: "api:\n  port: \"80${PORT_TAIL}\"\n",
			env:  map[string]string{"PORT_TAIL": "80"},
		},
		{
			name: "a flag built around a reference", tier: TierBootstrap, editorCatches: true,
			yaml: "stream:\n  debug: \"y${FLAG_TAIL}\"\n",
			env:  map[string]string{"FLAG_TAIL": "es"},
		},
		// A FRACTION IN A WHOLE-NUMBER FIELD was truncated by the decoder —
		// 8080.9 ran as 8080. It is refused at load now, so `integer` in the
		// schema is exactly what the engine reads.
		{
			name: "a fractional port", tier: TierBootstrap, editorCatches: true,
			yaml: "api:\n  port: 8080.9\n",
		},
		// Tier B keeps a reference VERBATIM, so in a closed set or a
		// pattern it is the literal text "${…}", which neither admits.
		{
			name: "a closed set written as a reference", tier: TierCompany, editorCatches: true,
			yaml: "name: Acme\nintegrations:\n  slack: {typing_status: \"${TYPING_STATUS}\"}\n",
		},
		{
			name: "a handle written as a reference", tier: TierCompany, editorCatches: true,
			yaml: "name: Acme\nroles:\n  - {name: CEO, handle: \"${CEO_HANDLE}\"}\n",
		},
		{
			name: "a unit id written as a reference", tier: TierCompany, editorCatches: true,
			yaml: "name: Acme\nunits:\n  - {name: Core, id: \"${UNIT_ID}\"}\n",
		},
		// A POINTER FIELD is the one Tier B exception: its consumer resolves
		// one WHOLE reference, so the schema admits exactly that beside the
		// field's own rule — and nothing looser, because a reference inside
		// other text is sent to the server as written.
		{
			name: "a Mattermost username named by a reference", tier: TierCompany,
			yaml: "name: Acme\nroles:\n  - {name: CTO, integrations: {mattermost: " +
				"{bot_token: \"${MM_CTO}\", username: \"${MM_CTO_USERNAME}\"}}}\n",
		},
		{
			name: "a Mattermost username with a reference inside it", tier: TierCompany, editorCatches: true,
			yaml: "name: Acme\nroles:\n  - {name: CTO, integrations: {mattermost: " +
				"{bot_token: \"${MM_CTO}\", username: \"bot-${MM_SUFFIX}\"}}}\n",
		},
		{
			name: "space around a Mattermost username's reference", tier: TierCompany, editorCatches: true,
			yaml: "name: Acme\nroles:\n  - {name: CTO, integrations: {mattermost: " +
				"{bot_token: \"${MM_CTO}\", username: \" ${MM_CTO_USERNAME}\"}}}\n",
		},
		// A token has no pattern for the schema to hang the rule on, so the
		// same refusal on a bot token is the validator's alone.
		{
			name: "a Mattermost bot token with a reference inside it", tier: TierCompany, validatorOnly: true,
			yaml: "name: Acme\nroles:\n  - {name: CTO, integrations: {mattermost: {bot_token: \"tok-${MM_CTO}\"}}}\n",
		},
		{
			name: "a Slack bot token with a reference inside it", tier: TierCompany, validatorOnly: true,
			yaml: "name: Acme\nroles:\n  - {name: CEO, integrations: {slack: " +
				"{bot_token: \"xoxb-${SLACK_CEO}\", signing_secret: \"${S}\"}}}\n",
		},
		// The company's root is the one position a null is refused at: an
		// empty company document is refused by name.
		{name: "a company that is YAML null", tier: TierCompany, yaml: "~\n", editorCatches: true},

		{
			name: "unknown provider type", tier: TierCompany, editorCatches: true,
			yaml: "name: Acme\nproviders:\n  llm:\n    default: {type: openai-compatable, model: m, base_url: https://x}\n",
		},
		{
			name: "unknown placement", tier: TierCompany, editorCatches: true,
			yaml: "name: Acme\nproviders:\n  sandbox: {local: {image: x}, default_run_in: chroot}\n",
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
		// A TOKEN BUDGET IS A MAPPING OF WINDOWS, and both layers say so
		// while the author is still typing: the one-number form every
		// example used to show, a 0 that is no longer "unlimited", and a
		// window the mapping does not have.
		{
			name: "a token budget that is one number", tier: TierCompany, editorCatches: true,
			yaml: "name: Acme\ntoken_budget: 1000000\n",
		},
		{
			name: "a seat's daily ceiling of 0", tier: TierCompany, editorCatches: true,
			yaml: "name: Acme\nroles:\n  - {name: Dev, token_budget: {day: 0}}\n",
		},
		{
			name: "a token budget window that does not exist", tier: TierCompany, editorCatches: true,
			yaml: "name: Acme\ntoken_budget: {year: 1000000}\n",
		},
		// And the RELATIONS between ceilings are only ever warnings, so
		// neither layer may refuse a budget whose week can never bind.
		{
			name: "a week ceiling seven days already reach", tier: TierCompany,
			yaml: "name: Acme\ntoken_budget: {day: 100, week: 700}\n",
		},
		// EVERY VENDOR BLOCK IS SERVED NOW, so what these cases pin is
		// the other direction: neither layer may refuse a config the
		// engine boots on. The schema because it would underline a
		// working file, the validator because the engine runs it.
		//
		// The knowledge base, and its own read scope beside it — a scope
		// with no backend is a validator rule a JSON Schema cannot
		// express, because it turns on a block's mere presence.
		{
			name: "a confluence knowledge base", tier: TierCompany,
			yaml: "name: Acme\nintegrations:\n  confluence: {url: \"https://wiki.example.com\", token: t, webhook_secret: s}\nknowledge:\n  scope: [HANDBOOK]\n",
		},
		// The hosted chat surface, which IS served. Neither layer may
		// refuse it: the schema because it would underline a working
		// file, the validator because the engine boots on it.
		{
			name: "a slack working indicator", tier: TierCompany,
			yaml: "name: Acme\nintegrations:\n  slack: {typing_status: addressed}\n",
		},
		// The hosted code host, which IS served — with an Enterprise
		// Server address, since github.com is the shape that carries no
		// url at all and would prove nothing about the field.
		{
			name: "a github enterprise deployment", tier: TierCompany,
			yaml: "name: Acme\nintegrations:\n  github: {enabled: true, url: \"https://github.example.com\", webhook_secret: s, provisioning: {org: acme, repos: [acme/engine], org_webhook: auto}}\n",
		},
		{
			name: "a github repo that is not owner/repo", tier: TierCompany,
			validatorOnly: true,
			yaml:          "name: Acme\nintegrations:\n  github: {enabled: true, webhook_secret: s, provisioning: {repos: [engine]}}\n",
		},
		// The Atlassian tracker, which IS served: a Cloud site named by its
		// cloud id, delivering through the Forge app. Neither layer may
		// refuse it — the schema because it would underline a working file,
		// the validator because the engine boots on it.
		{
			name: "a jira cloud site behind a forge app", tier: TierCompany,
			yaml: "name: Acme\nintegrations:\n  jira: {cloud_id: acme-cloud, token: \"${T}\"}\n  forge_app_id: acme-forge\n",
		},
		// The url-or-cloud-id rule is a validator one: a JSON Schema can
		// express "one of these two" only by restructuring the object, and
		// the restructured shape gives an author a worse message than the
		// error does.
		{
			name: "a jira block naming its instance twice", tier: TierCompany, validatorOnly: true,
			yaml: "name: Acme\nintegrations:\n  jira: {url: \"https://jira.example.com\", cloud_id: acme-cloud, token: t, webhook_secret: s}\n",
		},
		// The per-seat half: an app with BOTH credentials is a working
		// config, and one with a single half is a validator rule a JSON
		// Schema cannot express (a required pair inside an optional
		// object).
		{
			name: "a per-seat Slack app", tier: TierCompany,
			yaml: "name: Acme\nroles:\n  - {name: CEO, integrations: {slack: {bot_token: \"${T}\", signing_secret: \"${S}\"}}}\n",
		},
		{
			name: "a per-seat Slack app with half its credentials",
			tier: TierCompany, validatorOnly: true,
			yaml: "name: Acme\nroles:\n  - {name: CEO, integrations: {slack: {bot_token: \"${T}\"}}}\n",
		},
		{
			// validatorOnly, and for one reason: the schema cannot express
			// "this setting needs that backend" — the rule turns on a value
			// elsewhere in the document, and a clause that guessed would
			// underline configs the engine runs.
			//
			// A scope with no backend NAMED is deliberately not this case
			// any more: it derives the native backend, where a curated
			// floor over the company's own pages is ordinary.
			name: "a read scope for a knowledge base that is switched off",
			tier: TierCompany, validatorOnly: true,
			yaml: "name: Acme\nknowledge:\n  backend: none\n  scope: [HANDBOOK]\n",
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
		// A PEER LIST NAMING ONLY THIS NODE. The schema's two-node rule
		// counts ENTRIES, since it cannot compare one with this node's
		// address — so the validator must refuse this one too, or the
		// schema flags a file the engine runs.
		{
			name: "a peer list naming only this node", tier: TierBootstrap, editorCatches: true,
			yaml: "coordination:\n  type: embedded-kv\nstream:\n  cluster:\n    name: acme\n" +
				"    port: 6222\n    host: 10.0.0.11\n    peers: [nats://10.0.0.11:6222]\n",
		},
		{
			name: "an external stream with no url", tier: TierBootstrap, editorCatches: true,
			yaml: "coordination:\n  type: embedded-kv\nstream:\n  type: nats\n",
		},

		// Rules the schema cannot express. They belong here to prove the
		// SOUNDNESS direction on documents the schema must let through and
		// the validator must not.
		//
		// The credential-shape case used to be a per-seat Slack app carrying
		// a signing_secret with no bot_token. That seat block is refused
		// outright now, so the schema stopped letting it through and the
		// case stopped proving anything; GitLab's webhook token carries the
		// same shape of rule on a third-party app this build serves: a format
		// contract with the third-party app, checked in Go, invisible to the
		// schema.
		{
			name: "a signing secret gitlab would refuse", tier: TierCompany, validatorOnly: true,
			// Deliberately NOT a whsec_ token: GitLab accepts only
			// whsec_<standard base64 of 32 bytes>, and a bare shared secret
			// is what an operator reaches for first. The schema sees a
			// string and has nothing to say.
			yaml: "name: Acme\nintegrations:\n  gitlab: {enabled: true, url: https://gitlab.example.com, signing_secret: plain-shared-secret}\n",
		},
		// WHAT A REFERENCE RESOLVES TO is the node's business, so the schema
		// admits one wherever some value would do and the validator judges
		// the value it got.
		{
			name: "a log level from the environment that is not one", tier: TierBootstrap, validatorOnly: true,
			yaml: "logging:\n  level: \"${LOG_LEVEL}\"\n",
			env:  map[string]string{"LOG_LEVEL": "verbose"},
		},
		{
			name: "a node id from the environment that is not one", tier: TierBootstrap, validatorOnly: true,
			yaml: "node:\n  id: \"${HOSTNAME}\"\n",
			env:  map[string]string{"HOSTNAME": "builder 1"},
		},
		// And what a number's reference resolves to is the node's business
		// too: a fraction, a word, or nothing at all is refused at load, and
		// the schema, seeing only `${API_PORT}`, cannot tell.
		{
			name: "a port from the environment that is not a whole number", tier: TierBootstrap, validatorOnly: true,
			yaml: "api:\n  port: \"${API_PORT}\"\n",
			env:  map[string]string{"API_PORT": "8080.5"},
		},
		{
			name: "a port from the environment that is not a number", tier: TierBootstrap, validatorOnly: true,
			yaml: "api:\n  port: \"${API_PORT}\"\n",
			env:  map[string]string{"API_PORT": "http"},
		},
		{
			name: "a port from the environment that is empty", tier: TierBootstrap, validatorOnly: true,
			yaml: "api:\n  port: \"${API_PORT}\"\n",
			env:  map[string]string{"API_PORT": ""},
		},
		{name: "a stdio server with no command", tier: TierCompany, validatorOnly: true, yaml: "name: Acme\nmcp_servers:\n  - {name: calc}\n"},
		{name: "duplicate handles", tier: TierCompany, validatorOnly: true, yaml: "name: Acme\nroles:\n  - {name: \"Agent CEO\"}\n  - {name: \"agent ceo\"}\n"},
		{name: "duplicate seat names", tier: TierCompany, validatorOnly: true, yaml: "name: Acme\nroles:\n  - {name: Dev, handle: dev-one}\n  - {name: Dev, handle: dev-two}\n"},
		{name: "duplicate unit names", tier: TierCompany, validatorOnly: true, yaml: "name: Acme\nunits:\n  - {name: Core, children: [{name: Platform}]}\n  - {name: Edge, children: [{name: Platform}]}\n"},
		{name: "duplicate setup step names", tier: TierCompany, validatorOnly: true, yaml: "name: Acme\nproviders:\n  sandbox:\n    fake: true\n    setup:\n      - {name: registry, commands: [\"true\"]}\n      - {name: registry, commands: [\"true\"]}\n"},
		{name: "a ceiling below its base", tier: TierCompany, validatorOnly: true, yaml: "name: Acme\nturn_engine: {max_tool_rounds: 20, execute_max_tool_rounds_ceiling: 10}\n"},
	}
}

// The artifact has to work on the config it ships beside. The example
// carries the `# yaml-language-server: $schema=` modeline, so a schema that
// rejected it would put red underlines through the file a new operator
// copies first.
func TestShippedExamplesValidateAgainstTheSchema(t *testing.T) {
	t.Parallel()
	schema := compileSchema(t, TierCompany)
	// BOTH, because they exercise different halves of the model: the
	// full-stack one every integration block, the subscription one the
	// cli-agent provider, its sandbox cell and the seat-level `run_in`.
	for _, name := range shippedCompanies {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			data := readRepoFile(t, "examples", name)
			if err := schema.Validate(asJSON(t, string(data))); err != nil {
				t.Fatalf("examples/%s does not satisfy its own schema:\n%v", name, err)
			}
		})
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

// validateTier runs a document through the engine's own load path for its
// tier. Tier A resolves through env alone — the environment-only shape
// [EnvOnly] gives the real load, with the process environment replaced by
// the case's own, so a verdict cannot depend on the machine.
func validateTier(tier Tier, doc string, env map[string]string) error {
	if tier == TierBootstrap {
		_, err := ParseBootstrap([]byte(doc), NewResolver(MapSource(env)))
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

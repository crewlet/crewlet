package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"slices"
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
		// Slack's definition is still generated and still committed, even
		// though integrations.slack is refused (`not: {}`) and so nothing
		// references it today. It is asserted for the same reason the type
		// is kept: the def is where the vendor's schema lands when its
		// parser ships, and an enum that drifted in the meantime would
		// drift silently.
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

// refusedField is the pair of documents that pins one refusal in place: the
// same config with the setting OFF and with it ON.
type refusedField struct {
	tier    Tier
	off, on string
}

// THE PAIRS ARE WRITTEN HERE; THE LIST OF FIELDS IS DERIVED FROM THE MODELS.
//
// That asymmetry is the whole point. The parity table above is hand-written
// on both sides, so a field nobody thought to write down is a field nobody
// checks — and integrations.github is what that cost: the generator refused
// the KEY, so `github: {enabled: false}` was red-underlined by an editor and
// booted happily by the engine, in both directions of the invariant at once.
// A field tagged js:"unimplemented" with no pair below fails the sweep.
//
// The OFF document is the load-bearing half. Everyone remembers to prove a
// refusal refuses; the direction that breaks is the setting an operator uses
// to say "off", which the validator accepts and the schema must too.
func refusedDocuments() map[string]refusedField {
	return map[string]refusedField{
		"company:integrations.confluence": {tier: TierCompany,
			off: "name: Acme\n",
			on:  "name: Acme\nintegrations:\n  confluence: {url: https://acme.example.com/wiki, token: \"${CONFLUENCE_TOKEN}\"}\n",
		},
		"company:integrations.slack": {tier: TierCompany,
			off: "name: Acme\n",
			on:  "name: Acme\nintegrations:\n  slack: {typing_status: addressed}\n",
		},
		// The off document here is the bug, written down: a block carrying
		// its own switch is off when the switch is off, not when the key is
		// absent. An operator turning GitHub off leaves the block behind
		// with its secret reference intact, which is what makes turning it
		// back on a one-line change.
		"company:integrations.github": {tier: TierCompany,
			off: "name: Acme\nintegrations:\n  github: {enabled: false, webhook_secret: \"${GITHUB_WEBHOOK_SECRET}\"}\n",
			on:  "name: Acme\nintegrations:\n  github: {enabled: true, webhook_secret: \"${GITHUB_WEBHOOK_SECRET}\"}\n",
		},
		// A list is off when it is empty — an unscoped read scope, which
		// is the documented default rather than a mistake.
		"company:knowledge.confluence_spaces": {tier: TierCompany,
			off: "name: Acme\nknowledge:\n  confluence_spaces: []\n",
			on:  "name: Acme\nknowledge:\n  confluence_spaces: [HANDBOOK]\n",
		},
		// The same field, reached the other way. A seat inside a unit is
		// validated through the unit walk rather than the top-level one, and
		// a refusal that landed on only one of the two paths would leave
		// every real org chart — which is all units — unguarded.
		"company:units[].roles[].integrations.slack": {tier: TierCompany,
			off: "name: Acme\nunits:\n  - {name: Engineering, roles: [{name: CTO, integrations: {}}]}\n",
			on:  "name: Acme\nunits:\n  - {name: Engineering, roles: [{name: CTO, integrations: {slack: {bot_token: \"${T}\", signing_secret: \"${S}\"}}}]}\n",
		},
		// The wiki space a seat or a unit claims. Not a credential — WHERE
		// work files and where deliveries route — which is why leaving it
		// standing is the same silence as the org block: recorded,
		// rendered, never consulted. Three paths, because both a seat and a
		// unit can claim one and a seat can live in either place.
		"company:roles[].integrations.confluence": {tier: TierCompany,
			off: "name: Acme\nroles:\n  - {name: CEO, integrations: {}}\n",
			on:  "name: Acme\nroles:\n  - {name: CEO, integrations: {confluence: {space: HANDBOOK}}}\n",
		},
		"company:units[].integrations.confluence": {tier: TierCompany,
			off: "name: Acme\nunits:\n  - {name: Engineering, integrations: {}}\n",
			on:  "name: Acme\nunits:\n  - {name: Engineering, integrations: {confluence: {space: HANDBOOK}}}\n",
		},
		"company:units[].roles[].integrations.confluence": {tier: TierCompany,
			off: "name: Acme\nunits:\n  - {name: Engineering, roles: [{name: CTO, integrations: {}}]}\n",
			on:  "name: Acme\nunits:\n  - {name: Engineering, roles: [{name: CTO, integrations: {confluence: {space: HANDBOOK}}}]}\n",
		},
		"company:roles[].integrations.slack": {tier: TierCompany,
			off: "name: Acme\nroles:\n  - {name: CEO, integrations: {}}\n",
			on:  "name: Acme\nroles:\n  - {name: CEO, integrations: {slack: {bot_token: \"${T}\", signing_secret: \"${S}\"}}}\n",
		},
	}
}

// A REFUSAL MUST LAND WHERE THE VALIDATOR PUTS IT, NOT ON THE KEY.
//
// The schema and the validator each decide, independently, what "this build
// does not serve that" means for a given field. They agree today because
// [schemaGen.refuseWhenOn] reads the field's shape the same way the
// validators do. This is what holds them there, and it sweeps the models so
// a field added to one side cannot go unchecked on the other.
func TestEveryRefusedFieldIsRefusedWhereTheValidatorRefusesIt(t *testing.T) {
	t.Parallel()
	compiled := map[Tier]*jsonschema.Schema{
		TierBootstrap: compileSchema(t, TierBootstrap),
		TierCompany:   compileSchema(t, TierCompany),
	}
	roots := map[Tier]reflect.Type{
		TierBootstrap: reflect.TypeOf(Bootstrap{}),
		TierCompany:   reflect.TypeOf(Company{}),
	}
	if len(roots) != len(SchemaTiers) {
		t.Fatalf("a tier grew a root this sweep does not walk: %v", SchemaTiers)
	}

	var found []string
	for _, tier := range SchemaTiers {
		var paths []string
		refusedPaths(roots[tier], "", map[reflect.Type]bool{}, &paths)
		for _, path := range paths {
			found = append(found, string(tier)+":"+path)
		}
	}
	if len(found) == 0 {
		t.Fatal("the sweep found no refused fields, so it certifies nothing — " +
			"if the last vendor shipped, delete this test along with it")
	}

	docs := refusedDocuments()
	for _, key := range found {
		pair, ok := docs[key]
		if !ok {
			t.Errorf("%s is refused by this build and has no pair of documents, "+
				"so nothing checks that the schema refuses it in the same place "+
				"the validator does — add one to refusedDocuments", key)
			continue
		}
		t.Run(key, func(t *testing.T) {
			t.Parallel()
			if err := validateTier(pair.tier, pair.off); err != nil {
				t.Fatalf("the engine refuses the OFF document, so it cannot say "+
					"what the schema is allowed to accept — fix the fixture:\n%v", err)
			}
			if err := compiled[pair.tier].Validate(asJSON(t, pair.off)); err != nil {
				t.Fatalf("the schema refuses a config the engine runs, so an editor "+
					"red-underlines a working file:\n%v", err)
			}
			if err := validateTier(pair.tier, pair.on); err == nil {
				t.Fatal("the engine accepts the ON document, so this field is no " +
					"longer refused and the js:\"unimplemented\" tag should go with it")
			}
			if err := compiled[pair.tier].Validate(asJSON(t, pair.on)); err == nil {
				t.Fatal("the schema blesses a setting the engine refuses to boot on")
			}
		})
	}
	for key := range docs {
		if !slices.Contains(found, key) {
			t.Errorf("%s carries a pair of documents and is no longer refused by "+
				"the models — the pair is stale", key)
		}
	}
}

// A REFUSAL THE GENERATOR CANNOT STATE IS A FAULT, NOT A GUESS.
//
// The convenient fallback for an unknown shape is the blanket refusal that
// caused the bug above, so the generator has to be loud instead: a tag it
// cannot translate fails generation, which fails the build's schema tests
// long before the artifact could ship.
func TestARefusalTheGeneratorCannotStateFailsGeneration(t *testing.T) {
	t.Parallel()
	type untranslatable struct {
		Whatever any `yaml:"whatever,omitempty" js:"unimplemented" desc:"a shape with no notion of off"`
	}
	g := &schemaGen{defs: map[string]map[string]any{}}
	g.structSchema(reflect.TypeOf(untranslatable{}))
	if g.err == nil {
		t.Fatal("the generator described a shape it has no rule for, so the " +
			"schema now states a refusal nobody checked against the validator")
	}
	if !errors.Is(g.err, ErrUnknownValue) {
		t.Errorf("the fault is not one a caller can match on: %v", g.err)
	}
	if !strings.Contains(g.err.Error(), "refuseWhenOn") {
		t.Errorf("the fault does not name where to add the rule: %v", g.err)
	}
}

// refusedPaths appends the dotted YAML path of every js:"unimplemented"
// field under a config root, following the same walk the generator does:
// through embedded structs, pointers, slice elements and map values, with a
// guard on the path so a unit holding child units terminates.
func refusedPaths(t reflect.Type, prefix string, onPath map[reflect.Type]bool, out *[]string) {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if t.Kind() != reflect.Struct || onPath[t] {
		return
	}
	onPath[t] = true
	defer delete(onPath, t)

	for i := range t.NumField() {
		f := t.Field(i)
		if f.PkgPath != "" {
			continue
		}
		if f.Anonymous && f.Type.Kind() == reflect.Struct {
			refusedPaths(f.Type, prefix, onPath, out)
			continue
		}
		name, ok := yamlName(f)
		if !ok {
			continue
		}
		path := name
		if prefix != "" {
			path = prefix + "." + name
		}
		if _, refused := parseDirectives(f.Tag.Get("js"))["unimplemented"]; refused {
			// The whole subtree goes with it; there is nothing below a key
			// no document may carry.
			*out = append(*out, path)
			continue
		}
		ft := f.Type
		for ft.Kind() == reflect.Pointer {
			ft = ft.Elem()
		}
		switch ft.Kind() {
		case reflect.Slice, reflect.Array:
			refusedPaths(ft.Elem(), path+"[]", onPath, out)
		case reflect.Map:
			refusedPaths(ft.Elem(), path+"{}", onPath, out)
		case reflect.Struct:
			refusedPaths(ft, path, onPath, out)
		}
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
    integrations: {plane: {project: ENG}}
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
			yaml: "coordination:\n  type: embedded-kv\nstream:\n  replicas: 3\n  cluster:\n    name: acme\n    peers: [nats://b:6222, nats://c:6222]\n",
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
		// THE INTEGRATIONS THIS BUILD VALIDATES AND DOES NOT SERVE.
		//
		// Both layers refuse each of these, and the schema's half is the
		// half that matters here: a config the engine will not boot on must
		// be underlined while the author is still typing, not accepted by
		// the editor and then refused by `crewlet validate`. Each case dies
		// with the change that ships that vendor's parser and transport.
		//
		// This is what became of the old "both knowledge backends" case:
		// Confluence is refused outright now, so the Confluence-XOR-Plane
		// rule it exercised can no longer be reached — a document that sets
		// both is refused for the block, not for the overlap.
		{
			name: "an unserved knowledge base", tier: TierCompany, editorCatches: true,
			yaml: "name: Acme\nintegrations:\n  confluence: {url: https://acme.example.com/wiki, token: t}\n",
		},
		{
			name: "an unserved chat surface", tier: TierCompany, editorCatches: true,
			yaml: "name: Acme\nintegrations:\n  slack: {typing_status: addressed}\n",
		},
		{
			name: "an unserved code host", tier: TierCompany, editorCatches: true,
			yaml: "name: Acme\nintegrations:\n  github: {enabled: true, webhook_secret: s}\n",
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
		// The per-seat half of the same refusal. Leaving it standing while
		// the org block was refused would provision an app, hand the seat a
		// token, accept deliveries — and wake nobody.
		{
			name: "a per-seat Slack app", tier: TierCompany, editorCatches: true,
			yaml: "name: Acme\nroles:\n  - {name: CEO, integrations: {slack: {bot_token: \"${T}\", signing_secret: \"${S}\"}}}\n",
		},
		{
			name: "a plane scope with no plane", tier: TierCompany, editorCatches: true,
			yaml: "name: Acme\nknowledge:\n  plane_projects: [ENG]\n",
		},
		{
			name: "a confluence read scope with no confluence searcher", tier: TierCompany, editorCatches: true,
			yaml: "name: Acme\nknowledge:\n  confluence_spaces: [HANDBOOK]\n",
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
		//
		// The credential-shape case used to be a per-seat Slack app carrying
		// a signing_secret with no bot_token. That seat block is refused
		// outright now, so the schema stopped letting it through and the
		// case stopped proving anything; GitLab's webhook token carries the
		// same shape of rule on a vendor this build serves — a format
		// contract with the vendor, checked in Go, invisible to the schema.
		{
			name: "a signing secret gitlab would refuse", tier: TierCompany, validatorOnly: true,
			// Deliberately NOT a whsec_ token: GitLab accepts only
			// whsec_<standard base64 of 32 bytes>, and a bare shared secret
			// is what an operator reaches for first. The schema sees a
			// string and has nothing to say.
			yaml: "name: Acme\nintegrations:\n  gitlab: {enabled: true, url: https://gitlab.example.com, signing_secret: plain-shared-secret}\n",
		},
		{name: "a stdio server with no command", tier: TierCompany, validatorOnly: true, yaml: "name: Acme\nmcp_servers:\n  - {name: calc}\n"},
		{name: "duplicate handles", tier: TierCompany, validatorOnly: true, yaml: "name: Acme\nroles:\n  - {name: \"Agent CEO\"}\n  - {name: \"agent ceo\"}\n"},
		{name: "a ceiling below its base", tier: TierCompany, validatorOnly: true, yaml: "name: Acme\nturn_engine: {max_tool_rounds: 20, execute_max_tool_rounds_ceiling: 10}\n"},
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

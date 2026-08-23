package config

import (
	"errors"
	"strings"
	"testing"
)

// Every Tier B rejection, with the field path an operator can search for.
//
// The table is the point: a validator that rejects without naming WHERE is
// the failure mode this package is built against, so the path is asserted
// on every case rather than only the message.
func TestCompanyValidatorRejections(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		yaml string
		path string
		kind error
	}{
		{
			"no name", "mission: ship\n", "name", ErrMissing,
		},
		{
			"skill variable key is not an identifier",
			"name: Acme\nskill_variables:\n  base-url: https://x\n",
			"skill_variables.base-url", ErrUnknownValue,
		},
		{
			"coalesce window past the ack budget",
			"name: Acme\nnotification_coalesce_window_seconds: 61\n",
			"notification_coalesce_window_seconds", ErrOutOfRange,
		},

		// providers.llm
		{
			"unknown provider type",
			"name: Acme\nproviders:\n  llm:\n    default:\n      type: openai-compatable\n      model: m\n",
			"providers.llm.default.type", ErrUnknownValue,
		},
		{
			"provider with no model",
			"name: Acme\nproviders:\n  llm:\n    default:\n      type: anthropic\n",
			"providers.llm.default.model", ErrMissing,
		},
		{
			"reasoning on an openai-compatible provider",
			"name: Acme\nproviders:\n  llm:\n    default:\n      type: openai-compatible\n      model: m\n      base_url: https://x/v1/\n      reasoning: true\n",
			"providers.llm.default.reasoning", ErrConflict,
		},
		{
			"openai-compatible with no endpoint",
			"name: Acme\nproviders:\n  llm:\n    default:\n      type: openai-compatible\n      model: m\n",
			"providers.llm.default.base_url", ErrMissing,
		},
		{
			"cooldown below a minute",
			"name: Acme\nproviders:\n  llm:\n    default:\n      type: anthropic\n      model: m\n      cooldowns:\n        auth_seconds: 5\n",
			"providers.llm.default.cooldowns.auth_seconds", ErrOutOfRange,
		},
		{
			"cli-agent with no block",
			"name: Acme\nproviders:\n  llm:\n    sub:\n      type: cli-agent\n      model: sonnet\n",
			"providers.llm.sub.cli", ErrMissing,
		},
		{
			"cli block on an http provider",
			"name: Acme\nproviders:\n  llm:\n    default:\n      type: anthropic\n      model: m\n      cli:\n        agent: claude-code\n",
			"providers.llm.default.cli", ErrConflict,
		},
		{
			"unknown coding CLI",
			"name: Acme\nproviders:\n  llm:\n    sub:\n      type: cli-agent\n      model: sonnet\n      cli:\n        agent: claude-code-2\n",
			"providers.llm.sub.cli.agent", ErrUnknownValue,
		},
		{
			"shared state dir driving two CLIs",
			"name: Acme\nproviders:\n  llm:\n    big:\n      type: cli-agent\n      model: opus\n      cli: {agent: claude-code, state_dir: /var/lib/one}\n    small:\n      type: cli-agent\n      model: gpt\n      cli: {agent: codex, state_dir: /var/lib/one}\n",
			"cli.state_dir", ErrConflict,
		},

		// embeddings + sandbox
		{
			"embeddings with no model",
			"name: Acme\nproviders:\n  embeddings:\n    type: openai\n",
			"providers.embeddings.model", ErrMissing,
		},
		{
			"local sandbox with no block",
			"name: Acme\nproviders:\n  sandbox:\n    type: local\n",
			"providers.sandbox.local", ErrMissing,
		},
		{
			"local block on a remote sandbox",
			"name: Acme\nproviders:\n  sandbox:\n    type: e2b\n    local: {containment: direct}\n",
			"providers.sandbox.local", ErrConflict,
		},
		{
			"container containment with no image",
			"name: Acme\nproviders:\n  sandbox:\n    type: local\n    local: {containment: container}\n",
			"providers.sandbox.local.image", ErrMissing,
		},
		{
			"image on direct containment",
			"name: Acme\nproviders:\n  sandbox:\n    type: local\n    local: {containment: direct, image: acme/box}\n",
			"providers.sandbox.local.image", ErrConflict,
		},
		{
			"unbounded pause",
			"name: Acme\nproviders:\n  sandbox:\n    type: e2b\n    default_pause_ttl_seconds: -1\n",
			"providers.sandbox.default_pause_ttl_seconds", ErrOutOfRange,
		},
		{
			"a setup step that does nothing",
			"name: Acme\nproviders:\n  sandbox:\n    type: e2b\n    setup:\n      - name: empty\n",
			"providers.sandbox.setup[0]", ErrMissing,
		},

		// turn engine
		{
			"a cap of zero",
			"name: Acme\nturn_engine:\n  max_iterations: 0\n",
			"turn_engine.max_iterations", ErrOutOfRange,
		},
		{
			"a budget fraction above one",
			"name: Acme\nturn_engine:\n  subagent_budget_fraction: 1.5\n",
			"turn_engine.subagent_budget_fraction", ErrOutOfRange,
		},
		{
			"a ceiling below its own base",
			"name: Acme\nturn_engine:\n  max_tool_rounds: 20\n  execute_max_tool_rounds_ceiling: 10\n",
			"turn_engine.execute_max_tool_rounds_ceiling", ErrOutOfRange,
		},
		{
			"injecting more than is kept",
			"name: Acme\nturn_engine:\n  conversation_session:\n    max_entries: 5\n    injected_max_entries: 30\n",
			"turn_engine.conversation_session.injected_max_entries", ErrConflict,
		},

		// learning
		{
			"archiving before staling",
			"name: Acme\nlearning:\n  skill_curator:\n    stale_after_days: 90\n    archive_after_days: 30\n",
			"learning.skill_curator.archive_after_days", ErrOutOfRange,
		},
		{
			"retrieval limit past the prompt budget",
			"name: Acme\nlearning:\n  episodic:\n    retrieval_limit: 50\n",
			"learning.episodic.retrieval_limit", ErrOutOfRange,
		},
		{
			"compaction that keeps every row",
			"name: Acme\nlearning:\n  episode_lifecycle:\n    compaction_min_cluster_size: 3\n    exemplar_count: 3\n",
			"learning.episode_lifecycle.exemplar_count", ErrConflict,
		},

		// scheduling
		{
			"a tick that can miss a cron minute",
			"name: Acme\nscheduling:\n  tick_seconds: 300\n",
			"scheduling.tick_seconds", ErrOutOfRange,
		},
		{
			"an unknown timezone",
			"name: Acme\nscheduling:\n  default_timezone: Mars/Olympus\n",
			"scheduling.default_timezone", ErrUnknownValue,
		},
		{
			"catchup max below min",
			"name: Acme\nscheduling:\n  catchup_min_seconds: 600\n  catchup_max_seconds: 60\n",
			"scheduling.catchup_max_seconds", ErrOutOfRange,
		},

		// mcp servers
		{
			"a stdio server with no command",
			"name: Acme\nmcp_servers:\n  - name: calc\n",
			"mcp_servers[0].command", ErrMissing,
		},
		{
			"an http server with no url",
			"name: Acme\nmcp_servers:\n  - name: gh\n    transport: http\n",
			"mcp_servers[0].url", ErrMissing,
		},
		{
			"an http field on a stdio server",
			"name: Acme\nmcp_servers:\n  - name: calc\n    command: uvx\n    url: https://x\n",
			"mcp_servers[0].url", ErrConflict,
		},
		{
			"two servers under one name",
			"name: Acme\nmcp_servers:\n  - {name: calc, command: uvx}\n  - {name: calc, command: npx}\n",
			"mcp_servers[1].name", ErrConflict,
		},

		// integrations
		{
			"jira with neither url nor cloud id",
			"name: Acme\nintegrations:\n  jira:\n    token: t\n",
			"integrations.jira", ErrMissing,
		},
		{
			"jira with both",
			"name: Acme\nintegrations:\n  jira:\n    url: https://x\n    cloud_id: abc\n    token: t\n",
			"integrations.jira", ErrConflict,
		},
		{
			"jira with no token",
			"name: Acme\nintegrations:\n  jira:\n    url: https://x\n",
			"integrations.jira.token", ErrMissing,
		},
		{
			"github enabled with no secret",
			"name: Acme\nintegrations:\n  github:\n    enabled: true\n",
			"integrations.github.webhook_secret", ErrMissing,
		},
		{
			"gitlab enabled with no signing secret",
			"name: Acme\nintegrations:\n  gitlab:\n    enabled: true\n    url: https://gitlab.com\n",
			"integrations.gitlab.signing_secret", ErrMissing,
		},
		{
			"mattermost with a schemeless url",
			"name: Acme\nintegrations:\n  mattermost:\n    enabled: true\n    url: chat.example.com\n    team: acme\n",
			"integrations.mattermost.url", ErrUnknownValue,
		},
		{
			"mattermost with no team",
			"name: Acme\nintegrations:\n  mattermost:\n    enabled: true\n    url: https://chat.example.com\n",
			"integrations.mattermost.team", ErrMissing,
		},
		{
			"an unknown typing-status mode",
			"name: Acme\nintegrations:\n  slack:\n    typing_status: sometimes\n",
			"integrations.slack.typing_status", ErrUnknownValue,
		},
		{
			"a blank status phrase",
			"name: Acme\nintegrations:\n  slack:\n    status_phrases:\n      plan: [\"is planning...\", \"\"]\n",
			"integrations.slack.status_phrases.plan[1]", ErrMissing,
		},
		{
			"plane enabled with no workspace",
			"name: Acme\nintegrations:\n  plane:\n    enabled: true\n    url: https://plane.example.com\n    webhook_secret: s\n",
			"integrations.plane.workspace", ErrMissing,
		},
		{
			"a negative plane token expiry",
			"name: Acme\nintegrations:\n  plane:\n    enabled: true\n    url: https://plane.example.com\n    workspace: acme\n    webhook_secret: s\n    provisioning:\n      token_expiry_days: -1\n",
			"integrations.plane.provisioning.token_expiry_days", ErrOutOfRange,
		},
		{
			"an unknown gitlab access level",
			"name: Acme\nintegrations:\n  gitlab:\n    enabled: true\n    url: https://gitlab.com\n    signing_secret: s\n    provisioning:\n      access_level: owner\n",
			"integrations.gitlab.provisioning.access_level", ErrUnknownValue,
		},

		// seats
		{
			"a seat with no name",
			"name: Acme\nroles:\n  - goal: ship\n",
			"roles[0].name", ErrMissing,
		},
		{
			"a signing secret with no bot token",
			"name: Acme\nroles:\n  - name: CEO\n    integrations:\n      slack:\n        signing_secret: \"${S}\"\n",
			"roles[0].integrations.slack.signing_secret", ErrConflict,
		},
		{
			"a mixed literal/reference credential pair",
			"name: Acme\nroles:\n  - name: CEO\n    integrations:\n      slack:\n        bot_token: \"${TOK}\"\n        signing_secret: literal-secret\n",
			"roles[0].integrations.slack", ErrConflict,
		},
		{
			"a Mattermost username the server would refuse",
			"name: Acme\nroles:\n  - name: CEO\n    integrations:\n      mattermost:\n        bot_token: \"${T}\"\n        username: Agent CEO\n",
			"roles[0].integrations.mattermost.username", ErrUnknownValue,
		},
		{
			"a sandbox block that is never enabled",
			"name: Acme\nroles:\n  - name: CEO\n    sandbox:\n      env: {GITHUB_TOKEN: \"${T}\"}\n",
			"roles[0].sandbox.enabled", ErrConflict,
		},
		{
			"a unit with no name",
			"name: Acme\nunits:\n  - lead: CEO\n",
			"units[0].name", ErrMissing,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := rejects(t, tc.yaml, tc.path)
			if !errors.Is(err, tc.kind) {
				t.Fatalf("want %v, got:\n%v", tc.kind, err)
			}
		})
	}
}

// The knowledge backend is single-homed. Backend selection keys on block
// PRESENCE, because the scope lists default to empty (unscoped) and cannot
// be the signal.
func TestKnowledgeBackendIsSingleHomed(t *testing.T) {
	t.Parallel()

	t.Run("both backends", func(t *testing.T) {
		t.Parallel()
		err := rejects(t, `
name: Acme
integrations:
  confluence: {url: "https://x/wiki", token: t}
  plane: {enabled: true, url: "https://p", workspace: w, webhook_secret: s}
`, "integrations")
		if !strings.Contains(err.Error(), "cut-over") {
			t.Fatalf("the error should say a migration is a cut-over; got %v", err)
		}
	})

	t.Run("a scope for the disabled backend", func(t *testing.T) {
		t.Parallel()
		rejects(t, `
name: Acme
integrations:
  plane: {enabled: true, url: "https://p", workspace: w, webhook_secret: s}
knowledge:
  confluence_spaces: [HANDBOOK]
`, "knowledge.confluence_spaces")
		rejects(t, `
name: Acme
integrations:
  confluence: {url: "https://x/wiki", token: t}
knowledge:
  plane_projects: [ENG]
`, "knowledge.plane_projects")
	})

	t.Run("a plane scope with no plane", func(t *testing.T) {
		t.Parallel()
		rejects(t, "name: Acme\nknowledge:\n  plane_projects: [ENG]\n", "knowledge.plane_projects")
	})

	t.Run("a disabled plane block beside confluence is inert", func(t *testing.T) {
		t.Parallel()
		mustCompany(t, `
name: Acme
integrations:
  confluence: {url: "https://x/wiki", token: t}
  plane: {enabled: false}
knowledge:
  confluence_spaces: [HANDBOOK]
`)
	})
}

// The org model's own rules are reached through the built hierarchy, so a
// config that parses into a company nobody can run is still rejected.
func TestOrgRulesAreEnforcedThroughTheConfig(t *testing.T) {
	t.Parallel()

	t.Run("a human seat needs a contact", func(t *testing.T) {
		t.Parallel()
		rejects(t, "name: Acme\nroles:\n  - name: Founder\n    kind: human\n", "Founder")
	})

	t.Run("a human seat rejects runtime fields", func(t *testing.T) {
		t.Parallel()
		err := rejects(t, `
name: Acme
roles:
  - name: Founder
    kind: human
    contact: {slack_user_id: U0FOUNDER}
    token_budget: 100
`, "Founder")
		if !strings.Contains(err.Error(), "token_budget") {
			t.Fatalf("the error should name the offending field; got %v", err)
		}
	})

	t.Run("two seats resolving to one handle", func(t *testing.T) {
		t.Parallel()
		rejects(t, "name: Acme\nroles:\n  - {name: \"Agent CEO\"}\n  - {name: \"agent ceo\"}\n", "duplicate handle")
	})

	t.Run("a malformed explicit handle", func(t *testing.T) {
		t.Parallel()
		rejects(t, "name: Acme\nroles:\n  - {name: CEO, handle: \"Chief Exec\"}\n", "handle")
	})

	t.Run("a schedule with a bad cron", func(t *testing.T) {
		t.Parallel()
		rejects(t, `
name: Acme
roles:
  - name: CEO
    schedules:
      - {name: standup, cron: "9 * *", task: post}
`, "cron")
	})
}

// A minimal company loads: an org chart can be authored before any
// provider, credential or integration exists, which is what makes
// `crewlet validate` useful on a laptop.
func TestMinimalCompanyLoads(t *testing.T) {
	t.Parallel()
	cfg := mustCompany(t, "name: Acme\n")
	if cfg.Name != "Acme" {
		t.Fatalf("name = %q", cfg.Name)
	}
	org, err := cfg.Organization()
	if err != nil {
		t.Fatal(err)
	}
	if org.Name != "Acme" {
		t.Fatalf("org name = %q", org.Name)
	}
}

package config

import (
	"errors"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/queue"
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
		// The two reasoning DIALS on a cli-agent entry, refused for the
		// same reason `reasoning` itself already is: both are per-call API
		// parameters a headless coding CLI takes no flag for, so nothing
		// passes them to the backend and they validated clean while doing
		// nothing at all. An operator told to "raise the model's effort"
		// would set one, see no change, and have nothing to look at.
		{
			"reasoning_effort on a cli-agent provider",
			"name: Acme\nproviders:\n  llm:\n    sub:\n      type: cli-agent\n      model: haiku\n" +
				"      reasoning_effort: high\n      cli:\n        agent: claude-code\n",
			"providers.llm.sub.reasoning_effort", ErrConflict,
		},
		{
			"reasoning_budget_tokens on a cli-agent provider",
			"name: Acme\nproviders:\n  llm:\n    sub:\n      type: cli-agent\n      model: haiku\n" +
				"      reasoning_budget_tokens: 10000\n      cli:\n        agent: claude-code\n",
			"providers.llm.sub.reasoning_budget_tokens", ErrConflict,
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
			// A CATALOGUE IS A CHOICE, and every default is wrong in a
			// way nobody sees: `local` runs the coding agent on this
			// host, an absent block turns code work off while the
			// config says it is on, and the default this replaced named
			// a backend the engine had no code to build.
			"a sandbox block that names no backend",
			"name: Acme\nproviders:\n  sandbox:\n    default_coding_agent: opencode\n",
			"providers.sandbox", ErrMissing,
		},
		{
			"unbounded pause",
			"name: Acme\nproviders:\n  sandbox:\n    fake: true\n    default_pause_ttl_seconds: -1\n",
			"providers.sandbox.default_pause_ttl_seconds", ErrOutOfRange,
		},
		{
			"a negative company-wide round cap",
			"name: Acme\nproviders:\n  sandbox:\n    fake: true\n    default_max_turns: -5\n",
			"providers.sandbox.default_max_turns", ErrOutOfRange,
		},
		{
			// A seat that runs code in a company with no catalogue is
			// offered nothing and told nowhere: the gate reads as
			// working configuration and its code work quietly never
			// happens.
			"a seat that runs code with no catalogue at all",
			"name: Acme\nroles:\n  - name: SWE\n    sandbox: {enabled: true}\n",
			"roles[0].sandbox.enabled", ErrMissing,
		},
		{
			"a setup step that does nothing",
			"name: Acme\nproviders:\n  sandbox:\n    fake: true\n    setup:\n      - name: empty\n",
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
			"name: Acme\nturn_engine:\n  delegation:\n    budget_fraction: 1.5\n",
			"turn_engine.delegation.budget_fraction", ErrOutOfRange,
		},
		{
			"a ceiling below its own base",
			"name: Acme\nturn_engine:\n  max_tool_rounds: 20\n  execute_max_tool_rounds_ceiling: 10\n",
			"turn_engine.execute_max_tool_rounds_ceiling", ErrOutOfRange,
		},
		{
			"a conversation ledger that keeps nothing",
			"name: Acme\nturn_engine:\n  conversation_session:\n    max_entries: 0\n",
			"turn_engine.conversation_session.max_entries", ErrOutOfRange,
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
		// The hosted code host's own rules, which run now that it is
		// served.
		{
			"a github block with nothing to verify a delivery with",
			"name: Acme\nintegrations:\n  github: {enabled: true}\n",
			"integrations.github.webhook_secret", ErrMissing,
		},
		{
			"an enterprise server address with no scheme",
			"name: Acme\nintegrations:\n  github: {enabled: true, url: github.example.com, webhook_secret: s}\n",
			"integrations.github.url", ErrUnknownValue,
		},
		{
			"a github repo that is not owner/repo",
			"name: Acme\nintegrations:\n  github: {enabled: true, webhook_secret: s, provisioning: {repos: [engine]}}\n",
			"integrations.github.provisioning.repos[0]", ErrShape,
		},
		{
			"a demanded org hook with no org to put it on",
			"name: Acme\nintegrations:\n  github: {enabled: true, webhook_secret: s, provisioning: {org_webhook: \"true\"}}\n",
			"integrations.github.provisioning.org", ErrMissing,
		},
		{
			"an unknown github org-webhook mode",
			"name: Acme\nintegrations:\n  github: {enabled: true, webhook_secret: s, provisioning: {org: acme, org_webhook: maybe}}\n",
			"integrations.github.provisioning.org_webhook", ErrUnknownValue,
		},
		// The knowledge base's own rules, which DO run now that it is
		// served.
		{
			"a confluence block naming the instance twice",
			"name: Acme\nintegrations:\n  confluence: {url: \"https://wiki.example.com\", cloud_id: abc, token: t, webhook_secret: s}\n",
			"integrations.confluence", ErrConflict,
		},
		{
			"a confluence block with no org token",
			"name: Acme\nintegrations:\n  confluence: {url: \"https://wiki.example.com\", webhook_secret: s}\n",
			"integrations.confluence.token", ErrMissing,
		},
		{
			"a data centre confluence with nothing to verify a delivery with",
			"name: Acme\nintegrations:\n  confluence: {url: \"https://wiki.example.com\", token: t}\n",
			"integrations.confluence.webhook_secret", ErrMissing,
		},
		// The Atlassian tracker's own rules, which DO run now that it is
		// served.
		{
			"a jira block naming the instance twice",
			"name: Acme\nintegrations:\n  jira: {url: \"https://acme.example.com\", cloud_id: abc, token: t, webhook_secret: s}\n",
			"integrations.jira", ErrConflict,
		},
		{
			"a jira block naming the instance nowhere",
			"name: Acme\nintegrations:\n  jira: {token: t, webhook_secret: s}\n",
			"integrations.jira", ErrMissing,
		},
		{
			"a jira block with no org token",
			"name: Acme\nintegrations:\n  jira: {url: \"https://acme.example.com\", webhook_secret: s}\n",
			"integrations.jira.token", ErrMissing,
		},
		{
			"a data centre jira with nothing to verify a delivery with",
			"name: Acme\nintegrations:\n  jira: {url: \"https://acme.example.com\", token: t}\n",
			"integrations.jira.webhook_secret", ErrMissing,
		},
		{
			"a jira site url that is not a url",
			"name: Acme\nintegrations:\n  jira: {cloud_id: abc, site_url: acme.example.com, token: t}\n",
			"integrations.jira.site_url", ErrUnknownValue,
		},
		// A per-seat Slack app needs BOTH credentials, and each half fails
		// silently on its own: without a token the seat receives messages
		// it cannot answer, without a secret its route answers 503 while
		// the app's settings page reports a healthy request URL.
		{
			"a slack app with no signing secret",
			"name: Acme\nroles:\n  - name: CEO\n    integrations:\n      slack:\n        bot_token: \"${TOK}\"\n",
			"roles[0].integrations.slack.signing_secret", ErrMissing,
		},
		{
			"a slack app with no bot token",
			"name: Acme\nroles:\n  - name: CEO\n    integrations:\n      slack:\n        signing_secret: \"${S}\"\n",
			"roles[0].integrations.slack.bot_token", ErrMissing,
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

// The knowledge backend is single-homed, and with one backend that is now
// structural: what is still enforced is that a read scope names a backend
// the company actually configures. Selection keys on block PRESENCE, because
// the scope list defaults to empty (unscoped) and cannot be the signal.
func TestAKnowledgeScopeNeedsItsBackend(t *testing.T) {
	t.Parallel()

	// A SCOPE FOR A BACKEND THAT IS NOT THERE reads as a working narrowing
	// and narrows nothing — the silence this rule exists to end, one level
	// down from the block itself.
	t.Run("a confluence scope with no confluence", func(t *testing.T) {
		t.Parallel()
		err := rejects(t, "name: Acme\nknowledge:\n  confluence_spaces: [HANDBOOK]\n",
			"knowledge.confluence_spaces")
		if !errors.Is(err, ErrConflict) {
			t.Fatalf("want ErrConflict, got %v", err)
		}
	})

	// AND THE WORKING SHAPE STILL WORKS: the backend with its own scope,
	// and the reserved skills space defaulted rather than invented.
	t.Run("confluence with a confluence scope is accepted", func(t *testing.T) {
		t.Parallel()
		cfg := mustCompany(t, `
name: Acme
integrations:
  confluence: {url: "https://wiki.example.com", token: "${T}", webhook_secret: "${S}"}
knowledge:
  confluence_spaces: [HANDBOOK, ENG]
`)
		if got := cfg.Integrations.Confluence.SkillsSpaceKey(); got != "TS" {
			t.Errorf("skills space = %q, want the default", got)
		}
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

// A DISABLED BLOCK SKIPS ITS OWN REQUIRED FIELDS.
//
// `github: {enabled: false}` is an operator who turned the integration off
// and left the block behind — which is how a block gets turned off, since
// deleting it would lose the settings. Its webhook_secret rule is a
// requirement of RUNNING it, so applying it to a block that is not running
// would fail a config whose author already agreed with us.
func TestADisabledIntegrationSkipsItsOwnRules(t *testing.T) {
	t.Parallel()
	mustCompany(t, "name: Acme\nintegrations:\n  github: {enabled: false}\n")
	mustCompany(t, "name: Acme\nintegrations:\n  gitlab: {enabled: false}\n")
}

// FORGE IS A JIRA CLOUD CONFIG, NOT A REFUSAL. forge_app_id is the Atlassian
// Cloud delivery path, and Jira Cloud rides it — so the config that names a
// cloud id and an app id is the CORRECT one and must load. It carries no
// webhook secret on purpose: a relayed Cloud event is verified by its
// invocation token, and there is no HMAC anywhere on that path.
func TestJiraCloudRidesTheForgeRoute(t *testing.T) {
	t.Parallel()
	cfg := mustCompany(t, `
name: Acme
integrations:
  jira: {cloud_id: "acme-cloud", site_url: "https://acme.atlassian.net", token: "${T}"}
  forge_app_id: an-app
`)
	jira := cfg.Integrations.Jira
	if got := jira.BaseURL(); got != "https://api.atlassian.com/ex/jira/acme-cloud" {
		t.Errorf("base url = %q", got)
	}
	// THE TWO BASES ARE DIFFERENT PLACES, and that is the point of
	// site_url: the gateway is where the engine reads, and it is not
	// somewhere a browser can go.
	if got := jira.ShareableBaseURL(); got != "https://acme.atlassian.net" {
		t.Errorf("shareable base = %q", got)
	}
}

// A DATA CENTRE JIRA WITH A SECRET IS A COMPLETE CONFIG, and its shareable
// base defaults to the instance url — there is only one address.
func TestJiraDataCentreNeedsNoSeparateSiteURL(t *testing.T) {
	t.Parallel()
	cfg := mustCompany(t,
		"name: Acme\nintegrations:\n  jira: {url: \"https://jira.example.com\", token: \"${T}\", webhook_secret: \"${S}\"}\n")
	jira := cfg.Integrations.Jira
	if jira.BaseURL() != "https://jira.example.com" ||
		jira.ShareableBaseURL() != "https://jira.example.com" {
		t.Errorf("base = %q, shareable = %q", jira.BaseURL(), jira.ShareableBaseURL())
	}
}

// THE TWO COALESCING CEILINGS ARE ENFORCED, AND THE WINDOW'S IS THE
// CONTRACT'S OWN NUMBER.
//
// queue.BatchOptions clamps the window regardless of who set the field —
// programmatic construction bypasses validation entirely — and internal/queue
// states that "config validation mirrors this cap". A second literal here made
// that a claim nothing checked. The batch cap had no ceiling at all, on a knob
// that multiplies the dominant repeated content of every round of a turn.
func TestTheCoalescingCeilingsAreTheOnesTheContractEnforces(t *testing.T) {
	t.Parallel()
	if coalesceWindowMax != queue.MaxLingerSeconds {
		t.Errorf("the config ceiling is %v and the contract clamps at %v: a window "+
			"between them validates and is then silently cut",
			coalesceWindowMax, queue.MaxLingerSeconds)
	}
	for _, tc := range []struct {
		yaml  string
		field string
	}{
		{"name: Acme\nnotification_coalesce_max_batch: 101\n", "notification_coalesce_max_batch"},
		{"name: Acme\nnotification_coalesce_max_batch: 0\n", "notification_coalesce_max_batch"},
		{"name: Acme\nnotification_coalesce_window_seconds: 60.5\n", "notification_coalesce_window_seconds"},
	} {
		_, err := ParseCompany([]byte(tc.yaml))
		if err == nil || !strings.Contains(err.Error(), tc.field) {
			t.Errorf("%q: err = %v, want a report naming %s", tc.yaml, err, tc.field)
		}
	}
	// And the counterfactual: the boundary values are accepted, or the
	// assertions above pass for a validator that refuses everything.
	for _, ok := range []string{
		"name: Acme\nnotification_coalesce_max_batch: 100\n",
		"name: Acme\nnotification_coalesce_max_batch: 1\n",
		"name: Acme\nnotification_coalesce_window_seconds: 60\n",
	} {
		if _, err := ParseCompany([]byte(ok)); err != nil {
			t.Errorf("%q was refused: %v", ok, err)
		}
	}
}

// A COMPANY USING SLACK PER SEAT STILL DECLARES SLACK.
//
// Every agent carries its own Slack app under `role.integrations.slack`, and
// the company-level `slack:` block is working-indicator settings a company may
// never write. Asked of the block alone the answer is "no" — and the one
// caller of this deletes the surface's fleet status row on "no", which is where
// the engine records the public base Slack's Request URLs were set against.
// Slack serves no way to read that URL back, so that row is the only warning an
// operator ever gets that a moved address has stranded every agent's app.
func TestSlackIsDeclaredByASeatWithNoCompanyBlock(t *testing.T) {
	t.Parallel()
	company := &Company{
		Name: "Acme",
		Roles: []Role{{
			Name: "SRE Lead",
			Integrations: RoleIntegrations{Slack: &RoleSlack{
				BotToken:      "${SLACK_BOT_TOKEN}",
				SigningSecret: "${SLACK_SIGNING_SECRET}",
			}},
		}},
	}
	if !company.DeclaresIntegration("slack") {
		t.Fatal("a company whose seats hold Slack apps was reported as not " +
			"declaring Slack, which deletes the only record of the address " +
			"those apps deliver to")
	}
}

// AND A COMPANY USING IT NOWHERE DOES NOT, or the row would never be cleaned
// up and a later reconnect would inherit an address from the company before it.
func TestSlackIsNotDeclaredWithoutABlockOrASeat(t *testing.T) {
	t.Parallel()
	company := &Company{Name: "Acme", Roles: []Role{{Name: "SRE Lead"}}}
	if company.DeclaresIntegration("slack") {
		t.Fatal("a company using Slack nowhere was reported as declaring it")
	}
}

// A SURFACE THIS BUILD DOES NOT KNOW IS DECLARED, because the only caller
// deletes on false and a rolling upgrade puts a newer node's surface in front
// of an older reader. Erasing a peer's status row every tick, and watching it
// written back on every pass of theirs, is the failure this direction avoids.
func TestAnUnknownSurfaceIsTreatedAsDeclared(t *testing.T) {
	t.Parallel()
	company := &Company{Name: "Acme"}
	if !company.DeclaresIntegration("a-surface-from-a-newer-build") {
		t.Fatal("an unknown surface answered false, so an older node would " +
			"delete a newer node's status row")
	}
}

// AN INSTANCE NOBODY HAS NAMED IS NOT A DATA CENTER INSTANCE.
//
// A block with neither `url` nor `cloud_id` has an unknown deployment, and the
// problem that says so is the only one outstanding. The signing-secret rule
// fired on it anyway, because "not Cloud" was tested as `cloud == "" &&
// !IsAtlassianCloud(url)` and an empty url is not a Cloud url.
//
// What that produced is worse than a redundant line. An operator connecting
// Atlassian — having chosen Atlassian Cloud in the dialog, which is exempt
// from signing secrets on both of its routes — was told a secret was
// "required for a Data Center instance", naming a deployment they had not
// picked and a field the form does not offer. Two of the four problems on
// that screen were consequences of the other two, and they pointed the wrong
// way.
func TestAnUnidentifiedAtlassianInstanceAsksOnlyToBeNamed(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		in   Integrations
		path string
	}{
		{"jira", Integrations{Jira: &Jira{Token: "${JIRA_TOKEN}"}}, "integrations.jira"},
		{
			"confluence",
			Integrations{Confluence: &Confluence{Token: "${CONFLUENCE_TOKEN}"}},
			"integrations.confluence",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var err error
			if tc.in.Jira != nil {
				err = tc.in.Jira.validate(tc.path, false)
			} else {
				err = tc.in.Confluence.validate(tc.path, false)
			}
			if err == nil {
				t.Fatal("a block naming no instance was accepted")
			}
			got := err.Error()
			if !strings.Contains(got, "nowhere to search") &&
				!strings.Contains(got, "url or cloud_id") &&
				!strings.Contains(got, "give url") {
				t.Fatalf("the refusal does not ask for the instance: %s", got)
			}
			if strings.Contains(got, "required for a Data Center instance") {
				t.Fatalf("a block whose deployment is unknown was told a signing "+
					"secret is required for a DATA CENTER instance, which is a "+
					"deployment nobody has named and which Cloud is exempt from: %s", got)
			}
		})
	}
}

// AND A DATA CENTER INSTANCE STILL HAS TO HAVE ONE, or the guard above would
// be a way to lose the rule entirely: that route verifies a delivery with
// nothing else and answers 503 to every one without it.
func TestANamedDataCentreInstanceStillNeedsASigningSecret(t *testing.T) {
	t.Parallel()
	jira := &Jira{URL: "https://jira.example.com", Token: "${JIRA_TOKEN}"}
	err := jira.validate("integrations.jira", false)
	if err == nil || !strings.Contains(err.Error(), "required for a Data Center instance") {
		t.Fatalf("a Data Center instance was not asked for a signing secret: %v", err)
	}
}

// AND A CLOUD SITE IS STILL EXEMPT, on the route that names it by host.
func TestACloudSiteIsNotAskedForASigningSecret(t *testing.T) {
	t.Parallel()
	jira := &Jira{URL: "https://acme.atlassian.net", Token: "${JIRA_TOKEN}"}
	if err := jira.validate("integrations.jira", false); err != nil {
		t.Fatalf("a Cloud site was refused: %v", err)
	}
}

// CONNECTING ATLASSIAN ON CLOUD IS NOT REFUSED FOR THE SITE IT IS ABOUT TO
// DISCOVER.
//
// This is the whole Connect Atlassian flow, and it could not succeed. The
// setup form asks for neither the site url nor the cloud id on Cloud — both
// are declared Hidden, because the organization key reads every site this
// company has — and validation demanded one of them anyway. So the write was
// refused, and the pass that would have discovered and recorded the site never
// ran, because the write is what starts it. The operator was told to fill in a
// field that is not on the screen.
func TestConnectingAtlassianOnCloudIsNotRefusedForAnUndiscoveredSite(t *testing.T) {
	t.Parallel()
	in := Integrations{
		Atlassian: &Atlassian{
			OrgID:  "f1240761-c455-41b5-a7f5-4a64f9c6e729",
			APIKey: "${ATLASSIAN_API_KEY}",
		},
		Jira:       &Jira{Email: "ops@example.com", Token: "${ATLASSIAN_TOKEN}"},
		Confluence: &Confluence{Email: "ops@example.com", Token: "${ATLASSIAN_TOKEN}"},
	}
	if err := in.validate("integrations"); err != nil {
		t.Fatalf("connecting Atlassian on Cloud was refused: %v", err)
	}
}

// AND WITHOUT AN ORGANIZATION TO DISCOVER IT, THE SITE IS STILL REQUIRED.
//
// Atlassian.IsCloud answers true for an absent block, which is right for
// "which deployment is this" and wrong for "will anything supply the address":
// a company with no organization block has nothing to discover a site WITH.
func TestAConfluenceBlockWithNoOrganizationStillNeedsASite(t *testing.T) {
	t.Parallel()
	in := Integrations{
		Confluence: &Confluence{Email: "ops@example.com", Token: "${CONFLUENCE_TOKEN}"},
	}
	err := in.validate("integrations")
	if err == nil || !strings.Contains(err.Error(), "nowhere to search") {
		t.Fatalf("a Confluence block nothing can locate was accepted: %v", err)
	}
}

package config

import (
	"slices"
	"testing"

	"github.com/crewlet/crewlet/internal/org"
)

// A field that takes a scalar OR a list gets a named type with its own
// decoder, so no consumer downstream holds an `any` or learns which shape
// the operator wrote. The scalar form is a chain of one, not a special case.
func TestProviderKeysAcceptBothShapes(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		yaml string
		want []string
	}{
		{"scalar", "name: Acme\nroles:\n  - {name: CEO, llm_plan: big}\n", []string{"big"}},
		{"list", "name: Acme\nroles:\n  - {name: CEO, llm_plan: [big, small]}\n", []string{"big", "small"}},
		{"valueless", "name: Acme\nroles:\n  - {name: CEO, llm_plan: }\n", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cfg := mustCompany(t, tc.yaml)
			if got := []string(cfg.Roles[0].LLMPlan); !slices.Equal(got, tc.want) {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
		})
	}
}

// The `llm` field takes a third shape as well: a mapping per phase, because
// the phases do genuinely different work and splitting them by hand across
// seven flat fields is how six end up agreeing and one goes stale.
func TestPhaseLLMAcceptsAllThreeShapes(t *testing.T) {
	t.Parallel()

	t.Run("a scalar points every phase at one provider", func(t *testing.T) {
		t.Parallel()
		seat := mustCompany(t, "name: Acme\nroles:\n  - {name: CEO, llm: fast}\n").Roles[0].Seat()
		if !slices.Equal(seat.LLM, org.ProviderKeys{"fast"}) {
			t.Fatalf("llm = %v", seat.LLM)
		}
		if seat.LLMPlan != nil {
			t.Fatalf("a scalar must not fabricate per-phase chains: %v", seat.LLMPlan)
		}
	})

	t.Run("a list is a chain for every phase", func(t *testing.T) {
		t.Parallel()
		seat := mustCompany(t, "name: Acme\nroles:\n  - {name: CEO, llm: [fast, backup]}\n").Roles[0].Seat()
		if !slices.Equal(seat.LLM, org.ProviderKeys{"fast", "backup"}) {
			t.Fatalf("llm = %v", seat.LLM)
		}
	})

	t.Run("a mapping splits the phases", func(t *testing.T) {
		t.Parallel()
		seat := mustCompany(t, `
name: Acme
roles:
  - name: CEO
    llm:
      default: fast
      plan: [big, bigger]
      judge: cheap
`).Roles[0].Seat()
		if !slices.Equal(seat.LLM, org.ProviderKeys{"fast"}) {
			t.Fatalf("default = %v", seat.LLM)
		}
		if !slices.Equal(seat.LLMPlan, org.ProviderKeys{"big", "bigger"}) {
			t.Fatalf("plan = %v", seat.LLMPlan)
		}
		if !slices.Equal(seat.LLMJudge, org.ProviderKeys{"cheap"}) {
			t.Fatalf("judge = %v", seat.LLMJudge)
		}
		// A phase the mapping did not name stays unset, so the runtime
		// falls back to the default chain rather than to a fabricated one.
		if seat.LLMExecute != nil {
			t.Fatalf("execute = %v", seat.LLMExecute)
		}
	})

	t.Run("a flat field wins over the mapping", func(t *testing.T) {
		t.Parallel()
		seat := mustCompany(t, `
name: Acme
roles:
  - name: CEO
    llm: {default: fast, plan: from-mapping}
    llm_plan: from-flat
`).Roles[0].Seat()
		if !slices.Equal(seat.LLMPlan, org.ProviderKeys{"from-flat"}) {
			t.Fatalf("plan = %v", seat.LLMPlan)
		}
	})
}

// The authored seat and the runtime seat are different shapes on purpose;
// the transform is where the difference is paid for, once.
func TestSeatTransform(t *testing.T) {
	t.Parallel()
	cfg := mustCompany(t, `
name: Acme
roles:
  - name: Agent SWE
    handle: swe
    email: swe@example.com
    goal: ship
    manages: [Junior]
    token_budget: 1000
    mcp_env:
      gitlab:
        GITLAB_TOKEN: "${GL_SWE}"
    placement:
      node: node-2
      labels: {zone: eu}
    integrations:
      mattermost: {bot_token: "${MM_SWE}", username: swe-bot, channel: eng}
      plane: {project: ENGP}
    sandbox:
      enabled: true
      coding_agent: opencode
      env: {GITHUB_TOKEN: "${GH}"}
      mcp: {servers: [gitlab]}
  - name: Junior
`)
	seat := cfg.Roles[0].Seat()

	if seat.Handle() != "swe" {
		t.Fatalf("handle = %q", seat.Handle())
	}
	if seat.Mattermost.BotToken != "${MM_SWE}" || seat.Mattermost.Username != "swe-bot" ||
		seat.Mattermost.Channel != "eng" {
		t.Fatalf("mattermost = %+v", seat.Mattermost)
	}
	if seat.PlaneProject != "ENGP" {
		t.Fatalf("plane project = %q", seat.PlaneProject)
	}
	// The other two identities cannot be authored any more — jira and
	// confluence are refused — and a config that cannot name one must not
	// produce one from somewhere else.
	if seat.JiraProject != "" || seat.ConfluenceSpace != "" {
		t.Fatalf("a seat that named no Jira or Confluence identity got %q and %q",
			seat.JiraProject, seat.ConfluenceSpace)
	}
	if seat.Placement.Node != "node-2" || seat.Placement.Labels["zone"] != "eu" {
		t.Fatalf("placement = %+v", seat.Placement)
	}
	if seat.Sandbox == nil || !seat.Sandbox.Enabled || seat.Sandbox.CodingAgent != "opencode" {
		t.Fatalf("sandbox = %+v", seat.Sandbox)
	}
	if seat.MCPEnv["gitlab"]["GITLAB_TOKEN"] != "${GL_SWE}" {
		t.Fatalf("mcp_env = %v", seat.MCPEnv)
	}
}

// The refused halves of that same transform can no longer be reached from a
// config: role.integrations.slack, .jira and .confluence are all refused
// with ErrUnimplemented, because nothing parses a delivery from any of them,
// so the seat would carry an app and two routing identities that wake
// nobody. The TRANSFORM is still live code — the webhook route resolves each
// seat's signing secret out of org.Role.Slack, and the dashboard reports
// which identities a seat carries — so the authored seat is built directly
// here. Parsing YAML would assert the refusal instead of what this test is
// about, and deleting the transform would take working code out with the
// vendor it is waiting for.
func TestSeatTransformStillCarriesTheRefusedIdentities(t *testing.T) {
	t.Parallel()
	seat := (&Role{
		Name: "Agent SWE",
		Integrations: RoleIntegrations{
			Slack: &RoleSlack{
				BotToken:      "${SLACK_SWE}",
				SigningSecret: "${SIGN_SWE}",
				Channel:       "C123",
			},
			Jira:       &ProjectRef{Project: "ENG"},
			Confluence: &SpaceRef{Space: "ENGSPACE"},
		},
	}).Seat()

	if seat.JiraProject != "ENG" || seat.ConfluenceSpace != "ENGSPACE" {
		t.Errorf("identities = %q %q", seat.JiraProject, seat.ConfluenceSpace)
	}

	if seat.Slack.BotToken != "${SLACK_SWE}" || seat.Slack.SigningSecret != "${SIGN_SWE}" ||
		seat.Slack.Channel != "C123" {
		t.Fatalf("slack = %+v", seat.Slack)
	}
}

// Building the org is where the hierarchy actually forms: seats declared at
// the root with a unit reference move in, unit credentials layer under
// their members', and a lead picks up the members nobody else manages.
func TestOrganizationNormalisesTheHierarchy(t *testing.T) {
	t.Parallel()
	cfg := mustCompany(t, `
name: Acme
roles:
  - name: Contractor
    unit: Core
    mcp_env:
      gitlab: {GITLAB_HOST: contractor.example}
units:
  - name: Core
    lead: CTO
    mcp_env:
      gitlab: {GITLAB_TOKEN: "${SHARED}", GITLAB_HOST: gitlab.com}
    roles:
      - name: CTO
      - name: Engineer
`)
	o, err := cfg.Organization()
	if err != nil {
		t.Fatal(err)
	}

	if len(o.Roles) != 0 {
		t.Fatalf("the root seat should have moved into its unit; root holds %d", len(o.Roles))
	}
	unit := o.Unit("Core")
	if unit == nil || unit.Role("Contractor") == nil {
		t.Fatal("the moved seat is not a member of its unit")
	}

	// Inheritance merges per VARIABLE: a seat overriding one entry must
	// not silently drop the token beside it.
	contractor := o.Role("Contractor")
	if got := contractor.MCPEnv["gitlab"]["GITLAB_TOKEN"]; got != "${SHARED}" {
		t.Fatalf("the moved seat missed the unit's shared credential: %q", got)
	}
	if got := contractor.MCPEnv["gitlab"]["GITLAB_HOST"]; got != "contractor.example" {
		t.Fatalf("the seat's own override lost: %q", got)
	}

	// The lead auto-manages what nobody else does, which is what makes a
	// roster complete without listing every report twice.
	cto := o.Role("CTO")
	if !slices.Contains(cto.Manages, "Engineer") || !slices.Contains(cto.Manages, "Contractor") {
		t.Fatalf("lead manages = %v", cto.Manages)
	}
}

// The config a revision was built from must be untouched: a stored revision
// is read again on the next apply, and a normalisation that mutated it
// would compound.
func TestOrganizationDoesNotMutateTheConfig(t *testing.T) {
	t.Parallel()
	cfg := mustCompany(t, `
name: Acme
roles:
  - {name: Contractor, unit: Core}
units:
  - name: Core
    lead: CTO
    mcp_env: {gitlab: {GITLAB_TOKEN: "${SHARED}"}}
    roles:
      - {name: CTO}
`)
	if _, err := cfg.Organization(); err != nil {
		t.Fatal(err)
	}
	if len(cfg.Roles) != 1 || cfg.Roles[0].Name != "Contractor" {
		t.Fatalf("the root seat was moved in the config itself: %+v", cfg.Roles)
	}
	if len(cfg.Units[0].Roles) != 1 {
		t.Fatalf("the unit gained a member in the config itself: %d", len(cfg.Units[0].Roles))
	}
	if cfg.Units[0].Roles[0].MCPEnv != nil {
		t.Fatalf("inheritance leaked into the config: %v", cfg.Units[0].Roles[0].MCPEnv)
	}
}

// A dangling reference is not a validation failure: live config management
// bootstraps an org in pieces, so a unit can land before the seat that
// leads it, and refusing the revision would make that state unreachable.
func TestDanglingReferencesAreReportedNotRejected(t *testing.T) {
	t.Parallel()
	cfg := mustCompany(t, "name: Acme\nunits:\n  - {name: Core, lead: NotYetHired}\n")
	if len(cfg.DanglingRefs()) == 0 {
		t.Fatal("a dangling lead should be reported")
	}
}

// A toggle's third state is load-bearing: unset means "inherit", which is
// not the same answer as false.
func TestToggleKeepsItsThirdState(t *testing.T) {
	t.Parallel()
	unset := mustCompany(t, "name: Acme\nroles:\n  - {name: CEO}\n").Roles[0]
	if unset.LearningEnabled.IsSet() {
		t.Fatal("an unstated toggle must read as unset")
	}
	off := mustCompany(t, "name: Acme\nroles:\n  - {name: CEO, learning_enabled: false}\n").Roles[0]
	if !off.LearningEnabled.IsSet() || off.LearningEnabled.Or(true) {
		t.Fatal("an explicit false must be distinguishable from unset")
	}
}

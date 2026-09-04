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
		{"scalar", "name: Acme\nroles:\n  - {name: CEO, llm_review: big}\n", []string{"big"}},
		{"list", "name: Acme\nroles:\n  - {name: CEO, llm_review: [big, small]}\n", []string{"big", "small"}},
		{"valueless", "name: Acme\nroles:\n  - {name: CEO, llm_review: }\n", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cfg := mustCompany(t, tc.yaml)
			if got := []string(cfg.Roles[0].LLMReview); !slices.Equal(got, tc.want) {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
		})
	}
}

// The `llm` field takes a third shape as well: a mapping per phase, because
// the satellites do genuinely different work and splitting them by hand across
// flat fields is how five end up agreeing and one goes stale.
func TestPhaseLLMAcceptsAllThreeShapes(t *testing.T) {
	t.Parallel()

	t.Run("a scalar points every phase at one provider", func(t *testing.T) {
		t.Parallel()
		seat := mustCompany(t, "name: Acme\nroles:\n  - {name: CEO, llm: fast}\n").Roles[0].Seat()
		if !slices.Equal(seat.LLM, org.ProviderKeys{"fast"}) {
			t.Fatalf("llm = %v", seat.LLM)
		}
		if seat.LLMReview != nil {
			t.Fatalf("a scalar must not fabricate per-phase chains: %v", seat.LLMReview)
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
      review: [big, bigger]
      judge: cheap
`).Roles[0].Seat()
		if !slices.Equal(seat.LLM, org.ProviderKeys{"fast"}) {
			t.Fatalf("default = %v", seat.LLM)
		}
		if !slices.Equal(seat.LLMReview, org.ProviderKeys{"big", "bigger"}) {
			t.Fatalf("review = %v", seat.LLMReview)
		}
		if !slices.Equal(seat.LLMJudge, org.ProviderKeys{"cheap"}) {
			t.Fatalf("judge = %v", seat.LLMJudge)
		}
		// A phase the mapping did not name stays unset, so the runtime
		// falls back to the default chain rather than to a fabricated one.
		if seat.LLMSubagent != nil {
			t.Fatalf("subagent = %v", seat.LLMSubagent)
		}
	})

	t.Run("a flat field wins over the mapping", func(t *testing.T) {
		t.Parallel()
		seat := mustCompany(t, `
name: Acme
roles:
  - name: CEO
    llm: {default: fast, review: from-mapping}
    llm_review: from-flat
`).Roles[0].Seat()
		if !slices.Equal(seat.LLMReview, org.ProviderKeys{"from-flat"}) {
			t.Fatalf("review = %v", seat.LLMReview)
		}
	})
}

// The authored seat and the runtime seat are different shapes on purpose;
// the transform is where the difference is paid for, once.
func TestSeatTransform(t *testing.T) {
	t.Parallel()
	cfg := mustCompany(t, `
name: Acme
providers:
  sandbox:
    fake: true
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
    project: ENGP
    integrations:
      mattermost: {bot_token: "${MM_SWE}", username: swe-bot, channel: eng}
    sandbox:
      enabled: true
      run_in: container
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
	if seat.Project != "ENGP" {
		t.Fatalf("jira project = %q", seat.Project)
	}
	// The identity this seat did NOT author must stay empty: the two are
	// read from separate blocks, and a seat that names one must not come
	// out carrying the other from somewhere else.
	if seat.Space != "" {
		t.Fatalf("a seat that named no Confluence identity got %q",
			seat.Space)
	}
	if seat.Placement.Node != "node-2" || seat.Placement.Labels["zone"] != "eu" {
		t.Fatalf("placement = %+v", seat.Placement)
	}
	if seat.Sandbox == nil || !seat.Sandbox.Enabled || seat.Sandbox.CodingAgent != "opencode" ||
		seat.Sandbox.RunIn != "container" {
		t.Fatalf("sandbox = %+v", seat.Sandbox)
	}
	if seat.MCPEnv["gitlab"]["GITLAB_TOKEN"] != "${GL_SWE}" {
		t.Fatalf("mcp_env = %v", seat.MCPEnv)
	}
}

// THE OTHER HALF OF THAT TRANSFORM: the per-seat identities, which are three
// different things wearing one shape.
//
// A seat's Slack block is a CREDENTIAL — the webhook route resolves each
// seat's signing secret out of org.Role.Slack, so a seat that lost it stops
// verifying its own app's deliveries. Its Jira and Confluence blocks are
// ROUTING IDENTITY: where the seat files work, and where activity with no
// better recipient lands. All three are read off the authored config and
// none is declared anywhere else, so a transform that dropped one leaves a
// seat that looks complete and is unreachable.
//
// Through YAML rather than a struct literal, because the authored document
// is the surface this is a transform OF.
func TestSeatTransformCarriesEveryPerSeatIdentity(t *testing.T) {
	t.Parallel()
	cfg := mustCompany(t, `
name: Acme
roles:
  - name: Agent SWE
    handle: swe
    project: ENG
    space: ENGSPACE
    integrations:
      slack:
        bot_token: "${SLACK_SWE}"
        signing_secret: "${SIGN_SWE}"
        channel: C123
`)
	seat := cfg.Roles[0].Seat()

	if seat.Project != "ENG" || seat.Space != "ENGSPACE" {
		t.Errorf("identities = %q %q", seat.Project, seat.Space)
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

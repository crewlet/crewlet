package org

import (
	"errors"
	"slices"
	"strings"
	"testing"
)

// human returns a minimal valid human seat, with mutators applied.
func human(mutate ...func(*Role)) *Role {
	r := &Role{
		Name:    "Sarah Chen",
		Kind:    KindHuman,
		Email:   "sarah@example.com",
		Contact: &HumanContact{SlackUserID: "U0HUMAN"},
	}
	for _, m := range mutate {
		m(r)
	}
	return r
}

// lookupFrom builds an EnvLookup over a fixed map, so identity resolution
// is exercised without touching the process environment — which is what
// keeps these tests parallel.
func lookupFrom(vars map[string]string) EnvLookup {
	return func(name string) (string, bool) {
		v, ok := vars[name]
		return v, ok
	}
}

func TestHandleDerivationAndOverride(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name     string
		role     Role
		want     string
		wantFail bool
	}{
		{name: "derived from the name", role: Role{Name: "Sarah Chen"}, want: "sarah-chen"},
		{name: "explicit wins", role: Role{Name: "Senior Backend Engineer", DeclaredHandle: "sbe"}, want: "sbe"},
		{name: "explicit digits and hyphens", role: Role{Name: "Sarah", DeclaredHandle: "sarah-2"}, want: "sarah-2"},
		{name: "underscores rejected", role: Role{Name: "Sarah", DeclaredHandle: "Sarah_Chen"}, want: "Sarah_Chen", wantFail: true},
		{name: "leading hyphen rejected", role: Role{Name: "Sarah", DeclaredHandle: "-sarah"}, want: "-sarah", wantFail: true},
		{
			// The seat would derive no agent id and own no inbox: in the
			// chart, unreachable from everywhere else.
			name: "a name that slugifies to nothing", role: Role{Name: "###"}, want: "", wantFail: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := tc.role.Handle(); got != tc.want {
				t.Errorf("Handle() = %q, want %q", got, tc.want)
			}
			err := tc.role.Validate()
			if got := errors.Is(err, ErrInvalidHandle); got != tc.wantFail {
				t.Errorf("ErrInvalidHandle = %v, want %v (err: %v)", got, tc.wantFail, err)
			}
		})
	}
}

// TestMalformedHandleSuggestsAFix: the error has to be actionable, because
// the alternative is a seat that fails during engine start when external-id
// registration rejects the handle.
func TestMalformedHandleSuggestsAFix(t *testing.T) {
	t.Parallel()
	err := (&Role{Name: "Sarah", DeclaredHandle: "Sarah_Chen"}).Validate()
	if err == nil || !strings.Contains(err.Error(), `"sarah-chen"`) {
		t.Errorf("error does not suggest the slugified handle: %v", err)
	}
}

func TestSeatKindDefaultsToAgent(t *testing.T) {
	t.Parallel()
	r := &Role{Name: "Engineer"}
	if r.IsHuman() || !r.IsAgent() {
		t.Error("an unannotated seat is not an agent seat")
	}
	if err := r.Validate(); err != nil {
		t.Errorf("Validate() = %v, want nil", err)
	}
	if err := (&Role{Name: "X", Kind: "robot"}).Validate(); !errors.Is(err, ErrUnknownKind) {
		t.Errorf("Validate() = %v, want ErrUnknownKind", err)
	}
}

// TestHumanSeatRejectsEveryRuntimeField is the contract from
// docs/concepts/humans-in-the-org.md: a human seat is never spawned, so
// every field that only a running seat could use is dead config on one.
func TestHumanSeatRejectsEveryRuntimeField(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		field string
		set   func(*Role)
	}{
		{"llm", func(r *Role) { r.LLM = ProviderKeys{"gpt-4o"} }},
		{"llm_plan", func(r *Role) { r.LLMPlan = ProviderKeys{"gpt-4o"} }},
		{"llm_execute", func(r *Role) { r.LLMExecute = ProviderKeys{"gpt-4o"} }},
		{"llm_review", func(r *Role) { r.LLMReview = ProviderKeys{"gpt-4o"} }},
		{"llm_subagent", func(r *Role) { r.LLMSubagent = ProviderKeys{"gpt-4o"} }},
		{"llm_auxiliary", func(r *Role) { r.LLMAuxiliary = ProviderKeys{"gpt-4o"} }},
		{"llm_judge", func(r *Role) { r.LLMJudge = ProviderKeys{"gpt-4o"} }},
		{"llm_sandbox", func(r *Role) { r.LLMSandbox = ProviderKeys{"sb"} }},
		{"sandbox", func(r *Role) { r.Sandbox = &RoleSandbox{Enabled: true} }},
		{"token_budget", func(r *Role) { r.TokenBudget = 1000 }},
		{"learning_enabled", func(r *Role) { r.LearningEnabled = On() }},
		{"schedules", func(r *Role) {
			r.Schedules = []Schedule{{Name: "standup", Cron: "0 9 * * *", Task: "post"}}
		}},
		{"slack", func(r *Role) { r.Slack = SlackIdentity{BotToken: "xoxb-1"} }},
		{"mattermost", func(r *Role) { r.Mattermost = MattermostIdentity{BotToken: "mm-1"} }},
		{"integrations.jira", func(r *Role) { r.JiraProject = "ENG" }},
		{"integrations.confluence", func(r *Role) { r.ConfluenceSpace = "ENG" }},
		{"integrations.plane", func(r *Role) { r.PlaneProject = "eng" }},
		{"mcp_env", func(r *Role) { r.MCPEnv = MCPEnv{"atlassian": {"JIRA_USERNAME": "s"}} }},
		{"behavioral_guidelines", func(r *Role) { r.BehavioralGuidelines = []string{"Reply fast"} }},
	} {
		t.Run(tc.field, func(t *testing.T) {
			t.Parallel()
			err := human(tc.set).Validate()
			if !errors.Is(err, ErrHumanSeatField) {
				t.Fatalf("Validate() = %v, want ErrHumanSeatField", err)
			}
			// The message names the field an operator wrote, not a Go one:
			// its whole job is pointing at a line in their config.
			if !strings.Contains(err.Error(), tc.field) {
				t.Errorf("error does not name %q: %v", tc.field, err)
			}
		})
	}
	// A learning_enabled explicitly set to FALSE is still config a human
	// seat must not carry — the tri-state is what makes that detectable.
	if err := human(func(r *Role) { r.LearningEnabled = Off() }).Validate(); !errors.Is(err, ErrHumanSeatField) {
		t.Errorf("learning_enabled: false accepted on a human seat: %v", err)
	}
}

func TestHumanSeatKeepsItsDescriptiveFields(t *testing.T) {
	t.Parallel()
	// These are ROUTING CONTEXT, not decoration: they render into a lead's
	// roster and into colleague lookups, which is how work reaches the
	// person who owns it.
	r := human(func(r *Role) {
		r.Goal = "Keep the team unblocked"
		r.Backstory = "20 years in infrastructure"
		r.Responsibilities = []string{"Approvals", "Vendor calls"}
		r.Availability = "CET business hours; replies within ~4h"
		r.Manages = []string{"Dev"}
	})
	if err := r.Validate(); err != nil {
		t.Fatalf("Validate() = %v, want nil", err)
	}
}

func TestHumanSeatNeedsAContactIdentity(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		contact *HumanContact
	}{
		{"absent", nil},
		{"empty block", &HumanContact{}},
		{"whitespace only", &HumanContact{SlackUserID: "   "}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := human(func(r *Role) { r.Contact = tc.contact })
			r.Contact.Normalize()
			if err := r.Validate(); !errors.Is(err, ErrNoContact) {
				t.Errorf("Validate() = %v, want ErrNoContact", err)
			}
		})
	}
	// A ${VAR} reference IS a declared identity — the id is instance
	// specific and lives in the environment, not in a committed file.
	r := human(func(r *Role) { r.Contact = &HumanContact{PlaneUserID: "${PLANE_FOUNDER_USER_ID}"} })
	if err := r.Validate(); err != nil {
		t.Errorf("Validate() = %v, want nil", err)
	}
}

func TestAgentSeatRejectsHumanOnlyFields(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		field string
		set   func(*Role)
	}{
		{"contact", func(r *Role) { r.Contact = &HumanContact{SlackUserID: "U1"} }},
		{"availability", func(r *Role) { r.Availability = "9-5 CET" }},
	} {
		t.Run(tc.field, func(t *testing.T) {
			t.Parallel()
			r := &Role{Name: "Engineer"}
			tc.set(r)
			err := r.Validate()
			if !errors.Is(err, ErrAgentSeatField) {
				t.Fatalf("Validate() = %v, want ErrAgentSeatField", err)
			}
			if !strings.Contains(err.Error(), "kind: human") {
				t.Errorf("error does not suggest the likely fix: %v", err)
			}
		})
	}
}

func TestRoleValidateReportsEveryProblemAtOnce(t *testing.T) {
	t.Parallel()
	// A config author fixing one field per round trip is the failure mode
	// aggregation exists to prevent.
	r := human(func(r *Role) {
		r.DeclaredHandle = "Sarah_Chen"
		r.TokenBudget = 10
		r.Contact = &HumanContact{}
	})
	err := r.Validate()
	for _, want := range []error{ErrInvalidHandle, ErrHumanSeatField, ErrNoContact} {
		if !errors.Is(err, want) {
			t.Errorf("Validate() = %v, missing %v", err, want)
		}
	}
}

func TestNamelessSeatIsRejected(t *testing.T) {
	t.Parallel()
	// A seat with no name derives no handle, and therefore no agent id and
	// no inbox: it is in the chart and unreachable from everywhere else.
	if err := (&Role{}).Validate(); !errors.Is(err, ErrMissingName) {
		t.Errorf("Validate() = %v, want ErrMissingName", err)
	}
}

func TestContactNormalization(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		in   HumanContact
		want HumanContact
	}{
		{
			name: "case-normalised fields are lowercased",
			in:   HumanContact{GitHubLogin: "JaneDoe", GitLabUsername: "JaneDoe", PlaneUserID: "AB12CD34"},
			want: HumanContact{GitHubLogin: "janedoe", GitLabUsername: "janedoe", PlaneUserID: "ab12cd34"},
		},
		{
			name: "opaque ids keep their case",
			in:   HumanContact{SlackUserID: "U0HUMAN", AtlassianAccountID: "5b10AC8d", MattermostUserID: "Sarah.Chen"},
			want: HumanContact{SlackUserID: "U0HUMAN", AtlassianAccountID: "5b10AC8d", MattermostUserID: "Sarah.Chen"},
		},
		{
			name: "whitespace is never part of an identity",
			in:   HumanContact{SlackUserID: "  U1  ", GitHubLogin: "  JaneDoe "},
			want: HumanContact{SlackUserID: "U1", GitHubLogin: "janedoe"},
		},
		{
			// Lowercasing a reference would make it permanently
			// unresolvable: variable names are case-sensitive.
			name: "a whole ${VAR} reference is stored verbatim",
			in:   HumanContact{GitHubLogin: "${GH_FOUNDER_LOGIN}", PlaneUserID: " ${PLANE_FOUNDER_USER_ID} "},
			want: HumanContact{GitHubLogin: "${GH_FOUNDER_LOGIN}", PlaneUserID: "${PLANE_FOUNDER_USER_ID}"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := tc.in
			got.Normalize()
			if got != tc.want {
				t.Errorf("Normalize() = %+v, want %+v", got, tc.want)
			}
			// Idempotent: normalisation runs on every config apply.
			again := got
			again.Normalize()
			if again != got {
				t.Errorf("second Normalize() changed %+v to %+v", got, again)
			}
		})
	}
}

func TestContactRejectsEmbeddedEnvRefs(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		field string
		in    HumanContact
	}{
		{"github_login", HumanContact{GitHubLogin: "acme-${GH_SUFFIX}"}},
		{"slack_user_id", HumanContact{SlackUserID: "U${SLACK_SUFFIX}"}},
		// Two adjacent whole references still resolve to a concatenation,
		// which is not an identity.
		{"plane_user_id", HumanContact{PlaneUserID: "${A}${B}"}},
	} {
		t.Run(tc.field, func(t *testing.T) {
			t.Parallel()
			c := tc.in
			c.Normalize()
			err := c.Validate()
			if !errors.Is(err, ErrEmbeddedEnvRef) {
				t.Fatalf("Validate() = %v, want ErrEmbeddedEnvRef", err)
			}
			if !strings.Contains(err.Error(), tc.field) {
				t.Errorf("error does not name %q: %v", tc.field, err)
			}
		})
	}
}

// TestResolvedIdentitiesEnumeration pins the transport table: one Atlassian
// account id answers for both Jira and Confluence, and the order is the one
// registration and rendering walk.
func TestResolvedIdentitiesEnumeration(t *testing.T) {
	t.Parallel()
	c := HumanContact{SlackUserID: "U1", AtlassianAccountID: "abc", GitHubLogin: "jane"}
	got := c.ResolvedIdentities(lookupFrom(nil))
	want := []Identity{
		{TransportSlack, "U1"},
		{TransportJira, "abc"},
		{TransportConfluence, "abc"},
		{TransportGitHub, "jane"},
	}
	if !slices.Equal(got, want) {
		t.Errorf("ResolvedIdentities() = %v, want %v", got, want)
	}
}

// TestIdentitiesAreConfigVerbatim: the operator-facing view shows what the
// file says, references and all — the resolved view is a different question
// and a different method.
func TestIdentitiesAreConfigVerbatim(t *testing.T) {
	t.Parallel()
	c := HumanContact{SlackUserID: "U1", PlaneUserID: "${PLANE_FOUNDER_USER_ID}"}
	want := []Identity{{TransportSlack, "U1"}, {TransportPlane, "${PLANE_FOUNDER_USER_ID}"}}
	if got := c.Identities(); !slices.Equal(got, want) {
		t.Errorf("Identities() = %v, want %v", got, want)
	}
	if got := (*HumanContact)(nil).Identities(); got != nil {
		t.Errorf("nil contact yielded %v", got)
	}
}

func TestResolvedIdentitiesResolveThenNormalize(t *testing.T) {
	t.Parallel()
	c := HumanContact{SlackUserID: "U1", PlaneUserID: "${PLANE_FOUNDER_USER_ID}"}
	env := lookupFrom(map[string]string{"PLANE_FOUNDER_USER_ID": " AB12CD34-0000 "})
	got := c.ResolvedIdentities(env)
	want := []Identity{{TransportSlack, "U1"}, {TransportPlane, "ab12cd34-0000"}}
	if !slices.Equal(got, want) {
		t.Errorf("ResolvedIdentities() = %v, want %v", got, want)
	}
}

// TestResolvedIdentitiesOmitUnresolved: emitting the raw ${VAR} text would
// poison registration and mention markup with a string no payload can ever
// match, and the symptom would be a person who mysteriously never gets
// mentioned.
func TestResolvedIdentitiesOmitUnresolved(t *testing.T) {
	t.Parallel()
	c := HumanContact{SlackUserID: "U1", PlaneUserID: "${PLANE_FOUNDER_USER_ID}"}
	for _, tc := range []struct {
		name string
		env  EnvLookup
	}{
		{"unset", lookupFrom(nil)},
		{"set but empty", lookupFrom(map[string]string{"PLANE_FOUNDER_USER_ID": "  "})},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := c.ResolvedIdentities(tc.env)
			if !slices.Equal(got, []Identity{{TransportSlack, "U1"}}) {
				t.Errorf("ResolvedIdentities() = %v, want only the slack identity", got)
			}
		})
	}
	// The declared-but-unresolved identity still counts for validation:
	// the operator committed to it, the environment just has not caught up.
	if c.IsEmpty() {
		t.Error("a contact carrying only a reference reads as empty")
	}
}

// TestContactFieldTableIsConsistent guards the indices the transport table
// is written against: a reordered field list would silently register Slack
// ids as GitHub logins.
func TestContactFieldTableIsConsistent(t *testing.T) {
	t.Parallel()
	want := []string{
		"slack_user_id", "mattermost_user_id", "atlassian_account_id",
		"github_login", "gitlab_username", "plane_user_id",
	}
	for i, key := range want {
		if contactFields[i].key != key {
			t.Fatalf("contactFields[%d] = %q, want %q", i, contactFields[i].key, key)
		}
	}
	if len(contactFields) != len(want) {
		t.Fatalf("contactFields has %d entries, want %d", len(contactFields), len(want))
	}
	// Every transport must address a field that exists, and the ones that
	// share an account id must share the field.
	for _, t2 := range contactTransports {
		if t2.field < 0 || t2.field >= len(contactFields) {
			t.Fatalf("transport %q addresses field %d", t2.transport, t2.field)
		}
	}
	if contactFields[fieldAtlassianAccountID].key != "atlassian_account_id" {
		t.Error("jira and confluence no longer share the atlassian account id")
	}
}

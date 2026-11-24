package config_test

import (
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/config"
)

// fleetDoc is a company with two Claude Code Opus sign-ins, a Sonnet one and
// an Opus one on another harness, whose fleets, seat and helpers a case fills.
const fleetDoc = `
name: Acme
providers:
  sandbox: {fake: true}
  llm:
    claude-max: {model: opus, auth: {mode: subscription}}
    claude-team: {model: claude-opus-5-5, effort: medium, auth: {mode: subscription}}
    claude-api: {model: opus, auth: {mode: api-key, api_key: "${ANTHROPIC_API_KEY}"}}
    sonnet: {model: sonnet}
    open-opus: {harness: opencode, model: anthropic/claude-opus-5-5, env: {ANTHROPIC_API_KEY: "${K}"}}
  llm_fleets:
%s
roles:
  - name: Engineer
    handle: engineer
%s
%s
`

// parseFleetDoc decodes fleetDoc with its three holes filled, unvalidated.
func parseFleetDoc(t *testing.T, fleets, role, rest string) *config.Company {
	t.Helper()
	if fleets == "" {
		fleets = "    claude-opus: {name: Claude Opus, members: [claude-max, claude-team]}"
	}
	doc := strings.Replace(fleetDoc, "%s", fleets, 1)
	doc = strings.Replace(doc, "%s", role, 1)
	doc = strings.Replace(doc, "%s", rest, 1)
	c, err := config.ParseCompanyDocument([]byte(doc))
	if err != nil {
		t.Fatalf("parse: %v\n%s", err, doc)
	}
	return c
}

// said is every problem's words for a person in err, with its path.
func said(err error) map[string]string {
	out := map[string]string{}
	for _, p := range config.Problems(err) {
		out[p.Path] = p.Said
	}
	return out
}

// Every fleet rule refuses where it is written, in the words the dashboard
// shows for it, and the shapes the contract allows validate.
func TestEveryFleetRuleRefusesWhereWrittenInItsWords(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name, fleets, role, rest string
		path, said               string // "" path: valid
	}{
		{name: "members of one class differing in effort and sign-in",
			fleets: "    claude-opus: {name: Claude Opus, members: [claude-max, claude-team, claude-api]}"},
		{name: "an empty fleet", fleets: "    claude-opus: {name: Claude Opus}"},
		{name: "an entry in two fleets", fleets: "    a: {name: A, members: [claude-max]}\n" +
			"    b: {name: B, members: [claude-max, claude-api]}"},
		{name: "sequential with a threshold",
			fleets: "    claude-opus: {name: Claude Opus, strategy: sequential, members: [claude-max], threshold_points: 50}"},
		{name: "a member that is not an entry",
			fleets: "    claude-opus: {name: Claude Opus, members: [claude-max, nope]}",
			path:   "providers.llm_fleets.claude-opus.members[1]", said: "nope is not a model under Models"},
		{name: "a member listed twice",
			fleets: "    claude-opus: {name: Claude Opus, members: [claude-max, claude-max]}",
			path:   "providers.llm_fleets.claude-opus.members[1]", said: "claude-max is listed twice"},
		{name: "a member of another class",
			fleets: "    claude-opus: {name: Claude Opus, members: [claude-max, sonnet]}",
			path:   "providers.llm_fleets.claude-opus.members[1]", said: "Runs Claude Sonnet; this fleet runs Claude Opus."},
		{name: "a member on another harness",
			fleets: "    claude-opus: {name: Claude Opus, members: [claude-max, open-opus]}",
			path:   "providers.llm_fleets.claude-opus.members[1]",
			said:   "Runs Claude Opus on opencode; this fleet runs Claude Opus on claude-code."},
		{name: "no name", fleets: "    claude-opus: {members: [claude-max]}",
			path: "providers.llm_fleets.claude-opus.name", said: "Give the fleet a name."},
		{name: "a blank name", fleets: `    claude-opus: {name: "  "}`,
			path: "providers.llm_fleets.claude-opus.name", said: "Give the fleet a name."},
		{name: "a name over 60 characters", fleets: "    claude-opus: {name: " + strings.Repeat("é", 61) + "}",
			path: "providers.llm_fleets.claude-opus.name", said: "Use at most 60 characters."},
		{name: "a name another fleet has, ignoring case",
			fleets: "    a: {name: Claude Opus}\n    b: {name: claude opus}",
			path:   "providers.llm_fleets.b.name", said: "Another fleet is called Claude Opus."},
		{name: "a name default-fleet has", fleets: "    a: {name: Default-Fleet}",
			path: "providers.llm_fleets.default-fleet.name", said: "Another fleet is called Default-Fleet."},
		{name: "default-fleet renamed", fleets: "    default-fleet: {name: Everyone}",
			path: "providers.llm_fleets.default-fleet.name", said: "The default fleet can't be renamed."},
		{name: "an unknown strategy", fleets: "    claude-opus: {name: Claude Opus, strategy: random}",
			path: "providers.llm_fleets.claude-opus.strategy"},
		{name: "a threshold over 50", fleets: "    claude-opus: {name: Claude Opus, threshold_points: 51}",
			path: "providers.llm_fleets.claude-opus.threshold_points"},
		{name: "a negative threshold", fleets: "    claude-opus: {name: Claude Opus, threshold_points: -1}",
			path: "providers.llm_fleets.claude-opus.threshold_points"},
		{name: "a key outside the grammar", fleets: "    Claude_Opus: {name: Claude Opus}",
			path: "providers.llm_fleets.Claude_Opus", said: "Claude_Opus is not a fleet key: use lowercase " +
				"letters, digits and hyphens, e.g. claude-opus."},
		{name: "a key ending in a hyphen", fleets: "    claude-: {name: Claude}",
			path: "providers.llm_fleets.claude-", said: "claude- is not a fleet key: use lowercase " +
				"letters, digits and hyphens, e.g. claude."},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := parseFleetDoc(t, tc.fleets, tc.role, tc.rest).Validate()
			if tc.path == "" {
				if err != nil {
					t.Fatalf("refused: %v", err)
				}
				return
			}
			got, ok := said(err)[tc.path]
			if !ok {
				t.Fatalf("no problem at %s: %v", tc.path, err)
			}
			if tc.said != "" && got != tc.said {
				t.Fatalf("said %q, want %q", got, tc.said)
			}
		})
	}
}

// fleet:<key> is accepted in every place one entry key is, and refused inside
// a list or naming no fleet, at the field written.
func TestAFleetReferenceIsAcceptedWhereOneKeyIsAndRefusedInAList(t *testing.T) {
	t.Parallel()
	accepted := []struct{ name, role, rest string }{
		{name: "llm", role: "    llm: fleet:claude-opus"},
		{name: "llm in a mapping", role: "    llm: {default: fleet:claude-opus, review: fleet:default-fleet}"},
		{name: "each phase in the mapping", role: "    llm:\n      default: claude-max\n" +
			"      review: fleet:claude-opus\n      subagent: fleet:claude-opus\n      auxiliary: fleet:claude-opus\n" +
			"      judge: fleet:claude-opus\n      triage: fleet:claude-opus"},
		{name: "each flat phase", role: "    llm_review: fleet:claude-opus\n    llm_subagent: fleet:claude-opus\n" +
			"    llm_auxiliary: fleet:claude-opus\n    llm_judge: fleet:claude-opus\n    llm_triage: fleet:claude-opus"},
		{name: "default-fleet, synthesised", role: "    llm: fleet:default-fleet"},
		{name: "assistant.model", rest: "assistant: {model: fleet:claude-opus}"},
		{name: "advisor.model", rest: "advisor: {model: fleet:default-fleet}"},
	}
	for _, tc := range accepted {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if err := parseFleetDoc(t, "", tc.role, tc.rest).Validate(); err != nil {
				t.Fatalf("refused: %v", err)
			}
		})
	}
	refused := []struct{ name, role, rest, path, said string }{
		{"llm in a list", "    llm: [fleet:claude-opus, claude-api]", "", "roles[0].llm",
			"A fleet stands alone: choose the fleet or a list of models"},
		{"a phase in a list", "    llm_review: [claude-max, fleet:claude-opus]", "", "roles[0].llm_review",
			"A fleet stands alone: choose the fleet or a list of models"},
		{"two fleets in a list", "    llm: {review: [fleet:claude-opus, fleet:default-fleet]}", "",
			"roles[0].llm.review", "A fleet stands alone: choose the fleet or a list of models"},
		{"llm naming no fleet", "    llm: fleet:nope", "", "roles[0].llm", "nope is not a fleet"},
		{"a phase naming no fleet", "    llm: {review: fleet:nope}", "", "roles[0].llm.review", "nope is not a fleet"},
		{"assistant naming no fleet", "", "assistant: {model: fleet:nope}", "assistant.model", "nope is not a fleet"},
		{"advisor naming no fleet", "", "advisor: {model: fleet:nope}", "advisor.model", "nope is not a fleet"},
	}
	for _, tc := range refused {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := parseFleetDoc(t, "", tc.role, tc.rest).Validate()
			if got := said(err)[tc.path]; got != tc.said {
				t.Fatalf("at %s said %q, want %q: %v", tc.path, got, tc.said, err)
			}
		})
	}
}

// A fleet reference is held to its fleet even before any model exists: a seat
// on default-fleet validates in a company with no providers.llm.
func TestAFleetReferenceIsHeldWithNoModels(t *testing.T) {
	t.Parallel()
	doc := "name: Acme\nroles:\n  - name: Engineer\n    handle: engineer\n    llm: fleet:%s\n"
	if _, err := config.ParseCompany([]byte(strings.Replace(doc, "%s", config.DefaultFleetKey, 1))); err != nil {
		t.Fatalf("a seat on default-fleet with no models was refused: %v", err)
	}
	_, err := config.ParseCompany([]byte(strings.Replace(doc, "%s", "nope", 1)))
	if !errors.Is(err, config.ErrUnknownValue) {
		t.Fatalf("a seat on a fleet nobody has, with no models, validated: %v", err)
	}
}

// A seat job or helper on a fleet with no members is valid and warned of, once
// per subject and fleet, in the dashboard's words; none on a fleet with one.
func TestAJobOnAnEmptyFleetIsAWarningOnly(t *testing.T) {
	t.Parallel()
	c := parseFleetDoc(t, "    claude-opus: {name: Claude Opus, members: [claude-max]}",
		"    llm: fleet:default-fleet\n    llm_review: fleet:default-fleet\n    llm_judge: fleet:claude-opus",
		"assistant: {model: fleet:default-fleet}")
	if err := c.Validate(); err != nil {
		t.Fatalf("a seat on an empty fleet was refused: %v", err)
	}
	var got []string
	for _, w := range c.AdvisoryWarnings() {
		if strings.Contains(w.Message, "has no members yet") {
			got = append(got, w.Path+": "+w.Message)
		}
	}
	want := []string{
		"roles[0].llm: default-fleet has no members yet: Engineer waits until one joins",
		"assistant.model: default-fleet has no members yet: Ask Crewlet waits until one joins",
	}
	if !slices.Equal(got, want) {
		t.Fatalf("warnings\n%q\nwant\n%q", got, want)
	}
}

// A helper on a fleet runs on any member, so every member is held to the
// helper's rule, and the engine builds a helper's run of each.
func TestAHelperOnAFleetHoldsEveryMember(t *testing.T) {
	t.Parallel()
	c := parseFleetDoc(t, "    claude-opus: {name: Claude Opus, members: [claude-max, claude-team]}", "",
		"assistant: {model: fleet:claude-opus}\nadvisor: {model: claude-team}")
	if err := c.HelperModelProblem("fleet:claude-opus"); err != nil {
		t.Fatalf("a fleet of helper-capable members was refused: %v", err)
	}
	if got := c.HelperKeys(); !slices.Equal(got, []string{"claude-max", "claude-team"}) {
		t.Fatalf("HelperKeys() = %v, want both members once", got)
	}
	if model, err := c.AssistantModel(); err != nil || model != "fleet:claude-opus" {
		t.Fatalf("AssistantModel() = %q, %v", model, err)
	}
}

// Fleet membership is a written reference to the entry, so a removal must
// answer it; a fleet's own references are its seat jobs and helpers.
func TestFleetMembershipAndFleetUseAreReferences(t *testing.T) {
	t.Parallel()
	c := parseFleetDoc(t, "", "    llm: fleet:claude-opus\n    llm_review: claude-max",
		"assistant: {model: fleet:claude-opus}")
	var got []string
	for _, ref := range c.ProviderReferences("claude-max") {
		got = append(got, string(ref.Job)+" "+ref.Fleet+" "+ref.Segments.String())
	}
	want := []string{"review  roles[0].llm_review", " claude-opus providers.llm_fleets.claude-opus.members"}
	if !slices.Equal(got, want) {
		t.Fatalf("references to claude-max = %q, want %q", got, want)
	}
	got = nil
	for _, ref := range c.ProviderReferences(config.FleetRef("claude-opus")) {
		got = append(got, string(ref.Job)+" "+ref.Segments.String())
	}
	want = []string{"executor roles[0].llm", "assistant assistant.model"}
	if !slices.Equal(got, want) {
		t.Fatalf("references to fleet:claude-opus = %q, want %q", got, want)
	}
}

// A seat's executor on a fleet is judged on its members: one of harnesses that
// cannot run an executor is refused at the seat's llm. `custom` stands for
// them, as the one harness that never gets a runner ([config.RunnerlessProfile]).
func TestAnExecutorOnAFleetIsJudgedOnItsMembers(t *testing.T) {
	t.Parallel()
	house := "    house: {model: house-model, harness: custom, overrides: " + config.RunnerlessProfile + "}\n  llm_fleets:"
	doc := strings.Replace(fleetDoc, "  llm_fleets:", house, 1)
	doc = strings.Replace(doc, "%s", "    house: {name: House, members: [house]}", 1)
	doc = strings.Replace(doc, "%s", "    llm: fleet:house", 1)
	doc = strings.Replace(doc, "%s", "", 1)
	c, err := config.ParseCompanyDocument([]byte(doc))
	if err != nil {
		t.Fatalf("parse: %v\n%s", err, doc)
	}
	err = c.Validate()
	if !errors.Is(err, config.ErrConflict) || !strings.Contains(err.Error(), "runs on fleet house") {
		t.Fatalf("a seat whose fleet cannot run its executor validated: %v", err)
	}
}

// A seat pinned to an entry or a list resolves, validates and is referenced
// exactly as before, whatever fleets the company holds.
func TestAPinnedSeatIsUnchangedByFleets(t *testing.T) {
	t.Parallel()
	for _, role := range []string{"    llm: claude-max", "    llm: [claude-max, claude-api]\n    llm_review: sonnet", ""} {
		with := parseFleetDoc(t, "    claude-opus: {name: Claude Opus, members: [claude-team, claude-api]}", role, "")
		without := parseFleetDoc(t, "    {}", role, "")
		without.Providers.LLMFleets = nil
		if err := with.Validate(); err != nil {
			t.Fatalf("%q with fleets refused: %v", role, err)
		}
		for _, key := range []string{"claude-max", "claude-api", "sonnet"} {
			var a, b []string
			for _, ref := range with.ProviderReferences(key) {
				if ref.Fleet == "" {
					a = append(a, ref.Segments.String())
				}
			}
			for _, ref := range without.ProviderReferences(key) {
				b = append(b, ref.Segments.String())
			}
			if !slices.Equal(a, b) {
				t.Errorf("%q: seat references to %s are %v with fleets, %v without", role, key, a, b)
			}
		}
		for seat := range with.EachRole() {
			for other := range without.EachRole() {
				if got, want := with.ExecutorChain(seat), without.ExecutorChain(other); !slices.Equal(got, want) {
					t.Errorf("%q: executor chain %v with fleets, %v without", role, got, want)
				}
			}
		}
	}
}

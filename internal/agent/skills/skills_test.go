package skills_test

import (
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/prompts"
	"github.com/crewlet/crewlet/internal/agent/skills"
)

func skillFile(body string) string {
	return `---
key: chat-mentions
title: Mentioning people on chat
summary: How to write an @-mention so it actually pings
phases: [execute, review]
trigger:
  mcp_server: mattermost
---
` + body
}

func parse(t *testing.T, text string) skills.Skill {
	t.Helper()
	s, err := skills.Parse(text, skills.Source{PageID: "page-1", Version: 3})
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	return s
}

// ── the authoring format ──

func TestASkillIsFrontmatterPlusABody(t *testing.T) {
	t.Parallel()
	s := parse(t, skillFile("Write `@username`, not `<@id>`."))

	if s.Key != "chat-mentions" || s.Title != "Mentioning people on chat" {
		t.Fatalf("skill = %+v", s)
	}
	if s.Body != "Write `@username`, not `<@id>`." {
		t.Fatalf("body = %q", s.Body)
	}
	if !slices.Equal(s.Phases, []prompts.Phase{prompts.PhaseExecute, prompts.PhaseReview}) {
		t.Fatalf("phases = %v", s.Phases)
	}
	if s.SourcePageID != "page-1" || s.SourcePageVersion != 3 {
		t.Fatalf("provenance = %q/%d", s.SourcePageID, s.SourcePageVersion)
	}
}

// REQUIRED BY DEFAULT, and the default lives in one place so a file that
// omits the key and a skill built directly cannot disagree about what
// omitting it means.
func TestASkillIsRequiredUnlessItSaysOtherwise(t *testing.T) {
	t.Parallel()
	if !parse(t, skillFile("body")).Required {
		t.Fatal("a skill that says nothing is not required")
	}
	advisory := strings.Replace(skillFile("body"),
		"phases: [execute, review]", "phases: [execute]\nrequired: false", 1)
	if parse(t, advisory).Required {
		t.Fatal("required: false was ignored")
	}
}

// AN UNKNOWN KEY IS A TYPO, and a typo in `requred:` is a skill that
// silently stops being enforced. The knowledge base is edited by people, so
// the typo is the likely case rather than the exotic one.
func TestAMisspelledKeyIsRefusedRatherThanIgnored(t *testing.T) {
	t.Parallel()
	typo := strings.Replace(skillFile("body"), "summary:", "requred: false\nsummary:", 1)
	if _, err := skills.Parse(typo, skills.Source{}); err == nil {
		t.Fatal("a misspelled frontmatter key was accepted")
	}
}

// A PHASE NOBODY RECOGNISES IS AN ERROR rather than a skip: it would
// otherwise produce a skill offered in no phase, which looks from every
// angle like a skill nobody wrote.
func TestAnUnknownPhaseIsRefused(t *testing.T) {
	t.Parallel()
	wrong := strings.Replace(skillFile("body"), "[execute, review]", "[executed]", 1)
	if _, err := skills.Parse(wrong, skills.Source{}); err == nil {
		t.Fatal("an unknown phase was accepted")
	}
}

func TestASkillNeedsAFrontmatterASummaryAndATrigger(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ name, text string }{
		{"no frontmatter", "just a body"},
		{"no summary", strings.Replace(skillFile("b"),
			"summary: How to write an @-mention so it actually pings", "summary: ''", 1)},
		{"no trigger", strings.Replace(skillFile("b"),
			"trigger:\n  mcp_server: mattermost", "trigger: {}", 1)},
		{"two triggers", strings.Replace(skillFile("b"),
			"  mcp_server: mattermost", "  mcp_server: mattermost\n  tool: post", 1)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if _, err := skills.Parse(tc.text, skills.Source{}); err == nil {
				t.Fatalf("%s was accepted", tc.name)
			}
		})
	}
}

// The ADMISSION TEST: a knowledge container holds ordinary pages beside its
// skills, and telling them apart is what keeps a project's home page from
// producing a decode failure on every walk.
func TestOnlyAPageWithATriggerIsASkill(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		text string
		want bool
	}{
		{"a skill", skillFile("body"), true},
		{"a plain page", "# Onboarding\n\nRead this first.", false},
		{"frontmatter with no trigger", "---\ntitle: Notes\n---\nbody", false},
		{"malformed frontmatter", "---\n: : :\n---\nbody", false},
		{"empty", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := skills.IsSkill(tc.text); got != tc.want {
				t.Fatalf("IsSkill = %v, want %v", got, tc.want)
			}
		})
	}
}

// ── triggers ──

func surface(tools []string, servers ...string) prompts.Surface {
	return prompts.Surface{Tools: tools, MCPServers: servers}
}

func TestATriggerFiresForTheSurfaceItNames(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		trigger skills.Trigger
		on      prompts.Surface
		want    bool
	}{
		{"a server it has", skills.Trigger{MCPServer: "gitlab"},
			surface(nil, "gitlab"), true},
		{"a server it does not", skills.Trigger{MCPServer: "gitlab"},
			surface(nil, "jira"), false},
		{"a tool it has", skills.Trigger{Tool: "post_message"},
			surface([]string{"post_message"}), true},
		{"any_of, one matching", skills.Trigger{AnyOf: []skills.Trigger{
			{Tool: "absent"}, {Tool: "post_message"}}},
			surface([]string{"post_message"}), true},
		{"any_of, none matching", skills.Trigger{AnyOf: []skills.Trigger{
			{Tool: "absent"}, {Tool: "also_absent"}}},
			surface([]string{"post_message"}), false},
		{"all_of, all matching", skills.Trigger{AllOf: []skills.Trigger{
			{Tool: "post_message"}, {MCPServer: "gitlab"}}},
			surface([]string{"post_message"}, "gitlab"), true},
		{"all_of, one missing", skills.Trigger{AllOf: []skills.Trigger{
			{Tool: "post_message"}, {MCPServer: "gitlab"}}},
			surface([]string{"post_message"}), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := tc.trigger.Matches(tc.on); got != tc.want {
				t.Fatalf("Matches = %v, want %v", got, tc.want)
			}
		})
	}
}

// COVERS IS A DIFFERENT QUESTION FROM MATCHES: one asks whether the skill
// applies to the surface, the other whether THIS tool is one the skill is
// about. A skill gates only the tools its trigger names, never every tool on
// a surface that happened to catalogue it.
func TestCoversNamesTheToolsASkillIsAbout(t *testing.T) {
	t.Parallel()
	server := skills.Trigger{MCPServer: "gitlab"}
	if !server.Covers("create_mr", "gitlab") {
		t.Fatal("a server trigger does not cover its server's tools")
	}
	if server.Covers("create_mr", "jira") {
		t.Fatal("a server trigger covers another server's tool of the same name")
	}
	// A BUILTIN has no server, and a server trigger must not cover it —
	// otherwise one MCP skill gates every builtin on the surface.
	if server.Covers("lookup_colleague", "") {
		t.Fatal("a server trigger covers a builtin")
	}

	// A COMPOSITE UNIONS, all_of included: an all_of skill is about the
	// COMBINATION of its surfaces, so once the whole trigger has matched,
	// each of those surfaces' tools is covered.
	both := skills.Trigger{AllOf: []skills.Trigger{
		{Tool: "create_mr"}, {MCPServer: "mattermost"}}}
	for _, tc := range []struct{ tool, server string }{
		{"create_mr", ""}, {"post_message", "mattermost"},
	} {
		if !both.Covers(tc.tool, tc.server) {
			t.Fatalf("all_of does not cover %q on %q", tc.tool, tc.server)
		}
	}
}

// Exact-string matching is validated nowhere else, so a renamed upstream
// tool silently disables the skill AND the guard enforcing it.
func TestTriggerLivenessSeparatesDriftFromAForeignStack(t *testing.T) {
	t.Parallel()
	drift := skills.Trigger{AnyOf: []skills.Trigger{
		{Tool: "post_message"}, {Tool: "postMessage"}}}
	verdict := drift.Classify([]string{"post_message"}, nil)
	if !verdict.Live || !slices.Equal(verdict.Dangling, []string{"postMessage"}) {
		t.Fatalf("drift classified as %+v, want live with one dangling name", verdict)
	}

	foreign := skills.Trigger{Tool: "jira_transition"}
	if got := foreign.Classify([]string{"post_message"}, []string{"mattermost"}); got.Live {
		t.Fatalf("a wholly-dead trigger classified as live: %+v", got)
	}

	// A SERVER LEAF keeps a skill live even when its tool leaf dangles:
	// the skill still applies to that server's other tools.
	mixed := skills.Trigger{AnyOf: []skills.Trigger{
		{Tool: "renamed_away"}, {MCPServer: "mattermost"}}}
	if got := mixed.Classify(nil, []string{"mattermost"}); !got.Live {
		t.Fatalf("a live server leaf did not keep the skill live: %+v", got)
	}
}

// ── variables ──

// ONLY THE BRACED FORM, and only a known key. A skill body is tool
// documentation dense with shell, regex and currency `$`, and a substituter
// that touched any of it would corrupt the prose it exists to deliver.
func TestSubstitutionTouchesOnlyBracedKnownIdentifiers(t *testing.T) {
	t.Parallel()
	vars := map[string]string{"tenant": "nimbus", "empty": ""}
	for _, tc := range []struct{ in, want string }{
		{"go to ${tenant}.example.com", "go to nimbus.example.com"},
		{"an empty value renders empty: [${empty}]", "an empty value renders empty: []"},
		// Left VERBATIM: an unknown reference shows up as something a
		// reader can recognise and grep for, where blanking it reads as
		// authored prose with a hole in it.
		{"${nobody} defined this", "${nobody} defined this"},
		{"a bare $tenant is not a reference", "a bare $tenant is not a reference"},
		{"$$tenant stays", "$$tenant stays"},
		{"a single brace {page_id} is the body's own", "a single brace {page_id} is the body's own"},
		{"${a.b} is not an identifier", "${a.b} is not an identifier"},
		{"echo $PATH | grep '^\\$'", "echo $PATH | grep '^\\$'"},
		{"", ""},
	} {
		if got := skills.Substitute(tc.in, vars); got != tc.want {
			t.Fatalf("Substitute(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
	// With no variables at all it is the identity, including on text that
	// references one.
	if got := skills.Substitute("${tenant}", nil); got != "${tenant}" {
		t.Fatalf("with no variables, Substitute = %q", got)
	}
}

func TestVariableRefsFindsWhatSubstitutionWouldReplace(t *testing.T) {
	t.Parallel()
	got := skills.VariableRefs("${b} and ${a} and ${b} and $c and {d}")
	if !slices.Equal(got, []string{"a", "b"}) {
		t.Fatalf("VariableRefs = %v", got)
	}
}

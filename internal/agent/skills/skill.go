// Package skills is the tool-skill subsystem: operator-authored guidance on
// how to use a particular tool or MCP server, sourced from the team
// knowledge base and offered to a phase whose surface it applies to.
package skills

import (
	"fmt"
	"regexp"
	"slices"
	"strings"

	"github.com/crewlet/crewlet/internal/agent/prompts"
	"github.com/crewlet/crewlet/internal/logging"
)

var log = logging.Get("agent.skills")

// # What a tool skill is, and what it is not
//
// It is NOT a memory, a plan or a procedure the agent wrote. It is
// documentation an OPERATOR wrote about a tool the company deployed: how the
// mention markup works on their chat backend, which field their tracker
// calls a story point, what their branch naming convention is. The engine
// cannot know any of it, and a model guessing at it produces work that looks
// right and is wrong in the details.
//
// # The catalogue is a menu, and that is the whole design
//
// Bodies are never inlined. Every phase whose surface matches sees a
// one-line summary and can load the body on demand — because a company with
// twenty MCP servers has twenty skills, and inlining them would spend the
// prompt on documentation for tools this turn will not touch.

const (
	// MaxBodyBytes caps a rendered body.
	//
	// LOOSE, because a body is loaded into the conversation on demand
	// rather than sitting in every prompt prefix — this defends against a
	// runaway page rather than budgeting a prompt. A skill bigger than
	// this almost always wants to be several skills.
	MaxBodyBytes = 32 * 1024

	// MaxSummaryBytes caps the catalogue line.
	//
	// TIGHT, and for the opposite reason: the summary ALWAYS appears, for
	// every matching skill, whether or not the body is ever loaded. A
	// role with twenty servers pays for all twenty on every phase.
	//
	// It bounds the SOURCE bytes, before ${variable} substitution — a
	// summary naming a variable can render slightly longer, so keep the
	// values short.
	MaxSummaryBytes = 240

	// RequiredByDefault is the default for a skill that does not say.
	//
	// TRUE, and that is the safer default: an operator who wrote down how
	// their tracker's fields work did so because getting it wrong
	// matters, and a skill nobody is required to read is one the model
	// skips on the turns it is busiest.
	RequiredByDefault = true
)

// variablePattern matches the braced ${identifier} substitution form ONLY.
//
// Bare $name, a literal $$, single-brace {name} — which is what skill bodies
// use for their own agent-facing placeholders — and non-identifier keys are
// all deliberately unmatched. A skill body is tool documentation dense with
// shell, regex and currency `$`, and a substituter that touched any of those
// would silently corrupt the prose it exists to deliver.
var variablePattern = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)\}`)

// VariableKeyPattern is the allowed shape of a skill_variables key.
//
// The same identifier grammar the pattern above matches, so the key space an
// operator may define and the key space a skill may reference are provably
// identical rather than merely similar.
var VariableKeyPattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// Substitute replaces ${name} references in text from variables.
//
// An UNKNOWN reference is left as its literal ${name} rather than blanked:
// a missing variable then shows up in the prompt as something a reader can
// recognise and grep for, where an empty string reads as authored prose with
// a hole in it. The registry also warns about it at registration, because
// the only place that literal is ever seen is inside a prompt and nobody
// reads those.
func Substitute(text string, variables map[string]string) string {
	if len(variables) == 0 || !strings.Contains(text, "${") {
		// The common path costs one substring check.
		return text
	}
	return variablePattern.ReplaceAllStringFunc(text, func(match string) string {
		name := variablePattern.FindStringSubmatch(match)[1]
		if value, ok := variables[name]; ok {
			return value
		}
		return match
	})
}

// VariableRefs are the ${identifier} names text references.
func VariableRefs(text string) []string {
	var (
		out  []string
		seen = map[string]bool{}
	)
	for _, m := range variablePattern.FindAllStringSubmatch(text, -1) {
		if !seen[m[1]] {
			seen[m[1]] = true
			out = append(out, m[1])
		}
	}
	slices.Sort(out)
	return out
}

// Trigger says which tool surface a skill applies to.
//
// EXACTLY ONE field is set. The presence-by-field shape is what an operator
// authors directly in a page's YAML block:
//
//	trigger:
//	  mcp_server: github
//
//	trigger:
//	  any_of:
//	    - tool: query_episodes
//	    - tool: confluence_search
type Trigger struct {
	MCPServer string    `yaml:"mcp_server,omitempty" json:"mcp_server,omitempty"`
	Tool      string    `yaml:"tool,omitempty" json:"tool,omitempty"`
	AnyOf     []Trigger `yaml:"any_of,omitempty" json:"any_of,omitempty"`
	AllOf     []Trigger `yaml:"all_of,omitempty" json:"all_of,omitempty"`
}

// Validate refuses a trigger that does not name exactly one thing.
//
// An empty trigger would match nothing and a two-field one would have no
// defined meaning; both are authoring mistakes, and both are silent — a
// skill that matches nothing simply never appears, which reads exactly like
// a skill nobody wrote.
func (t Trigger) Validate() error {
	set := 0
	for _, present := range []bool{
		t.MCPServer != "", t.Tool != "", len(t.AnyOf) > 0, len(t.AllOf) > 0,
	} {
		if present {
			set++
		}
	}
	if set != 1 {
		return fmt.Errorf("skills: a trigger must set exactly one of "+
			"mcp_server, tool, any_of or all_of (it sets %d)", set)
	}
	for _, sub := range append(slices.Clone(t.AnyOf), t.AllOf...) {
		if err := sub.Validate(); err != nil {
			return err
		}
	}
	return nil
}

// Matches reports whether this trigger fires for a phase's surface.
func (t Trigger) Matches(surface prompts.Surface) bool {
	switch {
	case t.MCPServer != "":
		return slices.Contains(surface.MCPServers, t.MCPServer)
	case t.Tool != "":
		return slices.Contains(surface.Tools, t.Tool)
	case len(t.AnyOf) > 0:
		return slices.ContainsFunc(t.AnyOf, func(sub Trigger) bool {
			return sub.Matches(surface)
		})
	case len(t.AllOf) > 0:
		return !slices.ContainsFunc(t.AllOf, func(sub Trigger) bool {
			return !sub.Matches(surface)
		})
	}
	return false
}

// Covers reports whether this trigger is ABOUT one specific tool.
//
// A DIFFERENT QUESTION from Matches, and the guard needs both: Matches asks
// "does this skill apply to the current surface", while this asks "is THIS
// tool one of the tools the skill is about". A skill gates only the tools
// its trigger names, never every tool on a surface that happened to
// catalogue it.
//
// A tool leaf covers that exact name; a server leaf covers every tool that
// server publishes. A COMPOSITE UNIONS, for all_of as well as any_of: an
// all_of skill is about the combination of its surfaces, so once the whole
// trigger has matched, each of those surfaces' tools is covered.
func (t Trigger) Covers(tool, server string) bool {
	switch {
	case t.Tool != "":
		return t.Tool == tool
	case t.MCPServer != "":
		// A BUILTIN, whose server is "", is covered by no server trigger:
		// [Trigger.Validate] refuses an empty MCPServer, so reaching this
		// branch means t.MCPServer is non-empty and the comparison
		// answers false for it without a guard saying so.
		return t.MCPServer == server
	}
	for _, sub := range append(slices.Clone(t.AnyOf), t.AllOf...) {
		if sub.Covers(tool, server) {
			return true
		}
	}
	return false
}

// ToolLeaves are the exact tool names this trigger references.
func (t Trigger) ToolLeaves() []string { return leaves(t, func(t Trigger) string { return t.Tool }) }

// ServerLeaves are the MCP server names this trigger references.
func (t Trigger) ServerLeaves() []string {
	return leaves(t, func(t Trigger) string { return t.MCPServer })
}

func leaves(t Trigger, of func(Trigger) string) []string {
	if name := of(t); name != "" {
		return []string{name}
	}
	var out []string
	for _, sub := range append(slices.Clone(t.AnyOf), t.AllOf...) {
		out = append(out, leaves(sub, of)...)
	}
	slices.Sort(out)
	return slices.Compact(out)
}

// Liveness classifies a trigger against a deployment's actual tool surface.
//
// Trigger matching is exact-string and validated nowhere else, so an
// upstream MCP server renaming a tool silently disables both the skill's
// catalogue entry AND — for a required skill — the guard that was enforcing
// it. Nothing raises; the skill just stops existing.
//
// The split separates the two readings of a name that matches nothing. A
// PARTIALLY LIVE skill, whose other leaves do match, is almost certainly
// name drift and worth a warning. A skill whose whole trigger matches
// nothing is plausibly authored for a stack this company does not run, and
// is worth only a note.
type Liveness struct {
	// Dangling are the exact tool names matching no registered tool.
	Dangling []string

	// Live reports that some other part of the trigger still matches.
	Live bool
}

// Classify builds the liveness verdict.
func (t Trigger) Classify(knownTools, knownServers []string) Liveness {
	var out Liveness
	for _, name := range t.ToolLeaves() {
		if !slices.Contains(knownTools, name) {
			out.Dangling = append(out.Dangling, name)
		} else {
			out.Live = true
		}
	}
	if !out.Live {
		out.Live = slices.ContainsFunc(t.ServerLeaves(), func(name string) bool {
			return slices.Contains(knownServers, name)
		})
	}
	return out
}

// Skill is one piece of operator-authored tool guidance.
type Skill struct {
	// Key is what a model passes to load the body, and the sort key that
	// fixes catalogue order.
	Key string

	Title   string
	Summary string
	Body    string

	Trigger Trigger

	// Phases are the phases this skill is offered in. Empty means every
	// phase whose surface matches, which is the ordinary case: a skill
	// about a tool applies wherever that tool can be called.
	Phases []prompts.Phase

	// Required marks a skill the engine ENFORCES: calls to the tools its
	// trigger covers are refused until the model has loaded it this
	// session. See [Guard].
	Required bool

	// SourcePageID and SourcePageVersion are provenance, for logging and
	// for evicting a page that was deleted. They are backend-neutral: a
	// backend with no version concept stamps zero.
	SourcePageID      string
	SourcePageVersion int
}

// Validate refuses a skill that cannot be offered.
func (s Skill) Validate() error {
	switch {
	case strings.TrimSpace(s.Key) == "":
		return fmt.Errorf("skills: a skill needs a key")
	case strings.TrimSpace(s.Summary) == "":
		// The summary is the ONLY thing that always reaches the prompt.
		// A skill without one is invisible in the catalogue and can never
		// be chosen, so registering it would be storing something nothing
		// can reach.
		return fmt.Errorf("skills: skill %q needs a summary", s.Key)
	case len(s.Summary) > MaxSummaryBytes:
		return fmt.Errorf("skills: skill %q has a %d-byte summary (max %d)",
			s.Key, len(s.Summary), MaxSummaryBytes)
	case len(s.Body) > MaxBodyBytes:
		return fmt.Errorf("skills: skill %q has a %d-byte body (max %d)",
			s.Key, len(s.Body), MaxBodyBytes)
	}
	return s.Trigger.Validate()
}

// AppliesTo reports whether this skill is offered in a phase.
func (s Skill) AppliesTo(phase prompts.Phase) bool {
	return len(s.Phases) == 0 || slices.Contains(s.Phases, phase)
}

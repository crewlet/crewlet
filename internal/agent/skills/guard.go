package skills

import (
	"fmt"
	"maps"
	"slices"
	"strings"
	"sync"

	"github.com/crewlet/crewlet/internal/agent/prompts"
)

// Load-before-use, enforced in code rather than in prose.
//
// A skill's body carries what an agent should read BEFORE touching the tools
// its trigger covers — a workflow constraint, the mention markup, a
// delegation convention — and models routinely skip the load and go straight
// for the tool. Asking harder in the prompt does not fix that; refusing the
// call does.
//
// # Session scope, not turn scope
//
// "Loaded" is tracked per LLM SESSION — one phase's message history — because
// that is what determines whether the body is actually in the model's
// context. Plan, Execute and each sub-agent run on separate histories, so a
// body loaded during Plan is genuinely not in front of the executor. A
// round-cap extension continues the same history and the same surface, so
// the loaded set carries across it; a self_iterate starts a fresh session
// and therefore a fresh guard, which is correct rather than annoying.

// ExemptTools are never blocked, whatever a trigger says.
//
// Three kinds, each for a reason that is about DEADLOCK rather than policy:
// the unlock itself (gating it would make the session unrecoverable), the
// discovery meta-tools (the model may need them to reach the guarded tool at
// all, so blocking them adds rounds and protects nothing), and the phase
// submitters (blocking one bricks the phase — the rescue paths force them).
//
// A misauthored trigger can then cost a phase some tools; it can never cost
// the phase.
var ExemptTools = []string{
	LoaderTool,
	"activate_tool",
	"list_mcp_server_tools",
	"submit_work",
	"submit_review",
	"mark_onboarded",
}

// Blocked is a refused call, and what the model must do to proceed.
type Blocked struct {
	Tool string

	// Skill is the key to load, and Title is what it is about — both, so
	// the message names the exact call to make AND says why it is worth
	// making. A bare key reads as an obstacle.
	Skill string
	Title string
}

// Error renders the refusal the model sees.
//
// It NAMES THE EXACT CALL, because the error is the recovery path and a
// model told only "you must load a skill" spends a round discovering which.
// The discovery path is the catalogue, which already carries the marker; by
// the time this fires the model has skipped it.
func (b Blocked) Error() string {
	about := ""
	if b.Title != "" {
		about = " (" + b.Title + ")"
	}
	return fmt.Sprintf("%s is covered by the required tool skill %q%s, which "+
		"has not been loaded in this session. Call load_tool_skill(key=%q) "+
		"first, then retry %s.", b.Tool, b.Skill, about, b.Skill, b.Tool)
}

// Guard is one session's load-before-use state.
type Guard struct {
	registry *Registry
	phase    prompts.Phase
	surface  prompts.Surface

	mu     sync.Mutex
	loaded map[string]bool
}

// NewGuard arms the guard for one phase session, or returns nil.
//
// NIL WHEN THE SESSION CANNOT RECOVER: with no loader on the surface there
// is no way to satisfy the guard, so arming it would refuse tools the model
// has no path to unlock. Nil is also the answer for a phase with no required
// skills matching, which is the overwhelmingly common case and costs nothing
// on every call.
//
// The surface it is given is the SAME one the catalogue was built from, so
// what is enforced and what the model was shown cannot disagree.
func NewGuard(r *Registry, phase prompts.Phase, surface prompts.Surface) *Guard {
	if r == nil || !slices.Contains(surface.Tools, LoaderTool) {
		return nil
	}
	// REVIEW IS EXEMPT ENTIRELY: it has no domain tools and no loader, so
	// there is nothing to gate and a marker there would point at a tool
	// the reviewer does not have. The prompt package makes the same
	// exception for the catalogue's marker, from the same reasoning.
	if phase == prompts.PhaseReview {
		return nil
	}
	required := false
	for _, s := range r.Matching(phase, surface) {
		if s.Required {
			required = true
			break
		}
	}
	if !required {
		return nil
	}
	return &Guard{registry: r, phase: phase, surface: surface, loaded: map[string]bool{}}
}

// Restore re-arms a guard with the keys a previous slice of the SAME session
// already loaded.
//
// The one legitimate way to hand a guard somebody else's loads, and it exists
// for exactly one caller: an executor that suspended on a detached coding run
// is re-entered as the same message history days later, so the bodies it
// loaded before the suspend are still in front of the model. A guard rebuilt
// empty would block the very tools that session already unlocked, and the
// model would be told to load a skill it can see in its own transcript.
func (g *Guard) Restore(keys []string) {
	if g == nil {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	for _, key := range keys {
		if key != "" {
			g.loaded[key] = true
		}
	}
}

// LoadedKeys is what this session has loaded, sorted.
//
// Sorted because it is persisted into a suspended conversation's state, and a
// blob whose key order came from map iteration would differ on every write of
// the same fact.
func (g *Guard) LoadedKeys() []string {
	if g == nil {
		return nil
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	return slices.Sorted(maps.Keys(g.loaded))
}

// Loaded records a successful load, unlocking the tools that skill covers.
func (g *Guard) Loaded(key string) {
	if g == nil || key == "" {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	g.loaded[key] = true
}

// LoaderTool is the tool whose successful call unlocks a skill.
const LoaderTool = "load_tool_skill"

// Observe implements [tools.Guard]: it watches for the unlock.
//
// Reading the key back off the ARGUMENTS rather than the result, because the
// result is prose for the model and parsing a key out of it would be
// guessing at a format nothing promises. The arguments are what the model
// asked for and what the tool resolved.
func (g *Guard) Observe(tool string, args map[string]any) {
	if g == nil || tool != LoaderTool {
		return
	}
	key, _ := args["key"].(string)
	g.Loaded(strings.TrimSpace(key))
}

// Check implements [tools.Guard]: it reports why a call must be refused, or
// "" to let it through.
//
// The SERVER is passed alongside the tool because a trigger's server leaf
// covers every tool that server publishes, and a tool name alone cannot say
// which server published it.
//
// A STRING rather than an error value, because the tool layer hands the
// answer straight to the model and an error's text is not something the tool
// layer should have to know how to render.
func (g *Guard) Check(tool, server string) string {
	if blocked := g.Blocking(tool, server); blocked != nil {
		return blocked.Error()
	}
	return ""
}

// Blocking is Check with the structure kept, for the caller that publishes
// telemetry about what was refused and needs the skill key rather than the
// sentence.
func (g *Guard) Blocking(tool, server string) *Blocked {
	if g == nil || slices.Contains(ExemptTools, tool) {
		return nil
	}
	loaded := g.loadedSet()

	for _, s := range g.registry.Matching(g.phase, g.surface) {
		if !s.Required || loaded[s.Key] || !s.Trigger.Covers(tool, server) {
			continue
		}
		return &Blocked{Tool: tool, Skill: s.Key, Title: s.Title}
	}
	return nil
}

// Pending are the required skills this session has not yet loaded, sorted.
//
// For telemetry and for a phase record: an operator looking at a turn that
// stalled on a guard needs to see which skill it was waiting for, and the
// blocked call alone names only the first one.
func (g *Guard) Pending() []string {
	if g == nil {
		return nil
	}
	loaded := g.loadedSet()

	var out []string
	for _, s := range g.registry.Matching(g.phase, g.surface) {
		if s.Required && !loaded[s.Key] {
			out = append(out, s.Key)
		}
	}
	slices.Sort(out)
	return out
}

// Render substitutes a skill's variables for a body about to be loaded.
func (g *Guard) Render(text string) string {
	if g == nil {
		return text
	}
	return g.registry.Render(text)
}

// loadedSet copies the loaded keys out from under the lock.
//
// COPIED rather than held, so the registry walks in Check and Pending run
// outside this lock: those walks take the REGISTRY's lock, and a guard
// holding its own while waiting on another is the shape a deadlock is made
// of.
func (g *Guard) loadedSet() map[string]bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return maps.Clone(g.loaded)
}

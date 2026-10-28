package builtin

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/crewlet/crewlet/internal/agent/skills"
	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/tools"
)

// LoadToolSkillTool is the tool's wire name.
const LoadToolSkillTool = "load_tool_skill"

// ToolSkills is what load_tool_skill needs from the skill registry.
//
// Consumer-defined and one method wide. The registry is a live,
// webhook-updated store sourced from the team knowledge base; none of that
// reaches this package, which knows only that a key resolves to prose and the
// page it was read from.
type ToolSkills interface {
	// Load returns the rendered skill and its page, reporting whether the
	// key exists. One call, so the page a load is recorded against is the
	// page whose body the model was handed — a live registry can be edited
	// between two.
	Load(key string) (skills.Loaded, bool)

	// Keys are every key Load serves, sorted, which a load of any other
	// key answers with.
	Keys() []string
}

// maxSuggestedSkills bounds the keys a miss lists: a company's catalogue is a
// few dozen skills, and a miss answers in a line or two.
const maxSuggestedSkills = 30

// loadToolSkill fetches the body behind a catalogue entry.
//
// THE CATALOGUE IS A MENU and this is how a dish is ordered. Every phase
// whose surface matches sees a one-line summary per skill; the body arrives
// only when the model decides it needs it, because a company with twenty MCP
// servers would otherwise spend its prompt on documentation for tools this
// turn will not touch.
//
// It is also the UNLOCK for a required skill: the guard refuses calls to the
// tools a required skill covers until this has run for it in the current
// session. Which is why this tool is never itself gated — see
// skills.ExemptTools.
type loadToolSkill struct {
	skills ToolSkills
	events Telemetry
}

var _ tools.SeatCallable = (*loadToolSkill)(nil)

func (t *loadToolSkill) Name() string { return LoadToolSkillTool }

func (t *loadToolSkill) Description() string {
	return "Read the full guidance behind one entry in your tool-skills " +
		"catalogue: workflow examples, conventions, and the details of how " +
		"this company uses that tool. Load a skill BEFORE using the tools it " +
		"covers — entries marked required are enforced, and calls to their " +
		"tools are refused until you have."
}

func (t *loadToolSkill) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"key": map[string]any{
				"type":        "string",
				"description": "The skill key, exactly as the catalogue lists it",
			},
		},
		"required": []string{"key"},
	}
}

func (t *loadToolSkill) Call(ctx context.Context, args map[string]any) (tools.Result, error) {
	return t.CallForTurn(ctx, nil, args)
}

// CallForTurn is Call plus the seat, so a load can be counted.
//
// SEAT-AWARE ONLY FOR THE TELEMETRY. The body it returns is the company's,
// identical for every seat — unlike use_skill, which resolves against the
// caller's own synthesized skills and would hand one agent another's
// procedure if it did not know who was asking.
func (t *loadToolSkill) CallForTurn(ctx context.Context, turn *turnctx.Turn, args map[string]any) (tools.Result, error) {
	key := argString(args, "key")
	if key == "" {
		return failed("load_tool_skill needs a `key` — one of the keys your " +
			"tool-skills catalogue lists."), nil
	}
	loaded, ok := t.skills.Load(key)
	if !ok {
		// NAMES THE MISTAKE rather than reporting an empty skill: a model
		// that mistyped a key and got back "" would read it as a skill
		// with nothing in it and proceed, which is exactly the state the
		// required-skill guard exists to prevent.
		//
		// AND NAMES THE KEYS THAT EXIST: a model told only that its guess
		// was wrong guesses again (GP-38 tried "artifacts", then
		// "artifact", for the key "skill:artifacts").
		return refused(tools.RefusalNotFound, noSuchSkill(key, t.skills.Keys())), nil
	}
	// The REGISTRY kind, which is the whole reason SkillSourceKind has two
	// values: a company-published tool skill being loaded and a seat
	// reusing one it synthesized answer different questions, and a feed
	// that could not tell them apart answers neither.
	page := types.KnowledgeReadPage{
		ID: loaded.PageID, Container: loaded.Container, Title: loaded.Title,
	}
	note(ctx, t.events, turn, skillUsed(turn, key, "", types.SkillSourceRegistry, page))
	// AND A READ, because loading a skill's body is reading the page it was
	// written on: a knowledge base's "who opens this" must count the pages
	// an author maintains as skills, or the most-read pages in the company
	// are the ones it reports as unread.
	note(ctx, t.events, turn, knowledgeRead(turn, types.ReadViaSkillLoaded,
		loaded.Backend, "", []types.KnowledgeReadPage{page}))
	return tools.Result{Output: loaded.Body}, nil
}

// noSuchSkill is a miss's answer: the key asked for, the keys that hold it
// first ("did you mean"), then every key, at most [maxSuggestedSkills].
func noSuchSkill(asked string, keys []string) string {
	if len(keys) == 0 {
		return fmt.Sprintf("No tool skill %q: this company publishes no tool skills, so there is "+
			"nothing to load. Carry on without one.", asked)
	}
	want := strings.ToLower(strings.TrimSpace(asked))
	var near []string
	for _, k := range keys {
		lower := strings.ToLower(k)
		if want != "" && (strings.Contains(lower, want) || strings.Contains(want, lower)) {
			near = append(near, strconv.Quote(k))
		}
	}
	var b strings.Builder
	fmt.Fprintf(&b, "No tool skill %q. Use a key exactly as your catalogue lists it.", asked)
	if len(near) > 0 {
		fmt.Fprintf(&b, " Did you mean %s?", strings.Join(near, " or "))
	}
	shown := keys[:min(len(keys), maxSuggestedSkills)]
	fmt.Fprintf(&b, " The keys are: %s", strings.Join(shown, ", "))
	if rest := len(keys) - len(shown); rest > 0 {
		fmt.Fprintf(&b, ", and %d more", rest)
	}
	b.WriteString(".")
	return b.String()
}

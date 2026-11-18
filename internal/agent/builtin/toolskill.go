package builtin

import (
	"context"
	"fmt"
	"strings"

	"github.com/crewlet/crewlet/internal/agent/skills"
	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/toolname"
	"github.com/crewlet/crewlet/internal/tools"
	"github.com/crewlet/crewlet/internal/words"
)

// LoadToolSkillTool is the tool's wire name.
const LoadToolSkillTool = toolname.LoadToolSkill

// ToolSkills is what load_tool_skill needs from the skill registry: a key
// resolves to prose and the page it was read from. The live, webhook-updated
// registry behind it stays out of this package.
type ToolSkills interface {
	// Load returns the rendered skill and its page, reporting whether the
	// key exists. One call, so the recorded page is the one whose body the
	// model got: a live registry can be edited between two.
	Load(key string) (skills.Loaded, bool)

	// Keys are every key Load serves, sorted, which a load of any other
	// key answers with.
	Keys() []string
}

// maxSuggestedSkills bounds the keys a miss lists: a company's catalogue is a
// few dozen skills, and a miss answers in a line or two.
const maxSuggestedSkills = 30

// loadToolSkill fetches the body behind a catalogue entry. The prompt carries
// only a one-line summary per skill, so a company with many MCP servers does
// not spend it on documentation for tools the turn will not touch.
//
// It also UNLOCKS a required skill: the guard refuses the tools it covers
// until this has run for it in the session, so this tool is never itself
// gated (skills.ExemptTools).
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

// CallForTurn is Call plus the seat, used only to count the load: the body is
// the company's, identical for every seat (unlike use_skill's, which resolves
// against the caller's own synthesized skills).
func (t *loadToolSkill) CallForTurn(ctx context.Context, turn *turnctx.Turn, args map[string]any) (tools.Result, error) {
	key := argString(args, "key")
	if key == "" {
		return failed(LoadToolSkillTool + " needs a `key` — one of the keys your " +
			"tool-skills catalogue lists."), nil
	}
	loaded, ok := t.skills.Load(key)
	if !ok {
		// A refusal, not an empty skill a model would read as loaded and
		// proceed past the required-skill guard; and it lists the keys,
		// since a model told only that its guess was wrong guesses again
		// (GP-38).
		return refused(tools.RefusalNotFound, noSuchSkill(key, t.skills.Keys())), nil
	}
	// The REGISTRY kind: a company-published skill loaded and a seat
	// reusing one it synthesized answer different questions.
	page := types.KnowledgeReadPage{
		ID: loaded.PageID, Container: loaded.Container, Title: loaded.Title,
	}
	note(ctx, t.events, turn, skillUsed(turn, key, "", types.SkillSourceRegistry, page))
	// Also a read of the page the skill was written on, or the knowledge
	// base reports its most-read pages as unread.
	note(ctx, t.events, turn, knowledgeRead(turn, types.ReadViaSkillLoaded,
		loaded.Backend, "", []types.KnowledgeReadPage{page}))
	return tools.Result{Output: loaded.Body}, nil
}

// noSuchSkill is a miss's answer: the key asked for, the keys that hold it
// first ("did you mean"), then every key, at most [maxSuggestedSkills], each
// quoted as the key asked for is ([words.AndMore]).
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
			near = append(near, k)
		}
	}
	var b strings.Builder
	fmt.Fprintf(&b, "No tool skill %q. Use a key exactly as your catalogue lists it.", asked)
	if len(near) > 0 {
		fmt.Fprintf(&b, " Did you mean %s?", words.Or(near))
	}
	fmt.Fprintf(&b, " The keys are: %s.", words.AndMore(keys, maxSuggestedSkills))
	return b.String()
}

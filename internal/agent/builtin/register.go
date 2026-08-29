package builtin

import (
	"fmt"

	"github.com/crewlet/crewlet/internal/mcp"
	"github.com/crewlet/crewlet/internal/tools"
)

// Deps are the node-level things the builtins act through.
//
// NODE-LEVEL, not turn-level, and the split is the whole reason these tools
// can be registered once per epoch: a store or a service is the same for every
// seat, while the SEAT is what varies per call and arrives on the turn (see
// tools.SeatCallable). Getting that backwards would mean one registration per
// seat, and a planner's catalogue listing every builtin once per colleague.
//
// EVERY FIELD IS OPTIONAL. A node without a store, a company without agent-to-
// agent messaging, a `validate` run with neither — each is a real deployment,
// and each should get the tools it can actually serve rather than a boot
// failure or a tool that answers every call with a nil dereference.
type Deps struct {
	// A2A opens agent-to-agent channels. Nil omits a2a_ask.
	A2A Asker

	// Skills, Episodes, Diary and Onboarding are the learning stores. Each
	// nil omits the tools that need it.
	Skills     SkillStore
	Refinable  RefinableSkills
	Episodes   EpisodeStore
	Diary      DiaryStore
	Onboarding OnboardingStore

	// ToolSkills is the operator-authored tool guidance. Nil omits
	// load_tool_skill, which is what a company that has published none
	// has — and omitting it also disarms the required-skill guard, since
	// a guard with no unlock would refuse tools the model cannot free.
	ToolSkills ToolSkills

	// EpisodeLimit is how many turns query_episodes returns when the model
	// names no limit: the company's learning.episodic.retrieval_limit.
	// Zero takes [DefaultEpisodeLimit].
	//
	// It reaches a builtin at all because the setting was otherwise inert —
	// validated, schema'd, documented, and read by nothing, so
	// `retrieval_limit: 20` produced a new revision and changed nothing an
	// operator could observe.
	EpisodeLimit int

	// SkillBodyMax caps a refined skill's body:
	// learning.skill_refinement.max_body_chars. Zero takes
	// [DefaultSkillBodyMax].
	SkillBodyMax int

	// SkillVersionsKept caps a skill's archived history:
	// learning.skill_refinement.max_versions_kept. Zero lets the store
	// apply its own default.
	SkillVersionsKept int

	// Events is where the skill lifecycle's own telemetry goes. Nil
	// publishes nothing, which is what a registry built outside an engine
	// has — and what the shipped binary had until this existed, so
	// skill_used, skill_refined and skill_promoted were registered types
	// with topics and no publisher.
	Events Telemetry

	// Sandbox launches a detached coding run. Nil omits run_sandbox, which
	// is what a build with no providers.sandbox has — and omitting it is
	// the point: a seat offered a code tool that cannot start a box would
	// plan around one and fail at the call.
	Sandbox SandboxLauncher
}

// Register adds every builtin the given dependencies can support.
//
// It reports the names registered, so a caller can log what a seat will
// actually be able to reach — the difference between "the model never called
// query_episodes" and "query_episodes was not there" is otherwise invisible
// from the outside, and they send a reader to opposite places.
//
// A tool whose dependency is absent is OMITTED, never registered-and-broken.
// A model shown a tool that always fails learns to distrust the whole
// catalogue, and burns a round finding out each time.
func Register(reg *tools.Registry, deps Deps) ([]string, error) {
	if reg == nil {
		return nil, fmt.Errorf("builtin: no registry")
	}

	// lookup_colleague is unconditional: its corpus is the turn's own org,
	// so it works on any node that is running a company at all — and it is
	// the tool every other addressing decision goes through.
	candidates := []struct {
		tool tools.Callable
		on   bool
	}{
		{&lookupColleague{}, true},
		{&a2aAsk{svc: deps.A2A}, deps.A2A != nil},
		{&useSkill{skills: deps.Skills, events: deps.Events}, deps.Skills != nil},
		{&refineSkill{
			skills:   deps.Refinable,
			events:   deps.Events,
			bodyMax:  orDefault(deps.SkillBodyMax, DefaultSkillBodyMax),
			versions: deps.SkillVersionsKept,
		}, deps.Refinable != nil},
		{&queryEpisodes{
			episodes: deps.Episodes,
			limit:    orDefault(deps.EpisodeLimit, DefaultEpisodeLimit),
		}, deps.Episodes != nil},
		{&refreshMemory{diary: deps.Diary}, deps.Diary != nil},
		{&reflectAndPersist{diary: deps.Diary}, deps.Diary != nil},
		{&markOnboarded{onboarding: deps.Onboarding}, deps.Onboarding != nil},
		{&runSandbox{launcher: deps.Sandbox}, deps.Sandbox != nil},
		{&loadToolSkill{skills: deps.ToolSkills, events: deps.Events}, deps.ToolSkills != nil},
	}

	var names []string
	for _, c := range candidates {
		if !c.on {
			continue
		}
		if err := reg.RegisterWith(c.tool, tools.OriginBuiltin, annotationsFor(c.tool.Name())); err != nil {
			return names, fmt.Errorf("builtin: %w", err)
		}
		names = append(names, c.tool.Name())
	}
	log.Info("builtin_tools_registered", "count", len(names), "tools", names)
	return names, nil
}

// annotationsFor classifies a builtin for the delivery gate.
//
// THE GATE IS WHY THIS MATTERS. A turn that planned to act and then only read
// is a turn that delivered nothing, and the gate can only see that if it knows
// which calls were reads. Unannotated counts as NOT a known read — the safe
// default for an MCP server nobody has classified — so a read-only builtin
// left unannotated would make every recall look like a delivery.
//
// None of these writes to a SHARED surface: an agent's own diary, its own
// skills and its own onboarding marker are private state, and an A2A ask is a
// message to one colleague rather than something the company can see. The
// distinction is the one `writes_to_shared_surface` exists to draw — see
// docs/concepts/tool-capabilities.md.
func annotationsFor(name string) tools.Annotations {
	switch name {
	case LookupColleagueTool, UseSkillTool, QueryEpisodesTool, RefreshMemoryTool,
		LoadToolSkillTool:
		// Reads, and idempotent: asking twice costs a round and changes
		// nothing, which is what lets a phase retry one safely.
		return tools.Annotations{ReadOnly: mcp.Yes, Idempotent: mcp.Yes}
	case A2AAskTool:
		// The only one that leaves this process. Not destructive — an ask
		// is a message, not an edit — but NOT idempotent: asking twice
		// wakes a colleague twice and spends two of their turns.
		return tools.Annotations{ReadOnly: mcp.No, Destructive: mcp.No, OpenWorld: mcp.Yes}
	case MarkOnboardedTool:
		// A write whose repeat is genuinely free: the marker is a fact
		// about this seat, and setting it again sets the same fact.
		return tools.Annotations{ReadOnly: mcp.No, Destructive: mcp.No, Idempotent: mcp.Yes}
	case RunSandboxTool:
		// The one builtin that writes to a SHARED SURFACE, which
		// ReadOnly=No plus OpenWorld=Yes is how that is stated: a coding
		// run pushes branches and opens pull requests other people see, so
		// mcp.WritesToSharedSurface reads true and the sub-agent guard
		// keeps it away from a sub-agent acting under its parent's name.
		// Not destructive — a branch and a pull request are additive — and
		// nowhere near idempotent: a second call is a second run, a second
		// box, and a second set of commits.
		return tools.Annotations{ReadOnly: mcp.No, Destructive: mcp.No, OpenWorld: mcp.Yes}
	case RefineSkillTool:
		// It replaces a body. The prior version is archived, so this is
		// reversible — which is exactly what Destructive asks about.
		return tools.Annotations{ReadOnly: mcp.No, Destructive: mcp.No}
	default:
		// reflect_and_persist: a write, and each call is another note.
		return tools.Annotations{ReadOnly: mcp.No, Destructive: mcp.No}
	}
}

// The shipped values for the knobs a caller may leave at zero.
//
// They MATCH config.DefaultLearning, and they exist separately because this
// package does not import internal/config: a builtin registry built directly
// — a test, an embedder — must land on the same numbers a parsed company
// does, and a zero that meant "no episodes" or "no body at all" would be a
// tool that refuses everything.
const (
	// DefaultEpisodeLimit is learning.episodic.retrieval_limit's default.
	DefaultEpisodeLimit = 5

	// DefaultSkillBodyMax is learning.skill_refinement.max_body_chars's
	// default: the ceiling on a refined skill's whole body.
	DefaultSkillBodyMax = 20000
)

func orDefault(value, fallback int) int {
	if value <= 0 {
		return fallback
	}
	return value
}

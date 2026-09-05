package builtin

import (
	"fmt"
	"slices"

	"github.com/crewlet/crewlet/internal/mcp"
	"github.com/crewlet/crewlet/internal/tools"
)

// Deps are the node-level things the builtins act through.
//
// NODE-LEVEL, not turn-level, and the split is the whole reason these tools
// can be registered once per epoch: a store or a service is the same for every
// seat, while the SEAT is what varies per call and arrives on the turn (see
// tools.SeatCallable). Getting that backwards would mean one registration per
// seat, and an agent's catalogue listing every builtin once per colleague.
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

	// Recall is the turn-start prefetch's semantic search, re-run on demand. Nil
	// leaves query_episodes on recency and conversation and refresh_memory
	// on recency — which is what a company with no embeddings has, and what
	// EVERY company had while the tools declared no way to search at all.
	Recall Recaller

	// RefreshesPerTurn is how many DISTINCT context hints one turn may
	// re-filter its notes on: learning.personal_memory.max_refreshes_per_turn.
	// Zero takes [DefaultRefreshesPerTurn].
	RefreshesPerTurn int

	// Events is where the skill lifecycle's own telemetry goes. Nil
	// publishes nothing, which is what a registry built outside an engine
	// has — and what the shipped binary had until this existed, so
	// skill_used, skill_refined and skill_promoted were registered types
	// with topics and no publisher.
	Events Telemetry

	// Sandbox launches a detached coding run. Nil omits run_sandbox, which
	// is what a build with no providers.sandbox has — and omitting it is
	// the point: a seat offered a code tool that cannot start a box would
	// reach for one and fail at the call.
	Sandbox SandboxLauncher

	// Knowledge searches the team knowledge base on demand. Nil omits
	// search_knowledge, which is what a company with no knowledge backend
	// has — and it is the same seam the turn-start prefetch reads, so the
	// two can never disagree about scope, exclusions or credentials.
	Knowledge KnowledgeSearcher

	// Work is the native tracker. Both halves nil omits all five tools,
	// which is what a company running Jira has — and omitting them is the
	// point: a seat offered a tracker tool against a tracker this company
	// does not run would reach for it and fail at the call.
	Work WorkDeps
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
			recall:   deps.Recall,
			limit:    orDefault(deps.EpisodeLimit, DefaultEpisodeLimit),
		}, deps.Episodes != nil},
		{&refreshMemory{
			diary:    deps.Diary,
			recall:   deps.Recall,
			maxHints: deps.RefreshesPerTurn,
		}, deps.Diary != nil},
		{&reflectAndPersist{diary: deps.Diary}, deps.Diary != nil},
		{&markOnboarded{onboarding: deps.Onboarding}, deps.Onboarding != nil},
		{&runSandbox{launcher: deps.Sandbox}, deps.Sandbox != nil},
		{&loadToolSkill{skills: deps.ToolSkills, events: deps.Events}, deps.ToolSkills != nil},
		{&searchKnowledge{search: deps.Knowledge}, deps.Knowledge != nil},
		{&listWorkItems{deps: deps.Work}, deps.Work.Reader != nil},
		{&getWorkItem{deps: deps.Work}, deps.Work.Reader != nil},
		{&createWorkItem{deps: deps.Work}, deps.Work.Writer != nil},
		{&updateWorkItem{deps: deps.Work}, deps.Work.Writer != nil && deps.Work.Reader != nil},
		{&commentOnWorkItem{deps: deps.Work}, deps.Work.Writer != nil && deps.Work.Reader != nil},
	}

	var names []string
	for _, c := range candidates {
		if !c.on {
			continue
		}
		opts := []tools.Option{}
		if slices.Contains(WorkWrites(), c.tool.Name()) {
			// A DELIVERY. A turn woken by an assignment answers by moving
			// the item, commenting on it, or filing the follow-up — and
			// without this the gate sees only builtins, concludes the
			// turn reached nobody, and corrects it into another round.
			opts = append(opts, tools.Delivers())
		}
		if err := reg.RegisterWith(c.tool, tools.OriginBuiltin,
			annotationsFor(c.tool.Name()), opts...); err != nil {
			return names, fmt.Errorf("builtin: %w", err)
		}
		names = append(names, c.tool.Name())
	}
	log.Info("builtin_tools_registered", "count", len(names), "tools", names)
	return names, nil
}

// annotationsFor classifies a builtin for the delivery gate.
//
// THE DELIVERY CHECK IS WHY THIS MATTERS. A turn that set out to act and then
// only read is a turn that delivered nothing, and the check can only see that
// if it knows which calls were reads. Unannotated counts as NOT a known read — the safe
// default for an MCP server nobody has classified — so a read-only builtin
// left unannotated would make every recall look like a delivery.
//
// THE PRIVATE WRITES SAY SO, and the reason is that saying nothing meant the
// opposite. An agent's own diary, its own skills and its own onboarding marker
// are closed-world local state — nothing outside this engine sees them — but
// they were annotated ReadOnly=No with OpenWorld left UNKNOWN, and
// [mcp.WritesToSharedSurface] reads `ReadOnly == No && OpenWorld != No` as a
// shared write. So the comment that used to stand here ("none of these writes
// to a shared surface") described an intent the arithmetic contradicted: the
// classifier called all three shared, and the worker guard denied them for a
// reason nobody had chosen.
//
// They are now annotated OpenWorld=No, which is what they are, and denied to
// workers BY NAME on the engine-control denylist — the criterion this tree
// gives for its own tools, where naming is not a tool-stack coupling. A worker
// running under its parent's name must not write the parent's diary, refine
// the parent's skills or stamp the parent's onboarding marker, and that is a
// runtime invariant rather than a property of the surface it writes to.
func annotationsFor(name string) tools.Annotations {
	switch name {
	case LookupColleagueTool, UseSkillTool, QueryEpisodesTool, RefreshMemoryTool,
		LoadToolSkillTool, SearchKnowledgeTool:
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
		// CLOSED WORLD: the marker is a row in this node's own store.
		return tools.Annotations{
			ReadOnly: mcp.No, Destructive: mcp.No, Idempotent: mcp.Yes, OpenWorld: mcp.No,
		}
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
	case ListWorkItemsTool, GetWorkItemTool:
		// Reads, and idempotent: asking twice costs a round and changes
		// nothing.
		return tools.Annotations{ReadOnly: mcp.Yes, Idempotent: mcp.Yes}
	case CreateWorkItemTool:
		// A write everybody in the company sees, so OpenWorld is Yes and
		// [mcp.WritesToSharedSurface] reads true — which keeps it away
		// from a sub-agent acting under its parent's name. Not
		// destructive (an item is additive) and NOT idempotent: a second
		// call is a second item, which is the duplicate the tool's own
		// description tells the model to search for first.
		return tools.Annotations{ReadOnly: mcp.No, Destructive: mcp.No, OpenWorld: mcp.Yes}
	case CommentOnWorkTool:
		// Shared, additive, and idempotent IN PRACTICE because a comment
		// made from a turn takes a deterministic id — but declared NOT
		// idempotent, because the annotation describes the tool a model
		// is offered and a model calling it twice with different text
		// says two things. The idempotence is the engine protecting a
		// re-run turn, not a licence to repeat.
		return tools.Annotations{ReadOnly: mcp.No, Destructive: mcp.No, OpenWorld: mcp.Yes}
	case UpdateWorkItemTool:
		// Shared, and DESTRUCTIVE: it replaces a title, a description or
		// an assignee, and the previous value survives only in the change
		// record. That is what the flag asks — whether a call can undo
		// somebody's work — and a status flip riding the same tool does
		// not make the whole tool safe.
		return tools.Annotations{ReadOnly: mcp.No, Destructive: mcp.Yes, OpenWorld: mcp.Yes}
	case RefineSkillTool:
		// It replaces a body. The prior version is archived, so this is
		// reversible — which is exactly what Destructive asks about — and
		// the skill is this seat's own, so the world it writes to is closed.
		return tools.Annotations{ReadOnly: mcp.No, Destructive: mcp.No, OpenWorld: mcp.No}
	default:
		// reflect_and_persist: a write, and each call is another note, into
		// this agent's private diary and nowhere else.
		return tools.Annotations{ReadOnly: mcp.No, Destructive: mcp.No, OpenWorld: mcp.No}
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

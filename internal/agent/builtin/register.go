package builtin

import (
	"fmt"
	"slices"

	"github.com/crewlet/crewlet/internal/mcp"
	"github.com/crewlet/crewlet/internal/tools"
	"github.com/crewlet/crewlet/internal/tracker"
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

	// Pages is the native knowledge base. Both halves nil omits all five
	// tools, which is what a company running Confluence has.
	Pages PageDeps

	// Work is the native tracker. Both halves nil omits all five tools,
	// which is what a company running Jira has — and omitting them is the
	// point: a seat offered a tracker tool against a tracker this company
	// does not run would reach for it and fail at the call.
	Work WorkDeps

	// LeadsProject answers whether a seat leads the unit that owns a
	// project, which is the authority over that project's own settings.
	// Nil REFUSES rather than degrading — see [LeadsProject] — so a build
	// that wired no chart lookup leaves write_project able to declare a
	// tag and nothing else, which is the safe direction.
	LeadsProject LeadsProject
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
		{&updateWorkItem{deps: deps.Work, leads: deps.LeadsProject},
			deps.Work.Writer != nil && deps.Work.Reader != nil},
		{&commentOnWorkItem{deps: deps.Work}, deps.Work.Writer != nil && deps.Work.Reader != nil},
		// AND THE FOLD, which is a seat's for the reason the trash is
		// not: it leaves the item where it was, cancelled and linked, so
		// there is no absence for anybody to miss — and a seat can
		// already cancel and link by hand, which is the same gesture
		// without the children. See workmerge.go.
		{&mergeWorkItem{deps: deps.Work}, deps.Work.Merges != nil && deps.Work.Reader != nil},
		// READING THE CATALOGUE IS A SEAT'S, writing it is not: a create
		// refuses a type the company has not declared, and a model that
		// cannot read the catalogue can only guess at one.
		{&getWorkCatalogue{deps: deps.Work}, deps.Work.Reader != nil},
		// AND SO IS READING THE PROJECT, for the same reason and one more:
		// a create refuses a project the company does not have and a
		// required field left empty, and both refusals are one round a
		// model could have skipped by looking the container up first.
		{&listProjects{deps: deps.Work}, projectReads(deps.Work)},
		{&describeProject{deps: deps.Work}, projectReads(deps.Work)},
		{&sprintReport{deps: deps.Work}, projectReads(deps.Work)},
		// AND THE ONE PROJECT WRITE a seat holds, for one facet: a
		// create refuses a label the project has not declared, so a
		// seat without this could never use `labels` at all.
		{&writeProject{deps: deps.Work, leads: deps.LeadsProject},
			deps.Work.ProjectWriter != nil},
		// AND THE GOAL READ, which is a seat's because `goal_updated`
		// wakes every owner and every member: a seat told its goal moved
		// and holding no verb to read one has been handed a riddle.
		{&listWorkGoals{deps: deps.Work}, deps.Work.Reader != nil},
		// AND THE TWO ABOUT CHANGE rather than about state: a board says
		// what is there now, and no filter over its rows can answer
		// "who moved this" or "what is waiting on me".
		{&taskActivity{deps: deps.Work}, feedReads(deps.Work)},
		{&myWork{deps: deps.Work}, feedReads(deps.Work)},
		{&listPages{deps: deps.Pages}, deps.Pages.Reader != nil},
		{&getPage{deps: deps.Pages}, deps.Pages.Reader != nil},
		{&writePage{deps: deps.Pages}, deps.Pages.Writer != nil},
		{&savePage{deps: deps.Pages}, deps.Pages.Writer != nil && deps.Pages.Reader != nil},
		{&commentOnPage{deps: deps.Pages}, deps.Pages.Writer != nil && deps.Pages.Reader != nil},
	}

	var names []string
	for _, c := range candidates {
		if !c.on {
			continue
		}
		opts := []tools.Option{}
		if slices.Contains(WorkWrites(), c.tool.Name()) ||
			slices.Contains(PageWrites(), c.tool.Name()) {
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

// annotationsFor classifies a builtin for the delivery gate and the sub-agent
// guard.
//
// THE DELIVERY CHECK IS WHY ReadOnly MATTERS. A turn that set out to act and
// then only read is a turn that delivered nothing, and the check can only see
// that if it knows which calls were reads. Unannotated counts as NOT a known
// read — the safe default for an MCP server nobody has classified — so a
// read-only builtin left unannotated would make every recall look like a
// delivery.
//
// OPEN-WORLD IS A TRI-STATE, AND THE THIRD VALUE IS LOAD-BEARING HERE. The
// sub-agent guard asks [mcp.WritesToSharedSurface], whose rule is `ReadOnly ==
// No` AND `OpenWorld != No` — so leaving OpenWorld UNSET on a tool that writes
// only the agent's own private state classifies it as a write to a surface a
// human reads. That is the exact shape internal/mcp/probe.go reaches past the
// SDK to avoid producing by accident for a third-party server ("the sub-agent
// guard would deny every under-annotated tool in the company, having been told
// nothing at all"), and these three produced it by hand: a diary note, a
// refined skill and an onboarding marker were all denied to a worker the
// parent had explicitly granted them to, on the strength of a hint nobody had
// set. Each says `OpenWorld: mcp.No` now, because each is genuinely false —
// the question the classifier asks is whether a sub-agent would write "to a
// surface a human reads, under the parent agent's identity", and an agent's
// own memory is read by its own next turn and by nobody else.
//
// The DEFAULT arm stays conservative and is meant to have no members: a
// builtin nobody classified is treated as a shared write, so a future tool
// that posts somewhere is denied to workers until someone says otherwise.
// See docs/concepts/tool-capabilities.md.
func annotationsFor(name string) tools.Annotations {
	switch name {
	case LookupColleagueTool, UseSkillTool, QueryEpisodesTool, RefreshMemoryTool,
		LoadToolSkillTool, SearchKnowledgeTool:
		// Reads, and idempotent: asking twice costs a round and changes
		// nothing, which is what lets a phase retry one safely.
		return tools.Annotations{ReadOnly: mcp.Yes, Idempotent: mcp.Yes}
	case A2AAskTool:
		// One of the two that leave this process. Not destructive — an ask
		// is a message, not an edit — but NOT idempotent: asking twice
		// wakes a colleague twice and spends two of their turns. A worker
		// is additionally denied it BY NAME, because "one colleague's desk"
		// is a narrower objection than open-world and deserves its own
		// sentence; see internal/agent/subagent.
		return tools.Annotations{ReadOnly: mcp.No, Destructive: mcp.No, OpenWorld: mcp.Yes}
	case MarkOnboardedTool:
		// A write whose repeat is genuinely free: the marker is a fact
		// about this seat, and setting it again sets the same fact. Private
		// state — a row in the seat's own store — so OpenWorld is
		// explicitly No.
		return tools.Annotations{
			ReadOnly: mcp.No, Destructive: mcp.No, Idempotent: mcp.Yes, OpenWorld: mcp.No,
		}
	case ReflectAndPersistTool:
		// The agent's own diary: a write, and each call is another note, so
		// nowhere near idempotent. Private state — read back by this seat's
		// own next turn and by nobody else — so OpenWorld is explicitly No.
		// It used to fall through the default arm, where the missing hint
		// made it a shared write.
		return tools.Annotations{ReadOnly: mcp.No, Destructive: mcp.No, OpenWorld: mcp.No}
	case RunSandboxTool:
		// The one builtin that writes to a SHARED SURFACE, which
		// ReadOnly=No plus OpenWorld=Yes is how that is stated: a coding
		// run pushes branches and opens pull requests other people see, so
		// mcp.WritesToSharedSurface reads true and the sub-agent guard
		// keeps it away from a sub-agent acting under its parent's name,
		// which ALSO denies it by name for a second and independent
		// reason — a worker cannot park for a detached run's result.
		// Not destructive — a branch and a pull request are additive — and
		// nowhere near idempotent: a second call is a second run, a second
		// box, and a second set of commits.
		return tools.Annotations{ReadOnly: mcp.No, Destructive: mcp.No, OpenWorld: mcp.Yes}
	case ListWorkItemsTool, GetWorkItemTool, ListPagesTool, GetPageTool,
		tracker.GetWorkCatalogueTool, tracker.ListProjectsTool,
		tracker.DescribeProjectTool, tracker.SprintReportTool,
		tracker.TaskActivityTool, tracker.MyWorkTool,
		tracker.ListWorkGoalsTool:
		// Reads, and idempotent: asking twice costs a round and changes
		// nothing. The catalogue lookup belongs here with the rest — left
		// out, it fell to the default arm, whose ReadOnly=No with
		// OpenWorld unset is the combination [mcp.WritesToSharedSurface]
		// reads as TRUE, so a worker granted a read-only vocabulary
		// lookup was refused it as a write to a surface a human reads
		// while list_work_items beside it was admitted.
		return tools.Annotations{ReadOnly: mcp.Yes, Idempotent: mcp.Yes}
	case tracker.ManageSprintTool:
		// A WRITE EVERYBODY SEES — a start changes what a whole team is
		// expected to work on — so OpenWorld is Yes. Not destructive: a
		// close destroys nothing and the tasks keep their rows. And NOT
		// idempotent, which is the one that matters: a second `rollover`
		// call moves whatever arrived since, and a second `close` is
		// refused rather than free.
		return tools.Annotations{
			ReadOnly: mcp.No, Destructive: mcp.No, OpenWorld: mcp.Yes,
		}
	case tracker.WriteProjectTool:
		// A WRITE EVERYBODY SEES — a declared tag is a filter on
		// everybody's board and an archived project takes no work from
		// anyone — so OpenWorld is Yes and [mcp.WritesToSharedSurface]
		// reads true, which keeps it away from a sub-agent acting under
		// its parent's name.
		//
		// DESTRUCTIVE, and the reason is the facet a caller is most
		// likely to reach for by accident rather than the one it is
		// named after: `fields` REPLACES a project's declarations, so a
		// call sending a short list retires every field it left out.
		// Declaring a tag is additive and archiving is reversible by
		// nothing, and the flag asks whether a call can undo somebody
		// else's work — which this one can.
		//
		// NOT idempotent as a whole, although an add genuinely is: a
		// second `tags_rename` against a set a lead has since edited is
		// a different change, and the annotation describes the tool a
		// model is offered rather than its luckiest facet.
		return tools.Annotations{
			ReadOnly: mcp.No, Destructive: mcp.Yes, OpenWorld: mcp.Yes,
		}
	case tracker.RemoveWorkItemTool, tracker.RestoreWorkItemTool:
		// WRITES EVERYBODY SEES — a removal takes an item off every board
		// in the company — so OpenWorld is Yes and
		// [mcp.WritesToSharedSurface] reads true, which keeps them away
		// from a sub-agent acting under its parent's name. Not
		// destructive: a removal is reversible at any age and destroys
		// nothing, which is exactly what separates it from a purge. And
		// each is IDEMPOTENT: a task already in the trash is left there
		// and reported as success, because a half-finished subtree
		// removal has to be able to be re-run.
		return tools.Annotations{
			ReadOnly: mcp.No, Destructive: mcp.No,
			Idempotent: mcp.Yes, OpenWorld: mcp.Yes,
		}
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
	case WritePageTool, CommentOnPageTool:
		// Shared and additive: a page and a comment on one are both new
		// records everyone in the company can read, so OpenWorld is Yes
		// and the sub-agent guard keeps them from a worker acting under
		// its parent's name.
		return tools.Annotations{ReadOnly: mcp.No, Destructive: mcp.No, OpenWorld: mcp.Yes}
	case SavePageTool:
		// Shared, and DESTRUCTIVE: it replaces a body somebody wrote.
		// The revision history is what makes it recoverable, which is
		// exactly the distinction Destructive draws — reversible, not
		// harmless.
		return tools.Annotations{ReadOnly: mcp.No, Destructive: mcp.Yes, OpenWorld: mcp.Yes}
	case RefineSkillTool:
		// It replaces a body. The prior version is archived, so this is
		// reversible — which is exactly what Destructive asks about. The
		// body lives in the agent's OWN learning store, not in the
		// knowledge base `load_tool_skill` reads, so OpenWorld is
		// explicitly No.
		return tools.Annotations{ReadOnly: mcp.No, Destructive: mcp.No, OpenWorld: mcp.No}
	default:
		// NO MEMBERS, deliberately. Every builtin is named above, and a new
		// one that lands here is classified as a write to a shared surface
		// until somebody says which it is — the fail-closed direction for a
		// security boundary, and the reason the private-state tools are
		// named rather than defaulted.
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

// projectReads reports whether this build's reader answers the project seam.
//
// A TYPE ASSERTION rather than a second field on the deps, because the reader
// is ONE object: a build wiring the native tracker satisfies the whole seam
// and a build wiring none satisfies nothing, so a separate switch would be a
// second thing to keep in step with the first.
func projectReads(deps WorkDeps) bool {
	if deps.Reader == nil {
		return false
	}
	_, ok := deps.Reader.(ProjectReader)
	return ok
}

// feedReads reports whether this build's reader answers the feed seam.
func feedReads(deps WorkDeps) bool {
	if deps.Reader == nil {
		return false
	}
	_, ok := deps.Reader.(FeedReader)
	return ok
}

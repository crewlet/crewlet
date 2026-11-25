package builtin

import (
	"context"
	"fmt"
	"slices"

	"github.com/crewlet/crewlet/internal/artifactsdomain"
	"github.com/crewlet/crewlet/internal/chatlet"
	"github.com/crewlet/crewlet/internal/compact"
	"github.com/crewlet/crewlet/internal/doclet"
	"github.com/crewlet/crewlet/internal/gitlet"
	"github.com/crewlet/crewlet/internal/mcp"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/tools"
	"github.com/crewlet/crewlet/internal/worklet"
)

// Deps are the node-level things the builtins act through.
//
// Node-level so the tools register once per epoch: a store is the same for
// every seat, while the seat arrives on each call (tools.SeatCallable).
// Per-seat deps would list every builtin once per colleague.
//
// Every field is optional: a node without a store, or a company without A2A,
// gets the tools it can serve rather than a boot failure or a nil dereference.
type Deps struct {
	// A2A opens agent-to-agent channels. Nil omits a2a_ask.
	A2A Asker

	// Env resolves a `${VAR}` the chart's contacts carry (a Slack id, an
	// operator binding) for lookup_colleague and a2a_ask through this node's
	// chain: its secret store, then the environment it was handed. Nil reads
	// the process environment, as a registry built outside an engine has.
	// It is the chain notification routing resolves the same fields through,
	// so a person a notification mentions is also found by a lookup.
	Env org.EnvLookup

	// Skills, Episodes, Diary and Onboarding are the learning stores. Each
	// nil omits the tools that need it.
	Skills     SkillStore
	Refinable  RefinableSkills
	Episodes   EpisodeStore
	Diary      DiaryStore
	Onboarding OnboardingStore

	// ToolSkills is the operator-authored tool guidance. Nil omits
	// load_tool_skill and disarms the required-skill guard, since a guard
	// with no unlock would refuse tools the model cannot free.
	ToolSkills ToolSkills

	// EpisodeLimit is how many turns query_episodes returns when the model
	// names no limit: learning.episodic.retrieval_limit. Zero takes
	// [DefaultEpisodeLimit].
	EpisodeLimit int

	// SkillBodyMax caps a refined skill's body:
	// learning.skill_refinement.max_body_chars. Zero takes
	// [DefaultSkillBodyMax].
	SkillBodyMax int

	// SkillVersionsKept caps a skill's archived history:
	// learning.skill_refinement.max_versions_kept. Zero lets the store
	// apply its own default.
	SkillVersionsKept int

	// Compact condenses what does not fit where a bound is genuine: here, a
	// recalled turn's account of what it did, past
	// [learning.EpisodeAccountBytes], in query_episodes' answer. Nil
	// condenses nothing, and such an account is named by its size instead.
	Compact *compact.Compactor

	// Recall is the turn-start prefetch's semantic search, re-run on demand.
	// Nil leaves query_episodes on recency and conversation and refresh_memory
	// on recency, as a company with no embeddings has.
	Recall Recaller

	// RefreshesPerTurn is how many DISTINCT context hints one turn may
	// re-filter its notes on: learning.personal_memory.max_refreshes_per_turn.
	// Zero takes [DefaultRefreshesPerTurn].
	RefreshesPerTurn int

	// Events is where the skill lifecycle's telemetry (skill_used,
	// skill_refined, skill_promoted) goes. Nil publishes nothing, as a
	// registry built outside an engine has.
	Events Telemetry

	// Knowledge searches the team knowledge base on demand. Nil omits
	// search_knowledge. It is the seam the turn-start prefetch reads, so the
	// two never disagree about scope, exclusions or credentials.
	Knowledge KnowledgeSearcher

	// GrantHeld reads whether the engine's grant to a seat on an access lane
	// is in place, as the seat access ledger observed it; known is false where
	// the ledger cannot be read. Nil knows nothing.
	GrantHeld func(ctx context.Context, handle, lane string) (held, known bool)

	// Pages is Doclet. Both halves nil omits all five tools, as a company
	// running Confluence has.
	Pages PageDeps

	// Work is Worklet. Both halves nil omits every one of its tools, as a
	// company running Jira has: a seat offered a tracker tool for a tracker
	// the company does not run would reach for it and fail at the call.
	Work WorkDeps

	// Artifacts are the artifacts tools' dependencies; ArtifactData are the
	// datastore tools'. Each zero value omits its tools. See artifacts.go and
	// artifactdata.go.
	Artifacts    ArtifactDeps
	ArtifactData ArtifactDataDeps

	// Render is render_artifact's, a seat's alone. The zero value omits
	// it. See artifacts_render.go.
	Render RenderDeps

	// Code is the code read tools' — the company's own repositories, read on
	// the server. The zero value omits all of them, which is a company that
	// hosts no code. See codetools.go.
	Code CodeDeps

	// Health is the health tools': the fleet's nodes and their alarms, and the
	// Gitlet nodes' status. The zero value omits both. Registered for every
	// seat, offered only to one whose role carries the health grant: the table
	// refuses every other seat before any object is named ([Withheld]). See
	// healthtools.go.
	Health HealthDeps

	// SSH is the SSH tools': the company's servers, reached for a seat whose
	// SSH block grants them. The zero value omits both. Offered only to a seat
	// whose role carries the grant, as Health is. See ssh.go.
	SSH SSHDeps

	// Growth is the growth integrations' products (ADR-0170): each lists its
	// tools in its catalog.
	Growth GrowthDeps

	// Atlas is the atlas tools': the company's atlas, read as the seat within
	// whoever woke its turn (ADR-0109), a private address only within its own
	// reach. The zero value omits them. See atlas.go.
	Atlas AtlasDeps

	// AtlasRecord is the atlas's agent tools': what a seat records in the
	// atlas from a call of its own turn, and its dispute of what the atlas
	// holds (ADR-0132). The zero value omits them. See atlasrecord.go.
	AtlasRecord AtlasRecordDeps
	// Delegated is what a delegate's verdict tool acts through (ADR-0125);
	// a nil desk omits review_delegated.
	Delegated DelegatedDeps
	// Pulls are the company's GitHub pull requests, which ask_to_merge and
	// ask_to_deploy read and merge as the seat's own app; nil serves Gitlet's
	// changes alone, and with no Gitlet writes either omits both. See
	// release.go.
	Pulls PullRequests

	// Sessions are the seat's own executor sessions, which its executor
	// lists and reads back: its rows, the objects they name and the switch
	// each call asks. Any of the three nil omits both tools, which is what a
	// company that keeps no session has (`turn_engine.conversation_session.
	// enabled: false`), and a node with no object store. See sessions.go.
	Sessions SessionDeps

	// Withdraw is withdraw_ask's resolver and run desk (ADR-0111); its tracker
	// reads and writes are [Deps.Work]'s, set by [Register].
	Withdraw WithdrawDeps

	// Chat is Chatlet. Every half nil omits all nine tools, as a company on
	// Slack or Mattermost has: there the vendor's own MCP server is the
	// colleague surface, and `post_message` would answer into a room nobody
	// reads.
	Chat ChatDeps

	// Authorize decides whether the party behind a call may make it.
	//
	// [Register] wraps every tool in it once: the action is the tool's own
	// name, so one wrapper is the one place the check cannot be forgotten.
	// Nil refuses everything, unlike a nil skill gate: a missing authority
	// decision is a surface shipped without wiring one. See authority.go.
	//
	// [Register] pushes it down into [WorkDeps], [PageDeps] and [ChatDeps],
	// whose tools take some checks only after a read; one field means no
	// caller can set those three differently.
	Authorize Authorizer

	// Requesters composes who a seat's turn acts for (the requester ceiling,
	// ADR-0109), which the gate binds to every call the turn makes, holding
	// each app verb to the apps whoever woke the turn uses; the families
	// deciding a relation for them read it too. Nil binds nobody, and a call
	// is decided for the seat alone: a seat's registry is always handed one
	// (internal/engine's equip).
	Requesters Requesters
}

// Register adds every builtin the given dependencies can support.
//
// It returns the names registered, so a caller can log what a seat can
// reach: "the model never called query_episodes" and "query_episodes was
// not there" send a reader to opposite places.
//
// A tool whose dependency is absent is omitted, never registered and
// broken: a model shown a tool that always fails learns to distrust the
// whole catalogue.
func Register(reg *tools.Registry, deps Deps) ([]string, error) {
	if reg == nil {
		return nil, fmt.Errorf("builtin: no registry")
	}
	deps.Work.Authorize, deps.Pages.Authorize = deps.Authorize, deps.Authorize
	deps.Chat.Authorize = deps.Authorize
	// And the one composer, for [Deps.Authorize]'s reason: the families read
	// the ceiling the gate bound, and a second composer could compose somebody
	// else than the gate did.
	if deps.Requesters != nil {
		deps.Artifacts.Requesters, deps.Code.Requesters = deps.Requesters, deps.Requesters
		deps.Health.Requesters = deps.Requesters
	}

	deps.Work.entries = atlasReader{deps: deps.Atlas, code: deps.Code, authorize: deps.Authorize}

	withdraw := deps.Withdraw
	withdraw.Work = deps.Work

	// lookup_colleague is unconditional: its corpus is the turn's own org, and
	// every other addressing decision goes through it.
	candidates := []candidate{
		{&lookupColleague{env: deps.Env}, true},
		{&a2aAsk{svc: deps.A2A, env: deps.Env, requesters: deps.Requesters}, deps.A2A != nil},
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
			compact:  deps.Compact,
			limit:    orDefault(deps.EpisodeLimit, DefaultEpisodeLimit),
		}, deps.Episodes != nil},
		{&refreshMemory{
			diary:    deps.Diary,
			recall:   deps.Recall,
			maxHints: deps.RefreshesPerTurn,
		}, deps.Diary != nil},
		{&reflectAndPersist{diary: deps.Diary}, deps.Diary != nil},
		// THE SEAT'S OWN SESSIONS, all three parts or neither: missing any one,
		// neither tool can answer a call. See sessions.go.
		{&listSessions{deps: deps.Sessions}, deps.Sessions.serves()},
		{&readSession{deps: deps.Sessions, within: SessionReadWithin}, deps.Sessions.serves()},
		{&markOnboarded{onboarding: deps.Onboarding}, deps.Onboarding != nil},
		{&loadToolSkill{skills: deps.ToolSkills, events: deps.Events}, deps.ToolSkills != nil},
		{&searchKnowledge{search: deps.Knowledge, events: deps.Events, granted: deps.GrantHeld},
			deps.Knowledge != nil},
		{&listWorkItems{deps: deps.Work}, deps.Work.Reader != nil},
		{&getWorkItem{deps: deps.Work}, deps.Work.Reader != nil},
		{&createWorkItem{deps: deps.Work, pages: deps.Pages.Reader}, deps.Work.Writer != nil},
		{&updateWorkItem{deps: deps.Work},
			deps.Work.Writer != nil && deps.Work.Reader != nil},
		{&commentOnWorkItem{deps: deps.Work, pages: deps.Pages.Reader},
			deps.Work.Writer != nil && deps.Work.Reader != nil},
		// AND WITHDRAWING ONE'S OWN QUESTION, on a work item or one the
		// seat's executor waits on. See withdrawask.go.
		{&withdrawAsk{deps: withdraw, requesters: deps.Requesters}, withdraw.serves()},
		// AND ASKING FOR ACCESS, a decision the comment tool posts whose grant
		// option the engine builds. See requestaccess.go.
		{&requestAccess{comment: &commentOnWorkItem{deps: deps.Work, pages: deps.Pages.Reader}},
			deps.Work.Writer != nil && deps.Work.Reader != nil},
		// AND ASKING TO RUN ONE EXACT OPERATE COMMAND, a decision the comment
		// tool posts whose run option the engine builds. See sshask.go.
		{&askToOperate{deps: deps.SSH, comment: &commentOnWorkItem{deps: deps.Work, pages: deps.Pages.Reader}},
			deps.SSH.serves() && deps.Work.Writer != nil && deps.Work.Reader != nil},
		// AND THE FOLD, a seat's where the trash is not: it leaves the item
		// cancelled and linked, the gesture a seat can already make by hand,
		// so nothing goes missing. See workmerge.go.
		{&mergeWorkItem{deps: deps.Work}, deps.Work.Merges != nil && deps.Work.Reader != nil},
		// AND THE MOVE, which is a seat's behind the gate a re-route has:
		// the lead of the item's project decides where its work lives.
		// See workmove.go.
		{&moveWorkItem{deps: deps.Work},
			deps.Work.Moves != nil && deps.Work.Reader != nil},
		// AND THE RANKED SEARCH, which is the only way to reach what an
		// item's DESCRIPTION says — see worksearch.go.
		{&searchWorkItems{deps: deps.Work}, deps.Work.Search != nil},
		// READING THE CATALOGUE IS A SEAT'S, writing it is not: a create
		// refuses a type the company has not declared, and a model that
		// cannot read the catalogue can only guess at one.
		{&getWorkCatalogue{deps: deps.Work}, deps.Work.Reader != nil},
		// AND SO IS READING THE PROJECT: a create refuses an unknown project and
		// an empty required field, a round a model skips by looking first.
		{&listProjects{deps: deps.Work}, projectReads(deps.Work)},
		{&describeProject{deps: deps.Work}, projectReads(deps.Work)},
		// AND THE ONE PROJECT WRITE a seat holds, for one facet: a
		// create refuses a label the project has not declared, so a
		// seat without this could never use `labels` at all.
		{&writeProject{deps: deps.Work},
			deps.Work.ProjectWriter != nil},
		// AND THE TWO ABOUT CHANGE rather than about state: a board says
		// what is there now, and no filter over its rows can answer
		// "who moved this" or "what is waiting on me".
		{&taskActivity{deps: deps.Work}, feedReads(deps.Work)},
		{&myWork{deps: deps.Work}, feedReads(deps.Work)},
		// AND THE PROJECT'S FILES, which need the object store as well as the
		// tracker: a file is a row naming an object. See workfiles.go.
		{&listProjectFiles{deps: deps.Work}, deps.Work.Files != nil},
		{&readProjectFile{deps: deps.Work}, deps.Work.Files != nil && deps.Work.Objects != nil},
		{&writeProjectFile{deps: deps.Work},
			deps.Work.FileWriter != nil && deps.Work.Objects != nil},
		{&removeProjectFile{deps: deps.Work}, deps.Work.FileWriter != nil},
		// THE CHAT TOOLS, each gated on the half it needs: a readable, unwritable
		// chat gets the reads, not a `post_message` that fails at the call.
		{&postMessage{deps: deps.Chat}, deps.Chat.Writer != nil},
		{&replyInThread{deps: deps.Chat}, deps.Chat.Writer != nil},
		{&sendDM{deps: deps.Chat}, deps.Chat.Writer != nil},
		{&readChannel{deps: deps.Chat}, deps.Chat.Reader != nil},
		{&listChannels{deps: deps.Chat}, deps.Chat.Reader != nil},
		// AND THE RANKED SEARCH, on its own half: the inverted list is
		// this node's own index rather than the replicated rows, so a
		// build can hold the whole conversation and no way to rank it.
		{&searchMessages{deps: deps.Chat}, deps.Chat.Search != nil},
		{&reactToMessage{deps: deps.Chat}, deps.Chat.Writer != nil},
		{&joinChannel{deps: deps.Chat}, deps.Chat.Writer != nil},
		{&leaveChannel{deps: deps.Chat}, deps.Chat.Writer != nil},
		{&listPages{deps: deps.Pages}, deps.Pages.Reader != nil},
		{&getPage{deps: deps.Pages, events: deps.Events}, deps.Pages.Reader != nil},
		{&writePage{deps: deps.Pages}, deps.Pages.Writer != nil},
		{&savePage{deps: deps.Pages}, deps.Pages.Writer != nil && deps.Pages.Reader != nil},
		{&commentOnPage{deps: deps.Pages}, deps.Pages.Writer != nil && deps.Pages.Reader != nil},
	}
	// AND THE CODE TOOLS, the operator catalogue's own list ([CodeTools]),
	// the writes only where Gitlet's are wired (each a delivery, the switch
	// below); the health tools the same way ([HealthTools]). The release
	// requests and the verdict tool read Gitlet through the code tools' deps.
	release := ReleaseDeps{Code: deps.Code, Pulls: deps.Pulls}
	delegatedDeps := deps.Delegated
	delegatedDeps.Release = release
	candidates = slices.Concat(candidates, artifactCandidates(deps), artifactDataCandidates(deps),
		renderCandidates(deps), codeCandidates(deps.Code), healthCandidates(deps.Health), sshCandidates(deps.SSH),
		atlasCandidates(atlasReader{deps: deps.Atlas, code: deps.Code, authorize: deps.Authorize}),
		atlasAgentCandidates(deps.AtlasRecord,
			atlasReader{deps: deps.Atlas, code: deps.Code, authorize: deps.Authorize}),
		releaseCandidates(release), delegatedCandidates(delegatedDeps), growthCandidates(deps.Growth))

	var names []string
	for _, c := range candidates {
		if !c.on {
			continue
		}
		opts := []tools.Option{}
		if _, fixed := c.tool.(fixedTool); fixed {
			opts = append(opts, tools.AsksBeforeEveryCall)
		}
		if u, ok := c.tool.(interface{ OutputUnkept() bool }); ok && u.OutputUnkept() {
			opts = append(opts, tools.KeepsNoOutput)
		}
		// A DELIVERY, AND WHERE IT LANDS. A turn woken by an assignment answers by
		// writing to the item; without this the gate sees only builtins and
		// corrects the turn into another round. The surface stops the opposite
		// error: a founder waiting in a chat thread is on neither Worklet nor
		// Doclet. It is the feed's own source name, which the gate compares with
		// an inbound wake's NotificationSource: a second spelling makes every
		// obligation unreachable and the check falls back to "any delivery counts"
		// (TestTheNativeWritesDeliverToTheirOwnFeedSource).
		switch {
		case slices.Contains(WorkWrites(), c.tool.Name()):
			opts = append(opts, tools.DeliversTo(worklet.Source))
		case slices.Contains(PageWrites(), c.tool.Name()):
			opts = append(opts, tools.DeliversTo(doclet.Source))
		case slices.Contains(ChatWrites(), c.tool.Name()):
			// THE THREE POSTING TOOLS, NOT THE REACTION OR A JOIN: the gate asks
			// whether the person waiting was reached, and neither reaches anybody, so
			// counting them would let a turn discharge its obligation with a thumb.
			// See [chatlet.WriteTools].
			opts = append(opts, tools.DeliversTo(chatlet.Source))
		case slices.Contains(CodeWrites(), c.tool.Name()):
			// AND A CODE WRITE REACHES GITLET: a branch moved or a
			// repository changed is the answer to work that came from it.
			opts = append(opts, tools.DeliversTo(gitlet.Source))
		case slices.Contains(ArtifactCommentWrites(), c.tool.Name()):
			// AND A COMMENT OR A RESOLVE ANSWERS ON THE ARTIFACT, where the
			// person whose comment woke the turn reads it.
			opts = append(opts, tools.DeliversTo(artifactsdomain.Source))
		}
		// THE GATE WRAPS EVERY ONE, here rather than in each tool
		// ([Deps.Authorize]), so a new tool cannot ship ungated.
		if err := reg.RegisterWith(gate(c.tool, deps.Authorize, deps.Requesters),
			tools.OriginBuiltin,
			annotationsFor(c.tool.Name()), opts...); err != nil {
			return names, fmt.Errorf("builtin: %w", err)
		}
		names = append(names, c.tool.Name())
	}
	log.Info("builtin_tools_registered", "count", len(names), "tools", names)
	return names, nil
}

// candidate is one builtin a registry may offer, and whether its
// dependencies are present to serve it ([Register]).
type candidate struct {
	tool tools.Callable
	on   bool
}

// annotationsFor classifies a builtin for the delivery gate and the
// sub-agent guard, and [AnnotationsFor] for the MCP surfaces this engine
// serves (docs/concepts/tool-capabilities.md).
//
// ReadOnly lets the delivery check see a turn that only read: unannotated
// is not a known read, so an unannotated read-only builtin would make
// every recall look like a delivery.
//
// OpenWorld is a tri-state: [mcp.WritesToSharedSurface] is `ReadOnly == No`
// AND `OpenWorld != No`, so a write to private state only (a seat's or a
// person's own) says `OpenWorld: mcp.No`, or a sub-agent is denied it.
func annotationsFor(name string) tools.Annotations {
	if a, ok := growthAnnotations(name); ok {
		return a
	}
	if a, ok := artifactAnnotations(name); ok {
		return a
	}
	if a, ok := artifactDataAnnotations(name); ok {
		return a
	}
	if a, ok := codeAnnotations(name); ok {
		return a
	}
	if a, ok := healthAnnotations(name); ok {
		return a
	}
	if a, ok := sshAnnotations(name); ok {
		return a
	}
	if a, ok := releaseAnnotations(name); ok {
		return a
	}
	if a, ok := atlasAnnotations(name); ok {
		return a
	}
	if a, ok := atlasAgentAnnotations(name); ok {
		return a
	}
	if name == ReviewDelegatedTool {
		return reviewAnnotations()
	}
	switch name {
	case LookupColleagueTool, UseSkillTool, QueryEpisodesTool, RefreshMemoryTool,
		LoadToolSkillTool, SearchKnowledgeTool, ListSessionsTool, ReadSessionTool, AtlasSearchTool, AtlasGetTool,
		AtlasTraceTool:
		// Reads, and idempotent, so a phase may retry one. The session tools
		// read the seat's own record and take no hold.
		return tools.Annotations{ReadOnly: mcp.Yes, Idempotent: mcp.Yes}
	case A2AAskTool:
		// It leaves this process. Not destructive (a message, not an edit), and
		// not idempotent: asking twice spends two of a colleague's turns. A worker
		// is also denied it by name; see internal/agent/subagent.
		return tools.Annotations{ReadOnly: mcp.No, Destructive: mcp.No, OpenWorld: mcp.Yes}
	case MarkOnboardedTool:
		// Setting it again sets the same fact about this seat: idempotent.
		// Private state, so OpenWorld is No.
		return tools.Annotations{
			ReadOnly: mcp.No, Destructive: mcp.No, Idempotent: mcp.Yes, OpenWorld: mcp.No,
		}
	case ReflectAndPersistTool:
		// The agent's own diary: each call is another note, so not idempotent.
		// Read only by this seat's next turn, so OpenWorld is No.
		return tools.Annotations{ReadOnly: mcp.No, Destructive: mcp.No, OpenWorld: mcp.No}
	case ReadChannelTool, ListChannelsTool, SearchMessagesTool,
		ListWorkItemsTool, GetWorkItemTool, ListPagesTool, GetPageTool,
		worklet.GetWorkCatalogueTool, worklet.ListProjectsTool,
		worklet.DescribeProjectTool,
		worklet.TaskActivityTool, worklet.MyWorkTool,
		worklet.SearchWorkItemsTool,
		worklet.GetPersonTool, worklet.WorkInboxTool,
		worklet.ListWorkViewsTool,
		worklet.ListProjectFilesTool, worklet.ReadProjectFileTool:
		// Reads, and idempotent: asking twice costs a round and changes nothing.
		return tools.Annotations{ReadOnly: mcp.Yes, Idempotent: mcp.Yes}
	case worklet.MarkInboxTool, worklet.SetPinsTool, CodeMarkInboxTool, CodeMarkViewedTool, ArtifactMarkInboxTool:
		// A PERSON'S OWN STATE, written only for the person whose it is (the
		// tracker refuses anything else): no second party, so OpenWorld is No.
		// Idempotent: each replaces the list it names, so a retry after a lost
		// answer is safe.
		return tools.Annotations{
			ReadOnly: mcp.No, Destructive: mcp.No,
			Idempotent: mcp.Yes, OpenWorld: mcp.No,
		}
	case worklet.DeleteWorkViewTool:
		// SHARED: a deleted view leaves everyone's strip and pins. Destructive:
		// nothing brings it back. Not idempotent: a second delete finds no view
		// and is refused.
		return tools.Annotations{
			ReadOnly: mcp.No, Destructive: mcp.Yes, OpenWorld: mcp.Yes,
		}
	case worklet.SetPrioritiesTool:
		// THE ONE PERSON WRITE THAT REACHES ACROSS PEOPLE, hence not in the arm
		// above: a lead may set the queue of somebody in their line
		// (`priorities_set_by` shows them who), so OpenWorld is Yes. Idempotent,
		// as the list replaces; not destructive, since the previous order is not
		// work and the person's own next write takes their queue back.
		return tools.Annotations{
			ReadOnly: mcp.No, Destructive: mcp.No,
			Idempotent: mcp.Yes, OpenWorld: mcp.Yes,
		}
	case worklet.MergeWorkItemTool:
		// SHARED: the duplicate closes on every board and its subtasks move to
		// somebody else's item. Destructive: nothing puts the subtree back. Not
		// idempotent: a second fold of the pair is refused, and a fold of another
		// pair closes another item.
		return tools.Annotations{
			ReadOnly: mcp.No, Destructive: mcp.Yes, OpenWorld: mcp.Yes,
		}
	case worklet.WriteProjectFileTool:
		// SHARED with the project. Destructive: a put over an existing path
		// replaces it and the old content is collected. Idempotent: the same
		// content at the same path leaves the same file.
		return tools.Annotations{
			ReadOnly: mcp.No, Destructive: mcp.Yes, Idempotent: mcp.Yes, OpenWorld: mcp.Yes,
		}
	case worklet.RemoveProjectFileTool:
		// Destructive and shared for the put's reason: the content is collected.
		// Not idempotent: a second removal is refused, the file being gone.
		return tools.Annotations{
			ReadOnly: mcp.No, Destructive: mcp.Yes, OpenWorld: mcp.Yes,
		}
	case worklet.MoveWorkItemTool:
		// SHARED: an item and its subtree change boards and every key changes.
		// Not destructive: every former key still resolves and a move back undoes
		// it. Not idempotent: the same move again is refused, the item already
		// being there.
		return tools.Annotations{
			ReadOnly: mcp.No, Destructive: mcp.No, OpenWorld: mcp.Yes,
		}
	case worklet.WriteProjectTool:
		// SHARED: a declared tag filters everybody's board and an archived project
		// takes no work. Destructive: `fields` REPLACES a project's declarations,
		// so a short list retires every field it left out, and nothing undoes an
		// archive. Not idempotent as a whole: a second `tags_rename` against an
		// edited set is a different change, and the annotation describes the tool
		// a model is offered, not its luckiest facet.
		return tools.Annotations{
			ReadOnly: mcp.No, Destructive: mcp.Yes, OpenWorld: mcp.Yes,
		}
	case worklet.RemoveWorkItemTool, worklet.RestoreWorkItemTool:
		// SHARED: a removal takes an item off every board. Not destructive: a
		// removal is reversible at any age, unlike a purge. Idempotent: a subtree
		// removal or restore stopped part-way must be able to be made again.
		return tools.Annotations{
			ReadOnly: mcp.No, Destructive: mcp.No,
			Idempotent: mcp.Yes, OpenWorld: mcp.Yes,
		}
	case worklet.PlaceWorkItemTool:
		// SHARED: a lane change is a status change the item's people are woken
		// about. Not destructive: dragging the card back restores it. Idempotent:
		// the same drop finds the card in place and writes nothing.
		return tools.Annotations{
			ReadOnly: mcp.No, Destructive: mcp.No,
			Idempotent: mcp.Yes, OpenWorld: mcp.Yes,
		}
	case CreateWorkItemTool:
		// Shared, additive, and not idempotent: a second call is a second item,
		// the duplicate the tool's description tells the model to search for first.
		return tools.Annotations{ReadOnly: mcp.No, Destructive: mcp.No, OpenWorld: mcp.Yes}
	case CommentOnWorkTool:
		// Shared and additive. Not idempotent although a turn's comment takes a
		// deterministic id: that protects a re-run turn, and a model calling twice
		// with different text says two things.
		return tools.Annotations{ReadOnly: mcp.No, Destructive: mcp.No, OpenWorld: mcp.Yes}
	case UpdateWorkItemTool:
		// Shared and destructive: it replaces a title, a description or an
		// assignee, the old value surviving only in the change record. A status
		// flip riding the same tool does not make the tool safe.
		return tools.Annotations{ReadOnly: mcp.No, Destructive: mcp.Yes, OpenWorld: mcp.Yes}
	case PostMessageTool, ReplyInThreadTool, SendDMTool:
		// WORDS ON SOMEBODY ELSE'S SCREEN, a DM's included (it reaches exactly one
		// second party): shared, so a worker cannot speak in its parent's voice
		// where the room has no way to tell. Not destructive (no seat is offered
		// an edit) and not idempotent: a second call is a second message. The
		// derived key that makes a re-run turn post once is the engine's
		// protection, not a licence to repeat.
		return tools.Annotations{ReadOnly: mcp.No, Destructive: mcp.No, OpenWorld: mcp.Yes}
	case ReactToMessageTool:
		// SHARED, but idempotent where the three above are not: a reaction is
		// keyed on (message, emoji, handle). It is not a delivery, a different
		// flag: see the registration switch in [Register].
		return tools.Annotations{
			ReadOnly: mcp.No, Destructive: mcp.No,
			Idempotent: mcp.Yes, OpenWorld: mcp.Yes,
		}
	case JoinChannelTool, LeaveChannelTool:
		// A SEAT'S OWN SUBSCRIPTION, in `mark_inbox`'s arm for its reason: the
		// write path refuses any other handle, so no second party is written for
		// and OpenWorld is No. Idempotent: each states a membership rather than
		// toggling one.
		return tools.Annotations{
			ReadOnly: mcp.No, Destructive: mcp.No,
			Idempotent: mcp.Yes, OpenWorld: mcp.No,
		}
	case chatlet.AddChannelMembersTool:
		// People put in a room the others see: OpenWorld is Yes. Not destructive.
		// Idempotent: it states who is in the room after it.
		return tools.Annotations{
			ReadOnly: mcp.No, Destructive: mcp.No,
			Idempotent: mcp.Yes, OpenWorld: mcp.Yes,
		}
	case chatlet.CreateChannelTool:
		// A ROOM THE COMPANY SEES and its founding members are put in, so
		// OpenWorld is Yes. Not destructive — a room is additive — and NOT
		// idempotent: the same name again is refused as taken, and another
		// name is a second room.
		return tools.Annotations{ReadOnly: mcp.No, Destructive: mcp.No, OpenWorld: mcp.Yes}
	case chatlet.OpenDMTool:
		// A CONVERSATION THE OTHERS ARE IN, so OpenWorld is Yes although
		// nothing is said yet. IDEMPOTENT by construction: its id IS its
		// participants, so opening it again reaches the same room.
		return tools.Annotations{
			ReadOnly: mcp.No, Destructive: mcp.No,
			Idempotent: mcp.Yes, OpenWorld: mcp.Yes,
		}
	case chatlet.UpdateChannelTool, chatlet.SetChannelMembersTool, chatlet.EditMessageTool,
		chatlet.DeleteMessageTool:
		// SHARED and destructive: a setting is replaced (archiving closes the
		// room), a membership set removes readers, an edit replaces what somebody
		// said and a delete takes it back. Idempotent: each states its result.
		return tools.Annotations{
			ReadOnly: mcp.No, Destructive: mcp.Yes,
			Idempotent: mcp.Yes, OpenWorld: mcp.Yes,
		}
	case chatlet.MarkChatReadTool:
		// A PERSON'S OWN STATE, in `mark_inbox`'s arm: OpenWorld is No.
		// Idempotent: a cursor only advances and a mute states one room's.
		return tools.Annotations{
			ReadOnly: mcp.No, Destructive: mcp.No,
			Idempotent: mcp.Yes, OpenWorld: mcp.No,
		}
	case SaveHomeBoardTool, SaveInstructionsTool:
		// A PERSON'S OWN STATE, so OpenWorld is No. Destructive: it replaces
		// the record whole, and what it drops is gone. Not idempotent: a
		// repeat is refused, its revision having moved.
		return tools.Annotations{
			ReadOnly: mcp.No, Destructive: mcp.Yes,
			Idempotent: mcp.No, OpenWorld: mcp.No,
		}
	case WritePageTool, CommentOnPageTool:
		// Shared and additive: new records everyone in the company can read.
		return tools.Annotations{ReadOnly: mcp.No, Destructive: mcp.No, OpenWorld: mcp.Yes}
	case SavePageTool:
		// Shared and destructive: it replaces a body somebody wrote. The revision
		// history makes it reversible, not harmless.
		return tools.Annotations{ReadOnly: mcp.No, Destructive: mcp.Yes, OpenWorld: mcp.Yes}
	case AnswerKnowledgeTool:
		// A READ THAT SPENDS: a cache miss is a model call charged to the
		// company, so it is not read-only (which also keeps the dashboard from
		// refetching it on focus) and not idempotent. It writes nothing anybody
		// reads, so OpenWorld is No.
		return tools.Annotations{
			ReadOnly: mcp.No, Destructive: mcp.No, Idempotent: mcp.No, OpenWorld: mcp.No,
		}
	case AnswerExecutorTool:
		// A WRITE SOMEBODY ELSE ACTS ON: the answer resumes a seat's executor,
		// whose moves reach the company. Not destructive, and not idempotent: the
		// run declines a second answer once the first resumed it, which is the
		// engine's protection, not the tool's shape.
		return tools.Annotations{ReadOnly: mcp.No, Destructive: mcp.No, OpenWorld: mcp.Yes}
	case AtlasRefreshTool:
		// AN ASK, which replaces nothing and is the same ask made twice, so
		// neither destructive nor a second change. Open-world: honouring it,
		// the engine reads a vendor outside itself.
		return tools.Annotations{ReadOnly: mcp.No, Destructive: mcp.No,
			Idempotent: mcp.Yes, OpenWorld: mcp.Yes}
	case RunScheduleTool:
		// A WRITE SOMEBODY ELSE ACTS ON: the run starts a turn on each seat
		// the schedule wakes, whose moves reach the company, so OpenWorld is
		// Yes. It replaces nothing, and NOT idempotent as a tool: a second
		// call is a second run, unless it is the same request sent again.
		return tools.Annotations{ReadOnly: mcp.No, Destructive: mcp.No,
			Idempotent: mcp.No, OpenWorld: mcp.Yes}
	case PauseSeatTool:
		// DESTRUCTIVE: `stop_running` ends a turn and its executor mid-flight,
		// and what they had not done is lost. Otherwise reversible, and
		// idempotent. Closed-world: it changes only whether a seat works.
		return tools.Annotations{ReadOnly: mcp.No, Destructive: mcp.Yes,
			Idempotent: mcp.Yes, OpenWorld: mcp.No}
	case SteerTurnTool:
		// A WRITE SOMEBODY ELSE ACTS ON: the note changes what a running turn does
		// next. Not destructive, and not idempotent: a second call is a second
		// note. The box collapses a retry of one request, which is the engine's
		// protection, not the tool's shape.
		return tools.Annotations{ReadOnly: mcp.No, Destructive: mcp.No, OpenWorld: mcp.Yes}
	case ResumeSeatTool:
		// Not destructive: the held mail is delivered, nothing is lost.
		// Idempotent: resuming a seat that is not paused changes nothing.
		return tools.Annotations{ReadOnly: mcp.No, Destructive: mcp.No,
			Idempotent: mcp.Yes, OpenWorld: mcp.No}
	case RefineSkillTool:
		// Not destructive: the replaced body is archived. It lives in the agent's
		// own learning store, not the knowledge base load_tool_skill reads, so
		// OpenWorld is No.
		return tools.Annotations{ReadOnly: mcp.No, Destructive: mcp.No, OpenWorld: mcp.No}
	default:
		// NO MEMBERS, deliberately: an unclassified builtin is a shared write
		// until somebody says which it is, the fail-closed direction.
		return tools.Annotations{ReadOnly: mcp.No, Destructive: mcp.No}
	}
}

// The shipped values for the knobs a caller may leave at zero.
//
// They match config.DefaultLearning: this package does not import
// internal/config, and a registry built directly (a test, an embedder)
// must land on the same numbers, not a zero that refuses everything.
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
// A type assertion rather than a deps field: the reader is one object, so a
// separate switch would be a second thing to keep in step.
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

// AnnotationsFor is one builtin's hints, for a surface that serves the
// catalogue to somebody else's MCP client: [internal/api/operator] to an
// operator's assistant and [internal/api/mcpbridge] to a seat's sandboxed
// harness, whose clients otherwise cannot tell a read from an irreversible
// write.
//
// The hints belong to the name, not the implementation (one tool type
// serves a seat and an operator), so they live in one switch rather than a
// method on every tool. A name this package does not classify gets the
// conservative default ([annotationsFor]).
func AnnotationsFor(name string) tools.Annotations { return annotationsFor(name) }

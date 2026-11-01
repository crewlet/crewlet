// Package toolname declares, once, the names of the engine's own tools that a
// package must SPELL without being able to import the package that implements
// the tool.
//
// # Why a leaf
//
// A tool's name is its owner's: the runner implements the discovery pair, and
// a package that can import the runner reads the name off it. But most of the
// packages that must name these tools sit BELOW their owner. The skill guard
// exempts the discovery pair from every trigger and the runner imports the
// skill guard; the worker deny list keeps a child off its parent's pair and
// the runner imports the worker package; the prompts that tell a model to
// call them import only the org model and the provenance fence; and the
// delivery remedy in the turn loop imports neither. Each of them spelled the
// names again, as a literal, with nothing comparing the copies — so a rename
// would have stopped exempting the pair from the skill guard, let a worker
// inherit the parent's `activate_tool` (the one escalation its deny list
// exists to stop), and told every model to call tools that no longer exist,
// all of it compiling.
//
// So the name lives here, importing nothing, and every package above or below
// the owner reads it from one place. A name EVERY reader can import stays
// with its owner; a name lands here only when some package that must spell it
// cannot import the package that owns it, or when it is a tool a phase surface
// offers of its own that no row of the authority table names, which the walk
// below would otherwise never read (see [AskAndWait]).
//
// # Held by a walk
//
// This package's suite walks every non-test Go file in the module for EVERY
// tool name the engine offers a model: every tool-shaped verb of the authority
// table, which internal/agent/builtin holds equal to the tools its registry
// and the operator catalogue register, and every name declared here, among
// them the tools a phase surface offers of its own (the submissions, the
// discovery pair, `delegate` and the executor's [AskAndWait]), which no row
// names. A string literal carrying one, as a word of its own, is either that
// name's one declaration (a constant's whole value, in its owner) or a copy
// that fails the build. A prompt, a description or a refusal that names a tool
// builds its text from the constant, so a new copy of a name is caught the day
// it is written rather than the day the name changes.
//
// THE SET FAILS CLOSED: the runner's suite holds every tool a phase surface
// offers beyond the seat's registry to the same set
// (internal/toolname/toolnametest), so a tool built outside both sources
// fails there rather than dropping out of the walk unread. A hook's own name
// that no surface lists to a model, the tool bridge's `steer_take`, which a
// harness's hook calls, is no tool of a surface, and stays with its owner.
package toolname

// The discovery pair: the two meta-tools the executor reaches MCP tools
// through, since it is shown a server's name rather than its catalogue.
const (
	// ListMCPServerTools lists the tools one MCP server publishes.
	ListMCPServerTools = "list_mcp_server_tools"
	// Activate promotes a tool from the role's catalogue onto the next round.
	Activate = "activate_tool"
)

// Discovery is the discovery pair, in a slice of its own: a caller may keep or
// change what it is handed without reaching any other caller's copy.
func Discovery() []string {
	return []string{Activate, ListMCPServerTools}
}

// The phase submissions, which the runner implements. The skill guard exempts
// both (blocking one would brick its phase), and the prompts and the round-cap
// extension tell a model to call them.
const (
	// SubmitWork ends the executor's turn with its account of the work.
	SubmitWork = "submit_work"
	// SubmitReview is the reviewer's verdict on that account.
	SubmitReview = "submit_review"
)

// AskAndWait is the executor's run's other ending beside [SubmitWork]: it asks
// a person and parks on the answer (ADR-0068). The builtins implement it, the
// engine hands it to the runner outside the seat's registry, and the run's
// brief and the bridge that wakes the run's waiter name it.
//
// HERE AND NOT ONLY WITH ITS OWNER, though every reader can import the
// builtins: like a submission, it is the run's own ending and no verb the
// authority table decides, so no row names it, and declared only beside its
// implementation it was a tool name the walk this package's suite runs never
// read: a literal copy of it compiled and passed.
const AskAndWait = "ask_and_wait"

// The onboarding pass's two tools, which the builtins implement. The runner
// offers them without discovery, the skill guard exempts the terminator, and
// the prompts, the learning store's brief and the round-cap extension name
// them to the model.
const (
	// ReflectAndPersist writes a fact into the seat's own memory.
	ReflectAndPersist = "reflect_and_persist"
	// MarkOnboarded ends the onboarding pass.
	MarkOnboarded = "mark_onboarded"
)

// A2AAsk asks a colleague agent one question. The builtins implement it; the
// worker deny list keeps it from a worker, the authority table names its row,
// and the review prompt tells a reviewer the next round can reach a colleague
// with it.
const A2AAsk = "a2a_ask"

// LoadToolSkill unlocks a tool skill. The skill guard watches for it, a worker
// is granted it, and the prompts' skill catalogue tells a model to call it.
const LoadToolSkill = "load_tool_skill"

// Delegate hands work to short-lived workers. The executor's prompt renders
// its workers only when the seat holds it.
//
// THE ONE NAME HERE THAT IS ALSO AN ENGLISH WORD — "Do not delegate further"
// is prose about the act, not the tool — so the walk reads it as the tool's
// name only where the text marks it as code (see the suite).
const Delegate = "delegate"

// The tracker's two tools the escalation guidance names. The tracker owns its
// vocabulary; these two are here because the prompts, which import only the
// org model and the provenance fence, render the guidance only when the seat
// holds the first.
const (
	// CommentOnWorkItem comments on a work item, an ask among them.
	CommentOnWorkItem = "comment_on_work_item"
	// CreateWorkItem files a work item, an ask among them.
	CreateWorkItem = "create_work_item"
)

// WriteProject is the tracker's one project write a seat holds. The tracker
// owns it, and it is here because internal/caveat, which the tracker imports,
// words what a task filed to triage tells the model when its project's
// default assignee has left the chart: that the project's lead sets another
// with this tool.
const WriteProject = "write_project"

// The two names of the engine's own chat that a package below it spells. Chat
// owns its vocabulary, and these two are here because the prompts offer the
// first as the way to reach a person, and the search index, which the chat
// domain imports, names the second in what it logs.
const (
	// SendDM is a direct message to one colleague.
	SendDM = "send_dm"
	// SearchMessages searches the rooms a seat may read.
	SearchMessages = "search_messages"
)

// The two artifacts tools a format's reference names. The authority table
// declares every other artifacts tool's name, and the builtins read each off
// its row; these two are here because the format packages whose references
// tell a model to call them are pure, and the authority table's package
// reaches the store, which importing it would build into every reader of a
// format. A dashboard's reference points the model at the engine sources a
// board may read, and a deck's at the create that begins one from a starter.
const (
	// CreateArtifact makes an artifact, a deck from a starter among them.
	CreateArtifact = "create_artifact"
	// ListDashboardSources lists the engine sources a dashboard may read.
	ListDashboardSources = "list_dashboard_sources"
)

// LookupColleague resolves a name to a seat. The roster and the human
// colleagues section of a prompt tell a model to call it.
const LookupColleague = "lookup_colleague"

// QueryEpisodes recalls a seat's own past turns. The builtins implement it,
// and internal/config's schema describes the reflection setting that leaves
// its answer unsummarised — a description a struct tag cannot build, so the
// config type derives it from this constant.
const QueryEpisodes = "query_episodes"

// SubmitResult is how a delegated worker answers. The worker package owns it
// and imports the prompts, whose worker preamble tells a model to call it.
const SubmitResult = "submit_result"

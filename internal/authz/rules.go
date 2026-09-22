package authz

import (
	"maps"
	"slices"

	"github.com/crewlet/crewlet/internal/iam"
)

// The verbs this engine authorizes.
//
// NAMED AFTER THE VERB, NOT THE SURFACE. `PATCH /work/items/{key}` and the
// `update_work_item` tool are one action asked two ways, so they are one
// constant — which is the whole of what stops the operator MCP and a seat's
// own tools answering differently about one write.
//
// WHERE A TOOL EXISTS, ITS NAME IS THE ACTION, and that is load-bearing rather
// than tidy: it is what lets a walk in internal/agent/builtin hold this table
// against the tools this build actually registers, in BOTH directions. The
// walks inside this package can only check the table against itself — they
// went on passing over `list_work_goals` and `write_work_goal` after the goal
// verbs left the tracker, two rows deciding nothing, and would have gone on
// passing for as long as nobody happened to read them. A verb with no tool
// spells its name with a DOT (`pages.trash`, `config.read`), which is what
// tells the two apart.
const (
	// --- reading what the company is doing -------------------------- //
	ActionWorkRead      Action = "get_work_item"
	ActionWorkList      Action = "list_work_items"
	ActionWorkSearch    Action = "search_work_items"
	ActionWorkActivity  Action = "task_activity"
	ActionProjectRead   Action = "describe_project"
	ActionProjectList   Action = "list_projects"
	ActionCatalogueRead Action = "get_work_catalogue"
	ActionViewList      Action = "list_work_views"
	ActionPageRead      Action = "get_page"
	ActionPageList      Action = "list_pages"
	ActionKnowledgeRead Action = "search_knowledge"
	ActionColleagueRead Action = "lookup_colleague"

	// --- the caller's own working state ----------------------------- //
	ActionSkillUse      Action = "use_skill"
	ActionSkillLoad     Action = "load_tool_skill"
	ActionSkillRefine   Action = "refine_skill"
	ActionEpisodesQuery Action = "query_episodes"
	ActionMemoryRefresh Action = "refresh_memory"
	ActionMemoryPersist Action = "reflect_and_persist"
	ActionOnboardedMark Action = "mark_onboarded"

	// --- a colleague's ordinary work -------------------------------- //
	ActionColleagueAsk Action = "a2a_ask"
	ActionWorkCreate   Action = "create_work_item"
	ActionWorkUpdate   Action = "update_work_item"
	ActionWorkComment  Action = "comment_on_work_item"
	ActionWorkMerge    Action = "merge_work_item"
	ActionPageCreate   Action = "write_page"
	ActionPageSave     Action = "save_page"
	ActionPageComment  Action = "comment_on_page"

	// --- somebody's own record -------------------------------------- //
	ActionPersonRead    Action = "get_person"
	ActionMyWork        Action = "my_work"
	ActionInboxRead     Action = "work_inbox"
	ActionInboxMark     Action = "mark_inbox"
	ActionPinsSet       Action = "set_pins"
	ActionViewSave      Action = "save_work_view"
	ActionPrioritiesSet Action = "set_priorities"

	// --- a container's own policy ----------------------------------- //
	ActionProjectWrite   Action = "write_project"
	ActionCatalogueWrite Action = "write_work_catalogue"
	ActionPageRename     Action = "pages.rename"
	ActionContainerWrite Action = "pages.container"

	// --- taking something out of circulation ------------------------ //
	ActionWorkRemove  Action = "remove_work_item"
	ActionWorkRestore Action = "restore_work_item"
	ActionPageTrash   Action = "pages.trash"
	ActionPageRestore Action = "pages.restore"

	// --- beyond recovery -------------------------------------------- //
	ActionWorkPurge Action = "work.purge"
	ActionPagePurge Action = "pages.purge"

	// --- what somebody wrote ---------------------------------------- //
	ActionPageCommentEdit   Action = "pages.comment.edit"
	ActionPageCommentRemove Action = "pages.comment.remove"

	// --- the org chart ---------------------------------------------- //
	//
	// THE SPLIT IS THE DOMAIN'S OWN, not a taxonomy invented here. Half of
	// every chart object is OPAQUE to the chart domain and travels on the
	// row's `document` — a seat's model chain, its credentials, its sandbox
	// cell, its worker grants and schedules, an mcp_env block — because
	// internal/chart can say what a unit key and a parent mean and cannot
	// say what an `mcp_env` key is for. That half is equivalent to shell on
	// every engine host: a stdio MCP server is exec.Command with the
	// config's command. So it is decided as an operator surface, and the
	// public half — a name, a goal, who somebody manages — is decided by
	// whoever leads the unit.
	ActionChartRead        Action = "chart.read"
	ActionChartReadRuntime Action = "chart.runtime.read"
	ActionChartContent     Action = "chart.content.write"
	ActionChartRuntime     Action = "chart.runtime.write"
	ActionChartStructure   Action = "chart.structure.write"
	ActionChartRename      Action = "chart.rename"
	ActionChartImport      Action = "chart.import"

	// --- the company's own controls --------------------------------- //
	ActionConfigRead     Action = "config.read"
	ActionConfigWrite    Action = "config.write"
	ActionSecretList     Action = "secrets.list"
	ActionSecretReveal   Action = "secrets.reveal"
	ActionSecretWrite    Action = "secrets.write"
	ActionSetupRead      Action = "setup.read"
	ActionSetupConnect   Action = "setup.connect"
	ActionFleetOperate   Action = "fleet.operate"
	ActionTranscriptRead Action = "transcripts.read"
	ActionSandboxRun     Action = "run_sandbox"
)

// rule is one verb's row: which class decides it, and — for [ClassOperator]
// alone — which capability that class asks for.
//
// ONE TABLE RATHER THAN TWO. A class map beside a grant map is two things
// that must agree about which verbs are operator surfaces, and the day they
// disagree one verb is decided by a class that reads a grant nobody set.
type rule struct {
	class Class
	// grant is what [ClassOperator] requires. Empty on every other class,
	// which decides from the principal's relation to the object instead —
	// and empty on an operator row is a gate somebody forgot to fill in,
	// which [granted] refuses.
	grant iam.Grant
}

// rules is THE authority table: every verb this engine knows, and how it is
// decided.
//
// A VERB MISSING FROM HERE IS REFUSED, and the completeness walk in this
// package's tests is what turns that into a build failure rather than a
// production one. The alternative — a default class — is exactly how a new
// verb ships ungated, and it ships looking correct.
var rules = map[Action]rule{
	ActionWorkRead:      {class: ClassRead},
	ActionWorkList:      {class: ClassRead},
	ActionWorkSearch:    {class: ClassRead},
	ActionWorkActivity:  {class: ClassRead},
	ActionProjectRead:   {class: ClassRead},
	ActionProjectList:   {class: ClassRead},
	ActionCatalogueRead: {class: ClassRead},
	ActionViewList:      {class: ClassRead},
	ActionPageRead:      {class: ClassRead},
	ActionPageList:      {class: ClassRead},
	ActionKnowledgeRead: {class: ClassRead},
	ActionColleagueRead: {class: ClassRead},

	ActionSkillUse:      {class: ClassSelf},
	ActionSkillLoad:     {class: ClassSelf},
	ActionSkillRefine:   {class: ClassSelf},
	ActionEpisodesQuery: {class: ClassSelf},
	ActionMemoryRefresh: {class: ClassSelf},
	ActionMemoryPersist: {class: ClassSelf},
	ActionOnboardedMark: {class: ClassSelf},

	// AN ASK IS A COLLEAGUE WRITE, and the object is the colleague. It
	// writes no row, which is why it looks like it belongs nowhere — but
	// it spends somebody else's TURN and somebody else's budget, so a
	// credential with no write capability at all must not be able to make
	// every seat in the company think.
	ActionColleagueAsk: {class: ClassColleagueWrite},

	ActionWorkCreate:  {class: ClassColleagueWrite},
	ActionWorkUpdate:  {class: ClassColleagueWrite},
	ActionWorkComment: {class: ClassColleagueWrite},
	ActionWorkMerge:   {class: ClassColleagueWrite},
	ActionPageCreate:  {class: ClassColleagueWrite},
	ActionPageSave:    {class: ClassColleagueWrite},
	ActionPageComment: {class: ClassColleagueWrite},

	// READING SOMEBODY'S DAY IS THE LEAD RELATION, and MARKING it is not.
	// A lead re-orders what their report works on and says what is
	// important; marking somebody's mail read and rearranging their
	// pinned views is a gesture nobody asked a lead to make.
	ActionPersonRead:    {class: ClassOwnOrLead},
	ActionMyWork:        {class: ClassOwnOrLead},
	ActionInboxRead:     {class: ClassOwnOrLead},
	ActionPrioritiesSet: {class: ClassOwnOrLead},
	ActionInboxMark:     {class: ClassOwnRecord},
	ActionPinsSet:       {class: ClassOwnRecord},
	ActionViewSave:      {class: ClassOwnRecord},

	ActionProjectWrite:   {class: ClassContainer},
	ActionCatalogueWrite: {class: ClassContainer},
	ActionPageRename:     {class: ClassContainer},
	ActionContainerWrite: {class: ClassContainer},

	ActionWorkRemove:  {class: ClassDestructive},
	ActionWorkRestore: {class: ClassDestructive},
	ActionPageTrash:   {class: ClassDestructive},
	ActionPageRestore: {class: ClassDestructive},

	ActionWorkPurge: {class: ClassPurge},
	ActionPagePurge: {class: ClassPurge},

	ActionPageCommentEdit:   {class: ClassAuthored},
	ActionPageCommentRemove: {class: ClassAuthored},

	ActionChartRead: {class: ClassRead},
	// THE RUNTIME HALF IS THE COMPANY DOCUMENT by another name — the same
	// credentials, the same MCP commands, the same shape of every secret
	// the company holds — so it takes the grant that reads that document
	// rather than the one that reads the board.
	ActionChartReadRuntime: {class: ClassOperator, grant: iam.GrantConfigRead},
	// A CONTENT EDIT IS THE UNIT'S LEAD'S, which is what makes the chart
	// writable by somebody other than whoever holds the deployment: a lead
	// renaming their own team, restating its purpose or correcting a seat's
	// goal is not a configuration change.
	ActionChartContent: {class: ClassContainer},
	// STRUCTURE IS THE COMPANY'S. The domain serialises every structural
	// record on ONE subject for the whole chart, deliberately, because two
	// reparents through a common ancestor can each be locally valid and
	// jointly produce a cycle — so a move is never a fact about one unit
	// and is not one lead's to make.
	ActionChartStructure: {class: ClassOperator, grant: iam.GrantConfigWrite},
	// A RENAME IS THE COMPANY'S although the domain arbitrates it per
	// object rather than on the structure's one subject. An address is how
	// every other domain refers to a thing — a `manages:` entry, a lead, a
	// channel binding, the account name a vendor holds — so reassigning one
	// inside a namespace the whole company shares is not a fact about one
	// team, whatever subject it contends on.
	ActionChartRename:  {class: ClassOperator, grant: iam.GrantConfigWrite},
	ActionChartRuntime: {class: ClassOperator, grant: iam.GrantConfigWrite},
	ActionChartImport:  {class: ClassOperator, grant: iam.GrantConfigWrite},

	ActionConfigRead:     {class: ClassOperator, grant: iam.GrantConfigRead},
	ActionConfigWrite:    {class: ClassOperator, grant: iam.GrantConfigWrite},
	ActionSecretList:     {class: ClassOperator, grant: iam.GrantConfigRead},
	ActionSecretReveal:   {class: ClassOperator, grant: iam.GrantSecretRead},
	ActionSecretWrite:    {class: ClassOperator, grant: iam.GrantSecretWrite},
	ActionSetupRead:      {class: ClassOperator, grant: iam.GrantConfigRead},
	ActionSetupConnect:   {class: ClassOperator, grant: iam.GrantConfigWrite},
	ActionFleetOperate:   {class: ClassOperator, grant: iam.GrantFleetOperate},
	ActionTranscriptRead: {class: ClassOperator, grant: iam.GrantTranscriptRead},
	ActionSandboxRun:     {class: ClassOperator, grant: iam.GrantSandboxRun},
}

// Actions is every verb this build authorizes, sorted, for the walks that ask
// whether a surface covered all of them.
func Actions() []Action { return slices.Sorted(maps.Keys(rules)) }

// GrantOf reports the capability a verb requires, and whether the table knows
// the verb at all.
//
// EMPTY WITH ok IS A REAL ANSWER: every class but [ClassOperator] decides
// from the principal's relation to the object and asks for no capability. The
// pair is what lets a walk tell "this verb needs nothing" from "nobody has
// written a rule for this verb", which are the two answers a single empty
// string folds together.
//
// EXPORTED FOR THE WALKS, on [ClassOf]'s own terms — a caller that read this
// and decided for itself would be the second implementation this package
// exists to remove. Use [Decide].
func GrantOf(a Action) (iam.Grant, bool) {
	r, ok := rules[a]
	return r.grant, ok
}

// ClassOf reports which rule governs a verb.
//
// EXPORTED FOR THE WALKS rather than for a caller to branch on: a surface that
// read the class and decided for itself would be the second implementation
// this package exists to remove. Use [Decide].
func ClassOf(a Action) (Class, bool) {
	r, ok := rules[a]
	return r.class, ok
}

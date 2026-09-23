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

	// --- a field inside a write, decided against what was read ------ //
	//
	// DOTTED, so they are not tool names: a verb this build serves as a
	// tool carries the tool's own name, and these are asked FROM inside
	// one. They exist because the object is not in the arguments — which
	// project a task is filed under, and which project a label is being
	// declared in, come out of the stored row — so the registration gate
	// cannot form the object and the tool asks once it has read.
	ActionWorkRoute      Action = "work.route"
	ActionProjectPolicy  Action = "work.project.policy"
	ActionProjectArchive Action = "work.project.archive"

	// --- a gesture no tool makes ------------------------------------ //
	//
	// DOTTED for the same reason, and it is the HTTP write surface's
	// alone: a rank move places one card between two neighbours on a
	// board a PERSON is looking at, and no seat reorders a board it
	// cannot see.
	ActionWorkRank Action = "work.rank"

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
	ActionWorkCommentEdit   Action = "work.comment.edit"

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
	ActionConfigRead   Action = "config.read"
	ActionConfigWrite  Action = "config.write"
	ActionSecretList   Action = "secrets.list"
	ActionSecretReveal Action = "secrets.reveal"
	ActionSecretWrite  Action = "secrets.write"
	ActionSetupRead    Action = "setup.read"
	ActionSetupConnect Action = "setup.connect"
	ActionFleetOperate Action = "fleet.operate"
	ActionAuditRead    Action = "audit.read"
	ActionSandboxRun   Action = "run_sandbox"

	// --- the identity directory -------------------------------------- //
	//
	// FOUR VERBS FOR SIXTEEN ROUTES, because the routes differ in what
	// they do and not in how they are decided. What separates them is the
	// pair of questions this estate actually asks: is this about a person
	// or about the directory, and does it CHANGE anything.
	ActionDirectoryRead   Action = "iam.read"
	ActionDirectoryWrite  Action = "iam.write"
	ActionCredentialWrite Action = "iam.credential.write"
	ActionSessionEnd      Action = "iam.session.end"
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

	// humanOnly marks a verb NO AGENT may take, whatever its class would
	// allow — purging a task beyond recovery, taking a project out of
	// circulation. It is a fact about the VERB rather than about anybody's
	// relation to an object, which is why it is a field beside the class
	// and not a class of its own: the same fact composes with the
	// container relation and with the admin grant, and written as two
	// classes it was already two spellings of one rule.
	//
	// CHECKED BEFORE THE CLASS, so an agent holding the grant is told it
	// is a seat rather than that it lacks a capability it plainly has.
	humanOnly bool
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
	// A RANK MOVE IS A COLLEAGUE WRITE ON THE TASK, although the record
	// arbitrates on the project's ORDER rather than on the task: the order
	// is an object nobody owns, so the only relation to ask about is none,
	// and the capability that files work is the one that arranges it.
	ActionWorkRank:    {class: ClassColleagueWrite},
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

	// A VIEW IS NOT A RECORD, because half of them are not personal at
	// all: omitting the owner SHARES it, which is a tab on somebody's
	// project, unit or person page. See [ClassSavedView].
	ActionViewSave: {class: ClassSavedView},

	ActionPageRename: {class: ClassContainer},

	// `write_project` IS TWO GESTURES AND ONLY ONE IS THE LEAD'S, which
	// is what internal/tracker's own tag rule has always said: adding a
	// label is open to every colleague, and renaming or archiving one
	// takes the word off everybody's board. So the TOOL takes the
	// colleague write and the lead-only facets — a rename, an archive,
	// any policy edit — ask [ActionProjectPolicy] from inside it. Gating
	// the whole tool as the container's refused a colleague the one facet
	// the domain grants them.
	ActionProjectWrite:  {class: ClassColleagueWrite},
	ActionProjectPolicy: {class: ClassContainer},

	// AND RE-ROUTING IS THE LEAD'S for the same reason, asked from inside
	// `update_work_item`: it decides which team hears about the work, and
	// which project the task is filed under comes out of the stored row
	// rather than the arguments.
	ActionWorkRoute:      {class: ClassContainer},
	ActionContainerWrite: {class: ClassContainer},

	// THE CATALOGUE IS THE COMPANY'S, NOT A CONTAINER'S. Task types and
	// custom fields are declared once for the whole workspace — the tool
	// names no project and could not, so a container class asked about an
	// empty key and refused everybody but the admin. It is configuration,
	// and it takes the grant that edits the company's own document.
	ActionCatalogueWrite: {class: ClassOperator, grant: iam.GrantConfigWrite},

	ActionWorkRemove:  {class: ClassDestructive},
	ActionWorkRestore: {class: ClassDestructive},
	ActionPageTrash:   {class: ClassDestructive},
	ActionPageRestore: {class: ClassDestructive},

	ActionWorkPurge: {class: ClassOperator, grant: iam.GrantFleetOperate,
		humanOnly: true},
	ActionPagePurge: {class: ClassOperator, grant: iam.GrantFleetOperate,
		humanOnly: true},

	// ARCHIVING A PROJECT TAKES IT OUT OF CIRCULATION for everybody, so
	// it is the lead's like the rest of the policy AND it is never an
	// agent's: a project nobody can file into again is a company decision.
	// internal/tracker has always said so — it asked for a principal that
	// was not a seat — and this is that rule where every other one lives.
	ActionProjectArchive: {class: ClassContainer, humanOnly: true},

	ActionPageCommentEdit:   {class: ClassAuthored},
	ActionPageCommentRemove: {class: ClassAuthored},
	// A WORK ITEM'S REMARK IS AUTHORED THE SAME WAY a page's is, and the
	// class admits the deployment grant beside the author for the same
	// reason — but the WRITER refuses anybody but the author, exactly as
	// internal/pages does, because an edit puts words in somebody's mouth
	// where a removal only takes them down. The class answers "may you
	// touch this remark"; the writer answers "may you rewrite it".
	ActionWorkCommentEdit: {class: ClassAuthored},

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
	ActionChartContent: {class: ClassChartObject},
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

	ActionConfigRead:   {class: ClassOperator, grant: iam.GrantConfigRead},
	ActionConfigWrite:  {class: ClassOperator, grant: iam.GrantConfigWrite},
	ActionSecretList:   {class: ClassOperator, grant: iam.GrantConfigRead},
	ActionSecretReveal: {class: ClassOperator, grant: iam.GrantSecretRead},
	ActionSecretWrite:  {class: ClassOperator, grant: iam.GrantSecretWrite},
	ActionSetupRead:    {class: ClassOperator, grant: iam.GrantConfigRead},
	ActionSetupConnect: {class: ClassOperator, grant: iam.GrantConfigWrite},
	ActionFleetOperate: {class: ClassOperator, grant: iam.GrantFleetOperate},
	ActionAuditRead:    {class: ClassOperator, grant: iam.GrantAuditRead},
	ActionSandboxRun:   {class: ClassOperator, grant: iam.GrantSandboxRun},

	// A DIRECTORY READ IS THREE PARTIES' — the person it is about, the
	// party that writes it, and the party that audits it. The class holds
	// all three; see its own doc for why an auditor is in and why the
	// object is an id.
	ActionDirectoryRead: {class: ClassDirectoryRead},
	// AND A DIRECTORY WRITE IS ONE PARTY'S, with NO self path at all.
	// Editing your own grants is the escalation this estate exists to
	// close, so "it is my own row" must not be a way in — the record layer
	// refuses conferring what the caller does not hold, and this refuses
	// the gesture before it gets there.
	ActionDirectoryWrite: {class: ClassOperator, grant: iam.GrantPeopleManage},
	// MINTING AND REVOKING A CREDENTIAL, and ENDING SESSIONS, are the two
	// gestures a person legitimately makes about themselves — a personal
	// access token, signing out everywhere — so they carry the self path
	// the write above refuses.
	ActionCredentialWrite: {class: ClassDirectorySelf},
	ActionSessionEnd:      {class: ClassDirectorySelf},
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

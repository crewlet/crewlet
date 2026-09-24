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

	// --- a page every seat is handed ---------------------------------- //
	//
	// DOTTED because no tool is named for it: it is asked from inside
	// every page write — create, save, rename, trash, restore and purge —
	// once the container the page lives in is known, and the container is
	// the tool-skills one.
	ActionSkillPageWrite Action = "pages.skill.write"

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
	// ActionChartImportRead is the import LEDGER read — which revisions'
	// structure landed, and where. The importer's own grant, as it always
	// was, under a verb of its own because it asks for no step-up: a client
	// polling its import is reading.
	ActionChartImportRead Action = "chart.import.read"

	// --- the company's own controls --------------------------------- //
	ActionConfigRead   Action = "config.read"
	ActionConfigWrite  Action = "config.write"
	ActionSecretList   Action = "secrets.list"
	ActionSecretReveal Action = "secrets.reveal"
	ActionSecretWrite  Action = "secrets.write"
	ActionSetupRead    Action = "setup.read"
	ActionSetupConnect Action = "setup.connect"
	ActionFleetOperate Action = "fleet.operate"
	// ActionFleetRead is the deployment's own controls READ — the
	// maintenance window's status and the value a reanchor must echo. The
	// same grant as [ActionFleetOperate] and its own verb because it asks
	// for no step-up: a read changes nothing, and a status an operator
	// cannot see without re-proving who they are is a status they stop
	// checking.
	ActionFleetRead  Action = "fleet.read"
	ActionAuditRead  Action = "audit.read"
	ActionSandboxRun Action = "run_sandbox"

	// --- one seat's own trail, read by somebody else ----------------- //
	//
	// A SEAT'S MEMORY AND ITS THREADS, read over the API: what it wrote in
	// its diary, what it learned, who it has worked with, and what it said
	// on a chat surface this engine does not own. DOTTED, because no tool
	// reads another seat's trail — a seat's own memory tools take no
	// handle at all ([ClassSelf]) — and it is ITS OWN VERB rather than
	// [ActionAuditRead] because the object is a seat: a question naming
	// one asks this, and the identity estate's trail asks the other.
	ActionSeatTrailRead Action = "audit.seat.read"

	// --- the identity directory -------------------------------------- //
	//
	// SIX VERBS FOR SIXTEEN ROUTES, because the routes differ in what
	// they do and not in how they are decided. What separates them is the
	// three questions this estate actually asks: is this about a person or
	// about the directory, does it CHANGE anything, and — for a change —
	// does it hand over an authority or a way to prove who somebody is,
	// which is what the sensitive step-up window exists for.
	ActionDirectoryRead  Action = "iam.read"
	ActionDirectoryWrite Action = "iam.write"
	// ActionDirectoryAuthority changes what somebody ALREADY ENROLLED may
	// do or how they prove who they are: an edit of their row (grants,
	// stage, seat, login, reach, provider link) and a second-factor reset.
	// Its own verb beside [ActionDirectoryWrite] because it asks for the
	// SENSITIVE window where every other directory write asks the ordinary
	// one.
	ActionDirectoryAuthority Action = "iam.authority.write"
	ActionCredentialWrite    Action = "iam.credential.write"
	// ActionCredentialProof changes how ONE PERSON PROVES WHO THEY ARE:
	// enrolling or replacing a second factor, regenerating the recovery
	// codes, and revoking a password, a second factor, the recovery codes
	// or a provider link. It is asked from both surfaces that make those
	// changes — `/auth` for a person's own, `/iam` for anybody's — so one
	// row decides them whichever route a request took.
	ActionCredentialProof Action = "iam.credential.proof"
	ActionSessionEnd      Action = "iam.session.end"

	// ActionSessionInvalidate ends EVERY session in the company at once —
	// the restore runbook's last step. The deployment's own grant, like
	// [ActionFleetOperate], and a verb of its own because it asks for the
	// SENSITIVE window where the deployment's other controls ask for the
	// ordinary one: it is the one gesture here that cannot be taken back for
	// anybody, and it signs out the whole company including the person
	// making it.
	ActionSessionInvalidate Action = "iam.invalidate"
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

	// recency is how recently the principal must have PROVED who they are
	// for this verb — the step-up requirement, decided on the row so a
	// REST route, a socket frame and a tool asking about one verb cannot
	// disagree about it.
	//
	// EVERY ROW STATES ONE and the zero is refused by a walk in this
	// package's tests, because the zero would read as [iam.RecencyAny]: a
	// sensitive verb somebody added without deciding would ship open to a
	// session proved last week, and look exactly like a verb that was
	// decided to need nothing.
	//
	// CHECKED AFTER THE CLASS ALLOWS, never before. A principal refused
	// the verb outright is told what they lack; telling them to prove who
	// they are first would send them through a step-up only to be refused
	// by the rule it was never going to change.
	recency iam.Recency

	// selfRecency is the proof the SELF arm asks for — the person the
	// object IS, admitted as themselves ([ReasonSelf]) — where that differs
	// from what every other arm of the row asks.
	//
	// IT EXISTS BECAUSE ONE GESTURE HAS TWO RISKS depending on who makes
	// it. Ending every session somebody holds is the first thing they do
	// on finding an intruder in their account, and making them re-prove a
	// password the intruder may also hold turns the fastest response to a
	// compromise into the slowest — while the SAME verb made by an
	// administrator ends another person's sessions and every machine token
	// they hold, and a loop over the directory does it to the whole
	// company. One recency on the row could only be right for one of them.
	// ON THE ROW rather than in the handler, so a REST route and a socket
	// question about the verb still get one answer.
	//
	// ITS ZERO MEANS "WHAT THE ROW ASKS", which is the safe direction for
	// an unset value: a row nobody gave a second window to asks its one
	// window of everybody. A walk holds a set one to naming a real window
	// on a class that has a self arm at all.
	selfRecency iam.Recency
}

// rules is THE authority table: every verb this engine knows, and how it is
// decided.
//
// A VERB MISSING FROM HERE IS REFUSED, and the completeness walk in this
// package's tests is what turns that into a build failure rather than a
// production one. The alternative — a default class — is exactly how a new
// verb ships ungated, and it ships looking correct.
var rules = map[Action]rule{
	ActionWorkRead:      {class: ClassRead, recency: iam.RecencyAny},
	ActionWorkList:      {class: ClassRead, recency: iam.RecencyAny},
	ActionWorkSearch:    {class: ClassRead, recency: iam.RecencyAny},
	ActionWorkActivity:  {class: ClassRead, recency: iam.RecencyAny},
	ActionProjectRead:   {class: ClassRead, recency: iam.RecencyAny},
	ActionProjectList:   {class: ClassRead, recency: iam.RecencyAny},
	ActionCatalogueRead: {class: ClassRead, recency: iam.RecencyAny},
	ActionViewList:      {class: ClassRead, recency: iam.RecencyAny},
	ActionPageRead:      {class: ClassRead, recency: iam.RecencyAny},
	ActionPageList:      {class: ClassRead, recency: iam.RecencyAny},
	ActionKnowledgeRead: {class: ClassRead, recency: iam.RecencyAny},
	ActionColleagueRead: {class: ClassRead, recency: iam.RecencyAny},

	ActionSkillUse:      {class: ClassSelf, recency: iam.RecencyAny},
	ActionSkillLoad:     {class: ClassSelf, recency: iam.RecencyAny},
	ActionSkillRefine:   {class: ClassSelf, recency: iam.RecencyAny},
	ActionEpisodesQuery: {class: ClassSelf, recency: iam.RecencyAny},
	ActionMemoryRefresh: {class: ClassSelf, recency: iam.RecencyAny},
	ActionMemoryPersist: {class: ClassSelf, recency: iam.RecencyAny},
	ActionOnboardedMark: {class: ClassSelf, recency: iam.RecencyAny},

	// AN ASK IS A COLLEAGUE WRITE, and the object is the colleague. It
	// writes no row, which is why it looks like it belongs nowhere — but
	// it spends somebody else's TURN and somebody else's budget, so a
	// credential with no write capability at all must not be able to make
	// every seat in the company think.
	ActionColleagueAsk: {class: ClassColleagueWrite, recency: iam.RecencyAny},

	ActionWorkCreate:  {class: ClassColleagueWrite, recency: iam.RecencyAny},
	ActionWorkUpdate:  {class: ClassColleagueWrite, recency: iam.RecencyAny},
	ActionWorkComment: {class: ClassColleagueWrite, recency: iam.RecencyAny},
	ActionWorkMerge:   {class: ClassColleagueWrite, recency: iam.RecencyAny},
	// A RANK MOVE IS A COLLEAGUE WRITE ON THE TASK, although the record
	// arbitrates on the project's ORDER rather than on the task: the order
	// is an object nobody owns, so the only relation to ask about is none,
	// and the capability that files work is the one that arranges it.
	ActionWorkRank:    {class: ClassColleagueWrite, recency: iam.RecencyAny},
	ActionPageCreate:  {class: ClassColleagueWrite, recency: iam.RecencyAny},
	ActionPageSave:    {class: ClassColleagueWrite, recency: iam.RecencyAny},
	ActionPageComment: {class: ClassColleagueWrite, recency: iam.RecencyAny},

	// READING SOMEBODY'S DAY IS THE LEAD RELATION, and MARKING it is not.
	// A lead re-orders what their report works on and says what is
	// important; marking somebody's mail read and rearranging their
	// pinned views is a gesture nobody asked a lead to make.
	ActionPersonRead:    {class: ClassOwnOrLead, recency: iam.RecencyAny},
	ActionMyWork:        {class: ClassOwnOrLead, recency: iam.RecencyAny},
	ActionInboxRead:     {class: ClassOwnOrLead, recency: iam.RecencyAny},
	ActionPrioritiesSet: {class: ClassOwnOrLead, recency: iam.RecencyAny},
	ActionInboxMark:     {class: ClassOwnRecord, recency: iam.RecencyAny},
	ActionPinsSet:       {class: ClassOwnRecord, recency: iam.RecencyAny},

	// A VIEW IS NOT A RECORD, because half of them are not personal at
	// all: omitting the owner SHARES it, which is a tab on somebody's
	// project, unit or person page. See [ClassSavedView].
	ActionViewSave: {class: ClassSavedView, recency: iam.RecencyAny},

	ActionPageRename: {class: ClassContainer, recency: iam.RecencyAny},

	// `write_project` IS TWO GESTURES AND ONLY ONE IS THE LEAD'S, which
	// is what internal/tracker's own tag rule has always said: adding a
	// label is open to every colleague, and renaming or archiving one
	// takes the word off everybody's board. So the TOOL takes the
	// colleague write and the lead-only facets — a rename, an archive,
	// any policy edit — ask [ActionProjectPolicy] from inside it. Gating
	// the whole tool as the container's refused a colleague the one facet
	// the domain grants them.
	ActionProjectWrite:  {class: ClassColleagueWrite, recency: iam.RecencyAny},
	ActionProjectPolicy: {class: ClassContainer, recency: iam.RecencyAny},

	// AND RE-ROUTING IS THE LEAD'S for the same reason, asked from inside
	// `update_work_item`: it decides which team hears about the work, and
	// which project the task is filed under comes out of the stored row
	// rather than the arguments.
	ActionWorkRoute:      {class: ClassContainer, recency: iam.RecencyAny},
	ActionContainerWrite: {class: ClassContainer, recency: iam.RecencyAny},

	// A TOOL SKILL IS CONFIGURATION WRITTEN AS A PAGE. The skills container's
	// pages are injected into a phase of EVERY seat's turn as instructions,
	// so writing one rewrites the prompt the whole company runs under — the
	// same reach as editing the company document, and it takes the grant
	// that edits that document rather than the colleague write that
	// authors an ordinary page. `knowledge:write` alone must not be a way
	// to put words in every agent's mouth.
	//
	// AND NEVER AN AGENT'S, which internal/pages' store also refuses: a
	// seat writing the instructions every seat obeys is a model editing
	// the rules it is judged by. Marked here too, so a seat is told it is
	// a seat rather than that it lacks a grant.
	ActionSkillPageWrite: {class: ClassOperator, grant: iam.GrantConfigWrite,
		humanOnly: true, recency: iam.RecencyAny},

	// THE CATALOGUE IS THE COMPANY'S, NOT A CONTAINER'S. Task types and
	// custom fields are declared once for the whole workspace — the tool
	// names no project and could not, so a container class asked about an
	// empty key and refused everybody but the admin. It is configuration,
	// and it takes the grant that edits the company's own document.
	ActionCatalogueWrite: {class: ClassOperator, grant: iam.GrantConfigWrite, recency: iam.RecencyAny},

	ActionWorkRemove:  {class: ClassDestructive, recency: iam.RecencyAny},
	ActionWorkRestore: {class: ClassDestructive, recency: iam.RecencyAny},
	ActionPageTrash:   {class: ClassDestructive, recency: iam.RecencyAny},
	ActionPageRestore: {class: ClassDestructive, recency: iam.RecencyAny},

	ActionWorkPurge: {class: ClassOperator, grant: iam.GrantFleetOperate,
		humanOnly: true, recency: iam.RecencyAny},
	ActionPagePurge: {class: ClassOperator, grant: iam.GrantFleetOperate,
		humanOnly: true, recency: iam.RecencyAny},

	// ARCHIVING A PROJECT TAKES IT OUT OF CIRCULATION for everybody, so
	// it is the lead's like the rest of the policy AND it is never an
	// agent's: a project nobody can file into again is a company decision.
	// internal/tracker has always said so — it asked for a principal that
	// was not a seat — and this is that rule where every other one lives.
	ActionProjectArchive: {class: ClassContainer, humanOnly: true, recency: iam.RecencyAny},

	ActionPageCommentEdit:   {class: ClassAuthored, recency: iam.RecencyAny},
	ActionPageCommentRemove: {class: ClassAuthored, recency: iam.RecencyAny},
	// A WORK ITEM'S REMARK IS AUTHORED THE SAME WAY a page's is, and the
	// class admits the deployment grant beside the author for the same
	// reason — but the WRITER refuses anybody but the author, exactly as
	// internal/pages does, because an edit puts words in somebody's mouth
	// where a removal only takes them down. The class answers "may you
	// touch this remark"; the writer answers "may you rewrite it".
	ActionWorkCommentEdit: {class: ClassAuthored, recency: iam.RecencyAny},

	ActionChartRead: {class: ClassRead, recency: iam.RecencyAny},
	// THE RUNTIME HALF IS THE COMPANY DOCUMENT by another name — the same
	// credentials, the same MCP commands, the same shape of every secret
	// the company holds — so it takes the grant that reads that document
	// rather than the one that reads the board.
	ActionChartReadRuntime: {class: ClassOperator, grant: iam.GrantConfigRead, recency: iam.RecencyAny},
	// A CONTENT EDIT IS THE UNIT'S LEAD'S, which is what makes the chart
	// writable by somebody other than whoever holds the deployment: a lead
	// renaming their own team, restating its purpose or correcting a seat's
	// goal is not a configuration change.
	ActionChartContent: {class: ClassChartObject, recency: iam.RecencyStepUp},
	// STRUCTURE IS THE COMPANY'S. The domain serialises every structural
	// record on ONE subject for the whole chart, deliberately, because two
	// reparents through a common ancestor can each be locally valid and
	// jointly produce a cycle — so a move is never a fact about one unit
	// and is not one lead's to make.
	ActionChartStructure: {class: ClassOperator, grant: iam.GrantConfigWrite, recency: iam.RecencyStepUp},
	// A RENAME IS THE COMPANY'S although the domain arbitrates it per
	// object rather than on the structure's one subject. An address is how
	// every other domain refers to a thing — a `manages:` entry, a lead, a
	// channel binding, the account name a vendor holds — so reassigning one
	// inside a namespace the whole company shares is not a fact about one
	// team, whatever subject it contends on.
	ActionChartRename:  {class: ClassOperator, grant: iam.GrantConfigWrite, recency: iam.RecencyStepUp},
	ActionChartRuntime: {class: ClassOperator, grant: iam.GrantConfigWrite, recency: iam.RecencyStepUp},
	ActionChartImport:  {class: ClassOperator, grant: iam.GrantConfigWrite, recency: iam.RecencyStepUp},
	// THE LEDGER IS READ ON THE IMPORTER'S GRANT and asks for no proof:
	// polling whether an import landed changes nothing.
	ActionChartImportRead: {class: ClassOperator, grant: iam.GrantConfigWrite, recency: iam.RecencyAny},

	ActionConfigRead:   {class: ClassOperator, grant: iam.GrantConfigRead, recency: iam.RecencyAny},
	ActionConfigWrite:  {class: ClassOperator, grant: iam.GrantConfigWrite, recency: iam.RecencyStepUp},
	ActionSecretList:   {class: ClassOperator, grant: iam.GrantConfigRead, recency: iam.RecencyAny},
	ActionSecretReveal: {class: ClassOperator, grant: iam.GrantSecretRead, recency: iam.RecencySensitive},
	ActionSecretWrite:  {class: ClassOperator, grant: iam.GrantSecretWrite, recency: iam.RecencyStepUp},
	ActionSetupRead:    {class: ClassOperator, grant: iam.GrantConfigRead, recency: iam.RecencyAny},
	ActionSetupConnect: {class: ClassOperator, grant: iam.GrantConfigWrite, recency: iam.RecencyStepUp},
	ActionFleetOperate: {class: ClassOperator, grant: iam.GrantFleetOperate, recency: iam.RecencyStepUp},
	ActionFleetRead:    {class: ClassOperator, grant: iam.GrantFleetOperate, recency: iam.RecencyAny},
	ActionAuditRead:    {class: ClassOperator, grant: iam.GrantAuditRead, recency: iam.RecencyAny},
	ActionSandboxRun:   {class: ClassOperator, grant: iam.GrantSandboxRun, recency: iam.RecencyAny},

	// A SEAT'S TRAIL IS THE AUDIT READ, whoever's seat it is, and NOT the
	// owner-or-lead rule a person's queue takes. Its diary carries what its
	// prompts decided and its threads what it said on the company's behalf
	// — the record of what happened, which is exactly what `audit:read`
	// opens on `/events` for every seat at once. Decided by the lead
	// relation instead, an auditor holding that grant was refused one
	// seat's threads while reading every phase record the seat ever wrote;
	// and a lead holding only `state:read` would have read a trail the
	// grant that governs it was never given to them.
	ActionSeatTrailRead: {class: ClassOperator, grant: iam.GrantAuditRead, recency: iam.RecencyAny},

	// A DIRECTORY READ IS THREE PARTIES' — the person it is about, the
	// party that writes it, and the party that audits it. The class holds
	// all three; see its own doc for why an auditor is in and why the
	// object is an id.
	ActionDirectoryRead: {class: ClassDirectoryRead, recency: iam.RecencyAny},
	// AND A DIRECTORY WRITE IS ONE PARTY'S, with NO self path at all.
	// Editing your own grants is the escalation this estate exists to
	// close, so "it is my own row" must not be a way in — the record layer
	// refuses conferring what the caller does not hold, and this refuses
	// the gesture before it gets there.
	//
	// THE ORDINARY WINDOW for enrolling somebody, inviting them, removing
	// them and issuing the bootstrap code — the design's own: every
	// identity-directory write asks `step_up`, and the sensitive window is
	// kept for the gestures that hand over a value or change an authority
	// somebody already holds, below. Each of these is bounded another way:
	// an enrolment and an invitation confer only what their writer holds
	// (internal/iamdomain), a removal is announced and audited, and the
	// bootstrap code opens only an engine nobody is enrolled in. Asking the
	// sensitive window of all of them sent an administrator who proved half
	// an hour ago to re-prove for every invitation they sent.
	ActionDirectoryWrite: {class: ClassOperator, grant: iam.GrantPeopleManage, recency: iam.RecencyStepUp},
	// AND THE SENSITIVE WINDOW FOR CHANGING WHAT AN ENROLLED PERSON MAY DO
	// OR HOW THEY PROVE IT — one PATCH of their row, whatever fields it
	// carries, and a second-factor reset — which are the design's two
	// sensitive directory gestures: changing somebody's grants hands them
	// an authority, and resetting their second factor is how a phished
	// password becomes an account.
	ActionDirectoryAuthority: {class: ClassOperator, grant: iam.GrantPeopleManage,
		recency: iam.RecencySensitive},
	// MINTING AND REVOKING A MACHINE TOKEN, CHANGING HOW SOMEBODY PROVES
	// WHO THEY ARE, and ENDING SESSIONS are the gestures a person
	// legitimately makes about themselves — a personal access token, a
	// second factor, signing out everywhere — so they carry the self path
	// the writes above refuse.
	//
	// AND THEY ASK DIFFERENT PROOFS. A machine token's mint and revocation
	// ask the ordinary window, as every directory write does: what a token
	// carries is cut to its owner's own grants on every request and never
	// includes a grant that needs a person present, so a mint hands over
	// no authority the owner does not already exercise.
	ActionCredentialWrite: {class: ClassDirectorySelf, recency: iam.RecencyStepUp},
	// A CHANGE TO HOW SOMEBODY PROVES WHO THEY ARE asks the SENSITIVE
	// window, the second-factor reset's reason whichever route it takes:
	// replacing a factor or revoking one is a reset by another name, a
	// fresh set of recovery codes is a set of values that bypass the
	// factor and are shown once, and enrolling a first factor on a stolen
	// session is a hold on the account its owner cannot shake off.
	ActionCredentialProof: {class: ClassDirectorySelf, recency: iam.RecencySensitive},
	// ENDING SESSIONS ASKS OPPOSITE PROOFS OF ITS TWO ARMS ([rule.selfRecency]).
	// The person themselves asks for NONE: it is the first thing somebody
	// does on finding somebody else in their account — and it ends every
	// machine token they hold as well — and a gesture that makes them
	// re-prove a password the intruder may also hold makes the fastest
	// response to a compromise the slowest, which is the reason
	// internal/iamdomain's own revocation takes no grant at all. An
	// administrator ending SOMEBODY ELSE's asks the ordinary window, as
	// every other directory write does: it signs a colleague out of
	// everything and stops every pipeline they run, and a week-old cookie
	// looping over the directory would do it to the whole company.
	ActionSessionEnd: {class: ClassDirectorySelf, recency: iam.RecencyStepUp,
		selfRecency: iam.RecencyAny},
	// ENDING EVERY SESSION IN THE COMPANY is the deployment's grant — a
	// restore is run by whoever runs the deployment — at the SENSITIVE
	// window: it is irreversible for everybody at once. A Tier A token is
	// fresh by construction, so the restore runbook's CLI step still runs
	// on the day nobody can sign in.
	ActionSessionInvalidate: {class: ClassOperator, grant: iam.GrantFleetOperate, recency: iam.RecencySensitive},
}

// RecencyOf reports how recent a proof a verb asks for, and whether the table
// knows the verb at all.
//
// EXPORTED FOR THE WALKS and for a caller that has to SAY what a refusal it
// did not make would have needed, on [ClassOf]'s terms: a caller that read
// this and decided for itself would be the second implementation this
// package exists to remove. Use [Decide].
func RecencyOf(a Action) (iam.Recency, bool) {
	r, ok := rules[a]
	return r.recency, ok
}

// SelfRecencyOf reports the proof a verb asks of its SELF arm where that
// differs from [RecencyOf]'s, and empty where the row asks one window of
// everybody — see [rule.selfRecency].
//
// EXPORTED FOR THE WALKS, on [RecencyOf]'s terms. Use [Decide].
func SelfRecencyOf(a Action) (iam.Recency, bool) {
	r, ok := rules[a]
	return r.selfRecency, ok
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

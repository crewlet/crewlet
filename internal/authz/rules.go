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
	// A PROJECT'S FILES are its work's output — reports, specs, notes —
	// so reading one is reading the board, asked of the PROJECT the file
	// is kept in (the object is that project, by its key).
	ActionFileList Action = "list_project_files"
	ActionFileRead Action = "read_project_file"

	// --- the caller's own working state ----------------------------- //
	ActionSkillUse      Action = "use_skill"
	ActionSkillLoad     Action = "load_tool_skill"
	ActionSkillRefine   Action = "refine_skill"
	ActionEpisodesQuery Action = "query_episodes"
	ActionMemoryRefresh Action = "refresh_memory"
	ActionMemoryPersist Action = "reflect_and_persist"
	ActionOnboardedMark Action = "mark_onboarded"

	// --- a colleague's ordinary work -------------------------------- //
	//
	// AN ASK IS AN AGENT'S, and the only one in this block: a seat whose
	// role offers it puts a question to a colleague seat, spending that
	// seat's turn — see [rule.seatOnly].
	ActionColleagueAsk Action = "a2a_ask"
	ActionWorkCreate   Action = "create_work_item"
	ActionWorkUpdate   Action = "update_work_item"
	ActionWorkComment  Action = "comment_on_work_item"
	ActionWorkMerge    Action = "merge_work_item"
	ActionPageCreate   Action = "write_page"
	ActionPageSave     Action = "save_page"
	ActionPageComment  Action = "comment_on_page"
	// Keeping a file in a project is a colleague's ordinary work, as
	// filing an item in it is.
	ActionFileWrite Action = "write_project_file"

	// ActionWorkThread admits a request to post on a work item's THREAD
	// over HTTP, and decides nothing else: who may actually post — a
	// tracker holder, or the person an open ask on the item names, and only
	// their answer — is [ActionWorkComment]'s decision, which the tool makes
	// once it has read the item. DOTTED because it is no tool: a route has
	// to admit before the item is read, and admitted on the comment's own
	// verb a person without the app would be refused at the door to the one
	// thread they were asked to answer on.
	ActionWorkThread Action = "work.thread"

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

	// --- a person's arrangement of a board --------------------------- //
	//
	// A PLACE MOVES ONE CARD between two neighbours, and into another
	// lane, on a board a PERSON is looking at — the operator catalogue's
	// tool and the HTTP rank route, which is an adapter over it. No seat
	// is given it: a seat moves work between lanes with update_work_item
	// and does not reorder a board it cannot see, which is why its row is
	// human-only as well as the tracker's.
	ActionWorkPlace Action = "place_work_item"

	// --- a container's own policy ----------------------------------- //
	ActionProjectWrite   Action = "write_project"
	ActionCatalogueWrite Action = "write_work_catalogue"
	ActionPageRename     Action = "pages.rename"

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
	// A MOVE TAKES AN ITEM OUT OF ITS PROJECT, which is the gesture a
	// removal is decided on: asked of the project the item is IN (read by
	// the tool from the stored row), because that project's lead is who
	// may take work out of it — a colleague may file work in a project and
	// may not carry it away again.
	ActionWorkMove    Action = "move_work_item"
	ActionPageTrash   Action = "pages.trash"
	ActionPageRestore Action = "pages.restore"
	// REMOVING A FILE TAKES IT OUT OF ITS PROJECT, and is decided as
	// removing an item is: by the project's lead, because a colleague may
	// add a report to a project and may not take somebody else's away.
	ActionFileRemove Action = "remove_project_file"

	// --- beyond recovery -------------------------------------------- //
	ActionWorkPurge Action = "work.purge"
	ActionPagePurge Action = "pages.purge"

	// --- what somebody wrote ---------------------------------------- //
	ActionPageCommentEdit   Action = "pages.comment.edit"
	ActionPageCommentRemove Action = "pages.comment.remove"
	ActionWorkCommentEdit   Action = "work.comment.edit"

	// --- the org chart ---------------------------------------------- //
	//
	// The org chart is the company document's `roles:` and `units:`, and it
	// is written through the same routes and the same revision as every
	// setting. TWO VERBS rather than one per field or per gesture, because
	// the question is the same for every change a write makes — is the
	// place it reaches inside a unit this principal leads — and the surface
	// asks it of each place the write reaches ([ClassSubtree]). What a lead
	// may not change is decided there too, by asking [ActionConfigWrite]: a
	// setting, and a credential anywhere in the chart.
	ActionOrgWrite Action = "config.org.write"
	ActionOrgRead  Action = "config.org.read"

	// --- whether a seat works, and what its turn is doing ------------ //
	//
	// A PERSON'S DECISION ABOUT A SEAT, and never a seat's: pausing one,
	// resuming one, sending a note to the turn it is running, answering
	// the question a coding run it launched parked on. Each is decided on
	// the SEAT the gesture is about, by the owner-or-lead class — the
	// seat's holder, whoever leads it, or admin — and
	// each is decided INSIDE its tool, because the seat is never simply
	// what the arguments state: a typed handle resolves through the org
	// (`@sre`), a turn names the seat only the node running it can say,
	// and a run names the seat its row recorded.
	ActionSeatPause  Action = "pause_seat"
	ActionSeatResume Action = "resume_seat"
	ActionTurnSteer  Action = "steer_turn"
	ActionRunAnswer  Action = "answer_run"

	// ActionKnowledgeAnswer is a person's question answered from what the
	// company has written down, by a model, at the company's expense. A
	// READ in what it discloses — the pages and the board, each searched
	// only for a caller who holds its app — and a person's alone, because
	// it runs on the auxiliary model of the seat the asker is bound to and
	// a seat has search_knowledge and a model of its own.
	ActionKnowledgeAnswer Action = "answer_knowledge"

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
	// maintenance window's status and the value a reanchor must echo.
	// Admin's as [ActionFleetOperate] is, and its own verb because it asks
	// for no step-up: a read changes nothing, and a status an operator
	// cannot see without re-proving who they are is a status they stop
	// checking.
	ActionFleetRead Action = "fleet.read"
	ActionAuditRead Action = "audit.read"
	// ActionSandboxRun starts a detached coding run: an agent's, and only
	// one whose role offers the tool — see [rule.seatOnly].
	ActionSandboxRun Action = "run_sandbox"

	// --- one seat's own trail, read by somebody else ----------------- //
	//
	// A SEAT'S MEMORY, ITS THREADS AND ITS TURNS, read over the API: what
	// it wrote in its diary, what it learned, who it has worked with, what
	// it said on a chat surface this engine does not own, and the prompts
	// and responses of its own turns. DOTTED, because no tool reads another
	// seat's trail — a seat's own memory tools take no handle at all
	// ([ClassSelf]) — and it is ITS OWN VERB rather than [ActionAuditRead]
	// because the object is ONE seat: a question naming one asks this, and
	// the company-wide trail asks the other.
	ActionSeatTrailRead Action = "audit.seat.read"

	// --- the identity directory -------------------------------------- //
	//
	// SEVEN VERBS FOR TWENTY ROUTES, because the routes differ in what
	// they do and not in how they are decided. What separates them is the
	// three questions this estate actually asks: is this about a person or
	// about the directory, does it CHANGE anything, and — for a change to
	// somebody's own credentials — may a machine token acting as them make
	// it ([Presence]).
	ActionDirectoryRead  Action = "iam.read"
	ActionDirectoryWrite Action = "iam.write"
	// ActionSeatsRead is everybody's read of WHO HOLDS EACH HUMAN SEAT — the
	// org chart's people, named. Admitted to everybody, because the chart is
	// everybody's overview and a seat whose holder nobody could see would
	// draw an org chart of empty boxes; WHAT each caller sees of a holder is
	// the route's to mask per row (a login and a stage are the holder's and
	// admin's, an open invitation admin's).
	ActionSeatsRead Action = "iam.seats.read"
	// ActionCredentialWrite mints or revokes ONE PERSON's MACHINE TOKEN — a
	// personal access token, or a service account's.
	ActionCredentialWrite Action = "iam.credential.write"
	// ActionCredentialProof changes how ONE PERSON PROVES WHO THEY ARE:
	// enrolling or replacing a second factor, regenerating the recovery
	// codes, and revoking a password, a second factor or the recovery
	// codes. It is asked from both surfaces that make those changes —
	// `/auth` for a person's own, `/iam` for anybody's — so one row decides
	// them whichever route a request took.
	//
	// ITS OWN VERB BESIDE [ActionCredentialWrite], at the same window,
	// because the two differ in who must be PRESENT: a token revoking a
	// token — itself included, from wherever somebody found it leaked — is
	// containment, while a token changing how its owner signs in is the
	// first half of taking the account. One verb for both left the table
	// admitting a token on every proof change and each route remembering
	// to refuse it.
	ActionCredentialProof Action = "iam.credential.proof"
	ActionSessionEnd      Action = "iam.session.end"

	// ActionSessionInvalidate ends EVERY session in the company at once —
	// the restore runbook's last step. A verb of its own beside the
	// deployment's other controls because it is the one gesture here that
	// cannot be taken back for anybody, and it signs out the whole company
	// including the person making it.
	ActionSessionInvalidate Action = "iam.invalidate"
)

// Presence is which arms of a verb need a PERSON present: refused to a request
// that presented a machine token ([iam.Principal.MachineToken]), whatever the
// token carries and however fresh its proof, because a token proves nobody is
// at a keyboard. See the package doc.
//
// THREE VALUES AND NOT A BOOL, because one verb needs a person on some arms
// and not others: ending sessions is a token's to do about its own owner — it
// ends the token too, and is what somebody who finds one leaked reaches for —
// and never about anybody else.
type Presence string

const (
	// PresenceOptional is the zero: no arm needs a person, so a machine
	// token acting as its owner takes the verb wherever the class admits
	// the owner. The zero is the open end, as [rule.humanOnly]'s is, and
	// held the same way — by walks that derive which rows must close it
	// from the table rather than from memory.
	PresenceOptional Presence = ""
	// PresenceRequired needs a person on every arm, the self arm included.
	PresenceRequired Presence = "required"
	// PresenceRequiredForOthers needs a person on every arm but the self
	// arm: about the caller's own record a token acts as its owner, about
	// anybody else it does not.
	PresenceRequiredForOthers Presence = "required_for_others"
)

// Presences are the three, in declaration order.
var Presences = []Presence{
	PresenceOptional, PresenceRequired, PresenceRequiredForOthers,
}

// Valid reports whether a presence is one this build knows.
func (p Presence) Valid() bool { return slices.Contains(Presences, p) }

// rule is one verb's row: which class decides it, which app it belongs to,
// and the facts about the verb itself that every class composes with.
//
// ONE TABLE RATHER THAN SEVERAL. A class map beside an app map beside a
// human-only list is several things that must agree about one verb, and the
// day they disagree one verb is decided by a class that reads a field nobody
// set.
type rule struct {
	class Class

	// app is the app this verb belongs to — [iam.GrantTracker],
	// [iam.GrantKnowledge] — or empty for a verb that belongs to none. It is
	// the requirement of [ClassApp] and [ClassAppOrAsked], and it is ANDED
	// onto every relation class ([withApp]): a project's lead who does not
	// use the tracker does not set its policy, and a comment's author who no
	// longer uses the knowledge base does not edit it there. An
	// administrator holds every app through [iam.Expand], so naming one
	// never narrows admin's path.
	//
	// A FIELD ON THE ROW AND NOT A PER-KIND LOOKUP, because the table is
	// what a REST route, a socket question, a tool and every walk read: an
	// app derived from the object's kind would answer one verb differently
	// on two objects, and a walk over the table would certify a row whose
	// app was decided somewhere else.
	app iam.Grant

	// humanOnly marks a verb NO AGENT may take, whatever its class would
	// allow — purging a task beyond recovery, taking a project out of
	// circulation, writing the chart. It is a fact about the VERB rather
	// than about anybody's relation to an object, which is why it is a
	// field beside the class and not a class of its own: the same fact
	// composes with the container relation and with admin, and written as
	// two classes it was already two spellings of one rule.
	//
	// CHECKED BEFORE THE CLASS, so an agent holding the permission is told
	// it is a seat rather than that it lacks a capability it plainly has.
	humanOnly bool

	// seatOnly is humanOnly's MIRROR: a verb ONLY AN AGENT SEAT may take —
	// putting a question to a colleague seat, starting a coding run — with
	// no permission asked at all. Which seats are offered it is the role's
	// tool registry's decision, which already says which seat runs which
	// tool; a person has no turn for a colleague's answer to land in and no
	// executor for a run to resume.
	//
	// IT REPLACED TWO PERMISSIONS. Starting a coding run was a permission
	// of its own that only agents ever held, and asking a colleague rode on
	// the permission that wrote work — so a role that did not use the
	// tracker could not ask anybody anything. Neither was a decision anybody
	// made about a PERSON, which is what a permission is for.
	//
	// CHECKED BEFORE THE CLASS, as humanOnly is, so a person holding admin
	// is told the verb is an agent's ([ReasonSeatOnly]) rather than sent to
	// ask for a permission that would change nothing. A walk refuses a row
	// that is both.
	seatOnly bool

	// seated marks a verb made AS A SEAT: what it writes is attributed to a
	// seat handle, or what it reads is answered for one, so a principal
	// acting as itself — a Tier A token, a service account the identity
	// directory binds to no seat, the development principal — cannot take
	// it, whatever it holds. A person always acts as their human seat
	// (ADR-0032) and an agent as its own, so neither is ever refused it.
	//
	// A FACT ABOUT THE VERB, beside humanOnly and seatOnly, and for their
	// reason: the class weighs permissions and relations, and no
	// permission gives a credential a seat to speak or be answered as.
	// Left to each tool, the refusal came after the table had already
	// admitted the call — so a person's surface asking the table what to
	// offer (internal/agent/builtin's Admits) offered every such verb to
	// an unbound credential, and every press of it was refused.
	//
	// CHECKED AFTER seatOnly and BEFORE THE CLASS: an unbound credential
	// holding admin is told it has no seat ([ReasonUnseated]) rather than
	// a grant it already has, and the refusal names no permission. A walk
	// refuses a row that is both seat-only and seated — a seat-only verb
	// is a seat's already, so the second mark would read as a decision
	// nobody made.
	seated bool

	// presence is which arms need a PERSON at the keyboard — see
	// [Presence] — refused to a request that presented a machine token.
	//
	// A FIELD ON THE ROW AND NOT A CHECK IN EACH ROUTE: the table is what
	// every surface and every walk reads, so a route that refused a token
	// itself was the one place the rule was stated, a walk over the table
	// certified a row that admitted the token, and the next route on that
	// verb could forget.
	//
	// CHECKED BEFORE THE CLASS where it covers every arm, as
	// [rule.humanOnly] is — a token told it lacks a permission is sent to
	// ask for something that would change nothing — and AFTER the class
	// where it spares the self arm, because only the class can say which
	// arm admitted.
	presence Presence

	// recency is how recently the principal must have PROVED who they are
	// for this verb — the step-up requirement, decided on the row so a
	// REST route, a socket frame and a tool asking about one verb cannot
	// disagree about it.
	//
	// EVERY ROW STATES ONE and the zero is refused by a walk in this
	// package's tests, because the zero would read as [iam.RecencyAny]: a
	// step-up verb somebody added without deciding would ship open to a
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
	// --- using the tracker ------------------------------------------- //
	//
	// THE APP IS THE WHOLE RULE: reading the board, a project, its files,
	// its views and its catalogue, and filing, updating, merging and
	// arranging work. A company that hands somebody the tracker is saying
	// exactly this, and a company that does not hands them the overview.
	//
	// READING ONE ITEM IS ALSO THE ASKED PERSON'S. Somebody a colleague put
	// a question to reads the item the question is on, whether or not they
	// use the tracker — see [ClassAppOrAsked].
	ActionWorkRead:      {class: ClassAppOrAsked, app: iam.GrantTracker, recency: iam.RecencyAny},
	ActionWorkList:      {class: ClassApp, app: iam.GrantTracker, recency: iam.RecencyAny},
	ActionWorkSearch:    {class: ClassApp, app: iam.GrantTracker, recency: iam.RecencyAny},
	ActionWorkActivity:  {class: ClassApp, app: iam.GrantTracker, recency: iam.RecencyAny},
	ActionProjectRead:   {class: ClassApp, app: iam.GrantTracker, recency: iam.RecencyAny},
	ActionProjectList:   {class: ClassApp, app: iam.GrantTracker, recency: iam.RecencyAny},
	ActionCatalogueRead: {class: ClassApp, app: iam.GrantTracker, recency: iam.RecencyAny},
	ActionViewList:      {class: ClassApp, app: iam.GrantTracker, recency: iam.RecencyAny},
	// A PROJECT'S FILES ARE ITS WORK'S OUTPUT, read and kept on the tracker
	// as its items are — never on the knowledge base a body of prose might
	// suggest.
	ActionFileList:  {class: ClassApp, app: iam.GrantTracker, recency: iam.RecencyAny},
	ActionFileRead:  {class: ClassApp, app: iam.GrantTracker, recency: iam.RecencyAny},
	ActionFileWrite: {class: ClassApp, app: iam.GrantTracker, recency: iam.RecencyAny},

	ActionWorkCreate: {class: ClassApp, app: iam.GrantTracker, recency: iam.RecencyAny},
	ActionWorkUpdate: {class: ClassApp, app: iam.GrantTracker, recency: iam.RecencyAny},
	// A COMMENT IS THE TRACKER'S, OR THE ANSWER THE ASKED PERSON OWES. The
	// asked arm admits a person an open ask names — and the tool then takes
	// only their answer, which the tracker's writer checks again is theirs.
	ActionWorkComment: {class: ClassAppOrAsked, app: iam.GrantTracker, recency: iam.RecencyAny},
	ActionWorkMerge:   {class: ClassApp, app: iam.GrantTracker, recency: iam.RecencyAny},
	// A PLACE IS THE APP ON THE TASK, although the record arbitrates on the
	// project's ORDER rather than on the task: the order is an object nobody
	// owns, so the only relation to ask about is none. A person's gesture —
	// see [ActionWorkPlace].
	ActionWorkPlace: {class: ClassApp, app: iam.GrantTracker, humanOnly: true,
		recency: iam.RecencyAny},
	// THE ROUTE TO AN ITEM'S THREAD ADMITS EVERYBODY and decides nothing:
	// [ActionWorkComment] decides once the item is read. See
	// [ActionWorkThread].
	ActionWorkThread: {class: ClassEverybody, recency: iam.RecencyAny},

	// `write_project` IS TWO GESTURES AND ONLY ONE IS THE LEAD'S, which is
	// what internal/tracker's own tag rule has always said: adding a label
	// is open to everybody who uses the tracker, and renaming or archiving
	// one takes the word off everybody's board. So the TOOL takes the app
	// and the lead-only facets — a rename, an archive, any policy edit —
	// ask [ActionProjectPolicy] from inside it.
	ActionProjectWrite:  {class: ClassApp, app: iam.GrantTracker, recency: iam.RecencyAny},
	ActionProjectPolicy: {class: ClassContainer, app: iam.GrantTracker, recency: iam.RecencyAny},
	// AND RE-ROUTING IS THE LEAD'S for the same reason, asked from inside
	// `update_work_item`: it decides which team hears about the work, and
	// which project the task is filed under comes out of the stored row
	// rather than the arguments.
	ActionWorkRoute: {class: ClassContainer, app: iam.GrantTracker, recency: iam.RecencyAny},
	// ARCHIVING A PROJECT TAKES IT OUT OF CIRCULATION for everybody, so it
	// is the lead's like the rest of the policy AND it is never an agent's:
	// a project nobody can file into again is a company decision.
	ActionProjectArchive: {class: ClassContainer, app: iam.GrantTracker, humanOnly: true,
		recency: iam.RecencyAny},

	// AN ITEM'S REPORTER TIDIES IT, beside its project's lead and admin:
	// whoever filed a duplicate takes it out again, and puts it back. A
	// subtree stays the lead's (the tool refuses an author it), and a purge
	// stays admin's. See [ClassTidy].
	ActionWorkRemove:  {class: ClassTidy, app: iam.GrantTracker, recency: iam.RecencyAny},
	ActionWorkRestore: {class: ClassTidy, app: iam.GrantTracker, recency: iam.RecencyAny},
	// A MOVE TAKES AN ITEM OUT OF ITS PROJECT, and a file's removal takes a
	// file out of one: the project's lead or admin, never every colleague
	// who may add to it.
	ActionWorkMove:   {class: ClassDestructive, app: iam.GrantTracker, recency: iam.RecencyAny},
	ActionFileRemove: {class: ClassDestructive, app: iam.GrantTracker, recency: iam.RecencyAny},

	// A WORK ITEM'S REMARK IS ITS AUTHOR'S, or admin's, and the tracker's:
	// the class answers "may you touch this remark", and the WRITER refuses
	// anybody but the author an edit, because an edit puts words in
	// somebody's mouth where a removal only takes them down.
	ActionWorkCommentEdit: {class: ClassAuthored, app: iam.GrantTracker, recency: iam.RecencyAny},

	// A VIEW IS NOT A RECORD, because half of them are not personal at
	// all: omitting the owner SHARES it, which is a tab on somebody's
	// project, unit or person page. See [ClassSavedView].
	ActionViewSave: {class: ClassSavedView, app: iam.GrantTracker, recency: iam.RecencyAny},

	// --- somebody's own record, on the tracker ------------------------ //
	//
	// READING SOMEBODY'S DAY IS THE LEAD RELATION, and MARKING it is not. A
	// lead re-orders what their report works on and says what is
	// important; marking somebody's mail read and rearranging their pinned
	// views is a gesture nobody asked a lead to make. Every one of them is
	// the TRACKER's — a person's notices name its tasks and their queue
	// lists them — so each takes the app beside the relation, and a person
	// who does not use the tracker has no queue there to keep.
	ActionPersonRead:    {class: ClassOwnOrLead, app: iam.GrantTracker, recency: iam.RecencyAny},
	ActionMyWork:        {class: ClassOwnOrLead, app: iam.GrantTracker, recency: iam.RecencyAny},
	ActionInboxRead:     {class: ClassOwnOrLead, app: iam.GrantTracker, recency: iam.RecencyAny},
	ActionPrioritiesSet: {class: ClassOwnOrLead, app: iam.GrantTracker, recency: iam.RecencyAny},
	ActionInboxMark:     {class: ClassOwnRecord, app: iam.GrantTracker, recency: iam.RecencyAny},
	ActionPinsSet:       {class: ClassOwnRecord, app: iam.GrantTracker, recency: iam.RecencyAny},

	// --- using the knowledge base ------------------------------------ //
	ActionPageRead:      {class: ClassApp, app: iam.GrantKnowledge, recency: iam.RecencyAny},
	ActionPageList:      {class: ClassApp, app: iam.GrantKnowledge, recency: iam.RecencyAny},
	ActionKnowledgeRead: {class: ClassApp, app: iam.GrantKnowledge, recency: iam.RecencyAny},
	// WHERE A PAGE MAY BE CREATED is internal/pages' store's to decide — a
	// space the company declares — for every author alike; this row decides
	// only WHO.
	ActionPageCreate:  {class: ClassApp, app: iam.GrantKnowledge, recency: iam.RecencyAny},
	ActionPageSave:    {class: ClassApp, app: iam.GrantKnowledge, recency: iam.RecencyAny},
	ActionPageComment: {class: ClassApp, app: iam.GrantKnowledge, recency: iam.RecencyAny},
	// A PAGE'S CREATOR TIDIES IT — renames, trashes, restores — beside its
	// container's lead and admin. See [ClassTidy].
	ActionPageRename:  {class: ClassTidy, app: iam.GrantKnowledge, recency: iam.RecencyAny},
	ActionPageTrash:   {class: ClassTidy, app: iam.GrantKnowledge, recency: iam.RecencyAny},
	ActionPageRestore: {class: ClassTidy, app: iam.GrantKnowledge, recency: iam.RecencyAny},
	// A REMARK IS ITS AUTHOR'S, as a work item's is; the store refuses
	// anybody but the author an edit, and takes a moderator's removal.
	ActionPageCommentEdit:   {class: ClassAuthored, app: iam.GrantKnowledge, recency: iam.RecencyAny},
	ActionPageCommentRemove: {class: ClassAuthored, app: iam.GrantKnowledge, recency: iam.RecencyAny},

	// A QUESTION ANSWERED FROM WHAT THE COMPANY WROTE DOWN draws on
	// whichever apps the asker uses: the tool searches the pages only for a
	// knowledge holder and the board only for a tracker holder, so either
	// app admits and a caller with neither has nothing it could search. And
	// a person's alone: it runs on the auxiliary model of the seat the
	// asker holds, and a seat has search_knowledge and a model of its own.
	// And SEATED: a principal no seat binds — a Tier A token's session or a
	// service account, never a person, who holds a human seat for as long
	// as they are here (ADR-0032) — has no seat whose model to answer on
	// and nobody the spend is charged for.
	ActionKnowledgeAnswer: {class: ClassAnyApp, humanOnly: true, seated: true,
		recency: iam.RecencyAny},

	// --- everybody's overview ---------------------------------------- //
	//
	// WHO TO ASK IS EVERYBODY'S: the colleague lookup answers from the org
	// chart, which everybody sees. The person-facing question masks the
	// external contact ids for anybody but admin; a seat's tool keeps them,
	// because an agent mentions people by them.
	ActionColleagueRead: {class: ClassEverybody, recency: iam.RecencyAny},
	// WHO HOLDS EACH HUMAN SEAT is the org chart's people, named — see
	// [ActionSeatsRead]. The route masks each holder per caller.
	ActionSeatsRead: {class: ClassEverybody, recency: iam.RecencyAny},

	// --- an agent's own ---------------------------------------------- //
	//
	// THE CALLER'S OWN WORKING STATE, no permission and no admin path.
	ActionSkillUse:      {class: ClassSelf, recency: iam.RecencyAny},
	ActionSkillLoad:     {class: ClassSelf, recency: iam.RecencyAny},
	ActionSkillRefine:   {class: ClassSelf, recency: iam.RecencyAny},
	ActionEpisodesQuery: {class: ClassSelf, recency: iam.RecencyAny},
	ActionMemoryRefresh: {class: ClassSelf, recency: iam.RecencyAny},
	ActionMemoryPersist: {class: ClassSelf, recency: iam.RecencyAny},
	ActionOnboardedMark: {class: ClassSelf, recency: iam.RecencyAny},
	// AN ASK AND A CODING RUN ARE AN AGENT'S, whichever apps it uses: an
	// ask spends a colleague seat's turn and a run hands a box the seat's
	// whole tool surface, and the ROLE decides which seats are offered
	// either. No person takes them — see [rule.seatOnly].
	ActionColleagueAsk: {class: ClassEverybody, seatOnly: true, recency: iam.RecencyAny},
	ActionSandboxRun:   {class: ClassEverybody, seatOnly: true, recency: iam.RecencyAny},

	// --- whether a seat works, and what its turn is doing ------------- //
	//
	// WHETHER A SEAT WORKS IS ITS LEAD'S TO DECIDE, or admin's, exactly as
	// what it works on is ([ActionPrioritiesSet]) — and never any reader's.
	// HUMAN-ONLY on top: a seat that could pause a colleague, resume
	// itself, steer another's turn or answer its own run's question would
	// be overruling the people who run it. No app: a lead who uses neither
	// app still runs their team.
	ActionSeatPause:  {class: ClassOwnOrLead, humanOnly: true, recency: iam.RecencyAny},
	ActionSeatResume: {class: ClassOwnOrLead, humanOnly: true, recency: iam.RecencyAny},
	ActionTurnSteer:  {class: ClassOwnOrLead, humanOnly: true, recency: iam.RecencyAny},
	// A RUN'S QUESTION IS ITS REQUESTER'S TO ANSWER, or the run's seat's
	// lead's: the tool asks this row about the REQUESTER's seat when the
	// caller is that requester (the self arm), and about the run's seat
	// otherwise — so the person who asked for the work answers what it
	// asks them, and nobody who merely leads them does.
	ActionRunAnswer: {class: ClassOwnOrLead, humanOnly: true, recency: iam.RecencyAny},

	// A SEAT'S TRAIL IS ITS LEAD'S TO READ, and admin's: its diary, its
	// episodes, its threads and its own turns' prompts and responses. A lead
	// steers, pauses and answers for their agents, and could do none of it
	// well without reading what those agents did; somebody who merely
	// shares the company does not read every prompt an agent was given.
	// The COMPANY-WIDE trail — every event, every turn, every channel, the
	// identity estate's audit — stays admin's ([ActionAuditRead]).
	ActionSeatTrailRead: {class: ClassOwnOrLead, recency: iam.RecencyAny},

	// --- running the company ------------------------------------------ //
	//
	// A TOOL SKILL IS CONFIGURATION WRITTEN AS A PAGE. The skills
	// container's pages are injected into a phase of EVERY seat's turn as
	// instructions, so writing one rewrites the prompt the whole company
	// runs under — the same reach as editing the company document, and it
	// takes admin rather than the knowledge app that authors an ordinary
	// page. AND NEVER AN AGENT'S, which internal/pages' store also refuses:
	// a seat writing the instructions every seat obeys is a model editing
	// the rules it is judged by.
	ActionSkillPageWrite: {class: ClassAdmin, humanOnly: true, recency: iam.RecencyAny},
	// THE CATALOGUE IS THE COMPANY'S, NOT A CONTAINER'S. Task types and
	// custom fields are declared once for the whole workspace — the tool
	// names no project and could not. It is configuration, and admin's.
	ActionCatalogueWrite: {class: ClassAdmin, recency: iam.RecencyAny},

	// --- beyond recovery --------------------------------------------- //
	//
	// A PURGE IS ADMIN'S, A PERSON'S, AND A RECENT PROOF'S. Nothing undoes
	// it, so it is the one tidy an author does not take for themselves —
	// trash and restore are theirs — and it asks the step-up window as
	// every other irreversible gesture does.
	ActionWorkPurge: {class: ClassAdmin, humanOnly: true, recency: iam.RecencyStepUp},
	ActionPagePurge: {class: ClassAdmin, humanOnly: true, recency: iam.RecencyStepUp},

	// --- the org chart ------------------------------------------------- //
	//
	// THE ORG CHART IS ITS LEADS' AS WELL AS ADMIN'S. A lead writes what
	// lies inside the units they lead — see [ClassSubtree]. HUMAN-ONLY and
	// at the step-up window, as every other write of the company document
	// is: a seat rewriting the org it runs inside is a model choosing its
	// own team.
	ActionOrgWrite: {class: ClassSubtree, humanOnly: true, recency: iam.RecencyStepUp},
	// AND A LEAD READS WHAT THEY MAY WRITE: one seat or one unit of their
	// subtree, masked as every read of the document is, without admin.
	ActionOrgRead: {class: ClassSubtree, recency: iam.RecencyAny},

	// --- the company's own controls ------------------------------------ //
	ActionConfigRead:  {class: ClassAdmin, recency: iam.RecencyAny},
	ActionConfigWrite: {class: ClassAdmin, recency: iam.RecencyStepUp},
	ActionSecretList:  {class: ClassAdmin, recency: iam.RecencyAny},
	// A REVEAL HANDS OVER A CREDENTIAL'S VALUE, which is a person's to read
	// and never a pipeline's: the row refuses any request a token presented.
	ActionSecretReveal: {class: ClassAdmin, recency: iam.RecencyStepUp,
		presence: PresenceRequired},
	ActionSecretWrite:  {class: ClassAdmin, recency: iam.RecencyStepUp},
	ActionSetupRead:    {class: ClassAdmin, recency: iam.RecencyAny},
	ActionSetupConnect: {class: ClassAdmin, recency: iam.RecencyStepUp},
	ActionFleetOperate: {class: ClassAdmin, recency: iam.RecencyStepUp},
	ActionFleetRead:    {class: ClassAdmin, recency: iam.RecencyAny},
	// THE COMPANY-WIDE TRAIL — every event, turn and phase, every agent-to-
	// agent channel, the identity estate's own audit. Admin's: one seat's
	// trail is its lead's too ([ActionSeatTrailRead]); everybody's is not.
	ActionAuditRead: {class: ClassAdmin, recency: iam.RecencyAny},

	// --- the identity directory ---------------------------------------- //
	//
	// A DIRECTORY READ IS THE PERSON IT IS ABOUT, OR ADMIN — see
	// [ClassDirectory] for why the object is an id.
	ActionDirectoryRead: {class: ClassDirectory, recency: iam.RecencyAny},
	// AND A DIRECTORY WRITE IS ADMIN'S, with NO self path at all. Editing
	// your own permissions is the escalation this estate exists to close,
	// so "it is my own row" must not be a way in — the record layer refuses
	// conferring what the caller does not hold, and this refuses the
	// gesture before it gets there.
	//
	// EVERY ONE OF THEM — enrolling somebody, inviting them, editing their
	// row, resetting their second factor, removing them — asks the step-up
	// window, and needs a person present: what keeps a week-old cookie away
	// is the window, and what keeps a stolen pipeline credential away is the
	// row's own refusal of a token and internal/iamdomain's refusal of an
	// administrative record written through one.
	ActionDirectoryWrite: {class: ClassAdmin, recency: iam.RecencyStepUp,
		presence: PresenceRequired},
	// MINTING AND REVOKING A MACHINE TOKEN, CHANGING HOW SOMEBODY PROVES
	// WHO THEY ARE, and ENDING SESSIONS are the gestures a person
	// legitimately makes about themselves — a personal access token, a
	// second factor, signing out everywhere — so they carry the self path
	// the writes above refuse.
	//
	// THE SELF PATH ADMITS A MACHINE TOKEN AS ITS OWNER — it acts as them
	// and is stepped up by construction — so WHO MUST BE PRESENT is what
	// tells these three apart ([rule.presence]). A token may revoke a token,
	// itself included, which is what somebody who finds one leaked reaches
	// for wherever they found it; it may never change how its owner proves
	// who they are; and about anybody else it may do none of them.
	ActionCredentialWrite: {class: ClassDirectory, recency: iam.RecencyStepUp,
		presence: PresenceRequiredForOthers},
	ActionCredentialProof: {class: ClassDirectory, recency: iam.RecencyStepUp,
		presence: PresenceRequired},
	// ENDING SESSIONS ASKS OPPOSITE PROOFS OF ITS TWO ARMS
	// ([rule.selfRecency]). The person themselves asks for NONE: it is the
	// first thing somebody does on finding somebody else in their account —
	// and it ends every machine token they hold as well — and a gesture
	// that makes them re-prove a password the intruder may also hold makes
	// the fastest response to a compromise the slowest, which is the reason
	// internal/iamdomain's own revocation takes no permission at all. An
	// administrator ending SOMEBODY ELSE's asks `step_up`, as every other
	// directory write does, and a person present: it signs a colleague out
	// of everything and stops every pipeline they run, and a week-old
	// cookie — or a token — looping over the directory would do it to the
	// whole company. The self arm stays open to a token for the
	// revocation's reason above: what it ends includes the token.
	ActionSessionEnd: {class: ClassDirectory, recency: iam.RecencyStepUp,
		selfRecency: iam.RecencyAny, presence: PresenceRequiredForOthers},
	// ENDING EVERY SESSION IN THE COMPANY is admin's, a person's, and a
	// recent proof's: it ends every person's authority at once, every
	// machine token included, and cannot be taken back. A Tier A token holds
	// admin, is its own credential rather than a machine token and is fresh
	// by construction, so the restore runbook's CLI step still runs on the
	// day nobody can sign in.
	ActionSessionInvalidate: {class: ClassAdmin, recency: iam.RecencyStepUp,
		presence: PresenceRequired},
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

// PresenceOf reports which arms of a verb need a person present, and whether
// the table knows the verb at all — see [Presence].
//
// EXPORTED FOR THE WALKS, on [RecencyOf]'s terms. Use [Decide].
func PresenceOf(a Action) (Presence, bool) {
	r, ok := rules[a]
	return r.presence, ok
}

// AppOf reports the app a verb belongs to, and whether the table knows the
// verb at all.
//
// EMPTY WITH ok IS A REAL ANSWER: a verb that belongs to no app — everybody's
// overview, an agent's own, running the company, the directory. The pair is
// what lets a walk tell "this verb belongs to no app" from "nobody has written
// a rule for this verb", which are the two answers a single empty string folds
// together.
//
// EXPORTED FOR THE WALKS, on [RecencyOf]'s terms — a caller that read this
// and decided for itself would be the second implementation this package
// exists to remove. Use [Decide].
func AppOf(a Action) (iam.Grant, bool) {
	r, ok := rules[a]
	return r.app, ok
}

// HumanOnlyOf reports whether NO AGENT may take a verb — see [rule.humanOnly]
// — and whether the table knows the verb at all.
//
// EXPORTED FOR THE WALKS, on [RecencyOf]'s terms. Use [Decide].
func HumanOnlyOf(a Action) (bool, bool) {
	r, ok := rules[a]
	return r.humanOnly, ok
}

// SeatOnlyOf reports whether ONLY AN AGENT SEAT may take a verb — see
// [rule.seatOnly] — and whether the table knows the verb at all.
//
// EXPORTED FOR THE WALKS, on [RecencyOf]'s terms. Use [Decide].
func SeatOnlyOf(a Action) (bool, bool) {
	r, ok := rules[a]
	return r.seatOnly, ok
}

// SeatedOf reports whether a verb is made AS A SEAT — see [rule.seated] — and
// whether the table knows the verb at all.
//
// EXPORTED FOR THE WALKS, on [RecencyOf]'s terms. Use [Decide].
func SeatedOf(a Action) (bool, bool) {
	r, ok := rules[a]
	return r.seated, ok
}

// Actions is every verb this build authorizes, sorted, for the walks that ask
// whether a surface covered all of them.
func Actions() []Action { return slices.Sorted(maps.Keys(rules)) }

// ClassOf reports which rule governs a verb.
//
// EXPORTED FOR THE WALKS rather than for a caller to branch on: a surface that
// read the class and decided for itself would be the second implementation
// this package exists to remove. Use [Decide].
func ClassOf(a Action) (Class, bool) {
	r, ok := rules[a]
	return r.class, ok
}

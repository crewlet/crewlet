package authz

import (
	"maps"
	"slices"

	"github.com/crewlet/crewlet/internal/artifacts"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/toolname"
)

// The verbs this engine authorizes.
//
// NAMED AFTER THE VERB, NOT THE SURFACE. `PATCH /worklet/items/{key}` and the
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
	ActionColleagueRead Action = toolname.LookupColleague
	// A PROJECT'S FILES are its work's output — reports, specs, notes —
	// so reading one is reading the board, asked of the PROJECT the file
	// is kept in (the object is that project, by its key).
	ActionFileList Action = "list_project_files"
	ActionFileRead Action = "read_project_file"

	// --- the caller's own working state ----------------------------- //
	ActionSkillUse      Action = "use_skill"
	ActionSkillLoad     Action = toolname.LoadToolSkill
	ActionSkillRefine   Action = "refine_skill"
	ActionEpisodesQuery Action = "query_episodes"
	ActionMemoryRefresh Action = "refresh_memory"
	ActionMemoryPersist Action = toolname.ReflectAndPersist
	ActionOnboardedMark Action = toolname.MarkOnboarded

	// --- a colleague's ordinary work -------------------------------- //
	//
	// AN ASK IS AN AGENT'S, and the only one in this block: a seat whose
	// role offers it puts a question to a colleague seat, spending that
	// seat's turn — see [rule.seatOnly].
	ActionColleagueAsk Action = toolname.A2AAsk
	ActionWorkCreate   Action = toolname.CreateWorkItem
	ActionWorkUpdate   Action = "update_work_item"
	ActionWorkComment  Action = toolname.CommentOnWorkItem
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
	ActionProjectWrite   Action = toolname.WriteProject
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
	// A ROOM'S MESSAGES DESTROYED, and its horizon set. DOTTED, because no
	// tool is named for any of the three: each is a route of the human
	// write surface, said in nobody's voice in the room (internal/chatlet).
	ActionChatErase     Action = "chat.erase"
	ActionChatPrune     Action = "chat.prune"
	ActionChatRetention Action = "chat.channel.retention"

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

	// --- using the chat ---------------------------------------------- //
	//
	// THE NINE A SEAT HOLDS, and a person's surface serves too: reading a
	// room, finding what was said, saying something, reacting, moving one's
	// own membership. Named after the tools, which internal/chatlet declares.
	ActionChatList   Action = "list_channels"
	ActionChatRead   Action = "read_channel"
	ActionChatSearch Action = toolname.SearchMessages
	ActionChatPost   Action = "post_message"
	ActionChatReply  Action = "reply_in_thread"
	ActionChatDirect Action = toolname.SendDM
	ActionChatReact  Action = "react_to_message"
	ActionChatJoin   Action = "join_channel"
	ActionChatLeave  Action = "leave_channel"

	// --- a person's chat gestures ------------------------------------ //
	//
	// THE SEVEN NO SEAT IS GIVEN, each for the reason internal/chatlet writes
	// beside its name: a room and its membership are furniture a company
	// arranges, a seat corrects what it said by saying the correction, and
	// a read cursor is a person's attention.
	ActionChatCreate  Action = "create_channel"
	ActionChatOpenDM  Action = "open_dm"
	ActionChatUpdate  Action = "update_channel"
	ActionChatMembers Action = "set_channel_members"
	ActionChatEdit    Action = "edit_message"
	ActionChatDelete  Action = "delete_message"
	ActionChatMark    Action = "mark_chat_read"
	// ActionChatArchive is archiving a room, or opening it again: asked
	// from inside `update_channel` once the stored room is read, because
	// who may is the room's creator, its unit's lead or admin — and both
	// halves of that are on the row, never in the arguments. DOTTED, because
	// no tool is named for it.
	ActionChatArchive Action = "chat.channel.archive"

	// --- the company's own controls --------------------------------- //
	ActionConfigRead  Action = "config.read"
	ActionConfigWrite Action = "config.write"
	// THE COMPANY'S PRINCIPLES (its mission, vision and standing policies),
	// read and written as one resource (`GET` and `PUT /config/charter`,
	// what Company › Principles edits in place). THEIR OWN VERBS, decided
	// exactly as [ActionConfigRead] and [ActionConfigWrite] decide the whole
	// document, for one reason: they are the one part of the company
	// document the Dashboard Assistant may carry (ADR-0052's exception),
	// and an assistant column is per verb. On the document's own verbs the
	// assistant would be one approval from every secret reference, seat and
	// provider; on these it reaches three fields of prose and nothing else.
	ActionPrinciplesRead  Action = "config.principles.read"
	ActionPrinciplesWrite Action = "config.principles.write"
	ActionSecretList      Action = "secrets.list"
	ActionSecretReveal    Action = "secrets.reveal"
	ActionSecretWrite     Action = "secrets.write"
	ActionSetupRead       Action = "setup.read"
	ActionSetupConnect    Action = "setup.connect"
	// ActionSetupAccessWrite is saving what each agent seat may do on one
	// integration (`PUT /setup/integrations/{kind}/access`): whether the
	// seat is on, its access in the vendor's own terms and its scope. ITS
	// OWN VERB AND NOT [ActionSetupConnect], because the two carry different
	// things: a connect carries vendor credentials in its body and a pass
	// that mints them, which the assistant never takes, and this carries
	// neither, so a person's assistant may prepare it for their approval.
	ActionSetupAccessWrite Action = "setup.access.write"
	// ActionSetupAccessRaise is that save when it raises a seat to an
	// administration role at a vendor (Datadog Admin Role, GitLab
	// Maintainer or Owner, Jira Administrators, Confluence Admin,
	// Mattermost System Admin, GitHub Read and write on Contents or
	// Workflows). Asked ON TOP of [ActionSetupAccessWrite] once the body
	// shows such a raise, and refused to a machine token: an agent made an
	// administrator at a vendor is a decision a person makes at a keyboard.
	ActionSetupAccessRaise Action = "setup.access.raise"
	ActionFleetOperate     Action = "fleet.operate"
	// ActionFleetRead is the deployment's own controls READ — the
	// maintenance window's status and the value a reanchor must echo.
	// Admin's as [ActionFleetOperate] is, and its own verb because it asks
	// for no step-up: a read changes nothing, and a status an operator
	// cannot see without re-proving who they are is a status they stop
	// checking.
	ActionFleetRead Action = "fleet.read"
	// ActionModelsCheck runs a model's checks on the answering node: the
	// doctor over the API (`POST /llm/providers/{key}/check`, a few tokens of
	// a real call, charged to no seat) and a host login's status read
	// (`.../host-login/check`). ActionModelsRetry clears a model's sit-out
	// on every node, or probes it first (`.../retry`). Both change RUNTIME
	// state the company already reaches, never config or a credential, so
	// they are admin's at any recency and an assistant's on approval; a
	// credential's write and a device sign-in are `secrets.write`.
	ActionModelsCheck Action = "models.check"
	ActionModelsRetry Action = "models.retry"
	ActionAuditRead   Action = "audit.read"

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
	// ActionSignInWrite ISSUES OR CLEARS A WAY FOR SOMEBODY ELSE TO SIGN IN:
	// creating a person with their first password link, inviting an address
	// with an invitation link, issuing a password reset link, and resetting
	// somebody's second factor. Admin's, at the step-up window, with a
	// person present, exactly as [ActionDirectoryWrite] is.
	//
	// ITS OWN VERB BESIDE THE DIRECTORY'S OTHER WRITES because three of its
	// four answers carry a bearer credential in clear (a link is a
	// password) and the fourth clears how somebody proves who they are: a
	// sign-in ceremony, which the Dashboard Assistant never takes
	// ([assistantNever]). Kept on [ActionDirectoryWrite] it would have
	// decided an ordinary edit of somebody's permissions and the issue of a
	// password with one assistant column.
	ActionSignInWrite Action = "iam.signin.write"
	ActionSessionEnd  Action = "iam.session.end"

	// ActionSessionInvalidate ends EVERY session in the company at once —
	// the restore runbook's last step. A verb of its own beside the
	// deployment's other controls because it is the one gesture here that
	// cannot be taken back for anybody, and it signs out the whole company
	// including the person making it.
	ActionSessionInvalidate Action = "iam.invalidate"

	// --- artifacts ---------------------------------------------------- //
	//
	// ONE ARTIFACT IS THE OBJECT OF EVERY ONE OF THEM: its versions, files,
	// assets, data, presence and view tickets are all decided as the
	// artifact, by the caller's role on it ([ClassShared]). The tools are
	// named for what a seat and a person's assistant call; the dotted verbs
	// are a person's gestures on the HTTP surface, which no tool offers.
	//
	// READING. Listing is a FILTER and never a refusal: it answers the rows
	// reading admits ([Visible]). Reading one is everything a viewer sees,
	// its history and its presence included, so there is no separate verb
	// for either.
	ActionArtifactList Action = "list_artifacts"
	ActionArtifactRead Action = "get_artifact"
	// THE DASHBOARD SOURCE CATALOGUE is the engine's own vocabulary, read
	// before a board exists, so it is everybody's: what a source ANSWERS is
	// decided as each board's viewer, never by who listed it.
	ActionDashboardSources Action = toolname.ListDashboardSources
	// THE FORMAT REFERENCE is the engine's own vocabulary too: the exact
	// shape of a kind's index (`brand.json`, `deck.json`), read before an
	// artifact of it exists, holding no row of any domain.
	ActionArtifactFormat Action = "get_artifact_format"
	// ActionDashboardSourceRead previews ONE engine source's rows before any
	// board exists (`GET /artifacts/sources/preview/{name}`): a builder's
	// read, so it asks the artifacts app, as making a board does, while the
	// rows themselves are decided by the source's own question, as the
	// viewer. ITS OWN VERB AND A READ, because mounted on
	// [ActionArtifactCreate] a GET was decided as a write, and the person's
	// assistant could not read what the person could.
	ActionDashboardSourceRead Action = "artifacts.source.read"
	// AUTHORING, each asking the `artifacts` app beside the role.
	ActionArtifactCreate  Action = toolname.CreateArtifact
	ActionArtifactPublish Action = "publish_artifact"
	ActionArtifactUpdate  Action = "update_artifact"
	ActionArtifactAsset   Action = "upload_artifact_asset"
	ActionArtifactSchema  Action = "set_artifact_schema"
	// SHARING is the owner's: the owner, the lead of an agent owner, or
	// admin. What a share may name and grant is the artifacts domain's to
	// refuse where the record is formed.
	ActionArtifactShare   Action = "share_artifact"
	ActionArtifactUnshare Action = "unshare_artifact"
	// THE DATA, which every participant reaches and the collection's own
	// rules then decide row by row once the store is read: the row admits,
	// the domain decides, as `work.thread` does for a work item's thread.
	ActionArtifactDataRead  Action = "read_artifact_data"
	ActionArtifactDataQuery Action = "query_artifact_data"
	ActionArtifactDataWrite Action = "write_artifact_data"

	// ActionArtifactForce discards a newer version than the one the writer
	// read: a publish asks it from inside, once it knows the base is stale.
	// A PERSON'S, PRESENT: a seat merges and never throws away a
	// colleague's version, and no pipeline does either.
	ActionArtifactForce Action = "artifacts.publish.force"
	// TAKING ONE OUT OF CIRCULATION AND BACK is the owner's and a person's:
	// trashing stops every viewer at once, so a model persuaded by a row it
	// read must not be able to take a team's sheet offline.
	ActionArtifactTrash   Action = "artifacts.trash"
	ActionArtifactRestore Action = "artifacts.restore"
	// BEYOND RECOVERY is admin's, as every purge is; the trash reaper purges
	// an owner's trash once its term is up.
	ActionArtifactPurge Action = "artifacts.purge"
	// HANDING ONE TO ANOTHER SEAT is how ownership moves, and the only way.
	ActionArtifactTransfer Action = "artifacts.transfer"
	// A person's own pin of an artifact in their sidebar: their record, on
	// an artifact they may read.
	ActionArtifactPin Action = "artifacts.pin"
	// THE WHOLE DATA, whatever each collection's read rule, which is why it
	// is an editor's.
	ActionArtifactExport Action = "artifacts.export"
	// What the company's artifacts hold against its quotas: admin's.
	ActionArtifactUsage Action = "artifacts.usage"
	// A viewer's browser reporting that an artifact finished rendering, for
	// the service objective: everybody's, because it carries no artifact
	// content and a reader of any artifact sends it.
	ActionArtifactReady Action = "artifacts.ready"
	// ActionArtifactTicket mints the frame a viewer's browser embeds a
	// version in (`POST /artifacts/{id}/tickets`): a view ticket bound to
	// the caller's credential. Everybody's as a gate, because what the frame
	// opens is [ActionArtifactRead]'s decision on the artifact, asked once
	// the head is read. ITS OWN VERB because the answer is a bearer
	// credential, which the Dashboard Assistant never receives
	// ([assistantNever]); mounted on the gallery's verb it was decided by a
	// row the assistant reads through.
	ActionArtifactTicket Action = "artifacts.ticket"

	// --- artifact folders and the company's brand -------------------- //
	//
	// A FOLDER GRANTS NOTHING (ADR-0047). Its own visibility is decided as
	// a relation on its top-level folder ([KindArtifactFolder], read by
	// [ActionArtifactRead]'s row, since the ladder is the artifact's), and
	// filing an artifact asks TWO decisions: the artifact's
	// ([ActionArtifactMove]) and the destination folder's
	// ([ActionArtifactFolderFile]), each through the requester ceiling.
	//
	// CREATING A FOLDER is the app (an agent creates subfolders only, a rule
	// the artifacts writer holds) beside contributor on the parent.
	ActionArtifactFolderCreate Action = "create_artifact_folder"
	// FILING ONE ARTIFACT is an edit of where it lives: editor on it.
	ActionArtifactMove Action = "move_artifact"
	// FILING INTO A FOLDER, or creating a folder inside one: contributor on
	// the destination, asked beside the move or the create.
	ActionArtifactFolderFile Action = "artifacts.folder.file"
	// MANAGING A FOLDER is its owner's, the owner's lead's or admin's, and
	// a person's: a rename, a move under another parent, a removal (which
	// moves what it holds up a level) and taking an artifact out of it.
	ActionArtifactFolderRename Action = "artifacts.folder.rename"
	ActionArtifactFolderMove   Action = "artifacts.folder.move"
	ActionArtifactFolderRemove Action = "artifacts.folder.remove"
	ActionArtifactFolderUnfile Action = "artifacts.folder.unfile"

	// THE ADOPTED BRAND IS THE COMPANY'S PUBLIC FACE (ADR-0048): everybody
	// reads it, every agent under every ceiling included, and only an
	// administrator, a person at a recent proof, changes it.
	ActionBrandRead  Action = "get_company_brand"
	ActionBrandAdopt Action = "artifacts.brand.adopt"
	ActionBrandClear Action = "artifacts.brand.clear"

	// --- the Dashboard Assistant (ADR-0050 to ADR-0053) ---
	//
	// The assistant's own verbs: using it, reading a conversation,
	// approving or declining a proposal, and an administrator's audited
	// read. Each is dotted, and lands here with the surface that asks it.

	// ActionAssistantConverse is using the assistant at all: starting a
	// conversation, asking in it, stopping a turn, clearing one. Whose
	// conversation it is is never this verb's question: the store answers
	// 404 for anybody but its owner (ADR-0053).
	ActionAssistantConverse Action = "assistant.converse"
	// ActionAssistantConversationRead is reading one's own conversations:
	// the list, a transcript, a turn's live stream and a card's export.
	ActionAssistantConversationRead Action = "assistant.conversation.read"
	// ActionAssistantApprove is a person approving one of their own
	// proposals: `POST /assistant/proposals/{id}/approve`. Everybody's,
	// because whose proposal it is is the store's answer (ADR-0053) and what
	// the write may do is the write's own row, decided again when it is sent.
	// The write's own step-up is asked by the approval itself (ADR-0051).
	ActionAssistantApprove Action = "assistant.approve"
	// ActionAssistantDecline is a person declining one of their own
	// proposals.
	ActionAssistantDecline Action = "assistant.decline"
	// ActionAssistantAuditRead is an administrator reading somebody else's
	// conversation for compliance (decision D06): admin's, with a recent
	// proof, every read recorded beside the reader's name and reason and
	// shown to the conversation's owner.
	ActionAssistantAuditRead Action = "assistant.conversation.audit_read"
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

// AssistClass is what the Dashboard Assistant may do with a verb: take it
// under a read grant, carry it only as the one approved request an exec grant
// binds, or never (ADR-0052). See [rule.assist].
//
// IT ONLY EVER TAKES AWAY. An assisted principal is the person, composed from
// their own identity rows, so the class and every relation are decided for
// them exactly as for their own click; this says which of those admissions
// the assistant may also make, and no value of it admits anything the
// person's authority refuses.
type AssistClass string

const (
	// AssistRead is a verb an assistant read grant may take: a read the
	// person may make, which a turn's tool loop makes on their behalf with
	// nothing for them to approve.
	AssistRead AssistClass = "read"
	// AssistApprove is a verb the assistant may carry only as the one
	// request a person approved: an exec grant binds the request's digest,
	// so the write that runs is the preview the person saw (ADR-0051).
	AssistApprove AssistClass = "approve"
	// AssistNever is a verb no assistant grant takes, whoever approved what.
	AssistNever AssistClass = "never"
)

// AssistClasses are the three, in declaration order.
var AssistClasses = []AssistClass{AssistRead, AssistApprove, AssistNever}

// Valid reports whether an assist class is one this build knows. The zero is
// not one, because a row nobody decided for the assistant must not read as
// any of the three.
func (c AssistClass) Valid() bool { return slices.Contains(AssistClasses, c) }

// rule is one verb's row: which class decides it, which app it belongs to,
// and the facts about the verb itself that every class composes with.
//
// ONE TABLE RATHER THAN SEVERAL. A class map beside an app map beside a
// human-only list is several things that must agree about one verb, and the
// day they disagree one verb is decided by a class that reads a field nobody
// set.
type rule struct {
	class Class

	// app is the app this verb belongs to ([iam.GrantWorklet],
	// [iam.GrantDoclet], [iam.GrantArtifacts], [iam.GrantChatlet]), or empty
	// for a verb that belongs to none. It is
	// the requirement of [ClassApp] and [ClassAppOrAsked], and it is ANDED
	// onto every relation class ([withApp]): a project's lead who does not
	// use Worklet does not set its policy, and a comment's author who no
	// longer uses Doclet does not edit it there. An
	// administrator holds every app through [iam.Expand], so naming one
	// never narrows admin's path.
	//
	// A FIELD ON THE ROW AND NOT A PER-KIND LOOKUP, because the table is
	// what a REST route, a socket question, a tool and every walk read: an
	// app derived from the object's kind would answer one verb differently
	// on two objects, and a walk over the table would certify a row whose
	// app was decided somewhere else.
	app iam.Grant

	// anyOf is the apps a [ClassAnyApp] row admits on, any ONE of them, and
	// the set its refusal names. Empty on every other class, and a walk
	// holds both halves: a ClassAnyApp row names at least two apps and no
	// other row names any.
	//
	// ON THE ROW AND NEVER "EVERY APP". The class used to iterate [iam.Apps],
	// which was right only while every app was something its one verb
	// searches: with chat declared, a gesture answered from the pages and
	// the board admitted a holder of chat alone to a search of nothing, and
	// told everybody it refused that chat would do. Which apps a gesture
	// draws on is a fact about that gesture, so the row says it.
	anyOf []iam.Grant

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

	// irreversible marks a verb NOTHING UNDOES: a purge, an erase of chat
	// messages, a prune of a room's past, and a room's horizon, which is a
	// prune on a delay. A fact about the VERB, beside humanOnly, and read by
	// whatever shows the gesture to a person before they make it (the
	// Dashboard Assistant's card marks it destructive and says it cannot be
	// undone): a route's method cannot say it, since a purge is a POST and
	// a horizon a PUT, and the row is where "a recent proof, since nothing
	// undoes it" was already decided. A walk holds every such row to a
	// recent proof. Decides nothing in [Decide].
	irreversible bool

	// seatOnly is humanOnly's MIRROR: a verb ONLY AN AGENT SEAT may take —
	// putting a question to a colleague seat — with no permission asked at
	// all. Which seats are offered it is the role's tool registry's
	// decision, which already says which seat runs which tool; a person has
	// no turn for a colleague's answer to land in.
	//
	// IT REPLACED A PERMISSION. Asking a colleague rode on the permission
	// that wrote work — so a role that did not use the tracker could not ask
	// anybody anything — and that was never a decision anybody made about a
	// PERSON, which is what a permission is for.
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

	// minRole is the least ROLE on an artifact a [ClassShared] row admits:
	// viewer to read, contributor and up to write what the page itself is
	// made of, owner to share and take it out of circulation.
	//
	// REQUIRED ON EVERY SHARED ROW AND REFUSED ON EVERY OTHER, by a walk in
	// this package's tests, for [rule.recency]'s reason: a zero role read as
	// the lowest would be a shared verb somebody added without deciding,
	// shipped open to every viewer. [artifacts.Role.Includes] already admits
	// nobody on a zero requirement; the walk makes that a build failure
	// instead of a verb nobody can take.
	minRole artifacts.Role

	// steerable says an agent's OWN object on this verb is steerable by
	// whoever woke it: the requester ceiling ([DecideWithin]) spares an
	// agent acting on what it owns, so "please fix the typo in your table"
	// from a colleague who only views it is still a request it can carry
	// out, as it carries out an edit to any other work it owns.
	//
	// THE ZERO IS THE CEILING ALWAYS APPLYING, the safe direction: a verb
	// that discloses content is never steerable, or anybody who can message
	// an agent could have it read its private work back to them. And
	// neither is sharing, which hands the artifact to whoever the share
	// names: a steerable share would be that same read, one step removed.
	steerable bool

	// assist is what the verb IS to the Dashboard Assistant (see
	// [AssistClass]): [AssistRead], a read a turn makes on the person's
	// behalf, or [AssistApprove], a change it carries only as the one request
	// the person approved. Asked of a principal acting through an assistant
	// grant ([iam.AssistPrefix]) and of nobody else.
	//
	// PARITY WITH THE PERSON DECIDES IT (ADR-0052): a verb the person may
	// take is one their assistant may take, as a read or as an approved
	// change. The only verbs it never takes are [assistantNever]'s, each
	// with its reason, and that list is the ONE place [AssistNever] is
	// spelled: a row states the verb's nature and never Never, so the
	// human-only set cannot be widened by a row nobody reads beside the list.
	//
	// EVERY ROW STATES ONE and the zero is refused by a walk in this
	// package's tests, for [rule.recency]'s reason: a verb added without
	// deciding would otherwise read as one of the classes. [Decide] reads a
	// zero as [AssistNever] besides, so a missed row fails closed.
	assist AssistClass
}

// rules is THE authority table: every verb this engine knows, and how it is
// decided.
//
// A VERB MISSING FROM HERE IS REFUSED, and the completeness walk in this
// package's tests is what turns that into a build failure rather than a
// production one. The alternative — a default class — is exactly how a new
// verb ships ungated, and it ships looking correct.
var rules = map[Action]rule{
	// --- using Worklet ------------------------------------------- //
	//
	// THE APP IS THE WHOLE RULE: reading the board, a project, its files,
	// its views and its catalogue, and filing, updating, merging and
	// arranging work. A company that hands somebody the tracker is saying
	// exactly this, and a company that does not hands them the overview.
	//
	// READING ONE ITEM IS ALSO THE ASKED PERSON'S. Somebody a colleague put
	// a question to reads the item the question is on, whether or not they
	// use Worklet (see [ClassAppOrAsked]).
	ActionWorkRead: {class: ClassAppOrAsked, app: iam.GrantWorklet, recency: iam.RecencyAny,
		assist: AssistRead},
	ActionWorkList: {class: ClassApp, app: iam.GrantWorklet, recency: iam.RecencyAny,
		assist: AssistRead},
	ActionWorkSearch: {class: ClassApp, app: iam.GrantWorklet, recency: iam.RecencyAny,
		assist: AssistRead},
	ActionWorkActivity: {class: ClassApp, app: iam.GrantWorklet, recency: iam.RecencyAny,
		assist: AssistRead},
	ActionProjectRead: {class: ClassApp, app: iam.GrantWorklet, recency: iam.RecencyAny,
		assist: AssistRead},
	ActionProjectList: {class: ClassApp, app: iam.GrantWorklet, recency: iam.RecencyAny,
		assist: AssistRead},
	ActionCatalogueRead: {class: ClassApp, app: iam.GrantWorklet, recency: iam.RecencyAny,
		assist: AssistRead},
	ActionViewList: {class: ClassApp, app: iam.GrantWorklet, recency: iam.RecencyAny,
		assist: AssistRead},
	// A PROJECT'S FILES ARE ITS WORK'S OUTPUT, read and kept on the tracker
	// as its items are — never on the knowledge base a body of prose might
	// suggest.
	ActionFileList: {class: ClassApp, app: iam.GrantWorklet, recency: iam.RecencyAny,
		assist: AssistRead},
	ActionFileRead: {class: ClassApp, app: iam.GrantWorklet, recency: iam.RecencyAny,
		assist: AssistRead},
	ActionFileWrite: {class: ClassApp, app: iam.GrantWorklet, recency: iam.RecencyAny,
		assist: AssistApprove},

	ActionWorkCreate: {class: ClassApp, app: iam.GrantWorklet, recency: iam.RecencyAny,
		assist: AssistApprove},
	ActionWorkUpdate: {class: ClassApp, app: iam.GrantWorklet, recency: iam.RecencyAny,
		assist: AssistApprove},
	// A COMMENT IS THE TRACKER'S, OR THE ANSWER THE ASKED PERSON OWES. The
	// asked arm admits a person an open ask names — and the tool then takes
	// only their answer, which the tracker's writer checks again is theirs.
	ActionWorkComment: {class: ClassAppOrAsked, app: iam.GrantWorklet, recency: iam.RecencyAny,
		assist: AssistApprove},
	ActionWorkMerge: {class: ClassApp, app: iam.GrantWorklet, recency: iam.RecencyAny,
		assist: AssistApprove},
	// A PLACE IS THE APP ON THE TASK, although the record arbitrates on the
	// project's ORDER rather than on the task: the order is an object nobody
	// owns, so the only relation to ask about is none. A person's gesture —
	// see [ActionWorkPlace].
	ActionWorkPlace: {class: ClassApp, app: iam.GrantWorklet, humanOnly: true,
		recency: iam.RecencyAny, assist: AssistApprove},
	// THE ROUTE TO AN ITEM'S THREAD ADMITS EVERYBODY and decides nothing:
	// [ActionWorkComment] decides once the item is read. See
	// [ActionWorkThread].
	ActionWorkThread: {class: ClassEverybody, recency: iam.RecencyAny, assist: AssistApprove},

	// `write_project` IS TWO GESTURES AND ONLY ONE IS THE LEAD'S, which is
	// what internal/worklet's own tag rule has always said: adding a label
	// is open to everybody who uses Worklet, and renaming or archiving
	// one takes the word off everybody's board. So the TOOL takes the app
	// and the lead-only facets — a rename, an archive, any policy edit —
	// ask [ActionProjectPolicy] from inside it.
	ActionProjectWrite: {class: ClassApp, app: iam.GrantWorklet, recency: iam.RecencyAny,
		assist: AssistApprove},
	ActionProjectPolicy: {class: ClassContainer, app: iam.GrantWorklet,
		recency: iam.RecencyAny, assist: AssistApprove},
	// AND RE-ROUTING IS THE LEAD'S for the same reason, asked from inside
	// `update_work_item`: it decides which team hears about the work, and
	// which project the task is filed under comes out of the stored row
	// rather than the arguments.
	ActionWorkRoute: {class: ClassContainer, app: iam.GrantWorklet, recency: iam.RecencyAny,
		assist: AssistApprove},
	// ARCHIVING A PROJECT TAKES IT OUT OF CIRCULATION for everybody, so it
	// is the lead's like the rest of the policy AND it is never an agent's:
	// a project nobody can file into again is a company decision.
	ActionProjectArchive: {class: ClassContainer, app: iam.GrantWorklet, humanOnly: true,
		recency: iam.RecencyAny, assist: AssistApprove},

	// AN ITEM'S REPORTER TIDIES IT, beside its project's lead and admin:
	// whoever filed a duplicate takes it out again, and puts it back. A
	// subtree stays the lead's (the tool refuses an author it), and a purge
	// stays admin's. See [ClassTidy].
	ActionWorkRemove: {class: ClassTidy, app: iam.GrantWorklet, recency: iam.RecencyAny,
		assist: AssistApprove},
	ActionWorkRestore: {class: ClassTidy, app: iam.GrantWorklet, recency: iam.RecencyAny,
		assist: AssistApprove},
	// A MOVE TAKES AN ITEM OUT OF ITS PROJECT, and a file's removal takes a
	// file out of one: the project's lead or admin, never every colleague
	// who may add to it.
	ActionWorkMove: {class: ClassDestructive, app: iam.GrantWorklet, recency: iam.RecencyAny,
		assist: AssistApprove},
	ActionFileRemove: {class: ClassDestructive, app: iam.GrantWorklet, recency: iam.RecencyAny,
		assist: AssistApprove},

	// A WORK ITEM'S REMARK IS ITS AUTHOR'S, or admin's, and the tracker's:
	// the class answers "may you touch this remark", and the WRITER refuses
	// anybody but the author an edit, because an edit puts words in
	// somebody's mouth where a removal only takes them down.
	ActionWorkCommentEdit: {class: ClassAuthored, app: iam.GrantWorklet, recency: iam.RecencyAny,
		assist: AssistApprove},

	// A VIEW IS NOT A RECORD, because half of them are not personal at
	// all: omitting the owner SHARES it, which is a tab on somebody's
	// project, unit or person page. See [ClassSavedView].
	ActionViewSave: {class: ClassSavedView, app: iam.GrantWorklet, recency: iam.RecencyAny,
		assist: AssistApprove},

	// --- somebody's own record, on the tracker ------------------------ //
	//
	// READING SOMEBODY'S DAY IS THE LEAD RELATION, and MARKING it is not. A
	// lead re-orders what their report works on and says what is
	// important; marking somebody's mail read and rearranging their pinned
	// views is a gesture nobody asked a lead to make. Every one of them is
	// the TRACKER's — a person's notices name its tasks and their queue
	// lists them — so each takes the app beside the relation, and a person
	// who does not use Worklet has no queue there to keep.
	ActionPersonRead: {class: ClassOwnOrLead, app: iam.GrantWorklet, recency: iam.RecencyAny,
		assist: AssistRead},
	ActionMyWork: {class: ClassOwnOrLead, app: iam.GrantWorklet, recency: iam.RecencyAny,
		assist: AssistRead},
	ActionInboxRead: {class: ClassOwnOrLead, app: iam.GrantWorklet, recency: iam.RecencyAny,
		assist: AssistRead},
	ActionPrioritiesSet: {class: ClassOwnOrLead, app: iam.GrantWorklet, recency: iam.RecencyAny,
		assist: AssistApprove},
	ActionInboxMark: {class: ClassOwnRecord, app: iam.GrantWorklet, recency: iam.RecencyAny,
		assist: AssistApprove},
	ActionPinsSet: {class: ClassOwnRecord, app: iam.GrantWorklet, recency: iam.RecencyAny,
		assist: AssistApprove},

	// --- using Doclet ------------------------------------ //
	ActionPageRead: {class: ClassApp, app: iam.GrantDoclet, recency: iam.RecencyAny,
		assist: AssistRead},
	ActionPageList: {class: ClassApp, app: iam.GrantDoclet, recency: iam.RecencyAny,
		assist: AssistRead},
	ActionKnowledgeRead: {class: ClassApp, app: iam.GrantDoclet, recency: iam.RecencyAny,
		assist: AssistRead},
	// WHERE A PAGE MAY BE CREATED is internal/doclet's store's to decide (a
	// space the company declares) for every author alike; this row decides
	// only WHO.
	ActionPageCreate: {class: ClassApp, app: iam.GrantDoclet, recency: iam.RecencyAny,
		assist: AssistApprove},
	ActionPageSave: {class: ClassApp, app: iam.GrantDoclet, recency: iam.RecencyAny,
		assist: AssistApprove},
	ActionPageComment: {class: ClassApp, app: iam.GrantDoclet, recency: iam.RecencyAny,
		assist: AssistApprove},
	// A PAGE'S CREATOR TIDIES IT — renames, trashes, restores — beside its
	// container's lead and admin. See [ClassTidy].
	ActionPageRename: {class: ClassTidy, app: iam.GrantDoclet, recency: iam.RecencyAny,
		assist: AssistApprove},
	ActionPageTrash: {class: ClassTidy, app: iam.GrantDoclet, recency: iam.RecencyAny,
		assist: AssistApprove},
	ActionPageRestore: {class: ClassTidy, app: iam.GrantDoclet, recency: iam.RecencyAny,
		assist: AssistApprove},
	// A REMARK IS ITS AUTHOR'S, as a work item's is; the store refuses
	// anybody but the author an edit, and takes a moderator's removal.
	ActionPageCommentEdit: {class: ClassAuthored, app: iam.GrantDoclet, recency: iam.RecencyAny,
		assist: AssistApprove},
	ActionPageCommentRemove: {class: ClassAuthored, app: iam.GrantDoclet, recency: iam.RecencyAny,
		assist: AssistApprove},

	// A QUESTION ANSWERED FROM WHAT THE COMPANY WROTE DOWN draws on
	// whichever of its two apps the asker uses: the tool searches the pages
	// only for a knowledge holder and the board only for a tracker holder,
	// so either app admits and a caller with neither has nothing it could
	// search — whatever else they hold. And
	// a person's alone: it runs on the auxiliary model of the seat the
	// asker holds, and a seat has search_knowledge and a model of its own.
	// And SEATED: a principal no seat binds — a Tier A token's session or a
	// service account, never a person, who holds a human seat for as long
	// as they are here (ADR-0032) — has no seat whose model to answer on
	// and nobody the spend is charged for.
	ActionKnowledgeAnswer: {class: ClassAnyApp,
		anyOf:     []iam.Grant{iam.GrantWorklet, iam.GrantDoclet},
		humanOnly: true, seated: true, recency: iam.RecencyAny, assist: AssistApprove},

	// --- using the chat ---------------------------------------------- //
	//
	// THE APP IS THE WHOLE RULE for using chat, as it is for the tracker's
	// and the knowledge base's own blocks above. WHICH ROOM is the chat
	// domain's: a read of a room the caller may not read answers NOT FOUND
	// (internal/chatlet's Visible — never a refusal that confirms the room
	// exists), and a write to one is refused inside the write's own
	// snapshot, which is the state log's one write authority. A room's
	// membership is a replicated row and not a relation of the chart, so a
	// relation here would be read in another transaction.
	//
	// EVERY CHAT ROW IS SEATED ([rule.seated]): a room renders a speaker by
	// seat, its membership is seats and a direct conversation's identity is
	// their handles, so a credential acting as itself has no seat to speak,
	// be a member or be shown the rooms as. A pipeline that should post is
	// given a human seat and its credential bound to it.
	ActionChatList: {class: ClassApp, app: iam.GrantChatlet, seated: true, recency: iam.RecencyAny,
		assist: AssistRead},
	ActionChatRead: {class: ClassApp, app: iam.GrantChatlet, seated: true, recency: iam.RecencyAny,
		assist: AssistRead},
	ActionChatSearch: {class: ClassApp, app: iam.GrantChatlet, seated: true, recency: iam.RecencyAny,
		assist: AssistRead},
	ActionChatPost: {class: ClassApp, app: iam.GrantChatlet, seated: true, recency: iam.RecencyAny,
		assist: AssistApprove},
	ActionChatReply: {class: ClassApp, app: iam.GrantChatlet, seated: true, recency: iam.RecencyAny,
		assist: AssistApprove},
	ActionChatDirect: {class: ClassApp, app: iam.GrantChatlet, seated: true, recency: iam.RecencyAny,
		assist: AssistApprove},
	ActionChatReact: {class: ClassApp, app: iam.GrantChatlet, seated: true, recency: iam.RecencyAny,
		assist: AssistApprove},
	// A SEAT MOVES ITS OWN MEMBERSHIP and nobody else's — the tools take no
	// handle, and the write path refuses any other — so the app is the rule.
	ActionChatJoin: {class: ClassApp, app: iam.GrantChatlet, seated: true, recency: iam.RecencyAny,
		assist: AssistApprove},
	ActionChatLeave: {class: ClassApp, app: iam.GrantChatlet, seated: true, recency: iam.RecencyAny,
		assist: AssistApprove},

	// --- a person's chat gestures ------------------------------------ //
	//
	// HUMAN-ONLY, each for a fact about the verb (internal/chatlet's
	// vocabulary says which): a seat that made rooms as a side effect of
	// its work would leave a sidebar nobody chose, and one that rewrote
	// what it said would leave a record the next turn reads as having
	// always said it. A room's settings and its whole membership are any
	// member's who can reach it — the store refuses whoever cannot — and
	// its ARCHIVE is not: see [ActionChatArchive].
	ActionChatCreate: {class: ClassApp, app: iam.GrantChatlet, humanOnly: true, seated: true,
		recency: iam.RecencyAny, assist: AssistApprove},
	ActionChatOpenDM: {class: ClassApp, app: iam.GrantChatlet, humanOnly: true, seated: true,
		recency: iam.RecencyAny, assist: AssistApprove},
	ActionChatUpdate: {class: ClassApp, app: iam.GrantChatlet, humanOnly: true, seated: true,
		recency: iam.RecencyAny, assist: AssistApprove},
	ActionChatMembers: {class: ClassApp, app: iam.GrantChatlet, humanOnly: true, seated: true,
		recency: iam.RecencyAny, assist: AssistApprove},
	// A MESSAGE IS ITS AUTHOR'S, or admin's — the comment precedent: the
	// class answers "may you touch this remark", and the WRITER refuses
	// anybody but the author an EDIT, because an edit puts words in
	// somebody's mouth where a delete only takes them down. Admin's delete
	// is MODERATION, and it is decided on the unread message, so a
	// moderator takes down what was said in a room they are not in without
	// reading it ([ClassAuthored]).
	ActionChatEdit: {class: ClassAuthored, app: iam.GrantChatlet, humanOnly: true, seated: true,
		recency: iam.RecencyAny, assist: AssistApprove},
	ActionChatDelete: {class: ClassAuthored, app: iam.GrantChatlet, humanOnly: true, seated: true,
		recency: iam.RecencyAny, assist: AssistApprove},
	// A READ CURSOR IS ONE'S OWN RECORD, as an inbox's marks are, and the
	// tool takes no handle, so the record is always the caller's own — kept
	// under their SEAT ([iam.RecordOwner]), whose next holder inherits it.
	ActionChatMark: {class: ClassOwnRecord, app: iam.GrantChatlet, humanOnly: true, seated: true,
		recency: iam.RecencyAny, assist: AssistApprove},
	// ARCHIVING A ROOM CLOSES THE CONVERSATION for everybody in it, so it is
	// a TIDY rather than anybody's: the room's creator, the lead of the unit
	// a unit's room belongs to or of a unit above it, or admin. Never an
	// agent's, as no room setting is.
	ActionChatArchive: {class: ClassTidy, app: iam.GrantChatlet, humanOnly: true, seated: true,
		recency: iam.RecencyAny, assist: AssistApprove},

	// --- everybody's overview ---------------------------------------- //
	//
	// WHO TO ASK IS EVERYBODY'S: the colleague lookup answers from the org
	// chart, which everybody sees. The person-facing question masks the
	// external contact ids for anybody but admin; a seat's tool keeps them,
	// because an agent mentions people by them.
	ActionColleagueRead: {class: ClassEverybody, recency: iam.RecencyAny, assist: AssistRead},
	// WHO HOLDS EACH HUMAN SEAT is the org chart's people, named — see
	// [ActionSeatsRead]. The route masks each holder per caller.
	ActionSeatsRead: {class: ClassEverybody, recency: iam.RecencyAny, assist: AssistRead},

	// --- an agent's own ---------------------------------------------- //
	//
	// THE CALLER'S OWN WORKING STATE, no permission and no admin path.
	ActionSkillUse:      {class: ClassSelf, recency: iam.RecencyAny, assist: AssistRead},
	ActionSkillLoad:     {class: ClassSelf, recency: iam.RecencyAny, assist: AssistRead},
	ActionSkillRefine:   {class: ClassSelf, recency: iam.RecencyAny, assist: AssistApprove},
	ActionEpisodesQuery: {class: ClassSelf, recency: iam.RecencyAny, assist: AssistRead},
	ActionMemoryRefresh: {class: ClassSelf, recency: iam.RecencyAny, assist: AssistRead},
	ActionMemoryPersist: {class: ClassSelf, recency: iam.RecencyAny, assist: AssistApprove},
	ActionOnboardedMark: {class: ClassSelf, recency: iam.RecencyAny, assist: AssistApprove},
	// AN ASK IS AN AGENT'S, whichever apps it uses: it spends a colleague
	// seat's turn, and the ROLE decides which seats are offered it. No
	// person takes it — see [rule.seatOnly].
	ActionColleagueAsk: {class: ClassEverybody, seatOnly: true, recency: iam.RecencyAny,
		assist: AssistApprove},

	// --- whether a seat works, and what its turn is doing ------------- //
	//
	// WHETHER A SEAT WORKS IS ITS LEAD'S TO DECIDE, or admin's, exactly as
	// what it works on is ([ActionPrioritiesSet]) — and never any reader's.
	// HUMAN-ONLY on top: a seat that could pause a colleague, resume
	// itself, steer another's turn or answer its own run's question would
	// be overruling the people who run it. No app: a lead who uses no app
	// still runs their team.
	ActionSeatPause: {class: ClassOwnOrLead, humanOnly: true, recency: iam.RecencyAny,
		assist: AssistApprove},
	ActionSeatResume: {class: ClassOwnOrLead, humanOnly: true, recency: iam.RecencyAny,
		assist: AssistApprove},
	ActionTurnSteer: {class: ClassOwnOrLead, humanOnly: true, recency: iam.RecencyAny,
		assist: AssistApprove},
	// A RUN'S QUESTION IS ITS REQUESTER'S TO ANSWER, or the run's seat's
	// lead's: the tool asks this row about the REQUESTER's seat when the
	// caller is that requester (the self arm), and about the run's seat
	// otherwise — so the person who asked for the work answers what it
	// asks them, and nobody who merely leads them does.
	ActionRunAnswer: {class: ClassOwnOrLead, humanOnly: true, recency: iam.RecencyAny,
		assist: AssistApprove},

	// A SEAT'S TRAIL IS ITS LEAD'S TO READ, and admin's: its diary, its
	// episodes, its threads and its own turns' prompts and responses. A lead
	// steers, pauses and answers for their agents, and could do none of it
	// well without reading what those agents did; somebody who merely
	// shares the company does not read every prompt an agent was given.
	// The COMPANY-WIDE trail — every event, every turn, every channel, the
	// identity estate's audit — stays admin's ([ActionAuditRead]).
	ActionSeatTrailRead: {class: ClassOwnOrLead, recency: iam.RecencyAny, assist: AssistRead},

	// --- running the company ------------------------------------------ //
	//
	// A TOOL SKILL IS CONFIGURATION WRITTEN AS A PAGE. The skills
	// container's pages are injected into a phase of EVERY seat's turn as
	// instructions, so writing one rewrites the prompt the whole company
	// runs under — the same reach as editing the company document, and it
	// takes admin rather than the knowledge app that authors an ordinary
	// page. AND NEVER AN AGENT'S, which internal/doclet's store also refuses:
	// a seat writing the instructions every seat obeys is a model editing
	// the rules it is judged by.
	ActionSkillPageWrite: {class: ClassAdmin, humanOnly: true, recency: iam.RecencyAny,
		assist: AssistApprove},
	// THE CATALOGUE IS THE COMPANY'S, NOT A CONTAINER'S. Task types and
	// custom fields are declared once for the whole workspace — the tool
	// names no project and could not. It is configuration, and admin's.
	ActionCatalogueWrite: {class: ClassAdmin, recency: iam.RecencyAny, assist: AssistApprove},

	// --- beyond recovery --------------------------------------------- //
	//
	// A PURGE IS ADMIN'S, A PERSON'S, AND A RECENT PROOF'S. Nothing undoes
	// it, so it is the one tidy an author does not take for themselves —
	// trash and restore are theirs — and it asks the step-up window as
	// every other irreversible gesture does.
	ActionWorkPurge: {class: ClassAdmin, humanOnly: true, irreversible: true,
		recency: iam.RecencyStepUp, assist: AssistApprove},
	ActionPagePurge: {class: ClassAdmin, humanOnly: true, irreversible: true,
		recency: iam.RecencyStepUp, assist: AssistApprove},
	// A CHAT DESTRUCTION IS A PURGE'S: admin's, a person's or a credential's
	// — never an agent's, which could erase the evidence of what it said —
	// and a recent proof's, since nothing undoes it. An erase and a prune
	// destroy what people wrote; a room's retention is a prune on a delay,
	// so setting a shorter one is the same decision taken in advance.
	//
	// NOT SEATED, unlike every other chat verb: none of the three is said in
	// a room, so the store records whoever made it under their own name —
	// an administrator's pipeline token included — and none READS the room
	// it destroys from, which is what lets an administrator who is in no
	// private room take a credential pasted into one out of it without being
	// shown what else was said there. Which room exists is the store's to
	// answer; a destruction of a room the store does not hold destroys
	// nothing and says so.
	ActionChatErase: {class: ClassAdmin, humanOnly: true, irreversible: true,
		recency: iam.RecencyStepUp, assist: AssistApprove},
	ActionChatPrune: {class: ClassAdmin, humanOnly: true, irreversible: true,
		recency: iam.RecencyStepUp, assist: AssistApprove},
	ActionChatRetention: {class: ClassAdmin, humanOnly: true, irreversible: true,
		recency: iam.RecencyStepUp, assist: AssistApprove},

	// --- the org chart ------------------------------------------------- //
	//
	// THE ORG CHART IS ITS LEADS' AS WELL AS ADMIN'S. A lead writes what
	// lies inside the units they lead — see [ClassSubtree]. HUMAN-ONLY and
	// at the step-up window, as every other write of the company document
	// is: a seat rewriting the org it runs inside is a model choosing its
	// own team. A PERSON'S ASSISTANT carries it as the person's own edit,
	// approved from a before-and-after of the seats and units it changes,
	// at the same step-up window (ADR-0052).
	ActionOrgWrite: {class: ClassSubtree, humanOnly: true, recency: iam.RecencyStepUp,
		assist: AssistApprove},
	// AND A LEAD READS WHAT THEY MAY WRITE: one seat or one unit of their
	// subtree, masked as every read of the document is, without admin.
	ActionOrgRead: {class: ClassSubtree, recency: iam.RecencyAny, assist: AssistRead},

	// --- the company's own controls ------------------------------------ //
	ActionConfigRead:  {class: ClassAdmin, recency: iam.RecencyAny, assist: AssistRead},
	ActionConfigWrite: {class: ClassAdmin, recency: iam.RecencyStepUp, assist: AssistApprove},
	// THE PRINCIPLES, AS THE DOCUMENT IS: admin's, the write at the step-up
	// window. The charter is the company's root, nobody's subtree, so no
	// lead reaches either verb.
	ActionPrinciplesRead:  {class: ClassAdmin, recency: iam.RecencyAny, assist: AssistRead},
	ActionPrinciplesWrite: {class: ClassAdmin, recency: iam.RecencyStepUp, assist: AssistApprove},
	ActionSecretList:      {class: ClassAdmin, recency: iam.RecencyAny, assist: AssistRead},
	// A REVEAL HANDS OVER A CREDENTIAL'S VALUE, which is a person's to read
	// and never a pipeline's: the row refuses any request a token presented.
	ActionSecretReveal: {class: ClassAdmin, recency: iam.RecencyStepUp,
		presence: PresenceRequired, assist: AssistRead},
	ActionSecretWrite:  {class: ClassAdmin, recency: iam.RecencyStepUp, assist: AssistApprove},
	ActionSetupRead:    {class: ClassAdmin, recency: iam.RecencyAny, assist: AssistRead},
	ActionSetupConnect: {class: ClassAdmin, recency: iam.RecencyStepUp, assist: AssistApprove},
	// AN AGENT'S ACCESS AT A VENDOR is admin's at the step-up window, as
	// every other write of the company document is, and the assistant
	// prepares it for the person's approval: it names no credential and no
	// variable, only what the seat may do. A RAISE TO AN ADMINISTRATION
	// ROLE also needs a person present, so no pipeline holding a token can
	// make an agent a vendor's administrator; an approved assistant request
	// is that person, deciding that one request (ADR-0051).
	ActionSetupAccessWrite: {class: ClassAdmin, recency: iam.RecencyStepUp,
		assist: AssistApprove},
	ActionSetupAccessRaise: {class: ClassAdmin, recency: iam.RecencyStepUp,
		presence: PresenceRequired, assist: AssistApprove},
	ActionFleetOperate: {class: ClassAdmin, recency: iam.RecencyStepUp, assist: AssistApprove},
	ActionFleetRead:    {class: ClassAdmin, recency: iam.RecencyAny, assist: AssistRead},
	// A MODEL'S CHECKS AND ITS RETRY are admin's with no step-up: neither
	// writes the company or a credential, and the person reaching for them
	// is the one watching a model refuse right now. The assistant proposes
	// each and the person approves it, since a check spends tokens and a
	// retry moves every node.
	ActionModelsCheck: {class: ClassAdmin, recency: iam.RecencyAny, assist: AssistApprove},
	ActionModelsRetry: {class: ClassAdmin, recency: iam.RecencyAny, assist: AssistApprove},
	// THE COMPANY-WIDE TRAIL — every event, turn and phase, every agent-to-
	// agent channel, the identity estate's own audit. Admin's: one seat's
	// trail is its lead's too ([ActionSeatTrailRead]); everybody's is not.
	ActionAuditRead: {class: ClassAdmin, recency: iam.RecencyAny, assist: AssistRead},

	// --- the identity directory ---------------------------------------- //
	//
	// A DIRECTORY READ IS THE PERSON IT IS ABOUT, OR ADMIN — see
	// [ClassDirectory] for why the object is an id.
	ActionDirectoryRead: {class: ClassDirectory, recency: iam.RecencyAny, assist: AssistRead},
	// AND A DIRECTORY WRITE IS ADMIN'S, with NO self path at all. Editing
	// your own permissions is the escalation this estate exists to close,
	// so "it is my own row" must not be a way in — the record layer refuses
	// conferring what the caller does not hold, and this refuses the
	// gesture before it gets there.
	//
	// EVERY ONE OF THEM (editing somebody's row, removing them, withdrawing
	// an invitation, and [ActionSignInWrite]'s four) asks the step-up
	// window, and needs a person present: what keeps a week-old cookie away
	// is the window, and what keeps a stolen pipeline credential away is the
	// row's own refusal of a token and internal/iamdomain's refusal of an
	// administrative record written through one.
	ActionDirectoryWrite: {class: ClassAdmin, recency: iam.RecencyStepUp,
		presence: PresenceRequired, assist: AssistApprove},
	ActionSignInWrite: {class: ClassAdmin, recency: iam.RecencyStepUp,
		presence: PresenceRequired, assist: AssistApprove},
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
		presence: PresenceRequiredForOthers, assist: AssistApprove},
	ActionCredentialProof: {class: ClassDirectory, recency: iam.RecencyStepUp,
		presence: PresenceRequired, assist: AssistApprove},
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
		selfRecency: iam.RecencyAny, presence: PresenceRequiredForOthers, assist: AssistApprove},
	// ENDING EVERY SESSION IN THE COMPANY is admin's, a person's, and a
	// recent proof's: it ends every person's authority at once, every
	// machine token included, and cannot be taken back. A Tier A token holds
	// admin, is its own credential rather than a machine token and is fresh
	// by construction, so the restore runbook's CLI step still runs on the
	// day nobody can sign in.
	ActionSessionInvalidate: {class: ClassAdmin, recency: iam.RecencyStepUp,
		presence: PresenceRequired, assist: AssistApprove},

	// --- artifacts ------------------------------------------------------- //
	//
	// A SHARE IS A RELATION AND THE APP GATES AUTHORING ONLY. Reading,
	// filling in data and sharing ask the role alone; creating, publishing,
	// renaming, adding an asset, reshaping the data and taking one out of
	// circulation ask the `artifacts` app beside it ([withApp]). See "An
	// artifact is decided per object, by its shares" in the package doc.
	//
	// LISTING IS EVERYBODY'S, because it is a filter over what reading
	// admits ([Visible]): a caller is shown the artifacts they may open and
	// told nothing about the rest.
	ActionArtifactList: {class: ClassEverybody, recency: iam.RecencyAny, assist: AssistRead},
	// THE CATALOGUE IS EVERYBODY'S: it names reads and who may make them,
	// and holds no row of any domain.
	ActionDashboardSources: {class: ClassEverybody, recency: iam.RecencyAny, assist: AssistRead},
	// THE FORMAT REFERENCE IS EVERYBODY'S, for the catalogue's reason: it is
	// the engine's vocabulary, and holds no row of any domain.
	ActionArtifactFormat: {class: ClassEverybody, recency: iam.RecencyAny, assist: AssistRead},
	// A SOURCE'S PREVIEW IS A BUILDER'S, the app and nothing else: the rows
	// are the source's own question's to decide, as the viewer.
	ActionDashboardSourceRead: {class: ClassApp, app: iam.GrantArtifacts, recency: iam.RecencyAny,
		assist: AssistRead},
	ActionArtifactRead: {class: ClassShared, minRole: artifacts.RoleViewer,
		recency: iam.RecencyAny, assist: AssistRead},
	// CREATING IS THE APP AND NOTHING ELSE: there is no artifact yet to hold
	// a relation to, and its creator becomes its owner.
	ActionArtifactCreate: {class: ClassApp, app: iam.GrantArtifacts,
		recency: iam.RecencyAny, steerable: true, assist: AssistApprove},
	ActionArtifactPublish: {class: ClassShared, app: iam.GrantArtifacts,
		minRole: artifacts.RoleEditor, recency: iam.RecencyAny, steerable: true,
		assist: AssistApprove},
	ActionArtifactUpdate: {class: ClassShared, app: iam.GrantArtifacts,
		minRole: artifacts.RoleEditor, recency: iam.RecencyAny, steerable: true,
		assist: AssistApprove},
	ActionArtifactAsset: {class: ClassShared, app: iam.GrantArtifacts,
		minRole: artifacts.RoleEditor, recency: iam.RecencyAny, steerable: true,
		assist: AssistApprove},
	ActionArtifactSchema: {class: ClassShared, app: iam.GrantArtifacts,
		minRole: artifacts.RoleEditor, recency: iam.RecencyAny, steerable: true,
		assist: AssistApprove},
	ActionArtifactShare: {class: ClassShared, minRole: artifacts.RoleOwner,
		recency: iam.RecencyAny, assist: AssistApprove},
	ActionArtifactUnshare: {class: ClassShared, minRole: artifacts.RoleOwner,
		recency: iam.RecencyAny, assist: AssistApprove},
	// EVERY PARTICIPANT REACHES THE DATA, and the collection decides: a
	// viewer may append to a collection that takes contributions, and an
	// editor may be kept out of another's private rows.
	ActionArtifactDataRead: {class: ClassShared, minRole: artifacts.RoleViewer,
		recency: iam.RecencyAny, assist: AssistRead},
	ActionArtifactDataQuery: {class: ClassShared, minRole: artifacts.RoleViewer,
		recency: iam.RecencyAny, assist: AssistRead},
	ActionArtifactDataWrite: {class: ClassShared, minRole: artifacts.RoleViewer,
		recency: iam.RecencyAny, steerable: true, assist: AssistApprove},
	// DISCARDING A NEWER VERSION is a person's, present: refused to every
	// seat before the class is read, and to every machine token.
	ActionArtifactForce: {class: ClassShared, app: iam.GrantArtifacts,
		minRole: artifacts.RoleEditor, humanOnly: true, presence: PresenceRequired,
		recency: iam.RecencyAny, assist: AssistApprove},
	// TRASH AND RESTORE are the owner's, beside the app, and never an
	// agent's: a trashed artifact stops serving at once.
	ActionArtifactTrash: {class: ClassShared, app: iam.GrantArtifacts,
		minRole: artifacts.RoleOwner, humanOnly: true, recency: iam.RecencyAny,
		assist: AssistApprove},
	ActionArtifactRestore: {class: ClassShared, app: iam.GrantArtifacts,
		minRole: artifacts.RoleOwner, humanOnly: true, recency: iam.RecencyAny,
		assist: AssistApprove},
	// A PURGE IS ADMIN'S, A PERSON'S AND A RECENT PROOF'S, as every other
	// purge is: nothing undoes it.
	ActionArtifactPurge: {class: ClassAdmin, humanOnly: true, irreversible: true,
		presence: PresenceRequired, recency: iam.RecencyStepUp, assist: AssistApprove},
	// OWNERSHIP MOVES BY A PERSON'S GESTURE, never a token's and never a
	// seat's: an agent handing its work to itself, or a pipeline moving
	// somebody's work away from them, is the escalation the rule closes.
	ActionArtifactTransfer: {class: ClassShared, minRole: artifacts.RoleOwner,
		humanOnly: true, presence: PresenceRequired, recency: iam.RecencyAny, assist: AssistApprove},
	ActionArtifactPin: {class: ClassShared, minRole: artifacts.RoleViewer,
		humanOnly: true, recency: iam.RecencyAny, assist: AssistApprove},
	ActionArtifactExport: {class: ClassShared, minRole: artifacts.RoleEditor,
		recency: iam.RecencyAny, assist: AssistRead},
	ActionArtifactUsage: {class: ClassAdmin, recency: iam.RecencyAny, assist: AssistRead},
	// A VIEWER'S BROWSER REPORTING A RENDER is everybody's: it carries no
	// artifact content.
	ActionArtifactReady: {class: ClassEverybody, recency: iam.RecencyAny, assist: AssistApprove},
	// A FRAME'S TICKET is everybody's as a gate: the artifact's own read
	// decides what it opens. See [ActionArtifactTicket].
	ActionArtifactTicket: {class: ClassEverybody, recency: iam.RecencyAny, assist: AssistApprove},

	// --- artifact folders and the company's brand -------------------- //
	//
	// A FOLDER IS DECIDED AS AN ARTIFACT IS, on [KindArtifactFolder]: its
	// owner and its top-level folder's shares. Creating one is the app;
	// the parent's contributor role is asked beside it
	// ([ActionArtifactFolderFile]).
	ActionArtifactFolderCreate: {class: ClassApp, app: iam.GrantArtifacts,
		recency: iam.RecencyAny, steerable: true, assist: AssistApprove},
	ActionArtifactMove: {class: ClassShared, app: iam.GrantArtifacts,
		minRole: artifacts.RoleEditor, recency: iam.RecencyAny, steerable: true, assist: AssistApprove},
	ActionArtifactFolderFile: {class: ClassShared, app: iam.GrantArtifacts,
		minRole: artifacts.RoleContributor, recency: iam.RecencyAny, steerable: true, assist: AssistApprove},
	// MANAGING A FOLDER is the owner's and a person's: a seat that renamed,
	// moved or removed a folder a team files into would reorganise work it
	// does not own, and a model persuaded by a row it read must not be able
	// to.
	ActionArtifactFolderRename: {class: ClassShared, app: iam.GrantArtifacts,
		minRole: artifacts.RoleOwner, humanOnly: true, recency: iam.RecencyAny, assist: AssistApprove},
	ActionArtifactFolderMove: {class: ClassShared, app: iam.GrantArtifacts,
		minRole: artifacts.RoleOwner, humanOnly: true, recency: iam.RecencyAny, assist: AssistApprove},
	ActionArtifactFolderRemove: {class: ClassShared, app: iam.GrantArtifacts,
		minRole: artifacts.RoleOwner, humanOnly: true, recency: iam.RecencyAny, assist: AssistApprove},
	ActionArtifactFolderUnfile: {class: ClassShared, app: iam.GrantArtifacts,
		minRole: artifacts.RoleOwner, humanOnly: true, recency: iam.RecencyAny, assist: AssistApprove},
	// THE BRAND IS EVERYBODY'S TO READ, and an administrator's, a person's
	// and a recent proof's to change: an adoption restyles every page the
	// company publishes, as far-reaching as a purge is irreversible.
	ActionBrandRead: {class: ClassEverybody, recency: iam.RecencyAny, assist: AssistRead},
	ActionBrandAdopt: {class: ClassAdmin, humanOnly: true,
		presence: PresenceRequired, recency: iam.RecencyStepUp, assist: AssistApprove},
	ActionBrandClear: {class: ClassAdmin, humanOnly: true,
		presence: PresenceRequired, recency: iam.RecencyStepUp, assist: AssistApprove},

	// --- the Dashboard Assistant (ADR-0050 to ADR-0053) ---
	//
	// THE ASSISTANT'S OWN VERBS, each never the assistant's
	// ([assistantNever]), HUMAN-ONLY and needing a PERSON PRESENT: a grant
	// that could converse, approve or read a conversation would be the
	// assistant acting on its own behalf, and approving its own proposal is
	// the one gesture the whole design keeps from it.
	// TestTheHumanOnlyListIsExactlyTheIntendedSet holds every `assistant.`
	// row here to all three.
	//
	// EVERYBODY'S BY CLASS, and still only a PERSON's: the surface answers
	// a machine token, a service account and a Tier A token's session
	// 403 assistant_not_available through the one predicate the guard
	// derives grants by (iam.Assistable), because using the assistant is
	// asking it to act as you, and only a person signed in through their
	// own browser has a you it may act as (decision D04).
	ActionAssistantConverse: {class: ClassEverybody, humanOnly: true,
		presence: PresenceRequired, recency: iam.RecencyAny, assist: AssistApprove},
	ActionAssistantConversationRead: {class: ClassEverybody, humanOnly: true,
		presence: PresenceRequired, recency: iam.RecencyAny, assist: AssistRead},
	ActionAssistantApprove: {class: ClassEverybody, humanOnly: true, recency: iam.RecencyAny,
		presence: PresenceRequired, assist: AssistApprove},
	ActionAssistantDecline: {class: ClassEverybody, humanOnly: true, recency: iam.RecencyAny,
		presence: PresenceRequired, assist: AssistApprove},
	ActionAssistantAuditRead: {class: ClassAdmin, humanOnly: true,
		recency: iam.RecencyStepUp, presence: PresenceRequired, assist: AssistRead},
}

// RecencyOf reports how recent a proof a verb asks for, and whether the table
// knows the verb at all.
//
// EXPORTED FOR THE WALKS, for a caller that has to SAY what a refusal it
// did not make would have needed, on [ClassOf]'s terms, and for a
// registration that refuses a verb its transport could not refuse in its own
// words (internal/api/queries' postures): a caller that read this and decided
// for itself would be the second implementation this package exists to
// remove. Use [Decide].
func RecencyOf(a Action) (iam.Recency, bool) {
	r, ok := rules[a]
	return r.recency, ok
}

// IrreversibleOf reports whether nothing undoes a verb ([rule.irreversible]),
// and whether the table knows the verb at all.
//
// EXPORTED FOR WHAT SHOWS A GESTURE TO A PERSON before they make it (the
// Dashboard Assistant's card), on [RecencyOf]'s terms: it says what the
// gesture is, and decides nothing.
func IrreversibleOf(a Action) (bool, bool) {
	r, ok := rules[a]
	return r.irreversible, ok
}

// SelfRecencyOf reports the proof a verb asks of its SELF arm where that
// differs from [RecencyOf]'s, and empty where the row asks one window of
// everybody — see [rule.selfRecency].
//
// EXPORTED on [RecencyOf]'s terms. Use [Decide].
func SelfRecencyOf(a Action) (iam.Recency, bool) {
	r, ok := rules[a]
	return r.selfRecency, ok
}

// PresenceOf reports which arms of a verb need a person present, and whether
// the table knows the verb at all — see [Presence].
//
// EXPORTED on [RecencyOf]'s terms. Use [Decide].
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

// AnyOf reports the apps a [ClassAnyApp] verb admits on, any one of them —
// see [rule.anyOf] — and whether the table knows the verb at all. Nil for
// every other class.
//
// EXPORTED FOR THE WALKS, and for a caller that has to NAME the set a refusal
// it did not make would have named, on [ClassOf]'s terms. A copy, so no
// caller edits the row. Use [Decide].
func AnyOf(a Action) ([]iam.Grant, bool) {
	r, ok := rules[a]
	return slices.Clone(r.anyOf), ok
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

// MinRoleOf reports the least artifact role a verb's row admits (see
// [rule.minRole]), empty for a row that is not [ClassShared], and whether the
// table knows the verb at all.
//
// EXPORTED FOR THE WALKS, on [RecencyOf]'s terms. Use [Decide].
func MinRoleOf(a Action) (artifacts.Role, bool) {
	r, ok := rules[a]
	return r.minRole, ok
}

// SteerableOf reports whether the requester ceiling spares an agent acting on
// its own object on this verb (see [rule.steerable]), and whether the table
// knows the verb at all.
//
// EXPORTED FOR THE WALKS, and for the one held outside this package that
// pairs it with the tools' own annotations: a steerable verb must be a write.
// Use [DecideWithin].
func SteerableOf(a Action) (bool, bool) {
	r, ok := rules[a]
	return r.steerable, ok
}

// AssistOf reports what the Dashboard Assistant may do with a verb (see
// [rule.assist]), and whether the table knows the verb at all.
//
// EXPORTED FOR THE WALKS, this package's and those that hold what the
// assistant is offered against what the table lets it take, on [RecencyOf]'s
// terms. Use [Decide].
func AssistOf(a Action) (AssistClass, bool) {
	r, ok := rules[a]
	if !ok {
		return "", false
	}
	return assistOf(a, r), true
}

// assistOf is what the assistant may do with verb a, whose row is r: never
// for a verb on [assistantNever], and the row's own nature otherwise.
func assistOf(a Action, r rule) AssistClass {
	if _, never := assistantNever[a]; never {
		return AssistNever
	}
	return r.assist
}

// AssistNeverReason is why the Dashboard Assistant never takes a verb, as a
// reviewer reads it, and whether the human-only list names the verb at all:
// for the walk that holds the list to the set it is meant to be.
func AssistNeverReason(a Action) (string, bool) {
	entry, never := assistantNever[a]
	return entry.why, never
}

// AssistNeverSentence is what the person is told when their assistant meets a
// verb on the human-only list: why it stays with them, in plain words, and
// where they do it themselves. Empty for a verb the list does not name.
//
// ONE SENTENCE PER FAMILY, so the person reads the same answer for every
// gesture of one kind (inviting somebody and creating them both hand out a
// sign-in link) and never a verb's internal name.
func AssistNeverSentence(a Action) string { return assistantNever[a].tell }

// neverEntry is one verb of the human-only list: the reason a reviewer reads,
// and the sentence the person is told.
type neverEntry struct{ why, tell string }

// The sentences the person is told, one per family of the human-only list.
const (
	tellSignIn = "Creating a person or an invitation, or resetting somebody's password or " +
		"second factor, hands out a way to sign in, so it stays with you: use People in " +
		"Settings."
	tellToken = "Creating or revoking an access token hands out or ends a credential, so it " +
		"stays with you: use API tokens in Settings."
	tellProof = "Changing how somebody proves who they are stays with them: they do it from " +
		"their Account page."
	tellSessions = "Signing people out stays with you: use People in Settings."
	tellSecret   = "A secret's value never passes through the assistant, so this stays with " +
		"you: use Secrets in Settings."
	tellSetup = "Connecting an integration means entering its credentials, so it stays with " +
		"you: use Integrations."
	tellFrame = "An artifact's page opens from the artifact itself: open it from Artifacts."
	tellOwn   = "The assistant never approves, declines or reads its own conversations: that " +
		"stays with you."
	tellSeat      = "Only an agent does this, as part of its own work."
	tellKnowledge = "Asking the knowledge base's own model would spend outside this " +
		"conversation's budget, so the assistant searches the pages itself instead."
	tellReady = "Only an open artifact reports that it has finished drawing."
)

// assistantNever is THE HUMAN-ONLY LIST: every verb the Dashboard Assistant
// never takes, whoever approved what, each with the reason a reviewer reads
// and the sentence the person is told (ADR-0052). Every other verb is the
// person's own through their assistant, as a read or as the one change they
// approved, because a grant is the person's own session narrowed and never
// widened (ADR-0050, ADR-0051).
//
// FOUR KINDS OF VERB AND NO OTHER, and a verb joins only in a reviewed diff
// that names which:
//
//   - a sign-in or session ceremony: how somebody proves who they are, or
//     the sessions and tokens that carry it. Creating a person and inviting
//     one are here because the answer IS a sign-in link, a bearer
//     credential, which is why a seat is added through the assistant and a
//     person is not;
//   - an answer, or a request body, that carries a bearer credential or
//     secret material in clear, which would then sit in a model's context,
//     a transcript and a stored proposal;
//   - the assistant's own decisions, so it can never approve itself;
//   - a gesture whose effect a grant would falsify or hide: a model spend
//     outside the turn's own meter, and a render only an open page reports;
//
// plus the one seat-only verb, asking a colleague (`a2a_ask`): no person takes
// it either, so parity alone keeps it from the person's assistant.
var assistantNever = map[Action]neverEntry{
	ActionCredentialWrite: {"a machine token's mint answers the token in clear, and revoking " +
		"one is a session ceremony", tellToken},
	ActionCredentialProof: {"changes how somebody proves who they are: a second factor, the " +
		"recovery codes or a password", tellProof},
	ActionSignInWrite: {"creating a person or an invitation answers a sign-in link, a bearer " +
		"credential in clear, and a password reset or a second factor reset is a sign-in " +
		"ceremony", tellSignIn},
	ActionSessionEnd:        {"a session ceremony: it signs somebody out", tellSessions},
	ActionSessionInvalidate: {"a session ceremony: it signs everybody out, irreversibly", tellSessions},
	ActionSecretReveal:      {"answers a credential's value in clear", tellSecret},
	ActionSecretWrite: {"a secret's value travels in the request body, so a model would author " +
		"it and a transcript would keep it", tellSecret},
	ActionSetupConnect: {"an integration's inputs carry vendor credentials in the request body, " +
		"and provisioning mints vendor secrets", tellSetup},
	ActionArtifactTicket: {"a view ticket is a bearer credential bound to the session that " +
		"opened the frame", tellFrame},
	ActionAssistantConverse:         {"the assistant's own verb: it never acts on its own behalf", tellOwn},
	ActionAssistantConversationRead: {"the assistant's own verb: it never acts on its own behalf", tellOwn},
	ActionAssistantApprove:          {"the assistant never approves its own proposal", tellOwn},
	ActionAssistantDecline:          {"the assistant never decides its own proposal", tellOwn},
	ActionAssistantAuditRead: {"a compliance read of somebody's conversations is the " +
		"administrator's own, recorded beside their name", tellOwn},
	ActionKnowledgeAnswer: {"a model call inside a model turn, spent outside the turn's own " +
		"budget meter; the assistant reads the same pages itself", tellKnowledge},
	ActionArtifactReady: {"render telemetry an open frame reports; a grant renders nothing, " +
		"so its report would falsify the service objective", tellReady},
	ActionColleagueAsk: {"an agent seat's own verb, which no person takes", tellSeat},
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

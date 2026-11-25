package authz

import (
	"maps"
	"slices"
	"sync"

	"github.com/crewlet/crewlet/internal/artifacts"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/toolname"
)

// The verbs this engine authorizes.
//
// NAMED AFTER THE VERB, NOT THE SURFACE: `PATCH /worklet/items/{key}` and the
// `update_work_item` tool are one action asked two ways, so one constant, and
// the operator MCP and a seat's tools cannot answer differently about a write.
//
// WHERE A TOOL EXISTS, ITS NAME IS THE ACTION, so a walk in
// internal/agent/builtin holds this table against the registered tools in BOTH
// directions (walks here only check the table against itself). A verb with no
// tool spells its name with a DOT (`pages.trash`, `config.read`).
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
	// A PROJECT'S FILES are its work's output, so reading one is reading the
	// board, asked of the PROJECT it is kept in (the object, by its key).
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
	// AN AGENT SEAT'S OWN EXECUTOR SESSIONS: the caller's own state, an
	// agent's alone — see [rule.seatOnly].
	ActionSessionsList Action = "list_sessions"
	ActionSessionRead  Action = toolname.ReadSession

	// --- a colleague's ordinary work -------------------------------- //
	//
	// AN ASK IS AN AGENT'S, the only one in this block: it spends the
	// colleague seat's turn — see [rule.seatOnly].
	ActionColleagueAsk Action = toolname.A2AAsk
	ActionWorkCreate   Action = toolname.CreateWorkItem
	ActionWorkUpdate   Action = "update_work_item"
	ActionWorkComment  Action = toolname.CommentOnWorkItem
	ActionWorkMerge    Action = "merge_work_item"
	ActionPageCreate   Action = "write_page"
	ActionPageSave     Action = "save_page"
	ActionPageComment  Action = "comment_on_page"
	// Keeping a file in a project is ordinary work, as filing an item is.
	ActionFileWrite Action = "write_project_file"

	// ActionWorkThread admits a post on a work item's THREAD over HTTP and
	// decides nothing else: who may post is [ActionWorkComment]'s decision,
	// made by the tool once it has read the item. DOTTED: a route admits before
	// the item is read, and on the comment's verb a person without the app
	// would be refused the one thread they were asked on.
	ActionWorkThread Action = "work.thread"

	// --- the company's code ----------------------------------------- //
	//
	// THE GITLET APP AT THE ROUTE, AND DECIDED PER REPOSITORY behind it: a
	// principal without [iam.GrantGitlet] is refused before any repository is
	// read (ADR-0110); who among the holders may read, fetch or push is a GRANT
	// the serving node derives from the chart for the repository a request
	// names (gitlet.GrantOf, ADR-0088), which the route cannot ask before the
	// repository is read. DOTTED: no tool is named.
	//
	// ActionCodeRead admits Gitlet's rows on REST or the socket.
	ActionCodeRead Action = "code.read"
	// ActionCodeGit admits the git transport and the file and ZIP downloads;
	// the grant then decides by service: a push is refused at its advertisement
	// to a principal that may push nothing, each command judged by the ref rule.
	ActionCodeGit Action = "code.git"

	// THE CODE READ TOOLS, decided as the REST reads above are: judged per
	// repository by the grant the SERVING node derives from its chart (a
	// listing leaves out what the asker may not read, a read of one such
	// repository is refused `not_permitted`). ActionCodeReadFile is the tool
	// `code_read`, named apart from the dotted ActionCodeRead.
	ActionCodeListRepos Action = "code_list_repos"
	ActionCodeGetRepo   Action = "code_get_repo"
	ActionCodeResolve   Action = "code_resolve"
	ActionCodeReadFile  Action = "code_read"
	ActionCodeDiff      Action = "code_diff"
	ActionCodeCompare   Action = "code_compare"
	ActionCodeLandedIn  Action = "code_landed_in"
	ActionCodeLog       Action = "code_log"
	ActionCodeBlame     Action = "code_blame"
	// AND THE CHANGE READS, the same footing: each held where answered to the
	// grant on the change's repository.
	ActionCodeListChanges Action = "code_list_changes"
	ActionCodeGetChange   Action = "code_get_change"
	// ActionCodeSearch is the tool `code_search`, each repository judged by the
	// grant its answering Gitlet node derives. The same-named question on REST
	// and the socket is decided by ActionCodeRead.
	ActionCodeSearch Action = "code_search"
	// ActionCodeRepoReaders is WHO READ a `visibility: unit` repository: the
	// operator tool and the query registry's question, ONE verb asked two ways.
	ActionCodeRepoReaders Action = "code_repo_readers"
	// ActionCodeMarkInbox is a person's gesture over their own CODE notices:
	// marks are kept by each domain on its own log (internal/inbox), so each
	// domain has a verb of its own.
	ActionCodeMarkInbox Action = "code_mark_inbox"
	// ActionCodeMarkViewed is a person's marks over a change's reviewed files,
	// kept on the code log beside their inbox's.
	ActionCodeMarkViewed Action = "code_mark_viewed"
	// ActionArtifactMarkInbox is a person's gesture over their own COMMENT
	// notices, the artifacts log's half, under no app: a comment is a relation.
	ActionArtifactMarkInbox Action = "artifact_mark_inbox"
	// ActionHomeBoardSave is the tool `save_home_board`: a person's Home board,
	// the widgets they placed and how they arranged them.
	ActionHomeBoardSave Action = "save_home_board"
	// ActionAssistantInstructionsSave is the tool `save_assistant_instructions`:
	// a person's own instructions on how Ask Crewlet writes to them.
	ActionAssistantInstructionsSave Action = "save_assistant_instructions"

	// THE CODE WRITE TOOLS, decided as the git transport's push is: judged per
	// repository where decided (an engine write by the grant's ref rule and the
	// protections on the building Gitlet node and again at the committer; a
	// lifecycle by the one lifecycle rule on the data node; gitlet.GrantOf,
	// ADR-0088). Named apart from the dotted ActionCodeRead and ActionCodeGit.
	ActionCodeCommitFiles   Action = "code_commit_files"
	ActionCodeRevert        Action = "code_revert"
	ActionCodeCherryPick    Action = "code_cherry_pick"
	ActionCodeCreateBranch  Action = "code_create_branch"
	ActionCodeDeleteBranch  Action = "code_delete_branch"
	ActionCodeCreateTag     Action = "code_create_tag"
	ActionCodeDeleteTag     Action = "code_delete_tag"
	ActionCodeCreateRepo    Action = "code_create_repo"
	ActionCodeUpdateRepo    Action = "code_update_repo"
	ActionCodeDeleteRepo    Action = "code_delete_repo"
	ActionCodeRestoreRepo   Action = "code_restore_repo"
	ActionCodeProtectBranch Action = "code_protect_branch"
	// AND THE CHANGE WRITES, decided by the repository's committer from its own
	// chart (D21: who may push the source to open one; its owner, a maintainer
	// or a person to steer it).
	ActionCodeOpenChange   Action = "code_open_change"
	ActionCodeUpdateChange Action = "code_update_change"
	ActionCodeCloseChange  Action = "code_close_change"
	// And its conversation, each write decided by the change's writer from its
	// own chart (D21: who may ask, review, answer, delete or dismiss).
	ActionCodeRequestReview   Action = "code_request_review"
	ActionCodeReviewChange    Action = "code_review_change"
	ActionCodeCommentOnChange Action = "code_comment_on_change"
	ActionCodeDeleteComment   Action = "code_delete_comment"
	ActionCodeResolveFinding  Action = "code_resolve_finding"
	// Suggestions applied, and a change rebased (a forced push of its source),
	// are engine writes judged as every engine write is.
	ActionCodeApplySuggestions Action = "code_apply_suggestions"
	ActionCodeRebaseChange     Action = "code_rebase_change"
	// Merge, queue and dequeue: authority, gate and a person's override are
	// judged by the committer (M5-D2, M5-D7).
	ActionCodeMergeChange   Action = "code_merge_change"
	ActionCodeDequeueChange Action = "code_dequeue_change"
	// A merged change reverted by a change of its own, opened by the push of
	// its revert's branch (M5-D25), judged as every engine write is.
	ActionCodeRevertChange Action = "code_revert_change"
	// A commit's CHECKS: failures read by the repository's grant; a rerun
	// decided by the checks writer from its own chart (a seat reruns only what
	// its infrastructure ended, on a repository it may write or a change it
	// wrote); a person's own CI reporting a check, a person's alone.
	ActionCodeGetCheckFailures Action = "code_get_check_failures"
	ActionCodeRerunChecks      Action = "code_rerun_checks"
	ActionCodeReportCheck      Action = "code_report_check"
	// ActionCodePurgeUnreachable is a person's purge of every object no ref
	// reaches, served by the repository's maintainer.
	ActionCodePurgeUnreachable Action = "code_purge_unreachable"
	// ActionCodeImportRepo is a person's create of a repository filled from
	// one at another host, once or as a pull mirror.
	ActionCodeImportRepo Action = "code_import_repo"

	// --- the company's atlas ----------------------------------------- //
	//
	// ActionAtlasSearch is the tool `atlas_search`. The app and nothing
	// narrower decides it (ADR-0127).
	ActionAtlasSearch Action = "atlas_search"
	// ActionAtlasGet is the tool `atlas_get`: one entry and what relies on it.
	ActionAtlasGet Action = "atlas_get"
	// ActionAtlasTrace is the tool `atlas_trace`: an entry's dependents or
	// dependencies.
	ActionAtlasTrace Action = "atlas_trace"
	// The tools a person changes the atlas with, each one claim on the atlas's
	// log in the person's own name.
	ActionAtlasAddEntry Action = "atlas_add_entry"
	ActionAtlasCorrect  Action = "atlas_correct"
	ActionAtlasConfirm  Action = "atlas_confirm"
	ActionAtlasRelate   Action = "atlas_relate"
	ActionAtlasUnrelate Action = "atlas_unrelate"
	ActionAtlasSetOwner Action = "atlas_set_owner"
	ActionAtlasRetire   Action = "atlas_retire"
	ActionAtlasRestore  Action = "atlas_restore"
	ActionAtlasMerge    Action = "atlas_merge"
	ActionAtlasSplit    Action = "atlas_split"
	// ActionAtlasPurge is the tool `atlas_purge`: erased everywhere, history
	// included, and kept from coming back.
	ActionAtlasPurge Action = "atlas_purge"
	// ActionAtlasRefresh is the tool `atlas_refresh`: a person's ask to read
	// one source now.
	ActionAtlasRefresh Action = "atlas_refresh"
	// ActionAtlasRecord is the tool `atlas_record`: an agent's record of what
	// a tool call of its own turn showed it (ADR-0132).
	ActionAtlasRecord Action = "atlas_record"
	// ActionAtlasDispute is the tool `atlas_dispute`: an agent's word that
	// something is wrong or gone; removes nothing.
	ActionAtlasDispute Action = "atlas_dispute"

	// --- somebody's own record -------------------------------------- //
	ActionPersonRead Action = "get_person"
	ActionMyWork     Action = "my_work"
	ActionInboxRead  Action = "work_inbox"
	ActionInboxMark  Action = "mark_inbox"
	// ActionInbox is reading a person's COMPOSED inbox, every domain's notices
	// in one list (the query registry's `inbox`). DOTTED: a seat is woken, not
	// shown a list; `work_inbox` stays the tracker's own tool.
	ActionInbox         Action = "inbox.read"
	ActionPinsSet       Action = "set_pins"
	ActionViewSave      Action = "save_work_view"
	ActionViewDelete    Action = "delete_work_view"
	ActionPrioritiesSet Action = "set_priorities"

	// --- a field inside a write, decided against what was read ------ //
	//
	// DOTTED: asked FROM inside a tool. The object is not in the arguments
	// (the project a task is filed under, or a label declared in, comes out of
	// the stored row), so the registration gate cannot form it and the tool
	// asks once it has read.
	ActionWorkRoute      Action = "work.route"
	ActionProjectPolicy  Action = "work.project.policy"
	ActionProjectArchive Action = "work.project.archive"

	// --- a person's arrangement of a board --------------------------- //
	//
	// A PLACE MOVES ONE CARD between two neighbours, or into another lane, on
	// a board a PERSON is looking at (the HTTP rank route adapts the tool). No
	// seat has it: a seat moves work with update_work_item and does not
	// reorder a board it cannot see, so its row is human-only.
	ActionWorkPlace Action = "place_work_item"

	// --- a container's own policy ----------------------------------- //
	ActionProjectWrite   Action = toolname.WriteProject
	ActionCatalogueWrite Action = "write_work_catalogue"
	ActionPageRename     Action = "pages.rename"

	// --- a page every seat is handed ---------------------------------- //
	//
	// DOTTED: asked from inside every page write once the page's container is
	// known, and the container is the tool-skills one.
	ActionSkillPageWrite Action = "pages.skill.write"

	// --- taking something out of circulation ------------------------ //
	ActionWorkRemove  Action = "remove_work_item"
	ActionWorkRestore Action = "restore_work_item"
	// A MOVE TAKES AN ITEM OUT OF ITS PROJECT, so it is decided as a removal:
	// asked of the project the item is IN (read from the stored row), whose
	// lead alone may take work out — a colleague may file, not carry away.
	ActionWorkMove    Action = "move_work_item"
	ActionPageTrash   Action = "pages.trash"
	ActionPageRestore Action = "pages.restore"
	// A PAGE PUBLISHED, OR TAKEN BACK TO A DRAFT, is the same kind of move: no
	// search returns a draft, so status decides whether the company reads the
	// page. DOTTED: save_page asks it when a `status` differs from the page's,
	// as it asks the rename for a `title`.
	ActionPagePublish Action = "pages.publish"
	// REMOVING A FILE is decided as removing an item is: by the project's
	// lead; a colleague may not take somebody else's report away.
	ActionFileRemove Action = "remove_project_file"

	// --- beyond recovery -------------------------------------------- //
	ActionWorkPurge Action = "work.purge"
	ActionPagePurge Action = "pages.purge"
	// A ROOM'S MESSAGES DESTROYED, and its horizon set. DOTTED: each is a
	// human-surface route, said in nobody's voice (internal/chatlet).
	ActionChatErase     Action = "chat.erase"
	ActionChatPrune     Action = "chat.prune"
	ActionChatRetention Action = "chat.channel.retention"

	// --- what somebody wrote ---------------------------------------- //
	ActionPageCommentEdit   Action = "pages.comment.edit"
	ActionPageCommentRemove Action = "pages.comment.remove"
	ActionWorkCommentEdit   Action = "work.comment.edit"
	ActionWorkCommentRemove Action = "work.comment.remove"
	// ActionWithdrawAsk is a seat withdrawing its own question, or a call its
	// executor holds for approval (ADR-0111): an agent's alone.
	ActionWithdrawAsk Action = toolname.WithdrawAsk
	// ActionRequestAccess is a seat asking a person for one grant from a
	// closed list (ADR-0118), on a work item.
	ActionRequestAccess Action = toolname.RequestAccess

	// --- the org chart ---------------------------------------------- //
	//
	// The org chart (`roles:`, `units:`) is written through the same routes and
	// revision as every setting. TWO VERBS, not one per field, because the
	// question is the same for every change — is the place it reaches inside a
	// unit this principal leads — asked of each place the write reaches
	// ([ClassSubtree]). What a lead may not change (a setting, a credential
	// anywhere in the chart) is decided by [ActionConfigWrite].
	ActionOrgWrite Action = "config.org.write"
	ActionOrgRead  Action = "config.org.read"

	// --- whether a seat works, and what its turn is doing ------------ //
	//
	// A PERSON'S DECISION ABOUT A SEAT, never a seat's: pause, resume, a note
	// to its running turn, an answer to a parked executor. Each is decided on
	// the SEAT by the owner-or-lead class (holder, whoever leads it, or admin)
	// INSIDE its tool, because the seat is not what the arguments state: a
	// handle resolves through the org (`@sre`), a turn names the seat only the
	// node running it can say, a run names the seat its row recorded.
	ActionSeatPause      Action = "pause_seat"
	ActionSeatResume     Action = "resume_seat"
	ActionTurnSteer      Action = "steer_turn"
	ActionExecutorAnswer Action = "answer_executor"
	// ActionScheduleRun is the tool `run_schedule`: a person running a schedule
	// now. It wakes the runner seats, so it is decided on each, as a pause is.
	ActionScheduleRun Action = "run_schedule"

	// ActionKnowledgeAnswer is a person's question answered by a model from
	// what the company has written down, at its expense. A READ in what it
	// discloses (pages and board, each searched only for a holder of its app),
	// and a person's alone: it runs on the auxiliary model of the asker's
	// bound seat, and a seat has search_knowledge and a model of its own.
	ActionKnowledgeAnswer Action = "answer_knowledge"

	// --- using the chat ---------------------------------------------- //
	//
	// THE NINE A SEAT HOLDS, which a person's surface serves too. Named after
	// the tools internal/chatlet declares.
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
	// beside its name (rooms are furniture a company arranges, a seat
	// corrects by saying the correction, a read cursor is a person's).
	ActionChatCreate  Action = "create_channel"
	ActionChatOpenDM  Action = "open_dm"
	ActionChatUpdate  Action = "update_channel"
	ActionChatMembers Action = "set_channel_members"
	ActionChatAdd     Action = "add_channel_members"
	ActionChatEdit    Action = "edit_message"
	ActionChatDelete  Action = "delete_message"
	ActionChatMark    Action = "mark_chat_read"
	// ActionChatArchive is archiving or reopening a room: asked from inside
	// `update_channel` once the stored room is read, because who may (creator,
	// unit lead or admin) is on the row, never in the arguments. DOTTED.
	ActionChatArchive Action = "chat.channel.archive"
	// ActionChatAutoJoin is a public room's auto-join setting, asked from
	// inside `update_channel`, DOTTED for the archive's reason.
	ActionChatAutoJoin Action = "chat.channel.autojoin"

	// --- the company's own controls --------------------------------- //
	ActionConfigRead  Action = "config.read"
	ActionConfigWrite Action = "config.write"
	// THE COMPANY'S PRINCIPLES (mission, vision, standing policies), one
	// resource (`GET` and `PUT /config/charter`). THEIR OWN VERBS, decided as
	// [ActionConfigRead] and [ActionConfigWrite] decide the whole document,
	// because they are the one part of it the Dashboard Assistant may carry
	// (ADR-0052's exception) and an assistant column is per verb: on the
	// document's verbs it would be one approval from every secret reference,
	// seat and provider; on these it reaches three fields of prose.
	ActionPrinciplesRead  Action = "config.principles.read"
	ActionPrinciplesWrite Action = "config.principles.write"
	ActionSecretList      Action = "secrets.list"
	ActionSecretReveal    Action = "secrets.reveal"
	ActionSecretWrite     Action = "secrets.write"
	ActionSetupRead       Action = "setup.read"
	ActionSetupConnect    Action = "setup.connect"
	// ActionSetupAccessWrite is saving what each agent seat may do on one
	// integration (`PUT /setup/integrations/{kind}/access`). ITS OWN VERB, NOT
	// [ActionSetupConnect]: a connect carries vendor credentials and a pass
	// that mints them, which the assistant never takes; this carries neither,
	// so a person's assistant may prepare it for their approval.
	ActionSetupAccessWrite Action = "setup.access.write"
	// ActionSetupAccessRaise is that save when it raises a seat to an
	// administration role at a vendor (Datadog Admin Role, GitLab Maintainer
	// or Owner, Jira Administrators, Confluence Admin, Mattermost System
	// Admin, GitHub write on Contents or Workflows). Asked ON TOP of
	// [ActionSetupAccessWrite] and refused to a machine token: making an agent
	// a vendor administrator is a person's decision at a keyboard.
	ActionSetupAccessRaise Action = "setup.access.raise"
	// ActionFleetOperate is the deployment's own controls: a backup, retention,
	// the capacity window, the fleet broker's membership, discarding a wake
	// feed's dead letter, and a verify of a Gitlet node's cache of one
	// repository, which reads every byte of THIS NODE's copy, holds a
	// maintenance slot and memory, and with a repair rewrites the copy every
	// fetch on the node is served from: an operation on the node, as a backup
	// is, not something a repository's grant decides.
	ActionFleetOperate Action = "fleet.operate"
	// ActionFleetRead is those controls READ (maintenance status, the value a
	// reanchor must echo, dead letters, a verify's outcome). Admin's, and its
	// own verb because it asks for no step-up: a status an operator cannot see
	// without re-proving who they are is one they stop checking.
	ActionFleetRead Action = "fleet.read"
	// ActionModelsCheck runs a model's checks on the answering node: the
	// doctor (`POST /llm/providers/{key}/check`, a few tokens of a real call,
	// charged to no seat) and a host login's status read. ActionModelsRetry
	// clears a model's sit-out on every node, or probes it first. Both change
	// RUNTIME state the company already reaches, never config or a credential,
	// so they are admin's at any recency and an assistant's on approval; a
	// credential's write and a device sign-in are `secrets.write`.
	ActionModelsCheck Action = "models.check"
	ActionModelsRetry Action = "models.retry"
	// ActionAdvisorRun asks the Advisor to run now, or records a person's
	// decision on a finding: records the company keeps, never config or a
	// credential, which a fix's own route writes (ADR-0164).
	ActionAdvisorRun Action = "advisor.run"
	// ActionToolsRetry restarts an MCP server now (`POST
	// /mcp-servers/{name}/retry`). Like a model's retry it changes RUNTIME
	// state only, from the spec it runs.
	ActionToolsRetry Action = "tools.retry"
	// ActionToolsCheck tests an MCP server where it runs, never calling a tool
	// (`POST /mcp-servers/{name}/check`), or checks an address before a server
	// is added (`POST /mcp-probe`). Like a restart it changes no config or
	// credential: a test brings up a connection of its own and closes it. A
	// test of a DRAFT (`POST /mcp-checks`) runs what a write would, so it is
	// decided as that write, `config.write` and `secrets.write` on top, and an
	// address check as the write that would make the engine dial it,
	// `config.write` on top (internal/api/mcpapi).
	ActionToolsCheck Action = "tools.check"
	// ActionToolsApprove answers a call an agent's run made to a tool at Needs
	// approval, parked on the answer (`POST /mcp-approvals/{id}`, ADR-0077):
	// allow once, allow always, or deny. An administrator's, or a lead of the
	// seat whose run made it; a person's alone, never an agent's or the
	// assistant's, since the level exists so a person decides that call.
	// Always allow also writes the tool's level, so `config.write` on top, at
	// its step-up (internal/api/mcpapi).
	ActionToolsApprove Action = "tools.approve"
	// ActionSSHOperateApprove answers an agent's operate command over SSH, held
	// for a person (`POST /mcp-approvals/{id}`). An administrator's, or a
	// person `integrations.ssh.approvers` names, at the step-up window; never
	// a lead's, an agent's or the assistant's.
	ActionSSHOperateApprove Action = "ssh.operate.approve"
	// ActionGrowthSpendApprove, ActionGrowthPublicApprove and
	// ActionGrowthAdminApprove answer a growth integration's held spend,
	// public or admin act (`POST /mcp-approvals/{id}`, ADR-0171): an
	// administrator alone, present, at the step-up window; never a lead, a
	// named person, a delegate, a machine token or the assistant.
	ActionGrowthSpendApprove  Action = "growth.spend.approve"
	ActionGrowthPublicApprove Action = "growth.public.approve"
	ActionGrowthAdminApprove  Action = "growth.admin.approve"
	// ActionSSHRead runs an allowlisted read command on one of the agent's SSH
	// hosts (the server's gate holds the allowlist); an agent's alone by its
	// SSH grant, in its own run.
	ActionSSHRead Action = "ssh_read"
	// ActionSSHOperate asks to run any command on an SSH host, held for a
	// person's approval every time (ADR-0077); an agent's alone by its operate
	// grant, in its own run.
	ActionSSHOperate Action = "ssh_operate"
	// ActionReviewDelegated answers a request delegated to an agent seat:
	// allow, escalate or deny (ADR-0125). A delegate's alone, by the grant an
	// enabled delegated-decisions rule gives it, in its own run.
	ActionReviewDelegated Action = "review_delegated"
	// ActionDelegatedRead reads the delegated-decisions rules and what
	// delegates decided (`GET /delegated-decisions`): an administrator's, or a
	// person a rule names in escalate_to.
	ActionDelegatedRead Action = "delegated_decisions.read"
	// ActionSSHAskOperate asks a person, on a work item, to approve one exact
	// operate command before it is called, so the call runs without a second
	// prompt (ADR-0125). An agent's alone, by its operate grant.
	ActionSSHAskOperate Action = "ask_to_operate"
	// ActionAskToMerge asks to merge one change at exactly the head it names,
	// held for a delegate or a person (ADR-0125); approval merges that head as
	// the agent, whose own credential and the target's rules still decide.
	// Any agent's, in its own run.
	ActionAskToMerge Action = "ask_to_merge"
	// ActionAskToDeploy asks to deploy one commit to an environment, held
	// likewise; approval is handed back to the run, which deploys with its own
	// tools. Any agent's, in its own run.
	ActionAskToDeploy Action = "ask_to_deploy"
	// ActionReleaseApprove answers a merge or deploy an agent asked for
	// (`POST /mcp-approvals/{id}`). An administrator's, the seat's lead's, or a
	// person the kind's delegated-decisions rule names in escalate_to, at the
	// step-up window; never an agent's or the assistant's.
	ActionReleaseApprove Action = "release.approve"
	// ActionSSHRequester is a server reached FOR whoever woke the turn of an
	// agent holding an SSH grant: anybody of the company who may act, a
	// colleague seat among them, and nobody outside it.
	ActionSSHRequester Action = "ssh.requester.read"
	// ActionGrowthRequester is a growth vendor reached FOR whoever woke the
	// turn of an agent holding a growth lane: anybody of the company who may
	// act, a colleague seat among them, and nobody outside it.
	ActionGrowthRequester Action = "growth.requester.read"
	// ActionToolsRead is reading Settings › MCP & tools: one server whole
	// (`GET /mcp-servers/{name}`) and the curated catalogue. EVERYBODY'S, IN
	// TWO READINGS: whoever [ActionConfigRead] admits also reads launch lines,
	// failure sentences, credential headers and variables, what removal takes
	// with it and the community registry; everybody else reads each server's
	// condition and tools and the catalogue (internal/api/queries'
	// mcpmember.go). The servers' list is the question `mcp_servers_status`.
	ActionToolsRead Action = "tools.read"
	ActionAuditRead Action = "audit.read"
	// ActionFleetHealth is the tool `fleet_health`: every live node's own
	// heartbeat with its firing alarms. ActionCodeStatus is the tool
	// `code_status`: the Gitlet nodes' caches and most-served repositories, or
	// one repository's state on each. READS OF THE DEPLOYMENT, admin's as the
	// `fleet` question is, and a SEAT'S only where its role carries the health
	// grant ([SeatGrantHealth], `role.health_tools`); see [rule.seatGrant].
	ActionFleetHealth Action = "fleet_health"
	ActionCodeStatus  Action = "code_status"
	// ActionFleetHealthRead is the health read FOR whoever woke a granted
	// seat's turn: the requester ceiling's question of `fleet_health`
	// ([DecideWithin]), which the tool asks of that somebody before reading.
	// DOTTED. Not [ActionFleetHealth] again: a requester holds no admin and a
	// colleague seat no grant of the asker's, so the tool's own row would
	// refuse every turn anybody woke. What the seat reads is its grant's; this
	// decides only for whom.
	ActionFleetHealthRead Action = "fleet.health.read"

	// --- one seat's own trail, read by somebody else ----------------- //
	//
	// A SEAT'S MEMORY, SESSIONS AND TURNS, read over the API (diary, what it
	// learned, each turn's session and ending, prompts and responses). DOTTED:
	// no tool reads another seat's trail (a seat's own memory tools take no
	// handle, [ClassSelf]); and ITS OWN VERB rather than [ActionAuditRead]
	// because the object is ONE seat: a question naming one asks this, the
	// company-wide trail asks the other.
	ActionSeatTrailRead Action = "audit.seat.read"

	// --- the identity directory -------------------------------------- //
	//
	// SEVEN VERBS FOR TWENTY ROUTES: routes differ in what they do, not in how
	// they are decided. Three questions separate them: a person or the
	// directory, does it CHANGE anything, and for somebody's own credentials,
	// may a machine token acting as them make it ([Presence]).
	ActionDirectoryRead  Action = "iam.read"
	ActionDirectoryWrite Action = "iam.write"
	// ActionSeatsRead is everybody's read of WHO HOLDS EACH HUMAN SEAT: the
	// chart is everybody's overview and a seat whose holder nobody could see
	// would draw empty boxes. WHAT a caller sees of a holder is the route's to
	// mask per row (a login and a stage are the holder's and admin's, an open
	// invitation admin's).
	ActionSeatsRead Action = "iam.seats.read"
	// ActionCredentialWrite mints or revokes ONE PERSON's MACHINE TOKEN, a
	// personal access token or a service account's.
	ActionCredentialWrite Action = "iam.credential.write"
	// ActionCredentialProof changes how ONE PERSON PROVES WHO THEY ARE: a
	// second factor enrolled or replaced, recovery codes regenerated, a
	// password, second factor or recovery codes revoked. Asked from `/auth`
	// and `/iam`, so one row decides them whichever route a request took.
	//
	// ITS OWN VERB BESIDE [ActionCredentialWrite], at the same window, because
	// they differ in who must be PRESENT: a token revoking a token is
	// containment, a token changing how its owner signs in is the first half of
	// taking the account. One verb would admit a token on every proof change
	// and leave each route to refuse it.
	ActionCredentialProof Action = "iam.credential.proof"
	// ActionSignInWrite ISSUES OR CLEARS A WAY FOR SOMEBODY ELSE TO SIGN IN:
	// creating a person with a first password link, an invitation link, a
	// password reset link, resetting a second factor. Admin's, at the step-up
	// window, with a person present, as [ActionDirectoryWrite] is.
	//
	// ITS OWN VERB because three of its four answers carry a bearer credential
	// in clear (a link is a password) and the fourth clears how somebody
	// proves who they are: a ceremony the Dashboard Assistant never takes
	// ([assistantNever]). On [ActionDirectoryWrite] one assistant column would
	// decide both a permissions edit and the issue of a password.
	ActionSignInWrite Action = "iam.signin.write"
	ActionSessionEnd  Action = "iam.session.end"

	// ActionSessionInvalidate ends EVERY session in the company at once, the
	// restore runbook's last step. Its own verb: it cannot be taken back for
	// anybody and signs out the person making it.
	ActionSessionInvalidate Action = "iam.invalidate"

	// --- artifacts ---------------------------------------------------- //
	//
	// ONE ARTIFACT IS THE OBJECT OF EVERY ONE OF THEM: versions, files, assets,
	// data, presence and view tickets are decided as the artifact, by the
	// caller's role on it ([ClassShared]). Tools are named for what a seat and
	// an assistant call; dotted verbs are a person's HTTP gestures.
	//
	// READING. Listing is a FILTER, never a refusal: it answers the rows
	// reading admits ([Visible]). Reading one is everything a viewer sees,
	// history and presence included, so no separate verb for either.
	ActionArtifactList Action = "list_artifacts"
	ActionArtifactRead Action = "get_artifact"
	// ActionArtifactRender renders a version in the engine's browser as a
	// viewer sees it: a read of all get_artifact reads, data included.
	ActionArtifactRender Action = toolname.RenderArtifact
	// THE DASHBOARD SOURCE CATALOGUE is the engine's own vocabulary, read
	// before a board exists, so everybody's: what a source ANSWERS is decided
	// as each board's viewer.
	ActionDashboardSources Action = toolname.ListDashboardSources
	// THE FORMAT REFERENCE is the engine's own vocabulary too (a kind's index
	// shape, `brand.json`, `deck.json`), holding no row of any domain.
	ActionArtifactFormat Action = "get_artifact_format"
	// ActionDashboardSourceRead previews ONE engine source's rows before any
	// board exists (`GET /artifacts/sources/preview/{name}`): a builder's read,
	// so it asks the artifacts app, while the rows are decided by the source's
	// own question, as the viewer. ITS OWN VERB AND A READ: on
	// [ActionArtifactCreate] a GET was decided as a write and the assistant
	// could not read what the person could.
	ActionDashboardSourceRead Action = "artifacts.source.read"
	// AUTHORING, each asking the `artifacts` app beside the role.
	ActionArtifactCreate  Action = toolname.CreateArtifact
	ActionArtifactPublish Action = "publish_artifact"
	ActionArtifactUpdate  Action = "update_artifact"
	ActionArtifactAsset   Action = "upload_artifact_asset"
	ActionArtifactSchema  Action = "set_artifact_schema"
	// SHARING is the owner's: the owner, the lead of an agent owner, or admin.
	// What a share may name and grant is refused by the artifacts domain.
	ActionArtifactShare   Action = "share_artifact"
	ActionArtifactUnshare Action = "unshare_artifact"
	// THE DATA, which every participant reaches and the collection's rules
	// then decide row by row: the row admits, the domain decides, as
	// `work.thread` does.
	ActionArtifactDataRead  Action = "read_artifact_data"
	ActionArtifactDataQuery Action = "query_artifact_data"
	ActionArtifactDataWrite Action = "write_artifact_data"
	// COMMENTS (ADR-0154): reading threads is a viewer's, every write a
	// commenter's; the domain's writer keeps an edit to the remark's author
	// and a removal to its author or an administrator moderating.
	ActionArtifactCommentList    Action = "list_artifact_comments"
	ActionArtifactComment        Action = "comment_on_artifact"
	ActionArtifactCommentResolve Action = "resolve_artifact_comment"
	ActionArtifactCommentEdit    Action = "edit_artifact_comment"
	ActionArtifactCommentRemove  Action = "remove_artifact_comment"

	// ActionArtifactForce discards a newer version than the writer read: a
	// publish asks it from inside once the base is stale. A PERSON'S, PRESENT:
	// a seat merges and never throws away a colleague's version.
	ActionArtifactForce Action = "artifacts.publish.force"
	// TAKING ONE OUT OF CIRCULATION AND BACK is the owner's and a person's:
	// trashing stops every viewer at once, so a model persuaded by a row it
	// read must not be able to take a team's sheet offline.
	ActionArtifactTrash   Action = "artifacts.trash"
	ActionArtifactRestore Action = "artifacts.restore"
	// BEYOND RECOVERY is admin's, as every purge is; the trash reaper purges
	// an owner's trash once its term is up.
	ActionArtifactPurge Action = "artifacts.purge"
	// HANDING ONE TO ANOTHER SEAT is the only way ownership moves.
	ActionArtifactTransfer Action = "artifacts.transfer"
	// A person's own sidebar pin, on an artifact they may read.
	ActionArtifactPin Action = "artifacts.pin"
	// THE WHOLE DATA, whatever each collection's read rule: an editor's.
	ActionArtifactExport Action = "artifacts.export"
	// What the company's artifacts hold against its quotas: admin's.
	ActionArtifactUsage Action = "artifacts.usage"
	// A viewer's browser reporting a finished render, for the service
	// objective: everybody's, as it carries no artifact content.
	ActionArtifactReady Action = "artifacts.ready"
	// ActionArtifactTicket mints the view ticket, bound to the caller's
	// credential, a viewer's browser embeds a version with
	// (`POST /artifacts/{id}/tickets`). Everybody's as a gate: what the frame
	// opens is [ActionArtifactRead]'s decision. ITS OWN VERB because the
	// answer is a bearer credential the Dashboard Assistant never receives
	// ([assistantNever]).
	ActionArtifactTicket Action = "artifacts.ticket"

	// --- artifact folders and the company's brand -------------------- //
	//
	// A FOLDER GRANTS NOTHING (ADR-0047). Its visibility is decided as a
	// relation on its top-level folder ([KindArtifactFolder], read by
	// [ActionArtifactRead]'s row), and filing an artifact asks TWO decisions,
	// the artifact's ([ActionArtifactMove]) and the destination's
	// ([ActionArtifactFolderFile]), each through the requester ceiling.
	//
	// CREATING A FOLDER is the app (an agent creates subfolders only, held by
	// the artifacts writer) beside contributor on the parent.
	ActionArtifactFolderCreate Action = "create_artifact_folder"
	// FILING ONE ARTIFACT is an edit of where it lives: editor on it.
	ActionArtifactMove Action = "move_artifact"
	// FILING INTO A FOLDER, or creating one inside: contributor on the
	// destination, asked beside the move or the create.
	ActionArtifactFolderFile Action = "artifacts.folder.file"
	// MANAGING A FOLDER (rename, move, removal, which moves its contents up a
	// level, unfiling) is its owner's, the owner's lead's or admin's, and a
	// person's.
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
	// The assistant's own verbs, each dotted, landing here with the surface
	// that asks it.

	// ActionAssistantConverse is using the assistant at all. Whose conversation
	// it is is never this verb's question: the store answers 404 for anybody
	// but its owner (ADR-0053).
	ActionAssistantConverse Action = "assistant.converse"
	// ActionAssistantConversationRead is reading one's own conversations.
	ActionAssistantConversationRead Action = "assistant.conversation.read"
	// ActionAssistantApprove is a person approving one of their own proposals
	// (`POST /assistant/proposals/{id}/approve`). Everybody's: whose proposal
	// it is is the store's answer (ADR-0053), and what the write may do is the
	// write's own row, decided again when sent. Its step-up is asked by the
	// approval itself (ADR-0051).
	ActionAssistantApprove Action = "assistant.approve"
	// ActionAssistantDecline is a person declining one of their own proposals.
	ActionAssistantDecline Action = "assistant.decline"
	// ActionAssistantAuditRead is an administrator reading somebody else's
	// conversation for compliance (decision D06): admin's, with a recent proof,
	// each read recorded with the reader's name and reason and shown to the
	// conversation's owner.
	ActionAssistantAuditRead Action = "assistant.conversation.audit_read"
	// ActionSSHAuditRead is reading the SSH audit, each command's text opened.
	// A command may carry a credential typed inline, so it is a person's read.
	ActionSSHAuditRead Action = "ssh.audit.read"
)

// Presence is which arms of a verb need a PERSON present: refused to a request
// that presented a machine token ([iam.Principal.MachineToken]), whatever the
// token carries and however fresh its proof, because a token proves nobody is
// at a keyboard. See the package doc.
//
// THREE VALUES, NOT A BOOL: ending sessions is a token's to do about its own
// owner (it ends the token too, what somebody who finds one leaked reaches
// for) and never about anybody else.
type Presence string

const (
	// PresenceOptional is the zero: no arm needs a person, so a machine token
	// takes the verb wherever the class admits its owner. The zero is the open
	// end, as [rule.humanOnly]'s is, held by walks that derive which rows must
	// close it from the table.
	PresenceOptional Presence = ""
	// PresenceRequired needs a person on every arm, the self arm included.
	PresenceRequired Presence = "required"
	// PresenceRequiredForOthers needs a person on every arm but the self
	// arm: about the caller's own record a token acts as its owner, about
	// anybody else it does not.
	PresenceRequiredForOthers Presence = "required_for_others"
	// PresencePerson needs a person on every arm, and refuses a credential
	// acting as itself as well as a token: a deployment's or a pipeline's
	// admin credential is nobody at a keyboard either.
	PresencePerson Presence = "person"
)

// Presences are the four, in declaration order.
var Presences = []Presence{
	PresenceOptional, PresenceRequired, PresenceRequiredForOthers, PresencePerson,
}

// Valid reports whether a presence is one this build knows.
func (p Presence) Valid() bool { return slices.Contains(Presences, p) }

// SeatGrant is an authority a seat's ROLE carries in the company document —
// `role.health_tools: true` — that admits an AGENT SEAT to the verbs whose
// rows name it ([rule.seatGrant]), in place of their class.
//
// A FACT ABOUT THE ROLE, NOT A PERMISSION. A seat holds every app and never
// admin (internal/agent/turnctx); what it may do beyond the apps is its
// role's to say, so the field is admin's to set, never a lead's
// (internal/config's org diff lists it apart). See [Chart.SeatGranted]. The
// zero is no grant: a row naming none decides every principal by its class.
type SeatGrant string

const (
	// SeatGrantHealth is `role.health_tools`: the fleet's health and
	// Gitlet's status, which a person reads as an administrator.
	SeatGrantHealth SeatGrant = "health_tools"

	// SeatGrantSSHRead is an agent on at SSH with at least one host
	// (`roles[].integrations.ssh`): the server's allowlisted reads.
	SeatGrantSSHRead SeatGrant = "ssh_read"

	// SeatGrantSSHOperate is an agent on at SSH at operate: commands a
	// person approves one at a time.
	SeatGrantSSHOperate SeatGrant = "ssh_operate"

	// SeatGrantDelegate is an agent an enabled delegated-decisions rule names: the
	// requests that rule delegates to it (ADR-0125).
	SeatGrantDelegate SeatGrant = "delegated_decisions"
)

// SeatGrants are every grant, in declaration order.
var SeatGrants = []SeatGrant{SeatGrantHealth, SeatGrantSSHRead, SeatGrantSSHOperate, SeatGrantDelegate}

// Valid reports whether a grant is one this build knows: one of
// [SeatGrants], or a registered growth product's ([GrowthSeatGrantOf]).
func (g SeatGrant) Valid() bool {
	if slices.Contains(SeatGrants, g) {
		return true
	}
	_, _, ok := GrowthSeatGrantOf(g)
	return ok
}

// AssistClass is what the Dashboard Assistant may do with a verb: take it
// under a read grant, carry it only as the one approved request an exec grant
// binds, or never (ADR-0052). See [rule.assist].
//
// IT ONLY EVER TAKES AWAY. An assisted principal is the person, decided
// exactly as for their own click; this says which of those admissions the
// assistant may also make, and none admits what the person's authority refuses.
type AssistClass string

const (
	// AssistRead is a read the person may make, which a read grant lets a
	// turn's tool loop make with nothing to approve.
	AssistRead AssistClass = "read"
	// AssistApprove may be carried only as the one request a person approved:
	// an exec grant binds its digest, so the write that runs is the preview the
	// person saw (ADR-0051).
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
// ONE TABLE, NOT SEVERAL: a class map beside an app map beside a human-only
// list must agree about one verb, and the day they disagree a verb is decided
// by a class that reads a field nobody set.
type rule struct {
	class Class

	// app is the app this verb belongs to, or empty. It is the requirement of
	// [ClassApp] and [ClassAppOrAsked] and is ANDED onto every relation class
	// ([withApp]): a project's lead who does not use Worklet does not set its
	// policy. Admin holds every app through [iam.Expand].
	//
	// A FIELD ON THE ROW, NOT A PER-KIND LOOKUP, which would answer one verb
	// differently on two objects and let a walk certify a row whose app was
	// decided elsewhere.
	app iam.Grant

	// anyOf is the apps a [ClassAnyApp] row admits on, any ONE, and the set its
	// refusal names. A walk holds a ClassAnyApp row to at least two apps and
	// every other row to none.
	//
	// ON THE ROW, NEVER "EVERY APP": iterating [iam.Apps] admitted a holder of
	// chat alone to a search of nothing.
	anyOf []iam.Grant

	// humanOnly marks a verb NO AGENT may take, whatever its class allows. A
	// fact about the VERB, so a field beside the class, composing with the
	// container relation and with admin.
	//
	// CHECKED BEFORE THE CLASS, so an agent holding the permission is told it
	// is a seat, not that it lacks a capability it plainly has.
	humanOnly bool

	// irreversible marks a verb NOTHING UNDOES (a purge, a chat erase, a prune,
	// a room's horizon). Read by whatever shows the gesture to a person first
	// (the assistant's card); a route's method cannot say it (a purge is a
	// POST, a horizon a PUT). A walk holds every such row to a recent proof.
	// Decides nothing in [Decide].
	irreversible bool

	// seatOnly is humanOnly's MIRROR: a verb ONLY AN AGENT SEAT may take, with
	// no permission asked. A person has no turn for a colleague's answer to land
	// in and reads a seat's sessions as its trail ([ActionSeatTrailRead]). Two
	// take an app beside it (the atlas record and dispute write a shared
	// surface), so the requester ceiling holds them to whoever woke the turn
	// (ADR-0132).
	//
	// CHECKED BEFORE THE CLASS, so an admin is told the verb is an agent's
	// ([ReasonSeatOnly]) rather than sent to ask for a permission that changes
	// nothing. A walk refuses a row that is both.
	seatOnly bool

	// seatGrant is the ROLE GRANT that decides an AGENT SEAT on this verb in
	// place of the class ([SeatGrant]), admitted where the chart says its role
	// carries it ([Chart.SeatGranted]); every other principal is decided by the
	// class.
	//
	// FOR A VERB WHOSE CLASS NO SEAT MEETS: the health tools are admin's, a seat
	// never holds admin (internal/agent/turnctx), and admin would let it rewrite
	// the company it runs inside. Read by [Decide] and internal/agent/builtin's
	// Admits, so a seat is offered a granted verb exactly where admitted. A walk
	// refuses a grant beside humanOnly or seatOnly.
	seatGrant SeatGrant
	// seated marks a verb made AS A SEAT (what it writes is attributed to a
	// seat handle, or what it reads is answered for one), so a principal acting
	// as itself (Tier A token, unbound service account, development principal)
	// cannot take it, whatever it holds. A person acts as their human seat
	// (ADR-0032).
	//
	// CHECKED AFTER seatOnly and BEFORE THE CLASS: an unbound admin is told it
	// has no seat ([ReasonUnseated]). A walk refuses a row both seat-only and
	// seated.
	seated bool

	// presence is which arms need a PERSON at the keyboard ([Presence]),
	// refused to a request that presented a machine token.
	//
	// A FIELD ON THE ROW, NOT A CHECK IN EACH ROUTE: a route refusing a token
	// itself would let a walk certify a row that admitted it.
	//
	// CHECKED BEFORE THE CLASS where it covers every arm, as [rule.humanOnly]
	// is, and AFTER where it spares the self arm, since only the class can say
	// which arm admitted.
	presence Presence

	// recency is how recently the principal must have PROVED who they are (the
	// step-up), decided on the row so REST, the socket and a tool cannot
	// disagree.
	//
	// EVERY ROW STATES ONE and a walk refuses the zero, which would read as
	// [iam.RecencyAny] and ship a step-up verb open to a session proved last
	// week.
	//
	// CHECKED AFTER THE CLASS ALLOWS: a principal refused outright is told what
	// they lack, not sent through a step-up only to be refused anyway.
	recency iam.Recency

	// selfRecency is the proof the SELF arm asks for (the person the object IS,
	// [ReasonSelf]) where it differs from the row's. ONE GESTURE, TWO RISKS:
	// ending every session somebody holds is the first thing they do on finding
	// an intruder, and re-proving a password the intruder may hold slows that,
	// while the SAME verb by an administrator ends another person's sessions and
	// tokens. ITS ZERO MEANS "WHAT THE ROW ASKS"; a walk holds a set one to a
	// real window on a class with a self arm.
	selfRecency iam.Recency

	// minRole is the least ROLE on an artifact a [ClassShared] row admits.
	//
	// REQUIRED ON EVERY SHARED ROW AND REFUSED ON EVERY OTHER, by a walk, for
	// [rule.recency]'s reason: [artifacts.Role.Includes] admits nobody on a zero
	// requirement, and the walk makes that a build failure instead of a verb
	// nobody can take.
	minRole artifacts.Role

	// steerable says an agent's OWN object on this verb is steerable by
	// whoever woke it: the requester ceiling ([DecideWithin]) spares an agent
	// acting on what it owns.
	//
	// THE ZERO IS THE CEILING ALWAYS APPLYING, the safe direction: a verb that
	// discloses content is never steerable, or anybody who can message an agent
	// could have it read its private work back. Nor is sharing, which would be
	// that same read one step removed.
	steerable bool

	// assist is what the verb IS to the Dashboard Assistant ([AssistClass]),
	// asked only of a principal acting through an assistant grant
	// ([iam.AssistPrefix]).
	//
	// PARITY WITH THE PERSON DECIDES IT (ADR-0052). The only verbs it never
	// takes are [assistantNever]'s, the ONE place [AssistNever] is spelled.
	// EVERY ROW STATES ONE and a walk refuses the zero, for [rule.recency]'s
	// reason; [Decide] reads a zero as [AssistNever], so a missed row fails
	// closed.
	assist AssistClass
}

// rules is THE authority table: every verb this engine knows, and how it is
// decided.
//
// A VERB MISSING FROM HERE IS REFUSED, and the completeness walk in this
// package's tests turns that into a build failure. A default class is how a
// new verb ships ungated, looking correct.
var rules = map[Action]rule{
	// --- using Worklet ------------------------------------------- //
	//
	// THE APP IS THE WHOLE RULE: reading the board, a project, its files, views
	// and catalogue, and filing, updating, merging and arranging work.
	//
	// READING ONE ITEM IS ALSO THE ASKED PERSON'S: somebody a colleague put a
	// question to reads the item it is on, whether or not they use Worklet
	// (see [ClassAppOrAsked]).
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
	// A PROJECT'S FILES are read and kept on the tracker as its items are,
	// never on the knowledge base.
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
	// A COMMENT IS THE TRACKER'S, OR THE ANSWER THE ASKED PERSON OWES: the
	// asked arm admits a person an open ask names, the tool then takes only
	// their answer, which the tracker's writer checks again is theirs.
	ActionWorkComment: {class: ClassAppOrAsked, app: iam.GrantWorklet, recency: iam.RecencyAny,
		assist: AssistApprove},
	ActionWorkMerge: {class: ClassApp, app: iam.GrantWorklet, recency: iam.RecencyAny,
		assist: AssistApprove},
	// A PLACE IS THE APP ON THE TASK, although the record arbitrates on the
	// project's ORDER, an object nobody owns, so no relation is asked. A
	// person's gesture ([ActionWorkPlace]).
	ActionWorkPlace: {class: ClassApp, app: iam.GrantWorklet, humanOnly: true,
		recency: iam.RecencyAny, assist: AssistApprove},
	// THE ROUTE TO AN ITEM'S THREAD ADMITS EVERYBODY and decides nothing:
	// [ActionWorkComment] decides once the item is read ([ActionWorkThread]).
	ActionWorkThread: {class: ClassEverybody, recency: iam.RecencyAny, assist: AssistApprove},

	// THE CODE ROUTES TAKE THE GITLET APP and decide nothing more: the
	// repository's grant decides once it is read ([ActionCodeRead],
	// [ActionCodeGit]). A machine token takes both when its grants carry the
	// app (a token bound to no seat fetches and never pushes, ADR-0085), and
	// neither asks a proof: a read changes nothing and a push is decided per
	// ref by its grant.
	ActionCodeRead: {class: ClassApp, app: iam.GrantGitlet, recency: iam.RecencyAny,
		assist: AssistRead},
	ActionCodeGit: {class: ClassApp, app: iam.GrantGitlet, recency: iam.RecencyAny,
		assist: AssistApprove},
	// AND THE CODE READ TOOLS, on the same footing: the repository's grant
	// decides on the answering node. Not seat-only (a person's assistant reads
	// too), not human-only, no proof.
	ActionCodeListRepos: {class: ClassApp, app: iam.GrantGitlet, recency: iam.RecencyAny,
		assist: AssistRead},
	ActionCodeGetRepo: {class: ClassApp, app: iam.GrantGitlet, recency: iam.RecencyAny,
		assist: AssistRead},
	ActionCodeResolve: {class: ClassApp, app: iam.GrantGitlet, recency: iam.RecencyAny,
		assist: AssistRead},
	ActionCodeReadFile: {class: ClassApp, app: iam.GrantGitlet, recency: iam.RecencyAny,
		assist: AssistRead},
	ActionCodeDiff: {class: ClassApp, app: iam.GrantGitlet, recency: iam.RecencyAny,
		assist: AssistRead},
	ActionCodeCompare: {class: ClassApp, app: iam.GrantGitlet, recency: iam.RecencyAny,
		assist: AssistRead},
	ActionCodeLandedIn: {class: ClassApp, app: iam.GrantGitlet, recency: iam.RecencyAny,
		assist: AssistRead},
	ActionCodeLog: {class: ClassApp, app: iam.GrantGitlet, recency: iam.RecencyAny,
		assist: AssistRead},
	ActionCodeBlame: {class: ClassApp, app: iam.GrantGitlet, recency: iam.RecencyAny,
		assist: AssistRead},
	// A CHANGE IS ITS REPOSITORY'S CONTENTS, read on the footing above: a
	// listing leaves out changes of a repository the asker may not read, and a
	// read of one is refused `not_permitted`.
	ActionCodeListChanges: {class: ClassApp, app: iam.GrantGitlet, recency: iam.RecencyAny,
		assist: AssistRead},
	ActionCodeGetChange: {class: ClassApp, app: iam.GrantGitlet, recency: iam.RecencyAny,
		assist: AssistRead},
	// A SEARCH IS A READ OF EVERY REPOSITORY IT COVERS: one the asker may not
	// read is never searched, and one a repo: filter names is refused
	// `not_permitted`.
	ActionCodeSearch: {class: ClassApp, app: iam.GrantGitlet, recency: iam.RecencyAny,
		assist: AssistRead},
	// AND THE CODE WRITE TOOLS, on code.git's footing: the gate admits every
	// holder of the app and the deciding node judges from its own chart (an
	// engine write by the grant's ref rule and protections, a lifecycle request
	// by the lifecycle rule). Not human-only, no proof, as a push asks none:
	// which ref may move is the grant's question, which a verb-keyed row cannot
	// answer before the repository is read.
	ActionCodeCommitFiles: {class: ClassApp, app: iam.GrantGitlet, recency: iam.RecencyAny,
		assist: AssistApprove},
	ActionCodeRevert: {class: ClassApp, app: iam.GrantGitlet, recency: iam.RecencyAny,
		assist: AssistApprove},
	ActionCodeCherryPick: {class: ClassApp, app: iam.GrantGitlet, recency: iam.RecencyAny,
		assist: AssistApprove},
	ActionCodeCreateBranch: {class: ClassApp, app: iam.GrantGitlet, recency: iam.RecencyAny,
		assist: AssistApprove},
	ActionCodeDeleteBranch: {class: ClassApp, app: iam.GrantGitlet, recency: iam.RecencyAny,
		assist: AssistApprove},
	ActionCodeCreateTag: {class: ClassApp, app: iam.GrantGitlet, recency: iam.RecencyAny,
		assist: AssistApprove},
	ActionCodeDeleteTag: {class: ClassApp, app: iam.GrantGitlet, recency: iam.RecencyAny,
		assist: AssistApprove},
	ActionCodeCreateRepo: {class: ClassApp, app: iam.GrantGitlet, recency: iam.RecencyAny,
		assist: AssistApprove},
	ActionCodeUpdateRepo: {class: ClassApp, app: iam.GrantGitlet, recency: iam.RecencyAny,
		assist: AssistApprove},
	ActionCodeDeleteRepo: {class: ClassApp, app: iam.GrantGitlet, recency: iam.RecencyAny,
		assist: AssistApprove},
	ActionCodeRestoreRepo: {class: ClassApp, app: iam.GrantGitlet, recency: iam.RecencyAny,
		assist: AssistApprove},
	ActionCodeProtectBranch: {class: ClassApp, app: iam.GrantGitlet, recency: iam.RecencyAny,
		assist: AssistApprove},
	// AND THE CHANGE WRITES: decided by the repository's committer from its own
	// chart (who may push the proposed branch, who owns or maintains a change);
	// a seat opens and steers its own, so neither human-only nor a proof.
	ActionCodeOpenChange: {class: ClassApp, app: iam.GrantGitlet, recency: iam.RecencyAny,
		assist: AssistApprove},
	ActionCodeUpdateChange: {class: ClassApp, app: iam.GrantGitlet, recency: iam.RecencyAny,
		assist: AssistApprove},
	ActionCodeCloseChange: {class: ClassApp, app: iam.GrantGitlet, recency: iam.RecencyAny,
		assist: AssistApprove},
	// AND ITS CONVERSATION, decided by the change's writer from its own chart
	// (who may review, who wrote the change, who raised a finding); a seat
	// reviews, so neither human-only nor a proof.
	ActionCodeRequestReview: {class: ClassApp, app: iam.GrantGitlet, recency: iam.RecencyAny,
		assist: AssistApprove},
	ActionCodeReviewChange: {class: ClassApp, app: iam.GrantGitlet, recency: iam.RecencyAny,
		assist: AssistApprove},
	ActionCodeCommentOnChange: {class: ClassApp, app: iam.GrantGitlet, recency: iam.RecencyAny,
		assist: AssistApprove},
	ActionCodeDeleteComment: {class: ClassApp, app: iam.GrantGitlet, recency: iam.RecencyAny,
		assist: AssistApprove},
	ActionCodeResolveFinding: {class: ClassApp, app: iam.GrantGitlet, recency: iam.RecencyAny,
		assist: AssistApprove},
	// AND ITS SUGGESTIONS APPLIED, on code.git's footing as every engine write.
	ActionCodeApplySuggestions: {class: ClassApp, app: iam.GrantGitlet, recency: iam.RecencyAny,
		assist: AssistApprove},
	// AND ITS REBASE, on the same footing.
	ActionCodeRebaseChange: {class: ClassApp, app: iam.GrantGitlet, recency: iam.RecencyAny,
		assist: AssistApprove},
	// AND ITS MERGE AND DEQUEUE, decided by the repository's committer (the
	// gate's merge_by; an override is a person's alone): a seat merges a change
	// its gate lets it, so neither human-only nor a proof here.
	ActionCodeMergeChange: {class: ClassApp, app: iam.GrantGitlet, recency: iam.RecencyAny,
		assist: AssistApprove},
	ActionCodeDequeueChange: {class: ClassApp, app: iam.GrantGitlet, recency: iam.RecencyAny,
		assist: AssistApprove},
	// AND ITS REVERT, on the footing of every engine write.
	ActionCodeRevertChange: {class: ClassApp, app: iam.GrantGitlet, recency: iam.RecencyAny,
		assist: AssistApprove},
	// AND A COMMIT'S CHECKS: failures read and an attempt rerun, decided per
	// repository in the tool and by the checks writer. A REPORT is a person's
	// own CI's, so human-only: a seat runs no CI, and its report would be a
	// check result nobody ran (M5-D24); no assistant carries one either
	// ([assistantNever]).
	ActionCodeGetCheckFailures: {class: ClassApp, app: iam.GrantGitlet, recency: iam.RecencyAny,
		assist: AssistRead},
	ActionCodeRerunChecks: {class: ClassApp, app: iam.GrantGitlet, recency: iam.RecencyAny,
		assist: AssistApprove},
	ActionCodeReportCheck: {class: ClassApp, app: iam.GrantGitlet, humanOnly: true,
		recency: iam.RecencyAny, assist: AssistApprove},
	// A PURGE OF UNREACHABLE OBJECTS IS A PERSON'S, at a fresh proof: it drops
	// history no ref reaches and nothing brings it back (a leaked secret's
	// purge is its use), so never an agent's, and a step-up as every purge is
	// ([ActionWorkPurge]). THE APP at the gate: who maintains the repository is
	// its grant, which the tool judges once it is read.
	ActionCodePurgeUnreachable: {class: ClassApp, app: iam.GrantGitlet, humanOnly: true,
		irreversible: true, recency: iam.RecencyStepUp, assist: AssistApprove},
	// AN IMPORT IS A PERSON'S (U49 a): it has Gitlet DIAL ANOTHER HOST, once or
	// as a pull mirror, which a model would point at any URL a page it read
	// named, so never an agent's; the data node refuses a seat's create
	// carrying an import or mirror again, whatever its grant. THE APP at the
	// gate, no step-up: every holder maintains every repository (ADR-0088,
	// ADR-0110), and an import, like a create, takes nothing away.
	ActionCodeImportRepo: {class: ClassApp, app: iam.GrantGitlet, humanOnly: true,
		recency: iam.RecencyAny, assist: AssistApprove},
	// WHO READ A REPOSITORY IS ADMIN'S, as the company-wide trail is
	// ([ActionAuditRead]): it names every person, credential and seat that read
	// a unit's code, an audit of the people as much as the code, so NEVER AN
	// AGENT'S (a model watching the people it works for) and NOT THE OWNING
	// UNIT'S LEADS: the company gives that with the admin permission, not a
	// place on the chart.
	ActionCodeRepoReaders: {class: ClassAdmin, humanOnly: true, recency: iam.RecencyAny, assist: AssistRead},
	// A PERSON'S CODE MARKS ARE THEIR OWN, as their Worklet marks are
	// ([ActionInboxMark]); nobody asked a lead to mark somebody's mail read.
	// HUMAN-ONLY, since a seat is woken, never shown a list, and THE GITLET
	// APP: a person without it reaches no repository (ADR-0110), so no code
	// notice is theirs. The FILES MARKED VIEWED likewise.
	ActionCodeMarkInbox: {class: ClassOwnRecord, app: iam.GrantGitlet, humanOnly: true,
		recency: iam.RecencyAny, assist: AssistApprove},
	ActionCodeMarkViewed: {class: ClassOwnRecord, app: iam.GrantGitlet, humanOnly: true,
		recency: iam.RecencyAny, assist: AssistApprove},
	ActionArtifactMarkInbox: {class: ClassOwnRecord, humanOnly: true, recency: iam.RecencyAny,
		assist: AssistApprove},
	// A HOME BOARD IS ONE'S OWN RECORD, as a read cursor is: the tool takes no
	// handle, and it is kept under their SEAT ([iam.RecordOwner]), whose next
	// holder inherits it. No app: Home is everybody's.
	ActionHomeBoardSave: {class: ClassOwnRecord, humanOnly: true, seated: true,
		recency: iam.RecencyAny, assist: AssistApprove},
	// AND SO ARE ONE'S OWN ASK CREWLET INSTRUCTIONS, which Ask Crewlet may
	// propose ("answer shorter from now on") for the person to approve.
	ActionAssistantInstructionsSave: {class: ClassOwnRecord, humanOnly: true, seated: true,
		recency: iam.RecencyAny, assist: AssistApprove},

	// --- reading the atlas ------------------------------------------- //
	//
	// THE APP ALONE: every holder reads every entry (ADR-0127), a seat held to
	// whoever woke its turn (ADR-0109).
	ActionAtlasSearch: {class: ClassApp, app: iam.GrantAtlas, recency: iam.RecencyAny,
		assist: AssistRead},
	ActionAtlasGet: {class: ClassApp, app: iam.GrantAtlas, recency: iam.RecencyAny,
		assist: AssistRead},
	ActionAtlasTrace: {class: ClassApp, app: iam.GrantAtlas, recency: iam.RecencyAny,
		assist: AssistRead},

	// --- changing the atlas ------------------------------------------ //
	//
	// THE APP, AND A PERSON'S. Every holder corrects every entry (ADR-0127),
	// each write a claim in their own name that outranks every inference and
	// keeps the replaced one as history, so none needs a proof: a retire is
	// undone by a restore, a claim by the next. NEVER AN AGENT'S: it records
	// only what a call of its own turn showed it, through tools of its own, so
	// a model steered by what it read cannot decide what the company's map
	// says. AND SEATED: a claim names its author by seat, so a credential
	// acting as no seat records no claim in a person's rank.
	ActionAtlasAddEntry: {class: ClassApp, app: iam.GrantAtlas, humanOnly: true, seated: true,
		recency: iam.RecencyAny, assist: AssistApprove},
	ActionAtlasCorrect: {class: ClassApp, app: iam.GrantAtlas, humanOnly: true, seated: true,
		recency: iam.RecencyAny, assist: AssistApprove},
	ActionAtlasConfirm: {class: ClassApp, app: iam.GrantAtlas, humanOnly: true, seated: true,
		recency: iam.RecencyAny, assist: AssistApprove},
	ActionAtlasRelate: {class: ClassApp, app: iam.GrantAtlas, humanOnly: true, seated: true,
		recency: iam.RecencyAny, assist: AssistApprove},
	ActionAtlasUnrelate: {class: ClassApp, app: iam.GrantAtlas, humanOnly: true, seated: true,
		recency: iam.RecencyAny, assist: AssistApprove},
	ActionAtlasSetOwner: {class: ClassApp, app: iam.GrantAtlas, humanOnly: true, seated: true,
		recency: iam.RecencyAny, assist: AssistApprove},
	ActionAtlasRetire: {class: ClassApp, app: iam.GrantAtlas, humanOnly: true, seated: true,
		recency: iam.RecencyAny, assist: AssistApprove},
	ActionAtlasRestore: {class: ClassApp, app: iam.GrantAtlas, humanOnly: true, seated: true,
		recency: iam.RecencyAny, assist: AssistApprove},
	ActionAtlasMerge: {class: ClassApp, app: iam.GrantAtlas, humanOnly: true, seated: true,
		recency: iam.RecencyAny, assist: AssistApprove},
	ActionAtlasSplit: {class: ClassApp, app: iam.GrantAtlas, humanOnly: true, seated: true,
		recency: iam.RecencyAny, assist: AssistApprove},
	// A PURGE IS ADMIN'S, A PERSON'S AND A STEP-UP, as every purge is
	// ([ActionWorkPurge]): it erases what every holder reads, history
	// included (ADR-0127).
	ActionAtlasPurge: {class: ClassAdmin, humanOnly: true, seated: true, irreversible: true,
		recency: iam.RecencyStepUp, assist: AssistApprove},

	// A READ OF A SOURCE ASKED FOR NOW IS THE APP'S AND A PERSON'S: it changes
	// nothing a person decided and is bounded by the engine's own floor. Never
	// an agent's: a read spends the company's vendor rate limit, and an agent
	// that doubts an entry disputes it.
	ActionAtlasRefresh: {class: ClassApp, app: iam.GrantAtlas, humanOnly: true, recency: iam.RecencyAny,
		assist: AssistApprove},

	// --- an agent's record in the atlas ------------------------------ //
	//
	// THE APP, AND AN AGENT'S ALONE (ADR-0132): an agent changes the atlas only
	// with these two, at the lowest rank. Held to whoever woke the turn as
	// every atlas verb is (ADR-0109), so a turn a stranger or somebody without
	// Atlas woke records nothing: every agent reads back what one records.
	ActionAtlasRecord: {class: ClassApp, app: iam.GrantAtlas, seatOnly: true, recency: iam.RecencyAny,
		assist: AssistApprove},
	ActionAtlasDispute: {class: ClassApp, app: iam.GrantAtlas, seatOnly: true, recency: iam.RecencyAny,
		assist: AssistApprove},

	// `write_project` IS TWO GESTURES AND ONLY ONE IS THE LEAD'S: adding a label
	// is open to every Worklet user, renaming or archiving one takes the word
	// off everybody's board. So the TOOL takes the app and the lead-only facets
	// (rename, archive, policy edit) ask [ActionProjectPolicy] from inside it.
	ActionProjectWrite: {class: ClassApp, app: iam.GrantWorklet, recency: iam.RecencyAny,
		assist: AssistApprove},
	ActionProjectPolicy: {class: ClassContainer, app: iam.GrantWorklet,
		recency: iam.RecencyAny, assist: AssistApprove},
	// AND RE-ROUTING IS THE LEAD'S, asked from inside `update_work_item`: it
	// decides which team hears about the work, and the project comes out of
	// the stored row, not the arguments.
	ActionWorkRoute: {class: ClassContainer, app: iam.GrantWorklet, recency: iam.RecencyAny,
		assist: AssistApprove},
	// ARCHIVING A PROJECT TAKES IT OUT OF CIRCULATION for everybody: the lead's
	// like the rest of the policy, and never an agent's (a company decision).
	ActionProjectArchive: {class: ClassContainer, app: iam.GrantWorklet, humanOnly: true,
		recency: iam.RecencyAny, assist: AssistApprove},

	// AN ITEM'S REPORTER TIDIES IT, beside its project's lead and admin. A
	// subtree stays the lead's (the tool refuses an author it), a purge
	// admin's. See [ClassTidy].
	ActionWorkRemove: {class: ClassTidy, app: iam.GrantWorklet, recency: iam.RecencyAny,
		assist: AssistApprove},
	ActionWorkRestore: {class: ClassTidy, app: iam.GrantWorklet, recency: iam.RecencyAny,
		assist: AssistApprove},
	// A MOVE TAKES AN ITEM OUT OF ITS PROJECT, as a file's removal does: the
	// lead's or admin's, not every colleague's who may add to it.
	ActionWorkMove: {class: ClassDestructive, app: iam.GrantWorklet, recency: iam.RecencyAny,
		assist: AssistApprove},
	ActionFileRemove: {class: ClassDestructive, app: iam.GrantWorklet, recency: iam.RecencyAny,
		assist: AssistApprove},

	// A WORK ITEM'S REMARK IS ITS AUTHOR'S, or admin's, and the tracker's: the
	// class answers "may you touch this remark" and the WRITER refuses anybody
	// but the author an edit, which puts words in somebody's mouth where a
	// removal only takes them down.
	ActionWorkCommentEdit: {class: ClassAuthored, app: iam.GrantWorklet, recency: iam.RecencyAny,
		assist: AssistApprove},
	// AND ITS REMOVAL, which the writer takes from the author and a moderating
	// admin, as a page's: taking words down is visible to everybody.
	ActionWorkCommentRemove: {class: ClassAuthored, app: iam.GrantWorklet, recency: iam.RecencyAny,
		assist: AssistApprove},

	// A VIEW IS NOT A RECORD: omitting the owner SHARES it, as a tab on a
	// project, unit or person page. See [ClassSavedView].
	ActionViewSave: {class: ClassSavedView, app: iam.GrantWorklet, recency: iam.RecencyAny,
		assist: AssistApprove},
	// A DELETE IS DECIDED AS THE SAVE THAT WOULD REPLACE THE VIEW, on the
	// stored owner and container: who may rewrite a view may take it away.
	ActionViewDelete: {class: ClassSavedView, app: iam.GrantWorklet, recency: iam.RecencyAny,
		assist: AssistApprove},

	// --- somebody's own record, on the tracker ------------------------ //
	//
	// READING SOMEBODY'S DAY IS THE LEAD RELATION, and MARKING it is not: a lead
	// re-orders what their report works on, but marking somebody's mail read or
	// rearranging their pins is a gesture nobody asked a lead to make. Each is
	// the TRACKER's, so each takes the app beside the relation.
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
	// THE COMPOSED INBOX IS NO APP'S: each half is read under its own
	// permission (Worklet's with the app, Gitlet's by each repository's grant),
	// so a person without Worklet still reads the changes they are asked to
	// review. Whose it is stays the lead relation, as `work_inbox`'s is.
	ActionInbox: {class: ClassOwnOrLead, recency: iam.RecencyAny, assist: AssistRead},
	ActionPinsSet: {class: ClassOwnRecord, app: iam.GrantWorklet, recency: iam.RecencyAny,
		assist: AssistApprove},

	// --- using Doclet ------------------------------------ //
	ActionPageRead: {class: ClassApp, app: iam.GrantDoclet, recency: iam.RecencyAny,
		assist: AssistRead},
	ActionPageList: {class: ClassApp, app: iam.GrantDoclet, recency: iam.RecencyAny,
		assist: AssistRead},
	ActionKnowledgeRead: {class: ClassApp, app: iam.GrantDoclet, recency: iam.RecencyAny,
		assist: AssistRead},
	// WHERE A PAGE MAY BE CREATED is internal/doclet's store's to decide for
	// every author alike; this row decides only WHO.
	ActionPageCreate: {class: ClassApp, app: iam.GrantDoclet, recency: iam.RecencyAny,
		assist: AssistApprove},
	ActionPageSave: {class: ClassApp, app: iam.GrantDoclet, recency: iam.RecencyAny,
		assist: AssistApprove},
	ActionPageComment: {class: ClassApp, app: iam.GrantDoclet, recency: iam.RecencyAny,
		assist: AssistApprove},
	// A PAGE'S CREATOR TIDIES IT (rename, trash, restore, publish) beside its
	// container's lead and admin. See [ClassTidy].
	ActionPageRename: {class: ClassTidy, app: iam.GrantDoclet, recency: iam.RecencyAny,
		assist: AssistApprove},
	ActionPageTrash: {class: ClassTidy, app: iam.GrantDoclet, recency: iam.RecencyAny,
		assist: AssistApprove},
	ActionPageRestore: {class: ClassTidy, app: iam.GrantDoclet, recency: iam.RecencyAny,
		assist: AssistApprove},
	ActionPagePublish: {class: ClassTidy, app: iam.GrantDoclet, recency: iam.RecencyAny,
		assist: AssistApprove},
	// A REMARK IS ITS AUTHOR'S, as a work item's is; the store refuses
	// anybody but the author an edit, and takes a moderator's removal.
	ActionPageCommentEdit: {class: ClassAuthored, app: iam.GrantDoclet, recency: iam.RecencyAny,
		assist: AssistApprove},
	ActionPageCommentRemove: {class: ClassAuthored, app: iam.GrantDoclet, recency: iam.RecencyAny,
		assist: AssistApprove},

	// A QUESTION ANSWERED FROM WHAT THE COMPANY WROTE DOWN draws on whichever
	// of two apps the asker uses (pages for a knowledge holder, the board for a
	// tracker holder), so either admits and a caller with neither has nothing
	// to search. A person's alone: it runs on the auxiliary model of the
	// asker's seat, and a seat has search_knowledge and a model of its own. And
	// SEATED: a principal no seat binds (a Tier A session or service account;
	// a person holds a human seat, ADR-0032) has no model to answer on and
	// nobody to charge.
	ActionKnowledgeAnswer: {class: ClassAnyApp,
		anyOf:     []iam.Grant{iam.GrantWorklet, iam.GrantDoclet},
		humanOnly: true, seated: true, recency: iam.RecencyAny, assist: AssistApprove},

	// --- using the chat ---------------------------------------------- //
	//
	// THE APP IS THE WHOLE RULE for using chat. WHICH ROOM is the chat domain's:
	// a read of a room the caller may not read answers NOT FOUND
	// (internal/chatlet's Visible, never a refusal that confirms the room), and
	// a write is refused inside its own snapshot, the state log's one write
	// authority. Membership is a replicated row, not a chart relation, which
	// would be read in another transaction.
	//
	// EVERY CHAT ROW IS SEATED ([rule.seated]): rooms render speakers and
	// membership by seat. A pipeline that posts gets a human seat.
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
	// A SEAT MOVES ITS OWN MEMBERSHIP only (the tools take no handle, the write
	// path refuses any other), so the app is the rule.
	ActionChatJoin: {class: ClassApp, app: iam.GrantChatlet, seated: true, recency: iam.RecencyAny,
		assist: AssistApprove},
	ActionChatLeave: {class: ClassApp, app: iam.GrantChatlet, seated: true, recency: iam.RecencyAny,
		assist: AssistApprove},

	// --- a person's chat gestures ------------------------------------ //
	//
	// HUMAN-ONLY, each for a fact about the verb (internal/chatlet says which):
	// a seat making rooms as a side effect would leave a sidebar nobody chose,
	// and one rewriting what it said would leave a record the next turn reads as
	// always said. A room's settings and membership are any reachable member's
	// (the store refuses the rest); its ARCHIVE is not ([ActionChatArchive]).
	ActionChatCreate: {class: ClassApp, app: iam.GrantChatlet, humanOnly: true, seated: true,
		recency: iam.RecencyAny, assist: AssistApprove},
	ActionChatOpenDM: {class: ClassApp, app: iam.GrantChatlet, humanOnly: true, seated: true,
		recency: iam.RecencyAny, assist: AssistApprove},
	ActionChatUpdate: {class: ClassApp, app: iam.GrantChatlet, humanOnly: true, seated: true,
		recency: iam.RecencyAny, assist: AssistApprove},
	ActionChatMembers: {class: ClassApp, app: iam.GrantChatlet, humanOnly: true, seated: true,
		recency: iam.RecencyAny, assist: AssistApprove},
	ActionChatAdd: {class: ClassApp, app: iam.GrantChatlet, humanOnly: true, seated: true,
		recency: iam.RecencyAny, assist: AssistApprove},
	// A MESSAGE IS ITS AUTHOR'S, or admin's, the comment precedent: the WRITER
	// refuses anybody but the author an EDIT, which puts words in somebody's
	// mouth. Admin's delete is MODERATION, decided on the unread message, so a
	// moderator removes what was said in a room they are not in without reading
	// it ([ClassAuthored]).
	ActionChatEdit: {class: ClassAuthored, app: iam.GrantChatlet, humanOnly: true, seated: true,
		recency: iam.RecencyAny, assist: AssistApprove},
	ActionChatDelete: {class: ClassAuthored, app: iam.GrantChatlet, humanOnly: true, seated: true,
		recency: iam.RecencyAny, assist: AssistApprove},
	// A READ CURSOR IS ONE'S OWN RECORD, as an inbox's marks are: the tool takes
	// no handle, and it is kept under their SEAT ([iam.RecordOwner]), whose next
	// holder inherits it.
	ActionChatMark: {class: ClassOwnRecord, app: iam.GrantChatlet, humanOnly: true, seated: true,
		recency: iam.RecencyAny, assist: AssistApprove},
	// ARCHIVING A ROOM CLOSES THE CONVERSATION for everybody, so a TIDY: the
	// room's creator, the lead of its unit or a unit above, or admin. Never an
	// agent's, as no room setting is.
	ActionChatArchive: {class: ClassTidy, app: iam.GrantChatlet, humanOnly: true, seated: true,
		recency: iam.RecencyAny, assist: AssistApprove},
	// AUTO-JOIN decides about the whole company's rooms, not one conversation:
	// an administrator's, as a person, never an agent's.
	ActionChatAutoJoin: {class: ClassAdmin, humanOnly: true, seated: true,
		recency: iam.RecencyAny, assist: AssistApprove},

	// --- everybody's overview ---------------------------------------- //
	//
	// WHO TO ASK IS EVERYBODY'S: the lookup answers from the org chart. The
	// person-facing question masks external contact ids for all but admin; a
	// seat's tool keeps them, as an agent mentions people by them.
	ActionColleagueRead: {class: ClassEverybody, recency: iam.RecencyAny, assist: AssistRead},
	// WHO HOLDS EACH HUMAN SEAT: see [ActionSeatsRead]; the route masks per
	// caller.
	ActionSeatsRead: {class: ClassEverybody, recency: iam.RecencyAny, assist: AssistRead},
	// WHAT EACH MCP SERVER IS DOING AND WHICH TOOLS IT SERVES is everybody's,
	// as each model's condition is: it tells a member why an agent lacks a
	// tool. A member's answer names no launch line, header, variable, account
	// or call ([ActionToolsRead]); the configuration's reader gets the rest by
	// [ActionConfigRead]'s row.
	ActionToolsRead: {class: ClassEverybody, recency: iam.RecencyAny, assist: AssistRead},

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
	// A SEAT'S OWN EXECUTOR SESSIONS: its own state, an agent's alone; a person
	// reads them as the trail ([ActionSeatTrailRead]) — see [rule.seatOnly].
	ActionSessionsList: {class: ClassSelf, seatOnly: true, recency: iam.RecencyAny, assist: AssistRead},
	ActionSessionRead:  {class: ClassSelf, seatOnly: true, recency: iam.RecencyAny, assist: AssistRead},
	// A SEAT'S OWN QUESTION WITHDRAWN: the tracker's writer holds it to its
	// author and a run to its seat.
	ActionWithdrawAsk: {class: ClassSelf, seatOnly: true, recency: iam.RecencyAny, assist: AssistApprove},
	// A SEAT ASKING FOR ACCESS is posted through the work comment's own gate;
	// the grant is a person's write.
	ActionRequestAccess: {class: ClassSelf, seatOnly: true, recency: iam.RecencyAny, assist: AssistApprove},
	// AN ASK IS AN AGENT'S, whichever apps it uses: it spends a colleague
	// seat's turn, and the ROLE decides which seats are offered it
	// ([rule.seatOnly]).
	ActionColleagueAsk: {class: ClassEverybody, seatOnly: true, recency: iam.RecencyAny,
		assist: AssistApprove},

	// --- whether a seat works, and what its turn is doing ------------- //
	//
	// WHETHER A SEAT WORKS IS ITS LEAD'S TO DECIDE, or admin's, as what it
	// works on is ([ActionPrioritiesSet]). HUMAN-ONLY on top: a seat pausing a
	// colleague, resuming itself, steering another's turn or answering its own
	// run's question would overrule the people who run it. No app: a lead who
	// uses none still runs their team.
	ActionSeatPause: {class: ClassOwnOrLead, humanOnly: true, recency: iam.RecencyAny,
		assist: AssistApprove},
	ActionSeatResume: {class: ClassOwnOrLead, humanOnly: true, recency: iam.RecencyAny,
		assist: AssistApprove},
	ActionTurnSteer: {class: ClassOwnOrLead, humanOnly: true, recency: iam.RecencyAny,
		assist: AssistApprove},
	// RUNNING A SCHEDULE NOW starts a turn on every seat it wakes, the
	// decision a pause is the other half of: its lead's too. The tool asks this
	// row about each of those seats.
	ActionScheduleRun: {class: ClassOwnOrLead, humanOnly: true, recency: iam.RecencyAny,
		assist: AssistApprove},
	// A RUN'S QUESTION IS ITS REQUESTER'S TO ANSWER, or the run's seat's lead's:
	// the tool asks this row about the REQUESTER's seat when the caller is that
	// requester (the self arm), else about the run's seat, so whoever asked for
	// the work answers it and nobody who merely leads them does.
	ActionExecutorAnswer: {class: ClassOwnOrLead, humanOnly: true, recency: iam.RecencyAny,
		assist: AssistApprove},

	// A SEAT'S TRAIL IS ITS LEAD'S TO READ, and admin's (diary, episodes,
	// threads, its turns' prompts and responses): a lead steers, pauses and
	// answers for their agents and could not without reading what they did;
	// somebody who merely shares the company does not read every prompt. The
	// COMPANY-WIDE trail stays admin's ([ActionAuditRead]).
	ActionSeatTrailRead: {class: ClassOwnOrLead, recency: iam.RecencyAny, assist: AssistRead},

	// --- running the company ------------------------------------------ //
	//
	// A TOOL SKILL IS CONFIGURATION WRITTEN AS A PAGE: its container's pages
	// are injected into a phase of EVERY seat's turn as instructions, so
	// writing one rewrites the prompt the whole company runs under, the reach of
	// editing the company document: admin's, not the knowledge app's. NEVER AN
	// AGENT'S, which internal/doclet's store also refuses: a seat writing the
	// instructions every seat obeys edits the rules it is judged by.
	ActionSkillPageWrite: {class: ClassAdmin, humanOnly: true, recency: iam.RecencyAny,
		assist: AssistApprove},
	// THE CATALOGUE IS THE COMPANY'S, NOT A CONTAINER'S: task types and custom
	// fields are declared once for the workspace. Configuration, admin's.
	ActionCatalogueWrite: {class: ClassAdmin, recency: iam.RecencyAny, assist: AssistApprove},

	// --- beyond recovery --------------------------------------------- //
	//
	// A PURGE IS ADMIN'S, A PERSON'S, AND A STEP-UP. Nothing undoes it, so it
	// is the one tidy an author does not take for themselves (trash and
	// restore are theirs).
	ActionWorkPurge: {class: ClassAdmin, humanOnly: true, irreversible: true,
		recency: iam.RecencyStepUp, assist: AssistApprove},
	ActionPagePurge: {class: ClassAdmin, humanOnly: true, irreversible: true,
		recency: iam.RecencyStepUp, assist: AssistApprove},
	// A CHAT DESTRUCTION IS A PURGE'S: admin's, a person's or a credential's,
	// never an agent's (it could erase the evidence of what it said), at a
	// step-up. A room's retention is a prune on a delay, so a shorter one is the
	// same decision taken in advance.
	//
	// NOT SEATED, unlike every other chat verb: none is said in a room, so the
	// store records whoever made it under their own name, and none READS the
	// room, so an administrator in no private room can remove a pasted
	// credential without seeing what else was said. Destroying a room the
	// store does not hold destroys nothing and says so.
	ActionChatErase: {class: ClassAdmin, humanOnly: true, irreversible: true,
		recency: iam.RecencyStepUp, assist: AssistApprove},
	ActionChatPrune: {class: ClassAdmin, humanOnly: true, irreversible: true,
		recency: iam.RecencyStepUp, assist: AssistApprove},
	ActionChatRetention: {class: ClassAdmin, humanOnly: true, irreversible: true,
		recency: iam.RecencyStepUp, assist: AssistApprove},

	// --- the org chart ------------------------------------------------- //
	//
	// THE ORG CHART IS ITS LEADS' AS WELL AS ADMIN'S: a lead writes what lies
	// inside the units they lead ([ClassSubtree]). HUMAN-ONLY at the step-up,
	// as every write of the company document is: a seat rewriting its own org
	// is a model choosing its own team. A PERSON'S ASSISTANT carries it as
	// their own edit, approved from a before-and-after of the seats and units
	// it changes, at the same step-up (ADR-0052).
	ActionOrgWrite: {class: ClassSubtree, humanOnly: true, recency: iam.RecencyStepUp,
		assist: AssistApprove},
	// AND A LEAD READS WHAT THEY MAY WRITE, masked as every read of the
	// document is, without admin.
	ActionOrgRead: {class: ClassSubtree, recency: iam.RecencyAny, assist: AssistRead},

	// --- the company's own controls ------------------------------------ //
	ActionConfigRead:  {class: ClassAdmin, recency: iam.RecencyAny, assist: AssistRead},
	ActionConfigWrite: {class: ClassAdmin, recency: iam.RecencyStepUp, assist: AssistApprove},
	// THE PRINCIPLES, AS THE DOCUMENT IS: admin's, the write at the step-up.
	// The charter is the root, nobody's subtree, so no lead reaches either.
	ActionPrinciplesRead:  {class: ClassAdmin, recency: iam.RecencyAny, assist: AssistRead},
	ActionPrinciplesWrite: {class: ClassAdmin, recency: iam.RecencyStepUp, assist: AssistApprove},
	ActionSecretList:      {class: ClassAdmin, recency: iam.RecencyAny, assist: AssistRead},
	// A REVEAL HANDS OVER A CREDENTIAL'S VALUE, a person's to read, never a
	// pipeline's: the row refuses any request a token presented.
	ActionSecretReveal: {class: ClassAdmin, recency: iam.RecencyStepUp,
		presence: PresenceRequired, assist: AssistRead},
	ActionSecretWrite:  {class: ClassAdmin, recency: iam.RecencyStepUp, assist: AssistApprove},
	ActionSetupRead:    {class: ClassAdmin, recency: iam.RecencyAny, assist: AssistRead},
	ActionSetupConnect: {class: ClassAdmin, recency: iam.RecencyStepUp, assist: AssistApprove},
	// AN AGENT'S ACCESS AT A VENDOR is admin's at the step-up, as every write of
	// the company document is, and the assistant prepares it for approval: it
	// names no credential or variable, only what the seat may do. A RAISE TO AN
	// ADMINISTRATION ROLE also needs a person present, so no pipeline token can
	// make an agent a vendor's administrator; an approved assistant request is
	// that person deciding that one request (ADR-0051). HUMAN-ONLY: no agent
	// changes an agent's access, and choosing a grant request's option asks both
	// from inside comment_on_work_item (ADR-0118).
	ActionSetupAccessWrite: {class: ClassAdmin, humanOnly: true, recency: iam.RecencyStepUp,
		assist: AssistApprove},
	ActionSetupAccessRaise: {class: ClassAdmin, humanOnly: true, recency: iam.RecencyStepUp,
		presence: PresenceRequired, assist: AssistApprove},
	ActionFleetOperate: {class: ClassAdmin, recency: iam.RecencyStepUp, assist: AssistApprove},
	ActionFleetRead:    {class: ClassAdmin, recency: iam.RecencyAny, assist: AssistRead},
	// THE FLEET'S HEALTH IS ADMIN'S, as the `fleet` question is, and A SEAT'S
	// ONLY BY ITS ROLE'S GRANT: an agent whose role carries `health_tools` reads
	// both, every other is refused before the class is read ([rule.seatGrant]).
	// Reads, so no proof. The assistant reads them for an administrator as it
	// reads `fleet`.
	ActionFleetHealth: {class: ClassAdmin, seatGrant: SeatGrantHealth, recency: iam.RecencyAny,
		assist: AssistRead},
	ActionCodeStatus: {class: ClassAdmin, seatGrant: SeatGrantHealth, recency: iam.RecencyAny,
		assist: AssistRead},
	// AND READ FOR ANYBODY OF THE COMPANY WHO MAY ACT: a granted seat tells a
	// colleague or person the health, and nobody the company does not know nor
	// anybody who may not act in it now ([ActionFleetHealthRead]). Everybody's,
	// as the overview is: the grant decides what is read, this row for whom.
	ActionFleetHealthRead: {class: ClassEverybody, recency: iam.RecencyAny, assist: AssistRead},
	// A MODEL'S CHECKS AND ITS RETRY are admin's with no step-up: neither
	// writes the company or a credential, and the person reaching for them is
	// watching a model refuse right now. The assistant proposes each, since a
	// check spends tokens and a retry moves every node.
	ActionModelsCheck: {class: ClassAdmin, recency: iam.RecencyAny, assist: AssistApprove},
	ActionModelsRetry: {class: ClassAdmin, recency: iam.RecencyAny, assist: AssistApprove},
	// THE ANALYST'S RUN is admin's with no step-up, for a model check's reason;
	// the assistant proposes a run, since it spends tokens.
	ActionAdvisorRun: {class: ClassAdmin, recency: iam.RecencyAny, assist: AssistApprove},
	// AN MCP SERVER'S RESTART is admin's with no step-up, for a model retry's
	// reason; the assistant proposes it, since it ends every instance's session
	// on every node it names.
	ActionToolsRetry: {class: ClassAdmin, recency: iam.RecencyAny, assist: AssistApprove},
	// AN MCP SERVER'S TEST is admin's with no step-up, for the restart's
	// reason; the assistant proposes it, since it starts a process or dials an
	// address on every node it names.
	ActionToolsCheck: {class: ClassAdmin, recency: iam.RecencyAny, assist: AssistApprove},
	// A HELD TOOL CALL'S ANSWER is the seat's lead's or an administrator's, as
	// answering the run's own question is, and a person's alone: the level
	// holds the call for a person, so an agent answering would be an agent
	// approving an agent. No step-up for once or deny (often given from a phone
	// while the run waits); Always allow asks `config.write`'s in the handler.
	// The human-only list keeps the assistant from it.
	ActionToolsApprove: {class: ClassOwnOrLead, humanOnly: true, recency: iam.RecencyAny,
		assist: AssistApprove},
	// A PRODUCTION COMMAND IS APPROVED BY A PERSON THE COMPANY CHOSE, at a
	// step-up as any raise of power: the list the document names, or an
	// administrator, never a lead by position in the chart.
	ActionSSHOperateApprove: {class: ClassNamed, humanOnly: true, recency: iam.RecencyStepUp,
		assist: AssistApprove},
	// MONEY, WHAT CUSTOMERS SEE AND A VENDOR ACCOUNT'S OWN ACCESS are decided
	// by an administrator in person at a step-up, and never by a delegate: a
	// list the document names only routes the question (ADR-0171).
	ActionGrowthSpendApprove: {class: ClassAdmin, humanOnly: true, presence: PresencePerson,
		recency: iam.RecencyStepUp, assist: AssistApprove},
	ActionGrowthPublicApprove: {class: ClassAdmin, humanOnly: true, presence: PresencePerson,
		recency: iam.RecencyStepUp, assist: AssistApprove},
	ActionGrowthAdminApprove: {class: ClassAdmin, humanOnly: true, presence: PresencePerson,
		recency: iam.RecencyStepUp, assist: AssistApprove},
	// A SERVER IS REACHED BY AN AGENT ITS ROLE GRANTS IT TO (SSH on with a host
	// for a read, at operate for a command a person approves). The tools answer
	// only an agent's own run; a person reaches a server with their own key.
	ActionSSHRead: {class: ClassAdmin, seatGrant: SeatGrantSSHRead, recency: iam.RecencyAny,
		assist: AssistRead},
	ActionSSHOperate: {class: ClassAdmin, seatGrant: SeatGrantSSHOperate, recency: iam.RecencyAny,
		assist: AssistApprove},
	ActionSSHAskOperate: {class: ClassAdmin, seatGrant: SeatGrantSSHOperate, recency: iam.RecencyAny,
		assist: AssistApprove},
	// A MERGE OR A DEPLOY IS ASKED FOR BY ANY AGENT, in its own run: asking
	// changes nothing, and what an approval carries out is still the agent's
	// credential's and the target's rules'.
	ActionAskToMerge:  {class: ClassEverybody, recency: iam.RecencyAny, assist: AssistApprove},
	ActionAskToDeploy: {class: ClassEverybody, recency: iam.RecencyAny, assist: AssistApprove},
	// AND APPROVED BY A PERSON ANSWERABLE FOR IT, at a step-up as an SSH
	// command: the seat's lead, an administrator, or a person the kind's rule
	// names in escalate_to ([Object.Named]).
	ActionReleaseApprove: {class: ClassOwnOrLead, humanOnly: true, recency: iam.RecencyStepUp,
		assist: AssistApprove},
	// A DELEGATED REQUEST IS DECIDED BY A SEAT A RULE NAMED, which can never
	// request the kind it decides; the engine, not this row, bounds what its
	// allow carries out (ADR-0125).
	ActionReviewDelegated: {class: ClassAdmin, seatGrant: SeatGrantDelegate, recency: iam.RecencyAny,
		assist: AssistApprove},
	// WHAT DELEGATES DECIDED is read by the people they answer to: a rule's
	// escalate_to names, or an administrator.
	ActionDelegatedRead: {class: ClassNamed, humanOnly: true, recency: iam.RecencyAny, assist: AssistRead},
	// AND REACHED FOR ANYBODY OF THE COMPANY WHO MAY ACT, as the health tools
	// read ([ActionFleetHealthRead]): the grant decides what, this row for whom.
	ActionSSHRequester:    {class: ClassEverybody, recency: iam.RecencyAny, assist: AssistRead},
	ActionGrowthRequester: {class: ClassEverybody, recency: iam.RecencyAny, assist: AssistRead},
	// THE COMPANY-WIDE TRAIL (events, turns, phases, agent-to-agent channels,
	// the identity estate's audit) is admin's; one seat's trail is its lead's
	// too ([ActionSeatTrailRead]).
	ActionAuditRead: {class: ClassAdmin, recency: iam.RecencyAny, assist: AssistRead},

	// --- the identity directory ---------------------------------------- //
	//
	// A DIRECTORY READ IS THE PERSON IT IS ABOUT, OR ADMIN ([ClassDirectory]
	// says why the object is an id).
	ActionDirectoryRead: {class: ClassDirectory, recency: iam.RecencyAny, assist: AssistRead},
	// AND A DIRECTORY WRITE IS ADMIN'S, with NO self path: editing your own
	// permissions is the escalation this estate exists to close, so "my own
	// row" must not be a way in (the record layer also refuses conferring what
	// the caller does not hold).
	//
	// EVERY ONE (editing or removing somebody, withdrawing an invitation,
	// [ActionSignInWrite]'s four) asks the step-up and a person present: the
	// window keeps a week-old cookie away, the row's refusal of a token and
	// internal/iamdomain's refusal of an administrative record written through
	// one keep a stolen pipeline credential away.
	ActionDirectoryWrite: {class: ClassAdmin, recency: iam.RecencyStepUp,
		presence: PresenceRequired, assist: AssistApprove},
	ActionSignInWrite: {class: ClassAdmin, recency: iam.RecencyStepUp,
		presence: PresenceRequired, assist: AssistApprove},
	// MINTING AND REVOKING A MACHINE TOKEN, CHANGING HOW SOMEBODY PROVES WHO
	// THEY ARE, and ENDING SESSIONS are gestures a person legitimately makes
	// about themselves, so they carry the self path the writes above refuse.
	//
	// THE SELF PATH ADMITS A MACHINE TOKEN AS ITS OWNER (it acts as them, is
	// stepped up by construction), so WHO MUST BE PRESENT tells these three
	// apart ([rule.presence]): a token may revoke a token, itself included
	// (what somebody who finds one leaked reaches for); it may never change how
	// its owner proves who they are; about anybody else it may do none.
	ActionCredentialWrite: {class: ClassDirectory, recency: iam.RecencyStepUp,
		presence: PresenceRequiredForOthers, assist: AssistApprove},
	ActionCredentialProof: {class: ClassDirectory, recency: iam.RecencyStepUp,
		presence: PresenceRequired, assist: AssistApprove},
	// ENDING SESSIONS ASKS OPPOSITE PROOFS OF ITS TWO ARMS
	// ([rule.selfRecency]). The person themselves asks NONE: it is the first
	// response to an intruder, and re-proving a password the intruder may hold
	// would make it the slowest (internal/iamdomain's own revocation takes no
	// permission either). An administrator ending SOMEBODY ELSE's asks
	// `step_up` and a person present, as every directory write does: it stops
	// every pipeline the colleague runs, and a looping week-old cookie or token
	// would do it to the whole company. The self arm stays open to a token:
	// what it ends includes the token.
	ActionSessionEnd: {class: ClassDirectory, recency: iam.RecencyStepUp,
		selfRecency: iam.RecencyAny, presence: PresenceRequiredForOthers, assist: AssistApprove},
	// ENDING EVERY SESSION IN THE COMPANY is admin's, a person's, and a
	// step-up: it ends every authority at once, machine tokens included, and
	// cannot be taken back. A Tier A token holds admin, is not a machine token
	// and is fresh by construction, so the restore runbook's CLI step still
	// runs on the day nobody can sign in.
	ActionSessionInvalidate: {class: ClassAdmin, recency: iam.RecencyStepUp,
		presence: PresenceRequired, assist: AssistApprove},

	// --- artifacts ------------------------------------------------------- //
	//
	// A SHARE IS A RELATION AND THE APP GATES AUTHORING ONLY. Reading, filling
	// in data and sharing ask the role alone; creating, publishing, renaming,
	// adding an asset, reshaping the data and taking one out of circulation ask
	// the `artifacts` app beside it ([withApp]). See "An artifact is decided per
	// object, by its shares" in the package doc.
	//
	// LISTING IS EVERYBODY'S: a filter over what reading admits ([Visible]),
	// telling a caller nothing about the rest.
	ActionArtifactList: {class: ClassEverybody, recency: iam.RecencyAny, assist: AssistRead},
	// THE CATALOGUE IS EVERYBODY'S: it holds no row of any domain.
	ActionDashboardSources: {class: ClassEverybody, recency: iam.RecencyAny, assist: AssistRead},
	// THE FORMAT REFERENCE IS EVERYBODY'S, for the same reason.
	ActionArtifactFormat: {class: ClassEverybody, recency: iam.RecencyAny, assist: AssistRead},
	// A SOURCE'S PREVIEW IS A BUILDER'S, the app alone: the rows are decided by
	// the source's own question, as the viewer.
	ActionDashboardSourceRead: {class: ClassApp, app: iam.GrantArtifacts, recency: iam.RecencyAny,
		assist: AssistRead},
	ActionArtifactRead: {class: ClassShared, minRole: artifacts.RoleViewer,
		recency: iam.RecencyAny, assist: AssistRead},
	ActionArtifactRender: {class: ClassShared, minRole: artifacts.RoleViewer,
		recency: iam.RecencyAny, assist: AssistRead},
	// CREATING IS THE APP ALONE: no artifact yet to hold a relation to; the
	// creator becomes owner.
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
	// EVERY PARTICIPANT REACHES THE DATA and the collection decides: a viewer
	// may append to a contributing collection, an editor may be kept out of
	// another's private rows.
	ActionArtifactDataRead: {class: ClassShared, minRole: artifacts.RoleViewer,
		recency: iam.RecencyAny, assist: AssistRead},
	ActionArtifactDataQuery: {class: ClassShared, minRole: artifacts.RoleViewer,
		recency: iam.RecencyAny, assist: AssistRead},
	ActionArtifactDataWrite: {class: ClassShared, minRole: artifacts.RoleViewer,
		recency: iam.RecencyAny, steerable: true, assist: AssistApprove},
	// COMMENTS ARE THE COMMENTER'S RUNG (ADR-0154), with no app: commenting
	// is reading closely, not authoring. Reading threads is a viewer's. None
	// is steerable, so a turn's requester must hold the role too.
	ActionArtifactCommentList: {class: ClassShared, minRole: artifacts.RoleViewer,
		recency: iam.RecencyAny, assist: AssistRead},
	ActionArtifactComment: {class: ClassShared, minRole: artifacts.RoleCommenter,
		recency: iam.RecencyAny, assist: AssistApprove},
	ActionArtifactCommentResolve: {class: ClassShared, minRole: artifacts.RoleCommenter,
		recency: iam.RecencyAny, assist: AssistApprove},
	ActionArtifactCommentEdit: {class: ClassShared, minRole: artifacts.RoleCommenter,
		recency: iam.RecencyAny, assist: AssistApprove},
	ActionArtifactCommentRemove: {class: ClassShared, minRole: artifacts.RoleCommenter,
		recency: iam.RecencyAny, assist: AssistApprove},
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
	// A PURGE IS ADMIN'S, A PERSON'S AND A STEP-UP, as every purge is.
	ActionArtifactPurge: {class: ClassAdmin, humanOnly: true, irreversible: true,
		presence: PresenceRequired, recency: iam.RecencyStepUp, assist: AssistApprove},
	// OWNERSHIP MOVES BY A PERSON'S GESTURE, never a token's or a seat's: an
	// agent handing its work to itself, or a pipeline moving somebody's work
	// away, is the escalation this closes.
	ActionArtifactTransfer: {class: ClassShared, minRole: artifacts.RoleOwner,
		humanOnly: true, presence: PresenceRequired, recency: iam.RecencyAny, assist: AssistApprove},
	ActionArtifactPin: {class: ClassShared, minRole: artifacts.RoleViewer,
		humanOnly: true, recency: iam.RecencyAny, assist: AssistApprove},
	ActionArtifactExport: {class: ClassShared, minRole: artifacts.RoleEditor,
		recency: iam.RecencyAny, assist: AssistRead},
	ActionArtifactUsage: {class: ClassAdmin, recency: iam.RecencyAny, assist: AssistRead},
	// A RENDER REPORT is everybody's: it carries no artifact content.
	ActionArtifactReady: {class: ClassEverybody, recency: iam.RecencyAny, assist: AssistApprove},
	// A FRAME'S TICKET is everybody's as a gate ([ActionArtifactTicket]).
	ActionArtifactTicket: {class: ClassEverybody, recency: iam.RecencyAny, assist: AssistApprove},

	// --- artifact folders and the company's brand -------------------- //
	//
	// A FOLDER IS DECIDED AS AN ARTIFACT IS, on [KindArtifactFolder] (its owner
	// and top-level folder's shares). Creating one is the app, with the parent's
	// contributor role asked beside it ([ActionArtifactFolderFile]).
	ActionArtifactFolderCreate: {class: ClassApp, app: iam.GrantArtifacts,
		recency: iam.RecencyAny, steerable: true, assist: AssistApprove},
	ActionArtifactMove: {class: ClassShared, app: iam.GrantArtifacts,
		minRole: artifacts.RoleEditor, recency: iam.RecencyAny, steerable: true, assist: AssistApprove},
	ActionArtifactFolderFile: {class: ClassShared, app: iam.GrantArtifacts,
		minRole: artifacts.RoleContributor, recency: iam.RecencyAny, steerable: true, assist: AssistApprove},
	// MANAGING A FOLDER is the owner's and a person's: a model persuaded by a
	// row it read must not reorganise a folder a team files into.
	ActionArtifactFolderRename: {class: ClassShared, app: iam.GrantArtifacts,
		minRole: artifacts.RoleOwner, humanOnly: true, recency: iam.RecencyAny, assist: AssistApprove},
	ActionArtifactFolderMove: {class: ClassShared, app: iam.GrantArtifacts,
		minRole: artifacts.RoleOwner, humanOnly: true, recency: iam.RecencyAny, assist: AssistApprove},
	ActionArtifactFolderRemove: {class: ClassShared, app: iam.GrantArtifacts,
		minRole: artifacts.RoleOwner, humanOnly: true, recency: iam.RecencyAny, assist: AssistApprove},
	ActionArtifactFolderUnfile: {class: ClassShared, app: iam.GrantArtifacts,
		minRole: artifacts.RoleOwner, humanOnly: true, recency: iam.RecencyAny, assist: AssistApprove},
	// THE BRAND IS EVERYBODY'S TO READ, an administrator's, a person's and a
	// step-up's to change: adoption restyles every page the company publishes.
	ActionBrandRead: {class: ClassEverybody, recency: iam.RecencyAny, assist: AssistRead},
	ActionBrandAdopt: {class: ClassAdmin, humanOnly: true,
		presence: PresenceRequired, recency: iam.RecencyStepUp, assist: AssistApprove},
	ActionBrandClear: {class: ClassAdmin, humanOnly: true,
		presence: PresenceRequired, recency: iam.RecencyStepUp, assist: AssistApprove},

	// --- the Dashboard Assistant (ADR-0050 to ADR-0053) ---
	//
	// THE ASSISTANT'S OWN VERBS, never the assistant's ([assistantNever]),
	// HUMAN-ONLY and needing a PERSON PRESENT: a grant that could converse or
	// approve would be the assistant acting for itself.
	// TestTheHumanOnlyListIsExactlyTheIntendedSet holds every row here to it.
	//
	// EVERYBODY'S BY CLASS, still only a PERSON's: a machine token, service
	// account or Tier A session gets 403 assistant_not_available through
	// iam.Assistable, since only a person signed in through their browser has a
	// you the assistant may act as (decision D04).
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
	ActionSSHAuditRead: {class: ClassAdmin, humanOnly: true, recency: iam.RecencyAny, assist: AssistRead},
}

// RecencyOf reports how recent a proof a verb asks for, and whether the table
// knows the verb at all.
//
// EXPORTED FOR THE WALKS, for a caller that must SAY what a refusal it did
// not make would have needed (on [ClassOf]'s terms), and for a registration
// refusing a verb its transport cannot refuse in its own words
// (internal/api/queries' postures). A caller deciding for itself would be a
// second implementation. Use [Decide].
func RecencyOf(a Action) (iam.Recency, bool) {
	r, ok := ruleOf(a)
	return r.recency, ok
}

// IrreversibleOf reports whether nothing undoes a verb ([rule.irreversible]),
// and whether the table knows the verb at all.
//
// EXPORTED FOR WHAT SHOWS A GESTURE TO A PERSON first (the assistant's card),
// on [RecencyOf]'s terms; decides nothing.
func IrreversibleOf(a Action) (bool, bool) {
	r, ok := ruleOf(a)
	return r.irreversible, ok
}

// SelfRecencyOf reports the proof a verb asks of its SELF arm where that
// differs from [RecencyOf]'s, and empty where the row asks one window of
// everybody — see [rule.selfRecency].
//
// EXPORTED on [RecencyOf]'s terms. Use [Decide].
func SelfRecencyOf(a Action) (iam.Recency, bool) {
	r, ok := ruleOf(a)
	return r.selfRecency, ok
}

// PresenceOf reports which arms of a verb need a person present, and whether
// the table knows the verb at all — see [Presence].
//
// EXPORTED on [RecencyOf]'s terms. Use [Decide].
func PresenceOf(a Action) (Presence, bool) {
	r, ok := ruleOf(a)
	return r.presence, ok
}

// AppOf reports the app a verb belongs to, and whether the table knows the
// verb at all.
//
// EMPTY WITH ok IS A REAL ANSWER: a verb belonging to no app (overview, an
// agent's own, running the company, the directory). The pair lets a walk tell
// that from "nobody wrote a rule for this verb", which one empty string folds
// together.
//
// EXPORTED FOR THE WALKS, on [RecencyOf]'s terms. Use [Decide].
func AppOf(a Action) (iam.Grant, bool) {
	r, ok := ruleOf(a)
	return r.app, ok
}

// AnyOf reports the apps a [ClassAnyApp] verb admits on, any one of them —
// see [rule.anyOf] — and whether the table knows the verb at all. Nil for
// every other class.
//
// EXPORTED FOR THE WALKS, and for a caller that must NAME the set a refusal it
// did not make would have named. A copy, so no caller edits the row. Use
// [Decide].
func AnyOf(a Action) ([]iam.Grant, bool) {
	r, ok := ruleOf(a)
	return slices.Clone(r.anyOf), ok
}

// HumanOnlyOf reports whether NO AGENT may take a verb — see [rule.humanOnly]
// — and whether the table knows the verb at all.
//
// EXPORTED FOR THE WALKS, on [RecencyOf]'s terms. Use [Decide].
func HumanOnlyOf(a Action) (bool, bool) {
	r, ok := ruleOf(a)
	return r.humanOnly, ok
}

// SeatOnlyOf reports whether ONLY AN AGENT SEAT may take a verb — see
// [rule.seatOnly] — and whether the table knows the verb at all.
//
// EXPORTED FOR THE WALKS, on [RecencyOf]'s terms. Use [Decide].
func SeatOnlyOf(a Action) (bool, bool) {
	r, ok := ruleOf(a)
	return r.seatOnly, ok
}

// SeatGrantOf reports the role grant that decides an agent seat on a verb —
// see [rule.seatGrant] — empty where its class decides every principal, and
// whether the table knows the verb at all.
//
// EXPORTED FOR THE WALKS, on [RecencyOf]'s terms. Use [Decide].
func SeatGrantOf(a Action) (SeatGrant, bool) {
	r, ok := ruleOf(a)
	return r.seatGrant, ok
}

// MinRoleOf reports the least artifact role a verb's row admits (see
// [rule.minRole]), empty for a row that is not [ClassShared], and whether the
// table knows the verb at all.
//
// EXPORTED FOR THE WALKS, on [RecencyOf]'s terms. Use [Decide].
func MinRoleOf(a Action) (artifacts.Role, bool) {
	r, ok := ruleOf(a)
	return r.minRole, ok
}

// SteerableOf reports whether the requester ceiling spares an agent acting on
// its own object on this verb (see [rule.steerable]), and whether the table
// knows the verb at all.
//
// EXPORTED FOR THE WALKS, and for the one outside this package that pairs it
// with the tools' annotations: a steerable verb must be a write. Use
// [DecideWithin].
func SteerableOf(a Action) (bool, bool) {
	r, ok := ruleOf(a)
	return r.steerable, ok
}

// AssistOf reports what the Dashboard Assistant may do with a verb (see
// [rule.assist]), and whether the table knows the verb at all.
//
// EXPORTED FOR THE WALKS, here and wherever what the assistant is offered is
// held against what the table lets it take. Use [Decide].
func AssistOf(a Action) (AssistClass, bool) {
	r, ok := ruleOf(a)
	if !ok {
		return "", false
	}
	return assistOf(a, r), true
}

// assistOf is what the assistant may do with verb a, whose row is r: never
// for a verb on [assistantNever], and the row's own nature otherwise.
func assistOf(a Action, r rule) AssistClass {
	if _, never := assistantNever()[a]; never {
		return AssistNever
	}
	return r.assist
}

// AssistNeverReason is why the Dashboard Assistant never takes a verb, as a
// reviewer reads it, and whether the human-only list names the verb at all:
// for the walk that holds the list to the set it is meant to be.
func AssistNeverReason(a Action) (string, bool) {
	entry, never := assistantNever()[a]
	return entry.why, never
}

// AssistNeverSentence is what the person is told when their assistant meets a
// verb on the human-only list: why it stays with them, in plain words, and
// where they do it themselves. Empty for a verb the list does not name.
//
// ONE SENTENCE PER FAMILY, so one kind of gesture gets one answer and never a
// verb's internal name.
func AssistNeverSentence(a Action) string { return assistantNever()[a].tell }

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
	tellSecret   = "A secret's value never passes through Ask Crewlet, so this stays with " +
		"you: use Secrets in Settings."
	tellSetup = "Connecting an integration means entering its credentials, so it stays with " +
		"you: use Integrations."
	tellFrame = "An artifact's page opens from the artifact itself: open it from Artifacts."
	tellOwn   = "Ask Crewlet never approves, declines or reads its own conversations: that " +
		"stays with you."
	tellSeat      = "Only an agent does this, as part of its own work."
	tellKnowledge = "Asking the knowledge base's own model would spend outside this " +
		"conversation's budget, so Ask Crewlet searches the pages itself instead."
	tellReady    = "Only an open artifact reports that it has finished drawing."
	tellApproval = "A call an agent waits on is approved by a person, so this stays with " +
		"you: answer it from the Inbox or Settings › MCP & tools."
	tellCheck = "A check result is reported by the CI that ran the check, with its own token: " +
		"Ask Crewlet runs no CI."
	tellSSHAudit = "An SSH command's text can carry a credential typed into it, so the SSH audit " +
		"stays with you: read it from Integrations."
)

// assistantNever is THE HUMAN-ONLY LIST: every verb the Dashboard Assistant
// never takes, each with a reviewer's reason and the person's sentence
// (ADR-0052). Every other verb is the person's own (ADR-0050, ADR-0051).
// FIVE KINDS AND NO OTHER; a verb joins only in a reviewed diff that names
// which: a sign-in or session ceremony; an answer or body carrying a
// credential in clear (it would sit in a model's context and stored
// proposal); the assistant's own decisions; a gesture whose effect a grant
// would falsify (spend outside the turn's meter, a render or check result only
// its source reports); a person's approval of an agent's call. Seat-only verbs
// are kept out by parity alone. BUILT AT FIRST USE: Gitlet's push hook pays
// every init.
var assistantNever = sync.OnceValue(func() map[Action]neverEntry {
	return map[Action]neverEntry{
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
		ActionWithdrawAsk: {"an agent seat's own verb: a person resolves their own question from its " +
			"work item, and withdraws nothing an agent's run waits on", tellSeat},
		ActionRequestAccess: {"an agent seat's own verb: a person changes an agent's access in the " +
			"organization, and asks nobody for it", tellSeat},
		ActionAtlasRecord: {"an agent seat's own verb: a person records in the atlas in their own rank, " +
			"with the tools that add, correct and confirm", tellSeat},
		ActionAtlasDispute: {"an agent seat's own verb: a person who finds the atlas wrong corrects or " +
			"retires what it holds", tellSeat},
		ActionSessionsList: {"an agent seat's own verb: only an agent seat keeps executor sessions, and a " +
			"person reads a seat's as its trail", tellSeat},
		ActionSessionRead: {"an agent seat's own verb: only an agent seat keeps executor sessions, and a " +
			"person reads a seat's as its trail", tellSeat},
		ActionCodeReportCheck: {"a check's result is reported by the CI that ran it, and an assistant runs " +
			"no CI, so its report would be a result nobody ran", tellCheck},
		ActionToolsApprove: {"a tool at Needs approval holds each call for a person's decision, " +
			"and the assistant deciding it would be a model approving a model's call", tellApproval},
		ActionSSHOperateApprove: {"an agent's command on a production server runs only as a person " +
			"approved it, and the assistant approving it would be a model approving a model's command", tellApproval},
		ActionGrowthSpendApprove: {"ad spend an agent asked for starts only as an administrator approved " +
			"it, and the assistant approving it would be a model approving a model's spend", tellApproval},
		ActionGrowthPublicApprove: {"what an agent asked to publish goes out only as an administrator " +
			"approved it, and the assistant approving it would be a model approving a model's words", tellApproval},
		ActionGrowthAdminApprove: {"an agent's change to a vendor account's users, keys, billing or policies " +
			"happens only as an administrator approved it, and the assistant approving it would be a model " +
			"approving a model's change", tellApproval},
		ActionReleaseApprove: {"a merge or a deploy an agent asked for goes ahead only as a person or a " +
			"delegate approved it, and the assistant approving it would be a model approving a model's request",
			tellApproval},
		ActionSSHAuditRead: {"the SSH audit answers each command's text in clear, which can carry a " +
			"credential typed into it", tellSSHAudit},
	}
})

// SeatedOf reports whether a verb is made AS A SEAT — see [rule.seated] — and
// whether the table knows the verb at all.
//
// EXPORTED FOR THE WALKS, on [RecencyOf]'s terms. Use [Decide].
func SeatedOf(a Action) (bool, bool) {
	r, ok := ruleOf(a)
	return r.seated, ok
}

// Actions is every verb this build authorizes, sorted, for the walks that ask
// whether a surface covered all of them: the table's and every registered
// growth catalog's tools ([ruleOf]).
func Actions() []Action {
	return slices.Compact(slices.Sorted(slices.Values(append(slices.Collect(maps.Keys(rules)),
		growthVerbs()...))))
}

// ClassOf reports which rule governs a verb.
//
// EXPORTED FOR THE WALKS, not for a caller to branch on: a surface deciding
// for itself would be a second implementation. Use [Decide].
func ClassOf(a Action) (Class, bool) {
	r, ok := ruleOf(a)
	return r.class, ok
}

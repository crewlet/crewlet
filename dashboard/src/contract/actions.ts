/**
 * Every change the dashboard makes, as the tool the engine runs for it.
 *
 * The dashboard writes through ONE route — `POST /operator/act/{tool}`, as the
 * principal the request resolves to (ADR-0024): a person the identity
 * directory binds to a seat writes as that seat, a credential nobody is bound
 * through under its own login, and nobody is refused for being unbound — what
 * each may do is the authority table's, as on every surface. The tools behind
 * it are the operator catalogue's, the same implementations a person's own
 * assistant calls. This table is what the dashboard is allowed to send there:
 * `protocol/act.ts` takes only a key of it, and only the arguments its row
 * names, as the body's `args`; the operation is the request's
 * `Idempotency-Key`, minted once per gesture and sent again on its retry,
 * never an `op_id` argument, which the route refuses.
 *
 * ONE ROW PER TOOL A CONTROL PRESSES, and no other: `app/source.test.ts` holds
 * the keys to the `useAct("…")` literals outside the suites, both ways, so a
 * row lands with the control that sends it rather than ahead of it.
 *
 * Each row is:
 *
 *  - `args` — the arguments a screen may pass. Every one is a property of the
 *    tool's own schema, and every property the schema REQUIRES is here;
 *  - `domain` — the log the write lands in, whose position raises this tab's
 *    read floor for that domain (`protocol/session.ts`), or `null` for a
 *    write that lands in no log a question reads. A wrong one is SILENT: a
 *    write filed under `null` raises no floor, so the screen's next read may
 *    come from before the press;
 *  - `apps` — the apps ANY ONE of which the change needs, in the order its
 *    authority row names them: its app for an app's change (Gitlet for every
 *    change on the code log, ADR-0110), the row's own set for one that draws
 *    on whichever of several the caller holds (`answer_knowledge`, the
 *    tracker and the knowledge base it answers from), and none for a change
 *    no app holds (pausing a seat, answering an executor). What a control
 *    the engine does not offer is held with —
 *    "needs Worklet access" — so a reader is told what to ask an admin for
 *    (`lib/useWriteAccess.ts`). It is the authority table's, stated per row
 *    rather than guessed from the domain: a change can need an app and land
 *    in no log;
 *  - `refreshes` — questions OUTSIDE that domain's session set
 *    (`contract/domains.ts`) that the write also moves, asked again without a
 *    floor because they cannot take one;
 *  - `scope` — `person` for a change somebody makes to the company's work in
 *    their own name, `company` for one that rewrites what every seat's tracker
 *    means (a project's settings, the catalogue), which a screen offers only
 *    where it says so. It is the authority table's: a write decided by a
 *    container's lead or by admin is `company`.
 *
 * HELD AGAINST THE REAL CATALOGUE by `internal/api/operator`'s
 * `TestEveryActionTheDashboardTakesIsOneTheActTransportServes`: every tool is
 * served by the act transport and is not a read, every argument is one its
 * schema takes, every required one is present, every domain is the log its
 * tool writes, and every scope and app set is the one its authority row
 * gives it. A renamed argument in Go is a red build here
 * rather than a button the tools' own gate refuses `invalid_body` — an
 * argument the tool does not declare — the first time somebody presses it.
 */
/** Every question the atlas's screens ask, each moved by any write to it. */
const ATLAS_READS = [
  "atlas_entries",
  "atlas_map",
  "atlas_entry",
  "atlas_trace",
  "atlas_facets",
  "atlas_log",
  "atlas_activity",
  "atlas_review",
] as const;

export const ACTIONS = {
  create_work_item: {
    // THE PALETTE'S TWO CREATES: "Create task" (a title, filed where the
    // person's own create lands, or on the project they are looking at) and
    // "Ask" (the question as the title, `ask` naming the agent who owes the
    // answer, which lands in the asker's inbox).
    //
    // AND THE TASK PAGE'S "+ SUB-TASK": a title filed under the task it is
    // on (`parent`), in that task's project.
    //
    // AND THE NEW TASK SHEET (`routes/worklet/NewTask.tsx`), which files the
    // whole of a task in one record: its type, the status it starts in (a
    // board lane's "+" names its own), its priority, due date and labels.
    args: [
      "title",
      "body",
      "project",
      "assignee",
      "ask",
      "parent",
      "type",
      "status",
      "priority",
      "due",
      "labels",
    ],
    domain: "tracker",
    apps: ["worklet"],
    refreshes: ["work_search"],
    scope: "person",
  },
  answer_knowledge: {
    // A WRITE THAT CHANGES NOTHING, because it SPENDS: one model call per
    // question, and a read would be asked again on every refocus and
    // reconnect. It lands in no log, so it raises no floor.
    args: ["q"],
    domain: null,
    apps: ["worklet", "doclet"],
    refreshes: [],
    scope: "person",
  },
  update_work_item: {
    // `linked` AND `linked_pages` ARE THE INBOX COMPOSER'S "Link a task" and
    // "Attach a page": each is a delta (`{add: [...]}`) on the item the
    // conversation is about, never the whole relation set.
    //
    // THE TASK PAGE EDITS THE REST: the title and description, the labels
    // (which REPLACE the set), the schedule, a custom field by slug, one
    // checklist gesture at a time, and whether the person follows it.
    args: [
      "item",
      "assignee",
      "reason",
      "status",
      "priority",
      "if_match",
      "linked",
      "linked_pages",
      "title",
      "body",
      "labels",
      "due",
      "start",
      "estimate_minutes",
      "points",
      "fields",
      "checklist",
      "watch",
    ],
    domain: "tracker",
    apps: ["worklet"],
    refreshes: ["work_search"],
    scope: "person",
  },
  restore_work_item: {
    args: ["item"],
    domain: "tracker",
    apps: ["worklet"],
    refreshes: ["work_search"],
    scope: "person",
  },
  write_project: {
    // "EDIT PROJECT" on a project's page: the lead's target date. A project's
    // settings are what every seat's tracker reads, so this is a COMPANY
    // write, offered only on the project's own page — and the engine decides
    // who may make it: the project's lead, or an admin.
    args: ["project", "target_date"],
    domain: "tracker",
    apps: ["worklet"],
    refreshes: [],
    scope: "company",
  },
  set_pins: {
    args: ["views", "favorites"],
    domain: "tracker",
    apps: ["worklet"],
    refreshes: [],
    scope: "person",
  },
  set_priorities: {
    // MY WORK'S REORDER: a row of somebody's priorities dragged, or moved
    // with Alt and an arrow, to a new place. The WHOLE list is sent, made
    // from the person's own stored one (`routes/me/reorder.ts`), and it is
    // conditional on the record's `version` — a reorder from a screen that
    // read an older order is refused rather than putting that order back.
    // `handle` is whose queue: the reader's own, or a report's for a lead.
    args: ["handle", "items", "if_match"],
    domain: "tracker",
    apps: ["worklet"],
    refreshes: [],
    scope: "person",
  },
  comment_on_work_item: {
    // A DECISION ANSWERED IN PLACE: the option button sends the choice as an
    // answer to the ask it belongs to, which closes the ask and wakes the
    // asker with what was chosen.
    //
    // AND A REPLY IN THE ASK'S OWN THREAD (`reply_to`), which answers
    // nothing: a person asking the asker a question back leaves the ask open.
    //
    // AND AN ASK FROM THE TASK PAGE: a question put to one seat (`ask`),
    // optionally with the options it is to choose between (`decision`).
    args: ["item", "answers", "choice", "body", "reply_to", "ask", "decision"],
    domain: "tracker",
    apps: ["worklet"],
    refreshes: [],
    scope: "person",
  },
  answer_executor: {
    // A PARKED EXECUTOR ANSWERED BY ITS TURN. It lands on the seat's
    // inbox rather than in a log a question reads, so it raises no floor;
    // the executor's record moves when the node holding the seat resumes it.
    //
    // AN EXECUTOR THAT ASKED A PERSON TO CHOOSE is answered with one of its options
    // (`choice`), words (`answer`), or both: a choice with a note.
    //
    // THE QUESTION THE READER SAW (`launch_id`, the run's own on the row the
    // screen drew) rides every answer: a run that moved on to another
    // question refuses it `conflict` rather than taking an answer to a
    // question nobody showed.
    args: ["turn_id", "launch_id", "choice", "answer"],
    domain: null,
    apps: [],
    refreshes: ["executors", "decisions"],
    scope: "person",
  },
  pause_seat: {
    // A SEAT'S PROFILE PAUSES IT: its mail waits, its schedules are skipped,
    // and the turn it is on finishes first unless `stop_running` asks for it
    // to end at its next round — and its executor in its box to end at once.
    // The pause is a coordination record rather
    // than a log a question reads, so it raises no floor; the seat's state
    // moves on the agents push, which every node sends when its watch lands.
    args: ["handle", "reason", "stop_running"],
    domain: null,
    apps: [],
    refreshes: [],
    scope: "person",
  },
  resume_seat: {
    // AND RESUMES IT: the mail that waited is delivered first, in order.
    args: ["handle"],
    domain: null,
    apps: [],
    refreshes: [],
    scope: "person",
  },
  steer_turn: {
    // STEER ON THE TURN TRACE: a note to a turn running now, read at its
    // next step — a round of the engine's loop, or a step of its executor in
    // its box. It crosses to the node running the turn on an ephemeral
    // ask, or waits on the executor's record in the coordination store, never in
    // a log, so it raises no floor; what became of it is the turn's own
    // `agent_turn_steered`, which the trace re-reads.
    args: ["turn_id", "note"],
    domain: null,
    apps: [],
    refreshes: ["turn"],
    scope: "person",
  },
  write_page: {
    // "SAVE AS A PAGE" in the Inbox's composer: what the person wrote becomes
    // a new page in the knowledge base, which is then attached to the item
    // (`update_work_item{linked_pages}`) — the long form a comment's size
    // limit sends a writer to.
    //
    // AND DOCLET'S COMPOSER: "New page" and a tree's Create,
    // filed in a space at its top or under a page (`parent`), with the
    // composer's `labels`, and Save as draft (`status: "draft"`). A folder is
    // the same write with an empty body: a page that only groups pages.
    args: ["title", "body", "container", "parent", "labels", "status"],
    domain: "pages",
    apps: ["doclet"],
    refreshes: ["knowledge"],
    scope: "person",
  },
  save_page: {
    // "EDIT" ON A PAGE: the body the person rewrote, saved against the
    // version they opened (`base_version`), so a save that raced somebody
    // else's is refused `stale_version` rather than overwriting it. A
    // one-line `message` says what changed, as a seat's save does.
    //
    // AND A PAGE'S WATCH EYE: `watch` alone, against the version on screen,
    // follows the page as the person or stops following it.
    args: ["page", "base_version", "body", "message", "watch"],
    domain: "pages",
    apps: ["doclet"],
    refreshes: ["knowledge"],
    scope: "person",
  },
  comment_on_page: {
    // A PAGE'S THREAD: a remark, or a reply to one (`reply_to`).
    args: ["page", "body", "reply_to"],
    domain: "pages",
    apps: ["doclet"],
    refreshes: [],
    scope: "person",
  },
  place_work_item: {
    // A BOARD DRAG: the card dropped `before` a neighbour, or `after` the last
    // card of a lane, and into another lane with `status`. The engine mints
    // the place between the neighbours as the board stands when it lands.
    args: ["item", "before", "after", "status", "if_match"],
    domain: "tracker",
    apps: ["worklet"],
    refreshes: ["work_search"],
    scope: "person",
  },
  save_work_view: {
    // "+ VIEW": the query on screen, saved under a name on this container,
    // shared, or kept to the person who saved it with `personal` — whose
    // record it is the engine decides from the caller, never from a name a
    // screen sends.
    args: ["container", "name", "type", "params", "personal"],
    domain: "tracker",
    apps: ["worklet"],
    refreshes: [],
    scope: "person",
  },
  // `seen` IS HOW FAR THIS PAGE READ THE DOMAIN'S LOG (the notice's own
  // position, the furthest of those a mark names, or the newest loaded): the
  // mark is decided by whichever data node is asked, and it waits for that
  // position rather than refusing a notice the person was shown as unknown.
  mark_inbox: {
    args: ["read", "unread", "snooze", "unsnooze", "read_through", "seen"],
    domain: "tracker",
    apps: ["worklet"],
    refreshes: [],
    scope: "person",
  },
  // ---- THE COMPANY'S OWN CHAT (`routes/chatlet`) -------------------------- //
  //
  // Every gesture the chat screen makes, each the tool a person's own
  // assistant calls for it, made as the seat the session holds. The handles
  // a message names and whether it addressed the whole room are the ENGINE's
  // reading of the body — the tool takes no `mentions` and no flag — so a
  // message is sent as what was typed.
  post_message: {
    // THE COMPOSER'S SEND. A message is sequenced and arbitrates nothing at
    // the broker, so the operation key (the request's `Idempotency-Key`) is
    // what tells a retry from a second remark: the outbox repeats an
    // unanswered message under the key it was first sent with
    // (`routes/chatlet/outbox.ts`).
    args: ["channel", "body", "links"],
    domain: "chat",
    apps: ["chatlet"],
    refreshes: [],
    scope: "person",
  },
  reply_in_thread: {
    // THE THREAD PANE'S SEND: `reply_to` is the thread's root, and the engine
    // files a reply to a reply under the same one.
    args: ["channel", "reply_to", "body", "links"],
    domain: "chat",
    apps: ["chatlet"],
    refreshes: [],
    scope: "person",
  },
  edit_message: {
    // A ROW'S "EDIT", offered on the reader's own messages only: the engine
    // refuses an edit from anybody but the author, an admin's included.
    args: ["channel", "message_id", "body"],
    domain: "chat",
    apps: ["chatlet"],
    refreshes: [],
    scope: "person",
  },
  delete_message: {
    // A ROW'S "DELETE": a tombstone the thread keeps its root through. The
    // author's own, or an admin's moderation of anybody's — so it is offered
    // on the reader's own messages, and on everybody's to an admin.
    args: ["channel", "message_id"],
    domain: "chat",
    apps: ["chatlet"],
    refreshes: [],
    scope: "person",
  },
  react_to_message: {
    // AN EMOJI ON A MESSAGE, or taken back (`remove`).
    args: ["channel", "message_id", "emoji", "remove"],
    domain: "chat",
    apps: ["chatlet"],
    refreshes: [],
    scope: "person",
  },
  join_channel: {
    // "JOIN" on a room the reader may read and is not in — the room's own
    // header, and the browse dialog's rows.
    args: ["channel"],
    domain: "chat",
    apps: ["chatlet"],
    refreshes: [],
    scope: "person",
  },
  leave_channel: {
    args: ["channel"],
    domain: "chat",
    apps: ["chatlet"],
    refreshes: [],
    scope: "person",
  },
  create_channel: {
    // "NEW ROOM": the name it claims (`channel`, as every chat tool names its
    // room), who may read it, its topic, and the people in it from the start.
    // A unit's room is the org chart's and is never made here.
    args: ["channel", "kind", "topic", "members"],
    domain: "chat",
    apps: ["chatlet"],
    refreshes: [],
    scope: "person",
  },
  open_dm: {
    // "NEW DIRECT MESSAGE": the others in it. The author is added by the
    // engine, and opening one that exists is that room.
    args: ["to"],
    domain: "chat",
    apps: ["chatlet"],
    refreshes: [],
    scope: "person",
  },
  update_channel: {
    // A ROOM'S TOPIC AND DESCRIPTION DIALOGS, and only what moved; the
    // template a person set them from, which the transcript records as one
    // line; and an administrator's auto-join switch in Add people. Archiving
    // is not offered there (it closes the room for everybody in it and is its
    // creator's, its unit's lead's or an admin's), and neither is how long the
    // room keeps what is said, which is an admin's own gesture.
    args: ["channel", "topic", "purpose", "template", "auto_join"],
    domain: "chat",
    apps: ["chatlet"],
    refreshes: [],
    scope: "person",
  },
  add_channel_members: {
    // "ADD PEOPLE": the people and agents to put in the room, and nobody
    // taken out. The engine decides it against the room as it stands, so two
    // people adding at once both land.
    args: ["channel", "members"],
    domain: "chat",
    apps: ["chatlet"],
    refreshes: [],
    scope: "person",
  },
  set_channel_members: {
    // A MEMBER'S "GETS EVERY MESSAGE" SWITCH in the room's Members tab: the
    // whole membership with one agent's `follow_all` moved, stated against
    // the membership the tab read (`expected_members`), so a change somebody
    // made meanwhile is refused rather than written back over.
    args: ["channel", "members", "expected_members"],
    domain: "chat",
    apps: ["chatlet"],
    refreshes: [],
    scope: "person",
  },
  mark_chat_read: {
    // HOW FAR THE READER HAS READ, flushed on a timer and on the way out
    // (`routes/chatlet/cursor.ts`), and a room's MUTE. A person's attention is a
    // coordination record rather than a record on the chat log, so it lands
    // in no log a question reads and raises no floor.
    //
    // IT MOVES THE RAIL, and asks it nothing here. `chat_channels` answers
    // each room's unread count and mute from this very record, and the
    // write's answer IS the record as it left it, named by its seat — the
    // form the `chat_cursor_moved` push takes, which reaches every tab of the
    // seat's, this one included. The press applies that answer to the tab's
    // read state (`markRead`), and the rail asks again once the revision
    // moves, whichever of the two arrives first. A `refreshes` here asked a
    // second time whenever the push won the race, and the second ask
    // discarded the first after the engine had counted every room's unread.
    //
    // A THREAD'S OWN PLACE rides it too (`thread_cursors`), and "Mark unread"
    // is `rewind`: the one way a room's place moves back, set exactly.
    //
    // AND THE READER'S OTHER MARKS, on the same record and the same push: how
    // a room reaches them (`notify`), a room starred or unstarred, a message
    // saved or unsaved, a thread followed or unfollowed, each named alone, so
    // two tabs marking two things at once both land.
    args: [
      "cursors",
      "thread_cursors",
      "rewind",
      "mute",
      "unmute",
      "notify",
      "star",
      "unstar",
      "save",
      "unsave",
      "follow",
      "unfollow",
    ],
    domain: null,
    apps: ["chatlet"],
    refreshes: [],
    scope: "person",
  },
  // GITLET'S HALF OF AN INBOX'S MARKS: each domain keeps the marks over
  // its own notices on its own log, so a code notice is marked here and
  // "Mark all read" sends both, each with its own `seen`. The person's own
  // record, and the Gitlet app's as every code change is (ADR-0110): a reader
  // without it is listed no code notice to mark.
  code_mark_inbox: {
    args: ["read", "unread", "snooze", "unsnooze", "read_through", "seen"],
    domain: "code",
    apps: ["gitlet"],
    refreshes: [],
    scope: "person",
  },
  // A REPOSITORY'S LIFE (`routes/gitlet/repo/RepoWrites.tsx`,
  // `routes/gitlet/ImportRepo.tsx`). Each lands on the code log, whose position
  // raises this tab's code floor, and the repository's row and the listing
  // are both in that domain's session set. Settings › Search mode and the
  // mirrors, each one settings change of its own; a mirror is a person's
  // alone, which the deciding data node holds every asker to (U49 a).
  code_update_repo: {
    args: ["repo", "search", "mirror_from", "mirror_to"],
    domain: "code",
    apps: ["gitlet"],
    refreshes: [],
    scope: "person",
  },
  // PURGE UNREACHABLE HISTORY, asked of the repository's maintainer once the
  // person has read who keeps copies (a ConfirmModal): human-only, step-up.
  code_purge_unreachable: {
    args: ["repo"],
    domain: "code",
    apps: ["gitlet"],
    refreshes: [],
    scope: "person",
  },
  // IMPORT A REPOSITORY from another host, or follow one as a pull mirror
  // (`mirror`): human-only. `credential` is the variable's NAME.
  code_import_repo: {
    args: ["name", "url", "credential", "owner", "visibility", "default_branch", "mirror"],
    domain: "code",
    apps: ["gitlet"],
    refreshes: [],
    scope: "person",
  },
  // A CHANGE'S PAGE (`routes/gitlet/change/`). Every one lands on the code log,
  // whose position raises this tab's code floor, and every question a change
  // page reads is in that domain's session set — its routing too — so none
  // refreshes anything beyond it. Each names the change by its reference
  // (`change`, web!42), which carries its repository, so `repo` is a
  // property the tool takes and the page never needs to send.
  code_review_change: {
    // APPROVE, REQUEST CHANGES AND COMMENT: one review of one revision, with
    // the findings the reader drafted on its lines (kept in the tab's
    // session storage until this one press sends them all), and `carries`,
    // the reader's own standing findings it keeps standing.
    args: ["repo", "change", "revision", "verdict", "summary", "findings", "carries"],
    domain: "code",
    apps: ["gitlet"],
    refreshes: [],
    scope: "person",
  },
  code_comment_on_change: {
    // THE CONVERSATION'S COMPOSER: a comment of its own; a reply to a thread
    // or a finding; one on a line (`anchor`); and Resolve on a thread.
    args: ["repo", "change", "body", "thread", "finding", "anchor", "resolve"],
    domain: "code",
    apps: ["gitlet"],
    refreshes: [],
    scope: "person",
  },
  code_request_review: {
    // REQUEST REVIEW: ask reviewers, or take an ask back.
    args: ["repo", "change", "reviewers", "withdraw", "note"],
    domain: "code",
    apps: ["gitlet"],
    refreshes: [],
    scope: "person",
  },
  code_update_change: {
    // READY FOR REVIEW on a draft (`draft: false`), against the version on
    // screen, so a mark that raced a push or an edit is refused rather than
    // readying what the reader did not see.
    args: ["repo", "change", "expected_version", "draft"],
    domain: "code",
    apps: ["gitlet"],
    refreshes: [],
    scope: "person",
  },
  code_close_change: {
    // CLOSE AND REOPEN, against the version on screen (`expected_version`),
    // so a close that raced a push is refused rather than closing what the
    // reader did not see.
    args: ["repo", "change", "expected_version", "reason", "reopen"],
    domain: "code",
    apps: ["gitlet"],
    refreshes: [],
    scope: "person",
  },
  code_open_change: {
    // A COMPARISON'S OPEN CHANGE (`routes/gitlet/commit/ComparePage.tsx`): the
    // branch compared, proposed into the base branch, its title, description,
    // work item (prefilled with the one the push that brought its head was
    // charged to), reviewers to ask once it is open, and whether it opens as
    // a draft. It names its repository, since no change exists yet to carry
    // one.
    args: ["repo", "branch", "target", "title", "body", "draft", "reviewers", "work_item"],
    domain: "code",
    apps: ["gitlet"],
    refreshes: [],
    scope: "person",
  },
  code_apply_suggestions: {
    // APPLY SUGGESTION on a finding: a commit on the change's branch putting
    // the suggested lines in place, against the head on screen
    // (`expected_head`), its finding resolved as a step.
    args: ["repo", "change", "findings", "expected_head", "message"],
    domain: "code",
    apps: ["gitlet"],
    refreshes: [],
    scope: "person",
  },
  code_merge_change: {
    // MERGE, ADD TO QUEUE AND OVERRIDE (`routes/gitlet/change/Merge.tsx`): the
    // head on screen (`expected_head`), how it merges, whether its branch
    // goes and its work item closes, and — Override's — a person's reason to
    // merge past the gate. Its record lands on the code log; a work item it
    // closes is a second write on Worklet's, which the change page does not
    // read.
    args: [
      "repo",
      "change",
      "expected_head",
      "method",
      "delete_branch",
      "close_task",
      "override_reason",
    ],
    domain: "code",
    apps: ["gitlet"],
    refreshes: [],
    scope: "person",
  },
  code_rerun_checks: {
    // RE-RUN CHECKS: every check of the change's head whose latest attempt
    // did not pass, each at its next attempt (`all_failed`), on the checks
    // log the change's checks tab reads.
    args: ["repo", "change", "all_failed"],
    domain: "code",
    apps: ["gitlet"],
    refreshes: [],
    scope: "person",
  },
  // COMPANY › ATLAS (`routes/atlas`): what a holder of Atlas adds, corrects and
  // decides. THE ATLAS'S LOG IS NO SESSION DOMAIN: its questions read at the
  // dashboard's level and take no floor, so each row names them as
  // `refreshes` and they are asked again once the write is answered.
  //
  // ADD ENTRY records the entry with what its dialog knows of it, in one call.
  atlas_add_entry: {
    args: ["kind", "key", "name", "purpose", "belongs_to", "seat", "unit", "doclet_page"],
    domain: null,
    apps: ["atlas"],
    refreshes: ATLAS_READS,
    scope: "person",
  },
  // A FACT'S VALUE as the person knows it, or `wrong` for the value shown.
  atlas_correct: {
    args: ["atlas_entry", "fact", "value", "wrong"],
    domain: null,
    apps: ["atlas"],
    refreshes: ATLAS_READS,
    scope: "person",
  },
  // CONFIRM: the entry, or in Review one value or one relation its sources
  // disagree on.
  atlas_confirm: {
    args: ["atlas_entry", "fact", "value", "relation", "to"],
    domain: null,
    apps: ["atlas"],
    refreshes: ATLAS_READS,
    scope: "person",
  },
  // A RELATION DRAWN to another entry, or to the Doclet page that documents it.
  atlas_relate: {
    args: ["atlas_entry", "relation", "to", "doclet_page"],
    domain: null,
    apps: ["atlas"],
    refreshes: ATLAS_READS,
    scope: "person",
  },
  atlas_unrelate: {
    args: ["atlas_entry", "relation", "to", "doclet_page"],
    domain: null,
    apps: ["atlas"],
    refreshes: ATLAS_READS,
    scope: "person",
  },
  atlas_set_owner: {
    args: ["atlas_entry", "seat", "unit", "clear"],
    domain: null,
    apps: ["atlas"],
    refreshes: ATLAS_READS,
    scope: "person",
  },
  atlas_retire: {
    args: ["atlas_entry"],
    domain: null,
    apps: ["atlas"],
    refreshes: ATLAS_READS,
    scope: "person",
  },
  atlas_restore: {
    args: ["atlas_entry"],
    domain: null,
    apps: ["atlas"],
    refreshes: ATLAS_READS,
    scope: "person",
  },
  atlas_merge: {
    args: ["atlas_entry", "other"],
    domain: null,
    apps: ["atlas"],
    refreshes: ATLAS_READS,
    scope: "person",
  },
  atlas_split: {
    args: ["atlas_entry", "other"],
    domain: null,
    apps: ["atlas"],
    refreshes: ATLAS_READS,
    scope: "person",
  },
  // REFRESH NOW (Atlas › Sources): an ask that one source be read, which the
  // node holding the collect duty honours within a minute. It writes no log,
  // so the sources' standings are asked again once it is answered.
  atlas_refresh: {
    args: ["integration"],
    domain: null,
    apps: ["atlas"],
    refreshes: ["atlas_sources"],
    scope: "person",
  },
  // A PURGE IS AN ADMINISTRATOR'S, after a fresh sign-in check the transport
  // asks for (`protocol/rest.ts`).
  atlas_purge: {
    args: ["atlas_entry", "value"],
    domain: null,
    apps: [],
    refreshes: ATLAS_READS,
    scope: "company",
  },
} as const;

/**
 * The longest `reason` a `pause_seat` carries, in characters (Unicode code
 * points): the engine's `builtin.MaxPauseReasonRunes`, which refuses a longer
 * one. The pause dialog says so before the press. Held by
 * `internal/agent/builtin.TestTheDashboardBoundsAPauseReasonAtTheEnginesCap`.
 */
export const PAUSE_REASON_MAX_RUNES = 500;

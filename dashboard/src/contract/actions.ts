/**
 * Every change the dashboard makes, as the tool the engine runs for it.
 *
 * The dashboard writes through ONE route — `POST /operator/act/{tool}`, as the
 * person the presented token is bound to (ADR-0024) — and the tools behind it
 * are the operator catalogue's, the same implementations a person's own
 * assistant calls. This table is what the dashboard is allowed to send there:
 * `protocol/act.ts` takes only a key of it, and only the arguments its row
 * names.
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
 *    write that lands in no log a question reads;
 *  - `refreshes` — questions the write moves that its `domain` does not
 *    already cover (`contract/domains.ts`), asked again on its answer: one
 *    outside that domain's session set, which takes no floor, or — for a
 *    write that lands in no log — one that reads what it changed, asked at
 *    whatever floor this tab already holds for it;
 *  - `scope` — `person` for a change any bound person makes to the company's
 *    work in their own name, `company` for one that rewrites what every seat's
 *    tracker means (a project's settings, the catalogue), which a screen
 *    offers only where it says so.
 *
 * HELD AGAINST THE REAL CATALOGUE by `internal/api/operator`'s
 * `TestEveryActionTheDashboardTakesIsOneTheActTransportServes`: every tool is
 * served by the act transport and is not a read, every argument is one its
 * schema takes, every required one is present, every scope is the engine's,
 * and every `domain` is the log the tool's write lands on — by the family it
 * is registered under, a person's chat attention landing on none. A renamed
 * argument in Go is a red build here rather than a button that is refused
 * `invalid` the first time somebody presses it.
 */
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
    // AND THE NEW TASK SHEET (`routes/work/NewTask.tsx`), which files the
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
    refreshes: ["work_search"],
    scope: "person",
  },
  answer_knowledge: {
    // A WRITE THAT CHANGES NOTHING, because it SPENDS: one model call per
    // question, and a read would be asked again on every refocus and
    // reconnect. It lands in no log, so it raises no floor.
    args: ["q"],
    domain: null,
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
    refreshes: ["work_search"],
    scope: "person",
  },
  restore_work_item: {
    args: ["item"],
    domain: "tracker",
    refreshes: ["work_search"],
    scope: "person",
  },
  write_project: {
    // "EDIT PROJECT" on a project's page: the lead's target date. A project's
    // settings are what every seat's tracker reads, so this is a COMPANY
    // write, offered only on the project's own page — and the engine decides
    // who may make it (the project's lead, or a person acting as themselves).
    args: ["project", "target_date"],
    domain: "tracker",
    refreshes: [],
    scope: "company",
  },
  set_pins: {
    args: ["views", "favorites"],
    domain: "tracker",
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
    refreshes: [],
    scope: "person",
  },
  answer_run: {
    // A PARKED CODING RUN ANSWERED BY ITS TURN. It lands on the seat's
    // inbox rather than in a log a question reads, so it raises no floor;
    // the run's record moves when the node holding the seat resumes it.
    args: ["turn_id", "answer"],
    domain: null,
    refreshes: ["sandbox_runs", "decisions"],
    scope: "person",
  },
  pause_seat: {
    // A SEAT'S PROFILE PAUSES IT: its mail waits, its schedules are skipped,
    // and the turn it is on finishes first unless `stop_running` asks for it
    // to end at its next round. The pause is a coordination record rather
    // than a log a question reads, so it raises no floor; the seat's state
    // moves on the agents push, which every node sends when its watch lands.
    args: ["handle", "reason", "stop_running"],
    domain: null,
    refreshes: [],
    scope: "person",
  },
  resume_seat: {
    // AND RESUMES IT: the mail that waited is delivered first, in order.
    args: ["handle"],
    domain: null,
    refreshes: [],
    scope: "person",
  },
  steer_turn: {
    // STEER ON THE TURN TRACE: a note to a turn running now, read at its
    // next round. It crosses to the node running the turn on an ephemeral
    // ask rather than into a log, so it raises no floor; what became of it
    // is the turn's own `agent_turn_steered`, which the trace re-reads.
    args: ["turn_id", "note"],
    domain: null,
    refreshes: ["turn"],
    scope: "person",
  },
  write_page: {
    // "SAVE AS A PAGE" in the Inbox's composer: what the person wrote becomes
    // a new page in the knowledge base, which is then attached to the item
    // (`update_work_item{linked_pages}`) — the long form a comment's size
    // limit sends a writer to.
    //
    // AND THE KNOWLEDGE BASE'S COMPOSER: "New page" and a tree's Create,
    // filed in a space at its top or under a page (`parent`), with the
    // composer's `labels`, and Save as draft (`status: "draft"`). A folder is
    // the same write with an empty body: a page that only groups pages.
    args: ["title", "body", "container", "parent", "labels", "status"],
    domain: "pages",
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
    refreshes: ["knowledge"],
    scope: "person",
  },
  comment_on_page: {
    // A PAGE'S THREAD: a remark, or a reply to one (`reply_to`).
    args: ["page", "body", "reply_to"],
    domain: "pages",
    refreshes: [],
    scope: "person",
  },
  place_work_item: {
    // A BOARD DRAG: the card dropped `before` a neighbour, or `after` the last
    // card of a lane, and into another lane with `status`. The engine mints
    // the place between the neighbours as the board stands when it lands.
    args: ["item", "before", "after", "status", "if_match"],
    domain: "tracker",
    refreshes: ["work_search"],
    scope: "person",
  },
  save_work_view: {
    // "+ VIEW": the query on screen, saved under a name on this container,
    // shared or kept to the person who saved it.
    args: ["container", "name", "type", "params", "owner"],
    domain: "tracker",
    refreshes: [],
    scope: "person",
  },
  mark_inbox: {
    args: ["read", "unread", "snooze", "unsnooze", "read_through"],
    domain: "tracker",
    refreshes: [],
    scope: "person",
  },
  // ---- THE COMPANY'S OWN CHAT (`routes/chat`) -------------------------- //
  //
  // Every gesture the chat screen makes, each the tool a person's own
  // assistant calls for it. The handles a message names and whether it
  // addressed the whole room are the ENGINE's reading of the body — the tool
  // takes no `mentions` and no flag — so a message is sent as what was typed.
  post_message: {
    // THE COMPOSER'S SEND. A message is additive and arbitrates nothing at
    // the broker, so the request id is what tells a retry from a second
    // remark: the outbox repeats an unanswered message under the id it was
    // first sent with (`routes/chat/outbox.ts`).
    args: ["channel", "body"],
    domain: "chat",
    refreshes: [],
    scope: "person",
  },
  reply_in_thread: {
    // THE THREAD PANE'S SEND: `reply_to` is the thread's root, and the engine
    // files a reply to a reply under the same one.
    args: ["channel", "reply_to", "body"],
    domain: "chat",
    refreshes: [],
    scope: "person",
  },
  edit_message: {
    // A ROW'S "EDIT", offered on the reader's own messages only: the engine
    // refuses an edit from anybody but the author, operator included.
    args: ["channel", "message", "body"],
    domain: "chat",
    refreshes: [],
    scope: "person",
  },
  delete_message: {
    // A ROW'S "DELETE": a tombstone the thread keeps its root through. A
    // person acts as an operator here (ADR-0024), so it is offered on any
    // message the room shows — moderation is the credential's.
    args: ["channel", "message"],
    domain: "chat",
    refreshes: [],
    scope: "person",
  },
  react_to_message: {
    // AN EMOJI ON A MESSAGE, or taken back (`remove`).
    args: ["channel", "message", "emoji", "remove"],
    domain: "chat",
    refreshes: [],
    scope: "person",
  },
  join_channel: {
    // "JOIN" on a room the reader may read and is not in — the room's own
    // header, and the browse dialog's rows.
    args: ["channel"],
    domain: "chat",
    refreshes: [],
    scope: "person",
  },
  leave_channel: {
    args: ["channel"],
    domain: "chat",
    refreshes: [],
    scope: "person",
  },
  create_channel: {
    // "NEW ROOM": the name it claims, who may read it, its topic, and the
    // people in it from the start. A unit's room is the org chart's and is
    // never made here.
    args: ["name", "kind", "topic", "members"],
    domain: "chat",
    refreshes: [],
    scope: "person",
  },
  open_dm: {
    // "NEW DIRECT MESSAGE": the others in it. The author is added by the
    // engine, and opening one that exists is that room.
    args: ["to"],
    domain: "chat",
    refreshes: [],
    scope: "person",
  },
  update_channel: {
    // A ROOM'S "TOPIC" DIALOG: the topic and the purpose, and only what moved.
    // Archiving and retention are not offered there — each decides what
    // survives on every data node.
    args: ["channel", "topic", "purpose"],
    domain: "chat",
    refreshes: [],
    scope: "person",
  },
  mark_chat_read: {
    // HOW FAR THE READER HAS READ, flushed on a timer and on the way out
    // (`routes/chat/cursor.ts`), and a room's MUTE. A person's attention is a
    // coordination record rather than a record on the chat log, so it lands
    // in no log a question reads and raises no floor.
    //
    // IT MOVES THE RAIL, and asks it nothing here. `chat_channels` answers
    // each room's unread count and mute from this very record, and the
    // write's answer IS the record as it left it, named by its person — the
    // form the `chat_cursor_moved` push takes, which reaches every tab of the
    // person's, this one included. The press applies that answer to the
    // tab's read state (`markRead`), and the rail asks again once the
    // revision moves, whichever of the two arrives first. A `refreshes` here
    // asked a second time whenever the push won the race, and the second ask
    // discarded the first after the engine had counted every room's unread.
    args: ["cursors", "mute", "unmute"],
    domain: null,
    refreshes: [],
    scope: "person",
  },
} as const;

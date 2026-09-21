/**
 * The client's mirror of the server's live projection.
 *
 * The store holds what it is given and tells interested listeners. **It
 * derives nothing.** This file once carried a second implementation of the
 * engine's state machine — an event→state map, sandbox lifecycle tracking and
 * an 85-line reimplementation of the server's token aggregation, all applied to
 * raw events as they streamed past. Three copies of that logic meant three ways
 * to drift, and a refresh routinely disagreed with what had been on screen a
 * moment earlier. The server computes the projection once and pushes the
 * result; everything here is assignment.
 *
 * Subscriptions are per-slice, and that is not an optimisation detail: an
 * `agents` overlay is pushed TWICE PER TOOL-LOOP ROUND, so a store that woke
 * every listener on every envelope would re-render the whole application
 * several times a second for the length of a turn.
 */

import type {
  AgentRow,
  ChatChange,
  ChatPresenceFrame,
  ChatReadState,
  FeedRow,
  EventEnvelope,
  HealthPush,
  OrgBudget,
  OrgProjection,
  Overlay,
  Rollup,
  SandboxEntry,
  ScheduleRow,
  Snapshot,
  ToolRow,
} from "./types.ts";

/**
 * Longest activity feed a tab keeps.
 *
 * Matches the server's own retention (`livestate.EventFeedLimit`) so a
 * reconnect's snapshot neither truncates the feed nor leaves rows the server
 * cannot resend. Exported because it is also the limit of what anything derived
 * from the feed can HONESTLY claim to know: a busy company fills 400 events in
 * minutes, and a panel covering an hour has to say where the record actually
 * starts rather than drawing the gap as quiet.
 */
export const MAX_EVENTS = 400;

/**
 * How many completed-phase envelopes a tab keeps, PAYLOAD AND ALL.
 *
 * This is the one slice retained for its payload rather than for its row, and
 * it exists because a live phase has no durable half until one arrives: the
 * projection clears `live_call` the instant a phase completes, and the query
 * that answers a seat's history was answered ONCE, at mount. Without this the
 * turn a reader is watching vanishes the moment its last phase lands — most
 * visibly on a seat's FIRST turn, where the mount-time history is empty and the
 * page is left claiming the seat has never run.
 *
 * 200, and the bound is on the PAYLOADS rather than on the rows: a phase carries
 * its verbatim system prompt, its response and every tool result, which is why
 * the server caps one page of these same rows at 60 on row size alone
 * (`store.MaxPhasePage`) and a seat's history at 50 (`store.AgentPhaseLimit`).
 * 200 is above every one of those and above the ~40 phases a turn reaches when
 * it self-iterates to the default cap of 3 with a full 8-task delegate fan-out
 * each round — so a tab watching one turn keeps all of it — while staying inside
 * what a browser should hold in payloads of this size.
 *
 * Eviction is drop-oldest and the buffer is COMPANY-WIDE, because one socket
 * serves every screen. So this is not a guarantee: a fleet completing more than
 * 200 phases while a tab sits open can evict a record that tab still wants, and
 * a turn then renders with a phase missing rather than with all of them. What
 * bounds the damage is that these only ever SUPPLEMENT a query answer — every
 * screen re-asks on reconnect, and a reload is authoritative — so the loss is a
 * card that is late, never a turn that is gone.
 */
export const MAX_PHASES = 200;

/**
 * How many live chat frames a tab keeps FOR ONE ROOM.
 *
 * TWO HUNDRED, from the company census rather than from a feeling: the design
 * is sized for 20,000 messages a day across a rail of about twenty rooms, so
 * two hundred is roughly an hour of the busiest room in the company and a
 * multiple of that everywhere else. A frame is small — no body ever travels on
 * one — so the bound is about a tab that is left open for a week rather than
 * about bytes on any one screen.
 *
 * Eviction is drop-oldest, per room, and it costs nothing a reader can see: a
 * frame is EVIDENCE that something landed, and what a screen draws is the
 * transcript it re-read because of it. An evicted frame is a refetch that
 * already happened.
 */
export const MAX_CHAT_LIVE = 200;

/**
 * How many rooms a tab keeps live state for at once.
 *
 * `chat.MaxRailChannels` — two hundred and fifty-six, the cap the engine puts
 * on one person's rail and on their read cursors. Frames arrive for every room
 * the viewer may READ, which is wider than the rail (a public room and a
 * unit's room are readable by any seat), so this is a ceiling on a set that
 * genuinely can exceed the rail rather than a restatement of it. Past it the
 * room whose newest frame is oldest is dropped, which is the room nobody is
 * looking at.
 */
export const MAX_CHAT_ROOMS = 256;

/**
 * What this tab has seen happen in one room since it connected.
 *
 * NOT A TRANSCRIPT, and it must never become one: a frame carries no body, so
 * everything rendered from this would be a second, poorer copy of the
 * conversation, disagreeing with the transcript the first time an edit raced a
 * reload. What it is for is deciding what to re-read.
 */
export interface ChatRoomLive {
  /**
   * The highest per-room message number any frame has announced.
   *
   * THE LIVE HIGH-WATER MARK, and the whole reason a screen can be cheap: a
   * transcript that has rendered through 41 and sees 43 here knows it is
   * behind without comparing any rows. It never moves backwards — a frame
   * that arrives out of order is late, not new — and the FIRST frame for a
   * room sets it rather than being measured against zero, because a tab that
   * has just connected is not behind on a room it has never seen.
   */
  seq: number;

  /** The broker's instant on the newest frame seen here, which is what orders
   *  the eviction above. */
  at: string;

  /** The frames themselves, newest first, bounded by [MAX_CHAT_LIVE]. */
  changes: ChatChange[];
}

/** Every room this tab has seen a frame for. */
export interface ChatLive {
  rooms: Record<string, ChatRoomLive>;
}

export interface StoreState {
  agents: AgentRow[];
  events: FeedRow[];
  /**
   * The `agent_phase_completed` envelopes seen on this socket, newest first.
   *
   * NOT part of what a snapshot replaces: the snapshot carries payload-free
   * feed rows, so a reconnect adds to this rather than re-establishing it, and
   * the durable history each screen loads comes from its own query.
   */
  phases: EventEnvelope[];
  sandboxes: SandboxEntry[];
  org: OrgProjection;
  tools: ToolRow[];
  health: HealthPush;
  tokens: Rollup | null;
  budget: OrgBudget;
  schedules: ScheduleRow[] | null;
  /**
   * What the company's conversation has done since this tab connected.
   *
   * ITS OWN SLICE, and not part of what a snapshot replaces: the degraded
   * REST snapshot carries no chat at all, so folding this into the snapshot
   * would blank an open transcript every five seconds while the socket is
   * down.
   */
  chat: ChatLive;
  /**
   * Who is looking at, typing in, or working on a message in the rooms this
   * socket said it is watching — the server's current answer, whole.
   *
   * Its own slice because it MOVES ON A DIFFERENT CLOCK: presence is
   * re-probed every few seconds whatever the conversation does, so a rail
   * that woke on it would re-render the whole channel list on a tick.
   */
  chatPresence: ChatPresenceFrame;
  /**
   * Where this person has read to, what they have muted, and whether they are
   * in do-not-disturb — or null when nothing has said.
   *
   * NULL IS NOT AN EMPTY CURSOR SET. The coordination record may be
   * unreadable, and a zero cursor for every room is what "you have read
   * nothing anywhere" looks like: a screen badging a whole company unread
   * because a bucket read failed is the failure this distinction exists to
   * prevent.
   */
  chatRead: ChatReadState | null;
  connected: boolean;
  /**
   * Whether the engine REFUSED this browser, as opposed to being unreachable.
   * Distinct from `connected` because the repair differs and the reader cannot
   * tell them apart: a stopped engine comes back on its own, a rejected token
   * never does.
   */
  authRejected: boolean;
}

export type Slice = keyof StoreState;

// What a snapshot replaces. `phases` is deliberately absent: a snapshot carries
// payload-free feed rows and no phase payloads, so emitting it here would wake
// every phase reader for an answer that did not move. THE THREE CHAT SLICES
// are absent for a stronger reason: a snapshot carries no chat, so listing one
// here would hand an open transcript an empty answer every time the degraded
// poll ticks.
const ALL_DATA_SLICES: Slice[] = [
  "agents",
  "events",
  "sandboxes",
  "org",
  "tools",
  "tokens",
  "budget",
  "schedules",
  "health",
];

function emptyState(): StoreState {
  return {
    agents: [],
    events: [],
    phases: [],
    sandboxes: [],
    org: {},
    tools: [],
    health: { status: "unknown" },
    tokens: null,
    budget: {},
    schedules: null,
    chat: { rooms: {} },
    chatPresence: { rooms: {} },
    chatRead: null,
    connected: false,
    authRejected: false,
  };
}

export class Store {
  state: StoreState = emptyState();

  private subs = new Map<Slice, Set<() => void>>();

  /**
   * A monotonic counter per slice.
   *
   * React binds through `useSyncExternalStore`, which compares snapshots by
   * identity and re-renders when they differ. Slices are mutated in place (the
   * arrays are large and pushed at high frequency), so the version is what
   * gives each slice a cheap, stable identity to compare — and it is per-slice
   * rather than global for the same reason the subscriptions are.
   */
  private versions: Record<string, number> = {};

  version(slice: Slice): number {
    return this.versions[slice] ?? 0;
  }

  /** Call `fn` when any of `slices` changes. Returns an unsubscribe function. */
  subscribe(slices: readonly Slice[], fn: () => void): () => void {
    for (const slice of slices) {
      let set = this.subs.get(slice);
      if (!set) {
        set = new Set();
        this.subs.set(slice, set);
      }
      set.add(fn);
    }
    return () => {
      for (const slice of slices) this.subs.get(slice)?.delete(fn);
    };
  }

  private emit(...slices: Slice[]): void {
    for (const slice of slices) this.versions[slice] = (this.versions[slice] ?? 0) + 1;
    const called = new Set<() => void>();
    for (const slice of slices) {
      for (const fn of this.subs.get(slice) ?? []) {
        if (called.has(fn)) continue;
        called.add(fn);
        fn();
      }
    }
  }

  // ---- pushes ------------------------------------------------------------

  applySnapshot(snap: Snapshot | null | undefined): void {
    if (!snap) return;
    this.state.agents = snap.agents ?? [];
    this.state.events = (snap.events ?? []).slice(0, MAX_EVENTS);
    this.state.sandboxes = snap.sandboxes ?? [];
    this.state.org = snap.org ?? {};
    this.state.tools = snap.tools ?? [];
    if (snap.tokens && snap.tokens.totals) this.state.tokens = snap.tokens;
    this.state.budget = snap.budget ?? {};
    // A bare list here, unlike the push's `{schedules: […]}` object.
    if (snap.schedules) this.state.schedules = snap.schedules;
    // NOT `connected`. That belongs to the transport, which knows whether the
    // socket is open; deriving it from a payload's contents meant a snapshot
    // arriving over the degraded REST fallback announced a live connection
    // that did not exist.
    if (snap.health) this.state.health = snap.health;
    this.emit(...ALL_DATA_SLICES);
  }

  /** Changed seat overlays, keyed by role. */
  applyAgents(rows: (Overlay & { role: string })[] | unknown): void {
    // `Array.isArray` is load-bearing. The server sent this as an object keyed
    // by role once; every push was silently discarded and seats rendered idle
    // for the whole of a turn, with both sides' own suites green. That is the
    // bug internal/e2e/golden_test.go exists to catch.
    if (!Array.isArray(rows) || rows.length === 0) return;
    const byRole = new Map<string, Overlay & { role: string }>(
      (rows as (Overlay & { role: string })[]).map((r) => [r.role, r]),
    );
    this.state.agents = this.state.agents.map((a) => {
      const patch = byRole.get(a.role);
      if (!patch) return a;
      byRole.delete(a.role);
      return { ...a, ...patch };
    });
    // A seat the roster does not carry yet (a role added by a live revision)
    // still belongs on screen.
    for (const row of byRole.values()) {
      this.state.agents = [...this.state.agents, { id: row.role, ...row }];
    }
    this.emit("agents");
  }

  /**
   * The complete seat list, replacing what is on screen.
   *
   * Distinct from `applyAgents`, which merges changed overlays by role: a merge
   * cannot express a deletion, so a revision that removes a role would leave
   * its card rendered until the next reload.
   */
  applySeats(rows: AgentRow[] | unknown): void {
    if (!Array.isArray(rows)) return;
    // Keep the live overlay each seat already carries — the config payload is
    // static config and knows nothing about what a seat is doing right now.
    const live = new Map(this.state.agents.map((a) => [a.role, a]));
    this.state.agents = (rows as AgentRow[]).map((row) => {
      const current = live.get(row.role);
      return current ? { ...current, ...row } : row;
    });
    this.emit("agents");
  }

  applySandboxes(list: SandboxEntry[] | null | undefined): void {
    this.state.sandboxes = list ?? [];
    // `agents` too: a seat's effective state folds in whether it is parked on
    // a sandbox question, so a sandbox move is a seat move.
    this.emit("sandboxes", "agents");
  }

  applyTokens(rollup: Rollup | null | undefined): void {
    if (!rollup) return;
    this.state.tokens = rollup;
    this.emit("tokens");
  }

  applyBudget(budget: OrgBudget | null | undefined): void {
    this.state.budget = budget ?? {};
    this.emit("budget");
  }

  applySchedules(payload: { schedules?: ScheduleRow[] } | null): void {
    if (!payload) return;
    // Applied only when present: the push carries the CONFIGURED rows and
    // nothing else, so an absent key means "unchanged" rather than "empty".
    if (payload.schedules) this.state.schedules = payload.schedules;
    this.emit("schedules");
  }

  applyOrg(org: OrgProjection | null | undefined): void {
    this.state.org = org ?? {};
    this.emit("org");
  }

  applyTools(tools: ToolRow[] | null | undefined): void {
    this.state.tools = tools ?? [];
    this.emit("tools");
  }

  applyHealth(health: HealthPush | null | undefined): void {
    this.state.health = health ?? { status: "unknown" };
    this.state.connected = !!health && health.status !== "unknown";
    this.emit("health");
  }

  setConnected(value: boolean): void {
    this.state.connected = value;
    // A dropped socket CLEARS the health slice rather than freezing it. A stale
    // "healthy" is a lie with a timestamp nobody can see.
    if (!value) this.state.health = { status: "unknown" };
    this.emit("health");
  }

  setAuthRejected(value: boolean): void {
    const next = !!value;
    if (this.state.authRejected === next) return;
    this.state.authRejected = next;
    this.emit("health");
  }

  applyEvent(ev: EventEnvelope | null | undefined): void {
    if (!ev || !ev.id) return;
    // Only events the server PERSISTS belong in the feed. Streaming a
    // non-persisted type into it produced rows that vanished on the next
    // snapshot and 404'd when clicked.
    if (ev.category && !this.state.events.some((e) => e.id === ev.id)) {
      this.state.events = [ev as FeedRow, ...this.state.events].slice(0, MAX_EVENTS);
      this.emit("events");
    }
    // A completed phase is kept WITH ITS PAYLOAD, in its own slice.
    //
    // It is the durable half of a phase the reader is watching live, and the
    // wire has always carried it — `Ingest` broadcasts the whole envelope, the
    // payload included, before it pushes the overlay that clears `live_call`.
    // Nothing read it, so the finished record never arrived and the turn simply
    // went away. Assignment, not derivation: the phase record itself is built
    // by `fromPhaseEvent`, where the screens that render one already build it
    // from the store's own query answers.
    //
    // The payload guard is not defensive: a snapshot's rows and the `events`
    // query both answer with payload-free rows, and one of those without a
    // payload would evict a real record for a phase nothing can render.
    if (ev.type === "agent_phase_completed" && ev.payload) {
      if (!this.state.phases.some((p) => p.id === ev.id)) {
        this.state.phases = [ev, ...this.state.phases].slice(0, MAX_PHASES);
        this.emit("phases");
      }
    }
  }

  // ---- chat --------------------------------------------------------------

  /**
   * One committed chat record, as the socket announced it.
   *
   * ONE METHOD FOR SIX PUSH KINDS, because the frame already says which it is:
   * `op` is the record's own word (`post`, `edit`, `react`, …) and the kind is
   * a routing label the server derived FROM it. Taking the kind as well would
   * be a second vocabulary for one fact, and the two would disagree on exactly
   * the op somebody adds next.
   *
   * IT DERIVES NOTHING ABOUT WHAT A ROOM CONTAINS. A frame carries no body, so
   * a transcript rendered from these would be a second copy of the
   * conversation — the failure this store's own doc opens with. What is kept
   * is the high-water mark a screen compares against what it has rendered, and
   * the frames themselves, so a screen can tell a new message from a reaction
   * without asking the engine which it was.
   */
  applyChatChange(change: ChatChange | null | undefined): void {
    // A frame with no room cannot be filed and one with no op id cannot be
    // recognised coming back, so neither is kept. Both are impossible from
    // this engine; a frame from a NEWER one is not, and dropping it silently
    // is better than filing it under "".
    if (!change || !change.channel_id || !change.op_id) return;
    const rooms = this.state.chat.rooms;
    const room = rooms[change.channel_id] ?? { seq: 0, at: "", changes: [] };
    // A REDELIVERY IS NOT A SECOND EVENT. The op id is the record's own
    // idempotency key, so a frame that arrives twice — a reconnect that
    // overlaps, a hub that re-sent — is the same record, and appending it
    // would count one message as two everywhere a screen counts these.
    if (room.changes.some((seen) => seen.op_id === change.op_id)) return;
    const seq = typeof change.channel_seq === "number" ? change.channel_seq : 0;
    rooms[change.channel_id] = {
      // NEVER BACKWARDS. A frame that arrives out of order is late, not new,
      // and a mark that moved down would make a screen re-read a room it is
      // already ahead of, for ever.
      seq: seq > room.seq ? seq : room.seq,
      at: change.at > room.at ? change.at : room.at,
      changes: [change, ...room.changes].slice(0, MAX_CHAT_LIVE),
    };
    this.evictChatRooms();
    this.emit("chat");
  }

  /**
   * Keep the live state to [MAX_CHAT_ROOMS] rooms, dropping the one whose
   * newest frame is oldest.
   *
   * By the frame's own instant rather than by insertion order: a room that has
   * been quiet for a week is the one nobody is looking at, whatever order this
   * tab happened to hear about it in.
   */
  private evictChatRooms(): void {
    const rooms = this.state.chat.rooms;
    const ids = Object.keys(rooms);
    if (ids.length <= MAX_CHAT_ROOMS) return;
    const oldest = ids.reduce((worst, id) =>
      (rooms[id]?.at ?? "") < (rooms[worst]?.at ?? "") ? id : worst,
    );
    delete rooms[oldest];
  }

  /** Who is in the rooms this socket is watching, whole. */
  applyChatPresence(frame: ChatPresenceFrame | null | undefined): void {
    // REPLACED RATHER THAN MERGED, because the frame is the whole answer for
    // the rooms this socket asked about: a room that has gone quiet appears by
    // being ABSENT, and merging would leave the last person who was typing
    // there for the life of the tab.
    this.state.chatPresence = frame?.rooms ? frame : { rooms: {} };
    this.emit("chatPresence");
  }

  /**
   * This person's own read state, from their flush or from another tab's.
   *
   * IT IS NEVER MERGED WITH WHAT IS ON SCREEN. The record is authoritative and
   * whole — the engine advances cursors and ignores a value below what it
   * holds — so the answer to a flush and the push another tab caused are the
   * same fact, and the last one to arrive is the current one.
   */
  applyChatRead(state: ChatReadState | null | undefined): void {
    if (!state) return;
    this.state.chatRead = state;
    this.emit("chatRead");
  }

  // ---- reads -------------------------------------------------------------

  agentById(id: string): AgentRow | null {
    return this.state.agents.find((a) => a.id === id || a.role === id) ?? null;
  }

  /**
   * Resolve a seat by whatever the URL carried.
   *
   * Seats are addressed by HANDLE — the canonical identity everywhere else in
   * the system, and the one an operator can read off a chat mention. Runtime
   * ids and role names still resolve, because links minted before the move
   * used them and they are in people's history.
   */
  agentByKey(key: string | null | undefined): AgentRow | null {
    if (!key) return null;
    const wanted = String(key);
    const lower = wanted.toLowerCase();
    return (
      this.state.agents.find(
        (a) =>
          a.handle === wanted ||
          a.id === wanted ||
          a.role === wanted ||
          String(a.handle ?? "").toLowerCase() === lower ||
          String(a.role ?? "").toLowerCase() === lower,
      ) ?? null
    );
  }
}

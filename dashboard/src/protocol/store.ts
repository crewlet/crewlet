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
  CallVersions,
  ChatChange,
  ChatPresenceFrame,
  ChatReadState,
  FeedRow,
  EventEnvelope,
  LiveCall,
  LiveCallAnswer,
  OrgBudget,
  OrgProjection,
  Overlay,
  Rollup,
  SandboxEntry,
  ScheduleRow,
  Snapshot,
  ToolRow,
} from "./types.ts";
// RELATIVE, like every contract import in this directory: it is also built
// alone as `protocol.js`, where the `~` alias does not exist.
import { LIVE_CALL_DETAIL, MAX_EVENTS } from "../contract/wire.ts";
import type { EngineHealth } from "../contract/health.ts";
import { comparePositions, type LogPosition } from "./session.ts";

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
 * already happened — PROVIDED the signal a screen re-reads on is one eviction
 * cannot hold still. That is why a screen watches [ChatRoomLive.frame] and
 * [ChatLive.frames] and never the length of this list: past the bound a new
 * frame leaves the length where it was, and a room watched by its length
 * stopped re-reading at its two-hundredth frame.
 */
export const MAX_CHAT_LIVE = 200;

/**
 * How many rooms a tab keeps live state for at once.
 *
 * TWO HUNDRED AND FIFTY-SIX, sized from the most rooms one person's rail
 * lists (`chat.MaxRailChannels`), so the rooms a reader is in fit when they
 * are the ones talking — and a CLIENT bound, deliberately held to no engine
 * constant. Frames arrive for every room the viewer may READ, which is wider
 * than the rail (a public room and a unit's room are readable by any seat), so
 * no value here could promise the rail's rooms a place, and nothing is owed
 * one: what this bounds is memory — this many rooms of [MAX_CHAT_LIVE] frames
 * at most, none of them carrying a body — for a tab left open for a week on a
 * company with more readable rooms than that.
 *
 * Past it the room whose newest frame is oldest is dropped, which is the room
 * nobody is looking at, and the loss costs what an evicted frame costs: a
 * frame is evidence a screen already re-read on, and the room's next frame
 * starts its live state again — at a [ChatRoomLive.frame] above any it had
 * before, since that number is the tab's count rather than the room's.
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

  /**
   * Where on the chat log the newest frame seen here sits, or null when no
   * frame named one — what orders the eviction above, and the rooms a tab
   * offers as the company's busiest. THE LOG'S ORDER, never the frame's `at`:
   * the broker's instant is RFC 3339 with its trailing zeros trimmed, so
   * compared as text it put `…:05Z` after every fraction of its own second
   * and could move this room's newest moment backwards (`comparePositions`).
   */
  position: LogPosition | null;

  /**
   * Which accepted frame, counted across the whole tab ([ChatLive.frames]),
   * was this room's newest.
   *
   * WHAT A SCREEN SHOWING THIS ROOM RE-READS ON, because it is the one number
   * here that moves on every frame and on nothing else. Not the length of
   * [changes], which is bounded and so stands still on every frame past the
   * bound; not [seq], which a reaction or an edit does not move. And the
   * TAB'S count rather than the room's own, so a room evicted at
   * [MAX_CHAT_ROOMS] and heard from again comes back above every number it
   * had — a per-room counter would start again at one, which is a value the
   * screen may already be holding.
   */
  frame: number;

  /** The frames themselves, newest first, bounded by [MAX_CHAT_LIVE]. */
  changes: HeldChatChange[];
}

/**
 * One frame as this tab holds it: the change the engine sent, and which
 * accepted frame it was.
 *
 * THE ORDINAL IS WHAT LETS A SCREEN ASK WHAT ARRIVED SINCE IT LAST LOOKED,
 * across rooms as well as within one. A screen that re-read everything it
 * holds on every frame paid for a frame that touched none of it; one that
 * reads only what a frame names has to know which frames it has not yet
 * answered, and a frame's own position cannot say that — two rooms' frames
 * arrive in whatever order the hub sent them, not in log order.
 */
export interface HeldChatChange extends ChatChange {
  /** Which accepted frame this was, counted as [ChatLive.frames] counts. */
  frame: number;
}

/** Every room this tab has seen a frame for. */
export interface ChatLive {
  rooms: Record<string, ChatRoomLive>;
  /**
   * How many frames this tab has accepted, in every room, since it loaded.
   *
   * WHAT A SCREEN THAT SPANS THE ROOMS RE-READS ON — the rail, the mention
   * feed. It only rises: a redelivery is not counted, and neither bound
   * takes anything back from it, so a frame in a room that has just been
   * evicted moves it exactly as a frame anywhere else does. A sum of what
   * the rooms hold could not say that — past either bound a new frame and
   * the one it pushed out cancel. And it survives [Store.forgetPerson]: it
   * carries nothing about anybody, only that something landed, and a count
   * that went back to zero could come back to a number a screen holds.
   */
  frames: number;

  /**
   * The latest frame this tab has LET GO of — pushed off a room's list by
   * [MAX_CHAT_LIVE], dropped with a room by [MAX_CHAT_ROOMS], or forgotten
   * with a person ([Store.forgetPerson]) — as its [HeldChatChange.frame], or
   * 0 while nothing has been.
   *
   * WHAT TELLS "NOTHING ARRIVED" FROM "WHAT ARRIVED IS GONE". A screen that
   * asks for the frames since one it answered and gets none has its answer
   * only while this is below that frame: above it, a frame it never saw was
   * let go, and the honest answer is that it cannot say what changed.
   */
  dropped: number;
}

/** What a heavy field reads as while nothing of it is held. */
const NOTHING_HELD: Record<string, unknown> = {
  prompt: "",
  prompt_messages: null,
  response: "",
  round_narration: null,
  tool_executions: null,
  rounds: null,
};

/**
 * One seat's live call as a push states it, merged onto the copy a tab holds —
 * and whether the push says the tab missed a change it no longer carries.
 *
 * THE PUSH LEAVES OUT WHAT DID NOT MOVE. A call's heavy fields
 * (`LIVE_CALL_DETAIL`) travel only on the push that moved their version, and
 * every push names every version. So a field the push carries is taken — unless
 * its version is older than the one held, a push overtaken by a snapshot or an
 * answer — and one it leaves out is the copy held, at the version held, for the
 * SAME call (`turn_id`, `phase`, `iteration`); a call of its own holds nothing
 * yet. A push naming a version newer than the one held, without the field, is
 * a tab that missed the push which carried it: `stale`, and the field stays
 * what is held until the call is fetched whole.
 */
export function mergeLiveCall(
  held: LiveCall | null | undefined,
  next: LiveCall | null | undefined,
): { call: LiveCall | null; stale: boolean } {
  if (!next) return { call: null, stale: false };
  const same = !!held && sameCall(held, next);
  const merged: Record<string, unknown> = { ...next };
  const versions: Partial<CallVersions> = {};
  let stale = false;
  for (const [detail, fields] of Object.entries(LIVE_CALL_DETAIL) as [
    keyof CallVersions,
    readonly string[],
  ][]) {
    const pushed = next.versions[detail];
    const have = same ? held.versions[detail] : 0;
    const carried = fields.every((f) => f in next);
    if (carried && pushed >= have) {
      versions[detail] = pushed;
      continue;
    }
    for (const f of fields) {
      merged[f] = same ? (held as unknown as Record<string, unknown>)[f] : NOTHING_HELD[f];
    }
    versions[detail] = have;
    if (!carried && pushed > have) stale = true;
  }
  merged.versions = versions;
  return { call: merged as unknown as LiveCall, stale };
}

/** Whether two copies are of one call: one turn, one phase, one iteration. */
function sameCall(a: LiveCall, b: LiveCall): boolean {
  return a.turn_id === b.turn_id && a.phase === b.phase && a.iteration === b.iteration;
}

/**
 * The heavy fields of `older` — a copy of the call `held` is, read BEFORE the
 * pushes that brought `held` — whose versions are newer than `held`'s, laid onto
 * `held`; or null when it brings none, or is not a copy of the same call.
 *
 * A version is never handed out twice and only grows, so a newer one is newer
 * content whenever it was read: the pushes after `older` left those fields out
 * because the tab was meant to hold them already, and their copy is what the
 * tab missed. Everything else — the light fields, and every heavy field the tab
 * holds at a version as new — stays `held`'s, which the later pushes wrote.
 */
function newerDetail(held: LiveCall, older: LiveCall): LiveCall | null {
  if (!sameCall(held, older)) return null;
  let merged: Record<string, unknown> | null = null;
  const versions: Partial<CallVersions> = { ...held.versions };
  for (const [detail, fields] of Object.entries(LIVE_CALL_DETAIL) as [
    keyof CallVersions,
    readonly string[],
  ][]) {
    const version = older.versions[detail];
    if (version <= held.versions[detail] || !fields.every((f) => f in older)) continue;
    merged ??= { ...held };
    for (const f of fields) merged[f] = (older as unknown as Record<string, unknown>)[f];
    versions[detail] = version;
  }
  if (!merged) return null;
  merged.versions = versions;
  return merged as unknown as LiveCall;
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
  /**
   * The org chart the engine pushed, or NULL until the first one arrives.
   *
   * Null rather than `{}`, because `{}` is an ANSWER — the projection of a
   * node running no company — and a screen has to tell that from "nothing has
   * been said yet": the charter drew "No mission is set" and a policy count
   * of 0 on every cold tab, before the handshake had said anything at all.
   */
  org: OrgProjection | null;
  tools: ToolRow[];
  /**
   * The engine's own health: the `health` push, `api.Health` WHOLE, replaced
   * by the snapshot and by every five-second tick. `null` while it is not
   * known — the socket is down, or no frame has arrived yet.
   */
  health: EngineHealth | null;
  tokens: Rollup | null;
  /**
   * The company's live token meter as the last `budget` push stated it, and
   * `null` UNTIL ONE HAS. Three facts, not two: nobody has read the counter
   * yet, nothing is capped (`org.windows` empty), or a reading. An empty
   * object used to stand for both of the first two, so for the first seconds
   * after every engine start a capped company was drawn as having no budget
   * and its operator was offered "Set one".
   */
  budget: OrgBudget | null;
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
   * When this tab LOST the engine: the moment the socket closed, or a dial
   * failed, while nothing was connected. Null while connected, and before the
   * first dial has been answered either way, which is the one thing that
   * tells a tab still connecting from one that cannot reach the engine: both
   * are disconnected, and only the second is worth an amber dot. A retry that
   * fails again does not move it, so it says how long the engine has been
   * gone rather than when it was last asked.
   */
  lostAt: number | null;
  /**
   * Whether the engine has reported to this tab at all. The screens keep the
   * last state they were sent, and "showing the last state received" is true
   * only of a tab that received one.
   */
  everConnected: boolean;
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
    org: null,
    tools: [],
    health: null,
    tokens: null,
    budget: null,
    schedules: null,
    chat: { rooms: {}, frames: 0, dropped: 0 },
    chatPresence: { rooms: {} },
    chatRead: null,
    connected: false,
    lostAt: null,
    everConnected: false,
    authRejected: false,
  };
}

export class Store {
  state: StoreState = emptyState();

  /**
   * The push kinds this build does not know, each with how many arrived.
   *
   * IGNORED AND COUNTED. A fleet part way through an upgrade has a node
   * pushing a kind this bundle was built before, and throwing on it — or
   * applying it to a slice by a guess — would break a screen over a frame it
   * has no use for. But the same fall-through is exactly what this build's
   * own engine sending a kind its own client forgot looks like, which is the
   * silent failure the e2e replay exists to catch: so it is kept here, and
   * the replay asserts it is empty. Not a slice, because nothing renders it
   * and a listener woken by a frame nobody can read would be woken for
   * nothing.
   */
  readonly unknownPushes = new Map<string, number>();

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
    this.state.budget = snap.budget ?? null;
    // A bare list here, unlike the push's `{schedules: […]}` object.
    if (snap.schedules) this.state.schedules = snap.schedules;
    // NOT `connected`. That belongs to the transport, which knows whether the
    // socket is open; deriving it from a payload's contents meant a snapshot
    // arriving over the degraded REST fallback announced a live connection
    // that did not exist.
    if (snap.health) this.state.health = snap.health;
    this.emit(...ALL_DATA_SLICES);
  }

  /**
   * Changed seat overlays, keyed by role — and the roles whose live call this
   * tab now holds behind what the push describes, which the socket fetches
   * whole (`mergeLiveCall`).
   */
  applyAgents(rows: (Overlay & { role: string })[] | unknown): string[] {
    // `Array.isArray` is load-bearing. The server sent this as an object keyed
    // by role once; every push was silently discarded and seats rendered idle
    // for the whole of a turn, with both sides' own suites green. That is the
    // bug internal/e2e/golden_test.go exists to catch.
    if (!Array.isArray(rows) || rows.length === 0) return [];
    const stale: string[] = [];
    const merge = (
      held: AgentRow | undefined,
      patch: Overlay & { role: string },
    ): Overlay & { role: string } => {
      if (!("live_call" in patch)) return patch;
      const heldSeq = held?.live_call_seq ?? 0;
      const patchSeq = patch.live_call_seq;
      if (patchSeq !== undefined && patchSeq < heldSeq) {
        // This push's live_call is BEHIND one a `live_call` answer already
        // applied for the seat (the answer jumped the sequence ahead of a
        // push generated earlier). Keep the held call and its sequence; the
        // push's other fields — status, turn — are ordered on the wire and
        // still apply.
        return { ...patch, live_call: held?.live_call ?? null, live_call_seq: heldSeq };
      }
      const { call, stale: behind } = mergeLiveCall(held?.live_call, patch.live_call);
      if (behind) stale.push(patch.role);
      return { ...patch, live_call: call, live_call_seq: patchSeq ?? heldSeq };
    };
    const byRole = new Map<string, Overlay & { role: string }>(
      (rows as (Overlay & { role: string })[]).map((r) => [r.role, r]),
    );
    this.state.agents = this.state.agents.map((a) => {
      const patch = byRole.get(a.role);
      if (!patch) return a;
      byRole.delete(a.role);
      return { ...a, ...merge(a, patch) };
    });
    // A seat the roster does not carry yet (a role added by a live revision)
    // still belongs on screen.
    for (const row of byRole.values()) {
      this.state.agents = [...this.state.agents, { id: row.role, ...merge(undefined, row) }];
    }
    this.emit("agents");
    return stale;
  }

  /**
   * One seat's live call, fetched WHOLE because a push said this tab had
   * missed a change to it — and whether the answer was CURRENT: at or past the
   * sequence the seat has applied, so that nothing a push has said since is
   * missing from it.
   *
   * ORDERED BY `live_call_seq`, because this answer runs on its own goroutine
   * and the socket can deliver it AFTER pushes the engine generated later. A
   * current answer is merged like a push that carries everything. One BEHIND
   * the applied sequence describes the slot as it was before those pushes, and
   * the sequence decides only what it can order:
   *
   * - when they cleared the call or began another, the answer is DROPPED, so a
   *   cleared seat or a newer call is never overwritten by a running call read
   *   a moment before it — a per-field version cannot say this, since a cleared
   *   null carries none and two calls' fields are never compared;
   * - when they are the same call, the answer still brings every heavy field
   *   whose version is newer than the copy held (`newerDetail`), because those
   *   are exactly what the tab asked for — the pushes since left them out —
   *   while the light fields stay the newer pushes'. Dropping it whole lost
   *   that repair, and nothing asked again until the seat's next push.
   *
   * A behind answer is the caller's cue to ask again if a push said the call
   * was behind while this answer was out (`LiveSocket.fetchCalls`).
   */
  applyLiveCall(answer: LiveCallAnswer | null | undefined): boolean {
    if (!answer || typeof answer.role !== "string") return true;
    let moved = false;
    let current = true;
    this.state.agents = this.state.agents.map((a) => {
      if (a.role !== answer.role) return a;
      const heldSeq = a.live_call_seq ?? 0;
      const seq = answer.live_call_seq;
      if (seq >= heldSeq) {
        moved = true;
        return {
          ...a,
          live_call: mergeLiveCall(a.live_call, answer.live_call).call,
          live_call_seq: seq,
        };
      }
      current = false;
      const fresher = a.live_call && answer.live_call && newerDetail(a.live_call, answer.live_call);
      if (!fresher) return a;
      moved = true;
      return { ...a, live_call: fresher };
    });
    if (moved) this.emit("agents");
    return current;
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
    this.state.budget = budget ?? null;
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

  applyHealth(health: EngineHealth | null | undefined): void {
    this.state.health = health ?? null;
    this.link(!!health);
    this.emit("health");
  }

  setConnected(value: boolean): void {
    this.link(value);
    // A dropped socket CLEARS the health slice rather than freezing it. A stale
    // "healthy" is a lie with a timestamp nobody can see.
    if (!value) this.state.health = null;
    this.emit("health");
  }

  /**
   * The transport's state, and the two facts about its history the frame
   * reads (`lostAt`, `everConnected`), written in ONE place so a health frame
   * and the socket's own open and close cannot disagree about them.
   */
  private link(connected: boolean): void {
    this.state.connected = connected;
    if (connected) {
      this.state.everConnected = true;
      this.state.lostAt = null;
    } else if (this.state.lostAt === null) {
      this.state.lostAt = Date.now();
    }
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

  /** Count one frame whose `kind` this build does not dispatch. */
  noteUnknownPush(kind: unknown): void {
    const name = typeof kind === "string" ? kind : JSON.stringify(kind ?? null);
    this.unknownPushes.set(name, (this.unknownPushes.get(name) ?? 0) + 1);
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
   * is the high-water mark a screen compares against what it has rendered, the
   * count a screen re-reads on ([ChatRoomLive.frame], [ChatLive.frames]), and
   * the frames themselves, so a screen can tell a new message from a reaction
   * without asking the engine which it was.
   */
  applyChatChange(change: ChatChange | null | undefined): void {
    // A frame with no room cannot be filed and one with no op id cannot be
    // recognised coming back, so neither is kept. Both are impossible from
    // this engine; a frame from a NEWER one is not, and dropping it silently
    // is better than filing it under "".
    if (!change || !change.channel_id || !change.op_id) return;
    const live = this.state.chat;
    const rooms = live.rooms;
    const room = rooms[change.channel_id] ?? { seq: 0, position: null, frame: 0, changes: [] };
    // A REDELIVERY IS NOT A SECOND EVENT. The op id is the record's own
    // idempotency key, so a frame that arrives twice — a reconnect that
    // overlaps, a hub that re-sent — is the same record, and appending it
    // would count one message as two everywhere a screen counts these.
    if (room.changes.some((seen) => seen.op_id === change.op_id)) return;
    const seq = typeof change.channel_seq === "number" ? change.channel_seq : 0;
    live.frames += 1;
    const changes = [{ ...change, frame: live.frames }, ...room.changes];
    for (const gone of changes.slice(MAX_CHAT_LIVE)) {
      live.dropped = Math.max(live.dropped, gone.frame);
    }
    rooms[change.channel_id] = {
      // NEVER BACKWARDS. A frame that arrives out of order is late, not new,
      // and a mark that moved down would make a screen re-read a room it is
      // already ahead of, for ever.
      seq: seq > room.seq ? seq : room.seq,
      // THE LATER OF THE TWO, so a late frame cannot move the room's newest
      // moment backwards; a frame that named no position moves nothing.
      position:
        comparePositions(change.position ?? null, room.position) > 0
          ? (change.position ?? null)
          : room.position,
      frame: live.frames,
      changes: changes.slice(0, MAX_CHAT_LIVE),
    };
    this.evictChatRooms();
    this.emit("chat");
  }

  /**
   * Keep the live state to [MAX_CHAT_ROOMS] rooms, dropping the one whose
   * newest frame is oldest.
   *
   * By where the room's newest frame sits on the log rather than by insertion
   * order: a room that has been quiet for a week is the one nobody is looking
   * at, whatever order this tab happened to hear about it in. A room no frame
   * gave a position goes first, being the one nothing can say is recent.
   */
  private evictChatRooms(): void {
    const live = this.state.chat;
    const rooms = live.rooms;
    const ids = Object.keys(rooms);
    if (ids.length <= MAX_CHAT_ROOMS) return;
    const oldest = ids.reduce((worst, id) =>
      comparePositions(rooms[id]?.position ?? null, rooms[worst]?.position ?? null) < 0
        ? id
        : worst,
    );
    // ITS NEWEST FRAME IS THE LATEST OF ITS OWN that goes with it.
    live.dropped = Math.max(live.dropped, rooms[oldest]?.frame ?? 0);
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
   * same fact about different moments of one record.
   *
   * AND THE NEWER MOMENT WINS, BY THE RECORD'S REVISION, never by arrival:
   * two flushes are taken in one order and their pushes can arrive in the
   * other, so a frame below the revision held is late and is dropped — kept,
   * it would put back a cursor or a mute the record has already moved past.
   * `updated_at` cannot order them, being each flush's own instant from
   * whichever node took it.
   *
   * BUT ONLY WITHIN ONE PERSON. The revision orders two states of one
   * person's record — on the coordination KV it is the bucket's own
   * sequence, shared by everybody's — so a state NAMING SOMEBODY ELSE is not
   * older or newer than the one held: it is the person this tab now is,
   * after a credential change or a seat its token is bound to moving.
   * Compared, it was dropped whenever the last person's latest flush was
   * newer than this person's record, and the rail answered as the new
   * person was thrown away until they wrote again. It replaces the held
   * state, and everything else this tab held about the last person goes
   * with it ([forgetPerson]).
   */
  applyChatRead(state: ChatReadState | null | undefined): void {
    if (!state) return;
    const held = this.state.chatRead;
    if (held && held.handle !== state.handle) {
      this.forgetPerson();
    } else if (held && (state.revision ?? 0) < (held.revision ?? 0)) {
      return;
    }
    this.state.chatRead = state;
    this.emit("chatRead");
  }

  /**
   * Forget everything this tab holds about the PERSON it was reading as: the
   * frames of the rooms they could read, who was in the rooms they had open,
   * and their read state.
   *
   * ALL THREE ARE ONE PERSON'S. A frame reaches a socket only for a room its
   * person may read and carries that room's name and an excerpt of what was
   * said, presence is for the rooms they had open, and a read state is theirs
   * by its key — so a tab whose credential now names somebody else, kept
   * any of them, would hold the last person's private rooms under the next
   * person's name, and offer the next person a Mute that undid the last
   * one's. Called by the socket when its credential changes, and by
   * [applyChatRead] when a state names somebody new.
   */
  forgetPerson(): void {
    // THE COUNT STAYS: it is nobody's ([ChatLive.frames]). And every frame
    // it counted is let go of, so a screen asking what arrived since one it
    // answered is told it cannot say ([ChatLive.dropped]).
    const { frames } = this.state.chat;
    this.state.chat = { rooms: {}, frames, dropped: frames };
    this.state.chatPresence = { rooms: {} };
    this.state.chatRead = null;
    this.emit("chat");
    this.emit("chatPresence");
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

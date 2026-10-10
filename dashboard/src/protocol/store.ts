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
// every phase reader for an answer that did not move.
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
    connected: false,
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
    this.state.connected = !!health;
    this.emit("health");
  }

  setConnected(value: boolean): void {
    this.state.connected = value;
    // A dropped socket CLEARS the health slice rather than freezing it. A stale
    // "healthy" is a lie with a timestamp nobody can see.
    if (!value) this.state.health = null;
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

  /** Count one frame whose `kind` this build does not dispatch. */
  noteUnknownPush(kind: unknown): void {
    const name = typeof kind === "string" ? kind : JSON.stringify(kind ?? null);
    this.unknownPushes.set(name, (this.unknownPushes.get(name) ?? 0) + 1);
  }
}

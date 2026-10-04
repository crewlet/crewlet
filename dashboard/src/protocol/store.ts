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
  FeedRow,
  EventEnvelope,
  InboxChange,
  OrgBudget,
  OrgProjection,
  OverlayRow,
  Rollup,
  SandboxEntry,
  ScheduleRow,
  Snapshot,
  ToolRow,
} from "./types.ts";
import { share } from "./share.ts";

// RELATIVE, like every contract import in this directory: it is also built
// alone as `protocol.js`, where the `~` alias does not exist. `MAX_EVENTS` is
// the server's own feed length, held there by a Go gate.
import { MAX_EVENTS } from "../contract/wire.ts";
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
   * by the snapshot and by every five-second tick. `{status: "unknown"}` while
   * the socket is down — the one value here that is not a push, and asserts
   * nothing beyond that it is not known.
   */
  health: EngineHealth;
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
   * tell them apart: a stopped engine comes back on its own, a session the
   * engine refused never does — a sign-in is the repair.
   */
  authRejected: boolean;
  /**
   * Why the engine KNOWS who this browser is and will not serve it this
   * surface, or null. Set by a `4403` close (the seat is gone from the chart,
   * or the grant the socket needs was withdrawn) and by a `403` handshake;
   * cleared when a socket opens. Distinct from `authRejected`, whose repair is
   * a credential: signing in again reaches the same person with the same
   * access, so the only repair is an administrator's.
   */
  accessRefused: string | null;
  /**
   * How many `inbox_changed` frames this socket has delivered, per seat.
   *
   * A COUNTER, NOT THE INBOX: the frame carries identifiers and a hint rather
   * than the notices, so what a screen does with it is ask `work_inbox` again
   * (see `useQuery`'s `refetchOnInboxOf`), and a count moving is the whole of
   * what it needs to know. Only the seat this socket WATCHES is ever sent
   * one. NOT part of what a snapshot replaces: a reconnect re-asks every query
   * anyway, so there is nothing for a snapshot to restore.
   */
  inboxMoves: Record<string, number>;
  /**
   * How many org projections this socket has been pushed.
   *
   * THE PUSH AS AN EVENT, beside `org` as a state. A chart write that landed is
   * followed by an org push, so a screen holding something it read from the
   * chart reads it again on one — and a write that changed only what the
   * projection leaves out (a seat's model chain, its credentials, a unit's
   * knowledge space) is pushed as a projection deep-equal to the last, which
   * the store shares and so does not move. Keyed on `org`'s identity, those
   * screens went on showing the settings from before the write. NOT part of
   * what a snapshot replaces: a reconnect re-reads every chart read anyway,
   * and the org builder checks its draft again on one.
   */
  orgPushes: number;
}

export type Slice = keyof StoreState;

function emptyState(): StoreState {
  return {
    agents: [],
    events: [],
    phases: [],
    sandboxes: [],
    org: null,
    tools: [],
    health: { status: "unknown" },
    tokens: null,
    budget: null,
    schedules: null,
    connected: false,
    authRejected: false,
    accessRefused: null,
    inboxMoves: {},
    orgPushes: 0,
  };
}

export class Store {
  state: StoreState = emptyState();

  /**
   * The push kinds this build does not know, each with how many arrived.
   *
   * IGNORED AND COUNTED. A node on another build may push a kind this bundle
   * was built before, and throwing on it — or applying it to a slice by a
   * guess — would break a screen over a frame it has no use for. But the same
   * fall-through is exactly what this build's own engine sending a kind its
   * own client forgot looks like, which is the silent failure the e2e replay
   * exists to catch: so it is kept here, and the replay asserts it is empty.
   * Not a slice, because nothing renders it and a listener woken by a frame
   * nobody can read would be woken for nothing.
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

  /**
   * Replace one slice with what was pushed, SHARED with what it held
   * (`./share.ts`), and say whether anything moved.
   *
   * A push is an answer like any other — parsed afresh off the wire — so a
   * spend rollup pushed after every phase handed the spend tables a new object
   * for every seat and every turn, and every row of both was drawn again for
   * the one turn that finished. Shared, a push that changed nothing moves no
   * version and wakes nobody, and one that changed something keeps the objects
   * of everything it did not change.
   */
  private replace<K extends Slice>(slice: K, next: StoreState[K]): boolean {
    const kept = share(this.state[slice], next);
    if (kept === this.state[slice]) return false;
    this.state[slice] = kept;
    return true;
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
    // ONLY THE SLICES IT MOVED are announced: a reconnect's snapshot is mostly
    // what this tab already holds, and announcing every slice redrew every
    // screen for it. `phases` is never among them — a snapshot carries
    // payload-free feed rows and no phase payloads.
    const moved: Slice[] = [];
    const put = <K extends Slice>(slice: K, next: StoreState[K]) => {
      if (this.replace(slice, next)) moved.push(slice);
    };
    put("agents", snap.agents ?? []);
    put("events", (snap.events ?? []).slice(0, MAX_EVENTS));
    put("sandboxes", snap.sandboxes ?? []);
    put("org", snap.org ?? {});
    put("tools", snap.tools ?? []);
    if (snap.tokens && snap.tokens.totals) put("tokens", snap.tokens);
    put("budget", snap.budget ?? null);
    // A bare list here, unlike the push's `{schedules: […]}` object.
    if (snap.schedules) put("schedules", snap.schedules);
    // NOT `connected`. That belongs to the transport, which knows whether the
    // socket is open; deriving it from a payload's contents meant a snapshot
    // arriving over the degraded REST fallback announced a live connection
    // that did not exist.
    if (snap.health) put("health", snap.health);
    if (moved.length > 0) this.emit(...moved);
  }

  /**
   * Changed seat overlays, merged onto the roster rows by AGENT ID.
   *
   * By the id and nothing else. They were merged by ROLE NAME, which is prose:
   * two seats sharing a name both took every overlay either of them moved, so
   * each card rendered whatever the other was last doing. The handle is no
   * better a key — a rename moves it while the overlays already in flight were
   * cut before it — and the id, derived from the handle a seat was created
   * under, is the one value a rename leaves where it was.
   *
   * An overlay for a seat the roster does not carry is DROPPED rather than
   * appended as a row of its own. A seat reaches this list through the roster
   * (the snapshot, or a `seats` push after every published company), and the
   * server merges its live overlay into that roster row before sending it —
   * so nothing dropped here is lost, while an appended row would be a card
   * with no name, no handle and no page, for a seat the server may already
   * have removed.
   */
  applyAgents(rows: OverlayRow[] | unknown): void {
    // `Array.isArray` is load-bearing. The server sent this as an object keyed
    // by role once; every push was silently discarded and seats rendered idle
    // for the whole of a turn, with both sides' own suites green. That is the
    // bug internal/e2e/golden_test.go exists to catch.
    if (!Array.isArray(rows) || rows.length === 0) return;
    const byID = new Map<string, OverlayRow>();
    for (const row of rows as OverlayRow[]) {
      if (row && typeof row.agent_id === "string" && row.agent_id !== "") {
        byID.set(row.agent_id, row);
      }
    }
    // A PATCH THAT RESTATES WHAT THE ROW SAYS MOVES NOTHING: an overlay is
    // pushed twice per tool-loop round, and a round that changed only one seat
    // re-sends the others' state as it was.
    const merged = this.state.agents.map((a) => {
      const patch = byID.get(a.agent_id);
      return patch ? { ...a, ...patch } : a;
    });
    if (this.replace("agents", merged)) this.emit("agents");
  }

  /**
   * The complete seat list, replacing what is on screen.
   *
   * Distinct from `applyAgents`, which merges changed overlays: a merge cannot
   * express a deletion, so a revision that removes a seat would leave its card
   * rendered until the next reload.
   */
  applySeats(rows: AgentRow[] | unknown): void {
    if (!Array.isArray(rows)) return;
    // Keep the live overlay each seat already carries, matched by AGENT ID —
    // which is what a renamed seat still carries when its handle and name
    // have both moved.
    const live = new Map(this.state.agents.map((a) => [a.agent_id, a]));
    const roster = (rows as AgentRow[]).map((row) => {
      const current = live.get(row.agent_id);
      return current ? { ...current, ...row } : row;
    });
    if (this.replace("agents", roster)) this.emit("agents");
  }

  applySandboxes(list: SandboxEntry[] | null | undefined): void {
    // `agents` too: a seat's effective state folds in whether it is parked on
    // a sandbox question, so a sandbox move is a seat move.
    if (this.replace("sandboxes", list ?? [])) this.emit("sandboxes", "agents");
  }

  applyTokens(rollup: Rollup | null | undefined): void {
    if (!rollup) return;
    if (this.replace("tokens", rollup)) this.emit("tokens");
  }

  applyBudget(budget: OrgBudget | null | undefined): void {
    if (this.replace("budget", budget ?? null)) this.emit("budget");
  }

  applySchedules(payload: { schedules?: ScheduleRow[] } | null): void {
    if (!payload) return;
    // Applied only when present: the push carries the CONFIGURED rows and
    // nothing else, so an absent key means "unchanged" rather than "empty".
    if (payload.schedules && this.replace("schedules", payload.schedules)) {
      this.emit("schedules");
    }
  }

  applyOrg(org: OrgProjection | null | undefined): void {
    // EVERY PUSH IS COUNTED, one deep-equal to the last included — see
    // `orgPushes` — while `org` moves only when the projection did.
    this.state.orgPushes += 1;
    if (this.replace("org", org ?? {})) this.emit("org", "orgPushes");
    else this.emit("orgPushes");
  }

  applyTools(tools: ToolRow[] | null | undefined): void {
    if (this.replace("tools", tools ?? [])) this.emit("tools");
  }

  applyHealth(health: EngineHealth | null | undefined): void {
    const connected = !!health && health.status !== "unknown";
    const moved = this.replace("health", health ?? { status: "unknown" });
    if (!moved && connected === this.state.connected) return;
    this.state.connected = connected;
    this.emit("health");
  }

  setConnected(value: boolean): void {
    this.state.connected = value;
    // A dropped socket CLEARS the health slice rather than freezing it. A stale
    // "healthy" is a lie with a timestamp nobody can see.
    if (!value) this.state.health = { status: "unknown" };
    this.emit("health");
  }

  /**
   * One seat's inbox moved. The SEAT is read off the payload's own `handle`
   * rather than trusted from anywhere else, and a frame without one moves
   * nothing — a counter under "" would be a seat no screen asks about.
   */
  applyInboxChanged(change: Partial<InboxChange> | null | undefined): void {
    const handle = change?.handle;
    if (typeof handle !== "string" || handle === "") return;
    this.state.inboxMoves = {
      ...this.state.inboxMoves,
      [handle]: (this.state.inboxMoves[handle] ?? 0) + 1,
    };
    this.emit("inboxMoves");
  }

  setAccessRefused(reason: string | null): void {
    const next = reason === null ? null : reason || "refused";
    if (this.state.accessRefused === next) return;
    this.state.accessRefused = next;
    this.emit("health");
  }

  /** Count one frame whose `kind` this build does not dispatch. */
  noteUnknownPush(kind: unknown): void {
    const name = typeof kind === "string" ? kind : JSON.stringify(kind ?? null);
    this.unknownPushes.set(name, (this.unknownPushes.get(name) ?? 0) + 1);
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
}

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
  HealthPush,
  OrgBudget,
  OrgTree,
  Overlay,
  Rollup,
  SandboxEntry,
  ScheduleRow,
  ScheduleRunRow,
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

export interface StoreState {
  agents: AgentRow[];
  events: FeedRow[];
  sandboxes: SandboxEntry[];
  org: OrgTree;
  tools: ToolRow[];
  health: HealthPush;
  tokens: Rollup | null;
  budget: OrgBudget;
  schedules: ScheduleRow[] | null;
  recentRuns: ScheduleRunRow[] | null;
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
    sandboxes: [],
    org: {},
    tools: [],
    health: { status: "unknown" },
    tokens: null,
    budget: {},
    schedules: null,
    recentRuns: null,
    connected: false,
    authRejected: false,
  };
}

export class Store {
  state: StoreState = emptyState();

  private subs = new Map<Slice, Set<() => void>>();
  private eventSubs = new Set<(ev: EventEnvelope) => void>();

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

  /** Call `fn` for every event envelope, as it arrives. */
  onEvent(fn: (ev: EventEnvelope) => void): () => void {
    this.eventSubs.add(fn);
    return () => {
      this.eventSubs.delete(fn);
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

  applySchedules(
    payload: { schedules?: ScheduleRow[]; recent_runs?: ScheduleRunRow[] } | null,
  ): void {
    if (!payload) return;
    // Each key is applied only when present: the push omits `recent_runs`, and
    // overwriting with undefined would blank what the screen already fetched.
    if (payload.schedules) this.state.schedules = payload.schedules;
    if (payload.recent_runs) this.state.recentRuns = payload.recent_runs;
    this.emit("schedules");
  }

  applyOrg(org: OrgTree | null | undefined): void {
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
    for (const fn of this.eventSubs) fn(ev);
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

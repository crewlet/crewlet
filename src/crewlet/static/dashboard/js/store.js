// The client's mirror of the server's live projection.
//
// This file used to contain a second implementation of the engine's
// state machine: an event→state map, sandbox lifecycle tracking, and an
// 85-line reimplementation of the server's token aggregation, all
// applied to raw events as they streamed past. Three copies of the same
// logic (this one, `api/live_state.py`, `api/tokens.py`) meant three
// ways to drift, and a refresh routinely disagreed with what had been on
// screen a moment earlier.
//
// Now the server computes the projection once and pushes the result. The
// store holds what it is given and tells interested views. It derives
// nothing.
//
// Subscriptions are per-slice. A health tick touches `health` and wakes
// only the header; a progress round touches `agents` and wakes only the
// views showing agents. Previously every envelope woke every view.

// Longest activity feed a tab keeps. Matches the server's own retention
// (EVENT_FEED_LIMIT in api/live_state.py) so a reconnect's snapshot
// neither truncates the feed nor leaves rows the server can't resend.
//
// Exported because it is also the limit of what anything derived from the
// feed can HONESTLY claim to know: a busy org fills 400 events in minutes,
// and a panel covering an hour has to say where the record actually starts
// rather than drawing the gap as quiet (see js/pulse.js).
export const MAX_EVENTS = 400;

export class Store {
  constructor() {
    this.state = {
      agents: [],
      events: [],
      sandboxes: [],
      org: {},
      tools: [],
      health: { status: "unknown" },
      tokens: null,
      // The engine's live token meter: `{meter_id, seq, org:{used,max,
      // refused_at}}`, or `{}` when nothing is reporting one. Per-seat
      // figures ride on each agent's overlay, not here.
      budget: {},
      schedules: null,
      recentRuns: null,
      connected: false,
      // Whether the engine REFUSED this browser rather than being
      // unreachable. Distinct from `connected` because the repair is
      // different and the reader cannot guess which one they are looking
      // at: a stopped engine comes back on its own, a rejected token
      // never does.
      authRejected: false,
    };
    // slice → Set<fn>
    this._subs = new Map();
    this._eventSubs = new Set();
  }

  /**
   * Call `fn` when any of `slices` changes.
   * Returns an unsubscribe function.
   */
  subscribe(slices, fn) {
    for (const slice of slices) {
      if (!this._subs.has(slice)) this._subs.set(slice, new Set());
      this._subs.get(slice).add(fn);
    }
    return () => {
      for (const slice of slices) this._subs.get(slice)?.delete(fn);
    };
  }

  /** Call `fn` for every event envelope, as it arrives. */
  onEvent(fn) {
    this._eventSubs.add(fn);
    return () => this._eventSubs.delete(fn);
  }

  _emit(...slices) {
    const called = new Set();
    for (const slice of slices) {
      for (const fn of this._subs.get(slice) || []) {
        if (called.has(fn)) continue;
        called.add(fn);
        fn(this.state);
      }
    }
  }

  // ---- pushes ----

  applySnapshot(snap) {
    if (!snap) return;
    this.state.agents = snap.agents || [];
    this.state.events = (snap.events || []).slice(0, MAX_EVENTS);
    this.state.sandboxes = snap.sandboxes || [];
    this.state.org = snap.org || {};
    this.state.tools = snap.tools || [];
    if (snap.tokens && snap.tokens.totals) this.state.tokens = snap.tokens;
    this.state.budget = snap.budget || {};
    if (snap.schedules) this.state.schedules = snap.schedules;
    // NOT `connected`. That belongs to the transport, which knows
    // whether the socket is open; deriving it from a payload's contents
    // meant a snapshot arriving over the degraded-mode REST fallback
    // announced a live connection that did not exist.
    if (snap.health) this.state.health = snap.health;
    this._emit(
      "agents",
      "events",
      "sandboxes",
      "org",
      "tools",
      "tokens",
      "budget",
      "schedules",
      "health",
    );
  }

  /** Changed agent overlays, keyed by role. */
  applyAgents(rows) {
    if (!Array.isArray(rows) || !rows.length) return;
    const byRole = new Map(rows.map((r) => [r.role, r]));
    this.state.agents = this.state.agents.map((a) => {
      const patch = byRole.get(a.role);
      if (!patch) return a;
      byRole.delete(a.role);
      return { ...a, ...patch };
    });
    // A seat the config list doesn't carry yet (a role added by a live
    // revision) still belongs on screen.
    for (const row of byRole.values()) {
      this.state.agents = [...this.state.agents, { id: row.role, ...row }];
    }
    this._emit("agents");
  }

  /**
   * The complete seat list, replacing what is on screen.
   *
   * Distinct from `applyAgents`, which merges changed overlays by role:
   * a merge cannot express a deletion, so a revision that removes a role
   * would leave its card rendered until the next reload.
   */
  applySeats(rows) {
    if (!Array.isArray(rows)) return;
    // Keep the live overlay each seat already carries — the config
    // payload is static config and knows nothing about what a seat is
    // doing right now.
    const live = new Map(this.state.agents.map((a) => [a.role, a]));
    this.state.agents = rows.map((row) => {
      const current = live.get(row.role);
      return current ? { ...current, ...row } : row;
    });
    this._emit("agents");
  }

  applySandboxes(list) {
    this.state.sandboxes = list || [];
    this._emit("sandboxes", "agents");
  }

  applyTokens(rollup) {
    if (!rollup) return;
    this.state.tokens = rollup;
    this._emit("tokens");
  }

  applyBudget(budget) {
    this.state.budget = budget || {};
    this._emit("budget");
  }

  applySchedules(payload) {
    if (!payload) return;
    if (payload.schedules) this.state.schedules = payload.schedules;
    if (payload.recent_runs) this.state.recentRuns = payload.recent_runs;
    this._emit("schedules");
  }

  applyOrg(org) {
    this.state.org = org || {};
    this._emit("org");
  }

  applyTools(tools) {
    this.state.tools = tools || [];
    this._emit("tools");
  }

  applyHealth(health) {
    this.state.health = health || { status: "unknown" };
    this.state.connected = !!health && health.status !== "unknown";
    this._emit("health");
  }

  setConnected(value) {
    this.state.connected = value;
    if (!value) this.state.health = { status: "unknown" };
    this._emit("health");
  }

  setAuthRejected(value) {
    const next = !!value;
    if (this.state.authRejected === next) return;
    this.state.authRejected = next;
    this._emit("health");
  }

  applyEvent(ev) {
    if (!ev || !ev.id) return;
    // Only events the server persists belong in the feed. Streaming a
    // non-persisted type into it produced rows that vanished on the next
    // snapshot and 404'd when clicked.
    if (ev.category && !this.state.events.some((e) => e.id === ev.id)) {
      this.state.events = [ev, ...this.state.events].slice(0, MAX_EVENTS);
      this._emit("events");
    }
    for (const fn of this._eventSubs) fn(ev);
  }

  // ---- reads ----

  agentById(id) {
    return this.state.agents.find((a) => a.id === id || a.role === id) || null;
  }
}

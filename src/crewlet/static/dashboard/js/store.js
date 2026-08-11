// Reactive store: mirrors the server's live-state projection on the
// client so the UI updates smoothly between snapshots.
//
// The snapshot is authoritative (each agent already carries its in-flight
// `live_call`, so a refresh re-renders the live row); incoming stream
// events are projected onto agents incrementally for real-time feel.

const MAX_EVENTS = 250;

const _EVENT_STATE = {
  agent_spawned: "idle",
  task_started: "working",
  task_completed: "idle",
  task_failed: "idle",
  agent_terminated: "terminated",
  agent_turn_progress: "working",
  agent_phase_started: "working",
  agent_phase_completed: "working",
  reflection_completed: "idle",
  llm_unavailable: "afk",
  "turn.guard_breach": "afk",
  budget_exhausted: "afk",
};
const _AFK = new Set(["llm_unavailable", "turn.guard_breach", "budget_exhausted"]);

// Cap on the per-turn list, matching the server's recent_turns default
// (api/routes/tokens.py) so the live-folded `by_turn` stays bounded.
const RECENT_TURNS_CAP = 50;

// ---- live token-breakdown folding ---------------------------------------
// Mirrors the server's aggregate_phase_events (api/tokens.py) so the Token
// Spend rollup stays live between /tokens/breakdown fetches — the same way
// per-agent rows fold agent_turn_completed. Without this the headline and
// phase bar freeze at whatever the one-time fetch returned while the
// per-agent rows keep climbing (the "dashboard shows 0 while the CEO has
// already consumed" bug).

function _accum(b, p) {
  b.input_tokens += p.input_tokens || 0;
  b.output_tokens += p.output_tokens || 0;
  b.total_tokens += p.total_tokens || 0;
  b.calls += 1;
}

function _listRow(list, field, key) {
  let row = list.find((r) => r[field] === key);
  if (!row) {
    row = { [field]: key, input_tokens: 0, output_tokens: 0, total_tokens: 0, calls: 0 };
    list.push(row);
  }
  return row;
}

function _nestedPhase(obj, phase) {
  let row = obj[phase];
  if (!row) {
    row = { input_tokens: 0, output_tokens: 0, total_tokens: 0, calls: 0 };
    obj[phase] = row;
  }
  return row;
}

function _sortByTotal(list) {
  list.sort((a, b) => (b.total_tokens || 0) - (a.total_tokens || 0));
}

// Fold one agent_phase_completed envelope into a /tokens/breakdown `data`
// object, in place — same buckets and ordering the server produces so the
// dashboard's Tokens view renders identically off the folded data.
export function foldPhaseEvent(data, ev) {
  const p = ev.payload || {};
  const phase = p.phase || "unknown";
  const model = p.model || p.provider_key || "unknown";
  const worker = p.worker || "";
  const role = p.role || p.agent_role || "unknown";
  const agentId = p.agent_id || "";
  const turnId = p.turn_id || "";
  const ts = ev.timestamp || "";

  if (!data.totals) {
    data.totals = { input_tokens: 0, output_tokens: 0, total_tokens: 0, calls: 0 };
  }
  _accum(data.totals, p);

  data.by_phase = data.by_phase || [];
  _accum(_listRow(data.by_phase, "phase", phase), p);
  _sortByTotal(data.by_phase);

  data.by_model = data.by_model || [];
  _accum(_listRow(data.by_model, "model", model), p);
  _sortByTotal(data.by_model);

  // `worker` is meaningful only on auxiliary-phase calls; other phases
  // leave it blank and must not create a spurious blank-worker row.
  if (phase === "auxiliary" && worker) {
    data.by_worker = data.by_worker || [];
    _accum(_listRow(data.by_worker, "worker", worker), p);
    _sortByTotal(data.by_worker);
  }

  data.by_agent = data.by_agent || [];
  let agent = data.by_agent.find((r) => r.role === role);
  if (!agent) {
    agent = {
      role,
      handle: "",
      agent_id: agentId,
      input_tokens: 0,
      output_tokens: 0,
      total_tokens: 0,
      calls: 0,
      by_phase: {},
    };
    data.by_agent.push(agent);
  }
  if (agentId) agent.agent_id = agentId;
  _accum(agent, p);
  agent.by_phase = agent.by_phase || {};
  _accum(_nestedPhase(agent.by_phase, phase), p);
  _sortByTotal(data.by_agent);

  if (turnId) {
    data.by_turn = data.by_turn || [];
    let turn = data.by_turn.find((r) => r.turn_id === turnId);
    if (!turn) {
      turn = {
        turn_id: turnId,
        role,
        handle: "",
        agent_id: agentId,
        started_at: ts,
        ended_at: ts,
        input_tokens: 0,
        output_tokens: 0,
        total_tokens: 0,
        calls: 0,
        by_phase: {},
      };
      data.by_turn.push(turn);
    }
    if (agentId) turn.agent_id = agentId;
    if (ts) {
      if (!turn.started_at || ts < turn.started_at) turn.started_at = ts;
      if (!turn.ended_at || ts > turn.ended_at) turn.ended_at = ts;
    }
    _accum(turn, p);
    turn.by_phase = turn.by_phase || {};
    _accum(_nestedPhase(turn.by_phase, phase), p);
    // Newest-first, capped — matches the server's by_turn projection.
    data.by_turn.sort((a, b) => (b.ended_at || "").localeCompare(a.ended_at || ""));
    if (data.by_turn.length > RECENT_TURNS_CAP) data.by_turn.length = RECENT_TURNS_CAP;
  }
}

export class Store {
  constructor() {
    this.state = {
      agents: [],
      events: [],
      sandboxes: [], // in-flight detached sandbox runs (running-sandboxes panel)
      org: {},
      tools: [],
      health: { status: "unknown" },
      tokens: null, // cached /tokens/breakdown { window, data }
      schedules: null,
      connected: false,
    };
    this._subs = new Set();
    this._eventSubs = new Set();
    this._countedTurnIds = new Set();
    // agent_phase_completed ids already folded into state.tokens — the
    // token-rollup analogue of _countedTurnIds (prevents a re-delivered
    // live event from being folded twice).
    this._countedPhaseIds = new Set();
  }

  subscribe(fn) {
    this._subs.add(fn);
    return () => this._subs.delete(fn);
  }
  onEvent(fn) {
    this._eventSubs.add(fn);
    return () => this._eventSubs.delete(fn);
  }
  _emit() {
    for (const fn of this._subs) fn(this.state);
  }

  // ---- snapshot ----
  applySnapshot(snap) {
    if (!snap) return;
    this.state.agents = (snap.agents || []).map((a) => ({ ...a }));
    this.state.events = (snap.events || []).slice(0, MAX_EVENTS);
    this.state.sandboxes = snap.sandboxes || [];
    this.state.org = snap.org || {};
    this.state.tools = snap.tools || [];
    if (snap.health) this.applyHealth(snap.health, true);
    // Seed token dedup with the turn-completed ids already counted into
    // the snapshot's agent totals — prevents the register-before-snapshot
    // race from double-counting a turn.
    this._countedTurnIds = new Set(
      this.state.events
        .filter((e) => e.type === "agent_turn_completed")
        .map((e) => e.id),
    );
    this._emit();
  }

  applyHealth(h, silent = false) {
    this.state.health = h || { status: "unknown" };
    this.state.connected = (h && h.status && h.status !== "unknown") || false;
    if (!silent) this._emit();
  }

  setConnected(v) {
    this.state.connected = v;
    if (!v) this.state.health = { status: "unknown" };
    this._emit();
  }

  setTokens(window, data) {
    this.state.tokens = { window, data };
    // A fresh baseline supersedes anything folded so far; live folding
    // resumes from the baseline's watermark (data.aggregated_through).
    this._countedPhaseIds = new Set();
  }
  setSchedules(data) {
    this.state.schedules = data;
  }

  // ---- incremental event ----
  applyEvent(ev) {
    if (!ev || !ev.id) return;
    // Progress events update agents but are NOT persisted to the feed.
    if (ev.type !== "agent_turn_progress") {
      if (!this.state.events.some((e) => e.id === ev.id)) {
        this.state.events.unshift(ev);
        if (this.state.events.length > MAX_EVENTS) this.state.events.length = MAX_EVENTS;
      }
    }
    this._projectAgents(ev);
    this._projectSandboxes(ev);
    this._foldTokens(ev);
    // Notify per-event subscribers (agent detail, schedules, etc.).
    for (const fn of this._eventSubs) fn(ev);
    this._emit();
  }

  // ---- running-sandboxes projection ----
  // Mirrors LiveState._apply_sandbox (api/live_state.py): started → add,
  // clarification → awaiting-input, completed → drop. Keyed by turn_id.
  _projectSandboxes(ev) {
    const id = ev.payload && ev.payload.turn_id;
    if (!id) return;
    const p = ev.payload;
    const list = this.state.sandboxes || (this.state.sandboxes = []);
    if (ev.type === "sandbox_run_completed") {
      this.state.sandboxes = list.filter((s) => s.turn_id !== id);
    } else if (ev.type === "sandbox_run_started") {
      const entry = {
        turn_id: id,
        role: p.role || "",
        agent_handle: p.agent_handle || "",
        agent_id: p.agent_id || "",
        coding_agent: p.coding_agent || "",
        sandbox_id: p.sandbox_id || "",
        task: p.task || "",
        status: "running",
        started_at: ev.timestamp || "",
      };
      this.state.sandboxes = [...list.filter((s) => s.turn_id !== id), entry];
    } else if (ev.type === "sandbox_clarification_requested") {
      this.state.sandboxes = list.map((s) =>
        s.turn_id === id
          ? { ...s, status: "awaiting_input", question: p.question || "", audience: p.audience || "" }
          : s,
      );
    }
  }

  // Fold a completed phase's tokens into the cached breakdown so the
  // Token Spend rollup tracks live activity. No-op until a baseline has
  // been fetched (the next /tokens/breakdown captures the gap) and for
  // events the baseline already counted (timestamp <= its watermark).
  _foldTokens(ev) {
    if (ev.type !== "agent_phase_completed") return;
    const t = this.state.tokens;
    if (!t || !t.data) return;
    const through = t.data.aggregated_through || "";
    if (through && ev.timestamp && ev.timestamp <= through) return;
    if (this._countedPhaseIds.has(ev.id)) return;
    this._countedPhaseIds.add(ev.id);
    foldPhaseEvent(t.data, ev);
  }

  _findAgent(payload) {
    const rid = payload.agent_id || "";
    const role = payload.role || payload.agent_role || "";
    let a = rid ? this.state.agents.find((x) => x.runtime_id === rid) : null;
    if (!a && role) {
      a = this.state.agents.find((x) => x.role === role);
      if (a && rid && !a.runtime_id) a.runtime_id = rid;
    }
    return a;
  }

  _projectAgents(ev) {
    const p = ev.payload || {};
    const a = this._findAgent(p);
    if (!a) return;
    const type = ev.type;

    if (type === "agent_turn_progress") {
      a.state = "working";
      a.afk_reason = "";
      a.current_phase = p.phase || a.current_phase;
      a.current_iteration = p.iteration || 0;
      a.live_call = {
        turn_id: p.turn_id || "",
        phase: p.phase || "",
        iteration: p.iteration || 0,
        model: p.model || "",
        response: p.response || "",
        tool_executions: p.tool_executions || [],
        round_num: p.round_num || 0,
        rounds: (p.round_num || 0) + 1,
        total_tokens: p.total_tokens || 0,
        in_progress: true,
      };
      return;
    }

    if (type === "agent_turn_completed") {
      if (!this._countedTurnIds.has(ev.id)) {
        this._countedTurnIds.add(ev.id);
        a.input_tokens = (a.input_tokens || 0) + (p.input_tokens || 0);
        a.output_tokens = (a.output_tokens || 0) + (p.output_tokens || 0);
        a.total_tokens = (a.total_tokens || 0) + (p.total_tokens || 0);
      }
      a.live_call = null;
      return;
    }

    const newState = _EVENT_STATE[type];
    if (!newState) return;

    if (type === "agent_phase_started") {
      a.state = "working";
      a.afk_reason = "";
      a.current_phase = p.phase || null;
      a.current_iteration = p.iteration || 0;
      a.live_call = {
        turn_id: p.turn_id || "",
        phase: p.phase || "",
        iteration: p.iteration || 0,
        model: "",
        response: "",
        tool_executions: [],
        round_num: -1,
        rounds: 0,
        in_progress: true,
      };
    } else if (type === "agent_phase_completed") {
      a.state = "working";
      a.afk_reason = "";
      const lc = a.live_call;
      if (
        lc &&
        lc.turn_id === (p.turn_id || "") &&
        lc.phase === (p.phase || "") &&
        (lc.iteration || 0) === (p.iteration || 0)
      ) {
        a.live_call = null;
      }
    } else if (type === "task_started") {
      a.state = "working";
      a.current_task = p.task_id || null;
      a.afk_reason = "";
    } else if (type === "task_completed" || type === "task_failed") {
      a.state = "idle";
      a.current_task = null;
      a.current_phase = null;
      a.current_iteration = 0;
      a.afk_reason = "";
      a.live_call = null;
    } else if (type === "reflection_completed") {
      a.state = "idle";
      a.current_phase = null;
      a.current_iteration = 0;
      a.live_call = null;
    } else if (type === "agent_terminated") {
      a.state = "terminated";
      a.live_call = null;
    } else if (_AFK.has(type)) {
      a.state = "afk";
      a.afk_reason = p.kind || type;
      a.live_call = null;
    }
  }
}

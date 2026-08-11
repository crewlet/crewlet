// Thin REST client. All reads degrade to null on error (callers null-check).

const BASE = location.origin;

async function get(path, opts) {
  try {
    const r = await fetch(BASE + path, opts);
    if (!r.ok) return { _error: r.status };
    return await r.json();
  } catch {
    return { _error: 0 };
  }
}

function ok(v) {
  return v && !v._error;
}

export const api = {
  snapshot: () => get("/stream/snapshot"),
  agent: (id) => get(`/agents/${encodeURIComponent(id)}`),
  agentMemory: (id) => get(`/agents/${encodeURIComponent(id)}/memory`),
  event: (id) => get(`/events/${encodeURIComponent(id)}`),
  trace: (traceId) =>
    get(`/events?trace_id=${encodeURIComponent(traceId)}&limit=200`),
  schedules: () => get("/schedules"),
  tokens: ({ sinceDays = 7, agentRole = "", recentTurns = 50 } = {}) => {
    const p = new URLSearchParams({
      since_days: sinceDays,
      recent_turns: recentTurns,
    });
    if (agentRole) p.set("agent_role", agentRole);
    return get(`/tokens/breakdown?${p}`);
  },
  // --- config (auth-gated) ---
  config: (token) => get("/config", { headers: authH(token) }),
  configAudit: (token) =>
    get("/config/audit?limit=50", { headers: authH(token) }),
  configDiff: (revId, token) =>
    get(`/config/revisions/${encodeURIComponent(revId)}/diff`, {
      headers: authH(token),
    }),
  revertConfig: (revId, summary, token) =>
    get(`/config/revisions/${encodeURIComponent(revId)}/revert`, {
      method: "POST",
      headers: { ...authH(token), "X-Summary": summary },
    }),
};

function authH(token) {
  return token ? { Authorization: "Bearer " + token } : {};
}

export { ok };

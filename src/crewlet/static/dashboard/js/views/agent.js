// Agent detail: live state, current task, per-phase token summary, durable
// memory, and the LLM invocation history (with in-flight live rows) last.

import {
  esc,
  escAttr,
  fmtNum,
  fmtTime,
  mdLite,
  relTime,
  shortId,
  trunc,
  untilTime,
} from "../format.js";
import { icon } from "../icons.js";
import {
  avatarFor,
  stateBadgeClass,
  phaseSuffix,
  afkQuip,
  phaseColor,
  phaseInk,
  PHASE_ORDER,
  effectiveAgentState,
  stateLabel,
} from "../state.js";
import { empty, phaseBar } from "../ui.js";
import { copyToClipboard, toast } from "../dom.js";
import {
  isCodingAgentPhase,
  toolsAvailable,
  promptSections,
  responseBody,
  tokenStats,
  triggerSource,
} from "../llm.js";

export function createAgentView({ store, api, navigate, params }) {
  const id = params.id;
  const llmSub = params.turn
    ? { turn: params.turn, phase: params.phase, iter: String(params.iter || "0") }
    : null;

  let root;
  let data = null; // /agents/{id}
  let memory = null;
  let phaseSummary = null;
  let llm = []; // history records
  const liveCalls = new Map();
  // Expansion state — keyed by stable identifiers so a re-render (or live
  // streaming) keeps whatever the user expanded.
  const openTurns = new Set(); // turn_id (completed turns the user opened)
  const userCollapsed = new Set(); // turn_id (live turns the user collapsed)
  const openRows = new Set(); // rowKey (completed phase rows opened)
  const collapsedLive = new Set(); // rowKey (live rows the user collapsed)
  const collapsedThinks = new Set(); // think key (reasoning the user collapsed)
  const openTools = new Set(); // tool key (tool details the user opened)
  const openPrompts = new Set(); // prompt key (long messages the user opened)
  const collapsedPrompts = new Set(); // prompt key (short messages the user collapsed)
  let phaseWindow = 7;

  const isThinkOpen = (k) => !collapsedThinks.has(k);
  const isToolOpen = (k) => openTools.has(k);
  // Long prompt messages collapse by default, short ones stay open; an
  // explicit user toggle (either direction) wins and survives re-renders.
  const isPromptOpen = (k, long) =>
    openPrompts.has(k) ? true : collapsedPrompts.has(k) ? false : !long;

  function liveAgent() {
    return (store.state.agents || []).find((a) => a.id === id) || data || {};
  }

  // ---- record normalisation ----
  function fromPhase(p, ts) {
    const msgs = [];
    if (p.system_prompt) msgs.push({ role: "system", content: p.system_prompt });
    if (p.user_prompt) msgs.push({ role: "user", content: p.user_prompt });
    return {
      timestamp: ts,
      model: p.model || p.provider_key || "",
      phase: p.phase || "",
      iteration: p.iteration || 0,
      worker: p.worker || "",
      decision: p.decision || "",
      turn_id: p.turn_id || "",
      trigger: p.trigger || {},
      prompt_messages: msgs,
      response: p.response || "",
      input_tokens: p.input_tokens || 0,
      output_tokens: p.output_tokens || 0,
      total_tokens: p.total_tokens || 0,
      tool_executions: p.tool_executions || [],
      tools_available: p.tools_available || [],
      tool_catalogue: p.tool_catalogue || [],
      backend: p.backend || "native",
      coding_agent: p.coding_agent || "",
      cost_usd: p.cost_usd || 0,
      delivered_refs: p.delivered_refs || [],
    };
  }

  function recKey(r) {
    return `${r.turn_id}|${r.phase}|${r.iteration}|${r.timestamp}`;
  }
  function unshiftRecord(r) {
    const key = recKey(r);
    if (!llm.some((x) => recKey(x) === key)) llm.unshift(r);
    if (llm.length > 200) llm.length = 200;
  }

  // ---- grouping ----
  function groupTurns() {
    const groups = new Map();
    for (const r of llm) {
      if (!groups.has(r.turn_id)) groups.set(r.turn_id, { items: [], live: [] });
      groups.get(r.turn_id).items.push(r);
    }
    for (const [key, lc] of liveCalls) {
      const tid = lc.turn_id || key;
      if (!groups.has(tid)) groups.set(tid, { items: [], live: [] });
      groups.get(tid).live.push(lc);
    }
    const mkGroup = (gid, items, live, trigger, isOnboarding) => {
      const all = [...items, ...live];
      const ts = all.map((x) => x.timestamp).filter(Boolean).sort();
      return {
        turn_id: gid,
        baseTurnId: gid.split("#")[0],
        items,
        live,
        trigger,
        isOnboarding,
        firstTs: ts[0] || "",
        lastTs: ts[ts.length - 1] || "",
        isLive: live.length > 0,
      };
    };
    const arr = [...groups.entries()].flatMap(([turn_id, g]) => {
      // Drop the whole-turn aggregate when individual phase rows exist.
      const hasPhases = g.items.some((r) => r.phase && r.phase !== "turn");
      const items = hasPhases ? g.items.filter((r) => r.phase !== "turn") : g.items;
      // Read top-to-bottom in execution order: oldest first, with the
      // phase order (onboarding → plan → execute → review → …) breaking ties.
      items.sort((a, b) => {
        if (a.timestamp !== b.timestamp) return a.timestamp < b.timestamp ? -1 : 1;
        return (PHASE_ORDER[a.phase] || 50) - (PHASE_ORDER[b.phase] || 50);
      });
      // The trigger is a turn-level property — every phase carries the same
      // descriptor; take the first non-empty one.
      const trigger =
        [...items, ...g.live].map((x) => x.trigger).find((tr) => tr && (tr.type || tr.id)) ||
        {};
      // Onboarding is one-time first-turn SETUP — it just rides the turn the
      // trigger happened to wake. Split it into its own group (no trigger
      // chip) so it doesn't read as part of the notification's task. A
      // "#onboarding" suffix keeps the group key / toggle state distinct while
      // the rows keep the real turn_id underneath.
      const onb = items.filter((r) => r.phase === "onboarding");
      const rest = items.filter((r) => r.phase !== "onboarding");
      const out = [];
      if (rest.length || g.live.length || !onb.length)
        out.push(mkGroup(turn_id, rest, g.live, trigger, false));
      if (onb.length) out.push(mkGroup(`${turn_id}#onboarding`, onb, [], {}, true));
      return out;
    });
    // Most-recent turn first in the list; phases within each turn stay
    // chronological (sorted above).
    arr.sort((a, b) => (a.lastTs < b.lastTs ? 1 : -1));
    return arr;
  }

  // ---- renderers ----
  function renderHead() {
    const a = liveAgent();
    const st = effectiveAgentState(a, store.state.sandboxes);
    const badge = stateBadgeClass(st);
    const quip =
      a.state === "afk"
        ? `<div class="afk-quip" style="margin-top:6px">${esc(afkQuip(a.afk_reason))}</div>`
        : "";
    return `
      <div class="card" style="padding:18px;display:flex;gap:16px;align-items:center;margin-bottom:14px">
        ${avatarFor(a.role || data.role)}
        <div style="flex:1;min-width:0">
          <div style="font-size:18px;font-weight:680">${esc(a.role || data.role || "Agent")}</div>
          <div class="row-sub"><span class="mono">${esc(shortId(a.runtime_id || data.id, 14))}</span> · @${esc(data.handle || "")}</div>
          ${quip}
        </div>
        <div style="text-align:right">
          <span class="badge ${badge}"><i class="dot"></i>${esc(stateLabel(st))}${esc(phaseSuffix(a))}</span>
          <div class="tok-chip" style="justify-content:flex-end;margin-top:8px">
            <span class="in">${fmtNum(a.input_tokens || 0)}</span> in ·
            <span class="out">${fmtNum(a.output_tokens || 0)}</span> out ·
            <span class="tot">${fmtNum(a.total_tokens || 0)}</span></div>
        </div>
      </div>`;
  }

  function renderTask() {
    const a = liveAgent();
    if (!a.current_task) return "";
    return `
      <div class="card" style="padding:14px 16px;margin-bottom:14px;display:flex;align-items:center;gap:10px">
        <span class="dot working" style="width:10px;height:10px"></span>
        <div style="flex:1"><b>Current task</b> · ${esc(a.current_task)}</div>
        <span class="badge live"><i class="dot"></i>in progress</span>
      </div>`;
  }

  function renderPhaseSummary() {
    if (!phaseSummary || !phaseSummary.by_phase || !phaseSummary.by_phase.length) return "";
    const t = phaseSummary.totals;
    return `
      <div class="card" style="padding:14px 16px;margin-bottom:14px">
        <div class="row-sub" style="margin-bottom:8px">
          ${icon("zap", "sm")} last ${phaseWindow}d · ${fmtNum(t.total_tokens)} tokens ·
          ${fmtNum(t.input_tokens)} in / ${fmtNum(t.output_tokens)} out · ${fmtNum(t.calls)} calls
        </div>
        ${phaseBar(phaseSummary.by_phase, t.total_tokens)}
      </div>`;
  }

  // A live row is open by default (so you watch it stream) unless the
  // user collapsed it; a completed row is closed unless the user opened it.
  function rowIsOpen(key, isLive) {
    return isLive ? !collapsedLive.has(key) : openRows.has(key);
  }

  function rowPreview(r) {
    if (r._live)
      return r.response ? trunc(r.response, 90) : "Working — response streams in…";
    return trunc((r.response || "").replace(/<\/?think>/g, ""), 90);
  }

  function rowBody(r, key) {
    return `
      ${triggerSource(r.trigger)}
      ${toolsAvailable(r)}
      <div class="block-label">Prompt</div>${promptSections(r, {
        keyPrefix: key,
        isOpen: isPromptOpen,
      })}
      <div class="block-label">Response</div>${responseBody(r.response, r.tool_executions, {
        keyPrefix: key,
        thinkOpen: isThinkOpen,
        toolOpen: isToolOpen,
        codingAgent: isCodingAgentPhase(r),
        model: r.model,
      })}`;
  }

  // Sandbox Execute badge: coding agent + cost + PR link.
  // Renders only on a sandbox-backed Execute phase; native phases are bare.
  function sandboxBadge(r) {
    if (r.backend !== "sandbox") return "";
    const cost = r.cost_usd ? ` · $${Number(r.cost_usd).toFixed(2)}` : "";
    const ref = (r.delivered_refs || [])[0];
    const pr = ref
      ? ` · <a href="${escAttr(ref)}" target="_blank" rel="noopener" data-action="stop">PR</a>`
      : "";
    return `<span class="chip sandbox" title="Sandboxed Execute (${escAttr(r.coding_agent || "coding agent")})">⛁ ${esc(r.coding_agent || "sandbox")}${cost}${pr}</span>`;
  }

  function phaseRow(r, key) {
    const open = rowIsOpen(key, r._live);
    const tail = r._live
      ? `<span class="badge live"><i class="dot"></i>live · round ${r.rounds || 0}</span>`
      : `<button class="btn sm" data-action="open-llm" data-turn="${esc(r.turn_id)}" data-phase="${esc(r.phase)}" data-iter="${esc(r.iteration)}">Details</button>`;
    return `
      <div class="llm-row ${open ? "open" : ""} ${r._live ? "is-live" : ""}" data-rowkey="${escAttr(key)}" style="--phase-color:${phaseColor(r.phase)}">
        <div class="llm-row-head" data-action="toggle-row" data-key="${escAttr(key)}">
          ${icon("chevron", "chevron")}
          <span class="ph-pill" style="color:${phaseInk(r.phase)}">${esc(r.phase)}${r.iteration ? " #" + r.iteration : ""}</span>
          ${r.worker ? `<span class="chip">${esc(r.worker)}</span>` : ""}
          ${sandboxBadge(r)}
          <span class="llm-model">${esc(r.model || "—")}</span>
          <span class="llm-preview">${esc(rowPreview(r))}</span>
          ${tail}
        </div>
        <div class="llm-row-body">${rowBody(r, key)}</div>
      </div>`;
  }

  function renderTurns() {
    const groups = groupTurns();
    if (!groups.length) return empty("cpu", "No LLM invocations yet");
    return groups
      .map((g) => {
        const autoOpen =
          (g.isLive && !userCollapsed.has(g.turn_id)) || openTurns.has(g.turn_id);
        const phaseCount = {};
        let total = 0;
        for (const r of g.items) {
          phaseCount[r.phase] = (phaseCount[r.phase] || 0) + 1;
          total += r.total_tokens || 0;
        }
        const pills = Object.keys(phaseCount)
          .sort((a, b) => (PHASE_ORDER[a] || 50) - (PHASE_ORDER[b] || 50))
          .map(
            (p) =>
              `<span class="ph-pill" style="color:${phaseInk(p)}">${esc(p)}${phaseCount[p] > 1 ? " ×" + phaseCount[p] : ""}</span>`,
          )
          .join("");

        const liveRows = g.live
          .map((lc) =>
            phaseRow(
              { ...lc, _live: true },
              `live|${lc.turn_id}|${lc.phase}|${lc.iteration}`,
            ),
          )
          .join("");
        const rows = g.items
          .map((r) => phaseRow(r, recKey(r)))
          .join("");

        // Onboarding groups get a distinct "setup" label + no trigger chip;
        // task turns keep the "turn <id>" label and the trigger source.
        const label = g.isOnboarding
          ? `${icon("book", "sm")}<span class="turn-id">onboarding</span><span class="row-sub mono">${esc(shortId(g.baseTurnId))}</span>`
          : `<span class="turn-id">turn ${esc(shortId(g.turn_id))}</span>`;
        return `
          <div class="turn ${autoOpen ? "open" : ""} ${g.isLive ? "live" : ""} ${g.isOnboarding ? "onboarding" : ""}" data-turn="${esc(g.turn_id)}">
            <div class="turn-head" data-action="toggle-turn" data-turn="${esc(g.turn_id)}">
              ${icon("chevron", "chevron")}
              <span class="row-ts">${esc(fmtTime(g.firstTs))}</span>
              ${label}
              <span class="turn-pills">${pills}</span>
              ${g.isOnboarding ? "" : triggerSource(g.trigger, { compact: true })}
              <span style="flex:1"></span>
              ${g.isLive ? '<span class="badge live"><i class="dot"></i>live</span>' : ""}
              <span class="tok-chip"><span class="tot">${fmtNum(total)}</span></span>
              <button class="btn sm" data-action="copy-turn" data-turn="${esc(g.turn_id)}">${icon("copy", "sm")}</button>
            </div>
            <div class="turn-body">${rows}${liveRows}</div>
          </div>`;
      })
      .join("");
  }

  // Render memory/summary text readably: light markdown, preserved
  // newlines, and a "Show more" clamp for long blocks (e.g. the full
  // task prompt stored on an episode).
  function memText(raw, threshold = 280) {
    const long = (raw || "").length > threshold;
    return `<div class="mem-text ${long ? "clamped" : ""}">${mdLite(raw)}</div>${
      long ? '<span class="mem-more" data-action="toggle-clamp">Show more</span>' : ""
    }`;
  }

  function renderMemory() {
    if (!memory) return "";
    const m = memory;
    const blocks = [];
    const long = m.personal_memories?.long || [];
    const short = m.personal_memories?.short || [];
    const total =
      long.length +
      short.length +
      (m.episodes || []).length +
      (m.counterparty_profiles || []).length +
      (m.synthesized_skills || []).length;

    function block(key, color, iconId, title, count, body, open) {
      return `
        <div class="mem-card ${open ? "open" : ""}" data-mem="${key}">
          <div class="mem-head" data-action="toggle-mem" data-mem="${key}" style="color:${color}">
            <span class="mem-icon">${icon(iconId, "sm")}</span>
            <div style="flex:1;color:var(--text)"><div class="mem-title">${esc(title)}</div></div>
            <span class="badge">${count}</span>
            ${icon("chevron", "chevron")}
          </div>
          <div class="mem-body">${count ? body : '<div class="mem-entry"><span class="empty-sub">None yet</span></div>'}</div>
        </div>`;
    }

    const memEntries = (entries, kind) =>
      entries
        .map(
          (e) => `
        <div class="mem-entry ${e.expired ? "expired" : ""}">
          <div class="mem-entry-meta">
            <span class="mono">${esc(shortId(e.id, 10))}</span>
            <span class="badge ${kind === "short" ? "info" : "purple"}">${kind}</span>
            ${e.ttl_until ? `<span>${e.expired ? `expired ${esc(relTime(e.ttl_until))}` : `expires ${esc(untilTime(e.ttl_until))}`}</span>` : ""}
          </div>
          ${memText(e.content)}
        </div>`,
        )
        .join("");

    const episodes = (m.episodes || [])
      .map((e) => {
        const oc = {
          done: "var(--green-ink)",
          self_iterate: "var(--amber-ink)",
        }[e.review_outcome] || "var(--text-muted)";
        return `
        <div class="mem-entry">
          <div class="mem-entry-meta">
            <span style="color:${oc};font-weight:600">${esc(e.review_outcome || "—")}</span>
            <span>${(e.duration_ms / 1000).toFixed(1)}s</span>
            ${e.task_id ? `<span class="mono">${esc(e.task_id)}</span>` : ""}
            <span>${esc(relTime(e.ended_at))}</span>
          </div>
          ${e.task_summary ? `<div class="mem-label">Task</div>${memText(e.task_summary)}` : ""}
          ${e.plan_summary && e.plan_summary !== e.task_summary ? `<div class="mem-label">Plan</div>${memText(e.plan_summary)}` : ""}
          ${(e.tool_sequence || []).length ? `<div class="mem-label">Tools</div><div class="chip-row">${e.tool_sequence.slice(0, 8).map((t) => `<span class="chip">${esc(t)}</span>`).join("")}</div>` : ""}
        </div>`;
      })
      .join("");

    const counterparties = (m.counterparty_profiles || [])
      .map(
        (c) => `
        <div class="mem-entry">
          <div class="mem-entry-meta">
            <b style="color:var(--text)">${esc(c.subject_label)}</b>
            ${c.subject_handle ? `<span class="mono">@${esc(c.subject_handle)}</span>` : ""}
            ${c.subject_platform ? `<span>${esc(c.subject_platform)}</span>` : ""}
            <span>${c.interaction_count} interactions</span>
            <span>${esc(relTime(c.last_updated_at))}</span>
          </div>
          ${Object.keys(c.traits || {}).length ? `<div class="mem-entry-content">${Object.entries(c.traits).map(([k, v]) => `<div><b>${esc(k)}:</b> ${esc(typeof v === "string" ? v : JSON.stringify(v))}</div>`).join("")}</div>` : ""}
        </div>`,
      )
      .join("");

    const skills = (m.synthesized_skills || [])
      .map(
        (s) => `
        <div class="mem-entry">
          <div class="mem-entry-meta">
            <b style="color:var(--text)">${esc(s.name)}</b>
            <span class="badge">v${s.version}</span>
            <span>${esc(relTime(s.updated_at))}</span>
          </div>
          ${memText(s.description)}
          ${(s.tool_sequence || []).length ? `<div class="chip-row" style="margin-top:6px">${s.tool_sequence.slice(0, 6).map((t) => `<span class="chip">${esc(t)}</span>`).join("")}</div>` : ""}
        </div>`,
      )
      .join("");

    blocks.push(block("long", "var(--purple-ink)", "database", "Long-term memory", long.length, memEntries(long, "long"), true));
    blocks.push(block("short", "var(--cyan-ink)", "clock", "Short-term memory", short.length, memEntries(short, "short"), false));
    blocks.push(block("episodes", "var(--blue-ink)", "activity", "Episodes", (m.episodes || []).length, episodes, false));
    blocks.push(block("cp", "var(--green-ink)", "users", "Counterparty profiles", (m.counterparty_profiles || []).length, counterparties, false));
    blocks.push(block("skills", "var(--amber-ink)", "book", "Synthesized skills", (m.synthesized_skills || []).length, skills, false));

    return `
      <div class="sec"><span class="sec-title">${icon("database", "sm")} Memory <span class="sec-count">${total}</span></span></div>
      ${blocks.join("")}`;
  }

  // ---- LLM detail sub-panel ----
  function findRecord() {
    return llm.find(
      (r) =>
        r.turn_id === llmSub.turn &&
        r.phase === llmSub.phase &&
        String(r.iteration) === llmSub.iter,
    );
  }
  function renderLlmDetail() {
    const r = findRecord();
    if (!r) {
      root.innerHTML = `<div class="back-link" data-action="back-agent">${icon("chevron", "sm")} Back to agent</div>${empty("cpu", "LLM record not found", "It may have scrolled out of the recent history.")}`;
      return;
    }
    root.innerHTML = `
      <div class="back-link" data-action="back-agent">${icon("chevron", "sm")} Back to agent</div>
      <div class="card llm-detail" style="padding:18px;--phase-color:${phaseColor(r.phase)}">
        <div style="display:flex;align-items:center;gap:10px;margin-bottom:12px">
          <span class="ph-pill" style="color:${phaseInk(r.phase)}">${esc(r.phase)}${r.iteration ? " #" + r.iteration : ""}</span>
          <span class="llm-model">${esc(r.model || "—")}</span>
          <span class="row-ts">${esc(fmtTime(r.timestamp))}</span>
          <span style="flex:1"></span>
          <button class="btn sm" data-action="copy-record">${icon("copy", "sm")} Copy</button>
        </div>
        ${tokenStats(r)}
        ${triggerSource(r.trigger)}
        ${toolsAvailable(r)}
        <div class="block-label">Prompt</div>${promptSections(r, {
          keyPrefix: "detail|" + recKey(r),
          isOpen: isPromptOpen,
        })}
        <div class="block-label">Response</div>${responseBody(r.response, r.tool_executions, {
          codingAgent: isCodingAgentPhase(r),
          model: r.model,
        })}
      </div>`;
  }

  // ---- full render ----
  function renderAll() {
    if (llmSub) {
      renderLlmDetail();
      return;
    }
    root.innerHTML = `
      <div class="back-link" data-action="nav-dashboard">${icon("chevron", "sm")} Agents</div>
      <div id="agent-head">${renderHead()}</div>
      <div id="agent-task">${renderTask()}</div>
      <div id="agent-phase">${renderPhaseSummary()}</div>
      <div id="agent-memory">${renderMemory()}</div>
      <div class="sec"><span class="sec-title">${icon("cpu", "sm")} LLM Invocations <span class="sec-count" id="agent-llm-count">${llm.length}</span></span></div>
      <div id="agent-llm">${renderTurns()}</div>`;
  }

  function refreshHead() {
    const head = root && root.querySelector("#agent-head");
    if (head) head.innerHTML = renderHead();
    const task = root && root.querySelector("#agent-task");
    if (task) task.innerHTML = renderTask();
  }

  // Full re-render of the LLM list — used only for structural changes
  // (a phase started/completed, a turn finished). Infrequent, and
  // expansion survives because every open/closed bit is re-applied from
  // the tracked sets. The high-frequency progress path patches in place
  // (``patchLiveRow``) instead, so streaming never wipes the section.
  function fullRenderLlm() {
    const el = root && root.querySelector("#agent-llm");
    if (el) el.innerHTML = renderTurns();
    const count = root && root.querySelector("#agent-llm-count");
    if (count) count.textContent = llm.length;
    refreshHead();
  }

  function reloadPhaseSummary() {
    if (!data) return;
    api.tokens({ sinceDays: phaseWindow, agentRole: data.role }).then((d) => {
      if (d && !d._error) {
        phaseSummary = d;
        const el = root && root.querySelector("#agent-phase");
        if (el) el.innerHTML = renderPhaseSummary();
      }
    });
  }

  // Patch just the live row's streaming bits in place — no section
  // rebuild, so nothing the user expanded elsewhere closes.
  function patchLiveRow(k) {
    const lc = liveCalls.get(k);
    if (!lc || !root) return;
    const rowKey = `live|${lc.turn_id}|${lc.phase}|${lc.iteration}`;
    const sel = window.CSS && CSS.escape ? CSS.escape(rowKey) : rowKey;
    const rowEl = root.querySelector(`[data-rowkey="${sel}"]`);
    if (!rowEl) {
      fullRenderLlm(); // row not in the DOM yet (e.g. progress before start)
      return;
    }
    const r = { ...lc, _live: true };
    const prev = rowEl.querySelector(".llm-preview");
    if (prev) prev.textContent = rowPreview(r);
    const model = rowEl.querySelector(".llm-model");
    if (model && r.model) model.textContent = r.model;
    const badge = rowEl.querySelector(".badge.live");
    if (badge) badge.innerHTML = `<i class="dot"></i>live · round ${r.rounds || 0}`;
    if (rowEl.classList.contains("open")) {
      const body = rowEl.querySelector(".llm-row-body");
      if (body) body.innerHTML = rowBody(r, rowKey);
    }
  }

  // ---- data loading ----
  async function load() {
    const [a, mem] = await Promise.all([api.agent(id), api.agentMemory(id)]);
    if (!a || a._error) {
      root.innerHTML = empty("user", "Agent not found");
      return;
    }
    data = a;
    llm = (a.llm_history || []).slice();
    memory = mem && !mem._error ? mem : null;
    // Seed the in-flight live row from the snapshot/agent payload so a
    // refresh mid-call re-renders it immediately — the whole point of
    // carrying `live_call` through the projection.  Stream progress
    // events then keep it current.
    const lc = a.live_call;
    if (lc && lc.turn_id !== undefined) {
      liveCalls.set(`${lc.turn_id}|${lc.phase}|${lc.iteration || 0}`, {
        turn_id: lc.turn_id || "",
        phase: lc.phase || "",
        iteration: lc.iteration || 0,
        model: lc.model || "",
        trigger: lc.trigger || {},
        response: lc.response || "",
        rounds: lc.rounds || 0,
        tool_executions: lc.tool_executions || [],
        prompt_messages: lc.prompt_messages || [],
      });
    }
    renderAll();
    // Per-agent phase summary (deferred).
    api.tokens({ sinceDays: phaseWindow, agentRole: a.role }).then((d) => {
      if (d && !d._error) {
        phaseSummary = d;
        const el = root && root.querySelector("#agent-phase");
        if (el) el.innerHTML = renderPhaseSummary();
      }
    });
  }

  // ---- live projection ----
  function isMine(p) {
    const rid = p.agent_id || "";
    const role = p.role || p.agent_role || "";
    return (
      (rid && (rid === data?.runtime_id || rid === liveAgent().runtime_id)) ||
      (role && role === (data?.role || liveAgent().role))
    );
  }

  function onEvent(ev) {
    if (!data || llmSub) return; // the LLM detail sub-view is static
    const p = ev.payload || {};
    if (!isMine(p)) return; // head/task already track store state via update()
    const t = ev.type;
    const k = `${p.turn_id}|${p.phase}|${p.iteration || 0}`;

    // High-frequency path: a streamed round updates only the live row.
    if (t === "agent_turn_progress") {
      // Keep the source across the round-by-round overwrites: prefer the
      // round's own trigger, else the placeholder seeded by phase_started.
      const prev = liveCalls.get(k);
      liveCalls.set(k, {
        turn_id: p.turn_id || "",
        phase: p.phase || "",
        iteration: p.iteration || 0,
        model: p.model || "",
        trigger: p.trigger || (prev && prev.trigger) || {},
        response: p.response || "",
        rounds: (p.round_num || 0) + 1,
        tool_executions: p.tool_executions || [],
        prompt_messages: p.prompt_messages || [],
      });
      patchLiveRow(k);
      return;
    }

    // Structural changes (infrequent) → re-render, expansion preserved.
    if (t === "agent_phase_started") {
      liveCalls.set(k, {
        turn_id: p.turn_id || "",
        phase: p.phase || "",
        iteration: p.iteration || 0,
        model: "",
        trigger: p.trigger || {},
        response: "",
        rounds: 0,
        tool_executions: [],
      });
    } else if (t === "agent_phase_completed") {
      unshiftRecord(fromPhase(p, ev.timestamp));
      liveCalls.delete(k);
    } else if (t === "agent_turn_completed") {
      for (const key of [...liveCalls.keys()])
        if (key.startsWith((p.turn_id || "") + "|")) liveCalls.delete(key);
      reloadPhaseSummary();
    } else if (t === "task_completed" || t === "task_failed") {
      liveCalls.clear();
    } else {
      return; // not an LLM-section event
    }
    fullRenderLlm();
  }

  return {
    mount(el) {
      root = el;
      root.innerHTML = '<div class="skel skel-row" style="margin:24px 0"></div>';
      load();
    },
    onAction(action, target) {
      if (action === "toggle-turn") {
        const tn = target.dataset.turn;
        const card = target.closest(".turn");
        const open = card.classList.toggle("open");
        if (open) {
          openTurns.add(tn);
          userCollapsed.delete(tn);
        } else {
          openTurns.delete(tn);
          userCollapsed.add(tn);
        }
      } else if (action === "toggle-row") {
        const card = target.closest(".llm-row");
        const key = target.dataset.key;
        const open = card.classList.toggle("open");
        const live = key.startsWith("live|");
        if (live) open ? collapsedLive.delete(key) : collapsedLive.add(key);
        else open ? openRows.add(key) : openRows.delete(key);
      } else if (action === "toggle-think") {
        const open = target.closest(".think").classList.toggle("open");
        const k = target.dataset.tkey;
        open ? collapsedThinks.delete(k) : collapsedThinks.add(k);
      } else if (action === "toggle-tool") {
        const k = target.dataset.tkey;
        const sel = window.CSS && CSS.escape ? CSS.escape(k) : k;
        const detail = root.querySelector(`[data-tool-key="${sel}"]`);
        if (detail) {
          const open = detail.classList.toggle("open");
          open ? openTools.add(k) : openTools.delete(k);
        }
      } else if (action === "toggle-prompt") {
        const key = target.dataset.pkey;
        const open = target.closest(".msg-block").classList.toggle("open");
        if (open) {
          openPrompts.add(key);
          collapsedPrompts.delete(key);
        } else {
          collapsedPrompts.add(key);
          openPrompts.delete(key);
        }
      } else if (action === "toggle-mem") {
        target.closest(".mem-card").classList.toggle("open");
      } else if (action === "toggle-clamp") {
        const txt = target.previousElementSibling;
        if (txt && txt.classList.contains("mem-text")) {
          const expanded = txt.classList.toggle("expanded");
          target.textContent = expanded ? "Show less" : "Show more";
        }
      } else if (action === "open-event") {
        const eid = target.dataset.eventId;
        if (eid) navigate("/events/" + encodeURIComponent(eid));
      } else if (action === "open-llm") {
        const d = target.dataset;
        navigate(
          `/agents/${encodeURIComponent(id)}/llm/${encodeURIComponent(d.turn)}/${encodeURIComponent(d.phase)}/${encodeURIComponent(d.iter)}`,
        );
      } else if (action === "back-agent" || action === "nav-dashboard") {
        navigate(
          action === "back-agent" ? "/agents/" + encodeURIComponent(id) : "/dashboard",
        );
      } else if (action === "copy-turn") {
        const g = groupTurns().find((x) => x.turn_id === target.dataset.turn);
        copyToClipboard(JSON.stringify(g ? g.items : [], null, 2)).then(() =>
          toast("Turn copied"),
        );
      } else if (action === "copy-record") {
        copyToClipboard(JSON.stringify(findRecord() || {}, null, 2)).then(() =>
          toast("Copied"),
        );
      }
    },
    update() {
      // Head/task reflect store-projected live state.
      if (root && !llmSub && root.querySelector("#agent-head")) {
        root.querySelector("#agent-head").innerHTML = renderHead();
        root.querySelector("#agent-task").innerHTML = renderTask();
      }
    },
    onEvent,
    destroy() {
      root = null;
    },
  };
}

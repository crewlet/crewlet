// Agent detail: live state, current task, per-phase token summary, durable
// memory, and the LLM invocation history (with the in-flight call) last.
//
// Two things changed here and they are related. The in-flight call is no
// longer reconstructed from the raw event stream — the server projects it
// and pushes it, so this view reads `agent.live_call` and renders it, and
// a mid-call refresh shows the same row it showed a second ago. And every
// render returns markup that the shell patches in, so a streaming round
// updates the row that changed instead of rebuilding the page around it.

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
  failureOf,
  failureBlock,
  failureLabel,
  tokenStats,
  triggerSource,
} from "../llm.js";
import { recordFromEvent, recordFromLiveCall } from "../records.js";

// Longest LLM history this page keeps in memory. The server returns 50
// and live phase completions push onto the front; 200 is several hours
// of a busy agent and bounds a tab left open overnight.
const MAX_RECORDS = 200;

export function createAgentView({ store, query, navigate, refresh, params }) {
  const id = params.id;
  const llmSub = params.turn
    ? { turn: params.turn, phase: params.phase, iter: String(params.iter || "0") }
    : null;

  let data = null; // the `agent` query's reply
  let memory = null;
  let phaseSummary = null;
  let llm = []; // history records, newest first
  let loading = true;
  let loadError = "";
  const phaseWindow = 7;

  // Expansion state.
  //
  // One override map, not a pair of "opened" / "collapsed" sets. Things
  // here have different DEFAULTS — a live row opens itself, a completed
  // one does not, a long prompt collapses and a short one does not — and
  // tracking that with two sets means every toggle has to reason about
  // which set the default put it in. That is how the first click on a
  // collapsed turn ended up doing nothing and every second click after
  // it got eaten. An override is simply "the reader disagreed with the
  // default", so a toggle is always `set(key, !isOpen(key))`.
  const overrides = new Map(); // key → boolean
  const isOpen = (key, byDefault) =>
    overrides.has(key) ? overrides.get(key) : byDefault;
  const toggle = (key, byDefault) => overrides.set(key, !isOpen(key, byDefault));

  const isThinkOpen = (k) => isOpen(`think:${k}`, true);
  const isToolOpen = (k) => isOpen(`tool:${k}`, false);
  // Long prompt messages collapse by default, short ones stay open.
  const isPromptOpen = (k, long) => isOpen(`prompt:${k}`, !long);

  function liveAgent() {
    return store.agentById(id) || data || {};
  }

  function recKey(r) {
    return `${r.turn_id}|${r.phase}|${r.iteration}|${r.timestamp}`;
  }

  function unshiftRecord(r) {
    const key = recKey(r);
    if (!llm.some((x) => recKey(x) === key)) llm.unshift(r);
    if (llm.length > MAX_RECORDS) llm.length = MAX_RECORDS;
  }

  // ---- grouping ----
  function groupTurns() {
    const groups = new Map();
    for (const r of llm) {
      if (!groups.has(r.turn_id)) groups.set(r.turn_id, { items: [], live: [] });
      groups.get(r.turn_id).items.push(r);
    }
    const live = recordFromLiveCall(liveAgent().live_call);
    // A phase that finished publishes a history record, and the
    // projection keeps a frozen copy of the same call when it failed —
    // rendering both would show one phase twice, the durable record
    // beside its own snapshot. The record wins; it is the fuller one.
    const alreadyRecorded =
      live &&
      llm.some(
        (r) =>
          r.turn_id === live.turn_id &&
          r.phase === live.phase &&
          (r.iteration || 0) === (live.iteration || 0),
      );
    if (live && !alreadyRecorded) {
      const tid = live.turn_id || "live";
      if (!groups.has(tid)) groups.set(tid, { items: [], live: [] });
      groups.get(tid).live.push(live);
    }

    const mkGroup = (gid, items, liveRows, trigger, isOnboarding) => {
      const all = [...items, ...liveRows];
      const stamps = all.map((x) => x.timestamp).filter(Boolean).sort();
      return {
        turn_id: gid,
        baseTurnId: gid.split("#")[0],
        items,
        live: liveRows,
        trigger,
        isOnboarding,
        firstTs: stamps[0] || "",
        lastTs: stamps[stamps.length - 1] || "",
        isLive: liveRows.some((r) => r._live),
        failed: all.some((r) => r.failed || r._failed),
      };
    };

    const arr = [...groups.entries()].flatMap(([turn_id, g]) => {
      // Drop the whole-turn aggregate when individual phase rows exist —
      // unless the turn FAILED, because the aggregate is the only row
      // carrying the turn-level reason it stopped. A mid-turn failure
      // (Plan fine, Execute died) used to render the Plan card and
      // nothing else, so the turn simply looked like it ended.
      const hasPhases = g.items.some((r) => r.phase && r.phase !== "turn");
      const items = hasPhases
        ? g.items.filter((r) => r.phase !== "turn" || r.failed)
        : g.items;
      // Read top-to-bottom in execution order: oldest first, with the
      // phase order (onboarding → plan → execute → review) breaking ties.
      items.sort((a, b) => {
        if (a.timestamp !== b.timestamp) return a.timestamp < b.timestamp ? -1 : 1;
        return (PHASE_ORDER[a.phase] || 50) - (PHASE_ORDER[b.phase] || 50);
      });
      // The trigger is a turn-level property — every phase carries the
      // same descriptor; take the first non-empty one.
      const trigger =
        [...items, ...g.live]
          .map((x) => x.trigger)
          .find((tr) => tr && (tr.type || tr.id)) || {};
      // Onboarding is one-time first-turn SETUP — it rides whichever turn
      // the trigger happened to wake. Split it into its own group (no
      // trigger chip) so it doesn't read as part of the task. The
      // "#onboarding" suffix keeps the group key distinct while the rows
      // keep the real turn_id underneath.
      const onboarding = items.filter((r) => r.phase === "onboarding");
      const rest = items.filter((r) => r.phase !== "onboarding");
      const out = [];
      if (rest.length || g.live.length || !onboarding.length) {
        out.push(mkGroup(turn_id, rest, g.live, trigger, false));
      }
      if (onboarding.length) {
        out.push(mkGroup(`${turn_id}#onboarding`, onboarding, [], {}, true));
      }
      return out;
    });
    // Most-recent turn first; phases within a turn stay chronological.
    arr.sort((a, b) => (a.lastTs < b.lastTs ? 1 : -1));
    return arr;
  }

  // ---- header ----
  function renderHead() {
    const a = liveAgent();
    const st = effectiveAgentState(a, store.state.sandboxes);
    const failure = a.last_error;
    const quip =
      a.state === "afk"
        ? `<div class="afk-quip" style="margin-top:6px">${esc(afkQuip(a.afk_reason))}</div>`
        : "";
    return `
      <div class="card agent-head" data-k="head">
        ${avatarFor(a.role || data?.role)}
        <div class="agent-head-id">
          <div class="agent-head-name">${esc(a.role || data?.role || "Agent")}</div>
          <div class="row-sub"><span class="mono">${esc(shortId(a.runtime_id || data?.id, 14))}</span> · @${esc(data?.handle || "")}</div>
          ${quip}
        </div>
        <div class="agent-head-state">
          <span class="badge ${stateBadgeClass(st)}"><i class="dot"></i>${esc(stateLabel(st))}${esc(phaseSuffix(a))}</span>
          <div class="tok-chip">
            <span class="in">${fmtNum(a.input_tokens || 0)}</span> in ·
            <span class="out">${fmtNum(a.output_tokens || 0)}</span> out ·
            <span class="tot">${fmtNum(a.total_tokens || 0)}</span></div>
        </div>
      </div>
      ${failure ? renderFailureCard(failure) : ""}`;
  }

  // The seat-level "why it stopped" card. The engine knows precisely why
  // a turn died; before this it reached no screen at all.
  function renderFailureCard(failure) {
    return `
      <div class="card failure-card" data-k="failure">
        ${icon("alert", "sm")}
        <div class="failure-card-body">
          <div class="failure-card-title">${esc(failureLabel(failure.kind))}
            ${failure.phase ? `<span class="ph-pill" style="color:${phaseInk(failure.phase)}">${esc(failure.phase)}</span>` : ""}
          </div>
          ${failure.message ? `<pre class="failure-msg">${esc(failure.message)}</pre>` : ""}
        </div>
        <span class="row-ts">${esc(relTime(failure.at))}</span>
      </div>`;
  }

  function renderTask() {
    const a = liveAgent();
    if (!a.current_task) return "";
    return `
      <div class="card current-task" data-k="task">
        <span class="dot working"></span>
        <div style="flex:1"><b>Current task</b> · ${esc(a.current_task)}</div>
        <span class="badge live"><i class="dot"></i>in progress</span>
      </div>`;
  }

  function renderPhaseSummary() {
    if (!phaseSummary || !phaseSummary.by_phase || !phaseSummary.by_phase.length) {
      return "";
    }
    const t = phaseSummary.totals;
    return `
      <div class="card phase-summary" data-k="phases">
        <div class="row-sub" style="margin-bottom:8px">
          ${icon("zap", "sm")} last ${phaseWindow}d · ${fmtNum(t.total_tokens)} tokens ·
          ${fmtNum(t.input_tokens)} in / ${fmtNum(t.output_tokens)} out · ${fmtNum(t.calls)} calls
        </div>
        ${phaseBar(phaseSummary.by_phase, t.total_tokens)}
      </div>`;
  }

  // ---- LLM rows ----

  // A live row is open by default so you watch it stream, and a FAILED
  // row opens itself because the reason is the thing worth reading.
  // Everything else starts closed. The reader's own toggle always wins.
  function rowDefaultOpen(r) {
    return !!(r._live || r.failed || r._failed);
  }
  function rowIsOpen(key, r) {
    return isOpen(`row:${key}`, rowDefaultOpen(r));
  }

  function rowPreview(r) {
    const failure = failureOf(r);
    if (failure) return failure.message || failureLabel(failure.kind);
    if (r._live) {
      return r.response ? trunc(r.response, 90) : "Working — response streams in…";
    }
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
        failure: failureOf(r),
        inProgress: !!r._live,
      })}`;
  }

  // Sandbox Execute badge: coding agent + cost + PR link. Renders only on
  // a sandbox-backed Execute phase; native phases are bare.
  function sandboxBadge(r) {
    if (r.backend !== "sandbox") return "";
    const cost = r.cost_usd ? ` · $${Number(r.cost_usd).toFixed(2)}` : "";
    const ref = (r.delivered_refs || [])[0];
    const pr = ref
      ? ` · <a href="${escAttr(ref)}" target="_blank" rel="noopener" data-action="stop">PR</a>`
      : "";
    return `<span class="chip sandbox" title="Sandboxed Execute (${escAttr(r.coding_agent || "coding agent")})">⛁ ${esc(r.coding_agent || "sandbox")}${cost}${pr}</span>`;
  }

  // The round-budget pips: one dot per completed tool-call round. On a
  // live row they read as progress; on a finished one, as how much of
  // the budget the phase actually needed.
  function rounds(r) {
    const used = r.rounds || r.rounds_used || 0;
    if (!used) return "";
    const capped = Math.min(used, 12);
    const pips = Array.from(
      { length: capped },
      (_, i) => `<i class="pip" style="--i:${i}"></i>`,
    ).join("");
    return `<span class="pips ${r._live ? "is-live" : ""}" title="${used} round${used === 1 ? "" : "s"}">${pips}</span>`;
  }

  function phaseRow(r, key) {
    const failed = !!failureOf(r);
    const open = rowIsOpen(key, r);
    const tail = r._live
      ? `<span class="badge live"><i class="dot"></i>live · round ${r.rounds || 0}</span>`
      : failed
        ? `<span class="badge failed"><i class="dot"></i>failed</span>`
        : `<button class="btn sm" data-action="open-llm" data-turn="${escAttr(r.turn_id)}" data-phase="${escAttr(r.phase)}" data-iter="${escAttr(String(r.iteration))}">Details</button>`;
    return `
      <div class="llm-row ${open ? "open" : ""} ${r._live ? "is-live" : ""} ${failed ? "is-failed" : ""}"
           data-k="row:${escAttr(key)}" data-rowkey="${escAttr(key)}"
           style="--phase-color:${failed ? "var(--red)" : phaseColor(r.phase)}">
        <div class="llm-row-head" data-action="toggle-row" data-key="${escAttr(key)}">
          ${icon("chevron", "chevron")}
          <span class="ph-pill" style="color:${failed ? "var(--red-ink)" : phaseInk(r.phase)}">${esc(r.phase)}${r.iteration ? " #" + r.iteration : ""}</span>
          ${r.worker ? `<span class="chip">${esc(r.worker)}</span>` : ""}
          ${sandboxBadge(r)}
          <span class="llm-model">${esc(r.model || "—")}</span>
          ${rounds(r)}
          <span class="llm-preview">${esc(rowPreview(r))}</span>
          ${tail}
        </div>
        <div class="llm-row-body">${open ? rowBody(r, key) : ""}</div>
      </div>`;
  }

  // A live turn opens itself, and so does the NEWEST failed one — that
  // is the turn an operator came to read. Older failures stay collapsed;
  // auto-opening every one buries the page in transcripts of the same
  // outage. `newestFailed` is passed in rather than looked up, so the
  // rule costs one pass over the groups instead of one per group.
  function turnDefaultOpen(group, newestFailed) {
    if (!group) return false;
    return !!(group.isLive || group === newestFailed);
  }

  function renderTurns() {
    const groups = groupTurns();
    if (!groups.length) {
      return loading
        ? '<div class="skel skel-row"></div>'
        : empty("cpu", "No LLM invocations yet");
    }
    const newestFailed = groups.find((g) => g.failed);
    return groups
      .map((g) => {
        const autoOpen = isOpen(
          `turn:${g.turn_id}`,
          turnDefaultOpen(g, newestFailed),
        );
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
            phaseRow(lc, `live|${lc.turn_id}|${lc.phase}|${lc.iteration}`),
          )
          .join("");
        const rows = g.items.map((r) => phaseRow(r, recKey(r))).join("");

        // Onboarding groups get a distinct "setup" label and no trigger
        // chip; task turns keep the "turn <id>" label and the source.
        const label = g.isOnboarding
          ? `${icon("book", "sm")}<span class="turn-id">onboarding</span><span class="row-sub mono">${esc(shortId(g.baseTurnId))}</span>`
          : `<span class="turn-id">turn ${esc(shortId(g.turn_id))}</span>`;
        return `
          <div class="turn ${autoOpen ? "open" : ""} ${g.isLive ? "live" : ""} ${g.failed ? "failed" : ""} ${g.isOnboarding ? "onboarding" : ""}"
               data-k="turn:${escAttr(g.turn_id)}" data-turn="${escAttr(g.turn_id)}">
            <div class="turn-head" data-action="toggle-turn" data-turn="${escAttr(g.turn_id)}">
              ${icon("chevron", "chevron")}
              <span class="row-ts">${esc(fmtTime(g.firstTs))}</span>
              ${label}
              <span class="turn-pills">${pills}</span>
              ${g.isOnboarding ? "" : triggerSource(g.trigger, { compact: true })}
              <span style="flex:1"></span>
              ${g.failed ? '<span class="badge failed"><i class="dot"></i>failed</span>' : ""}
              ${g.isLive ? '<span class="badge live"><i class="dot"></i>live</span>' : ""}
              <span class="tok-chip"><span class="tot">${fmtNum(total)}</span></span>
              <button class="btn sm" data-action="copy-turn" data-turn="${escAttr(g.turn_id)}">${icon("copy", "sm")}</button>
            </div>
            <div class="turn-body">${autoOpen ? rows + liveRows : ""}</div>
          </div>`;
      })
      .join("");
  }

  // ---- memory ----
  function memText(raw, key, threshold = 280) {
    const long = (raw || "").length > threshold;
    const expanded = isOpen(`text:${key}`, false);
    return `<div class="mem-text ${long && !expanded ? "clamped" : ""} ${expanded ? "expanded" : ""}">${mdLite(raw)}</div>${
      long
        ? `<span class="mem-more" data-action="toggle-clamp" data-ckey="${escAttr(key)}">${expanded ? "Show less" : "Show more"}</span>`
        : ""
    }`;
  }

  function renderMemory() {
    if (!memory) return "";
    const m = memory;
    const long = m.personal_memories?.long || [];
    const short = m.personal_memories?.short || [];
    const total =
      long.length +
      short.length +
      (m.episodes || []).length +
      (m.counterparty_profiles || []).length +
      (m.synthesized_skills || []).length;

    const block = (key, color, iconId, title, count, body) => `
      <div class="mem-card ${isOpen(`mem:${key}`, false) ? "open" : ""}" data-k="mem:${key}" data-mem="${key}">
        <div class="mem-head" data-action="toggle-mem" data-mem="${key}" style="color:${color}">
          <span class="mem-icon">${icon(iconId, "sm")}</span>
          <div style="flex:1;color:var(--text)"><div class="mem-title">${esc(title)}</div></div>
          <span class="badge">${count}</span>
          ${icon("chevron", "chevron")}
        </div>
        <div class="mem-body">${count ? body : '<div class="mem-entry"><span class="empty-sub">None yet</span></div>'}</div>
      </div>`;

    const memEntries = (entries, kind) =>
      entries
        .map(
          (e) => `
        <div class="mem-entry ${e.expired ? "expired" : ""}" data-k="m:${escAttr(e.id)}">
          <div class="mem-entry-meta">
            <span class="mono">${esc(shortId(e.id, 10))}</span>
            <span class="badge ${kind === "short" ? "info" : "purple"}">${kind}</span>
            ${e.ttl_until ? `<span>${e.expired ? `expired ${esc(relTime(e.ttl_until))}` : `expires ${esc(untilTime(e.ttl_until))}`}</span>` : ""}
          </div>
          ${memText(e.content, `mem:${e.id}`)}
        </div>`,
        )
        .join("");

    const episodes = (m.episodes || [])
      .map((e) => {
        const outcome =
          { done: "var(--green-ink)", self_iterate: "var(--amber-ink)" }[
            e.review_outcome
          ] || "var(--text-muted)";
        return `
        <div class="mem-entry" data-k="ep:${escAttr(e.id)}">
          <div class="mem-entry-meta">
            <span style="color:${outcome};font-weight:600">${esc(e.review_outcome || "—")}</span>
            <span>${(e.duration_ms / 1000).toFixed(1)}s</span>
            ${e.task_id ? `<span class="mono">${esc(e.task_id)}</span>` : ""}
            <span>${esc(relTime(e.ended_at))}</span>
          </div>
          ${e.task_summary ? `<div class="mem-label">Task</div>${memText(e.task_summary, `ep-task:${e.id}`)}` : ""}
          ${e.plan_summary && e.plan_summary !== e.task_summary ? `<div class="mem-label">Plan</div>${memText(e.plan_summary, `ep-plan:${e.id}`)}` : ""}
          ${(e.tool_sequence || []).length ? `<div class="mem-label">Tools</div><div class="chip-row">${e.tool_sequence.slice(0, 8).map((t) => `<span class="chip">${esc(t)}</span>`).join("")}</div>` : ""}
        </div>`;
      })
      .join("");

    const counterparties = (m.counterparty_profiles || [])
      .map(
        (c) => `
        <div class="mem-entry" data-k="cp:${escAttr(c.subject_label)}">
          <div class="mem-entry-meta">
            <b style="color:var(--text)">${esc(c.subject_label)}</b>
            ${c.subject_handle ? `<span class="mono">@${esc(c.subject_handle)}</span>` : ""}
            ${c.subject_platform ? `<span>${esc(c.subject_platform)}</span>` : ""}
            <span>${c.interaction_count} interactions</span>
            <span>${esc(relTime(c.last_updated_at))}</span>
          </div>
          ${
            Object.keys(c.traits || {}).length
              ? `<div class="mem-entry-content">${Object.entries(c.traits)
                  .map(
                    ([k, v]) =>
                      `<div><b>${esc(k)}:</b> ${esc(typeof v === "string" ? v : JSON.stringify(v))}</div>`,
                  )
                  .join("")}</div>`
              : ""
          }
        </div>`,
      )
      .join("");

    const skills = (m.synthesized_skills || [])
      .map(
        (s) => `
        <div class="mem-entry" data-k="sk:${escAttr(s.id)}">
          <div class="mem-entry-meta">
            <b style="color:var(--text)">${esc(s.name)}</b>
            <span class="badge">v${s.version}</span>
            <span>${esc(relTime(s.updated_at))}</span>
          </div>
          ${memText(s.description, `sk:${s.id}`)}
          ${(s.tool_sequence || []).length ? `<div class="chip-row" style="margin-top:6px">${s.tool_sequence.slice(0, 6).map((t) => `<span class="chip">${esc(t)}</span>`).join("")}</div>` : ""}
        </div>`,
      )
      .join("");

    return `
      <div class="sec"><span class="sec-title">${icon("database", "sm")} Memory <span class="sec-count">${total}</span></span></div>
      ${block("long", "var(--purple-ink)", "database", "Long-term memory", long.length, memEntries(long, "long"))}
      ${block("short", "var(--cyan-ink)", "clock", "Short-term memory", short.length, memEntries(short, "short"))}
      ${block("episodes", "var(--blue-ink)", "activity", "Episodes", (m.episodes || []).length, episodes)}
      ${block("cp", "var(--green-ink)", "users", "Counterparty profiles", (m.counterparty_profiles || []).length, counterparties)}
      ${block("skills", "var(--amber-ink)", "book", "Synthesized skills", (m.synthesized_skills || []).length, skills)}`;
  }

  // ---- LLM detail sub-panel ----
  function findRecord() {
    const match = (r) =>
      r.turn_id === llmSub.turn &&
      r.phase === llmSub.phase &&
      String(r.iteration) === llmSub.iter;
    const live = recordFromLiveCall(liveAgent().live_call);
    return llm.find(match) || (live && match(live) ? live : null);
  }

  function renderLlmDetail() {
    const back = `<div class="back-link" data-action="back-agent">${icon("chevron", "sm")} Back to agent</div>`;
    if (loading) return back + '<div class="skel skel-row"></div>';
    const r = findRecord();
    if (!r) {
      return (
        back +
        empty(
          "cpu",
          "LLM record not found",
          "It may have scrolled out of the recent history.",
        )
      );
    }
    const failure = failureOf(r);
    return `
      ${back}
      <div class="card llm-detail ${failure ? "is-failed" : ""}" data-k="detail"
           style="--phase-color:${failure ? "var(--red)" : phaseColor(r.phase)}">
        <div class="llm-detail-head">
          <span class="ph-pill" style="color:${failure ? "var(--red-ink)" : phaseInk(r.phase)}">${esc(r.phase)}${r.iteration ? " #" + r.iteration : ""}</span>
          <span class="llm-model">${esc(r.model || "—")}</span>
          <span class="row-ts">${esc(fmtTime(r.timestamp))}</span>
          <span style="flex:1"></span>
          <button class="btn sm" data-action="copy-record">${icon("copy", "sm")} Copy</button>
        </div>
        ${failure ? failureBlock(failure) : ""}
        ${tokenStats(r)}
        ${triggerSource(r.trigger)}
        ${toolsAvailable(r)}
        <div class="block-label">Prompt</div>${promptSections(r, {
          keyPrefix: "detail|" + recKey(r),
          isOpen: isPromptOpen,
        })}
        <div class="block-label">Response</div>${responseBody(r.response, r.tool_executions, {
          keyPrefix: "detail|" + recKey(r),
          thinkOpen: isThinkOpen,
          toolOpen: isToolOpen,
          codingAgent: isCodingAgentPhase(r),
          model: r.model,
          failure,
          inProgress: !!r._live,
        })}
      </div>`;
  }

  // ---- data loading ----
  async function load() {
    try {
      data = await query("agent", { id });
    } catch (err) {
      loading = false;
      loadError = err.message;
      refresh();
      return;
    }
    llm = (data.llm_history || []).slice();
    loading = false;
    refresh();

    // The heavier reads follow the first paint rather than gating it.
    query("agent_memory", { id })
      .then((m) => {
        memory = m;
        refresh();
      })
      .catch(() => {});
    query("tokens", { since_days: phaseWindow, agent_role: data.role })
      .then((t) => {
        phaseSummary = t;
        refresh();
      })
      .catch(() => {});
  }

  function isMine(payload) {
    const rid = payload.agent_id || "";
    const role = payload.role || payload.agent_role || "";
    return (
      (rid && (rid === data?.runtime_id || rid === liveAgent().runtime_id)) ||
      (role && role === (data?.role || liveAgent().role))
    );
  }

  return {
    // The in-flight call and every header field live on the agent
    // projection, so an `agents` push is all this view needs to redraw a
    // streaming round. No per-event bookkeeping, no refetch.
    slices: ["agents", "sandboxes"],

    mount() {
      load();
    },

    render(state) {
      if (loadError) {
        return empty(
          "user",
          loadError === "not_found" ? "Agent not found" : "Could not load agent",
          loadError === "offline" ? "Reconnecting…" : "",
        );
      }
      if (llmSub) return renderLlmDetail();
      return `
        <div class="back-link" data-action="nav-agents">${icon("chevron", "sm")} Agents</div>
        ${renderHead()}
        ${renderTask()}
        ${renderPhaseSummary()}
        ${renderMemory()}
        <div class="sec"><span class="sec-title">${icon("cpu", "sm")} LLM Invocations <span class="sec-count">${llm.length}</span></span></div>
        <div class="turns">${renderTurns()}</div>`;
    },

    // A completed phase becomes a history row the moment it lands, so the
    // list grows without a refetch. Everything else about the row — the
    // live call, the tokens, the state — arrives on the agent push.
    onEvent(ev) {
      if (!data || llmSub) return;
      if (!isMine(ev.payload || {})) return;
      const record = recordFromEvent(ev);
      if (!record) return;
      unshiftRecord(record);
      refresh();
    },

    onAction(action, target) {
      if (action === "toggle-turn") {
        // Read the rendered state rather than re-deriving the default:
        // it is what the reader is looking at, and it cannot disagree.
        const turnId = target.dataset.turn;
        const card = target.closest(".turn");
        overrides.set(`turn:${turnId}`, !(card && card.classList.contains("open")));
      } else if (action === "toggle-row") {
        // The row's default depends on what kind of row it is, so read
        // it back off the rendered element rather than re-deriving it.
        const key = target.dataset.key;
        const row = target.closest(".llm-row");
        const wasOpen = !!row && row.classList.contains("open");
        overrides.set(`row:${key}`, !wasOpen);
      } else if (action === "toggle-think") {
        toggle(`think:${target.dataset.tkey}`, true);
      } else if (action === "toggle-tool") {
        toggle(`tool:${target.dataset.tkey}`, false);
      } else if (action === "toggle-prompt") {
        const key = target.dataset.pkey;
        const block = target.closest(".msg-block");
        overrides.set(
          `prompt:${key}`,
          !(block && block.classList.contains("open")),
        );
      } else if (action === "toggle-mem") {
        toggle(`mem:${target.dataset.mem}`, false);
      } else if (action === "toggle-clamp") {
        toggle(`text:${target.dataset.ckey}`, false);
      } else if (action === "open-event") {
        const eid = target.dataset.eventId;
        if (eid) navigate("/events/" + encodeURIComponent(eid));
        return;
      } else if (action === "open-llm") {
        const d = target.dataset;
        navigate(
          `/agents/${encodeURIComponent(id)}/llm/${encodeURIComponent(d.turn)}/${encodeURIComponent(d.phase)}/${encodeURIComponent(d.iter)}`,
        );
        return;
      } else if (action === "back-agent") {
        navigate("/agents/" + encodeURIComponent(id));
        return;
      } else if (action === "nav-agents") {
        navigate("/agents");
        return;
      } else if (action === "copy-turn") {
        const g = groupTurns().find((x) => x.turn_id === target.dataset.turn);
        copyToClipboard(JSON.stringify(g ? g.items : [], null, 2)).then(() =>
          toast("Turn copied"),
        );
        return;
      } else if (action === "copy-record") {
        copyToClipboard(JSON.stringify(findRecord() || {}, null, 2)).then(() =>
          toast("Copied"),
        );
        return;
      } else {
        return;
      }
      refresh();
    },
  };
}

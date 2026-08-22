// One seat, addressed by handle, asked four questions.
//
// This was the deepest single scroll in the product: the head card, the
// failure, the current task, the phase rollup, five memory blocks and
// then — below all of it — the LLM invocation history, which is the
// thing the page is actually for. Four unrelated questions stacked in
// one column meant the answer to any of them was somewhere else on the
// page, and the most-read one was last.
//
// So the questions are tabs:
//
//   now      what is it doing, why did it stop, and what has it said
//   memory   what does it carry between turns
//   cost     what has it spent, and what stops it spending more
//   access   what can it reach, and how is it reached
//
// The tab is a query parameter, so a tab is a link. Only the mounted
// tab's data is queried: a reader opening a seat to read its turns does
// not pay for a diary read they never look at.
//
// The seat is addressed by HANDLE — the identity everything else in the
// system uses, and the one an operator can read off a Slack mention.
// `store.agentByKey` resolves a runtime id or a role name too, so links
// minted before the move still land.
//
// Two older properties survive intact and are load-bearing. The
// in-flight call is not reconstructed from the raw event stream — the
// server projects it and pushes it, so this reads `agent.live_call` and
// a mid-call refresh shows the same row it showed a second ago. And
// every render returns markup the shell patches in, so a streaming round
// updates the row that changed instead of rebuilding the page.

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
  CONTACT_FIELDS,
  avatarFor,
  stateBadgeClass,
  phaseSuffix,
  afkQuip,
  phaseColor,
  phaseInk,
  integrationMeta,
  roleInk,
  PHASE_ORDER,
  effectiveAgentState,
  stateLabel,
} from "../state.js";
import { flattenSeats } from "../org.js";
import { replaceParams } from "../router.js";
import {
  empty,
  emptyOrPending,
  phaseBar,
  sectionHead,
  skeletonRows,
  statStrip,
} from "../ui.js";
import { copyToClipboard, toast } from "../dom.js";
import {
  isCodingAgentPhase,
  toolsAvailable,
  promptSections,
  responseBody,
  failureOf,
  failureBlock,
  failureLabel,
  stripThink,
  tokenStats,
  triggerSource,
} from "../llm.js";
import { recordFromEvent, recordFromLiveCall } from "../records.js";

// Longest LLM history this page keeps in memory. The server returns 50
// and live phase completions push onto the front; 200 is several hours
// of a busy agent and bounds a tab left open overnight.
const MAX_RECORDS = 200;

// The window the spend rollup covers. Seven days is long enough that a
// seat which ran yesterday is not reported as costing nothing, and short
// enough that the figure describes the company as it is configured now.
const PHASE_WINDOW = 7;

// The four questions, in the order a reader arrives with them.
const TABS = [
  { key: "now", label: "Now", icon: "activity" },
  { key: "memory", label: "Memory", icon: "database" },
  { key: "cost", label: "Cost", icon: "zap" },
  { key: "access", label: "Access", icon: "key" },
];
const TAB_KEYS = new Set(TABS.map((t) => t.key));
const DEFAULT_TAB = "now";

export function createSeatView({ store, query, navigate, refresh, params = {} }) {
  const key = params.key || "";
  const llmSub = params.turn
    ? { turn: params.turn, phase: params.phase, iter: String(params.iter || "0") }
    : null;

  // The router threads the path — the seat key, and for the llm
  // sub-route the record's coordinates — but not the query string, so
  // the tab is read off the location once here and written back with
  // `replaceParams`. Replace rather than push: a reader who looked at
  // three tabs and pressed Back expects to leave the seat, not to walk
  // back out through them.
  let tab = TAB_KEYS.has(params.tab) ? params.tab : tabFromUrl() || DEFAULT_TAB;

  let data = null; // the `agent` query's reply
  let llm = []; // history records, newest first
  let loading = true;
  let loadError = "";

  let memory = null;
  let memoryError = "";
  let memoryAsked = false;

  let cost = null; // the `tokens` rollup for this seat
  let costError = "";
  let costAsked = false;

  let disposed = false;
  let unwatch = null;

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
  const isOpen = (k, byDefault) => (overrides.has(k) ? overrides.get(k) : byDefault);
  const toggle = (k, byDefault) => overrides.set(k, !isOpen(k, byDefault));

  const isThinkOpen = (k) => isOpen(`think:${k}`, true);
  const isToolOpen = (k) => isOpen(`tool:${k}`, false);
  // Long prompt messages collapse by default, short ones stay open.
  const isPromptOpen = (k, long) => isOpen(`prompt:${k}`, !long);

  // ---- identity ----

  // The seat's row in the pushed projection, or null while the first
  // snapshot is still in flight.
  function seatRow() {
    return (store.agentByKey && store.agentByKey(key)) || null;
  }

  // The id the `agent` / `agent_memory` queries take: the configured
  // seat id, which is what a handle has to be resolved INTO. A cold page
  // load lands on this route before the first snapshot, so this is empty
  // for a moment on every direct link.
  function seatId() {
    const seat = seatRow();
    return (seat && seat.id) || "";
  }

  function liveAgent() {
    return seatRow() || data || {};
  }

  function roleName() {
    return (data && data.role) || liveAgent().role || liveAgent().name || "";
  }

  function seatHandle() {
    return (data && data.handle) || liveAgent().handle || key;
  }

  function seatPath() {
    return `/seats/${encodeURIComponent(key)}`;
  }

  function syncUrl() {
    replaceParams(`seats/${encodeURIComponent(key)}`, {
      tab: tab === DEFAULT_TAB ? "" : tab,
    });
  }

  // ---- data loading ----

  async function loadSeat() {
    const id = seatId();
    if (!id) return;
    try {
      data = await query("agent", { id });
    } catch (err) {
      loading = false;
      loadError = (err && err.message) || "failed";
      if (!disposed) refresh();
      return;
    }
    llm = (data.llm_history || []).slice();
    loading = false;
    if (!disposed) refresh();
  }

  async function loadMemory() {
    const id = seatId();
    if (!id) return;
    memoryAsked = true;
    try {
      memory = await query("agent_memory", { id });
      memoryError = "";
    } catch (err) {
      // Swallowing this is what made a failed diary read look exactly
      // like a seat that remembers nothing: the section rendered as
      // empty markup and said neither.
      memory = null;
      memoryError = (err && err.message) || "failed";
    }
    if (!disposed) refresh();
  }

  async function loadCost() {
    const role = roleName();
    if (!role) return;
    costAsked = true;
    try {
      cost = await query("tokens", { since_days: PHASE_WINDOW, agent_role: role });
      costError = "";
    } catch (err) {
      cost = null;
      costError = (err && err.message) || "failed";
    }
    if (!disposed) refresh();
  }

  // The rollup is read by the head chip (every tab) and by the Cost tab;
  // the diary is read by the Memory tab alone. Nothing else is fetched
  // for a tab nobody opened.
  function ensureTabData() {
    if (tab === "memory" && !memoryAsked) loadMemory();
    if (!costAsked) loadCost();
  }

  function begin() {
    if (!seatId()) return false;
    loadSeat();
    if (!llmSub) ensureTabData();
    return true;
  }

  function dropWatch() {
    if (unwatch) unwatch();
    unwatch = null;
  }

  // ---- records ----

  function recKey(r) {
    return `${r.turn_id}|${r.phase}|${r.iteration}|${r.timestamp}`;
  }

  // The identity a record's expand/collapse state hangs off. A record
  // from the live projection carries its own timestamp-free `_key`
  // (see records.js) because its timestamp moves on every streamed
  // round; a stored record is keyed by `recKey`, whose timestamp is
  // what separates a resumed Execute phase's two records.
  function keyFor(r) {
    return r._key || recKey(r);
  }

  function unshiftRecord(r) {
    const k = recKey(r);
    if (!llm.some((x) => recKey(x) === k)) llm.unshift(r);
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
      const stamps = all
        .map((x) => x.timestamp)
        .filter(Boolean)
        .sort();
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

  // ---- head ----

  function backLink() {
    return `<div class="back-link" data-action="go" data-route="/org?lens=seats"
                 role="link" tabindex="0">${icon("chevron", "sm")} Seats</div>`;
  }

  function renderHead(state) {
    const a = liveAgent();
    const role = roleName() || "Seat";
    const st = effectiveAgentState(a, state.sandboxes);
    const quip =
      a.state === "afk"
        ? `<div class="afk-quip seat-quip">${esc(afkQuip(a.afk_reason))}</div>`
        : "";
    return `
      <div class="card agent-head" data-k="head">
        ${avatarFor(role)}
        <div class="agent-head-id">
          <div class="agent-head-name" style="color:${roleInk(role)}">${esc(role)}</div>
          <div class="row-sub">
            <span class="mono">${esc(shortId(a.runtime_id || (data && data.id), 14))}</span>
            · @${esc(seatHandle())}
          </div>
          ${quip}
        </div>
        <div class="agent-head-state">
          <span class="badge ${stateBadgeClass(st)}"><i class="dot"></i>${esc(stateLabel(st))}${esc(phaseSuffix(a))}</span>
          ${tokenChip(a)}
          ${budgetChip(a)}
          <button class="btn sm" data-action="go"
                  data-route="/activity?actor=${escAttr(encodeURIComponent(roleName()))}"
                  title="This seat's events on Activity">${icon("inbox", "sm")} Events</button>
        </div>
      </div>`;
  }

  // The seat's token usage, from the PHASE rollup when it has loaded.
  //
  // The projection's own per-agent counters (`a.total_tokens`) are fed by
  // `agent_turn_completed`, so they count only turns that reached the
  // end — which meant a seat whose provider kept dying showed "0 in · 0
  // out · 0" in its header directly above a phase card reporting a
  // quarter of a million tokens over the same window. Both numbers were
  // real; only one of them answers "what has this seat spent". Reading
  // the phase rollup here makes the header and the Cost tab two views of
  // one figure rather than two figures.
  function tokenChip(a) {
    const t = cost && cost.totals;
    const [input, output, total] = t
      ? [t.input_tokens, t.output_tokens, t.total_tokens]
      : [a.input_tokens || 0, a.output_tokens || 0, a.total_tokens || 0];
    return `
      <div class="tok-chip" title="last ${PHASE_WINDOW} days">
        <span class="in">${fmtNum(input)}</span> in ·
        <span class="out">${fmtNum(output)}</span> out ·
        <span class="tot">${fmtNum(total)}</span></div>`;
  }

  // The seat's budget, from the engine's live meter.
  //
  // Deliberately a separate figure from the token chip above it, and
  // labelled: the chip is a 7-day phase rollup and this is a meter over
  // the engine's run. Merging them, or dividing one by the other, would
  // produce a percentage wrong by however long the engine has been up.
  function budgetChip(a) {
    const meter = a.budget;
    const cap = capOf(a);
    if (!cap) return "";
    if (!meter) {
      return `<div class="tok-chip" title="no engine is reporting a live meter">
        cap <span class="tot">${fmtNum(cap)}</span></div>`;
    }
    const pct = Math.max(0, Math.min(100, (meter.used / cap) * 100));
    const tone = meter.refused_at ? "over" : pct >= 75 ? "warn" : "";
    return `
      <div class="tok-chip budget ${tone}"
           title="${escAttr(`${fmtNum(meter.used)} of ${fmtNum(cap)} tokens used this engine run`)}">
        <span class="seat-budget-track sm"><i style="width:${pct.toFixed(1)}%"></i></span>
        <span class="tot">${fmtNum(meter.used)}</span> / ${fmtNum(cap)}
        ${meter.refused_at ? '<span class="warn-ink">exhausted</span>' : ""}
      </div>`;
  }

  // The cap, from the live meter when one is reporting and from the
  // configured `token_budget` otherwise. Config is on the wire whether
  // or not an engine is attached, so a cap renders either way.
  function capOf(a) {
    const meter = a.budget;
    if (meter && meter.max) return meter.max;
    return Number((data && data.token_budget) || a.token_budget || 0);
  }

  // ---- tabs ----

  function tabBar() {
    const counts = {
      now: llm.length || null,
      memory: memoryTotal(),
      cost: null,
      access: null,
    };
    const one = (t) => {
      const on = tab === t.key;
      const count = counts[t.key];
      return `
        <span class="seat-tab${on ? " on" : ""}" role="tab" tabindex="0"
              data-k="tab:${t.key}" data-action="seat-tab" data-tab="${t.key}"
              aria-selected="${on ? "true" : "false"}">
          ${icon(t.icon, "sm")}<span class="seat-tab-label">${esc(t.label)}</span>
          ${count == null ? "" : `<span class="seat-tab-count">${esc(String(count))}</span>`}
        </span>`;
    };
    return `<div class="seat-tabs" role="tablist" aria-label="Seat view">${TABS.map(one).join("")}</div>`;
  }

  // ---- tab: now ----

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
    if (!cost || !cost.by_phase || !cost.by_phase.length) return "";
    const t = cost.totals;
    // Totals live in the header chip; repeating them here was the same
    // three numbers twice on one screen. The Cost tab breaks them down.
    return `
      <div class="card phase-summary" data-k="phases">
        <div class="row-sub seat-phase-lede">
          ${icon("zap", "sm")} where it went · last ${PHASE_WINDOW}d · ${fmtNum(t.calls)} calls
        </div>
        ${phaseBar(cost.by_phase, t.total_tokens)}
      </div>`;
  }

  // A live row is open by default so you watch it stream, and a FAILED
  // row opens itself because the reason is the thing worth reading.
  // Everything else starts closed. The reader's own toggle always wins.
  function rowDefaultOpen(r) {
    return !!(r._live || r.failed || r._failed);
  }
  function rowIsOpen(k, r) {
    return isOpen(`row:${k}`, rowDefaultOpen(r));
  }

  function rowPreview(r) {
    const failure = failureOf(r);
    if (failure) return failure.message || failureLabel(failure.kind);
    // A one-line preview has nowhere to draw the reasoning block the
    // body renders, so the markers come off either way — a live row's
    // response carries them now too.
    const text = trunc(stripThink(r.response), 90);
    if (r._live) return text || "Working — response streams in…";
    return text;
  }

  function rowBody(r, k) {
    return `
      ${triggerSource(r.trigger)}
      ${toolsAvailable(r)}
      <div class="block-label">Prompt</div>${promptSections(r, {
        keyPrefix: k,
        isOpen: isPromptOpen,
      })}
      <div class="block-label">Response</div>${responseBody(r.response, r.tool_executions, {
        keyPrefix: k,
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
    const spend = r.cost_usd ? ` · $${Number(r.cost_usd).toFixed(2)}` : "";
    const ref = (r.delivered_refs || [])[0];
    const pr = ref
      ? ` · <a href="${escAttr(ref)}" target="_blank" rel="noopener" data-action="stop">PR</a>`
      : "";
    return `<span class="chip sandbox" title="Sandboxed Execute (${escAttr(r.coding_agent || "coding agent")})">⛁ ${esc(r.coding_agent || "sandbox")}${spend}${pr}</span>`;
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

  function phaseRow(r, k) {
    const failed = !!failureOf(r);
    const open = rowIsOpen(k, r);
    const tail = r._live
      ? `<span class="badge live"><i class="dot"></i>live · round ${r.rounds || 0}</span>`
      : failed
        ? `<span class="badge failed"><i class="dot"></i>failed</span>`
        : `<button class="btn sm" data-action="open-llm" data-turn="${escAttr(r.turn_id)}" data-phase="${escAttr(r.phase)}" data-iter="${escAttr(String(r.iteration))}">Details</button>`;
    return `
      <div class="llm-row ${open ? "open" : ""} ${r._live ? "is-live" : ""} ${failed ? "is-failed" : ""}"
           data-k="row:${escAttr(k)}" data-rowkey="${escAttr(k)}"
           style="--phase-color:${failed ? "var(--red)" : phaseColor(r.phase)}">
        <div class="llm-row-head" data-action="toggle-row" data-key="${escAttr(k)}">
          ${icon("chevron", "chevron")}
          <span class="ph-pill" style="color:${failed ? "var(--red-ink)" : phaseInk(r.phase)}">${esc(r.phase)}${r.iteration ? " #" + r.iteration : ""}</span>
          ${r.worker ? `<span class="chip">${esc(r.worker)}</span>` : ""}
          ${sandboxBadge(r)}
          <span class="llm-model">${esc(r.model || "—")}</span>
          ${rounds(r)}
          <span class="llm-preview">${esc(rowPreview(r))}</span>
          ${tail}
        </div>
        <div class="llm-row-body">${open ? rowBody(r, k) : ""}</div>
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

  function renderTurns(state) {
    const groups = groupTurns();
    if (!groups.length) {
      if (loading) return skeletonRows(3);
      return emptyOrPending(
        state,
        () => skeletonRows(3),
        "cpu",
        "No LLM invocations yet",
        "Nothing has woken this seat, or its history is older than the event store keeps.",
      );
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

        const liveRows = g.live.map((lc) => phaseRow(lc, keyFor(lc))).join("");
        const rows = g.items.map((r) => phaseRow(r, keyFor(r))).join("");

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

  function nowPanel(state) {
    const a = liveAgent();
    return `
      ${a.last_error ? renderFailureCard(a.last_error) : ""}
      ${renderTask()}
      ${renderPhaseSummary()}
      ${sectionHead("cpu", "LLM invocations", llm.length, {
        action: "go",
        label: "Events",
        route: `/activity?actor=${encodeURIComponent(roleName())}`,
      })}
      <div class="turns">${renderTurns(state)}</div>`;
  }

  // ---- tab: memory ----

  function memoryTotal() {
    if (!memory) return null;
    const m = memory;
    return (
      (m.personal_memories?.long || []).length +
      (m.personal_memories?.short || []).length +
      (m.episodes || []).length +
      (m.counterparty_profiles || []).length +
      (m.synthesized_skills || []).length
    );
  }

  function memText(raw, k, threshold = 280) {
    const long = (raw || "").length > threshold;
    const expanded = isOpen(`text:${k}`, false);
    return `<div class="mem-text ${long && !expanded ? "clamped" : ""} ${expanded ? "expanded" : ""}">${mdLite(raw)}</div>${
      long
        ? `<span class="mem-more" data-action="toggle-clamp" data-ckey="${escAttr(k)}">${expanded ? "Show less" : "Show more"}</span>`
        : ""
    }`;
  }

  function memoryPanel() {
    if (memoryError) {
      return empty(
        "database",
        "Could not read this seat's memory",
        memoryError === "not_found"
          ? "The engine does not know this seat."
          : "The query failed. This says nothing about what the seat remembers — only that it could not be read.",
      );
    }
    if (!memory) return skeletonRows(4);
    const m = memory;
    const long = m.personal_memories?.long || [];
    const short = m.personal_memories?.short || [];

    const block = (k, color, iconId, title, count, body) => `
      <div class="mem-card ${isOpen(`mem:${k}`, false) ? "open" : ""}" data-k="mem:${k}" data-mem="${k}">
        <div class="mem-head" data-action="toggle-mem" data-mem="${k}" style="color:${color}">
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
      ${sectionHead("database", "Memory", memoryTotal(), null)}
      ${block("long", "var(--purple-ink)", "database", "Long-term memory", long.length, memEntries(long, "long"))}
      ${block("short", "var(--cyan-ink)", "clock", "Short-term memory", short.length, memEntries(short, "short"))}
      ${block("episodes", "var(--blue-ink)", "activity", "Episodes", (m.episodes || []).length, episodes)}
      ${block("cp", "var(--green-ink)", "users", "Counterparty profiles", (m.counterparty_profiles || []).length, counterparties)}
      ${block("skills", "var(--amber-ink)", "book", "Synthesized skills", (m.synthesized_skills || []).length, skills)}
      <div class="seat-note">Memory is durable and per seat: the diary and its episodes live in PostgreSQL, keyed by this agent's runtime id. A deployment without a database carries none of it.</div>`;
  }

  // ---- tab: cost ----

  /**
   * The seat's budget, in whichever of three states is true.
   *
   * The bar is drawn ONLY from the engine's live meter, because that is
   * the only figure measured over the same span as the cap: both are
   * the engine run. The 7-day rollup below covers a different span, and
   * dividing it into the cap would produce a percentage wrong by however
   * long the engine has been up.
   *
   * "Exhausted" keys off the engine's own refusal timestamp rather than
   * off `used >= max`: a refused charge increments nothing, so the
   * counter stops short of the cap by the size of the round that would
   * not fit, and a ratio test is false for a seat that is fully blocked.
   */
  function budgetMeter() {
    const a = liveAgent();
    const meter = a.budget;
    const cap = capOf(a);
    if (!cap) {
      return `<div class="seat-note" data-k="budget">No <code>token_budget</code> is set on this role, so nothing caps this seat on its own. Any org-wide budget still applies — it is on Spend &amp; Budgets.</div>`;
    }
    if (!meter) {
      return `
        <div class="card seat-meter is-unmetered" data-k="budget">
          <div class="seat-meter-fig"><span class="num">${fmtNum(cap)}</span>
            <span class="seat-meter-of">token cap</span></div>
          <div class="seat-meter-note">No engine is reporting a live meter for this seat, so how much of the cap has been spent cannot be shown. The cap itself is configuration, and is real.</div>
        </div>`;
    }
    const pct = Math.max(0, Math.min(100, (meter.used / cap) * 100));
    const tone = meter.refused_at ? "over" : pct >= 75 ? "warn" : "";
    return `
      <div class="card seat-meter ${tone}" data-k="budget">
        <div class="seat-meter-fig"><span class="num">${fmtNum(meter.used)}</span>
          <span class="seat-meter-of">of ${fmtNum(cap)} tokens · this run</span></div>
        <div class="seat-budget-track"><i style="width:${pct.toFixed(1)}%"></i></div>
        ${
          meter.refused_at
            ? `<div class="seat-meter-note warn-ink">${icon("alert", "sm")} Refusing charges since ${esc(relTime(meter.refused_at))} — this seat is exhausted and will start no more work.</div>`
            : `<div class="seat-meter-note">The engine's live meter, over this engine run: it resets when the process does. The durable counter that survives a restart is on Spend &amp; Budgets.</div>`
        }
      </div>`;
  }

  function costTables() {
    const phases = (cost.by_phase || []).map((p) => {
      const avg = p.calls ? Math.round(p.total_tokens / p.calls) : 0;
      return `<tr data-k="ph:${escAttr(p.phase)}">
        <td><span class="ph-pill" style="color:${phaseInk(p.phase)}">${esc(p.phase)}</span></td>
        <td class="num">${fmtNum(p.input_tokens)}</td>
        <td class="num">${fmtNum(p.output_tokens)}</td>
        <td class="num">${fmtNum(p.total_tokens)}</td>
        <td class="num">${fmtNum(p.calls)}</td>
        <td class="num">${fmtNum(avg)}</td>
      </tr>`;
    });
    const models = (cost.by_model || []).map(
      (m) => `<tr data-k="md:${escAttr(m.model)}">
        <td><span class="mono">${esc(m.model)}</span></td>
        <td class="num">${fmtNum(m.input_tokens)}</td>
        <td class="num">${fmtNum(m.output_tokens)}</td>
        <td class="num">${fmtNum(m.total_tokens)}</td>
        <td class="num">${fmtNum(m.calls)}</td>
      </tr>`,
    );
    return `
      ${sectionHead("zap", "By stage", null, null)}
      <div class="list tbl-wrap"><table class="tbl">
        <thead><tr><th>Phase</th><th class="num">Input</th><th class="num">Output</th>
          <th class="num">Total</th><th class="num">Calls</th><th class="num">Avg/call</th></tr></thead>
        <tbody>${phases.join("")}</tbody>
      </table></div>
      ${
        models.length
          ? `${sectionHead("cpu", "By model", null, null)}
             <div class="list tbl-wrap"><table class="tbl">
               <thead><tr><th>Model</th><th class="num">Input</th><th class="num">Output</th>
                 <th class="num">Total</th><th class="num">Calls</th></tr></thead>
               <tbody>${models.join("")}</tbody>
             </table></div>`
          : ""
      }`;
  }

  function costPanel(state) {
    let body;
    if (costError) {
      body = empty(
        "zap",
        "Could not read this seat's spend",
        costError === "no_event_store"
          ? "Spend is rolled up from stored phase events. Without an event store there is nothing to aggregate."
          : "The query failed. Nothing here is a statement that this seat spent nothing.",
      );
    } else if (!cost) {
      body = skeletonRows(3);
    } else if (!cost.by_phase || !cost.by_phase.length) {
      body = emptyOrPending(
        state,
        () => skeletonRows(3),
        "zap",
        `No phase events in the last ${PHASE_WINDOW} days`,
        "This seat has run nothing the event store still holds.",
      );
    } else {
      const t = cost.totals;
      body = `
        ${statStrip([
          { label: "Total", value: fmtNum(t.total_tokens), foot: `last ${PHASE_WINDOW}d` },
          { label: "Input", value: fmtNum(t.input_tokens) },
          { label: "Output", value: fmtNum(t.output_tokens) },
          { label: "Phase calls", value: fmtNum(t.calls) },
        ])}
        <div class="card phase-summary" data-k="bar">${phaseBar(cost.by_phase, t.total_tokens)}</div>
        ${costTables()}`;
    }
    return `
      ${sectionHead("zap", "Budget", null, { action: "go", label: "All budgets", route: "/spend" })}
      ${budgetMeter()}
      ${sectionHead("clock", `Spend · last ${PHASE_WINDOW}d`, null, null)}
      ${body}`;
  }

  // ---- tab: access ----

  // The seat as the org config describes it. The projection carries what
  // a seat is DOING; where it sits, what it can reach and how a person
  // reaches it are configuration, and arrive on the `org` snapshot.
  function orgSeat(state) {
    const wanted = String(key).toLowerCase();
    const role = String(roleName() || "").toLowerCase();
    const handle = String(seatHandle() || "").toLowerCase();
    return (
      flattenSeats(state.org).find((s) => {
        const name = String(s.name || "").toLowerCase();
        const h = String(s.handle || "").toLowerCase();
        return h === wanted || name === wanted || (role && name === role) || (handle && h === handle);
      }) || null
    );
  }

  function integrationChips(keys) {
    if (!keys || !keys.length) return "";
    return `<div class="seat-chips">${keys
      .map((k) => {
        const m = integrationMeta(k);
        return `<span class="int-badge" data-k="i:${escAttr(k)}" style="--int-color:${m.color}">${icon(m.icon, "sm")}${esc(m.label)}</span>`;
      })
      .join("")}</div>`;
  }

  // Where a person reaches this seat. Each chip is keyed by the contact
  // field it came from — the field set is fixed and ordered by
  // CONTACT_FIELDS, so a key identifies a chip across renders.
  function contactChips(seat) {
    const out = [];
    for (const [field, k] of CONTACT_FIELDS) {
      const value = seat.contact && seat.contact[field];
      if (!value) continue;
      const m = integrationMeta(k);
      out.push(
        `<span class="int-badge" data-k="c:${escAttr(field)}" style="--int-color:${m.color}" title="${escAttr(field)}">${icon(m.icon, "sm")}${esc(value)}</span>`,
      );
    }
    if (seat.email) {
      out.push(`<span class="chip" data-k="c:email">${esc(seat.email)}</span>`);
    }
    return out.length ? `<div class="seat-chips">${out.join("")}</div>` : "";
  }

  function fact(label, value) {
    return `<div class="seat-fact" data-k="f:${escAttr(label)}">
      <span class="seat-fact-label">${esc(label)}</span>
      <span class="seat-fact-value">${value}</span>
    </div>`;
  }

  function accessPanel(state) {
    const seat = orgSeat(state);
    if (!seat) {
      return emptyOrPending(
        state,
        () => skeletonRows(3),
        "key",
        "This seat is not in the org tree",
        "It is running, but no role in the active company configuration matches it — the config was changed under a live engine.",
      );
    }
    const where = seat.unitPath.length ? seat.unitPath.join(" › ") : "root level";
    const contacts = contactChips(seat);
    return `
      ${sectionHead("plug", "Surfaces it can act on", seat.integrations.length, {
        action: "go",
        label: "All integrations",
        route: "/integrations",
      })}
      ${
        seat.integrations.length
          ? integrationChips(seat.integrations) +
            `<div class="seat-note">Read from the MCP servers wired to this seat — its own <code>mcp_env</code> plus every ancestor unit's, which it inherits. A surface here is one this seat holds credentials for, not one it has used.</div>`
          : empty(
              "plug",
              "No integration surfaces",
              "This seat has no `mcp_env` of its own and inherits none from its unit chain, so it can act only through the engine's builtin tools.",
            )
      }
      ${sectionHead("users", "Where it sits", null, { action: "go", label: "Org chart", route: "/org?lens=chart" })}
      <div class="card seat-facts" data-k="facts">
        ${fact("Handle", `<span class="mono">@${esc(seat.handle)}</span>`)}
        ${fact("Unit", esc(where))}
        ${seat.lead ? fact("Unit lead", esc(seat.lead)) : ""}
        ${seat.goal ? fact("Goal", esc(seat.goal)) : ""}
        ${
          seat.tokenBudget
            ? fact("Token budget", `<span class="num">${fmtNum(seat.tokenBudget)}</span>`)
            : ""
        }
      </div>
      ${
        contacts
          ? `${sectionHead("message", "Reachable as", null, null)}${contacts}
             <div class="seat-note">The identities this seat is addressed by on each platform, from the role's <code>contact</code> block. They are how an inbound message is routed to this seat.</div>`
          : ""
      }`;
  }

  // ---- a human seat ----

  // A human teammate holds a seat in the chart, is addressed by name and
  // mentioned in threads — but the engine runs nothing for them, so
  // there are no turns, no diary and no spend to tab between. The one
  // question this page can answer for them is how they are reached.
  function humanPanel(seat) {
    const contacts = contactChips(seat);
    const where = seat.unitPath.length ? seat.unitPath.join(" › ") : "root level";
    return `
      ${backLink()}
      <div class="card agent-head" data-k="head">
        ${avatarFor(seat.name)}
        <div class="agent-head-id">
          <div class="agent-head-name" style="color:${roleInk(seat.name)}">${esc(seat.name)}</div>
          <div class="row-sub"><span class="mono">@${esc(seat.handle)}</span> · ${esc(where)}</div>
        </div>
        <div class="agent-head-state">
          <span class="badge purple">human</span>
          ${seat.availability ? `<div class="row-sub">${esc(seat.availability)}</div>` : ""}
        </div>
      </div>
      <div class="seat-note">A human seat. The engine addresses this person — it never runs them — so there is no turn history, no memory and no spend under this name.</div>
      ${sectionHead("message", "Reachable as", null, null)}
      ${
        contacts ||
        empty(
          "message",
          "No contact identities",
          "Add a `contact` block to this role so mentions of this person route to their seat.",
        )
      }
      ${
        seat.manages && seat.manages.length
          ? `${sectionHead("users", "Manages", seat.manages.length, null)}
             <div class="seat-chips">${seat.manages
               .map(
                 (n) =>
                   `<span class="chip clickable" data-k="mg:${escAttr(n)}" data-action="seat" data-seat="${escAttr(n)}" role="link" tabindex="0">${esc(n)}</span>`,
               )
               .join("")}</div>`
          : ""
      }`;
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
    const back = `<div class="back-link" data-action="back-seat" role="link" tabindex="0">${icon("chevron", "sm")} Back to ${esc(roleName() || "seat")}</div>`;
    if (loading && !loadError) return back + skeletonRows(3);
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
          keyPrefix: "detail|" + keyFor(r),
          isOpen: isPromptOpen,
        })}
        <div class="block-label">Response</div>${responseBody(r.response, r.tool_executions, {
          keyPrefix: "detail|" + keyFor(r),
          thinkOpen: isThinkOpen,
          toolOpen: isToolOpen,
          codingAgent: isCodingAgentPhase(r),
          model: r.model,
          failure,
          inProgress: !!r._live,
        })}
      </div>`;
  }

  // ---- events ----

  function isMine(payload) {
    const rid = payload.agent_id || "";
    const role = payload.role || payload.agent_role || "";
    return (
      (rid && (rid === (data && data.runtime_id) || rid === liveAgent().runtime_id)) ||
      (role && role === roleName())
    );
  }

  return {
    // The in-flight call and every header field live on the agent
    // projection, so an `agents` push is all this view needs to redraw a
    // streaming round. `org` backs the Access tab, and `health` is what
    // tells a genuinely-empty list apart from a page that cannot see.
    slices: ["agents", "sandboxes", "budget", "org", "health"],

    mount() {
      if (begin()) return;
      // The seat list has not arrived yet — a direct link lands on this
      // route before the first snapshot, and a handle cannot be resolved
      // into a seat id without it. Wait for the agents slice rather than
      // reporting a seat that exists as missing.
      if (typeof store.subscribe === "function") {
        unwatch = store.subscribe(["agents"], () => {
          if (begin()) dropWatch();
        });
      }
    },

    render(state) {
      if (llmSub) return renderLlmDetail();

      const seat = seatRow();
      if (!seat) {
        // Not in the agents list. It may still be a human seat, which is
        // in the org chart and never in the runtime — the engine
        // addresses those, it does not run them.
        const human = orgSeat(state);
        if (human && human.kind === "human") return humanPanel(human);
        return (
          backLink() +
          emptyOrPending(
            state,
            () => skeletonRows(4),
            "user",
            `No seat named “${key}”`,
            "No role in the active company configuration has this handle, id or name.",
          )
        );
      }
      if (loadError) {
        return (
          backLink() +
          empty(
            "user",
            loadError === "not_found" ? "Seat not found" : "Could not load this seat",
            loadError === "offline"
              ? "Reconnecting…"
              : "The engine knows this seat's configuration but could not answer for it.",
          )
        );
      }

      const panel =
        tab === "memory"
          ? memoryPanel()
          : tab === "cost"
            ? costPanel(state)
            : tab === "access"
              ? accessPanel(state)
              : nowPanel(state);
      return `
        ${backLink()}
        ${renderHead(state)}
        ${tabBar()}
        <div class="seat-panel" data-k="panel:${tab}">${panel}</div>`;
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
      // Only the Now tab draws these. The record is kept either way, so
      // switching to it later shows the turn — but a tab that cannot
      // show the change does not re-render for it.
      if (tab === "now") refresh();
    },

    onAction(action, target) {
      if (action === "seat-tab") {
        const next = target.dataset.tab;
        if (!TAB_KEYS.has(next) || next === tab) return;
        tab = next;
        syncUrl();
        ensureTabData();
      } else if (action === "toggle-turn") {
        // Read the rendered state rather than re-deriving the default:
        // it is what the reader is looking at, and it cannot disagree.
        const turnId = target.dataset.turn;
        const card = target.closest(".turn");
        overrides.set(`turn:${turnId}`, !(card && card.classList.contains("open")));
      } else if (action === "toggle-row") {
        // The row's default depends on what kind of row it is, so read
        // it back off the rendered element rather than re-deriving it.
        const k = target.dataset.key;
        const row = target.closest(".llm-row");
        const wasOpen = !!row && row.classList.contains("open");
        overrides.set(`row:${k}`, !wasOpen);
      } else if (action === "toggle-think") {
        toggle(`think:${target.dataset.tkey}`, true);
      } else if (action === "toggle-tool") {
        toggle(`tool:${target.dataset.tkey}`, false);
      } else if (action === "toggle-prompt") {
        const k = target.dataset.pkey;
        const block = target.closest(".msg-block");
        overrides.set(`prompt:${k}`, !(block && block.classList.contains("open")));
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
          `${seatPath()}/llm/${encodeURIComponent(d.turn)}/${encodeURIComponent(d.phase)}/${encodeURIComponent(d.iter)}`,
        );
        return;
      } else if (action === "back-seat") {
        navigate(seatPath());
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

    destroy() {
      disposed = true;
      dropWatch();
    },
  };
}

// The tab named by the location's query string, or "" for none.
//
// `parseRoute` carries the seat route's PATH — the key, and the record's
// coordinates on the llm sub-route — and does not thread a query string
// through it, so this reads the one parameter that belongs to the view.
function tabFromUrl() {
  const hash = (globalThis.location && globalThis.location.hash) || "";
  const query = String(hash).split("?")[1] || "";
  const found = new URLSearchParams(query).get("tab") || "";
  return TAB_KEYS.has(found) ? found : "";
}

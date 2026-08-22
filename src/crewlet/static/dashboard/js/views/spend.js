// Spend & Budgets: what the company cost, and what is about to stop it.
//
// These were two screens' worth of question answered by one. The Tokens
// view broke spend down by phase, model, agent and turn and said nothing
// about caps; the caps appeared only as a bar on a seat card, drawn from
// the engine's live meter. So the question an operator actually has —
// "is anything about to run out of budget, and has anything already
// been refused" — had no surface at all, and the one number that could
// have answered it (the shared counter that survives restarts) was
// reachable only from `crewlet budgets show`.
//
// The two halves are kept visibly apart, because they measure different
// spans and mixing them is how a percentage becomes a lie:
//
//   the live meter   one engine RUN — resets when the process does
//   durable usage    every process, since the last deliberate reset
//   the 24h/7d/30d   a rolling window over the event store
//
// A bar is drawn only where its numerator and denominator share a span.

import { esc, escAttr, fmtNum, fmtDateTime, relTime, shortId } from "../format.js";
import { icon } from "../icons.js";
import { PHASE_ORDER, phaseColor, phaseInk } from "../state.js";
import {
  empty,
  emptyOrPending,
  phaseBar,
  sectionHead,
  skeletonCards,
} from "../ui.js";

const WINDOWS = [
  { d: 1, label: "24h" },
  { d: 7, label: "7d" },
  { d: 30, label: "30d" },
];

// How often to re-read the shared counter. It moves on every charge from
// every node, and nothing pushes it — but it is a cost figure, not a
// live signal, so it does not need to be current to the second.
const BUDGET_POLL_MS = 30_000;

export function createSpendView({ store, query, refresh }) {
  // The live rollup the server pushes covers one window (its own), and
  // that is the default here. Ask for any other window and this view
  // queries for it and holds the answer locally — the pushed rollup
  // keeps updating underneath and takes over again when the window
  // returns to the live one.
  const LIVE_WINDOW_FALLBACK = 1;
  let liveWindow = liveOf(store.state) || LIVE_WINDOW_FALLBACK;
  let win = liveWindow;
  let queried = null; // {window, data} for a non-live window
  let loading = false;

  let budgets = null;
  let budgetError = false;
  let timer = null;
  let disposed = false;

  async function loadBudgets() {
    try {
      budgets = await query("budgets");
      budgetError = false;
    } catch {
      budgetError = true;
    }
    if (!disposed) refresh();
  }

  function windowLabel() {
    const match = WINDOWS.find((w) => w.d === win);
    return match ? match.label : `${win}d`;
  }

  // ---- budgets ----

  // A meter with a cap, drawn only when both cover the same span.
  //
  // Exhaustion is a STATE, not a ratio: the engine refuses a charge that
  // would exceed the cap and increments nothing, so a seat charged in
  // 3k-token rounds against a 100k cap stalls near 99k and can never
  // reach its own maximum. A bar keyed on used >= max shows a
  // permanently blocked seat at 97% and calls it healthy.
  function meterBar(used, max, refusedAt) {
    if (!max) return "";
    const pct = Math.round(Math.min(100, (used / max) * 100));
    const tone = refusedAt ? "over" : pct >= 75 ? "warn" : "";
    return `
      <div class="meter ${tone}" title="${esc(fmtNum(used))} of ${esc(fmtNum(max))}">
        <i style="width:${refusedAt ? 100 : pct}%"></i>
      </div>`;
  }

  function orgBudget() {
    const org = budgets.org || {};
    const hasCap = !!org.max_tokens;
    const refused = org.refused_at;
    return `
      <div class="bud-org panel" data-k="bud:org">
        <div class="eyebrow">Org budget</div>
        ${
          hasCap
            ? `<div class="bud-figure display"><span class="num">${esc(fmtNum(org.durable_used))}</span>
                 <span class="bud-of">of ${esc(fmtNum(org.max_tokens))} tokens</span></div>
               ${meterBar(org.durable_used, org.max_tokens, refused)}
               <div class="bud-note">The shared counter, across every node and every restart — this is what the engine enforces.</div>`
            : `<div class="bud-figure display"><span class="num">${esc(fmtNum(org.durable_used))}</span>
                 <span class="bud-of">tokens spent</span></div>
               <div class="bud-note">No org-wide cap is configured, so nothing is enforced at this level.</div>`
        }
        ${
          refused
            ? `<div class="bud-refused warn-ink">${icon("alert", "sm")} Refusing charges since ${esc(relTime(refused))}</div>`
            : ""
        }
        ${
          org.durable_updated_at
            ? `<div class="bud-meta">last charged ${esc(relTime(org.durable_updated_at))}</div>`
            : ""
        }
      </div>`;
  }

  function seatBudgets() {
    const seats = (budgets.seats || []).filter(
      (s) => s.max_tokens || s.durable_used,
    );
    if (!seats.length) {
      return empty(
        "zap",
        "No per-seat budgets",
        "Set `token_budget` on a role to cap what one seat may spend.",
      );
    }
    const rows = seats
      .map((s) => {
        const refused = s.refused_at;
        return `<tr class="${refused ? "is-refused" : ""}" data-k="bud:${escAttr(s.agent_id)}"
                    data-action="seat" data-seat="${escAttr(s.handle || s.role)}">
          <td>${esc(s.role)}</td>
          <td class="num">${esc(fmtNum(s.durable_used))}</td>
          <td class="num">${s.max_tokens ? esc(fmtNum(s.max_tokens)) : '<span class="zero">no cap</span>'}</td>
          <td>${meterBar(s.durable_used, s.max_tokens, refused)}</td>
          <td>${
            refused
              ? `<span class="badge failed"><i class="dot"></i>refused</span>`
              : s.live_used === null || s.live_used === undefined
                ? `<span class="zero">no live meter</span>`
                : `<span class="bud-live">${esc(fmtNum(s.live_used))} this run</span>`
          }</td>
          <td class="row-ts">${s.durable_updated_at ? esc(relTime(s.durable_updated_at)) : ""}</td>
        </tr>`;
      })
      .join("");
    return `<div class="list tbl-wrap"><table class="tbl">
      <thead><tr>
        <th>Seat</th><th class="num">Used</th><th class="num">Cap</th>
        <th>Against cap</th><th>Now</th><th>Last charge</th>
      </tr></thead>
      <tbody>${rows}</tbody></table></div>`;
  }

  function budgetSection(state) {
    if (budgets === null) {
      return (
        sectionHead("zap", "Budgets") +
        (budgetError
          ? empty(
              "zap",
              "Could not read budgets",
              "The query failed — retrying every 30 seconds.",
            )
          : skeletonCards(2))
      );
    }
    // `durable: false` means the shared counter could not be read, which
    // is a different statement from "nothing has been spent". Drawing
    // every seat at the bottom of its cap would be the wrong one.
    if (!budgets.durable) {
      return (
        sectionHead("zap", "Budgets") +
        emptyOrPending(
          state,
          skeletonCards(2),
          "zap",
          "No shared usage counter",
          "Budget usage lives in PostgreSQL so every node enforces against one number. Without a database each process would count alone, so nothing durable is recorded.",
        )
      );
    }
    return (
      sectionHead("zap", "Budgets") +
      `<div class="bud-grid">${orgBudget()}</div>` +
      sectionHead("users", "Per seat", (budgets.seats || []).length) +
      seatBudgets()
    );
  }

  // ---- spend breakdown ----

  function statGrid(t) {
    const cell = (label, value, sub) => `
      <div class="stat">
        <div class="stat-label">${esc(label)}</div>
        <div class="stat-value">${value}</div>
        ${sub ? `<div class="stat-sub">${esc(sub)}</div>` : ""}
      </div>`;
    return `<div class="stat-grid">
      ${cell("Total tokens", fmtNum(t.total_tokens), `last ${windowLabel()}`)}
      ${cell("Input", fmtNum(t.input_tokens))}
      ${cell("Output", fmtNum(t.output_tokens))}
      ${cell("Phase calls", fmtNum(t.calls))}
    </div>`;
  }

  function table(title, iconId, head, rows) {
    if (!rows) return "";
    return `<div class="tok-section">
      <div class="sec"><span class="sec-title">${icon(iconId, "sm")}${esc(title)}</span></div>
      <div class="list tbl-wrap"><table class="tbl"><thead>${head}</thead><tbody>${rows}</tbody></table></div>
    </div>`;
  }

  function phaseTable(phases) {
    const rows = phases
      .map((p) => {
        const avg = p.calls ? Math.round(p.total_tokens / p.calls) : 0;
        return `<tr data-k="ph:${escAttr(p.phase)}">
          <td><span class="phase-tag" style="color:${phaseInk(p.phase)}">${esc(p.phase)}</span></td>
          <td class="num">${fmtNum(p.input_tokens)}</td>
          <td class="num">${fmtNum(p.output_tokens)}</td>
          <td class="num">${fmtNum(p.total_tokens)}</td>
          <td class="num">${fmtNum(p.calls)}</td>
          <td class="num">${fmtNum(avg)}</td>
        </tr>`;
      })
      .join("");
    return table(
      "Spend by stage",
      "zap",
      "<tr><th>Phase</th><th class='num'>Input</th><th class='num'>Output</th><th class='num'>Total</th><th class='num'>Calls</th><th class='num'>Avg/call</th></tr>",
      rows,
    );
  }

  function modelTable(models) {
    if (!models.length) return "";
    const rows = models
      .map(
        (m) => `<tr data-k="md:${escAttr(m.model)}">
        <td><span class="mono" style="color:var(--purple-ink)">${esc(m.model)}</span></td>
        <td class="num">${fmtNum(m.input_tokens)}</td>
        <td class="num">${fmtNum(m.output_tokens)}</td>
        <td class="num">${fmtNum(m.total_tokens)}</td>
        <td class="num">${fmtNum(m.calls)}</td>
      </tr>`,
      )
      .join("");
    return table(
      "Spend by model",
      "cpu",
      "<tr><th>Model</th><th class='num'>Input</th><th class='num'>Output</th><th class='num'>Total</th><th class='num'>Calls</th></tr>",
      rows,
    );
  }

  function workerTable(workers) {
    if (!workers.length) return "";
    const rows = workers
      .map((w) => {
        const avg = w.calls ? Math.round(w.total_tokens / w.calls) : 0;
        return `<tr data-k="wk:${escAttr(w.worker)}">
          <td><span class="mono">${esc(w.worker)}</span></td>
          <td class="num">${fmtNum(w.total_tokens)}</td>
          <td class="num">${fmtNum(w.calls)}</td>
          <td class="num">${fmtNum(avg)}</td>
        </tr>`;
      })
      .join("");
    return table(
      "Auxiliary workers",
      "refresh",
      "<tr><th>Worker</th><th class='num'>Total</th><th class='num'>Calls</th><th class='num'>Avg/call</th></tr>",
      rows,
    );
  }

  function agentMatrix(byAgent, byPhase) {
    if (!byAgent.length) return "";
    const phases = byPhase.map((p) => p.phase);
    let maxCell = 1;
    for (const a of byAgent)
      for (const p of phases)
        maxCell = Math.max(maxCell, (a.by_phase[p] || {}).total_tokens || 0);
    const head =
      "<tr><th>Agent</th>" +
      phases.map((p) => `<th class="num">${esc(p)}</th>`).join("") +
      "<th class='num'>Total</th></tr>";
    const rows = byAgent
      .map((a) => {
        const cells = phases
          .map((p) => {
            const v = (a.by_phase[p] || {}).total_tokens || 0;
            // Sequential magnitude fill, mixed from the phase's own mark step
            // so the ramp re-steps with the theme. Every cell also prints its
            // value, which is the relief the low-magnitude end needs.
            const pct = v ? (8 + 44 * (v / maxCell)).toFixed(1) : 0;
            const fill = v
              ? `color-mix(in srgb, ${phaseColor(p)} ${pct}%, transparent)`
              : "transparent";
            return `<td class="num heat" style="background:${fill}">${v ? fmtNum(v) : '<span class="zero">·</span>'}</td>`;
          })
          .join("");
        return `<tr class="clickable" data-k="ag:${escAttr(a.agent_id)}"
                    data-action="seat" data-seat="${escAttr(a.handle || a.role)}">
          <td>${esc(a.role)}</td>${cells}<td class="num"><b>${fmtNum(a.total_tokens)}</b></td>
        </tr>`;
      })
      .join("");
    return table("Spend by agent × stage", "users", head, rows);
  }

  function turnTable(byTurn) {
    if (!byTurn.length) return "";
    const phaseSet = new Set();
    for (const t of byTurn) for (const p of Object.keys(t.by_phase || {})) phaseSet.add(p);
    const phases = [...phaseSet].sort(
      (a, b) => (PHASE_ORDER[a] || 50) - (PHASE_ORDER[b] || 50),
    );
    const head =
      "<tr><th>When</th><th>Agent</th><th>Turn</th>" +
      phases.map((p) => `<th class="num">${esc(p)}</th>`).join("") +
      "<th class='num'>Total</th></tr>";
    const rows = byTurn
      .map((t) => {
        const cells = phases
          .map((p) => {
            const v = (t.by_phase[p] || {}).total_tokens || 0;
            return `<td class="num">${v ? fmtNum(v) : '<span class="zero">·</span>'}</td>`;
          })
          .join("");
        return `<tr class="clickable" data-k="tn:${escAttr(t.turn_id)}"
                    data-action="seat" data-seat="${escAttr(t.handle || t.role)}">
          <td class="mono nowrap">${esc(fmtDateTime(t.ended_at || t.started_at))}</td>
          <td>${esc(t.role)}</td>
          <td class="mono">${esc(shortId(t.turn_id))}</td>
          ${cells}<td class="num"><b>${fmtNum(t.total_tokens)}</b></td>
        </tr>`;
      })
      .join("");
    return table("Recent turns", "clock", head, rows);
  }

  function windowBar() {
    return `<div class="tok-window">
      ${WINDOWS.map((w) => `<button class="${w.d === win ? "active" : ""}" data-action="win" data-d="${w.d}">${w.label}</button>`).join("")}
    </div>`;
  }

  function liveOf(state) {
    return state.tokens && state.tokens.since_days ? state.tokens.since_days : 0;
  }

  function rollup(state) {
    // The pushed rollup names its own window. Adopt it the first time it
    // lands so the selector agrees with what is on screen.
    const pushed = liveOf(state);
    if (pushed && pushed !== liveWindow) {
      if (win === liveWindow) win = pushed;
      liveWindow = pushed;
    }
    if (win === liveWindow) return state.tokens;
    return queried && queried.window === win ? queried.data : null;
  }

  async function load() {
    loading = true;
    refresh();
    try {
      const data = await query("tokens", { since_days: win });
      queried = { window: win, data };
    } catch {
      queried = { window: win, data: null };
    }
    loading = false;
    refresh();
  }

  function breakdown(state) {
    const d = rollup(state);
    if (loading || !d) return windowBar() + skeletonCards(4);
    if (!d.by_phase || !d.by_phase.length) {
      return windowBar() + empty("zap", `No phase events in the last ${windowLabel()}`);
    }
    return `
      ${windowBar()}
      ${statGrid(d.totals)}
      <div class="card phase-panel">${phaseBar(d.by_phase, d.totals.total_tokens)}</div>
      ${phaseTable(d.by_phase)}
      ${modelTable(d.by_model || [])}
      ${workerTable(d.by_worker || [])}
      ${agentMatrix(d.by_agent || [], d.by_phase)}
      ${turnTable(d.by_turn || [])}`;
  }

  return {
    // The live window re-renders as the server pushes new spend; a
    // queried window is static until the reader changes it back.
    slices: ["tokens", "agents", "budget", "health", "connected"],

    mount() {
      loadBudgets();
      timer = setInterval(loadBudgets, BUDGET_POLL_MS);
    },

    destroy() {
      disposed = true;
      if (timer) clearInterval(timer);
      timer = null;
    },

    render(state) {
      return `
        ${budgetSection(state)}
        ${sectionHead("clock", "Spend")}
        ${breakdown(state)}`;
    },

    // Seat navigation is handled globally; this view owns the window switch.
    onAction(action, t) {
      if (action !== "win") return;
      win = Number(t.dataset.d);
      if (win === liveWindow) {
        queried = null;
        refresh();
      } else {
        load();
      }
    },

    __loadBudgetsForTest: loadBudgets,
  };
}

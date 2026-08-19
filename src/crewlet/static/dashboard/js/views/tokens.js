// Token spend breakdown: by phase / model / worker / agent / turn.

import { esc, fmtNum, fmtDateTime, shortId } from "../format.js";
import { icon } from "../icons.js";
import { PHASE_ORDER, phaseColor, phaseInk } from "../state.js";
import { empty, phaseBar, skeletonCards } from "../ui.js";

const WINDOWS = [
  { d: 1, label: "24h" },
  { d: 7, label: "7d" },
  { d: 30, label: "30d" },
];

export function createTokensView({ store, query, refresh }) {
  // The live rollup the server pushes covers one window (its own), and
  // that is the default here. Ask for any other window and this view
  // queries for it and holds the answer locally — the pushed rollup
  // keeps updating underneath and takes over again when the window
  // returns to the live one.
  // Read the window the server actually pushes, and re-read it once the
  // rollup arrives: mounting this view cold (a deep link, a reload) has
  // no rollup yet, and guessing left the selector highlighting "7d" over
  // a rollup covering a day.
  const LIVE_WINDOW_FALLBACK = 1;
  let liveWindow = liveOf(store.state) || LIVE_WINDOW_FALLBACK;
  let win = liveWindow;
  let queried = null; // {window, data} for a non-live window
  let loading = false;

  function windowLabel() {
    const match = WINDOWS.find((w) => w.d === win);
    return match ? match.label : `${win}d`;
  }

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
        return `<tr>
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
        (m) => `<tr>
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
        return `<tr>
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
        return `<tr class="clickable" data-action="agent" data-id="${esc(a.agent_id)}">
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
        return `<tr class="clickable" data-action="agent" data-id="${esc(t.agent_id)}">
          <td class="mono" style="font-size:11px">${esc(fmtDateTime(t.ended_at || t.started_at))}</td>
          <td>${esc(t.role)}</td>
          <td class="mono" style="font-size:11px">${esc(shortId(t.turn_id))}</td>
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

  return {
    // The live window re-renders as the server pushes new spend; a
    // queried window is static until the reader changes it back.
    slices: ["tokens"],

    render(state) {
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
    },

    // "agent" navigation is handled globally; we only own the window switch.
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
  };
}

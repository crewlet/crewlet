// Dashboard overview: widgets + the seat row + engine activity.

import { esc, fmtNum, relTime, trunc } from "../format.js";
import { icon } from "../icons.js";
import { avatarFor, stateBadgeClass, effectiveAgentState, stateLabel } from "../state.js";
import { flattenSeats } from "../org.js";
import { seatRow } from "../cards.js";
import { empty, phaseBar, sectionHead, activityRow, skeletonCards } from "../ui.js";

export function createDashboardView({ store, api, navigate }) {
  let root;

  function widgets(state) {
    const agents = state.agents || [];
    const tools = state.tools || [];
    const tok = state.tokens && state.tokens.data;
    const totals = tok ? tok.totals : null;
    const working = agents.filter((a) => a.state === "working").length;

    const bars = agents
      .slice(0, 22)
      .map((a) => {
        const h = a.state === "working" ? 44 : a.state === "idle" ? 22 : 8;
        const c =
          a.state === "working"
            ? "var(--amber)"
            : a.state === "idle"
              ? "var(--green)"
              : "var(--text-dim)";
        return `<i class="mini-bar" style="height:${h}px;background:${c}" title="${esc(a.role)}: ${esc(a.state || "offline")}"></i>`;
      })
      .join("");

    return `
      <div class="widgets">
        <div class="widget green dot-texture">
          <div class="widget-head">${icon("users", "sm")} Agents</div>
          <div class="widget-big">${fmtNum(agents.length)}</div>
          <div class="widget-foot">${fmtNum(working)} working now</div>
          <div class="mini-bars">${bars || ""}</div>
          <div class="legend">
            <span><i style="background:var(--amber)"></i>Working</span>
            <span><i style="background:var(--green)"></i>Idle</span>
            <span><i style="background:var(--text-dim)"></i>Other</span>
          </div>
        </div>
        <div class="widget pink clickable" data-action="view-tools">
          <div class="widget-head">${icon("wrench", "sm")} Tools</div>
          <div class="widget-big">${fmtNum(tools.length)}</div>
          <div class="widget-foot">registered tools</div>
        </div>
        <div class="widget purple clickable" data-action="view-tokens">
          <div class="widget-head">${icon("zap", "sm")} Token Spend</div>
          <div class="widget-big">${totals ? fmtNum(totals.total_tokens) : "—"}</div>
          <div class="widget-foot">
            ${totals ? `${fmtNum(totals.input_tokens)} in · ${fmtNum(totals.output_tokens)} out` : "last 7 days"}
          </div>
          <div style="margin-top:12px">${tok ? phaseBar(tok.by_phase, totals.total_tokens) : ""}</div>
        </div>
      </div>`;
  }

  // Who is on the team and what each one is doing, as cards. Human seats sit
  // in the row alongside agents — they hold seats in the same org chart.
  function seats(state) {
    const all = flattenSeats(state.org);
    if (!all.length) return "";
    return (
      sectionHead("users", "The team", all.length, { action: "view-agents", label: "All agents" }) +
      seatRow(all, { agents: state.agents, sandboxes: state.sandboxes })
    );
  }

  function agentsList(state) {
    const agents = state.agents || [];
    if (!agents.length) return empty("user", "No agents running");
    return (
      '<div class="list">' +
      agents
        .map((a) => {
          const st = effectiveAgentState(a, state.sandboxes);
          const badgeCls = stateBadgeClass(st);
          const task = a.current_task
            ? `<span class="badge info text">${esc(trunc(a.current_task, 22))}</span>`
            : "";
          return `
          <div class="row clickable" data-action="agent" data-id="${esc(a.id)}">
            ${avatarFor(a.role)}
            <div class="row-body">
              <div class="row-title">${esc(a.role || a.name)}</div>
              <div class="row-sub">
                <span class="tok-chip">${icon("zap", "sm")}
                  <span class="in">${fmtNum(a.input_tokens || 0)}</span> in ·
                  <span class="out">${fmtNum(a.output_tokens || 0)}</span> out ·
                  <span class="tot">${fmtNum(a.total_tokens || 0)}</span></span>
              </div>
            </div>
            ${task}
            <span class="badge ${badgeCls}"><i class="dot"></i>${esc(stateLabel(st))}</span>
          </div>`;
        })
        .join("") +
      "</div>"
    );
  }

  function activityList(state) {
    // Sort rather than trust arrival order: the feed mixes the snapshot's
    // buffer with live pushes, and a late-delivered event would otherwise
    // pin itself to the top with an older clock time than the row below it.
    const events = [...(state.events || [])]
      .sort((a, b) => String(b.timestamp || "").localeCompare(String(a.timestamp || "")))
      .slice(0, 10);
    if (!events.length) return empty("inbox", "No recent activity");
    return (
      '<div class="list act-list">' +
      events.map((e) => activityRow(e, { agents: state.agents })).join("") +
      "</div>"
    );
  }

  // In-flight detached sandbox jobs. Returns "" when none, so the panel
  // surfaces on the overview exactly when something is running.
  function sandboxList(state) {
    const runs = state.sandboxes || [];
    if (!runs.length) return "";
    const byRole = new Map((state.agents || []).map((a) => [a.role, a]));
    const rows = runs
      .map((s) => {
        const agent = byRole.get(s.role);
        const navId = agent ? agent.id : "";
        const attrs = navId
          ? `class="row clickable" data-action="agent" data-id="${esc(navId)}"`
          : `class="row"`;
        const awaiting = s.status === "awaiting_input";
        const statusBadge = awaiting
          ? `<span class="badge warn"><i class="dot"></i>awaiting input</span>`
          : `<span class="badge info"><i class="dot"></i>running</span>`;
        const chip = `<span class="chip sandbox" title="Sandboxed Execute (${esc(s.coding_agent || "coding agent")})">⛁ ${esc(s.coding_agent || "sandbox")}</span>`;
        const task = s.task ? `<div class="row-sub">${esc(trunc(s.task, 90))}</div>` : "";
        const q =
          awaiting && s.question
            ? `<div class="row-sub" style="color:var(--orange-ink)">“${esc(trunc(s.question, 90))}”</div>`
            : "";
        return `
          <div ${attrs}>
            ${avatarFor(s.role)}
            <div class="row-body">
              <div class="row-title">${esc(s.role || s.agent_handle)} ${chip}</div>
              ${task}${q}
            </div>
            ${statusBadge}
            <span class="row-ts">${esc(relTime(s.started_at))}</span>
          </div>`;
      })
      .join("");
    return (
      sectionHead("cpu", "Running sandboxes", runs.length, null) +
      `<div class="list">${rows}</div>`
    );
  }

  function render(state) {
    const agents = state.agents || [];
    const active = agents.filter((a) => a.state === "working").length;
    const hasSeats = flattenSeats(state.org).length > 0;
    root.innerHTML = `
      ${widgets(state)}
      ${seats(state)}
      ${sandboxList(state)}
      ${hasSeats ? "" : sectionHead("users", "Agents", `${active} active`, null) + agentsList(state)}
      ${sectionHead("activity", "Engine activity events", (state.events || []).length, { action: "view-events", label: "View all" })}
      ${activityList(state)}`;
  }

  return {
    mount(el) {
      root = el;
      root.innerHTML = skeletonCards(3);
      // Lazily fetch the token rollup for the widget if not cached.
      if (!store.state.tokens) {
        api.tokens({ sinceDays: 7 }).then((d) => {
          if (d && !d._error) {
            store.setTokens(7, d);
            if (root.isConnected) render(store.state);
          }
        });
      }
      render(store.state);
    },
    update(state) {
      if (root && root.isConnected) render(state);
    },
  };
}

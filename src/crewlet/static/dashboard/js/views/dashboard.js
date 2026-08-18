// The overview: what the company is doing right now, then who it is, then
// what just happened.
//
// The lead panel is the live turn board — one row per agent actually
// mid-turn, showing its phase, how many rounds it has spent, and the
// response text as it streams. That panel is the reason to keep this page
// open, and it is what the dashboard did not have: it showed counts and a
// feed, and you had to open an agent to learn whether anything was
// happening at all.

import { esc, escAttr, fmtNum, relTime, trunc } from "../format.js";
import { icon } from "../icons.js";
import {
  avatarFor,
  effectiveAgentState,
  phaseColor,
  phaseInk,
  roleInk,
  stateBadgeClass,
  stateLabel,
  statusLine,
} from "../state.js";
import { flattenSeats } from "../org.js";
import { seatRow } from "../cards.js";
import {
  bucketSeries,
  empty,
  phaseBar,
  sectionHead,
  activityRow,
  skeletonCards,
  statWidget,
} from "../ui.js";
import { failureLabel } from "../llm.js";

// Window the overview's trend lines cover. One hour at 5-minute
// resolution: long enough to show whether the company has been busy,
// short enough that a burst two minutes ago is still visible as a spike
// rather than averaged into the floor.
const TREND_MINUTES = 60;
const TREND_BUCKETS = 12;

// Rows on the overview's activity feed. The full history is one click
// away on Activity; this is a glance, not a log.
const FEED_ROWS = 12;

export function createDashboardView({ store }) {
  function widgets(state) {
    const agents = state.agents || [];
    const tokens = state.tokens;
    const totals = tokens ? tokens.totals : null;
    const working = agents.filter((a) => a.state === "working").length;
    const afk = agents.filter((a) => a.state === "afk").length;

    const eventTrend = bucketSeries(state.events || [], {
      minutes: TREND_MINUTES,
      buckets: TREND_BUCKETS,
    });
    const spendTrend = bucketSeries(tokens ? tokens.by_turn || [] : [], {
      minutes: TREND_MINUTES,
      buckets: TREND_BUCKETS,
      valueOf: (turn) => turn.total_tokens || 0,
    });

    return `
      <div class="widgets">
        ${statWidget({
          hue: "green",
          iconId: "users",
          label: "Agents",
          value: fmtNum(agents.length),
          foot: `${fmtNum(working)} working now${afk ? ` · <span class="warn-ink">${fmtNum(afk)} stopped</span>` : ""}`,
          series: eventTrend,
        })}
        ${statWidget({
          hue: "purple",
          iconId: "zap",
          label: "Token Spend",
          value: totals ? fmtNum(totals.total_tokens) : "—",
          foot: totals
            ? `${fmtNum(totals.input_tokens)} in · ${fmtNum(totals.output_tokens)} out`
            : "waiting for the first turn",
          series: spendTrend,
          action: "view-tokens",
        })}
        <div class="widget pink clickable" data-action="view-tools" data-k="w:tools">
          <div class="widget-head">${icon("wrench", "sm")} Tools</div>
          <div class="widget-big">${fmtNum((state.tools || []).length)}</div>
          <div class="widget-foot">registered across every seat</div>
          ${tokens && totals ? `<div class="widget-bar">${phaseBar(tokens.by_phase, totals.total_tokens)}</div>` : ""}
        </div>
      </div>`;
  }

  // ---- the live board ----

  // One row per agent mid-turn. Everything on it comes from the server's
  // in-flight call, so it survives a refresh and needs no reconstruction
  // from the event stream.
  function liveRow(agent) {
    const call = agent.live_call;
    const phase = call.phase || agent.current_phase || "";
    const roundCount = call.rounds || 0;
    const pips = Array.from(
      { length: Math.min(Math.max(roundCount, 1), 12) },
      (_, i) => `<i class="pip" style="--i:${i}"></i>`,
    ).join("");
    const text = call.response
      ? trunc(call.response.replace(/<\/?think>/g, " "), 260)
      : "";
    const tools = (call.tool_executions || []).slice(-4);
    return `
      <div class="live-row clickable" data-k="live:${escAttr(agent.role)}"
           data-action="agent" data-id="${escAttr(agent.id)}"
           style="--phase-color:${phaseColor(phase)}">
        <div class="live-row-top">
          ${avatarFor(agent.role)}
          <span class="live-who" style="color:${roleInk(agent.role)}">${esc(agent.role)}</span>
          <span class="ph-pill" style="color:${phaseInk(phase)}">${esc(phase || "working")}${call.iteration ? " #" + call.iteration : ""}</span>
          <span class="pips is-live" title="${roundCount} round${roundCount === 1 ? "" : "s"}">${pips}</span>
          <span class="live-model">${esc(call.model || "")}</span>
          <span style="flex:1"></span>
          <span class="live-tokens">${fmtNum(call.total_tokens || 0)}</span>
        </div>
        ${text ? `<div class="live-text">${esc(text)}</div>` : '<div class="live-text is-waiting">thinking…</div>'}
        ${
          tools.length
            ? `<div class="live-tools">${tools
                .map(
                  (t) =>
                    `<span class="chip ${t.success === false ? "err" : ""}">${esc(t.name || "tool")}</span>`,
                )
                .join("")}</div>`
            : ""
        }
      </div>`;
  }

  // A seat the engine stopped, and why. This is the counterpart to the
  // live board: an agent that is not working because something broke
  // should be as visible as one that is working.
  function stoppedRow(agent) {
    const failure = agent.last_error || {};
    return `
      <div class="live-row is-failed clickable" data-k="stopped:${escAttr(agent.role)}"
           data-action="agent" data-id="${escAttr(agent.id)}">
        <div class="live-row-top">
          ${avatarFor(agent.role)}
          <span class="live-who" style="color:${roleInk(agent.role)}">${esc(agent.role)}</span>
          <span class="badge failed"><i class="dot"></i>${esc(failureLabel(failure.kind))}</span>
          ${failure.phase ? `<span class="ph-pill" style="color:${phaseInk(failure.phase)}">${esc(failure.phase)}</span>` : ""}
          <span style="flex:1"></span>
          <span class="row-ts">${esc(relTime(failure.at))}</span>
        </div>
        ${failure.message ? `<div class="live-text">${esc(trunc(failure.message, 220))}</div>` : ""}
      </div>`;
  }

  function liveBoard(state) {
    const agents = state.agents || [];
    const live = agents.filter((a) => a.live_call && a.live_call.turn_id !== undefined);
    const stopped = agents.filter((a) => !a.live_call && a.last_error);
    if (!live.length && !stopped.length) {
      return (
        sectionHead("activity", "Live now", 0, null) +
        `<div class="idle-panel">
          <span class="dot idle"></span>
          <div>
            <div class="idle-title">Nothing in flight</div>
            <div class="empty-sub">Every seat is idle. A message, a work item, or a schedule will wake one.</div>
          </div>
        </div>`
      );
    }
    return (
      sectionHead("activity", "Live now", live.length + stopped.length, null) +
      `<div class="live-board">
        ${live.map(liveRow).join("")}
        ${stopped.map(stoppedRow).join("")}
      </div>`
    );
  }

  // Who is on the team and what each one is doing, as cards. Human seats
  // sit in the row alongside agents — they hold seats in the same chart.
  function seats(state) {
    const all = flattenSeats(state.org);
    if (!all.length) return "";
    return (
      sectionHead("users", "The team", all.length, {
        action: "view-agents",
        label: "All agents",
      }) + seatRow(all, { agents: state.agents, sandboxes: state.sandboxes })
    );
  }

  // Fallback for an org with no chart data: the plain agent list.
  function agentsList(state) {
    const agents = state.agents || [];
    if (!agents.length) return empty("user", "No agents running");
    return (
      '<div class="list">' +
      agents
        .map((a) => {
          const st = effectiveAgentState(a, state.sandboxes);
          return `
          <div class="row clickable" data-k="a:${escAttr(a.role || a.id)}" data-action="agent" data-id="${escAttr(a.id)}">
            ${avatarFor(a.role)}
            <div class="row-body">
              <div class="row-title">${esc(a.role || a.name)}</div>
              <div class="row-sub">${esc(statusLine(a, { sandbox: (state.sandboxes || []).find((s) => s.role === a.role) }))}</div>
            </div>
            <span class="badge ${stateBadgeClass(st)}"><i class="dot"></i>${esc(stateLabel(st))}</span>
          </div>`;
        })
        .join("") +
      "</div>"
    );
  }

  function sandboxList(state) {
    const runs = state.sandboxes || [];
    if (!runs.length) return "";
    const rows = runs
      .map((s) => {
        const waiting = s.status === "awaiting_input";
        return `
        <div class="row" data-k="sb:${escAttr(s.turn_id)}">
          ${icon("code", "sm")}
          <div class="row-body">
            <div class="row-title">${esc(s.role || s.agent_handle || "agent")}
              <span class="chip">${esc(s.coding_agent || "coding agent")}</span>
            </div>
            <div class="row-sub">${esc(trunc(waiting ? s.question || "waiting on an answer" : s.task || "", 110))}</div>
          </div>
          <span class="badge ${waiting ? "afk" : "live"}"><i class="dot"></i>${waiting ? "needs an answer" : "running"}</span>
          <span class="row-ts">${esc(relTime(s.started_at))}</span>
        </div>`;
      })
      .join("");
    return (
      sectionHead("cpu", "Running sandboxes", runs.length, null) +
      `<div class="list">${rows}</div>`
    );
  }

  function activityList(state) {
    const events = (state.events || []).slice(0, FEED_ROWS);
    if (!events.length) return empty("activity", "No engine activity yet");
    return `<div class="list feed">${events
      .map(
        (ev) =>
          `<div data-k="e:${escAttr(ev.id)}">${activityRow(ev, { agents: state.agents })}</div>`,
      )
      .join("")}</div>`;
  }

  return {
    slices: ["agents", "events", "sandboxes", "org", "tools", "tokens", "health"],

    render(state) {
      if (!state.connected && !(state.agents || []).length) {
        return skeletonCards(3);
      }
      const hasSeats = flattenSeats(state.org).length > 0;
      return `
        ${widgets(state)}
        ${liveBoard(state)}
        ${seats(state)}
        ${sandboxList(state)}
        ${hasSeats ? "" : sectionHead("users", "Agents", null, null) + agentsList(state)}
        ${sectionHead("activity", "Engine activity", (state.events || []).length, {
          action: "view-events",
          label: "View all",
        })}
        ${activityList(state)}`;
    },
  };
}

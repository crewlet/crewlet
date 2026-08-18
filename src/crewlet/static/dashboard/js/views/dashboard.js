// The overview: is anything happening, did anything break, who is doing
// it, and what did it cost.
//
// The page is built around one lead panel — the company pulse — because
// that is the question the dashboard is opened to answer and the one it
// used to answer worst. It had three counters, a scrolling log, and no
// sense of time at all: an org that had been flat out for twenty minutes
// looked identical to one that had been asleep since yesterday. The
// pulse puts an hour of every seat's real activity on one grid, marks
// failures in red, and folds the counters into a strip beside it so the
// figures cost a corner of the panel instead of the whole fold.
//
// Below it, in order of urgency: what is in flight right now (as a turn
// rail, not a phase name), which seat stopped and why, who is on the
// team, and what just happened.

import { esc, escAttr, fmtCompact, fmtNum, relTime, trunc } from "../format.js";
import {
  afkQuip,
  avatarFor,
  phaseColor,
  phaseInk,
  roleInk,
  staleness,
} from "../state.js";
import { flattenSeats } from "../org.js";
import { seatRow } from "../cards.js";
import { buildPulse, pulseGrid } from "../pulse.js";
import {
  empty,
  phaseBar,
  sectionHead,
  activityRow,
  skeletonCards,
  statStrip,
  turnRail,
} from "../ui.js";
import { failureLabel } from "../llm.js";

// Rows on the overview's activity feed. The full history is one click
// away on Activity; this is a glance, not a log.
const FEED_ROWS = 12;

export function createDashboardView({ store }) {
  // ---- the lead panel ----

  // The headline: seats working, out of seats that could be. Written as
  // a sentence rather than a bare count because "2" on its own is the
  // kind of number that means nothing without its denominator, and this
  // is the first thing on the page.
  function headline(pulse) {
    const total = pulse.rows.length;
    const working = pulse.working;
    if (!total) return { figure: "—", rest: "no agent seats configured" };
    if (working) {
      return { figure: String(working), rest: `of ${total} seats working` };
    }
    return { figure: "0", rest: `of ${total} seats working — all quiet` };
  }

  function hero(state, pulse) {
    const tokens = state.tokens;
    const totals = tokens ? tokens.totals : null;
    const head = headline(pulse);
    const stopped = (state.agents || []).filter(
      (a) => a.state === "afk" || a.last_error,
    ).length;

    // Claim the window the feed can actually speak for, not the nominal
    // one: the projection keeps a bounded number of events, and a busy
    // org fills it in minutes.
    const sub = pulse.total
      ? `${fmtNum(pulse.total)} event${pulse.total === 1 ? "" : "s"} in the last ${pulse.covered} minutes` +
        (pulse.failures
          ? ` · <span class="warn-ink">${fmtNum(pulse.failures)} failed</span>`
          : "") +
        (pulse.blindTo ? " · older activity is past the retained feed" : "")
      : `nothing has happened in the last ${pulse.covered} minutes`;

    return `
      <section class="hero panel dot-texture" data-k="hero">
        <div class="hero-head">
          <div class="hero-lede">
            <div class="eyebrow">Company pulse</div>
            <div class="hero-figure display">
              <span class="num">${esc(head.figure)}</span>
              <span class="hero-rest">${esc(head.rest)}</span>
            </div>
            <div class="hero-sub">${sub}</div>
          </div>
          ${statStrip([
            {
              label: "LLM calls",
              value: totals ? fmtNum(totals.calls || 0) : "—",
              foot: "last 24h",
            },
            {
              label: "Spend",
              value: totals ? fmtCompact(totals.total_tokens) : "—",
              foot: totals
                ? `${fmtCompact(totals.input_tokens)} in · ${fmtCompact(totals.output_tokens)} out`
                : "no turns yet",
            },
            {
              label: "Stopped",
              value: fmtNum(stopped),
              foot: stopped ? "needs attention" : "all seats healthy",
              tone: stopped ? "red" : "",
            },
          ])}
        </div>
        ${pulseGrid(pulse)}
        ${
          totals && totals.total_tokens
            ? `<div class="hero-phases" data-k="hero:phases">${phaseBar(
                tokens.by_phase,
                totals.total_tokens,
              )}</div>`
            : ""
        }
      </section>`;
  }

  // One bucketing pass per render, threaded through to the hero grid and
  // every seat card's strip — deriving it twice is how the two would end
  // up disagreeing about the same seat.
  function pulseFor(state) {
    return buildPulse({
      seats: flattenSeats(state.org),
      agents: state.agents,
      events: state.events,
      tokens: state.tokens,
      sandboxes: state.sandboxes,
    });
  }

  // ---- the live board ----

  // One card per agent mid-turn: the turn as an object, with the phase
  // rail showing where it has been and where it is going, its rounds as
  // pips, and the response as it streams. Everything comes from the
  // server's in-flight call, so it survives a refresh and needs no
  // reconstruction from the event stream.
  function liveCard(agent) {
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
    // How long since this call last moved. A row whose last round landed
    // minutes ago drops its animation and says its age instead — pips
    // that keep pulsing on a hung turn actively claim progress that is
    // not happening.
    const stale = staleness(call.updated_at);
    return `
      <div class="live-row clickable ${stale ? "is-" + stale : ""}" data-k="live:${escAttr(agent.role)}"
           data-action="agent" data-id="${escAttr(agent.id)}"
           style="--phase-color:${phaseColor(phase)}">
        <div class="live-row-top">
          ${avatarFor(agent.role)}
          <span class="live-who" style="color:${roleInk(agent.role)}">${esc(agent.role)}</span>
          <span class="ph-pill" style="color:${phaseInk(phase)}">${esc(phase || "working")}${call.iteration ? " #" + call.iteration : ""}</span>
          <span class="pips ${stale ? "" : "is-live"}" title="${roundCount} round${roundCount === 1 ? "" : "s"}">${pips}</span>
          <span class="live-age" title="last round">${esc(
            stale === "stalled"
              ? `no round in ${relTime(call.updated_at).replace(" ago", "")}`
              : relTime(call.updated_at),
          )}</span>
          <span class="live-model">${esc(call.model || "")}</span>
          <span style="flex:1"></span>
          <span class="live-tokens num">${fmtNum(call.total_tokens || 0)}</span>
        </div>
        ${turnRail(phase)}
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
    // `last_error` is the full record and is what a live failure sets. It
    // does not survive an API restart — the projection hydrates an
    // agent's state and AFK reason from the store, not the payload that
    // caused it — so a seat that is merely known to be AFK still renders
    // here, with the reason it does have. Keying the board on
    // `last_error` alone hid every broken seat after a restart, which is
    // exactly when an operator goes looking for them.
    const failure = agent.last_error || {};
    const kind = failure.kind || agent.afk_reason || "error";
    const message = failure.message || (agent.state === "afk" ? afkQuip(agent.afk_reason) : "");
    return `
      <div class="live-row is-failed clickable" data-k="stopped:${escAttr(agent.role)}"
           data-action="agent" data-id="${escAttr(agent.id)}">
        <div class="live-row-top">
          ${avatarFor(agent.role)}
          <span class="live-who" style="color:${roleInk(agent.role)}">${esc(agent.role)}</span>
          <span class="badge failed"><i class="dot"></i>${esc(failureLabel(kind))}</span>
          ${failure.phase ? `<span class="ph-pill" style="color:${phaseInk(failure.phase)}">${esc(failure.phase)}</span>` : ""}
          <span style="flex:1"></span>
          ${failure.at ? `<span class="row-ts">${esc(relTime(failure.at))}</span>` : ""}
        </div>
        ${message ? `<div class="live-text">${esc(trunc(message, 220))}</div>` : ""}
      </div>`;
  }

  function liveBoard(state) {
    const agents = state.agents || [];
    // A failed call is still *a call* — the projection keeps it on the
    // agent so its detail page can show what died. It is not live, so it
    // belongs in the stopped half of this board, not the running one.
    const isRunning = (a) =>
      a.live_call &&
      a.live_call.turn_id !== undefined &&
      !a.live_call.failed &&
      a.state !== "afk";
    const live = agents.filter(isRunning);
    const stopped = agents.filter(
      (a) => !isRunning(a) && (a.last_error || a.state === "afk"),
    );
    if (!live.length && !stopped.length) return "";
    return (
      sectionHead("activity", "In flight", live.length + stopped.length, null) +
      `<div class="live-board">
        ${live.map(liveCard).join("")}
        ${stopped.map(stoppedRow).join("")}
      </div>`
    );
  }

  // Who is on the team and what each one is doing, as cards. Human seats
  // sit in the row alongside agents — they hold seats in the same chart.
  function seats(state, pulse) {
    const all = flattenSeats(state.org);
    if (!all.length) return "";
    return (
      sectionHead("users", "The team", all.length, {
        action: "view-agents",
        label: "All agents",
      }) +
      seatRow(all, {
        agents: state.agents,
        sandboxes: state.sandboxes,
        pulse,
      })
    );
  }

  // Fallback for an org with no chart data: the live agent list, rendered
  // as the same card so the two paths look alike.
  function agentsList(state, pulse) {
    const agents = state.agents || [];
    if (!agents.length) return empty("user", "No agents running");
    return seatRow(
      agents.map((a) => ({
        name: a.role || a.name,
        handle: a.handle || "",
        kind: "agent",
        integrations: [],
        unitPath: [],
      })),
      { agents, sandboxes: state.sandboxes, pulse },
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
      const pulse = pulseFor(state);
      return `
        ${hero(state, pulse)}
        ${liveBoard(state)}
        ${hasSeats ? seats(state, pulse) : sectionHead("users", "Agents", null, null) + agentsList(state, pulse)}
        ${sandboxList(state)}
        ${sectionHead("activity", "Engine activity", (state.events || []).length, {
          action: "view-events",
          label: "View all",
        })}
        ${activityList(state)}`;
    },
  };
}

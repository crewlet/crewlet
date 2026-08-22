// Mission Control: what needs me, what is happening, who is doing it.
//
// The overview this replaces led with the company pulse, which answers
// "is anything happening" — a good question, and not the one an operator
// arrives with. They arrive with "is anything waiting on me", and the
// dashboard knew the answer in four different rooms: a sandbox parked on
// a question was a badge further down this page, a stopped seat was a
// card among the healthy ones, a budget refusing charges was a bar on a
// seat page, and an engine discarding every webhook was a line in a
// popover.
//
// So the attention queue leads, the pulse follows, and the live board
// carries turns and detached sandbox runs together — "what is running"
// has included both since code work moved into a suspended Execute loop.

import { esc, escAttr, fmtCompact, fmtNum, relTime, trunc } from "../format.js";
import {
  afkQuip,
  avatarFor,
  phaseColor,
  phaseInk,
  staleness,
  seatTone,
} from "../state.js";
import { flattenSeats } from "../org.js";
import { seatRow } from "../cards.js";
import { buildPulse, pulseGrid } from "../pulse.js";
import { buildAttention } from "../attention.js";
import {
  attentionList,
  empty,
  phaseBar,
  sectionHead,
  activityRow,
  skeletonCards,
  statStrip,
  turnRail,
} from "../ui.js";
import { failureLabel, stripThink } from "../llm.js";

// Rows on the overview's activity strip. The full history is one click
// away on Activity; this is a glance, not a log.
const FEED_ROWS = 12;

export function createMissionView({ store }) {
  // ---- the attention queue ----

  function attention(state) {
    const queue = buildAttention(state);
    // The panel is drawn even when it is empty, and that is deliberate:
    // "nothing is waiting on you" is an answer, and a panel that
    // disappears when there is nothing to do teaches the reader to
    // check whether it is there rather than read it.
    return `
      <section class="panel att-panel" data-k="attention">
        <div class="sec">
          <span class="sec-title">Needs you
            ${queue.items.length ? `<span class="sec-count">${queue.items.length}</span>` : ""}
          </span>
        </div>
        ${attentionList(queue)}
      </section>`;
  }

  // ---- the pulse ----

  function headline(pulse, health) {
    const total = pulse.rows.length;
    const working = pulse.working;
    if (!total) {
      // "No agent seats configured" is true of an unconfigured engine
      // too, and it is the wrong sentence for it: it reads as a company
      // whose config simply has no agents in it, which sends the reader
      // looking at YAML that is not being used at all.
      return health && health.configured === false
        ? { figure: "—", rest: "no company configuration is active" }
        : { figure: "—", rest: "no agent seats configured" };
    }
    if (working) {
      return { figure: String(working), rest: `of ${total} seats working` };
    }
    return { figure: "0", rest: `of ${total} seats working — all quiet` };
  }

  function hero(state, pulse) {
    const tokens = state.tokens;
    const totals = tokens ? tokens.totals : null;
    const head = headline(pulse, state.health);
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
            orgBudgetStat(state),
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

  // The org-wide token meter, when an engine is reporting one and the
  // org has a cap at all.
  //
  // `statStrip` drops a falsy entry, so a company with no org cap — or
  // an API with no engine behind it — simply has one fewer figure
  // rather than a tile reading zero. The percentage is honest here for
  // the same reason it is on a seat card: meter and cap cover the same
  // engine run.
  function orgBudgetStat(state) {
    const org = (state.budget || {}).org;
    if (!org || !org.max) return null;
    const pct = Math.round(Math.min(100, (org.used / org.max) * 100));
    return {
      label: "Org budget",
      value: `${pct}%`,
      foot: `${fmtCompact(org.used)} of ${fmtCompact(org.max)} this run`,
      tone: org.refused_at ? "red" : pct >= 75 ? "amber" : "",
    };
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
  function liveCard(agent, sandboxes) {
    const call = agent.live_call;
    const phase = call.phase || agent.current_phase || "";
    const roundCount = call.rounds || 0;
    const pips = Array.from(
      { length: Math.min(Math.max(roundCount, 1), 12) },
      (_, i) => `<i class="pip" style="--i:${i}"></i>`,
    ).join("");
    const text = call.response ? trunc(stripThink(call.response, " "), 260) : "";
    const tools = (call.tool_executions || []).slice(-4);
    // How long since this call last moved. A row whose last round landed
    // minutes ago drops its animation and says its age instead — pips
    // that keep pulsing on a hung turn actively claim progress that is
    // not happening.
    const stale = staleness(call.updated_at);
    return `
      <div class="live-row clickable ${stale ? "is-" + stale : ""}" data-k="live:${escAttr(agent.role)}"
           data-action="seat" data-seat="${escAttr(agent.handle || agent.role)}"
           style="--phase-color:${phaseColor(phase)}">
        <div class="live-row-top">
          ${avatarFor(agent.role, seatTone(agent, sandboxes))}
          <span class="live-who">${esc(agent.role)}</span>
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

  // A detached coding run, on the same board as the turns.
  //
  // A seat running one is idle by design — the engine frees it the moment
  // the run detaches, so the box can take days without holding an agent —
  // which means a board keyed on live turns alone showed a company doing
  // nothing while its most expensive work was under way.
  function sandboxCard(run) {
    const waiting = run.status === "awaiting_input";
    const seat = run.role || run.agent_handle || "agent";
    return `
      <div class="live-row clickable" data-k="sbx:${escAttr(run.turn_id)}"
           data-action="go" data-route="/work"
           style="--phase-color:var(--cyan)">
        <div class="live-row-top">
          ${avatarFor(seat, "needs")}
          <span class="live-who">${esc(seat)}</span>
          <span class="chip">${esc(run.coding_agent || "coding agent")}</span>
          <span class="badge ${waiting ? "afk" : "live"}"><i class="dot"></i>${waiting ? "needs an answer" : "sandbox"}</span>
          <span style="flex:1"></span>
          <span class="row-ts">${esc(relTime(run.started_at))}</span>
        </div>
        <div class="live-text">${esc(
          trunc(waiting ? run.question || "waiting on an answer" : run.task || "writing code", 220),
        )}</div>
      </div>`;
  }

  // A seat the engine stopped, and why. This is the counterpart to the
  // live board: an agent that is not working because something broke
  // should be as visible as one that is working.
  function stoppedRow(agent, sandboxes) {
    // `last_error` is the full record and is what a live failure sets. It
    // does not survive an API restart — the projection hydrates an
    // agent's state and AFK reason from the store, not the payload that
    // caused it — so a seat that is merely known to be AFK still renders
    // here, with the reason it does have. Keying the board on
    // `last_error` alone hid every broken seat after a restart, which is
    // exactly when an operator goes looking for them.
    const failure = agent.last_error || {};
    const kind = failure.kind || agent.afk_reason || "error";
    const message =
      failure.message || (agent.state === "afk" ? afkQuip(agent.afk_reason) : "");
    return `
      <div class="live-row is-failed clickable" data-k="stopped:${escAttr(agent.role)}"
           data-action="seat" data-seat="${escAttr(agent.handle || agent.role)}">
        <div class="live-row-top">
          ${avatarFor(agent.role, seatTone(agent, sandboxes))}
          <span class="live-who">${esc(agent.role)}</span>
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
    const runs = state.sandboxes || [];
    const stopped = agents.filter(
      (a) => !isRunning(a) && (a.last_error || a.state === "afk"),
    );
    const total = live.length + runs.length + stopped.length;
    if (!total) return "";
    return (
      sectionHead("activity", "In flight", total, {
        action: "go",
        label: "Work board",
        route: "/work",
      }) +
      `<div class="live-board">
        ${live.map((a) => liveCard(a, state.sandboxes)).join("")}
        ${runs.map(sandboxCard).join("")}
        ${stopped.map((a) => stoppedRow(a, state.sandboxes)).join("")}
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
        action: "go",
        label: "Org",
        route: "/org",
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

  function activityList(state) {
    const events = (state.events || []).slice(0, FEED_ROWS);
    if (!events.length) return empty("activity", "No engine activity yet");
    return `<div class="list feed">${events
      .map((ev) => activityRow(ev, { agents: state.agents }))
      .join("")}</div>`;
  }

  return {
    slices: [
      "agents",
      "events",
      "sandboxes",
      "org",
      "tools",
      "tokens",
      "budget",
      "health",
      "connected",
    ],

    render(state) {
      if (!state.connected && !(state.agents || []).length) {
        return skeletonCards(3);
      }
      const hasSeats = flattenSeats(state.org).length > 0;
      const pulse = pulseFor(state);
      return `
        ${attention(state)}
        ${hero(state, pulse)}
        ${liveBoard(state)}
        ${hasSeats ? seats(state, pulse) : sectionHead("users", "Agents", null, null) + agentsList(state, pulse)}
        ${sectionHead("activity", "Engine activity", (state.events || []).length, {
          action: "go",
          label: "View all",
          route: "/activity",
        })}
        ${activityList(state)}`;
    },
  };
}

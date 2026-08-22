// Work: every unit of work in flight, including the ones nothing else
// can see.
//
// Detached coding runs are the most expensive thing this engine does and
// had one panel on the overview, showing only the runs the in-memory
// projection still held. That projection sweeps an entry after twelve
// hours and a parked question can legitimately wait days, so the states
// that most need a person were the ones least likely to be on screen:
//
//   awaiting_clarification  the box is paused and being billed for, and
//                           somebody has to answer before it moves
//   reseed                  the pause outlived its TTL, the box was
//                           reclaimed, and the run will re-seed from its
//                           pushed branch when the answer arrives
//
// A `reseed` run had no surface anywhere: no box, no busy seat, nothing
// in the feed. It looked exactly like work that had finished.
//
// The durable rows come from the pending-run store over the `sandbox_runs`
// query; live turns come from the pushed projection.

import { esc, escAttr, fmtNum, relTime, trunc } from "../format.js";
import {
  avatarFor,
  phaseColor,
  phaseInk,
  staleness,
  seatTone,
} from "../state.js";
import {
  empty,
  emptyOrPending,
  sectionHead,
  skeletonRows,
  turnRail,
} from "../ui.js";
import { stripThink } from "../llm.js";

// How often to re-read the durable rows. A parked run changes when a
// human answers somewhere else entirely — in Slack, on an issue — so
// there is no event on this socket to wait for.
const POLL_MS = 20_000;

// What each durable status means, in the operator's terms rather than
// the column's. The distinction that matters is whether a box is still
// up: a paused box is being billed for a snapshot, a reclaimed one is
// not, and both are waiting on the same answer.
const STATUS = {
  running: { label: "running", cls: "live", note: "" },
  awaiting_clarification: {
    label: "needs an answer",
    cls: "afk",
    note: "box held — the snapshot is being billed for",
  },
  reseed: {
    label: "needs an answer",
    cls: "afk",
    note: "box reclaimed — will re-seed from the pushed branch",
  },
  resumed: { label: "resuming", cls: "live", note: "" },
};

export function createWorkView({ query, refresh }) {
  let runs = null;
  let loadError = "";
  let timer = null;
  let disposed = false;

  async function load() {
    try {
      const data = await query("sandbox_runs");
      runs = data && Array.isArray(data.runs) ? data.runs : [];
      loadError = "";
    } catch (err) {
      // The durable half is unavailable — an engine with no database, or
      // a query this build does not answer. The live half still renders;
      // say which one is missing rather than showing a bare empty board.
      runs = runs || [];
      loadError = err && err.message === "no_pending_store" ? "no_store" : "failed";
    }
    if (!disposed) refresh();
  }

  // ---- live turns ----

  function liveTurn(agent, sandboxes) {
    const call = agent.live_call;
    const phase = call.phase || agent.current_phase || "";
    const stale = staleness(call.updated_at);
    const text = call.response ? trunc(stripThink(call.response, " "), 200) : "";
    return `
      <div class="live-row clickable ${stale ? "is-" + stale : ""}"
           data-k="turn:${escAttr(agent.role)}"
           data-action="seat" data-seat="${escAttr(agent.handle || agent.role)}"
           style="--phase-color:${phaseColor(phase)}">
        <div class="live-row-top">
          ${avatarFor(agent.role, seatTone(agent, sandboxes))}
          <span class="live-who">${esc(agent.role)}</span>
          <span class="ph-pill" style="color:${phaseInk(phase)}">${esc(phase || "working")}${call.iteration ? " #" + call.iteration : ""}</span>
          <span class="live-age">${esc(
            stale === "stalled"
              ? `no round in ${relTime(call.updated_at).replace(" ago", "")}`
              : relTime(call.updated_at),
          )}</span>
          <span style="flex:1"></span>
          <span class="live-tokens num">${fmtNum(call.total_tokens || 0)}</span>
        </div>
        ${turnRail(phase)}
        ${text ? `<div class="live-text">${esc(text)}</div>` : '<div class="live-text is-waiting">thinking…</div>'}
      </div>`;
  }

  // ---- sandbox runs ----

  function runRow(run) {
    const status = STATUS[run.status] || { label: run.status, cls: "", note: "" };
    const waiting = run.status === "awaiting_clarification" || run.status === "reseed";
    const seat = run.role || run.agent_handle || "agent";
    const headline = waiting
      ? run.question || "waiting on an answer"
      : run.task_description || "writing code";
    return `
      <div class="work-run ${waiting ? "is-waiting" : ""}" data-k="run:${escAttr(run.turn_id)}">
        <div class="work-run-top">
          ${avatarFor(seat, "needs")}
          <span class="live-who">${esc(seat)}</span>
          <span class="chip">${esc(run.coding_agent || "coding agent")}</span>
          ${run.branch ? `<span class="chip mono">${esc(run.branch)}</span>` : ""}
          <span class="badge ${status.cls}"><i class="dot"></i>${esc(status.label)}</span>
          <span style="flex:1"></span>
          <span class="row-ts">${esc(relTime(run.started_at || run.updated_at))}</span>
        </div>
        <div class="work-run-body">${esc(trunc(headline, 280))}</div>
        <div class="work-run-foot">
          ${status.note ? `<span class="caution-ink">${esc(status.note)}</span>` : ""}
          ${
            run.audience
              ? `<span class="work-meta">asked ${esc(run.audience)}</span>`
              : ""
          }
          ${
            run.owner
              ? `<span class="work-meta mono" title="the node holding this run">${esc(run.owner)}</span>`
              : ""
          }
          ${waiting ? answerHint(run) : ""}
        </div>
      </div>`;
  }

  // Where an answer can actually come from.
  //
  // A run kicked off by anything other than an external notification —
  // a schedule, a task assignment, an A2A wake — stored a conversation
  // key no inbound message can ever reproduce, so no chat reply will
  // ever match it. Saying "reply in the thread" there would send
  // somebody to a thread that does not exist.
  function answerHint(run) {
    if (run.answerable_in_chat) {
      return `<span class="work-meta">answer by replying where the task came from</span>`;
    }
    return `<span class="work-meta caution-ink">no conversation to reply in — this run was started by a schedule or an internal trigger</span>`;
  }

  function runsSection(state) {
    if (runs === null) return sectionHead("cpu", "Sandbox runs") + skeletonRows(3);
    const waiting = runs.filter(
      (r) => r.status === "awaiting_clarification" || r.status === "reseed",
    );
    const active = runs.filter(
      (r) => r.status === "running" || r.status === "resumed",
    );
    const body = [];
    if (waiting.length) {
      body.push(
        sectionHead("question", "Waiting on an answer", waiting.length),
        `<div class="work-list">${waiting.map(runRow).join("")}</div>`,
      );
    }
    if (active.length) {
      body.push(
        sectionHead("cpu", "Running", active.length),
        `<div class="work-list">${active.map(runRow).join("")}</div>`,
      );
    }
    if (!body.length) {
      body.push(
        sectionHead("cpu", "Sandbox runs", 0),
        loadError === "no_store"
          ? empty(
              "cpu",
              "No pending-run store",
              "Detached sandbox runs are held in PostgreSQL. Without a database the engine cannot park one, so there is nothing durable to list.",
            )
          : loadError
            ? empty(
                "cpu",
                "Could not read sandbox runs",
                "The query failed — retrying. Any run below is from the last successful read.",
              )
            : emptyOrPending(
                state,
                skeletonRows(2),
                "cpu",
                "No sandbox runs",
                "Code work runs here when a role has `sandbox.enabled` and its plan calls `run_sandbox`.",
              ),
      );
    }
    return body.join("");
  }

  function turnsSection(state) {
    const live = (state.agents || []).filter(
      (a) => a.live_call && a.live_call.turn_id !== undefined && !a.live_call.failed,
    );
    if (!live.length) return "";
    return (
      sectionHead("activity", "Live turns", live.length) +
      `<div class="live-board">${live.map((a) => liveTurn(a, state.sandboxes)).join("")}</div>`
    );
  }

  return {
    slices: ["agents", "sandboxes", "health", "connected"],

    mount() {
      load();
      // A parked run changes when a human answers somewhere else
      // entirely — in Slack, on an issue — so there is no event on this
      // socket to wait for.
      timer = setInterval(load, POLL_MS);
    },

    destroy() {
      disposed = true;
      if (timer) clearInterval(timer);
      timer = null;
    },

    render(state) {
      return `
        ${turnsSection(state)}
        ${runsSection(state)}`;
    },

    __loadForTest: load,
  };
}

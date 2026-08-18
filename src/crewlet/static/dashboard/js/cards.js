// The seat card — one agent or human, with what it is doing right now.
//
// Shared by the overview's seat row and the Agents index so the two never
// drift. Everything on the card comes from live state or the org config;
// nothing is inferred to fill space.
//
// The card reads top-down as a sentence: who this is, what it is doing,
// what it has been doing for the last hour, and what that cost. The
// activity strip is the same device as the overview's pulse grid, built
// from the same bucketing pass, so a card and the hero can never tell
// different stories about the same seat.

import { esc, escAttr, fmtCompact, relTime, trunc } from "./format.js";
import { icon } from "./icons.js";
import { pulseSpark } from "./pulse.js";
import {
  avatarFor,
  effectiveAgentState,
  integrationMeta,
  phaseInk,
  roleInk,
  stateBadgeClass,
  stateLabel,
  statusLine,
} from "./state.js";

/** Integration chips, from the MCP servers actually wired to the seat. */
function integrationChips(keys) {
  return (keys || [])
    .map((k) => {
      const m = integrationMeta(k);
      return `<span class="int-badge sm" style="--int-color:${m.color}">${icon(
        m.icon,
        "sm",
      )}${esc(m.label)}</span>`;
    })
    .join("");
}

/**
 * Render one seat card.
 *
 * `seat` is a flattened org seat (see org.js); `agent` is its live row from
 * the projection (absent for human seats and for agents the engine has not
 * spawned); `sandbox` is its in-flight detached run, when it has one;
 * `pulseRow` / `pulseMax` are this seat's row from `buildPulse` and the
 * grid's shared ceiling, when the caller has built one.
 */
export function seatCard(seat, {
  agent,
  sandbox,
  sandboxes,
  selected,
  pulseRow,
  pulseMax,
} = {}) {
  const human = seat.kind === "human";
  const live = agent ? { ...agent, ...seat } : seat;
  const state = human ? "human" : agent ? effectiveAgentState(agent, sandboxes) : "offline";
  const status = statusLine(live, { sandbox, human });
  const nav =
    agent && !human
      ? `data-action="agent" data-id="${escAttr(agent.id)}"`
      : "";

  // The right-hand marker: a human seat is not run by the engine, so it
  // gets its unit rather than a run state it can never be in. A working
  // agent gets its phase, which is the one thing a state badge cannot
  // say and the reader most wants.
  // Keyed on the EFFECTIVE state, not the raw one: a seat with a detached
  // sandbox run in flight reads as `awaiting_sandbox`, and its stored
  // phase is whatever the kick-off turn last ran. Showing that phase put
  // "REVIEW #1" on a card whose own status line said "writing code in a
  // sandbox" — two answers to one question, on one card.
  const phase = state === "working" ? (agent && agent.current_phase) || "" : "";
  const marker = human
    ? `<span class="badge text">${esc(seat.unit || "human")}</span>`
    : phase
      ? `<span class="ph-pill" style="color:${phaseInk(phase)}">${esc(phase)}${
          agent.current_iteration ? " #" + agent.current_iteration : ""
        }</span>`
      : `<span class="badge ${stateBadgeClass(state)}"><i class="dot"></i>${esc(stateLabel(state))}</span>`;

  // The cost line. A human seat spends nothing and an agent that has not
  // run yet has nothing to report — both say so by omission rather than
  // by rendering a zero that looks like a measurement.
  //
  // The cap is stated, never turned into a percentage: the engine meters
  // a role's budget cumulatively for the process lifetime
  // (`concurrency.BudgetManager`), while the figure beside it is the
  // dashboard's 24-hour spend window. Dividing one by the other would
  // read as "how much budget is left" and be wrong by however long the
  // engine has been up. Side by side, the orders of magnitude are
  // comparable and nothing is claimed that was not measured.
  const meta = [];
  if (pulseRow && pulseRow.tokens) meta.push(`${fmtCompact(pulseRow.tokens)} tokens`);
  if (!human && seat.tokenBudget) meta.push(`cap ${fmtCompact(seat.tokenBudget)}`);
  if (pulseRow && pulseRow.lastAt) {
    meta.push(relTime(new Date(pulseRow.lastAt).toISOString()));
  }

  const cls = [
    "seat-card",
    nav ? "clickable" : "",
    selected ? "selected" : "",
    !human && (state === "afk" || (agent && agent.last_error)) ? "is-failed" : "",
  ]
    .filter(Boolean)
    .join(" ");

  return `
    <div class="${cls}" data-k="seat:${escAttr(seat.name)}"
         ${nav} style="--seat:${roleInk(seat.name)}" title="${escAttr(seat.name)}">
      <div class="seat-top">
        <span class="seat-state dot ${esc(state)}"></span>
        ${avatarFor(seat.name)}
        <span class="seat-name" style="color:${roleInk(seat.name)}">${esc(seat.name)}</span>
        ${marker}
      </div>
      <div class="seat-status">${esc(trunc(status, 96))}</div>
      ${pulseRow ? pulseSpark(pulseRow, pulseMax) : ""}
      <div class="seat-foot">
        <span class="seat-badges">${integrationChips(seat.integrations)}</span>
        ${meta.length ? `<span class="seat-meta">${esc(meta.join(" · "))}</span>` : ""}
      </div>
    </div>`;
}

/**
 * The seat row: root-level seats first, then each unit's, in config order —
 * the same order the org chart reads top to bottom.
 */
export function seatRow(seats, { agents, sandboxes, selected, pulse } = {}) {
  if (!seats.length) return "";
  const byRole = new Map((agents || []).map((a) => [a.role || a.name, a]));
  const sbByRole = new Map((sandboxes || []).map((s) => [s.role, s]));
  const pulseByRole = new Map(((pulse && pulse.rows) || []).map((r) => [r.name, r]));
  const cards = seats
    .map((s) =>
      seatCard(s, {
        agent: byRole.get(s.name),
        sandbox: sbByRole.get(s.name),
        sandboxes,
        selected: selected === s.name,
        pulseRow: pulseByRole.get(s.name),
        pulseMax: pulse ? pulse.max : 1,
      }),
    )
    .join("");
  return `<div class="seat-grid">${cards}</div>`;
}

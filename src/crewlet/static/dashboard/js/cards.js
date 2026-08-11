// The seat card — one agent or human, with what it is doing right now.
//
// Shared by the overview's seat row and the Agents index so the two never
// drift. Everything on the card comes from live state or the org config;
// nothing is inferred to fill space.

import { esc, escAttr, trunc } from "./format.js";
import { icon } from "./icons.js";
import {
  avatarFor,
  effectiveAgentState,
  integrationMeta,
  roleInk,
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
 * spawned); `sandbox` is its in-flight detached run, when it has one.
 */
export function seatCard(seat, { agent, sandbox, sandboxes, selected } = {}) {
  const human = seat.kind === "human";
  const live = agent ? { ...agent, ...seat } : seat;
  const state = human ? "human" : agent ? effectiveAgentState(agent, sandboxes) : "offline";
  const status = statusLine(live, { sandbox, human });
  const nav =
    agent && !human
      ? `data-action="agent" data-id="${escAttr(agent.id)}"`
      : "";

  return `
    <div class="seat-card ${nav ? "clickable" : ""} ${selected ? "selected" : ""}"
         ${nav} title="${escAttr(seat.name)}">
      <span class="seat-state dot ${esc(state)}"></span>
      ${avatarFor(seat.name)}
      <div class="seat-name" style="color:${roleInk(seat.name)}">${esc(seat.name)}</div>
      <div class="seat-badges">${integrationChips(seat.integrations)}</div>
      <div class="seat-status">${esc(trunc(status, 72))}</div>
    </div>`;
}

/**
 * The seat row: root-level seats first, then each unit's, in config order —
 * the same order the org chart reads top to bottom.
 */
export function seatRow(seats, { agents, sandboxes, selected } = {}) {
  if (!seats.length) return "";
  const byRole = new Map((agents || []).map((a) => [a.role || a.name, a]));
  const sbByRole = new Map((sandboxes || []).map((s) => [s.role, s]));
  const cards = seats
    .map((s) =>
      seatCard(s, {
        agent: byRole.get(s.name),
        sandbox: sbByRole.get(s.name),
        sandboxes,
        selected: selected === s.name,
      }),
    )
    .join("");
  return `<div class="seat-row">${cards}</div>`;
}

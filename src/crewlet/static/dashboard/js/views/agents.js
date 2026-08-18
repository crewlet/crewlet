// Agents index: every agent seat as a card, with what it is doing now.

import { flattenSeats } from "../org.js";
import { seatCard } from "../cards.js";
import { buildPulse } from "../pulse.js";
import { empty, sectionHead, skeletonCards } from "../ui.js";

// Seat order on the index: what needs attention, then what is happening,
// then everything else.
const STATE_RANK = {
  afk: 0,
  working: 1,
  awaiting_sandbox: 1,
  idle: 2,
  offline: 3,
  terminated: 4,
};

function rank(agent) {
  if (!agent) return STATE_RANK.offline;
  if (agent.last_error) return -1;
  const r = STATE_RANK[agent.state];
  return r === undefined ? STATE_RANK.offline : r;
}

export function createAgentsView({ store }) {
  return {
    slices: ["agents", "org", "sandboxes", "events", "tokens", "budget", "health"],

    render(state) {
      const agents = state.agents || [];
      // Prefer org seats (they carry the unit chain + integrations); fall
      // back to the live agent list so the view works before /org lands.
      const seats = flattenSeats(state.org).filter((s) => s.kind === "agent");
      const list = (seats.length
        ? [...seats]
        : agents.map((a) => ({
            name: a.role || a.name,
            handle: a.handle || "",
            kind: "agent",
            integrations: [],
            unitPath: [],
          })));
      if (!list.length) {
        return state.connected
          ? empty("user", "No agents configured")
          : skeletonCards(6);
      }
      const byRole = new Map(agents.map((a) => [a.role || a.name, a]));
      const sandboxByRole = new Map((state.sandboxes || []).map((s) => [s.role, s]));
      const working = agents.filter((a) => a.state === "working").length;
      // Urgency order, not config order: on a thirty-seat org the one
      // broken seat is wherever the YAML happens to put it, which is the
      // worst possible place for it. Ties keep config order (a stable
      // sort), so the grid still reads like the org chart within a band.
      list.sort((a, b) => rank(byRole.get(a.name)) - rank(byRole.get(b.name)));
      // The same hour of activity the overview's pulse grid shows, so a
      // card means the same thing on both screens.
      const pulse = buildPulse({
        seats,
        agents,
        events: state.events,
        tokens: state.tokens,
        sandboxes: state.sandboxes,
      });
      const pulseByRole = new Map(pulse.rows.map((r) => [r.name, r]));

      return (
        sectionHead("users", "Agents", `${working} active`, null) +
        `<div class="seat-grid">${list
          .map((seat) =>
            seatCard(seat, {
              agent: byRole.get(seat.name),
              sandbox: sandboxByRole.get(seat.name),
              sandboxes: state.sandboxes,
              pulseRow: pulseByRole.get(seat.name),
              pulseMax: pulse.max,
            }),
          )
          .join("")}</div>`
      );
    },
  };
}

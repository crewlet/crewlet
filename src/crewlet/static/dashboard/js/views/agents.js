// Agents index: every agent seat as a card, with what it is doing now.

import { flattenSeats } from "../org.js";
import { seatCard } from "../cards.js";
import { empty, sectionHead, skeletonCards } from "../ui.js";

export function createAgentsView({ store }) {
  return {
    slices: ["agents", "org", "sandboxes", "health"],

    render(state) {
      const agents = state.agents || [];
      // Prefer org seats (they carry the unit chain + integrations); fall
      // back to the live agent list so the view works before /org lands.
      const seats = flattenSeats(state.org).filter((s) => s.kind === "agent");
      const list = seats.length
        ? seats
        : agents.map((a) => ({
            name: a.role || a.name,
            handle: a.handle || "",
            kind: "agent",
            integrations: [],
            unitPath: [],
          }));
      if (!list.length) {
        return state.connected
          ? empty("user", "No agents configured")
          : skeletonCards(6);
      }
      const byRole = new Map(agents.map((a) => [a.role || a.name, a]));
      const sandboxByRole = new Map((state.sandboxes || []).map((s) => [s.role, s]));
      const working = agents.filter((a) => a.state === "working").length;

      return (
        sectionHead("users", "Agents", `${working} active`, null) +
        `<div class="seat-grid">${list
          .map((seat) =>
            seatCard(seat, {
              agent: byRole.get(seat.name),
              sandbox: sandboxByRole.get(seat.name),
              sandboxes: state.sandboxes,
            }),
          )
          .join("")}</div>`
      );
    },
  };
}

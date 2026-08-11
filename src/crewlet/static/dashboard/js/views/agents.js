// Agents index: every agent seat as a card, with what it is doing now.

import { flattenSeats } from "../org.js";
import { seatCard } from "../cards.js";
import { empty, sectionHead, skeletonCards } from "../ui.js";

export function createAgentsView({ store }) {
  let root;

  function render(state) {
    const agents = state.agents || [];
    // Prefer org seats (they carry the unit chain + integrations); fall back
    // to the live agent list so the view still works before /org lands.
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
      root.innerHTML = empty("user", "No agents configured");
      return;
    }
    const byRole = new Map(agents.map((a) => [a.role || a.name, a]));
    const sbByRole = new Map((state.sandboxes || []).map((s) => [s.role, s]));
    const working = agents.filter((a) => a.state === "working").length;

    root.innerHTML =
      sectionHead("users", "Agents", `${working} active`, null) +
      `<div class="seat-grid">${list
        .map((s) =>
          seatCard(s, {
            agent: byRole.get(s.name),
            sandbox: sbByRole.get(s.name),
            sandboxes: state.sandboxes,
          }),
        )
        .join("")}</div>`;
  }

  return {
    mount(el) {
      root = el;
      if (!store.state.agents.length) root.innerHTML = skeletonCards(6);
      render(store.state);
    },
    update(state) {
      if (root && root.isConnected) render(state);
    },
  };
}

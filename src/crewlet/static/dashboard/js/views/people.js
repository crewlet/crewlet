// People directory: every seat in the org chart, agents and humans alike,
// with the identities each one is reachable at.

import { esc, escAttr, trunc } from "../format.js";
import { icon } from "../icons.js";
import {
  avatarFor,
  effectiveAgentState,
  integrationMeta,
  roleInk,
  CONTACT_FIELDS,
} from "../state.js";
import { flattenSeats, managerOf } from "../org.js";
import { empty, sectionHead, skeletonRows } from "../ui.js";

export function createPeopleView({ store }) {
  let root;
  let filter = "all";

  function contactChips(seat) {
    const out = [];
    for (const [field, key] of CONTACT_FIELDS) {
      const v = seat.contact && seat.contact[field];
      if (!v) continue;
      const m = integrationMeta(key);
      out.push(
        `<span class="int-badge sm" style="--int-color:${m.color}" title="${escAttr(field)}">${icon(
          m.icon,
          "sm",
        )}${esc(v)}</span>`,
      );
    }
    if (seat.email) {
      out.push(`<span class="chip">${esc(seat.email)}</span>`);
    }
    return out.join("");
  }

  function seatRow(seat, seats, agents, sandboxes) {
    const agent = agents.find((a) => (a.role || a.name) === seat.name);
    const human = seat.kind === "human";
    const state = human ? "human" : agent ? effectiveAgentState(agent, sandboxes) : "offline";
    const badge = human
      ? '<span class="badge purple">human</span>'
      : `<span class="badge ${state === "working" ? "live" : state === "idle" ? "done" : state === "afk" ? "afk" : "pending"}"><i class="dot"></i>${esc(state)}</span>`;
    const nav =
      agent && !human ? `class="row clickable" data-action="agent" data-id="${escAttr(agent.id)}"` : 'class="row"';
    const manager = managerOf(seats, seat.name);
    const where = [seat.unitPath.join(" › "), manager ? `reports to ${manager}` : ""]
      .filter(Boolean)
      .join(" · ");

    return `
      <div ${nav}>
        ${avatarFor(seat.name)}
        <div class="row-body">
          <div class="row-title" style="color:${roleInk(seat.name)}">${esc(seat.name)}
            <span class="org-role-handle">@${esc(seat.handle)}</span></div>
          <div class="row-sub">${esc(where || "root level")}</div>
          ${seat.goal ? `<div class="row-sub">${esc(trunc(seat.goal, 96))}</div>` : ""}
          <div class="chip-row">${contactChips(seat)}</div>
        </div>
        ${seat.availability ? `<span class="badge text">${esc(trunc(seat.availability, 28))}</span>` : ""}
        ${badge}
      </div>`;
  }

  function render(state) {
    const seats = flattenSeats(state.org);
    if (!seats.length) {
      root.innerHTML = empty("users", "No seats configured");
      return;
    }
    const agents = state.agents || [];
    const shown = seats.filter((s) => filter === "all" || s.kind === filter);
    const counts = {
      all: seats.length,
      agent: seats.filter((s) => s.kind === "agent").length,
      human: seats.filter((s) => s.kind === "human").length,
    };
    const pill = (key, label) =>
      `<span class="pill ${filter === key ? "active" : ""}" data-action="people-filter" data-k="${key}">${label} <span class="ct">${counts[key]}</span></span>`;

    root.innerHTML = `
      ${sectionHead("users", "People directory", seats.length, null)}
      <div class="filters">
        ${pill("all", "Everyone")}${pill("agent", "Agents")}${pill("human", "Humans")}
      </div>
      <div class="list">${shown
        .map((s) => seatRow(s, seats, agents, state.sandboxes))
        .join("")}</div>`;
  }

  return {
    mount(el) {
      root = el;
      if (!store.state.org || !store.state.org.name) root.innerHTML = skeletonRows(6);
      render(store.state);
    },
    update(state) {
      if (root && root.isConnected) render(state);
    },
    onAction(action, t) {
      if (action === "people-filter") {
        filter = t.dataset.k;
        render(store.state);
      }
    },
  };
}

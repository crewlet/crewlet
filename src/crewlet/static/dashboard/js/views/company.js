// Company overview: who this org is, and its shape at a glance.

import { esc, escAttr, fmtNum } from "../format.js";
import { icon } from "../icons.js";
import { flattenSeats, flattenUnits } from "../org.js";
import { emptyOrPending, sectionHead, skeletonCards } from "../ui.js";

// One figure in the header grid. Keyed by its label: the set is fixed and
// the label is what identifies a tile across renders, so a changing value
// rewrites one number instead of the whole grid.
function stat(label, value, sub) {
  return `
      <div class="stat" data-k="stat:${escAttr(label)}">
        <div class="stat-label">${esc(label)}</div>
        <div class="stat-value">${esc(String(value))}</div>
        ${sub ? `<div class="stat-sub">${esc(sub)}</div>` : ""}
      </div>`;
}

export function createCompanyView({ store }) {
  return {
    // Pure store read: the org tree for the company's shape, the live agent
    // list for how much of it the engine is actually running.
    slices: ["org", "agents", "health"],

    render(state) {
      const org = state.org || {};
      if (!org.name && !(org.units || []).length && !(org.roles || []).length) {
        return emptyOrPending(
          state,
          () => skeletonCards(4),
          "building",
          "No company configured",
          "Import a company config with `crewlet config import`.",
        );
      }

      const seats = flattenSeats(org);
      const units = flattenUnits(org);
      const humans = seats.filter((s) => s.kind === "human");
      const agents = seats.filter((s) => s.kind === "agent");
      const running = (state.agents || []).filter((a) => a.state !== "offline").length;
      const policies = org.policies || [];

      return `
      <div class="card company-hero">
        <div class="eyebrow">Company</div>
        <h2 class="company-name">${esc(org.name || "Unnamed company")}</h2>
        ${org.mission ? `<p class="company-line">${esc(org.mission)}</p>` : ""}
        ${org.vision ? `<p class="company-line muted">${esc(org.vision)}</p>` : ""}
      </div>

      <div class="stat-grid" style="margin-top:16px">
        ${stat("Seats", fmtNum(seats.length), `${agents.length} agent · ${humans.length} human`)}
        ${stat("Units", fmtNum(units.length), units.length ? `${units.filter((u) => !u.depth).length} at the top` : "flat org")}
        ${stat("Spawned", fmtNum(running), "agents the engine is running")}
        ${stat("Policies", fmtNum(policies.length), policies.length ? "org-wide" : "none set")}
      </div>

      ${
        policies.length
          ? sectionHead("clipboard", "Policies", policies.length, null) +
            // A policy has no id — its own text is its identity.
            `<div class="list" data-k="policies">${policies
              .map(
                (p) => `<div class="row" data-k="policy:${escAttr(p)}"><span class="row-icon">${icon("check", "sm")}</span>
                  <div class="row-body"><div class="row-title" style="font-weight:500">${esc(p)}</div></div></div>`,
              )
              .join("")}</div>`
          : ""
      }

      ${
        units.length
          ? sectionHead("folder", "Units", units.length, { action: "view-org", label: "Org chart" }) +
            // Unit names are unique across the tree, so the name keys the row
            // even as a unit moves depth under a live config revision.
            `<div class="list" data-k="units">${units
              .map(
                (u) => `
              <div class="row" data-k="unit:${escAttr(u.name)}">
                <span class="row-icon" style="margin-left:${u.depth * 16}px">${icon("folder", "sm")}</span>
                <div class="row-body">
                  <div class="row-title">${esc(u.name)} <span class="unit-type">${esc(u.type)}</span></div>
                  ${u.purpose ? `<div class="row-sub">${esc(u.purpose)}</div>` : ""}
                </div>
                ${u.lead ? `<span class="badge">lead · ${esc(u.lead)}</span>` : ""}
                <span class="row-ts">${fmtNum(u.seats)} seat${u.seats === 1 ? "" : "s"}</span>
              </div>`,
              )
              .join("")}</div>`
          : ""
      }`;
    },
  };
}

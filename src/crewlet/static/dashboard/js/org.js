// Org-tree walking, shared by every view that needs seats rather than the
// raw `/org` payload.
//
// `/org` returns the company config's tree verbatim: root-level `roles`, plus
// `units` nesting to any depth, each with its own `roles` and `children`.
// Views want the *flat* answer — every seat with the unit chain it sits in,
// who leads that unit, and the MCP surfaces it inherits — so the walk lives
// here once instead of being re-derived per view.

import { contactIntegrations, seatIntegrations } from "./state.js";

function slugify(name) {
  return String(name || "")
    .toLowerCase()
    .replace(/[^a-z0-9]+/g, "-")
    .replace(/^-|-$/g, "");
}

/**
 * Flatten an `/org` payload into seats.
 *
 * Each seat carries:
 *   name, handle, kind ("agent" | "human"), goal, backstory, email,
 *   manages[], contact{}, availability,
 *   unit        — the containing unit's name ("" for root-level seats)
 *   unitPath[]  — every ancestor unit name, outermost first
 *   lead        — the effective lead of the containing unit (inherited from
 *                 the nearest ancestor that sets one, matching the engine)
 *   integrations[] — MCP server keys, own + inherited from the unit chain
 */
export function flattenSeats(org) {
  const seats = [];

  const emit = (role, unitPath, lead, envChain) => {
    if (!role || !role.name) return;
    const human = role.kind === "human";
    seats.push({
      name: role.name,
      handle: role.handle || slugify(role.name),
      kind: human ? "human" : "agent",
      goal: role.goal || "",
      backstory: role.backstory || "",
      email: role.email || "",
      manages: role.manages || [],
      contact: role.contact || {},
      availability: role.availability || "",
      unit: unitPath.length ? unitPath[unitPath.length - 1] : "",
      unitPath: [...unitPath],
      lead,
      // A human seat carries no MCP credentials — show where they are
      // reachable instead of an empty chip row.
      integrations: human
        ? contactIntegrations(role.contact)
        : seatIntegrations([...envChain, role.mcp_env]),
    });
  };

  const walk = (unit, path, inheritedLead, envChain) => {
    if (!unit) return;
    const nextPath = [...path, unit.name];
    // Lead inheritance: a child unit with no `lead` takes its parent's.
    const lead = unit.lead || inheritedLead;
    const nextEnv = [...envChain, unit.mcp_env];
    for (const role of unit.roles || []) emit(role, nextPath, lead, nextEnv);
    for (const child of unit.children || []) walk(child, nextPath, lead, nextEnv);
  };

  for (const role of (org && org.roles) || []) emit(role, [], "", []);
  for (const unit of (org && org.units) || []) walk(unit, [], "", []);
  return seats;
}

/** Every unit in the tree, flattened, with its depth and seat count. */
export function flattenUnits(org) {
  const out = [];
  const walk = (unit, depth) => {
    if (!unit) return;
    out.push({
      name: unit.name,
      type: unit.type || "unit",
      purpose: unit.purpose || "",
      lead: unit.lead || "",
      depth,
      seats: (unit.roles || []).length,
    });
    for (const child of unit.children || []) walk(child, depth + 1);
  };
  for (const unit of (org && org.units) || []) walk(unit, 0);
  return out;
}

/** The seat that manages `name`, or "" when nobody claims it. */
export function managerOf(seats, name) {
  const direct = seats.find((s) => (s.manages || []).includes(name));
  if (direct) return direct.name;
  // Not named explicitly: a unit lead auto-manages its own unit's seats.
  const seat = seats.find((s) => s.name === name);
  if (seat && seat.lead && seat.lead !== name) return seat.lead;
  return "";
}

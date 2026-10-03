/**
 * The org the live chart's suites share: a company shaped like the approved
 * chart, with its derived hierarchy.
 */

import type { OrgProjection, OrgSeat, OrgUnit } from "~/protocol/types.ts";

/** A founder above everything; a CEO and CTO in Leadership · Executives; two
 *  engineers in Engineering · Core, which the CTO leads from outside it; a PM
 *  in Product · Management, and DevRel in Product · Developer Relations whose
 *  lead the PM is by inheritance. */
export const CHART_ORG = {
  name: "Acme",
  roles: [
    { name: "Jane Founder", handle: "jane", kind: "human", availability: "reviews releases" },
  ],
  units: [
    {
      name: "Leadership",
      children: [{ name: "Executives", lead: "CEO", roles: [{ name: "CEO" }, { name: "CTO" }] }],
    },
    { name: "Engineering", children: [{ name: "Core", roles: [{ name: "SWE" }, { name: "FE" }] }] },
    {
      name: "Product",
      children: [
        { name: "Management", lead: "PM", roles: [{ name: "PM" }] },
        { name: "Developer Relations", roles: [{ name: "DevRel" }] },
      ],
    },
  ],
  derived: {
    seats: [
      seat("jane", "Jane Founder", "", "human"),
      seat("ceo", "CEO", "jane"),
      seat("cto", "CTO", "ceo"),
      seat("swe", "SWE", "cto"),
      seat("fe", "FE", "cto"),
      seat("pm", "PM", "ceo"),
      seat("devrel", "DevRel", "pm"),
    ],
    units: [
      { name: "Leadership", seats: [], lead: "" },
      { name: "Executives", seats: ["ceo", "cto"], lead: "ceo" },
      { name: "Engineering", seats: [], lead: "" },
      { name: "Core", seats: ["swe", "fe"], lead: "cto" },
      { name: "Product", seats: [], lead: "" },
      { name: "Management", seats: ["pm"], lead: "pm" },
      { name: "Developer Relations", seats: ["devrel"], lead: "pm", lead_inherited: true },
    ],
  },
} as unknown as OrgProjection;

function seat(handle: string, name: string, manager: string, kind = "agent") {
  return {
    handle,
    name,
    kind,
    manager,
    managers: manager ? [manager] : [],
    reports: [],
    auto_reports: [],
    placed_by_ref: false,
  };
}

/**
 * `org` as the engine sends it to a reader WITHOUT `config:read`: every seat,
 * at any depth, with its resolved model chain and tool sources removed — what
 * `internal/api`'s `OrgProjection.For` does for that audience. A suite drawing
 * a screen for such a reader hands it this, because a projection carrying the
 * chain to a reader the engine never sends it to certifies a screen against a
 * wire that does not exist.
 */
export function forStateReader(org: OrgProjection): OrgProjection {
  const seats = (roles: OrgSeat[] | undefined) =>
    roles?.map(({ llm: _llm, tool_sources: _tools, ...rest }) => rest);
  const units = (list: OrgUnit[] | undefined): OrgUnit[] | undefined =>
    list?.map((u) => ({
      ...u,
      ...(u.roles ? { roles: seats(u.roles) } : {}),
      ...(u.children ? { children: units(u.children) } : {}),
    }));
  const copy = structuredClone(org);
  return {
    ...copy,
    ...(copy.roles ? { roles: seats(copy.roles) } : {}),
    ...(copy.units ? { units: units(copy.units) } : {}),
  };
}

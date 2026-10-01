/**
 * The org the live chart's suites share: a company shaped like the approved
 * chart, with its derived hierarchy.
 */

import type { OrgProjection } from "~/protocol/types.ts";

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

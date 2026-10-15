/**
 * The org the live chart's suites share: a company shaped like the approved
 * chart, with its derived hierarchy.
 */

import type { OrgProjection } from "~/protocol/types.ts";

/** A founder above everything; a CEO and CTO in Leadership · Executives; two
 *  engineers in Engineering · Core, which the CTO leads from outside it; a PM
 *  in Product · Management, and DevRel in Product · Developer Relations whose
 *  lead the PM is by inheritance.
 *
 *  AS THE ENGINE SENDS IT: every seat carries the handle the engine minted for
 *  it and every unit its key, a lead is named by handle, and the derived block
 *  carries each unit's key beside its name — those, never a name, are what the
 *  chart is addressed by. */
export const CHART_ORG = {
  name: "Acme",
  roles: [
    { name: "Jane Founder", handle: "jane", kind: "human", availability: "reviews releases" },
  ],
  units: [
    {
      id: "leadership",
      name: "Leadership",
      children: [
        {
          id: "executives",
          name: "Executives",
          lead: "ceo",
          roles: [
            { name: "CEO", handle: "ceo" },
            { name: "CTO", handle: "cto" },
          ],
        },
      ],
    },
    {
      id: "engineering",
      name: "Engineering",
      children: [
        {
          id: "core",
          name: "Core",
          roles: [
            { name: "SWE", handle: "swe" },
            { name: "FE", handle: "fe" },
          ],
        },
      ],
    },
    {
      id: "product",
      name: "Product",
      children: [
        { id: "management", name: "Management", lead: "pm", roles: [{ name: "PM", handle: "pm" }] },
        {
          id: "developer-relations",
          name: "Developer Relations",
          roles: [{ name: "DevRel", handle: "devrel" }],
        },
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
      { id: "leadership", name: "Leadership", seats: [], lead: "" },
      { id: "executives", name: "Executives", seats: ["ceo", "cto"], lead: "ceo" },
      { id: "engineering", name: "Engineering", seats: [], lead: "" },
      { id: "core", name: "Core", seats: ["swe", "fe"], lead: "cto" },
      { id: "product", name: "Product", seats: [], lead: "" },
      { id: "management", name: "Management", seats: ["pm"], lead: "pm" },
      {
        id: "developer-relations",
        name: "Developer Relations",
        seats: ["devrel"],
        lead: "pm",
        lead_inherited: true,
      },
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

// @vitest-environment node
/**
 * What a person leads, read off the org push.
 *
 * What these protect: a lead's scope is every unit whose EFFECTIVE lead is
 * their seat and every unit inside one — a sub-unit with a lead of its own
 * included, since leading the unit above is leading it — with the units they
 * lead whose parent they do not as the places a draft opens on; the seats in
 * scope are those units' direct members as the engine placed them; and
 * nobody leads anything while bound to no seat or while the push carries no
 * derivation.
 */

import { describe, expect, test } from "vitest";
import type { OrgProjection } from "~/protocol/index.ts";
import { CHART_ORG } from "~/test/orgchart.ts";
import { leadScope, NO_SCOPE } from "./leadScope.ts";

const keys = (map: ReadonlyMap<string, string>) => [...map.keys()].sort();

describe("leadScope", () => {
  test("a lead of a unit leads every unit inside it, a sub-unit's own lead included", () => {
    const org = structuredClone(CHART_ORG) as OrgProjection;
    // Engineering is the CTO's now, above Core, whose own lead is somebody else.
    org.derived!.units!.find((u) => u.id === "engineering")!.lead = "cto";
    org.derived!.units!.find((u) => u.id === "core")!.lead = "swe";
    const scope = leadScope(org, "cto");
    expect(keys(scope.units)).toEqual(["core", "engineering"]);
    expect(scope.units.get("core")).toBe("engineering");
    expect(scope.tops).toEqual([{ key: "engineering", name: "Engineering" }]);
    expect(keys(scope.seats)).toEqual(["fe", "swe"]);
    // Core's own lead leads Core alone.
    expect(leadScope(org, "swe").tops.map((t) => t.key)).toEqual(["core"]);
  });

  test("a lead of a sub-unit only leads it, and nothing above or beside it", () => {
    const scope = leadScope(CHART_ORG, "cto");
    expect(keys(scope.units)).toEqual(["core"]);
    expect(scope.tops).toEqual([{ key: "core", name: "Core" }]);
  });

  test("two units nobody else leads between are two places to open, in the company's order", () => {
    const scope = leadScope(CHART_ORG, "pm");
    expect(scope.tops.map((t) => t.key)).toEqual(["management", "developer-relations"]);
    expect(keys(scope.seats)).toEqual(["devrel", "pm"]);
    // Each seat and unit names the one of the two a draft of it opens on.
    expect(scope.seats.get("devrel")).toBe("developer-relations");
    expect(scope.units.get("management")).toBe("management");
  });

  test("a seat that leads nothing, an unbound reader and a push with no derivation lead nothing", () => {
    expect(leadScope(CHART_ORG, "swe")).toBe(NO_SCOPE);
    expect(leadScope(CHART_ORG, "")).toBe(NO_SCOPE);
    const bare = { ...CHART_ORG, derived: undefined } as OrgProjection;
    expect(leadScope(bare, "cto")).toBe(NO_SCOPE);
    expect(leadScope(null, "cto")).toBe(NO_SCOPE);
  });

  // A ROOT SEAT PLACED IN A UNIT BY ITS `unit:` REFERENCE is a direct member
  // of that unit, as the engine places it, so it is the unit lead's to change.
  test("a root seat the engine placed in a led unit is in scope", () => {
    const org = structuredClone(CHART_ORG) as OrgProjection;
    org.derived!.units!.find((u) => u.id === "core")!.seats = ["swe", "fe", "jane"];
    expect(leadScope(org, "cto").seats.has("jane")).toBe(true);
  });
});

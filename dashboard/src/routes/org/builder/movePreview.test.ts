// @vitest-environment node
/**
 * The Move dialog's preview.
 *
 * What these protect: what follows from the rows is read off the draft — the
 * lead and channel a moved unit's subtree resolves to under its new parent,
 * the agent seats that onboard again (their marker hashes the units above
 * them), and the tool credential servers a seat's home unit hands it; and who
 * a seat reports to is the engine's derivation of the SAVED chart, said as
 * "today" only while the draft's chart is still the saved one and unknown
 * after, never guessed.
 */

import { describe, expect, test } from "vitest";
import { COMPANY_KEY } from "./model/keys.ts";
import { chartOf, fixtureChart } from "./model/testkit.ts";
import { movePreview } from "./movePreview.ts";
import { checkedEdit, loadedState, record } from "./testState.ts";

/** The fixture company with Engineering's channel set, and Dev reporting to the VP. */
function state() {
  const chart = fixtureChart();
  const engineering = chart.units.find((u) => u.key === "engineering")!;
  (engineering as { channel?: string }).channel = "eng";
  return checkedEdit(chart, { seats: { dev: { manager: "vp-engineering" } } });
}

describe("a seat", () => {
  test("names who it reports to today, its onboarding and the credentials its unit stops handing it", () => {
    expect(movePreview(state(), "seat:dev", "unit:sales")).toEqual({
      reportsTo: "VP Engineering",
      endsAsLeadOf: null,
      destinationLead: null,
      leads: [],
      channels: [],
      onboarding: ["Dev"],
      credentials: { gained: [], lost: ["tracker"] },
    });
  });

  test("moving into a unit with a lead names that lead, and a seat already there onboards nobody", () => {
    const preview = movePreview(state(), "seat:sre", "unit:engineering")!;
    expect(preview.destinationLead).toBe("VP Engineering");
    expect(preview.onboarding).toEqual(["SRE"]);
    expect(preview.credentials).toEqual({ gained: ["tracker"], lost: [] });
    // Control: the lead moving into its own unit is not told it will be led by itself.
    const stay = movePreview(state(), "seat:vp-engineering", "unit:engineering")!;
    expect(stay.onboarding).toEqual([]);
    expect(stay.destinationLead).toBeNull();
    // A lead inherited from above leads the unit's members too.
    expect(movePreview(state(), "seat:dev", "unit:platform")!.destinationLead).toBe(
      "VP Engineering",
    );
  });

  // The engine's auto-management reaches a unit's DIRECT members, so a seat
  // its unit's lead manages only automatically stops reporting to that lead
  // when it leaves the unit.
  test("a manager who is only the home unit's lead ends with a move out of that unit", () => {
    const managed = checkedEdit(fixtureChart(), {
      seats: {
        dev: { manager: "vp-engineering" },
        "vp-engineering": { auto_reports: ["dev"], reports: ["dev"] },
      },
    });
    expect(movePreview(managed, "seat:dev", "unit:sales")!.endsAsLeadOf).toBe("Engineering");
    expect(movePreview(managed, "seat:dev", COMPANY_KEY)!.endsAsLeadOf).toBe("Engineering");
    // Control: an explicit manager is not the move's to end.
    expect(movePreview(state(), "seat:dev", "unit:sales")!.endsAsLeadOf).toBeNull();
  });

  test("who a seat reports to is unknown once the draft's chart is not the saved one, and without a derivation", () => {
    const edited = record(state(), {
      type: "updateSeat",
      target: "seat:designer",
      set: [{ path: ["goal"], value: "Draw" }],
    });
    expect(movePreview(edited, "seat:dev", "unit:sales")!.reportsTo).toBeUndefined();
    expect(movePreview(loadedState(), "seat:dev", "unit:sales")!.reportsTo).toBeUndefined();
    // What follows from the rows is still said.
    expect(movePreview(edited, "seat:dev", "unit:sales")!.onboarding).toEqual(["Dev"]);
  });

  test("a human seat gains and loses no tool credentials, and onboards nothing", () => {
    const human = record(state(), {
      type: "changeKind",
      target: "seat:dev",
      kind: "human",
      contact: { github_login: "dev" },
    });
    const preview = movePreview(human, "seat:dev", COMPANY_KEY)!;
    expect(preview.credentials).toEqual({ gained: [], lost: [] });
    expect(preview.onboarding).toEqual([]);
  });
});

describe("a unit", () => {
  test("names the leads and channels its subtree inherits, and every agent seat that onboards again", () => {
    expect(movePreview(state(), "unit:platform", COMPANY_KEY)).toEqual({
      reportsTo: null,
      endsAsLeadOf: null,
      destinationLead: null,
      leads: [{ unit: "Platform", before: "VP Engineering", after: "" }],
      channels: [{ unit: "Platform", before: "eng", after: "" }],
      onboarding: ["Designer", "SRE"],
      credentials: { gained: [], lost: [] },
    });
  });

  test("a unit that declares its own lead and channel keeps them, and what it inherits reaches a destination added in this draft", () => {
    expect(movePreview(state(), "unit:engineering", "unit:sales")).toMatchObject({
      leads: [],
      channels: [],
      onboarding: ["Dev", "VP Engineering", "Designer", "SRE"],
    });
    const added = record(state(), {
      type: "addUnit",
      key: "new:ops",
      placement: { parent: COMPANY_KEY },
      data: { key: "ops", name: "Ops", lead: "sre", channel: "ops" },
    });
    expect(movePreview(added, "unit:platform", "new:ops")).toMatchObject({
      leads: [{ unit: "Platform", before: "VP Engineering", after: "SRE" }],
      channels: [{ unit: "Platform", before: "eng", after: "ops" }],
    });
  });

  test("a subtree whose units name their own keep them, below one that inherits", () => {
    const chart = chartOf({
      units: [
        { key: "a", name: "A", lead: "x" },
        { key: "b", name: "B", parent: "a" },
        { key: "c", name: "C", parent: "b", lead: "y" },
        { key: "d", name: "D", lead: "z" },
      ],
      seats: [
        { handle: "x", name: "X", unit: "a" },
        { handle: "y", name: "Y", unit: "c" },
        { handle: "z", name: "Z", unit: "d" },
      ],
    });
    const preview = movePreview(checkedEdit(chart), "unit:b", "unit:d")!;
    expect(preview.leads).toEqual([{ unit: "B", before: "X", after: "Z" }]);
  });
});

test("a node the draft does not hold previews nothing", () => {
  expect(movePreview(state(), "seat:nobody", COMPANY_KEY)).toBeNull();
});

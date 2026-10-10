// @vitest-environment node
/**
 * The Move dialog's preview.
 *
 * What these protect: the preview reads what the last check reported at both
 * ends and nothing else, so a draft no check has described previews nothing;
 * a moved seat names who it reports to now, the lead that manages the
 * destination's members, the onboarding it triggers and the tool credential
 * servers its home unit gives or takes; and a moved unit names only the leads
 * and channels its subtree inherits.
 */

import { describe, expect, test } from "vitest";
import { COMPANY_KEY } from "./model/keys.ts";
import { builderReducer, INITIAL_BUILDER } from "./model/reducer.ts";
import { toDocument } from "./model/document.ts";
import { fixtureCompany, fixtureDerived } from "./model/testkit.ts";
import { movePreview } from "./movePreview.ts";
import { keyedState, recheck } from "./testState.ts";

const engineering = "units[0]";
const platform = "units[0].children[0]";

function state() {
  const doc = fixtureCompany();
  doc.units![0]!.channel = "eng";
  return keyedState(doc, {
    seats: { "units[0].roles[1]": { manager: "vp-engineering" } },
    units: {
      [engineering]: { channel: "eng" },
      [platform]: {
        lead: "vp-engineering",
        lead_inherited: true,
        channel: "eng",
        channel_inherited: true,
      },
    },
  });
}

describe("a seat", () => {
  test("names who it reports to before the move, the destination's lead, its onboarding and the credentials it loses", () => {
    expect(movePreview(state(), "seat:dev", "unit:Sales")).toEqual({
      known: true,
      reportsTo: "VP Engineering",
      endsAsLeadOf: null,
      destinationLead: null,
      leads: [],
      channels: [],
      onboarding: ["Dev"],
      credentials: { gained: [], lost: ["tracker"] },
    });
  });

  test("moving into a unit with a lead names that lead; moving within the same chain onboards nobody", () => {
    const preview = movePreview(state(), "seat:sre", "unit:Engineering");
    expect(preview.destinationLead).toBe("VP Engineering");
    expect(preview.onboarding).toEqual(["SRE"]);
    expect(preview.credentials).toEqual({ gained: ["tracker"], lost: [] });

    const stay = movePreview(state(), "seat:vp-engineering", "unit:Engineering");
    expect(stay.onboarding).toEqual([]);
    expect(stay.destinationLead).toBeNull();
  });

  // The engine's auto-management reaches a unit's direct members, so a seat
  // its unit's lead manages only automatically stops reporting to that lead
  // when it leaves the unit, and keeps doing so when it stays.
  test("a manager who is only the home unit's lead ends with a move out of that unit", () => {
    const managed = keyedState(fixtureCompany(), {
      seats: {
        "units[0].roles[0]": { auto_reports: ["dev"], reports: ["dev"] },
        "units[0].roles[1]": { manager: "vp-engineering" },
      },
    });
    expect(movePreview(managed, "seat:dev", "unit:Sales").endsAsLeadOf).toBe("Engineering");
    expect(movePreview(managed, "seat:dev", COMPANY_KEY).endsAsLeadOf).toBe("Engineering");
    // An explicit manager, the fixture's default, is not the move's to end.
    expect(movePreview(state(), "seat:dev", "unit:Sales").endsAsLeadOf).toBeNull();
    // A root seat placed in Platform by its unit reference and written into
    // Platform stays a direct member there.
    const placed = keyedState(fixtureCompany(), {
      seats: {
        "roles[1]": { unit_path: "units[0].children[0]", manager: "sre", placed_by_ref: true },
        "units[0].children[0].roles[0]": { auto_reports: ["designer"] },
      },
    });
    expect(movePreview(placed, "seat:designer", "unit:Platform").endsAsLeadOf).toBeNull();
    expect(movePreview(placed, "seat:designer", "unit:Sales").endsAsLeadOf).toBe("Platform");
  });

  test("a human seat gains and loses no tool credentials", () => {
    const doc = fixtureCompany();
    doc.units![0]!.roles![1] = { name: "Dev", kind: "human", contact: { github_login: "dev" } };
    const preview = movePreview(keyedState(doc), "seat:dev", COMPANY_KEY);
    expect(preview.credentials).toEqual({ gained: [], lost: [] });
    expect(preview.onboarding).toEqual([]);
  });
});

describe("a unit", () => {
  test("names the leads and channels its subtree inherits, and every agent seat that onboards again", () => {
    expect(movePreview(state(), "unit:Platform", COMPANY_KEY)).toEqual({
      known: true,
      reportsTo: null,
      endsAsLeadOf: null,
      destinationLead: null,
      leads: [{ unit: "Platform", before: "VP Engineering", after: "" }],
      channels: [{ unit: "Platform", before: "eng", after: "" }],
      onboarding: ["SRE"],
      credentials: { gained: [], lost: [] },
    });
  });

  test("a unit that declares its own lead and channel keeps them", () => {
    expect(movePreview(state(), "unit:Engineering", "unit:Sales")).toMatchObject({
      leads: [],
      channels: [],
      onboarding: ["VP Engineering", "Dev", "SRE"],
    });
  });
});

// A check of an older draft described a company the operator has since
// changed. Read through it, a destination added since is a unit with no lead
// and no channel, and the dialog would say the moved unit loses both.
test("a destination the current check has not described previews nothing, not a unit with no lead", () => {
  const added = builderReducer(state(), {
    type: "record",
    intent: {
      type: "addUnit",
      key: "new:ops",
      placement: { parent: COMPANY_KEY, after: "unit:Sales" },
      data: { name: "Ops", lead: "SRE", channel: "ops" },
    },
  });
  expect(movePreview(added, "unit:Platform", "new:ops").known).toBe(false);
  expect(movePreview(added, "seat:dev", "unit:Sales").known).toBe(false);

  const checked = recheck(added, {
    units: {
      [platform]: {
        lead: "vp-engineering",
        lead_inherited: true,
        channel: "eng",
        channel_inherited: true,
      },
      "units[2]": { lead: "sre", channel: "ops" },
    },
  });
  expect(movePreview(checked, "unit:Platform", "new:ops")).toMatchObject({
    known: true,
    leads: [{ unit: "Platform", before: "VP Engineering", after: "SRE" }],
    channels: [{ unit: "Platform", before: "eng", after: "ops" }],
  });

  // And a check of this draft whose derivation leaves the destination out
  // has not described it either.
  const sent = toDocument(added.draft);
  const derived = fixtureDerived(sent.document);
  const partial = builderReducer(added, {
    type: "checked",
    settled: {
      generation: added.generation,
      sent,
      baseRevision: added.base.revision,
      outcome: {
        status: "clean",
        warnings: [],
        derived: { ...derived, units: (derived.units ?? []).filter((u) => u.name !== "Ops") },
      },
    },
  });
  expect(movePreview(partial, "unit:Platform", "new:ops").known).toBe(false);
  expect(movePreview(partial, "unit:Platform", COMPANY_KEY).known).toBe(true);
});

test("a draft no check has described previews nothing", () => {
  const loaded = builderReducer(INITIAL_BUILDER, {
    type: "load",
    mode: "edit",
    document: fixtureCompany(),
    revision: "rev-1",
  });
  expect(movePreview(loaded, "unit:Platform", COMPANY_KEY).known).toBe(false);
});

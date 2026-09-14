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
import { fixtureCompany } from "./model/testkit.ts";
import { movePreview } from "./movePreview.ts";
import { keyedState } from "./testState.ts";

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
  test("names who it reports to now, the destination's lead, its onboarding and the credentials it loses", () => {
    expect(movePreview(state(), "seat:dev", "unit:Sales")).toEqual({
      known: true,
      reportsTo: "VP Engineering",
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

test("a draft no check has described previews nothing", () => {
  const loaded = builderReducer(INITIAL_BUILDER, {
    type: "load",
    mode: "edit",
    document: fixtureCompany(),
    revision: "rev-1",
  });
  expect(movePreview(loaded, "unit:Platform", COMPANY_KEY).known).toBe(false);
});

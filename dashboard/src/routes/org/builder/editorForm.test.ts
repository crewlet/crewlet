// @vitest-environment node
/**
 * The node editor's form as values.
 *
 * What these protect: an untouched form applies nothing; each part names only
 * a field whose value changed from the form's start, at the path the chart
 * keeps it (the prose and relations on the object, the runtime half under
 * `runtime`); an emptied field is the field removed, never an empty value the
 * base did not have; a box nobody typed in is no change, a MASKED address
 * included; a model chain is read whichever shape the chart wrote it in; and
 * the parts record as one edit that applies what the form says.
 */

import { describe, expect, test } from "vitest";
import { REDACTED } from "~/lib/format.ts";
import { COMPANY_KEY } from "./model/keys.ts";
import { locate, type SeatData, type UnitData } from "./model/draft.ts";
import { builderReducer } from "./model/reducer.ts";
import { fixtureSettings } from "./model/testkit.ts";
import {
  companyForm,
  companyParts,
  editIntent,
  providerKeys,
  renames,
  seatForm,
  seatParts,
  tokenBudgetError,
  unitForm,
  unitParts,
} from "./editorForm.ts";
import { loadedState } from "./testState.ts";

const dev = (): SeatData => ({
  handle: "dev",
  name: "Dev",
  goal: "Build",
  runtime: { llm: "fast", future_runtime_key: 7 },
});

const engineering = (): UnitData => ({
  key: "engineering",
  name: "Engineering",
  type: "department",
  lead: "vp-engineering",
  runtime: { schedules: [{ name: "standup", cron: "0 9 * * 1-5", task: "Run standup" }] },
});

describe("an untouched form", () => {
  test("applies nothing, for the company, a unit and a seat", () => {
    const company = fixtureSettings();
    expect(companyParts(companyForm(company), companyForm(company))).toEqual([]);
    expect(unitParts("unit:engineering", unitForm(engineering()), unitForm(engineering()))).toEqual(
      [],
    );
    const form = seatForm(dev(), "developer");
    expect(seatParts("seat:dev", form, form)).toEqual([]);
  });

  // A RENAME COMES FROM THE BOX, NOT FROM THE MODEL'S TRIM. The model writes a
  // trimmed name, so a stored name with spaces around it differs from what a
  // rename would write, and asking only that question renamed such a node
  // whenever anything else in its form was applied.
  test("a name stored with spaces is not a rename until somebody types", () => {
    const padded: SeatData = { handle: "dev", name: " Dev ", goal: "Build" };
    const initial = seatForm(padded, "");
    expect(seatParts("seat:dev", initial, { ...initial, goal: "Ship" })).toEqual([
      { type: "updateSeat", target: "seat:dev", set: [{ path: ["goal"], value: "Ship" }] },
    ]);
    expect(renames(" Dev ", "Developer")).toBe(true);
    // A box that gained nothing but a space is no rename either.
    expect(renames("Dev", "Dev ")).toBe(false);
  });

  // THE COMPANY'S NAME COSTS MOST: an agent seat's id is derived from it.
  test("a charter whose name is stored with spaces is not renamed by another edit", () => {
    const company = { ...fixtureSettings(), name: " Acme " };
    const initial = companyForm(company);
    expect(companyParts(initial, { ...initial, mission: "Make more things." })).toEqual([
      { type: "updateCompany", set: [{ path: ["mission"], value: "Make more things." }] },
    ]);
    expect(companyParts(initial, { ...initial, name: "Acme Labs" })).toEqual([
      { type: "updateCompany", set: [{ path: ["name"], value: "Acme Labs" }] },
    ]);
  });

  test("a single-line value stored with spaces is written only when its box changes", () => {
    const padded: SeatData = {
      handle: "dev",
      name: "Dev",
      email: "dev@example.com ",
      project: "OPS ",
      runtime: { contact: { github_login: " dev" }, mattermost: { channel: " eng" } },
    };
    const initial = seatForm(padded, "");
    expect(seatParts("seat:dev", initial, { ...initial, goal: "Ship" })).toEqual([
      { type: "updateSeat", target: "seat:dev", set: [{ path: ["goal"], value: "Ship" }] },
    ]);
    // Typing in the box is a change, written as the model writes it.
    expect(seatParts("seat:dev", initial, { ...initial, email: "dev@example.org " })).toEqual([
      {
        type: "updateSeat",
        target: "seat:dev",
        set: [{ path: ["email"], value: "dev@example.org" }],
      },
    ]);
    const unit: UnitData = { key: "ops", name: "Ops", type: "team ", channel: " ops" };
    const unitInitial = unitForm(unit);
    expect(unitParts("unit:ops", unitInitial, { ...unitInitial, purpose: "Run it" })).toEqual([
      { type: "updateUnit", target: "unit:ops", set: [{ path: ["purpose"], value: "Run it" }] },
    ]);
  });

  // THE CHART SERVES A SEAT'S ADDRESS MASKED, and a write handing the mask
  // back is restored from the row. So a masked address is a value like any
  // other to the form: untouched, it is no change and goes back as it came.
  test("a masked address nobody replaced is no change, and replacing it writes what was typed", () => {
    const masked: SeatData = { handle: "ops", name: "Ops", email: REDACTED };
    const initial = seatForm(masked, "");
    expect(initial.email).toBe(REDACTED);
    expect(seatParts("seat:ops", initial, { ...initial, goal: "Run" })).toEqual([
      { type: "updateSeat", target: "seat:ops", set: [{ path: ["goal"], value: "Run" }] },
    ]);
    expect(seatParts("seat:ops", initial, { ...initial, email: "ops@example.com" })).toEqual([
      {
        type: "updateSeat",
        target: "seat:ops",
        set: [{ path: ["email"], value: "ops@example.com" }],
      },
    ]);
    // Replaced with nothing is the address removed.
    expect(seatParts("seat:ops", initial, { ...initial, email: "" })).toEqual([
      { type: "updateSeat", target: "seat:ops", set: [{ path: ["email"] }] },
    ]);
  });
});

describe("a seat", () => {
  test("names only the fields that changed, each where the chart keeps it, and an emptied field removes it", () => {
    const initial = seatForm(dev(), "");
    const form = {
      ...initial,
      goal: "",
      backstory: "Came from ops",
      project: "ENG",
      tokenBudget: "5000",
    };
    expect(seatParts("seat:dev", initial, form)).toEqual([
      {
        type: "updateSeat",
        target: "seat:dev",
        set: [
          { path: ["goal"] },
          { path: ["backstory"], value: "Came from ops" },
          { path: ["project"], value: "ENG" },
          { path: ["runtime", "token_budget"], value: 5000 },
        ],
      },
    ]);
  });

  test("a new handle is the seat's rename, and the name is a part of its own", () => {
    const initial = seatForm(dev(), "");
    expect(
      seatParts("seat:dev", initial, { ...initial, handle: "developer", name: "Developer" }),
    ).toEqual([
      { type: "renameSeat", target: "seat:dev", name: "Developer" },
      { type: "updateSeat", target: "seat:dev", set: [{ path: ["handle"], value: "developer" }] },
    ]);
  });

  test("the manages list, a schedule toggle and the access level are their own parts", () => {
    const data: SeatData = {
      handle: "dev",
      name: "Dev",
      manages: ["sre"],
      runtime: { schedules: [{ name: "digest", cron: "0 8 * * *", task: "Digest" }] },
    };
    const initial = seatForm(data, "developer");
    const form = {
      ...initial,
      manages: ["sre", "platform"],
      schedules: { digest: false },
      accessLevel: "",
    };
    expect(seatParts("seat:dev", initial, form)).toEqual([
      { type: "updateSeat", target: "seat:dev", set: [], accessLevel: null },
      { type: "setManages", target: "seat:dev", manages: ["sre", "platform"] },
      { type: "setScheduleEnabled", target: "seat:dev", schedule: "digest", enabled: false },
    ]);
  });

  test("a token budget of 0 is unlimited, the same as none, and a malformed one is refused before Apply", () => {
    const initial = seatForm(dev(), "");
    expect(seatParts("seat:dev", initial, { ...initial, tokenBudget: "0" })).toEqual([]);
    expect(seatForm({ handle: "x", name: "X", runtime: { token_budget: 0 } }, "").tokenBudget).toBe(
      "",
    );
    expect(tokenBudgetError("12.5")).toBe(
      "Give a whole number of tokens, or leave it empty for unlimited.",
    );
    expect(tokenBudgetError("-1")).not.toBeUndefined();
    expect(tokenBudgetError("99999999999999999999")).toBe(
      "Give a budget of at most 9007199254740991, or leave it empty for unlimited.",
    );
    expect(tokenBudgetError(" 42 ")).toBeUndefined();
    expect(tokenBudgetError("")).toBeUndefined();
  });

  // THE CHART WRITES ONE PROVIDER KEY AS A BARE STRING and several as a list
  // (`org.ProviderKeys`), so a form that read only the list read every seat
  // pinned to one provider as a seat that names none.
  test("a model chain is read whichever shape the chart wrote it in", () => {
    expect(providerKeys(undefined)).toEqual([]);
    expect(providerKeys("fast")).toEqual(["fast"]);
    expect(providerKeys(" ")).toEqual([]);
    expect(providerKeys(["fast", "smart", ""])).toEqual(["fast", "smart"]);
    const one = seatForm(dev(), "");
    expect(one.llm).toEqual(["fast"]);
    // Unchanged, whichever shape: nothing.
    expect(seatParts("seat:dev", one, { ...one, goal: "Build" })).toEqual([]);
    expect(seatParts("seat:dev", one, { ...one, llm: ["fast", "smart"] })).toEqual([
      {
        type: "updateSeat",
        target: "seat:dev",
        set: [{ path: ["runtime", "llm"], value: ["fast", "smart"] }],
      },
    ]);
  });

  test("the integration and contact fields write inside the runtime half", () => {
    const initial = seatForm(dev(), "");
    const form = {
      ...initial,
      githubTier: "review",
      githubRepos: ["acme/api"],
      mattermostUsername: " dev-bot ",
      contact: { ...initial.contact, slack_user_id: "U1" },
      availability: "Weekdays",
    };
    expect(seatParts("seat:dev", initial, form)).toEqual([
      {
        type: "updateSeat",
        target: "seat:dev",
        set: [
          { path: ["runtime", "contact", "slack_user_id"], value: "U1" },
          { path: ["runtime", "availability"], value: "Weekdays" },
          { path: ["runtime", "github", "tier"], value: "review" },
          { path: ["runtime", "github", "repos"], value: ["acme/api"] },
          { path: ["runtime", "mattermost", "username"], value: "dev-bot" },
        ],
      },
    ]);
  });
});

describe("a unit and the company", () => {
  test("a unit's rename, address, fields, lead and schedule toggle are its parts; clearing the lead names none", () => {
    const initial = unitForm(engineering());
    expect(initial.schedules).toEqual({ standup: true });
    const form = {
      ...initial,
      name: "Product Engineering",
      key: "product-engineering",
      purpose: "Build it",
      space: "ENG",
      lead: "",
      schedules: { standup: false },
    };
    expect(unitParts("unit:engineering", initial, form)).toEqual([
      { type: "renameUnit", target: "unit:engineering", name: "Product Engineering" },
      {
        type: "updateUnit",
        target: "unit:engineering",
        set: [
          { path: ["key"], value: "product-engineering" },
          { path: ["purpose"], value: "Build it" },
          { path: ["space"], value: "ENG" },
        ],
      },
      { type: "setLead", target: "unit:engineering" },
      {
        type: "setScheduleEnabled",
        target: "unit:engineering",
        schedule: "standup",
        enabled: false,
      },
    ]);
  });

  test("the charter's changed fields are one part", () => {
    const initial = companyForm(fixtureSettings());
    expect(companyParts(initial, { ...initial, vision: "Everywhere", policies: [] })).toEqual([
      {
        type: "updateCompany",
        set: [{ path: ["vision"], value: "Everywhere" }, { path: ["policies"] }],
      },
    ]);
  });
});

test("the parts record as one edit that applies what the form says", () => {
  const state = loadedState();
  const found = locate(state.draft, "seat:dev");
  if (found?.kind !== "seat") throw new Error("no dev");
  const initial = seatForm(found.node.data, "developer");
  const form = { ...initial, name: "Developer", goal: "Ship", manages: ["sre"] };
  const next = builderReducer(state, {
    type: "record",
    intent: editIntent("seat:dev", seatParts("seat:dev", initial, form)),
  });
  expect(next.log.ops).toHaveLength(1);
  const after = locate(next.draft, "seat:dev");
  expect(after?.kind === "seat" && after.node.data).toMatchObject({
    name: "Developer",
    goal: "Ship",
    manages: ["sre"],
    runtime: { llm: "fast", future_runtime_key: 7 },
  });
  expect(editIntent(COMPANY_KEY, [])).toEqual({ type: "edit", target: COMPANY_KEY, intents: [] });
});

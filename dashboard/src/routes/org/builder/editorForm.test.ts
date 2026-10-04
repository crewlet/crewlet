// @vitest-environment node
/**
 * The node editor's form as values.
 *
 * What these protect: an untouched form applies nothing; each part names only
 * a field whose value changed from the form's start; an emptied field is the
 * field removed, never an empty value the base did not have; a model chain
 * keeps the shape the seat wrote; and the parts record as one edit that
 * applies what the form says.
 */

import { tokenBudgetError } from "~/lib/budget.ts";
import { describe, expect, test } from "vitest";
import type { ConfigRole } from "~/protocol/index.ts";
import { COMPANY_KEY } from "./model/keys.ts";
import { fixtureCompany } from "./model/testkit.ts";
import { locate } from "./model/draft.ts";
import { builderReducer } from "./model/reducer.ts";
import {
  companyForm,
  companyParts,
  editIntent,
  llmChain,
  renames,
  seatForm,
  seatParts,
  tokenBudgetErrors,
  unitForm,
  unitParts,
} from "./editorForm.ts";
import { keyedState } from "./testState.ts";

const dev = (): ConfigRole => fixtureCompany().units![0]!.roles![1]!;

describe("an untouched form", () => {
  test("applies nothing, for the company, a unit and a seat", () => {
    const company = fixtureCompany();
    expect(companyParts(companyForm(company), companyForm(company))).toEqual([]);
    const unit = company.units![0]!;
    expect(
      unitParts("unit:engineering", unitForm(unit), unitForm(unit), { editableKey: false }),
    ).toEqual([]);
    const form = seatForm(dev(), "developer");
    expect(seatParts("seat:dev", dev(), form, form, { editableHandle: false })).toEqual([]);
  });

  // A RENAME COMES FROM THE BOX, NOT FROM THE MODEL'S TRIM. The model writes a
  // trimmed name, so a stored name with spaces around it differs from what a
  // rename would write, and asking only that question renamed such a node
  // whenever anything else in its form was applied. Renaming a unit re-keys
  // its schedules and makes every agent under it onboard again.
  test("a name the document stored with spaces is not a rename until somebody types", () => {
    const padded: ConfigRole = { name: " Dev ", goal: "Build" };
    const initial = seatForm(padded, "");
    expect(
      seatParts(
        "seat:dev",
        padded,
        initial,
        { ...initial, goal: "Ship" },
        { editableHandle: false },
      ),
    ).toEqual([
      { type: "updateSeat", target: "seat:dev", set: [{ path: ["goal"], value: "Ship" }] },
    ]);
    expect(renames(" Dev ", "Developer")).toBe(true);

    const unit = { name: " Engineering " };
    const unitInitial = unitForm(unit);
    expect(
      unitParts(
        "unit:e",
        unitInitial,
        { ...unitInitial, purpose: "Build" },
        { editableKey: false },
      ),
    ).toEqual([
      { type: "updateUnit", target: "unit:e", set: [{ path: ["purpose"], value: "Build" }] },
    ]);
    // And a box that gained nothing but a space is no rename either: the model
    // trims before it compares, so the part would only be refused.
    expect(renames("Dev", "Dev ")).toBe(false);
  });

  // THE COMPANY'S NAME IS THE ONE THAT COSTS MOST: an agent seat's id is
  // derived from it, so a phantom rename of a name stored with a space gives
  // every agent seat a new id over an edit to the mission.
  test("a charter whose name the document stored with spaces is not renamed by another edit", () => {
    const company = { ...fixtureCompany(), name: " Acme " };
    const initial = companyForm(company);
    expect(companyParts(initial, { ...initial, mission: "Make more things." })).toEqual([
      { type: "updateCompany", set: [{ path: ["mission"], value: "Make more things." }] },
    ]);
    expect(companyParts(initial, { ...initial, name: "Acme Labs" })).toEqual([
      { type: "updateCompany", set: [{ path: ["name"], value: "Acme Labs" }] },
    ]);
  });

  // Every single-line value is written trimmed, so each of them had the same
  // phantom edit as the name: applying a goal wrote the trimmed email.
  test("a single-line value stored with spaces is written only when its box changes", () => {
    const padded: ConfigRole = {
      name: "Dev",
      email: "dev@example.com ",
      contact: { github_login: " dev" },
      project: "OPS ",
      space: " ENG",
      integrations: {
        mattermost: { channel: " eng", username: "dev-bot " },
      },
    };
    const initial = seatForm(padded, "");
    expect(
      seatParts(
        "seat:dev",
        padded,
        initial,
        { ...initial, goal: "Ship" },
        { editableHandle: true },
      ),
    ).toEqual([
      { type: "updateSeat", target: "seat:dev", set: [{ path: ["goal"], value: "Ship" }] },
    ]);
    // Typing in the box is a change, written as the model writes it.
    expect(
      seatParts(
        "seat:dev",
        padded,
        initial,
        { ...initial, email: "dev@example.org " },
        { editableHandle: false },
      ),
    ).toEqual([
      {
        type: "updateSeat",
        target: "seat:dev",
        set: [{ path: ["email"], value: "dev@example.org" }],
      },
    ]);

    const unit = {
      name: "Ops",
      type: "team ",
      channel: " ops",
      project: " OPS",
    };
    const unitInitial = unitForm(unit);
    expect(
      unitParts(
        "unit:ops",
        unitInitial,
        { ...unitInitial, purpose: "Run it" },
        { editableKey: false },
      ),
    ).toEqual([
      { type: "updateUnit", target: "unit:ops", set: [{ path: ["purpose"], value: "Run it" }] },
    ]);
  });
});

describe("a seat", () => {
  test("names only the fields that changed, and an emptied field removes it", () => {
    const initial = seatForm(dev(), "");
    const form = {
      ...initial,
      goal: "",
      backstory: "Came from ops",
      contact: { ...initial.contact },
      tokenBudget: { ...initial.tokenBudget, day: "5000" },
    };
    expect(seatParts("seat:dev", dev(), initial, form, { editableHandle: false })).toEqual([
      {
        type: "updateSeat",
        target: "seat:dev",
        set: [
          { path: ["goal"] },
          { path: ["backstory"], value: "Came from ops" },
          { path: ["token_budget", "day"], value: 5000 },
        ],
      },
    ]);
  });

  test("a rename, the manages list, a schedule toggle and the access level are their own parts", () => {
    const data: ConfigRole = {
      name: "Dev",
      manages: ["SRE"],
      schedules: [{ name: "digest", cron: "0 8 * * *", task: "Digest" }],
    };
    const initial = seatForm(data, "developer");
    const form = {
      ...initial,
      name: "Developer",
      manages: ["SRE", "Platform"],
      schedules: { digest: false },
      accessLevel: "",
    };
    expect(seatParts("seat:dev", data, initial, form, { editableHandle: false })).toEqual([
      { type: "renameSeat", target: "seat:dev", name: "Developer" },
      { type: "updateSeat", target: "seat:dev", set: [], accessLevel: null },
      { type: "setManages", target: "seat:dev", manages: ["SRE", "Platform"] },
      { type: "setScheduleEnabled", target: "seat:dev", schedule: "digest", enabled: false },
    ]);
  });

  test("an existing seat's handle is never written, however the form holds it", () => {
    const initial = seatForm(dev(), "");
    const form = { ...initial, handle: "renamed" };
    expect(seatParts("seat:dev", dev(), initial, form, { editableHandle: false })).toEqual([]);
    expect(seatParts("new:x", dev(), initial, form, { editableHandle: true })).toEqual([
      { type: "updateSeat", target: "new:x", set: [{ path: ["handle"], value: "renamed" }] },
    ]);
  });

  // ONE PART PER WINDOW, never the whole mapping, so a colleague's ceiling
  // on another window survives an update onto their revision; and a window
  // emptied is that key removed, which is how the engine spells "no ceiling".
  test("a token budget writes each window it changed, and an emptied window is removed", () => {
    const data: ConfigRole = { name: "Dev", token_budget: { day: 100, week: 700 } };
    const initial = seatForm(data, "");
    expect(initial.tokenBudget).toEqual({ day: "100", week: "700", month: "" });
    expect(
      seatParts(
        "seat:dev",
        data,
        initial,
        { ...initial, tokenBudget: { day: "", week: "700", month: "40000" } },
        { editableHandle: false },
      ),
    ).toEqual([
      {
        type: "updateSeat",
        target: "seat:dev",
        set: [{ path: ["token_budget", "day"] }, { path: ["token_budget", "month"], value: 40000 }],
      },
    ]);
  });

  test("a ceiling of 0 or a malformed one is refused before Apply, naming the window", () => {
    // A stored 0 is SHOWN, not hidden as an empty box: it is the one value
    // the engine refuses, and a form that hid it could never correct it.
    expect(seatForm({ name: "X", token_budget: { day: 0 } }, "").tokenBudget.day).toBe("0");
    expect(tokenBudgetError("day", "0")).toBe(
      "A ceiling of 0 is refused: leave it empty for no daily ceiling.",
    );
    expect(tokenBudgetError("week", "12.5")).toBe(
      "Give a whole number of tokens (40000000, or 40M), or leave it empty for no weekly ceiling.",
    );
    expect(tokenBudgetError("month", "-1")).not.toBeUndefined();
    // The ceiling is what a JSON number carries without rounding, and the
    // message names it rather than blaming the engine, which holds an int64.
    expect(tokenBudgetError("month", "99999999999999999999")).toBe(
      "Give a ceiling of at most 9007199254740991, or leave it empty for no monthly ceiling.",
    );
    expect(tokenBudgetError("day", " 42 ")).toBeUndefined();
    expect(tokenBudgetError("day", "")).toBeUndefined();
    expect(tokenBudgetErrors({ day: "0", week: "", month: "x" })).toEqual({
      day: "A ceiling of 0 is refused: leave it empty for no daily ceiling.",
      month:
        "Give a whole number of tokens (40000000, or 40M), or leave it empty for no monthly ceiling.",
    });
  });

  test("a model chain keeps the shape the seat wrote, and a per-phase mapping is not editable", () => {
    expect(llmChain(undefined)).toEqual([]);
    expect(llmChain("fast")).toEqual(["fast"]);
    expect(llmChain(["fast", "smart"])).toEqual(["fast", "smart"]);
    expect(llmChain({ default: "fast", review: ["smart"] })).toBeNull();

    const single: ConfigRole = { name: "A", llm: "fast" };
    const one = seatForm(single, "");
    expect(
      seatParts("seat:a", single, one, { ...one, llm: ["smart"] }, { editableHandle: false }),
    ).toEqual([{ type: "updateSeat", target: "seat:a", set: [{ path: ["llm"], value: "smart" }] }]);

    const listed: ConfigRole = { name: "A", llm: ["fast", "smart"] };
    const two = seatForm(listed, "");
    expect(
      seatParts("seat:a", listed, two, { ...two, llm: ["smart"] }, { editableHandle: false }),
    ).toEqual([
      { type: "updateSeat", target: "seat:a", set: [{ path: ["llm"], value: ["smart"] }] },
    ]);

    const mapped: ConfigRole = { name: "A", llm: { default: "fast" } };
    const form = seatForm(mapped, "");
    expect(seatParts("seat:a", mapped, form, form, { editableHandle: false })).toEqual([]);
  });

  // THE PROJECT IS THE DOCUMENT'S OWN FIELD. No seat and no unit carries a
  // vendor's block for it: the engine refuses `integrations.jira` as an
  // unknown key, so a form that wrote one was refused on every check.
  test("the integration fields write inside the seat's own blocks, and what it owns beside them", () => {
    const initial = seatForm(dev(), "");
    const form = {
      ...initial,
      githubTier: "review",
      githubRepos: ["acme/api"],
      project: " OPS ",
      space: "ENG",
    };
    expect(seatParts("seat:dev", dev(), initial, form, { editableHandle: false })).toEqual([
      {
        type: "updateSeat",
        target: "seat:dev",
        set: [
          { path: ["integrations", "github", "tier"], value: "review" },
          { path: ["integrations", "github", "repos"], value: ["acme/api"] },
          { path: ["project"], value: "OPS" },
          { path: ["space"], value: "ENG" },
        ],
      },
    ]);
  });
});

describe("a unit and the company", () => {
  // A UNIT'S KEY IS ITS IDENTITY: a unit the company holds keeps the one it
  // has, so its form never writes one, while a unit this draft created is
  // given its key until the save.
  test("a key is written only for a unit this draft created", () => {
    const initial = unitForm({ name: "Ops", id: "ops" });
    expect(initial.id).toBe("ops");
    const typed = { ...initial, id: "operations" };
    expect(unitParts("unit:ops", initial, typed, { editableKey: false })).toEqual([]);
    expect(unitParts("new:u1", initial, typed, { editableKey: true })).toEqual([
      { type: "updateUnit", target: "new:u1", set: [{ path: ["id"], value: "operations" }] },
    ]);
  });

  test("a unit's rename, fields, lead and schedule toggle are its parts; clearing the lead names none", () => {
    const unit = fixtureCompany().units![0]!;
    const initial = unitForm(unit);
    expect(initial.schedules).toEqual({ standup: true });
    const form = {
      ...initial,
      name: "Product Engineering",
      purpose: "Build it",
      lead: "",
      space: "ENG",
      schedules: { standup: false },
    };
    expect(unitParts("unit:engineering", initial, form, { editableKey: false })).toEqual([
      { type: "renameUnit", target: "unit:engineering", name: "Product Engineering" },
      {
        type: "updateUnit",
        target: "unit:engineering",
        set: [
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
    const company = fixtureCompany();
    const initial = companyForm(company);
    expect(companyParts(initial, { ...initial, vision: "Everywhere", policies: [] })).toEqual([
      {
        type: "updateCompany",
        set: [{ path: ["vision"], value: "Everywhere" }, { path: ["policies"] }],
      },
    ]);
  });
});

test("the parts record as one edit that applies what the form says", () => {
  const state = keyedState(fixtureCompany());
  const initial = seatForm(dev(), "developer");
  const form = { ...initial, name: "Developer", goal: "Ship", manages: ["SRE"] };
  const next = builderReducer(state, {
    type: "record",
    intent: editIntent(
      "seat:dev",
      seatParts("seat:dev", dev(), initial, form, { editableHandle: false }),
    ),
  });
  expect(next.log.ops).toHaveLength(1);
  const found = locate(next.draft, "seat:dev");
  expect(found?.kind === "seat" && found.node.data).toMatchObject({
    name: "Developer",
    goal: "Ship",
    manages: ["SRE"],
  });
  expect(editIntent(COMPANY_KEY, [])).toEqual({ type: "edit", target: COMPANY_KEY, intents: [] });
});

// @vitest-environment node

/**
 * The assistant's changes to the org builder's draft are the builder's own:
 * named by handle and unit key, translated into the builder's intents,
 * recorded through the one door every edit takes, and refused whole, in the
 * builder's words, when any one of them would be.
 */

import { describe, expect, test } from "vitest";
import { parseOrgOps } from "./assistantActions.ts";
import { seatWithHandle, unitWithKey } from "./model/draft.ts";
import { builderReducer, INITIAL_BUILDER, type BuilderState } from "./model/reducer.ts";
import { scopedDocument } from "./model/scope.ts";
import { countingKeys, fixtureCompany } from "./model/testkit.ts";

function loaded(): BuilderState {
  return builderReducer(INITIAL_BUILDER, {
    type: "load",
    mode: "edit",
    document: fixtureCompany(),
    revision: "rev-1",
  });
}

/** The draft after applying what was parsed, as the builder's dispatch would. */
function applied(state: BuilderState, raw: unknown) {
  const parsed = parseOrgOps(raw, state, countingKeys());
  if (!parsed.ok) throw new Error(parsed.reason);
  return parsed.value.intents.reduce(
    (s, intent) => builderReducer(s, { type: "record", intent }),
    state,
  );
}

describe("org.draft.apply_ops", () => {
  test("adds a seat to a unit named by its key, at the end of its seats", () => {
    const state = loaded();
    const after = applied(state, {
      ops: [{ op: "add_seat", in: "platform", data: { name: "QA", handle: "qa", goal: "Test" } }],
    });
    const platform = unitWithKey(after.draft, "platform")!;
    expect(platform.roles.at(-1)?.data.name).toBe("QA");
    expect(after.log.ops).toHaveLength(1);
  });

  test("moves and renames by handle, each change against the draft the one before left", () => {
    const after = applied(loaded(), {
      ops: [
        { op: "move", seat: "dev", to: "sales" },
        { op: "rename", seat: "dev", name: "Developer" },
      ],
    });
    const sales = unitWithKey(after.draft, "sales")!;
    expect(sales.roles.map((r) => r.data.name)).toContain("Developer");
    expect(after.log.ops).toHaveLength(2);
  });

  test("words each change as the builder's own log does, for the card", () => {
    const parsed = parseOrgOps(
      { ops: [{ op: "rename", unit: "sales", name: "Revenue" }] },
      loaded(),
      countingKeys(),
    );
    expect(parsed.ok && parsed.value.words).toHaveLength(1);
  });

  test("refuses the whole set when one change would be refused, naming it", () => {
    const state = loaded();
    const parsed = parseOrgOps(
      {
        ops: [
          { op: "rename", seat: "dev", name: "Developer" },
          { op: "remove", seat: "nobody" },
        ],
      },
      state,
      countingKeys(),
    );
    expect(parsed).toEqual({
      ok: false,
      reason: 'Change 2: There is no seat "nobody" in this draft.',
    });
    expect(seatWithHandle(state.draft, "dev")?.data.name).toBe("Dev");
  });

  test("refuses what is not one of its changes, and keys a change does not have", () => {
    const state = loaded();
    for (const raw of [
      null,
      { ops: "rename" },
      { ops: [] },
      { ops: [{ op: "apply_template", template: "empty" }] },
      { ops: [{ op: "rename", seat: "dev", name: "X", key: "new:1" }] },
      { ops: [{ op: "rename", seat: "dev", unit: "sales", name: "X" }] },
      { ops: [{ op: "remove", seat: "dev" }], extra: true },
      { ops: Array.from({ length: 21 }, () => ({ op: "rename", seat: "dev", name: "X" })) },
    ]) {
      expect(parseOrgOps(raw, state, countingKeys()).ok, JSON.stringify(raw)).toBe(false);
    }
  });

  test("refuses a lead's draft a change outside the unit they lead, as the builder does", () => {
    const platform = fixtureCompany().units![0]!.children![0]!;
    const state = builderReducer(INITIAL_BUILDER, {
      type: "load",
      mode: "edit",
      document: scopedDocument("Acme", platform),
      revision: "rev-1",
      scope: "platform",
    });
    const inside = parseOrgOps(
      { ops: [{ op: "add_seat", in: "platform", data: { name: "QA", handle: "qa" } }] },
      state,
      countingKeys(),
    );
    expect(inside.ok).toBe(true);
    const top = parseOrgOps(
      { ops: [{ op: "add_seat", data: { name: "QA", handle: "qa" } }] },
      state,
      countingKeys(),
    );
    expect(top.ok).toBe(false);
  });

  test("refuses a builder that has not loaded a company", () => {
    const parsed = parseOrgOps(
      { ops: [{ op: "rename", seat: "dev", name: "X" }] },
      INITIAL_BUILDER,
      countingKeys(),
    );
    expect(parsed.ok).toBe(false);
  });
});

// @vitest-environment node
/**
 * Preflight: what a dialog shows before it confirms.
 *
 * What these protect: a dialog previews exactly the operation the reducer
 * will record (a refusal included); a schedule is named only when the
 * operation strands it, by either of the engine's two runner rules; and the
 * mass removal count is the review's own.
 */

import { describe, expect, test } from "vitest";
import type { CompanyDocument } from "~/protocol/index.ts";
import { fromDocument } from "./model/document.ts";
import { builderReducer, INITIAL_BUILDER } from "./model/reducer.ts";
import {
  massRemoval,
  newlyStranded,
  removedSeats,
  simulate,
  strandedSchedules,
  strandedSentence,
} from "./preflight.ts";
import { keyedState } from "./testState.ts";

const draftOf = (doc: CompanyDocument) => fromDocument(doc);

describe("simulate", () => {
  test("previews the operation the reducer records, and its refusal before the company loads", () => {
    const doc: CompanyDocument = {
      name: "X",
      units: [
        { name: "Ops", id: "ops", lead: "runner", roles: [{ name: "Runner", handle: "runner" }] },
      ],
    };
    expect(simulate(INITIAL_BUILDER, { type: "remove", target: "seat:runner" })).toMatchObject({
      ok: false,
      refusal: "not_loaded",
    });

    const state = keyedState(doc);
    const preview = simulate(state, { type: "remove", target: "seat:runner" });
    if (!preview.ok) throw new Error(preview.message);
    expect(preview.report.cleared).toEqual([{ kind: "lead", holder: "unit:ops", from: "runner" }]);
    const dispatched = builderReducer(state, {
      type: "record",
      intent: { type: "remove", target: "seat:runner" },
    });
    expect(dispatched.log.ops[0]).toEqual(preview.op);
    expect(dispatched.draft).toEqual(preview.after);
    expect(state.draft).not.toEqual(preview.after);
  });
});

describe("stranded schedules", () => {
  const fanOut = (members: CompanyDocument["roles"], enabled?: boolean): CompanyDocument => ({
    name: "X",
    roles: [{ name: "Boss", handle: "boss" }],
    units: [
      {
        name: "Ops",
        id: "ops",
        schedules: [
          {
            name: "sweep",
            cron: "0 * * * *",
            task: "Sweep",
            ...(enabled === false ? { enabled } : {}),
          },
        ],
        roles: members,
      },
    ],
  });

  test("a schedule for members needs a direct agent member, and an operation that takes the last one strands it", () => {
    const before = draftOf(fanOut([{ name: "Runner", handle: "runner" }]));
    const after = draftOf(fanOut([]));
    expect(strandedSchedules(before)).toEqual([]);
    expect(newlyStranded(before, after)).toEqual([
      { unit: "unit:ops", unitName: "Ops", schedule: "sweep", reason: "members" },
    ]);
    expect(strandedSentence(newlyStranded(before, after)[0]!)).toBe(
      "Schedule sweep on Ops would have no runner: it runs on the unit's direct agent members, and the unit would have none. The engine refuses the save until it is disabled or has a runner.",
    );
  });

  test("a human member runs nothing, a disabled schedule needs no runner, and a root seat placed by reference is a member", () => {
    expect(
      strandedSchedules(draftOf(fanOut([{ name: "Pat", handle: "pat", kind: "human" }]))),
    ).toHaveLength(1);
    expect(strandedSchedules(draftOf(fanOut([], false)))).toEqual([]);
    // A root seat's `unit:` names the unit's key, never its name.
    const placed = fanOut([]);
    placed.roles = [{ name: "Floater", handle: "floater", unit: "ops" }];
    expect(strandedSchedules(draftOf(placed))).toEqual([]);
    placed.roles = [{ name: "Floater", handle: "floater", unit: "Ops" }];
    expect(strandedSchedules(draftOf(placed))).toHaveLength(1);
  });

  test("a schedule for the lead is stranded by a human effective lead, inherited from above too", () => {
    const doc = (kind: "human" | "agent"): CompanyDocument => ({
      name: "X",
      units: [
        {
          name: "Division",
          id: "division",
          lead: "head",
          roles: [
            {
              name: "Head",
              handle: "head",
              ...(kind === "human" ? { kind, contact: { github_login: "h" } } : {}),
            },
          ],
          children: [
            {
              name: "Team",
              id: "team",
              schedules: [{ name: "standup", cron: "0 9 * * *", task: "Standup", target: "lead" }],
              roles: [{ name: "Member", handle: "member" }],
            },
          ],
        },
      ],
    });
    const before = draftOf(doc("agent"));
    const after = draftOf(doc("human"));
    expect(newlyStranded(before, after)).toEqual([
      { unit: "unit:team", unitName: "Team", schedule: "standup", reason: "lead" },
    ]);
    expect(strandedSentence(newlyStranded(before, after)[0]!)).toBe(
      "Schedule standup on Team would have no runner: it runs as the unit's lead, and the lead would be a human seat. The engine refuses the save until it is disabled or has a runner.",
    );
  });

  test("a schedule stranded before the operation is not the operation's consequence", () => {
    const stranded = draftOf(fanOut([]));
    expect(newlyStranded(stranded, stranded)).toEqual([]);
  });

  test("a kind change the Change kind dialog previews strands the lead schedule it breaks", () => {
    const doc: CompanyDocument = {
      name: "X",
      units: [
        {
          name: "Team",
          id: "team",
          lead: "lead",
          schedules: [{ name: "standup", cron: "0 9 * * *", task: "Standup", target: "lead" }],
          roles: [
            { name: "Lead", handle: "lead" },
            { name: "Member", handle: "member" },
          ],
        },
      ],
    };
    const state = keyedState(doc);
    const preview = simulate(state, {
      type: "changeKind",
      target: "seat:lead",
      kind: "human",
      contact: { github_login: "lead" },
    });
    if (!preview.ok) throw new Error(preview.message);
    expect(newlyStranded(state.draft, preview.after).map((s) => s.schedule)).toEqual(["standup"]);
  });
});

describe("removals", () => {
  test("more than half of the saved company's seats is a mass removal, counted as the review counts it", () => {
    const doc: CompanyDocument = {
      name: "X",
      roles: [{ name: "A", handle: "a" }],
      units: [
        {
          name: "Ops",
          id: "ops",
          roles: [
            { name: "B", handle: "b" },
            { name: "C", handle: "c" },
          ],
        },
      ],
    };
    const state = keyedState(doc);
    const unit = simulate(state, { type: "remove", target: "unit:ops" });
    const seat = simulate(state, { type: "remove", target: "seat:a" });
    if (!unit.ok || !seat.ok) throw new Error("expected both to record");
    expect(massRemoval(state.baseDraft, unit.after)).toEqual({ removed: 2, total: 3 });
    expect(massRemoval(state.baseDraft, seat.after)).toBeNull();
    expect(removedSeats(state.draft, unit.after).map((s) => s.data.name)).toEqual(["B", "C"]);
  });
});

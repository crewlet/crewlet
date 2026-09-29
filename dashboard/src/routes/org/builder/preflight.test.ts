// @vitest-environment node
/**
 * Preflight: what a dialog shows before it confirms.
 *
 * What these protect: a dialog previews exactly the operation the reducer
 * will record, a refusal included; a schedule is named only when the
 * operation strands it, by either of the engine's two runner rules, the lead
 * one reading the lead a unit RESOLVES to by the engine's cascade; and the mass
 * removal count is the review's own.
 */

import { describe, expect, test } from "vitest";
import type { ChartRead, ChartSeat } from "~/protocol/index.ts";
import { fromChart } from "./model/document.ts";
import { builderReducer } from "./model/reducer.ts";
import { chartOf, strippedChart } from "./model/testkit.ts";
import {
  massRemoval,
  newlyStranded,
  removedSeats,
  simulate,
  strandedSchedules,
  strandedSentence,
} from "./preflight.ts";
import { loadedState } from "./testState.ts";

const draftOf = (chart: ChartRead) => fromChart({ name: "X" }, chart);

describe("simulate", () => {
  test("previews the operation the reducer records, what it clears, and a refusal the reducer would give", () => {
    const chart = chartOf({
      units: [{ key: "ops", name: "Ops", lead: "runner" }],
      seats: [{ handle: "runner", name: "Runner", unit: "ops" }],
    });
    const state = loadedState(chart, { name: "X" });
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
    // A reader the chart did not show the runtime half is refused as the reducer refuses them.
    const reader = loadedState(strippedChart(chart), { name: "X" });
    expect(
      simulate(reader, { type: "changeKind", target: "seat:runner", kind: "human" }),
    ).toMatchObject({ ok: false, refusal: "runtime_hidden" });
  });
});

describe("stranded schedules", () => {
  const fanOut = (members: ChartSeat[], enabled?: boolean): ChartRead =>
    chartOf({
      units: [
        {
          key: "ops",
          name: "Ops",
          runtime: {
            schedules: [
              {
                name: "sweep",
                cron: "0 * * * *",
                task: "Sweep",
                ...(enabled === false ? { enabled } : {}),
              },
            ],
          },
        },
      ],
      seats: [{ handle: "boss", name: "Boss" }, ...members.map((m) => ({ ...m, unit: "ops" }))],
    });

  test("a schedule for members needs a direct agent member, and an operation that takes the last one strands it", () => {
    const before = draftOf(fanOut([{ handle: "runner", name: "Runner" }]));
    const after = draftOf(fanOut([]));
    expect(strandedSchedules(before)).toEqual([]);
    expect(newlyStranded(before, after)).toEqual([
      { unit: "unit:ops", unitName: "Ops", schedule: "sweep", reason: "members" },
    ]);
    expect(strandedSentence(newlyStranded(before, after)[0]!)).toBe(
      "Schedule sweep on Ops would have no runner: it runs on the unit's direct agent members, and the unit would have none. Nothing refuses the save, so it would simply never run: disable it, or give it a runner.",
    );
  });

  test("a human member runs nothing, and a disabled schedule needs no runner", () => {
    expect(
      strandedSchedules(draftOf(fanOut([{ handle: "pat", name: "Pat", kind: "human" }]))),
    ).toHaveLength(1);
    expect(strandedSchedules(draftOf(fanOut([], false)))).toEqual([]);
  });

  test("a schedule for the lead is stranded by a human lead the unit resolves to, inherited from above too", () => {
    const chart = (kind: "human" | "agent"): ChartRead =>
      chartOf({
        units: [
          { key: "division", name: "Division", lead: "head" },
          {
            key: "team",
            name: "Team",
            parent: "division",
            runtime: {
              schedules: [{ name: "standup", cron: "0 9 * * *", task: "Standup", target: "lead" }],
            },
          },
        ],
        seats: [
          { handle: "head", name: "Head", unit: "division", ...(kind === "human" ? { kind } : {}) },
          { handle: "member", name: "Member", unit: "team" },
        ],
      });
    const before = draftOf(chart("agent"));
    const after = draftOf(chart("human"));
    expect(newlyStranded(before, after)).toEqual([
      { unit: "unit:team", unitName: "Team", schedule: "standup", reason: "lead" },
    ]);
    expect(strandedSentence(newlyStranded(before, after)[0]!)).toBe(
      "Schedule standup on Team would have no runner: it runs as the unit's lead, and the lead would be a human seat. Nothing refuses the save, so it would simply never run: disable it, or give it a runner.",
    );
  });

  test("a schedule stranded before the operation is not the operation's consequence", () => {
    const stranded = draftOf(fanOut([]));
    expect(newlyStranded(stranded, stranded)).toEqual([]);
  });

  test("a kind change the Change kind dialog previews strands the lead schedule it breaks", () => {
    const chart = chartOf({
      units: [
        {
          key: "team",
          name: "Team",
          lead: "lead",
          runtime: {
            schedules: [{ name: "standup", cron: "0 9 * * *", task: "Standup", target: "lead" }],
          },
        },
      ],
      seats: [
        { handle: "lead", name: "Lead", unit: "team" },
        { handle: "member", name: "Member", unit: "team" },
      ],
    });
    const state = loadedState(chart, { name: "X" });
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
    const chart = chartOf({
      units: [{ key: "ops", name: "Ops" }],
      seats: [
        { handle: "a", name: "A" },
        { handle: "b", name: "B", unit: "ops" },
        { handle: "c", name: "C", unit: "ops" },
      ],
    });
    const state = loadedState(chart, { name: "X" });
    const unit = simulate(state, { type: "remove", target: "unit:ops" });
    const seat = simulate(state, { type: "remove", target: "seat:a" });
    if (!unit.ok || !seat.ok) throw new Error("expected both to record");
    expect(massRemoval(state.baseDraft, unit.after)).toEqual({ removed: 2, total: 3 });
    expect(massRemoval(state.baseDraft, seat.after)).toBeNull();
    expect(removedSeats(state.draft, unit.after).map((s) => s.data.name)).toEqual(["B", "C"]);
  });
});

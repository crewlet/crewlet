/**
 * What the Advisor's page draws, read off the engine's answers: the figures
 * against the previous range, the charts' series, and each finding's
 * headline, numbers and labelled presses.
 */

import { expect, test } from "vitest";
import type { WorkFlowAnswer, WorkloadAnswer } from "~/protocol/index.ts";
import type { TurnOutcomeRow } from "~/protocol/turnOutcomes.ts";
import type { AdvisorKeptFinding, AdvisorReport } from "~/protocol/advisorFindings.ts";
import { deltaOf, figure } from "./Numbers.tsx";
import {
  actWords,
  actionsOf,
  capacityHours,
  explainer,
  findingView,
  firstSentence,
  flowBySeatChart,
  statsOf,
  stuckHours,
  tokensPerTurnChart,
  wastedWakesChart,
} from "./view.ts";

const turn = (
  seat: string,
  wake: string,
  turns: number,
  tokens: number,
  nothing: number,
): TurnOutcomeRow => ({
  seat,
  agent_id: `id-${seat}`,
  wake,
  turns,
  tokens,
  nothing,
  nothing_tokens: Math.round((tokens / turns) * nothing),
});

function finding(patch: Partial<AdvisorKeptFinding> = {}): AdvisorKeptFinding {
  return {
    id: "f-1",
    key: "k",
    class: "wake.noop_share",
    source: "rule",
    actor: "owner",
    area: "wakes",
    severity: "costs",
    subject: { kind: "seat", id: "pm", name: "Agent PM", seat: "pm", unit: "", schedule: "" },
    title: "An agent was woken for turns that changed nothing.",
    why: "Most of its turns moved no work. Each one costs tokens.",
    item: "",
    names: [],
    path: "",
    entries: null,
    explanation: null,
    state: "open",
    close_reason: null,
    ineffective: null,
    snoozed_until: null,
    opened_at: "2026-11-19T08:00:00Z",
    last_confirmed_at: "2026-11-20T04:00:00Z",
    closed_at: null,
    effect_due_at: null,
    confirmed: "fresh",
    revision: 1,
    seen_before: 0,
    impact: null,
    evidence: { window: { from: "", to: "" }, facts: [], records: [] },
    fix: { kind: "none", sentence: "Nothing to send.", options: [] },
    ...patch,
  };
}

const flow: WorkFlowAnswer = {
  bucket: "day",
  points: [
    {
      window: "2026-11-14",
      start: "2026-11-14T00:00:00Z",
      end: "",
      not_started: 20,
      active: 10,
      done: 0,
      closed: 0,
      completed: 3,
    },
    {
      window: "2026-11-15",
      start: "2026-11-15T00:00:00Z",
      end: "",
      not_started: 22,
      active: 12,
      done: 0,
      closed: 0,
      completed: 5,
    },
  ],
  now: { not_started: 25, active: 15, blocked: 3, overdue: 2 },
  blocked_history: false,
  complete: true,
};

const report = (metrics: [string, number, number | null][]): AdvisorReport =>
  ({
    week: "2026-W46",
    metrics: metrics.map(([key, value, was]) => ({ key, label: key, value, unit: "hours", was })),
  }) as unknown as AdvisorReport;

// THE FIGURES ARE THE SAME NUMBERS THE OTHER SCREENS SHOW, each against the
// previous range where one is read, and a figure not measured is null.
test("the strip's figures come from the answers, each against the previous range", () => {
  const stats = statsOf({
    flow,
    workload: null,
    outcomes: [
      turn("pm", "work_item:lead_fallback", 185, 185_000, 124),
      turn("sre", "schedule:poll", 15, 45_000, 0),
    ],
    previous: [turn("pm", "work_item:lead_fallback", 100, 50_000, 50)],
    report: report([
      ["wait_h", 20, 10],
      ["held_h", 4, 2],
      ["blocked_h", 2, 1],
    ]),
    findings: [],
  });
  const by = Object.fromEntries(stats.map((s) => [s.key, s]));
  expect(by.open_items).toMatchObject({ value: 40, was: 30, unit: "items" });
  expect(by.stuck_hours).toMatchObject({ value: 26, was: 13, unit: "hours", window: "last week" });
  expect(by.capacity_lost_hours!.value).toBeNull();
  expect(by.wasted_turns_percent).toMatchObject({ value: 62, was: 50, unit: "percent" });
  expect(by.tokens_per_turn).toMatchObject({ value: 1150, was: 500, unit: "tokens" });
});

test("a change is said in the ink of whether it is the one wanted, a share in points", () => {
  const stat = { key: "k", label: "l", unit: "percent" as const, better: "down" as const };
  expect(deltaOf({ ...stat, value: 62, was: 50 })).toEqual({ value: "+12 pts", polarity: "bad" });
  expect(deltaOf({ ...stat, value: 40, was: 50 })).toEqual({ value: "−10 pts", polarity: "good" });
  expect(deltaOf({ ...stat, unit: "items", value: 40, was: 30 })).toEqual({
    value: "+33%",
    polarity: "bad",
  });
  expect(deltaOf({ ...stat, value: 40, was: null })).toBeUndefined();
  expect(figure(1150, "tokens")).toEqual({ value: "1,150", unit: "tokens" });
  expect(figure(1, "items")).toEqual({ value: "1", unit: "item" });
});

test("hours waiting are the newest week's waited, held and blocked hours, and unknown without a report", () => {
  expect(stuckHours(null)).toBeNull();
  expect(stuckHours(report([["wait_h", 1.25, null]]))).toEqual({ value: 1.3, was: null });
});

test("hours lost to model limits are the capacity finding's hours, and not measured without one", () => {
  expect(capacityHours([])).toBeNull();
  const held = finding({
    class: "capacity.model_held",
    evidence: {
      window: { from: "", to: "" },
      facts: [
        { key: "held_h", label: "Hours held", value: 11.5, unit: "hours" },
        { key: "seats", label: "Seats", value: 4, unit: "count" },
      ],
      records: [],
    },
  });
  expect(capacityHours([held])).toBe(11.5);
});

// ADVICE PER REASON: a kind sums a reason that wastes most of its turns into
// one that wastes none.
test("wasted turns are counted by the whole wake reason across agents, the costliest first", () => {
  const chart = wastedWakesChart([
    turn("pm", "work_item:lead_fallback", 100, 100_000, 70),
    turn("cto", "work_item:lead_fallback", 85, 85_000, 54),
    turn("pm", "work_item:pickup", 50, 50_000, 0),
    turn("sre", "chat_message:mention", 255, 255_000, 152),
  ])!;
  expect(chart.kind).toBe("bars");
  expect(chart.x.labels).toHaveLength(2);
  expect(chart.series.find((s) => s.id === "nothing")!.values).toEqual([152, 124]);
  expect(chart.series.find((s) => s.id === "moved")!.values).toEqual([103, 61]);
});

test("tokens a turn are per agent, the costliest first, beside the median agent", () => {
  const chart = tokensPerTurnChart(
    [
      turn("a2", "x", 124, 1_314_400_000, 0),
      turn("be", "x", 71, 71_000_000, 0),
      turn("pm", "x", 10, 8_000_000, 0),
    ],
    (h) => h.toUpperCase(),
  )!;
  expect(chart.x.labels).toEqual(["A2", "BE", "PM"]);
  expect(chart.series[0]!.values).toEqual([10_600_000, 1_000_000, 800_000]);
  expect(chart.note).toBe("The median agent spends 1M tokens a turn.");
});

test("work in progress per agent is stacked by the flow state that holds it", () => {
  const workload = {
    rows: [
      {
        handle: "sre",
        flow: { working: 1, waiting: 14, held: 0, blocked: 0, due: 0, stuck: 0, moving: 0 },
      },
      {
        handle: "idle",
        flow: { working: 0, waiting: 0, held: 0, blocked: 0, due: 0, stuck: 0, moving: 0 },
      },
    ],
  } as unknown as WorkloadAnswer;
  const chart = flowBySeatChart(workload, (h) => h)!;
  expect(chart.kind).toBe("stacked_columns");
  expect(chart.x.labels).toEqual(["sre"]);
  expect(chart.series.find((s) => s.id === "waiting")!.values).toEqual([14]);
});

// ONE PRESS THAT SAYS WHAT IT DOES, the fix's other changes in the menu.
test("a fix's presses are labelled with what each does, the first the row's button", () => {
  const several = finding({
    fix: {
      kind: "config_change",
      sentence: "Run Agent PM's learning on a smaller model.",
      options: [
        { label: "Learn on Haiku", change: { entity: "roles", id: "pm", set: {}, summary: "" } },
        { label: "Learn on Mini", change: { entity: "roles", id: "pm", set: {}, summary: "" } },
      ],
    },
  });
  expect(actionsOf(several)).toEqual({
    primary: { kind: "apply", label: "Learn on Haiku", option: 0 },
    secondary: [{ kind: "apply", label: "Learn on Mini", option: 1 }],
  });
  expect(
    actionsOf(
      finding({
        fix: { kind: "open_setting", sentence: "s", options: [], setting: "providers.llm" },
      }),
    ).primary,
  ).toMatchObject({ kind: "open", label: "Add a model entry" });
  expect(
    actionsOf(
      finding({
        fix: { kind: "open_page", sentence: "s", options: [], page: "item", target: "ENG-12" },
      }),
    ).primary,
  ).toEqual({ kind: "open", label: "Open ENG-12", href: "#/worklet/ENG-12" });
  expect(actionsOf(finding({ state: "applied" })).primary).toBeNull();
});

test("a sentence becomes a press by its first clause", () => {
  expect(
    actWords("Take a backup now, then schedule backups that are copied off this server."),
  ).toBe("Take a backup now");
  expect(
    actWords("Add a seat like Backend: the same job, model and limits under a new name."),
  ).toBe("Add a seat like Backend");
  expect(actWords("Let one agent watch the engine's health and wake on its alarms.")).toBe(
    "Let one agent watch the engine's health",
  );
  expect(actWords("Change Agent PM's round cap.")).toBe("Change Agent PM's round cap");
});

test("a row is its headline, one line of why and at most three numbers", () => {
  const view = findingView(
    finding({
      impact: { hours_per_week: 31, tokens_per_week: null },
      evidence: {
        window: { from: "", to: "" },
        facts: [
          { key: "a", label: "Turns", value: 185, unit: "turns" },
          { key: "b", label: "Changed nothing", value: 124, unit: "turns" },
          { key: "c", label: "Share", value: 67, unit: "percent" },
        ],
        records: [],
      },
    }),
  );
  expect(view.why).toBe("Most of its turns moved no work.");
  expect(view.stats.map((s) => s.key)).toEqual(["hours_per_week", "a", "b"]);
  expect(firstSentence("No full stop")).toBe("No full stop");
});

test("a figure links the open finding that explains it, the most costly first", () => {
  const note = finding({ id: "n", severity: "note" });
  const cost = finding({ id: "c", severity: "costs" });
  expect(explainer("wasted_turns_percent", [note, cost])?.id).toBe("c");
  expect(explainer("open_items", [note, cost])).toBeNull();
});

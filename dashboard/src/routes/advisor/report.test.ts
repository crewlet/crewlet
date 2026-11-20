/**
 * A week's report as its page and the list of weeks draw it: the headline
 * figures beside the week before, a first week said as one, every other
 * number with its change, the week across its days, and each week's card.
 */

import { expect, test } from "vitest";
import type {
  AdvisorMetric,
  AdvisorReport,
  AdvisorReportSummary,
} from "~/protocol/advisorFindings.ts";
import {
  compact,
  dayCharts,
  firstWeek,
  otherRows,
  reportFigure,
  weekCards,
  weekFigures,
  weekShort,
} from "./report.ts";

const m = (key: string, value: number, unit: string, was: number | null = null): AdvisorMetric => ({
  key,
  label: key,
  value,
  unit,
  was,
});

function summary(
  week: string,
  from: string,
  to: string,
  patch: Partial<AdvisorReportSummary> = {},
) {
  return {
    week,
    period: { from, to },
    written_at: to,
    opened: 0,
    resolved: 0,
    applied: 0,
    dismissed: 0,
    headline: [],
    ...patch,
  } satisfies AdvisorReportSummary;
}

// A NUMBER IN A FEW CHARACTERS: whole under ten thousand, three figures above.
test("a big number is said in three figures, a small one in full", () => {
  expect(compact(2_540_000_000)).toBe("2.54B");
  expect(compact(412_345)).toBe("412k");
  expect(compact(15_720)).toBe("15.7k");
  expect(compact(3_116)).toBe("3,116");
  expect(compact(999_600)).toBe("1M");
  expect(compact(250_000_000)).toBe("250M");
  expect(reportFigure(2_540_000_000, "tokens")).toEqual({ value: "2.54B", unit: "tokens" });
  expect(reportFigure(1, "turns")).toEqual({ value: "1", unit: "turn" });
  expect(reportFigure(17.4, "hours")).toEqual({ value: "17", unit: "h" });
});

// THE HEADLINE: turns, tokens, the share of turns that changed nothing (its
// count beside it) and the hours work waited, each beside the week before.
test("a week's figures are its turns, tokens, the share that changed nothing and the hours work waited", () => {
  const figures = weekFigures([
    m("turns", 3_116, "turns", 2_000),
    m("turns_nothing", 535, "turns", 500),
    m("tokens", 2_540_000_000, "tokens", 2_000_000_000),
    m("wait_h", 29, "hours", 40),
    m("budget_stops", 2, "count"),
  ]);
  expect(figures.map((f) => [f.key, f.value, f.was, f.better])).toEqual([
    ["turns", 3_116, 2_000, null],
    ["tokens", 2_540_000_000, 2_000_000_000, "down"],
    ["changed_nothing", 17, 25, "down"],
    ["wait_h", 29, 40, "down"],
  ]);
  expect(figures[2]!.detail).toBe("535 of 3,116 turns");
  // A SOURCE NOT READ IS A FIGURE NOT MEASURED, never a zero; with no hours
  // of waiting the median pickup wait stands in.
  const bare = weekFigures([m("pickup_p50", 12, "minutes")]);
  expect(bare.map((f) => [f.key, f.value])).toEqual([
    ["turns", null],
    ["tokens", null],
    ["changed_nothing", null],
    ["pickup_p50", 12],
  ]);
});

// A FIRST WEEK HAS NOTHING TO BE COMPARED WITH, and is said so once, never as
// a column of "not measured".
test("a week with nothing before it is a first week", () => {
  expect(firstWeek([m("turns", 1, "turns"), m("tokens", 2, "tokens")])).toBe(true);
  expect(firstWeek([m("turns", 1, "turns", 1), m("tokens", 2, "tokens")])).toBe(false);
  expect(firstWeek([])).toBe(false);
});

// EVERY OTHER NUMBER, in the report's order, with its change in words.
test("the rest of a week's numbers are listed with their change", () => {
  const rows = otherRows([
    m("turns", 3_116, "turns", 2_000),
    m("budget_stops", 2, "count", 2),
    m("held_h", 85, "hours", 80),
    m("round_caps", 3, "count"),
    m("alarm_tokens_nothing", 0, "tokens", 0),
  ]);
  expect(rows.map((r) => [r.key, r.value, r.change?.value ?? null])).toEqual([
    ["budget_stops", "2", "same"],
    ["held_h", "85 h", "+6%"],
    ["round_caps", "3", null],
    ["alarm_tokens_nothing", "0 tokens", "same"],
  ]);
});

// THE WEEK ACROSS ITS DAYS: turns split by whether they committed anything,
// and tokens, each day named by its weekday, the busiest said in a line.
test("a week's days are drawn as turns and tokens each day", () => {
  const report = {
    days: [
      { day: "2026-11-09", turns: 10, turns_nothing: 4, tokens: 1_000 },
      { day: "2026-11-10", turns: 30, turns_nothing: 5, tokens: 9_000 },
      { day: "2026-11-11", turns: 0, turns_nothing: 0, tokens: 0 },
    ],
  } as unknown as AdvisorReport;
  const [turns, tokens] = dayCharts(report);
  expect(turns!.x.labels).toEqual(["Mon 9", "Tue 10", "Wed 11"]);
  expect(turns!.series).toEqual([
    { id: "moved", label: "Committed something", values: [6, 25, 0] },
    { id: "nothing", label: "Changed nothing", values: [4, 5, 0] },
  ]);
  expect(turns!.note).toBe("The busiest day was Tue 10: 30 turns, 5 of them changed nothing.");
  expect(tokens!.series[0]!.values).toEqual([1_000, 9_000, 0]);
  expect(tokens!.note).toBe("The most was spent on Tue 10: 9,000 tokens.");
  expect(dayCharts({ days: [] } as unknown as AdvisorReport)).toEqual([]);
});

// A WEEK'S CARD: its days, its figures and what it moved, beside the week
// before where the list holds it.
test("a week's card says its days, its figures and its findings beside the week before", () => {
  const cards = weekCards([
    summary("2026-W47", "2026-11-16T00:00:00Z", "2026-11-23T00:00:00Z", {
      opened: 3,
      resolved: 1,
      headline: [
        m("turns", 100, "turns", 80),
        m("turns_nothing", 20, "turns", 8),
        m("tokens", 9_000, "tokens", 10_000),
      ],
    }),
    summary("2026-W46", "2026-11-09T00:00:00Z", "2026-11-16T00:00:00Z", {
      opened: 1,
      headline: [
        m("turns", 80, "turns"),
        m("turns_nothing", 8, "turns"),
        m("tokens", 10_000, "tokens"),
      ],
    }),
    // A GAP: the week before this one was not reported, so its findings
    // have nothing beside them.
    summary("2026-W44", "2026-10-26T00:00:00Z", "2026-11-02T00:00:00Z", { opened: 5 }),
  ]);
  expect(cards.map((c) => [c.week, c.title, c.days, c.first])).toEqual([
    ["2026-W47", "Week 47", "Nov 16 to 22, 2026", false],
    ["2026-W46", "Week 46", "Nov 9 to 15, 2026", true],
    ["2026-W44", "Week 44", "Oct 26 to Nov 1, 2026", false],
  ]);
  expect(cards[0]!.figures.map((f) => [f.key, f.value, f.was])).toEqual([
    ["turns", 100, 80],
    ["tokens", 9_000, 10_000],
    ["changed_nothing", 20, 10],
    ["opened", 3, 1],
  ]);
  expect(cards[0]!.figures[3]!.detail).toBe("1 resolved · 0 applied · 0 dismissed");
  expect(cards[2]!.figures[3]!.was).toBeNull();
});

test("a week is said as its days in short", () => {
  expect(weekShort("2026-W46")).toBe("Nov 9 to 15, 2026");
  expect(weekShort("2026-W53")).toBe("Dec 28, 2026 to Jan 3, 2027");
  expect(weekShort("2026-W44")).toBe("Oct 26 to Nov 1, 2026");
  expect(weekShort("soon")).toBe("soon");
});

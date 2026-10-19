/**
 * The board's arithmetic, held to the engine's: the fold a stat draws gives
 * the numbers `dashspec.Fold` gives over the same rows (the Go suite's
 * pipeline fixture), a number is written as its format says, a refresh reaches
 * every dataset standing on the one that moved, and an export guards every
 * text cell a spreadsheet would run.
 */

import { expect, test } from "vitest";
import { toCsv } from "@crewlethq/ui";
import type { DashboardBoardDataset, DashboardRow } from "~/protocol/index.ts";
import {
  compareValues,
  csvOf,
  datasetStatus,
  definitionWords,
  exportName,
  fold,
  formatNumber,
  intervalMs,
  refreshWords,
  withDependents,
} from "./dashboard.ts";

/** The deals the engine's calculation suite folds (`dashspec` `dealsTable`). */
const deals: DashboardRow[] = [
  { _id: "d1", region: "emea", amount: 50000, closed: "2026-10-05" },
  { _id: "d2", region: "amer", amount: 100000, closed: "2026-10-07" },
  { _id: "d3", region: "emea", amount: 30000, closed: "2026-10-12" },
  { _id: "d4", region: "amer", amount: null },
];

test("every fold gives the engine's number over the same rows", () => {
  expect(fold("sum", "number", deals, "amount")).toBe(180000);
  expect(fold("avg", "number", deals, "amount")).toBe(60000);
  expect(fold("min", "number", deals, "amount")).toBe(30000);
  expect(fold("max", "number", deals, "amount")).toBe(100000);
  expect(fold("first", "number", deals, "amount")).toBe(50000);
  expect(fold("last", "number", deals, "amount")).toBe(30000);
  expect(fold("count", "number", deals, "amount")).toBe(3);
  expect(fold("count", "number", deals, "")).toBe(4);
  expect(fold("max", "date", deals, "closed")).toBe("2026-10-12");
  // A FOLD OVER NOTHING IS NO VALUE, never a zero nobody measured; a count
  // of nothing is zero, which is a number of rows.
  expect(fold("sum", "number", [], "amount")).toBeNull();
  expect(fold("count", "number", [], "")).toBe(0);
});

test("values order as the engine orders them, none first", () => {
  expect(compareValues("number", null, 1)).toBe(-1);
  expect(compareValues("number", 2, 10)).toBe(-1);
  expect(compareValues("date", "2026-10-02", "2026-10-01T23:00:00Z")).toBe(1);
  expect(compareValues("boolean", false, true)).toBe(-1);
  expect(compareValues("text", "b", "a")).toBe(1);
});

test("a number is written as its format says, and an absent one as no value", () => {
  expect(formatNumber(1234.567, "number")).toBe((1234.57).toLocaleString());
  expect(formatNumber(1234.567, "integer")).toBe((1235).toLocaleString());
  expect(formatNumber(0.256, "percent")).toBe(`${(25.6).toLocaleString()}%`);
  expect(formatNumber(1_250_000, "compact")).toBe("1.3M");
  expect(formatNumber(3, "number", "deals")).toBe("3 deals");
  expect(formatNumber(null, "number")).not.toMatch(/\d/);
});

const dataset = (
  id: string,
  freshness: DashboardBoardDataset["freshness"],
): DashboardBoardDataset => ({ id, title: id, source: "calc", shape: { columns: [] }, freshness });

test("a refresh reaches every dataset standing on the one that moved, through every calculation", () => {
  const board = [
    dataset("deals", { mode: "live", collection: "deals" }),
    dataset("by_region", { mode: "derived", inputs: ["deals", "targets"] }),
    dataset("total", { mode: "derived", inputs: ["by_region"] }),
    dataset("work", { mode: "manual" }),
  ];
  expect([...withDependents(board, ["deals"])].sort()).toEqual(["by_region", "deals", "total"]);
  expect([...withDependents(board, ["work"])]).toEqual(["work"]);
});

test("an interval is never shorter than the engine's minute", () => {
  expect(intervalMs({ mode: "interval", every_seconds: 120 })).toBe(120_000);
  expect(intervalMs({ mode: "interval", every_seconds: 5 })).toBe(60_000);
  expect(intervalMs({ mode: "manual" })).toBe(0);
  expect(refreshWords({ mode: "interval", every_seconds: 60 })).toBe("Every minute");
  // The state line says "Live"; the refresh line says how, never "Live" twice.
  expect(refreshWords({ mode: "live", collection: "deploys" })).toBe("As its rows are written");
  expect(refreshWords({ mode: "signal", signal: "tokens", every_seconds: 7200 })).toBe(
    "On change, and every 2 hours",
  );
});

test("a dataset's status is what the engine answered, live for a store and stale past two intervals", () => {
  const ready = (received: number) => ({
    answer: { artifact: "a", version: 1, dataset: "d", status: "ready" as const },
    loading: false,
    failure: null,
    received,
  });
  const live = { source: "store" as const, freshness: { mode: "live" as const } };
  const every = {
    source: "engine" as const,
    freshness: { mode: "interval" as const, every_seconds: 60 },
  };
  expect(datasetStatus(live, undefined, 0)).toBe("loading");
  expect(datasetStatus(live, ready(0), 1)).toBe("live");
  expect(datasetStatus(every, ready(0), 60_000)).toBe("ready");
  expect(datasetStatus(every, ready(0), 121_000)).toBe("stale");
  expect(
    datasetStatus(every, { ...ready(0), answer: { ...ready(0).answer, status: "refused" } }, 1),
  ).toBe("refused");
  expect(
    datasetStatus(every, { ...ready(0), answer: { ...ready(0).answer, status: "unanswered" } }, 1),
  ).toBe("failed");
});

test("a widget's CSV is its dataset's rows, every formula-led text cell guarded and every number bare", () => {
  const { header, rows } = csvOf({
    artifact: "a",
    version: 1,
    dataset: "deals",
    status: "ready",
    table: {
      columns: [
        { key: "name", type: "text" },
        { key: "delta", type: "number" },
        { key: "won", type: "boolean" },
      ],
      rows: [
        { name: '=HYPERLINK("x")', delta: -3, won: true },
        { name: "Acme, Inc.", delta: null, won: false },
      ],
    },
  });
  const csv = toCsv(header, rows);
  expect(csv.split("\r\n")[0]).toBe("name,delta,won");
  expect(csv).toContain(`"'=HYPERLINK(""x"")",-3,true`);
  expect(csv).toContain(`"Acme, Inc.",,false`);
  expect(exportName("Q4 Delivery", "pipeline_total", "csv")).toBe("q4-delivery-pipeline-total.csv");
});

test("the rail says what a dataset reads", () => {
  expect(
    definitionWords({
      id: "work",
      title: "My work",
      source: "engine",
      engine: { name: "tracker.items", params: { scope: { param: "scope" }, limit: 50 } },
    }),
  ).toBe("tracker.items, scope the scope filter, limit 50");
  expect(
    definitionWords({
      id: "total",
      title: "Total",
      source: "calc",
      calc: {
        from: "deals",
        steps: [{ op: "group", aggregates: [{ column: "amount", agg: "sum", of: "amount" }] }],
      },
    }),
  ).toBe("From deals; group by all rows: amount = sum(amount)");
});

import { allDays, chartTimeOf, formatDay, tableLayout, wordsOf } from "./dashboard.ts";

test("a calendar day is drawn at its own midnight in the reader's zone, never the evening before", () => {
  // The suite runs in Europe/Berlin (vitest.config.ts): midnight UTC there is
  // already the day, so the West's case is held by the local constructor.
  const at = chartTimeOf("2026-10-01");
  const d = new Date(at);
  expect([d.getFullYear(), d.getMonth(), d.getDate(), d.getHours()]).toEqual([2026, 9, 1, 0]);
  expect(chartTimeOf("2026-10-01T12:00:00Z")).toBe(Date.parse("2026-10-01T12:00:00Z"));
  expect(Number.isNaN(chartTimeOf(3))).toBe(true);
});

test("a time axis of days is labelled with dates, never a time of day", () => {
  expect(allDays(["2026-10-01", "2026-10-02"])).toBe(true);
  expect(allDays(["2026-10-01", "2026-10-02T10:00:00Z"])).toBe(false);
  expect(allDays([])).toBe(false);
  const label = formatDay(chartTimeOf("2026-10-01"));
  expect(label).toMatch(/2026/);
  expect(label).not.toMatch(/\d:\d\d|AM|PM/);
});

test("an id is shown in its column's own words, as the domain's own screens show it", () => {
  expect(wordsOf({ key: "status", type: "text", words: "tracker_status" }, "in_progress")).toBe(
    "In progress",
  );
  expect(wordsOf({ key: "g", type: "text", words: "tracker_status_group" }, "not_started")).toBe(
    "Not started",
  );
  expect(wordsOf({ key: "p", type: "text", words: "tracker_priority" }, "high")).toBe("High");
  expect(wordsOf({ key: "a", type: "text", words: "agent_activity" }, "working")).toBe("Working");
  expect(wordsOf({ key: "x", type: "text" }, "in_progress")).toBe("in_progress");
  expect(wordsOf(undefined, null)).toBe("");
});

test("a table column starts as wide as what it holds, within its bounds", () => {
  const rows: DashboardRow[] = [
    { outcome: "succeeded", n: 4, notes: "x ".repeat(100) },
    { outcome: "failed", n: 12, notes: "short" },
  ];
  const { widths, flex, flexMin } = tableLayout(
    [
      { key: "outcome", label: "Outcome", column: { key: "outcome", type: "text" } },
      { key: "n", label: "N", column: { key: "n", type: "number" } },
      { key: "notes", label: "Notes", column: { key: "notes", type: "text" } },
    ],
    rows,
  );
  // "succeeded" is wider than its heading "OUTCOME": 9 x 7.5 + 36.
  expect(widths.outcome).toBe(104);
  expect(widths.outcome).toBeGreaterThan(7 * 8.5 + 36);
  expect(widths.n).toBe(72);
  expect(widths.notes).toBe(320);
  // The prose column, the text with the longest values, takes the room left.
  expect(flex).toBe("notes");
  // Fitted no narrower than about twenty characters, or its own width if less.
  expect(flexMin).toBe(180);
});

test("a column of a closed vocabulary, of identifiers or of numbers never flexes, however long", () => {
  const rows: DashboardRow[] = [
    { status: "in_progress", n: 123456789, key: "FAIR-2", version: "2026.10.19", note: "a b" },
  ];
  const { flex } = tableLayout(
    [
      {
        key: "status",
        label: "Status",
        column: { key: "status", type: "text", words: "tracker_status" },
      },
      { key: "n", label: "N", column: { key: "n", type: "number" } },
      { key: "key", label: "Key", column: { key: "key", type: "text" } },
      { key: "version", label: "Version", column: { key: "version", type: "text" } },
      { key: "note", label: "Note", column: { key: "note", type: "text" } },
    ],
    rows,
  );
  // An identifier, however long, is held whole; only words with spaces flex.
  expect(flex).toBe("note");
  expect(
    tableLayout([{ key: "n", label: "N", column: { key: "n", type: "number" } }], rows).flex,
  ).toBeUndefined();
});

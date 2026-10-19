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

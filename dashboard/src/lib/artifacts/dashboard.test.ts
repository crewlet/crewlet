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
  // A COUNT OR A SUM OF NO ROWS IS ZERO: the source answered, and nothing
  // totals nought. Rows that hold no value total nothing anybody measured,
  // and a fold that picks or averages has nothing to pick: no value.
  expect(fold("sum", "number", [], "amount")).toBe(0);
  expect(fold("count", "number", [], "")).toBe(0);
  expect(fold("sum", "number", [{ _id: "d4", amount: null }], "amount")).toBeNull();
  for (const agg of ["avg", "min", "max", "first", "last"] as const) {
    expect(fold(agg, "number", [], "amount")).toBeNull();
  }
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
  ).toBe("tracker.items, scope from the scope filter, limit 50");
  // A filter is named as a reader sees it, and a comparison in words.
  const params = [
    { key: "scope", label: "Whose work" },
    { key: "svc", label: "Service" },
  ];
  expect(
    definitionWords(
      {
        id: "work",
        title: "My work",
        source: "engine",
        engine: { name: "tracker.items", params: { scope: { param: "scope" } } },
      },
      params,
    ),
  ).toBe("tracker.items, scope from the Whose work filter");
  expect(
    definitionWords(
      {
        id: "mine",
        title: "Mine",
        source: "calc",
        calc: {
          from: "deploys",
          steps: [
            {
              op: "filter",
              where: [
                { column: "service", cmp: "eq", value: { param: "svc" } },
                { column: "cost", cmp: "present" },
              ],
            },
          ],
        },
      },
      params,
    ),
  ).toBe("From deploys; keep rows where service is the Service filter and cost has a value");
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

import { allDays, chartTimeOf, formatDay, tableProseColumn, wordsOf } from "./dashboard.ts";

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
  const label = formatDay(chartTimeOf("2026-10-01"), Date.parse("2027-01-04T12:00:00Z"));
  expect(label).toMatch(/2026/);
  expect(label).not.toMatch(/\d:\d\d|AM|PM/);
  // The year a reader is in goes without saying.
  expect(formatDay(chartTimeOf("2026-10-01"), Date.parse("2026-10-19T12:00:00Z"))).not.toMatch(
    /2026/,
  );
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

test("the prose column of a table is the free text with the longest words in it", () => {
  const rows: DashboardRow[] = [
    { outcome: "succeeded", n: 4, notes: "x ".repeat(100) },
    { outcome: "failed", n: 12, notes: "short" },
  ];
  expect(
    tableProseColumn(
      [
        { key: "outcome", column: { key: "outcome", type: "text" } },
        { key: "n", column: { key: "n", type: "number" } },
        { key: "notes", column: { key: "notes", type: "text" } },
      ],
      rows,
    ),
  ).toBe("notes");
});

test("a column of a closed vocabulary, of identifiers or of numbers is never the prose, however long", () => {
  const rows: DashboardRow[] = [
    { status: "in_progress now", n: 123456789, key: "FAIR-2", version: "2026.10.19", note: "a b" },
  ];
  const shown = (keys: string[]) =>
    keys.map((key) => ({
      key,
      column: {
        key,
        type: key === "n" ? ("number" as const) : ("text" as const),
        ...(key === "status" ? { words: "tracker_status" as const } : {}),
      },
    }));
  // An identifier, however long, is held whole; only words with spaces flex.
  expect(tableProseColumn(shown(["status", "n", "key", "version", "note"]), rows)).toBe("note");
  // WITH NO PROSE the first column takes the room, so a table reaches its side.
  expect(tableProseColumn(shown(["key", "n"]), rows)).toBe("key");
});

import {
  boardSpan,
  cellText,
  figureFormat,
  foldSeries,
  OTHER_SERIES,
  scaleFormat,
} from "./dashboard.ts";

test("past the ramp's four hues and a residual, a chart's series fold into one Other", () => {
  const five = ["a", "b", "c", "d", "e"].map((id) => ({ id, name: id.toUpperCase() }));
  // Five fit: the fifth takes the residual and keeps its name.
  expect(foldSeries(five).series).toEqual(five);
  const six = [...five, { id: "f", name: "F" }];
  const { series, fold } = foldSeries(six);
  expect(series.map((s) => s.name)).toEqual(["A", "B", "C", "D", "Other"]);
  expect(fold("b")).toBe("b");
  expect(fold("e")).toBe(OTHER_SERIES);
  expect(fold("f")).toBe(OTHER_SERIES);
});

test("a narrow widget widens to an even span, never drawn under its least width", () => {
  // A board 807px wide with a 16px gap: two columns are 121px, which is enough.
  expect(boardSpan(2, 807, 16)).toBe(2);
  // At 710px two columns are 105px: three are drawn instead, and four tile a row.
  expect(boardSpan(2, 710, 16)).toBe(3);
  expect(boardSpan(1, 710, 16)).toBe(3);
  // A widget wide enough keeps its own span, even an uneven one.
  expect(boardSpan(5, 710, 16)).toBe(5);
  // Not yet measured, a widget keeps its span.
  expect(boardSpan(2, 0, 16)).toBe(2);
});

test("a figure's number reads compact unless its author chose a format", () => {
  expect(figureFormat({})).toBe("compact");
  expect(figureFormat({ format: "integer" })).toBe("integer");
});

test("a chart's scale is written one way on every line, short from a thousand and without its unit", () => {
  const scale = scaleFormat({ format: "integer" });
  expect([0, 2500, 7500, 10000, -20000].map(scale)).toEqual(["0", "2.5k", "7.5k", "10k", "-20k"]);
  expect(scaleFormat({ format: "percent" })(0.25)).toBe("25%");
});

test("a table cell writes a date as a reader does, never as the engine stores it", () => {
  const day = cellText({ key: "d", type: "date" }, "2026-09-01");
  expect(day).not.toMatch(/2026-09-01/);
  expect(day).toMatch(/Sep/);
  expect(cellText({ key: "d", type: "date" }, "2026-09-01T10:00:00Z")).not.toMatch(/T10/);
  expect(cellText({ key: "b", type: "boolean" }, true)).toBe("Yes");
  expect(cellText({ key: "n", type: "number" }, 1234.567)).toBe((1234.57).toLocaleString());
});

// AN ALL-DAY DUE DATE IS STORED AS THE COMPANY'S MIDNIGHT, and it has no time to
// draw: it read "12:00 AM". The suite runs in Europe/Berlin. Mutation: drop
// `atMidnight`, and the first case reads the instant.
test("an instant on a midnight where the reader is reads as that day, and any other with its time", () => {
  const column = { key: "d", type: "date" as const };
  expect(cellText(column, "2026-08-31T22:00:00Z")).toBe(cellText(column, "2026-09-01"));
  expect(cellText(column, "2026-09-01T08:30:00Z")).toMatch(/10:30/);
});

// A SEAT'S VALUE STAYS ITS HANDLE, and a reader is shown the chart's name.
test("a seat is written by the name the chart gives it, and by its handle with no chart", () => {
  const column = { key: "a", type: "text" as const, words: "seat" as const };
  expect(wordsOf(column, "pm", () => "Product Manager")).toBe("Product Manager");
  expect(cellText(column, "pm", () => "Product Manager")).toBe("Product Manager");
  expect(wordsOf(column, "pm")).toBe("pm");
});

test("an absent value and a priority of none are written as nothing, which a table marks", () => {
  expect(cellText({ key: "d", type: "date" }, null)).toBe("");
  expect(cellText({ key: "t", type: "text" }, "")).toBe("");
  expect(cellText({ key: "p", type: "text", words: "tracker_priority" }, "none")).toBe("");
  expect(cellText({ key: "p", type: "text", words: "tracker_priority" }, "high")).toBe("High");
});

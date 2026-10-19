/**
 * One widget's figure, drawn from the rows its dataset answered: every figure
 * says what it shows (a count names what it counts, a meter its value and its
 * maximum, a share each part and its value, a note its title), a time axis of
 * days reads as dates, and a table shows a domain's ids in its own words.
 */

import { cleanup, render, screen, within } from "@testing-library/react";
import { afterEach, expect, test } from "vitest";
import { ToastProvider } from "@crewlethq/ui";

import { DashboardWidget } from "./DashboardWidget.tsx";
import type {
  DashboardBoardDataset,
  DashboardColumn,
  DashboardRow,
  DashboardWidget as Widget,
} from "~/protocol/index.ts";

afterEach(cleanup);

function draw(widget: Widget, columns: DashboardColumn[], rows: DashboardRow[], title = "Deploys") {
  const dataset: DashboardBoardDataset = {
    id: "ds",
    title,
    source: "store",
    shape: { columns },
    freshness: { mode: "live", collection: "ds" },
  };
  render(
    <ToastProvider>
      <DashboardWidget
        boardTitle="Pulse"
        widget={{ ...widget, dataset: widget.type === "note" ? undefined : "ds" }}
        dataset={widget.type === "note" ? undefined : dataset}
        answer={{
          artifact: "a",
          version: 1,
          dataset: "ds",
          status: "ready",
          as_of: "2026-10-18T10:00:00Z",
          table: { columns, rows },
        }}
        status="ready"
        detail={undefined}
        asOf="2026-10-18T10:00:00Z"
        onCite={undefined}
        onFocus={() => {}}
      />
    </ToastProvider>,
  );
}

test("a count names what it counts: its dataset, never the word Rows", () => {
  draw(
    { id: "n", type: "stat", title: "Deploys this month", agg: "count", w: 3 },
    [],
    [{}, {}, {}],
  );
  expect(screen.queryByText("Rows")).toBeNull();
  expect(screen.getAllByText("Deploys").length).toBeGreaterThan(0);
  expect(screen.getByText("3")).toBeTruthy();
});

test("a meter shows what it measures, its count and its maximum", () => {
  draw(
    { id: "m", type: "meter", title: "Failed or rolled back", agg: "count", max: 5, w: 6 },
    [{ key: "outcome", type: "text" }],
    [{ outcome: "failed" }, { outcome: "failed" }, { outcome: "rolled_back" }],
    "Failed deploys",
  );
  expect(screen.getByText("3 of 5")).toBeTruthy();
  expect(screen.getAllByText("Failed deploys").length).toBeGreaterThan(0);
});

test("a share names each part and its value beside the bar", () => {
  draw(
    { id: "s", type: "share", title: "Who spent", label: "handle", value: "tokens", w: 6 },
    [
      { key: "handle", type: "text" },
      { key: "tokens", type: "number" },
    ],
    [
      { handle: "crewlet-pm", tokens: 4000 },
      { handle: "crewlet-swe", tokens: 5000 },
    ],
  );
  const legend = within(screen.getByRole("list"));
  expect(legend.getByText("crewlet-pm")).toBeTruthy();
  expect(legend.getByText("crewlet-swe")).toBeTruthy();
  expect(legend.getByText(/5,000|5000/)).toBeTruthy();
});

test("a note shows its title above its words", () => {
  draw(
    { id: "t", type: "note", title: "How to read this board", text: "Live as you.", w: 4 },
    [],
    [],
  );
  expect(screen.getByRole("heading", { name: "How to read this board" })).toBeTruthy();
  // The note's own heading names it, as a frame's title names a figure: a
  // card on the board, never a label over loose prose.
  expect(screen.getByRole("region", { name: "How to read this board" })).toBeTruthy();
});

test("a table reaches its frame's sides, with no pager or settings over its rows", () => {
  draw(
    { id: "w", type: "table", title: "Open work", columns: ["key"], w: 8 },
    [{ key: "key", type: "text" }],
    [{ key: "FAIR-1" }, { key: "FAIR-2" }],
  );
  const frame = screen.getByRole("region", { name: "Open work" });
  expect(frame.classList.contains("crewlet-widget-frame--flush")).toBe(true);
  expect(frame.querySelector(".crewlet-data-table__toolbar")).toBeNull();
  expect(screen.queryByRole("button", { name: /settings/i })).toBeNull();
  expect(screen.queryByRole("button", { name: /sort/i })).toBeNull();
});

test("a figure says its type, which the stylesheet stands a stat and a meter on", () => {
  draw({ id: "n", type: "stat", title: "Deploys", agg: "count", w: 3 }, [], [{}]);
  expect(document.querySelector(".dashboard-figure")?.getAttribute("data-figure")).toBe("stat");
  cleanup();
  draw(
    { id: "w", type: "table", title: "Open work", columns: ["key"], w: 8 },
    [{ key: "key", type: "text" }],
    [{ key: "FAIR-1" }],
  );
  expect(
    screen
      .getByRole("region", { name: "Open work" })
      .classList.contains("crewlet-widget-frame--flush"),
  ).toBe(true);
  expect(document.querySelector(".dashboard-figure")?.getAttribute("data-figure")).toBe("table");
});

test("a table shows a tracker status in Work's words, never its id", () => {
  draw(
    { id: "w", type: "table", title: "Open work", columns: ["key", "status"], w: 8 },
    [
      { key: "key", type: "text" },
      { key: "status", type: "text", words: "tracker_status" },
    ],
    [{ key: "FAIR-1", status: "in_progress" }],
  );
  expect(screen.getByText("In progress")).toBeTruthy();
  expect(screen.queryByText("in_progress")).toBeNull();
  // Each column is held at what it holds, so the table never fits it narrower.
  // "In progress" is eleven characters: 11 x 7.5 + 36 = 118.5, held at 119px.
  const heading = screen
    .getAllByRole("columnheader")
    .find((h) => /status/i.test(h.textContent ?? ""));
  expect(heading?.style.width).toBe("119px");
});

test("a chart over days labels its axis with dates and no time of day", () => {
  draw(
    { id: "c", type: "timeseries", title: "Deploys per day", x: "day", y: ["n"], w: 12 },
    [
      { key: "day", type: "date" },
      { key: "n", type: "number" },
    ],
    [
      { day: "2026-10-01", n: 3 },
      { day: "2026-10-18", n: 5 },
    ],
  );
  const text = document.body.textContent ?? "";
  expect(text).not.toMatch(/\d{1,2}:\d{2}(:\d{2})?\s?(AM|PM)?/);
  expect(text).toMatch(/2026/);
});

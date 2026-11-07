/**
 * One widget's figure, drawn from the rows its dataset answered: every figure
 * says what it shows (a count names what it counts, a meter its value and its
 * maximum, a share each part and its value, a note its title), a time axis of
 * days reads as dates, and a table shows a domain's ids in its own words.
 */

import { cleanup, render, screen, within } from "@testing-library/react";
import { afterEach, expect, test, vi } from "vitest";
import { ToastProvider } from "@crewlethq/ui";

import { DashboardWidget } from "./DashboardWidget.tsx";
import type {
  DashboardBoardDataset,
  DashboardColumn,
  DashboardRow,
  DashboardWidget as Widget,
} from "~/protocol/index.ts";

vi.mock("~/lib/artifacts/people.ts", async (original) => ({
  ...(await original<typeof import("~/lib/artifacts/people.ts")>()),
  useSeatNames: () => (handle: string) => (handle === "pm" ? "Product Manager" : handle),
}));

afterEach(cleanup);

function draw(
  widget: Widget,
  columns: DashboardColumn[],
  rows: DashboardRow[],
  title = "Deploys",
  source?: Pick<DashboardBoardDataset, "source" | "engine">,
) {
  const dataset: DashboardBoardDataset = {
    id: "ds",
    title,
    source: "store",
    shape: { columns },
    freshness: { mode: "live", collection: "ds" },
    ...source,
  };
  render(
    <ToastProvider>
      <DashboardWidget
        boardTitle="Pulse"
        widget={{ ...widget, dataset: widget.type === "note" ? undefined : "ds" }}
        dataset={widget.type === "note" ? undefined : dataset}
        answer={{
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
  // The count at the tile's step, what it is out of under it, and the bar,
  // which a screen reader hears it all from once.
  expect(screen.getByText("3")).toBeTruthy();
  expect(screen.getByText("of 5")).toBeTruthy();
  expect(screen.getByRole("meter").getAttribute("aria-valuetext")).toBe("3 of 5");
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
  expect(within(frame).getByRole("table")).toBeTruthy();
  expect(screen.queryByRole("button", { name: /settings/i })).toBeNull();
  expect(screen.queryByRole("button", { name: /^key/i })).toBeNull();
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
  expect(text).toMatch(/Oct/);
});

/** An engine source as the catalogue lists it, windowed or not. */
function engineSource(
  name: string,
  windowed: boolean,
): Pick<DashboardBoardDataset, "source" | "engine"> {
  return {
    source: "engine",
    engine: {
      name,
      domain: name.split(".")[0]!,
      summary: "",
      access: "",
      params: [],
      columns: [],
      key: "",
      max_rows: 500,
      ...(windowed ? { windowed: true } : {}),
    },
  };
}

test("every widget kind says one phrase over no rows, worded by whether its source has a window", () => {
  const columns = [
    { key: "day", type: "date" as const },
    { key: "n", type: "number" as const },
    { key: "label", type: "text" as const },
  ];
  const widgets = [
    { id: "t", type: "timeseries" as const, title: "T", x: "day", y: ["n"], w: 6 },
    { id: "c", type: "columns" as const, title: "C", x: "day", y: ["n"], w: 6 },
    { id: "a", type: "activity" as const, title: "A", x: "day", y: ["n"], w: 6 },
    { id: "b", type: "bars" as const, title: "B", label: "label", value: "n", w: 6 },
    { id: "s", type: "share" as const, title: "S", label: "label", value: "n", w: 6 },
    { id: "r", type: "table" as const, title: "R", columns: ["label", "n"], w: 6 },
  ];
  // A span of company days: nothing happened in it.
  for (const widget of widgets) {
    draw(widget, columns, [], "Spend", engineSource("spend.daily", true));
    expect(screen.getAllByText("Nothing in this window").length).toBeGreaterThan(0);
    cleanup();
  }
  // What is true now, from the engine or the artifact's own rows: there is
  // no window to name.
  for (const source of [engineSource("tracker.workload", false), undefined]) {
    for (const widget of widgets) {
      draw(widget, columns, [], "Workload", source);
      expect(screen.getAllByText("Nothing to show").length).toBeGreaterThan(0);
      expect(screen.queryByText("Nothing in this window")).toBeNull();
      cleanup();
    }
  }
});

test("a stat that counts or sums no rows is 0, and one with no value to pick is the empty mark", () => {
  const columns = [{ key: "open", type: "number" as const }];
  for (const agg of ["sum", "count"] as const) {
    draw({ id: "o", type: "stat", title: "Open work", agg, value: "open", w: 3 }, columns, []);
    expect(screen.getByText("0")).toBeTruthy();
    cleanup();
  }
  // Rows that hold no value, and a fold that picks one, have none to show.
  draw({ id: "o", type: "stat", title: "Open work", agg: "sum", value: "open", w: 3 }, columns, [
    { open: null },
  ]);
  expect(screen.queryByText("0")).toBeNull();
  cleanup();
  draw(
    { id: "o", type: "stat", title: "Open work", agg: "last", value: "open", w: 3 },
    columns,
    [],
  );
  expect(screen.queryByText("0")).toBeNull();
});

// THE DATASET ORDERED AND CUT THE ROWS, so a head that sorted what is left would
// read as an order over every row. Mutation: give a column a `sortValue`.
test("a widget's table sorts nothing: no head is a button", () => {
  draw(
    { id: "w", type: "table", title: "Open work", columns: ["key", "title"], w: 8 },
    [
      { key: "key", type: "text" },
      { key: "title", type: "text" },
    ],
    [{ key: "FAIR-1", title: "Ship the release" }],
  );
  const heads = screen.getAllByRole("columnheader");
  expect(heads.map((h) => h.textContent)).toEqual(["key", "title"]);
  for (const head of heads) expect(within(head).queryByRole("button")).toBeNull();
});

// A SEAT IS READ BY ITS NAME, in a table as on a bar, while its value stays the
// handle a filter compares. Mutation: drop `seat` from `wordsOf`.
test("a seat column draws the chart's name, in a table and on a bar", () => {
  const columns: DashboardColumn[] = [
    { key: "assignee", type: "text", words: "seat" },
    { key: "open", type: "number" },
  ];
  draw({ id: "w", type: "table", title: "Work", columns: ["assignee"], w: 8 }, columns, [
    { assignee: "pm", open: 2 },
  ]);
  expect(screen.getByText("Product Manager")).toBeTruthy();
  expect(screen.queryByText("pm")).toBeNull();
  cleanup();
  draw({ id: "b", type: "bars", title: "Load", label: "assignee", value: "open", w: 8 }, columns, [
    { assignee: "pm", open: 2 },
  ]);
  expect(screen.getByText("Product Manager")).toBeTruthy();
});

test("a table draws every absent value as the empty mark, a priority of none included", () => {
  draw(
    { id: "w", type: "table", title: "Open work", columns: ["key", "priority", "due"], w: 8 },
    [
      { key: "key", type: "text" },
      { key: "priority", type: "text", words: "tracker_priority" },
      { key: "due", type: "date" },
    ],
    [{ key: "ENG-1", priority: "none", due: null }],
  );
  expect(screen.queryByText("None")).toBeNull();
  expect(document.querySelectorAll(".crewlet-empty-value")).toHaveLength(2);
});

test("a chart of one named column draws no key, and a split of one band names it", () => {
  const columns = [
    { key: "day", type: "date" as const },
    { key: "n", type: "number" as const, label: "Deploys" },
    { key: "team", type: "text" as const },
  ];
  const rows = [
    { day: "2026-10-01", n: 3, team: "Platform" },
    { day: "2026-10-02", n: 5, team: "Platform" },
  ];
  draw(
    { id: "c", type: "columns", title: "Deploys per day", x: "day", y: ["n"], w: 6 },
    columns,
    rows,
  );
  expect(document.querySelector(".crewlet-legend")).toBeNull();
  cleanup();
  draw(
    { id: "c", type: "columns", title: "Deploys per day", x: "day", y: ["n"], by: "team", w: 6 },
    columns,
    rows,
  );
  expect(
    within(document.querySelector(".crewlet-legend") as HTMLElement).getByText("Platform"),
  ).toBeTruthy();
});

test("in a row, a stat's and a meter's frame hold their title at two lines", () => {
  render(
    <ToastProvider>
      <DashboardWidget
        boardTitle="Pulse"
        widget={{ id: "s", type: "stat", title: "Open", dataset: "ds", agg: "count", w: 3 }}
        dataset={{
          id: "ds",
          title: "Deploys",
          source: "store",
          shape: { columns: [] },
          freshness: { mode: "live", collection: "ds" },
        }}
        answer={{
          status: "ready",
          table: { columns: [], rows: [{}] },
        }}
        status="ready"
        detail={undefined}
        asOf={undefined}
        onCite={undefined}
        onFocus={() => {}}
        inRow
      />
    </ToastProvider>,
  );
  expect(document.querySelector(".crewlet-widget-frame--hold-title")).not.toBeNull();
});

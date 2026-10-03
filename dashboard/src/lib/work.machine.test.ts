/**
 * The list machine's own arithmetic: the two filters with an operator, the
 * Recent segment, the order `[` and `]` walk, the busy day, and the question a
 * task page carries back to its list.
 *
 * Every rule here has a wrong form that narrows the work to a DIFFERENT set
 * than the chip says — `≥ normal` sent as `normal`, "not wontfix" sent as
 * "wontfix", a day that is the company's rather than the reader's — and none
 * of them throws.
 */

import { expect, test } from "vitest";

import {
  aroundParams,
  buildItemsParams,
  dayOfRange,
  dayRange,
  drawnRows,
  dueFilterLabel,
  filterChips,
  finishedLanes,
  hiddenLanes,
  hideParam,
  listParam,
  loadedOf,
  manualOrder,
  NO_FILTERS,
  priorityFilter,
  readPriorityFilter,
  readTagFilter,
  seededScope,
  tagFilter,
  viewQuery,
} from "./work.ts";
import { mergePages } from "~/routes/work/usePagedItems.ts";
import type { WorkGroup, WorkSummary } from "~/protocol/index.ts";

const row = (key: string, over: Partial<WorkSummary> = {}): WorkSummary => ({
  id: key.toLowerCase(),
  key,
  project: "ENG",
  title: key,
  type: "task",
  status: "todo",
  updated: "2031-04-16T00:00:00Z",
  version: 1,
  ...over,
});

// ---------------------------------------------------------------------------
// Priority: is, is not, ≥
// ---------------------------------------------------------------------------

// THE WIRE HAS ONE SPELLING — a list — so the operator is expanded on the way
// out, and a saved view carrying it stays a plain list any reader can run.
test("≥ normal writes the CSV of normal and every step above it", () => {
  expect(priorityFilter("atleast", "normal")).toBe("normal,high,urgent");
  expect(priorityFilter("not", "low")).toBe("none,normal,high,urgent");
  expect(priorityFilter("is", "high")).toBe("high");
  // AND THE ADDRESS READS BACK AS THE OPERATOR THAT WROTE IT, so a pasted link
  // and a chip agree.
  for (const op of ["is", "not", "atleast"] as const) {
    for (const value of ["low", "normal", "high"]) {
      expect(readPriorityFilter(priorityFilter(op, value)), `${op} ${value}`).toEqual({
        op,
        values: [value],
      });
    }
  }
});

test("a priority list written by hand is read as exactly what it holds", () => {
  expect(readPriorityFilter("low,urgent")).toEqual({ op: "is", values: ["low", "urgent"] });
  // "is not none" and "≥ low" are one set, and the reader says it the short way.
  expect(readPriorityFilter("low,normal,high,urgent")).toEqual({ op: "atleast", values: ["low"] });
  const chip = filterChips({ ...NO_FILTERS, priority: "normal,high,urgent" })[0]!;
  expect([chip.verb, chip.value]).toEqual(["≥", "Normal"]);
});

// ---------------------------------------------------------------------------
// Labels: any, all, not
// ---------------------------------------------------------------------------

// THE GRAMMAR'S OWN THREE MODES, and `any` bare — the engine's default and what
// every address already holds, so there are not two spellings of one filter.
test("labels not writes none:, all writes all:, and any is bare", () => {
  expect(tagFilter("none", ["wontfix"])).toBe("none:wontfix");
  expect(tagFilter("all", ["api", "flaky"])).toBe("all:api,flaky");
  expect(tagFilter("any", ["api", "flaky"])).toBe("api,flaky");
  expect(tagFilter("none", [])).toBe("");
  expect(readTagFilter("none:wontfix")).toEqual({ mode: "none", tags: ["wontfix"] });
  expect(readTagFilter("api,flaky")).toEqual({ mode: "any", tags: ["api", "flaky"] });
  const chip = filterChips({ ...NO_FILTERS, tag: "none:wontfix" })[0]!;
  expect([chip.verb, chip.value]).toEqual(["not", "wontfix"]);
  // NAMED AS EVERY OTHER SURFACE NAMES THE FIELD — the card, the rail, the
  // About lens and the New task sheet all say Labels, never "Tag".
  expect(chip.label).toBe("Labels");
});

// ---------------------------------------------------------------------------
// The Recent segment
// ---------------------------------------------------------------------------

// RECENT HAS ONE HOME: a board with nothing said about finished work opens on
// it, every other shape opens on Open, and a view that said something keeps
// what it said.
test("the scope Recent has one home", () => {
  expect(seededScope({}, "board")).toBe("recent");
  expect(seededScope({}, "list")).toBe("open");
  expect(seededScope({ status_group: "done,closed" }, "board")).toBe("closed");
  expect(seededScope({ show_closed: "true" }, "board")).toBe("all");
  expect(seededScope({ closed_since: "sow" }, "list")).toBe("recent");
  // AND ON THE WIRE IT IS `closed_since` ALONE, even over a view that widened
  // with `show_closed` — the engine refuses the two together.
  const params = buildItemsParams({
    container: "workspace",
    shape: "board",
    view: { show_closed: "true" },
    filters: { ...NO_FILTERS, scope: "recent", removed: true },
  });
  expect(params.closed_since).toBe("sow");
  expect(params.show_closed).toBeUndefined();
});

// THE DONE LANE UNDER RECENT HOLDS THIS WEEK'S DELIVERIES, so its "N more" is
// the week's — and only a finished lane on a status axis is one.
test("the finished lanes are the status axes' done and closed lanes", () => {
  expect([...finishedLanes("status")].sort()).toEqual(["cancelled", "closed", "done"]);
  expect([...finishedLanes("status_group")].sort()).toEqual(["closed", "done"]);
  expect(finishedLanes("assignee").size).toBe(0);
});

// ---------------------------------------------------------------------------
// The order `[` and `]` walk
// ---------------------------------------------------------------------------

// THE DRAWING ORDER: lanes left to right and each top to bottom, sub-bands
// through, a hidden lane skipped and a task a label board draws twice visited
// where it is first drawn — the engine's own order for `around=`.
test("peek steps through board lanes and a grouped list in drawing order", () => {
  const groups: WorkGroup[] = [
    { key: "todo", count: 2, rows: [row("ENG-1"), row("ENG-2")] },
    { key: "done", count: 1, rows: [row("ENG-9")] },
    { key: "in_progress", count: 1, rows: [row("ENG-3"), row("ENG-1")] },
  ];
  expect(drawnRows([], groups).map((r) => r.key)).toEqual(["ENG-1", "ENG-2", "ENG-9", "ENG-3"]);
  expect(drawnRows([], groups, new Set(["done"])).map((r) => r.key)).toEqual([
    "ENG-1",
    "ENG-2",
    "ENG-3",
  ]);
  const nested: WorkGroup[] = [
    {
      key: "todo",
      count: 2,
      rows: [],
      subgroups: [
        { key: "ada", count: 1, rows: [row("ENG-5")] },
        { key: "", count: 1, rows: [row("ENG-4")] },
      ],
    },
  ];
  expect(drawnRows([], nested).map((r) => r.key)).toEqual(["ENG-5", "ENG-4"]);
  // AN UNGROUPED ANSWER IS ITS ROWS.
  expect(drawnRows([row("ENG-7")], []).map((r) => r.key)).toEqual(["ENG-7"]);
});

// THE LANES PUT AWAY round-trip through the address, the unset lane included —
// a list of empty strings is a list nobody can read back.
test("hidden lanes round-trip through hide=, the unset lane as a dash", () => {
  const written = hideParam(["done", ""]);
  expect(written).toBe("done,-");
  expect([...hiddenLanes(written)]).toEqual(["done", ""]);
});

// ---------------------------------------------------------------------------
// Every row reachable
// ---------------------------------------------------------------------------

// A CURSOR RESUMES AFTER THE LAST ROW IT WAS MINTED ON, so a task that moved
// between two asks can arrive on both pages — and is drawn once, where it was
// first drawn.
test("rows past the first page are merged in once each", () => {
  expect(
    mergePages([
      [row("ENG-1"), row("ENG-2")],
      [row("ENG-2"), row("ENG-3")],
    ]).map((r) => r.key),
  ).toEqual(["ENG-1", "ENG-2", "ENG-3"]);
  expect(loadedOf(100, 240)).toBe("100 of 240 loaded");
  expect(loadedOf(100, 10000, true)).toBe("100 of 10,000+ loaded");
  expect(loadedOf(100, 100)).toBe("100 loaded");
});

// A BUSY DAY IS THE READER'S DAY, bounded by the reader's own midnights — a
// bare date resolves on the company's clock, and a task due at 23:30 in the
// cell would be missing from the list its "+N more" opened.
test("every task on a busy day is reachable by a range of the reader's own day", () => {
  const range = dayRange("2031-04-16");
  const [from, to] = range.replace("range:", "").split("..");
  expect(new Date(from!).getDate()).toBe(16);
  expect(new Date(from!).getHours()).toBe(0);
  expect(Date.parse(to!) - Date.parse(from!)).toBeGreaterThanOrEqual(23 * 3_600_000);
  expect(dayOfRange(range)).toBe("2031-04-16");
  expect(dueFilterLabel(range)).toMatch(/^on /);
  // AND A RANGE SOMEBODY WROTE BY HAND IS NOT CLAIMED AS ONE DAY.
  expect(dayOfRange("range:2031-04-16..2031-04-18")).toBe("");
});

// ---------------------------------------------------------------------------
// The list a task was opened from
// ---------------------------------------------------------------------------

// THE QUESTION, NEVER THE ROWS: a task page asks it again with `around=` and is
// told where the task sits in the WHOLE answer.
test("3 of 18 comes from around=, asked of the list's own question", () => {
  const list = listParam({
    container: "project:ENG",
    group_by: "status",
    group_limit: 50,
    fields: "tags,spend",
    closed_since: "sow",
  });
  expect(new URLSearchParams(list).has("group_limit")).toBe(false);
  expect(new URLSearchParams(list).has("fields")).toBe(false);
  expect(aroundParams(list, "ENG-4")).toEqual({
    container: "project:ENG",
    group_by: "status",
    closed_since: "sow",
    around: "ENG-4",
    group_limit: 1,
  });
  expect(aroundParams(listParam({ container: "workspace", limit: 100 }), "ENG-4")).toEqual({
    container: "workspace",
    around: "ENG-4",
    limit: 1,
  });
  expect(aroundParams("", "ENG-4")).toBeNull();
});

// A SAVED VIEW IS THE QUESTION WITHOUT ITS PAGING, and a calendar's month is
// its axis rather than a narrowing — saved, it would open on that month for
// ever.
test("a saved view carries the query and not the page or the calendar's window", () => {
  expect(
    viewQuery(
      { container: "workspace", priority: "high", limit: 500, due: "range:a..b", sort: "due" },
      "calendar",
    ),
  ).toEqual({ priority: "high" });
  expect(viewQuery({ container: "workspace", sort: "due", cursor: "c" }, "list")).toEqual({
    sort: "due",
  });
});

// A DRAG CHANGES THE MANUAL ORDER ONLY: said out loud, or nothing said inside a
// project, whose default it is — never the company's unsorted list.
test("only the manual order is one a drag can change", () => {
  expect(manualOrder("rank", "")).toBe(true);
  expect(manualOrder("", "ENG")).toBe(true);
  expect(manualOrder("", "")).toBe(false);
  expect(manualOrder("due", "ENG")).toBe(false);
});

// EVERY ROW IS FILTERED ON ITS OWN, on every shape and over any view. The
// grammar's default filters ROOTS and lets their subtrees ride along, and no
// shape here draws a tree — so under Open a flat list listed the finished
// subtasks of every open parent, and a person's own day drew subtasks held by
// somebody else.
test("no shape lets a subtree ride along on its root", () => {
  for (const shape of ["list", "board", "table", "calendar", "timeline"] as const) {
    const params = buildItemsParams({
      container: "workspace",
      shape,
      view: { subtasks: "collapsed" },
      filters: { ...NO_FILTERS, scope: "open" },
      range: { from: "2031-04-01", to: "2031-05-01" },
      lock: { assignee: "ada" },
    });
    expect(params.subtasks, shape).toBe("separate");
  }
});

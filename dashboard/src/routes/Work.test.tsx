/**
 * What the board screen derives, and what it draws once it has.
 *
 * # The derivation
 *
 * A grouped answer replaces `items` with `groups`, because returning both
 * would be the same rows twice, so every number the header shows and the empty
 * panel's own guard had to learn about the second shape. They did not: a fully
 * populated board reported 0 shown, 0 in progress and 0 blocked, and drew "No
 * work has been filed yet" directly underneath its own columns.
 *
 * # The drawing
 *
 * The cases below the derivation are about the same facts reaching a reader
 * through the design system's own markup: a column's count is over the whole
 * set, a blocker sits BESIDE a status rather than instead of it, the open and
 * closed control asks three questions rather than two, and the two empty
 * answers this screen can give are told apart. Each of them is a claim about
 * what the screen says, so each is asserted through what is on screen rather
 * than through the shape of the answer.
 */

import { cleanup, fireEvent, render, screen, within } from "@testing-library/react";
import { afterEach, expect, test } from "vitest";
import { projectKeys, shownRows, Work } from "./Work.tsx";
import { Router } from "~/app/router.tsx";
import { ClientContext } from "~/lib/store-hooks.ts";
import { LiveSocket, Store } from "~/protocol/index.ts";
import type {
  WorkGroup,
  WorkItemsAnswer,
  WorkProjectRow,
  WorkProjectsAnswer,
  WorkSummary,
  WorkViewsAnswer,
} from "~/protocol/index.ts";

const row = (id: string, over: Partial<WorkSummary> = {}): WorkSummary => ({
  id,
  key: "ENG-" + id,
  project: "ENG",
  title: id,
  status: "todo",
  updated: "2031-04-16T00:00:00Z",
  version: 1,
  ...over,
});

test("a flat answer's rows are its items", () => {
  const items = [row("a"), row("b")];
  expect(shownRows(items, [])).toEqual(items);
});

test("a grouped answer's rows are its columns' rows, and items is empty", () => {
  const groups: WorkGroup[] = [
    { key: "todo", count: 40, rows: [row("a"), row("b")] },
    { key: "in_progress", count: 7, rows: [row("c")] },
  ];
  // THE SERVER SENDS NO `items` HERE, which is the whole shape of a grouped
  // answer, so a screen reading `items` sees an empty board.
  expect(shownRows([], groups).map((r) => r.id)).toEqual(["a", "b", "c"]);
});

test("a subgroup's rows are not counted twice", () => {
  const groups: WorkGroup[] = [
    {
      key: "todo",
      count: 2,
      rows: [row("a"), row("b")],
      subgroups: [{ key: "api", count: 1, rows: [row("a")] }],
    },
  ];
  expect(shownRows([], groups).map((r) => r.id)).toEqual(["a", "b"]);
});

// THE PROJECT FILTER OFFERS THE COMPANY'S PROJECTS, not the page's.
//
// Derived from the rows it could only ever offer what was already on screen,
// so a board narrowed to one project offered exactly that one and no way back
// to another: the filter became a one-way door.
test("the project filter comes from the listing, not from the visible rows", () => {
  const listed: WorkProjectRow[] = [
    {
      key: "ENG",
      name: "Engineering",
      unit: { resolved: true },
      lead: {},
      task_counts: { open: 1, done: 0, closed: 0 },
      version: 1,
    },
    {
      key: "OPS",
      name: "Operations",
      unit: { resolved: true },
      lead: {},
      task_counts: { open: 0, done: 0, closed: 0 },
      version: 1,
    },
  ];
  expect(projectKeys(listed, [row("a")])).toEqual(["ENG", "OPS"]);
});

// AND FALLS BACK RATHER THAN EMPTYING while the listing is in flight: a
// control that disappears on every re-read is worse than one offering less.
test("the filter falls back to the rows before the listing arrives", () => {
  expect(projectKeys(undefined, [row("a"), row("b", { project: "OPS" })])).toEqual(["ENG", "OPS"]);
  expect(projectKeys([], [row("a")])).toEqual(["ENG"]);
});

// ---------------------------------------------------------------------------
// The screen
// ---------------------------------------------------------------------------

const items = (over: Partial<WorkItemsAnswer> = {}): WorkItemsAnswer => ({
  items: [],
  total_hint: 0,
  complete: true,
  ...over,
});

const projects = (keys: string[]): WorkProjectsAnswer => ({
  projects: keys.map((key) => ({
    key,
    name: key,
    unit: { resolved: true },
    lead: {},
    task_counts: { open: 0, done: 0, closed: 0 },
    version: 1,
  })),
  total: keys.length,
  complete: true,
});

/**
 * The board, with only the answers a case cares about.
 *
 * Every other query resolves to null, which is exactly what a node that does
 * not serve one answers, so a case says what it is about and nothing else.
 */
function mount(answers: Record<string, unknown>, hash = "#/work") {
  location.hash = hash;
  const store = new Store();
  const socket = new LiveSocket(store);
  (socket as unknown as { query: (what: string) => Promise<unknown> }).query = (what) =>
    Promise.resolve(answers[what] ?? null);
  render(
    <ClientContext.Provider value={{ store, socket }}>
      <Router>
        <Work />
      </Router>
    </ClientContext.Provider>,
  );
}

/** One stat tile, by the label above its number. */
function stat(label: string): HTMLElement {
  const tile = screen.getByText(label).parentElement;
  if (!tile) throw new Error(`no stat tile labelled ${label}`);
  return tile;
}

afterEach(() => {
  cleanup();
  localStorage.clear();
  location.hash = "#/";
});

// THE BOARD'S OWN COUNTS COME FROM THE COLUMNS when there are columns. This is
// the derivation above, asserted where a reader meets it: the header read zero
// on a board that was drawing three tasks.
test("a grouped board counts the rows its columns carry, not the empty items array", async () => {
  mount({
    work_items: items({
      groups: [
        {
          key: "ada",
          label: "ada",
          count: 40,
          rows: [row("a", { status: "in_progress" }), row("b", { status: "todo", blocked: true })],
        },
        { key: "grace", label: "grace", count: 7, rows: [row("c", { status: "in_review" })] },
      ],
    }),
  });

  await screen.findByText("ada");
  expect(within(stat("Shown")).getByText("3")).toBeTruthy();
  expect(within(stat("In progress")).getByText("1")).toBeTruthy();
  expect(within(stat("In review")).getByText("1")).toBeTruthy();
  // BLOCKED IS COUNTED FROM THE FLAG. The blocked row's own status is `todo`,
  // so a screen counting a status would report none.
  expect(within(stat("Blocked")).getByText("1")).toBeTruthy();
  // AND A COLUMN'S COUNT IS OVER THE WHOLE SET, never over the rows it carries:
  // a column of forty says forty and hands back two.
  expect(screen.getByText("40")).toBeTruthy();
  expect(screen.getByText("38 more in this column")).toBeTruthy();
});

// A LANE THE ANSWER COULD NOT FIT IS SAID, rather than silently cut: a board
// drawing sixty-four of two hundred columns and saying nothing looks like a
// company with sixty-four assignees.
test("columns that did not fit, and an axis that double-counts, are both said", async () => {
  mount({
    work_items: items({
      groups: [{ key: "api", label: "api", count: 1, rows: [row("a")] }],
      groups_dropped: 3,
      groups_overlap: true,
    }),
  });

  await screen.findByText(/3 more columns did not fit/);
  expect(screen.getByText(/counts add up to more than the total/)).toBeTruthy();
});

// A BLOCKER SITS BESIDE A STATUS, never instead of it: a task in progress with
// an open blocker is both, and a column showing one would hide the other.
test("a blocked task keeps its own status beside the blocker", async () => {
  mount({
    work_items: items({
      items: [row("a", { title: "Fix the ingest lag", status: "in_progress", blocked: true })],
      total_hint: 1,
    }),
    work_projects: projects(["ENG"]),
  });

  const cell = (await screen.findByText("Fix the ingest lag")).closest("tr")!;
  expect(within(cell).getByText("In progress")).toBeTruthy();
  expect(within(cell).getByText("Blocked")).toBeTruthy();
  // AND THE KEY IS STILL THE ROW'S OWN ADDRESS, which is what somebody quotes
  // into a tool call.
  expect(within(cell).getByText("ENG-a")).toBeTruthy();
});

// THREE QUESTIONS, NOT TWO. "Open", "closed" and "everything" are three real
// questions and a two-state control makes the third unreachable, which is how
// a board that can never show a closed item ships.
test("the open and closed control asks all three questions, and All is reachable", async () => {
  mount({
    work_items: items({ items: [row("a")], total_hint: 1 }),
    work_projects: projects(["ENG"]),
  });

  const group = await screen.findByRole("radiogroup", { name: "Open or closed" });
  expect(
    within(group)
      .getAllByRole("radio")
      .map((el) => el.textContent),
  ).toEqual(["Open", "Closed", "All"]);
  // THE THIRD SEGMENT HAS TO SURVIVE THE URL. Written as the empty string it
  // was deleted from the query on the way out and read back as the default on
  // the way in, so pressing All left the control on Open.
  fireEvent.click(within(group).getByRole("radio", { name: "All" }));
  expect(location.hash).toContain("scope=all");
  expect(
    within(screen.getByRole("radiogroup", { name: "Open or closed" }))
      .getByRole("radio", {
        name: "All",
      })
      .getAttribute("aria-checked"),
  ).toBe("true");
});

// A SAVED VIEW IS A SET OF DEFAULTS, never a lock: picking one puts its
// parameters on the URL, where every explicit control still overrides them.
test("a saved view puts its own parameters on the URL and is marked as followed", async () => {
  const views: WorkViewsAnswer = {
    views: [
      {
        key: "review",
        name: "Waiting on review",
        type: "list",
        container: { kind: "workspace", id: "w" },
        builtin: false,
        params: { status: "in_review", status_group: "done,closed" },
      },
    ],
    complete: true,
  };
  mount({ work_items: items({ items: [row("a")], total_hint: 1 }), work_views: views });

  const chip = await screen.findByRole("radio", { name: "Waiting on review" });
  fireEvent.click(chip);
  expect(location.hash).toContain("view=review");
  expect(location.hash).toContain("status=in_review");
  // THE SEGMENTS ARE A SHORTHAND for the two groups each names, so a view
  // carrying exactly those lands on the segment rather than on All with an
  // invisible filter.
  expect(location.hash).toContain("scope=closed");
  expect(
    screen.getByRole("radio", { name: "Waiting on review" }).getAttribute("aria-checked"),
  ).toBe("true");
});

// THE TWO EMPTY ANSWERS ARE DIFFERENT FACTS with different remedies, and the
// screen this replaces drew BOTH of them, stacked, on a company that had never
// filed anything.
test("a company that has filed nothing says so, and says what files the first item", async () => {
  mount({ work_items: items() });

  await screen.findByText("No work has been filed yet");
  expect(screen.getByText(/create_work_item/)).toBeTruthy();
  expect(screen.queryByText("Nothing matches")).toBeNull();
});

test("a board with work behind it but nothing on screen blames the filters", async () => {
  mount({ work_items: items(), work_projects: projects(["ENG", "OPS"]) });

  await screen.findByText("Nothing matches");
  expect(screen.queryByText("No work has been filed yet")).toBeNull();
});

// STALENESS AND COVERAGE ARE DIFFERENT FACTS, and a screen that renders the
// freshness badge and swallows the coverage flag is worse than a stale tile: a
// person reads "a moment ago" and concludes the board is right.
test("an answer that could not account for everything says so, and names the remedy", async () => {
  mount({
    work_items: items({
      items: [row("a")],
      total_hint: 1,
      complete: false,
      read_level: "stale",
      incomplete: {
        records: 3,
        from: { stream: "s", generation: 1, seq: 42 },
        scope: ["project:ENG"],
        version: 7,
      },
    }),
    work_projects: projects(["ENG"]),
  });

  await screen.findByText(/3 record\(s\) this build cannot read/);
  expect(screen.getByText(/Affected: project:ENG/)).toBeTruthy();
  // THE REMEDY IS A BUILD THAT CAN READ THE RECORDS, not a refresh, and a
  // banner that does not say so sends somebody to press reload for an hour.
  expect(screen.getByText(/not a refresh/)).toBeTruthy();
  // And the freshness the engine actually served, beside it rather than
  // instead of it.
  expect(screen.getByText("stale")).toBeTruthy();
});

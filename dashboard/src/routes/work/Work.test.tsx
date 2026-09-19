/**
 * What a tracker row and a board card CLAIM, and the wrong form of each.
 *
 * Every case here is a rendering that, drawn wrong, says something true about
 * a different task: a card with no blocked mark is a card somebody picks up, a
 * calendar chip on the wrong side of midnight is a deadline nobody set, and a
 * column footer that counts rows rather than the whole set tells a lead their
 * backlog is fifty items long.
 *
 * The components take an `href` and an `onOpen` rather than reaching for a
 * router, so every one of them renders here with no routing context — which is
 * also why the board can be drawn inside a peek panel and a sprint report.
 */

import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, expect, test, vi } from "vitest";
import { EMPTY_VALUE } from "@crewlethq/ui";
import {
  ActivityFeed,
  Board,
  CalendarView,
  ProjectHead,
  Work,
  WorkspaceHead,
  patchedHref,
} from "./Work.tsx";
import { BoardCard, WorkRow } from "~/components/work.tsx";
import { Router } from "~/app/router.tsx";
import { useClient, useConnection, useOrg } from "~/lib/store-hooks.ts";
import { calendarWeeks, dayKey, dayLabel, filterPatchForGroup } from "~/lib/work.ts";
import type {
  QueryName,
  WorkGroup,
  WorkProjectDetail,
  WorkProjectRow,
  WorkSummary,
} from "~/protocol/index.ts";

vi.mock("~/lib/store-hooks.ts", async () => {
  const actual =
    await vi.importActual<typeof import("~/lib/store-hooks.ts")>("~/lib/store-hooks.ts");
  return { ...actual, useClient: vi.fn(), useConnection: vi.fn(), useOrg: vi.fn() };
});

afterEach(() => {
  cleanup();
  vi.restoreAllMocks();
  location.hash = "#/";
});

const row = (id: string, over: Partial<WorkSummary> = {}): WorkSummary => ({
  id,
  key: "ENG-" + id,
  project: "ENG",
  title: "a task called " + id,
  type: "task",
  status: "todo",
  updated: "2031-04-16T00:00:00Z",
  version: 1,
  ...over,
});

// ONE CLOCK for every case here, so a date's rendering is a function of the
// row alone — a suite that read the real clock would change its own expected
// text on New Year's Day.
const NOW = Date.parse("2031-04-16T00:00:00Z");

const card = (over: Partial<WorkSummary> = {}) =>
  render(<BoardCard row={row("1", over)} href="#/work/ENG-1" now={NOW} />);

// ---------------------------------------------------------------------------
// The card
// ---------------------------------------------------------------------------

// A BLOCKED CARD SAYS SO IN WORDS AND IN SHAPE. The badge is the word; the
// rail is what makes a column of forty scannable for the three that cannot
// move. A card missing either is a card somebody picks up.
test("a blocked card carries both the badge and the rail", () => {
  const { container } = card({ blocked: true });
  expect(screen.getByText("Blocked")).toBeTruthy();
  expect(container.querySelector(".work-card")?.getAttribute("data-blocked")).toBe("true");
});

test("an unblocked card carries neither", () => {
  const { container } = card();
  expect(screen.queryByText("Blocked")).toBeNull();
  expect(container.querySelector(".work-card")?.getAttribute("data-blocked")).toBeNull();
});

// THE ROW'S OWN OVERDUE FLAG, never a comparison of our own: the server
// derives it against the company's day start, and a browser re-deriving it
// from its own midnight is how one screen shows a task as overdue and another
// does not.
test("an overdue due date is marked and an on-time one is not", () => {
  const { container } = card({ due: "2031-04-01T00:00:00Z", overdue: true });
  expect(container.querySelector(".work-due.overdue")).toBeTruthy();
  cleanup();
  const plain = render(
    <BoardCard row={row("2", { due: "2031-09-01T00:00:00Z" })} href="#/work/ENG-2" now={NOW} />,
  );
  expect(plain.container.querySelector(".work-due")).toBeTruthy();
  expect(plain.container.querySelector(".work-due.overdue")).toBeNull();
});

// NOBODY IS A STATE, and the one worth seeing: an unassigned item routes to
// the project's lead, and a project with no lead routes to nobody at all.
test("an unassigned card draws the empty seat rather than a blank", () => {
  const { container } = card();
  expect(container.querySelector(".work-nobody")).toBeTruthy();
  expect(screen.getByText("Unassigned")).toBeTruthy();
  cleanup();
  const held = render(
    <BoardCard
      row={row("3", { assignee: "ada" })}
      href="#/work/ENG-3"
      now={NOW}
      chrome={{ seatName: () => "Ada Lovelace" }}
    />,
  );
  expect(held.container.querySelector(".work-nobody")).toBeNull();
  expect(held.container.querySelector(".crewlet-avatar")?.textContent).toBe("AL");
});

// A CARD IS A REAL ANCHOR, so middle-click and ⌘-click open the item's own
// page. A plain click opens the peek instead, because the board is the place
// the reader is — and a modified click must fall through to the browser or
// the affordance is a lie.
test("a plain click peeks and a modified click follows the link", () => {
  const onOpen = vi.fn();
  const { container } = render(
    <BoardCard row={row("4")} href="#/work/ENG-4" onOpen={onOpen} now={NOW} />,
  );
  const anchor = container.querySelector("a.work-card");
  expect(anchor?.getAttribute("href")).toBe("#/work/ENG-4");

  const plain = new MouseEvent("click", { bubbles: true, cancelable: true });
  anchor?.dispatchEvent(plain);
  expect(onOpen).toHaveBeenCalledTimes(1);
  expect(plain.defaultPrevented).toBe(true);

  const meta = new MouseEvent("click", { bubbles: true, cancelable: true, metaKey: true });
  anchor?.dispatchEvent(meta);
  expect(onOpen).toHaveBeenCalledTimes(1);
  expect(meta.defaultPrevented).toBe(false);
});

// THE TYPE IS ON THE ROW, so a card can draw its own mark. Before it was, a
// board could group BY a value its cards could not SHOW.
test("a card draws its type's own mark and names it for a screen reader", () => {
  card({ type: "bug" });
  expect(screen.getByText("Bug")).toBeTruthy();
});

// ---------------------------------------------------------------------------
// The board
// ---------------------------------------------------------------------------

const group = (key: string, over: Partial<WorkGroup> = {}): WorkGroup => ({
  key,
  count: 1,
  rows: [],
  ...over,
});

// THE SCREEN OWNS THE OVERFLOW ADDRESS, so the fixture supplies one that
// names a project — which is what lets the case below assert that the column
// footer renders what it was handed rather than a `#/work` of its own.
const overflowHref = (axis: string, key: string) =>
  `#/work/ENG?view=list&group_by=${axis}&group=${key}`;

const board = (groups: WorkGroup[], axis = "status") =>
  render(
    <Board
      now={NOW}
      groups={groups}
      axis={axis}
      chrome={{}}
      hrefOf={(r) => `#/work/${r.key}`}
      onOpen={() => {}}
      onOverflow={() => {}}
      overflowHref={overflowHref}
    />,
  );

// A COLUMN'S COUNT IS OVER THE WHOLE SET and its rows are a slice, so a column
// of four hundred says four hundred and hands back fifty. The overflow has to
// be REACHABLE rather than merely counted: a board that said "350 more" with
// no way to see them is a board that hides a backlog.
test("a capped column offers its overflow and an uncapped one does not", () => {
  board([group("todo", { count: 53, rows: [row("a")] })]);
  const more = screen.getByText("52 more →");
  // THE SCREEN'S OWN ADDRESS, verbatim. The footer used to build
  // `href(["work"], …)` itself, which dropped the project segment and every
  // live filter — and a middle click follows the href rather than the
  // `onClick`, so the column of 53 opened as every project's todo column.
  expect(more.getAttribute("href")).toBe(overflowHref("status", "todo"));
  cleanup();
  board([group("todo", { count: 1, rows: [row("a")] })]);
  expect(screen.queryByText(/more →/)).toBeNull();
});

// THE LINK AND THE CLICK NAME THE SAME PLACE. Only the screen knows both
// halves of that place — the project is a path segment and the filters are
// keys beside it — so the address is a patch of where the reader already is,
// never a URL rebuilt from the three keys the control moves.
test("a patched address keeps the path it is on and the filters it is under", () => {
  expect(
    patchedHref(
      ["work", "ENG"],
      new URLSearchParams("assignee=ada&scope=open"),
      filterPatchForGroup("status", "todo"),
    ),
  ).toBe("#/work/ENG?assignee=ada&scope=open&view=list&group_by=status&group=todo");
  // AN EMPTY VALUE IS THE KEY'S ABSENCE, matching `useParam`: a control that
  // clears the grouping must drop the key rather than write `group_by=`.
  expect(patchedHref(["work"], new URLSearchParams("group_by=status"), { group_by: "" })).toBe(
    "#/work",
  );
});

test("an overflow link hands the axis and the column back to the screen", () => {
  const onOverflow = vi.fn();
  render(
    <Board
      now={NOW}
      groups={[group("ada", { count: 9, rows: [row("a")] })]}
      axis="assignee"
      chrome={{}}
      hrefOf={(r) => `#/work/${r.key}`}
      onOpen={() => {}}
      onOverflow={onOverflow}
      overflowHref={overflowHref}
    />,
  );
  fireEvent.click(screen.getByText("8 more →"));
  expect(onOverflow).toHaveBeenCalledWith("assignee", "ada");
});

// AN EMPTY COLUMN IS STILL A COLUMN, and it says it is empty: a heading over
// nothing at all reads as a column whose rows failed to arrive.
test("a column with no rows says so rather than rendering nothing", () => {
  board([group("in_review", { count: 0, rows: [] })]);
  expect(screen.getByText("Nothing here")).toBeTruthy();
});

test("a column is headed by what its own axis means", () => {
  board([group("", { count: 3, rows: [] })], "assignee");
  expect(screen.getByText("Unassigned")).toBeTruthy();
});

// ---------------------------------------------------------------------------
// The calendar
// ---------------------------------------------------------------------------

const calendar = (rows: WorkSummary[], month = "2031-04") =>
  render(
    <CalendarView
      weeks={calendarWeeks(month, "")}
      rows={rows}
      month={month}
      chrome={{}}
      hrefOf={(r) => `#/work/${r.key}`}
      onOpen={() => {}}
      onMonth={() => {}}
      onToday={() => {}}
    />,
  );

// A TASK LANDS ON THE DAY IT IS DUE, in the reader's own zone, and on no other
// day: a chip drawn on two cells is a deadline the reader cannot trust, and
// one drawn on the wrong cell is a deadline nobody set.
test("a due task appears once, on its own day", () => {
  const due = new Date(2031, 3, 16, 14, 0, 0).toISOString();
  const { container } = calendar([row("a", { due })]);
  const chips = container.querySelectorAll(".work-cal-chip");
  expect(chips).toHaveLength(1);
  const cell = chips[0]?.closest(".work-cal-cell");
  expect(cell?.querySelector(".work-cal-day")?.textContent).toBe("16");
  expect(dayKey(due)).toBe("2031-04-16");
});

// AN OVERDUE CHIP IS TINTED AND A FINISHED ONE IS QUIETER — and neither is
// hidden: a calendar that dropped delivered work would answer "what is due"
// with a month that forgets what was done.
test("overdue and finished chips are marked differently and both are drawn", () => {
  const due = new Date(2031, 3, 16, 9, 0, 0).toISOString();
  const { container } = calendar([
    row("late", { due, overdue: true }),
    row("shipped", { due, status: "done", status_group: "done" }),
  ]);
  expect(container.querySelectorAll(".work-cal-chip")).toHaveLength(2);
  expect(container.querySelector('.work-cal-chip[data-overdue="true"]')).toBeTruthy();
  expect(container.querySelector('.work-cal-chip[data-done="true"]')).toBeTruthy();
});

// THE CALENDAR NAMES BOTH OF THE SETS ITS WINDOW LEAVES OUT.
//
// This asserted a sentence the product cannot reach: `buildItemsParams` bounds
// the fetch to the grid's days and the engine compiles that to `due_at IS NOT
// NULL AND due_at >= ? AND due_at < ?`, so an undated row can never be in
// `rows` — the count was always zero and the clause it gated never rendered.
// Both omissions are stated unconditionally instead, because neither count is
// ours to give: neither set is in this answer and the grammar has no `due=null`
// to ask for one. The rows here all carry due dates, which is the only kind the
// query can return.
test("the calendar names both of the sets its window leaves out", () => {
  const { container } = calendar([row("a", { due: new Date(2031, 3, 16, 9, 0, 0).toISOString() })]);
  const note = container.querySelector(".work-cal-note")!;
  expect(note.textContent).toContain("no due date");
  expect(note.textContent).toContain("outside");
});

// TWO CELLS, ONE NUMERAL. April 2031 opens on 31 March and closes on 4 May, so
// `1` is drawn twice — once for April and once for May. The tint that separates
// them is a colour; this is the half that is not.
test("every cell says which date it is, so a repeated numeral is not ambiguous", () => {
  const { container } = calendar([]);
  const cells = [...container.querySelectorAll(".work-cal-cell")];
  const ones = cells.filter((c) => c.querySelector(".work-cal-day")?.textContent === "1");
  expect(ones).toHaveLength(2);
  const spoken = ones.map((c) => c.querySelector(".sr-only")?.textContent);
  expect(spoken[0]).toBe(dayLabel("2031-04-01"));
  expect(spoken[1]).toBe(dayLabel("2031-05-01"));
  expect(spoken[0]).not.toBe(spoken[1]);
  // And the numeral is not read twice over the date beside it.
  expect(ones[0]?.querySelector(".work-cal-day")?.getAttribute("aria-hidden")).toBe("true");
});

// A DAY WITH MORE THAN THE CELL HOLDS SAYS HOW MANY, rather than truncating
// to whatever fits: a cell showing three of eleven is a day that looks quiet.
test("a crowded day folds the rest into a count", () => {
  const due = new Date(2031, 3, 16, 9, 0, 0).toISOString();
  const { container } = calendar([
    row("a", { due }),
    row("b", { due }),
    row("c", { due }),
    row("d", { due }),
    row("e", { due }),
  ]);
  expect(container.querySelectorAll(".work-cal-chip")).toHaveLength(3);
  expect(screen.getByText("+2 more")).toBeTruthy();
});

// ---------------------------------------------------------------------------
// The heads
// ---------------------------------------------------------------------------

const project = (over: Partial<WorkProjectDetail> = {}): WorkProjectDetail => ({
  key: "ENG",
  name: "Engineering",
  unit: { key: "platform", name: "Platform", resolved: true },
  lead: { handle: "ada", kind: "agent" },
  task_counts: { open: 12, done: 40, closed: 3 },
  version: 1,
  statuses: [],
  types: [],
  fields: [],
  policy_stamp: 1,
  complete: true,
  ...over,
});

// A PROJECT'S CENSUS IS A SHAPE AS WELL AS THREE NUMBERS, and the bar carries
// its own legend: an unlabelled stack of three colours is three colours.
test("the census bar is drawn with its legend, and only where there is work", () => {
  // THE CENSUS IS DRAWN OUT OF THE DESIGN SYSTEM'S OWN PARTS NOW, so the two
  // selectors are theirs: `.stackbar`/`.legend` were our recipes' classes and
  // no longer exist. What is asserted is unchanged — a bar, and a key beside
  // it — and the legend is a real `role="list"` over there, which is what the
  // second selector could have been written against instead.
  const { container } = render(<ProjectHead detail={project()} />);
  expect(container.querySelector(".crewlet-stacked-bar")).toBeTruthy();
  expect(container.querySelector(".crewlet-legend")).toBeTruthy();
  cleanup();
  const fresh = render(
    <ProjectHead detail={project({ task_counts: { open: 0, done: 0, closed: 0 } })} />,
  );
  expect(fresh.container.querySelector(".crewlet-stacked-bar")).toBeNull();
});

// THE WORKSPACE RANKS ITS PROJECTS BY OPEN WORK, in ONE hue: a hue per project
// would be identity colouring by row — rename the project and its colour
// changes — and the label already says which project it is.
test("the workspace chart ranks projects by open work in a single hue", () => {
  const projects: WorkProjectRow[] = [
    {
      key: "OPS",
      name: "Operations",
      unit: { resolved: true },
      lead: {},
      task_counts: { open: 3, done: 1, closed: 0 },
      version: 1,
    },
    {
      key: "ENG",
      name: "Engineering",
      unit: { resolved: true },
      lead: {},
      task_counts: { open: 11, done: 4, closed: 2 },
      version: 1,
    },
  ];
  const { container } = render(<WorkspaceHead projects={projects} />);
  const labels = [...container.querySelectorAll(".key-mark")].map((el) => el.textContent);
  expect(labels).toEqual(["ENG", "OPS"]);
  // ONE HUE STILL, read where their `BarList` puts it: the bar's colour is a
  // custom property on the fill rather than a `background` on a child div, so
  // the selector and the read both move to theirs. `.meter-track > div` was
  // our own chart's markup and is gone with it.
  const fills = [...container.querySelectorAll(".crewlet-bar-list__bar")].map((el) =>
    (el as HTMLElement).style.getPropertyValue("--crewlet-bar-list-bar-color"),
  );
  expect(fills.length).toBe(2);
  expect(new Set(fills).size).toBe(1);
});

// A ONE-BAR CHART IS A NUMBER, so a company with one project gets the facts
// and no chart at all.
test("a single project draws its facts and no comparison", () => {
  const { container } = render(
    <WorkspaceHead
      projects={[
        {
          key: "ENG",
          name: "Engineering",
          unit: { resolved: true },
          lead: {},
          task_counts: { open: 2, done: 0, closed: 0 },
          version: 1,
        },
      ]}
    />,
  );
  // NO CHART AT ALL, asserted against their bar list rather than against our
  // old track class — a company with one project gets the facts and nothing
  // to compare them with.
  expect(container.querySelector(".crewlet-bar-list")).toBeNull();
  expect(screen.getByText("Projects")).toBeTruthy();
});

// ---------------------------------------------------------------------------
// The row
// ---------------------------------------------------------------------------

// ONE ROW SHAPE EVERYWHERE. My work rendered four of a row's facts and the
// board six, and only one of the two knew a task could be blocked.
test("a list row carries the same facts a card does", () => {
  const { container } = render(
    <WorkRow
      row={row("1", {
        blocked: true,
        priority: "urgent",
        assignee: "ada",
        due: "2031-04-01T00:00:00Z",
        overdue: true,
      })}
      href="#/work/ENG-1"
      now={Date.parse("2031-04-16T00:00:00Z")}
      chrome={{ seatName: () => "Ada Lovelace" }}
    />,
  );
  expect(screen.getByText("Blocked")).toBeTruthy();
  expect(container.querySelector('.work-prio[data-priority="urgent"]')).toBeTruthy();
  expect(container.querySelector(".work-due.overdue")).toBeTruthy();
  expect(container.querySelector(".crewlet-avatar")?.textContent).toBe("AL");
});

// A ROW IS A TABLE, so a row missing a value keeps the COLUMN. Every cell
// here is a grid track, and the marks inside them render nothing for an
// absent value — so a row that dropped the cell too would pull every column
// after it one place left, and a list of forty would have its status badges
// at forty different places. This is the assertion that the cells are the
// row's and not the marks'.
test("a row with nothing to say in a column still keeps the column", () => {
  const bare = render(
    <WorkRow row={row("1")} href="#/work/ENG-1" now={NOW} chrome={{}} />,
  ).container;
  const full = render(
    <WorkRow
      row={row("2", { priority: "urgent", due: "2031-04-01T00:00:00Z", assignee: "ada" })}
      href="#/work/ENG-2"
      now={NOW}
      chrome={{ seatName: () => "Ada Lovelace" }}
    />,
  ).container;
  const cells = (el: Element) =>
    [...el.querySelectorAll(".work-row > .work-cell")].map((c) => c.className);
  expect(cells(bare)).toEqual(cells(full));
  expect(cells(bare)).toHaveLength(4);
});

// A DATE COLUMN DROPS THE YEAR IT SHARES WITH THE READER. Repeating "2031"
// down forty rows is how the one row due in 2032 goes unnoticed — and the
// full instant is still on the title, so nothing is lost.
test("a due date in this year is drawn without it, and another year keeps it", () => {
  const drawn = (due: string) =>
    render(
      <WorkRow row={row("1", { due })} href="#/work/ENG-1" now={NOW} chrome={{}} />,
    ).container.querySelector(".work-due")?.textContent ?? "";
  expect(drawn("2031-09-01T00:00:00Z")).not.toContain("2031");
  expect(drawn("2032-09-01T00:00:00Z")).toContain("2032");
});

// THE DEFAULT IS DRAWN AS NOTHING. `normal` is what a task gets when nobody
// said, so it is most of a board — a mark on every card is a mark that says
// nothing, and it buries the four that ARE urgent under forty that are not.
// `none` and `normal` differ in the data and agree on screen, which is the
// one place they should.
test("an ordinary priority draws no mark at all", () => {
  const marks = (priority?: string) =>
    render(
      <WorkRow
        row={row("1", { priority })}
        href="#/work/ENG-1"
        now={Date.parse("2031-04-16T00:00:00Z")}
        chrome={{}}
      />,
    ).container.querySelectorAll(".work-prio").length;
  expect(marks("normal")).toBe(0);
  expect(marks(undefined)).toBe(0);
  expect(marks("low")).toBe(1);
  expect(marks("high")).toBe(1);
});

// ---------------------------------------------------------------------------
// The screen
// ---------------------------------------------------------------------------

/** One socket answering each question with a fixture, and refusing the named
 *  ones — because a per-query refusal is exactly the state these cases are
 *  about: `socket.ts` arms a timer per query id, so one question can fail
 *  while every other on the screen answers. */
function serving(
  answers: Partial<Record<QueryName, unknown>>,
  refusing: Partial<Record<QueryName, string>> = {},
) {
  const query = vi.fn(async (what: string) => {
    const refusal = refusing[what as QueryName];
    if (refusal) throw new Error(refusal);
    return answers[what as QueryName] ?? {};
  });
  vi.mocked(useClient).mockReturnValue({ socket: { query } } as never);
  vi.mocked(useConnection).mockReturnValue({ connected: true } as never);
  vi.mocked(useOrg).mockReturnValue({
    name: "Acme",
    roles: [{ name: "Ada Okonkwo", handle: "ada", kind: "agent" }],
  } as never);
  return query;
}

const mountWork = () =>
  render(
    <Router>
      <Work />
    </Router>,
  );

// A FAILED PROJECT READ IS NOT A COMPANY THAT HAS FILED NOTHING. The empty
// state was gated on the ITEMS query — a different question, which answers
// fine while `work_projects` is refused, times out, or has simply not landed
// — so the screen stated as a fact that the company had no work, over a read
// that never happened. A reader acts on that by filing the duplicate.
test("a refused project list is said to be a refusal, not an empty company", async () => {
  serving(
    { work_items: { items: [], groups: [], complete: true } },
    { work_projects: "unavailable" },
  );
  mountWork();
  await waitFor(() => expect(screen.getByText(/This is not an empty company/)).toBeTruthy());
  expect(screen.queryByText("No work has been filed yet")).toBeNull();
});

// AND A READ THAT ANSWERED IS ALLOWED TO CONCLUDE IT.
test("a project list that answered with nothing says the company has filed nothing", async () => {
  serving({
    work_items: { items: [], groups: [], complete: true },
    work_projects: { projects: [], total: 0, complete: true },
  });
  mountWork();
  await waitFor(() => expect(screen.getByText("No work has been filed yet")).toBeTruthy());
});

// THE LIST IS HEADED BY THE AXIS THE QUERY WAS SENT ON. A saved view may
// carry `group_by` with nothing in the URL — `buildItemsParams` resolves the
// axis as the URL's OR the view's — so a list handed the URL key alone had no
// axis to label with and fell through to the raw group key: columns headed
// `ada` where the board one line above headed them "Ada Okonkwo".
test("a saved view's grouping heads the list's columns by name", async () => {
  serving({
    work_views: {
      views: [
        {
          id: "v-1",
          key: "by-owner",
          name: "By owner",
          type: "list",
          container: { kind: "workspace", id: "" },
          builtin: false,
          default: true,
          params: { group_by: "assignee" },
        },
      ],
      complete: true,
    },
    work_items: {
      items: [],
      groups: [{ key: "ada", count: 3, rows: [] }],
      complete: true,
    },
    work_projects: {
      projects: [
        {
          key: "ENG",
          name: "Engineering",
          unit: { resolved: true },
          lead: {},
          task_counts: { open: 3, done: 0, closed: 0 },
          version: 1,
        },
      ],
      total: 1,
      complete: true,
    },
  });
  const { container } = mountWork();
  await waitFor(() => expect(container.querySelector(".work-group-head")).toBeTruthy());
  expect(container.querySelector(".work-group-head")?.textContent).toContain("Ada Okonkwo");
  expect(container.querySelector(".work-group-head")?.textContent).not.toContain("ada");
});

// ---------------------------------------------------------------------------
// The table, and the trash
// ---------------------------------------------------------------------------

/** The builtin strip as the engine mints it, with one tab chosen as default. */
function strip(defaultKey: string) {
  const rows = [
    { key: "list", name: "List", type: "list", params: {} },
    { key: "table", name: "Table", type: "table", params: {} },
    {
      key: "trash",
      name: "Trash",
      type: "table",
      params: { removed: "true", show_closed: "true" },
    },
  ];
  return {
    views: rows.map((v) => ({
      ...v,
      container: { kind: "workspace", id: "" },
      builtin: true,
      default: v.key === defaultKey,
    })),
    complete: true,
  };
}

/** What `work_items` was actually asked, which is the claim most of these make. */
function asked(query: ReturnType<typeof serving>): Record<string, unknown> {
  // THE CALL'S SECOND ARGUMENT, which the stub's own signature does not
  // declare — `serving` takes the question alone, because every other case
  // here asks only which questions were put. The parameters are the whole
  // claim of the cases below, so they are read off the recorded call.
  const calls = query.mock.calls as unknown as [string, Record<string, unknown>?][];
  return calls.findLast(([what]) => what === "work_items")?.[1] ?? {};
}

// THE TRASH ASKS FOR REMOVED WORK WHATEVER STATE IT WAS IN.
//
// This is the end of the round trip `seededScope` sits in the middle of. The
// tab's own parameters say `removed=true, show_closed=true`; the scope segment
// is seeded from those parameters and then WRITES ITS GROUP BACK over them.
// Seeded `open` — which is what a view naming no `status_group` used to get —
// it wrote `status_group=not_started,active`, so the one tab whose whole job is
// "what did my assistant delete" answered with the removed tasks that were
// still open and hid every removal of anything already done. Silently: the
// rows it showed were real.
test("the trash asks for removed work without re-narrowing it to open", async () => {
  const query = serving({
    work_views: strip("trash"),
    work_items: { items: [], groups: [], complete: true },
    work_activity: { records: [], complete: true },
  });
  mountWork();
  await waitFor(() => expect(asked(query).removed).toBe("true"));
  expect(asked(query).show_closed).toBe("true");
  expect(asked(query).status_group).toBeUndefined();
});

// AND THE ORDINARY TABLE IS AN ORDINARY BOARD QUESTION. Same tab strip, same
// screen, and none of the trash's parameters — so a reader on Table is not
// quietly looking at deleted work.
test("the table tab asks for live work, the way the list does", async () => {
  const query = serving({
    work_views: strip("table"),
    work_items: { items: [], groups: [], complete: true },
    work_activity: { records: [], complete: true },
  });
  mountWork();
  await waitFor(() => expect(query.mock.calls.some(([what]) => what === "work_items")).toBe(true));
  expect(asked(query).removed).toBeUndefined();
  expect(asked(query).status_group).toBe("not_started,active");
});

// WHO REMOVED IT COMES FROM THE HISTORY, because the row does not carry it.
//
// A list row is deliberately not a document read per card, so it has no
// tombstone on it: the removal is a fact about the COMMIT and lives in
// `work_activity`. A trash that drew the rows alone could say what is in the
// bin and not one thing about how it got there — which is the entire question
// somebody opens it with.
test("a removed row names who removed it, from the feed rather than the row", async () => {
  serving({
    work_views: strip("trash"),
    work_items: {
      items: [row("t-1", { key: "ENG-9", title: "the wrong subtree" })],
      groups: [],
      complete: true,
    },
    work_activity: {
      records: [
        {
          id: "h-1",
          log_seq: 9,
          log_stream: "CREWLET_WORK_LOG",
          log_generation: 1,
          at: "2031-04-15T00:00:00Z",
          effective_at: "2031-04-15T00:00:00Z",
          kind: "removed",
          actor: "ada",
          actor_kind: "seat",
          subject_kind: "task",
          subject_id: "t-1",
          subject_key: "ENG-9",
          notified: false,
        },
      ],
      complete: true,
    },
  });
  mountWork();
  // WAITED FOR ON THE NAME, not on the row. The rows and the feed are two
  // queries and the name comes from the SECOND — so waiting for the row and
  // then asserting the name synchronously passes only while the feed happens
  // to resolve in the same tick, which is what made this case flake.
  await waitFor(() => expect(screen.getAllByText("Ada Okonkwo").length).toBeGreaterThan(0));
  expect(screen.getByText("the wrong subtree")).toBeTruthy();
  // And the way back is on the row: this dashboard writes nothing, so what it
  // offers is the call an assistant would make.
  expect(screen.getByText("Restore")).toBeTruthy();
});

// A ROW THE FEED'S PAGE DOES NOT REACH SAYS SO.
//
// The rows and the history page independently, so a task removed long enough
// ago has no entry on the loaded page. "Removed by nobody" is not a fact this
// product can state, and a blank cell in a date column reads as "just now".
test("a removal older than the loaded history draws a dash, not a blank", async () => {
  const { container } = (() => {
    serving({
      work_views: strip("trash"),
      work_items: {
        items: [row("t-2", { key: "ENG-10", title: "removed long ago" })],
        groups: [],
        complete: true,
      },
      work_activity: { records: [], complete: true },
    });
    return mountWork();
  })();
  await waitFor(() => expect(screen.getByText("removed long ago")).toBeTruthy());
  const dashed = [...container.querySelectorAll(".crewlet-empty-value")].filter((el) =>
    (el.textContent ?? "").includes("older than the loaded history"),
  );
  expect(dashed.length).toBeGreaterThan(0);
});

// A PURGE HAS NO ROW AT ALL, AND IS STILL ON THE SCREEN.
//
// A removal hides a task and a restore brings it back at any age. A purge
// destroys the rows, so there is nothing for the grid to list and the history
// entry is the only evidence the work ever existed. Drawn nowhere, an emptied
// trash is indistinguishable from a company that has removed nothing.
test("a purged task appears in its own band, marked irreversible", async () => {
  serving({
    work_views: strip("trash"),
    work_items: { items: [], groups: [], complete: true },
    work_activity: {
      records: [
        {
          id: "h-2",
          log_seq: 11,
          log_stream: "CREWLET_WORK_LOG",
          log_generation: 1,
          at: "2031-04-14T00:00:00Z",
          effective_at: "2031-04-14T00:00:00Z",
          kind: "purged",
          actor: "U0FOUNDER",
          actor_kind: "operator",
          subject_kind: "task",
          subject_id: "t-3",
          subject_key: "ENG-11",
          excerpt: "duplicate of **ENG-4**",
          notified: false,
        },
      ],
      complete: true,
    },
  });
  mountWork();
  await waitFor(() => expect(screen.getByText("Purged")).toBeTruthy());
  // WITHIN THE BAND. The same record also reaches the screen's ordinary
  // activity feed — one is a page of the whole log and the other is the three
  // tombstone kinds — and a purge honestly belongs in both, so the assertions
  // that are about THIS band have to be scoped to it.
  const band = screen.getByText("Purged").closest(".crewlet-card") as HTMLElement;
  expect(band).toBeTruthy();
  // FLATTENED, like the feed's. An operator's own reason is free text, so it can
  // carry markdown too, and an excerpt rendered two ways on one screen is the
  // drift a shared helper exists to stop.
  expect(within(band).getByText("duplicate of ENG-4")).toBeTruthy();
  expect(band.textContent).not.toContain("**");
  expect(within(band).getByText("irreversible")).toBeTruthy();
  // NO LINK. The task is gone, so an anchor would lead to a NotFound on every
  // row — and the key is the entry's own, because there is no task row left to
  // resolve one from.
  // BY ROLE, not by walking up from a span: a wrapper element's own
  // `closest("a")` is null whether or not the anchor is INSIDE it, which is an
  // assertion that passes either way.
  expect(within(band).getByText("ENG-11")).toBeTruthy();
  expect(within(band).queryByRole("link", { name: "ENG-11" })).toBeNull();
  // AND AN EMPTY TRASH IS NOT "NOTHING MATCHES": the band above IS the answer
  // on a company whose removals have all been purged.
  expect(screen.queryByText("Nothing matches")).toBeNull();
});

// MARKDOWN IS THE CONTRACT, so an excerpt is a markdown fragment. The feed cell
// is ONE LINE — `.truncate` is `white-space: nowrap` — so the row drew "##
// Understanding the work This task is to interview…" with the hashes in it,
// which reads as a bug in the engine rather than as a heading.
test("the activity feed draws a markdown excerpt as prose, not as its source", () => {
  const { container } = render(
    <ActivityFeed
      records={[
        {
          id: "h-9",
          log_seq: 12,
          log_stream: "CREWLET_WORK_LOG",
          log_generation: 1,
          at: "2031-04-15T00:00:00Z",
          effective_at: "2031-04-15T00:00:00Z",
          kind: "created",
          subject_kind: "task",
          subject_id: "t-9",
          subject_key: "ENG-12",
          excerpt:
            "## Understanding the work\n\nInterview three desks about **settlement**, then read [the guide](https://docs.crewlet.ai/x).",
          notified: true,
        },
      ]}
      now={NOW}
    />,
  );
  const cell = container.querySelector(".work-feed-what") as HTMLElement;
  // BOTH HALVES. The negative alone passes on a cell that renders nothing at
  // all, which is the shape of an assertion that cannot fail.
  expect(cell.textContent).toBe(
    "Understanding the work Interview three desks about settlement, then read the guide.",
  );
  expect(cell.textContent).not.toContain("#");
  expect(cell.textContent).not.toContain("**");
  expect(cell.textContent).not.toContain("https://");
});

// ---------------------------------------------------------------------------
// The filter bar
// ---------------------------------------------------------------------------

// THE BAR IS A TOOLBAR, and `toolbar` is not a synonym for "the row of
// controls at the top": `.screen:has(.toolbar)` is what publishes
// `--sticky-top`, which is the offset every other sticky band in the same
// scroller starts at — the grid's column heads among them. Hand-rolled here,
// `.work-filters` restated every one of `.toolbar`'s declarations and omitted
// the class, so the property stayed at its 0px default and the table's own
// header parked underneath an opaque band. See styles/sticky.test.ts for the
// other half of this; a class name is the only part of it the DOM can see.
test("the filter bar declares itself the screen's toolbar", async () => {
  serving({ work_items: { items: [], groups: [], complete: true } });
  const { container } = mountWork();
  await waitFor(() => expect(container.querySelector(".work-filters")).toBeTruthy());
  expect(container.querySelector(".work-filters")?.classList.contains("toolbar")).toBe(true);
});

// AND ITS CONTROLS TRAVEL IN GROUPS. What the bar draws varies by shape — the
// type, sprint, group-by and sort pickers each appear on some tabs and not
// others — and as one flat wrapping row that put the scope control at x≈345 on
// List, x≈1338 on Board and x≈1155 on Calendar, with the Overdue chip wrapping
// to a line of its own ~1,200px from the count. A group is the wrap unit, so a
// break falls between the question and the switches and never inside either.
test("the scope control and the chips are one group, not loose children of the bar", async () => {
  serving({ work_items: { items: [], groups: [], complete: true } });
  const { container } = mountWork();
  await waitFor(() => expect(container.querySelector(".work-filters-switches")).toBeTruthy());
  const switches = container.querySelector(".work-filters-switches") as HTMLElement;
  // The scope segments and both chips, in one box.
  for (const label of ["Open", "Closed", "All", "Blocked", "Overdue"]) {
    expect(within(switches).getByText(label), `${label} is not in the switch group`).toBeTruthy();
  }
  // And the search field is NOT — it belongs to the question, which is the
  // group that takes the bar's slack.
  const ask = container.querySelector(".work-filters-ask") as HTMLElement;
  expect(ask.querySelector(".work-filters-search")).toBeTruthy();
  expect(switches.querySelector(".work-filters-search")).toBeNull();
  // Both groups are children of the bar itself, which is what makes them the
  // wrap unit — nested one inside the other they would wrap as one.
  expect(ask.parentElement).toBe(container.querySelector(".work-filters"));
  expect(switches.parentElement).toBe(container.querySelector(".work-filters"));
});

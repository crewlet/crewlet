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
 * also why the board can be drawn inside a peek panel.
 */

import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, expect, test, vi } from "vitest";
import { EMPTY_VALUE } from "@crewlethq/ui";
import { Work } from "./Work.tsx";
import { patchedHref } from "./ItemsView.tsx";
import { Board } from "./shapes/Board.tsx";
import { CalendarView } from "./shapes/Calendar.tsx";
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
// the project's lead, and a project with no lead routes to nobody at all. On a
// card it is drawn wherever the foot is drawn — beside the facts it belongs
// with, never alone, which is the case below.
test("an unassigned card draws the empty seat rather than a blank", () => {
  const { container } = card({ due: "2031-09-01T00:00:00Z" });
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

// THE GHOST HOLDS A TRACK OPEN, AND A CARD HAS NO TRACK. On `.work-row` the
// dashed square is a grid cell, and a cell that vanished would take its column
// with it and pull every later one a place left — which is the case further
// down. A card is inline flow, so on a task nobody has touched the same square
// came out alone under the title, floating in a foot with nothing else in it
// and reading as a control somebody could press. The foot is its marks: with
// none of them it is not drawn, and one fact brings it back.
test("a card with nothing set draws no foot, while the same task as a row keeps its cell", () => {
  const bare = card().container;
  expect(bare.querySelector(".work-card-foot")).toBeNull();
  expect(bare.querySelector(".work-nobody")).toBeNull();
  cleanup();
  const asRow = render(<WorkRow row={row("1")} href="#/work/ENG-1" now={NOW} chrome={{}} />);
  expect(asRow.container.querySelector(".work-nobody")).toBeTruthy();
  cleanup();
  // ONE MARK IS ENOUGH, and the ghost comes back with it: "nobody holds this,
  // and it is due on Monday" is the pair a board column is scanned for.
  const dated = card({ due: "2031-09-01T00:00:00Z" }).container;
  expect(dated.querySelector(".work-card-foot")).toBeTruthy();
  expect(dated.querySelector(".work-nobody")).toBeTruthy();
  cleanup();
  // And a card whose only fact is that it is blocked still has a foot, which
  // is what says the predicate reads every mark rather than the assignee.
  expect(card({ blocked: true }).container.querySelector(".work-card-foot")).toBeTruthy();
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
  ).toBe("#/work/ENG?assignee=ada&scope=open&shape=list&group_by=status&group=todo");
  // `null` IS THE KEY'S ABSENCE: a control that clears the grouping drops the
  // key rather than writing `group_by=`.
  expect(patchedHref(["work"], new URLSearchParams("group_by=status"), { group_by: null })).toBe(
    "#/work",
  );
  // AND `""` IS A VALUE, because one narrowing's value IS the empty string —
  // the column holding the rows with no value on the axis. Written as a
  // deletion, the "Unassigned" column's own overflow link loaded the whole
  // board; the engine tells the two apart with `Params.Has`, and
  // `URLSearchParams` round-trips `group=`.
  expect(patchedHref(["work"], new URLSearchParams(), filterPatchForGroup("assignee", ""))).toBe(
    "#/work?shape=list&group_by=assignee&group=",
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
// nothing at all reads as a column whose rows failed to arrive. This is the
// engine's own shape now — a closed axis carries every lane the scope admits
// at `count: 0` with no rows (`internal/tracker/grouping.go`), so a young
// company's open work is three lanes with one card rather than one lane.
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
  // FIVE: the priority that opens the row, the status, and the three trailing
  // facts — due, type and who holds it. The key and the title are the row's own
  // elastic tracks rather than cells, because neither is ever absent.
  expect(cells(bare)).toHaveLength(5);
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

// AND THE EMPTY STATE REPLACES THE LIST rather than following it: drawn under
// it, a company with no projects read two sentences about one blank — the
// list's "nothing matches" and this — and the first was wrong.
test("a company with no projects gets one empty state, not two", async () => {
  serving({
    work_items: { items: [], groups: [], complete: true },
    work_projects: { projects: [], total: 0, complete: true },
  });
  const { container } = mountWork();
  await waitFor(() => expect(screen.getByText("No work has been filed yet")).toBeTruthy());
  expect(screen.queryByText("Nothing matches")).toBeNull();
  expect(screen.queryByText(/Nothing is open/)).toBeNull();
  expect(container.querySelector(".work-bar")).toBeNull();
});

// AN UNFILTERED EMPTY SCOPE SAYS WHAT WOULD FILL IT. "Nothing matches" is the
// filter-miss sentence, and it was drawn over an unfiltered board — blaming a
// narrowing that did not exist, with no chip on screen to take off.
test("an empty scope says why, and only a filter says nothing matched", async () => {
  const company = {
    work_items: { items: [], groups: [], complete: true },
    work_projects: {
      projects: [
        {
          key: "ENG",
          name: "Engineering",
          unit: { resolved: true },
          lead: {},
          task_counts: { open: 0, done: 0, closed: 0 },
          version: 1,
        },
      ],
      total: 1,
      complete: true,
    },
  };
  serving(company);
  mountWork();
  await waitFor(() => expect(screen.getByText("Nothing is open")).toBeTruthy());
  expect(screen.queryByText("Nothing matches")).toBeNull();
  cleanup();

  location.hash = "#/work?scope=closed";
  serving(company);
  mountWork();
  await waitFor(() => expect(screen.getByText("Nothing has been finished yet")).toBeTruthy());
  cleanup();

  location.hash = "#/work?assignee=ada";
  serving(company);
  mountWork();
  await waitFor(() => expect(screen.getByText("Nothing matches")).toBeTruthy());
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
  await waitFor(() => expect(container.querySelector(".grid-band-head")).toBeTruthy());
  expect(container.querySelector(".grid-band-head")?.textContent).toContain("Ada Okonkwo");
  expect(container.querySelector(".grid-band-head")?.textContent).not.toContain("ada");
});

/** A company with one project — the listing every list-screen case needs,
 *  because a listing that answered with NONE replaces the list with the
 *  "nothing filed" state. */
const oneProject = {
  projects: [
    {
      key: "ENG",
      name: "Engineering",
      unit: { resolved: true },
      lead: {},
      task_counts: { open: 1, done: 0, closed: 0 },
      version: 1,
    },
  ],
  total: 1,
  complete: true,
};

/** A strip of one saved view carrying its own arrangement. */
function savedWith(params: Record<string, string>) {
  return {
    views: [
      {
        id: "v-1",
        key: "arranged",
        name: "Arranged",
        type: "list",
        container: { kind: "workspace", id: "" },
        builtin: false,
        default: true,
        params,
      },
    ],
    complete: true,
  };
}

/** The Display menu, opened. */
async function openDisplay() {
  const button = await screen.findByRole("button", {
    name: /^(List|Board|Table|Calendar|Timeline)/,
  });
  fireEvent.click(button);
  return button;
}

// THE PICKERS SHOW WHAT THE QUERY WAS SENT WITH, not what the address holds.
// Built from the URL key alone they sat on "No grouping" over a list the
// engine had grouped by assignee — and a reader looking for the control that
// turned it off found one that already said it was off.
test("a view's own arrangement is what the display controls show", async () => {
  serving({
    work_views: savedWith({ group_by: "assignee", group_by2: "priority", sort: "due" }),
    work_items: { items: [], groups: [], complete: true },
    work_projects: oneProject,
  });
  mountWork();
  // THE BUTTON SAYS IT FIRST, because the arrangement has to be readable
  // without opening anything.
  await waitFor(() => expect(screen.getByRole("button", { name: /List · Assignee/ })).toBeTruthy());
  await openDisplay();
  expect(screen.getByRole("combobox", { name: "Group by" }).textContent).toContain("Assignee");
  expect(screen.getByRole("combobox", { name: "Then by" }).textContent).toContain("Priority");
  expect(screen.getByRole("combobox", { name: "Order by" }).textContent).toContain("Due soonest");
});

// AND TURNING ONE OFF IS A VALUE ON THE ADDRESS. `group_by=` is DELETED by the
// router, after which the view supplies the axis again — so the option did
// nothing at all, twice over: the control snapped back and the rows never
// changed.
test("turning a view's grouping off says so on the address and on the wire", async () => {
  const query = serving({
    work_views: savedWith({ group_by: "assignee", sort: "due" }),
    work_items: { items: [], groups: [], complete: true },
    work_projects: oneProject,
  });
  mountWork();
  await waitFor(() => expect(asked(query).group_by).toBe("assignee"));
  await openDisplay();
  fireEvent.click(screen.getByRole("combobox", { name: "Group by" }));
  fireEvent.mouseDown(await screen.findByRole("option", { name: "No grouping" }));
  await waitFor(() => expect(location.hash).toContain("group_by=none"));
  // AND `none` NEVER REACHES THE ENGINE: it is this dashboard's word for the
  // absence of a key, not an axis the grammar has.
  await waitFor(() => expect(asked(query).group_by).toBeUndefined());
  expect(asked(query).sort).toBe("due");
});

// AN ORDINARY UNGROUPED LIST WRITES NOTHING, because there is nothing to
// override: `group_by=none` on the address of a list no view arranged is a key
// that says what the absence of it already said.
test("turning off an arrangement nobody set leaves the address clean", async () => {
  serving({
    work_views: savedWith({}),
    work_items: { items: [], groups: [], complete: true },
    work_projects: oneProject,
  });
  mountWork();
  await openDisplay();
  fireEvent.click(screen.getByRole("combobox", { name: "Order by" }));
  fireEvent.mouseDown(await screen.findByRole("option", { name: "Default order" }));
  await waitFor(() => expect(screen.queryByRole("option", { name: "Default order" })).toBeNull());
  expect(location.hash).not.toContain("sort=");
});

// A COLUMN NARROWING IS NAMED BY THE AXIS IT WAS CUT ON, and that axis can be
// the view's: labelled from the URL key alone the chip read "Column is ada"
// over a board whose own heading said "Assignee · Ada Okonkwo".
test("a column chip takes its name from the effective axis", async () => {
  location.hash = "#/work?group=ada";
  serving({
    work_views: savedWith({ group_by: "assignee" }),
    work_items: { items: [], groups: [], complete: true },
    work_projects: oneProject,
  });
  const { container } = mountWork();
  await waitFor(() => expect(container.querySelector(".work-chips")).toBeTruthy());
  const chips = container.querySelector(".work-chips")?.textContent ?? "";
  expect(chips).toContain("Assignee");
  expect(chips).toContain("Ada Okonkwo");
  expect(chips).not.toContain("Column");
});

// ---------------------------------------------------------------------------
// The grid: one renderer, two column sets
// ---------------------------------------------------------------------------

/**
 * The head row as it is drawn, in order — a glyph head reading "".
 *
 * BY TEXT AND IN ORDER, because a set is not a set of names: `cols=` carries
 * the order as well as the selection, which is the whole reason the key is per
 * shape. A comparison that sorted these would pass on a table drawing the
 * list's arrangement.
 */
const heads = (container: HTMLElement) =>
  [...container.querySelectorAll(".grid-th")].map((el) => (el.textContent ?? "").trim());

/** What the list draws with nothing chosen: the compact row, as columns. */
const LIST_SET = ["", "Key", "Status", "Title", "Due", "", "", "Updated"];

/** And the table's: one field per column, the identity first. */
const TABLE_SET = [
  "Key",
  "",
  "Title",
  "Status",
  "Priority",
  "Assignee",
  "Project",
  "Due",
  "Updated",
];

// TWO DEFAULT SETS, ONE RENDERER. The list and the table were two components
// over one answer, so every rule the grid had — one track list, a capped shrink
// column, the card a row becomes on a phone, the cursor `j` and `k` walk — the
// list simply did not have. What is left of the difference is which columns are
// on and in what order.
test("the list and the table are the same grid drawn in two column sets", async () => {
  serving({ work_items: { items: [row("1")], groups: [], total_hint: 1, complete: true } });
  const asList = mountWork();
  await waitFor(() => expect(asList.container.querySelector(".grid-wrap")).toBeTruthy());
  expect(heads(asList.container)).toEqual(LIST_SET);
  cleanup();

  location.hash = "#/work?shape=table";
  serving({ work_items: { items: [row("1")], groups: [], total_hint: 1, complete: true } });
  const asTable = mountWork();
  await waitFor(() => expect(asTable.container.querySelector(".grid-wrap")).toBeTruthy());
  expect(heads(asTable.container)).toEqual(TABLE_SET);
  // ONE GRID, both times: the list is not a hand-rolled panel any more, so it
  // has a head row at all and every column of it is sized by the same tracks.
  expect(asTable.container.querySelectorAll(".grid-wrap").length).toBe(1);
});

// `cols=` SELECTS WITHIN THE ACTIVE SET, and a name that set does not hold is
// ignored rather than drawn or refused. `restore` is a real column — the trash
// listing's — so this is the shape a stale link actually takes rather than a
// nonsense word.
test("cols narrows within the active set and ignores what the set does not hold", async () => {
  location.hash = "#/work?cols.list=key,title,restore";
  serving({ work_items: { items: [row("1")], groups: [], total_hint: 1, complete: true } });
  const { container } = mountWork();
  await waitFor(() => expect(container.querySelector(".grid-wrap")).toBeTruthy());
  expect(heads(container)).toEqual(["Key", "Title"]);
});

// AND A `cols=` NAMING NOTHING THE SET HOLDS LEAVES THE DEFAULT STANDING,
// rather than a grid with no columns at all.
test("a cols naming nothing this set draws falls back to the default set", async () => {
  location.hash = "#/work?cols.list=restore,removed_by";
  serving({ work_items: { items: [row("1")], groups: [], total_hint: 1, complete: true } });
  const { container } = mountWork();
  await waitFor(() => expect(container.querySelector(".grid-wrap")).toBeTruthy());
  expect(heads(container)).toEqual(LIST_SET);
});

// THE KEY IS PER SHAPE, which is what stops one arrangement destroying the
// other. `cols=` carries an ORDER and that order is the SET's, so a value
// written against the table and read against the list draws the list's columns
// in the table's arrangement — a row nobody asked for, and one no validation
// can catch, because every name in it is legal in both sets.
test("each shape keeps its own column arrangement", async () => {
  location.hash = "#/work?shape=table&cols.list=key,title";
  serving({ work_items: { items: [row("1")], groups: [], total_hint: 1, complete: true } });
  const { container } = mountWork();
  await waitFor(() => expect(container.querySelector(".grid-wrap")).toBeTruthy());
  // The list's narrowing is on the address and the table pays no attention.
  expect(heads(container)).toEqual(TABLE_SET);
  cleanup();

  location.hash = "#/work?shape=table&cols.table=key,title";
  serving({ work_items: { items: [row("1")], groups: [], total_hint: 1, complete: true } });
  const own = mountWork();
  await waitFor(() => expect(own.container.querySelector(".grid-wrap")).toBeTruthy());
  expect(heads(own.container)).toEqual(["Key", "Title"]);
});

// AND THE DISPLAY MENU OFFERS COLUMNS ON BOTH GRID SHAPES. It offered them on
// the table alone, under a rule that read "the column set belongs to the table,
// which is the one shape with columns" — which described the implementation
// rather than the product: the only thing making it true was that the list was
// a different component with no columns to give.
test("the Display menu offers the active set's columns on the list too", async () => {
  serving({
    work_views: savedWith({}),
    work_items: { items: [row("1")], groups: [], total_hint: 1, complete: true },
    work_projects: oneProject,
  });
  mountWork();
  await openDisplay();
  await waitFor(() => expect(screen.getByText("Columns")).toBeTruthy());
  // THROUGH THE DOCUMENT, not the mount's own container: the menu is a
  // `Popover`, so its panel is portalled out of the tree the screen rendered.
  const boxes = [...document.querySelectorAll(".work-display-col")].map(
    (el) => el.textContent ?? "",
  );
  // THE WHOLE VOCABULARY, not only what is drawn: the four the compact set
  // leaves off are choices here rather than columns a reader has to switch
  // shape to reach.
  expect(boxes).toEqual([
    "Priority",
    "Key",
    "Status",
    "Title",
    "Project",
    "Due",
    "Start",
    "Points",
    "Estimate",
    "Type",
    "Assignee",
    "Updated",
  ]);
  const ticked = [...document.querySelectorAll<HTMLInputElement>(".work-display-col input")]
    .map((box, at) => (box.checked ? boxes[at] : null))
    .filter(Boolean);
  // AND THE TICKS ARE THE DEFAULT SET until somebody moves one.
  expect(ticked).toEqual([
    "Priority",
    "Key",
    "Status",
    "Title",
    "Due",
    "Type",
    "Assignee",
    "Updated",
  ]);
});

// A BAND SAYS BOTH NUMBERS, on either shape. The engine's count is over the
// whole group and the rows under it are a page, so a heading reading `3` over
// two visible rows is the number a person plans against — the rule the grid's
// own bands already kept and the hand-rolled list restated.
test("a grouped answer draws bands with the engine's count on both shapes", async () => {
  const grouped = {
    work_items: {
      items: [],
      groups: [{ key: "todo", count: 3, rows: [row("1"), row("2")] }],
      total_hint: 3,
      complete: true,
    },
  };
  for (const shape of ["list", "table"]) {
    location.hash = `#/work?shape=${shape}&group_by=status`;
    serving(grouped);
    const { container } = mountWork();
    await waitFor(() => expect(container.querySelector(".grid-band-head")).toBeTruthy());
    const band = container.querySelector(".grid-band-head")!;
    expect(band.textContent, shape).toContain("To do");
    expect(band.textContent, shape).toContain("2 of 3");
    expect(container.querySelectorAll(".grid-row").length, shape).toBe(2);
    cleanup();
  }
});

// A SECOND AXIS IS A BAND INSIDE A BAND, on either shape — and the table drew
// NEITHER. `buildItemsParams` sends `group_by2` for both, the answer comes back
// with the rows under `subgroups` rather than under `groups[].rows`, and a grid
// that read only `rows` drew one empty heading per group with every row of the
// answer nowhere on the screen.
test("a twice-grouped answer nests its bands and keeps its rows", async () => {
  const nested = {
    work_items: {
      items: [],
      groups: [
        {
          key: "todo",
          count: 2,
          rows: [],
          subgroups: [{ key: "ada", count: 2, rows: [row("1", { assignee: "ada" }), row("2")] }],
        },
      ],
      total_hint: 2,
      complete: true,
    },
  };
  for (const shape of ["list", "table"]) {
    location.hash = `#/work?shape=${shape}&group_by=status&group_by2=assignee`;
    serving(nested);
    const { container } = mountWork();
    await waitFor(() => expect(container.querySelectorAll(".grid-row").length).toBe(2));
    const bands = [...container.querySelectorAll(".grid-band-head")].map((el) => el.textContent);
    expect(bands.length, shape).toBe(2);
    expect(bands[0], shape).toContain("To do");
    expect(bands[1], shape).toContain("Ada Okonkwo");
    // THE OUTER BAND COUNTS WHAT IS UNDER IT, sub-bands included — read off
    // its own `rows` it reported every twice-grouped group as holding nothing.
    expect(bands[0], shape).toContain("2");
    cleanup();
  }
});

// THE END OF A COMPLETE ANSWER IS SAID ON BOTH SHAPES. The list closed with it
// and the table closed with "N loaded" — a count the toolbar three lines above
// already carries — so the one shape a reader picks to compare a field was the
// one that could not tell them whether they were looking at all of it.
test("the foot says the end of it on both shapes", async () => {
  for (const shape of ["list", "table"]) {
    location.hash = `#/work?shape=${shape}`;
    serving({
      work_items: { items: [row("1"), row("2")], groups: [], total_hint: 2, complete: true },
    });
    const { container } = mountWork();
    await waitFor(() => expect(container.querySelector(".grid-foot")).toBeTruthy());
    expect(container.querySelector(".grid-foot")?.textContent, shape).toBe(
      "That is all of it · 2 items",
    );
    cleanup();
  }
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

// ---------------------------------------------------------------------------
// The filter bar
// ---------------------------------------------------------------------------

// THE BAR IS A TOOLBAR, and `toolbar` is not a synonym for "the row of
// controls at the top": `.screen:has(.toolbar)` is what publishes
// `--sticky-top`, which is the offset every other sticky band in the same
// scroller starts at — the grid's column heads and a list's group bands among
// them. Hand-rolled, this bar restated every one of `.toolbar`'s declarations
// and omitted the class, so the property stayed at its 0px default and the
// table's own header parked underneath an opaque band. See
// styles/sticky.test.ts for the other half of this; a class name is the only
// part of it the DOM can see.
test("the bar declares itself the screen's toolbar", async () => {
  serving({ work_items: { items: [], groups: [], complete: true } });
  const { container } = mountWork();
  await waitFor(() => expect(container.querySelector(".work-bar")).toBeTruthy());
  expect(container.querySelector(".work-bar")?.classList.contains("toolbar")).toBe(true);
});

// THREE CONTROLS, NOT ELEVEN. The bar carried a search box, five selects, a
// three-way switch, two chips, a Clear button and the count — and which of
// them appeared depended on the shape, so the scope switch sat at x≈345 on
// List, x≈1338 on Board and x≈1155 on Calendar. What is left is the Filter
// menu, the substring mark, the scope switch and the Display menu, so there is
// no arrangement left for a wrap to disturb.
test("the bar is two menus, a switch and a mark", async () => {
  serving({ work_items: { items: [], groups: [], complete: true } });
  const { container } = mountWork();
  await waitFor(() => expect(container.querySelector(".work-bar")).toBeTruthy());
  const bar = container.querySelector(".work-bar") as HTMLElement;
  expect(within(bar).getByText("Filter")).toBeTruthy();
  // The Display button SAYS WHAT IS ON, so the arrangement is readable without
  // opening anything — which is what the strip of shape tabs used to do.
  expect(within(bar).getByText("List")).toBeTruthy();
  for (const label of ["Open", "Closed", "All"]) {
    expect(within(bar).getByText(label), `${label} is not in the bar`).toBeTruthy();
  }
  // AND THE PICKERS ARE GONE FROM IT. A control per field is what a menu
  // replaces, and one left behind would be a second way to set the same key.
  expect(within(bar).queryByLabelText("Status")).toBeNull();
  expect(within(bar).queryByLabelText("Priority")).toBeNull();
  expect(within(bar).queryByLabelText("Assignee")).toBeNull();
});

// A CHIP IS DRAWN ONLY WHERE A FILTER IS APPLIED, so an unfiltered list has no
// chip row at all — which is the whole difference from a bar of controls that
// were all drawn whether or not they were set.
test("an unfiltered list draws no chips, and a filtered one says the whole narrowing", async () => {
  serving({ work_items: { items: [], groups: [], complete: true } });
  const bare = mountWork();
  await waitFor(() => expect(bare.container.querySelector(".work-bar")).toBeTruthy());
  expect(bare.container.querySelector(".work-chips")).toBeNull();
  cleanup();

  location.hash = "#/work?assignee=ada&blocked=true";
  serving({ work_items: { items: [], groups: [], complete: true } });
  const filtered = mountWork();
  await waitFor(() => expect(filtered.container.querySelector(".work-chips")).toBeTruthy());
  const chips = filtered.container.querySelector(".work-chips") as HTMLElement;
  // The company's own word for the person, not the handle the URL carries.
  expect(within(chips).getByText("Ada Okonkwo")).toBeTruthy();
  expect(within(chips).getByText("Blocked")).toBeTruthy();
  // And one control that takes them all off.
  expect(within(chips).getByText("Clear")).toBeTruthy();
});

// TAKING A CHIP OFF CLEARS THE ONE KEY IT NAMES and nothing else, which is
// what makes a chip removable without a table of removers beside the table of
// chips.
test("removing a chip clears its own key and leaves the rest", async () => {
  location.hash = "#/work?assignee=ada&priority=high";
  serving({ work_items: { items: [], groups: [], complete: true } });
  const { container } = mountWork();
  await waitFor(() => expect(container.querySelector(".work-chips")).toBeTruthy());
  const chips = container.querySelector(".work-chips") as HTMLElement;
  fireEvent.click(within(chips).getByLabelText("Remove the Assignee is Ada Okonkwo filter"));
  await waitFor(() => expect(location.hash).not.toContain("assignee=ada"));
  expect(location.hash).toContain("priority=high");
});

// THE SAVED VIEWS ARE THE STRIP AND THE SHAPES ARE NOT. A saved view is a
// QUERY somebody arranged; a shape is how any query is drawn. Mixed into one
// strip they read as the same kind of thing, and a company with four saved
// views had a strip of nine.
test("the strip holds what somebody saved, never the five shapes", async () => {
  serving({
    work_views: {
      views: [
        ...["list", "board", "table"].map((key) => ({
          id: "",
          key,
          name: key[0]!.toUpperCase() + key.slice(1),
          type: key,
          container: { kind: "workspace", id: "" },
          builtin: true,
          default: key === "board",
          params: {},
        })),
        {
          id: "v-1",
          key: "overdue-by-owner",
          name: "Overdue, by owner",
          type: "list",
          container: { kind: "workspace", id: "" },
          builtin: false,
          params: { group_by: "assignee" },
        },
      ],
      complete: true,
    },
    work_items: { items: [], groups: [], complete: true },
  });
  mountWork();
  await waitFor(() => expect(screen.getByText("Overdue, by owner")).toBeTruthy());
  const tabs = screen.getAllByRole("tab").map((el) => el.textContent);
  expect(tabs).toEqual(["All work", "Overdue, by owner"]);
});

// AND THE SHAPE IS ITS OWN KEY, so switching a saved view to another drawing
// keeps the view. They were one key, so a reader who wanted their saved board
// as a list lost the filters that made it theirs.
test("the shape is a key beside the view rather than the same one", async () => {
  location.hash = "#/work?view=overdue-by-owner&shape=list";
  const query = serving({
    work_views: {
      views: [
        {
          id: "v-1",
          key: "overdue-by-owner",
          name: "Overdue, by owner",
          type: "board",
          container: { kind: "workspace", id: "" },
          builtin: false,
          params: { assignee: "ada" },
        },
      ],
      complete: true,
    },
    work_items: { items: [], groups: [], complete: true },
  });
  mountWork();
  // THE VIEW'S OWN FILTER SURVIVED the shape override, which is the whole
  // point of the two keys.
  await waitFor(() => expect(asked(query).assignee).toBe("ada"));
  // And a list is a paged question rather than a set of columns.
  expect(asked(query).limit).toBe(100);
  expect(asked(query).group_by).toBeUndefined();
});

// ---------------------------------------------------------------------------
// The sparse state
// ---------------------------------------------------------------------------

/** A project row, for the one fact these cases need from the directory. */
const listedProject = (counts = { open: 0, done: 0, closed: 0 }): WorkProjectRow => ({
  key: "ENG",
  name: "Engineering",
  unit: { resolved: true },
  lead: {},
  task_counts: counts,
  version: 1,
});

// THE LANDING SHAPE IS THE LIST, and nothing in the engine decides it: no
// builtin view is marked `default`, so this fallback is what every company that
// has saved nothing lands on. A board's information is the comparison ACROSS
// its lanes, so at one item it is one 292px card in a 1500px field — where a
// list degrades to one full-width row and is still a list.
test("a container with no default view opens on the list", async () => {
  const query = serving({
    work_items: { items: [row("1")], groups: [], total_hint: 1, complete: true },
  });
  const { container } = mountWork();
  await waitFor(() => expect(container.querySelector(".grid-wrap")).toBeTruthy());
  // A LIST IS A PAGED QUESTION rather than a set of columns, which is the half
  // of this that reaches the engine.
  expect(asked(query).limit).toBe(100);
  expect(asked(query).group_by).toBeUndefined();
  expect(asked(query).group_limit).toBeUndefined();
  // And the Display button says which shape is on without anything opening.
  expect(screen.getByText("List")).toBeTruthy();
  expect(container.querySelector(".work-board")).toBeNull();
});

// A BOARD IS THE WORKFLOW, NOT THE OCCUPIED PART OF IT — and the ENGINE says
// so: `work_items` carries every column the query's own predicate admits, the
// empty ones at `count: 0` with an empty row list, so the one-item company
// gets the denominator the comparison a board exists for needs. What is left
// here is the DRAWING: a zero lane is a head with its count and a body saying
// it is empty, rather than a heading over nothing.
test("a board draws every declared lane the scope admits, and says which are empty", async () => {
  location.hash = "#/work?shape=board";
  serving({
    work_items: {
      items: [],
      // AS THE ENGINE SENDS IT on the Open segment: the three open statuses,
      // and no Done lane, because the query excluded finished work.
      groups: [
        { key: "todo", count: 1, rows: [row("1")] },
        { key: "in_progress", count: 0, rows: [] },
        { key: "in_review", count: 0, rows: [] },
      ],
      total_hint: 1,
      complete: true,
    },
  });
  const { container } = mountWork();
  await waitFor(() => expect(container.querySelector(".work-board")).toBeTruthy());
  const heads = [...container.querySelectorAll(".work-col-head")].map((el) => el.textContent ?? "");
  expect(heads.length).toBe(3);
  expect(heads[0]).toContain("To do");
  expect(heads[1]).toContain("In progress");
  expect(heads[2]).toContain("In review");
  // A ZERO LANE CARRIES ITS OWN COUNT and says it is empty, rather than being a
  // heading over nothing.
  expect(heads[1]).toContain("0");
  expect(container.querySelectorAll(".work-col-empty").length).toBe(2);
  // AND NO DEAD DONE LANE: the Open segment's query cannot return one, so
  // drawing it would be a column the reader can never fill from here.
  expect(heads.some((head) => head.includes("Done"))).toBe(false);
});

// AN OPEN SET IS NOT A BOARD. A lane per possible assignee is every handle the
// company could ever hold, so an assignee board is still the answer's own.
test("an assignee board draws only the columns the answer carried", async () => {
  location.hash = "#/work?shape=board&group_by=assignee";
  serving({
    work_items: {
      items: [],
      groups: [{ key: "ada", count: 1, rows: [row("1", { assignee: "ada" })] }],
      total_hint: 1,
      complete: true,
    },
  });
  const { container } = mountWork();
  await waitFor(() => expect(container.querySelector(".work-board")).toBeTruthy());
  expect(container.querySelectorAll(".work-col-head").length).toBe(1);
});

// THE STRIP IS DRAWN WHETHER OR NOT ANYBODY HAS SAVED ANYTHING. Its first tab
// is the container's own list — a real destination, and the one thing that
// names this page inside its own content column — so gating the whole strip on
// somebody else's saved query left the toolbar as the top edge of the screen.
test("the strip and its way into the inventory survive a company that saved nothing", async () => {
  serving({ work_items: { items: [], groups: [], complete: true } });
  mountWork();
  await waitFor(() => expect(screen.getByRole("tab", { name: "All work" })).toBeTruthy());
  // A REAL ANCHOR, middle-clickable like every other way out of a screen.
  const more = screen.getByText("All views →");
  expect(more.tagName).toBe("A");
  expect(more.getAttribute("href")).toBe("#/work/views");
  // AND NO COUNT ON THE CONTAINER TAB. The engine's total is over the FILTER
  // rather than over the container, so a number here would read as the
  // container's size and be the size of whatever is narrowed.
  expect(screen.getByRole("tab", { name: "All work" }).textContent).toBe("All work");
});

// (a) A NARROWING THAT MATCHED NOTHING CARRIES THE CONTROL THAT REMOVES IT,
// rather than a sentence describing one.
test("a filtered list that matched nothing offers to clear the filters", async () => {
  location.hash = "#/work?assignee=ada";
  serving({
    work_items: { items: [], groups: [], total_hint: 0, complete: true },
    work_projects: { projects: [listedProject()], total: 1, complete: true },
  });
  mountWork();
  await waitFor(() => expect(screen.getByText("Nothing matches")).toBeTruthy());
  fireEvent.click(screen.getByRole("button", { name: "Clear filters" }));
  await waitFor(() => expect(location.hash).not.toContain("assignee=ada"));
});

// (b) NOTHING IN THIS SCOPE IS NOT A FILTER THAT MATCHED NOTHING. "Widen them"
// named a control that is not on the screen: a chip row exists only when a
// filter does, so an unfiltered list had nothing to widen and nothing to clear.
test("an unfiltered list with nothing open names the scope switch, not the filters", async () => {
  serving({
    work_items: { items: [], groups: [], total_hint: 0, complete: true },
    work_projects: { projects: [listedProject()], total: 1, complete: true },
  });
  mountWork();
  await waitFor(() => expect(screen.getByText("Nothing is open")).toBeTruthy());
  expect(screen.queryByText("Nothing matches")).toBeNull();
  // AND IT CLAIMS NO NUMBER AT WORKSPACE SCOPE: `work_items` counts what
  // MATCHED, so how much finished work sits behind the other segment is not in
  // this answer at all. It names the switch and says nothing about what is
  // behind it, where a project's own counted sentence can say exactly.
  expect(screen.queryByText(/\d+ items? under Closed/)).toBeNull();
  expect(screen.getByText(/Finished work is under Closed/)).toBeTruthy();
});

// (c) AND A COMPANY THAT HAS FILED NOTHING GETS ONE PANEL, not two stacked.
// The page holds the wider fact — the project list — and the list holds the
// rows, so neither can gate the other's sentence without being told.
test("a first-run company gets the page's own panel and no second one from the list", async () => {
  serving({
    work_items: { items: [], groups: [], total_hint: 0, complete: true },
    work_projects: { projects: [], total: 0, complete: true },
  });
  mountWork();
  await waitFor(() => expect(screen.getByText("No work has been filed yet")).toBeTruthy());
  expect(screen.queryByText("Nothing is open")).toBeNull();
  expect(screen.queryByText("Nothing matches")).toBeNull();
});

// A LIST THAT REACHED ITS END AND ONE THAT WAS CUT OFF ended the same way:
// rows, then page ground. The count in the bar is silent once everything
// matching is on screen (`totalHint`) and a cursor is invisible, so the reader
// of a page could not tell whether there was another one.
test("a complete list closes with the end of it and an incomplete one does not", async () => {
  serving({
    work_items: { items: [row("1"), row("2")], groups: [], total_hint: 2, complete: true },
  });
  const whole = mountWork();
  await waitFor(() => expect(whole.container.querySelector(".grid-foot")).toBeTruthy());
  expect(whole.container.querySelector(".grid-foot")?.textContent).toBe(
    "That is all of it · 2 items",
  );
  cleanup();

  serving({
    work_items: {
      items: [row("1"), row("2")],
      groups: [],
      total_hint: 240,
      next_cursor: "c1",
      complete: true,
    },
  });
  const paged = mountWork();
  await waitFor(() => expect(paged.container.querySelector(".grid-wrap")).toBeTruthy());
  expect(paged.container.querySelector(".grid-foot")).toBeNull();
});

// AND A NARROWING CHANGES THE NOUN AND NOTHING ELSE. It never says how many the
// filter hides: the engine's `total_hint` is counted over the same predicate as
// the rows, so the unfiltered total is not in this answer and synthesising it
// would take a second query at a second instant.
test("a narrowed list says its rows are the ones that match, and counts nothing else", async () => {
  location.hash = "#/work?assignee=ada";
  serving({
    work_items: { items: [row("1")], groups: [], total_hint: 1, complete: true },
  });
  const { container } = mountWork();
  await waitFor(() => expect(container.querySelector(".grid-foot")).toBeTruthy());
  expect(container.querySelector(".grid-foot")?.textContent).toBe(
    "That is all of it · 1 item matches",
  );
});

// THE UNSET COLUMN IS A COLUMN, and its overflow has to reach it. `group=` with
// nothing after it is the engine's own key for the rows with no value on the
// axis (`Params.Has` is what tells it from an absent key) — and every writer on
// this screen read an empty value as "no key": the query builder dropped it,
// `useParam` could not say it, and the address writer deleted it. So following
// "2 more →" out of Unassigned loaded the whole board, which is the one column
// overflow a lead actually follows.
test("the unset column's overflow narrows to that column rather than the whole board", async () => {
  location.hash = "#/work?shape=board&group_by=assignee";
  const query = serving({
    work_items: {
      items: [],
      groups: [{ key: "", count: 3, rows: [row("1")] }],
      total_hint: 3,
      complete: true,
    },
  });
  const { container } = mountWork();
  await waitFor(() => expect(container.querySelector(".work-col-foot a")).toBeTruthy());
  const more = container.querySelector(".work-col-foot a") as HTMLAnchorElement;
  // THE LINK CARRIES THE KEY WITH NOTHING AFTER IT, which is what a middle
  // click follows.
  expect(more.getAttribute("href")).toContain("group_by=assignee");
  expect(more.getAttribute("href")).toMatch(/[?&]group=(&|$)/);
  // AND THE CLICK NAMES THE SAME PLACE, down to the key being present.
  fireEvent.click(more);
  await waitFor(() => expect(location.hash).toContain("shape=list"));
  expect(new URLSearchParams(location.hash.split("?")[1]).has("group")).toBe(true);
  // Which reaches the wire as a narrowing rather than as nothing.
  await waitFor(() => expect(asked(query).group).toBe(""));
  expect(asked(query).group_by).toBe("assignee");
  // And the chip says which column it is, in the axis's own word — the same
  // resolver the column head it came from uses, so the two cannot disagree.
  const chips = container.querySelector(".work-chips") as HTMLElement;
  expect(within(chips).getByText("Unassigned")).toBeTruthy();
  expect(within(chips).getByText("Assignee")).toBeTruthy();
});

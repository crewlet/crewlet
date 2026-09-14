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

import { cleanup, fireEvent, render, screen } from "@testing-library/react";
import { afterEach, expect, test, vi } from "vitest";
import { Board, CalendarView, ProjectHead, WorkspaceHead } from "./Work.tsx";
import { BoardCard, WorkRow } from "~/components/work.tsx";
import { calendarWeeks, dayKey } from "~/lib/work.ts";
import type {
  WorkGroup,
  WorkProjectDetail,
  WorkProjectRow,
  WorkSummary,
} from "~/protocol/index.ts";

afterEach(cleanup);

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
  expect(held.container.querySelector(".avatar")?.textContent).toBe("AL");
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
    />,
  );

// A COLUMN'S COUNT IS OVER THE WHOLE SET and its rows are a slice, so a column
// of four hundred says four hundred and hands back fifty. The overflow has to
// be REACHABLE rather than merely counted: a board that said "350 more" with
// no way to see them is a board that hides a backlog.
test("a capped column offers its overflow and an uncapped one does not", () => {
  board([group("todo", { count: 53, rows: [row("a")] })]);
  const more = screen.getByText("52 more →");
  expect(more.getAttribute("href")).toContain("group_by=status");
  expect(more.getAttribute("href")).toContain("group=todo");
  cleanup();
  board([group("todo", { count: 1, rows: [row("a")] })]);
  expect(screen.queryByText(/more →/)).toBeNull();
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

// UNDATED WORK IS NOT ON THE CALENDAR AND THE SCREEN SAYS SO. A reader who
// cannot see the omission reads the month as the whole backlog.
test("undated rows are counted out loud rather than dropped in silence", () => {
  calendar([row("a"), row("b")]);
  expect(screen.getByText(/2 of the items on this page carry no due date/)).toBeTruthy();
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
  const { container } = render(<ProjectHead detail={project()} />);
  expect(container.querySelector(".stackbar")).toBeTruthy();
  expect(container.querySelector(".legend")).toBeTruthy();
  cleanup();
  const fresh = render(
    <ProjectHead detail={project({ task_counts: { open: 0, done: 0, closed: 0 } })} />,
  );
  expect(fresh.container.querySelector(".stackbar")).toBeNull();
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
  const fills = [...container.querySelectorAll(".meter-track > div")].map(
    (el) => (el as HTMLElement).style.background,
  );
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
  expect(container.querySelector(".meter-track")).toBeNull();
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
  expect(container.querySelector(".avatar")?.textContent).toBe("AL");
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

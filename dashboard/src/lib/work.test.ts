/**
 * The tracker's derivations, and the wrong form each one has.
 *
 * Every case below is a rendering whose wrong shape reads as a DIFFERENT FACT
 * rather than as a missing one — a column headed with a blank instead of
 * "Unassigned", a burndown filter that silently widens to the whole company, a
 * calendar that puts a task on the wrong side of midnight. None of them is
 * visible in a diff and none of them throws.
 */

import { expect, test } from "vitest";
import {
  totalHint,
  anyFilter,
  bucketByDay,
  buildItemsParams,
  calendarWeeks,
  dayKey,
  defaultView,
  describeChange,
  describeHistory,
  fieldValueText,
  filterPatchForGroup,
  fmtMinutes,
  gridRange,
  groupLabel,
  monthOf,
  NO_FILTERS,
  projectKeys,
  scopeOf,
  shapeOf,
  shiftMonth,
  shownRows,
  statusLabel,
  typeIcon,
  typeName,
} from "./work.ts";
import type {
  WorkActivityRecord,
  WorkChange,
  WorkFieldDef,
  WorkFieldValue,
  WorkGroup,
  WorkProjectRow,
  WorkSummary,
  WorkView,
} from "~/protocol/index.ts";

const row = (id: string, over: Partial<WorkSummary> = {}): WorkSummary => ({
  id,
  key: "ENG-" + id,
  project: "ENG",
  title: id,
  type: "task",
  status: "todo",
  updated: "2031-04-16T00:00:00Z",
  version: 1,
  ...over,
});

const view = (over: Partial<WorkView> = {}): WorkView => ({
  key: "board",
  name: "Board",
  type: "board",
  container: { kind: "project", id: "ENG" },
  builtin: true,
  ...over,
});

const group = (key: string, over: Partial<WorkGroup> = {}): WorkGroup => ({
  key,
  count: 1,
  rows: [],
  ...over,
});

// ---------------------------------------------------------------------------
// Vocabulary
// ---------------------------------------------------------------------------

// A STATUS WEARS THE PROJECT'S OWN WORD. A company may rename what it calls
// each of the six, and a screen that rendered the slug shows a word the team
// does not use.
test("a status takes the project's label, then the shipped one, then itself", () => {
  expect(statusLabel("in_progress")).toBe("In progress");
  expect(
    statusLabel("in_progress", [
      { status: "in_progress", label: "Doing", group: "active", description: "" },
    ]),
  ).toBe("Doing");
  // A STATUS THIS BUILD HAS NEVER HEARD OF RENDERS AS ITSELF rather than
  // vanishing — the set is closed on the engine and open on the wire.
  expect(statusLabel("escalated")).toBe("Escalated");
});

// A TYPE IS AN ICON, and a company's own type is the neutral box rather than
// nothing: a card with no mark reads as a card whose type failed to load.
test("every shipped type has its own icon and an unknown one falls back", () => {
  expect(typeIcon("bug")).toBe("bug");
  expect(typeIcon("milestone")).toBe("flag");
  expect(typeIcon("incident")).toBe("box");
  expect(typeIcon(undefined)).toBe("box");
});

test("a type takes the company's own name for it", () => {
  expect(typeName("bug")).toBe("Bug");
  expect(typeName("bug", [{ slug: "bug", name: "Defect" }])).toBe("Defect");
});

// AN UNESTIMATED TASK IS A DASH, never "0m": zero is a measurement and an
// unsized task is one nobody has sized, which is the difference a sprint
// report spends a whole column on.
test("an absent estimate is a dash and a present one reads in hours", () => {
  expect(fmtMinutes(undefined)).toBe("—");
  expect(fmtMinutes(0)).toBe("—");
  expect(fmtMinutes(45)).toBe("45m");
  expect(fmtMinutes(90)).toBe("1h 30m");
  expect(fmtMinutes(480)).toBe("8h");
});

// ---------------------------------------------------------------------------
// Grouping
// ---------------------------------------------------------------------------

// THE EMPTY COLUMN IS NAMED, and only the client can name it: the label
// depends on what the axis MEANS, so an empty assignee is "Unassigned" and an
// empty sprint is "No sprint". A blank heading leaves a column of real work
// nobody can identify.
test("an empty group key is named for its own axis", () => {
  expect(groupLabel("assignee", group(""))).toBe("Unassigned");
  expect(groupLabel("sprint", group(""))).toBe("No sprint");
  expect(groupLabel("tag", group(""))).toBe("Untagged");
  expect(groupLabel("type", group(""))).toBe("No type");
});

test("a group takes the server's label when it sends one", () => {
  expect(groupLabel("type", group("f1", { label: "Severity: high" }))).toBe("Severity: high");
});

test("an assignee column shows the seat's name rather than its handle", () => {
  expect(groupLabel("assignee", group("ada"), { seatName: () => "Ada Lovelace" })).toBe(
    "Ada Lovelace",
  );
});

// ---------------------------------------------------------------------------
// Changes
// ---------------------------------------------------------------------------

const record = (over: Partial<WorkActivityRecord> = {}): WorkActivityRecord => ({
  id: "r",
  log_seq: 1,
  log_stream: "s",
  log_generation: 1,
  at: "2031-04-16T00:00:00Z",
  effective_at: "2031-04-16T00:00:00Z",
  kind: "status",
  subject_kind: "task",
  subject_id: "t",
  notified: true,
  ...over,
});

test("a feed change renders its deltas, then its excerpt, then its kind", () => {
  expect(record({ fields: { status: { from: "todo", to: "in_progress" } } })).toBeTruthy();
  expect(describeChange(record({ fields: { status: { from: "todo", to: "in_progress" } } }))).toBe(
    "status: todo → in_progress",
  );
  expect(describeChange(record({ kind: "comment", excerpt: "rolled back" }))).toBe("rolled back");
  expect(describeChange(record({ kind: "comment_resolved" }))).toBe("comment resolved");
});

// AN EMPTY SIDE IS A DASH on either end: "assignee:  → ada" reads as a
// rendering bug, where "assignee: — → ada" reads as an assignment.
test("an empty side of a feed delta renders as a dash", () => {
  expect(describeChange(record({ fields: { assignee: { from: "", to: "ada" } } }))).toBe(
    "assignee: — → ada",
  );
  expect(describeChange(record({ fields: { assignee: { from: "ada", to: "" } } }))).toBe(
    "assignee: ada → —",
  );
});

const change = (over: Partial<WorkChange> = {}): WorkChange => ({
  id: "h",
  kind: "status",
  at: "2031-04-16T00:00:00Z",
  log_seq: 1,
  ...over,
});

// A HISTORY ENTRY'S FIELDS ARE TWO SHAPES, and a renderer that assumed one
// printed `[object Object]` on the other: the applier writes a from/to pair
// where it can compare two documents, and the notification's own values —
// plain scalars — where it cannot, which is every comment, mention and ask.
test("a history entry renders a delta pair and a bare value alike", () => {
  expect(describeHistory(change({ fields: { status: { from: "todo", to: "done" } } }))).toBe(
    "status: todo → done",
  );
  expect(describeHistory(change({ kind: "comment", fields: { mentions: ["ada", "bo"] } }))).toBe(
    "mentions: ada, bo",
  );
  expect(describeHistory(change({ kind: "watcher_added" }))).toBe("watcher added");
  expect(describeHistory(change({ kind: "comment", excerpt: "shipped it" }))).toBe("shipped it");
});

// ---------------------------------------------------------------------------
// Custom fields
// ---------------------------------------------------------------------------

const defs = new Map<string, WorkFieldDef>([
  [
    "f-sev",
    {
      id: "f-sev",
      slug: "severity",
      name: "Severity",
      type: "dropdown",
      config: {
        options: [
          { id: "o-1", slug: "high", name: "High" },
          { id: "o-2", slug: "low", name: "Low" },
        ],
      },
    },
  ],
  [
    "f-eff",
    { id: "f-eff", slug: "effort", name: "Effort", type: "number", config: { unit: "days" } },
  ],
]);

const value = (over: Partial<WorkFieldValue> & { id: string }): WorkFieldValue => ({
  value: null,
  ...over,
});

// A CHOICE STORES THE OPTION'S ID, which is what lets a company rename an
// option without orphaning the tasks that chose it — so a panel that rendered
// the stored value would print a uuid under a field heading.
test("a choice renders its option's name, and an unknown option its id", () => {
  expect(fieldValueText(value({ id: "f-sev", type: "dropdown", value: "o-1" }), defs)).toBe("High");
  expect(fieldValueText(value({ id: "f-sev", type: "dropdown", value: "o-9" }), defs)).toBe("o-9");
});

test("a number wears its declared unit and a checkbox reads as a word", () => {
  expect(fieldValueText(value({ id: "f-eff", type: "number", value: 3 }), defs)).toBe("3 days");
  expect(fieldValueText(value({ id: "f-x", type: "checkbox", value: false }), defs)).toBe("No");
  expect(fieldValueText(value({ id: "f-x", type: "checkbox", value: true }), defs)).toBe("Yes");
});

// AN UNSET FIELD IS A DASH. Rendering `null` as "null" is the shape that makes
// a properties panel look broken on every task that did not set a field.
test("an unset value is an em dash", () => {
  expect(fieldValueText(value({ id: "f-eff", type: "number", value: null }), defs)).toBe("—");
});

test("a people field resolves each handle to a name", () => {
  expect(
    fieldValueText(value({ id: "f-p", type: "people", value: ["ada"] }), defs, () => "Ada"),
  ).toBe("Ada");
});

// ---------------------------------------------------------------------------
// Views and the query
// ---------------------------------------------------------------------------

test("a builtin view's shape is its own, and an unknown key draws the board", () => {
  const views = [
    view({ key: "list", type: "list" }),
    view({ key: "calendar", type: "calendar" }),
    view({ key: "saved", type: "calendar", builtin: false }),
  ];
  expect(shapeOf("list", views)).toBe("list");
  expect(shapeOf("calendar", views)).toBe("calendar");
  expect(shapeOf("saved", views)).toBe("calendar");
  // A STRIP THAT HAS NOT ARRIVED is the ordinary state of the first paint,
  // and a body that waited for it would flash empty on every navigation.
  expect(shapeOf("board", [])).toBe("board");
});

test("the landing tab is the one the container marks, else the board", () => {
  expect(defaultView([view({ key: "list" }), view({ key: "mine", default: true })])).toBe("mine");
  expect(defaultView([view({ key: "list" })])).toBe("board");
});

const build = (over: Partial<Parameters<typeof buildItemsParams>[0]> = {}) =>
  buildItemsParams({
    container: "project:ENG",
    shape: "board",
    view: {},
    filters: NO_FILTERS,
    ...over,
  });

// OPEN AND CLOSED ARE STATUS GROUPS, not a boolean, and the third segment
// DELETES the key — including one a view brought with it. A control that
// could not reach "everything" is how a board that never shows a closed item
// ships.
test("the three scope segments are three real questions", () => {
  expect(build({ filters: { ...NO_FILTERS, scope: "open" } }).status_group).toBe(
    "not_started,active",
  );
  expect(build({ filters: { ...NO_FILTERS, scope: "closed" } }).status_group).toBe("done,closed");
  const all = build({
    view: { status_group: "not_started,active" },
    filters: { ...NO_FILTERS, scope: "" },
  });
  expect(all.status_group).toBeUndefined();
});

// A VIEW IS A SET OF DEFAULTS AND EVERY EXPLICIT KEY OVERRIDES IT, which is
// what makes picking another assignee on a saved board give you that board
// with one key changed rather than a board that quietly stops being saved.
test("an explicit filter beats the view it was opened from", () => {
  const params = build({
    view: { assignee: "ada", q: "login" },
    filters: { ...NO_FILTERS, assignee: "bo" },
  });
  expect(params.assignee).toBe("bo");
  expect(params.q).toBe("login");
});

// A SPRINT NAMES ONE OF A PROJECT'S OWN, so the engine refuses every value but
// `none` at the workspace. Sending it anyway turns a board into a refusal the
// screen would then have to explain.
test("a sprint filter is dropped at the workspace and kept in a project", () => {
  expect(
    build({
      container: "workspace",
      view: { sprint: "active" },
      filters: { ...NO_FILTERS, sprint: "4" },
    }).sprint,
  ).toBeUndefined();
  expect(build({ filters: { ...NO_FILTERS, sprint: "4" } }).sprint).toBe("4");
  // `none` is the backlog and means the same thing everywhere.
  expect(build({ container: "workspace", filters: { ...NO_FILTERS, sprint: "none" } }).sprint).toBe(
    "none",
  );
});

// A BOARD IS COLUMNS AND MINTS NO CURSOR; a list is a page. Sending `limit` on
// a grouped answer bounds nothing, and sending `group_by` on a list turns its
// rows into columns the list cannot draw.
test("each shape asks its own question of the one grammar", () => {
  const board = build({ shape: "board" });
  expect(board.group_by).toBe("status");
  expect(board.group_limit).toBe(50);
  expect(board.limit).toBeUndefined();

  const list = build({ shape: "list" });
  expect(list.group_by).toBeUndefined();
  expect(list.limit).toBe(100);

  const grouped = build({ shape: "list", filters: { ...NO_FILTERS, groupBy: "assignee" } });
  expect(grouped.group_by).toBe("assignee");
  expect(grouped.group_limit).toBe(100);
});

// THE CALENDAR HAS ITS OWN AXIS: a grouping brought in by a view would replace
// the rows with columns, and a month drawn from columns has empty cells.
test("the calendar drops every grouping and asks for the grid's own range", () => {
  const params = build({
    shape: "calendar",
    view: { group_by: "status" },
    range: { from: "2031-03-31", to: "2031-05-05" },
  });
  expect(params.group_by).toBeUndefined();
  expect(params.due).toBe("range:2031-03-31..2031-05-05");
  expect(params.limit).toBe(500);
  expect(params.sort).toBe("due");
});

// THE SORT IS OMITTED where nobody asked for one, so the ENGINE's default
// applies — the manual order inside a project and the most recently touched
// across the company, which are two different right answers.
test("no sort is sent unless somebody chose one", () => {
  expect(build({ shape: "list" }).sort).toBeUndefined();
  expect(build({ shape: "list", filters: { ...NO_FILTERS, sort: "due" } }).sort).toBe("due");
});

test("the quick filters map onto the grammar's own keys", () => {
  expect(build({ filters: { ...NO_FILTERS, blocked: true } }).blocked).toBe(true);
  expect(build({ filters: { ...NO_FILTERS, overdue: true } }).due).toBe("overdue");
});

test("a column's overflow lands on the list, narrowed to that column", () => {
  expect(filterPatchForGroup("assignee", "ada")).toEqual({
    view: "list",
    group_by: "assignee",
    group: "ada",
  });
});

test("a clear control appears only once something is narrowing the rows", () => {
  expect(anyFilter(NO_FILTERS)).toBe(false);
  expect(anyFilter({ ...NO_FILTERS, scope: "" })).toBe(true);
  expect(anyFilter({ ...NO_FILTERS, assignee: "ada" })).toBe(true);
});

test("a view's status groups map back onto the three segments", () => {
  expect(scopeOf("not_started,active")).toBe("open");
  expect(scopeOf("done,closed")).toBe("closed");
  expect(scopeOf("active")).toBe("");
  expect(scopeOf(undefined)).toBe("");
});

// ---------------------------------------------------------------------------
// Rows
// ---------------------------------------------------------------------------

test("a flat answer's rows are its items and a grouped one's are its columns'", () => {
  const items = [row("a"), row("b")];
  expect(shownRows(items, [])).toEqual(items);
  expect(
    shownRows(
      [],
      [
        group("todo", { count: 40, rows: [row("a"), row("b")] }),
        group("in_progress", { count: 7, rows: [row("c")] }),
      ],
    ).map((r) => r.id),
  ).toEqual(["a", "b", "c"]);
});

test("a subgroup's rows are not counted twice", () => {
  expect(
    shownRows(
      [],
      [
        group("todo", {
          count: 2,
          rows: [row("a"), row("b")],
          subgroups: [group("api", { rows: [row("a")] })],
        }),
      ],
    ).map((r) => r.id),
  ).toEqual(["a", "b"]);
});

// THE PROJECT FILTER OFFERS THE COMPANY'S PROJECTS, not the page's. Derived
// from the rows it could only ever offer what was already on screen, so a
// board narrowed to one project offered exactly that one and no way back.
test("the project list comes from the listing, falling back to the rows", () => {
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
  // AND FALLS BACK RATHER THAN EMPTYING while the listing is in flight: a
  // control that disappears on every re-read is worse than one offering less.
  expect(projectKeys(undefined, [row("a"), row("b", { project: "OPS" })])).toEqual(["ENG", "OPS"]);
  expect(projectKeys([], [row("a")])).toEqual(["ENG"]);
});

// ---------------------------------------------------------------------------
// The calendar
// ---------------------------------------------------------------------------

// MONDAY FIRST, because `internal/tracker/dates.go` pins the week's start
// there: a saved view carrying `sow`/`eow` means Monday, and a grid that
// started on Sunday would draw "this week" across two of its own rows.
test("a month's grid starts on the Monday on or before the first", () => {
  // 1 April 2031 is a Tuesday, so the grid opens on 31 March.
  const weeks = calendarWeeks("2031-04", "");
  expect(weeks[0]?.[0]?.key).toBe("2031-03-31");
  expect(weeks[0]?.[0]?.inMonth).toBe(false);
  expect(weeks[0]?.[1]?.key).toBe("2031-04-01");
  expect(weeks[0]?.[1]?.inMonth).toBe(true);
  expect(weeks.every((w) => w.length === 7)).toBe(true);
});

// FIVE ROWS OR SIX, as the month needs. A fixed six leaves an empty row on
// most months, and an empty row of a calendar reads as a week in which nothing
// is due rather than as a week that is not there.
test("a month takes as many rows as it needs and no more", () => {
  // February 2027 starts on a Monday and has 28 days: exactly four rows.
  expect(calendarWeeks("2027-02", "")).toHaveLength(4);
  // March 2031 starts on a Saturday and has 31 days: six.
  expect(calendarWeeks("2031-03", "")).toHaveLength(6);
});

test("exactly one cell is today, and only when it is in the grid", () => {
  const weeks = calendarWeeks("2031-04", "2031-04-16");
  const marked = weeks.flat().filter((c) => c.today);
  expect(marked).toHaveLength(1);
  expect(marked[0]?.day).toBe(16);
  expect(
    calendarWeeks("2031-04", "2029-01-01")
      .flat()
      .some((c) => c.today),
  ).toBe(false);
});

// THE RANGE IS HALF-OPEN AND COVERS THE GRID, not the month: the first and
// last rows carry days of the neighbouring months, and a query bounded by the
// month would draw those cells permanently empty.
test("the grid's range runs from its first cell to the day after its last", () => {
  const weeks = calendarWeeks("2031-04", "");
  const range = gridRange(weeks);
  expect(range.from).toBe("2031-03-31");
  const last = weeks[weeks.length - 1]?.[6]?.key;
  expect(last).toBe("2031-05-04");
  expect(range.to).toBe("2031-05-05");
});

test("a month shifts across a year boundary in both directions", () => {
  expect(shiftMonth("2031-12", 1)).toBe("2032-01");
  expect(shiftMonth("2031-01", -1)).toBe("2030-12");
});

// A DUE DATE IS BUCKETED IN THE READER'S OWN DAY. The instant arrives in UTC
// and a task due at half past eleven at night is due that night to the person
// reading it — expressed against `Date` rather than a literal, so the case
// holds in whatever zone the test runs in.
test("an instant is bucketed on the day it falls on locally", () => {
  const at = new Date(2031, 3, 16, 23, 30, 0);
  expect(dayKey(at.toISOString())).toBe("2031-04-16");
  expect(dayKey(undefined)).toBe("");
  expect(dayKey("not a date")).toBe("");
});

// ROWS WITHOUT A DUE DATE ARE DROPPED rather than bucketed under today:
// putting undated work on the current day invents a deadline nobody set.
test("only dated rows reach a day, ordered by when they are due", () => {
  const due = (iso: string) => new Date(iso).toISOString();
  const buckets = bucketByDay([
    row("late", { due: due("2031-04-16T18:00:00Z") }),
    row("early", { due: due("2031-04-16T06:00:00Z") }),
    row("undated"),
  ]);
  const key = dayKey(due("2031-04-16T06:00:00Z"));
  expect(buckets.get(key)?.map((r) => r.id)).toEqual(["early", "late"]);
  expect([...buckets.values()].flat()).toHaveLength(2);
});

test("the current month comes from the reader's own clock", () => {
  const now = new Date(2031, 6, 4, 12, 0, 0);
  expect(monthOf(now.getTime())).toBe("2031-07");
});

// A COUNT SAID TWICE IS A COUNT THAT READS AS TWO FACTS. The hint exists to
// say there is MORE than what is on screen; when there is not, the row count
// has already answered and "15 items of 15 matching" just asks the reader to
// compare two numbers that can never differ.
test("the total hint is silent when it repeats what is on screen", () => {
  expect(totalHint(15, 15)).toBe("");
  expect(totalHint(9, 15)).toBe("");
  expect(totalHint(0, 0)).toBe("");
  expect(totalHint(240, 50)).toBe("of 240 matching");
});

// THE CAP IS THE ENGINE'S, and it is reported whatever the shown count is:
// "10000+" is a statement about how far the engine counted, which no number
// of rows on screen can imply.
test("a capped count says so even when every row is on screen", () => {
  expect(totalHint(10000, 10000, true)).toBe("of 10000+ matching");
});

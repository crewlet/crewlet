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
// THE ONE MARK, from the design system rather than re-spelled here: a test
// carrying its own copy of the glyph goes green on whatever is written.
import { EMPTY_VALUE } from "@crewlethq/ui";
import {
  TYPE_ICON,
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
  seededScope,
  shapeOf,
  shiftMonth,
  shownRows,
  statusLabel,
  typeIcon,
  typeName,
  type LabelContext,
  countedLabel,
  dayLabel,
  monthOrNow,
  pageCount,
  pageNote,
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
//
// THE NAMES ARE THE DESIGN SYSTEM'S. They read as Material Symbols rather than
// as words we chose, which is the point: one vocabulary means the mark a table
// asks for and the mark `~/ui/glyph.tsx` draws cannot be two different ideas
// spelled the same. `bug_report` is one of the four this build holds because
// `@crewlethq/icons` has not vendored it — see `src/ui/symbols/README.md`.
test("every shipped type has its own icon and an unknown one falls back", () => {
  expect(typeIcon("bug")).toBe("bug_report");
  expect(typeIcon("milestone")).toBe("flag");
  expect(typeIcon("incident")).toBe("package_2");
  expect(typeIcon(undefined)).toBe("package_2");
});

// AND NO TWO TYPES SHARE A MARK, which is the property the table exists for
// and the one a rename can quietly break: every substitution in the port was a
// choice between drawings, so two types landing on one glyph would read as one
// type with nothing in the build to say otherwise.
test("no two work types draw the same mark", () => {
  const marks = Object.values(TYPE_ICON);
  expect([...new Set(marks)].sort()).toEqual([...marks].sort());
});

test("a type takes the company's own name for it", () => {
  expect(typeName("bug")).toBe("Bug");
  expect(typeName("bug", [{ slug: "bug", name: "Defect" }])).toBe("Defect");
});

// AN UNESTIMATED TASK IS A DASH, never "0m": zero is a measurement and an
// unsized task is one nobody has sized, which is the difference a sprint
// report spends a whole column on.
test("an absent estimate is a dash and a present one reads in hours", () => {
  expect(fmtMinutes(undefined)).toBe(EMPTY_VALUE);
  expect(fmtMinutes(0)).toBe(EMPTY_VALUE);
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
  expect(
    describeChange(record({ fields: { status: { from: "todo", to: "in_progress" } } }), {}),
  ).toBe("Status: To do → In progress");
  expect(describeChange(record({ kind: "comment", excerpt: "rolled back" }), {})).toBe(
    "rolled back",
  );
  expect(describeChange(record({ kind: "comment_resolved" }), {})).toBe("comment resolved");
});

// AN EMPTY SIDE IS A DASH on either end: "Assignee:  → ada" reads as a
// rendering bug, where `Assignee: ${EMPTY_VALUE} → ada` reads as an assignment.
test("an empty side of a feed delta renders as a dash", () => {
  expect(describeChange(record({ fields: { assignee: { from: "", to: "ada" } } }), {})).toBe(
    `Assignee: ${EMPTY_VALUE} → ada`,
  );
  expect(describeChange(record({ fields: { assignee: { from: "ada", to: "" } } }), {})).toBe(
    `Assignee: ada → ${EMPTY_VALUE}`,
  );
});

// AN EXCERPT IS A CUT OF A MARKDOWN BODY, and this string is drawn in a one-line
// cell. Returned raw it printed the source: "## Understanding the work This task
// is to interview…", hashes and all.
test("a change's excerpt is the prose it renders to, not its markdown", () => {
  expect(
    describeChange(
      record({
        kind: "created",
        excerpt: "## Understanding the work\n\nInterview three **desks**.",
      }),
      {},
    ),
  ).toBe("Understanding the work Interview three desks.");
  expect(
    describeChange(record({ kind: "comment", excerpt: "see [the guide](https://x.example)" }), {}),
  ).toBe("see the guide");
});

// AND A HISTORY ENTRY DOES NOT QUOTE THE THREAD AT ALL. The excerpt of a comment
// IS the comment body, so the History tab printed the Thread tab back one
// clipped line at a time and the row never said what the change WAS. The feed
// above keeps its excerpt rung — a cross-item feed has no thread beside it — and
// this one names the kind instead.
test("a history entry says what the change was, and never quotes it", () => {
  expect(describeHistory(change({ kind: "comment", excerpt: "shipped it" }), {})).toBe("commented");
  expect(describeHistory(change({ kind: "watchers" }), {})).toBe("changed the watchers");
  // A KIND THIS BUILD HAS NEVER HEARD OF still renders as itself: a rolling
  // upgrade puts a newer peer's kinds on the wire.
  expect(describeHistory(change({ kind: "watcher_added" }), {})).toBe("watcher added");
});

// A HISTORY LINE SPEAKS THE READER'S LANGUAGE, NOT THE LOG'S.
//
// The engine stores `in_review`, a handle and a whole RFC3339 instant, while the
// same page's header chip says "In review", its assignee chip says the seat's
// name and its Due row says "Sep 19, 2026". The history was the one surface in
// the product printing the stored form, six inches under the rail printing the
// other.
test("a history entry renders every field in the reader's own vocabulary", () => {
  const ctx: LabelContext = {
    seatName: (h) => (h === "frontend-engineer" ? "Frontend Engineer" : h),
  };
  expect(
    describeHistory(
      change({
        fields: {
          status: { from: "todo", to: "in_review" },
          assignee: { from: "", to: "frontend-engineer" },
          due: { from: "", to: "2026-09-19T00:00:00Z" },
        },
      }),
      ctx,
    ),
  ).toBe(
    `Status: To do → In review, Assignee: ${EMPTY_VALUE} → Frontend Engineer, ` +
      `Due: ${EMPTY_VALUE} → Sep 19, 2026`,
  );
});

// AND A DELTA NEVER RENDERS AS NO CHANGE. `internal/tracker/wake.go` compares
// the WHOLE instant precisely so that moving a due time inside one day is a
// change at all; printed as a day, both sides read "Sep 19, 2026" and the line
// claims a field moved to where it already was.
test("a due-time move inside one day renders the time on both sides", () => {
  const said = describeHistory(
    change({ fields: { due: { from: "2026-09-19T07:00:00Z", to: "2026-09-19T15:00:00Z" } } }),
    {},
  );
  expect(said).toContain("09:00");
  expect(said).toContain("17:00");
});

test("a delta takes the project's own label and the project's own tag name", () => {
  expect(
    describeHistory(change({ fields: { status: { from: "todo", to: "done" } } }), {
      statuses: [{ status: "done", label: "Shipped", group: "done", description: "" }],
    }),
  ).toBe("Status: To do → Shipped");
  expect(
    describeHistory(change({ fields: { tags: { from: "", to: "p1" } } }), {
      tags: [{ slug: "p1", label: "Priority one" }],
    }),
  ).toBe(`Tags: ${EMPTY_VALUE} → Priority one`);
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
  expect(describeHistory(change({ fields: { status: { from: "todo", to: "done" } } }), {})).toBe(
    "Status: To do → Done",
  );
  expect(
    describeHistory(change({ kind: "comment", fields: { mentions: ["ada", "bo"] } }), {}),
  ).toBe("Mentions: ada, bo");
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
  expect(fieldValueText(value({ id: "f-eff", type: "number", value: null }), defs)).toBe(
    EMPTY_VALUE,
  );
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

// THE SCREEN'S OWN VOCABULARY NEVER REACHES THE WIRE. `scope` and `overdue`
// are words this dashboard uses for two of its controls, and the engine's
// grammar has neither — it asks for a status GROUP and for `due=overdue`.
// The engine REFUSES a parameter it does not read rather than ignoring one,
// which is right and which makes a leaked key fail the WHOLE read: the
// answer is then empty, indistinguishable from an empty container. That is
// exactly how an item page's subtask panel came to draw nothing at all.
test("a control's own name is translated rather than sent", () => {
  const params = build({
    filters: { ...NO_FILTERS, scope: "closed", overdue: true },
  });
  expect(Object.keys(params)).not.toContain("scope");
  expect(Object.keys(params)).not.toContain("overdue");
  // The questions are still asked, in the engine's own words.
  expect(params.status_group).toBe("done,closed");
  expect(params.due).toBe("overdue");
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

// AND TWO OF THE THREE HAVE TO SAY `show_closed`, because a status group
// alone cannot widen the answer: `internal/tracker/read.go` ANDs an
// unconditional `t.status_group IN ('not_started','active')` unless the key
// is set. So `done,closed` without it is the intersection of two disjoint
// halves — Closed returned NOTHING on every company, and All returned
// exactly what Open did, with no error and no empty state to say so.
//
// Asserted here rather than on a screen because it is not a rendering
// question: both segments drew a perfectly ordinary empty board.
test("the segments that include finished work say so on the wire", () => {
  expect(build({ filters: { ...NO_FILTERS, scope: "closed" } }).show_closed).toBe("true");
  expect(build({ filters: { ...NO_FILTERS, scope: "" } }).show_closed).toBe("true");
  // Open is the one that must NOT: the group already excludes finished work,
  // and a key contradicting the segment a reader picked is worse than absent.
  expect(build({ filters: { ...NO_FILTERS, scope: "open" } }).show_closed).toBeUndefined();
});

// AND IT IS A FLOOR, NOT AN OVERRIDE. A saved view carrying `show_closed`
// states a NARROWER window inside the same segment — `recent:168h` is
// "finished in the last week" — and the segment is derived from that same
// view's `status_group`, so writing `true` over it would turn the view its
// author saved into "everything ever closed" without anybody touching the
// control. Measured on a running engine: under `done,closed`, `true` answers
// 1 row and `recent:1h` answers 0, so the difference is real rather than
// theoretical.
test("a view's own show_closed survives the segment on screen", () => {
  const view = { show_closed: "recent:168h" };
  expect(build({ view, filters: { ...NO_FILTERS, scope: "closed" } }).show_closed).toBe(
    "recent:168h",
  );
  expect(build({ view, filters: { ...NO_FILTERS, scope: "" } }).show_closed).toBe("recent:168h");
  // Untouched under `open` too, where every value of the key is inert because
  // the group already excludes everything it could admit.
  expect(build({ view, filters: { ...NO_FILTERS, scope: "open" } }).show_closed).toBe(
    "recent:168h",
  );
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

// AND THE ROUND TRIP IS WHAT THE SEGMENT IS SEEDED FROM.
//
// The tracker reads a view's `status_group` back through [scopeOf] to decide
// which segment the control shows, and [buildItemsParams] then writes that
// segment BACK onto `status_group`, overwriting whatever the view spread in.
// So the two have to be inverses over the groups a view can carry: anywhere
// they are not, a saved view is answered with a filter nobody saved, and the
// segment names a scope the rows do not match.
//
// Seeded from the view the pair is a round trip; seeded from a constant it
// was not, which is the whole of the defect — a view saved over closed work
// opened on the default segment and was answered as OPEN work.
test("a view's own status group survives being read into the segment and back", () => {
  for (const group of ["not_started,active", "done,closed"]) {
    const params = build({
      view: { status_group: group },
      filters: { ...NO_FILTERS, scope: scopeOf(group) },
    });
    expect(params.status_group).toBe(group);
  }
});

// A VIEW WITH NO GROUP LEAVES THE SEGMENT AT ITS OWN DEFAULT, which is `open`
// — the tracker opens on unfinished work, and a view that says nothing about
// status is not a view asking for everything.
//
// OVER THE SEEDING ITSELF, because this used to compute `scopeOf(undefined) ||
// "open"` inside the assertion: it held for any falsy answer and equally for
// one that returned "open" outright, and the `||` that actually decides it sat
// in the screen where no test reached it. Deleting it there left this suite
// green and opened the tracker on every closed task the company has.
test("a view naming no status group is seeded open rather than empty", () => {
  expect(seededScope(undefined)).toBe("open");
  expect(seededScope({})).toBe("open");
  expect(seededScope({ status_group: "" })).toBe("open");
  // And a group the segments DO express seeds itself, which is the round trip.
  expect(seededScope({ status_group: "not_started,active" })).toBe("open");
  expect(seededScope({ status_group: "done,closed" })).toBe("closed");
});

// A VIEW THAT WIDENED TO FINISHED WORK ITSELF OPENS ON THE WIDE SEGMENT.
//
// `open` is the seed for a view that says NOTHING about status. A view setting
// `show_closed` has said something: it asked for finished work explicitly, and
// seeding `open` over it writes `status_group=not_started,active`, which
// excludes exactly the half it widened to. Two real views were in that shape —
// "and whatever finished this week" answered as open work alone, and the
// builtin TRASH tab answered as the removed tasks that were still open,
// silently hiding every removal of anything already done, which is the one
// thing that tab exists to show.
//
// THROUGH THE QUERY, not just the segment, because the segment is only half
// the round trip: seeded `open`, `buildItemsParams` writes the narrow group
// back over the view and the widening is lost there rather than here.
test("a view that asked for finished work is not re-narrowed to open", () => {
  for (const widened of ["true", "recent:168h"]) {
    const view = { removed: "true", show_closed: widened };
    const scope = seededScope(view);
    expect(scope, widened).toBe("");
    const params = build({ view, filters: { ...NO_FILTERS, scope } });
    expect(params.status_group, widened).toBeUndefined();
    // AND THE VIEW'S OWN VALUE STANDS: the empty segment supplies the key
    // only where nothing else did, so `recent:168h` is not overwritten with
    // "everything ever closed".
    expect(params.show_closed, widened).toBe(widened);
  }
  // `false` is an author EXCLUDING finished work on purpose, which is not a
  // widening: seeding the empty segment there would label the screen "All"
  // over a query that shows less.
  expect(seededScope({ show_closed: "false" })).toBe("open");
  // And a view that names a group is decided by the group, whatever else it
  // carries — the three segments can express it, so the round trip holds.
  expect(seededScope({ status_group: "done,closed", show_closed: "true" })).toBe("closed");
});

// AND A GROUP THE SEGMENTS CANNOT EXPRESS STILL OPENS ON UNFINISHED WORK,
// which is the one case where the round trip cannot preserve the view: three
// segments cannot name a fourth set, and [buildItemsParams] overwrites
// `status_group` from the segment whichever one it is. So the only question is
// which wider set — `open` adds the rest of open work, the empty segment would
// add every closed task too — and what matters is that the control and the
// query AGREE about the one on screen.
//
// This fed the query `scope: ""` and named the value `seeded`, a state the
// screen produces for no group at all: it certified a reading the product does
// not have, over an input only a deliberate click on All can reach.
test("a group outside the three segments opens wider, and says which", () => {
  expect(scopeOf("active")).toBe("");
  const scope = seededScope({ status_group: "active" });
  expect(scope).toBe("open");
  const params = build({
    view: { status_group: "active" },
    filters: { ...NO_FILTERS, scope },
  });
  expect(params.status_group).toBe("not_started,active");
  // The segment says open work, and the query does not quietly add closed.
  expect(params.show_closed).toBeUndefined();
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

// ---------------------------------------------------------------------------
// What a count is a count OF
// ---------------------------------------------------------------------------

// FOUR SHAPES DRAW THIS INDICATOR over the same question and the calendar does
// not: its fetch is bounded to the grid's days, so under identical filters it
// counted six where the list counted seventeen, in the same corner of the same
// bar in the same words. `total_hint` cannot cover it — the engine counts it
// over the same predicate as the rows, so it equals the row count and
// `totalHint` correctly falls silent.
test("a count over a windowed question says which window it counted", () => {
  expect(countedLabel(17, {})).toBe("17 items");
  expect(countedLabel(1, {})).toBe("1 item");
  // THE ROUND TRIP, so the two sides cannot drift: the sentence is derived from
  // the params that were actually sent.
  const windowed = buildItemsParams({
    container: "project:ENG",
    shape: "calendar",
    view: {},
    filters: NO_FILTERS,
    range: { from: "2031-03-31", to: "2031-05-05" },
  });
  expect(countedLabel(6, windowed)).toBe("6 items due in this window");
});

// THE GRAMMAR HAS ONE `due` KEY, and the calendar's own axis is already spending
// it — so the Overdue chip could be pressed and narrow nothing at all.
test("the calendar's window wins the one due key, and never leaves it unset", () => {
  const range = { from: "2031-03-31", to: "2031-05-05" };
  const pressed = buildItemsParams({
    container: "project:ENG",
    shape: "calendar",
    view: {},
    filters: { ...NO_FILTERS, overdue: true },
    range,
  });
  expect(pressed.due).toBe("range:2031-03-31..2031-05-05");
  // AND NO WINDOW MEANS NO CALENDAR QUESTION: `sort=due` over the whole backlog
  // returns the five hundred soonest-due tasks in the company, which the grid
  // cannot draw and the toolbar would count as if the reader had asked for them.
  const unwindowed = buildItemsParams({
    container: "project:ENG",
    shape: "calendar",
    view: {},
    filters: { ...NO_FILTERS, overdue: true },
  });
  expect(unwindowed.due).toBeUndefined();
});

// A MONTH THE ADDRESS BAR MANGLED FALLS BACK TO THE READER'S OWN. `calendarWeeks`
// over an unparseable one returns NO WEEKS: `Number` gives `NaN`, `??` does not
// catch `NaN`, the cell count is `NaN` and the loop never runs — which takes
// `buildItemsParams` down the calendar branch with no range at all.
test("a month the address bar mangled falls back to the reader's own", () => {
  const now = Date.parse("2031-04-16T12:00:00Z");
  expect(monthOrNow("2031-04", now)).toBe("2031-04");
  expect(monthOrNow("oops", now)).toBe(monthOf(now));
  expect(monthOrNow("2031-13", now)).toBe(monthOf(now));
  expect(monthOrNow("", now)).toBe(monthOf(now));
  // WHY the guard exists, rather than tidiness.
  expect(calendarWeeks("oops", "")).toHaveLength(0);
});

// A CELL'S DATE IS SAID IN FULL, because the grid's own answer to "which month
// is this" is a colour. Asserted without naming a month: the locale is the
// reader's and a literal would pin the suite to the runner's.
test("a day's label carries more than the numeral, and separates repeated ones", () => {
  expect(dayLabel("2031-03-31")).toContain("31");
  expect(dayLabel("2031-03-31")).not.toBe("31");
  // April 2031's grid holds 1 April and 1 May. Same numeral, two cells.
  expect(dayLabel("2031-04-01")).not.toBe(dayLabel("2031-05-01"));
  // And it is the browser's calendar, like the cells it labels — never a UTC
  // instant, which shifts a day west of Greenwich.
  expect(dayLabel("2031-03-31")).toBe(
    new Date(2031, 2, 31).toLocaleDateString(undefined, {
      weekday: "long",
      day: "numeric",
      month: "long",
      year: "numeric",
    }),
  );
});

// A COUNT OVER ONE PAGE SAYS WHETHER THE PAGE IS THE WHOLE OF IT. The card draws
// this string verbatim, so the `+` is the entire claim.
test("a count over one page says whether the page is the whole of it", () => {
  expect(pageCount(3, false)).toBe("3");
  expect(pageCount(20, true)).toBe("20+");
});

test("a page that is the whole set says nothing at all", () => {
  // Not "and that is all of them": a note that always drew is a note nobody
  // reads by the time it matters.
  expect(pageNote(3, false, "change")).toBe("");
});

test("a capped page names what it is the newest of, agreeing with its noun", () => {
  expect(pageNote(20, true, "change")).toBe("The newest 20 changes; there are more.");
  expect(pageNote(1, true, "change")).toBe("The newest 1 change; there are more.");
});

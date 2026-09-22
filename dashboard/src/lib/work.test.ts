/**
 * The tracker's derivations, and the wrong form each one has.
 *
 * Every case below is a rendering whose wrong shape reads as a DIFFERENT FACT
 * rather than as a missing one — a column headed with a blank instead of
 * "Unassigned", a filter that silently widens to the whole company, a
 * calendar that puts a task on the wrong side of midnight. None of them is
 * visible in a diff and none of them throws.
 */

import { expect, test } from "vitest";
// THE ONE MARK, from the design system rather than re-spelled here: a test
// carrying its own copy of the glyph goes green on whatever is written.
import { EMPTY_VALUE } from "@crewlethq/ui";
import {
  SCOPES,
  STATUSES,
  TYPE_ICON,
  totalHint,
  anyFilter,
  axisLabel,
  bandsByDue,
  dueBucket,
  filterChips,
  bucketByDay,
  buildItemsParams,
  calendarWeeks,
  dayKey,
  defaultView,
  describeChange,
  endNote,
  LANDING_SHAPE,
  padGroups,
  SCOPE_GROUPS,
  STATUS_GROUPS,
  effectiveArrangement,
  EXPLICIT_NONE,
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
  type Scope,
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
// unsized task is one nobody has sized, and those are different facts.
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
// empty tag is "Untagged". A blank heading leaves a column of real work
// nobody can identify.
test("an empty group key is named for its own axis", () => {
  expect(groupLabel("assignee", group(""))).toBe("Unassigned");
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

test("a builtin view's shape is its own, and an unknown key draws the landing shape", () => {
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
  expect(shapeOf("board", [])).toBe(LANDING_SHAPE);
});

// A CONTAINER NOBODY HAS SAVED A DEFAULT FOR OPENS ON THE LIST. A board's
// information is the comparison across its lanes, so it is the worst shape at
// low N — one card 292px wide in a 1500px field — where a list degrades to one
// full-width row and keeps being a list. The board stays one press away.
test("the landing tab is the one the container marks, else the list", () => {
  expect(defaultView([view({ key: "list" }), view({ key: "mine", default: true })])).toBe("mine");
  expect(defaultView([view({ key: "board" })])).toBe("list");
  expect(LANDING_SHAPE).toBe("list");
});

const build = (over: Partial<Parameters<typeof buildItemsParams>[0]> = {}) =>
  buildItemsParams({
    container: "project:ENG",
    shape: "board",
    view: {},
    filters: NO_FILTERS,
    ...over,
  });

// THE SCREEN'S OWN VOCABULARY NEVER REACHES THE WIRE. `scope` is the word this
// dashboard uses for a control the engine's grammar does not have — it asks for
// a status GROUP.
// The engine REFUSES a parameter it does not read rather than ignoring one,
// which is right and which makes a leaked key fail the WHOLE read: the
// answer is then empty, indistinguishable from an empty container. That is
// exactly how an item page's subtask panel came to draw nothing at all.
test("a control's own name is translated rather than sent", () => {
  const params = build({
    filters: { ...NO_FILTERS, scope: "closed", due: "overdue" },
  });
  expect(Object.keys(params)).not.toContain("scope");
  // The questions are still asked, in the engine's own words.
  expect(params.status_group).toBe("done,closed");
  // AND `due` IS ALREADY THE GRAMMAR'S OWN KEY, which is why the Overdue switch
  // this screen used to carry is one VALUE of it rather than a second key: the
  // grammar has exactly one `due`, and the calendar's window spends the same
  // one.
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

// ---------------------------------------------------------------------------
// The arrangement: the reader's, the view's, or off
// ---------------------------------------------------------------------------

// AN ARRANGEMENT IS THREE-VALUED and an empty string is only two of them. The
// reader's own choice, the saved view's where they made none, and OFF — which
// `""` cannot say, because the router DELETES a key set to it and the view's
// own value is then handed straight back. So there is a word for off, and one
// resolver reads it for all three keys.
test("an arrangement is the reader's, then the view's, then nothing", () => {
  expect(effectiveArrangement("assignee", "status")).toBe("assignee");
  expect(effectiveArrangement("", "status")).toBe("status");
  expect(effectiveArrangement("", undefined)).toBe("");
  expect(effectiveArrangement(EXPLICIT_NONE, "status")).toBe("");
  // AND `none` NEVER REACHES THE WIRE: it is this dashboard's word for the
  // absence of a key, not an axis or a sort the engine has.
  expect(effectiveArrangement(EXPLICIT_NONE, undefined)).toBe("");
});

// A SAVED VIEW'S GROUPING IS IN FORCE UNTIL SOMEBODY TURNS IT OFF, and turning
// it off has to be SAID. `group_by=` deleted the override and left the view
// supplying the axis, so the one control for it could not do the one thing it
// offered.
test("a reader turns a view's own grouping off rather than deleting the key", () => {
  const inherited = build({ shape: "list", view: { group_by: "assignee" } });
  expect(inherited.group_by).toBe("assignee");

  const blank = build({ shape: "list", view: { group_by: "assignee" }, filters: NO_FILTERS });
  expect(blank.group_by).toBe("assignee");

  const off = build({
    shape: "list",
    view: { group_by: "assignee" },
    filters: { ...NO_FILTERS, groupBy: EXPLICIT_NONE },
  });
  expect(off.group_by).toBeUndefined();
  expect(off.group_limit).toBeUndefined();
});

test("the second grouping and the order are turned off the same way", () => {
  const view = { group_by: "status", group_by2: "priority", sort: "due" };

  const inherited = build({ shape: "list", view });
  expect(inherited.group_by2).toBe("priority");
  expect(inherited.sort).toBe("due");

  const off = build({
    shape: "list",
    view,
    filters: { ...NO_FILTERS, groupBy2: EXPLICIT_NONE, sort: EXPLICIT_NONE },
  });
  expect(off.group_by).toBe("status");
  expect(off.group_by2).toBeUndefined();
  expect(off.sort).toBeUndefined();
});

// A TIMELINE'S DEFAULT ORDER IS ITS DATE AXIS rather than the engine's, so an
// explicit "default order" over a view's own sort lands there and not on a
// deleted key — the bars would otherwise arrive in whatever order the engine
// prefers and zig-zag down the page.
test("a timeline's own default order is what an explicit none resolves to", () => {
  expect(build({ shape: "timeline", view: { sort: "due" } }).sort).toBe("due");
  expect(
    build({
      shape: "timeline",
      view: { sort: "due" },
      filters: { ...NO_FILTERS, sort: EXPLICIT_NONE },
    }).sort,
  ).toBe("start");
});

// A BOARD IS ALWAYS GROUPED — it is what a board IS — so turning a view's axis
// off lands on the status axis rather than on a board with no columns.
test("a board turned off its view's axis falls back to status", () => {
  expect(build({ shape: "board", view: { group_by: "assignee" } }).group_by).toBe("assignee");
  expect(
    build({
      shape: "board",
      view: { group_by: "assignee" },
      filters: { ...NO_FILTERS, groupBy: EXPLICIT_NONE },
    }).group_by,
  ).toBe("status");
});

test("the quick filters map onto the grammar's own keys", () => {
  expect(build({ filters: { ...NO_FILTERS, blocked: true } }).blocked).toBe(true);
  expect(build({ filters: { ...NO_FILTERS, due: "overdue" } }).due).toBe("overdue");
  // THE TRASH IS A FILTER, which is what the engine says it is — and it carries
  // `show_closed` with it, because a removed task is very often a finished one
  // and the group predicate is ANDed unconditionally otherwise.
  const trash = build({ filters: { ...NO_FILTERS, removed: true, scope: "all" } });
  expect(trash.removed).toBe("true");
  expect(trash.show_closed).toBe("true");
  // AND THE COMPANY'S OWN FIELDS GO THROUGH VERBATIM: the grammar for one is
  // the engine's, and a client that re-spelled it would be a second copy of a
  // table the engine refuses against.
  const fields = build({ filters: { ...NO_FILTERS, fields: { "f.area": "any:api,ui" } } });
  expect(fields["f.area"]).toBe("any:api,ui");
});

// `shape`, NEVER `view`. The two are different keys — a view is the saved query
// and the shape is how it is drawn — and writing `view=list` here threw away
// whichever saved view the reader was on, so following "52 more" out of a saved
// board landed them on the container's default filters.
test("a column's overflow lands on the list, narrowed to that column", () => {
  expect(filterPatchForGroup("assignee", "ada")).toEqual({
    shape: "list",
    group_by: "assignee",
    group: "ada",
  });
  // AND THE UNSET COLUMN'S KEY IS THE EMPTY STRING, which the patch carries as
  // a VALUE. Dropped, the "Unassigned" column's own "N more →" narrowed to
  // nothing and loaded the whole board — the one column whose overflow a lead
  // actually follows.
  expect(filterPatchForGroup("assignee", "").group).toBe("");
});

// THE NARROWING IS THREE-VALUED, and the middle value is the one the engine
// distinguishes with `Params.Has`: absent is the whole board, `""` is the
// column holding the rows with NO value on this axis, and anything else is that
// value. Spelled as one string, the empty column could not be asked for at all.
test("an absent column narrowing and an empty one are different questions", () => {
  const narrowed = (group: string | undefined) =>
    build({ shape: "list", view: {}, filters: { ...NO_FILTERS, groupBy: "assignee", group } });
  // ABSENT: no key on the wire, so the answer is every column.
  expect("group" in narrowed(undefined)).toBe(false);
  // PRESENT AND EMPTY: the key is sent, holding nothing, which is the
  // unassigned column.
  expect(narrowed("")).toMatchObject({ group: "" });
  expect("group" in narrowed("")).toBe(true);
  expect(narrowed("ada")).toMatchObject({ group: "ada" });
  // AND A VIEW'S OWN `group` STANDS where the reader asked for nothing — a
  // view is a set of defaults, and absence is what inherits them.
  expect(
    build({
      shape: "list",
      view: { group_by: "assignee", group: "ada" },
      filters: { ...NO_FILTERS, group: undefined },
    }),
  ).toMatchObject({ group: "ada" });
});

test("a clear control appears only once something is narrowing the rows", () => {
  expect(anyFilter(NO_FILTERS)).toBe(false);
  expect(anyFilter({ ...NO_FILTERS, scope: "all" })).toBe(true);
  expect(anyFilter({ ...NO_FILTERS, assignee: "ada" })).toBe(true);
  expect(anyFilter({ ...NO_FILTERS, removed: true })).toBe(true);
  expect(anyFilter({ ...NO_FILTERS, fields: { "f.area": "api" } })).toBe(true);
  // THE ARRANGEMENT IS NOT A NARROWING. Group by, then by and the order decide
  // how the same answer is DRAWN — they are the Display menu's — and counting
  // them here put a Clear control over a board nobody had filtered which, once
  // pressed, flattened the arrangement the reader had chosen and removed
  // nothing.
  expect(anyFilter({ ...NO_FILTERS, groupBy: "assignee" })).toBe(false);
  expect(anyFilter({ ...NO_FILTERS, groupBy2: "type" })).toBe(false);
  expect(anyFilter({ ...NO_FILTERS, sort: "due" })).toBe(false);
  // Narrowing a board to ONE of its columns narrows the whole query, totals
  // included, so that one stays — READ ON ITS PRESENCE, because the unset
  // column's own key is the empty string and a truth test called that no
  // narrowing at all.
  expect(anyFilter({ ...NO_FILTERS, group: "ada" })).toBe(true);
  expect(anyFilter({ ...NO_FILTERS, group: "" })).toBe(true);
  expect(anyFilter({ ...NO_FILTERS, group: undefined })).toBe(false);
});

test("a view's status groups map back onto the three segments", () => {
  expect(scopeOf("not_started,active")).toBe("open");
  expect(scopeOf("done,closed")).toBe("closed");
  // AND THE THIRD SEGMENT HAS A NAME. It was the empty string, and a scope is a
  // URL key: the router's own writer DELETES a key set to `""`, so choosing All
  // wrote nothing, the parameter read back as its fallback — `open` on almost
  // every container — and the segment snapped back on the next render. The one
  // segment whose whole job is to show finished work could not be selected.
  expect(scopeOf("active")).toBe("all");
  expect(scopeOf(undefined)).toBe("all");
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
    expect(scope, widened).toBe("all");
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
  expect(scopeOf("active")).toBe("all");
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

// A LIST THAT ENDED AND A LIST THAT WAS CUT OFF END THE SAME WAY without this:
// rows, then page ground. `totalHint` is silent once everything matching is on
// screen and a cursor is invisible, so the reader of a hundred rows cannot tell
// whether the hundred-and-first exists.
test("a complete list says it is complete and an incomplete one says nothing", () => {
  const cases: { note: string; want: string }[] = [
    // Complete: the hint counted the same set the rows came from.
    { note: endNote({ shown: 2, hint: 2, narrowed: false }), want: "That is all of it · 2 items" },
    { note: endNote({ shown: 1, hint: 1, narrowed: false }), want: "That is all of it · 1 item" },
    // A NARROWING CHANGES THE NOUN AND NOTHING ELSE. It never says how many the
    // filter hides: `total_hint` counts what MATCHED, over the same predicate
    // as the rows, so the unfiltered total is not in this answer at all.
    {
      note: endNote({ shown: 2, hint: 2, narrowed: true }),
      want: "That is all of it · 2 items match",
    },
    {
      note: endNote({ shown: 1, hint: 1, narrowed: true }),
      want: "That is all of it · 1 item matches",
    },
    // A page with a cursor is not the end of anything.
    { note: endNote({ shown: 100, hint: 100, cursor: "c1", narrowed: false }), want: "" },
    // More matches than rows: the count in the bar already says there is more.
    { note: endNote({ shown: 100, hint: 240, narrowed: false }), want: "" },
    // A COUNT THAT STOPPED AT THE CEILING is not a count that finished.
    { note: endNote({ shown: 100, hint: 100, capped: true, narrowed: false }), want: "" },
    // AND AN EMPTY LIST GETS THE EMPTY STATE, never "that is all of it" over
    // nothing at all.
    { note: endNote({ shown: 0, hint: 0, narrowed: false }), want: "" },
    { note: endNote({ shown: 0, hint: 0, narrowed: true }), want: "" },
  ];
  for (const c of cases) expect(c.note).toBe(c.want);
});

// THE GRAMMAR HAS ONE `due` KEY, and the calendar's own axis is already spending
// it — so a due filter set on another shape could survive into this one and
// narrow nothing at all, which is how a reader concludes their filter matched
// everything. The Filter menu does not offer one here for the same reason.
test("the calendar's window wins the one due key, and never leaves it unset", () => {
  const range = { from: "2031-03-31", to: "2031-05-05" };
  const pressed = buildItemsParams({
    container: "project:ENG",
    shape: "calendar",
    view: {},
    filters: { ...NO_FILTERS, due: "overdue" },
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
    filters: { ...NO_FILTERS, due: "overdue" },
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

// ---------------------------------------------------------------------------
// The chips
// ---------------------------------------------------------------------------

// A CHIP IS ONE URL KEY, which is what makes it removable without a table of
// removers beside the table of chips — the screen clears the key the chip
// names. Asserted over the PARAMS rather than over the words, because the
// words are a company's own and the keys are the grammar's.
test("every applied filter is one chip, naming the key it clears", () => {
  const chips = filterChips({
    ...NO_FILTERS,
    q: "auth",
    status: "in_progress",
    type: "bug",
    priority: "high",
    assignee: "ada",
    tag: "api",
    due: "overdue",
    blocked: true,
    removed: true,
    fields: { "f.area": "any:api,ui" },
  });
  expect(chips.map((c) => c.param)).toEqual([
    "q",
    "status",
    "type",
    "priority",
    "assignee",
    "tag",
    "due",
    "blocked",
    "removed",
    "f.area",
  ]);
});

// NOTHING IS ON, SO THERE IS NO ROW. An unfiltered list draws no chips at all,
// which is the whole difference from the bar of eleven controls this replaced:
// the two that were narrowing looked exactly like the nine that were not.
test("an unfiltered list has no chips", () => {
  expect(filterChips(NO_FILTERS)).toEqual([]);
  // AND THE SCOPE IS NEVER ONE. It is a three-valued switch that is always set
  // to something, drawn in the bar beside them: as a chip it would either be
  // permanently present or absent on its default, which hides the one segment
  // that decides whether finished work is on screen.
  expect(filterChips({ ...NO_FILTERS, scope: "all" })).toEqual([]);
});

// IN THE COMPANY'S OWN WORDS, resolved through the same context a column head
// uses — so a board narrowed to one column is headed and chipped with one word
// rather than with a name and a slug.
test("a chip says what the company calls the value", () => {
  const ctx: LabelContext = {
    statuses: [{ status: "in_progress", label: "Doing", group: "active", description: "" }],
    types: [{ slug: "bug", name: "Defect" }],
    tags: [{ slug: "api", label: "API" }],
    seatName: (handle) => (handle === "ada" ? "Ada Okonkwo" : handle),
    fields: [{ id: "f1", slug: "area", name: "Area", type: "labels" }],
  };
  const chips = filterChips(
    {
      ...NO_FILTERS,
      status: "in_progress",
      type: "bug",
      tag: "api",
      assignee: "ada",
      fields: { "f.area": "not_null" },
    },
    ctx,
  );
  const value = (param: string) => chips.find((c) => c.param === param)?.value;
  expect(value("status")).toBe("Doing");
  expect(value("type")).toBe("Defect");
  expect(value("tag")).toBe("API");
  expect(value("assignee")).toBe("Ada Okonkwo");
  // `null` AND `not_null` ARE QUESTIONS ABOUT THE ROW rather than about a
  // value, and printed raw they read as "nothing" and "anything".
  expect(chips.find((c) => c.param === "f.area")?.label).toBe("Area");
  expect(value("f.area")).toBe("set");
  expect(
    filterChips({ ...NO_FILTERS, fields: { "f.area": "null" } }, ctx).find(
      (c) => c.param === "f.area",
    )?.value,
  ).toBe("not set");
});

// UNASSIGNED IS A VALUE the grammar spells `none` — the one a lead opens a
// board to ask for — and printed raw it reads as a filter that failed to
// resolve somebody's name.
test("the unassigned filter is a word rather than the grammar's token", () => {
  expect(filterChips({ ...NO_FILTERS, assignee: "none" })[0]?.value).toBe("Unassigned");
});

// THE COLUMN A BOARD WAS NARROWED TO is named by its own AXIS, which is the
// same resolver the column head uses.
test("a board narrowed to one column is chipped by that column's axis", () => {
  const chip = filterChips(
    { ...NO_FILTERS, groupBy: "assignee", group: "ada" },
    { seatName: () => "Ada Okonkwo" },
  )[0];
  expect(chip?.label).toBe("Assignee");
  expect(chip?.value).toBe("Ada Okonkwo");
  // AND THE UNSET COLUMN IS NAMED BY ITS AXIS, not left unchipped: a board
  // narrowed to Unassigned is a narrowed board, and a chip is the only thing
  // that says so and the only way off it.
  const unset = filterChips({ ...NO_FILTERS, groupBy: "assignee", group: "" })[0];
  expect(unset?.label).toBe("Assignee");
  expect(unset?.value).toBe("Unassigned");
});

// ONE VALUE OF ONE AXIS, split out of `groupLabel` because two surfaces ask it
// and only one of them holds a group: a column head has the answer the engine
// returned, and a chip has nothing but the key out of the URL.
test("an axis names its own empty key", () => {
  expect(axisLabel("assignee", "")).toBe("Unassigned");
  expect(axisLabel("tag", "")).toBe("Untagged");
  expect(axisLabel("status", "")).toBe("No status");
});

// ---------------------------------------------------------------------------
// The declared lanes
// ---------------------------------------------------------------------------

/** What a padded answer looks like from the outside: the keys and the counts. */
const lanes = (groups: WorkGroup[]) => groups.map((g) => `${g.key}:${g.count}`);

// A BOARD IS THE WORKFLOW, NOT THE OCCUPIED PART OF IT. The engine's grouping
// is a plain GROUP BY — one entry per distinct value PRESENT — so a one-item
// company drew one lane in a 1500px field, and the comparison a board exists
// for had no denominator.
test("a closed-set axis draws every declared value the scope admits, in order", () => {
  const cases: {
    axis: string;
    scope: Scope;
    groups: WorkGroup[];
    want: string[];
  }[] = [
    // OPEN DRAWS NO DEAD DONE LANE: the query it describes cannot return one.
    {
      axis: "status",
      scope: "open",
      groups: [group("todo", { count: 1, rows: [row("1")] })],
      want: ["todo:1", "in_progress:0", "in_review:0"],
    },
    // AND CLOSED DRAWS THE THREE FINISHED ONES, which is the other half of the
    // same mapping — `done` and `cancelled` are both the `done` group.
    {
      axis: "status",
      scope: "closed",
      groups: [],
      want: ["done:0", "cancelled:0", "closed:0"],
    },
    { axis: "status", scope: "all", groups: [], want: STATUSES.map((s) => `${s.value}:0`) },
    // THE GROUP AXIS IS NARROWED BY THE SAME KEY it is drawn from.
    { axis: "status_group", scope: "open", groups: [], want: ["not_started:0", "active:0"] },
    { axis: "status_group", scope: "closed", groups: [], want: ["done:0", "closed:0"] },
    { axis: "status_group", scope: "all", groups: [], want: STATUS_GROUPS.map((g) => `${g}:0`) },
    // A PRIORITY IS ORTHOGONAL TO A STATUS, so no scope narrows it.
    {
      axis: "priority",
      scope: "open",
      groups: [group("urgent", { count: 3 })],
      want: ["none:0", "low:0", "normal:0", "high:0", "urgent:3"],
    },
  ];
  for (const c of cases) {
    expect(lanes(padGroups({ axis: c.axis, groups: c.groups, scope: c.scope })), c.axis).toEqual(
      c.want,
    );
  }
});

// THE ENGINE'S OWN ANSWER SURVIVES THE MERGE. A pad that rebuilt a lane would
// draw a heading over rows it had thrown away — the one thing this must never
// do — so a declared value the answer carries is the answer's own object.
test("padding keeps the engine's counts, rows and subgroups on the lanes it has", () => {
  const answered = group("in_progress", {
    count: 400,
    rows: [row("1"), row("2")],
    subgroups: [group("ada", { count: 2 })],
  });
  const padded = padGroups({ axis: "status", groups: [answered], scope: "open" });
  const found = padded.find((g) => g.key === "in_progress");
  expect(found).toBe(answered);
  expect(found?.count).toBe(400);
  expect(found?.rows.map((r) => r.key)).toEqual(["ENG-1", "ENG-2"]);
  expect(found?.subgroups?.length).toBe(1);
});

// A KEY THE DECLARATION DOES NOT NAME IS KEPT, at the end. The empty key is a
// real column ("No status"), and a newer peer may write a status this build has
// never heard of — dropping either would hide rows.
test("a lane the declaration does not name survives, after the declared ones", () => {
  const padded = padGroups({
    axis: "status",
    scope: "open",
    groups: [group("", { count: 2 }), group("triaging", { count: 5 })],
  });
  expect(lanes(padded)).toEqual(["todo:0", "in_progress:0", "in_review:0", ":2", "triaging:5"]);
});

// AN OPEN SET IS NOT A BOARD. A lane per possible assignee, tag, type or
// project is a column of every value the company could ever hold.
test("an open-set axis passes through untouched", () => {
  const answered = [group("ada", { count: 2 })];
  for (const axis of ["assignee", "tag", "type", "project", "unit", "parent", "f.severity"]) {
    expect(padGroups({ axis, groups: answered, scope: "open" }), axis).toBe(answered);
  }
});

// A `group=` NARROWING ASKED FOR ONE LANE AND GOT ONE. Padding it back to the
// declared set would redraw the columns the reader just narrowed away.
test("a board narrowed to one column stays one column", () => {
  const answered = [group("in_review", { count: 2 })];
  expect(padGroups({ axis: "status", groups: answered, scope: "open", group: "in_review" })).toBe(
    answered,
  );
  // INCLUDING THE UNSET ONE, which is read on the key's presence: `""` is a
  // column here and a truth test padded it back to the whole declared set,
  // redrawing exactly the columns the reader had narrowed away.
  const unset = [group("", { count: 2 })];
  expect(padGroups({ axis: "status", groups: unset, scope: "open", group: "" })).toBe(unset);
  expect(padGroups({ axis: "status", groups: unset, scope: "open" })).not.toBe(unset);
});

// THE CONTAINER'S OWN DECLARATION DECIDES THE SET where it has one, so the
// status→group mapping the scope narrowing turns on is the ENGINE's rather than
// this build's shipped copy.
test("a project's own status declaration is what the scope narrows", () => {
  const statuses = [
    { status: "todo" as const, label: "Backlog", group: "not_started", description: "" },
    { status: "in_progress" as const, label: "Doing", group: "active", description: "" },
    { status: "done" as const, label: "Shipped", group: "done", description: "" },
  ];
  expect(lanes(padGroups({ axis: "status", groups: [], scope: "open", statuses }))).toEqual([
    "todo:0",
    "in_progress:0",
  ]);
  expect(lanes(padGroups({ axis: "status", groups: [], scope: "closed", statuses }))).toEqual([
    "done:0",
  ]);
});

// ONE SPELLING OF THE SCOPE MAPPING, because three readers turn on it: the
// query builder writes the key, `scopeOf` reads it back off a saved view, and
// the padding narrows the lanes with it. Written twice it drifts silently — a
// segment whose group nothing reads back snaps the control to the wrong value.
test("the scope segment, the query and the lanes read one mapping", () => {
  for (const scope of SCOPES) {
    const params = buildItemsParams({
      container: "workspace",
      shape: "board",
      view: {},
      filters: { ...NO_FILTERS, scope },
    });
    expect(scopeOf(params.status_group as string | undefined), scope).toBe(scope);
    expect(params.status_group ?? "", scope).toBe(SCOPE_GROUPS[scope]);
  }
});

// ---------------------------------------------------------------------------
// The due bands
// ---------------------------------------------------------------------------

/** A row with just the fields a band is decided from. */
function due(fields: Partial<WorkSummary>): WorkSummary {
  return {
    id: fields.key ?? "x",
    key: fields.key ?? "ENG-1",
    project: "ENG",
    title: "t",
    type: "task",
    status: "todo",
    updated: "2026-03-10T00:00:00Z",
    version: 1,
    ...fields,
  } as WorkSummary;
}

// OVERDUE IS THE ROW'S OWN FLAG, never a comparison of ours: the engine derives
// it against the COMPANY's day start, and a browser re-deriving it from its own
// midnight is how one screen shows a task as overdue and another does not.
test("the overdue band is the engine's flag, not a date comparison", () => {
  const now = Date.parse("2026-03-10T12:00:00Z");
  // Dated in the FUTURE and flagged overdue is not a state the engine produces;
  // it is the test that the flag is what decides, rather than the date.
  expect(dueBucket(due({ due: "2026-03-20T00:00:00Z", overdue: true }), now)).toBe("overdue");
});

// WHICH IS WHY THERE IS A SIXTH BAND. `overdue` means open AND past its date,
// so a task finished late is past its date and not overdue — and calling it
// Overdue would be a false claim about work somebody delivered, while calling
// it Today would invent a date nobody set.
test("a past date that is not overdue is its own band", () => {
  const now = Date.parse("2026-03-10T12:00:00Z");
  expect(dueBucket(due({ due: "2026-03-01T00:00:00Z" }), now)).toBe("earlier");
});

test("a day with no date is not put on today", () => {
  const now = Date.parse("2026-03-10T12:00:00Z");
  expect(dueBucket(due({}), now)).toBe("none");
});

// THE WEEK IS THE COMPANY'S WEEK. `internal/tracker/dates.go` starts one on
// Monday and the calendar draws it the same way, so "this week" here is the
// week a saved view's `eow` means.
test("today, this week and later are cut on the company's own week", () => {
  // Tuesday 10 March 2026, local noon — built through the browser's own
  // calendar because that is what the bands are read in.
  const now = new Date(2026, 2, 10, 12, 0, 0).getTime();
  const on = (y: number, m: number, d: number) =>
    dueBucket(due({ due: new Date(y, m, d, 9, 0, 0).toISOString() }), now);
  expect(on(2026, 2, 10)).toBe("today");
  // Wednesday and Sunday are inside the same Monday-first week.
  expect(on(2026, 2, 11)).toBe("week");
  expect(on(2026, 2, 15)).toBe("week");
  // Monday is the next one.
  expect(on(2026, 2, 16)).toBe("later");
});

// EMPTY BANDS ARE DROPPED, in the order a day is read. A band per bucket
// whether or not anything is in it would put five headings over a person
// holding one task.
test("the bands are the ones that hold something, in the day's own order", () => {
  const now = new Date(2026, 2, 10, 12, 0, 0).getTime();
  const bands = bandsByDue(
    [
      due({ key: "A", due: new Date(2026, 2, 20, 9).toISOString() }),
      due({ key: "B", overdue: true, due: new Date(2026, 2, 2, 9).toISOString() }),
      due({ key: "C", due: new Date(2026, 2, 10, 9).toISOString() }),
      due({ key: "D" }),
    ],
    now,
  );
  expect(bands.map((b) => b.key)).toEqual(["overdue", "today", "later", "none"]);
  expect(bands.map((b) => b.rows.map((r) => r.key))).toEqual([["B"], ["C"], ["A"], ["D"]]);
});

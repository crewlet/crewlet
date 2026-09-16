/**
 * The tracker's vocabulary and arithmetic, over values.
 *
 * NO REACT AND NO DOM, for the reason `textindex`'s ranking and the tracker's
 * own coercion table are pure on the engine's side: a rule that can only be
 * exercised by rendering a screen is a rule nobody re-measures, and every
 * judgement here — what a status is called, which column a filter builds, which
 * day a due date lands on — has a wrong form that reads as a different fact
 * rather than as a missing one.
 *
 * It also exists because the screens now SHARE these answers. A board, a list,
 * a calendar, a peek panel, an item page, My work and the sprint report all
 * draw the same task, and the previous screen's helpers lived inside the one
 * file that happened to need them first — which is how the sprint report came
 * to import a change renderer from the board.
 */

import { browserDay, fmtDate, fmtDateTime, humanize, parseUTC } from "./format.ts";
import type { IconName } from "~/ui/Icon.tsx";
import type {
  WorkActivityRecord,
  WorkChange,
  WorkFieldDef,
  WorkFieldValue,
  WorkGroup,
  WorkProjectRow,
  WorkProjectTag,
  WorkStatus,
  WorkStatusDef,
  WorkSummary,
  WorkTypeDef,
  WorkView,
} from "~/protocol/index.ts";

export type Tone = "neutral" | "positive" | "caution" | "critical" | "info";

// ---------------------------------------------------------------------------
// The closed sets
// ---------------------------------------------------------------------------

/**
 * The tracker's SIX statuses, in the order a board draws them.
 *
 * `blocked` is deliberately not among them: a blocker is DATA carried beside
 * the status, because a task can be both in progress and blocked and a single
 * field cannot say so. `cancelled` is here and reads as "finished without
 * being delivered", which is the whole of what a close reason used to say.
 */
export const STATUSES: { value: WorkStatus; label: string; group: string }[] = [
  { value: "todo", label: "To do", group: "not_started" },
  { value: "in_progress", label: "In progress", group: "active" },
  { value: "in_review", label: "In review", group: "active" },
  { value: "done", label: "Done", group: "done" },
  { value: "cancelled", label: "Cancelled", group: "done" },
  { value: "closed", label: "Closed", group: "closed" },
];

/** A status is a STATE, which is the one thing colour is spent on here. */
export const STATUS_TONE: Record<string, Tone> = {
  todo: "neutral",
  in_progress: "info",
  in_review: "caution",
  done: "positive",
  cancelled: "neutral",
  closed: "neutral",
};

/**
 * What a status is called, preferring the PROJECT's own label.
 *
 * A company may rename what it calls each of the six, and a screen that
 * rendered the slug would be showing a word the team does not use — while a
 * screen that rendered only the project's would have nothing to say on a board
 * that spans projects. So: the project's label, then the shipped one, then the
 * slug itself, which is what a status this build has never heard of renders as
 * rather than vanishing.
 */
export function statusLabel(status: string, defs?: WorkStatusDef[]): string {
  const declared = defs?.find((d) => d.status === status);
  if (declared?.label) return declared.label;
  return STATUSES.find((s) => s.value === status)?.label ?? humanize(status);
}

export const PRIORITIES = ["none", "low", "normal", "high", "urgent"] as const;

/**
 * The icon a task type is drawn with.
 *
 * AN ICON RATHER THAN A HUE, which is the product's own rule: a type is
 * identity, and identity is carried by a name, a mark and a position. Seven
 * slugs ship with the engine; a company's own type falls through to the
 * neutral box rather than to a generated anything.
 */
export const TYPE_ICON: Record<string, IconName> = {
  task: "checkSquare",
  bug: "bug",
  epic: "zap",
  story: "book",
  spike: "compass",
  chore: "wrench",
  milestone: "flag",
};

export function typeIcon(slug: string | undefined): IconName {
  return TYPE_ICON[slug ?? ""] ?? "box";
}

/** What a type is called, preferring the company's own declaration. */
export function typeName(slug: string | undefined, types?: WorkTypeDef[]): string {
  if (!slug) return "";
  return types?.find((t) => t.slug === slug)?.name ?? humanize(slug);
}

/**
 * An estimate as a person reads it.
 *
 * AN EM DASH FOR AN ABSENT ONE, never "0m": zero is a measurement and an
 * unestimated task is one nobody has sized, which is the difference a sprint
 * report spends a whole column on.
 */
export function fmtMinutes(minutes: number | undefined): string {
  if (!minutes) return "—";
  const hours = Math.floor(minutes / 60);
  const rest = minutes % 60;
  if (hours === 0) return `${rest}m`;
  if (rest === 0) return `${hours}h`;
  return `${hours}h ${rest}m`;
}

// ---------------------------------------------------------------------------
// Grouping
// ---------------------------------------------------------------------------

export interface LabelContext {
  statuses?: WorkStatusDef[];
  types?: WorkTypeDef[];
  tags?: WorkProjectTag[];
  seatName?: (handle: string) => string;
}

/**
 * What a board column is headed, per axis.
 *
 * THE EMPTY KEY IS A REAL COLUMN on every axis — "nobody is assigned" is a
 * question a board answers rather than a row it hides — and it is the one the
 * server cannot label, because the label depends on what the axis MEANS: an
 * empty assignee is "Unassigned" and an empty sprint is "No sprint", and
 * rendering either as a blank heading leaves a column of work nobody can name.
 */
export function groupLabel(axis: string, group: WorkGroup, ctx: LabelContext = {}): string {
  const key = group.key;
  if (group.label) return group.label;
  switch (axis) {
    case "status":
      return key ? statusLabel(key, ctx.statuses) : "No status";
    case "status_group":
      return key ? humanize(key) : "No group";
    case "assignee":
      return key ? (ctx.seatName?.(key) ?? key) : "Unassigned";
    case "priority":
      return key ? humanize(key) : "No priority";
    case "type":
      return key ? typeName(key, ctx.types) : "No type";
    case "tag":
      return key ? (ctx.tags?.find((t) => t.slug === key)?.label ?? key) : "Untagged";
    case "sprint":
      return key ? `Sprint ${key}` : "No sprint";
    case "parent":
      return key || "No parent";
    default:
      return key || "—";
  }
}

/** The axes a board may be cut on, with what each is called in the picker. */
export const GROUP_AXES: { value: string; label: string; projectOnly?: boolean }[] = [
  { value: "status", label: "Status" },
  { value: "status_group", label: "Status group" },
  { value: "assignee", label: "Assignee" },
  { value: "priority", label: "Priority" },
  { value: "type", label: "Type" },
  { value: "tag", label: "Tag" },
  { value: "sprint", label: "Sprint", projectOnly: true },
];

/** The orderings a list may ask for. */
export const SORTS: { value: string; label: string }[] = [
  { value: "rank", label: "Manual order" },
  { value: "-updated", label: "Recently updated" },
  { value: "due", label: "Due soonest" },
  { value: "-priority", label: "Most important" },
  { value: "-created", label: "Newest" },
  { value: "title", label: "Title" },
];

// ---------------------------------------------------------------------------
// Rendering a change
// ---------------------------------------------------------------------------

/**
 * One commit in a sentence, for the activity feed.
 *
 * FROM THE DELTAS where there are any, because "todo → in_progress" is what
 * every reader of a change wants and the kind alone does not say it. The
 * excerpt is the fallback, and the kind is the last resort — a row with
 * neither is still a real commit, and rendering it blank would make it look
 * like a rendering bug.
 */
export function describeChange(record: WorkActivityRecord): string {
  const moved = Object.entries(record.fields ?? {});
  if (moved.length > 0) {
    return moved.map(([field, d]) => `${field}: ${d.from || "—"} → ${d.to || "—"}`).join(", ");
  }
  if (record.excerpt) return record.excerpt;
  return record.kind.replaceAll("_", " ");
}

/**
 * One entry of a task's own history, in a sentence.
 *
 * A DIFFERENT SHAPE FROM THE FEED'S, and that is not duplication: a history
 * entry's `fields` is the notification SNAPSHOT, whose values are sometimes a
 * from/to pair and sometimes the state the change produced — the applier
 * writes the deltas where it can compare two documents and the notification's
 * own fields where it cannot (a comment, a mention, an ask). A renderer that
 * assumed one shape printed `[object Object]` on the other.
 */
export function describeHistory(entry: WorkChange): string {
  const moved = Object.entries(entry.fields ?? {});
  const said: string[] = [];
  for (const [field, raw] of moved) {
    const delta = raw as { from?: unknown; to?: unknown } | null;
    if (delta && typeof delta === "object" && ("from" in delta || "to" in delta)) {
      said.push(`${field}: ${scalar(delta.from) || "—"} → ${scalar(delta.to) || "—"}`);
      continue;
    }
    const value = scalar(raw);
    said.push(value ? `${field}: ${value}` : field);
  }
  if (said.length > 0) return said.join(", ");
  if (entry.excerpt) return entry.excerpt;
  return entry.kind.replaceAll("_", " ");
}

/** One value of a snapshot as text. Objects and arrays are rare and shallow. */
function scalar(value: unknown): string {
  if (value === null || value === undefined) return "";
  if (Array.isArray(value)) return value.map(scalar).filter(Boolean).join(", ");
  if (typeof value === "object") return JSON.stringify(value);
  return String(value);
}

/**
 * A custom field's value, in the word a person reads.
 *
 * THE OPTION'S NAME RATHER THAN ITS ID. A choice field stores the option's id
 * — which is what lets a company rename an option without orphaning every task
 * that chose it — so a panel that rendered the stored value would print a uuid
 * under a field heading. The id is the fallback for an option the declaration
 * no longer carries, because a value nobody can explain is still a value
 * somebody set.
 */
export function fieldValueText(
  field: WorkFieldValue,
  defs: Map<string, WorkFieldDef>,
  seatName: (handle: string) => string = (h) => h,
): string {
  const value = field.value;
  if (value === null || value === undefined || value === "") return "—";
  const def = defs.get(field.id);
  const options = def?.config?.options ?? [];
  const named = (id: unknown) => options.find((o) => o.id === id)?.name ?? String(id);

  switch (field.type) {
    case "checkbox":
      return value === true ? "Yes" : "No";
    case "date":
      return def?.config?.time ? fmtDateTime(String(value)) : fmtDate(String(value));
    case "number":
    case "progress":
    case "rollup": {
      const unit = def?.config?.unit ? ` ${def.config.unit}` : "";
      return `${value}${unit}`;
    }
    case "dropdown":
      return named(value);
    case "labels":
      return Array.isArray(value) ? value.map(named).join(", ") : named(value);
    case "people":
      return Array.isArray(value)
        ? value.map((h) => seatName(String(h))).join(", ")
        : seatName(String(value));
    case "relationship":
      return Array.isArray(value) ? value.map(String).join(", ") : String(value);
    default:
      return typeof value === "object" ? JSON.stringify(value) : String(value);
  }
}

/** The one-word state a field value is in, or empty for an ordinary one. */
export function fieldValueState(field: WorkFieldValue): string {
  if (field.undeclared) return "undeclared";
  if (field.foreign) return "from another tracker";
  if (field.hidden) return "archived";
  return "";
}

// ---------------------------------------------------------------------------
// Views
// ---------------------------------------------------------------------------

export type Shape = "list" | "board" | "calendar" | "timeline";

/** The shape a view is drawn in. */
export function shapeOf(viewKey: string, views: WorkView[]): Shape {
  const view = views.find((v) => v.key === viewKey);
  if (view) return view.type;
  // A KEY NOTHING RESOLVES DRAWS THE BOARD rather than nothing: a strip that
  // has not arrived yet is the ordinary state of the first paint, and a body
  // that waited for it would flash empty on every navigation.
  return "board";
}

/**
 * The tab a container lands on.
 *
 * ONE ROW MAY CARRY `default` and the applier settles that in the same
 * transaction as the write, so there is never a second claim to fall back
 * from. Absent, the board: it is the shape that answers "what is moving",
 * which is the question somebody opening a tracker has.
 */
export function defaultView(views: WorkView[]): string {
  return views.find((v) => v.default)?.key ?? "board";
}

/** A view's saved query, or an empty set of defaults. */
export function viewParams(viewKey: string, views: WorkView[]): Record<string, string> {
  return views.find((v) => v.key === viewKey)?.params ?? {};
}

// ---------------------------------------------------------------------------
// The query a screen builds
// ---------------------------------------------------------------------------

export interface TrackerFilters {
  q: string;
  status: string;
  type: string;
  priority: string;
  assignee: string;
  sprint: string;
  /** `open` (the default), `closed`, or empty for everything. */
  scope: string;
  groupBy: string;
  group: string;
  sort: string;
  blocked: boolean;
  overdue: boolean;
}

export const NO_FILTERS: TrackerFilters = {
  q: "",
  status: "",
  type: "",
  priority: "",
  assignee: "",
  sprint: "",
  scope: "open",
  groupBy: "",
  group: "",
  sort: "",
  blocked: false,
  overdue: false,
};

/** Whether anything is narrowing the rows, so a Clear control can appear. */
export function anyFilter(f: TrackerFilters): boolean {
  return Boolean(
    f.q ||
    f.status ||
    f.type ||
    f.priority ||
    f.assignee ||
    f.sprint ||
    f.groupBy ||
    f.group ||
    f.sort ||
    f.blocked ||
    f.overdue ||
    f.scope !== "open",
  );
}

/**
 * The `work_items` parameters one screen state asks for.
 *
 * # A view is a set of DEFAULTS and every explicit key overrides it
 *
 * Which is what makes picking a different assignee on a saved board give you
 * that board with one key changed, rather than a board that silently stops
 * being the saved one. Storing the view's id and letting it win instead would
 * make the filter controls lie about what is on screen the moment somebody
 * touched one.
 *
 * # The four shapes ask four different questions of one grammar
 *
 * A board is `group_by` and no cursor — across a set of columns there is no
 * single order to be after. A list is a page with a sort. A calendar is a DATE
 * RANGE and no grouping at all, because its own axis is the month it is
 * drawing and a column inside a day is not a thing. A timeline is a big
 * unpaged page ordered by start: its axis is derived from the rows present, so
 * a second page would redraw the first one's window.
 */
export function buildItemsParams(args: {
  container: string;
  shape: Shape;
  view: Record<string, string>;
  filters: TrackerFilters;
  /** The calendar's own bounds, as `YYYY-MM-DD` — see [gridRange]. */
  range?: { from: string; to: string };
}): Record<string, unknown> {
  const { container, shape, view, filters, range } = args;
  const params: Record<string, unknown> = { ...view, container };
  const inProject = container.startsWith("project:");

  const set = (key: string, value: string) => {
    if (value) params[key] = value;
  };
  set("q", filters.q);
  set("status", filters.status);
  set("type", filters.type);
  set("priority", filters.priority);
  set("assignee", filters.assignee);

  // A SPRINT NAMES ONE OF A PROJECT'S OWN, so the engine refuses every value
  // but `none` at the workspace — dropping it here rather than sending a
  // query that comes back as a refusal the screen would have to explain.
  if (inProject || filters.sprint === "none") set("sprint", filters.sprint);
  else if (!inProject) delete params.sprint;

  // OPEN AND CLOSED ARE STATUS GROUPS, not a boolean: the four groups are
  // what every rule in the tracker is written at, and `done` and `closed` are
  // two of them rather than one negation. The third segment deletes the key,
  // including one a view brought with it.
  //
  // AND THE TWO SEGMENTS THAT INCLUDE FINISHED WORK NEED `show_closed`, because
  // the group alone cannot widen the answer. `internal/tracker/read.go` ANDs an
  // unconditional `t.status_group IN ('not_started','active')` unless this key
  // is set, so sending `done,closed` without it asks for the intersection of
  // two disjoint halves — the Closed segment returned nothing at all, on every
  // company, and All returned exactly what Open did. Measured against a seeded
  // company through a running engine: 0 rows and 14 where the answers are 1
  // and 15.
  //
  // IT IS A FLOOR, NOT AN OVERRIDE. A saved view may carry its own
  // `show_closed` — `recent:168h` is "finished in the last week" — and that is
  // a NARROWER window its author chose, inside the same segment. Writing
  // `true` over it turns their view into "everything ever closed" the moment
  // the segment is derived from their own `status_group`, which is a scope
  // nobody picked. So the segment supplies the key only where nothing else
  // did, and never deletes one.
  //
  // Which is also why `open` touches it at all: under `not_started,active`
  // every value of `show_closed` is inert, since each of the three arms in
  // that switch either adds nothing or adds a predicate the group already
  // implies. Measured, all three answer identically.
  if (filters.scope === "open") {
    params.status_group = "not_started,active";
  } else if (filters.scope === "closed") {
    params.status_group = "done,closed";
    if (!params.show_closed) params.show_closed = "true";
  } else {
    delete params.status_group;
    if (!params.show_closed) params.show_closed = "true";
  }

  if (filters.blocked) params.blocked = true;
  if (filters.overdue) params.due = "overdue";

  const axis = filters.groupBy || (typeof view.group_by === "string" ? view.group_by : "");

  if (shape === "board") {
    params.group_by = axis || "status";
    params.group_limit = 50;
    delete params.limit;
    delete params.cursor;
    delete params.group;
  } else if (shape === "calendar") {
    // THE CALENDAR HAS ITS OWN AXIS, so a grouping brought in by a view is
    // dropped rather than sent: a grouped answer replaces the rows with
    // columns, and a month drawn from columns has nothing in its cells.
    delete params.group_by;
    delete params.group;
    delete params.group_limit;
    if (range) params.due = `range:${range.from}..${range.to}`;
    params.limit = 500;
    params.sort = "due";
    return params;
  } else if (shape === "timeline") {
    // THE TIMELINE KEEPS ITS GROUPING, unlike the calendar: a band per
    // assignee or per status is one axis stacked on the other, which is the
    // arrangement the view is for — where a month drawn from columns has
    // nothing in its cells.
    if (axis) {
      params.group_by = axis;
      params.group_limit = 100;
      set("group", filters.group);
    } else {
      delete params.group_by;
      delete params.group;
    }
    // MORE ROWS THAN A LIST, because a bar is one line where a list row is
    // three or four and the window is derived from the rows present: a
    // timeline paged at a hundred would draw a different axis on every page.
    params.limit = 500;
    // AND IT ARRIVES IN THE AXIS'S OWN ORDER unless the reader asked
    // otherwise, so the bars descend rather than zig-zag. The builtin view
    // carries the same value; this is what holds when a saved view or a
    // filter change drops it.
    params.sort = filters.sort || "start";
    return params;
  } else {
    if (axis) {
      params.group_by = axis;
      params.group_limit = 100;
      set("group", filters.group);
    } else {
      delete params.group_by;
      delete params.group;
    }
    params.limit = 100;
  }

  // THE SORT IS OMITTED where nobody asked for one, so the engine's own
  // default applies — the manual order inside a project and the most recently
  // touched across the company, which are two different right answers and
  // neither is something a client should hard-code.
  if (filters.sort) params.sort = filters.sort;
  return params;
}

/** Where a board column's overflow lands: the same rows as a list. */
export function filterPatchForGroup(axis: string, key: string): Record<string, string> {
  return { view: "list", group_by: axis, group: key };
}

// ---------------------------------------------------------------------------
// Rows
// ---------------------------------------------------------------------------

/**
 * EVERY ROW ON SCREEN, grouped or flat.
 *
 * A grouped answer carries NO flat `items` by construction — `groups` replaces
 * them, because returning both would be the same rows twice — so everything
 * derived from `items` alone reported a fully populated board as empty: the
 * header read 0 shown, 0 in progress, 0 blocked, and the panel at the foot of
 * the screen drew "No work has been filed yet" underneath the board's own
 * columns.
 *
 * The top-level rows are enough and subgroup rows are deliberately NOT added:
 * a subgroup's rows are a slice of its own column's, so folding them in would
 * count the same task twice.
 */
export function shownRows(items: WorkSummary[], groups: WorkGroup[]): WorkSummary[] {
  if (groups.length === 0) return items;
  return groups.flatMap((group) => group.rows);
}

/**
 * The project filter's options: the COMPANY's own listing, falling back to
 * whatever is on the page.
 *
 * The listing is the honest set — a filter built from the rows can only offer
 * the projects that happen to be on them, so a board already narrowed to one
 * offered exactly one choice and no way back. The fallback is not decoration:
 * the listing is a separate poll, and a filter that empties while it is in
 * flight is a control that flickers every time the board is re-read.
 */
export function projectKeys(listed: WorkProjectRow[] | undefined, shown: WorkSummary[]): string[] {
  if (listed && listed.length > 0) return listed.map((p) => p.key);
  const keys = new Set<string>();
  for (const item of shown) keys.add(item.project);
  return [...keys].sort();
}

/** The engine's own count against what is on screen. */
export function totalHint(hint: number, shown: number, capped?: boolean): string {
  if (hint <= 0) return "";
  // THE ENGINE SAYS WHETHER IT COUNTED THAT FAR, and the threshold is not
  // repeated here: comparing the hint against a ceiling of our own was a
  // second copy of the engine's, wrong at exactly one value — a set of
  // exactly ten thousand is EXACT and read as "10000+".
  if (capped) return `of ${hint}+ matching`;
  // SILENT WHEN IT ADDS NOTHING. "15 items of 15 matching" states one
  // number twice; the hint exists to say there is MORE than what is on
  // screen, and when there is not, the row count already said it.
  if (hint <= shown) return "";
  return `of ${hint} matching`;
}

/** A view's `status_group` mapped back onto the three segments. */
export function scopeOf(group: string | undefined): string {
  if (group === "not_started,active") return "open";
  if (group === "done,closed") return "closed";
  return "";
}

/**
 * The segment a view OPENS on, which is the other half of that round trip.
 *
 * TWO DIFFERENT ABSENCES land on the same segment for different reasons. A
 * view naming no `status_group` at all is not a view asking for everything —
 * the tracker opens on unfinished work — so it seeds `open`. A view naming a
 * group the three segments cannot express (`active` alone) seeds `open` too,
 * because [buildItemsParams] overwrites `status_group` from the segment
 * whichever one it is: there is no reading that answers such a view as saved,
 * only a choice of which wider set to show. `open` adds the rest of open work;
 * the empty segment would add every closed task as well. Both are honest —
 * the control and the query agree about which set is on screen — and `open`
 * is the one nearer to what the view's author asked for.
 *
 * HERE RATHER THAN AT THE CALL SITE so it can be asserted over. Spelled
 * `scopeOf(group) || "open"` inside the screen, it was the one step of the
 * round trip no test could reach: deleting it left every suite green while the
 * tracker opened on every closed task the company has.
 */
export function seededScope(group: string | undefined): string {
  return scopeOf(group) || "open";
}

// ---------------------------------------------------------------------------
// The calendar
// ---------------------------------------------------------------------------

export interface CalendarCell {
  /** The local `YYYY-MM-DD` this cell is, which is also its row key. */
  key: string;
  day: number;
  inMonth: boolean;
  today: boolean;
}

/** The month an instant falls in, locally, as `YYYY-MM`. */
export function monthOf(now: number): string {
  return browserDay(new Date(now)).slice(0, 7);
}

export function shiftMonth(month: string, by: number): string {
  const [y, m] = month.split("-").map(Number);
  const at = new Date(y ?? 1970, (m ?? 1) - 1 + by, 1);
  return browserDay(at).slice(0, 7);
}

export function monthLabel(month: string): string {
  const [y, m] = month.split("-").map(Number);
  return new Date(y ?? 1970, (m ?? 1) - 1, 1).toLocaleDateString(undefined, {
    month: "long",
    year: "numeric",
  });
}

/** Monday first, because the engine's own relative week tokens start there. */
export const WEEKDAYS = ["Mon", "Tue", "Wed", "Thu", "Fri", "Sat", "Sun"];

/**
 * The month's grid, Monday first, with the neighbouring days that fill it.
 *
 * MONDAY because `internal/tracker/dates.go` pins the week's start there — the
 * relative tokens a saved view can carry (`sow`, `eow`) mean Monday, and a
 * calendar that started on Sunday would draw "this week" across two of its own
 * rows.
 *
 * Five rows or six, as the month needs. A fixed six leaves an empty row on
 * most months, and an empty row of a calendar reads as a week in which nothing
 * is due rather than as a week that is not there.
 */
export function calendarWeeks(month: string, todayKey: string): CalendarCell[][] {
  const [y, m] = month.split("-").map(Number);
  const year = y ?? 1970;
  const index = (m ?? 1) - 1;
  const first = new Date(year, index, 1);
  const lead = (first.getDay() + 6) % 7;
  const days = new Date(year, index + 1, 0).getDate();
  const cells = Math.ceil((lead + days) / 7) * 7;

  const weeks: CalendarCell[][] = [];
  for (let i = 0; i < cells; i++) {
    const at = new Date(year, index, 1 - lead + i);
    const key = browserDay(at);
    if (i % 7 === 0) weeks.push([]);
    weeks[weeks.length - 1]?.push({
      key,
      day: at.getDate(),
      inMonth: at.getMonth() === index,
      today: key === todayKey,
    });
  }
  return weeks;
}

/**
 * The half-open range the grid covers, as the date filter spells it.
 *
 * THE GRID'S OWN BOUNDS rather than the month's, and that is the whole point:
 * the first and last rows carry days of the neighbouring months, and a query
 * bounded by the month would draw those cells permanently empty. It also
 * absorbs the one offset nothing else can: the engine resolves a bare date in
 * the COMPANY's zone and the cells are bucketed in the READER's, so a due date
 * within a day of either edge is inside the asked-for range either way.
 */
export function gridRange(weeks: CalendarCell[][]): { from: string; to: string } {
  const first = weeks[0]?.[0]?.key ?? "";
  const lastRow = weeks[weeks.length - 1];
  const last = lastRow?.[lastRow.length - 1]?.key ?? "";
  const after = new Date(`${last}T00:00:00`);
  after.setDate(after.getDate() + 1);
  return { from: first, to: browserDay(after) };
}

/** The local `YYYY-MM-DD` an instant falls on, or empty when it is unreadable. */
export function dayKey(ts: string | undefined): string {
  const at = parseUTC(ts);
  return at ? browserDay(at) : "";
}

/**
 * The rows of each day, ordered the way a day is read.
 *
 * ROWS WITHOUT A DUE DATE ARE DROPPED rather than bucketed under today: a
 * calendar is about when work is due, and putting undated work on the current
 * day would invent a deadline nobody set. The screen says so beneath the grid,
 * because a reader who cannot see the omission reads the month as the whole
 * backlog.
 */
export function bucketByDay(rows: WorkSummary[]): Map<string, WorkSummary[]> {
  const out = new Map<string, WorkSummary[]>();
  for (const row of rows) {
    const key = dayKey(row.due);
    if (!key) continue;
    const day = out.get(key);
    if (day) day.push(row);
    else out.set(key, [row]);
  }
  for (const day of out.values()) {
    day.sort((a, b) => (a.due ?? "").localeCompare(b.due ?? "") || a.key.localeCompare(b.key));
  }
  return out;
}

/** How many chips a calendar cell draws before it folds the rest into a count. */
export const CALENDAR_CELL_CHIPS = 3;

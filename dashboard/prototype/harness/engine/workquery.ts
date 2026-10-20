/**
 * `work_items`, the native tracker's one query, as the demo answers it.
 *
 * A PORT OF THE ENGINE'S GRAMMAR, held to its rules: the parameters it reads
 * and the values it refuses (`internal/tracker/query.go`, `dates.go`), the
 * presets and saved views it expands UNDER the caller's own parameters
 * (`expand.go`), the predicate every task is tested against, with the
 * open-work default and the subtree rule (`read.go`), the columns a board
 * draws (`grouping.go`), the totals (`totals.go`) and a task's place in the
 * drawing (`around.go`). Every screen that lists work asks through here, so a
 * filter the engine applies is applied here, in the same order of precedence.
 *
 * A parameter the grammar does not have is REFUSED `bad_params`, as the
 * engine refuses it, and so is every value the engine refuses: a filter
 * nobody parsed is a board quietly showing more than was asked for. The one
 * thing the demo company holds no data for, a custom field's values, is
 * answered without that term and said in a console warning naming it, which
 * the route walk fails on.
 */

import type {
  WorkAround,
  WorkFieldDef,
  WorkGroup,
  WorkItem,
  WorkItemsAnswer,
  WorkSummary,
  WorkTotal,
} from "~/protocol/types.ts";
import { documentOf, type DemoCompany, type DemoItem } from "./company.ts";
import { projectOfSeat } from "./derive.ts";
import { servedLevel } from "./freshness.ts";
import { bool, csv, has, int, refuse, text, type Params } from "./params.ts";
import { DemoRefusal, LOG } from "./refusal.ts";
import { addDays, dateIn, startOfDay, windowOf } from "./spend.ts";
import {
  ROW_FIELDS,
  STATUSES,
  findItem,
  groupOf,
  isOpen,
  openAsks,
  summaryOf,
  trackerEnvelope,
} from "./work.ts";

// ---- the vocabulary ----------------------------------------------------------

/** Every parameter the grammar reads (`tracker.QueryKeys`). */
const QUERY_KEYS = new Set([
  "any",
  "archived",
  "archived_at",
  "around",
  "asked_by",
  "asked_of",
  "assignee",
  "batch",
  "blocked",
  "blocking",
  "checklist_assignee",
  "closed",
  "closed_since",
  "collaborator",
  "container",
  "created",
  "cursor",
  "done",
  "due",
  "estimate",
  "fields",
  "finished",
  "flag",
  "group",
  "group_by",
  "group_by2",
  "group_limit",
  "has_children",
  "has_dependencies",
  "has_open_asks",
  "has_parent",
  "key",
  "limit",
  "linked_page",
  "max_lag_seconds",
  "max_lag_seq",
  "min_position",
  "parent",
  "points",
  "preset",
  "priorities",
  "priority",
  "q",
  "read_level",
  "references",
  "removed",
  "reporter",
  "root",
  "routing_unit",
  "show_closed",
  "sort",
  "spend_tokens",
  "start",
  "status",
  "status_entered",
  "status_group",
  "subgroup",
  "subtasks",
  "tag",
  "totals",
  "type",
  "unit",
  "updated",
  "view",
  "watcher",
]);

/** The prefix that makes a parameter a custom-field filter. */
const FIELD_PREFIX = "f.";

/** What a branch of `any` may not carry: each is about the ANSWER, not the rows. */
const ANSWER_KEYS = [
  "any",
  "limit",
  "cursor",
  "group_by",
  "group_by2",
  "group",
  "subgroup",
  "group_limit",
  "sort",
  "view",
  "preset",
  "totals",
  "removed",
  "archived",
  "show_closed",
  "closed_since",
  "subtasks",
  "fields",
  "around",
  "read_level",
  "max_lag_seconds",
  "max_lag_seq",
  "min_position",
];

const STATUS_GROUPS = ["not_started", "active", "done", "closed"];
const OPEN_GROUPS = ["not_started", "active"];
/** In the engine's declared order, which is also the rank `sort=priority` reads. */
const PRIORITIES = ["none", "low", "normal", "high", "urgent"];
const SUBTASK_MODES = ["collapsed", "expanded", "separate"];
const ARCHIVED_MODES = ["false", "true", "only"];
const PRESETS = ["my_queue", "priorities", "triage", "blocked", "overdue"];
const ATTENTION_FLAGS = [
  "cycle",
  "too_deep",
  "inconsistent_project",
  "key_collision",
  "one_sided",
  "one_sided_final",
];

const PAGE_DEFAULT = 50;
const PAGE_MAX = 500;
const GROUP_ROWS_DEFAULT = 20;
const GROUP_ROWS_MAX = 100;
const MAX_GROUPS = 64;
const MAX_GROUPS_WITH_SUBGROUPS = 16;
const MAX_SUBGROUPS = 16;
const TOTAL_HINT_CEILING = 10_000;
const MAX_ANY_BRANCHES = 8;
const MAX_TOTALS = 8;
const MAX_PRIORITIES = 32;
/** A title's bound, which is what bounds a substring of one (`tracker.MaxTitle`). */
const MAX_QUERY_TEXT = 256;

/** The nine date columns a filter may name, each read from the task. */
const DATE_COLUMNS: Record<string, (t: WorkItem) => string | undefined> = {
  due: (t) => t.due_at,
  start: (t) => t.start_at,
  created: (t) => t.created_at,
  updated: (t) => t.updated_at ?? t.created_at,
  done: (t) => t.done_at,
  closed: (t) => t.closed_at,
  finished: (t) => finishedAt(t),
  archived_at: () => undefined,
  status_entered: (t) => t.status_entered_at,
};

/** The numeric filters, each over a column the schema keeps NOT NULL at zero. */
const NUMBER_COLUMNS: Record<string, (t: WorkItem) => number> = {
  estimate: (t) => t.estimate_minutes ?? 0,
  points: (t) => t.points ?? 0,
  spend_tokens: (t) => t.spend?.tokens ?? 0,
};

const BOOL_KEYS = [
  "blocked",
  "blocking",
  "has_dependencies",
  "has_children",
  "has_parent",
  "has_open_asks",
] as const;
type BoolKey = (typeof BOOL_KEYS)[number];

/** What a caller may order by, and whether the column can be absent (absent sorts last). */
const SORT_COLUMNS: Record<string, { value: (t: WorkItem) => string | number | undefined }> = {
  rank: { value: (t) => t.rank ?? "" },
  updated: { value: (t) => instant(t.updated_at ?? t.created_at) },
  priority: { value: (t) => PRIORITIES.indexOf(t.priority ?? "none") },
  created: { value: (t) => instant(t.created_at) },
  title: { value: (t) => t.title },
  estimate: { value: (t) => t.estimate_minutes ?? 0 },
  points: { value: (t) => t.points ?? 0 },
  spend_tokens: { value: (t) => t.spend?.tokens ?? 0 },
  reopens: { value: () => 0 },
  status_entered: { value: (t) => instant(t.status_entered_at) },
  due: { value: (t) => instant(t.due_at) },
  start: { value: (t) => instant(t.start_at) },
  removed: { value: (t) => instant(t.removed?.at) },
};

const GROUP_KEYS = [
  "status",
  "status_group",
  "assignee",
  "priority",
  "tag",
  "type",
  "project",
  "unit",
  "routing_unit",
  "parent",
  "due:day",
  "due:week",
  "due:bucket",
  "start:week",
];

/** The columns a total may be taken over: numbers, and the instants an order statistic answers. */
const TOTAL_COLUMNS: Record<string, (t: WorkItem) => number | undefined> = {
  spend_tokens: (t) => t.spend?.tokens ?? 0,
  spend_turns: (t) => t.spend?.turns ?? 0,
  spend_rounds: (t) => t.spend?.rounds ?? 0,
  spend_input: (t) => t.spend?.input ?? 0,
  spend_output: (t) => t.spend?.output ?? 0,
  spend_cache_read: (t) => t.spend?.cache_read ?? 0,
  spend_cache_write: (t) => t.spend?.cache_write ?? 0,
  spend_wall_ms: (t) => t.spend?.wall_ms ?? 0,
  spend_workers: (t) => t.spend?.workers ?? 0,
  spend_sent_back: (t) => t.spend?.sent_back ?? 0,
  estimate_min: (t) => t.estimate_minutes ?? 0,
  points: (t) => t.points ?? 0,
  reassignments: (t) => t.reassignments ?? 0,
  reopens: () => 0,
  depth: (t) => t.depth ?? 0,
  due_at: (t) => instant(t.due_at),
  start_at: (t) => instant(t.start_at),
  created_at: (t) => instant(t.created_at),
  updated_at: (t) => instant(t.updated_at ?? t.created_at),
  finished_at: (t) => instant(finishedAt(t)),
};
const INSTANT_COLUMNS = new Set(["due_at", "start_at", "created_at", "updated_at", "finished_at"]);
const TOTAL_OPS = ["sum", "avg", "min", "max", "count", "median", "p90"];
const RANK_OF: Record<string, number> = { median: 50, p90: 90 };

/** The relative due bands, in drawing order, with the label each column carries. */
const DUE_BANDS: [string, string][] = [
  ["overdue", "Overdue"],
  ["earlier", "Earlier"],
  ["today", "Today"],
  ["this_week", "This week"],
  ["later", "Later"],
  ["", "No due date"],
];

// ---- reading a task ---------------------------------------------------------

/** An RFC 3339 instant as milliseconds, or undefined for an absent one. */
function instant(iso: string | undefined): number | undefined {
  if (!iso) return undefined;
  const at = Date.parse(iso);
  return Number.isNaN(at) ? undefined : at;
}

/** The earlier of a task's two finish stamps (`Task.FinishedAt`). */
function finishedAt(t: WorkItem): string | undefined {
  if (!t.done_at) return t.closed_at;
  if (!t.closed_at) return t.done_at;
  return Date.parse(t.closed_at) < Date.parse(t.done_at) ? t.closed_at : t.done_at;
}

// ---- dates on the company clock ---------------------------------------------

interface Clock {
  now: Date;
  zone: string;
  /** Today's date on the company clock. */
  today: string;
}

interface Anchor {
  at: number;
  allDay: boolean;
}

interface DateFilter {
  op: "lt" | "lte" | "gt" | "gte" | "range";
  from: Anchor;
  to?: Anchor;
  overdue?: boolean;
}

/** One date token resolved on the company clock (`tracker.ResolveDate`). */
function resolveDate(raw: string, clock: Clock): Anchor {
  const token = raw.trim().toLowerCase();
  const begins = (day: string): Anchor => ({
    at: startOfDay(day, clock.zone).getTime(),
    allDay: true,
  });
  const week = windowOf("week", clock.today);
  const month = windowOf("month", clock.today);
  switch (token) {
    case "":
      return refuse(
        "An empty date matches every task ever written: name an instant or a relative token.",
      );
    case "now":
      return { at: clock.now.getTime(), allDay: false };
    case "today":
      return begins(clock.today);
    case "yesterday":
      return begins(addDays(clock.today, -1));
    case "tomorrow":
      return begins(addDays(clock.today, 1));
    case "sow":
    case "eopw":
      return begins(week.first);
    case "eow":
    case "sonw":
      return begins(week.next);
    case "sopw":
      return begins(addDays(week.first, -7));
    case "eonw":
      return begins(addDays(week.first, 14));
    case "som":
    case "eopm":
      return begins(month.first);
    case "eom":
    case "sonm":
      return begins(month.next);
    case "sopm":
      return begins(windowOf("month", addDays(month.first, -1)).first);
    case "eonm":
      return begins(windowOf("month", month.next).next);
  }
  const offset = /^\+?(-?\d+)d$/.exec(token);
  if (offset) return begins(addDays(clock.today, Number(offset[1])));
  // An instant is read from the value as written: RFC 3339 spells its `T`
  // and its `Z` in capitals, with any fraction of a second.
  const written = raw.trim();
  if (/^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(\.\d+)?(Z|[+-]\d{2}:\d{2})$/.test(written)) {
    const at = Date.parse(written);
    if (!Number.isNaN(at)) return { at, allDay: false };
  }
  if (/^\d{4}-\d{2}-\d{2}$/.test(token) && !Number.isNaN(Date.parse(`${token}T00:00:00Z`))) {
    return begins(token);
  }
  return refuse(
    `"${raw.trim()}" is not a date: name an instant, a calendar date, an offset like +7d, or one of the relative tokens.`,
  );
}

/** The named windows a date filter may be written as (`tracker.DateAlias`). */
const DATE_ALIASES: Record<string, string> = {
  next7: "range:today..+7d",
  last7: "range:-7d..today",
  thisweek: "range:sow..eow",
  thismonth: "range:som..eom",
  lastmonth: "range:sopm..eopm",
  earlier: "lt:today",
};

function parseDateFilter(raw: string, clock: Clock): DateFilter {
  const value = DATE_ALIASES[raw.trim().toLowerCase()] ?? raw.trim();
  if (value.toLowerCase() === "overdue") {
    return { op: "lt", from: resolveDate("today", clock), overdue: true };
  }
  const at = value.indexOf(":");
  if (at < 0) {
    return refuse(
      `"${value}" names no comparison: a date filter is lt:, lte:, gt:, gte: or range:a..b.`,
    );
  }
  const op = value.slice(0, at).toLowerCase();
  const rest = value.slice(at + 1);
  if (op === "lt" || op === "lte" || op === "gt" || op === "gte") {
    return { op, from: resolveDate(rest, clock) };
  }
  if (op === "range") {
    const split = rest.indexOf("..");
    if (split < 0) return refuse(`A range is a..b, and "${rest}" carries no separator.`);
    const from = resolveDate(rest.slice(0, split), clock);
    const to = resolveDate(rest.slice(split + 2), clock);
    if (to.at < from.at)
      return refuse(`The range "${rest}" ends before it starts, so it matches nothing.`);
    return { op, from, to };
  }
  return refuse(`"${op}" is not a date comparison.`);
}

function dateHolds(value: number | undefined, filter: DateFilter): boolean {
  if (value === undefined) return false;
  switch (filter.op) {
    case "lt":
      return value < filter.from.at;
    case "lte":
      return value <= filter.from.at;
    case "gt":
      return value > filter.from.at;
    case "gte":
      return value >= filter.from.at;
    case "range":
      // Half-open, which is what makes "this week" hold every instant of Sunday.
      return value >= filter.from.at && value < (filter.to?.at ?? Infinity);
  }
}

interface NumFilter {
  op: "lt" | "lte" | "gt" | "gte" | "range" | "null" | "not_null";
  from: number;
  to: number;
}

function parseNumFilter(value: string): NumFilter {
  if (value === "null" || value === "not_null") return { op: value, from: 0, to: 0 };
  const at = value.indexOf(":");
  if (at < 0) {
    return refuse(
      `"${value}" names no comparison: a numeric filter is lt:, lte:, gt:, gte:, range:a..b, null or not_null.`,
    );
  }
  const op = value.slice(0, at);
  const rest = value.slice(at + 1);
  const number = (raw: string): number => {
    const n = Number(raw);
    return raw.trim() === "" || Number.isNaN(n) ? refuse(`"${raw}" is not a number.`) : n;
  };
  if (op === "lt" || op === "lte" || op === "gt" || op === "gte") {
    return { op, from: number(rest), to: 0 };
  }
  if (op === "range") {
    const split = rest.indexOf("..");
    if (split < 0) return refuse(`A range is a..b, and "${rest}" carries no separator.`);
    const from = number(rest.slice(0, split));
    const to = number(rest.slice(split + 2));
    if (to < from)
      return refuse(`The range "${rest}" ends below where it starts, so it matches nothing.`);
    return { op, from, to };
  }
  return refuse(`"${op}" is not a numeric comparison.`);
}

function numHolds(value: number, filter: NumFilter): boolean {
  switch (filter.op) {
    case "lt":
      return value < filter.from;
    case "lte":
      return value <= filter.from;
    case "gt":
      return value > filter.from;
    case "gte":
      return value >= filter.from;
    case "range":
      return value >= filter.from && value <= filter.to;
    case "null":
      return value === 0;
    case "not_null":
      return value !== 0;
  }
}

// ---- the parsed question ----------------------------------------------------

interface Query {
  project: string;
  keys: string[];
  /** Whose ordered list `priorities=` names, and every name their record may be kept under. */
  priorityListOf: string[];
  status: string[];
  statusNot: string[];
  statusGroups: string[];
  priorities: string[];
  types: string[];
  unit: string[];
  routingUnit: string[];
  assignee: string[];
  reporter: string[];
  collaborator: string[];
  watcher: string[];
  checklistAssignee: string[];
  tags: { mode: "any" | "all" | "none"; tags: string[] };
  parent: string;
  root: string;
  batch: string;
  references: string;
  linkedPage: string;
  bools: Partial<Record<BoolKey, boolean>>;
  askedOf: string;
  /** Who asked: the seat a bound person asks as, or a credential's login. */
  askedBy: string[];
  text: string;
  /** Custom-field terms, which the demo company holds no values for. */
  fields: string[];
  dates: [string, DateFilter][];
  numbers: [string, NumFilter][];
  flags: string[];
  removed: boolean;
  archived: string;
  showClosed: { all: boolean; since: number | null };
  subtasks: string;
  groupBy: string;
  groupBy2: string;
  group: string | undefined;
  subgroup: string | undefined;
  groupLimit: number;
  sort: { key: string; desc: boolean }[];
  totals: string[];
  rowFields: string[];
  around: string;
  cursor: string;
  limit: number;
  any: Query[];
  view: string;
  preset: string;
}

/** One flat parameter map read into one question (`tracker.ParseQuery`). */
function parseQuery(p: Params, clock: Clock, branch: boolean): Query {
  for (const key of Object.keys(p).sort()) {
    if (key.startsWith(FIELD_PREFIX) && key.length > FIELD_PREFIX.length) continue;
    if (!QUERY_KEYS.has(key)) {
      refuse(
        `"${key}" is not a query parameter: a filter nothing parses would silently widen this answer, so it is refused rather than ignored; a custom field is ${FIELD_PREFIX}<ref>.`,
      );
    }
  }
  const q: Query = {
    project: "",
    keys: csv(text(p, "key")),
    priorityListOf: text(p, "priorities").trim() ? [text(p, "priorities").trim()] : [],
    status: [],
    statusNot: [],
    statusGroups: [],
    priorities: [],
    types: csv(text(p, "type")),
    unit: csv(text(p, "unit")),
    routingUnit: csv(text(p, "routing_unit")),
    assignee: csv(text(p, "assignee")),
    reporter: csv(text(p, "reporter")),
    collaborator: csv(text(p, "collaborator")),
    watcher: csv(text(p, "watcher")),
    checklistAssignee: csv(text(p, "checklist_assignee")),
    tags: { mode: "any", tags: [] },
    parent: text(p, "parent"),
    root: text(p, "root"),
    batch: text(p, "batch"),
    references: text(p, "references"),
    linkedPage: text(p, "linked_page"),
    bools: {},
    askedOf: text(p, "asked_of"),
    askedBy: text(p, "asked_by").trim() ? [text(p, "asked_by").trim()] : [],
    text: text(p, "q").trim(),
    fields: [],
    dates: [],
    numbers: [],
    flags: [],
    removed: has(p, "removed") ? bool(p, "removed", true) : false,
    archived: "false",
    showClosed: { all: false, since: null },
    subtasks: "collapsed",
    groupBy: "",
    groupBy2: "",
    group: undefined,
    subgroup: undefined,
    groupLimit: int(p, "group_limit", 0),
    sort: [],
    totals: csv(text(p, "totals")),
    rowFields: [],
    around: text(p, "around").trim(),
    cursor: text(p, "cursor"),
    limit: int(p, "limit", 0),
    any: [],
    view: text(p, "view"),
    preset: text(p, "preset"),
  };

  for (const flag of csv(text(p, "flag"))) {
    if (!ATTENTION_FLAGS.includes(flag)) {
      refuse(`"${flag}" is not an attention flag. The flags are: ${ATTENTION_FLAGS.join(", ")}.`);
    }
    q.flags.push(flag);
  }

  const container = text(p, "container").trim();
  if (container && container !== "workspace") {
    let key = container;
    if (container.startsWith("project:")) key = container.slice("project:".length);
    else if (container.includes(":")) {
      refuse(
        `container "${container}" is not a task container: a task lives in the workspace or a project (container=workspace, container=project:<KEY>); a team's work is unit=<name> and a person's is assignee=<handle>.`,
      );
    }
    if (!key) refuse(`container "${container}" names no project.`);
    q.project = key;
  }

  for (const value of csv(text(p, "status"))) {
    const negated = value.startsWith("!");
    const slug = negated ? value.slice(1) : value;
    if (!STATUSES.some((s) => s.status === slug)) {
      refuse(`"${value}" is not one of this company's six statuses.`);
    }
    (negated ? q.statusNot : q.status).push(slug);
  }
  for (const value of csv(text(p, "status_group"))) {
    if (!STATUS_GROUPS.includes(value)) refuse(`"${value}" is not one of the four status groups.`);
    q.statusGroups.push(value);
  }

  const tag = text(p, "tag").trim();
  if (tag) {
    const at = tag.indexOf(":");
    if (at >= 0) {
      const mode = tag.slice(0, at);
      if (mode !== "any" && mode !== "all" && mode !== "none") {
        refuse(`"${mode}" is not a tag mode: the three are any:, all: and none:.`);
      }
      q.tags = { mode: mode as "any" | "all" | "none", tags: csv(tag.slice(at + 1)) };
    } else {
      q.tags = { mode: "any", tags: csv(tag) };
    }
  }

  for (const value of csv(text(p, "priority"))) {
    if (!PRIORITIES.includes(value)) refuse(`"${value}" is not one of the five priorities.`);
    q.priorities.push(value);
  }

  const subtasks = text(p, "subtasks");
  if (subtasks) {
    if (!SUBTASK_MODES.includes(subtasks)) {
      refuse(
        `"${subtasks}" is not a subtask mode: the three are collapsed, expanded and separate.`,
      );
    }
    q.subtasks = subtasks;
  }

  for (const key of Object.keys(DATE_COLUMNS)) {
    const value = text(p, key).trim();
    if (value) q.dates.push([key, parseDateFilter(value, clock)]);
  }
  for (const key of Object.keys(NUMBER_COLUMNS)) {
    const value = text(p, key).trim();
    if (value) q.numbers.push([key, parseNumFilter(value)]);
  }
  for (const key of Object.keys(p)) {
    if (key.startsWith(FIELD_PREFIX) && key.length > FIELD_PREFIX.length) q.fields.push(key);
  }
  for (const key of BOOL_KEYS) {
    if (has(p, key)) q.bools[key] = bool(p, key, true);
  }

  if (new TextEncoder().encode(q.text).length > MAX_QUERY_TEXT) {
    refuse(
      `The find text is longer than ${MAX_QUERY_TEXT} bytes, and no title is: to find work by what it is about, search it.`,
    );
  }

  const shown = text(p, "show_closed").trim();
  const since = text(p, "closed_since").trim();
  if (since) {
    if (shown) {
      refuse(
        `closed_since and show_closed both say which finished work is in the answer: closed_since=${since} is open work plus what finished since then, so drop show_closed=${shown}.`,
      );
    }
    q.showClosed.since = resolveDate(since, clock).at;
  } else if (shown === "true") {
    q.showClosed.all = true;
  } else if (shown && shown !== "false") {
    const recent = /^recent:(.+)$/.exec(shown);
    const ms = recent ? goDuration(recent[1]!) : null;
    if (!recent)
      refuse(`show_closed is false, true or recent:<duration>, and "${shown}" is none of them.`);
    if (ms === null || ms <= 0) refuse(`"${recent![1]}" is not a positive duration.`);
    q.showClosed.since = clock.now.getTime() - ms!;
  }

  const archived = text(p, "archived");
  if (archived) {
    if (!ARCHIVED_MODES.includes(archived)) {
      refuse(`archived is ${ARCHIVED_MODES.join(", ")}, and "${archived}" is none of them.`);
    }
    q.archived = archived;
  }

  parseGrouping(p, q);

  for (const value of csv(text(p, "sort"))) {
    const desc = value.startsWith("-");
    const key = desc ? value.slice(1) : value;
    if (key === FIELD_PREFIX)
      refuse(`"${value}" names no field to sort on: a custom-field sort is f.<slug>.`);
    if (!SORT_COLUMNS[key] && !key.startsWith(FIELD_PREFIX))
      refuse(`"${value}" is not a sort key.`);
    q.sort.push({ key, desc });
  }

  if (q.totals.length > MAX_TOTALS) {
    refuse(`${q.totals.length} totals were asked for and the cap is ${MAX_TOTALS}.`);
  }
  for (const entry of q.totals) {
    if (!entry.includes(":"))
      refuse(`A total is <column>:<op>, and "${entry}" carries no operation.`);
  }

  for (const name of csv(text(p, "fields"))) {
    if (!ROW_FIELDS.includes(name)) {
      refuse(`"${name}" is not a row field: the four are ${ROW_FIELDS.join(", ")}.`);
    }
    if (!q.rowFields.includes(name)) q.rowFields.push(name);
  }

  // Refused here, where the engine's parse reaches it; the level the answer
  // states is the engine's to add (`DemoEngine.answer`).
  servedLevel(p);

  if (!branch) q.any = parseAny(p, clock);
  return q;
}

/** A Go duration (`time.ParseDuration`), in milliseconds, or null. */
function goDuration(raw: string): number | null {
  const unit: Record<string, number> = {
    ns: 1e-6,
    us: 1e-3,
    µs: 1e-3,
    ms: 1,
    s: 1e3,
    m: 6e4,
    h: 3.6e6,
  };
  const parts = [...raw.matchAll(/(\d+(?:\.\d+)?)(ns|us|µs|ms|s|m|h)/g)];
  if (parts.length === 0 || parts.map((m) => m[0]).join("") !== raw) return null;
  return parts.reduce((sum, m) => sum + Number(m[1]) * unit[m[2]!]!, 0);
}

function parseGrouping(p: Params, q: Query): void {
  if (has(p, "group")) q.group = text(p, "group").trim();
  if (has(p, "subgroup")) q.subgroup = text(p, "subgroup").trim();
  for (const key of ["group_by", "group_by2"] as const) {
    const value = text(p, key).trim();
    if (!value) continue;
    if (!GROUP_KEYS.includes(value) && !value.startsWith(FIELD_PREFIX)) {
      refuse(`"${value}" is not a grouping.`);
    }
    if (key === "group_by") q.groupBy = value;
    else q.groupBy2 = value;
  }
  if (q.groupBy2 && q.groupBy2 === q.groupBy) {
    refuse(
      `The secondary grouping is "${q.groupBy}", the same as the first: every row would be alone in its own subgroup.`,
    );
  }
  if (q.groupBy2 && !q.groupBy) {
    refuse(
      "group_by2 was passed without group_by; the secondary grouping is a split of the first.",
    );
  }
  if (q.groupBy === "project" && q.project) {
    refuse(`group_by=project inside project ${q.project} puts every row in one group.`);
  }
  if (q.group !== undefined && !q.groupBy) {
    refuse(
      `group=${q.group} was passed without group_by: a column with no axis is a narrowing that would silently answer everything.`,
    );
  }
  if (q.subgroup !== undefined && !q.groupBy2) {
    refuse(
      `subgroup=${q.subgroup} was passed without group_by2, so there is no second axis for it to name.`,
    );
  }
  if (q.groupLimit !== 0 && !q.groupBy) {
    refuse(
      "group_limit bounds the rows one column carries and no group_by was passed: limit bounds a flat answer.",
    );
  }
  if (q.groupLimit < 0)
    refuse(`group_limit is ${q.groupLimit}, and a negative number of rows is not a bound.`);
}

/** The one level of disjunction: each branch a predicate in the same grammar (`parseAny`). */
function parseAny(p: Params, clock: Clock): Query[] {
  const raw = text(p, "any").trim();
  if (!raw) return [];
  let branches: unknown;
  try {
    branches = JSON.parse(raw);
  } catch {
    return refuse("any is a JSON list of parameter objects.");
  }
  if (
    !Array.isArray(branches) ||
    branches.some((b) => !b || typeof b !== "object" || Array.isArray(b))
  ) {
    return refuse("any is a JSON list of parameter objects.");
  }
  if (branches.length > MAX_ANY_BRANCHES) {
    refuse(`any carries ${branches.length} branches and the cap is ${MAX_ANY_BRANCHES}.`);
  }
  return (branches as Params[]).map((branch, i) => {
    for (const forbidden of ANSWER_KEYS) {
      if (Object.hasOwn(branch, forbidden)) {
        refuse(
          `any branch ${i} carries "${forbidden}", which is about the answer rather than the rows: a branch is a predicate, not a second query.`,
        );
      }
    }
    const parsed = parseQuery(branch, clock, true);
    if (!constrains(parsed)) {
      refuse(
        `any branch ${i} selects nothing: a branch is a predicate, and an empty one matches every task and makes the other branches decoration.`,
      );
    }
    return parsed;
  });
}

/** Whether a branch narrows anything at all. */
function constrains(q: Query): boolean {
  return Boolean(
    q.project ||
    q.keys.length ||
    q.priorityListOf.length ||
    q.status.length ||
    q.statusNot.length ||
    q.statusGroups.length ||
    q.priorities.length ||
    q.types.length ||
    q.unit.length ||
    q.routingUnit.length ||
    q.assignee.length ||
    q.reporter.length ||
    q.collaborator.length ||
    q.watcher.length ||
    q.checklistAssignee.length ||
    q.tags.tags.length ||
    q.parent ||
    q.root ||
    q.batch ||
    q.references ||
    q.linkedPage ||
    Object.keys(q.bools).length ||
    q.askedOf ||
    q.askedBy.length ||
    q.text ||
    q.fields.length ||
    q.dates.length ||
    q.numbers.length ||
    q.flags.length,
  );
}

// ---- expansion --------------------------------------------------------------

interface Viewer {
  handle: string;
  project: string;
}

/** A preset's defaults (`tracker.expandPreset`). */
function presetDefaults(name: string, viewer: Viewer): Params {
  const open = OPEN_GROUPS.join(",");
  switch (name) {
    case "my_queue": {
      if (!viewer.handle) {
        return refuse(
          "preset=my_queue is what one person is expected to do next and this read names nobody.",
        );
      }
      const unclaimed: Params = { assignee: "none" };
      if (viewer.project) unclaimed.container = `project:${viewer.project}`;
      return {
        any: JSON.stringify([{ assignee: viewer.handle }, unclaimed]),
        status_group: open,
        blocked: "false",
        sort: "-priority,due",
      };
    }
    case "priorities":
      if (!viewer.handle) {
        return refuse(
          "preset=priorities is one person's own ordered list and this read names nobody.",
        );
      }
      return { priorities: viewer.handle, status_group: open };
    case "triage":
      return { assignee: "none", status_group: open, sort: "-priority,created" };
    case "blocked":
      return { blocked: "true", status_group: open };
    case "overdue":
      return { due: "overdue" };
  }
  return refuse(`"${name}" is not a preset: the five are ${PRESETS.join(", ")}.`);
}

/**
 * The parameters a read is answered from: the preset's defaults, then the
 * saved view's, then the caller's own, each over the last (`Reader.Expand`),
 * with a custom field's `me` resolved to the viewer.
 */
function expand(c: DemoCompany, params: Params, viewer: Viewer): Params {
  const preset = text(params, "preset").trim();
  const viewId = text(params, "view").trim();
  let fromView: Params = {};
  if (viewId) {
    const view = c.work.views.find((v) => v.id === viewId);
    if (!view) {
      throw new DemoRefusal(
        "unavailable",
        `View ${viewId} is not on this node: it was deleted, or this copy has not caught up.`,
      );
    }
    fromView = { ...view.params };
  }
  const merged: Params = {
    ...(preset ? presetDefaults(preset, viewer) : {}),
    ...fromView,
    ...params,
  };
  const named = Object.keys(merged).filter(
    (key) => key.startsWith(FIELD_PREFIX) && text(merged, key).trim() === "me",
  );
  if (named.length > 0) {
    if (!viewer.handle) {
      refuse(`${named.sort().join(", ")} names \`me\` and this read names nobody.`);
    }
    for (const key of named) merged[key] = viewer.handle;
  }
  return merged;
}

// ---- the predicate ----------------------------------------------------------

interface Context {
  c: DemoCompany;
  clock: Clock;
  /** The day's boundaries the relative due bands cut on. */
  dayStart: number;
  dayEnd: number;
  weekEnd: number;
  /** Every task id waited on by a task, for `blocking`. */
  waitedOn: Map<string, DemoItem[]>;
  priorityList: string[] | null;
}

function blockers(ctx: Context, item: DemoItem): DemoItem[] {
  return item.links
    .filter((l) => l.kind === "waiting_on")
    .map((l) => findItem(ctx.c, l.other))
    .filter((other): other is DemoItem => other !== null);
}

/** The task at the top of `item`'s subtree, itself when it has no parent. */
function rootOf(c: DemoCompany, item: DemoItem): DemoItem {
  let at = item;
  const seen = new Set<string>([item.task.id]);
  while (at.task.parent) {
    const parent = c.work.items.find((i) => i.task.id === at.task.parent);
    if (!parent || seen.has(parent.task.id)) break;
    seen.add(parent.task.id);
    at = parent;
  }
  return at;
}

/** Whether `item` sits in the subtree under `rootId`, itself included. */
function descends(c: DemoCompany, item: DemoItem, rootId: string): boolean {
  let at: DemoItem | undefined = item;
  const seen = new Set<string>();
  while (at && !seen.has(at.task.id)) {
    if (at.task.id === rootId) return true;
    seen.add(at.task.id);
    const parentId: string | undefined = at.task.parent;
    at = parentId ? c.work.items.find((i) => i.task.id === parentId) : undefined;
  }
  return false;
}

/** Every key a task's description names, in document order (`taskKeysIn`). */
function keysIn(body: string): string[] {
  return [...new Set(body.match(/[A-Z][A-Z0-9]{1,9}-[0-9]+/g) ?? [])];
}

/** The keys and the names a unit filter matches (`unitSpellings`). */
function unitSpellings(c: DemoCompany, refs: string[]): Set<string> {
  const out = new Set<string>();
  for (const raw of refs) {
    const ref = raw.trim();
    if (!ref) continue;
    const unit = unitByRef(c, ref);
    if (!unit) {
      out.add(ref);
      continue;
    }
    out.add(unit.key);
    out.add(unit.name);
  }
  return out;
}

/** A unit of the chart by its key or its name, compared without case (`UnitByRef`). */
function unitByRef(c: DemoCompany, ref: string): { key: string; name: string } | null {
  const fold = (s: string) => s.trim().toLowerCase();
  const units: { id?: string; name: string; units?: unknown }[] = [];
  const walk = (list: unknown) => {
    for (const u of (list as { id?: string; name: string; units?: unknown }[] | undefined) ?? []) {
      units.push(u);
      walk(u.units);
    }
  };
  walk(documentOf(c).units);
  const unit =
    units.find((u) => u.id && fold(u.id) === fold(ref)) ??
    units.find((u) => fold(u.name) === fold(ref));
  return unit ? { key: unit.id?.trim() || unit.name.trim(), name: unit.name } : null;
}

function handleHolds(value: string | undefined, handles: string[]): boolean {
  const none = handles.includes("none");
  const named = handles.filter((h) => h !== "none");
  if (none && !value) return true;
  return named.includes(value ?? "") && Boolean(value);
}

/**
 * Whether a task satisfies a parsed question: `read.go`'s `compileWhere` for
 * one row. A BRANCH is a predicate only: the tombstone, the archive and the
 * finished-work default are the answer's, decided once at the top.
 */
function holds(ctx: Context, item: DemoItem, q: Query, branch: boolean): boolean {
  const { c } = ctx;
  const t = item.task;
  if (q.project && t.project !== q.project) return false;
  if (!branch && (q.removed ? !t.removed : Boolean(t.removed))) return false;
  if (q.keys.length > 0 && !q.keys.includes(t.key)) return false;
  if (q.priorityListOf.length > 0) {
    // Only the top level's list is read; a branch naming one holds nothing.
    const list = branch ? null : ctx.priorityList;
    if (!list?.length || !list.includes(t.id)) return false;
  }
  if (q.status.length > 0 && !q.status.includes(t.status)) return false;
  if (q.statusNot.includes(t.status)) return false;
  if (q.statusGroups.length > 0 && !q.statusGroups.includes(groupOf(t.status))) return false;
  if (q.priorities.length > 0 && !q.priorities.includes(t.priority ?? "none")) return false;
  if (q.types.length > 0 && !q.types.includes(t.type ?? "task")) return false;
  if (q.unit.length > 0 && !unitSpellings(c, q.unit).has(t.filed_unit ?? "")) return false;
  if (q.routingUnit.length > 0 && !unitSpellings(c, q.routingUnit).has(t.routing_unit ?? "")) {
    return false;
  }
  if (q.assignee.length > 0 && !handleHolds(t.assignee, q.assignee)) return false;
  if (q.reporter.length > 0 && !handleHolds(t.reporter, q.reporter)) return false;
  if (
    q.collaborator.length > 0 &&
    !(t.collaborators ?? []).some((h) => q.collaborator.includes(h))
  ) {
    return false;
  }
  if (q.watcher.length > 0 && !(t.watchers ?? []).some((h) => q.watcher.includes(h))) return false;
  if (
    q.checklistAssignee.length > 0 &&
    !(t.checklists ?? []).some((list) =>
      (list.items ?? []).some((i) => i.assignee && q.checklistAssignee.includes(i.assignee)),
    )
  ) {
    return false;
  }
  if (q.tags.tags.length > 0) {
    const mine = t.tags ?? [];
    const held = q.tags.tags.filter((tag) => mine.includes(tag));
    if (q.tags.mode === "any" && held.length === 0) return false;
    if (q.tags.mode === "all" && new Set(held).size !== new Set(q.tags.tags).size) return false;
    if (q.tags.mode === "none" && held.length > 0) return false;
  }
  if (q.parent && t.parent !== q.parent) return false;
  if (q.root && !descends(c, item, q.root)) return false;
  // No demo task was created in a batch.
  if (q.batch) return false;
  if (q.references) {
    const named = keysIn(t.body ?? "").includes(q.references);
    const target = c.work.items.find(
      (i) => i.task.key === q.references || (i.task.former_keys ?? []).includes(q.references),
    );
    if (!named || !target || target.task.id === t.id) return false;
  }
  if (q.linkedPage && !item.links.some((l) => l.kind === "page" && l.other === q.linkedPage)) {
    return false;
  }
  const b = q.bools;
  if (b.blocked !== undefined && blockers(ctx, item).some(isOpen) !== b.blocked) return false;
  if (b.has_children !== undefined) {
    const children = c.work.items.some((i) => i.task.parent === t.id);
    if (children !== b.has_children) return false;
  }
  if (b.has_parent !== undefined && Boolean(t.parent) !== b.has_parent) return false;
  if (b.has_open_asks !== undefined && openAsks(item).length > 0 !== b.has_open_asks) return false;
  if (q.askedOf && !openAsks(item).some((ask) => ask.ask === q.askedOf)) return false;
  if (q.askedBy.length > 0 && !openAsks(item).some((ask) => q.askedBy.includes(ask.author))) {
    return false;
  }
  if (b.blocking !== undefined) {
    const blocking = isOpen(item) && (ctx.waitedOn.get(t.id)?.length ?? 0) > 0;
    if (blocking !== b.blocking) return false;
  }
  if (b.has_dependencies !== undefined) {
    const deps = item.links.some((l) => l.kind === "waiting_on");
    if (deps !== b.has_dependencies) return false;
  }
  if (q.text) {
    // LIKE is case-blind over ASCII: a key's prefix, or anywhere in the title.
    const find = q.text.toLowerCase();
    if (!t.key.toLowerCase().startsWith(find) && !t.title.toLowerCase().includes(find)) {
      return false;
    }
  }
  for (const [key, filter] of q.dates) {
    if (!dateHolds(instant(DATE_COLUMNS[key]!(t)), filter)) return false;
    if (filter.overdue && !isOpen(item)) return false;
  }
  for (const [key, filter] of q.numbers) {
    if (!numHolds(NUMBER_COLUMNS[key]!(t), filter)) return false;
  }
  if (q.flags.length > 0) {
    // No demo task carries one of the four task flags; the two edge flags
    // are read off its relations.
    const flagged = q.flags.some(
      (flag) =>
        (flag === "one_sided" && item.links.some((l) => l.one_sided && !l.one_sided_final)) ||
        (flag === "one_sided_final" && item.links.some((l) => l.one_sided_final)),
    );
    if (!flagged) return false;
  }
  if (!branch) {
    const project = c.work.projects.find((pr) => pr.key === t.project);
    const archived = Boolean(t.archived) || Boolean(project?.archived);
    if (q.archived === "false" && archived) return false;
    if (q.archived === "only" && !archived) return false;
    const open = OPEN_GROUPS.includes(groupOf(t.status));
    if (!q.showClosed.all) {
      if (q.showClosed.since !== null) {
        const finished = instant(finishedAt(t));
        if (!open && !(finished !== undefined && finished >= q.showClosed.since)) return false;
      } else if (!open) {
        return false;
      }
    }
    if (q.any.length > 0 && !q.any.some((arm) => holds(ctx, item, arm, true))) return false;
  }
  return true;
}

// ---- the axes a board is drawn along ------------------------------------------

interface Axis {
  /** The column keys a task falls in: one, or several on a multi-valued axis. */
  keys: (item: DemoItem) => string[];
  /** The declared column order, where the axis has one. */
  order: string[];
  multi: boolean;
  /** What the column of tasks with no value is called. */
  unset: string;
  labels?: (key: string) => string;
  /** The stored spelling a requested column names. */
  canonical: (key: string) => string;
}

function utcDay(iso: string | undefined): string {
  return iso ? new Date(iso).toISOString().slice(0, 10) : "";
}

/** SQLite's `%Y-W%W`: the week of the year, weeks starting on the first Monday, in UTC. */
function utcWeek(iso: string | undefined): string {
  if (!iso) return "";
  const d = new Date(iso);
  const year = d.getUTCFullYear();
  const yday = Math.floor(
    (Date.UTC(year, d.getUTCMonth(), d.getUTCDate()) - Date.UTC(year, 0, 1)) / 86_400_000,
  );
  const monday0 = (d.getUTCDay() + 6) % 7;
  const week = Math.floor((yday + 7 - monday0) / 7);
  return `${year}-W${String(week).padStart(2, "0")}`;
}

function axisOf(ctx: Context, key: string): Axis {
  const plain = (keys: (item: DemoItem) => string[], extra: Partial<Axis> = {}): Axis => ({
    keys,
    order: [],
    multi: false,
    unset: "",
    canonical: (k) => k,
    ...extra,
  });
  const unitAxis = (column: (t: WorkItem) => string | undefined): Axis => {
    const keyOf = (value: string) => unitByRef(ctx.c, value)?.key ?? value;
    return plain((item) => [keyOf(column(item.task) ?? "")], {
      unset: "(no unit)",
      labels: (k) => unitByRef(ctx.c, k)?.name ?? "",
      canonical: keyOf,
    });
  };
  switch (key) {
    case "status":
      return plain((i) => [i.task.status], { order: STATUSES.map((s) => s.status) });
    case "status_group":
      return plain((i) => [groupOf(i.task.status)], { order: STATUS_GROUPS });
    case "assignee":
      return plain((i) => [i.task.assignee ?? ""], { unset: "(unassigned)" });
    case "priority":
      return plain((i) => [i.task.priority ?? "none"], { order: PRIORITIES });
    case "type":
      return plain((i) => [i.task.type ?? "task"]);
    case "project":
      return plain((i) => [i.task.project]);
    case "unit":
      return unitAxis((t) => t.filed_unit);
    case "routing_unit":
      return unitAxis((t) => t.routing_unit);
    case "parent":
      return plain((i) => [i.task.parent ?? ""], { unset: "(no parent)" });
    case "tag":
      return plain((i) => (i.task.tags?.length ? i.task.tags : [""]), {
        multi: true,
        unset: "(untagged)",
      });
    case "due:day":
      return plain((i) => [utcDay(i.task.due_at)], { unset: "(no due date)" });
    case "due:week":
      return plain((i) => [utcWeek(i.task.due_at)], { unset: "(no due date)" });
    case "start:week":
      return plain((i) => [utcWeek(i.task.start_at)], { unset: "(no start date)" });
    case "due:bucket": {
      const labels = Object.fromEntries(DUE_BANDS);
      return plain((i) => [dueBand(ctx, i)], {
        order: DUE_BANDS.map(([k]) => k),
        unset: labels[""]!,
        labels: (k) => labels[k] ?? "",
      });
    }
  }
  return refuse(`"${key}" is not a grouping.`);
}

function dueBand(ctx: Context, item: DemoItem): string {
  const due = instant(item.task.due_at);
  if (due === undefined) return "";
  if (due < ctx.dayStart && isOpen(item)) return "overdue";
  if (due < ctx.dayStart) return "earlier";
  if (due < ctx.dayEnd) return "today";
  if (due < ctx.weekEnd) return "this_week";
  return "later";
}

/** The statuses an answer can hold (`admittedStatuses`). */
function admittedStatuses(q: Query): string[] {
  let finished = q.showClosed.all || q.showClosed.since !== null;
  if (q.dates.some(([, f]) => f.overdue)) finished = false;
  return STATUSES.map((s) => s.status).filter((status) => {
    const group = groupOf(status);
    if (q.status.length > 0 && !q.status.includes(status)) return false;
    if (q.statusNot.includes(status)) return false;
    if (q.statusGroups.length > 0 && !q.statusGroups.includes(group)) return false;
    return finished || OPEN_GROUPS.includes(group);
  });
}

/** The columns a board draws even when empty (`admittedColumns`). */
function admittedColumns(ctx: Context, q: Query): string[] {
  let admitted: string[];
  switch (q.groupBy) {
    case "status":
      admitted = admittedStatuses(q);
      break;
    case "status_group": {
      const held = new Set(admittedStatuses(q).map(groupOf));
      admitted = STATUS_GROUPS.filter((g) => held.has(g));
      break;
    }
    case "priority":
      admitted = PRIORITIES.filter((p) => q.priorities.length === 0 || q.priorities.includes(p));
      break;
    case "due:bucket":
      admitted = admittedDueBands(ctx, q);
      break;
    default:
      return [];
  }
  if (q.group === undefined) return admitted;
  return admitted.includes(q.group) ? [q.group] : [];
}

function admittedDueBands(ctx: Context, q: Query): string[] {
  let open = false;
  let finished = false;
  for (const status of admittedStatuses(q)) {
    if (OPEN_GROUPS.includes(groupOf(status))) open = true;
    else finished = true;
  }
  const filter = q.dates.find(([key]) => key === "due")?.[1];
  const band = (key: string): [number, number] => {
    switch (key) {
      case "overdue":
      case "earlier":
        return [-Infinity, ctx.dayStart];
      case "today":
        return [ctx.dayStart, ctx.dayEnd];
      case "this_week":
        return [ctx.dayEnd, ctx.weekEnd];
      case "later":
        return [ctx.weekEnd, Infinity];
    }
    return [-Infinity, Infinity];
  };
  const range = (f: DateFilter): [number, number] => {
    switch (f.op) {
      case "lt":
        return [-Infinity, f.from.at];
      case "lte":
        return [-Infinity, f.from.at + 1];
      case "gt":
        return [f.from.at + 1, Infinity];
      case "gte":
        return [f.from.at, Infinity];
      case "range":
        return [f.from.at, f.to?.at ?? Infinity];
    }
  };
  const overlaps = ([a1, a2]: [number, number], [b1, b2]: [number, number]) => {
    const hi1 = a2 <= a1 ? a1 + 1 : a2;
    const hi2 = b2 <= b1 ? b1 + 1 : b2;
    return a1 < hi2 && b1 < hi1;
  };
  return DUE_BANDS.map(([key]) => key).filter((key) => {
    if (key === "overdue" && !open) return false;
    if (key === "earlier" && !finished) return false;
    if (filter && key === "") return false;
    if (filter?.overdue && key !== "overdue") return false;
    if (filter && !filter.overdue && !overlaps(band(key), range(filter))) return false;
    return true;
  });
}

// ---- ordering -----------------------------------------------------------------

type Term = { value: (t: WorkItem) => string | number | undefined; desc: boolean };

/** The order rows are drawn in (`sortTerms`): the caller's, else the default, then the id. */
function sortTerms(q: Query): Term[] {
  const terms: Term[] = [];
  for (const { key, desc } of q.sort) {
    const column = SORT_COLUMNS[key];
    if (column) terms.push({ value: column.value, desc });
  }
  if (terms.length === 0) {
    if (q.removed) terms.push({ value: SORT_COLUMNS.removed!.value, desc: true });
    else if (q.project) terms.push({ value: SORT_COLUMNS.rank!.value, desc: false });
    else terms.push({ value: SORT_COLUMNS.updated!.value, desc: true });
  }
  terms.push({ value: (t) => t.id, desc: false });
  return terms;
}

/** Sorted by the terms, an absent value last in either direction. */
function sortRows(rows: DemoItem[], terms: Term[]): DemoItem[] {
  return [...rows].sort((a, b) => {
    for (const term of terms) {
      const x = term.value(a.task);
      const y = term.value(b.task);
      if (x === y) continue;
      if (x === undefined) return 1;
      if (y === undefined) return -1;
      return (x < y ? -1 : 1) * (term.desc ? -1 : 1);
    }
    return 0;
  });
}

/** The demo's cursor: where the next page starts, bound to the order it was drawn in. */
function cursorOf(offset: number, order: string): string {
  return btoa(JSON.stringify({ o: offset, s: order }));
}

function offsetOf(cursor: string, order: string): number {
  if (!cursor) return 0;
  try {
    const decoded = JSON.parse(atob(cursor)) as { o?: unknown; s?: unknown };
    if (typeof decoded.o === "number" && decoded.s === order) return decoded.o;
  } catch {
    // Refused below.
  }
  return refuse("This cursor was not issued for this order: start from the first page again.");
}

// ---- totals -------------------------------------------------------------------

function totalsOf(rows: DemoItem[], entries: string[], warn: (term: string) => void): WorkTotal[] {
  const out: WorkTotal[] = [];
  for (const entry of entries) {
    const at = entry.indexOf(":");
    const column = entry.slice(0, at).trim();
    const op = entry
      .slice(at + 1)
      .trim()
      .toLowerCase();
    if (!TOTAL_OPS.includes(op))
      refuse(`"${op}" is not an aggregate: the seven are ${TOTAL_OPS.join(", ")}.`);
    const total: WorkTotal = { key: entry, column, op };
    if (column.startsWith(FIELD_PREFIX)) {
      warn(entry);
      out.push(total);
      continue;
    }
    let values: number[];
    if (column === "tasks" && op === "count") {
      values = rows.map(() => 1);
    } else {
      const read = TOTAL_COLUMNS[column];
      if (!read) {
        return refuse(
          `"${column}" is not a column a total may be taken over; the columns are ${Object.keys(TOTAL_COLUMNS).sort().join(", ")}, or f.<slug>, or tasks:count.`,
        );
      }
      if (INSTANT_COLUMNS.has(column) && !["min", "max", "median", "p90"].includes(op)) {
        refuse(
          `${column} is a date, and ${op} over dates is not a time: the four that answer an instant are min, max, median and p90.`,
        );
      }
      values = rows.map((r) => read(r.task)).filter((v): v is number => v !== undefined);
    }
    let value: number | undefined;
    if (op === "count") value = values.length;
    else if (values.length > 0) {
      const sorted = [...values].sort((a, b) => a - b);
      const sum = values.reduce((s, v) => s + v, 0);
      if (op === "sum") value = sum;
      else if (op === "avg") value = sum / values.length;
      else if (op === "min") value = sorted[0];
      else if (op === "max") value = sorted[sorted.length - 1];
      else {
        // The nearest rank: position ⌈rank·n/100⌉, 1-based.
        const rank = RANK_OF[op]!;
        value = sorted[Math.ceil((rank * sorted.length) / 100) - 1];
      }
    }
    if (value !== undefined) {
      if (INSTANT_COLUMNS.has(column)) total.at = new Date(value).toISOString();
      else total.value = value;
    }
    out.push(total);
  }
  return out;
}

// ---- the answer -------------------------------------------------------------------

/** The declared custom field a reference names, by id, slug or name (`resolveOneField`). */
function fieldOf(c: DemoCompany, ref: string): WorkFieldDef | undefined {
  const fields = [...c.work.fields, ...c.work.projects.flatMap((p) => p.fields ?? [])];
  return (
    fields.find((f) => f.id === ref) ??
    fields.find((f) => f.slug === ref) ??
    fields.find((f) => f.name.toLowerCase() === ref.toLowerCase())
  );
}

/** One page of a work query: rows, or a board's columns when it names a group. */
export function itemsAnswer(c: DemoCompany, params: Params, now: Date): WorkItemsAnswer {
  const doc = documentOf(c);
  const zone = typeof doc.timezone === "string" && doc.timezone ? doc.timezone : "UTC";
  const clock: Clock = { now, zone, today: dateIn(now, zone) };

  // `viewer` is the SURFACE's, never the grammar's: taken out of the bag
  // before the grammar reads it, and resolved through the chart.
  const { viewer: asked, ...bag } = params;
  const handle = typeof asked === "string" ? asked.trim() : "";
  const viewer: Viewer = { handle, project: handle ? projectOfSeat(doc, handle) : "" };
  // ONE NAME PER PERSON: a person bound to a seat writes AS the seat
  // (`iam.ActorFor`), so the seat is every name they asked or ranked under.
  const q = parseQuery(expand(c, bag, viewer), clock, false);

  const warned = new Set<string>();
  const warn = (term: string) => warned.add(term);
  const terms: string[] = [
    ...q.fields,
    ...q.any.flatMap((arm) => arm.fields),
    ...q.sort.map((s) => s.key).filter((k) => k.startsWith(FIELD_PREFIX)),
    ...[q.groupBy, q.groupBy2].filter((k) => k.startsWith(FIELD_PREFIX)),
    ...q.totals.map((t) => t.split(":")[0]!.trim()).filter((k) => k.startsWith(FIELD_PREFIX)),
  ];
  for (const term of terms) {
    const ref = term.slice(FIELD_PREFIX.length);
    if (!fieldOf(c, ref)) {
      refuse(
        `f.${ref} names no field this company declares: a filter nothing resolved would widen this answer silently, so it is refused.`,
      );
    }
    warn(term);
  }

  const dayStart = resolveDate("today", clock).at;
  const ctx: Context = {
    c,
    clock,
    dayStart,
    dayEnd: resolveDate("tomorrow", clock).at,
    weekEnd: resolveDate("eow", clock).at,
    waitedOn: new Map(),
    priorityList: null,
  };
  for (const item of c.work.items) {
    for (const blocker of blockers(ctx, item)) {
      const list = ctx.waitedOn.get(blocker.task.id) ?? [];
      list.push(item);
      ctx.waitedOn.set(blocker.task.id, list);
    }
  }
  if (q.priorityListOf.length > 0) {
    const person = q.priorityListOf.map((name) => c.work.people[name]).find(Boolean);
    ctx.priorityList = person?.priorities ?? [];
  }

  // THE SUBTREE RULE: outside `separate`, and unless the caller asked about a
  // subtree, a row is in the answer when its ROOT is, under its own
  // tombstone and in the same container.
  const rooted = q.subtasks !== "separate" && !q.parent && !q.root;
  const live = (item: DemoItem) => (q.removed ? Boolean(item.task.removed) : !item.task.removed);
  let matched = c.work.items.filter((item) =>
    rooted
      ? live(item) &&
        (!q.project || item.task.project === q.project) &&
        holds(ctx, rootOf(c, item), q, false)
      : holds(ctx, item, q, false),
  );
  const axis = q.groupBy && !q.groupBy.startsWith(FIELD_PREFIX) ? axisOf(ctx, q.groupBy) : null;
  if (axis && q.group !== undefined) {
    const column = axis.canonical(q.group);
    matched = matched.filter((item) => axis.keys(item).includes(column));
  }

  const order = sortTerms(q);
  const signature = JSON.stringify(q.sort);
  const sorted = sortRows(matched, order);
  const fields = q.rowFields;
  const row = (item: DemoItem) => summaryOf(c, item, now, fields);
  const limit = Math.min(q.limit > 0 ? q.limit : PAGE_DEFAULT, PAGE_MAX);

  const answer: WorkItemsAnswer = {
    items: [],
    total_hint: Math.min(matched.length, TOTAL_HINT_CEILING),
    ...trackerEnvelope(c),
  };
  if (matched.length > TOTAL_HINT_CEILING) answer.total_capped = true;
  if (q.view) answer.view = q.view;
  if (q.preset) answer.preset = q.preset;

  // The drawing order a task's neighbours are read from: the segments in the
  // order the board draws them, each in the answer's sort.
  let segments: DemoItem[][] = [sorted];
  let listed: DemoItem[] | null = null;
  if (axis) {
    const drawn = drawGroups(ctx, q, axis, sorted, row);
    answer.groups = drawn.groups;
    if (drawn.dropped > 0) answer.groups_dropped = drawn.dropped;
    if (axis.multi) answer.groups_overlap = true;
    segments = drawn.segments;
  } else if (q.priorityListOf.length > 0) {
    const at = new Map((ctx.priorityList ?? []).map((id, i) => [id, i]));
    listed = sorted
      .slice(0, MAX_PRIORITIES)
      .sort((a, b) => (at.get(a.task.id) ?? Infinity) - (at.get(b.task.id) ?? Infinity));
    const offset = offsetOf(q.cursor, signature);
    answer.items = listed.slice(offset, offset + limit).map(row);
    if (offset + limit < listed.length) answer.next_cursor = cursorOf(offset + limit, signature);
  } else {
    const offset = offsetOf(q.cursor, signature);
    answer.items = sorted.slice(offset, offset + limit).map(row);
    if (offset + limit < sorted.length) answer.next_cursor = cursorOf(offset + limit, signature);
  }

  if (q.around) answer.around = aroundOf(c, q, listed ?? null, segments, answer);
  if (q.totals.length > 0) answer.totals = totalsOf(matched, q.totals, warn);

  for (const term of warned) {
    console.warn(
      LOG,
      `work_items: the custom field term "${term}" is not modelled in this prototype, so the answer leaves it out.`,
    );
  }
  return answer;
}

/** A board's columns (`readGroups`), and the segments its drawing order runs through. */
function drawGroups(
  ctx: Context,
  q: Query,
  axis: Axis,
  sorted: DemoItem[],
  row: (item: DemoItem) => WorkSummary,
): { groups: WorkGroup[]; dropped: number; segments: DemoItem[][] } {
  const rowsPer = Math.min(q.groupLimit > 0 ? q.groupLimit : GROUP_ROWS_DEFAULT, GROUP_ROWS_MAX);
  const columns = q.groupBy2 ? MAX_GROUPS_WITH_SUBGROUPS : MAX_GROUPS;
  const counted = countColumns(sorted, axis, axis.order);
  const dropped = Math.max(0, counted.length - columns);
  const present = counted.slice(0, columns);
  // The columns a board draws even when empty, in their declared place.
  const admitted = admittedColumns(ctx, q);
  const filled: { key: string; members: DemoItem[] }[] = admitted.map(
    (key) => present.find((g) => g.key === key) ?? { key, members: [] },
  );
  for (const g of present) if (!admitted.includes(g.key)) filled.push(g);

  const inner = q.groupBy2 && !q.groupBy2.startsWith(FIELD_PREFIX) ? axisOf(ctx, q.groupBy2) : null;
  const segments: DemoItem[][] = [];
  const groups = filled.map(({ key, members }) => {
    const group: WorkGroup = {
      key,
      count: members.length,
      rows: members.slice(0, rowsPer).map(row),
    };
    const label = key === "" ? axis.unset : (axis.labels?.(key) ?? "");
    if (label) group.label = label;
    if (inner && members.length > 0) {
      let lanesOf = members;
      if (q.subgroup !== undefined) {
        const lane = inner.canonical(q.subgroup);
        lanesOf = members.filter((item) => inner.keys(item).includes(lane));
      }
      const lanes = countColumns(lanesOf, inner, []);
      const kept = lanes.slice(0, MAX_SUBGROUPS);
      group.subgroups = kept.map((lane) => {
        const out: WorkGroup = {
          key: lane.key,
          count: lane.members.length,
          rows: lane.members.slice(0, rowsPer).map(row),
        };
        const laneLabel = lane.key === "" ? inner.unset : (inner.labels?.(lane.key) ?? "");
        if (laneLabel) out.label = laneLabel;
        if (lane.members.length > 0) segments.push(lane.members);
        return out;
      });
      if (lanes.length > MAX_SUBGROUPS) group.subgroups_dropped = lanes.length - MAX_SUBGROUPS;
    } else if (members.length > 0) {
      segments.push(members);
    }
    return group;
  });
  return { groups, dropped, segments };
}

/** Every column a set falls in, with its members in the set's order (`groupCounts`). */
function countColumns(
  rows: DemoItem[],
  axis: Axis,
  order: string[],
): { key: string; members: DemoItem[] }[] {
  const columns = new Map<string, DemoItem[]>();
  for (const item of rows) {
    for (const key of new Set(axis.keys(item))) {
      const members = columns.get(key) ?? [];
      members.push(item);
      columns.set(key, members);
    }
  }
  const list = [...columns].map(([key, members]) => ({ key, members }));
  if (order.length > 0) {
    const at = (key: string) => (order.includes(key) ? order.indexOf(key) : order.length);
    return list.sort(
      (a, b) => at(a.key) - at(b.key) || (a.key < b.key ? -1 : a.key > b.key ? 1 : 0),
    );
  }
  return list.sort(
    (a, b) => b.members.length - a.members.length || (a.key < b.key ? -1 : a.key > b.key ? 1 : 0),
  );
}

/** One task's place in the answer's drawing order (`readAround`), or null when it is not drawn. */
function aroundOf(
  c: DemoCompany,
  q: Query,
  listed: DemoItem[] | null,
  segments: DemoItem[][],
  answer: WorkItemsAnswer,
): WorkAround | null {
  const target = findItem(c, q.around);
  if (!target) return null;
  const capped = (n: number) => ({
    total_hint: Math.min(n, TOTAL_HINT_CEILING),
    ...(n > TOTAL_HINT_CEILING ? { total_capped: true } : {}),
  });
  if (listed) {
    const at = listed.indexOf(target);
    if (at < 0) return null;
    return {
      position: at + 1,
      prev: listed[at - 1]?.task.key ?? null,
      next: listed[at + 1]?.task.key ?? null,
      ...capped(listed.length),
    };
  }
  // A board counts CARDS, which on a label board exceeds the tasks; a list
  // counts the answer's own total.
  const total = answer.groups
    ? capped(segments.reduce((sum, s) => sum + s.length, 0))
    : {
        total_hint: answer.total_hint,
        ...(answer.total_capped ? { total_capped: true } : {}),
      };
  let ahead = 0;
  for (const [i, segment] of segments.entries()) {
    const at = segment.indexOf(target);
    if (at < 0) {
      ahead += segment.length;
      continue;
    }
    const position = ahead + at + 1;
    const next = segment[at + 1] ?? segments.slice(i + 1).find((s) => s.length > 0)?.[0];
    const prev =
      segment[at - 1] ??
      segments
        .slice(0, i)
        .reverse()
        .find((s) => s.length > 0)
        ?.at(-1);
    return {
      position: position <= TOTAL_HINT_CEILING ? position : null,
      prev: prev?.task.key ?? null,
      next: next?.task.key ?? null,
      ...total,
    };
  }
  return null;
}

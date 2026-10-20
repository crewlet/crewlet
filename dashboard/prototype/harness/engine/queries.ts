/**
 * Every question the dashboard's socket can ask, answered from the demo
 * company.
 *
 * THE TABLE IS KEYED BY `QueryName` AND THE COMPILER HOLDS IT WHOLE: a
 * question the protocol module declares and this table does not answer is a
 * type error, and so is an answer of the wrong shape. Parameters are read as
 * the engine's registry reads them (`internal/api/queries`); one the demo has
 * no use for is ignored.
 */

import { ARTIFACT_QUERIES } from "./artifacts.ts";
import type {
  EventRecord,
  Page,
  PageSummary,
  QueryMap,
  QueryName,
  SeatActivityDay,
  SeatActivityRow,
  WorkView,
} from "~/protocol/types.ts";
import type { Coverage } from "~/contract/coverage.ts";
import { documentOf } from "./company.ts";
import type { DemoEngine } from "./engine.ts";
import { DemoRefusal, goQuote } from "./refusal.ts";
import { int, refuse, text as param } from "./params.ts";
import { derive, roster, scheduleRows, scopeNameOf, companyZone } from "./derive.ts";
import { recordOwnerOf, viewerOf } from "./principal.ts";
import { addDays, budgetsOf, dateIn, liveRollupOf, rollupOf, seriesOf } from "./spend.ts";
import { configDiff, configEntities, redacted, revisionsNewestFirst } from "./config.ts";
import { integrationsAnswer } from "./integrations.ts";
import {
  activityOf,
  BUILTIN_TYPES,
  companyFeedOf,
  decisionsOf,
  detailOf,
  findItem,
  findProject,
  flowOf,
  inboxOf,
  myWorkOf,
  personOf,
  projectDetail,
  projectRow,
  searchOf,
  trackerEnvelope,
  workloadOf,
} from "./work.ts";
import { itemsAnswer } from "./workquery.ts";

type Params = Record<string, unknown>;
type Answer<K extends QueryName> = (engine: DemoEngine, params: Params) => QueryMap[K];

const text = (value: unknown): string => (typeof value === "string" ? value : "");

/** The schedule history the schedules answer carries (`queries.RecentRunsLimit`). */
const RECENT_RUNS_LIMIT = 50;
/** One schedule's history page, at most (`queries.MaxScheduleRuns`). */
const MAX_SCHEDULE_RUNS = 50;
const count = (value: unknown, fallback: number): number => {
  const n = typeof value === "number" ? value : Number(value);
  return Number.isFinite(n) && n > 0 ? n : fallback;
};

/** Every answer covers the one node the demo runs. */
function coverage(engine: DemoEngine): Coverage {
  return {
    nodes: [{ id: engine.company.engine.health.node, answered: true, error: "" }],
    complete: true,
  };
}

/** The seat a question names, by whichever key it used. */
function seatParam(params: Params): string {
  return (
    text(params.agent_id) ||
    text(params.handle) ||
    text(params.seat) ||
    text(params.id) ||
    text(params.role)
  );
}

/** A seat's handle from a handle or a role name. */
function handleFor(engine: DemoEngine, key: string): string {
  const row = roster(documentOf(engine.company)).find(
    (r) => r.handle === key || r.role === key || r.id === key || r.agent_id === key,
  );
  return row?.handle ?? key;
}

function seat(engine: DemoEngine, key: string) {
  const handle = handleFor(engine, key);
  const found = engine.company.seats[handle];
  if (!found) throw new DemoRefusal("not_found");
  return { handle, seat: found };
}

/** Events matching the event log's own filters, newest first. */
function eventsMatching(engine: DemoEngine, params: Params): EventRecord[] {
  const category = text(params.category);
  const source = text(params.source);
  const type = text(params.type);
  const seatKey = text(params.seat);
  const agentId = seatKey
    ? (roster(documentOf(engine.company)).find((r) => r.handle === seatKey)?.agent_id ?? seatKey)
    : "";
  const since = text(params.since);
  const until = text(params.until);
  return engine.company.events.filter(
    (e) =>
      (!category || category.split(",").includes(e.category)) &&
      (!source || e.source === source) &&
      (!type || type.split(",").includes(e.type)) &&
      (!agentId || e.tags?.agent_id === agentId) &&
      (!since || e.timestamp >= since) &&
      (!until || e.timestamp < until),
  );
}

/** The phase records of a seat, or of the whole company. */
function phaseRecords(engine: DemoEngine, handle?: string): EventRecord[] {
  const wanted = handle
    ? roster(documentOf(engine.company)).find((r) => r.handle === handle)?.agent_id
    : "";
  return engine.company.events.filter(
    (e) => e.type === "agent_phase_completed" && (!wanted || e.tags?.agent_id === wanted),
  );
}

/** A page as a listing draws it. */
function summaryOfPage(engine: DemoEngine, page: Page): PageSummary {
  const { body, watchers: _w, muted: _m, created_at: _c, trashed_at: _t, ...summary } = page;
  const children = engine.company.pages.pages.filter((p) => p.page.parent_id === page.id).length;
  const out: PageSummary = { ...summary };
  if (children > 0) out.children = children;
  if (!body) out.folder = true;
  return out;
}

/** The builtin views every container has, beside the saved ones on it. */
function viewsOf(engine: DemoEngine, container: string): WorkView[] {
  const [kind = "project", id = ""] = container.split(":");
  const at = { kind, id };
  const builtin: WorkView[] = [
    { key: "list", name: "List", type: "list", container: at, builtin: true },
    { key: "board", name: "Board", type: "board", container: at, builtin: true, default: true },
    { key: "calendar", name: "Calendar", type: "calendar", container: at, builtin: true },
    {
      key: "timeline",
      name: "Timeline",
      type: "timeline",
      container: at,
      builtin: true,
      params: { sort: "start" },
    },
    { key: "table", name: "Table", type: "table", container: at, builtin: true },
    {
      key: "trash",
      name: "Trash",
      type: "table",
      container: at,
      builtin: true,
      params: { removed: "true", show_closed: "true" },
    },
  ];
  const saved = engine.company.work.views.filter(
    (v) => v.container.kind === kind && v.container.id === id,
  );
  return [...builtin, ...saved];
}

/**
 * One seat's activity over the `days` company days ending `today`, and the
 * `days` before them: the seat's stored row holds its whole day history, and
 * the window's totals are summed from it as the engine sums the usage
 * domain's days.
 */
function windowed(
  row: SeatActivityRow,
  today: string,
  days: number,
  previous: boolean,
): SeatActivityRow {
  const byDay = new Map(row.per_day.map((d) => [d.day, d]));
  const span = (from: number) =>
    Array.from({ length: days }, (_, i) => {
      const day = addDays(today, from + i);
      return byDay.get(day) ?? { day, turns: 0, failed: 0, tokens: 0 };
    });
  const sum = (list: SeatActivityDay[], key: "turns" | "failed" | "tokens") =>
    list.reduce((n, d) => n + d[key], 0);
  const current = span(-(days - 1));
  const { previous: _stored, ...rest } = row;
  const out: SeatActivityRow = {
    ...rest,
    per_day: current,
    turns: sum(current, "turns"),
    failed: sum(current, "failed"),
    tokens: sum(current, "tokens"),
  };
  if (previous) {
    const before = span(-(2 * days - 1));
    out.previous = {
      turns: sum(before, "turns"),
      failed: sum(before, "failed"),
      reviewed: 0,
      first_pass: 0,
      sent_back: 0,
      tokens: sum(before, "tokens"),
      per_day: before,
    };
  }
  return out;
}

/** The window a spend question names: its own days, or the last `days` company days. */
function spendWindow(engine: DemoEngine, params: Params): { from: string; to: string } {
  const zone = companyZone(documentOf(engine.company));
  const today = dateIn(engine.now(), zone);
  const from = text(params.from);
  const to = text(params.to);
  if (from && to) return { from, to };
  const days = count(params.days, 30);
  return { from: addDays(today, -(days - 1)), to: today };
}

export const QUERIES: { [K in QueryName]: Answer<K> } = {
  ...ARTIFACT_QUERIES,
  viewer: (e) => viewerOf(e.company),

  work_inbox: (e, p) => inboxOf(e.company, p, e.now()),

  // ASKED BY HANDLE, ANSWERED BY THE AGENT ID that handle resolves to.
  agent: (e, p) => {
    const { handle, seat: s } = seat(e, seatParam(p));
    const row = e.agentRows().find((r) => r.handle === handle);
    return {
      handle,
      agent_id: row?.agent_id ?? "",
      role: row?.role ?? handle,
      live: s.overlay,
      llm_history: s.history,
      next: "",
      coverage: coverage(e),
    };
  },

  // ASKED AND ANSWERED BY AGENT ID, the key every overlay is merged by.
  live_call: (e, p) => {
    const { handle, seat: s } = seat(e, seatParam(p));
    const row = e.agentRows().find((r) => r.handle === handle);
    return {
      agent_id: row?.agent_id ?? "",
      handle,
      live_call: s.overlay.live_call ?? null,
      live_call_seq: s.overlay.live_call_seq ?? 0,
    };
  },

  agent_memory: (e, p) => seat(e, seatParam(p)).seat.memory,

  agent_episode: (e, p) => {
    const { handle, seat: s } = seat(e, seatParam(p));
    return { handle, episode: s.episode, held_by: e.company.engine.health.node };
  },

  memory_overview: (e) => ({
    seats: Object.entries(e.company.seats).map(([handle, s]) => ({ handle, ...s.overview })),
    coverage: coverage(e),
  }),

  events: (e, p) => {
    const limit = count(p.limit, 50);
    const before = text(p.before_time);
    const rows = eventsMatching(e, p).filter((ev) => !before || ev.timestamp < before);
    const page = rows.slice(0, limit);
    const last = page[page.length - 1];
    return {
      events: page.map(({ payload: _p, ...row }) => ({ ...row, failed: row.failed ?? false })),
      next:
        rows.length > limit && last ? { before_time: last.timestamp, before_id: last.id } : null,
      exhausted: rows.length <= limit,
      coverage: coverage(e),
    };
  },

  event_series: (e, p) => {
    const now = e.now();
    const until = text(p.until) || now.toISOString();
    const since = text(p.since) || new Date(Date.parse(until) - 86_400_000).toISOString();
    const span = Date.parse(until) - Date.parse(since);
    const bucket = span > 3 * 86_400_000 ? "day" : span > 3 * 3_600_000 ? "hour" : "minute";
    const size = bucket === "day" ? 86_400_000 : bucket === "hour" ? 3_600_000 : 60_000;
    const start = Math.floor(Date.parse(since) / size) * size;
    const end = Math.ceil(Date.parse(until) / size) * size;
    const rows = eventsMatching(e, {
      ...p,
      since: new Date(start).toISOString(),
      until: new Date(end).toISOString(),
    });
    const bars = [];
    for (let at = start; at < end; at += size) {
      const inBar = rows.filter(
        (r) => Date.parse(r.timestamp) >= at && Date.parse(r.timestamp) < at + size,
      );
      bars.push({
        at: new Date(at).toISOString(),
        count: inBar.length,
        failed: inBar.filter((r) => r.failed).length,
      });
    }
    const byCategory: Record<string, number> = {};
    for (const r of eventsMatching(e, { ...p, category: "" })) {
      if (Date.parse(r.timestamp) >= start && Date.parse(r.timestamp) < end) {
        byCategory[r.category] = (byCategory[r.category] ?? 0) + 1;
      }
    }
    return {
      bucket,
      since: new Date(start).toISOString(),
      until: new Date(end).toISOString(),
      bars,
      total: rows.length,
      failed: rows.filter((r) => r.failed).length,
      by_category: byCategory,
      coverage: coverage(e),
    };
  },

  event: (e, p) => {
    const found = e.company.events.find((ev) => ev.id === text(p.id));
    if (!found) throw new DemoRefusal("not_found");
    return { ...found, coverage: coverage(e) };
  },

  trace: (e, p) => ({
    trace_id: text(p.trace_id),
    events: e.company.events.filter((ev) => ev.trace_id === text(p.trace_id)).reverse(),
    truncated: false,
    coverage: coverage(e),
  }),

  turn: (e, p) => {
    const id = text(p.turn_id) || text(p.id);
    const row = e.company.turns.find((t) => t.turn_id === id);
    const events = e.company.events.filter((ev) => ev.tags?.turn_id === id).reverse();
    if (!row && events.length === 0) throw new DemoRefusal("not_found");
    const answer: QueryMap["turn"] = {
      turn_id: id,
      events,
      truncated: false,
      trace_ids: [...new Set(events.map((ev) => ev.trace_id).filter(Boolean))],
      coverage: coverage(e),
      nodes: [e.company.engine.health.node],
    };
    if (row?.work_key) {
      answer.work_key = row.work_key;
      answer.attempts = e.company.turns.filter((t) => t.work_key === row.work_key).reverse();
    }
    return answer;
  },

  turns: (e, p) => {
    const key = seatParam(p);
    const agentId = key
      ? roster(documentOf(e.company)).find((r) => r.handle === key)?.agent_id
      : "";
    const before = text(p.before);
    const rows = e.company.turns.filter(
      (t) =>
        (!key || t.agent_id === agentId || t.role === key) && (!before || t.started_at < before),
    );
    const limit = count(p.limit, 50);
    const page = rows.slice(0, limit);
    return {
      turns: page,
      next: rows.length > limit ? (page[page.length - 1]?.started_at ?? null) : null,
      coverage: coverage(e),
    };
  },

  phases: (e, p) => ({
    phases: phaseRecords(e, text(p.seat) || undefined).slice(0, count(p.limit, 60)),
    next: {},
    exhausted: true,
    coverage: coverage(e),
  }),

  tokens: (e, p) => {
    const now = e.now();
    if (!p.from && !p.to && !p.days && !p.range) return liveRollupOf(e.company, now);
    const { from, to } = spendWindow(e, p);
    return rollupOf(e.company, from, to, now, text(p.seat) || undefined);
  },

  token_series: (e, p) => {
    const { from, to } = spendWindow(e, p);
    return seriesOf(
      e.company,
      {
        group: text(p.group) || "phase",
        bucket: p.bucket === "week" ? "week" : "day",
        from,
        to,
        ...(text(p.seat) ? { seat: text(p.seat) } : {}),
      },
      e.now(),
    );
  },

  seat_activity: (e, p) => {
    const zone = companyZone(documentOf(e.company));
    const days = count(p.days, 7);
    const today = dateIn(e.now(), zone);
    const wanted = text(p.seat);
    const previous = p.previous === true || p.previous === "true";
    const seats = Object.entries(e.company.seats)
      .filter(([handle]) => !wanted || handle === wanted)
      .map(([, s]) => windowed(s.activity, today, days, previous));
    const answer: QueryMap["seat_activity"] = {
      since: addDays(today, -(days - 1)),
      until: today,
      days,
      seats,
      quantile_resolution: 0.06,
    };
    if (previous) {
      answer.previous_since = addDays(today, -(2 * days - 1));
      answer.previous_until = addDays(today, -days);
    }
    return answer;
  },

  // THE DEMO APPLIES A REVISION THE MOMENT IT IS STORED, so every node reports
  // the epoch the fleet is on: a screen waiting for the fleet to converge
  // after a save sees it converge.
  fleet: (e) => {
    const { epoch, revisions } = e.company.config;
    const active = revisions[revisions.length - 1]?.meta;
    return {
      ...e.company.engine.fleet,
      nodes: e.company.engine.fleet.nodes.map((node) => ({
        ...node,
        config_epoch: epoch,
        config_status: "applied",
        config_revision_id: active?.revision_id ?? "",
        config_reported_at: active?.activated_at ?? e.company.now,
      })),
      target_epoch: epoch,
    };
  },
  fleet_broker: (e) => e.company.engine.broker,
  mcp_servers_status: (e) => e.company.engine.mcp,
  credential_pool: (e) => e.company.engine.credentials,
  backups: (e) => e.company.engine.backups,
  budgets: (e) => budgetsOf(e.company, e.now()),

  schedules: (e) => ({
    schedules: scheduleRows(documentOf(e.company), e.now()),
    recent_runs: e.company.scheduleRuns.slice(0, RECENT_RUNS_LIMIT),
  }),

  // THE SCOPE TUPLE IS THE SCHEDULE'S IDENTITY (`queries.scheduleRuns`): a
  // name alone would merge two units' `standup` histories into one list.
  schedule_runs: (e, p) => {
    const scope = param(p, "scope_type").trim();
    if (scope !== "role" && scope !== "unit") {
      refuse(
        scope
          ? `queries: bad parameters: scope_type=${goQuote(scope)} (want one of role, unit)`
          : "queries: bad parameters: scope_type is required",
      );
    }
    const scopeId = param(p, "scope_id").trim();
    const name = param(p, "name").trim();
    if (!scopeId) refuse("queries: bad parameters: scope_id is required");
    if (!name) refuse("queries: bad parameters: name is required");
    let limit = int(p, "limit", 0);
    if (limit <= 0 || limit > MAX_SCHEDULE_RUNS) limit = MAX_SCHEDULE_RUNS;
    const runs = e.company.scheduleRuns
      .filter((r) => r.scope_type === scope && r.scope_id === scopeId && r.schedule_name === name)
      .slice(0, limit);
    return {
      scope_type: scope,
      scope_id: scopeId,
      scope_name: scopeNameOf(documentOf(e.company), scope, scopeId),
      schedule_name: name,
      runs,
      // SAYS WHEN IT CUT: a page that filled reads as a schedule that fired exactly that often.
      truncated: runs.length === limit,
    };
  },

  integrations: (e) => integrationsAnswer(e.company, e.now()),

  sandbox_runs: (e) => ({ runs: e.company.runs.records }),

  sandbox_tail: (_e, p) => ({
    outcome: "not_running",
    turn_id: text(p.turn_id),
    launch_id: text(p.launch_id),
  }),

  retention: (e) => e.company.engine.retention,

  work_items: (e, p) => itemsAnswer(e.company, p, e.now()),

  work_item: (e, p) => {
    const item = findItem(e.company, text(p.id) || text(p.key));
    if (!item) throw new DemoRefusal("not_found");
    return detailOf(e.company, item, e.now());
  },

  work_comments: (e, p) => {
    const item = findItem(e.company, text(p.item) || text(p.id));
    if (!item) throw new DemoRefusal("not_found");
    return {
      item: item.task.id,
      key: item.task.key,
      title: item.task.title,
      comments: item.comments.slice(-count(p.limit, 50)),
      ...trackerEnvelope(e.company),
    };
  },

  work_item_turns: (e, p) => {
    const item = findItem(e.company, text(p.id) || text(p.item));
    if (!item) throw new DemoRefusal("not_found");
    return {
      item: item.task.id,
      key: item.task.key,
      turns: item.turns.slice(0, count(p.limit, 20)),
      ...trackerEnvelope(e.company),
    };
  },

  work_views: (e, p) => ({
    views: viewsOf(e, text(p.container) || "workspace:"),
    ...trackerEnvelope(e.company),
  }),

  work_files: (e, p) => ({
    project: text(p.project),
    files: e.company.work.files.filter((f) => f.project === text(p.project)),
    ...trackerEnvelope(e.company),
  }),

  work_saved_views: (e) => ({ views: e.company.work.views, ...trackerEnvelope(e.company) }),

  work_projects: (e, p) => {
    const archived = p.archived === true || p.archived === "true";
    const q = text(p.q).toLowerCase();
    const all = e.company.work.projects.filter(
      (project) => !q || `${project.key} ${project.name}`.toLowerCase().includes(q),
    );
    const rows = all.filter((project) => Boolean(project.archived) === archived);
    return {
      projects: rows.slice(0, count(p.limit, 100)).map((project) => projectRow(e.company, project)),
      total: rows.length,
      census: {
        active: all.filter((x) => !x.archived).length,
        archived: all.filter((x) => x.archived).length,
      },
      ...trackerEnvelope(e.company),
    };
  },

  work_project: (e, p) => {
    const project = findProject(e.company, text(p.key) || text(p.project) || text(p.id));
    if (!project) throw new DemoRefusal("not_found");
    return projectDetail(e.company, project);
  },

  work_workload: (e) => workloadOf(e.company, e.now()),
  work_activity: (e, p) => activityOf(e.company, p),
  work_my_work: (e, p) =>
    myWorkOf(e.company, text(p.handle) || recordOwnerOf(e.company.principal), e.now()),

  work_catalogue: (e) => ({
    types: [...BUILTIN_TYPES, ...e.company.work.types],
    fields: e.company.work.fields,
    policy_version: 1,
    types_version: 1,
    fields_version: 1,
    ...trackerEnvelope(e.company),
  }),

  work_person: (e, p) =>
    personOf(e.company, text(p.handle) || recordOwnerOf(e.company.principal), e.now()),
  work_search: (e, p) => searchOf(e.company, text(p.q), text(p.mode)),

  work_routing: (e, p) => {
    const id = text(p.record_id);
    const recipients = Object.entries(e.company.work.inbox).flatMap(([handle, notices]) =>
      notices
        .filter((n) => n.record_id === id)
        .map((n) => ({
          handle,
          reason: n.reason,
          addressed: n.addressed,
          ...(n.excerpt ? { excerpt: n.excerpt } : {}),
        })),
    );
    const change = e.company.work.items.flatMap((i) => i.history).find((c) => c.id === id);
    return {
      record_id: id,
      held: Boolean(change),
      notified: recipients.length > 0,
      recipients,
      delivery: change ? (recipients.length > 0 ? "reached" : "quiet") : "unknown",
      addressed: recipients.filter((r) => r.addressed).length,
      fallback: 0,
      ...trackerEnvelope(e.company),
    };
  },

  work_flow: (e, p) => flowOf(e.company, p, e.now()),
  company_feed: (e) => companyFeedOf(e.company),
  decisions: (e, p) =>
    decisionsOf(e.company, text(p.handle) || recordOwnerOf(e.company.principal), e.now()),

  pages: (e, p) => {
    const container = text(p.container);
    const parent = text(p.parent);
    const title = text(p.title).toLowerCase() || text(p.q).toLowerCase();
    const skills = p.skills === true || p.skills === "true";
    const status = text(p.status);
    const rows = e.company.pages.pages
      .map((dp) => dp.page)
      .filter(
        (page) =>
          (!container || page.container === container) &&
          (p.parent === undefined || (page.parent_id ?? "") === parent) &&
          (!title || page.title.toLowerCase().includes(title)) &&
          Boolean(page.skill) === skills &&
          (status ? page.status === status : page.status !== "trashed"),
      )
      .sort((a, b) => b.updated_at.localeCompare(a.updated_at));
    const limit = count(p.limit, 50);
    const answer: QueryMap["pages"] = {
      pages: rows.slice(0, limit).map((page) => summaryOfPage(e, page)),
      limit,
      total: rows.length,
      position: e.company.pages.logSeq,
      log_lag: 0,
      complete: true,
    };
    if (skills) {
      answer.skill_loaded_by = Object.fromEntries(
        e.company.pages.pages
          .filter((dp) => dp.page.skill)
          .map((dp) => [dp.page.id, dp.skillLoads ?? []]),
      );
    }
    return answer;
  },

  page: (e, p) => {
    const found = e.company.pages.pages.find((dp) => dp.page.id === text(p.id));
    if (!found) throw new DemoRefusal("not_found");
    const ancestors: PageSummary[] = [];
    for (let at = found.page.parent_id; at;) {
      const parent = e.company.pages.pages.find((dp) => dp.page.id === at);
      if (!parent) break;
      ancestors.unshift(summaryOfPage(e, parent.page));
      at = parent.page.parent_id;
    }
    const children = e.company.pages.pages.filter((dp) => dp.page.parent_id === found.page.id);
    const detail: QueryMap["page"] = {
      page: found.page,
      revision: found.page.revision,
      skill: Boolean(found.page.skill),
      onboarding: Boolean(found.page.onboarding),
      comments: found.comments,
      history: found.revisions.map(({ body: _b, title: _t, ...meta }) => meta),
      children: children.map((dp) => summaryOfPage(e, dp.page)),
      children_total: children.length,
      ancestors,
      linked_from: {
        pages: [],
        tasks: e.company.work.items
          .filter((i) => i.links.some((l) => l.kind === "page" && l.other === found.page.id))
          .map((i) => ({
            id: i.task.id,
            key: i.task.key,
            title: i.task.title,
            status: i.task.status,
            via: ["link"],
          })),
        pages_total: 0,
        tasks_total: 0,
      },
    };
    if (!found.page.body) detail.folder = true;
    if (found.skillLoads) detail.skill_loaded_by = found.skillLoads;
    detail.linked_from!.tasks_total = detail.linked_from!.tasks.length;
    return detail;
  },

  containers: (e) => ({
    containers: e.company.pages.containers.map((container) => ({
      ...container,
      pages: e.company.pages.pages.filter(
        (dp) => dp.page.container === container.key && dp.page.status !== "trashed",
      ).length,
    })),
  }),

  page_activity: (e, p) => {
    const page = text(p.page);
    const container = text(p.container);
    const changes = e.company.pages.pages
      .filter(
        (dp) => (!page || dp.page.id === page) && (!container || dp.page.container === container),
      )
      .flatMap((dp) => dp.changes)
      .sort((a, b) => b.at.localeCompare(a.at));
    return {
      changes: changes.slice(0, count(p.limit, 50)),
      complete: true,
    };
  },

  page_reads: (e, p) => {
    const found = e.company.pages.pages.find((dp) => dp.page.id === text(p.page));
    const zone = companyZone(documentOf(e.company));
    const today = dateIn(e.now(), zone);
    const readers = found?.reads ?? [];
    return {
      page: text(p.page),
      since: addDays(today, -29),
      until: today,
      days: 30,
      readers,
      readers_total: readers.length,
      distinct_seats_today: 0,
      elided: 0,
    };
  },

  page_revision: (e, p) => {
    const found = e.company.pages.pages.find(
      (dp) => dp.page.id === text(p.page) || dp.page.id === text(p.id),
    );
    const version = count(p.version, found?.page.version ?? 1);
    const revision = found?.revisions.find((r) => r.version === version);
    if (!found || !revision) throw new DemoRefusal("not_found");
    const answer: QueryMap["page_revision"] = {
      page_id: found.page.id,
      version,
      title: revision.title,
      body: revision.body,
      created_at: revision.created_at,
    };
    if (revision.message) answer.message = revision.message;
    if (revision.author) answer.author = revision.author;
    return answer;
  },

  conversations: (e, p) => {
    const { handle, seat: s } = seat(e, seatParam(p));
    return {
      handle,
      conversations: s.conversations.rows,
      conversations_total: s.conversations.rows.length,
      entries: s.conversations.entries,
      held_by: e.company.engine.health.node,
    };
  },

  a2a_channels: (e, p) => {
    const state = text(p.state) || "all";
    return {
      channels: e.company.a2a.filter(
        (ch) => state === "all" || (state === "open" ? !ch.closed_at : Boolean(ch.closed_at)),
      ),
      available: true,
      state,
      truncated: false,
    };
  },

  knowledge: (e, p) => {
    const q = text(p.q).trim().toLowerCase();
    const mode = p.mode === "keyword" || p.mode === "semantic" ? p.mode : "hybrid";
    const words = q.split(/\s+/).filter(Boolean);
    const hits = q
      ? e.company.pages.hits.filter((hit) =>
          words.some((w) => `${hit.title} ${hit.snippet}`.toLowerCase().includes(w)),
        )
      : [];
    return {
      backend: "native",
      query: text(p.q),
      hits,
      available: true,
      reason: "",
      note: "",
      mode,
      served_mode: mode,
      modes: ["hybrid", "keyword", "semantic"],
      degraded: "",
      coverage: { ...coverage(e), buckets_missing: 0 },
    };
  },

  colleague: (e, p) => {
    const q = text(p.q).trim().toLowerCase().replace(/^@/, "");
    const seats = derive(documentOf(e.company)).seats ?? [];
    const exact = seats.filter((s) => s.handle === q || s.name.toLowerCase() === q);
    const near = q
      ? seats.filter(
          (s) => !exact.includes(s) && (s.handle.includes(q) || s.name.toLowerCase().includes(q)),
        )
      : [];
    const candidates = [
      ...exact.map((s) => ({
        handle: s.handle,
        why: s.handle === q ? "handle matches exactly" : "name matches exactly",
      })),
      ...near.map((s) => ({ handle: s.handle, why: "name contains the text" })),
    ];
    return { match: exact.length === 1 ? candidates[0]! : null, candidates };
  },

  config: (e) => redacted(documentOf(e.company)),
  config_audit: (e) => revisionsNewestFirst(e.company),
  config_diff: (e, p) => configDiff(e.company, text(p.revision_id), text(p.against) || "active"),
  config_entities: (e, p) => configEntities(e.company, text(p.kind), text(p.id)),
};

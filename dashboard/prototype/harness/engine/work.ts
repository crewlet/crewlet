/**
 * The native tracker's answers, derived from the demo company's tasks.
 *
 * Every row a screen reads about work (a board's card, a project's counts, an
 * ask waiting in somebody's inbox, a My work block) is derived here from the
 * same tasks, comments and history, so a write to one task moves every answer
 * that names it, as it does against the engine. The query grammar is the
 * subset the dashboard sends (`internal/tracker/query.go` holds the whole of
 * it): a parameter outside that subset is ignored rather than refused.
 */

import type {
  CompanyFeedAnswer,
  DecisionsAnswer,
  FeedEntry,
  WorkActivityAnswer,
  WorkActivityRecord,
  WorkAskRow,
  WorkAskView,
  WorkBlocker,
  WorkChange,
  WorkComment,
  WorkDelta,
  WorkFlowAnswer,
  WorkInboxAnswer,
  WorkInboxNotice,
  WorkItem,
  WorkItemDetail,
  WorkloadAnswer,
  WorkMyWork,
  WorkPersonState,
  WorkProjectDetail,
  WorkProjectRow,
  WorkSearchAnswer,
  WorkStatusDef,
  WorkSummary,
  WorkTypeDef,
} from "~/protocol/types.ts";
import { documentOf, type DemoCompany, type DemoItem, type DemoProject } from "./company.ts";
import { derive } from "./derive.ts";
import { recordOwnerOf } from "./principal.ts";

/** The tracker's six statuses, as `internal/tracker/policy.go` states them. */
export const STATUSES: WorkStatusDef[] = [
  {
    status: "todo",
    label: "To do",
    group: "not_started",
    description: "not started; nobody is working on it yet",
  },
  {
    status: "in_progress",
    label: "In progress",
    group: "active",
    description: "somebody is doing the work now",
  },
  {
    status: "in_review",
    label: "In review",
    group: "active",
    description: "the work is done and awaits a colleague's check",
  },
  {
    status: "done",
    label: "Done",
    group: "done",
    description: "delivered; the work is finished and accepted",
  },
  {
    status: "cancelled",
    label: "Cancelled",
    group: "done",
    description: "finished WITHOUT being delivered: abandoned, a duplicate, or cancelled",
  },
  {
    status: "closed",
    label: "Closed",
    group: "closed",
    description: "totally completed and out of the way",
  },
];

/** The task types this build ships (`internal/tracker/catalogue.go`). */
export const BUILTIN_TYPES: WorkTypeDef[] = [
  ["task", "Task", "Tasks", "Ordinary work with an owner and an end."],
  ["bug", "Bug", "Bugs", "Something is wrong and somebody has to make it right."],
  ["epic", "Epic", "Epics", "A body of work with tasks under it."],
  ["story", "Story", "Stories", "A change stated as what somebody gets from it."],
  ["spike", "Spike", "Spikes", "Time boxed to answer a question, not to deliver."],
  ["chore", "Chore", "Chores", "Upkeep nobody asked for and everybody needs."],
  [
    "milestone",
    "Milestone",
    "Milestones",
    "A date the company committed to, and what has to land before it.",
  ],
].map(([slug, name, plural, description]) => ({
  slug: slug!,
  name: name!,
  plural: plural!,
  description: description!,
  builtin: true,
}));

/** The tracker's log, which a tracker write's position names. */
export const TRACKER_STREAM = "CREWLET_TRACKER_LOG";

/** A position as the engine spells it, `<stream>@<generation>:<sequence>`. */
export function positionOf(stream: string, seq: number): string {
  return `${stream}@1:${seq}`;
}

/** The status group a status is in. */
export function groupOf(status: string): string {
  return STATUSES.find((s) => s.status === status)?.group ?? "not_started";
}

const OPEN_GROUPS = new Set(["not_started", "active"]);

/**
 * A task moved to `status` at `at`: its group, when it entered it, and the two
 * finish stamps, which the engine sets by GROUP with the first entry winning
 * and a reopen clearing both (`stampFinish`), so "finished this week" agrees
 * with the state a task is in.
 */
export function enterStatus(task: WorkItem, status: string, at: string): void {
  task.status = status;
  task.status_group = groupOf(status) as WorkItem["status_group"];
  task.status_entered_at = at;
  switch (task.status_group) {
    case "done":
      task.done_at ??= at;
      delete task.closed_at;
      break;
    case "closed":
      task.done_at ??= at;
      task.closed_at ??= at;
      break;
    default:
      delete task.done_at;
      delete task.closed_at;
  }
}

/** Whether a task is still to be done. */
export function isOpen(item: DemoItem): boolean {
  return OPEN_GROUPS.has(groupOf(item.task.status));
}

/**
 * The envelope every tracker answer carries: complete, at the log's head. The
 * level it was served at is the read's own (`freshness.ts`), stated by
 * `DemoEngine.answer`.
 */
export function trackerEnvelope(c: DemoCompany) {
  return {
    log_seq: c.work.logSeq,
    applied_through: c.work.logSeq,
    log_lag: 0,
    complete: true,
  };
}

/** A task by id or key, keys compared without case. */
export function findItem(c: DemoCompany, ref: string): DemoItem | null {
  const wanted = ref.trim().toLowerCase();
  return c.work.items.find((i) => i.task.id === ref || i.task.key.toLowerCase() === wanted) ?? null;
}

export function findProject(c: DemoCompany, key: string): DemoProject | null {
  return c.work.projects.find((p) => p.key.toLowerCase() === key.toLowerCase()) ?? null;
}

/** The tasks this one waits on, each with whether it still holds this one up. */
function blockersOf(c: DemoCompany, item: DemoItem): WorkBlocker[] {
  return item.links
    .filter((l) => l.kind === "waiting_on")
    .map((l) => {
      const other = findItem(c, l.other);
      return { id: l.other, open: other ? isOpen(other) : false };
    });
}

/** The day `iso` falls on, `YYYY-MM-DD`, on the company clock. */
export function dayOf(iso: string, zone: string): string {
  return new Intl.DateTimeFormat("en-CA", {
    timeZone: zone,
    year: "numeric",
    month: "2-digit",
    day: "2-digit",
  }).format(new Date(iso));
}

function zoneOf(c: DemoCompany): string {
  const tz = documentOf(c).timezone;
  return typeof tz === "string" && tz ? tz : "UTC";
}

/** Whether an open task is past its due day on the company clock. */
function overdue(c: DemoCompany, item: DemoItem, now: Date): boolean {
  if (!item.task.due_at || !isOpen(item)) return false;
  const zone = zoneOf(c);
  return dayOf(item.task.due_at, zone) < dayOf(now.toISOString(), zone);
}

/** Open asks on a task: questions nobody has answered or resolved. */
export function openAsks(item: DemoItem): WorkComment[] {
  return item.comments.filter(
    (comment) =>
      comment.ask &&
      !comment.resolved &&
      !comment.removed &&
      !item.comments.some((answer) => answer.answers === comment.id),
  );
}

/** The opt-in facts a row carries when a read asks for them (`tracker.RowFieldNames`). */
export const ROW_FIELDS = ["tags", "dependents_count", "open_asks", "spend"];

/** A task as a board row draws it, with the opt-in `fields` it was asked for. */
export function summaryOf(
  c: DemoCompany,
  item: DemoItem,
  now: Date,
  fields: readonly string[] = [],
): WorkSummary {
  const t = item.task;
  const blockers = blockersOf(c, item);
  const row: WorkSummary = {
    id: t.id,
    key: t.key,
    project: t.project,
    title: t.title,
    type: t.type ?? "task",
    status: t.status,
    status_group: groupOf(t.status),
    priority: t.priority ?? "none",
    updated: t.updated_at ?? t.created_at ?? c.now,
    version: t.version,
    blocked: blockers.some((b) => b.open),
    overdue: overdue(c, item, now),
  };
  if (t.assignee) row.assignee = t.assignee;
  if (t.parent) row.parent = t.parent;
  if (t.depth) row.depth = t.depth;
  if (t.start_at) row.start = t.start_at;
  if (t.due_at) row.due = t.due_at;
  if (t.estimate_minutes) row.estimate_min = t.estimate_minutes;
  if (t.points) row.points = t.points;
  if (blockers.length > 0) row.waiting_on = blockers;
  if (t.archived) row.archived = true;
  if (t.rank) row.rank = t.rank;
  if (fields.includes("tags")) row.tags = t.tags ?? [];
  if (fields.includes("dependents_count")) row.dependents_count = (t.dependents ?? []).length;
  if (fields.includes("open_asks")) row.open_asks = openAsks(item).length;
  if (fields.includes("spend")) {
    row.spend = {
      tokens: t.spend?.tokens ?? 0,
      turns: t.spend?.turns ?? 0,
      workers: t.spend?.workers ?? 0,
      sent_back: t.spend?.sent_back ?? 0,
      reopens: 0,
    };
  }
  return row;
}

/** The project a `container=project:<KEY>` parameter names, or null. */
export function containerProject(container: unknown): string | null {
  if (typeof container !== "string") return null;
  const [kind, id] = container.split(":");
  return kind === "project" && id ? id : null;
}

/** An ask as it stands now: open, or closed by the answer that names it. */
export function askView(item: DemoItem, ask: WorkComment): WorkAskView {
  const answer = item.comments.find((c) => c.answers === ask.id);
  const view: WorkAskView = {
    comment: ask.id,
    asked_of: ask.ask ?? "",
    open: !answer && !ask.resolved,
  };
  if (answer) {
    view.answered_by = answer.author;
    view.answered_at = answer.created_at;
    if (answer.choice) view.choice = answer.choice;
  } else if (ask.resolved) {
    view.resolved = true;
    if (ask.resolved_by) view.answered_by = ask.resolved_by;
    if (ask.resolved_at) view.answered_at = ask.resolved_at;
  }
  if (ask.decision) view.decision = ask.decision;
  return view;
}

/** An ask as a row of the person's decisions. */
export function askRow(c: DemoCompany, item: DemoItem, ask: WorkComment, now: Date): WorkAskRow {
  const row: WorkAskRow = {
    ...summaryOf(c, item, now),
    comment: ask.id,
    asked_by: ask.author,
    asked_at: ask.created_at,
    body: ask.body,
    open: askView(item, ask).open,
    answer_with: ask.decision
      ? `comment_on_work_item(item="${item.task.key}", answers="${ask.id}", choice="<option id>")`
      : `comment_on_work_item(item="${item.task.key}", answers="${ask.id}", body="<your answer>")`,
  };
  if (ask.decision) row.decision = ask.decision;
  return row;
}

/** The open asks put to `handle`, newest first. */
export function asksOf(c: DemoCompany, handle: string, now: Date): WorkAskRow[] {
  return c.work.items
    .flatMap((item) =>
      openAsks(item)
        .filter((ask) => ask.ask === handle)
        .map((ask) => askRow(c, item, ask, now)),
    )
    .sort((a, b) => b.asked_at.localeCompare(a.asked_at));
}

/** What waits on `handle`'s decision: the open asks to them, and the runs parked on a question to them. */
export function decisionsOf(c: DemoCompany, handle: string, now: Date): DecisionsAnswer {
  const asks = asksOf(c, handle, now).map((ask) => ({
    kind: "ask" as const,
    at: ask.asked_at,
    ask,
  }));
  const runs = c.runs.records
    .filter(
      (run) =>
        run.status === "awaiting_clarification" && (run.audience_handles ?? []).includes(handle),
    )
    .map((run) => ({ kind: "run" as const, at: run.paused_at || run.started_at, run }));
  const items = [...asks, ...runs].sort((a, b) => b.at.localeCompare(a.at));
  const answer: DecisionsAnswer = {
    handle,
    items,
    total: items.length,
    capped: false,
    complete: true,
  };
  const oldest = items[items.length - 1];
  if (oldest) answer.oldest_at = oldest.at;
  return answer;
}

/** One task, whole, as its page draws it. */
export function detailOf(c: DemoCompany, item: DemoItem, now: Date): WorkItemDetail {
  const doc = documentOf(c);
  const units = derive(doc).units ?? [];
  // A TASK IS FILED UNDER A UNIT'S KEY, resolved against the chart for its name.
  const unitRef = (key: string | undefined) => {
    const unit = units.find((u) => u.id === key);
    return unit ? { key: unit.id, name: unit.name, resolved: true } : { resolved: false };
  };
  const keys: Record<string, string> = {};
  for (const change of item.history) {
    for (const value of Object.values(change.fields ?? {})) {
      for (const id of [
        (value as WorkDelta).from,
        (value as WorkDelta).to,
        ...((value as WorkDelta).added ?? []),
      ]) {
        const other = typeof id === "string" ? findItem(c, id) : null;
        if (other && other.task.id === id) keys[id] = other.task.key;
      }
    }
  }
  const detail: WorkItemDetail = {
    task: item.task,
    comments: item.comments,
    history: item.history,
    links: item.links.map((link) => {
      const other = findItem(c, link.other);
      return other
        ? { ...link, key: other.task.key, title: other.task.title, status: other.task.status }
        : link;
    }),
    blocked: blockersOf(c, item).some((b) => b.open),
    reassignment_budget: 8,
    ...trackerEnvelope(c),
  };
  if (Object.keys(keys).length > 0) detail.keys = keys;
  if (item.task.filed_unit || item.task.routing_unit) {
    detail.units = {
      filed: unitRef(item.task.filed_unit),
      routing: unitRef(item.task.routing_unit),
    };
  }
  const parent = item.task.parent ? findItem(c, item.task.parent) : null;
  if (parent) {
    detail.parent = {
      id: parent.task.id,
      key: parent.task.key,
      title: parent.task.title,
      status: parent.task.status,
    };
  }
  if (item.task.due_at) {
    const zone = zoneOf(c);
    const days = Math.round(
      (Date.parse(dayOf(item.task.due_at, zone)) - Date.parse(dayOf(now.toISOString(), zone))) /
        86_400_000,
    );
    detail.due_standing = { days, overdue: overdue(c, item, now) };
  }
  return detail;
}

/** The unit that owns a project, by its key, as the chart names it now. */
function projectUnit(c: DemoCompany, key: string): WorkProjectRow["unit"] {
  const unit = (derive(documentOf(c)).units ?? []).find((u) => u.id === key);
  return unit
    ? { key: unit.id, name: unit.name, resolved: true }
    : { key, name: key, resolved: false };
}

/** A project's row, its counts derived from its tasks. */
export function projectRow(c: DemoCompany, project: DemoProject): WorkProjectRow {
  const items = c.work.items.filter((i) => i.task.project === project.key && !i.task.removed);
  const count = (group: string) => items.filter((i) => groupOf(i.task.status) === group).length;
  const changes = items.flatMap((i) => i.history).sort((a, b) => b.at.localeCompare(a.at));
  const newest = changes[0];
  const row: WorkProjectRow = {
    key: project.key,
    name: project.name,
    unit: projectUnit(c, project.unit),
    lead: { handle: project.lead, kind: "agent" },
    task_counts: {
      todo: count("not_started"),
      active: count("active"),
      done: count("done"),
      closed: count("closed"),
    },
    version: project.version,
  };
  if (project.purpose) row.purpose = project.purpose;
  if (project.default_assignee) row.default_assignee = project.default_assignee;
  if (project.target_date) row.target_date = project.target_date;
  if (project.archived) row.archived = true;
  if (newest) {
    row.last_change = { at: newest.at };
    if (newest.actor) row.last_change.actor = newest.actor;
    if (newest.actor_kind) {
      row.last_change.actor_kind = newest.actor_kind as "agent" | "human" | "operator" | "system";
    }
  }
  return row;
}

/** A project whole, as its settings and its board read it. */
export function projectDetail(c: DemoCompany, project: DemoProject): WorkProjectDetail {
  return {
    ...projectRow(c, project),
    statuses: project.statuses,
    types: project.types,
    fields: project.fields.length > 0 ? [{ fields: project.fields }] : [],
    tags: project.tags,
    policy_stamp: project.version,
    ...trackerEnvelope(c),
  };
}

/** Everything one person is expected to look at. */
export function myWorkOf(c: DemoCompany, handle: string, now: Date): WorkMyWork {
  const rows = (items: DemoItem[]) =>
    items.slice(0, 20).map((i) => summaryOf(c, i, now, ROW_FIELDS));
  const open = c.work.items.filter((i) => isOpen(i) && !i.task.removed);
  const priorities = (c.work.people[handle]?.priorities ?? []).flatMap((id) => {
    const found = findItem(c, id);
    return found && isOpen(found) ? [found] : [];
  });
  const assigned = open.filter((i) => i.task.assignee === handle);
  const collaborating = open.filter((i) => (i.task.collaborators ?? []).includes(handle));
  const watching = c.work.items.filter((i) => (i.task.watchers ?? []).includes(handle));
  const asked = asksOf(c, handle, now);
  const total = (n: number) => ({ total: n });
  return {
    handle,
    priorities: rows(priorities),
    assigned: rows(assigned),
    asked_of_me: asked.slice(0, 20),
    checklist_items: [],
    collaborating: rows(collaborating),
    watching_recent: rows(watching),
    unblocked_recent: [],
    totals: {
      priorities: total(priorities.length),
      assigned: total(assigned.length),
      asked_of_me: total(asked.length),
      checklist_items: total(0),
      collaborating: total(collaborating.length),
      watching_recent: total(watching.length),
      unblocked_recent: total(0),
    },
    ...trackerEnvelope(c),
  };
}

/** Every seat's open load. */
export function workloadOf(c: DemoCompany, now: Date): WorkloadAnswer {
  const handles = (derive(documentOf(c)).seats ?? []).map((s) => s.handle);
  const rows = handles.map((handle) => {
    const mine = c.work.items.filter(
      (i) => isOpen(i) && !i.task.removed && i.task.assignee === handle,
    );
    return {
      handle,
      open: mine.length,
      points: mine.reduce((n, i) => n + (i.task.points ?? 0), 0),
      estimate_min: mine.reduce((n, i) => n + (i.task.estimate_minutes ?? 0), 0),
      blocked: mine.filter((i) => blockersOf(c, i).some((b) => b.open)).length,
      overdue: mine.filter((i) => overdue(c, i, now)).length,
      unscheduled: mine.filter((i) => !i.task.due_at && !i.task.start_at).length,
    };
  });
  return { rows, ...trackerEnvelope(c) };
}

/** A notice as it reads now: its ask re-read, so an answered question reads closed. */
/** Every wake reason the engine knows (`tracker.Reasons`). */
export const WAKE_REASONS = [
  "mention",
  "prioritised",
  "blocking",
  "asked",
  "answered",
  "assignee",
  "unassigned",
  "reporter",
  "thread",
  "unblocked",
  "routed_to",
  "parent_assignee",
  "checklist",
  "collaborator",
  "watcher",
  "unwatched",
  "purged",
  "lead_fallback",
] as const;

/**
 * The split a person who has said nothing gets (`tracker.DefaultPrimaryReasons`).
 * A person's own EMPTY list also means this, never "nothing is primary".
 */
export const DEFAULT_PRIMARY_REASONS = [
  "mention",
  "asked",
  "answered",
  "assignee",
  "unassigned",
  "reporter",
  "unblocked",
  "prioritised",
];

/** The split a read of `handle`'s inbox applies: their own, or the default. */
function primaryReasonsOf(c: DemoCompany, handle: string): string[] {
  const declared = c.work.people[handle]?.primary_reasons ?? [];
  return [...(declared.length > 0 ? declared : DEFAULT_PRIMARY_REASONS)];
}

/**
 * A notice as a read answers it now: which half of the split it falls in is
 * decided at READ time from the person's split, as the engine decides it, so a
 * changed split moves every notice already delivered.
 */
function noticeNow(c: DemoCompany, notice: WorkInboxNotice, primary: string[]): WorkInboxNotice {
  const out: WorkInboxNotice = { ...notice, primary: primary.includes(notice.reason) };
  const item = findItem(c, notice.subject_id);
  if (!item) return out;
  if (!out.subject_key) out.subject_key = item.task.key;
  if (notice.ask) {
    const ask = item.comments.find((comment) => comment.id === notice.ask?.comment);
    if (ask) out.ask = askView(item, ask);
  }
  return out;
}

/** A page of one person's inbox. */
export function inboxOf(
  c: DemoCompany,
  params: Record<string, unknown>,
  now: Date,
): WorkInboxAnswer {
  const handle = typeof params.handle === "string" ? params.handle : recordOwnerOf(c.principal);
  const primary = primaryReasonsOf(c, handle);
  const all = (c.work.inbox[handle] ?? []).map((n) => noticeNow(c, n, primary));
  const snoozedNow = (n: WorkInboxNotice) =>
    Boolean(n.snoozed && n.snoozed_until && Date.parse(n.snoozed_until) > now.getTime());
  const notices = all.filter((n) => {
    if (params.unread === true && n.read) return false;
    if (params.unread === false && !n.read) return false;
    if (params.primary_only === true && !n.primary) return false;
    if (params.snoozed === "exclude" && snoozedNow(n)) return false;
    if (params.snoozed === "only" && !snoozedNow(n)) return false;
    return true;
  });
  const limit = typeof params.limit === "number" ? params.limit : 50;
  const page = notices.slice(0, limit);
  return {
    handle,
    notices: page,
    primary_reasons: primary,
    seen_through: { stream: TRACKER_STREAM, generation: 1, seq: 0 },
    unread: page.filter((n) => !n.read).length,
    primary: page.filter((n) => n.primary).length,
    ...trackerEnvelope(c),
  };
}

/** One person's own record. */
export function personOf(c: DemoCompany, handle: string, now: Date): WorkPersonState {
  const person = c.work.people[handle];
  const entry = (n: WorkInboxNotice) => {
    const e: { record_id: string; position: number; until?: string } = {
      record_id: n.record_id,
      position: n.log_seq,
    };
    if (n.snoozed_until) e.until = n.snoozed_until;
    return e;
  };
  const inbox = c.work.inbox[handle] ?? [];
  const snoozed = inbox.filter((n) => n.snoozed && n.snoozed_until);
  return {
    handle,
    unread: [],
    read: inbox.filter((n) => n.read).map(entry),
    snoozed: snoozed.filter((n) => Date.parse(n.snoozed_until!) > now.getTime()).map(entry),
    due: snoozed.filter((n) => Date.parse(n.snoozed_until!) <= now.getTime()).map(entry),
    ...(person?.primary_reasons?.length ? { primary_reasons: [...person.primary_reasons] } : {}),
    priorities: person?.priorities ?? [],
    pinned_views: person?.pinned_views ?? [],
    favorites: person?.favorites ?? [],
    version: person?.version ?? 0,
    max_snooze_ahead: 30 * 86_400,
    held: Boolean(person),
    ...trackerEnvelope(c),
  };
}

/** Every change to the work, as the activity feed reads it, newest first. */
export function activityOf(c: DemoCompany, params: Record<string, unknown>): WorkActivityAnswer {
  const task = typeof params.task === "string" ? findItem(c, params.task) : null;
  const project =
    typeof params.project === "string" ? params.project : containerProject(params.container);
  const items = task
    ? [task]
    : c.work.items.filter(
        (i) => !project || i.task.project.toLowerCase() === project.toLowerCase(),
      );
  const records: WorkActivityRecord[] = items
    .flatMap((item) => item.history.map((change) => activityRecord(item, change)))
    .sort((a, b) => b.log_seq - a.log_seq);
  const limit = typeof params.limit === "number" ? params.limit : 50;
  const keys: Record<string, string> = {};
  for (const item of c.work.items) keys[item.task.id] = item.task.key;
  return { records: records.slice(0, limit), keys, ...trackerEnvelope(c) };
}

function activityRecord(item: DemoItem, change: WorkChange): WorkActivityRecord {
  const record: WorkActivityRecord = {
    id: change.id,
    log_seq: change.log_seq,
    log_stream: TRACKER_STREAM,
    log_generation: 1,
    at: change.at,
    effective_at: change.at,
    kind: change.kind,
    subject_kind: "task",
    subject_id: item.task.id,
    subject_key: item.task.key,
    project: item.task.project,
    notified: !change.quiet,
  };
  if (change.actor) record.actor = change.actor;
  if (change.actor_kind) record.actor_kind = change.actor_kind;
  if (change.operator_id) record.operator_id = change.operator_id;
  if (change.excerpt) record.excerpt = change.excerpt;
  if (change.comment_id) {
    record.comment_id = change.comment_id;
    const comment = item.comments.find((x) => x.id === change.comment_id);
    if (comment?.ask) record.ask = askView(item, comment);
    const asked = comment?.answers
      ? item.comments.find((x) => x.id === comment.answers)
      : undefined;
    if (asked) record.ask = askView(item, asked);
  }
  if (change.turn_id) record.turn_id = change.turn_id;
  if (change.fields) record.fields = change.fields as Record<string, WorkDelta>;
  return record;
}

/** What the company did, newest first: task creations, completions and hand-offs, pages and schedule runs. */
export function companyFeedOf(c: DemoCompany): CompanyFeedAnswer {
  const rows: FeedEntry[] = [];
  for (const item of c.work.items) {
    for (const change of item.history) {
      const kind =
        change.kind === "created"
          ? "created"
          : change.kind === "status" &&
              (change.fields?.status as WorkDelta | undefined)?.to === "done"
            ? "completed"
            : change.kind === "assignee"
              ? "handoff"
              : null;
      if (!kind) continue;
      const work: NonNullable<FeedEntry["work"]> = {
        id: change.id,
        kind,
        at: change.at,
        cursor: String(change.log_seq),
        task: item.task.id,
        key: item.task.key,
        title: item.task.title,
        project: item.task.project,
      };
      if (change.actor) work.actor = change.actor;
      if (change.actor_kind) work.actor_kind = change.actor_kind;
      if (kind === "completed")
        work.spend = { tokens: item.task.spend?.tokens ?? 0, turns: item.task.spend?.turns ?? 0 };
      if (kind === "handoff") {
        const delta = change.fields?.assignee as WorkDelta | undefined;
        work.from = delta?.from ?? "";
        work.to = delta?.to ?? "";
      }
      rows.push({ kind, at: change.at, work });
    }
  }
  for (const page of c.pages.pages) {
    for (const change of page.changes) {
      if (change.kind !== "created" && change.kind !== "saved") continue;
      const entry: NonNullable<FeedEntry["page"]> = {
        id: change.id,
        page_id: page.page.id,
        title: page.page.title,
        container: page.page.container,
        change: change.kind,
      };
      if (change.actor) entry.actor = change.actor;
      if (change.actor_kind) entry.actor_kind = change.actor_kind;
      rows.push({ kind: "page", at: change.at, page: entry });
    }
  }
  for (const run of c.scheduleRuns) {
    if (run.outcome !== "fired") continue;
    rows.push({
      kind: "schedule",
      at: run.fired_at,
      schedule: {
        scope_type: run.scope_type,
        scope_id: run.scope_id,
        name: run.schedule_name,
        target: run.target_handle,
        outcome: run.outcome,
        trace_id: run.trace_id,
        runs: 1,
      },
    });
  }
  rows.sort((a, b) => b.at.localeCompare(a.at));
  return { rows: rows.slice(0, 50), complete: true };
}

/** The work as a daily series, replayed from each task's own dates. */
export function flowOf(c: DemoCompany, params: Record<string, unknown>, now: Date): WorkFlowAnswer {
  const zone = zoneOf(c);
  const points = Math.max(1, Math.min(90, Number(params.points) || 14));
  const items = c.work.items.filter((i) => !i.task.removed);
  const today = Date.parse(`${dayOf(now.toISOString(), zone)}T00:00:00Z`);
  const series = Array.from({ length: points }, (_, k) => {
    const start = new Date(today - (points - 1 - k) * 86_400_000);
    const end = new Date(start.getTime() + 86_400_000);
    const at = Math.min(end.getTime(), now.getTime());
    const exists = items.filter((i) => Date.parse(i.task.created_at ?? c.now) <= at);
    const doneBy = (i: DemoItem) => i.task.done_at && Date.parse(i.task.done_at) <= at;
    const active = (i: DemoItem) =>
      groupOf(i.task.status) === "active" &&
      Date.parse(i.task.status_entered_at ?? i.task.created_at ?? c.now) <= at;
    return {
      window: start.toISOString().slice(0, 10),
      start: start.toISOString(),
      end: end.toISOString(),
      not_started: exists.filter(
        (i) => !doneBy(i) && !active(i) && groupOf(i.task.status) !== "closed",
      ).length,
      active: exists.filter((i) => !doneBy(i) && active(i)).length,
      done: exists.filter((i) => doneBy(i)).length,
      closed: exists.filter((i) => groupOf(i.task.status) === "closed").length,
      completed: items.filter(
        (i) =>
          i.task.done_at &&
          Date.parse(i.task.done_at) >= start.getTime() &&
          Date.parse(i.task.done_at) < end.getTime(),
      ).length,
    };
  });
  const open = items.filter(isOpen);
  return {
    bucket: "day",
    points: series,
    now: {
      not_started: open.filter((i) => groupOf(i.task.status) === "not_started").length,
      active: open.filter((i) => groupOf(i.task.status) === "active").length,
      blocked: open.filter((i) => blockersOf(c, i).some((b) => b.open)).length,
      overdue: open.filter((i) => overdue(c, i, now)).length,
    },
    blocked_history: false,
    complete: true,
  };
}

/** The ranked item search: every task whose key, title or body holds the phrase. */
export function searchOf(c: DemoCompany, q: string, mode: string): WorkSearchAnswer {
  const phrase = q.trim().toLowerCase();
  const hits = phrase
    ? c.work.items
        .filter((i) =>
          `${i.task.key} ${i.task.title} ${i.task.body ?? ""}`.toLowerCase().includes(phrase),
        )
        .map((i, at) => {
          const hit: WorkSearchAnswer["hits"][number] = {
            id: i.task.id,
            key: i.task.key,
            title: i.task.title,
            project: i.task.project,
            type: i.task.type ?? "task",
            status: i.task.status,
            priority: i.task.priority ?? "none",
            rank: at + 1,
            snippet: i.task.title,
          };
          if (i.task.assignee) hit.assignee = i.task.assignee;
          return hit;
        })
    : [];
  const asked = mode === "keyword" || mode === "semantic" ? mode : "hybrid";
  return {
    hits,
    available: true,
    mode: asked,
    served_mode: asked,
    modes: ["hybrid", "keyword", "semantic"],
    degraded: "",
    coverage: {
      nodes: [{ id: "node-0", answered: true, error: "" }],
      complete: true,
      buckets_missing: 0,
    },
  };
}

/** Advance the tracker's log by one record and answer where it landed. */
export function appendTracker(c: DemoCompany): { seq: number; position: string } {
  c.work.logSeq += 1;
  return { seq: c.work.logSeq, position: positionOf(TRACKER_STREAM, c.work.logSeq) };
}

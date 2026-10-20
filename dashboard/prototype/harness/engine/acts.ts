/**
 * The changes a person makes from the dashboard, `POST /operator/act/{tool}`,
 * applied to the demo company.
 *
 * Every write the act transport serves is listed in `contract/actions.ts`;
 * the ones modelled here change the company the way the engine's tool does
 * and answer as the transport answers (`applied`, the position the record
 * landed at, and the tool's receipt), so the screen that pressed the button
 * re-reads exactly what it would re-read against an engine. A change made as
 * the person is attributed as the engine attributes it (`iam.ActorFor`): a
 * person bound to a seat is the seat, and the credential they acted through
 * is recorded beside it as the operator.
 *
 * A tool the contract lists and this file does not model is refused with a
 * sentence saying so, and logged; it never pretends to have worked.
 */

import { ACTIONS } from "~/contract/actions.ts";
import type { ActionTool } from "~/protocol/act.ts";
import type { WorkChange, WorkComment, WorkDecision, WorkInboxNotice } from "~/protocol/types.ts";
import type { DemoItem, DemoPerson } from "./company.ts";
import type { DemoEngine } from "./engine.ts";
import { uuidV7 } from "./ids.ts";
import { actorOf, recordOwnerOf, viewerOf } from "./principal.ts";
import { LOG, notModelled } from "./refusal.ts";
import type { RestReply, RestRequest } from "./rest.ts";
import { pause, resume } from "./seats.ts";
import {
  WAKE_REASONS,
  appendTracker,
  findItem,
  findProject,
  enterStatus,
  positionOf,
} from "./work.ts";

/** What one modelled tool answers: where its record landed and its receipt, or a refusal. */
type Outcome =
  | { position: string | null; receipt: unknown }
  | { refused: { status: number; error: string; detail: string } };

type Args = Record<string, unknown>;
type Handler = (engine: DemoEngine, args: Args) => Outcome;

const PAGES_STREAM = "CREWLET_PAGES_LOG";

const refuse = (status: number, error: string, detail: string): Outcome => ({
  refused: { status, error, detail },
});
const str = (value: unknown): string => (typeof value === "string" ? value : "");
const strings = (value: unknown): string[] =>
  Array.isArray(value) ? value.filter((v): v is string => typeof v === "string") : [];

/** Who a change is made as, and the credential that made it. */
const actor = (engine: DemoEngine) => actorOf(engine.company);

/** The name the browser's own record is kept under: its inbox, pins and priorities. */
const owner = (engine: DemoEngine) => recordOwnerOf(engine.company.principal);

/** A task change, recorded on the task's history at the log's next position. */
function record(
  engine: DemoEngine,
  item: DemoItem,
  change: Omit<WorkChange, "id" | "at" | "log_seq" | "actor">,
): string {
  const { seq, position } = appendTracker(engine.company);
  const who = actor(engine);
  item.history.push({
    ...change,
    id: uuidV7(),
    at: engine.now().toISOString(),
    log_seq: seq,
    actor: who.name,
    actor_kind: who.kind,
    operator_id: who.operator_id,
  });
  item.task.version = seq;
  item.task.updated_at = engine.now().toISOString();
  return position;
}

/** A person's own record, created empty by their first write as the engine creates it. */
function personRecord(engine: DemoEngine, handle: string): DemoPerson {
  return (engine.company.work.people[handle] ??= {
    priorities: [],
    pinned_views: [],
    favorites: [],
    version: 0,
  });
}

/** A notice in `handle`'s inbox about a change to `item`. */
function notify(
  engine: DemoEngine,
  handle: string,
  item: DemoItem,
  notice: Partial<WorkInboxNotice>,
): void {
  const who = actor(engine);
  const inbox = (engine.company.work.inbox[handle] ??= []);
  inbox.unshift({
    record_id: uuidV7(),
    log_seq: engine.company.work.logSeq,
    log_stream: "CREWLET_TRACKER_LOG",
    log_generation: 1,
    at: engine.now().toISOString(),
    reason: "thread",
    primary: false,
    addressed: false,
    kind: "comment",
    subject_id: item.task.id,
    subject_key: item.task.key,
    actor: who.name,
    actor_kind: who.kind,
    read: false,
    ...notice,
  });
  engine.pushInboxChanged(handle, {
    unread_delta: 1,
    subject: item.task.id,
    reason: notice.reason ?? "thread",
  });
}

function item(engine: DemoEngine, args: Args): DemoItem | null {
  return findItem(engine.company, str(args.item));
}

const HANDLERS: Partial<Record<ActionTool, Handler>> = {
  pause_seat: (engine, args) => {
    const handle = str(args.handle);
    const seat = engine.company.seats[handle];
    if (!seat) return refuse(404, "not_found", `There is no agent seat "${handle}".`);
    pause(
      seat,
      actor(engine),
      engine.now().toISOString(),
      str(args.reason),
      args.stop_running === true,
    );
    engine.pushSeat(handle);
    return { position: null, receipt: { handle, paused: true } };
  },

  resume_seat: (engine, args) => {
    const handle = str(args.handle);
    const seat = engine.company.seats[handle];
    if (!seat) return refuse(404, "not_found", `There is no agent seat "${handle}".`);
    resume(seat);
    engine.pushSeat(handle);
    return { position: null, receipt: { handle, paused: false } };
  },

  comment_on_work_item: (engine, args) => {
    const task = item(engine, args);
    if (!task) return refuse(404, "not_found", `There is no work item "${str(args.item)}".`);
    const answers = str(args.answers);
    const asked = answers ? task.comments.find((c) => c.id === answers) : undefined;
    if (answers && !asked)
      return refuse(404, "not_found", "That question is not on this work item.");
    if (asked && task.comments.some((c) => c.answers === asked.id)) {
      return refuse(409, "already_answered", "Somebody has already answered it.");
    }
    const choice = str(args.choice);
    const option = choice ? asked?.decision?.options.find((o) => o.id === choice) : undefined;
    if (choice && !option)
      return refuse(422, "invalid", `"${choice}" is not one of the question's options.`);
    const body = str(args.body) || (option ? option.label : "");
    if (!body && !choice) return refuse(422, "invalid", "A comment needs a body.");
    const who = actor(engine);
    const comment: WorkComment = {
      id: uuidV7(),
      task: task.task.id,
      author: who.name,
      author_kind: who.kind,
      body,
      created_at: engine.now().toISOString(),
    };
    if (asked) comment.answers = asked.id;
    if (choice) comment.choice = choice;
    if (str(args.reply_to)) comment.reply_to = str(args.reply_to);
    if (str(args.ask)) comment.ask = str(args.ask);
    if (args.decision && typeof args.decision === "object")
      comment.decision = args.decision as WorkDecision;
    task.comments.push(comment);
    const position = record(engine, task, {
      kind: "comment",
      actor_kind: who.kind,
      comment_id: comment.id,
      excerpt: body.slice(0, 160),
    });
    if (asked) {
      // Whoever asked hears the answer: a person bound to a seat asked AS the
      // seat, so the ask's author is whose inbox it lands in.
      notify(engine, asked.author, task, {
        reason: "answered",
        primary: true,
        comment_id: comment.id,
        excerpt: body.slice(0, 160),
      });
    }
    if (comment.ask) {
      notify(engine, comment.ask, task, {
        reason: "asked",
        primary: true,
        addressed: true,
        comment_id: comment.id,
      });
    }
    return { position, receipt: { id: comment.id, item: task.task.id, key: task.task.key } };
  },

  update_work_item: (engine, args) => {
    const task = item(engine, args);
    if (!task) return refuse(404, "not_found", `There is no work item "${str(args.item)}".`);
    if (args.if_match !== undefined && Number(args.if_match) !== task.task.version) {
      return refuse(409, "stale_version", "Changed by somebody else since you opened it.");
    }
    const t = task.task;
    let position: string | null = null;
    const now = engine.now().toISOString();
    if (typeof args.status === "string" && args.status !== t.status) {
      const from = t.status;
      enterStatus(t, args.status, now);
      position = record(engine, task, {
        kind: "status",
        fields: { status: { from, to: args.status } },
      });
    }
    if (typeof args.assignee === "string" && args.assignee !== (t.assignee ?? "")) {
      const from = t.assignee ?? "";
      if (args.assignee) t.assignee = args.assignee;
      else delete t.assignee;
      position = record(engine, task, {
        kind: "assignee",
        fields: { assignee: { from, to: args.assignee } },
      });
      if (args.assignee)
        notify(engine, args.assignee, task, {
          reason: "assignee",
          primary: true,
          kind: "assignee",
        });
    }
    const fields: Record<string, { from: string; to: string }> = {};
    for (const key of ["priority", "title", "body", "due", "start"] as const) {
      const value = args[key];
      if (typeof value !== "string") continue;
      const field = key === "due" ? "due_at" : key === "start" ? "start_at" : key;
      const from = String((t as unknown as Record<string, unknown>)[field] ?? "");
      if (from === value) continue;
      (t as unknown as Record<string, unknown>)[field] = value;
      fields[key] = { from, to: value };
    }
    if (Array.isArray(args.labels)) {
      fields.tags = { from: (t.tags ?? []).join(", "), to: strings(args.labels).join(", ") };
      t.tags = strings(args.labels);
    }
    if (typeof args.points === "number") t.points = args.points;
    if (typeof args.estimate_minutes === "number") t.estimate_minutes = args.estimate_minutes;
    if (typeof args.watch === "boolean") {
      const me = actor(engine).name;
      t.watchers = args.watch
        ? [...new Set([...(t.watchers ?? []), me])]
        : (t.watchers ?? []).filter((h) => h !== me);
    }
    if (Object.keys(fields).length > 0) position = record(engine, task, { kind: "fields", fields });
    for (const key of ["linked", "linked_pages", "checklist", "fields"] as const) {
      if (args[key] !== undefined) {
        console.warn(
          LOG,
          `update_work_item ignored "${key}", which this prototype does not model.`,
        );
      }
    }
    position ??= record(engine, task, { kind: "fields", fields: {}, quiet: true });
    return { position, receipt: { id: t.id, key: t.key, version: t.version } };
  },

  create_work_item: (engine, args) => {
    const c = engine.company;
    const projectKey = str(args.project) || viewerOf(c).project;
    const project = findProject(c, projectKey);
    if (!project)
      return refuse(422, "invalid", `There is no project "${projectKey}" to file it in.`);
    const title = str(args.title).trim();
    if (!title) return refuse(422, "invalid", "A work item needs a title.");
    const number =
      1 +
      Math.max(
        0,
        ...c.work.items
          .filter((i) => i.task.project === project.key)
          .map((i) => Number(i.task.key.split("-")[1]) || 0),
      );
    const now = engine.now().toISOString();
    const parent = str(args.parent) ? findItem(c, str(args.parent)) : null;
    const status = str(args.status) || "todo";
    const created: DemoItem = {
      task: {
        id: uuidV7(),
        key: `${project.key}-${number}`,
        project: project.key,
        filed_unit: project.unit,
        routing_unit: project.unit,
        type: str(args.type) || "task",
        title,
        status,
        priority: str(args.priority) || "none",
        reporter: actor(engine).name,
        watchers: [actor(engine).name],
        tags: strings(args.labels),
        created_at: now,
        updated_at: now,
        rank: `z${number}`,
        version: 0,
      },
      comments: [],
      history: [],
      links: [],
      turns: [],
    };
    enterStatus(created.task, status, now);
    if (str(args.body)) created.task.body = str(args.body);
    if (str(args.assignee)) created.task.assignee = str(args.assignee);
    if (str(args.due)) created.task.due_at = str(args.due);
    if (parent) {
      created.task.parent = parent.task.id;
      created.task.depth = (parent.task.depth ?? 0) + 1;
    }
    c.work.items.push(created);
    const position = record(engine, created, { kind: "created" });
    if (str(args.ask)) {
      const who = actor(engine);
      const ask: WorkComment = {
        id: uuidV7(),
        task: created.task.id,
        author: who.name,
        author_kind: who.kind,
        body: title,
        ask: str(args.ask),
        created_at: now,
      };
      created.comments.push(ask);
    }
    if (created.task.assignee)
      notify(engine, created.task.assignee, created, {
        reason: "assignee",
        primary: true,
        kind: "created",
      });
    return { position, receipt: { id: created.task.id, key: created.task.key } };
  },

  place_work_item: (engine, args) => {
    const c = engine.company;
    const task = item(engine, args);
    if (!task) return refuse(404, "not_found", `There is no work item "${str(args.item)}".`);
    if (typeof args.status === "string" && args.status !== task.task.status) {
      const from = task.task.status;
      enterStatus(task.task, args.status, engine.now().toISOString());
      record(engine, task, { kind: "status", fields: { status: { from, to: args.status } } });
    }
    const lane = c.work.items
      .filter(
        (i) =>
          i.task.project === task.task.project && i.task.status === task.task.status && i !== task,
      )
      .sort((a, b) => (a.task.rank ?? "").localeCompare(b.task.rank ?? ""));
    const before = str(args.before) ? findItem(c, str(args.before)) : null;
    const after = str(args.after) ? findItem(c, str(args.after)) : null;
    const at = before ? lane.indexOf(before) : after ? lane.indexOf(after) + 1 : lane.length;
    lane.splice(Math.max(0, at), 0, task);
    lane.forEach((i, n) => (i.task.rank = `m${String(n).padStart(4, "0")}`));
    const position = record(engine, task, { kind: "fields", fields: {}, quiet: true });
    return { position, receipt: { placed: true } };
  },

  set_priorities: (engine, args) => {
    const handle = str(args.handle) || owner(engine);
    const person = personRecord(engine, handle);
    if (args.if_match !== undefined && Number(args.if_match) !== person.version) {
      return refuse(409, "stale_version", "Changed by somebody else since you opened it.");
    }
    person.priorities = strings(args.items);
    const { seq, position } = appendTracker(engine.company);
    person.version = seq;
    return { position, receipt: { handle, version: seq } };
  },

  set_pins: (engine, args) => {
    const handle = owner(engine);
    const person = personRecord(engine, handle);
    if (Array.isArray(args.views)) person.pinned_views = strings(args.views);
    if (Array.isArray(args.favorites))
      person.favorites = args.favorites as { kind: string; id: string }[];
    const { seq, position } = appendTracker(engine.company);
    person.version = seq;
    return { position, receipt: { handle } };
  },

  save_work_view: (engine, args) => {
    const [kind = "project", id = ""] = str(args.container).split(":");
    const view = {
      id: uuidV7(),
      key: uuidV7(),
      name: str(args.name) || "Untitled view",
      type: (str(args.type) || "list") as "list",
      container: { kind, id },
      builtin: false,
      params: (args.params ?? {}) as Record<string, string>,
      ...(str(args.owner) ? { owner: str(args.owner) } : {}),
    };
    engine.company.work.views.push(view);
    const { position } = appendTracker(engine.company);
    return { position, receipt: { id: view.id } };
  },

  mark_inbox: (engine, args) => {
    const handle = owner(engine);
    // PRESENCE, NOT VALUE, decides the split: an empty list takes the default
    // back, and an absent one leaves the person's choice as it is.
    const split = "primary_reasons" in args ? strings(args.primary_reasons) : null;
    const unknown = split?.find((r) => !(WAKE_REASONS as readonly string[]).includes(r.trim()));
    if (unknown !== undefined) {
      return refuse(
        422,
        "invalid",
        `"${unknown}" is not a wake reason. The reasons are: ${WAKE_REASONS.join(", ")}.`,
      );
    }
    const inbox = engine.company.work.inbox[handle] ?? [];
    const set = (ids: string[], change: (n: WorkInboxNotice) => void) =>
      inbox.filter((n) => ids.includes(n.record_id)).forEach(change);
    set(strings(args.read), (n) => (n.read = true));
    set(strings(args.unread), (n) => (n.read = false));
    set(strings(args.unsnooze), (n) => {
      n.snoozed = false;
      delete n.snoozed_until;
    });
    for (const s of Array.isArray(args.snooze)
      ? (args.snooze as { record_id?: string; until?: string }[])
      : []) {
      const n = inbox.find((x) => x.record_id === s.record_id);
      if (n && s.until) {
        n.snoozed = true;
        n.snoozed_until = s.until;
      }
    }
    const through = /@\d+:(\d+)$/.exec(str(args.read_through));
    if (through) for (const n of inbox) if (n.log_seq <= Number(through[1])) n.read = true;
    const { seq, position } = appendTracker(engine.company);
    if (split) {
      const person = personRecord(engine, handle);
      person.primary_reasons = split.map((r) => r.trim());
      person.version = seq;
    }
    return { position, receipt: { handle } };
  },

  write_page: (engine, args) => {
    const c = engine.company;
    const container = str(args.container) || c.pages.containers[0]?.key || "";
    if (!c.pages.containers.some((x) => x.key === container)) {
      return refuse(422, "invalid", `There is no space "${container}" to write the page in.`);
    }
    const title = str(args.title).trim();
    if (!title) return refuse(422, "invalid", "A page needs a title.");
    if (c.pages.pages.some((p) => p.page.container === container && p.page.title === title)) {
      return refuse(409, "exists", "That already exists.");
    }
    c.pages.logSeq += 1;
    const now = engine.now().toISOString();
    const who = actor(engine);
    const id = uuidV7();
    const body = str(args.body);
    c.pages.pages.push({
      page: {
        id,
        container,
        title,
        status: str(args.status) || "published",
        author: who.name,
        version: 1,
        labels: strings(args.labels),
        updated_at: now,
        revision: 1,
        body,
        created_at: now,
        saved_by: who.name,
        saved_at: now,
        ...(str(args.parent) ? { parent_id: str(args.parent) } : {}),
      },
      comments: [],
      revisions: [{ version: 1, author: who.name, created_at: now, body, title }],
      changes: [
        {
          id: uuidV7(),
          page_id: id,
          title,
          container,
          kind: "created",
          actor: who.name,
          actor_kind: who.kind,
          operator_id: who.operator_id,
          at: now,
          log_seq: c.pages.logSeq,
        },
      ],
      reads: [],
    });
    return { position: positionOf(PAGES_STREAM, c.pages.logSeq), receipt: { id } };
  },

  save_page: (engine, args) => {
    const c = engine.company;
    const found = c.pages.pages.find((p) => p.page.id === str(args.page));
    if (!found) return refuse(404, "not_found", "That page is not there any more.");
    const who = actor(engine);
    const now = engine.now().toISOString();
    if (typeof args.watch === "boolean" && args.body === undefined) {
      const watchers = new Set(found.page.watchers ?? []);
      if (args.watch) watchers.add(who.name);
      else watchers.delete(who.name);
      found.page.watchers = [...watchers];
      c.pages.logSeq += 1;
      return { position: positionOf(PAGES_STREAM, c.pages.logSeq), receipt: { id: found.page.id } };
    }
    if (Number(args.base_version) !== found.page.version) {
      return refuse(409, "stale_version", "Changed by somebody else since you opened it.");
    }
    c.pages.logSeq += 1;
    found.page.version += 1;
    found.page.revision = found.page.version;
    found.page.body = str(args.body);
    found.page.updated_at = now;
    found.page.saved_at = now;
    found.page.saved_by = who.name;
    const message = str(args.message);
    found.revisions.push({
      version: found.page.version,
      author: who.name,
      created_at: now,
      body: found.page.body,
      title: found.page.title,
      ...(message ? { message } : {}),
    });
    found.changes.unshift({
      id: uuidV7(),
      page_id: found.page.id,
      title: found.page.title,
      container: found.page.container,
      kind: "saved",
      actor: who.name,
      actor_kind: who.kind,
      operator_id: who.operator_id,
      at: now,
      log_seq: c.pages.logSeq,
      ...(message ? { excerpt: message } : {}),
    });
    return {
      position: positionOf(PAGES_STREAM, c.pages.logSeq),
      receipt: { id: found.page.id, version: found.page.version },
    };
  },

  comment_on_page: (engine, args) => {
    const c = engine.company;
    const found = c.pages.pages.find((p) => p.page.id === str(args.page));
    if (!found) return refuse(404, "not_found", "That page is not there any more.");
    const body = str(args.body).trim();
    if (!body) return refuse(422, "invalid", "A comment needs a body.");
    const who = actor(engine);
    const now = engine.now().toISOString();
    const id = uuidV7();
    found.comments.push({
      id,
      page_id: found.page.id,
      author: who.name,
      author_kind: who.kind,
      body,
      created_at: now,
      updated_at: now,
      ...(str(args.reply_to) ? { reply_to: str(args.reply_to) } : {}),
    });
    c.pages.logSeq += 1;
    return { position: positionOf(PAGES_STREAM, c.pages.logSeq), receipt: { id } };
  },
};

/** What each tool is called in a refusal a person reads. */
const LABELS: Partial<Record<ActionTool, string>> = {
  restore_work_item: "Restoring a work item",
  write_project: "Changing a project's settings",
  answer_knowledge: "Answering a question from the knowledge base",
  answer_run: "Answering a parked coding run",
  steer_turn: "Steering a running turn",
};

/** Answer `POST /operator/act/{tool}` as the act transport does. */
export function handleAct(engine: DemoEngine, tool: string, request: RestRequest): RestReply {
  if (request.method !== "POST") return { status: 405, body: { error: "method_not_allowed" } };
  if (!Object.hasOwn(ACTIONS, tool)) {
    return {
      status: 404,
      body: { error: "unknown_tool", detail: `The act transport serves no tool "${tool}".` },
    };
  }
  let args: Args;
  try {
    const body = JSON.parse(request.body ?? "{}") as { args?: unknown };
    args = body.args && typeof body.args === "object" ? (body.args as Args) : {};
  } catch {
    return {
      status: 400,
      body: { error: "invalid_body", detail: "The change could not be read." },
    };
  }
  const handler = HANDLERS[tool as ActionTool];
  if (!handler) {
    const what = LABELS[tool as ActionTool] ?? `The change "${tool}"`;
    console.warn(LOG, `The act "${tool}" is not modelled; it was refused.`, args);
    return { status: 422, body: { error: "invalid", detail: notModelled(what) } };
  }
  const outcome = handler(engine, args);
  if ("refused" in outcome) {
    return {
      status: outcome.refused.status,
      body: { error: outcome.refused.error, detail: outcome.refused.detail },
    };
  }
  return {
    status: 200,
    body: { outcome: "applied", position: outcome.position, receipt: outcome.receipt },
  };
}

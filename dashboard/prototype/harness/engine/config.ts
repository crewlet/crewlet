/**
 * The configuration surface, `/config`, over the demo company's revisions.
 *
 * Every change to the company DOCUMENT (an org edit, a seat's ceiling, a
 * model, an MCP server) is a new revision: validated whole, stored with its
 * audit summary, activated at the next epoch, and pushed to every tab as the
 * `org`, `seats`, `agents`, `schedules` and `health` frames a real apply
 * pushes. The writes keep the engine's preconditions (`If-Match` on an edit,
 * `If-None-Match: *` on a create) and answer its refusals in its words, so
 * the builder's conflict and problem handling runs as it does in use.
 *
 * VALIDATION IS THE ENGINE'S STRUCTURAL HALF: unnamed and duplicated seats,
 * duplicated handles and units, the references that name nothing, the
 * fields a person's seat and an agent's may each carry, and every schedule's
 * shape and whether anybody can run it. What the demo cannot judge (a
 * provider's credentials) it lets through, as a document the engine would
 * accept.
 */

import type {
  CompanyDocument,
  ConfigChange,
  ConfigDiff,
  ConfigProblem,
  ConfigRole,
  ConfigUnit,
  ConfigWarning,
  RevisionMeta,
} from "~/protocol/types.ts";
import { documentOf, type DemoCompany } from "./company.ts";
import { derive, handleOf, unitKeyOf, unitRunnerFacts } from "./derive.ts";
import type { DemoEngine } from "./engine.ts";
import { goQuote as q } from "./refusal.ts";
import { appendRevision } from "./revisions.ts";
import type { RestReply, RestRequest } from "./rest.ts";
import { scheduleFaults } from "./schedules.ts";

/** The document as the guarded surface serves it. A fixture writes credentials as `${VAR}` already. */
export function redacted(doc: CompanyDocument): CompanyDocument {
  return structuredClone(doc);
}

/** The entity tag naming a revision, as the engine writes it. */
function etagOf(revision: string): string {
  return `"${revision}"`;
}

function activeId(c: DemoCompany): string {
  return c.config.revisions[c.config.revisions.length - 1]?.meta.revision_id ?? "";
}

/** Every revision's metadata, newest first, the active one marked. */
export function revisionsNewestFirst(c: DemoCompany): RevisionMeta[] {
  const active = activeId(c);
  return [...c.config.revisions]
    .reverse()
    .map((r) => ({ ...r.meta, is_active: r.meta.revision_id === active }));
}

const isRecord = (v: unknown): v is Record<string, unknown> =>
  typeof v === "object" && v !== null && !Array.isArray(v);

/** RFC 7386: what a merge patch makes of a document. */
export function mergePatch(target: unknown, patch: unknown): unknown {
  if (!isRecord(patch)) return structuredClone(patch);
  const out: Record<string, unknown> = isRecord(target) ? { ...target } : {};
  for (const [key, value] of Object.entries(patch)) {
    if (value === null) delete out[key];
    else out[key] = mergePatch(out[key], value);
  }
  return out;
}

/** Every difference between two documents, by path, as `configapi.Change` lists them. */
function changes(from: unknown, to: unknown, path = ""): ConfigChange[] {
  if (JSON.stringify(from) === JSON.stringify(to)) return [];
  if (isRecord(from) && isRecord(to)) {
    const keys = [...new Set([...Object.keys(from), ...Object.keys(to)])].sort();
    return keys.flatMap((k) => {
      const at = path ? `${path}.${k}` : k;
      if (!(k in from)) return [{ path: at, kind: "added" as const, to: to[k] }];
      if (!(k in to)) return [{ path: at, kind: "removed" as const, from: from[k] }];
      return changes(from[k], to[k], at);
    });
  }
  if (Array.isArray(from) && Array.isArray(to)) {
    const out: ConfigChange[] = [];
    for (let i = 0; i < Math.max(from.length, to.length); i++) {
      const at = `${path}[${i}]`;
      if (i >= from.length) out.push({ path: at, kind: "added", to: to[i] });
      else if (i >= to.length) out.push({ path: at, kind: "removed", from: from[i] });
      else out.push(...changes(from[i], to[i], at));
    }
    return out;
  }
  return [{ path, kind: "changed", from, to }];
}

/** The difference between a revision and another, or the active one. */
export function configDiff(c: DemoCompany, revisionId: string, against: string): ConfigDiff {
  const find = (id: string) =>
    c.config.revisions.find((r) => r.meta.revision_id === (id === "active" ? activeId(c) : id));
  const from = find(revisionId);
  const to = find(against);
  const list = from && to ? changes(from.payload, to.payload) : [];
  return {
    from: from?.meta.revision_id ?? revisionId,
    to: to?.meta.revision_id ?? against,
    changes: list,
    changes_total: list.length,
  };
}

/** Every seat the document writes, with its authored path. */
function seatsOf(doc: CompanyDocument): { role: ConfigRole; path: (string | number)[] }[] {
  const out: { role: ConfigRole; path: (string | number)[] }[] = [];
  (doc.roles ?? []).forEach((role, i) => out.push({ role, path: ["roles", i] }));
  const walk = (units: ConfigUnit[] | undefined, at: (string | number)[]): void =>
    (units ?? []).forEach((unit, i) => {
      (unit.roles ?? []).forEach((role, j) => out.push({ role, path: [...at, i, "roles", j] }));
      walk(unit.children, [...at, i, "children"]);
    });
  walk(doc.units, ["units"]);
  return out;
}

/** Every unit the document writes, with its authored path. */
function unitsOf(doc: CompanyDocument): { unit: ConfigUnit; path: (string | number)[] }[] {
  const out: { unit: ConfigUnit; path: (string | number)[] }[] = [];
  const walk = (units: ConfigUnit[] | undefined, at: (string | number)[]): void =>
    (units ?? []).forEach((unit, i) => {
      out.push({ unit, path: [...at, i] });
      walk(unit.children, [...at, i, "children"]);
    });
  walk(doc.units, ["units"]);
  return out;
}

/** A path's segments as the engine spells the path. */
function spell(segments: (string | number)[]): string {
  return segments
    .map((s, i) => (typeof s === "number" ? `[${s}]` : i === 0 ? s : `.${s}`))
    .join("");
}

/** A value an operator wrote, as opposed to one left out or emptied. */
function written(value: unknown): boolean {
  if (value === undefined || value === null || value === "") return false;
  if (Array.isArray(value)) return value.length > 0;
  if (typeof value === "object") return Object.keys(value).length > 0;
  return true;
}

/**
 * The agent-only fields a person's seat must not carry, as the engine names
 * them (`Role.humanForbidden`): each would be configuration that looks live
 * on a seat that is never spawned.
 */
const AGENT_ONLY: [string, (role: ConfigRole) => boolean][] = [
  ["llm", (r) => written(r.llm)],
  ["llm_review", (r) => written(r.llm_review)],
  ["llm_subagent", (r) => written(r.llm_subagent)],
  ["llm_auxiliary", (r) => written(r.llm_auxiliary)],
  ["llm_judge", (r) => written(r.llm_judge)],
  ["llm_sandbox", (r) => written(r.llm_sandbox)],
  ["sandbox", (r) => r.sandbox !== undefined && r.sandbox !== null],
  ["token_budget", (r) => written(r.token_budget)],
  ["workers", (r) => written(r.workers)],
  ["learning_enabled", (r) => typeof r.learning_enabled === "boolean"],
  ["schedules", (r) => written(r.schedules)],
  ["placement", (r) => written(r.placement)],
  ["integrations.slack", (r) => written(r.integrations?.slack)],
  ["integrations.mattermost", (r) => written(r.integrations?.mattermost)],
  ["project", (r) => written(r.project)],
  ["space", (r) => written(r.space)],
  ["mcp_env", (r) => written(r.mcp_env)],
  ["behavioral_guidelines", (r) => written(r.behavioral_guidelines)],
];

/** The identities a person can be reached on (`HumanContact`). */
const CONTACT_FIELDS = [
  "slack_user_id",
  "mattermost_user_id",
  "atlassian_account_id",
  "github_login",
  "gitlab_username",
];

/**
 * Where a rule about some of a seat's fields is reported (`org.fieldOf`): at
 * the field when there is one, and at the seat when there are several.
 */
function fieldOf(names: string[]): string[] {
  return names.length === 1 ? names[0]!.split(".") : [];
}

/** The structural problems the engine refuses a document for, and the references it warns about. */
export function validate(doc: CompanyDocument): {
  problems: ConfigProblem[];
  warnings: ConfigWarning[];
} {
  const problems: ConfigProblem[] = [];
  const warnings: ConfigWarning[] = [];
  const seats = seatsOf(doc);
  const handles = new Map<string, number>();
  for (const { role, path } of seats) {
    if (!role.name?.trim()) {
      problems.push({
        path: spell([...path, "name"]),
        segments: [...path, "name"],
        kind: "missing",
        message: `${spell(path)}: a seat needs a name`,
      });
      continue;
    }
    // A NAME IS PROSE, and two seats may share one; the HANDLE is the seat's
    // identity, and two seats may not (`Organization.validateHandles`).
    const handle = handleOf(role);
    handles.set(handle, (handles.get(handle) ?? 0) + 1);
    if ((handles.get(handle) ?? 0) > 1) {
      problems.push({
        path: spell([...path, "handle"]),
        segments: [...path, "handle"],
        kind: "conflict",
        message: `${spell(path)}.handle: duplicate handle "${handle}": more than one seat derives it. The handle is the canonical seat identity, naming its inbox, its agent id and its external accounts, so give each of these seats a distinct name or an explicit handle`,
        seat: handle,
      });
    }
  }
  const unitKeys = new Set<string>();
  for (const { unit, path } of unitsOf(doc)) {
    if (!unit.name?.trim()) {
      problems.push({
        path: spell([...path, "name"]),
        segments: [...path, "name"],
        kind: "missing",
        message: `${spell(path)}: a unit needs a name`,
      });
      continue;
    }
    // A UNIT IS KEYED on its id, its name where it has none (`validateUnitKeys`).
    const key = unitKeyOf(unit);
    if (unitKeys.has(key.toLowerCase())) {
      problems.push({
        path: spell([...path, "id"]),
        segments: [...path, "id"],
        kind: "conflict",
        message: `${spell(path)}.id: duplicate unit key "${key}": more than one unit answers to it. A unit's key is its id when it declares one and its name otherwise; a manages entry or a seat's unit reference resolves to the first unit answering to it, and the org chart gives one address to one unit, so one team's work, routing and pages are filed under another. Give each of these units its own id`,
        unit: key,
      });
    }
    unitKeys.add(key.toLowerCase());
    if (unit.lead && !handles.has(unit.lead)) {
      warnings.push({
        kind: "dangling_reference",
        ref: "lead",
        path: spell([...path, "lead"]),
        segments: [...path, "lead"],
        seat: "",
        unit: unit.name,
        from: unit.name,
        to: unit.lead,
        message: `unit "${unit.name}" names lead "${unit.lead}", which is no seat's handle, so the unit and every descendant inheriting its lead run with no lead. Correct the handle or add a seat that derives it`,
      });
    }
  }
  // A RULE ABOUT ONE SEAT OR UNIT is reported at the field it is about, its
  // line led by that field's path, as the engine places it (`config.located`).
  const located = (
    at: (string | number)[],
    kind: ConfigProblem["kind"],
    message: string,
    about: { seat?: string; unit?: string },
  ) =>
    problems.push({
      path: spell(at),
      segments: at,
      kind,
      message: `${spell(at)}: ${message}`,
      ...about,
    });
  for (const { role, path } of seats) {
    const name = (role.name ?? "").trim();
    const seat = { seat: handleOf(role) };
    if (role.kind === "human") {
      const offending = AGENT_ONLY.filter(([, set]) => set(role)).map(([field]) => field);
      if (offending.length > 0) {
        located(
          [...path, ...fieldOf(offending)],
          "conflict",
          `role ${q(name)}: agent-only field set on a human seat: ${offending.join(", ")}`,
          seat,
        );
      }
      // NO CONTACT IS ADVICE, not a refusal (`Company.unreachableWarnings`):
      // a person who only uses the dashboard needs none.
      if (!CONTACT_FIELDS.some((field) => written(role.contact?.[field]))) {
        const handle = handleOf(role);
        warnings.push({
          kind: "advisory",
          ref: "",
          path: spell([...path, "contact"]),
          segments: [...path, "contact"],
          seat: handle,
          unit: "",
          from: "",
          to: "",
          message: `${handle} is a human seat with no chat contact, so chat messages to it reach nobody. Add one, or leave it if they only use the dashboard`,
        });
      }
    } else {
      const humanOnly = [
        ...(role.contact !== undefined && role.contact !== null ? ["contact"] : []),
        ...(role.availability ? ["availability"] : []),
      ];
      if (humanOnly.length > 0) {
        located(
          [...path, ...fieldOf(humanOnly)],
          "conflict",
          `role ${q(name)}: human-only field set on an agent seat: ${humanOnly.join(", ")} (did you mean kind: human?)`,
          seat,
        );
      }
    }
    for (const fault of scheduleFaults(`role ${q(name)}`, role.schedules ?? [])) {
      located([...path, ...fault.field], fault.kind, fault.message, seat);
    }
  }
  // A UNIT SCHEDULE NOBODY CAN RUN is refused rather than left to no-op at
  // every minute it is due (`Unit.Validate`, `validateLeadSchedules`).
  const facts = unitRunnerFacts(doc);
  for (const { unit, path } of unitsOf(doc)) {
    const name = (unit.name ?? "").trim();
    const about = { unit: unit.name };
    const schedules = unit.schedules ?? [];
    for (const fault of scheduleFaults(`unit ${q(name)}`, schedules)) {
      located([...path, ...fault.field], fault.kind, fault.message, about);
    }
    const fact = facts.get(unit);
    schedules.forEach((s, i) => {
      if (s.enabled === false) return;
      if (s.target === "lead") {
        if (fact?.lead?.kind !== "human") return;
        located(
          [...path, "schedules", i],
          "conflict",
          `unit ${q(unit.name)}: schedule ${q(s.name)}: schedule has no runner: it targets the unit lead, but the effective lead ${q(fact.lead.name)} is a human seat: define the schedule on an agent seat instead`,
          about,
        );
      } else if (!fact || fact.agents.length === 0) {
        located(
          [...path, "schedules", i],
          "conflict",
          `unit ${q(name)}: schedule ${q(s.name)}: schedule has no runner: target each fans out to direct agent members only (never descendants, never human seats), and this unit has none`,
          about,
        );
      }
    });
  }
  for (const { role, path } of seats) {
    for (const [i, entry] of (role.manages ?? []).entries()) {
      if (handles.has(entry) || unitKeys.has(entry.toLowerCase())) continue;
      warnings.push({
        kind: "dangling_reference",
        ref: "manages",
        path: spell([...path, "manages", i]),
        segments: [...path, "manages", i],
        seat: handleOf(role),
        unit: "",
        from: role.name,
        to: entry,
        message: `seat "${role.name}" manages "${entry}", which is neither a seat's handle nor a unit's key, so the entry manages nobody. Correct the entry or add a seat or unit answering to it`,
      });
    }
  }
  return { problems, warnings };
}

/** The refusal of a document with problems, as a validation error answers. */
function refusal(problems: ConfigProblem[], doc: CompanyDocument): RestReply {
  return {
    status: 422,
    body: {
      error: "validation_error",
      detail: problems.map((p) => p.message).join("\n"),
      hint: "Fix the fields named above.",
      problems,
      derived: derive(doc),
    },
  };
}

/** Store `next` as the active revision and push what an apply pushes. */
function commit(
  engine: DemoEngine,
  next: CompanyDocument,
  summary: string,
): { revision: string; epoch: number } {
  const revision = appendRevision(engine.company, next, summary, {
    at: engine.now().toISOString(),
    source: "api",
  });
  engine.pushConfiguration();
  return { revision, epoch: engine.company.config.epoch };
}

/** Where a role or a unit sits in the document, found by its id. */
function locate(doc: CompanyDocument, kind: string, id: string) {
  if (kind === "roles")
    return seatsOf(doc).find(({ role }) => handleOf(role) === id || role.name === id) ?? null;
  if (kind === "units")
    return unitsOf(doc).find(({ unit }) => unit.id === id || unit.name === id) ?? null;
  return null;
}

/** The flat collections a PUT can add to, by their address. */
function flat(doc: CompanyDocument, kind: string): Record<string, unknown> | unknown[] | null {
  if (kind === "llm-providers")
    return (
      ((doc.providers as Record<string, unknown> | undefined)?.llm as Record<string, unknown>) ??
      null
    );
  if (kind === "mcp-servers") return (doc.mcp_servers as unknown[] | undefined) ?? null;
  return null;
}

/** One addressable entity of the document, or null. */
function entityOf(doc: CompanyDocument, kind: string, id: string): Record<string, unknown> | null {
  const at = locate(doc, kind, id);
  if (at) return ("role" in at ? at.role : at.unit) as Record<string, unknown>;
  const collection = flat(doc, kind);
  if (Array.isArray(collection)) {
    return (
      (collection.find((s) => isRecord(s) && s.name === id) as Record<string, unknown>) ?? null
    );
  }
  if (collection && isRecord(collection[id])) return collection[id] as Record<string, unknown>;
  return null;
}

/** The document with one entity replaced, or added when it is new. */
function withEntity(
  doc: CompanyDocument,
  kind: string,
  id: string,
  entity: Record<string, unknown>,
): CompanyDocument {
  const next = structuredClone(doc);
  const at = locate(next, kind, id);
  if (at) {
    let parent: unknown = next;
    for (const segment of at.path.slice(0, -1))
      parent = (parent as Record<string | number, unknown>)[segment];
    (parent as Record<string | number, unknown>)[at.path[at.path.length - 1]!] = entity;
    return next;
  }
  if (kind === "llm-providers") {
    const providers = (next.providers ??= {}) as Record<string, unknown>;
    const llm = (providers.llm ??= {}) as Record<string, unknown>;
    llm[id] = entity;
    return next;
  }
  if (kind === "mcp-servers") {
    const servers = ((next.mcp_servers as unknown[] | undefined) ?? []).filter(
      (s) => !(isRecord(s) && s.name === id),
    );
    next.mcp_servers = [...servers, { ...entity, name: id }];
    return next;
  }
  return next;
}

/** Every entity id a collection holds, or one entity whole. */
export function configEntities(c: DemoCompany, kind: string, id: string) {
  const doc = documentOf(c);
  if (id) return { kind, id, entity: entityOf(doc, kind, id) ?? undefined };
  const ids =
    kind === "roles"
      ? seatsOf(doc).map(({ role }) => handleOf(role))
      : kind === "units"
        ? unitsOf(doc).map(({ unit }) => unit.id || unit.name)
        : (() => {
            const collection = flat(doc, kind);
            if (Array.isArray(collection))
              return collection.flatMap((s) =>
                isRecord(s) && typeof s.name === "string" ? [s.name] : [],
              );
            return collection ? Object.keys(collection) : [];
          })();
  return { kind, ids };
}

/** `${VAR}` references the document names, and where. */
function references(doc: CompanyDocument): { path: string; name: string }[] {
  const out: { path: string; name: string }[] = [];
  const walk = (value: unknown, path: string): void => {
    if (typeof value === "string") {
      for (const match of value.matchAll(/\$\{([A-Z0-9_]+)\}/g))
        out.push({ path, name: match[1]! });
    } else if (Array.isArray(value)) {
      value.forEach((v, i) => walk(v, `${path}[${i}]`));
    } else if (isRecord(value)) {
      for (const [k, v] of Object.entries(value)) walk(v, path ? `${path}.${k}` : k);
    }
  };
  walk(doc, "");
  return out;
}

/** The body a write sent, without its audit summary. */
function writeBody(
  request: RestRequest,
): { body: Record<string, unknown>; summary: string } | null {
  try {
    const parsed = JSON.parse(request.body ?? "{}") as unknown;
    if (!isRecord(parsed)) return null;
    const { _summary, ...body } = parsed;
    return { body, summary: typeof _summary === "string" ? _summary : "" };
  } catch {
    return null;
  }
}

const KINDS = new Set(["roles", "units", "llm-providers", "mcp-servers"]);

/** Answer one `/config` request. */
export function handleConfig(engine: DemoEngine, request: RestRequest): RestReply {
  const c = engine.company;
  const doc = documentOf(c);
  const active = activeId(c);
  const { method, path, query, headers } = request;
  const dryRun = query.get("dry_run") === "true";

  if (path === "/config/references" && method === "GET")
    return { status: 200, body: references(doc) };

  if (path.startsWith("/config/revisions/") && method === "GET") {
    const id = decodeURIComponent(path.slice("/config/revisions/".length));
    const found = c.config.revisions.find((r) => r.meta.revision_id === id);
    return found
      ? { status: 200, body: { ...found.meta, is_active: id === active, payload: found.payload } }
      : { status: 404, body: { error: "not_found", detail: `There is no revision ${id}.` } };
  }

  if (path === "/config") {
    if (method === "GET") {
      if (query.get("format") === "yaml") {
        return {
          status: 200,
          text: JSON.stringify(doc, null, 2),
          headers: { ETag: etagOf(active) },
        };
      }
      return { status: 200, body: redacted(doc), headers: { ETag: etagOf(active) } };
    }
    if (method !== "PATCH" && method !== "PUT") {
      return { status: 405, body: { error: "method_not_allowed" } };
    }
    if (method === "PUT" && headers.get("If-None-Match") === "*") {
      return { status: 412, body: { error: "already_configured", current_revision_id: active } };
    }
    if (headers.get("If-Match") !== etagOf(active)) {
      return { status: 409, body: { error: "revision_advanced", current_revision_id: active } };
    }
    const sent = writeBody(request);
    if (!sent)
      return {
        status: 400,
        body: { error: "invalid_body", detail: "The body is not a JSON object." },
      };
    const next = (method === "PUT" ? sent.body : mergePatch(doc, sent.body)) as CompanyDocument;
    return write(engine, next, sent.summary, dryRun, active);
  }

  const [, , kind = "", rawId = ""] = path.split("/");
  const id = decodeURIComponent(rawId);
  if (!KINDS.has(kind) || !id) return { status: 404, body: { error: "no_route" } };
  if (method === "GET") {
    const entity = entityOf(doc, kind, id);
    return entity
      ? { status: 200, body: structuredClone(entity), headers: { ETag: etagOf(active) } }
      : { status: 404, body: { error: "not_found" } };
  }
  if (method !== "PUT") return { status: 405, body: { error: "method_not_allowed" } };
  const creating = headers.get("If-None-Match") === "*";
  if (creating && entityOf(doc, kind, id)) {
    return { status: 412, body: { error: "entity_exists", current_revision_id: active } };
  }
  if (!creating && headers.get("If-Match") !== etagOf(active)) {
    return { status: 409, body: { error: "revision_advanced", current_revision_id: active } };
  }
  if (!creating && !entityOf(doc, kind, id)) return { status: 404, body: { error: "not_found" } };
  const sent = writeBody(request);
  if (!sent)
    return {
      status: 400,
      body: { error: "invalid_body", detail: "The body is not a JSON object." },
    };
  return write(engine, withEntity(doc, kind, id, sent.body), sent.summary, dryRun, active);
}

function write(
  engine: DemoEngine,
  next: CompanyDocument,
  summary: string,
  dryRun: boolean,
  base: string,
): RestReply {
  const { problems, warnings } = validate(next);
  if (problems.length > 0) return refusal(problems, next);
  if (dryRun) {
    return {
      status: 200,
      body: { valid: true, base_revision_id: base, warnings, derived: derive(next) },
    };
  }
  const { revision, epoch } = commit(engine, next, summary || "Changed from the dashboard");
  return { status: 201, body: { revision_id: revision, epoch, warnings, derived: derive(next) } };
}

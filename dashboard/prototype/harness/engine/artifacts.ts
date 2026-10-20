/**
 * The demo engine's artifacts: the estate a topic seeds, every question the
 * Artifacts screens ask of it, the REST writes they make, and the rooms an
 * open artifact joins.
 *
 * WHAT THE READER MAY SEE IS DECIDED HERE, as `get_artifact` decides it: the
 * owner holds owner, so does a seat that leads an agent owner, and every
 * other reader holds the highest role a share names them by (their seat, a
 * unit they are in, or the company). A reader with no role is answered
 * `not_found`, exactly as for an artifact nobody holds.
 *
 * A PAGE IS SERVED FROM THE BUILD. The real engine serves a version's files
 * from the artifact origin; here a topic ships each page as a static file
 * (`Topic.static`), and a ticket points the frame at it, with the shared
 * origin's sandbox, so the design system's `SandboxedFrame` and the engine's
 * own page runtime run their real handshake.
 *
 * THE KINDS ARE THE ENGINE'S: a doc, a deck, a design and a dashboard are
 * read from their files as the engine reads them (`engine/artifactKinds.ts`),
 * so the shipped screens draw, edit and save every kind for real.
 *
 * PROPOSED, NOT ENGINE FIELDS: `comments` and `changes` on a listing row and
 * a detail, the comments routes, the templates listing and creating from a
 * template. Each is a change the engine would need, served so a proposal can
 * be clicked through, and nothing here is evidence it exists.
 */

import type {
  ArtifactAccessAnswer,
  ArtifactActor,
  ArtifactAudience,
  ArtifactCapability,
  ArtifactDetail,
  ArtifactFrame,
  ArtifactHistoryEntry,
  ArtifactJoined,
  ArtifactKind,
  ArtifactOutline,
  ArtifactListAnswer,
  ArtifactListItem,
  ArtifactPresenceMember,
  ArtifactRole,
  ArtifactShare,
  ArtifactSource,
  ArtifactVersion,
  ArtifactVersionSummary,
  DataCollection,
  DataDoc,
  DataHistoryEntry,
  DataOp,
  LogPosition,
} from "~/protocol/types.ts";
import { ARTIFACT_ROLES, SHAREABLE_ROLES } from "~/contract/artifacts.ts";
import { documentOf, type DemoCompany } from "./company.ts";
import { derive } from "./derive.ts";
import { fixtureId } from "./ids.ts";
import { actorOf } from "./principal.ts";
import { DemoRefusal, LOG, notModelled } from "./refusal.ts";
import type { RestReply, RestRequest } from "./rest.ts";
import type { DemoEngine } from "./engine.ts";
import {
  beginUpload,
  boardOf,
  catalogue,
  datasetOf,
  deckOf,
  deckStarter,
  docOf,
  docStarter,
  outlineOf,
  publish,
  putUpload,
  sourcePath,
  type DemoUpload,
} from "./artifactKinds.ts";

// ---- the proposed shapes -------------------------------------------------

/** PROPOSED: one comment on an artifact, anchored to a version and, optionally, a place in it. */
export interface ProposedComment {
  id: string;
  author: string;
  body: string;
  at: string;
  version: number;
  /** Where in the artifact: a slide, a frame, a heading, a cell. */
  anchor?: string;
  resolved?: boolean;
  replies?: { id: string; author: string; body: string; at: string }[];
}

/** PROPOSED: what a listing row and a detail carry beyond the engine's own. */
export interface ProposedArtifactFields {
  /** Open comment threads. */
  comments: number;
  /** Versions published since this reader last opened it; 0 for none or never opened. */
  changes: number;
  /** The task it belongs to, by key. */
  work_item?: string;
}

// ---- the estate ----------------------------------------------------------

/** One file of a version, as text. */
export interface DemoArtifactFile {
  path: string;
  content_type: string;
  text: string;
}

/** One published version, with its files as text. */
export interface DemoArtifactVersion {
  number: number;
  label?: string;
  created_at: string;
  author: ArtifactActor;
  capabilities?: ArtifactCapability[];
  restored_from?: number;
  /** The path served at the version's root; "" for a sheet. */
  entry: string;
  files: DemoArtifactFile[];
  /**
   * The page an `html` artifact's ticket frames, a file in the build relative
   * to its root (`frames/page-split-calculator.html`).
   */
  frame?: string;
  /** A design's artboards: each board's path in `canvas.json` to its page in the build. */
  boards?: Record<string, string>;
}

/** A sheet's store. */
export interface DemoArtifactData {
  collections: DataCollection[];
  docs: DataDoc[];
  history: DataHistoryEntry[];
  /** The store's version: every write advances it, and a document carries the version that wrote it. */
  version: number;
}

/** One artifact as the demo keeps it. */
export interface DemoArtifact {
  id: string;
  kind: ArtifactKind;
  title: string;
  description?: string;
  owner: string;
  owner_kind: "agent" | "human";
  /** The seats that lead an agent owner, each holding owner. */
  leads: string[];
  status: "live" | "trashed";
  trashed_by?: ArtifactActor;
  trashed_at?: string;
  shares: ArtifactShare[];
  created_by: ArtifactActor;
  updated_by: ArtifactActor;
  created_at: string;
  updated_at: string;
  /** Oldest first; the last is the head. Empty for a sheet that never published a page. */
  versions: DemoArtifactVersion[];
  /** Newest first. */
  history: ArtifactHistoryEntry[];
  /** When each seat last opened it. */
  opened: Record<string, string>;
  pinnedBy: string[];
  /** Who else is in its room now. */
  room: ArtifactPresenceMember[];
  comments: ProposedComment[];
  work_item?: string;
  data?: DemoArtifactData;
}

/** PROPOSED: a starting point a person creates an artifact from. */
export interface DemoArtifactTemplate {
  id: string;
  kind: ArtifactKind;
  title: string;
  description: string;
  outline?: ArtifactOutline;
  /** The artifact a create copies, by id, with a new title. */
  from: string;
}

/** Every artifact in the company, and the templates a create starts from. */
export interface DemoArtifacts {
  items: DemoArtifact[];
  templates: DemoArtifactTemplate[];
  /** The uploads a save is bringing, by id (`POST …/uploads`). */
  uploads: Record<string, DemoUpload>;
  /** The log sequence every artifacts write advances. */
  seq: number;
}

/** No artifacts: the base company's estate. */
export function emptyArtifacts(): DemoArtifacts {
  return { items: [], templates: [], uploads: {}, seq: 1 };
}

// ---- who may see what ----------------------------------------------------

type Params = Record<string, unknown>;

const RANK = (role: ArtifactRole) => ARTIFACT_ROLES.indexOf(role);

function str(p: Params, key: string): string {
  const v = p[key];
  return typeof v === "string" ? v : typeof v === "number" ? String(v) : "";
}

function num(p: Params, key: string, fallback: number): number {
  const v = p[key];
  const n = typeof v === "number" ? v : typeof v === "string" && v !== "" ? Number(v) : NaN;
  return Number.isFinite(n) ? n : fallback;
}

function position(seq: number): LogPosition {
  return { stream: "artifacts", generation: 0, seq };
}

/** The handle the browser reads as, and the units it is in. */
function reader(c: DemoCompany): { seat: string; units: Set<string> } {
  const seat = c.principal.kind === "person" ? (c.principal.seat ?? "") : "";
  const units = new Set<string>();
  if (seat) {
    const derived = derive(documentOf(c));
    for (const unit of derived.units ?? []) {
      if ((unit.seats ?? []).includes(seat)) units.add(unit.id ?? unit.name);
    }
  }
  return { seat, units };
}

/** The reader's role on an artifact, or null where they hold none. */
export function roleOf(c: DemoCompany, a: DemoArtifact): ArtifactRole | null {
  const { seat, units } = reader(c);
  if (seat && (a.owner === seat || a.leads.includes(seat))) return "owner";
  let best: ArtifactRole | null = null;
  for (const share of a.shares) {
    const names =
      share.audience.kind === "company" ||
      (share.audience.kind === "seat" && share.audience.id === seat) ||
      (share.audience.kind === "unit" && units.has(share.audience.id ?? ""));
    if (names && (best === null || RANK(share.role) > RANK(best))) best = share.role;
  }
  return best;
}

function find(c: DemoCompany, id: string): DemoArtifact {
  const a = c.artifacts.items.find((x) => x.id === id);
  if (!a || roleOf(c, a) === null) {
    throw new DemoRefusal("not_found", `There is no artifact ${id} you can read.`);
  }
  return a;
}

export function head(a: DemoArtifact): DemoArtifactVersion | undefined {
  return a.versions[a.versions.length - 1];
}

const ZERO_TIME = "0001-01-01T00:00:00Z";

/** An artifact as a reader is shown it, with the proposed fields beside the engine's. */
function rowOf(
  c: DemoCompany,
  a: DemoArtifact,
  role: ArtifactRole,
): ArtifactDetail & ProposedArtifactFields {
  const top = head(a);
  const seeShares = RANK(role) >= RANK("editor");
  const { seat } = reader(c);
  const opened = a.opened[seat];
  const changes = opened ? a.versions.filter((v) => v.created_at > opened).length : 0;
  return {
    id: a.id,
    label: a.id.replace(/-/g, "").slice(0, 26),
    kind: a.kind,
    title: a.title,
    ...(a.description ? { description: a.description } : {}),
    owner: a.owner,
    owner_kind: a.owner_kind,
    head_number: top?.number ?? 0,
    contract: "1.0.0",
    capabilities: top?.capabilities ?? [],
    status: a.status,
    trashed_by: a.trashed_by ?? { name: "", kind: "" },
    trashed_at: a.trashed_at ?? ZERO_TIME,
    purge_begun_at: ZERO_TIME,
    access_epoch: 1,
    bytes_held: a.versions.reduce((n, v) => n + v.files.reduce((m, f) => m + f.text.length, 0), 0),
    ...(seeShares ? { shares: a.shares } : {}),
    created_by: a.created_by,
    updated_by: a.updated_by,
    created_at: a.created_at,
    updated_at: a.updated_at,
    version: c.artifacts.seq,
    role,
    share_count: a.shares.length,
    writer: RANK(role) >= RANK("editor"),
    comments: a.comments.filter((t) => !t.resolved).length,
    changes,
    ...(a.work_item ? { work_item: a.work_item } : {}),
  };
}

// ---- the questions -------------------------------------------------------

/** The questions the Artifacts screens ask, by name. */
export const ARTIFACT_QUERIES = {
  artifacts: (e: DemoEngine, p: Params): ArtifactListAnswer => {
    const c = e.company;
    const { seat, units } = reader(c);
    const scope = str(p, "scope") || "all";
    const kind = str(p, "kind");
    const q = str(p, "q").trim().toLowerCase();
    const status = str(p, "status") || "live";
    const pinned = p["pinned"] === true || p["pinned"] === "true";
    const workItem = str(p, "work_item");
    const sort = str(p, "sort") || "updated";
    const limit = Math.max(1, Math.min(200, num(p, "limit", 50)));
    const offset = Math.max(0, num(p, "cursor", 0));
    const rows: (ArtifactListItem & ProposedArtifactFields)[] = [];
    for (const a of c.artifacts.items) {
      const role = roleOf(c, a);
      if (role === null || a.status !== status) continue;
      if (kind && a.kind !== kind) continue;
      if (workItem && a.work_item !== workItem) continue;
      if (pinned && !a.pinnedBy.includes(seat)) continue;
      if (q && !`${a.title} ${a.description ?? ""}`.toLowerCase().includes(q)) continue;
      const mine = a.owner === seat;
      const sharedWithMe = a.shares.some(
        (s) =>
          (s.audience.kind === "seat" && s.audience.id === seat) ||
          (s.audience.kind === "unit" && units.has(s.audience.id ?? "")),
      );
      const company = a.shares.some((s) => s.audience.kind === "company");
      if (scope === "mine" && !mine) continue;
      if (scope === "shared" && (mine || !(sharedWithMe || company))) continue;
      if (scope === "company" && !company) continue;
      if (scope === "agents" && a.owner_kind !== "agent") continue;
      const { writer: _w, ...row } = rowOf(c, a, role);
      rows.push({
        ...row,
        pinned: a.pinnedBy.includes(seat),
        opened_at: a.opened[seat] ?? ZERO_TIME,
      });
    }
    const by: Record<string, (a: ArtifactListItem, b: ArtifactListItem) => number> = {
      updated: (a, b) => b.updated_at.localeCompare(a.updated_at),
      created: (a, b) => b.created_at.localeCompare(a.created_at),
      title: (a, b) => a.title.localeCompare(b.title),
      opened: (a, b) =>
        b.opened_at.localeCompare(a.opened_at) || b.updated_at.localeCompare(a.updated_at),
    };
    rows.sort(by[sort] ?? by["updated"]!);
    const page = rows.slice(offset, offset + limit);
    const next = offset + limit < rows.length ? String(offset + limit) : "";
    return {
      items: page,
      total: rows.length,
      ...(next ? { after: next } : {}),
      read_level: "stale",
      position: position(c.artifacts.seq),
      complete: true,
    };
  },

  artifact: (e: DemoEngine, p: Params): ArtifactDetail => {
    const a = find(e.company, str(p, "id"));
    return rowOf(e.company, a, roleOf(e.company, a)!);
  },

  artifact_versions: (e: DemoEngine, p: Params) => {
    const a = find(e.company, str(p, "id"));
    return { versions: [...a.versions].reverse().map((v) => summaryOf(a, v)) };
  },

  artifact_version: (e: DemoEngine, p: Params): ArtifactVersion => {
    const a = find(e.company, str(p, "id"));
    const v = versionOf(a, num(p, "n", 0));
    return {
      ...summaryOf(a, v),
      manifest: {
        entry: v.entry,
        files: v.files.map((f) => ({
          path: f.path,
          content_type: f.content_type,
          object: { hash: hashOf(f.text), size: f.text.length },
        })),
      },
    };
  },

  artifact_history: (e: DemoEngine, p: Params) => ({
    entries: find(e.company, str(p, "id")).history,
  }),

  artifact_assets: (e: DemoEngine, p: Params) => {
    find(e.company, str(p, "id"));
    return { assets: [] };
  },

  artifact_seen_by: (e: DemoEngine, p: Params) => {
    const a = find(e.company, str(p, "id"));
    return {
      shown: true,
      reads: Object.entries(a.opened)
        .map(([seat, opened_at]) => ({ seat, opened_at }))
        .sort((x, y) => y.opened_at.localeCompare(x.opened_at)),
    };
  },

  artifact_access: (e: DemoEngine, p: Params): ArtifactAccessAnswer => {
    const c = e.company;
    const a = find(c, str(p, "id"));
    const role = roleOf(c, a)!;
    const derived = derive(documentOf(c));
    return {
      owner: a.owner,
      owner_kind: a.owner_kind,
      role,
      ...(RANK(role) >= RANK("editor")
        ? {
            shares: a.shares.map((s) => ({
              ...s,
              ...(s.audience.kind === "unit"
                ? {
                    members:
                      (derived.units ?? []).find((u) => (u.id ?? u.name) === s.audience.id)
                        ?.seats ?? [],
                  }
                : {}),
            })),
          }
        : {}),
      leads: a.leads,
      share_count: a.shares.length,
    };
  },

  artifact_usage: (e: DemoEngine) => {
    const items = e.company.artifacts.items;
    const bytes = items.reduce(
      (n, a) =>
        n + a.versions.reduce((m, v) => m + v.files.reduce((k, f) => k + f.text.length, 0), 0),
      0,
    );
    return {
      usage: {
        artifacts: items.length,
        bytes,
        versions: items.reduce((n, a) => n + a.versions.length, 0),
        assets: 0,
        position: position(e.company.artifacts.seq),
        complete: true,
      },
      limits: {
        company_bytes: 5 * 1024 ** 3,
        artifacts: 10_000,
        data_bytes: 1024 ** 3,
        data_store_bytes: 64 * 1024 ** 2,
        pending_bytes: 256 * 1024 ** 2,
      },
    };
  },

  artifact_trail: (e: DemoEngine) => ({
    entries: e.company.artifacts.items
      .flatMap((a) =>
        a.history.map((h) => ({ ...h, artifact: a.id, title: a.title, owner: a.owner })),
      )
      .sort((x, y) => y.at.localeCompare(x.at)),
    position: position(e.company.artifacts.seq),
    complete: true,
  }),

  artifact_data_schema: (e: DemoEngine, p: Params) => {
    const a = find(e.company, str(p, "id"));
    const data = storeOf(a);
    return {
      collections: data.collections,
      store: {
        artifact: a.id,
        docs: data.docs.filter((d) => !d.deleted).length,
        footprint: JSON.stringify(data.docs).length,
        doc_bytes: JSON.stringify(data.docs).length,
        history_bytes: JSON.stringify(data.history).length,
        body_bytes: JSON.stringify(data.docs).length,
        field_entries: data.docs.reduce((n, d) => n + Object.keys(d.fields ?? {}).length, 0),
        writes: data.history.length,
        digest: hashOf(JSON.stringify(data.docs)),
        history_floor: 0,
        tombstones_floor: 0,
        version: data.version,
      },
      role: roleOf(e.company, a)!,
    };
  },

  artifact_data: (e: DemoEngine, p: Params) => {
    const a = find(e.company, str(p, "id"));
    const data = storeOf(a);
    const collection = str(p, "collection");
    const since = num(p, "since", 0);
    return {
      docs: data.docs.filter(
        (d) => d.collection === collection && (since === 0 ? !d.deleted : d.version > since),
      ),
      through: data.version,
      position: position(e.company.artifacts.seq),
      complete: true,
    };
  },

  artifact_data_doc: (e: DemoEngine, p: Params): DataDoc => {
    const data = storeOf(find(e.company, str(p, "id")));
    const doc = data.docs.find(
      (d) => d.collection === str(p, "collection") && d.id === str(p, "doc"),
    );
    if (!doc) throw new DemoRefusal("not_found", "There is no such document.");
    return doc;
  },

  artifact_data_query: (e: DemoEngine, p: Params) => {
    const data = storeOf(find(e.company, str(p, "id")));
    return {
      docs: data.docs.filter((d) => d.collection === str(p, "collection") && !d.deleted),
      through: data.version,
      position: position(e.company.artifacts.seq),
      complete: true,
    };
  },

  artifact_data_history: (e: DemoEngine, p: Params) => ({
    entries: [...storeOf(find(e.company, str(p, "id"))).history].reverse(),
    floor: 0,
    position: position(e.company.artifacts.seq),
    complete: true,
  }),

  artifact_data_state: (e: DemoEngine, p: Params) => {
    find(e.company, str(p, "id"));
    return { state: [] };
  },

  artifact_data_usage: (e: DemoEngine) => {
    const stores = e.company.artifacts.items.filter((a) => a.data);
    return {
      usage: {
        stores: stores.length,
        docs: stores.reduce((n, a) => n + a.data!.docs.length, 0),
        footprint: stores.reduce((n, a) => n + JSON.stringify(a.data!.docs).length, 0),
        position: position(e.company.artifacts.seq),
        complete: true,
      },
      data_bytes: 1024 ** 3,
      data_store_bytes: 64 * 1024 ** 2,
    };
  },
};

export function versionOf(a: DemoArtifact, n: number): DemoArtifactVersion {
  const v = n > 0 ? a.versions.find((x) => x.number === n) : head(a);
  if (!v) throw new DemoRefusal("not_found", `${a.title} has no version ${n}.`);
  return v;
}

function summaryOf(a: DemoArtifact, v: DemoArtifactVersion): ArtifactVersionSummary {
  return {
    artifact: a.id,
    number: v.number,
    version_id: fixtureId(`${a.id}:v${v.number}`),
    base: v.number - 1,
    ...(v.restored_from ? { restored_from: v.restored_from } : {}),
    ...(v.label ? { label: v.label } : {}),
    entry: v.entry,
    contract: "1.0.0",
    capabilities: v.capabilities ?? [],
    files: v.files.length,
    bytes: v.files.reduce((n, f) => n + f.text.length, 0),
    author: v.author,
    created_at: v.created_at,
    position: v.number,
  };
}

/**
 * An artifact's store. EVERY ARTIFACT HAS ONE, as the engine answers it: an
 * artifact that never wrote a collection reads as an empty store, and a write
 * naming a collection it does not hold is refused by the write.
 */
function storeOf(a: DemoArtifact): DemoArtifactData {
  return a.data ?? { collections: [], docs: [], history: [], version: 0 };
}

/** A short stable digest of a text, shaped like a content hash. */
function hashOf(text: string): string {
  let h = 2166136261;
  for (let i = 0; i < text.length; i++) h = Math.imul(h ^ text.charCodeAt(i), 16777619) >>> 0;
  return `sha256:${h.toString(16).padStart(8, "0").repeat(8)}`;
}

// ---- the writes ----------------------------------------------------------

export function refuse(status: number, error: string, detail: string): RestReply {
  return { status, body: { error, detail } };
}

export function body(request: RestRequest): Record<string, unknown> {
  if (!request.body) return {};
  try {
    const parsed = JSON.parse(request.body) as unknown;
    return parsed && typeof parsed === "object" ? (parsed as Record<string, unknown>) : {};
  } catch {
    return {};
  }
}

/** Record one applied change on an artifact's history, as the reader. */
export function record(
  e: DemoEngine,
  a: DemoArtifact,
  op: string,
  summary: ArtifactHistoryEntry["summary"] = {},
  number?: number,
): void {
  const c = e.company;
  const actor = actorOf(c);
  c.artifacts.seq += 1;
  a.history.unshift({
    position: c.artifacts.seq,
    op_id: fixtureId(`${a.id}:${op}:${c.artifacts.seq}`),
    op,
    outcome: "applied",
    ...(number !== undefined ? { number } : {}),
    actor: { name: actor.name, kind: actor.kind, operator_id: actor.operator_id },
    origin: "dashboard",
    at: e.now().toISOString(),
    summary,
  });
}

/** Say to the artifact's room that something moved on it. */
export function changed(e: DemoEngine, a: DemoArtifact, what: string): void {
  e.push("artifact_changed", {
    artifact: a.id,
    what,
    head: head(a)?.number ?? 0,
    position: position(e.company.artifacts.seq),
  });
}

/** The writes under `/artifacts`. */
export function handleArtifacts(e: DemoEngine, request: RestRequest): RestReply {
  const c = e.company;
  const parts = request.path.split("/").slice(2).map(decodeURIComponent);
  const { method } = request;
  if (parts.length === 1 && parts[0] === "ready" && method === "POST") return { status: 204 };
  if (parts.length === 1 && parts[0] === "sources" && method === "GET") return catalogue();
  if (parts[0] === "uploads") return putUpload(e, parts, request);
  if (parts.length === 0 && method === "POST") return create(e, request);
  if (parts.length === 1 && parts[0] === "templates" && method === "GET") {
    return { status: 200, body: { templates: c.artifacts.templates } };
  }
  const id = parts[0] ?? "";
  const a = c.artifacts.items.find((x) => x.id === id);
  const role = a ? roleOf(c, a) : null;
  if (!a || role === null) return refuse(404, "not_found", "There is no such artifact.");
  const can = (needed: ArtifactRole) => RANK(role) >= RANK(needed);
  const denied = (needed: ArtifactRole) =>
    refuse(
      403,
      "forbidden",
      `This needs the ${needed} role on the artifact, and you hold ${role}.`,
    );
  const rest = parts.slice(1);
  const seat = reader(c).seat;
  const now = e.now().toISOString();

  if (rest.length === 0 && method === "PATCH") {
    if (!can("editor")) return denied("editor");
    const b = body(request);
    if (typeof b["title"] === "string") a.title = b["title"];
    if (typeof b["description"] === "string") a.description = b["description"];
    a.updated_at = now;
    record(e, a, "patch", { fields: Object.keys(b) });
    changed(e, a, "meta");
    return { status: 200, body: {} };
  }
  if (rest.length === 0 && method === "DELETE") {
    if (!can("owner")) return denied("owner");
    const actor = actorOf(c);
    a.status = "trashed";
    a.trashed_by = { name: actor.name, kind: actor.kind };
    a.trashed_at = now;
    record(e, a, "trash");
    changed(e, a, "trash");
    return { status: 200, body: {} };
  }
  if (rest[0] === "restore" && rest.length === 1 && method === "POST") {
    if (!can("owner")) return denied("owner");
    a.status = "live";
    delete a.trashed_by;
    delete a.trashed_at;
    record(e, a, "restore");
    changed(e, a, "restore");
    return { status: 200, body: {} };
  }
  if (rest[0] === "pin" && rest.length === 1) {
    if (method === "PUT" && !a.pinnedBy.includes(seat)) a.pinnedBy.push(seat);
    if (method === "DELETE") a.pinnedBy = a.pinnedBy.filter((s) => s !== seat);
    return { status: 200, body: {} };
  }
  if (rest[0] === "shares") {
    if (!can("editor")) return denied("editor");
    if (method === "PUT" && rest.length === 1) {
      const b = body(request);
      const audience = b["audience"] as ArtifactAudience | undefined;
      const shareRole = b["role"];
      if (!audience || !(SHAREABLE_ROLES as readonly unknown[]).includes(shareRole)) {
        return refuse(400, "bad_request", "A share names an audience and a role.");
      }
      const key = audience.kind === "company" ? "company" : `${audience.kind}:${audience.id ?? ""}`;
      const actor = actorOf(c);
      a.shares = a.shares.filter(
        (s) =>
          (s.audience.kind === "company"
            ? "company"
            : `${s.audience.kind}:${s.audience.id ?? ""}`) !== key,
      );
      a.shares.push({
        audience: audience.kind === "company" ? { kind: "company" } : audience,
        role: shareRole as ArtifactRole,
        granted_by: { name: actor.name, kind: actor.kind },
        granted_at: now,
      });
      record(e, a, "share", { audience: key, role: String(shareRole) });
      changed(e, a, "share");
      return { status: 200, body: {} };
    }
    if (method === "DELETE" && rest.length === 2) {
      const key = rest[1]!;
      const [kind, ...idParts] = key.split(":");
      const who = `${kind}:${idParts.join(":")}`;
      a.shares = a.shares.filter((s) => `${s.audience.kind}:${s.audience.id ?? ""}` !== who);
      record(e, a, "unshare", { audience: who });
      changed(e, a, "share");
      return { status: 200, body: {} };
    }
  }
  if (rest[0] === "tickets" && method === "POST") {
    const b = body(request);
    const v = versionOf(a, typeof b["version"] === "number" ? b["version"] : 0);
    // A DESIGN'S ARTBOARD is framed by its path; a page by its entry.
    const document = typeof b["document"] === "string" ? b["document"] : "";
    const file = document ? v.boards?.[document] : v.frame;
    if (!file) return refuse(409, "artifact_not_framed", "This version has no page to open.");
    if (seat && request.query.get("purpose") !== "preview") a.opened[seat] = now;
    return { status: 200, body: frameOf(file, v.number, document || v.entry) };
  }
  if (rest[0] === "uploads" && rest.length === 1 && method === "POST") {
    if (!can("editor")) return denied("editor");
    return beginUpload(e, a, request);
  }
  if (rest[0] === "versions" && rest.length >= 3) {
    // A SHEET HAS NO VERSION: its outline names its kind alone.
    if (rest[2] === "outline" && method === "GET") {
      const n = Number(rest[1]);
      return {
        status: 200,
        body: outlineOf(a, n > 0 || a.kind !== "sheet" ? versionOf(a, n) : undefined),
      };
    }
    const v = versionOf(a, Number(rest[1]));
    const opened = () => {
      if (seat && request.query.get("purpose") !== "preview") a.opened[seat] = now;
    };
    if (rest[2] === "doc" && method === "GET") {
      const doc = docOf(v);
      if (!doc) return refuse(409, "not_a_doc", `${a.title} is not a doc.`);
      opened();
      return { status: 200, body: { artifact: a.id, version: v.number, doc } };
    }
    if (rest[2] === "deck" && method === "GET") {
      const deck = deckOf(v);
      if (!deck) return refuse(409, "not_a_deck", `${a.title} is not a deck.`);
      opened();
      return { status: 200, body: { artifact: a.id, version: v.number, deck } };
    }
    if (rest[2] === "dashboard" && method === "GET") {
      if (a.kind !== "dashboard")
        return refuse(409, "not_a_dashboard", `${a.title} is not a dashboard.`);
      opened();
      return boardOf(e, a, v);
    }
    if (rest[2] === "sources" && rest.length === 4 && method === "GET") {
      return datasetOf(e, a, v, rest[3]!, request.query);
    }
    if (rest[2] === "source" && method === "GET") {
      const file = v.files.find((f) => f.path === sourcePath(v));
      if (!file) return refuse(404, "not_found", "This version has no entry.");
      opened();
      const source: ArtifactSource = {
        artifact: a.id,
        kind: a.kind,
        version: v.number,
        path: file.path,
        content_type: file.content_type,
        size: file.text.length,
        text: file.text,
      };
      return { status: 200, body: source };
    }
    if (rest[2] === "files" && method === "GET") {
      const path = rest.slice(3).join("/");
      const file = v.files.find((f) => f.path === path);
      return file
        ? { status: 200, text: file.text, headers: { "Content-Type": file.content_type } }
        : refuse(404, "not_found", `This version has no file ${path}.`);
    }
    if (rest[2] === "restore" && method === "POST") {
      if (!can("editor")) return denied("editor");
      const top = head(a)!;
      const actor = actorOf(c);
      a.versions.push({
        ...structuredClone(v),
        number: top.number + 1,
        restored_from: v.number,
        created_at: now,
        author: { name: actor.name, kind: actor.kind },
      });
      delete a.versions[a.versions.length - 1]!.label;
      a.updated_at = now;
      a.updated_by = { name: actor.name, kind: actor.kind };
      record(e, a, "restore_version", { restored_from: v.number }, top.number + 1);
      changed(e, a, "version");
      return { status: 200, body: { number: top.number + 1 } };
    }
  }
  if (rest[0] === "versions" && rest.length === 2 && method === "PATCH") {
    if (!can("editor")) return denied("editor");
    const v = versionOf(a, Number(rest[1]));
    const label = body(request)["label"];
    if (typeof label === "string" && label) v.label = label;
    else delete v.label;
    record(e, a, "patch", { label: typeof label === "string" ? label : "" }, v.number);
    changed(e, a, "meta");
    return { status: 200, body: {} };
  }
  if (rest[0] === "versions" && rest.length === 1 && method === "POST") {
    if (!can("editor")) return denied("editor");
    return publish(e, a, request);
  }
  if (rest[0] === "data") return handleData(e, a, role, rest.slice(1), request);
  if (rest[0] === "comments") return handleComments(e, a, role, rest.slice(1), request);
  if (rest[0] === "transfer" || rest[0] === "purge") {
    console.warn(LOG, `${method} ${request.path} is not modelled; it was refused.`);
    return refuse(
      409,
      "not_modelled",
      notModelled(rest[0] === "transfer" ? "Transferring an artifact" : "Purging an artifact"),
    );
  }
  console.warn(LOG, `${method} ${request.path} is not an artifacts path the demo serves.`);
  return refuse(404, "no_route", `${method} ${request.path} is not served by the demo engine.`);
}

/** A ticket for a version: its page in the build, on the shared origin's sandbox. */
function frameOf(file: string, version: number, document: string): ArtifactFrame {
  const url = new URL(`../${file}`, location.href);
  url.hash = "";
  const nonce = Array.from(crypto.getRandomValues(new Uint8Array(24)), (b) =>
    b.toString(16).padStart(2, "0"),
  ).join("");
  return {
    frame_url: url.href,
    origin: url.origin,
    version,
    expires_at: new Date(Date.now() + 10 * 60_000).toISOString(),
    sandbox: "allow-scripts allow-forms",
    allow: "",
    mode: "shared",
    nonce,
    document,
  };
}

/**
 * `POST /artifacts`: a native sheet, a doc or a deck from the engine's
 * starter (`starter: true`), or PROPOSED, any kind copied from a template.
 */
function create(e: DemoEngine, request: RestRequest): RestReply {
  const c = e.company;
  const b = body(request);
  const title = typeof b["title"] === "string" ? b["title"].trim() : "";
  if (!title) return refuse(400, "bad_request", "An artifact needs a title.");
  const actor = actorOf(c);
  const by: ArtifactActor = { name: actor.name, kind: actor.kind };
  const now = e.now().toISOString();
  c.artifacts.seq += 1;
  const id = fixtureId(`created:${c.artifacts.seq}:${title}`);
  const template = c.artifacts.templates.find((t) => t.id === b["template"]);
  const source = template ? c.artifacts.items.find((x) => x.id === template.from) : undefined;
  if (b["template"] !== undefined && !source) {
    return refuse(400, "bad_request", "There is no such template.");
  }
  const kind = source?.kind ?? b["kind"];
  const workItem = typeof b["work_item"] === "string" && b["work_item"] ? b["work_item"] : "";
  const fresh = (k: ArtifactKind): DemoArtifact => ({
    id,
    kind: k,
    title,
    ...(typeof b["description"] === "string" && b["description"]
      ? { description: b["description"] }
      : {}),
    owner: actor.name,
    owner_kind: "human",
    leads: [],
    status: "live",
    shares: [],
    created_by: by,
    updated_by: by,
    created_at: now,
    updated_at: now,
    versions: [],
    history: [],
    opened: { [actor.name]: now },
    pinnedBy: [],
    room: [],
    comments: [],
    ...(workItem ? { work_item: workItem } : {}),
  });
  let made: DemoArtifact;
  if (source) {
    const top = head(source);
    made = {
      ...fresh(source.kind),
      versions: top ? [{ ...structuredClone(top), number: 1, created_at: now, author: by }] : [],
      ...(source.data ? { data: structuredClone(source.data) } : {}),
    };
    delete made.versions[0]?.label;
  } else if (kind === "sheet") {
    made = { ...fresh("sheet"), data: { collections: [], docs: [], history: [], version: 1 } };
  } else if ((kind === "doc" || kind === "slides") && b["starter"] === true) {
    const files = kind === "doc" ? docStarter(title) : deckStarter(title);
    made = {
      ...fresh(kind),
      versions: [
        {
          number: 1,
          created_at: now,
          author: by,
          capabilities: [],
          entry: kind === "doc" ? "index.md" : "deck.json",
          files,
        },
      ],
    };
  } else {
    return refuse(
      400,
      "bad_request",
      "The dashboard starts a sheet, or a doc or a deck from its starter. Every other kind is published by an agent or the CLI.",
    );
  }
  c.artifacts.items.unshift(made);
  record(e, made, "create", {}, made.versions.length > 0 ? 1 : undefined);
  return {
    status: 201,
    body: {
      artifact: id,
      label: id.replace(/-/g, "").slice(0, 26),
      number: made.versions.length,
      version_id: fixtureId(`${id}:v1`),
      op_id: fixtureId(`${id}:create`),
      outcome: "applied",
      position: position(c.artifacts.seq),
    },
  };
}

/** PROPOSED: an artifact's comment threads. */
function handleComments(
  e: DemoEngine,
  a: DemoArtifact,
  role: ArtifactRole,
  rest: string[],
  request: RestRequest,
): RestReply {
  if (request.method === "GET" && rest.length === 0) {
    return { status: 200, body: { comments: a.comments } };
  }
  if (RANK(role) < RANK("commenter")) {
    return refuse(403, "forbidden", `Commenting needs the commenter role, and you hold ${role}.`);
  }
  const b = body(request);
  const text = typeof b["body"] === "string" ? b["body"].trim() : "";
  const now = e.now().toISOString();
  const author = reader(e.company).seat || actorOf(e.company).name;
  if (request.method === "POST" && rest.length === 0) {
    if (!text) return refuse(400, "bad_request", "A comment says something.");
    const comment: ProposedComment = {
      id: fixtureId(`${a.id}:comment:${a.comments.length}:${now}`),
      author,
      body: text,
      at: now,
      version: head(a)?.number ?? 0,
      ...(typeof b["anchor"] === "string" && b["anchor"] ? { anchor: b["anchor"] } : {}),
    };
    a.comments.push(comment);
    return { status: 201, body: comment };
  }
  const thread = a.comments.find((t) => t.id === rest[0]);
  if (!thread) return refuse(404, "not_found", "There is no such comment.");
  if (request.method === "POST" && rest[1] === "replies") {
    if (!text) return refuse(400, "bad_request", "A reply says something.");
    thread.replies = [
      ...(thread.replies ?? []),
      { id: fixtureId(`${thread.id}:reply:${now}`), author, body: text, at: now },
    ];
    return { status: 201, body: thread };
  }
  if (request.method === "PATCH" && rest.length === 1) {
    thread.resolved = b["resolved"] === true;
    return { status: 200, body: thread };
  }
  return refuse(
    404,
    "no_route",
    `${request.method} ${request.path} is not served by the demo engine.`,
  );
}

/** A sheet's writes: a batch of ops, applied whole, and its push to the room. */
function handleData(
  e: DemoEngine,
  a: DemoArtifact,
  role: ArtifactRole,
  rest: string[],
  request: RestRequest,
): RestReply {
  const data = a.data;
  if (!data) return refuse(404, "not_found", `${a.title} keeps no datastore.`);
  if (rest[0] === "export.csv" && request.method === "GET") {
    const name = request.query.get("collection") ?? "";
    const coll = data.collections.find((x) => x.name === name);
    if (!coll) return refuse(404, "not_found", `There is no collection ${name}.`);
    const cols = (coll.columns ?? []).map((col) => col.key);
    const quote = (v: unknown) => `"${String(v ?? "").replace(/"/g, '""')}"`;
    const lines = [cols.map(quote).join(",")];
    for (const d of data.docs.filter((x) => x.collection === name && !x.deleted)) {
      lines.push(cols.map((k) => quote(d.fields?.[k])).join(","));
    }
    return { status: 200, text: `${lines.join("\n")}\n`, headers: { "Content-Type": "text/csv" } };
  }
  if (rest[0] === "schema" && request.method === "PUT") {
    if (RANK(role) < RANK("editor"))
      return refuse(403, "forbidden", "Changing the columns needs the editor role.");
    const collections = body(request)["collections"];
    if (!Array.isArray(collections))
      return refuse(400, "bad_request", "A schema is a list of collections.");
    data.collections = collections as DataCollection[];
    data.version += 1;
    e.push("artifact_data", { artifact: a.id, schema: true, through: data.version });
    return { status: 200, body: {} };
  }
  if (rest[0] === "writes" && request.method === "POST") {
    if (RANK(role) < RANK("contributor")) {
      return {
        status: 403,
        body: {
          error: "forbidden",
          verdict: "forbidden",
          detail: `Editing this sheet needs the contributor role, and you hold ${role}.`,
        },
      };
    }
    const ops = (body(request)["ops"] ?? []) as DataOp[];
    const actor = actorOf(e.company);
    const by: ArtifactActor = { name: actor.name, kind: actor.kind };
    const now = e.now().toISOString();
    data.version += 1;
    const deltas = [];
    for (const op of ops) {
      const at = data.docs.findIndex((d) => d.collection === op.collection && d.id === op.doc);
      const was = at >= 0 ? data.docs[at] : undefined;
      let fields: Record<string, unknown> | undefined = { ...(was?.fields ?? {}) };
      if (op.op === "set") fields = { ...(op.fields ?? {}) };
      else if (op.op === "merge") {
        for (const [k, v] of Object.entries(op.fields ?? {})) {
          if (v === null) delete fields[k];
          else fields[k] = v;
        }
      } else if (op.op === "delete") fields = undefined;
      else {
        console.warn(LOG, `A datastore ${op.op} op is not modelled; it was refused.`);
        return refuse(409, "not_modelled", notModelled(`A datastore ${op.op}`));
      }
      const doc: DataDoc = {
        collection: op.collection,
        id: op.doc,
        ...(fields ? { fields } : { deleted: true }),
        version: data.version,
        created_by: was?.created_by ?? by,
        created_at: was?.created_at ?? now,
        updated_by: by,
        updated_at: now,
      };
      if (at >= 0) data.docs[at] = doc;
      else data.docs.push(doc);
      deltas.push({
        collection: op.collection,
        doc: op.doc,
        version: data.version,
        ...(fields ? { body: fields } : { deleted: true }),
        read: "all",
        created_by: doc.created_by.name,
        updated_by: by,
        updated_at: now,
      });
    }
    const seq = data.history.length + 1;
    data.history.push({
      seq,
      op_id: fixtureId(`${a.id}:write:${seq}`),
      op: "write",
      verdict: "applied",
      actor: by,
      origin: "dashboard",
      at: now,
      changes: ops.map((op) => ({
        c: op.collection,
        d: op.doc,
        op: op.op === "delete" ? "delete" : "update",
      })),
      undoable: false,
      digest: hashOf(JSON.stringify(data.docs)),
    });
    a.updated_at = now;
    a.updated_by = by;
    e.push("artifact_data", { artifact: a.id, deltas, through: data.version });
    return {
      status: 200,
      body: {
        op_id: fixtureId(`${a.id}:write:${seq}`),
        outcome: "applied",
        verdict: "applied",
        seq,
        position: { stream: "artifact-data", generation: 0, seq: data.version },
      },
    };
  }
  console.warn(LOG, `${request.method} ${request.path} is not modelled; it was refused.`);
  return refuse(409, "not_modelled", notModelled("This datastore write"));
}

// ---- rooms ---------------------------------------------------------------

/** The session the browser's own tab holds in every room. */
export const OWN_SESSION = "session-you";

/** Join an artifact's room: the reader's role and who else is there. */
export function joinRoom(e: DemoEngine, id: string): ArtifactJoined {
  const c = e.company;
  const a = find(c, id);
  const seat = reader(c).seat;
  const now = e.now().toISOString();
  if (seat) a.opened[seat] = now;
  const self: ArtifactPresenceMember = {
    session: OWN_SESSION,
    seat,
    kind: "human",
    activity: "viewing",
    focus: { version: head(a)?.number ?? 0 },
    at: now,
  };
  const members = [self, ...a.room.map((m) => ({ ...m, at: now }))];
  return {
    artifact: a.id,
    session: OWN_SESSION,
    role: roleOf(c, a)!,
    roster: { artifact: a.id, members },
    through: a.data?.version ?? 0,
    resync: false,
  };
}

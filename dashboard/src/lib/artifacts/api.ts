/**
 * The artifacts' REST surface, as the requests a screen sends: every write
 * (`internal/api/artifactsapi`, `internal/api/artifactdataapi`), the frame a
 * page is embedded in, a native kind's content, and the ready report a frame
 * makes. The reads are the socket's questions (`artifacts`, `artifact`, …),
 * asked through `useQuery`; what is here is what no question answers.
 *
 * ONE PLACE SPELLS EVERY PATH, so a route the engine moves is one edit, and a
 * screen never builds a URL by hand. Each writer returns a [KeyedRequest] the
 * caller sends through `useKeyedGesture` (one key per gesture, kept for the
 * retry an unknown answer asks for) or `keyedWrite`.
 */

import { pathSegments } from "~/lib/segment.ts";
import { pathSegment, rest } from "~/protocol/index.ts";
import type {
  ArtifactAudience,
  ArtifactDeckAnswer,
  DeckPlan,
  SlidesStartersAnswer,
  ArtifactDocAnswer,
  ArtifactFrame,
  ArtifactSource,
  ArtifactOutline,
  DashboardBoardAnswer,
  DashboardCatalogueAnswer,
  DashboardDatasetAnswer,
  DashboardSourcePreview,
  ArtifactUploadIntent,
  ArtifactCapability,
  ArtifactCheckAnswer,
  DataCollection,
  DataOp,
  ShareableRole,
} from "~/protocol/index.ts";
import type { KeyedRequest } from "../keyedWrite.ts";
import type { DesignLayout } from "./designLayout.ts";
import { audienceKey } from "./words.ts";

const base = (id: string) => `/artifacts/${pathSegment(id)}`;

/**
 * Where a new artifact is placed, as every create's body says it: the task it
 * is made for (`work_item`), which lists it on that task's Artifacts card, and
 * the folder it is filed in (`folder`, an id), which the engine admits only
 * for a reader who may file into that folder. Either left out is none.
 */
function placement(fields: { workItem?: string; folder?: string }): {
  work_item?: string;
  folder?: string;
} {
  return {
    ...(fields.workItem ? { work_item: fields.workItem } : {}),
    ...(fields.folder ? { folder: fields.folder } : {}),
  };
}

/** A publish names the head it was based on as `If-Match: "<n>"`, the form an artifact's head takes as an ETag. */
export function ifMatch(head: number): Record<string, string> {
  return { "If-Match": `"${head}"` };
}

/** Rename an artifact, or change its description or icon (`PATCH /artifacts/{id}`). */
export function patchArtifact(
  id: string,
  fields: { title?: string; description?: string; icon?: string },
): Omit<KeyedRequest, "key"> {
  return { method: "PATCH", path: base(id), body: fields };
}

/** Set or clear one version's label (`PATCH /artifacts/{id}/versions/{n}`). Empty clears it. */
export function labelVersion(id: string, number: number, label: string): Omit<KeyedRequest, "key"> {
  return {
    method: "PATCH",
    path: `${base(id)}/versions/${pathSegment(String(number))}`,
    body: { label },
  };
}

/** Publish version `number` again as a new version, every file carried (`POST …/versions/{n}/restore`). */
export function restoreVersion(
  id: string,
  number: number,
  head: number,
): Omit<KeyedRequest, "key"> {
  return {
    method: "POST",
    path: `${base(id)}/versions/${pathSegment(String(number))}/restore`,
    body: {},
    headers: ifMatch(head),
  };
}

/** Grant one audience a role (`PUT /artifacts/{id}/shares`). */
export function shareArtifact(
  id: string,
  audience: ArtifactAudience,
  role: ShareableRole,
): Omit<KeyedRequest, "key"> {
  const who: ArtifactAudience =
    audience.kind === "company"
      ? { kind: "company" }
      : { kind: audience.kind, id: audience.id ?? "" };
  return { method: "PUT", path: `${base(id)}/shares`, body: { audience: who, role } };
}

/** Withdraw one audience's share (`DELETE /artifacts/{id}/shares/{audience}`). */
export function unshareArtifact(id: string, audience: ArtifactAudience): Omit<KeyedRequest, "key"> {
  return {
    method: "DELETE",
    path: `${base(id)}/shares/${pathSegment(audienceKey(audience))}`,
  };
}

/** Move ownership to another seat (`POST /artifacts/{id}/transfer`): a person's, with a person present. */
export function transferArtifact(id: string, to: string): Omit<KeyedRequest, "key"> {
  return { method: "POST", path: `${base(id)}/transfer`, body: { to } };
}

/**
 * Link an artifact that already exists to a tracker work item (`POST
 * /artifacts/{id}/work-items`), so the item's Artifacts panel lists it. The
 * caller reads the artifact and may change the item; the engine answers the
 * item's current key as `work_item`, and a link the artifact already has
 * answers success and writes nothing.
 *
 * `after` is the `position` the item's create answered with (the act's
 * `ActResult.position`, such as `CREWLET_TRACKER_LOG@1:9`), for a link sent
 * right after filing the item: the engine reads the item from no earlier than
 * that position, waiting for a node that has not applied the create yet, and
 * answers `503 unavailable` with the `behind` refusal and a `Retry-After` if
 * the node does not reach it in time. Omit it for an item filed earlier.
 */
export function linkWorkItem(
  id: string,
  workItem: string,
  after?: string,
): Omit<KeyedRequest, "key"> {
  const body: { work_item: string; after?: string } = { work_item: workItem };
  if (after) body.after = after;
  return { method: "POST", path: `${base(id)}/work-items`, body };
}

/** Move an artifact to the trash (`DELETE /artifacts/{id}`). */
export function trashArtifact(id: string): Omit<KeyedRequest, "key"> {
  return { method: "DELETE", path: base(id) };
}

/**
 * File an artifact in a folder, or at the top level with `folder` `""` (`PUT
 * /artifacts/{id}/folder`): an editor's on the artifact who may file into the
 * folder. A move changes nobody's access to it.
 */
export function fileArtifact(id: string, folder: string): Omit<KeyedRequest, "key"> {
  return { method: "PUT", path: `${base(id)}/folder`, body: { folder } };
}

/**
 * File several artifacts at once (`POST /artifacts/folder-moves`), each
 * decided on its own and answered in `results`, at most
 * `ARTIFACT_MOVE_MAX_ITEMS` in one request.
 */
export function fileArtifacts(
  items: readonly { artifact: string; folder: string }[],
): Omit<KeyedRequest, "key"> {
  return { method: "POST", path: "/artifacts/folder-moves", body: { items } };
}

const folderBase = (id: string) => `/artifact-folders/${pathSegment(id)}`;

/**
 * Make a folder (`POST /artifact-folders`): in `parent`, which decides who
 * sees it, or at the top level, where a person says who sees it
 * (`visibility`) and the engine refuses it below.
 */
export function createFolder(fields: {
  name: string;
  parent?: string;
  visibility?: "company" | "private";
}): Omit<KeyedRequest, "key"> {
  return {
    method: "POST",
    path: "/artifact-folders",
    body: {
      name: fields.name,
      ...(fields.parent ? { parent: fields.parent } : { visibility: fields.visibility }),
    },
  };
}

/** Rename a folder (`PATCH /artifact-folders/{id}`): its owner's, its owner's lead's or an administrator's. */
export function renameFolder(id: string, name: string): Omit<KeyedRequest, "key"> {
  return { method: "PATCH", path: folderBase(id), body: { name } };
}

/**
 * Move a folder and everything in it under `parent`, `""` for the top level
 * (`PATCH /artifact-folders/{id}`). A move that would let more people see it
 * is refused `409 folder_widens` until it is sent again with `confirmWiden`.
 */
export function moveFolder(
  id: string,
  parent: string,
  confirmWiden = false,
): Omit<KeyedRequest, "key"> {
  return {
    method: "PATCH",
    path: folderBase(id),
    body: { parent, ...(confirmWiden ? { confirm_widen: true } : {}) },
  };
}

/**
 * Remove a folder (`DELETE /artifact-folders/{id}`): what was in it moves up
 * to its parent, and nothing is deleted or trashed.
 */
export function removeFolder(id: string): Omit<KeyedRequest, "key"> {
  return { method: "DELETE", path: folderBase(id) };
}

/** Take an artifact out of the trash (`POST /artifacts/{id}/restore`). */
export function untrashArtifact(id: string): Omit<KeyedRequest, "key"> {
  return { method: "POST", path: `${base(id)}/restore`, body: {} };
}

/** Destroy a trashed artifact for good (`POST /artifacts/{id}/purge`): an administrator's, with a fresh proof. */
export function purgeArtifact(id: string): Omit<KeyedRequest, "key"> {
  return { method: "POST", path: `${base(id)}/purge`, body: {} };
}

/** Keep an artifact on the reader's own pinned list, or drop it (`PUT`/`DELETE /artifacts/{id}/pin`). */
export function pinArtifact(id: string, pinned: boolean): Omit<KeyedRequest, "key"> {
  return pinned
    ? { method: "PUT", path: `${base(id)}/pin`, body: {} }
    : { method: "DELETE", path: `${base(id)}/pin` };
}

/** Start a native sheet: a sheet artifact whose content is its datastore (`POST /artifacts`). */
export function createSheet(fields: {
  title: string;
  description?: string;
  workItem?: string;
  /** The folder it is filed in (an id), at the top level when left out. */
  folder?: string;
}): Omit<KeyedRequest, "key"> {
  return {
    method: "POST",
    path: "/artifacts",
    body: {
      kind: "sheet",
      title: fields.title,
      ...(fields.description ? { description: fields.description } : {}),
      ...placement(fields),
    },
  };
}

/**
 * Create a doc or a deck from the engine's starter (`POST /artifacts` with
 * `starter`): the engine publishes its own first version, held to the kind's
 * format, so the dashboard sends no upload. A doc may carry the text its
 * writer already wrote (`doc`), which the engine writes under the title's
 * heading in place of the starter's line.
 */
export function createFromStarter(fields: {
  kind: "doc" | "slides";
  title: string;
  workItem?: string;
  /** The folder it is filed in (an id), at the top level when left out. */
  folder?: string;
  /** A doc's text as its writer wrote it, under the title's heading. Doc only. */
  doc?: { body: string };
  /** A deck's plan: the theme it is drawn in and the slides it opens with. Slides only. */
  deck?: DeckPlan;
  /** Who else holds it from the start, and with which role: an agent asked to draft it. */
  shares?: { audience: ArtifactAudience; role: ShareableRole }[];
}): Omit<KeyedRequest, "key"> {
  return {
    method: "POST",
    path: "/artifacts",
    body: {
      kind: fields.kind,
      title: fields.title,
      starter: true,
      ...placement(fields),
      ...(fields.kind === "doc" && fields.doc ? { doc: { body: fields.doc.body } } : {}),
      ...(fields.kind === "slides" && fields.deck ? { deck: fields.deck } : {}),
      ...(fields.shares && fields.shares.length > 0 ? { shares: fields.shares } : {}),
    },
  };
}

/**
 * Make a design from the boards a person laid out (`POST /artifacts` with the
 * design layout starter): the engine publishes each board as a placeholder
 * page of its size, and `shares` grant who builds it (an agent's seat) its
 * role before the first draft.
 */
export function createDesign(fields: {
  title: string;
  layout: DesignLayout;
  shares?: { audience: ArtifactAudience; role: ShareableRole }[];
  workItem?: string;
  /** The folder it is filed in (an id), at the top level when left out. */
  folder?: string;
}): Omit<KeyedRequest, "key"> {
  return {
    method: "POST",
    path: "/artifacts",
    body: {
      kind: "design",
      title: fields.title,
      starter: true,
      layout: fields.layout,
      ...(fields.shares?.length ? { shares: fields.shares } : {}),
      ...placement(fields),
    },
  };
}

/**
 * Create an artifact whose first version is files an upload brought
 * ([uploadTexts] with no artifact): a dashboard a person composed, held to
 * its kind's format by the engine at publish.
 */
export function createFromUpload(fields: {
  kind: "dashboard";
  title: string;
  upload: string;
  files: PublishFile[];
  entry: string;
  workItem?: string;
  /** The folder it is filed in (an id), at the top level when left out. */
  folder?: string;
}): Omit<KeyedRequest, "key"> {
  return {
    method: "POST",
    path: "/artifacts",
    body: {
      kind: fields.kind,
      title: fields.title,
      upload: fields.upload,
      files: fields.files,
      entry: fields.entry,
      ...placement(fields),
    },
  };
}

/** Replace a store's collections (`PUT /artifacts/{id}/data/schema`). Every collection kept is named. */
export function configureSchema(
  id: string,
  collections: DataCollection[],
): Omit<KeyedRequest, "key"> {
  return { method: "PUT", path: `${base(id)}/data/schema`, body: { collections } };
}

/**
 * Where a datastore write comes from: the dashboard's own screens, or a page
 * script the bridge relays for the version it runs as. The engine checks a
 * bridge write's version declares the capability it uses.
 */
export type DataVia = { via: "dashboard" } | { via: "bridge"; version: number };

/** One batch of datastore ops (`POST /artifacts/{id}/data/writes`), all or nothing. */
export function writeData(
  id: string,
  ops: DataOp[],
  clientID: string,
  via: DataVia,
): Omit<KeyedRequest, "key"> {
  return {
    method: "POST",
    path: `${base(id)}/data/writes`,
    body: { ops, client_id: clientID, ...via },
  };
}

/** The inverse of one write, by its place in the store's history (`POST /artifacts/{id}/data/undo`). */
export function undoData(
  id: string,
  seq: number,
  clientID: string,
  via: DataVia,
): Omit<KeyedRequest, "key"> {
  return {
    method: "POST",
    path: `${base(id)}/data/undo`,
    body: { seq, client_id: clientID, ...via },
  };
}

/** Set or remove one of the viewer's own state keys (`POST /artifacts/{id}/data/state`). */
export function writeState(
  id: string,
  change: { key: string; value?: unknown; delete?: boolean },
  clientID: string,
  version: number,
): Omit<KeyedRequest, "key"> {
  return {
    method: "POST",
    path: `${base(id)}/data/state`,
    body: {
      key: change.key,
      ...(change.delete ? { delete: true } : { value: change.value }),
      client_id: clientID,
      via: "bridge",
      version,
    },
  };
}

/** A collection exported as CSV, under its read rule, to an editor (`GET …/data/export.csv`), as its text. */
export async function exportCSV(id: string, collection: string): Promise<string> {
  const blob = await rest.blob(
    `/artifacts/${pathSegment(id)}/data/export.csv?${new URLSearchParams({ collection })}`,
  );
  return blob.text();
}

/** One file of a version as raw bytes, an attachment on the API origin (`GET …/versions/{n}/files/{path}`). */
export function versionFile(
  id: string,
  number: number,
  path: string,
  signal?: AbortSignal,
): Promise<Blob> {
  return rest.blob(
    `/artifacts/${pathSegment(id)}/versions/${pathSegment(String(number))}/files/${pathSegments(path)}`,
    signal,
  );
}

/**
 * Mint the frame a version is embedded in (`POST /artifacts/{id}/tickets`):
 * a view ticket bound to this browser's sign-in, the artifact origin's URL
 * under it, and the sandbox and allow attributes set verbatim. Zero is the
 * head. `document` names the page the frame opens: left out for a page's
 * entry, one artboard's path for a design, whose every board is a frame of its
 * own. Not keyed: a ticket is a read, and the engine records the open itself.
 */
export async function mintFrame(
  id: string,
  version: number,
  document?: string,
  options: ReadOptions = {},
): Promise<ArtifactFrame> {
  const res = await rest.request("POST", `/artifacts/${pathSegment(id)}/tickets`, {
    body: {
      ...(version > 0 ? { version } : {}),
      ...(document ? { document } : {}),
    },
    ...previewQuery(options),
  });
  return res.body as ArtifactFrame;
}

/** How a content read is made. */
export interface ReadOptions {
  /**
   * A gallery's preview, not the reader opening the artifact
   * (`?purpose=preview`): the engine answers the same read under the same
   * authorization and records no open, so browsing a gallery never fills an
   * artifact's Seen by list or reorders the reader's Recently opened. A
   * preview's frame is granted only what drawing needs, for minutes, and
   * files no ready report.
   */
  preview?: boolean;
  signal?: AbortSignal;
}

function previewQuery(options: ReadOptions): {
  query?: Record<string, string>;
  signal?: AbortSignal;
} {
  return {
    ...(options.preview ? { query: { purpose: "preview" } } : {}),
    ...(options.signal ? { signal: options.signal } : {}),
  };
}

/** One text file of a version, read as text: a slide's source an edit replaces whole. */
export async function versionText(
  id: string,
  number: number,
  path: string,
  signal?: AbortSignal,
): Promise<string> {
  return (await versionFile(id, number, path, signal)).text();
}

/** A deck's version as its canonical tree (`GET …/versions/{n}/deck`): what every slide is drawn from. */
export async function readDeck(
  id: string,
  version: number,
  signal: AbortSignal,
): Promise<ArtifactDeckAnswer> {
  return (await rest.get(
    `/artifacts/${pathSegment(id)}/versions/${pathSegment(String(version))}/deck`,
    signal,
  )) as ArtifactDeckAnswer;
}

/**
 * What a deck can begin from (`GET /artifacts/starters/slides`): every theme
 * drawn as a deck of every layout, the layouts, and the default plan. The
 * same for every caller and every version of the engine's process.
 */
export async function readSlidesStarters(signal: AbortSignal): Promise<SlidesStartersAnswer> {
  return (await rest.get("/artifacts/starters/slides", signal)) as SlidesStartersAnswer;
}

/** A version's outline (`GET …/versions/{n}/outline`): what the gallery draws a native cover from. */
export async function readOutline(
  id: string,
  version: number,
  signal: AbortSignal,
): Promise<ArtifactOutline> {
  return (await rest.get(
    `/artifacts/${pathSegment(id)}/versions/${pathSegment(String(version))}/outline`,
    signal,
  )) as ArtifactOutline;
}

/** A dashboard's version as its board (`GET …/versions/{n}/dashboard`): the spec and every dataset's shape, no row. */
export async function readBoard(
  id: string,
  version: number,
  signal: AbortSignal,
  options: Omit<ReadOptions, "signal"> = {},
): Promise<DashboardBoardAnswer> {
  const res = await rest.request(
    "GET",
    `/artifacts/${pathSegment(id)}/versions/${pathSegment(String(version))}/dashboard`,
    previewQuery({ ...options, signal }),
  );
  return res.body as DashboardBoardAnswer;
}

/**
 * One dataset of a dashboard as this reader may read it (`GET
 * …/versions/{n}/sources/{dataset}`), with the board's filters as the query.
 */
export async function readDataset(
  id: string,
  version: number,
  dataset: string,
  params: Readonly<Record<string, string>>,
  signal: AbortSignal,
): Promise<DashboardDatasetAnswer> {
  const res = await rest.request(
    "GET",
    `/artifacts/${pathSegment(id)}/versions/${pathSegment(String(version))}/sources/${pathSegment(dataset)}`,
    { query: { ...params }, signal },
  );
  return res.body as DashboardDatasetAnswer;
}

/**
 * One engine source's rows as this reader may read them, before any board
 * exists (`GET /artifacts/sources/preview/{name}`), with the source's
 * parameters as the query: what New dashboard draws its suggestions from.
 */
export async function previewSource(
  name: string,
  params: Readonly<Record<string, string>>,
  signal: AbortSignal,
): Promise<DashboardSourcePreview> {
  const res = await rest.request("GET", `/artifacts/sources/preview/${pathSegment(name)}`, {
    query: { ...params },
    signal,
  });
  return res.body as DashboardSourcePreview;
}

/** The engine source catalogue a dashboard's engine datasets read (`GET /artifacts/sources`). */
export async function readCatalogue(signal: AbortSignal): Promise<DashboardCatalogueAnswer> {
  return (await rest.get("/artifacts/sources", signal)) as DashboardCatalogueAnswer;
}

/**
 * Publish a new version based on `head` (`POST …/versions` with `If-Match`):
 * a patch of exact edits, removals and the files an upload brought, and the
 * entry where it moves (a doc gaining its index); or a whole version, every
 * file named (a page saved again from its create page), with the capabilities
 * it declares.
 */
export function publishVersion(
  id: string,
  head: number,
  body:
    | {
        mode: "patch";
        edits: unknown[];
        files: unknown[];
        upload?: string;
        entry?: string;
        label?: string;
      }
    | {
        mode: "whole";
        files: PublishFile[];
        upload?: string;
        entry: string;
        capabilities: readonly ArtifactCapability[];
      },
): Omit<KeyedRequest, "key"> {
  return { method: "POST", path: `${base(id)}/versions`, body, headers: ifMatch(head) };
}

/**
 * Make an HTML page from the files an upload brought (`POST /artifacts`):
 * the engine lints every page, copies its allowlisted CDN files in and
 * publishes version 1, owned by the caller and shared with nobody.
 */
export function createPage(fields: {
  title: string;
  upload: string;
  files: PublishFile[];
  entry: string;
  capabilities: readonly ArtifactCapability[];
  workItem?: string;
  /** The folder it is filed in (an id), at the top level when left out. */
  folder?: string;
}): Omit<KeyedRequest, "key"> {
  return {
    method: "POST",
    path: "/artifacts",
    body: {
      kind: "html",
      title: fields.title,
      upload: fields.upload,
      files: fields.files,
      entry: fields.entry,
      capabilities: fields.capabilities,
      ...placement(fields),
    },
  };
}

/**
 * What a publish of a page's text files would say, every refusal and warning
 * with its file and line, with nothing written (`POST /artifacts/checks`).
 * Not keyed: a check is no operation. `signal` is the caller's, which aborts
 * a check the person's next edit superseded.
 */
export async function checkPage(
  files: readonly { path: string; text: string }[],
  entry: string,
  signal?: AbortSignal,
): Promise<ArtifactCheckAnswer> {
  const answer = await rest.request("POST", "/artifacts/checks", {
    body: { kind: "html", entry, files: files.map((f) => ({ path: f.path, text: f.text })) },
    ...(signal ? { signal } : {}),
  });
  return answer.body as ArtifactCheckAnswer;
}

/**
 * A file a publish names: one an upload brought, one carried by digest, one
 * carried from a retained version of the same artifact (`from`, no byte moved
 * and none read here), or one removed.
 */
export type PublishFile =
  | { path: string; upload: true }
  | { path: string; carry: string }
  | { path: string; from: number }
  | { path: string; remove: true };

/** A file a save brings: its text, or a picture's bytes. */
export interface UploadFile {
  path: string;
  body: string | Blob;
}

/** The SHA-256 of bytes, lowercase hex: how the engine names a file's bytes. */
async function digestOf(bytes: BufferSource): Promise<string> {
  const sum = await crypto.subtle.digest("SHA-256", bytes);
  return [...new Uint8Array(sum)].map((b) => b.toString(16).padStart(2, "0")).join("");
}

/** The SHA-256 of text as UTF-8, lowercase hex: how the engine names a file's bytes. */
export async function textDigest(text: string): Promise<string> {
  return digestOf(new TextEncoder().encode(text));
}

/** A file's digest and size, as its upload declares them. */
async function declared(f: UploadFile): Promise<{ sha256: string; size: number }> {
  if (typeof f.body === "string") {
    const bytes = new TextEncoder().encode(f.body);
    return { sha256: await digestOf(bytes), size: bytes.length };
  }
  return { sha256: await digestOf(await f.body.arrayBuffer()), size: f.body.size };
}

/**
 * Bring files into an artifact's next version, or into a new artifact's
 * first (`id` null): an upload intent (`POST /artifacts/{id}/uploads`, or
 * `POST /artifacts/uploads` for a create, keyed by `key`), then each file's
 * bytes put through the engine. Answers the upload to publish with and the
 * file each path is named by; a file the artifact already holds is carried by
 * its digest and moves no byte.
 *
 * THE ENGINE NEVER HANDS THE DASHBOARD A PUT STRAIGHT TO THE BUCKET: a
 * session's intent is the dashboard's (`originOf`), and `reachesBucket`
 * (internal/artifactupload) streams every one of its files through the engine,
 * since the dashboard's own policy reaches no other origin. A direct transfer
 * here is that contract broken, and refused rather than followed.
 */
export async function uploadFiles(
  id: string | null,
  files: readonly UploadFile[],
  key: string,
): Promise<{ upload: string; files: PublishFile[] }> {
  const facts = await Promise.all(files.map(declared));
  const entries = files.map((f, i) => ({ path: f.path, ...facts[i]! }));
  const ask = { body: { entries }, headers: { "Idempotency-Key": key } };
  const intent = (
    id === null
      ? await rest.request("POST", "/artifacts/uploads", ask)
      : await rest.request("POST", `/artifacts/${pathSegment(id)}/uploads`, ask)
  ).body as ArtifactUploadIntent;
  const carried = new Set(intent.carried ?? []);
  for (const t of intent.transfers ?? []) {
    if (t.direct) throw new Error(`the engine asked for ${t.path} to go straight to the bucket`);
    const f = files.find((x) => x.path === t.path);
    if (!f) throw new Error(`the engine asked for ${t.path}, which this save does not bring`);
    // THE PATH IS BUILT HERE, from the upload and the file, never followed
    // from the answer: the transfer route is the engine's own, and a client
    // that put bytes wherever an answer pointed would trust that answer with
    // the reader's credential.
    // THE PATH A LITERAL AT EACH CALL, which is what the dev proxy's gate reads.
    if (typeof f.body === "string") {
      await rest.putText(
        `/artifacts/uploads/${pathSegment(intent.upload)}/files/${pathSegments(t.path)}`,
        f.body,
      );
    } else {
      await rest.putBytes(
        `/artifacts/uploads/${pathSegment(intent.upload)}/files/${pathSegments(t.path)}`,
        f.body,
      );
    }
  }
  return {
    upload: intent.upload,
    files: files.map((f, i) =>
      carried.has(f.path)
        ? { path: f.path, carry: facts[i]!.sha256 }
        : { path: f.path, upload: true },
    ),
  };
}

/**
 * Bring text files into an artifact's next version, or into a new artifact's
 * first (`id` null): [uploadFiles] for a doc's tabs, a deck's slides or a
 * composed dashboard's spec.
 */
export function uploadTexts(
  id: string | null,
  files: readonly { path: string; text: string }[],
  key: string,
): Promise<{ upload: string; files: PublishFile[] }> {
  return uploadFiles(
    id,
    files.map((f) => ({ path: f.path, body: f.text })),
    key,
  );
}

/** A doc's version tab by tab (`GET …/versions/{n}/doc`): what the dashboard draws it from. */
export async function readDoc(
  id: string,
  version: number,
  signal: AbortSignal,
): Promise<ArtifactDocAnswer> {
  return (await rest.get(
    `/artifacts/${pathSegment(id)}/versions/${pathSegment(String(version))}/doc`,
    signal,
  )) as ArtifactDocAnswer;
}

/** A version's entry as text (`GET …/versions/{n}/source`): a design's canvas, which the dashboard draws itself. */
export async function readSource(
  id: string,
  version: number,
  signal: AbortSignal,
): Promise<ArtifactSource> {
  return (await rest.get(
    `/artifacts/${pathSegment(id)}/versions/${pathSegment(String(version))}/source`,
    signal,
  )) as ArtifactSource;
}

/**
 * Report how long one frame took from its ticket to its page's hello, or that
 * it failed (`POST /artifacts/ready`): the artifact-ready service objective,
 * the one indicator that sees an outage keeping a page from loading at all.
 * Each frame reports once; a report the engine refuses changes nothing the
 * reader sees, so it is fire and forget.
 */
export function reportReady(
  frameURL: string,
  outcome: "ready" | "failed",
  elapsedMs: number,
): void {
  void rest
    .post("/artifacts/ready", {
      frame_url: frameURL,
      outcome,
      elapsed_ms: Math.max(0, Math.round(elapsedMs)),
    })
    .catch(() => {
      // A REPORT IS EVIDENCE FOR THE ENGINE, never a gesture of the reader's:
      // a refused or unanswered one leaves the page exactly as it was.
    });
}

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

import { rest } from "~/protocol/index.ts";
import type {
  ArtifactAudience,
  ArtifactDeckAnswer,
  ArtifactDocAnswer,
  ArtifactFrame,
  ArtifactSource,
  ArtifactOutline,
  DashboardBoardAnswer,
  DashboardCatalogueAnswer,
  DashboardDatasetAnswer,
  ArtifactUploadIntent,
  DataCollection,
  DataOp,
  ShareableRole,
} from "~/protocol/index.ts";
import type { KeyedRequest } from "../keyedWrite.ts";
import type { DesignLayout } from "./designLayout.ts";
import { audienceKey } from "./words.ts";

const base = (id: string) => `/artifacts/${encodeURIComponent(id)}`;

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
  return { method: "PATCH", path: `${base(id)}/versions/${number}`, body: { label } };
}

/** Publish version `number` again as a new version, every file carried (`POST …/versions/{n}/restore`). */
export function restoreVersion(
  id: string,
  number: number,
  head: number,
): Omit<KeyedRequest, "key"> {
  return {
    method: "POST",
    path: `${base(id)}/versions/${number}/restore`,
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
    path: `${base(id)}/shares/${audienceKey(audience).split(":").map(encodeURIComponent).join(":")}`,
  };
}

/** Move ownership to another seat (`POST /artifacts/{id}/transfer`): a person's, with a person present. */
export function transferArtifact(id: string, to: string): Omit<KeyedRequest, "key"> {
  return { method: "POST", path: `${base(id)}/transfer`, body: { to } };
}

/** Move an artifact to the trash (`DELETE /artifacts/{id}`). */
export function trashArtifact(id: string): Omit<KeyedRequest, "key"> {
  return { method: "DELETE", path: base(id) };
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
}): Omit<KeyedRequest, "key"> {
  return {
    method: "POST",
    path: "/artifacts",
    body: {
      kind: "sheet",
      title: fields.title,
      ...(fields.description ? { description: fields.description } : {}),
      ...(fields.workItem ? { work_item: fields.workItem } : {}),
    },
  };
}

/**
 * Create a doc or a deck from the engine's starter (`POST /artifacts` with
 * `starter`): the engine publishes its own first version, held to the kind's
 * format, so the dashboard sends no upload.
 */
export function createFromStarter(fields: {
  kind: "doc" | "slides";
  title: string;
  workItem?: string;
}): Omit<KeyedRequest, "key"> {
  return {
    method: "POST",
    path: "/artifacts",
    body: {
      kind: fields.kind,
      title: fields.title,
      starter: true,
      ...(fields.workItem ? { work_item: fields.workItem } : {}),
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
      ...(fields.workItem ? { work_item: fields.workItem } : {}),
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
 * Link an artifact that already exists to a tracker item
 * (`POST /artifacts/{id}/work-items`), so the item's Artifacts card lists it.
 * The engine checks the caller can open the artifact and change the item.
 */
export function linkWorkItem(id: string, workItem: string): Omit<KeyedRequest, "key"> {
  return { method: "POST", path: `${base(id)}/work-items`, body: { work_item: workItem } };
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
    `/artifacts/${encodeURIComponent(id)}/data/export.csv?collection=${encodeURIComponent(collection)}`,
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
    `/artifacts/${encodeURIComponent(id)}/versions/${number}/files/${path.split("/").map(encodeURIComponent).join("/")}`,
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
  const res = await rest.request("POST", `/artifacts/${encodeURIComponent(id)}/tickets`, {
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
    `/artifacts/${encodeURIComponent(id)}/versions/${version}/deck`,
    signal,
  )) as ArtifactDeckAnswer;
}

/** A version's outline (`GET …/versions/{n}/outline`): what the gallery draws a native cover from. */
export async function readOutline(
  id: string,
  version: number,
  signal: AbortSignal,
): Promise<ArtifactOutline> {
  return (await rest.get(
    `/artifacts/${encodeURIComponent(id)}/versions/${version}/outline`,
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
    `/artifacts/${encodeURIComponent(id)}/versions/${version}/dashboard`,
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
    `/artifacts/${encodeURIComponent(id)}/versions/${version}/sources/${encodeURIComponent(dataset)}`,
    { query: { ...params }, signal },
  );
  return res.body as DashboardDatasetAnswer;
}

/** The engine source catalogue a dashboard's engine datasets read (`GET /artifacts/sources`). */
export async function readCatalogue(signal: AbortSignal): Promise<DashboardCatalogueAnswer> {
  return (await rest.get("/artifacts/sources", signal)) as DashboardCatalogueAnswer;
}

/**
 * Publish a new version based on `head` (`POST …/versions` with `If-Match`):
 * a patch of exact edits, removals and the files an upload brought, and the
 * entry where it moves (a doc gaining its index).
 */
export function publishVersion(
  id: string,
  head: number,
  body: {
    mode: "patch";
    edits: unknown[];
    files: unknown[];
    upload?: string;
    entry?: string;
    label?: string;
  },
): Omit<KeyedRequest, "key"> {
  return { method: "POST", path: `${base(id)}/versions`, body, headers: ifMatch(head) };
}

/** A file a publish names: one an upload brought, one carried by digest, or one removed. */
export type PublishFile =
  { path: string; upload: true } | { path: string; carry: string } | { path: string; remove: true };

/** The SHA-256 of text as UTF-8, lowercase hex: how the engine names a file's bytes. */
export async function textDigest(text: string): Promise<string> {
  const sum = await crypto.subtle.digest("SHA-256", new TextEncoder().encode(text));
  return [...new Uint8Array(sum)].map((b) => b.toString(16).padStart(2, "0")).join("");
}

/**
 * Bring text files into an artifact's next version: an upload intent
 * (`POST /artifacts/{id}/uploads`, keyed by `key`), then each file's bytes put
 * through the engine, the only transfer the dashboard is handed (its own
 * policy reaches no other origin). Answers the upload to publish with and the
 * file each path is named by; a file the artifact already holds is carried by
 * its digest and moves no byte.
 */
export async function uploadTexts(
  id: string,
  files: readonly { path: string; text: string }[],
  key: string,
): Promise<{ upload: string; files: PublishFile[] }> {
  const digests = await Promise.all(files.map((f) => textDigest(f.text)));
  const entries = files.map((f, i) => ({
    path: f.path,
    sha256: digests[i]!,
    size: new TextEncoder().encode(f.text).length,
  }));
  const intent = (
    await rest.request("POST", `/artifacts/${encodeURIComponent(id)}/uploads`, {
      body: { entries },
      headers: { "Idempotency-Key": key },
    })
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
    const segments = t.path.split("/").map(encodeURIComponent).join("/");
    await rest.putText(
      `/artifacts/uploads/${encodeURIComponent(intent.upload)}/files/${segments}`,
      f.text,
    );
  }
  return {
    upload: intent.upload,
    files: files.map((f, i) =>
      carried.has(f.path) ? { path: f.path, carry: digests[i]! } : { path: f.path, upload: true },
    ),
  };
}

/** A doc's version tab by tab (`GET …/versions/{n}/doc`): what the dashboard draws it from. */
export async function readDoc(
  id: string,
  version: number,
  signal: AbortSignal,
): Promise<ArtifactDocAnswer> {
  return (await rest.get(
    `/artifacts/${encodeURIComponent(id)}/versions/${version}/doc`,
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
    `/artifacts/${encodeURIComponent(id)}/versions/${version}/source`,
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

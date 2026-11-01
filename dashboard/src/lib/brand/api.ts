/**
 * The adopted brand's REST surface, and the one gesture that saves it: publish
 * a version of the company's kit, then adopt it.
 *
 * # Save is two writes, said as one
 *
 * An administrator's Save on Company › Brand publishes a version of the
 * company's kit (`POST /artifacts`, or `POST /artifacts/{id}/versions`) and
 * adopts that version (`POST /brand/adoptions`, an administrator's, a person's,
 * with a recent proof). Each write is keyed; a save whose answer nobody knows
 * is sent again with the same keys, which the engine resolves to the first
 * attempt's writes rather than a second version. A version that landed and
 * was not adopted (a contrast refusal, a proof the person declined) is said as
 * exactly that: the version stays in the kit's history, and the page offers to
 * review and adopt it.
 *
 * A FIRST KIT is shared with the company to view, so the version and the
 * history the Brand page names open for every reader; changing it stays its
 * owner's, an editor's it is shared with, or an administrator's.
 *
 * # Every file travels with the version
 *
 * A version names every file it holds, so a save brings each logo and face
 * the brand keeps: a file the person picked, or the adopted snapshot's own
 * bytes (`GET /brand/files/{hash}`), which the engine carries by digest
 * without moving a byte when the kit already holds it. Nothing depends on the
 * adopted version still being retained, or on the kit still being live: a kit
 * that is gone is replaced by a new one holding the same files.
 */

import { isAbort, newActOpID, pathSegment, rest, RestError } from "~/protocol/index.ts";
import { keyedWrite, type KeyedAnswer, type KeyedRequest } from "../keyedWrite.ts";
import { refusalText } from "../refusal.ts";
import {
  publishVersion,
  uploadFiles,
  type PublishFile,
  type UploadFile,
} from "../artifacts/api.ts";
import { serializeBrandDocument } from "./brand.ts";
import type { BrandDocument } from "~/protocol/index.ts";

/** The kit's index: its entry, and the one file every version holds. */
export const BRAND_ENTRY = "brand.json";

/** Adopt one version of a kit as the company's brand (`POST /brand/adoptions`). */
export function adoptBrand(kit: string, version: number): Omit<KeyedRequest, "key"> {
  return { method: "POST", path: "/brand/adoptions", body: { kit, version } };
}

/** Stop using the adopted brand (`DELETE /brand/adoptions/current`): artifacts return to Crewlet's look. */
export function clearBrand(): Omit<KeyedRequest, "key"> {
  return { method: "DELETE", path: "/brand/adoptions/current" };
}

/**
 * One file the adopted brand serves, as raw bytes (`GET /brand/files/{hash}`):
 * an attachment on the API origin, an SVG as text, shown through an object URL
 * typed from the adoption's own content type and never from the response.
 */
export function brandFile(hash: string, signal?: AbortSignal): Promise<Blob> {
  return rest.blob(`/brand/files/${pathSegment(hash)}`, signal);
}

/**
 * A file a brand save brings: its path in the kit's version and its bytes, or,
 * for a file a retained version of the same kit already holds, that version
 * (`from`), which the engine carries without a byte moving.
 */
export type BrandSaveFile = { path: string; body: Blob } | { path: string; from: number };

/** One save of the company's brand. */
export interface BrandSave {
  /** The kit the adopted brand came from, or null when none is adopted and a kit is made. */
  kit: string | null;
  /** The kit's head the save was decided against (`If-Match`). */
  head: number;
  /** A new kit's title. */
  title: string;
  document: BrandDocument;
  /** Every logo and face file the document names. */
  files: readonly BrandSaveFile[];
}

/**
 * The operation keys one save sends: minted when the person presses Save, and
 * kept for the retry an unknown answer asks for, so a retry is the same writes.
 */
export interface BrandSaveKeys {
  upload: string;
  write: string;
  adopt: string;
  /** The upload and the create of a new kit, where the adopted kit is gone. */
  newKitUpload: string;
  newKitWrite: string;
}

/** A fresh set of keys, for a save that is not the retry of an unknown one. */
export function newBrandSaveKeys(): BrandSaveKeys {
  return {
    upload: newActOpID(),
    write: newActOpID(),
    adopt: newActOpID(),
    newKitUpload: newActOpID(),
    newKitWrite: newActOpID(),
  };
}

/** What one save came to. */
export type BrandSaveAnswer =
  | {
      kind: "adopted";
      kit: string;
      version: number;
      /** The adoption is durable and this node has not applied it yet. */
      pending: boolean;
    }
  | {
      /** The version landed; the adoption did not, for the reason `text` says. */
      kind: "published";
      kit: string;
      version: number;
      text: string;
    }
  | { kind: "unknown"; text: string }
  | { kind: "refused"; text: string; error?: RestError };

/** Whether a refusal means the kit itself is gone: trashed, purged, or never readable here. */
function kitGone(answer: KeyedAnswer): boolean {
  return (
    answer.kind === "refused" &&
    answer.error !== undefined &&
    (answer.error.status === 404 || answer.error.status === 410 || answer.error.code === "trashed")
  );
}

/** The version a create or a publish landed (`artifactsapi.publishedAnswer`: `artifact`, `number`). */
function versionOf(answer: KeyedAnswer): { kit: string; version: number } | null {
  if (answer.kind !== "done") return null;
  const kit = answer.body.artifact;
  const version = answer.body.number;
  return typeof kit === "string" && typeof version === "number" ? { kit, version } : null;
}

/** An upload's failure, answered as a keyed write is: unknown where nothing answered, else the engine's words. */
function uploadFailure(err: unknown, key: string): KeyedAnswer {
  if (err instanceof RestError && (err.unanswered || err.status === 503)) {
    return {
      kind: "unknown",
      key,
      text: "The files may not have arrived. Try again. It won’t happen twice.",
    };
  }
  return {
    kind: "refused",
    text: refusalText(err),
    stale: false,
    ...(err instanceof RestError ? { error: err } : {}),
  };
}

/** Bring a save's files to `kit` (null: a new kit), then publish or create the version. */
async function writeVersion(
  save: BrandSave,
  kit: string | null,
  keys: { upload: string; write: string },
): Promise<KeyedAnswer> {
  const files: UploadFile[] = [
    { path: BRAND_ENTRY, body: serializeBrandDocument(save.document) },
    ...save.files.flatMap((f) => ("body" in f ? [{ path: f.path, body: f.body }] : [])),
  ];
  const carried: PublishFile[] = save.files.flatMap((f) =>
    "from" in f ? [{ path: f.path, from: f.from }] : [],
  );
  let up: Awaited<ReturnType<typeof uploadFiles>>;
  try {
    up = await uploadFiles(kit, files, keys.upload);
  } catch (err) {
    if (isAbort(err)) throw err;
    return uploadFailure(err, keys.upload);
  }
  const request: KeyedRequest =
    kit === null
      ? {
          method: "POST",
          path: "/artifacts",
          body: {
            kind: "brand",
            title: save.title,
            upload: up.upload,
            files: [...up.files, ...carried],
            entry: BRAND_ENTRY,
            // THE COMPANY'S KIT IS EVERY READER'S TO OPEN, as its adopted
            // brand is everyone's to read: its versions and history are
            // where the Brand page's version and History lead.
            shares: [{ audience: { kind: "company" }, role: "viewer" }],
          },
          key: keys.write,
        }
      : {
          ...publishVersion(kit, save.head, {
            mode: "whole",
            files: [...up.files, ...carried],
            upload: up.upload,
            entry: BRAND_ENTRY,
            capabilities: [],
          }),
          key: keys.write,
        };
  return keyedWrite(request);
}

/** What a draft save came to: the version that landed, or why none did. */
export type BrandDraftAnswer =
  | { kind: "saved"; kit: string; version: number; pending: boolean }
  | { kind: "unknown"; text: string }
  | { kind: "refused"; text: string; error?: RestError };

/**
 * Publish the save as a version of the company's kit (a first kit where
 * `save.kit` is null), adopting nothing: a draft, which an administrator
 * adopts later. Rejects only for the caller's own abort.
 */
export async function publishBrandDraft(
  save: BrandSave,
  keys: Pick<BrandSaveKeys, "upload" | "write" | "newKitUpload" | "newKitWrite">,
): Promise<BrandDraftAnswer> {
  let written = await writeVersion(save, save.kit, keys);
  // THE ADOPTED KIT IS GONE (its owner trashed it; the adoption's snapshot
  // outlives it): the brand is saved into a new kit holding the same files,
  // under keys of its own so a retry of either is the same write.
  if (save.kit !== null && kitGone(written)) {
    written = await writeVersion(save, null, {
      upload: keys.newKitUpload,
      write: keys.newKitWrite,
    });
  }
  if (written.kind === "unknown") return { kind: "unknown", text: written.text };
  if (written.kind === "refused") {
    return {
      kind: "refused",
      text: written.text,
      ...(written.error ? { error: written.error } : {}),
    };
  }
  const landed = versionOf(written);
  if (!landed) {
    return { kind: "refused", text: "The engine saved the brand and did not say which version." };
  }
  return { kind: "saved", ...landed, pending: written.pending };
}

/**
 * Publish the save as a version of the company's kit and adopt it. Rejects only
 * for the caller's own abort.
 */
export async function saveBrand(save: BrandSave, keys: BrandSaveKeys): Promise<BrandSaveAnswer> {
  const written = await publishBrandDraft(save, keys);
  if (written.kind !== "saved") return written;
  const landed = { kit: written.kit, version: written.version };
  const adopted = await keyedWrite({ ...adoptBrand(landed.kit, landed.version), key: keys.adopt });
  switch (adopted.kind) {
    case "done":
      return { kind: "adopted", ...landed, pending: adopted.pending };
    case "unknown":
      return { kind: "unknown", text: adopted.text };
    case "refused":
      return {
        kind: "published",
        ...landed,
        text: `Version ${landed.version} of the brand kit is saved and not published yet. ${adopted.text}`,
      };
  }
}

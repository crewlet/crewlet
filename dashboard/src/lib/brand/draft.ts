/**
 * An edit of the company's brand before it is saved: the next document, and
 * the files a person picked for it.
 *
 * # Drawn before it is saved
 *
 * The preview beside the cards draws the edit as it stands: a picked logo from
 * the browser's own copy of the file, a picked face registered from its bytes,
 * and everything the edit did not touch from the adopted brand ([draftSource]).
 *
 * # Saved whole
 *
 * A version names every file it holds, so a save brings every file the next
 * document names: what the person picked, and the adopted brand's own bytes for
 * the rest ([saveFiles]).
 *
 * # Refused here before the engine refuses it
 *
 * A file of the wrong type or past the format's size is refused when it is
 * picked, with the bound and what to bring instead, rather than after an
 * upload ([logoProblem], [fontProblem]). The engine checks every file again at
 * publish, and its refusal is the one that decides.
 */

import {
  BRAND_FAMILY_NAME_MAX_BYTES,
  BRAND_FAMILY_NAME_PATTERN,
  BRAND_FONT_FILES_MAX,
  BRAND_FONT_FILE_MAX_BYTES,
  BRAND_FONT_WEIGHT_MAX,
  BRAND_FONT_WEIGHT_MIN,
  BRAND_FONT_WEIGHT_STEP,
  BRAND_IMAGE_MAX_BYTES,
  BRAND_IMAGE_MAX_SIDE,
  BRAND_SVG_MAX_BYTES,
} from "~/contract/brand.ts";
import { fmtBytes, utf8Bytes } from "../format.ts";

/**
 * A bound in bytes as a sentence says it: "512 KiB", "2 MiB", the whole unit
 * where it is one, labelled in the binary units `fmtBytes` and the engine use.
 */
function boundWords(bytes: number): string {
  if (bytes % (1 << 20) === 0) return `${bytes / (1 << 20)} MiB`;
  if (bytes % (1 << 10) === 0) return `${bytes / (1 << 10)} KiB`;
  return fmtBytes(bytes);
}
import type { BrandDocument } from "~/protocol/index.ts";
import type { BrandSaveFile } from "./api.ts";
import type { BrandFaceRole, BrandLogoSlot } from "./brand.ts";
import type { BrandSource } from "./useBrand.ts";

/** An uploaded family's name, as the format admits it (its length held apart). */
const FAMILY_PATTERN = new RegExp(BRAND_FAMILY_NAME_PATTERN);

/** The weights a face file may draw, as CSS names them: the format's hundreds. */
export const FONT_WEIGHTS: readonly number[] = Array.from(
  { length: (BRAND_FONT_WEIGHT_MAX - BRAND_FONT_WEIGHT_MIN) / BRAND_FONT_WEIGHT_STEP + 1 },
  (_, i) => BRAND_FONT_WEIGHT_MIN + i * BRAND_FONT_WEIGHT_STEP,
);

/** What a logo slot says it takes, under its picker. */
export const LOGO_HINT = `SVG up to ${boundWords(BRAND_SVG_MAX_BYTES)}. PNG or WebP up to ${boundWords(BRAND_IMAGE_MAX_BYTES)} and ${BRAND_IMAGE_MAX_SIDE} px.`;

/** What the face files may be, beside their picker. */
export const FONT_HINT = `Up to ${BRAND_FONT_FILES_MAX} WOFF2 files of at most ${boundWords(BRAND_FONT_FILE_MAX_BYTES)} each.`;

/** What a person picked: the next document, and each picked file by the path the document names it at. */
export interface BrandEdit {
  document: BrandDocument;
  files: ReadonlyMap<string, File>;
}

/** The logo types the format admits, by content type, with the extension a picked file is saved under. */
const LOGO_EXTENSIONS: Readonly<Record<string, string>> = {
  "image/svg+xml": "svg",
  "image/png": "png",
  "image/webp": "webp",
};

/** The path segment each logo is saved under. */
const LOGO_NAMES: Readonly<Record<BrandLogoSlot, string>> = {
  primary: "primary",
  on_dark: "on-dark",
  mark: "mark",
};

/** A picked logo's content type: its own, or its extension's where the browser named none. */
function logoType(file: File): string | null {
  if (file.type in LOGO_EXTENSIONS) return file.type;
  const ext = file.name.split(".").pop()?.toLowerCase() ?? "";
  return Object.entries(LOGO_EXTENSIONS).find(([, e]) => e === ext)?.[0] ?? null;
}

/** Why a picked logo cannot be used, and what to bring instead; null for one the format admits. */
export function logoProblem(file: File): string | null {
  const type = logoType(file);
  if (!type) return "Choose an SVG, PNG or WebP file.";
  if (type === "image/svg+xml" && file.size > BRAND_SVG_MAX_BYTES) {
    return `An SVG logo can be at most ${boundWords(BRAND_SVG_MAX_BYTES)}. Simplify the drawing, or bring a PNG.`;
  }
  if (type !== "image/svg+xml" && file.size > BRAND_IMAGE_MAX_BYTES) {
    return `A PNG or WebP logo can be at most ${boundWords(BRAND_IMAGE_MAX_BYTES)}. Export it smaller.`;
  }
  return null;
}

/** The path a picked logo is saved at in the kit's next version. */
export function logoPath(slot: BrandLogoSlot, file: File): string {
  const type = logoType(file) ?? "image/png";
  return `logos/${LOGO_NAMES[slot]}.${LOGO_EXTENSIONS[type]}`;
}

/** Why a family of `count` files is too many; null for one the format admits. */
export function fontCountProblem(count: number): string | null {
  return count > BRAND_FONT_FILES_MAX
    ? `A family can have at most ${BRAND_FONT_FILES_MAX} files, such as regular, italic, and two weights.`
    : null;
}

/** Why picked face files cannot be used; null for files the format admits. */
export function fontProblem(files: readonly File[]): string | null {
  const count = fontCountProblem(files.length);
  if (count) return count;
  for (const file of files) {
    if (!file.name.toLowerCase().endsWith(".woff2")) {
      return `${file.name} is not a WOFF2 file. Convert the face to WOFF2 first.`;
    }
    if (file.size > BRAND_FONT_FILE_MAX_BYTES) {
      return `${file.name} is larger than ${boundWords(BRAND_FONT_FILE_MAX_BYTES)}. Subset the face to the scripts the company writes in.`;
    }
  }
  return null;
}

/** The path one face file is saved at in the kit's next version. */
export function fontPath(role: BrandFaceRole, index: number): string {
  return `fonts/${role}-${index}.woff2`;
}

/** Why a family name cannot be used; null for one the format admits. */
export function familyProblem(name: string): string | null {
  const trimmed = name.trim();
  if (trimmed === "") return "Name the family, as its publisher does.";
  if (utf8Bytes(trimmed) > BRAND_FAMILY_NAME_MAX_BYTES) {
    return `A family's name can be at most ${BRAND_FAMILY_NAME_MAX_BYTES} characters.`;
  }
  if (!FAMILY_PATTERN.test(trimmed)) {
    return "Use letters, digits, hyphens and single spaces, starting with a letter.";
  }
  return null;
}

/** A number per picked file, so a draft's source changes key when a different file is picked. */
const fileIds = new WeakMap<File, number>();
let nextFileId = 1;

function fileId(file: File): number {
  let id = fileIds.get(file);
  if (id === undefined) {
    id = nextFileId++;
    fileIds.set(file, id);
  }
  return id;
}

/** The type a picked file is shown or loaded as. */
function pickedType(path: string, file: File): string {
  if (path.endsWith(".woff2")) return "font/woff2";
  return logoType(file) ?? "application/octet-stream";
}

/**
 * The edit as a source of files: every picked file from memory, every other
 * file the document names from `base` (the adopted brand, or nothing for a
 * brand being set up).
 */
export function draftSource(base: BrandSource | null, edit: BrandEdit): BrandSource {
  const files = new Map(base?.files ?? []);
  const picked = [...edit.files.entries()];
  for (const [path, file] of picked) files.set(path, { path, type: pickedType(path, file) });
  const key = [
    base?.key ?? "none",
    ...picked.map(([path, file]) => `${path}=${fileId(file)}`),
    JSON.stringify(edit.document.logos),
    JSON.stringify(edit.document.type),
  ].join("|");
  return {
    key,
    document: edit.document,
    files,
    read: (file, signal) => {
      const own = edit.files.get(file.path);
      if (own) return Promise.resolve(own);
      if (!base) return Promise.reject(new Error(`${file.path} is not in this brand`));
      return base.read(file, signal);
    },
  };
}

/** Every path the document names: its logos, then each face's files. */
function namedPaths(doc: BrandDocument): string[] {
  return [
    doc.logos.primary,
    doc.logos.on_dark,
    doc.logos.mark,
    ...(doc.type.heading.files ?? []).map((f) => f.path),
    ...(doc.type.body.files ?? []).map((f) => f.path),
  ].filter((p): p is string => p !== undefined && p !== "");
}

/**
 * Where a draft save of a kit already published may carry files from: its
 * latest saved version, and each file a save already brought (by the path it
 * was saved at), so a file picked once is uploaded once.
 */
export interface DraftCarry {
  version: number;
  saved: ReadonlyMap<string, File>;
}

/**
 * The files a save of `edit` brings: every path its document names, each from
 * the edit or, untouched, from `base`. With `carry` (a setup's draft saves), a
 * file the kit's latest version already holds is carried from that version
 * (`{path, from}`), no byte moved and none read; only a file picked since the
 * last save travels. Rejects naming the first path nothing holds, which a save
 * says rather than publishing a kit that misses a file.
 */
export async function saveFiles(
  base: BrandSource | null,
  edit: BrandEdit,
  signal?: AbortSignal,
  carry?: DraftCarry,
): Promise<BrandSaveFile[]> {
  const out: BrandSaveFile[] = [];
  const seen = new Set<string>();
  for (const path of namedPaths(edit.document)) {
    if (seen.has(path)) continue;
    seen.add(path);
    const own = edit.files.get(path);
    const kept = carry !== undefined && carry.version > 0;
    if (own && !(kept && carry.saved.get(path) === own)) {
      out.push({ path, body: own });
      continue;
    }
    if (kept) {
      out.push({ path, from: carry.version });
      continue;
    }
    const held = base?.files.get(path);
    if (!base || !held) throw new Error(`The brand names ${path}, and no file for it is here.`);
    out.push({ path, body: await base.read(held, signal ?? new AbortController().signal) });
  }
  return out;
}

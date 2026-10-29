/**
 * What a person brings to a new HTML page, before it is anything the engine
 * holds: the files (typed, pasted, dropped or picked), which one is the page,
 * its title as the page names it, and the capabilities its script reaches for.
 *
 * PURE, so the create page draws from it and a test reads it without a DOM.
 * Nothing here parses markup: the dashboard reaches no markup sink (its policy
 * enforces Trusted Types with none allowed), so the title and the capabilities
 * are read off the text.
 */

import { version as KIT_VERSION } from "@crewlethq/artifact-kit/package.json";
import { pathSegment } from "~/lib/segment.ts";
import type { ArtifactCapability } from "~/protocol/index.ts";

/** One file of the page: its text where it is text, its bytes where it is a picture or a font. */
export type PageFile =
  { path: string; kind: "text"; text: string } | { path: string; kind: "bytes"; blob: Blob };

/**
 * The file types a page may bring, by extension, as the engine's media table
 * serves them (internal/artifacts/mediatype): text the editor shows and the
 * check reads, and bytes that move as they are.
 */
const TEXT_TYPES = new Set([
  "html",
  "htm",
  "css",
  "js",
  "mjs",
  "json",
  "svg",
  "txt",
  "md",
  "csv",
  "xml",
  "map",
]);
const BYTE_TYPES = new Set([
  "png",
  "jpg",
  "jpeg",
  "gif",
  "webp",
  "avif",
  "ico",
  "woff",
  "woff2",
  "ttf",
  "otf",
]);

/** What the pickers offer: every type above, as an input's `accept` takes them. */
export const PAGE_ACCEPT = [...TEXT_TYPES, ...BYTE_TYPES].map((e) => `.${e}`).join(",");

/** A path's extension, lowercase, without its dot. */
function extension(path: string): string {
  const name = path.slice(path.lastIndexOf("/") + 1);
  const dot = name.lastIndexOf(".");
  return dot > 0 ? name.slice(dot + 1).toLowerCase() : "";
}

/** Whether a page may bring a file at this path, and as what. */
export function fileKind(path: string): "text" | "bytes" | null {
  const ext = extension(path);
  if (TEXT_TYPES.has(ext)) return "text";
  if (BYTE_TYPES.has(ext)) return "bytes";
  return null;
}

/** Whether a path is a page the frame can open. */
export function isPage(path: string): boolean {
  const ext = extension(path);
  return ext === "html" || ext === "htm";
}

/** The kit release this dashboard ships, which the engine serves on the artifact origin. */
export const KIT_RELEASE = KIT_VERSION;

/**
 * The page a new HTML page starts from: the kit's stylesheets and theme
 * bootstrap at the release the engine serves, so it follows the reader's
 * theme and passes the publish checks as it stands.
 */
export function skeleton(): string {
  const kit = `/_crewlet/kit/${pathSegment(KIT_RELEASE)}`;
  return `<!doctype html>
<html lang="en">
  <head>
    <meta charset="utf-8">
    <meta name="viewport" content="width=device-width, initial-scale=1">
    <title>Untitled page</title>
    <link rel="stylesheet" href="${kit}/kit.css">
    <link rel="stylesheet" href="${kit}/content.css">
    <script src="${kit}/theme.js"></script>
  </head>
  <body>
    <main>
      <h1>Untitled page</h1>
      <p>Write the page here, or paste one over this.</p>
    </main>
  </body>
</html>
`;
}

/** The named and numeric character references a title is likely to carry. */
const ENTITIES: Readonly<Record<string, string>> = {
  amp: "&",
  lt: "<",
  gt: ">",
  quot: '"',
  apos: "'",
  nbsp: " ",
};

/**
 * The page's own title, from its `<title>`, as a reader would see it: its
 * references decoded and its whitespace collapsed; "" where it names none.
 */
export function titleOf(html: string): string {
  const found = /<title[^>]*>([\s\S]*?)<\/title\s*>/i.exec(html);
  if (!found) return "";
  return found[1]!
    .replace(/&(#x[0-9a-f]+|#\d+|[a-z]+);/gi, (all, ref: string) => {
      if (ref[0] === "#") {
        const code =
          ref[1] === "x" || ref[1] === "X"
            ? parseInt(ref.slice(2), 16)
            : parseInt(ref.slice(1), 10);
        return Number.isFinite(code) && code > 0 && code <= 0x10ffff
          ? String.fromCodePoint(code)
          : all;
      }
      return ENTITIES[ref.toLowerCase()] ?? all;
    })
    .replace(/\s+/g, " ")
    .trim();
}

/**
 * The capabilities a page's script reaches for, read off its text: a call on
 * `crewlet.data` needs `db`, and so on for each namespace of the runtime
 * (docs/reference/artifact-runtime.md). A guide for the person, never the
 * grant: what is declared is what they tick.
 */
const NAMESPACES: readonly [RegExp, ArtifactCapability][] = [
  [/\bcrewlet\s*\.\s*data\b/, "db"],
  [/\bcrewlet\s*\.\s*identity\b/, "identity"],
  [/\bcrewlet\s*\.\s*presence\b/, "presence"],
  [/\bcrewlet\s*\.\s*state\b/, "state"],
  [/\bcrewlet\s*\.\s*assets\b/, "assets"],
];

export function capabilitiesUsed(files: readonly PageFile[]): Set<ArtifactCapability> {
  const out = new Set<ArtifactCapability>();
  for (const f of files) {
    if (f.kind !== "text") continue;
    for (const [pattern, capability] of NAMESPACES) if (pattern.test(f.text)) out.add(capability);
  }
  return out;
}

/**
 * The page the frame opens: `index.html` at the top where there is one, else
 * the shallowest page, first by name; "" where nothing brought is a page.
 */
export function entryOf(files: readonly PageFile[]): string {
  const pages = files.filter((f) => isPage(f.path)).map((f) => f.path);
  if (pages.includes("index.html")) return "index.html";
  const depth = (p: string) => p.split("/").length;
  return [...pages].sort((a, b) => depth(a) - depth(b) || a.localeCompare(b))[0] ?? "";
}

/**
 * Paths as the page will hold them: a folder dropped or picked whole is the
 * page itself, so the one folder every file sits in is dropped from each path
 * (`site/index.html` is the page's `index.html`).
 */
export function withoutCommonFolder(paths: readonly string[]): string[] {
  if (paths.length === 0) return [];
  const first = paths[0]!.split("/")[0]!;
  const shared = paths.every((p) => p.includes("/") && p.split("/")[0] === first);
  return shared ? paths.map((p) => p.slice(first.length + 1)) : [...paths];
}

/** What reading what the person brought came to: the files kept, and what was left out and why. */
export interface Intake {
  files: PageFile[];
  /** Paths left out because a page cannot hold their type. */
  skipped: string[];
}

/**
 * Read what was dropped or picked into page files: text read as text, a
 * picture or a font kept as its bytes, any other type left out and named.
 * The common folder is dropped from every path.
 */
export async function readIntake(picked: readonly { path: string; file: File }[]): Promise<Intake> {
  const paths = withoutCommonFolder(picked.map((p) => p.path));
  const files: PageFile[] = [];
  const skipped: string[] = [];
  await Promise.all(
    picked.map(async ({ file }, i) => {
      const path = paths[i]!;
      const kind = fileKind(path);
      if (kind === null) skipped.push(path);
      else if (kind === "text") files.push({ path, kind, text: await file.text() });
      else files.push({ path, kind, blob: file });
    }),
  );
  files.sort((a, b) => a.path.localeCompare(b.path));
  skipped.sort();
  return { files, skipped };
}

/** The text files, as the check and the editor read them. */
export function textFiles(files: readonly PageFile[]): { path: string; text: string }[] {
  return files.flatMap((f) => (f.kind === "text" ? [{ path: f.path, text: f.text }] : []));
}

/** How many bytes the text files hold, as UTF-8. */
export function textBytes(files: readonly PageFile[]): number {
  const encoder = new TextEncoder();
  return textFiles(files).reduce((n, f) => n + encoder.encode(f.text).length, 0);
}

/**
 * One string that changes whenever what would be published changes: the
 * paths, each text and each picture's size and time. A run is offered only
 * when it differs from the last run's.
 */
export function fingerprint(files: readonly PageFile[], caps: readonly string[]): string {
  return JSON.stringify([
    files.map((f) =>
      f.kind === "text"
        ? [f.path, f.text]
        : [f.path, f.blob.size, (f.blob as File).lastModified ?? 0],
    ),
    [...caps].sort(),
  ]);
}

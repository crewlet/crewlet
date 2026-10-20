/**
 * The native kinds, read from a version's FILES as the engine reads them
 * (`internal/artifacts/docindex`, `slideformat`, `canvasformat`, `dashspec`),
 * so a fixture, a starter and a person's save are drawn by one derivation:
 *
 * - a DOC is `index.md`, or `doc.json` naming its tabs (`GET …/doc`);
 * - a DECK is `deck.json` and one `slides/<id>.html` a slide, each read into
 *   the canonical tree the dashboard draws (`GET …/deck`);
 * - a DESIGN is `canvas.json`, its boards each a page of the version, framed
 *   on its own ticket (`GET …/source`, a ticket naming its `document`);
 * - a DASHBOARD is `dashboard.json`, its datasets each read as the viewer
 *   (`GET …/dashboard`, `GET …/sources/{dataset}`, `GET /artifacts/sources`).
 *
 * Each version's outline (`GET …/outline`) is derived from the same reads,
 * and a person's save is a patch of exact edits and uploaded files
 * (`POST …/uploads`, `PUT /artifacts/uploads/…`, `POST …/versions`), held to
 * the kind's format before it lands.
 *
 * WHAT IS LEFT OUT: the engine's publish linter and the slide format's closed
 * subset are not ported. A slide is read with the browser's own HTML parser
 * and handed to the dashboard's renderer, which holds its own allowlists, so
 * a slide the engine would refuse is drawn here as far as the renderer goes.
 * A calc dataset runs `filter`, `group`, `compute`, `join`, `sort`, `limit`
 * and `select`; an engine dataset reads `tracker.items`, `tracker.workload`,
 * `spend.seats` and `spend.daily`, and any other source is answered
 * `unanswered`.
 */

import type {
  ArtifactActor,
  ArtifactDeck,
  ArtifactDoc,
  ArtifactKind,
  ArtifactOutline,
  ArtifactUploadIntent,
  DashboardArg,
  DashboardBoardAnswer,
  DashboardBoardDataset,
  DashboardColumn,
  DashboardDataset,
  DashboardDatasetAnswer,
  DashboardEngineSource,
  DashboardFreshness,
  DashboardOutline,
  DashboardRow,
  DashboardSpec,
  DashboardStep,
  DashboardTable,
  DashboardValue,
  DashboardWidget,
  DataColumn,
  DeckSection,
  DeckSlide,
  DesignOutline,
  DesignRect,
  SlideNode,
} from "~/protocol/types.ts";
import type { DemoCompany } from "./company.ts";
import { fixtureId } from "./ids.ts";
import { actorOf } from "./principal.ts";
import type { RestReply, RestRequest } from "./rest.ts";
import type { DemoEngine } from "./engine.ts";
import {
  body,
  changed,
  head,
  record,
  refuse,
  type DemoArtifact,
  type DemoArtifactFile,
  type DemoArtifactVersion,
} from "./artifacts.ts";

// ---- files a version holds -------------------------------------------------

export function fileText(v: DemoArtifactVersion, path: string): string | undefined {
  return v.files.find((f) => f.path === path)?.text;
}

function json<T>(v: DemoArtifactVersion, path: string): T | undefined {
  const text = fileText(v, path);
  if (text === undefined) return undefined;
  try {
    return JSON.parse(text) as T;
  } catch {
    return undefined;
  }
}

/** The media type a path is stored under, by its extension. */
export function contentTypeOf(path: string): string {
  const ext = path.slice(path.lastIndexOf(".") + 1).toLowerCase();
  switch (ext) {
    case "md":
      return "text/markdown; charset=utf-8";
    case "json":
      return "application/json";
    case "html":
      return "text/html; charset=utf-8";
    case "csv":
      return "text/csv; charset=utf-8";
    case "css":
      return "text/css; charset=utf-8";
    case "js":
      return "text/javascript; charset=utf-8";
    default:
      return "text/plain; charset=utf-8";
  }
}

/** One file of a version, typed by its path. */
export function textFile(path: string, text: string): DemoArtifactFile {
  return { path, content_type: contentTypeOf(path), text };
}

/** The file a version's source read answers: a doc's first tab, else the entry. */
export function sourcePath(v: DemoArtifactVersion): string {
  return v.entry === "doc.json" ? (docOf(v)?.tabs[0]?.path ?? "") : v.entry;
}

// ---- the doc (`docindex`) --------------------------------------------------

/** The most of a first tab the gallery's cover draws (`docindex.CoverBytes`, `coverLines`). */
const COVER_BYTES = 1024;
const COVER_LINES = 12;

/** A version's doc, tab by tab, or null for a version that is not one. */
export function docOf(v: DemoArtifactVersion): ArtifactDoc | null {
  if (v.entry === "doc.json") {
    const index = json<{ tabs?: { path: string; title: string }[] }>(v, "doc.json");
    if (!index?.tabs) return null;
    return {
      indexed: true,
      tabs: index.tabs.map((t) => ({
        path: t.path,
        title: t.title,
        text: fileText(v, t.path) ?? "",
      })),
    };
  }
  const text = fileText(v, v.entry);
  if (text === undefined || !v.entry.endsWith(".md")) return null;
  const base = v.entry.slice(v.entry.lastIndexOf("/") + 1).replace(/\.[^.]*$/, "");
  return { indexed: false, tabs: [{ path: v.entry, title: base, text }] };
}

/** The opening of a tab: at most a dozen lines and a kibibyte, cut at a line. */
function opening(text: string): string {
  let out = "";
  const lines = text.split(/(?<=\n)/);
  for (let i = 0; i < lines.length; i++) {
    const line = lines[i]!;
    if (i >= COVER_LINES || out.length + line.length > COVER_BYTES) {
      if (out === "") return line.slice(0, COVER_BYTES).replace(/\n+$/, "");
      break;
    }
    out += line;
  }
  return out.replace(/\n+$/, "");
}

/** The starter a doc begun in the dashboard publishes (`docindex.Starter`). */
export function docStarter(title: string): DemoArtifactFile[] {
  const escaped = title.trim().replace(/[\\`*_{}[\]()<>#+\-.!|~]/g, (c) => `\\${c}`);
  return [
    textFile(
      "index.md",
      `# ${escaped}\n\nStart writing here. Press Edit to change this text, and Save to publish it as a new version.\n`,
    ),
  ];
}

// ---- the deck (`slideformat`) ------------------------------------------------

/** `deck.json` as written. */
export interface DeckIndex {
  v?: number;
  title?: string;
  order?: string[];
  cover?: string;
  sections?: Record<string, { description: string; start: string }>;
}

/** Where slide `id`'s file is (`slideformat.SlidePath`). */
export const slidePath = (id: string) => `slides/${id}.html`;

/** The elements whose text is a run of a slide; whitespace anywhere else draws nothing. */
const TEXT_HOLDERS = new Set([
  "h1",
  "h2",
  "h3",
  "h4",
  "h5",
  "h6",
  "p",
  "li",
  "span",
  "strong",
  "em",
  "b",
  "i",
  "u",
  "s",
  "a",
  "code",
  "small",
  "mark",
  "sub",
  "sup",
  "blockquote",
  "figcaption",
  "td",
  "th",
  "label",
  "text",
  "tspan",
]);

const HEADINGS = ["h1", "h2", "h3", "h4", "h5", "h6"];

function slideNodeOf(el: Element): SlideNode {
  const tag = el.localName;
  const node: SlideNode = { tag };
  const attrs: Record<string, string> = {};
  for (const a of Array.from(el.attributes)) {
    if (a.name !== "style") attrs[a.name] = a.value;
  }
  if (Object.keys(attrs).length > 0) node.attrs = attrs;
  const style = (el.getAttribute("style") ?? "")
    .split(";")
    .map((decl) => {
      const at = decl.indexOf(":");
      return at < 0
        ? { p: "", v: "" }
        : { p: decl.slice(0, at).trim().toLowerCase(), v: decl.slice(at + 1).trim() };
    })
    .filter((d) => d.p !== "" && d.v !== "");
  if (style.length > 0) node.style = style;
  const children: SlideNode[] = [];
  for (const child of Array.from(el.childNodes)) {
    if (child.nodeType === 1) children.push(slideNodeOf(child as Element));
    else if (child.nodeType === 3) {
      const text = child.textContent ?? "";
      if (TEXT_HOLDERS.has(tag) || text.trim() !== "") children.push({ text });
    }
  }
  if (children.length > 0) node.children = children;
  return node;
}

function textOf(n: SlideNode): string {
  if (n.tag === undefined) return n.text ?? "";
  if (n.tag === "br") return " ";
  return (n.children ?? []).map(textOf).join("");
}

function firstText(n: SlideNode, tags: string[]): string {
  if (n.tag && tags.includes(n.tag)) {
    return textOf(n).split(/\s+/).filter(Boolean).join(" ").slice(0, 120);
  }
  for (const c of n.children ?? []) {
    const t = firstText(c, tags);
    if (t) return t;
  }
  return "";
}

/** One slide file read as the dashboard draws it: its section, notes taken out. */
export function parseSlide(id: string, html: string): DeckSlide | null {
  const doc = new DOMParser().parseFromString(`<!doctype html><body>${html}</body>`, "text/html");
  const section = doc.body.querySelector("section");
  if (!section) return null;
  const aside = Array.from(section.children).find((c) => c.localName === "aside");
  const notes = (aside?.textContent ?? "").trim();
  aside?.remove();
  const hidden = section.hasAttribute("hidden");
  const transition = section.getAttribute("data-transition") ?? "";
  const root = slideNodeOf(section);
  // THE SECTION'S OWN ATTRIBUTES are the slide's fields, never drawn.
  delete root.attrs;
  return {
    id,
    path: slidePath(id),
    title: firstText(root, HEADINGS) || firstText(root, ["p"]),
    ...(hidden ? { hidden: true } : {}),
    ...(transition ? { transition } : {}),
    ...(notes ? { notes } : {}),
    root,
  };
}

/** A version's deck, every slide read in order, or null for a version that is not one. */
export function deckOf(v: DemoArtifactVersion): ArtifactDeck | null {
  const index = json<DeckIndex>(v, "deck.json");
  if (!index?.order) return null;
  const order = index.order;
  const slides: DeckSlide[] = [];
  for (const id of order) {
    const slide = parseSlide(id, fileText(v, slidePath(id)) ?? "");
    if (!slide) return null;
    slides.push(slide);
  }
  const sections: DeckSection[] = Object.values(index.sections ?? {})
    .map((s) => ({ description: s.description, start: s.start }))
    .sort((a, b) => order.indexOf(a.start) - order.indexOf(b.start));
  return {
    title: index.title ?? "",
    cover: index.cover || order[0] || "",
    sections,
    faces: [],
    slides,
  };
}

/** A deck's files: its index and one file a slide, each `[id, markup]`. */
export function deckFiles(
  index: DeckIndex,
  slides: [id: string, markup: string][],
): DemoArtifactFile[] {
  return [
    textFile("deck.json", `${JSON.stringify(index, null, 2)}\n`),
    ...slides.map(([id, markup]) => textFile(slidePath(id), markup)),
  ];
}

/**
 * The starter deck a person begins in the dashboard (`slideformat.Starter`):
 * a cover, an agenda, two columns and a closing slide, each with its notes.
 */
export function deckStarter(title: string): DemoArtifactFile[] {
  const t = title.replace(/[&<>"']/g, (c) => `&#${c.charCodeAt(0)};`);
  const dark =
    "background:#1d2433;color:#f5f3ee;font-family:Georgia, serif;padding:128px;display:flex;flex-direction:column;justify-content:center;gap:32px";
  const light =
    "background:#f7f6f2;color:#1d2433;font-family:Georgia, serif;padding:128px;display:flex;flex-direction:column;gap:48px";
  const card =
    "flex:1;display:flex;flex-direction:column;gap:12px;background:#ffffff;padding:40px;border:1px solid #e2e4e8;border-radius:16px";
  return deckFiles(
    {
      v: 4,
      title,
      order: ["cover", "agenda", "points", "close"],
      cover: "cover",
      sections: {
        s1: { description: "Where we are", start: "cover" },
        s2: { description: "What we will do", start: "points" },
        s3: { description: "What we need", start: "close" },
      },
    },
    [
      [
        "cover",
        `<section id="cover" style="${dark}">
<h1 style="font-size:112px;font-weight:600;line-height:1.05;letter-spacing:-2px">${t}</h1>
<p style="font-size:40px;color:#c7cdd8">A subtitle: who it is for, and when</p>
<aside>The cover. Say what the deck is for in one sentence before the first slide.</aside>
</section>
`,
      ],
      [
        "agenda",
        `<section id="agenda" style="${light}">
<h2 style="font-size:72px;font-weight:400;line-height:1.1">Agenda</h2>
<ol style="font-size:40px;line-height:1.5">
<li>Where we are</li>
<li>What we will do</li>
<li>What we need</li>
</ol>
<aside>Three parts at most. Each one becomes a section of the outline.</aside>
</section>
`,
      ],
      [
        "points",
        `<section id="points" style="${light}">
<h2 style="font-size:72px;font-weight:400;line-height:1.1">One idea per slide</h2>
<div style="display:flex;gap:32px">
<div style="${card}">
<h3 style="font-size:36px;font-weight:600">The point</h3>
<p style="font-size:28px">Say it in one sentence a person remembers.</p>
</div>
<div style="${card}">
<h3 style="font-size:36px;font-weight:600">The evidence</h3>
<p style="font-size:28px">A number, a quote or a picture that backs it.</p>
</div>
</div>
<aside>Two cards side by side. Click any text to change it.</aside>
</section>
`,
      ],
      [
        "close",
        `<section id="close" style="${dark}">
<h2 style="font-size:88px;font-weight:600;line-height:1.1">What we need</h2>
<p style="font-size:40px;color:#c7cdd8">The one decision or the one ask, said plainly</p>
<aside>End on what the room should do next.</aside>
</section>
`,
      ],
    ],
  );
}

// ---- the design (`canvasformat`) ---------------------------------------------

/** `canvas.json`, version 1. */
export interface CanvasFile {
  v: 1;
  title?: string;
  boards: Record<string, { x: number; y: number; w: number; h: number; title?: string }>;
  order: string[];
  notes?: {
    id: string;
    kind: "title" | "sticky";
    x: number;
    y: number;
    w: number;
    h: number;
    text: string;
    color?: "yellow" | "pink" | "blue" | "green" | "purple";
  }[];
  cover?: string;
}

/** A design's outline: its boards' frames, for a cover (`canvasformat.Outline`). */
export function designOutline(v: DemoArtifactVersion): DesignOutline | null {
  const canvas = json<CanvasFile>(v, "canvas.json");
  if (!canvas || canvas.order.length === 0) return null;
  const rects: DesignRect[] = [
    ...canvas.order.map((p) => canvas.boards[p]).filter((b) => b !== undefined),
    ...(canvas.notes ?? []),
  ].map((r) => ({ x: r.x, y: r.y, w: r.w, h: r.h }));
  const x = Math.min(...rects.map((r) => r.x));
  const y = Math.min(...rects.map((r) => r.y));
  return {
    ...(canvas.title ? { title: canvas.title } : {}),
    count: canvas.order.length,
    cover: canvas.cover || canvas.order[0]!,
    bounds: {
      x,
      y,
      w: Math.max(...rects.map((r) => r.x + r.w)) - x,
      h: Math.max(...rects.map((r) => r.y + r.h)) - y,
    },
    boards: canvas.order.map((path) => {
      const b = canvas.boards[path]!;
      return { path, x: b.x, y: b.y, w: b.w, h: b.h, ...(b.title ? { title: b.title } : {}) };
    }),
    notes: (canvas.notes ?? []).length,
  };
}

// ---- the outline -----------------------------------------------------------

/** A version's outline, derived from its files (`GET …/versions/{n}/outline`). */
export function outlineOf(a: DemoArtifact, v: DemoArtifactVersion | undefined): ArtifactOutline {
  const out: ArtifactOutline = { artifact: a.id, kind: a.kind, version: v?.number ?? 0 };
  if (!v) return out;
  switch (a.kind) {
    case "doc": {
      const doc = docOf(v);
      if (doc) {
        out.doc = {
          tabs: doc.tabs.map((t) => ({ path: t.path, title: t.title, bytes: t.text.length })),
          cover: { tab: doc.tabs[0]?.path ?? "", markdown: opening(doc.tabs[0]?.text ?? "") },
        };
      }
      break;
    }
    case "slides": {
      const deck = deckOf(v);
      const slides = deck?.slides ?? [];
      const cover = slides.find((s) => s.id === deck?.cover) ?? slides[0];
      if (deck && cover) {
        out.slides = {
          title: deck.title,
          count: slides.length,
          cover: deck.cover,
          sections: deck.sections,
          slides: slides.map((s) => ({
            id: s.id,
            path: s.path,
            title: s.title,
            ...(s.hidden ? { hidden: true } : {}),
            ...(s.notes ? { notes: true } : {}),
          })),
          cover_slide: cover,
        };
      }
      break;
    }
    case "design": {
      const design = designOutline(v);
      if (design) out.design = design;
      break;
    }
    case "dashboard": {
      const spec = json<DashboardSpec>(v, "dashboard.json");
      if (spec) out.dashboard = dashboardOutline(spec);
      break;
    }
    case "html":
    case "sheet":
      break;
  }
  return out;
}

// ---- a person's publish ----------------------------------------------------

/** An upload a save is bringing: the paths it declared and the bytes put so far. */
export interface DemoUpload {
  artifact: string;
  paths: string[];
  received: Record<string, string>;
}

/** `POST /artifacts/{id}/uploads`: an intent naming every path the save will put. */
export function beginUpload(e: DemoEngine, a: DemoArtifact, request: RestRequest): RestReply {
  const b = body(request);
  const paths = (Array.isArray(b["entries"]) ? (b["entries"] as Record<string, unknown>[]) : [])
    .map((x) => x["path"])
    .filter((p): p is string => typeof p === "string" && p !== "");
  if (paths.length === 0) return refuse(400, "bad_request", "An upload names at least one file.");
  const c = e.company;
  c.artifacts.seq += 1;
  const id = fixtureId(`upload:${a.id}:${c.artifacts.seq}`);
  c.artifacts.uploads[id] = { artifact: a.id, paths, received: {} };
  const intent: ArtifactUploadIntent = {
    upload: id,
    expires_at: new Date(e.now().getTime() + 60 * 60_000).toISOString(),
    carried: [],
    transfers: paths.map((path) => ({
      path,
      method: "PUT",
      url: `/artifacts/uploads/${id}/files/${path}`,
      direct: false,
    })),
  };
  return { status: 201, body: intent };
}

/** `PUT /artifacts/uploads/{upload}/files/{path}`: one file's bytes, through the engine. */
export function putUpload(e: DemoEngine, parts: string[], request: RestRequest): RestReply {
  const upload = e.company.artifacts.uploads[parts[1] ?? ""];
  const path = parts.slice(3).join("/");
  if (!upload || parts[2] !== "files" || request.method !== "PUT") {
    return refuse(404, "not_found", "There is no such upload.");
  }
  if (!upload.paths.includes(path)) {
    return refuse(409, "not_declared", `The upload does not declare ${path}.`);
  }
  upload.received[path] = request.body ?? "";
  return { status: 204 };
}

/** Why the files a save leaves are not a version of the kind, or null when they are. */
function formatProblem(kind: ArtifactKind, v: DemoArtifactVersion): string | null {
  switch (kind) {
    case "doc":
      return docOf(v) ? null : "The doc is outside the doc format: its entry is not a tab.";
    case "slides":
      return deckOf(v)
        ? null
        : "The deck is outside the slide format: deck.json or a slide in its order does not read.";
    case "design":
      return designOutline(v) ? null : "The design's canvas.json does not read.";
    case "dashboard":
      return json<DashboardSpec>(v, "dashboard.json")
        ? null
        : "The board's dashboard.json does not read.";
    default:
      return null;
  }
}

/**
 * Publish a new version as the reader against the version they opened
 * (`POST …/versions`, `mode: patch`): each edit replaces exactly the text it
 * quotes, each named file is put from the upload, carried or removed, every
 * other file is carried, and the result is held to the kind's format.
 */
export function publish(e: DemoEngine, a: DemoArtifact, request: RestRequest): RestReply {
  const top = head(a);
  if (!top) {
    return refuse(409, "artifact_not_published", "This artifact has no version to build on.");
  }
  const match = /^"(\d+)"$/.exec(request.headers.get("If-Match") ?? "");
  if (!match) {
    return refuse(428, "precondition_required", "A publish names the version it was based on.");
  }
  if (Number(match[1]) !== top.number) {
    return {
      status: 412,
      body: {
        error: "version_moved",
        detail: `Version ${top.number} was published after the one you opened. Review it, then save again.`,
        live: { number: top.number },
      },
    };
  }
  const b = body(request);
  if (b["mode"] !== "patch") {
    return refuse(400, "bad_request", "A publish from the dashboard is a patch.");
  }
  const files = new Map(top.files.map((f) => [f.path, { ...f }]));
  const uploadId = typeof b["upload"] === "string" ? b["upload"] : "";
  const upload = uploadId ? e.company.artifacts.uploads[uploadId] : undefined;
  if (uploadId && upload?.artifact !== a.id) {
    return refuse(404, "not_found", "There is no such upload.");
  }
  let changes = 0;
  for (const f of (Array.isArray(b["files"]) ? b["files"] : []) as Record<string, unknown>[]) {
    const path = typeof f["path"] === "string" ? f["path"] : "";
    if (!path) continue;
    changes++;
    if (f["remove"] === true) files.delete(path);
    else if (f["upload"] === true) {
      const text = upload?.received[path];
      if (text === undefined) return refuse(409, "upload_incomplete", `${path} was never put.`);
      files.set(path, textFile(path, text));
    }
  }
  for (const edit of (Array.isArray(b["edits"]) ? b["edits"] : []) as Record<string, unknown>[]) {
    const path = String(edit["path"] ?? "");
    const oldStr = String(edit["old_str"] ?? "");
    const file = files.get(path);
    if (!file) {
      return refuse(409, "edit_failed", `${path} is not a file of version ${top.number}.`);
    }
    const at = file.text.indexOf(oldStr);
    if (at < 0 || file.text.indexOf(oldStr, at + 1) >= 0) {
      return refuse(
        409,
        "edit_failed",
        `The edit of ${path} does not match its text exactly once.`,
      );
    }
    file.text =
      file.text.slice(0, at) + String(edit["new_str"] ?? "") + file.text.slice(at + oldStr.length);
    changes++;
  }
  if (changes === 0) return refuse(400, "bad_request", "A publish changes at least one file.");
  const actor = actorOf(e.company);
  const by: ArtifactActor = { name: actor.name, kind: actor.kind };
  const now = e.now().toISOString();
  const next: DemoArtifactVersion = {
    ...structuredClone(top),
    number: top.number + 1,
    created_at: now,
    author: by,
    entry: typeof b["entry"] === "string" && b["entry"] ? b["entry"] : top.entry,
    files: [...files.values()],
  };
  delete next.restored_from;
  if (typeof b["label"] === "string" && b["label"]) next.label = b["label"];
  else delete next.label;
  const problem = formatProblem(a.kind, next);
  if (problem) return refuse(422, "artifact_refused", problem);
  if (uploadId) delete e.company.artifacts.uploads[uploadId];
  a.versions.push(next);
  a.updated_at = now;
  a.updated_by = by;
  record(
    e,
    a,
    "publish",
    { files: changes, ...(next.label ? { label: next.label } : {}) },
    next.number,
  );
  changed(e, a, "version");
  return {
    status: 200,
    body: {
      artifact: a.id,
      number: next.number,
      version_id: fixtureId(`${a.id}:v${next.number}`),
      outcome: "applied",
    },
  };
}

// ---- the dashboard (`dashspec`, `datasetsapi`) -------------------------------

/** The engine's own reads a board may name, as the catalogue lists them (`dashspec.Catalogue`). */
const CATALOGUE: DashboardEngineSource[] = [
  {
    name: "tracker.items",
    domain: "tracker",
    summary:
      "Work items from the tracker, one row each. `scope` narrows them to the viewer's own lists.",
    access: "people and agents holding the tracker; anybody else is told they cannot read it",
    params: [
      {
        key: "scope",
        type: "enum",
        summary: "whose work: all, or one of the viewer's own lists",
        enum: ["all", "mine", "queue", "priorities", "triage", "blocked", "overdue"],
        default: "all",
      },
      {
        key: "project",
        type: "text",
        summary: "a project key; absent reads every project",
        max_bytes: 32,
      },
      {
        key: "status_group",
        type: "enum",
        summary: "one status group; absent reads the open work",
        enum: ["not_started", "active", "done", "closed"],
      },
      {
        key: "limit",
        type: "number",
        summary: "the most rows answered",
        min: 1,
        max: 500,
        default: "100",
      },
    ],
    columns: [
      { key: "key", type: "text", label: "Key" },
      { key: "title", type: "text", label: "Title" },
      { key: "status", type: "text", label: "Status", words: "tracker_status" },
      { key: "status_group", type: "text", label: "Status group", words: "tracker_status_group" },
      { key: "priority", type: "text", label: "Priority", words: "tracker_priority" },
      { key: "assignee", type: "text", label: "Assignee" },
      { key: "project", type: "text", label: "Project" },
      { key: "due", type: "date", label: "Due" },
      { key: "updated", type: "date", label: "Updated" },
      { key: "points", type: "number", label: "Points" },
    ],
    key: "key",
    max_rows: 500,
  },
  {
    name: "tracker.workload",
    domain: "tracker",
    summary:
      "Open work per assignee across every project: how much each person or agent is carrying.",
    access: "people and agents holding the tracker; anybody else is told they cannot read it",
    params: [
      {
        key: "unit",
        type: "text",
        summary: "a team's key or name; absent reads everybody",
        max_bytes: 64,
      },
    ],
    columns: [
      { key: "handle", type: "text", label: "Assignee" },
      { key: "open", type: "number", label: "Open" },
      { key: "points", type: "number", label: "Points" },
    ],
    key: "handle",
    max_rows: 500,
  },
  {
    name: "spend.seats",
    domain: "spend",
    summary: "Token spend per seat over a window of company days.",
    access: "everybody the company admits: spend is the overview",
    params: [
      { key: "days", type: "number", summary: "company days ending today", min: 1, max: 90 },
    ],
    columns: [
      { key: "handle", type: "text", label: "Seat" },
      { key: "total_tokens", type: "number", label: "Tokens" },
      { key: "calls", type: "number", label: "Calls" },
    ],
    key: "handle",
    signal: "tokens",
    max_rows: 500,
  },
  {
    name: "spend.daily",
    domain: "spend",
    summary: "Token spend per company day, one row per day and band, banded by seat.",
    access: "everybody the company admits: spend is the overview",
    params: [
      {
        key: "days",
        type: "number",
        summary: "company days ending today",
        min: 1,
        max: 90,
        default: "14",
      },
    ],
    columns: [
      { key: "day", type: "date", label: "Day" },
      { key: "band", type: "text", label: "Band" },
      { key: "total_tokens", type: "number", label: "Tokens" },
    ],
    key: "",
    signal: "tokens",
    max_rows: 450,
  },
];

/** `GET /artifacts/sources`: the engine sources a board may read. */
export function catalogue(): RestReply {
  return { status: 200, body: { sources: CATALOGUE } };
}

/** What a datastore column is, as a dataset's column. */
function storeColumn(col: DataColumn): DashboardColumn {
  const type =
    col.type === "number" || col.type === "rating"
      ? "number"
      : col.type === "date"
        ? "date"
        : col.type === "checkbox"
          ? "boolean"
          : "text";
  return { key: col.key, type, ...(col.label ? { label: col.label } : {}) };
}

/** A datastore value as a row's value: an option's id as its words. */
function storeValue(col: DataColumn, v: unknown): DashboardValue {
  if (v === undefined || v === null) return null;
  const option = col.options?.find((o) => o.id === v);
  if (option) return option.label;
  if (Array.isArray(v)) {
    return v.map((x) => col.options?.find((o) => o.id === x)?.label ?? String(x)).join(", ");
  }
  if (typeof v === "number" || typeof v === "boolean" || typeof v === "string") return v;
  return JSON.stringify(v);
}

type Params = Readonly<Record<string, string>>;

function argOf(
  arg: DashboardArg | undefined,
  params: Params,
): string | number | boolean | undefined {
  if (arg === undefined) return undefined;
  if (typeof arg === "object") return params[arg.param];
  return arg;
}

/** The board's filters with the query's choices over their defaults. */
function paramsOf(spec: DashboardSpec, query: URLSearchParams): Params {
  const out: Record<string, string> = {};
  for (const p of spec.params ?? []) out[p.key] = query.get(p.key) ?? p.default;
  return out;
}

const dayOf = (iso: string | undefined) => (iso ? iso.slice(0, 10) : null);

function engineTable(
  c: DemoCompany,
  name: string,
  args: Record<string, unknown>,
): DashboardTable | null {
  const source = CATALOGUE.find((s) => s.name === name);
  if (!source) return null;
  switch (name) {
    case "tracker.items": {
      const project = typeof args["project"] === "string" ? args["project"] : "";
      const group = typeof args["status_group"] === "string" ? args["status_group"] : "";
      const limit = Number(args["limit"] ?? 100) || 100;
      const rows: DashboardRow[] = c.work.items
        .map((i) => i.task)
        .filter((t) => (!project || t.project === project) && (!group || t.status_group === group))
        .slice(0, limit)
        .map((t) => ({
          key: t.key,
          title: t.title,
          status: t.status,
          status_group: t.status_group ?? null,
          priority: t.priority ?? null,
          assignee: t.assignee ?? null,
          project: t.project,
          due: dayOf(t.due_at),
          updated: dayOf(t.updated_at),
          points: t.points ?? null,
        }));
      return { columns: source.columns, key: "key", rows };
    }
    case "tracker.workload": {
      const by = new Map<string, { open: number; points: number }>();
      for (const { task } of c.work.items) {
        if (task.status_group === "done" || task.status_group === "closed") continue;
        const who = task.assignee || "Unassigned";
        const row = by.get(who) ?? { open: 0, points: 0 };
        row.open += 1;
        row.points += task.points ?? 0;
        by.set(who, row);
      }
      return {
        columns: source.columns,
        key: "handle",
        rows: [...by].map(([handle, r]) => ({ handle, ...r })).sort((x, y) => y.open - x.open),
      };
    }
    case "spend.seats": {
      const by = new Map<string, { total_tokens: number; calls: number }>();
      for (const r of c.spend.daily) {
        const row = by.get(r.handle) ?? { total_tokens: 0, calls: 0 };
        row.total_tokens += r.input + r.output;
        row.calls += r.calls;
        by.set(r.handle, row);
      }
      return {
        columns: source.columns,
        key: "handle",
        rows: [...by].map(([handle, r]) => ({ handle, ...r })),
      };
    }
    case "spend.daily": {
      const days = Number(args["days"] ?? 14) || 14;
      const kept = new Set([...new Set(c.spend.daily.map((r) => r.day))].sort().slice(-days));
      const by = new Map<string, { day: string; band: string; total_tokens: number }>();
      for (const r of c.spend.daily) {
        if (!kept.has(r.day)) continue;
        const k = `${r.day} ${r.handle}`;
        const row = by.get(k) ?? { day: r.day, band: r.handle, total_tokens: 0 };
        row.total_tokens += r.input + r.output;
        by.set(k, row);
      }
      return {
        columns: source.columns,
        rows: [...by.values()].sort((x, y) => x.day.localeCompare(y.day)),
      };
    }
  }
  return null;
}

function csvTable(text: string, declared: DashboardColumn[] | undefined): DashboardTable {
  const [header = "", ...lines] = text.trim().split("\n");
  const keys = header.split(",").map((k) => k.trim());
  const columns = declared ?? keys.map((k) => ({ key: k, type: "text" as const }));
  const typeOf = new Map(columns.map((c) => [c.key, c.type]));
  return {
    columns,
    rows: lines.map((line) =>
      Object.fromEntries(
        line.split(",").map((cell, i) => {
          const key = keys[i] ?? `column${i}`;
          const v = cell.trim();
          return [key, typeOf.get(key) === "number" ? (v === "" ? null : Number(v)) : v];
        }),
      ),
    ),
  };
}

function compare(a: DashboardValue | undefined, cmp: string, b: unknown): boolean {
  if (cmp === "present") return a !== null && a !== undefined && a !== "";
  if (cmp === "absent") return a === null || a === undefined || a === "";
  // AN EMPTY FILTER CHOICE ("All") keeps every row.
  if (b === undefined || b === "") return true;
  const x = typeof a === "number" ? a : String(a ?? "");
  const y = typeof a === "number" ? Number(b) : String(b);
  switch (cmp) {
    case "eq":
      return x === y;
    case "ne":
      return x !== y;
    case "gt":
      return x > y;
    case "gte":
      return x >= y;
    case "lt":
      return x < y;
    case "lte":
      return x <= y;
    default:
      return true;
  }
}

function fold(agg: string, values: DashboardValue[]): DashboardValue {
  const nums = values.filter((v): v is number => typeof v === "number");
  switch (agg) {
    case "count":
      return values.length;
    case "sum":
      return nums.reduce((s, v) => s + v, 0);
    case "avg":
      return nums.length ? nums.reduce((s, v) => s + v, 0) / nums.length : null;
    case "min":
      return nums.length ? Math.min(...nums) : null;
    case "max":
      return nums.length ? Math.max(...nums) : null;
    case "first":
      return values[0] ?? null;
    case "last":
      return values[values.length - 1] ?? null;
    default:
      return null;
  }
}

function runStep(
  table: DashboardTable,
  step: DashboardStep,
  params: Params,
  read: (id: string) => DashboardTable | null,
): DashboardTable {
  switch (step.op) {
    case "filter":
      return {
        ...table,
        rows: table.rows.filter((r) =>
          (step.where ?? []).every((w) => compare(r[w.column], w.cmp, argOf(w.value, params))),
        ),
      };
    case "group": {
      const by = step.by ?? [];
      const groups = new Map<string, DashboardRow[]>();
      if (by.length === 0) groups.set("[]", []);
      for (const r of table.rows) {
        const k = JSON.stringify(by.map((col) => r[col] ?? null));
        groups.set(k, [...(groups.get(k) ?? []), r]);
      }
      const aggregates = step.aggregates ?? [];
      return {
        columns: [
          ...table.columns.filter((c) => by.includes(c.key)),
          ...aggregates.map((g) => ({ key: g.column, type: "number" as const })),
        ],
        rows: [...groups.values()].map((rows) => ({
          ...Object.fromEntries(by.map((col) => [col, rows[0]?.[col] ?? null])),
          ...Object.fromEntries(
            aggregates.map((g) => [
              g.column,
              fold(
                g.agg,
                rows.map((r) => r[g.of ?? g.column] ?? null),
              ),
            ]),
          ),
        })),
      };
    }
    case "compute": {
      const column = step.column ?? "value";
      const val = (x: string | number | undefined, r: DashboardRow): number | null => {
        if (typeof x === "number") return x;
        const v = r[x ?? ""];
        return typeof v === "number" ? v : null;
      };
      return {
        columns: [...table.columns, { key: column, type: "number" }],
        rows: table.rows.map((r) => {
          const a = val(step.left, r);
          const b = val(step.right, r);
          let out: number | null = null;
          if (a !== null && b !== null) {
            if (step.fn === "div") out = b === 0 ? null : a / b;
            else if (step.fn === "mul") out = a * b;
            else if (step.fn === "sub") out = a - b;
            else out = a + b;
          }
          return { ...r, [column]: out };
        }),
      };
    }
    case "join": {
      const other = read(step.with ?? "");
      if (!other) return table;
      const on = step.on ?? "";
      const cols = step.columns ?? [];
      return {
        columns: [...table.columns, ...other.columns.filter((c) => cols.includes(c.key))],
        rows: table.rows.map((r) => {
          const hit = other.rows.find((o) => o[on] === r[on]);
          return { ...r, ...Object.fromEntries(cols.map((c) => [c, hit?.[c] ?? null])) };
        }),
      };
    }
    case "sort":
      return {
        ...table,
        rows: [...table.rows].sort((x, y) => {
          for (const o of step.order ?? []) {
            const a = x[o.column] ?? "";
            const b = y[o.column] ?? "";
            if (a === b) continue;
            return (a < b ? -1 : 1) * (o.desc ? -1 : 1);
          }
          return 0;
        }),
      };
    case "limit":
      return { ...table, rows: table.rows.slice(0, step.count ?? table.rows.length) };
    case "select": {
      const keep = step.columns ?? [];
      return {
        columns: table.columns.filter((c) => keep.includes(c.key)),
        rows: table.rows.map((r) => Object.fromEntries(keep.map((k) => [k, r[k] ?? null]))),
      };
    }
  }
  return table;
}

/** One dataset of a board read as the viewer, or null where the demo cannot read it. */
function tableOf(
  c: DemoCompany,
  a: DemoArtifact,
  v: DemoArtifactVersion,
  spec: DashboardSpec,
  id: string,
  params: Params,
  depth = 0,
): DashboardTable | null {
  const d = spec.datasets.find((x) => x.id === id);
  if (!d || depth > 8) return null;
  switch (d.source) {
    case "store": {
      const coll = a.data?.collections.find((x) => x.name === d.store?.collection);
      if (!coll || !a.data) return null;
      const cols = coll.columns ?? [];
      return {
        columns: d.columns ?? cols.map(storeColumn),
        rows: a.data.docs
          .filter((doc) => doc.collection === coll.name && !doc.deleted)
          .map((doc) =>
            Object.fromEntries(
              cols.map((col) => [col.key, storeValue(col, doc.fields?.[col.key])]),
            ),
          ),
      };
    }
    case "engine": {
      const args = Object.fromEntries(
        Object.entries(d.engine?.params ?? {}).map(([k, arg]) => [k, argOf(arg, params)]),
      );
      return engineTable(c, d.engine?.name ?? "", args);
    }
    case "file": {
      const text = fileText(v, d.file?.path ?? "");
      return text === undefined ? null : csvTable(text, d.columns);
    }
    case "calc": {
      const read = (other: string) => tableOf(c, a, v, spec, other, params, depth + 1);
      const from = read(d.calc?.from ?? "");
      if (!from) return null;
      return (d.calc?.steps ?? []).reduce((t, step) => runStep(t, step, params, read), from);
    }
  }
  return null;
}

/** How a dataset is kept current (`dashspec.Freshness`). */
function freshnessOf(d: DashboardDataset): DashboardFreshness {
  switch (d.source) {
    case "store":
      return { mode: "live", ...(d.store ? { collection: d.store.collection } : {}) };
    case "engine": {
      if (d.refresh) return { mode: "interval", every_seconds: d.refresh.every_seconds };
      const source = CATALOGUE.find((s) => s.name === d.engine?.name);
      return source?.signal ? { mode: "signal", signal: source.signal } : { mode: "manual" };
    }
    case "file":
      return { mode: "fixed" };
    case "calc":
      return { mode: "derived", inputs: [d.calc?.from ?? ""] };
  }
  return { mode: "manual" };
}

/** A board's outline (`dashspec.Outline`): its widgets and its first stats, never a value. */
export function dashboardOutline(spec: DashboardSpec): DashboardOutline {
  const widgets: DashboardOutline["widgets"] = [];
  const walk = (list: DashboardWidget[], group?: string) => {
    for (const w of list) {
      widgets.push({
        id: w.id,
        type: w.type,
        ...(w.title ? { title: w.title } : {}),
        w: w.w,
        ...(w.dataset ? { dataset: w.dataset } : {}),
        ...(group ? { group } : {}),
      });
      if (w.children) walk(w.children, w.id);
    }
  };
  walk(spec.widgets);
  const titleOf = (id: string) => spec.datasets.find((d) => d.id === id)?.title ?? id;
  return {
    ...(spec.title ? { title: spec.title } : {}),
    widgets,
    stats: widgets
      .filter((w) => w.type === "stat" && w.dataset)
      .slice(0, 4)
      .map((w) => ({
        widget: w.id,
        label: w.title ?? "",
        dataset: w.dataset!,
        dataset_title: titleOf(w.dataset!),
      })),
    datasets: spec.datasets.map((d) => ({ id: d.id, title: d.title, source: d.source })),
  };
}

/** `GET …/versions/{n}/dashboard`: the spec and every dataset's shape, no row. */
export function boardOf(e: DemoEngine, a: DemoArtifact, v: DemoArtifactVersion): RestReply {
  const spec = json<DashboardSpec>(v, "dashboard.json");
  if (!spec) return refuse(404, "not_found", `${a.title} is not a dashboard.`);
  const params = paramsOf(spec, new URLSearchParams());
  const datasets: DashboardBoardDataset[] = spec.datasets.map((d) => {
    const engine =
      d.source === "engine" ? CATALOGUE.find((s) => s.name === d.engine?.name) : undefined;
    const columns =
      d.columns ?? engine?.columns ?? tableOf(e.company, a, v, spec, d.id, params)?.columns ?? [];
    const key = d.key ?? engine?.key;
    return {
      id: d.id,
      title: d.title,
      source: d.source,
      shape: { columns, ...(key ? { key } : {}) },
      freshness: freshnessOf(d),
      ...(engine ? { engine } : {}),
    };
  });
  const answer: DashboardBoardAnswer = {
    artifact: a.id,
    version: v.number,
    board: spec,
    datasets,
    outline: dashboardOutline(spec),
  };
  return { status: 200, body: answer };
}

/** `GET …/versions/{n}/sources/{dataset}`: one dataset as this reader may read it. */
export function datasetOf(
  e: DemoEngine,
  a: DemoArtifact,
  v: DemoArtifactVersion,
  id: string,
  query: URLSearchParams,
): RestReply {
  const spec = json<DashboardSpec>(v, "dashboard.json");
  if (!spec?.datasets.some((x) => x.id === id)) {
    return refuse(404, "not_found", `${a.title} has no dataset ${id}.`);
  }
  const table = tableOf(e.company, a, v, spec, id, paramsOf(spec, query));
  const answer: DashboardDatasetAnswer = table
    ? {
        artifact: a.id,
        version: v.number,
        dataset: id,
        status: "ready",
        as_of: e.now().toISOString(),
        table,
      }
    : {
        artifact: a.id,
        version: v.number,
        dataset: id,
        status: "unanswered",
        detail: "This dataset could not be read in the prototype.",
      };
  return { status: 200, body: answer };
}

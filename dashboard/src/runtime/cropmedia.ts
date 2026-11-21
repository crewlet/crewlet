/**
 * What a copy cannot load, drawn in: an SVG drawn as an image loads nothing
 * outside itself, so every picture, file and font the copy shows becomes data.
 */

import type { CropRegion } from "./crop.ts";
import type { Copied, HrefJob, PixelJob } from "./cropcopy.ts";

/** The most a picture waits on the page's files before what is still loading is drawn as a placeholder. */
export const CROP_MEDIA_BUDGET_MS = 2000;

/** One transparent pixel: an image with nothing to show draws nothing, never its alt text. */
export const TRANSPARENT =
  "data:image/gif;base64,R0lGODlhAQABAIAAAAAAAP///yH5BAEAAAAALAAAAAABAAEAAAIBRAA7";

/** The longest side an image is drawn in at, which a picture never needs more of. */
const MAX_DRAWN = 1600;

const XLINK = "http://www.w3.org/1999/xlink";
const SVG = "http://www.w3.org/2000/svg";

/** One axis of `object-position` as computed (`50%`, `12px`, `calc(100% - 8px)`): the offset into `free` pixels. */
export function offsetOf(value: string, free: number): number {
  let total = 0;
  for (const [, sign, n, unit] of value.matchAll(/([+-]?)\s*(\d*\.?\d+(?:e[+-]?\d+)?)(%|px)/gi)) {
    const v = parseFloat(n!) * (sign === "-" ? -1 : 1);
    total += unit === "%" ? (free * v) / 100 : v;
  }
  return total;
}

/** Where an image of `natural` size is painted in a content box, by `object-fit` and `object-position`. */
export function paintedRect(
  fit: string,
  position: string,
  box: { w: number; h: number },
  natural: { w: number; h: number },
): CropRegion {
  const nw = natural.w || box.w;
  const nh = natural.h || box.h;
  let w = box.w;
  let h = box.h;
  const contain = Math.min(box.w / nw, box.h / nh);
  if (fit === "contain" || (fit === "scale-down" && contain < 1)) {
    w = nw * contain;
    h = nh * contain;
  } else if (fit === "cover") {
    const s = Math.max(box.w / nw, box.h / nh);
    w = nw * s;
    h = nh * s;
  } else if (fit === "none" || fit === "scale-down") {
    w = nw;
    h = nh;
  }
  const [px = "50%", py = "50%"] = splitTop(position, " ");
  return { x: offsetOf(px, box.w - w), y: offsetOf(py, box.h - h), w, h };
}

/** `text` split on `sep` outside parentheses and quotes. */
function splitTop(text: string, sep: string): string[] {
  const out: string[] = [];
  let depth = 0;
  let quote = "";
  let at = 0;
  for (let i = 0; i < text.length; i++) {
    const c = text[i]!;
    if (quote) {
      if (c === "\\") i++;
      else if (c === quote) quote = "";
    } else if (c === '"' || c === "'") quote = c;
    else if (c === "(") depth++;
    else if (c === ")") depth--;
    else if (c === sep && depth === 0) {
      if (i > at) out.push(text.slice(at, i).trim());
      at = i + 1;
    }
  }
  if (text.length > at) out.push(text.slice(at).trim());
  return out.filter(Boolean);
}

/** The overlap of two rectangles, or null. */
export function overlap(a: CropRegion, b: CropRegion): CropRegion | null {
  const x = Math.max(a.x, b.x);
  const y = Math.max(a.y, b.y);
  const r = Math.min(a.x + a.w, b.x + b.w);
  const bottom = Math.min(a.y + a.h, b.y + b.h);
  return r > x && bottom > y ? { x, y, w: r - x, h: bottom - y } : null;
}

/** A family name as `font-family` and a font face both write it. */
export function familyKey(name: string): string {
  return name
    .trim()
    .replace(/^["']|["']$/g, "")
    .toLowerCase();
}

/** A decoded picture and its own size. */
interface Source {
  image: CanvasImageSource;
  w: number;
  h: number;
}

/** Data URLs of files already read, so a second picture of a page reads none again. */
const files = new Map<string, Promise<string | null>>();
const FILES_KEPT = 64;

/** Reads the page's files for one picture, each once, until its budget runs out. */
export class Reader {
  private readonly abort = new AbortController();
  private readonly ends: number;
  private readonly timer: ReturnType<typeof setTimeout>;
  private readonly images = new Map<string, Promise<Source | null>>();
  private readonly bitmaps: ImageBitmap[] = [];
  private readonly documents = new Map<string, Promise<Document | null>>();

  constructor(
    private readonly win: Window & typeof globalThis,
    budget: number,
  ) {
    this.ends = performance.now() + budget;
    this.timer = setTimeout(() => this.abort.abort(), budget);
  }

  close(): void {
    clearTimeout(this.timer);
    for (const b of this.bitmaps) b.close();
  }

  /** `work`, or null once the budget is spent. */
  within<T>(work: Promise<T>): Promise<T | null> {
    const left = this.ends - performance.now();
    if (left <= 0) return Promise.resolve(null);
    return new Promise((resolve) => {
      const stop = () => resolve(null);
      this.abort.signal.addEventListener("abort", stop, { once: true });
      work
        .then(
          (v) => resolve(v),
          () => resolve(null),
        )
        .finally(() => this.abort.signal.removeEventListener("abort", stop));
    });
  }

  private async blob(url: string): Promise<Blob | null> {
    const res = await this.win.fetch(url, { signal: this.abort.signal });
    return res.ok ? res.blob() : null;
  }

  /** A file as a data URL, kept for the next picture. */
  data(url: string): Promise<string | null> {
    let found = files.get(url);
    if (!found) {
      found = this.within(this.blob(url).then((b) => (b ? dataUrlOf(b, this.win) : null))).then(
        (v) => {
          if (v === null) files.delete(url);
          return v;
        },
      );
      if (files.size >= FILES_KEPT) files.delete(files.keys().next().value!);
      files.set(url, found);
    }
    return this.within(found);
  }

  /** A file decoded as a picture, once however many elements show it. */
  image(url: string): Promise<Source | null> {
    let found = this.images.get(url);
    if (!found) {
      found = this.within(this.blob(url).then((b) => (b ? this.decode(b) : null)));
      this.images.set(url, found);
    }
    return found;
  }

  /** A bitmap, which draws at once; an SVG, which no bitmap decodes, through an image. */
  private async decode(blob: Blob): Promise<Source | null> {
    if (blob.type !== "image/svg+xml") {
      try {
        const bitmap = await this.win.createImageBitmap(blob);
        this.bitmaps.push(bitmap);
        return { image: bitmap, w: bitmap.width, h: bitmap.height };
      } catch {
        // Not a raster the bitmap decoder takes: an image element may.
      }
    }
    const link = this.win.URL.createObjectURL(blob);
    try {
      const img = new this.win.Image();
      img.src = link;
      await img.decode();
      return { image: img, w: img.naturalWidth, h: img.naturalHeight };
    } finally {
      this.win.URL.revokeObjectURL(link);
    }
  }

  /** An SVG file as a document, read once; no markup is parsed from a string. */
  svg(url: string): Promise<Document | null> {
    let found = this.documents.get(url);
    if (!found) {
      found = this.within(
        new Promise<Document | null>((resolve) => {
          const xhr = new this.win.XMLHttpRequest();
          xhr.responseType = "document";
          xhr.overrideMimeType("image/svg+xml");
          xhr.onload = () => resolve(xhr.status === 200 ? xhr.responseXML : null);
          xhr.onerror = xhr.onabort = () => resolve(null);
          this.abort.signal.addEventListener("abort", () => xhr.abort(), { once: true });
          xhr.open("GET", url);
          xhr.send();
        }),
      );
      this.documents.set(url, found);
    }
    return found;
  }

  /** A file's text, kept for the next picture. */
  async text(url: string): Promise<string | null> {
    const data = await this.data(url);
    if (!data) return null;
    const comma = data.indexOf(",");
    const head = data.slice(0, comma);
    const body = data.slice(comma + 1);
    if (!head.endsWith(";base64")) return decodeURIComponent(body);
    const bytes = Uint8Array.from(atob(body), (c) => c.charCodeAt(0));
    return new TextDecoder().decode(bytes);
  }
}

function dataUrlOf(blob: Blob, win: Window & typeof globalThis): Promise<string> {
  return new Promise((resolve, reject) => {
    const reader = new win.FileReader();
    reader.onload = () => resolve(String(reader.result));
    reader.onerror = () => reject(reader.error);
    reader.readAsDataURL(blob);
  });
}

/** Whether the page may read a source's pixels: a file loaded from another origin may not be drawn out. */
function readable(source: CanvasImageSource, doc: Document): boolean {
  try {
    const c = doc.createElement("canvas");
    c.width = 1;
    c.height = 1;
    const g = c.getContext("2d")!;
    g.drawImage(source, 0, 0, 1, 1, 0, 0, 1, 1);
    g.getImageData(0, 0, 1, 1);
    return true;
  } catch {
    return false;
  }
}

/** Whether a URL is the page's own to draw out: its origin's, or data it holds. */
function ownFile(url: string, doc: Document): boolean {
  try {
    const u = new URL(url, doc.baseURI);
    return u.protocol === "data:" || u.origin === (doc.defaultView?.origin ?? doc.location.origin);
  } catch {
    return false;
  }
}

/** The pixels an element shows: its own where the page may read them, else its file read again. */
async function sourceOf(live: PixelJob["live"], reader: Reader): Promise<Source | null> {
  const doc = live.ownerDocument;
  if (live.localName === "img") {
    const img = live as HTMLImageElement;
    const url = img.currentSrc || img.src;
    if (!img.complete) await reader.within(img.decode());
    // A FILE FROM ANOTHER ORIGIN, which an opaque frame's every file is, is read again rather than probed.
    if (
      img.naturalWidth > 0 &&
      (ownFile(url, doc) || img.crossOrigin !== null) &&
      readable(img, doc)
    ) {
      return { image: img, w: img.naturalWidth, h: img.naturalHeight };
    }
    return url ? reader.image(url) : null;
  }
  if (live.localName === "canvas") {
    const canvas = live as HTMLCanvasElement;
    return canvas.width > 0 && canvas.height > 0 && readable(canvas, doc)
      ? { image: canvas, w: canvas.width, h: canvas.height }
      : null;
  }
  if (live.localName === "video") {
    const video = live as HTMLVideoElement;
    if (video.readyState >= 2 && video.videoWidth > 0 && readable(video, doc)) {
      return { image: video, w: video.videoWidth, h: video.videoHeight };
    }
    return video.poster ? reader.image(video.poster) : null;
  }
  const src = (live as HTMLInputElement).src;
  return src ? reader.image(src) : null;
}

/** What stands for a picture the page may not share: a muted box with a picture's outline. */
function placeholder(g: CanvasRenderingContext2D, w: number, h: number, unit: number): void {
  g.fillStyle = "rgba(128,128,128,0.22)";
  g.fillRect(0, 0, w, h);
  const s = Math.max(12 * unit, Math.min(64 * unit, Math.min(w, h) * 0.3));
  const x = (w - s) / 2;
  const y = (h - s * 0.8) / 2;
  g.strokeStyle = "rgba(128,128,128,0.8)";
  g.lineWidth = Math.max(1.5 * unit, s / 16);
  g.lineJoin = "round";
  g.strokeRect(x, y, s, s * 0.8);
  g.beginPath();
  g.moveTo(x + s * 0.1, y + s * 0.68);
  g.lineTo(x + s * 0.38, y + s * 0.38);
  g.lineTo(x + s * 0.58, y + s * 0.58);
  g.lineTo(x + s * 0.72, y + s * 0.46);
  g.lineTo(x + s * 0.9, y + s * 0.68);
  g.stroke();
  g.beginPath();
  g.arc(x + s * 0.72, y + s * 0.24, s * 0.07, 0, Math.PI * 2);
  g.stroke();
}

/** A canvas as a data URL: JPEG where every pixel is opaque, which is quick, else PNG. */
function encoded(canvas: HTMLCanvasElement, g: CanvasRenderingContext2D): string {
  const { data } = g.getImageData(0, 0, canvas.width, canvas.height);
  for (let i = 3; i < data.length; i += 4) {
    if (data[i] !== 255) return canvas.toDataURL("image/png");
  }
  return canvas.toDataURL("image/jpeg", 0.92);
}

/** Draws the part of a pixel job inside `region` at `scale` canvas pixels per drawing pixel, into its copy. */
async function drawPixels(
  job: PixelJob,
  region: CropRegion,
  scale: number,
  reader: Reader,
): Promise<void> {
  const out = job.out;
  const doc = job.live.ownerDocument;
  const seen = job.turned ? job.content : overlap(job.content, region);
  if (!seen || job.layout.w <= 0 || job.layout.h <= 0 || job.content.w <= 0 || job.content.h <= 0) {
    out.setAttribute("src", TRANSPARENT);
    return;
  }
  const kx = job.layout.w / job.content.w;
  const ky = job.layout.h / job.content.h;
  const lx = (seen.x - job.content.x) * kx;
  const ly = (seen.y - job.content.y) * ky;
  const lw = seen.w * kx;
  const lh = seen.h * ky;
  const shrink = Math.min(1, MAX_DRAWN / Math.max(seen.w * scale, seen.h * scale));
  const width = Math.max(1, Math.round(seen.w * scale * shrink));
  const height = Math.max(1, Math.round(seen.h * scale * shrink));
  const sx = width / lw;
  const sy = height / lh;
  const paint = (source: Source | null): string => {
    const canvas = doc.createElement("canvas");
    canvas.width = width;
    canvas.height = height;
    const g = canvas.getContext("2d")!;
    g.setTransform(sx, 0, 0, sy, -lx * sx, -ly * sy);
    g.beginPath();
    g.rect(0, 0, job.layout.w, job.layout.h);
    g.clip();
    if (source) {
      const p = paintedRect(job.fit, job.position, job.layout, source);
      g.imageSmoothingQuality = "high";
      g.drawImage(source.image, p.x, p.y, p.w, p.h);
    } else {
      placeholder(g, job.layout.w, job.layout.h, 1 / sx);
    }
    return encoded(canvas, g);
  };
  const source = await sourceOf(job.live, reader);
  let url: string;
  try {
    url = paint(source);
  } catch {
    // PIXELS THE PAGE MAY NOT READ even once read again: what stands for them instead.
    url = paint(null);
  }
  out.setAttribute("src", url);
  const [top, right, bottom, left] = job.padding;
  // THE VISIBLE PART FILLS A CONTENT BOX NARROWED TO IT by padding, so the box itself keeps its size.
  out.style.setProperty("box-sizing", "border-box");
  out.style.setProperty("width", `${job.border.w}px`);
  out.style.setProperty("height", `${job.border.h}px`);
  out.style.setProperty("padding-top", `${top + ly}px`);
  out.style.setProperty("padding-left", `${left + lx}px`);
  out.style.setProperty("padding-right", `${right + job.layout.w - lx - lw}px`);
  out.style.setProperty("padding-bottom", `${bottom + job.layout.h - ly - lh}px`);
  out.style.setProperty("object-fit", "fill");
}

/** An SVG `<image>` or `<use>` naming a file: the file inlined, or the part it names copied in. */
async function drawHref(
  job: HrefJob,
  doc: Document,
  scale: number,
  reader: Reader,
  index: number,
): Promise<void> {
  const out = job.out;
  const [file, part] = job.url.split("#", 2) as [string, string | undefined];
  if (job.kind === "use") {
    const found = part ? ((await reader.svg(file))?.getElementById(part) ?? null) : null;
    const svg = out.closest("svg");
    if (!found || !svg) return;
    const id = `crewlet-crop-use-${index}`;
    const defs = svg.ownerDocument.createElementNS(SVG, "defs");
    const copy = svg.ownerDocument.importNode(found, true) as Element;
    copy.setAttribute("id", id);
    defs.append(copy);
    svg.prepend(defs);
    out.removeAttributeNS(XLINK, "href");
    out.setAttribute("href", `#${id}`);
    return;
  }
  const w = Math.max(1, Math.min(MAX_DRAWN, Math.round(job.size.w * scale)));
  const h = Math.max(1, Math.min(MAX_DRAWN, Math.round(job.size.h * scale)));
  let href = /\.svg$/i.test(file.split("?")[0]!) ? await reader.data(file) : null;
  if (!href) {
    const source = await reader.image(file);
    // THE FILE'S OWN SHAPE, at no more pixels than the picture shows it at.
    const k = source ? Math.min(1, Math.max(w / source.w, h / source.h)) : 1;
    const canvas = doc.createElement("canvas");
    canvas.width = source ? Math.max(1, Math.round(source.w * k)) : w;
    canvas.height = source ? Math.max(1, Math.round(source.h * k)) : h;
    const g = canvas.getContext("2d")!;
    if (source) g.drawImage(source.image, 0, 0, canvas.width, canvas.height);
    else placeholder(g, canvas.width, canvas.height, 1);
    try {
      href = encoded(canvas, g);
    } catch {
      const blank = doc.createElement("canvas");
      blank.width = w;
      blank.height = h;
      const b = blank.getContext("2d")!;
      placeholder(b, w, h, 1);
      href = encoded(blank, b);
    }
  }
  out.removeAttributeNS(XLINK, "href");
  out.setAttribute("href", href);
}

/** URLs in a style's text, as computed styles write them. */
const URL_IN_STYLE = /url\("((?:[^"\\]|\\.)*)"\)/g;

/** Every `url()` a style names that is a file: read in, or dropped where it cannot be. */
async function inlineUrls(copied: Copied, reader: Reader): Promise<void> {
  const textOf = (entry: Copied["urls"][number]) =>
    "out" in entry ? (entry.out.getAttribute("style") ?? "") : entry.text;
  const wanted = new Set<string>();
  for (const entry of copied.urls) {
    for (const [, raw] of textOf(entry).matchAll(URL_IN_STYLE)) {
      const url = raw!.replace(/\\(.)/g, "$1");
      if (!url.startsWith("data:") && !url.startsWith("#")) wanted.add(url);
    }
  }
  const got = new Map<string, string | null>();
  await Promise.all([...wanted].map(async (url) => got.set(url, await reader.data(url))));
  // THE STYLE AS IT STANDS NOW: what was set on it since it was copied stays.
  for (const entry of copied.urls) {
    const text = textOf(entry).replace(URL_IN_STYLE, (whole, raw: string) => {
      const url = raw.replace(/\\(.)/g, "$1");
      if (url.startsWith("data:") || url.startsWith("#")) return whole;
      const data = got.get(url);
      return data ? `url("${data}")` : "none";
    });
    if ("out" in entry) entry.out.setAttribute("style", text);
    else copied.rules.push(`${entry.selector}{${text}}`);
  }
}

/** A font face rule, and the URL its files are named against. */
interface FaceRule {
  style: CSSStyleDeclaration;
  base: string;
}

/** Every `@font-face` the document's sheets hold, a sheet the page may not read fetched as text. */
async function faceRules(doc: Document, reader: Reader): Promise<FaceRule[]> {
  const out: FaceRule[] = [];
  const walk = async (rules: CSSRuleList, base: string): Promise<void> => {
    for (const rule of Array.from(rules)) {
      if (rule.type === 5) out.push({ style: (rule as CSSFontFaceRule).style, base });
      else if (rule.type === 3) {
        const imported = (rule as CSSImportRule).styleSheet;
        if (imported) await sheet(imported, imported.href ?? base);
      } else if ("cssRules" in rule) await walk((rule as CSSGroupingRule).cssRules, base);
    }
  };
  const sheet = async (s: CSSStyleSheet, base: string): Promise<void> => {
    let rules: CSSRuleList | null = null;
    try {
      rules = s.cssRules;
    } catch {
      const text = s.href ? await reader.text(s.href) : null;
      if (!text) return;
      try {
        const parsed = new (doc.defaultView as Window & typeof globalThis).CSSStyleSheet();
        parsed.replaceSync(text);
        rules = parsed.cssRules;
      } catch {
        return;
      }
    }
    await walk(rules, base);
  };
  for (const s of Array.from(doc.styleSheets)) await sheet(s, s.href ?? doc.baseURI);
  return out;
}

const normal = (v: string) => v.replace(/\s+/g, "").toLowerCase();

/** `@font-face` rules for the faces the copied text uses, their files as data. */
async function fontFaces(doc: Document, families: Set<string>, reader: Reader): Promise<string> {
  const loaded = [...(doc.fonts as unknown as Iterable<FontFace>)].filter(
    (f) => f.status === "loaded" && families.has(familyKey(f.family)),
  );
  if (loaded.length === 0) return "";
  const rules = await faceRules(doc, reader);
  const faces: string[] = [];
  await Promise.all(
    rules.map(async ({ style, base }) => {
      const family = style.getPropertyValue("font-family");
      const used = loaded.some(
        (f) =>
          familyKey(f.family) === familyKey(family) &&
          normal(f.style) === normal(style.getPropertyValue("font-style") || "normal") &&
          normal(f.weight) === normal(style.getPropertyValue("font-weight") || "normal") &&
          normal(f.unicodeRange) ===
            normal(style.getPropertyValue("unicode-range") || "U+0-10FFFF"),
      );
      if (!used) return;
      const src = /url\(\s*["']?([^"')]+)["']?\s*\)(\s*format\([^)]*\))?/.exec(
        style.getPropertyValue("src"),
      );
      if (!src) return;
      const data = await reader.data(new URL(src[1]!, base).href);
      if (!data) return;
      let face = `font-family:${family};src:url("${data}")${src[2] ?? ""};font-display:block;`;
      for (const p of [
        "font-style",
        "font-weight",
        "font-stretch",
        "unicode-range",
        "font-feature-settings",
      ]) {
        const v = style.getPropertyValue(p);
        if (v) face += `${p}:${v};`;
      }
      faces.push(`@font-face{${face}}`);
    }),
  );
  return faces.join("");
}

/**
 * Starts reading what a picture of `region` of `root` needs: each image under
 * it that the page may not draw out decodes while the copy is made.
 */
export function startReading(
  root: Element,
  region: CropRegion,
  budget = CROP_MEDIA_BUDGET_MS,
): Reader {
  const doc = root.ownerDocument;
  const win = (doc.defaultView ?? window) as Window & typeof globalThis;
  const reader = new Reader(win, budget);
  if (root !== doc.documentElement) return reader;
  for (const img of Array.from(doc.images)) {
    const url = img.currentSrc || img.src;
    if (!url || ownFile(url, doc) || img.crossOrigin !== null) continue;
    const r = img.getBoundingClientRect();
    const box = { x: r.left + win.scrollX, y: r.top + win.scrollY, w: r.width, h: r.height };
    if (overlap(box, region)) void reader.image(url);
  }
  return reader;
}

/**
 * Draws every resource the copy needs into it, at `scale` canvas pixels per
 * drawing pixel, and returns the style sheet it needs: its pseudo-elements and
 * its fonts.
 */
export async function drawIn(
  copied: Copied,
  reader: Reader,
  doc: Document,
  region: CropRegion,
  scale: number,
): Promise<string> {
  try {
    // ONE FILE THAT FAILS costs its own part of the picture, never the rest.
    const [, , fonts] = await Promise.all([
      Promise.all(
        copied.pixels.map((job) =>
          drawPixels(job, region, scale, reader).catch(() =>
            job.out.setAttribute("src", TRANSPARENT),
          ),
        ),
      ),
      Promise.all(
        copied.hrefs.map((job, i) => drawHref(job, doc, scale, reader, i).catch(() => undefined)),
      ),
      fontFaces(doc, copied.families, reader).catch(() => ""),
      inlineUrls(copied, reader),
    ]);
    return fonts + copied.rules.join("");
  } finally {
    reader.close();
  }
}

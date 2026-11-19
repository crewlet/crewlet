/**
 * A comment's picture shows what is under its pin, after the reader scrolled
 * the document or an element inside it. Drawn by the page's own runtime in a
 * frame, so it runs in a headless browser.
 */
import { afterEach, expect, test } from "vitest";
import { page } from "vitest/browser";
import { eventually } from "~/test/settle.ts";
import type { CommentPoint, CommentTarget } from "./bridge.ts";
import { PageComments } from "./comments.ts";
import { CROP_MARGIN, cropScale, drawRegion, type CropRegion } from "./crop.ts";
import { copyRegion, type Copied } from "./cropcopy.ts";

const BANDS = 24;
const colour = (i: number) => `hsl(${(i * 47) % 360} 75% 50%)`;
const bands = (prefix: string) =>
  Array.from(
    { length: BANDS },
    (_, i) =>
      `<div class="band" id="${prefix}${i}" data-crewlet-anchor="${prefix}${i}" style="background:${colour(i)}"></div>`,
  ).join("");

const BASE = `*{box-sizing:border-box} body{margin:0} .band{height:160px}`;

/** Content scrolls in `main`, a flex column with padding: a sideways strip, then a bar that sticks to its top. */
const SHELL = `<!doctype html><html><head><style>${BASE}
html,body{height:100%;overflow:hidden}
.shell{display:grid;grid-template-rows:60px 1fr;height:100%}
header{background:rgb(20,40,160)}
main{display:flex;flex-direction:column;padding:16px;overflow:auto;min-height:0}
.bar{position:sticky;top:0;height:40px;flex:none;background:rgb(250,250,20);z-index:1}
.strip{display:grid;grid-auto-flow:column;grid-auto-columns:200px;overflow-x:auto;flex:none;height:120px}
.band{flex:none}
</style></head><body><div class="shell"><header></header><main>
<div class="strip">${Array.from({ length: 12 }, (_, i) => `<div class="tile" style="background:${colour(i + 5)}"></div>`).join("")}</div>
<div class="bar"></div>
${bands("m")}
</main></div></body></html>`;

/** The document scrolls, under a bar fixed to the viewport's foot. */
const DOCUMENT = `<!doctype html><html><head><style>${BASE}
.foot{position:fixed;left:0;right:0;bottom:0;height:48px;background:rgb(0,200,120)}
</style></head><body>${bands("d")}<div class="foot"></div></body></html>`;

/** `body` scrolls inside a fixed `html`. */
const BODY = `<!doctype html><html><head><style>${BASE}
html{height:100%;overflow:hidden} body{height:100%;overflow:auto}
</style></head><body>${bands("b")}</body></html>`;

let frame: HTMLIFrameElement | null = null;

afterEach(() => {
  frame?.remove();
  frame = null;
});

interface Page {
  win: Window;
  doc: Document;
  comments: PageComments;
  sent: { method: string; params: Record<string, unknown> }[];
}

async function open(html: string): Promise<Page> {
  frame = document.createElement("iframe");
  frame.style.cssText = "position:fixed;left:0;top:0;width:800px;height:600px;border:0";
  const loaded = new Promise((resolve) => frame!.addEventListener("load", resolve, { once: true }));
  frame.srcdoc = html;
  document.body.append(frame);
  await loaded;
  const win = frame.contentWindow!;
  const sent: Page["sent"] = [];
  const comments = new PageComments({
    call: (method, params) => {
      sent.push({ method, params });
      return Promise.resolve(null);
    },
    refuse: (code, message) => Object.assign(new Error(message), { code }),
    window: win,
  });
  comments.receive({ mode: "place" });
  return { win, doc: win.document, comments, sent };
}

/** The reader clicks at a point of the viewport: the pick's point. */
function pick(p: Page, x: number, y: number): CommentPoint {
  const at = p.doc.elementFromPoint(x, y)!;
  const Click = (p.win as Window & { MouseEvent: typeof MouseEvent }).MouseEvent;
  at.dispatchEvent(
    new Click("click", { view: p.win, clientX: x, clientY: y, bubbles: true, cancelable: true }),
  );
  const sent = p.sent.findLast((s) => s.method === "comments.pick")!;
  return sent.params["point"] as CommentPoint;
}

let nextId = 1;

/** The picture of `target`, and where in it the document point `at` is. */
async function picture(p: Page, target: CommentTarget, at: { x: number; y: number }) {
  const id = nextId++;
  const region = p.comments.regionOf(target);
  p.comments.receive({ capture: { id, ...target } });
  await eventually(() =>
    expect(p.sent.some((s) => s.method === "comments.crop" && s.params["id"] === id)).toBe(true),
  );
  const image = p.sent.find((s) => s.method === "comments.crop" && s.params["id"] === id)!.params[
    "image"
  ] as string;
  expect(image).toMatch(/^data:image\/webp;base64,/);
  const img = new Image();
  img.src = image;
  await img.decode();
  const k = img.naturalWidth / region.w;
  return pixel(img, (at.x - region.x) * k, (at.y - region.y) * k);
}

/** The colour of an image around a point, as [r, g, b]. */
function pixel(img: HTMLImageElement, x: number, y: number): number[] {
  const canvas = document.createElement("canvas");
  canvas.width = img.naturalWidth;
  canvas.height = img.naturalHeight;
  const ctx = canvas.getContext("2d")!;
  ctx.drawImage(img, 0, 0);
  const d = ctx.getImageData(Math.round(x) - 2, Math.round(y) - 2, 5, 5).data;
  const sum = [0, 0, 0];
  for (let i = 0; i < d.length; i += 4) for (let c = 0; c < 3; c++) sum[c]! += d[i + c]!;
  return sum.map((v) => v / (d.length / 4));
}

/** What the page draws at a point of its viewport, as [r, g, b]. */
function shown(p: Page, x: number, y: number): number[] {
  const el = p.doc.elementFromPoint(x, y)!;
  return p.win
    .getComputedStyle(el)
    .backgroundColor.match(/[\d.]+/g)!
    .slice(0, 3)
    .map(Number);
}

function expectColour(got: number[], want: number[]) {
  for (let c = 0; c < 3; c++) expect(Math.abs(got[c]! - want[c]!)).toBeLessThan(24);
}

/** The document point under a viewport point. */
const docAt = (p: Page, x: number, y: number) => ({ x: x + p.win.scrollX, y: y + p.win.scrollY });

test("a point in an element scrolled down is drawn as the reader sees it, at the top, middle and foot", async () => {
  const p = await open(SHELL);
  p.doc.querySelector("main")!.scrollTop = 1700;
  for (const y of [140, 330, 560]) {
    const point = pick(p, 500, y);
    expectColour(await picture(p, { point }, docAt(p, 500, y)), shown(p, 500, y));
  }
});

test("a bar stuck to the top of a scrolled element is drawn stuck", async () => {
  const p = await open(SHELL);
  p.doc.querySelector("main")!.scrollTop = 1200;
  const point = pick(p, 400, 80);
  expect(p.doc.elementFromPoint(400, 80)!.className).toBe("bar");
  expectColour(await picture(p, { point }, docAt(p, 400, 80)), shown(p, 400, 80));
});

test("an element scrolled sideways is drawn at its scroll", async () => {
  const p = await open(SHELL);
  const strip = p.doc.querySelector<HTMLElement>(".strip")!;
  strip.scrollLeft = 900;
  const r = strip.getBoundingClientRect();
  const at = { x: r.left + 330, y: r.top + 60 };
  const point = pick(p, at.x, at.y);
  expectColour(await picture(p, { point }, docAt(p, at.x, at.y)), shown(p, at.x, at.y));
});

test("a part inside a scrolled element is drawn where it is shown", async () => {
  const p = await open(SHELL);
  p.doc.querySelector("main")!.scrollTop = 1500;
  const part = p.doc.getElementById("m10")!.getBoundingClientRect();
  const middle = { x: part.left + part.width / 2, y: part.top + part.height / 2 };
  expectColour(
    await picture(p, { name: "m10" }, docAt(p, middle.x, middle.y)),
    shown(p, middle.x, middle.y),
  );
});

test("a scrolled body is drawn as the reader sees it", async () => {
  const p = await open(BODY);
  p.doc.body.scrollTop = 2100;
  for (const y of [100, 450]) {
    const point = pick(p, 300, y);
    expectColour(await picture(p, { point }, docAt(p, 300, y)), shown(p, 300, y));
  }
});

test("in a scrolled document, a point is drawn as shown and a bar fixed to the viewport is drawn where it is fixed", async () => {
  const p = await open(DOCUMENT);
  p.win.scrollTo(0, 1900);
  const point = pick(p, 300, 250);
  expectColour(await picture(p, { point }, docAt(p, 300, 250)), shown(p, 300, 250));
  const foot = pick(p, 300, 580);
  expect(p.doc.elementFromPoint(300, 580)!.className).toBe("foot");
  expectColour(await picture(p, { point: foot }, docAt(p, 300, 580)), shown(p, 300, 580));
});

test("a point of the viewport, as a board names one, is drawn at the page's scroll", async () => {
  const p = await open(DOCUMENT);
  p.win.scrollTo(0, 1300);
  expectColour(
    await picture(p, { view: { x: 200, y: 330 } }, docAt(p, 200, 330)),
    shown(p, 200, 330),
  );
});

test("drawing a sticky bar leaves the page as it was", async () => {
  const p = await open(SHELL);
  const main = p.doc.querySelector("main")!;
  main.scrollTop = 1200;
  const bar = p.doc.querySelector<HTMLElement>(".bar")!;
  bar.setAttribute("style", "outline: 1px solid red");
  const before = bar.getBoundingClientRect().top;
  await picture(p, { point: pick(p, 400, 80) }, docAt(p, 400, 80));
  expect(bar.getAttribute("style")).toBe("outline: 1px solid red");
  expect(bar.getBoundingClientRect().top).toBe(before);
  expect(main.scrollTop).toBe(1200);
});

/** A screenshot-like file: bands of colour, so a picture shifted by a few pixels shows it. */
async function screenshotFile(seed: number): Promise<string> {
  const c = document.createElement("canvas");
  c.width = 1440;
  c.height = 900;
  const g = c.getContext("2d")!;
  for (let i = 0; i < 30; i++) {
    g.fillStyle = `hsl(${(seed * 67 + i * 23) % 360} 70% ${40 + (i % 3) * 15}%)`;
    g.fillRect(0, i * 30, 1440, 30);
  }
  g.fillStyle = "#fff";
  g.fillRect(430, 200, 580, 450);
  g.fillStyle = "rgb(250,238,200)";
  g.fillRect(460, 300, 520, 160);
  const blob = await new Promise<Blob>((resolve) => c.toBlob((b) => resolve(b!), "image/png"));
  return URL.createObjectURL(blob);
}

/** A before-and-after review: sections whose last paragraph's margin collapses through them, each with two screenshots. */
async function review(sections: number, extra = ""): Promise<string> {
  const files = await Promise.all([0, 1, 2, 3].map(screenshotFile));
  const parts = Array.from(
    { length: sections },
    (
      _,
      i,
    ) => `<section id="s${i}"><h2>Comparison ${i}</h2><p>Opened from the menu, then Fast review.</p>
<div class="pair"><figure><img src="${files[(2 * i) % 4]}" alt="Comparison ${i}, before"><div class="mark"></div></figure>
<figure data-crewlet-anchor="after-${i}"><img src="${files[(2 * i + 1) % 4]}" alt="Comparison ${i}, after"><div class="mark"></div></figure></div>
<p>axe: 0 violations.</p></section>`,
  ).join("");
  return `<!doctype html><html><head><style>${BASE}
body{background:#0d0d10;color:#e6e6ea;font:16px/1.5 sans-serif}
.col{max-width:960px;margin:0 auto;padding:24px}
section{border-top:1px solid #333}
h2{margin:28px 0 8px} p{margin:0 0 16px}
.pair{display:grid;grid-template-columns:1fr 1fr;gap:16px;background:#1b1b20;padding:12px}
figure{margin:0;position:relative} img{display:block;width:100%;height:auto}
.mark{position:absolute;left:80%;top:10%;width:12%;height:14%;border:2px dashed #e040a0}
${extra}</style></head><body><div class="col">${parts}</div></body></html>`;
}

/** The frame's own pixels over a rectangle of its viewport, at one per CSS pixel. */
async function screenOf(r: CropRegion): Promise<ImageData> {
  const shot = await page.screenshot({ element: frame!, save: false });
  const img = new Image();
  img.src = `data:image/png;base64,${shot}`;
  await img.decode();
  const k = img.naturalWidth / frame!.clientWidth;
  return pixelsOf(img, { x: r.x * k, y: r.y * k, w: r.w * k, h: r.h * k }, r.w, r.h);
}

function pixelsOf(img: CanvasImageSource, from: CropRegion, w: number, h: number): ImageData {
  const c = document.createElement("canvas");
  c.width = Math.round(w);
  c.height = Math.round(h);
  const g = c.getContext("2d")!;
  g.drawImage(img, from.x, from.y, from.w, from.h, 0, 0, c.width, c.height);
  return g.getImageData(0, 0, c.width, c.height);
}

/** The mean difference of two pictures' channels, 0 to 255. */
function meanDiff(a: ImageData, b: ImageData): number {
  let s = 0;
  for (let i = 0; i < a.data.length; i += 4) {
    for (let c = 0; c < 3; c++) s += Math.abs(a.data[i + c]! - b.data[i + c]!);
  }
  return s / (a.data.length / 4) / 3;
}

/** The picture `target` asks for, drawn at `density`, against what the frame shows over its region. */
async function againstScreen(p: Page, target: CommentTarget, density = 1) {
  const region = p.comments.regionOf(target);
  const de = p.doc.documentElement;
  const background = p.win.getComputedStyle(p.doc.body).backgroundColor;
  const url = await drawRegion(
    de,
    { w: de.scrollWidth, h: de.scrollHeight },
    region,
    background,
    density,
  );
  expect(url).toMatch(/^data:image\/webp;base64,/);
  const img = new Image();
  img.src = url!;
  await img.decode();
  const view = {
    x: region.x - p.win.scrollX,
    y: region.y - p.win.scrollY,
    w: region.w,
    h: region.h,
  };
  const drawn = pixelsOf(
    img,
    { x: 0, y: 0, w: img.naturalWidth, h: img.naturalHeight },
    region.w,
    region.h,
  );
  return { img, region, diff: meanDiff(drawn, await screenOf(view)) };
}

test("a point over a screenshot far down a review page is drawn as the reader sees it", async () => {
  await page.viewport(1200, 900);
  const p = await open(await review(12));
  await Promise.all([...p.doc.images].map((i) => i.decode()));
  for (const k of [1, 6, 11]) {
    const shot = p.doc.querySelectorAll("#s" + k + " img")[1]!;
    shot.scrollIntoView({ block: "center" });
    const r = shot.getBoundingClientRect();
    const point = pick(p, r.left + r.width * 0.45, r.top + r.height * 0.42);
    expect((await againstScreen(p, { point })).diff, `comparison ${k}`).toBeLessThan(8);
  }
});

test("a part pinned by name over a screenshot is drawn whole, with room around it", async () => {
  await page.viewport(1200, 900);
  const p = await open(await review(8));
  await Promise.all([...p.doc.images].map((i) => i.decode()));
  const part = p.doc.querySelector('[data-crewlet-anchor="after-5"]')!;
  part.scrollIntoView({ block: "center" });
  const { region, diff } = await againstScreen(p, { name: "after-5" });
  const r = part.getBoundingClientRect();
  expect(region.w).toBeCloseTo(r.width + 2 * CROP_MARGIN, 0);
  expect(diff).toBeLessThan(8);
});

test("a zoomed page is drawn where the reader sees it", async () => {
  await page.viewport(1200, 900);
  const p = await open(await review(6, "body{zoom:1.25} .col{max-width:720px}"));
  await Promise.all([...p.doc.images].map((i) => i.decode()));
  const shot = p.doc.querySelectorAll("#s4 img")[0]!;
  shot.scrollIntoView({ block: "center" });
  const r = shot.getBoundingClientRect();
  const point = pick(p, r.left + r.width * 0.5, r.top + r.height * 0.5);
  expect((await againstScreen(p, { point })).diff).toBeLessThan(8);
});

test("on a screen of twice the density the picture has twice the pixels, of the same region", async () => {
  await page.viewport(1200, 900);
  const p = await open(await review(4));
  await Promise.all([...p.doc.images].map((i) => i.decode()));
  const shot = p.doc.querySelectorAll("#s2 img")[1]!;
  shot.scrollIntoView({ block: "center" });
  const r = shot.getBoundingClientRect();
  const point = pick(p, r.left + r.width / 2, r.top + r.height / 2);
  const { img, region, diff } = await againstScreen(p, { point }, 2);
  expect(img.naturalWidth).toBe(Math.round(region.w * cropScale(region, 2)));
  expect(img.naturalWidth).toBeGreaterThan(region.w);
  expect(diff).toBeLessThan(8);
});

test("a long page is read only across the picture's rows, and no image outside them is drawn", async () => {
  await page.viewport(1200, 900);
  const counts: Copied["counts"][] = [];
  for (const sections of [6, 30]) {
    const p = await open(await review(sections));
    await Promise.all([...p.doc.images].map((i) => i.decode()));
    const shot = p.doc.querySelectorAll("#s3 img")[1]!;
    shot.scrollIntoView({ block: "center" });
    const r = shot.getBoundingClientRect();
    const region = p.comments.regionOf({
      point: pick(p, r.left + r.width / 2, r.top + r.height / 2),
    });
    const copied = copyRegion(p.doc.documentElement, region);
    counts.push(copied.counts);
    // The pair across the window's rows, each read at most once; every other image left out.
    expect(copied.pixels.map((j) => j.live)).toEqual([...p.doc.querySelectorAll("#s3 img")]);
    frame?.remove();
  }
  // The same rows read in full however long the page is; only the empty boxes grow.
  expect(counts[1]!.full).toBe(counts[0]!.full);
  expect(counts[1]!.shells).toBeGreaterThan(counts[0]!.shells);
});

/** Sections whose first block's top margin and last paragraph's bottom margin pass through them, unless `edge` stops one. */
const collapsing = (edge: string) => `<!doctype html><html><head><style>${BASE}
section{${edge}} section p{margin:0 0 16px;height:20px} section div{margin:24px 0 0}
</style></head><body>${Array.from(
  { length: BANDS },
  (_, i) =>
    `<section><div class="band" id="c${i}" style="background:${colour(i)}"></div><p></p></section>`,
).join("")}</body></html>`;

test("a block a child's margin passes through keeps that space, however far down the point is", async () => {
  for (const edge of [
    "border-top:1px solid rgb(90,90,90)",
    "border-bottom:1px solid rgb(90,90,90)",
  ]) {
    const p = await open(collapsing(edge));
    p.win.scrollTo(0, 3700);
    const tops = [...p.doc.querySelectorAll(".band")]
      .map((b) => b.getBoundingClientRect().top)
      .filter((top) => top > 10 && top < 560);
    expect(tops.length).toBeGreaterThan(1);
    for (const top of tops) {
      // Just inside a band's top edge, where a few pixels of drift land on the gap above it.
      const y = top + 8;
      const point = pick(p, 300, y);
      expectColour(await picture(p, { point }, docAt(p, 300, y)), shown(p, 300, y));
    }
    frame?.remove();
  }
});

test("words with a #, a % and their own line breaks are drawn as the page shows them", async () => {
  await page.viewport(1200, 900);
  const p = await open(`<!doctype html><html><head><style>${BASE}
body{background:#fff;color:#000;font:28px/1.3 monospace} pre{margin:20px;font:inherit}
</style></head><body><pre>#1 of 3: 100% done
\tnext  line, # and %20 kept
last</pre></body></html>`);
  const point = pick(p, 200, 60);
  expect((await againstScreen(p, { point })).diff).toBeLessThan(8);
});

/**
 * A comment's picture shows what is under its pin, after the reader scrolled
 * the document or an element inside it. Drawn by the page's own runtime in a
 * frame, so it runs in a headless browser.
 */
import { afterEach, expect, test } from "vitest";
import { eventually } from "~/test/settle.ts";
import type { CommentPoint, CommentTarget } from "./bridge.ts";
import { PageComments } from "./comments.ts";

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

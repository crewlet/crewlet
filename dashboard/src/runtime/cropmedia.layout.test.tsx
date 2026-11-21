/**
 * A comment's picture shows every image the page draws: an SVG drawn as an
 * image loads nothing outside itself, so each kind is read and drawn in. In
 * a headless browser, since only one draws.
 */
import { afterEach, expect, test } from "vitest";
import { page } from "vitest/browser";
import { drawRegion, type CropRegion } from "./crop.ts";

let frame: HTMLIFrameElement | null = null;

afterEach(() => {
  frame?.remove();
  frame = null;
});

/** A PNG of one colour, as a URL the page reads like a file of its own. */
async function file(rgb: [number, number, number], w = 64, h = 64): Promise<string> {
  const c = document.createElement("canvas");
  c.width = w;
  c.height = h;
  const g = c.getContext("2d")!;
  g.fillStyle = `rgb(${rgb.join(",")})`;
  g.fillRect(0, 0, w, h);
  const blob = await new Promise<Blob>((resolve) => c.toBlob((b) => resolve(b!), "image/png"));
  return URL.createObjectURL(blob);
}

/** An SVG file, as a URL. */
const svgFile = (text: string) => URL.createObjectURL(new Blob([text], { type: "image/svg+xml" }));

async function open(html: string): Promise<{ win: Window; doc: Document }> {
  await page.viewport(1200, 900);
  frame = document.createElement("iframe");
  frame.style.cssText = "position:fixed;left:0;top:0;width:1000px;height:700px;border:0";
  const loaded = new Promise((resolve) => frame!.addEventListener("load", resolve, { once: true }));
  frame.srcdoc = html;
  document.body.append(frame);
  await loaded;
  const win = frame.contentWindow!;
  await Promise.all([...win.document.images].map((i) => i.decode().catch(() => undefined)));
  await win.document.fonts.ready;
  return { win, doc: win.document };
}

/** A picture of `region` of the page's document, decoded to pixels. */
async function picture(win: Window, region: CropRegion): Promise<{ data: ImageData; k: number }> {
  const de = win.document.documentElement;
  const url = await drawRegion(de, { w: de.scrollWidth, h: de.scrollHeight }, region, "#ffffff", 1);
  expect(url).toMatch(/^data:image\/webp;base64,/);
  const img = new Image();
  img.src = url!;
  await img.decode();
  const c = document.createElement("canvas");
  c.width = img.naturalWidth;
  c.height = img.naturalHeight;
  const g = c.getContext("2d")!;
  g.drawImage(img, 0, 0);
  return { data: g.getImageData(0, 0, c.width, c.height), k: img.naturalWidth / region.w };
}

/** The mean colour of a picture over a rectangle of the document. */
function colourAt(
  p: { data: ImageData; k: number },
  region: CropRegion,
  box: CropRegion,
): number[] {
  const x0 = Math.round((box.x - region.x) * p.k);
  const y0 = Math.round((box.y - region.y) * p.k);
  const sum = [0, 0, 0];
  let n = 0;
  for (let y = y0; y < y0 + Math.round(box.h * p.k); y++) {
    for (let x = x0; x < x0 + Math.round(box.w * p.k); x++) {
      const i = (y * p.data.width + x) * 4;
      for (let c = 0; c < 3; c++) sum[c]! += p.data.data[i + c]!;
      n++;
    }
  }
  return sum.map((v) => Math.round(v / n));
}

function expectColour(got: number[], want: number[], what: string) {
  for (let c = 0; c < 3; c++) {
    expect(Math.abs(got[c]! - want[c]!), `${what}: ${got} is not ${want}`).toBeLessThan(20);
  }
}

/** The middle of an element's box, in document pixels, shrunk so its edges do not count. */
function inside(win: Window, el: Element, inset = 6): CropRegion {
  const r = el.getBoundingClientRect();
  return {
    x: r.left + win.scrollX + inset,
    y: r.top + win.scrollY + inset,
    w: r.width - 2 * inset,
    h: r.height - 2 * inset,
  };
}

const KINDS: [string, [number, number, number]][] = [
  ["img", [220, 40, 40]],
  ["picture", [40, 160, 60]],
  ["srcset", [40, 70, 220]],
  ["background", [230, 160, 20]],
  ["before", [150, 40, 190]],
  ["after", [20, 170, 170]],
  ["canvas", [240, 90, 160]],
  ["video", [110, 110, 30]],
  ["svg-image", [60, 60, 60]],
  ["svg-use", [200, 120, 80]],
  ["button", [90, 200, 120]],
];

test("every kind of image the page draws is in its picture, each where the page shows it", async () => {
  const urls = Object.fromEntries(
    await Promise.all(KINDS.map(async ([k, rgb]) => [k, await file(rgb)] as const)),
  );
  const sprite = svgFile(
    `<svg xmlns="http://www.w3.org/2000/svg"><symbol id="dot" viewBox="0 0 10 10"><rect width="10" height="10" fill="rgb(200,120,80)"/></symbol></svg>`,
  );
  const decoy = await file([0, 0, 0]);
  const { win, doc } = await open(`<!doctype html><html><head><style>
body{margin:0;background:#fff}
.grid{display:grid;grid-template-columns:repeat(4,160px);gap:20px;padding:20px}
.cell{width:160px;height:100px;display:block}
img.cell,video.cell,canvas.cell,input.cell{object-fit:cover;border:0;padding:0}
.background{background:url("${urls["background"]}") center/cover}
.before::before{content:url("${urls["before"]}");display:block;width:160px;height:100px;overflow:hidden}
.after::after{content:"";display:block;width:160px;height:100px;background-image:url("${urls["after"]}");background-size:cover}
</style></head><body><div class="grid">
<img class="cell" id="img" src="${urls["img"]}" alt="an image">
<picture><source srcset="${urls["picture"]}" type="image/png"><img class="cell" id="picture" src="${decoy}" alt="a picture"></picture>
<img class="cell" id="srcset" srcset="${urls["srcset"]} 1x, ${decoy} 3x" alt="a set">
<div class="cell background" id="background"></div>
<div class="cell before" id="before"></div>
<div class="cell after" id="after"></div>
<canvas class="cell" id="canvas" width="32" height="20"></canvas>
<video class="cell" id="video" poster="${urls["video"]}"></video>
<svg class="cell" id="svg-image" viewBox="0 0 160 100"><image href="${urls["svg-image"]}" width="160" height="100" preserveAspectRatio="none"/></svg>
<svg class="cell" id="svg-use" viewBox="0 0 160 100"><use href="${sprite}#dot" width="160" height="100" preserveAspectRatio="none"/></svg>
<input class="cell" id="button" type="image" src="${urls["button"]}" alt="a button">
</div></body></html>`);
  const g = doc.querySelector<HTMLCanvasElement>("#canvas")!.getContext("2d")!;
  g.fillStyle = "rgb(240,90,160)";
  g.fillRect(0, 0, 32, 20);
  const region = { x: 0, y: 0, w: 760, h: 400 };
  const p = await picture(win, region);
  const shown = { data: await screenPixels(region), k: 1 };
  for (const [kind] of KINDS) {
    const box = inside(win, doc.getElementById(kind)!);
    // The page shows the file there, not its white ground, and so does its picture.
    expect(
      colourAt(shown, region, box).some((v) => v < 235),
      kind,
    ).toBe(true);
    expectColour(colourAt(p, region, box), colourAt(shown, region, box), kind);
  }
});

test("an image the page may not read is drawn as a placeholder, never as its alt text", async () => {
  // A FOREIGN OBJECT drawn from a blob taints what draws it, as a file from another origin does.
  const foreign = svgFile(
    `<svg xmlns="http://www.w3.org/2000/svg" width="40" height="40"><foreignObject width="40" height="40"><div xmlns="http://www.w3.org/1999/xhtml" style="width:40px;height:40px;background:rgb(0,0,255)"></div></foreignObject></svg>`,
  );
  const { win, doc } = await open(`<!doctype html><html><head><style>
body{margin:0;background:rgb(255,255,255);font:24px/1.2 sans-serif;color:#000}
.cell{display:block;width:300px;height:200px;margin:20px;border:0}
</style></head><body>
<img class="cell" id="img" src="${foreign}" alt="Confirm Algorithm Change, 1440 px, light, after">
<canvas class="cell" id="canvas" width="40" height="40"></canvas>
</body></html>`);
  // A SERVER THAT DOES NOT SHARE ITS FILES with an opaque origin refuses the read again.
  (win as Window & { fetch: typeof fetch }).fetch = () => Promise.reject(new TypeError("refused"));
  const img = doc.querySelector<HTMLImageElement>("#img")!;
  const g = doc.querySelector<HTMLCanvasElement>("#canvas")!.getContext("2d")!;
  g.drawImage(img, 0, 0);
  const region = { x: 0, y: 0, w: 400, h: 500 };
  const p = await picture(win, region);
  // The placeholder's grey over the white page, not the page, not blue, not black words.
  const grey = [
    Math.round(255 * 0.78 + 128 * 0.22),
    Math.round(255 * 0.78 + 128 * 0.22),
    Math.round(255 * 0.78 + 128 * 0.22),
  ];
  for (const id of ["img", "canvas"]) {
    const box = inside(win, doc.getElementById(id)!);
    expectColour(colourAt(p, region, { ...box, h: box.h / 4 }), grey, `${id} placeholder`);
  }
});

test("the page's own fonts draw its words in the picture as on the page", async () => {
  const font = new URL(
    "../../node_modules/@crewlethq/artifact-kit/dist/fonts/geist-latin.woff2",
    import.meta.url,
  ).href;
  const words = `<p id="words">Reset all progress for this collection</p>`;
  const css = `body{margin:0;background:#fff;color:#000} p{margin:20px;font:64px/1 Probe, monospace;white-space:nowrap}`;
  const { win, doc } = await open(
    `<!doctype html><html><head><style>@font-face{font-family:Probe;src:url("${font}") format("woff2")} ${css}</style></head><body>${words}</body></html>`,
  );
  const box = doc.getElementById("words")!.getBoundingClientRect();
  const region = { x: 0, y: 0, w: 800, h: 120 };
  const p = await picture(win, region);
  const shot = await screenPixels(region);
  // The words' ink, column by column: a fallback face puts it elsewhere.
  expect(inkDiff(p.data, shot, Math.round(box.bottom))).toBeLessThan(0.06);
});

/** The frame's own pixels over `region` of its viewport, at one per CSS pixel. */
async function screenPixels(region: CropRegion): Promise<ImageData> {
  const shot = await page.screenshot({ element: frame!, save: false });
  const img = new Image();
  img.src = `data:image/png;base64,${shot}`;
  await img.decode();
  const k = img.naturalWidth / frame!.clientWidth;
  const c = document.createElement("canvas");
  c.width = region.w;
  c.height = region.h;
  c.getContext("2d")!.drawImage(
    img,
    region.x * k,
    region.y * k,
    region.w * k,
    region.h * k,
    0,
    0,
    region.w,
    region.h,
  );
  return c.getContext("2d")!.getImageData(0, 0, region.w, region.h);
}

/** The share of columns whose having ink differs between two pictures of words. */
function inkDiff(a: ImageData, b: ImageData, rows: number): number {
  const inked = (d: ImageData, x: number) => {
    for (let y = 0; y < Math.min(rows, d.height); y++)
      if (d.data[(y * d.width + x) * 4]! < 128) return true;
    return false;
  };
  let differ = 0;
  for (let x = 0; x < a.width; x++) if (inked(a, x) !== inked(b, x)) differ++;
  return differ / a.width;
}

test("a box the copy moves keeps its image: a header stuck to the top of a scrolled page", async () => {
  const stripes = await file([30, 120, 200], 64, 64);
  const { win } = await open(`<!doctype html><html><head><style>
body{margin:0;background:#fff}
header{position:sticky;top:0;height:80px;background:url("${stripes}") 0 0/40px 40px}
.row{height:200px;border-bottom:4px solid rgb(200,40,40)}
</style></head><body><header id="bar"></header>${'<div class="row"></div>'.repeat(30)}</body></html>`);
  win.scrollTo(0, 2400);
  const region = { x: 0, y: 2400, w: 600, h: 300 };
  const p = await picture(win, region);
  const shown = { data: await screenPixels({ x: 0, y: 0, w: 600, h: 300 }), k: 1 };
  const bar = { x: 10, y: 2410, w: 500, h: 60 };
  expectColour(colourAt(p, region, bar), colourAt(shown, region, bar), "stuck header");
});

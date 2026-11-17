/** A picture of part of a drawing via an SVG foreignObject, styles inlined; null where it fails. */

import { isCropImage } from "./bridge.ts";
import { ARTIFACT_COMMENT_CROP_MAX_SIDE } from "../contract/artifactsworkspace.ts";

/** A rectangle in the drawing's own CSS pixels. */
export interface CropRegion {
  x: number;
  y: number;
  w: number;
  h: number;
}

/** How much room a picture of an element keeps around it, so it reads in its place. */
export const CROP_MARGIN = 24;

/** The window a picture of a point shows, at most: about a paragraph and its neighbours. */
export const CROP_POINT_SIZE = { w: 480, h: 300 } as const;

/** A box grown by `margin` on every side and held inside the drawing. */
export function padRegion(
  box: CropRegion,
  size: { w: number; h: number },
  margin = CROP_MARGIN,
): CropRegion {
  const x = Math.max(0, box.x - margin);
  const y = Math.max(0, box.y - margin);
  const right = Math.min(size.w, box.x + box.w + margin);
  const bottom = Math.min(size.h, box.y + box.h + margin);
  return { x, y, w: Math.max(1, right - x), h: Math.max(1, bottom - y) };
}

/** The window around `at`, in the drawing's own pixels, held inside it. */
export function windowAround(at: Point, size: { w: number; h: number }): CropRegion {
  const w = Math.min(CROP_POINT_SIZE.w, size.w);
  const h = Math.min(CROP_POINT_SIZE.h, size.h);
  const x = Math.max(0, Math.min(size.w - w, at.x - w / 2));
  const y = Math.max(0, Math.min(size.h - h, at.y - h / 2));
  return { x, y, w, h };
}

/** The region around a point given as fractions of the drawing, held inside it. */
export function pointRegion(point: Point, size: { w: number; h: number }): CropRegion {
  return windowAround({ x: point.x * size.w, y: point.y * size.h }, size);
}

/** A position or a shift, in CSS pixels. */
export interface Point {
  x: number;
  y: number;
}

/** How a scrolled element's copy is displayed: a box that clips, around a wrapper laying out its children. */
export function scrollerDisplays(display: string): { outer: string; inner: string } {
  const inner = display.endsWith("flex")
    ? "flex"
    : display.endsWith("grid")
      ? "grid"
      : display.endsWith("box")
        ? "-webkit-box"
        : "flow-root";
  if (inner === "flow-root") return { outer: display, inner };
  const inline = display.startsWith("inline-") || display === "-webkit-inline-box";
  return { outer: inline ? "inline-block" : "block", inner };
}

/** Where a box fixed at `at` in the viewport goes in a copy whose box holding it has its corner at `holder`. */
export function fixedAt(at: Point, holder: Point, scale = 1): Point {
  return { x: (at.x - holder.x) / scale, y: (at.y - holder.y) / scale };
}

/** How far a sticky box is shifted from where it flows, less what a sticky box around it shifts it. */
export function stuckBy(shown: Point, flows: Point, around: Point = { x: 0, y: 0 }): Point {
  return { x: shown.x - flows.x - around.x, y: shown.y - flows.y - around.y };
}

/** The scale a region is drawn at: its long side at most [ARTIFACT_COMMENT_CROP_MAX_SIDE]. */
export function cropScale(region: CropRegion): number {
  return Math.min(1, ARTIFACT_COMMENT_CROP_MAX_SIDE / Math.max(region.w, region.h, 1));
}

/** The qualities and shrinks tried in turn until the picture fits the engine's bound. */
const QUALITIES = [0.8, 0.6, 0.4] as const;
const SHRINKS = [1, 0.7, 0.5, 0.35] as const;

/** Draws `region` of `root` (untransformed px, of `size` overall) as a WebP data URL, or null. */
export async function drawRegion(
  root: Element,
  size: { w: number; h: number },
  region: CropRegion,
  background = "#ffffff",
): Promise<string | null> {
  try {
    const image = await imageOf(root, size, region);
    const base = cropScale(region);
    for (const shrink of SHRINKS) {
      const canvas = document.createElement("canvas");
      canvas.width = Math.max(1, Math.round(region.w * base * shrink));
      canvas.height = Math.max(1, Math.round(region.h * base * shrink));
      const ctx = canvas.getContext("2d");
      if (!ctx) return null;
      ctx.fillStyle = background;
      ctx.fillRect(0, 0, canvas.width, canvas.height);
      ctx.drawImage(image, 0, 0, canvas.width, canvas.height);
      for (const quality of QUALITIES) {
        const url = canvas.toDataURL("image/webp", quality);
        if (isCropImage(url)) return url;
      }
    }
    return null;
  } catch {
    return null;
  }
}

/** A scrolled element's properties that lay out its children, which the wrapper in its copy takes over. */
const LAYS_OUT = [
  "flex-direction",
  "flex-wrap",
  "justify-content",
  "justify-items",
  "align-items",
  "align-content",
  "row-gap",
  "column-gap",
  "grid-template-columns",
  "grid-template-rows",
  "grid-template-areas",
  "grid-auto-columns",
  "grid-auto-rows",
  "grid-auto-flow",
  "column-count",
  "column-width",
  "column-fill",
  "column-rule-width",
  "column-rule-style",
  "column-rule-color",
  "-webkit-box-orient",
  "-webkit-line-clamp",
  "padding-top",
  "padding-right",
  "padding-bottom",
  "padding-left",
] as const;

/** Controls that scroll their own text, which no wrapper can hold. */
const UNWRAPPED = new Set(["textarea", "select", "input"]);

/**
 * `root` copied with its computed styles inline, as the reader sees it: a
 * clone keeps no scroll, so each scrolled element, fixed box and sticky box is
 * put back where the reader's scroll has it.
 */
function copyOf(root: Element): Element {
  const doc = root.ownerDocument;
  const win = doc.defaultView ?? window;
  const copy = root.cloneNode(true) as Element;
  const from = [root, ...root.querySelectorAll("*")];
  const to = [copy, ...copy.querySelectorAll("*")];
  const twin = new Map<Element, HTMLElement>();
  const scrolled: { el: HTMLElement; style: CSSStyleDeclaration }[] = [];
  const placed: { el: HTMLElement; viewport: Point | null }[] = [];
  const sticky: HTMLElement[] = [];
  from.forEach((el, i) => {
    const out = to[i] as (Element & ElementCSSInlineStyle) | undefined;
    if (!out?.style) return;
    const style = win.getComputedStyle(el);
    for (const p of style) out.style.setProperty(p, style.getPropertyValue(p));
    twin.set(el, out as HTMLElement);
    if (el === root || !(el instanceof win.HTMLElement)) return;
    if (style.position === "sticky") sticky.push(el);
    else if (style.position === "fixed" || style.position === "absolute") {
      // FIXED TO THE VIEWPORT when no ancestor holds it, which offsetParent says.
      const viewport = style.position === "fixed" && el.offsetParent === null;
      placed.push({
        el,
        viewport: viewport ? { x: parseFloat(style.left), y: parseFloat(style.top) } : null,
      });
    }
    if (
      (el.scrollTop !== 0 || el.scrollLeft !== 0) &&
      el !== doc.scrollingElement &&
      !UNWRAPPED.has(el.localName)
    ) {
      scrolled.push({ el, style });
    }
  });
  const bounds = root.getBoundingClientRect();
  const scale =
    root === doc.documentElement || !(root instanceof win.HTMLElement) || !root.offsetWidth
      ? 1
      : bounds.width / root.offsetWidth;
  const corner =
    root === doc.documentElement
      ? { x: -win.scrollX, y: -win.scrollY }
      : { x: bounds.left, y: bounds.top };
  const wrapped = new Set<Element>(scrolled.map((s) => s.el));
  const scrollerAround = (el: Element): HTMLElement | null => {
    for (let at = el.parentElement; at && at !== root; at = at.parentElement) {
      if (wrapped.has(at)) return at;
    }
    return null;
  };

  for (const [el, shift] of stickShifts(sticky, win)) {
    const out = twin.get(el)!;
    out.style.setProperty("position", "relative");
    out.style.setProperty("top", `${shift.y / scale}px`);
    out.style.setProperty("left", `${shift.x / scale}px`);
    out.style.setProperty("bottom", "auto");
    out.style.setProperty("right", "auto");
  }
  // A POSITIONED BOX THAT ITS SCROLLER DOES NOT CARRY leaves the wrapper that
  // stands for the scroll, for the box that holds it on the page.
  for (const { el, viewport } of placed) {
    const out = twin.get(el)!;
    const around = scrollerAround(el);
    if (viewport) {
      const at = fixedAt(viewport, corner, scale);
      out.style.setProperty("top", `${at.y}px`);
      out.style.setProperty("left", `${at.x}px`);
      out.style.setProperty("bottom", "auto");
      out.style.setProperty("right", "auto");
      if (around) copy.append(out);
      continue;
    }
    const holder = el.offsetParent;
    if (around && (!holder || !around.contains(holder))) {
      (holder ? (twin.get(holder) ?? copy) : copy).append(out);
    }
  }
  for (const { el, style } of scrolled) wrapScroll(el, style, twin.get(el)!);
  return copy;
}

/** Each sticky box's shift from where it flows, measured with every stick let go for a moment. */
function stickShifts(
  sticky: HTMLElement[],
  win: Window & typeof globalThis,
): Map<HTMLElement, Point> {
  const shifts = new Map<HTMLElement, Point>();
  if (sticky.length === 0) return shifts;
  const shown = sticky.map((el) => el.getBoundingClientRect());
  const kept = sticky.map((el) => el.getAttribute("style"));
  // NO FRAME IS DRAWN between letting go and putting back: the reader never sees it.
  for (const el of sticky) {
    el.style.setProperty("position", "relative", "important");
    el.style.setProperty("inset", "auto", "important");
  }
  const flows = sticky.map((el) => el.getBoundingClientRect());
  sticky.forEach((el, i) => {
    const style = kept[i];
    if (style === null || style === undefined) el.removeAttribute("style");
    else el.setAttribute("style", style);
  });
  const total = new Map<HTMLElement, Point>();
  sticky.forEach((el, i) => {
    total.set(
      el,
      stuckBy({ x: shown[i]!.left, y: shown[i]!.top }, { x: flows[i]!.left, y: flows[i]!.top }),
    );
  });
  for (const el of sticky) {
    let around: Point | undefined;
    for (let at = el.parentElement; at && !around; at = at.parentElement) {
      around = at instanceof win.HTMLElement ? total.get(at) : undefined;
    }
    const own = total.get(el)!;
    shifts.set(el, around ? stuckBy(own, { x: 0, y: 0 }, around) : own);
  }
  return shifts;
}

/** A scrolled element's copy: its children in a wrapper that lays them out as it did, moved by its scroll. */
function wrapScroll(el: HTMLElement, style: CSSStyleDeclaration, out: HTMLElement): void {
  const { outer, inner } = scrollerDisplays(style.display);
  const wrap = out.ownerDocument.createElementNS(
    "http://www.w3.org/1999/xhtml",
    "div",
  ) as HTMLElement;
  for (const p of LAYS_OUT) wrap.style.setProperty(p, style.getPropertyValue(p));
  // OVER THE PADDING BOX: its padding is the element's, so its children sit where they did.
  for (const side of ["top", "right", "bottom", "left"]) {
    wrap.style.setProperty(`margin-${side}`, `-${style.getPropertyValue(`padding-${side}`)}`);
  }
  wrap.style.setProperty("display", inner);
  wrap.style.setProperty("box-sizing", "border-box");
  wrap.style.setProperty("width", `${el.clientWidth}px`);
  wrap.style.setProperty("height", `${el.clientHeight}px`);
  wrap.style.setProperty("transform", `translate(${-el.scrollLeft}px, ${-el.scrollTop}px)`);
  wrap.append(...out.childNodes);
  out.append(wrap);
  out.style.setProperty("display", outer);
  out.style.setProperty("overflow", "hidden");
  out.style.setProperty("column-count", "auto");
  out.style.setProperty("column-width", "auto");
  out.style.setProperty("align-content", "normal");
}

/** The copy of `root` drawn as an image of `region`. */
async function imageOf(
  root: Element,
  size: { w: number; h: number },
  region: CropRegion,
): Promise<HTMLImageElement> {
  const copy = copyOf(root);
  // NOTHING THAT RUNS OR LOADS: a script in an image never runs, and a
  // stylesheet's rules are already inline.
  for (const el of copy.querySelectorAll("script, link, iframe, object, embed")) el.remove();
  if ("style" in copy) {
    const own = (copy as Element & ElementCSSInlineStyle).style;
    own.transform = "none";
    own.margin = "0";
  }
  const xhtml = new XMLSerializer().serializeToString(copy);
  const body = copy.namespaceURI === "http://www.w3.org/1999/xhtml" && copy.localName === "html";
  const svg =
    `<svg xmlns="http://www.w3.org/2000/svg" width="${region.w}" height="${region.h}">` +
    `<foreignObject x="${-region.x}" y="${-region.y}" width="${size.w}" height="${size.h}">` +
    (body ? xhtml : `<div xmlns="http://www.w3.org/1999/xhtml">${xhtml}</div>`) +
    `</foreignObject></svg>`;
  const img = new Image();
  img.src = `data:image/svg+xml;base64,${base64Of(svg)}`;
  await img.decode();
  return img;
}

/** Text as base64 of its UTF-8 bytes, in chunks so a long document never overflows the stack. */
function base64Of(text: string): string {
  const bytes = new TextEncoder().encode(text);
  let binary = "";
  for (let i = 0; i < bytes.length; i += 0x8000) {
    binary += String.fromCharCode(...bytes.subarray(i, i + 0x8000));
  }
  return btoa(binary);
}

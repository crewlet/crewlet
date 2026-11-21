/**
 * A picture of part of a drawing via an SVG foreignObject: a copy of what
 * crosses the part's rows, styles inline and files drawn in (cropcopy.ts,
 * cropmedia.ts), drawn at the size the picture needs; null where it fails.
 */

import { isCropImage } from "./bridge.ts";
import { ARTIFACT_COMMENT_CROP_MAX_SIDE } from "../contract/artifactsworkspace.ts";
import { copyRegion } from "./cropcopy.ts";
import { drawIn, startReading } from "./cropmedia.ts";

/** A rectangle in the drawing's own CSS pixels. */
export interface CropRegion {
  x: number;
  y: number;
  w: number;
  h: number;
}

/** A position or a shift, in CSS pixels. */
export interface Point {
  x: number;
  y: number;
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

/**
 * Canvas pixels per drawing pixel: as sharp as the reader's screen
 * (`density`), with the long side at most [ARTIFACT_COMMENT_CROP_MAX_SIDE].
 */
export function cropScale(region: CropRegion, density = 1): number {
  return Math.min(
    Math.max(1, density),
    ARTIFACT_COMMENT_CROP_MAX_SIDE / Math.max(region.w, region.h, 1),
  );
}

/**
 * An SVG as a data URL, escaping only what a URL would change: `%` and `#`,
 * and the tab and line breaks its parser drops. Far quicker than base64.
 */
export function svgDataUrl(svg: string): string {
  return `data:image/svg+xml;charset=utf-8,${svg.replace(/[%#\t\n\r]/g, (c) => `%${c.charCodeAt(0).toString(16).padStart(2, "0")}`)}`;
}

/** The qualities and shrinks tried in turn until the picture fits the engine's bound. */
const QUALITIES = [0.8, 0.6, 0.4] as const;
const SHRINKS = [1, 0.7, 0.5, 0.35] as const;

/**
 * Draws `region` of `root` (untransformed px, of `size` overall) as a WebP
 * data URL, at `density` canvas pixels per CSS pixel where the bound allows,
 * or null.
 */
export async function drawRegion(
  root: Element,
  size: { w: number; h: number },
  region: CropRegion,
  background = "#ffffff",
  density = root.ownerDocument.defaultView?.devicePixelRatio ?? 1,
): Promise<string | null> {
  try {
    const scale = cropScale(region, density);
    const image = await imageOf(root, size, region, scale);
    const w = Math.max(1, Math.round(region.w * scale));
    const h = Math.max(1, Math.round(region.h * scale));
    const drawn = document.createElement("canvas");
    drawn.width = w;
    drawn.height = h;
    const ctx = drawn.getContext("2d");
    if (!ctx) return null;
    ctx.fillStyle = background;
    ctx.fillRect(0, 0, w, h);
    ctx.drawImage(image, 0, 0, w, h);
    for (const shrink of SHRINKS) {
      let canvas = drawn;
      if (shrink !== 1) {
        canvas = document.createElement("canvas");
        canvas.width = Math.max(1, Math.round(w * shrink));
        canvas.height = Math.max(1, Math.round(h * shrink));
        canvas.getContext("2d")?.drawImage(drawn, 0, 0, canvas.width, canvas.height);
      }
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

/** The copy of `root` around `region` drawn as an image of it, at `scale` pixels per drawing pixel. */
async function imageOf(
  root: Element,
  size: { w: number; h: number },
  region: CropRegion,
  scale: number,
): Promise<HTMLImageElement> {
  // THE FILES DECODE OFF THE PAGE'S THREAD while it makes the copy.
  const reader = startReading(root, region);
  let copied;
  try {
    copied = copyRegion(root, region);
  } catch (err) {
    reader.close();
    throw err;
  }
  const sheet = await drawIn(copied, reader, root.ownerDocument, region, scale);
  const copy = copied.copy;
  if (sheet) {
    const style = copy.ownerDocument.createElementNS("http://www.w3.org/1999/xhtml", "style");
    style.textContent = sheet;
    copy.prepend(style);
  }
  if ("style" in copy) {
    const own = (copy as Element & ElementCSSInlineStyle).style;
    own.transform = "none";
    own.margin = "0";
  }
  const xhtml = new XMLSerializer().serializeToString(copy);
  const body = copy.namespaceURI === "http://www.w3.org/1999/xhtml" && copy.localName === "html";
  const w = Math.max(1, Math.round(region.w * scale));
  const h = Math.max(1, Math.round(region.h * scale));
  // DRAWN AT THE PICTURE'S OWN SIZE: the view box scales the copy, so nothing is drawn large and shrunk.
  const svg =
    `<svg xmlns="http://www.w3.org/2000/svg" width="${w}" height="${h}" ` +
    `viewBox="${region.x} ${region.y} ${region.w} ${region.h}" preserveAspectRatio="none">` +
    `<foreignObject x="0" y="0" width="${size.w}" height="${size.h}">` +
    (body ? xhtml : `<div xmlns="http://www.w3.org/1999/xhtml">${xhtml}</div>`) +
    `</foreignObject></svg>`;
  const img = new Image();
  // A DATA URL: Chromium taints a canvas that draws a foreignObject from a blob URL.
  img.src = svgDataUrl(svg);
  await img.decode();
  return img;
}

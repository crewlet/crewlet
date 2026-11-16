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

/** The region around a point given as fractions of the drawing, held inside it. */
export function pointRegion(point: { x: number; y: number }, size: { w: number; h: number }) {
  const w = Math.min(CROP_POINT_SIZE.w, size.w);
  const h = Math.min(CROP_POINT_SIZE.h, size.h);
  const x = Math.max(0, Math.min(size.w - w, point.x * size.w - w / 2));
  const y = Math.max(0, Math.min(size.h - h, point.y * size.h - h / 2));
  return { x, y, w, h };
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

/** The copy of `root` drawn as an image of `region`. */
async function imageOf(
  root: Element,
  size: { w: number; h: number },
  region: CropRegion,
): Promise<HTMLImageElement> {
  const copy = root.cloneNode(true) as Element;
  const from = [root, ...root.querySelectorAll("*")];
  const to = [copy, ...copy.querySelectorAll("*")];
  from.forEach((el, i) => {
    const out = to[i] as (Element & ElementCSSInlineStyle) | undefined;
    if (!out?.style) return;
    const style = getComputedStyle(el);
    for (const p of style) out.style.setProperty(p, style.getPropertyValue(p));
  });
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

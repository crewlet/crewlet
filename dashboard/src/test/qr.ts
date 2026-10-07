/**
 * What a camera reads off a QR code this dashboard drew. Imported by tests only.
 *
 * A REAL DECODE, NOT A SECOND CALL TO THE ENCODER. Comparing the drawing with
 * `encode()` of the expected value proves only that the component and the test
 * called one function alike — an inverted plate, a missing quiet zone or a
 * matrix drawn transposed would pass that as long as both sides did it. So the
 * rendered `<svg>` is rasterised here from its own attributes — the viewBox,
 * the plate's rect, the modules' path and the fill each carries — onto a page
 * as dark as the code's own ink, and handed to `jsqr`, a decoder that shares nothing with
 * the encoder, which is told NOT to try the reversed polarity: what comes back
 * is what a scanner that reads only dark-on-light would read, or null.
 *
 * `jsqr` is a development dependency and never reaches the bundle; its licence
 * (Apache-2.0) is therefore not among the notices the dashboard ships.
 */

import jsQR from "jsqr";

/**
 * The page the code is drawn on: as dark as a dark module.
 *
 * The dark theme's ground is `#101013`, and a decoder handed a noiseless
 * raster binarises adaptively and tells that from `#000000` — which no camera
 * pointed at a screen does through glare and its own noise. Drawn on the
 * theme's own value, a code with no plate, or none of its quiet zone, scanned
 * here and would not on a phone; drawn on the module's own ink, only the plate
 * gives the scanner any light to read.
 */
const GROUND: Rgb = [0, 0, 0];

/** Pixels per module, which is what a camera a hand's length off a screen sees and more. */
const SCALE = 4;

/**
 * Modules of page around the drawing that the scanner sees too. A camera
 * frames the code with whatever surrounds it, so a drawing whose own margin is
 * too thin meets the page here exactly as it would there.
 */
const PAGE = 8;

type Rgb = [number, number, number];

function ink(fill: string | null): Rgb {
  const hex = /^#([0-9a-f]{2})([0-9a-f]{2})([0-9a-f]{2})$/i.exec(fill ?? "");
  if (!hex) throw new Error(`the scanner reads a #rrggbb fill, and this drawing has ${fill}`);
  return [parseInt(hex[1]!, 16), parseInt(hex[2]!, 16), parseInt(hex[3]!, 16)];
}

/**
 * The run rectangles `ui/QrCode.tsx` writes, `M<x> <y>h<n>v1h-<n>z` each. A path
 * holding anything else is refused rather than read in part: a scanner that
 * skipped what it did not understand would read less and still pass.
 */
function runs(d: string): { x: number; y: number; n: number }[] {
  const out: { x: number; y: number; n: number }[] = [];
  const run = /M(-?\d+) (-?\d+)h(\d+)v1h-\3z/y;
  while (run.lastIndex < d.length) {
    const at = run.lastIndex;
    const m = run.exec(d);
    if (!m)
      throw new Error(
        `the module path is not run rectangles from offset ${at}: ${d.slice(at, at + 40)}`,
      );
    out.push({ x: Number(m[1]), y: Number(m[2]), n: Number(m[3]) });
  }
  return out;
}

/** What a dark-on-light scanner reads off `svg`, or null when it reads nothing. */
export function scan(svg: Element): string | null {
  const box = (svg.getAttribute("viewBox") ?? "").trim().split(/\s+/).map(Number);
  if (box.length !== 4 || box.some((v) => !Number.isInteger(v))) {
    throw new Error(
      `the scanner reads an integer viewBox, and this drawing has "${svg.getAttribute("viewBox")}"`,
    );
  }
  const [minX, minY, w, h] = box as [number, number, number, number];
  const width = (w + 2 * PAGE) * SCALE;
  const height = (h + 2 * PAGE) * SCALE;
  const pixels = new Uint8ClampedArray(width * height * 4);

  const paint = (x: number, y: number, cw: number, ch: number, [r, g, b]: Rgb) => {
    for (
      let py = Math.max(0, (y - minY + PAGE) * SCALE);
      py < Math.min(height, (y - minY + PAGE + ch) * SCALE);
      py++
    ) {
      for (
        let px = Math.max(0, (x - minX + PAGE) * SCALE);
        px < Math.min(width, (x - minX + PAGE + cw) * SCALE);
        px++
      ) {
        const i = (py * width + px) * 4;
        pixels[i] = r;
        pixels[i + 1] = g;
        pixels[i + 2] = b;
        pixels[i + 3] = 255;
      }
    }
  };

  paint(minX - PAGE, minY - PAGE, w + 2 * PAGE, h + 2 * PAGE, GROUND);
  for (const rect of svg.querySelectorAll("rect")) {
    const n = (name: string) => Number(rect.getAttribute(name) ?? 0);
    paint(n("x"), n("y"), n("width"), n("height"), ink(rect.getAttribute("fill")));
  }
  for (const path of svg.querySelectorAll("path")) {
    const fill = ink(path.getAttribute("fill"));
    for (const { x, y, n } of runs(path.getAttribute("d") ?? "")) paint(x, y, n, 1, fill);
  }

  return jsQR(pixels, width, height, { inversionAttempts: "dontInvert" })?.data ?? null;
}

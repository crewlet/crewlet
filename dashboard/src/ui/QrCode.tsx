/**
 * A QR code, drawn as inline SVG from the encoder's module matrix.
 *
 * # Inline SVG, and nothing else
 *
 * The encoder (`uqr`) answers a matrix of booleans and this component draws it
 * as one `<path>` on one `<rect>`. Not an `<img>` with a `data:` URI, not a
 * string of markup set as HTML, and nothing fetched: the page's
 * Content-Security-Policy admits all three only by being widened, and the one
 * value this draws today is a credential — a second factor's seed — which must
 * not leave the DOM it is shown in. An element is also what a test can read
 * back and decode.
 *
 * # Black on a white plate, in either theme
 *
 * A QR code is not a colour of the palette: it is a symbol a camera reads, and
 * its two inks are part of the symbol. A code is specified dark on light, and
 * reading one inverted is something some scanners do and others do not, so a
 * code inverted for the dark theme is one some authenticator apps will not
 * read — and it is NOT inverted. It carries its own white plate, the quiet
 * zone included, which makes it the one place besides a third-party app's
 * mark (`VendorMark`) where the dashboard paints colours no token names (see
 * the design document's "The one rule"): nothing reads state from them and
 * nothing else is painted with them. The plate is kept under forced colours
 * for the same reason, in `components.css`.
 *
 * # Four modules of quiet zone, and level M
 *
 * The margin a reader needs to find the three finder patterns is four modules
 * of light on every side — the standard's own figure — and a code drawn
 * tighter than that is one a scanner may not find against whatever surrounds
 * it. Error correction is M, which still reads with 15% of the symbol lost:
 * the level authenticator codes are conventionally drawn at. L leaves less
 * room for glare on a screen, and Q or H grow the symbol, which shrinks every
 * module on the same plate.
 */

import { useMemo } from "react";
import { encode } from "uqr";

/** Light modules around the symbol, on every side. */
export const QUIET_ZONE = 4;

/** The dark modules' ink. Not a token: see "Black on a white plate" above. */
export const QR_DARK = "#000000";

/** The plate's ink, the quiet zone included. Not a token, for the same reason. */
export const QR_LIGHT = "#ffffff";

/**
 * The symbol's modules for `value`, `true` for dark, row by row — or null for a
 * value too long for any QR code to hold.
 *
 * NULL RATHER THAN A THROW, because the encoder refuses an oversized value
 * with a `RangeError` and a code that cannot be drawn is an aid missing, not a
 * screen that should fall over: whoever renders one keeps the value's other
 * forms beside it. Any other error is a fault and is not caught.
 */
export function qrModules(value: string): boolean[][] | null {
  try {
    return encode(value, { ecc: "M", border: 0 }).data;
  } catch (err) {
    if (err instanceof RangeError) return null;
    throw err;
  }
}

/**
 * The dark modules as one SVG path in module units: one rectangle per
 * horizontal run of dark modules, so the path holds a subpath per run rather
 * than per module and adjacent modules share an edge instead of meeting at
 * two anti-aliased ones.
 */
export function modulePath(modules: readonly (readonly boolean[])[]): string {
  let d = "";
  modules.forEach((row, y) => {
    let x = 0;
    while (x < row.length) {
      if (!row[x]) {
        x++;
        continue;
      }
      let end = x;
      while (end < row.length && row[end]) end++;
      d += `M${x} ${y}h${end - x}v1h-${end - x}z`;
      x = end;
    }
  });
  return d;
}

/**
 * `value` as a QR code, with `label` as the image's accessible name — or
 * nothing, for a value no QR code can hold.
 */
export function QrCode({ value, label }: { value: string; label: string }) {
  const modules = useMemo(() => qrModules(value), [value]);
  if (!modules) return null;
  const span = modules.length + 2 * QUIET_ZONE;
  return (
    <svg
      className="qr-code"
      viewBox={`${-QUIET_ZONE} ${-QUIET_ZONE} ${span} ${span}`}
      role="img"
      aria-label={label}
      shapeRendering="crispEdges"
    >
      <rect x={-QUIET_ZONE} y={-QUIET_ZONE} width={span} height={span} fill={QR_LIGHT} />
      <path d={modulePath(modules)} fill={QR_DARK} />
    </svg>
  );
}

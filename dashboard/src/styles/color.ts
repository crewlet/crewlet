/**
 * Colour maths for the palette suite — sRGB, WCAG, OKLab and dichromat
 * simulation, with no dependencies.
 *
 * This exists because the token file makes three promises that cannot be
 * checked by looking at it: that every text step clears contrast on the WORST
 * surface it can land on (not the panel it was designed against), that a fill
 * step is never used where a text step belongs, and that the six data hues stay
 * separable — including for red-green colour vision. Those were measured by
 * hand once in the system this replaces and written into a doc comment, where
 * nothing kept them true. Here they are computed from the stylesheet that
 * actually ships.
 */

export type RGB = { r: number; g: number; b: number };

// ---------------------------------------------------------------------------
// Parsing
// ---------------------------------------------------------------------------

/**
 * Pull custom properties out of a stylesheet, grouped by selector.
 *
 * Deliberately not a CSS parser: tokens.css is a flat file of blocks holding
 * nothing but custom properties, and a regex that assumes that will fail
 * loudly — an empty token set — rather than silently mis-parse if it ever
 * stops being true.
 */
export function parseTokens(css: string): Map<string, Map<string, string>> {
  const clean = css.replace(/\/\*[\s\S]*?\*\//g, "");
  const bySelector = new Map<string, Map<string, string>>();
  const block = /([^{}]+)\{([^{}]*)\}/g;
  let m: RegExpExecArray | null;
  while ((m = block.exec(clean))) {
    const selectors = (m[1] ?? "").split(",").map((s) => s.trim());
    const decls = new Map<string, string>();
    for (const line of (m[2] ?? "").split(";")) {
      const idx = line.indexOf(":");
      if (idx < 0) continue;
      const name = line.slice(0, idx).trim();
      if (!name.startsWith("--")) continue;
      decls.set(name, line.slice(idx + 1).trim());
    }
    for (const sel of selectors) {
      const prior = bySelector.get(sel) ?? new Map<string, string>();
      for (const [k, v] of decls) prior.set(k, v);
      bySelector.set(sel, prior);
    }
  }
  return bySelector;
}

/** Merge the selector blocks that apply to one theme, in cascade order. */
export function themeTokens(
  bySelector: Map<string, Map<string, string>>,
  selectors: readonly string[],
): Map<string, string> {
  const out = new Map<string, string>();
  for (const sel of selectors) {
    const block = bySelector.get(sel);
    if (!block) continue;
    for (const [k, v] of block) out.set(k, v);
  }
  return out;
}

// ---------------------------------------------------------------------------
// Colour values
// ---------------------------------------------------------------------------

export function parseHex(hex: string): RGB | null {
  const m = /^#([0-9a-f]{6})$/i.exec(hex.trim());
  if (!m) return null;
  const n = parseInt(m[1] as string, 16);
  return { r: (n >> 16) & 255, g: (n >> 8) & 255, b: n & 255 };
}

export function parseRgba(value: string): { rgb: RGB; a: number } | null {
  const m = /^rgba?\(([^)]+)\)$/i.exec(value.trim());
  if (!m) return null;
  const parts = (m[1] as string)
    .split(/[,/\s]+/)
    .filter(Boolean)
    .map(Number);
  const [r, g, b, a] = parts;
  if (r === undefined || g === undefined || b === undefined) return null;
  return { rgb: { r, g, b }, a: a === undefined ? 1 : a };
}

/** Composite a possibly-translucent value over an opaque backdrop. */
export function flatten(value: string, backdrop: RGB): RGB | null {
  const hex = parseHex(value);
  if (hex) return hex;
  const rgba = parseRgba(value);
  if (!rgba) return null;
  const { rgb, a } = rgba;
  return {
    r: rgb.r * a + backdrop.r * (1 - a),
    g: rgb.g * a + backdrop.g * (1 - a),
    b: rgb.b * a + backdrop.b * (1 - a),
  };
}

// ---------------------------------------------------------------------------
// WCAG
// ---------------------------------------------------------------------------

function channel(v: number): number {
  const c = v / 255;
  return c <= 0.04045 ? c / 12.92 : ((c + 0.055) / 1.055) ** 2.4;
}

export function luminance({ r, g, b }: RGB): number {
  return 0.2126 * channel(r) + 0.7152 * channel(g) + 0.0722 * channel(b);
}

export function contrast(a: RGB, b: RGB): number {
  const la = luminance(a);
  const lb = luminance(b);
  const [hi, lo] = la > lb ? [la, lb] : [lb, la];
  return (hi + 0.05) / (lo + 0.05);
}

// ---------------------------------------------------------------------------
// OKLab — perceptual distance, which is what "these two hues are separable"
// actually means. RGB distance says magenta and red are far apart and says the
// same of two greens a reader cannot tell apart at 11px.
// ---------------------------------------------------------------------------

export function toOklab({ r, g, b }: RGB): { L: number; a: number; b: number } {
  const lr = channel(r);
  const lg = channel(g);
  const lb = channel(b);
  const l = Math.cbrt(0.4122214708 * lr + 0.5363325363 * lg + 0.0514459929 * lb);
  const m = Math.cbrt(0.2119034982 * lr + 0.6806995451 * lg + 0.1073969566 * lb);
  const s = Math.cbrt(0.0883024619 * lr + 0.2817188376 * lg + 0.6299787005 * lb);
  return {
    L: 0.2104542553 * l + 0.793617785 * m - 0.0040720468 * s,
    a: 1.9779984951 * l - 2.428592205 * m + 0.4505937099 * s,
    b: 0.0259040371 * l + 0.7827717662 * m - 0.808675766 * s,
  };
}

/** Perceptual distance, scaled to roughly CIE ΔE units so thresholds read familiar. */
export function deltaE(x: RGB, y: RGB): number {
  const a = toOklab(x);
  const b = toOklab(y);
  return Math.hypot(a.L - b.L, a.a - b.a, a.b - b.b) * 100;
}

export function chroma(c: RGB): number {
  const { a, b } = toOklab(c);
  return Math.hypot(a, b) * 100;
}

// ---------------------------------------------------------------------------
// Dichromat simulation (Brettel/Viénot-style linear approximation)
// ---------------------------------------------------------------------------

function fromLinear(v: number): number {
  const c = v <= 0.0031308 ? v * 12.92 : 1.055 * v ** (1 / 2.4) - 0.055;
  return Math.min(255, Math.max(0, c * 255));
}

/** Simulate protanopia or deuteranopia. Enough to answer "do these collide?". */
export function simulate(c: RGB, kind: "protan" | "deutan"): RGB {
  const r = channel(c.r);
  const g = channel(c.g);
  const b = channel(c.b);
  // LMS
  const L = 0.31399022 * r + 0.63951294 * g + 0.04649755 * b;
  const M = 0.15537241 * r + 0.75789446 * g + 0.08670142 * b;
  const S = 0.01775239 * r + 0.10944209 * g + 0.87256922 * b;
  let L2 = L;
  let M2 = M;
  const S2 = S;
  if (kind === "protan") L2 = 1.05118294 * M - 0.05116099 * S;
  else M2 = 0.9513092 * L + 0.04866992 * S;
  const rr = 5.47221206 * L2 - 4.6419601 * M2 + 0.16963708 * S2;
  const gg = -1.1252419 * L2 + 2.29317094 * M2 - 0.1678952 * S2;
  const bb = 0.02980165 * L2 - 0.19318073 * M2 + 1.16364789 * S2;
  return { r: fromLinear(rr), g: fromLinear(gg), b: fromLinear(bb) };
}

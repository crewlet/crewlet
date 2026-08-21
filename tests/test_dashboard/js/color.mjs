// Colour maths for the palette suite — sRGB, WCAG, OKLab, and dichromat
// simulation, with no dependencies.
//
// This exists because the dashboard's palette makes three promises that
// cannot be checked by looking at it: that every hue stays separable from
// its neighbours (including for red-green colour vision), that every mark
// and label clears contrast, and that the themes stay inside a comfort
// band rather than merely above a legibility floor. Those were measured
// by hand once and written into a doc comment, where nothing kept them
// true. Here they are computed from the stylesheet that actually ships.

// ---------------------------------------------------------------------
// Parsing
// ---------------------------------------------------------------------

/**
 * Pull custom properties out of a stylesheet, grouped by selector.
 *
 * Deliberately not a CSS parser: tokens.css is a flat file of `:root`
 * blocks holding nothing but custom properties, and a regex that assumes
 * that will fail loudly (an empty token set) rather than silently
 * mis-parse if it ever stops being true.
 */
export function parseTokens(css) {
  const clean = css.replace(/\/\*[\s\S]*?\*\//g, "");
  const bySelector = new Map();
  const block = /([^{}]+)\{([^{}]*)\}/g;
  let m;
  while ((m = block.exec(clean))) {
    const selectors = m[1].split(",").map((s) => s.trim());
    const decls = new Map();
    for (const line of m[2].split(";")) {
      const idx = line.indexOf(":");
      if (idx < 0) continue;
      const name = line.slice(0, idx).trim();
      if (!name.startsWith("--")) continue;
      decls.set(name, line.slice(idx + 1).trim());
    }
    for (const sel of selectors) {
      const prior = bySelector.get(sel) || new Map();
      for (const [k, v] of decls) prior.set(k, v);
      bySelector.set(sel, prior);
    }
  }
  return bySelector;
}

/** Merge the selector blocks that apply to one theme, in cascade order. */
export function themeTokens(bySelector, selectors) {
  const out = new Map();
  for (const sel of selectors) {
    const block = bySelector.get(sel);
    if (!block) continue;
    for (const [k, v] of block) out.set(k, v);
  }
  return out;
}

// ---------------------------------------------------------------------
// Colour values
// ---------------------------------------------------------------------

/** An RGBA colour with channels in 0..255 and alpha in 0..1. */
function rgba(r, g, b, a = 1) {
  return { r, g, b, a };
}

/**
 * Parse one colour literal, following `var(--x)` references through the
 * token map. Returns null for anything that is not a colour (a gradient,
 * a shadow, a length), so callers can assert on what they expect.
 */
export function parseColor(value, tokens, seen = new Set()) {
  let v = String(value || "").trim();

  const ref = /^var\(\s*(--[\w-]+)\s*(?:,\s*([^)]+))?\)$/.exec(v);
  if (ref) {
    const name = ref[1];
    if (seen.has(name)) return null; // a cycle is a bug, not a colour
    seen.add(name);
    const target = tokens.get(name);
    if (target !== undefined) return parseColor(target, tokens, seen);
    return ref[2] ? parseColor(ref[2], tokens, seen) : null;
  }

  const hex = /^#([0-9a-f]{3}|[0-9a-f]{4}|[0-9a-f]{6}|[0-9a-f]{8})$/i.exec(v);
  if (hex) {
    const h = hex[1];
    const wide = h.length > 4;
    const size = wide ? 2 : 1;
    const at = (i) => {
      const part = h.slice(i * size, i * size + size);
      const full = wide ? part : part + part;
      return parseInt(full, 16);
    };
    const hasAlpha = h.length === 4 || h.length === 8;
    return rgba(at(0), at(1), at(2), hasAlpha ? at(3) / 255 : 1);
  }

  const fn = /^rgba?\(([^)]+)\)$/i.exec(v);
  if (fn) {
    const parts = fn[1]
      .split(/[,/]/)
      .map((p) => p.trim())
      .filter(Boolean);
    if (parts.length < 3) return null;
    const num = (p) =>
      p.endsWith("%") ? (parseFloat(p) / 100) * 255 : parseFloat(p);
    const alpha = parts[3] === undefined
      ? 1
      : parts[3].endsWith("%")
        ? parseFloat(parts[3]) / 100
        : parseFloat(parts[3]);
    return rgba(num(parts[0]), num(parts[1]), num(parts[2]), alpha);
  }

  return null;
}

/**
 * Composite a colour over an opaque backdrop.
 *
 * The dashboard's whole surface theory is alpha-over-ground, so nearly
 * every panel, hairline and muted label is translucent. Measuring one of
 * those against its nominal token value rather than against what the eye
 * receives is how a "passing" contrast number can describe a colour that
 * is never painted.
 */
export function over(color, backdrop) {
  const a = color.a;
  return rgba(
    color.r * a + backdrop.r * (1 - a),
    color.g * a + backdrop.g * (1 - a),
    color.b * a + backdrop.b * (1 - a),
    1,
  );
}

/** Composite through a stack, base first. */
export function flatten(stack) {
  return stack.reduce((base, layer) => over(layer, base));
}

// ---------------------------------------------------------------------
// WCAG contrast
// ---------------------------------------------------------------------

function toLinear(channel) {
  const c = channel / 255;
  return c <= 0.04045 ? c / 12.92 : Math.pow((c + 0.055) / 1.055, 2.4);
}

export function luminance(color) {
  return (
    0.2126 * toLinear(color.r) +
    0.7152 * toLinear(color.g) +
    0.0722 * toLinear(color.b)
  );
}

/** WCAG 2.x contrast ratio, 1..21. */
export function contrast(a, b) {
  const la = luminance(a);
  const lb = luminance(b);
  const hi = Math.max(la, lb);
  const lo = Math.min(la, lb);
  return (hi + 0.05) / (lo + 0.05);
}

// ---------------------------------------------------------------------
// OKLab
// ---------------------------------------------------------------------

/** Perceptual coordinates: L 0..1, a/b roughly -0.4..0.4. */
export function oklab(color) {
  const r = toLinear(color.r);
  const g = toLinear(color.g);
  const b = toLinear(color.b);

  const l = 0.4122214708 * r + 0.5363325363 * g + 0.0514459929 * b;
  const m = 0.2119034982 * r + 0.6806995451 * g + 0.1073969566 * b;
  const s = 0.0883024619 * r + 0.2817188376 * g + 0.6299787005 * b;

  const l_ = Math.cbrt(l);
  const m_ = Math.cbrt(m);
  const s_ = Math.cbrt(s);

  return {
    L: 0.2104542553 * l_ + 0.793617785 * m_ - 0.0040720468 * s_,
    a: 1.9779984951 * l_ - 2.428592205 * m_ + 0.4505937099 * s_,
    b: 0.0259040371 * l_ + 0.7827717662 * m_ - 0.808675766 * s_,
  };
}

/** Perceptual distance, scaled ×100 so the gates read as whole numbers. */
export function deltaE(x, y) {
  const p = oklab(x);
  const q = oklab(y);
  return (
    100 *
    Math.hypot(p.L - q.L, p.a - q.a, p.b - q.b)
  );
}

/** OKLab chroma ×100 — how saturated a colour is, perceptually. */
export function chroma(color) {
  const c = oklab(color);
  return 100 * Math.hypot(c.a, c.b);
}

// ---------------------------------------------------------------------
// Dichromat simulation
// ---------------------------------------------------------------------
// Viénot, Brettel & Mollon (1999), applied in linear RGB. The dashboard
// stacks up to ten hue families edge to edge, so "are these two distinct"
// has to be asked with the two most common forms of colour blindness in
// the room, not only with normal vision.

const CVD_MATRIX = {
  protan: [
    [0.11238, 0.88762, 0.0],
    [0.11238, 0.88762, 0.0],
    [0.00401, -0.00401, 1.0],
  ],
  deutan: [
    [0.29275, 0.70725, 0.0],
    [0.29275, 0.70725, 0.0],
    [-0.02234, 0.02234, 1.0],
  ],
};

function fromLinear(value) {
  const c = Math.min(1, Math.max(0, value));
  const s = c <= 0.0031308 ? c * 12.92 : 1.055 * Math.pow(c, 1 / 2.4) - 0.055;
  return s * 255;
}

export function simulate(color, kind) {
  const matrix = CVD_MATRIX[kind];
  if (!matrix) throw new Error(`unknown vision type: ${kind}`);
  const v = [toLinear(color.r), toLinear(color.g), toLinear(color.b)];
  const out = matrix.map((row) =>
    row[0] * v[0] + row[1] * v[1] + row[2] * v[2],
  );
  return rgba(fromLinear(out[0]), fromLinear(out[1]), fromLinear(out[2]), 1);
}

export { rgba };

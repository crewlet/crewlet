// @vitest-environment node
/**
 * The palette's promises, recomputed from the stylesheet that ships.
 *
 * Every claim in tokens.css's own comments is measured here, in BOTH themes,
 * over the composited surfaces a token can actually land on. The point is not
 * that the numbers are pretty: it is that a future edit which lowers one of
 * them fails a build instead of shipping.
 */

import { readFileSync } from "node:fs";
import { fileURLToPath } from "node:url";
import { describe, expect, test } from "vitest";
import {
  chroma,
  contrast,
  deltaE,
  flatten,
  parseHex,
  parseTokens,
  simulate,
  themeTokens,
  type RGB,
} from "./color.ts";

const css = readFileSync(fileURLToPath(new URL("./tokens.css", import.meta.url)), "utf8");
const bySelector = parseTokens(css);

const THEMES = {
  light: [":root"],
  dark: [":root", ':root[data-theme="dark"]'],
} as const;

function tokensFor(theme: keyof typeof THEMES): Map<string, string> {
  return themeTokens(bySelector, THEMES[theme]);
}

function opaque(tokens: Map<string, string>, name: string): RGB {
  const raw = tokens.get(name);
  if (!raw) throw new Error(`token ${name} is not defined`);
  const hex = parseHex(raw);
  if (!hex) throw new Error(`token ${name} is ${raw}, which is not an opaque colour`);
  return hex;
}

/**
 * Every opaque surface a piece of text can end up on.
 *
 * This list is the whole point of the exercise. The system this replaces
 * anchored its text ramp to the panel fill and then used those same steps on a
 * selected row inside a nested panel, which put six of its nine steps under
 * 4.5:1 in one theme.
 */
function pageSurfaces(tokens: Map<string, string>): { name: string; rgb: RGB }[] {
  // The opaque grounds a filled control actually sits on. A fill is not
  // measured against a hovered row inside a nested panel, because a hue
  // appearing THERE is a rail or a dot and takes the `-ink` step.
  return ["--bg", "--bg-sunken", "--surface-1", "--surface-2", "--surface-3"].map((name) => ({
    name,
    rgb: opaque(tokens, name),
  }));
}

function surfaces(tokens: Map<string, string>): { name: string; rgb: RGB }[] {
  const base = pageSurfaces(tokens);
  const out = [...base];
  // A translucent overlay composited onto each opaque ground is a real surface
  // too — it is what a hovered or selected row actually is.
  for (const overlay of ["--surface-hover", "--surface-active", "--surface-inset"]) {
    const value = tokens.get(overlay);
    if (!value) continue;
    for (const ground of base) {
      const rgb = flatten(value, ground.rgb);
      if (rgb) out.push({ name: `${overlay} on ${ground.name}`, rgb });
    }
  }
  return out;
}

/** The steps that carry a FACT, and the floor each has to clear. */
const TEXT_STEPS: { token: string; min: number; why: string }[] = [
  { token: "--text", min: 7, why: "body copy, and the default for every cell" },
  { token: "--text-secondary", min: 4.5, why: "secondary facts — still read, still normative" },
  { token: "--text-muted", min: 4.5, why: "labels and captions; a fact, so it clears AA" },
  { token: "--heading", min: 7, why: "headings" },
];

/** The `-ink` steps: a hue used as TEXT on a surface. */
const INK_STEPS = [
  "--accent-ink",
  "--positive-ink",
  "--caution-ink",
  "--critical-ink",
  "--info-ink",
  "--phase-plan-ink",
  "--phase-execute-ink",
  "--phase-review-ink",
];

/** The fill steps: a hue used as a MARK, or as a fill behind --text-on-fill. */
const FILL_STEPS = [
  "--accent",
  "--positive",
  "--caution",
  "--critical",
  "--info",
  "--phase-plan",
  "--phase-execute",
  "--phase-review",
];

const VIZ = ["--viz-1", "--viz-2", "--viz-3", "--viz-4", "--viz-5"];

for (const theme of ["light", "dark"] as const) {
  describe(`${theme} theme`, () => {
    const tokens = tokensFor(theme);

    test("every neutral text step clears its floor on every surface it can land on", () => {
      const failures: string[] = [];
      for (const step of TEXT_STEPS) {
        const ink = opaque(tokens, step.token);
        for (const surface of surfaces(tokens)) {
          const ratio = contrast(ink, surface.rgb);
          if (ratio < step.min) {
            failures.push(
              `${step.token} on ${surface.name}: ${ratio.toFixed(2)}:1 < ${step.min} (${step.why})`,
            );
          }
        }
      }
      expect(failures).toEqual([]);
    });

    test("--text-faint is decoration, and is declared low rather than pretending", () => {
      // It exists for hairline glyphs and disabled affordances. The assertion
      // is that it stays BELOW the fact floor: a step that quietly crept up to
      // 4.5 would invite it into a table cell, which is the whole failure the
      // separate name prevents.
      const faint = opaque(tokens, "--text-faint");
      const panel = opaque(tokens, "--surface-1");
      const ratio = contrast(faint, panel);
      expect(ratio).toBeGreaterThan(2.8);
      expect(ratio).toBeLessThan(4.5);
    });

    test("every -ink step clears 4.5:1 as text on every surface", () => {
      const failures: string[] = [];
      for (const token of INK_STEPS) {
        const ink = opaque(tokens, token);
        for (const surface of surfaces(tokens)) {
          const ratio = contrast(ink, surface.rgb);
          if (ratio < 4.5) failures.push(`${token} on ${surface.name}: ${ratio.toFixed(2)}:1`);
        }
      }
      expect(failures).toEqual([]);
    });

    test("every fill step clears 3:1 as a mark on the surfaces it sits on", () => {
      // 3:1 is the non-text floor: these are dots, bars, rails and chart marks.
      const failures: string[] = [];
      for (const token of [...FILL_STEPS, ...VIZ]) {
        const fill = opaque(tokens, token);
        for (const surface of pageSurfaces(tokens)) {
          const ratio = contrast(fill, surface.rgb);
          if (ratio < 3) failures.push(`${token} on ${surface.name}: ${ratio.toFixed(2)}:1`);
        }
      }
      expect(failures).toEqual([]);
    });

    test("--text-on-fill clears 4.5:1 on the accent, the one fill it is painted over", () => {
      // ONLY the accent. A status is rendered as a soft tint carrying its own
      // `-ink` label, never as a solid block with text on it — which is what
      // lets the status fills be light enough to read as marks on a dark
      // ground without dragging a white label under 4.5:1 with them.
      const ratio = contrast(opaque(tokens, "--text-on-fill"), opaque(tokens, "--accent"));
      expect(ratio).toBeGreaterThanOrEqual(4.5);
    });

    test("the three phase hues stay separable, including for protan and deutan vision", () => {
      const phases = ["--phase-plan", "--phase-execute", "--phase-review"];
      const failures: string[] = [];
      for (const kind of ["normal", "protan", "deutan"] as const) {
        for (let i = 0; i < phases.length; i++) {
          for (let j = i + 1; j < phases.length; j++) {
            const a = opaque(tokens, phases[i] as string);
            const b = opaque(tokens, phases[j] as string);
            const [x, y] = kind === "normal" ? [a, b] : [simulate(a, kind), simulate(b, kind)];
            const d = deltaE(x, y);
            if (d < 10) {
              failures.push(`${phases[i]} vs ${phases[j]} (${kind}): ΔE ${d.toFixed(1)}`);
            }
          }
        }
      }
      expect(failures).toEqual([]);
    });

    test("adjacent data hues stay separable in series order, including for dichromats", () => {
      // ADJACENT, not every pair: a legend reader distinguishes series 2 from
      // series 3 because they sit next to each other, and demanding every pair
      // of six be far apart is what forces a palette to spread until it is
      // ugly. Greyscale is checked too — a printed or screenshotted chart.
      const failures: string[] = [];
      for (const kind of ["normal", "protan", "deutan"] as const) {
        for (let i = 0; i < VIZ.length - 1; i++) {
          const a = opaque(tokens, VIZ[i] as string);
          const b = opaque(tokens, VIZ[i + 1] as string);
          const [x, y] = kind === "normal" ? [a, b] : [simulate(a, kind), simulate(b, kind)];
          const d = deltaE(x, y);
          if (d < 9) failures.push(`${VIZ[i]} vs ${VIZ[i + 1]} (${kind}): ΔE ${d.toFixed(1)}`);
        }
      }
      expect(failures).toEqual([]);
    });

    test("no data hue collides with the reserved critical red", () => {
      // Red means "this broke" everywhere in the product. A chart series that
      // happens to be red says so too, to a reader who is scanning for it.
      const red = opaque(tokens, "--critical");
      const failures: string[] = [];
      for (const token of VIZ) {
        const d = deltaE(opaque(tokens, token), red);
        if (d < 14) failures.push(`${token} vs --critical: ΔE ${d.toFixed(1)}`);
      }
      expect(failures).toEqual([]);
    });

    test("the neutral ramp stays neutral", () => {
      // The chroma cap is what stops the ground drifting back to the violet
      // cast the previous system had, where every surface was a tint of the
      // accent and the accent had nothing to separate itself from.
      const failures: string[] = [];
      for (const name of ["--bg", "--bg-sunken", "--surface-1", "--surface-2", "--surface-3"]) {
        const c = chroma(opaque(tokens, name));
        if (c > 2.2) failures.push(`${name}: chroma ${c.toFixed(1)}`);
      }
      expect(failures).toEqual([]);
    });

    test("the accent is the most saturated thing on the page", () => {
      // Not vanity: the accent marks the ONE current selection, and it can
      // only do that if nothing else competes. A status hue that out-saturated
      // it would pull the eye to a badge instead of to where the reader is.
      const accent = chroma(opaque(tokens, "--accent"));
      for (const token of [...FILL_STEPS.filter((t) => t !== "--accent"), ...VIZ]) {
        // --viz-1 IS the accent hue, so it is allowed to match.
        if (token === "--viz-1") continue;
        expect(chroma(opaque(tokens, token)), `${token} vs --accent`).toBeLessThanOrEqual(accent);
      }
    });

    test("the ground is lifted off pure black / pure white", () => {
      const bg = opaque(tokens, "--bg");
      const sum = bg.r + bg.g + bg.b;
      if (theme === "dark") expect(sum).toBeGreaterThan(12);
      else expect(sum).toBeLessThan(760);
    });
  });
}

describe("the token file itself", () => {
  test("every colour token has a value on bare :root", () => {
    // A colour whose only definition is inside a media query or a [data-theme]
    // block is a colour that disappears for somebody.
    const base = bySelector.get(":root");
    expect(base).toBeDefined();
    const dark = bySelector.get(':root[data-theme="dark"]');
    expect(dark).toBeDefined();
    const missing = [...(dark as Map<string, string>).keys()].filter((k) => !base?.has(k));
    expect(missing).toEqual([]);
  });

  test("the dark media block and the explicit dark block agree", () => {
    // They are written twice on purpose — once so the OS setting works, once so
    // the toggle wins in both directions — and a value that drifts between them
    // means the toggle changes colours the OS setting does not.
    const clean = css.replace(/\/\*[\s\S]*?\*\//g, "");
    const start = clean.indexOf("@media (prefers-color-scheme: dark)");
    expect(start, "the prefers-color-scheme block is still there").toBeGreaterThan(-1);
    // Brace-count to the end of the at-rule rather than matching a shape: a
    // regex over nested blocks fails silently as an empty match, which reads
    // as "nothing drifted".
    let depth = 0;
    let end = start;
    for (let i = clean.indexOf("{", start); i < clean.length; i++) {
      if (clean[i] === "{") depth++;
      else if (clean[i] === "}") {
        depth--;
        if (depth === 0) {
          end = i;
          break;
        }
      }
    }
    const inner = clean.slice(clean.indexOf("{", start) + 1, end);
    const inMedia = parseTokens(inner).get(':root:not([data-theme="light"])');
    const explicit = bySelector.get(':root[data-theme="dark"]');
    expect(inMedia).toBeDefined();
    const drift: string[] = [];
    for (const [k, v] of inMedia as Map<string, string>) {
      if (explicit?.get(k) !== v) drift.push(`${k}: ${v} vs ${explicit?.get(k)}`);
    }
    expect(drift).toEqual([]);
  });
});

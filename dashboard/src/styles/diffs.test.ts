// @vitest-environment node
import { readFileSync } from "node:fs";
import { createRequire } from "node:module";
import { describe, expect, test } from "vitest";
import { DEFAULT_THEME_ID, themeRegistry } from "@crewlethq/tokens";
import {
  OPAQUE_SURFACES,
  RUNG_STEPS,
  deltaE,
  paletteStates,
  parseHex,
  type Rgb,
} from "@crewlethq/tokens/test/palette";

import { sheetRules, type SheetRule } from "../test/sheets.ts";
import { modules, parse, walk, lineOf, type Lang, type Node } from "../test/source.ts";

/**
 * DIFFS CARRY NO HUE ("Diffs carry no hue" in docs/reference/dashboard-design.md).
 *
 * Colour carries STATE here, and a diff is not one: a removed line is not a
 * failure and an added one is not done. The design system's three diffs —
 * `DiffView`, `DiffSummary`, `DiffList` — tint an added line with the success
 * state and a removed one with the danger state, and so did this tree's own
 * configuration diff. On the page that draws a change, a red failed-check chip
 * beside red removed lines makes one red mean two things. So every diff is
 * `components/DiffView.tsx`, and this file holds the rule three ways:
 *
 * - IN THE SHEETS: no rule that names a diff spends a state's hue, a chart
 *   series' or an identity's.
 * - IN THE PALETTE: the two surface steps that tell an added line from a
 *   removed one are one rung either side of the diff's own ground, and are
 *   measured against it in every state of every theme the dashboard offers.
 * - IN THE TREE: nothing imports the package's diffs, and nothing hands its
 *   `VersionCompare` the two texts that would make it draw one.
 *
 * The accent is not among the hues forbidden: it says where the reader is (a
 * focus ring, a range they picked), which is never a line's kind.
 */

const require_ = createRequire(import.meta.url);

/** A token that is a hue nothing but its own kind of fact may wear. */
const HUE = /--color-(?:feedback|data|node)-[\w-]*/g;

/** Every rule naming a diff that spends one of those hues, as `file selector -> token`. */
export function huedDiffRules(rules: readonly SheetRule[]): string[] {
  return rules
    .filter((rule) => /diff/i.test(rule.selector))
    .flatMap((rule) =>
      [...rule.body.matchAll(HUE)].map((m) => `${rule.file} ${rule.selector} -> ${m[0]}`),
    );
}

describe("in the sheets", () => {
  test("no rule that names a diff spends a state's, a series' or an identity's hue", () => {
    const diffs = sheetRules().filter((rule) => /diff/i.test(rule.selector));
    // A VACUITY FLOOR: a scan that found no diff rule checked nothing.
    expect(diffs.length).toBeGreaterThan(10);
    expect(huedDiffRules(sheetRules())).toEqual([]);
  });

  test("it can tell — the configuration diff's old tints are reported", () => {
    // THE RULES THIS REPLACED, as they were written.
    const old: SheetRule[] = [
      {
        file: "screens.css",
        selector: '.config-diff-line[data-kind="added"]',
        body: "background: var(--color-feedback-success-soft); color: var(--color-feedback-success-ink);",
      },
      {
        file: "screens.css",
        selector: ".turn-card.failed",
        body: "color: var(--color-feedback-danger-ink);",
      },
    ];
    expect(huedDiffRules(old)).toEqual([
      'screens.css .config-diff-line[data-kind="added"] -> --color-feedback-success-soft',
      'screens.css .config-diff-line[data-kind="added"] -> --color-feedback-success-ink',
    ]);
  });
});

/** The token the sheets' one rule for exactly `selector` paints its background with. */
function paint(selector: string): string {
  const rules = sheetRules().filter((r) => r.selector === selector);
  expect(rules, `${selector} is declared ${rules.length} times, not once`).toHaveLength(1);
  const token = /background(?:-color)?:\s*var\((--[\w-]+)\)/.exec(rules[0]!.body)?.[1];
  expect(token, `${selector} paints no token`).toBeTruthy();
  return token!;
}

/** Every state of every theme the dashboard offers, as `[name, resolved values]`. */
function states(): [string, Map<string, string>][] {
  const at = (n: string) => readFileSync(require_.resolve(`@crewlethq/tokens/css${n}`), "utf8");
  const base = { tokens: at(""), themes: at("/themes") };
  return themeRegistry.flatMap((theme) => {
    const sources =
      theme.id === DEFAULT_THEME_ID
        ? base
        : {
            ...base,
            themes: `${base.themes}\n${at(`/themes/${theme.id}`)}`,
            palette: theme.id,
            tint: theme.tint,
            contrast: theme.contrast,
          };
    return Object.entries(paletteStates(sources))
      .filter(([state]) => state !== "base")
      .map(([state, values]) => [`${theme.id}/${state}`, values] as [string, Map<string, string>]);
  });
}

function colour(values: Map<string, string>, token: string): Rgb {
  const rgb = parseHex(values.get(token) ?? "");
  expect(rgb, `${token} is ${values.get(token)}, not an opaque colour`).not.toBeNull();
  return rgb!;
}

/** The ΔE the palette holds `upper` off `lower` by, from the package's own ladder. */
function floor(upper: string, lower: string): number {
  const step = RUNG_STEPS.find(([, u, l]) => u === upper && l === lower);
  expect(step, `the palette holds no step from ${lower} up to ${upper}`).toBeTruthy();
  return step![3];
}

describe("in the palette", () => {
  // TWO SURFACE STEPS, ONE EITHER SIDE OF THE GROUND: the rungs are the
  // package's ladder (`OPAQUE_SURFACES`, frame to raised), so an added line
  // stands on the rung above the diff's ground and a removed one on the rung
  // below it. Opaque rungs, because every text step is measured on those and
  // on no translucent overlay a theme could thin out of sight.
  test("an added line is the rung above the diff's ground, a removed one the rung below", () => {
    const ground = paint(".diff-view__body");
    const up = paint('.diff-view__line[data-kind="add"]');
    const down = paint('.diff-view__line[data-kind="remove"]');
    const at = OPAQUE_SURFACES.indexOf(ground);
    expect(at, `${ground} is not one of the ladder's rungs`).toBeGreaterThan(0);
    expect(up).toBe(OPAQUE_SURFACES[at + 1]);
    expect(down).toBe(OPAQUE_SURFACES[at - 1]);
  });

  // AND EACH STEP IS ONE A READER SEES, in every state of every theme: as far
  // off the ground as the palette holds that rung off the next (three ΔE for
  // raised over the card, 1.5 for the card over the sheet), and the two steps
  // never one colour.
  test("each step stands off the ground by the palette's own floor, in every theme", () => {
    const ground = paint(".diff-view__body");
    const up = paint('.diff-view__line[data-kind="add"]');
    const down = paint('.diff-view__line[data-kind="remove"]');
    const all = states();
    // A VACUITY FLOOR: every theme in three states.
    expect(all.length).toBe(themeRegistry.length * 3);
    for (const [state, values] of all) {
      const g = colour(values, ground);
      const u = colour(values, up);
      const d = colour(values, down);
      expect(deltaE(u, g), `${state}: an added line off the ground`).toBeGreaterThanOrEqual(
        floor(up, ground),
      );
      expect(deltaE(g, d), `${state}: a removed line off the ground`).toBeGreaterThanOrEqual(
        floor(ground, down),
      );
      expect(deltaE(u, d), `${state}: an added line off a removed one`).toBeGreaterThan(0);
    }
  });
});

/** The package's own diffs, which tint by state. */
const HUED_KIT_DIFFS = new Set(["DiffView", "DiffSummary", "DiffList"]);

/**
 * Where a module draws a diff in the state hues: an import of one of the
 * package's diffs, or a `VersionCompare` handed `before` and `after` (which it
 * draws as the package's `DiffView` and `DiffSummary`) or a spread that could
 * hand them unseen.
 */
export function huedDiffSites(path: string, text: string, lang: Lang): string[] {
  const line = lineOf(text);
  const out: string[] = [];
  walk(parse(text, lang), (node: Node) => {
    if (node.type === "ImportDeclaration" && (node.source as Node).value === "@crewlethq/ui") {
      for (const spec of node.specifiers as Node[]) {
        if (spec.type !== "ImportSpecifier") continue;
        const imported = spec.imported as Node;
        const name = String(imported.type === "Identifier" ? imported.name : imported.value);
        if (HUED_KIT_DIFFS.has(name)) out.push(`${path}:${line(spec.start)} imports ${name}`);
      }
      return false;
    }
    if (node.type !== "JSXOpeningElement") return;
    if ((node.name as Node & { name?: string }).name !== "VersionCompare") return;
    for (const attribute of node.attributes as Node[]) {
      if (attribute.type === "JSXSpreadAttribute") {
        out.push(`${path}:${line(attribute.start)} spreads into VersionCompare`);
        continue;
      }
      const name = (attribute.name as Node & { name?: string }).name;
      if (name === "before" || name === "after") {
        out.push(`${path}:${line(attribute.start)} hands VersionCompare \`${name}\``);
      }
    }
  });
  return out;
}

describe("in the tree", () => {
  test("every diff is the one DiffView: no hued diff of the package is drawn", () => {
    const sites = modules().flatMap((m) => huedDiffSites(m.path, m.text, m.lang));
    expect(
      sites,
      "draw it with components/DiffView.tsx, handed to VersionCompare as `changes`",
    ).toEqual([]);
  });

  test("and the scan reads the comparisons there are", () => {
    // A VACUITY FLOOR: the page's history, the editor's "their change" and an
    // artifact's versions each compare through `VersionCompare`.
    const comparing = modules().filter(
      (m) => m.lang === "tsx" && m.text.includes("<VersionCompare"),
    );
    expect(comparing.length).toBeGreaterThanOrEqual(3);
  });

  test("it can tell — the package's diff and the two texts are reported", () => {
    const old = [
      'import { Button, DiffView, collapseDiff } from "@crewlethq/ui";',
      'export const A = () => <VersionCompare fromLabel="a" toLabel="b" lens="changes" before={x} after={y} />;',
      'export const B = () => <VersionCompare {...compared} fromLabel="a" toLabel="b" lens="changes" />;',
      'export const C = () => <VersionCompare fromLabel="a" toLabel="b" lens="changes" changes={z} />;',
    ].join("\n");
    expect(huedDiffSites("old.tsx", old, "tsx")).toEqual([
      "old.tsx:1 imports DiffView",
      "old.tsx:2 hands VersionCompare `before`",
      "old.tsx:2 hands VersionCompare `after`",
      "old.tsx:3 spreads into VersionCompare",
    ]);
  });
});

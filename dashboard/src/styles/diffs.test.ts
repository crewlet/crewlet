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

import { KIT, kitModule, kitReaches, spelled } from "../test/kit.ts";
import { sheetRules, type SheetRule } from "../test/sheets.ts";
import {
  isLocalSource,
  lineOf,
  modules,
  parse,
  stringValue,
  walk,
  type Lang,
  type Node,
} from "../test/source.ts";

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
 * - IN THE TREE: nothing reaches the package's diffs — by name, by the module
 *   that draws them, through a namespace, a dynamic import or a re-export of a
 *   module the package says exports them — and nothing hands its
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
 * Whether a module of the package, as `kitModule` names it, is a diff's own:
 * the module named after one (`@crewlethq/ui/DiffView`, which draws the
 * summary too, and `@crewlethq/ui/DiffList`). Imported by its path, such a
 * module brings its diffs and their sheet whatever is taken from it, even
 * nothing.
 */
const ownDiffModule = (module: string | null) => module !== null && HUED_KIT_DIFFS.has(module);

/**
 * Whether a module of the package hands an importer one of the three, as its
 * built modules say in the release installed (`test/kit.ts`): the diffs' own,
 * and every module that hands the rest of the package on — its root and its
 * `generated` module today.
 */
const reachesDiffs = (module: string | null) => kitReaches(module, HUED_KIT_DIFFS);

/** A module of the package as an import spells it: the root, or `@crewlethq/ui/<module>`. */
const spelling = (module: string) => (module === "" ? KIT : `${KIT}/${module}`);

/** The identifier a JSX member expression starts from: `ui` in `<ui.Diff.View>`. */
function rootOf(name: Node): string {
  let at = name;
  while (at.type === "JSXMemberExpression") at = at.object as Node;
  return String(at.name);
}

/**
 * Where a module draws a diff in the state hues, by every way a module can
 * reach the package's three:
 *
 * - an import of one by name, from the package's root or from any module of
 *   it, under whatever local name;
 * - an import of a diff's own module (`@crewlethq/ui/DiffView`,
 *   `@crewlethq/ui/DiffList`), whatever it takes;
 * - a namespace import, a dynamic import or a whole re-export (`export *`) of
 *   any module that hands one on — the root, `@crewlethq/ui/generated`, a
 *   diff's own — each of which reaches them under names no gate can follow,
 *   and a JSX element naming one through a member expression (`<ui.DiffList>`)
 *   unless its object is a namespace of this tree's own modules;
 * - a re-export of one by name, or of a diff's own module, which hands them on
 *   to whatever imports this one;
 * - a `VersionCompare` handed `before` and `after` (which it draws as the
 *   package's `DiffView` and `DiffSummary`), or a spread that could hand them
 *   unseen, whether it is drawn by its own name, by the local name it was
 *   imported under, or through a member expression.
 */
export function huedDiffSites(path: string, text: string, lang: Lang): string[] {
  const line = lineOf(text);
  const out: string[] = [];
  const at = (node: Node) => `${path}:${line(node.start)}`;
  const program = parse(text, lang);
  // THE NAMES THIS MODULE GIVES THE PACKAGE'S PIECES, read before the walk,
  // since a module's imports bind before any of its code runs.
  const compares = new Set(["VersionCompare"]);
  const ownNamespaces = new Set<string>();
  for (const node of program.body as Node[]) {
    if (node.type !== "ImportDeclaration") continue;
    const source = (node.source as Node).value;
    for (const spec of node.specifiers as Node[]) {
      const local = String((spec.local as Node).name);
      if (spec.type === "ImportNamespaceSpecifier" && isLocalSource(String(source))) {
        ownNamespaces.add(local);
      }
      if (
        spec.type === "ImportSpecifier" &&
        kitModule(source) !== null &&
        spelled(spec.imported as Node) === "VersionCompare"
      ) {
        compares.add(local);
      }
    }
  }
  walk(program, (node: Node) => {
    switch (node.type) {
      case "ImportDeclaration": {
        const module = kitModule((node.source as Node).value);
        if (module === null) return false;
        const own = ownDiffModule(module);
        if (own) out.push(`${at(node)} imports ${spelling(module)}`);
        for (const spec of node.specifiers as Node[]) {
          if (spec.type === "ImportSpecifier") {
            const name = spelled(spec.imported as Node);
            if (HUED_KIT_DIFFS.has(name)) out.push(`${at(spec)} imports ${name}`);
          } else if (spec.type === "ImportNamespaceSpecifier" && !own && reachesDiffs(module)) {
            const local = String((spec.local as Node).name);
            out.push(`${at(spec)} imports ${spelling(module)} whole, as ${local}`);
          }
        }
        return false;
      }
      case "ImportExpression": {
        const module = kitModule(stringValue(node.source as Node));
        if (module !== null && reachesDiffs(module)) {
          out.push(`${at(node)} imports ${spelling(module)} at run time`);
        }
        return;
      }
      case "ExportNamedDeclaration": {
        const module = kitModule((node.source as Node | null)?.value);
        if (module === null) return;
        if (ownDiffModule(module)) out.push(`${at(node)} re-exports ${spelling(module)}`);
        for (const spec of node.specifiers as Node[]) {
          const name = spelled(spec.local as Node);
          if (HUED_KIT_DIFFS.has(name)) out.push(`${at(spec)} re-exports ${name}`);
        }
        return false;
      }
      case "ExportAllDeclaration": {
        const module = kitModule((node.source as Node).value);
        if (module !== null && reachesDiffs(module)) {
          out.push(`${at(node)} re-exports ${spelling(module)} whole`);
        }
        return false;
      }
      case "JSXOpeningElement": {
        const name = node.name as Node;
        let drawn: string;
        if (name.type === "JSXMemberExpression") {
          drawn = String((name.property as Node).name);
          if (HUED_KIT_DIFFS.has(drawn) && !ownNamespaces.has(rootOf(name))) {
            out.push(`${at(node)} draws ${rootOf(name)}.${drawn}`);
          }
          if (drawn !== "VersionCompare") return;
        } else {
          drawn = String(name.name);
          if (!compares.has(drawn)) return;
        }
        for (const attribute of node.attributes as Node[]) {
          if (attribute.type === "JSXSpreadAttribute") {
            out.push(`${at(attribute)} spreads into VersionCompare`);
            continue;
          }
          const handed = (attribute.name as Node & { name?: string }).name;
          if (handed === "before" || handed === "after") {
            out.push(`${at(attribute)} hands VersionCompare \`${handed}\``);
          }
        }
        return;
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

  // EVERY WAY ROUND THE NAMED IMPORT is reported too: the package exports
  // each of its modules by path (`./*`), and a namespace, a dynamic import, a
  // re-export or an alias reaches the same three diffs under names the plain
  // check never reads — through the root, and through `generated`, which
  // hands on every module as the root does. Each line below is one way, but
  // the import naming the `Compare` after it and the last four, which are
  // not: the tree's own DiffView through a namespace, and a module of the
  // package that hands no diff on, imported and re-exported whole.
  test("it can tell — every other way to the package's diffs is reported", () => {
    const ways = [
      'import { DiffList } from "@crewlethq/ui/DiffList";',
      'import { DiffView as Kit } from "@crewlethq/ui/DiffView";',
      'import "@crewlethq/ui/DiffView";',
      'import * as ui from "@crewlethq/ui";',
      "export const D = () => <ui.DiffSummary added={1} removed={0} />;",
      'export const L = async () => (await import("@crewlethq/ui")).DiffList;',
      'export const V = async () => (await import("@crewlethq/ui/DiffView")).DiffView;',
      'export { DiffList as List } from "@crewlethq/ui";',
      'export { DiffSummary } from "@crewlethq/ui/DiffView";',
      'export * from "@crewlethq/ui";',
      'import * as all from "@crewlethq/ui/generated";',
      'export const H = async () => (await import("@crewlethq/ui/generated")).DiffView;',
      'export * from "@crewlethq/ui/generated";',
      'import { VersionCompare as Compare } from "@crewlethq/ui";',
      'export const E = () => <Compare lens="changes" before={x} after={y} />;',
      "export const F = () => <ui.VersionCompare {...compared} />;",
      'import * as own from "~/components/DiffView.tsx";',
      'export const G = () => <own.DiffView before="a" after="b" />;',
      'import * as buttons from "@crewlethq/ui/Button";',
      'export * from "@crewlethq/ui/Button";',
    ].join("\n");
    expect(huedDiffSites("ways.tsx", ways, "tsx")).toEqual([
      "ways.tsx:1 imports @crewlethq/ui/DiffList",
      "ways.tsx:1 imports DiffList",
      "ways.tsx:2 imports @crewlethq/ui/DiffView",
      "ways.tsx:2 imports DiffView",
      "ways.tsx:3 imports @crewlethq/ui/DiffView",
      "ways.tsx:4 imports @crewlethq/ui whole, as ui",
      "ways.tsx:5 draws ui.DiffSummary",
      "ways.tsx:6 imports @crewlethq/ui at run time",
      "ways.tsx:7 imports @crewlethq/ui/DiffView at run time",
      "ways.tsx:8 re-exports DiffList",
      "ways.tsx:9 re-exports @crewlethq/ui/DiffView",
      "ways.tsx:9 re-exports DiffSummary",
      "ways.tsx:10 re-exports @crewlethq/ui whole",
      "ways.tsx:11 imports @crewlethq/ui/generated whole, as all",
      "ways.tsx:12 imports @crewlethq/ui/generated at run time",
      "ways.tsx:13 re-exports @crewlethq/ui/generated whole",
      "ways.tsx:15 hands VersionCompare `before`",
      "ways.tsx:15 hands VersionCompare `after`",
      "ways.tsx:16 spreads into VersionCompare",
    ]);
  });
});

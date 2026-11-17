// @vitest-environment node
import { readFileSync } from "node:fs";
import { createRequire } from "node:module";
import { describe, expect, test } from "vitest";
import { DEFAULT_THEME_ID, themeRegistry } from "@crewlethq/tokens";
import {
  CODE_GROUNDS,
  CODE_PICK,
  DIFF_HUNK,
  DIFF_ROWS,
  OPAQUE_SURFACES,
  RUNG_STEPS,
  deltaE,
  paletteStates,
  parseHex,
  type Rgb,
} from "@crewlethq/tokens/test/palette";

import { KIT, kitModule, kitReaches, spelled } from "../test/kit.ts";
import { declared, sheetRules, splitSelectors, type SheetRule } from "../test/sheets.ts";
import {
  classWrites,
  isLocalSource,
  lineOf,
  modules,
  parse,
  resolveSource,
  stringValue,
  walk,
  type Lang,
  type Node,
} from "../test/source.ts";

/**
 * A DIFF CARRIES NO HUE OUTSIDE GITLET
 * (`dashboard-design.md#a-diff-carries-no-hue-outside-gitlet`).
 *
 * Colour carries STATE here, and a diff is not one: a removed line is not a
 * failure and an added one is not done. The design system's three diffs —
 * `DiffView`, `DiffSummary`, `DiffList` — tint an added line with the success
 * state and a removed one with the danger state wherever they are drawn, and
 * so did this tree's own configuration diff. On the page that draws a change,
 * a red failed-check chip beside red removed lines makes one red mean two
 * things. So every diff is `components/DiffView.tsx`, and this file holds the
 * rule four ways:
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
 * - IN GITLET: only `.diff-view--code` reads the diff colours, every Gitlet
 *   diff passes `hue="code"` and `tokens` and no other passes either, no rule
 *   colours code inside a changed run, and the steps it paints are the kit's measured ones.
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

/** The diff family's tokens. */
const DIFF_FAMILY = /--color-diff-[\w-]+/g;

/**
 * The rules outside a diff that may read its sign inks, the step diff.css
 * draws a line's sign in: a diffstat's figures and squares, and a file tree's counts.
 */
export const DIFFSTAT_READERS: readonly string[] = [
  ".code-diffstat__plus",
  ".code-diffstat__minus",
  '.code-diffstat__squares > [data-kind="added"]',
  '.code-diffstat__squares > [data-kind="removed"]',
  '.code-ftree__counts > [data-sign="+"]',
  '.code-ftree__counts > [data-sign="−"]',
];

/** The steps a diffstat reads. */
const DIFFSTAT_STEP = /^--color-diff-(?:added|removed)-sign$/;

/**
 * Every rule reading the diff family that is not wholly a coloured diff's in
 * `diff.css`, nor a diffstat's step in `gitlet.css`, as `file selector -> token`.
 */
export function uncodedDiffRules(rules: readonly SheetRule[]): string[] {
  return rules.flatMap((rule) => {
    const selectors = splitSelectors(rule.selector);
    const coded =
      rule.file === "diff.css" && selectors.every((s) => /^\.diff-view--code(?![\w-])/.test(s));
    const stat = rule.file === "gitlet.css" && selectors.every((s) => DIFFSTAT_READERS.includes(s));
    return coded
      ? []
      : [...rule.body.matchAll(DIFF_FAMILY)]
          .filter((m) => !(stat && DIFFSTAT_STEP.test(m[0])))
          .map((m) => `${rule.file} ${rule.selector} -> ${m[0]}`);
  });
}

/**
 * Every rule colouring code inside a changed run, as `file selector`: the kit
 * measures a changed word in the code's own text only.
 */
export function runSyntaxRules(rules: readonly SheetRule[]): string[] {
  return rules
    .filter((rule) =>
      splitSelectors(rule.selector).some(
        (s) => /\.diff-view__run(?![\w-])/.test(s) && /\.code-syn-/.test(s),
      ),
    )
    .map((rule) => `${rule.file} ${rule.selector}`);
}

/** Every rule that takes a changed run's underline away, as `file selector`. */
export function runUnderlineRemovals(rules: readonly SheetRule[]): string[] {
  return rules
    .filter(
      (rule) =>
        /\.diff-view__run(?![\w-])/.test(rule.selector) &&
        /(?:^|[;\s])text-decoration(?:-line)?\s*:\s*(?:none|initial|unset)\b/.test(rule.body),
    )
    .map((rule) => `${rule.file} ${rule.selector}`);
}

describe("in Gitlet's sheets", () => {
  test("only a coloured diff's rules read the diff colours", () => {
    const reading = sheetRules().filter((rule) => /--color-diff-/.test(rule.body));
    // A VACUITY FLOOR: the two kinds' rows, signs, numbers and runs, and the hunk.
    expect(reading.length).toBeGreaterThanOrEqual(8);
    expect(uncodedDiffRules(sheetRules())).toEqual([]);
  });

  test("it can tell: the diff colours read outside a coloured diff are reported", () => {
    const rule = (file: string, selector: string, body: string): SheetRule => ({
      file,
      selector,
      body,
    });
    expect(
      uncodedDiffRules([
        rule(
          "diff.css",
          '.diff-view__line[data-kind="add"]',
          "background: var(--color-diff-added-row);",
        ),
        rule("gitlet.css", ".code-ftree__status", "color: var(--color-diff-added-sign);"),
        rule("diff.css", ".diff-view--code .x, .y", "color: var(--color-diff-removed-sign);"),
        rule(
          "diff.css",
          '.diff-view--code .diff-view__line[data-kind="add"]',
          "background: var(--color-diff-added-row);",
        ),
      ]),
    ).toEqual([
      'diff.css .diff-view__line[data-kind="add"] -> --color-diff-added-row',
      "gitlet.css .code-ftree__status -> --color-diff-added-sign",
      "diff.css .diff-view--code .x, .y -> --color-diff-removed-sign",
    ]);
  });

  // TWO-SIDED: every diffstat reader is a rule that reads a step, so the list holds no stale name.
  test("every diffstat reader named reads a sign ink", () => {
    const reading = sheetRules().filter(
      (rule) => rule.file === "gitlet.css" && /--color-diff-/.test(rule.body),
    );
    const named = new Set(reading.flatMap((rule) => splitSelectors(rule.selector)));
    expect(DIFFSTAT_READERS.filter((s) => !named.has(s))).toEqual([]);
  });

  test("it can tell: another rule reading a sign, or a diffstat reading a row or mark, is reported", () => {
    expect(
      uncodedDiffRules([
        {
          file: "gitlet.css",
          selector: ".code-ftree__name",
          body: "color: var(--color-diff-added-sign);",
        },
        {
          file: "gitlet.css",
          selector: ".code-diffstat__plus",
          body: "background: var(--color-diff-added-row);",
        },
        {
          file: "gitlet.css",
          selector: '.code-diffstat__squares > [data-kind="added"]',
          body: "background: var(--color-diff-added-mark);",
        },
        {
          file: "gitlet.css",
          selector: ".code-diffstat__plus",
          body: "color: var(--color-diff-added-sign);",
        },
      ]),
    ).toEqual([
      "gitlet.css .code-ftree__name -> --color-diff-added-sign",
      "gitlet.css .code-diffstat__plus -> --color-diff-added-row",
      'gitlet.css .code-diffstat__squares > [data-kind="added"] -> --color-diff-added-mark',
    ]);
  });

  test("nothing takes a changed run's underline away", () => {
    const runs = sheetRules().filter((rule) => /\.diff-view__run(?![\w-])/.test(rule.selector));
    // A VACUITY FLOOR: the base run and the two coloured kinds'.
    expect(runs.length).toBeGreaterThanOrEqual(3);
    expect(runUnderlineRemovals(sheetRules())).toEqual([]);
  });

  test("no rule colours code inside a changed run", () => {
    expect(runSyntaxRules(sheetRules())).toEqual([]);
  });

  test("it can tell: code coloured inside a changed run is reported", () => {
    expect(
      runSyntaxRules([
        {
          file: "gitlet-code.css",
          selector: ".diff-view__run .code-syn-keyword",
          body: "color: var(--color-syntax-keyword);",
        },
        {
          file: "gitlet-code.css",
          selector: ".code-syn-keyword, .diff-view--code .diff-view__run .code-syn-type",
          body: "color: var(--color-syntax-type);",
        },
        { file: "gitlet-code.css", selector: ".code-syn-keyword", body: "color: red;" },
        { file: "diff.css", selector: ".diff-view__run", body: "color: inherit;" },
      ]),
    ).toEqual([
      "gitlet-code.css .diff-view__run .code-syn-keyword",
      "gitlet-code.css .code-syn-keyword, .diff-view--code .diff-view__run .code-syn-type",
    ]);
  });

  test("it can tell: a run's underline taken away is reported", () => {
    const run = '.diff-view--code .diff-view__line[data-kind="add"] .diff-view__run';
    const tint = "background: var(--color-diff-added-word);";
    expect(
      runUnderlineRemovals([
        { file: "diff.css", selector: run, body: `${tint} text-decoration-line: none;` },
        { file: "diff.css", selector: run, body: `${tint} text-decoration: none;` },
        { file: "diff.css", selector: ".diff-view__run", body: "text-decoration-line: underline;" },
      ]),
    ).toEqual([`diff.css ${run}`, `diff.css ${run}`]);
  });
});

/**
 * Every rule ruling a picked line by a shadow alone, as `file selector`: forced
 * colours drop every shadow and paint an outline, so a pick needs one.
 */
export function unoutlinedPicks(rules: readonly SheetRule[]): string[] {
  return rules
    .filter(
      (rule) =>
        rule.selector.includes("[data-picked]") &&
        /(?:^|[;\s])box-shadow\s*:/.test(rule.body) &&
        !/(?:^|[;\s])outline\s*:(?!\s*(?:none\b|0(?:px)?\s*(?:;|$)))/.test(rule.body),
    )
    .map((rule) => `${rule.file} ${rule.selector}`);
}

describe("a picked line", () => {
  test("a pick ruled by a shadow carries an outline, which forced colours paint", () => {
    const ruled = sheetRules().filter(
      (rule) => rule.selector.includes("[data-picked]") && /box-shadow\s*:/.test(rule.body),
    );
    // A VACUITY FLOOR: the diff's pick and the file view's.
    expect(ruled.length).toBeGreaterThanOrEqual(2);
    expect(unoutlinedPicks(sheetRules())).toEqual([]);
  });

  test("it can tell: a pick ruled by a shadow alone is reported", () => {
    const shadow = "box-shadow: inset 2px 0 0 var(--color-border-strong);";
    const pick = (body: string): SheetRule => ({
      file: "diff.css",
      selector: ".diff-view__line[data-picked]",
      body,
    });
    expect(unoutlinedPicks([pick(shadow)])).toEqual(["diff.css .diff-view__line[data-picked]"]);
    expect(unoutlinedPicks([pick(`${shadow} outline: 2px solid transparent;`)])).toEqual([]);
    expect(unoutlinedPicks([pick(`${shadow} outline: none;`)])).toEqual([
      "diff.css .diff-view__line[data-picked]",
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

/** A coloured diff's line of one kind, as its rules select it. */
const codedLine = (kind: string) =>
  `.diff-view--code .diff-view__line[data-kind="${kind === "added" ? "add" : "remove"}"]`;

/** The coloured diff's rules that read the diff colours: each kind's row, sign, number and run, and the skip. */
const CODED_STEPS = [
  ...DIFF_ROWS.flatMap(({ kind }) => [
    codedLine(kind),
    `${codedLine(kind)} .diff-view__mark`,
    `${codedLine(kind)}:not([data-picked]) .diff-view__number`,
    `${codedLine(kind)} .diff-view__run`,
  ]),
  ".diff-view--code .diff-view__skip",
];

/** Every `diff.css` rule reading the diff colours that is not one of `CODED_STEPS`, as `file selector`. */
export function unlistedDiffPaints(rules: readonly SheetRule[]): string[] {
  return rules
    .filter((r) => r.file === "diff.css" && /--color-diff-/.test(r.body))
    .filter((r) => !CODED_STEPS.includes(r.selector))
    .map((r) => `${r.file} ${r.selector}`);
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

  // THE COLOURED DIFF PAINTS WHAT THE PALETTE MEASURES: the family's steps,
  // over a ground the kit measures them on. A step drawn elsewhere is unmeasured.
  test("a coloured diff paints the kit's measured steps, on a ground they are measured on", () => {
    expect(CODE_GROUNDS).toContain(paint(".diff-view__body"));
    for (const { kind, row, gutter, word, sign } of DIFF_ROWS) {
      const line = codedLine(kind);
      expect(declared(line, "background")).toBe(row);
      expect(declared(line, "box-shadow")).toBe(gutter);
      expect(declared(`${line} .diff-view__mark`, "color")).toBe(sign);
      expect(declared(`${line}:not([data-picked]) .diff-view__number`, "color")).toBe(sign);
      expect(declared(`${line} .diff-view__run`, "background")).toBe(word);
    }
    expect(declared(".diff-view--code .diff-view__skip", "background")).toBe(DIFF_HUNK.row);
    expect(
      declared(".diff-view--code .diff-view__line[data-kind][data-picked]", "background"),
    ).toBe(CODE_PICK);
    expect(CODED_STEPS).toHaveLength(9);
    expect(unlistedDiffPaints(sheetRules())).toEqual([]);
  });

  test("it can tell: a coloured diff painting a step outside those it declares", () => {
    const rule = (selector: string, body: string): SheetRule => ({
      file: "diff.css",
      selector,
      body,
    });
    expect(
      unlistedDiffPaints([
        rule(".diff-view--code .diff-view__text", "background: var(--color-diff-added-mark);"),
        rule(`${codedLine("added")} .diff-view__run`, "background: var(--color-diff-added-word);"),
        rule(".diff-view--code .diff-view__skip", "background: var(--color-diff-hunk-row);"),
      ]),
    ).toEqual(["diff.css .diff-view--code .diff-view__text"]);
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

/** Every Gitlet module drawing a DiffView, each with `hue="code"` and `tokens`: two-sided. */
const CODE_DIFFS: { file: string; why: string }[] = [
  {
    file: "routes/gitlet/change/Files.tsx",
    why: "A change's files, each read in its revision's diff, with its findings.",
  },
  {
    file: "routes/gitlet/change/Finding.tsx",
    why: "A finding's lines as its revision's diff marks them, and its suggestion as a small diff.",
  },
  {
    file: "routes/gitlet/commit/DiffFiles.tsx",
    why: "A commit's files and a comparison's, each read in its diff.",
  },
];

const DIFF_MODULE = "components/DiffView";

/** Whether a module sits in the Gitlet workspace. */
const inGitlet = (path: string) => path.startsWith("routes/gitlet/");

/** Each JSX element of a module that draws the tree's DiffView, by any local name. */
function diffElements(path: string, text: string, lang: Lang): Node[] {
  const program = parse(text, lang);
  const names = new Set<string>();
  const spaces = new Set<string>();
  for (const node of program.body as Node[]) {
    if (node.type !== "ImportDeclaration") continue;
    const to = resolveSource(path, String((node.source as Node).value));
    if (to?.replace(/\.tsx?$/, "") !== DIFF_MODULE) continue;
    for (const spec of node.specifiers as Node[]) {
      const local = String((spec.local as Node).name);
      if (spec.type === "ImportNamespaceSpecifier") spaces.add(local);
      else if (spec.type === "ImportSpecifier" && spelled(spec.imported as Node) === "DiffView") {
        names.add(local);
      }
    }
  }
  const out: Node[] = [];
  walk(program, (node: Node) => {
    if (node.type !== "JSXOpeningElement") return;
    const name = node.name as Node;
    const drawn =
      name.type === "JSXIdentifier"
        ? names.has(String(name.name))
        : name.type === "JSXMemberExpression" &&
          (name.object as Node).type === "JSXIdentifier" &&
          spaces.has(String((name.object as Node).name)) &&
          (name.property as Node).name === "DiffView";
    if (drawn) out.push(node);
  });
  return out;
}

/** Whether a module draws the tree's DiffView. */
export function drawsDiffView(path: string, text: string, lang: Lang): boolean {
  return diffElements(path, text, lang).length > 0;
}

/**
 * Where a module draws the tree's DiffView against the rule: in Gitlet without
 * `hue="code"` or tokens, outside it with either, or anywhere through a spread.
 */
export function codeSites(path: string, text: string, lang: Lang): string[] {
  const line = lineOf(text);
  const out: string[] = [];
  for (const element of diffElements(path, text, lang)) {
    const at = `${path}:${line(element.start)}`;
    let hue: Node | null = null;
    let tokens: Node | null = null;
    for (const attribute of element.attributes as Node[]) {
      if (attribute.type === "JSXSpreadAttribute") out.push(`${at} spreads into DiffView`);
      else if ((attribute.name as Node).name === "hue") hue = attribute;
      else if ((attribute.name as Node).name === "tokens") tokens = attribute;
    }
    if (!inGitlet(path)) {
      if (hue) out.push(`${at} passes hue to DiffView`);
      if (tokens) out.push(`${at} passes tokens to DiffView`);
      continue;
    }
    if (!tokens) out.push(`${at} draws DiffView without tokens`);
    if (!hue) {
      out.push(`${at} draws DiffView without hue="code"`);
      continue;
    }
    const value = hue.value as Node | null;
    const given =
      value?.type === "JSXExpressionContainer"
        ? stringValue(value.expression as Node)
        : stringValue(value ?? undefined);
    if (given !== "code") out.push(`${at} passes a hue other than "code"`);
  }
  return out;
}

describe("in Gitlet's tree", () => {
  test('every Gitlet diff passes hue="code" and tokens, and no other diff passes either', () => {
    expect(modules().flatMap((m) => codeSites(m.path, m.text, m.lang))).toEqual([]);
  });

  test("every Gitlet module that draws a DiffView is listed, and every listed one still draws one", () => {
    const drawing = modules()
      .filter((m) => inGitlet(m.path) && drawsDiffView(m.path, m.text, m.lang))
      .map((m) => m.path);
    expect(drawing).toEqual(CODE_DIFFS.map((d) => d.file).sort());
    for (const { file, why } of CODE_DIFFS) expect(why.length, file).toBeGreaterThan(40);
  });

  test("only DiffView writes its code hue's class", () => {
    for (const m of modules()) {
      const writes = classWrites(m, "diff-view--code");
      if (m.path === "components/DiffView.tsx") expect(writes, m.path).not.toEqual([]);
      else expect(writes, m.path).toEqual([]);
    }
  });

  test("it can tell: a hue or tokens outside Gitlet, a spread, a Gitlet diff without either and a hand-written class are reported", () => {
    const named = 'import { DiffView } from "~/components/DiffView.tsx";';
    expect(
      codeSites(
        "routes/settings/Config.tsx",
        `${named}\nconst A = () => <DiffView hunks={h} tokens={t} />;`,
        "tsx",
      ),
    ).toEqual(["routes/settings/Config.tsx:2 passes tokens to DiffView"]);
    expect(
      codeSites(
        "routes/settings/Config.tsx",
        `${named}\nconst A = () => <DiffView hunks={h} hue="code" />;`,
        "tsx",
      ),
    ).toEqual(["routes/settings/Config.tsx:2 passes hue to DiffView"]);
    expect(
      codeSites(
        "routes/doclet/X.tsx",
        'import * as d from "../../components/DiffView.tsx";\nconst A = () => <d.DiffView hue="code" />;',
        "tsx",
      ),
    ).toEqual(["routes/doclet/X.tsx:2 passes hue to DiffView"]);
    expect(
      codeSites(
        "routes/doclet/X.tsx",
        'import { DiffView as Diff } from "~/components/DiffView.tsx";\nconst A = () => <Diff {...p} />;',
        "tsx",
      ),
    ).toEqual(["routes/doclet/X.tsx:2 spreads into DiffView"]);
    const files = "routes/gitlet/change/Files.tsx";
    expect(codeSites(files, `${named}\nconst A = () => <DiffView hunks={h} />;`, "tsx")).toEqual([
      `${files}:2 draws DiffView without tokens`,
      `${files}:2 draws DiffView without hue="code"`,
    ]);
    expect(
      codeSites(files, `${named}\nconst A = () => <DiffView hue={pick} tokens={t} />;`, "tsx"),
    ).toEqual([`${files}:2 passes a hue other than "code"`]);
    expect(
      codeSites(files, `${named}\nconst A = () => <DiffView hunks={h} hue="code" />;`, "tsx"),
    ).toEqual([`${files}:2 draws DiffView without tokens`]);
    expect(
      codeSites(files, `${named}\nconst A = () => <DiffView hunks={h} tokens={t} />;`, "tsx"),
    ).toEqual([`${files}:2 draws DiffView without hue="code"`]);
    const talk = "routes/gitlet/change/Conversation.tsx";
    expect(
      codeSites(talk, `${named}\nconst A = () => <DiffView hunks={h} tokens={t} />;`, "tsx"),
    ).toEqual([`${talk}:2 draws DiffView without hue="code"`]);
    expect(
      codeSites(
        "routes/gitlet/commit/DiffFiles.tsx",
        `${named}\nconst A = () => <DiffView hunks={h} hue="code" tokens={t} />;`,
        "tsx",
      ),
    ).toEqual([]);
    const written = {
      path: files,
      text: 'const c = cx("diff-view--code", x);',
      lang: "tsx" as const,
    };
    expect(classWrites(written, "diff-view--code")).toEqual([1]);
  });
});

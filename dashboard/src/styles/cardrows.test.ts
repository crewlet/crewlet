// @vitest-environment node
import { expect, test } from "vitest";

import { sheetRules, splitSelectors, type SheetRule } from "../test/sheets.ts";
import {
  lineOf,
  modules,
  parse,
  resolveSource,
  stringValue,
  walk,
  type Module,
  type Node,
} from "../test/source.ts";

/**
 * Cards side by side end at one height. A row of kit cards is laid out by the
 * kit's CardGrid; a local grid or flex row that holds them may not align them
 * to the start, and no sheet may move a card off its row's height.
 */

/** The kit's card surfaces, by export name. */
const KIT_CARDS = new Set([
  "Card",
  "StatCard",
  "StatusCard",
  "LinkCard",
  "ActionCard",
  "CatalogTile",
  "PinTile",
  "FolderTile",
  "ArtifactCard",
  "WidgetFrame",
  "PricingCard",
  "BriefCard",
  "ProposalCard",
]);

const UNEVEN =
  /(?:^|[;\s])align-items\s*:\s*(start|flex-start|end|flex-end|center|baseline|self-start|self-end)\b/;
const OFF_ROW =
  /(?:^|[;\s])align-self\s*:\s*(start|flex-start|end|flex-end|center|baseline|self-start|self-end)\b/;

interface Scope {
  module: Module;
  /** Local name to the kit export it imports. */
  kit: Map<string, string>;
  /** Local name to the module and export it imports. */
  imports: Map<string, { path: string; name: string }>;
  /** The component functions this module defines, by name. */
  defined: Map<string, Node>;
}

const byPath = new Map(modules().map((m) => [m.path, m]));
const scopes = new Map<string, Scope>();

function scopeOf(module: Module): Scope {
  const known = scopes.get(module.path);
  if (known) return known;
  const scope: Scope = { module, kit: new Map(), imports: new Map(), defined: new Map() };
  for (const statement of parse(module.text, module.lang).body as Node[]) {
    if (statement.type === "ImportDeclaration") {
      const source = String((statement.source as Node).value);
      const path = resolveSource(module.path, source);
      for (const spec of statement.specifiers as Node[]) {
        if (spec.type !== "ImportSpecifier") continue;
        const local = String((spec.local as Node).name);
        const imported = String((spec.imported as Node).name ?? (spec.imported as Node).value);
        if (source === "@crewlethq/ui") scope.kit.set(local, imported);
        else if (path)
          scope.imports.set(local, { path: path.replace(/\.tsx?$/, ""), name: imported });
      }
      continue;
    }
    const declaration =
      statement.type === "ExportNamedDeclaration"
        ? (statement.declaration as Node | null)
        : statement;
    if (!declaration) continue;
    if (declaration.type === "FunctionDeclaration" && declaration.id)
      scope.defined.set(String((declaration.id as Node).name), declaration);
    if (declaration.type === "VariableDeclaration")
      for (const d of declaration.declarations as Node[]) {
        const init = d.init as Node | null;
        if (init && /FunctionExpression$/.test(init.type))
          scope.defined.set(String((d.id as Node).name), init);
      }
  }
  scopes.set(module.path, scope);
  return scope;
}

function moduleAt(path: string): Module | undefined {
  return byPath.get(`${path}.tsx`) ?? byPath.get(`${path}.ts`) ?? byPath.get(`${path}/index.tsx`);
}

/** The JSX a component function returns, from its own body only. */
function returned(fn: Node): Node[] {
  const body = fn.body as Node;
  if (body.type !== "BlockStatement") return [body];
  const out: Node[] = [];
  walk(body, (node) => {
    if (node !== body && /Function/.test(node.type)) return false;
    if (node.type === "ReturnStatement" && node.argument) out.push(node.argument as Node);
  });
  return out;
}

const deciding = new Set<string>();
const decided = new Map<string, boolean>();

/** Whether the component `name`, as `scope` sees it, draws a card at its root. */
function drawsACard(scope: Scope, name: string): boolean {
  const kit = scope.kit.get(name);
  if (kit) return KIT_CARDS.has(kit);
  let home = scope;
  let local = name;
  const imported = scope.imports.get(name);
  if (imported) {
    const module = moduleAt(imported.path);
    if (!module || module.lang !== "tsx") return false;
    home = scopeOf(module);
    local = imported.name;
    if (home.kit.has(local) || home.imports.has(local)) return drawsACard(home, local);
  }
  const key = `${home.module.path}#${local}`;
  if (decided.has(key)) return decided.get(key)!;
  const fn = home.defined.get(local);
  if (!fn || deciding.has(key)) return false;
  deciding.add(key);
  const answer = returned(fn).some((node) => cardsIn(home, node) > 0);
  deciding.delete(key);
  decided.set(key, answer);
  return answer;
}

const tagOf = (el: Node) => (el.openingElement as Node).name as Node;

/** How many cards `node` draws at its top: a list drawn by `.map` counts as two. */
function cardsIn(scope: Scope, node: Node | null | undefined): number {
  if (!node) return 0;
  switch (node.type) {
    case "JSXElement": {
      const tag = tagOf(node);
      if (tag.type !== "JSXIdentifier") return 0;
      const name = String(tag.name);
      if (/^[A-Z]/.test(name)) return drawsACard(scope, name) ? 1 : 0;
      // A wrapper of one card (a focus target, a list item) stands for its card.
      const kids = (node.children as Node[]).filter(
        (c) => c.type !== "JSXText" || String(c.value).trim(),
      );
      return kids.length === 1 && cardsIn(scope, kids[0]) === 1 ? 1 : 0;
    }
    case "JSXFragment":
      return (node.children as Node[]).reduce((n, c) => n + cardsIn(scope, c), 0);
    case "JSXExpressionContainer":
      return cardsIn(scope, node.expression as Node);
    case "ConditionalExpression":
      return Math.max(
        cardsIn(scope, node.consequent as Node),
        cardsIn(scope, node.alternate as Node),
      );
    case "LogicalExpression":
      return cardsIn(scope, node.right as Node);
    case "ParenthesizedExpression":
      return cardsIn(scope, node.expression as Node);
    case "CallExpression": {
      const callee = node.callee as Node;
      const fn = (node.arguments as Node[])[0];
      if (callee.type !== "MemberExpression" || !fn || !/Function/.test(fn.type)) return 0;
      return returned(fn).some((r) => cardsIn(scope, r) > 0) ? 2 : 0;
    }
    default:
      return 0;
  }
}

/** The class names written into an element's `className`, literal parts only. */
function classesOf(el: Node): string[] {
  const attr = ((el.openingElement as Node).attributes as Node[]).find(
    (a) => a.type === "JSXAttribute" && (a.name as Node).name === "className",
  );
  if (!attr?.value) return [];
  const out: string[] = [];
  walk(attr.value as Node, (node) => {
    const text = stringValue(node);
    if (text) out.push(...text.split(/\s+/).filter(Boolean));
  });
  return out;
}

/** The rules whose selector is the class itself, in any at-rule or with an attribute. */
function rulesFor(cls: string, rules: SheetRule[]): SheetRule[] {
  const own = new RegExp(`^\\.${cls.replace(/[-_]/g, "\\$&")}(?:\\[[^\\]]*\\])*$`);
  return rules.filter((r) => splitSelectors(r.selector).some((s) => own.test(s)));
}

/** A sheet sticks a pane inside the element: a list's detail keeps to its row's top to stick. */
const holdsASticky = (classes: string[], rules: SheetRule[]) =>
  rules.some(
    (r) =>
      /position\s*:\s*sticky/.test(r.body) &&
      splitSelectors(r.selector).some((s) => classes.some((c) => s.startsWith(`.${c} `))),
  );

const laysOutARow = (body: string) =>
  /display\s*:\s*(inline-)?grid/.test(body) ||
  (/display\s*:\s*(inline-)?flex/.test(body) && !/flex-direction\s*:\s*column/.test(body));

test("no row of kit cards is aligned off its height", () => {
  const rules = sheetRules();
  const found: string[] = [];
  for (const module of modules().filter((m) => m.lang === "tsx")) {
    const scope = scopeOf(module);
    const line = lineOf(module.text);
    walk(parse(module.text, module.lang), (node) => {
      if (node.type !== "JSXElement") return;
      const cards = (node.children as Node[]).reduce((n, c) => n + cardsIn(scope, c), 0);
      const classes = classesOf(node);
      if (cards < 2 || holdsASticky(classes, rules)) return;
      for (const cls of classes) {
        const own = rulesFor(cls, rules);
        if (!own.some((r) => laysOutARow(r.body))) continue;
        for (const rule of own.filter((r) => UNEVEN.test(r.body)))
          found.push(`${module.path}:${line(node.start)} .${cls} (${rule.file})`);
      }
    });
  }
  expect(found, "lay cards side by side in the kit's CardGrid").toEqual([]);
});

test("no sheet moves a kit card or a CardGrid cell off its row's height", () => {
  const found = sheetRules()
    .filter((r) => /\.crewlet-card(?:-grid)?\b/.test(r.selector))
    .filter(
      (r) =>
        OFF_ROW.test(r.body) || (/\.crewlet-card-grid\b/.test(r.selector) && UNEVEN.test(r.body)),
    )
    .map((r) => `${r.file}: ${r.selector}`);
  expect(found).toEqual([]);
});

// The finder sees a card through a wrapper element and through an import.
test("the finder reads a component's card through a wrapper and an import", () => {
  const entry = moduleAt("routes/settings/models/EntryPage")!;
  const scope = scopeOf(entry);
  expect(drawsACard(scope, "SignInCard")).toBe(true);
  expect(drawsACard(scope, "TestCard")).toBe(true);
  expect(drawsACard(scope, "UsageLimitsCard")).toBe(true);
  expect(drawsACard(scope, "HarnessMark")).toBe(false);
});

/**
 * The dashboard's own source, read as a syntax tree: the one walk of its
 * modules that every suite reading them shares.
 *
 * PARSED, NEVER GREPPED. The parser is Vite's own (`parseAst`, the one the
 * bundle is built with), so a gate reads exactly what ships: a comment that
 * explains a rule is free to name what the rule forbids, a string that says
 * `fetch(` is not a call, and prettier breaking a call over four lines changes
 * nothing. A pattern over the text gets all three wrong in one direction or the
 * other, and the wrong direction for a gate is the quiet one.
 *
 * ONE WALK OF `src/`'S MODULES. Fourteen suites carried their own, each
 * deciding for itself what a module is — which extensions, whether a suite is
 * excluded by `.test.` or by `.test.tsx?$` — and reporting paths relative to
 * whichever root it started from. A gate that disagreed about that
 * with its neighbours would certify a different tree while its name claimed
 * the same one. Every suite that reads the tree's code now takes it from
 * `modules()`, and one whose subject is a single directory or extension
 * narrows it by `path` rather than walking again. A STYLESHEET is not a
 * module, and the suites about the sheets list them themselves.
 *
 * Not a suite itself, and never imported by anything that ships: it reads the
 * filesystem, which a browser does not have.
 */

import { readdirSync, readFileSync, statSync } from "node:fs";
import { join, relative } from "node:path";
import { parseAst } from "vite";

/**
 * The source root. `process.cwd()` rather than `import.meta.url`: under the
 * jsdom environment a module's URL is the test server's rather than a file,
 * and Vitest runs from `dashboard/`.
 */
export const SRC = join(process.cwd(), "src");

/** How a module is parsed: TypeScript, TypeScript with JSX, or a declaration file. */
export type Lang = "ts" | "tsx" | "dts";

/** One module of the tree. */
export interface Module {
  /** Relative to `src/`, slash-separated: `protocol/rest.ts`. */
  path: string;
  text: string;
  lang: Lang;
}

let cached: Module[] | null = null;

/**
 * Every `.ts` and `.tsx` module under `src/` that is not itself a suite, sorted
 * by path.
 *
 * The test kits (`testing.tsx`, the builder's `testkit.tsx`) are in it: they
 * are not suites, and the Go gates that read this tree through
 * `internal/clientsource` read them too. A rule that cannot hold of a test kit
 * says so in its own suite rather than here.
 */
export function modules(): Module[] {
  if (cached) return cached;
  const out: Module[] = [];
  (function walk(dir: string): void {
    for (const entry of readdirSync(dir)) {
      const full = join(dir, entry);
      if (statSync(full).isDirectory()) {
        walk(full);
        continue;
      }
      if (!/\.tsx?$/.test(entry) || entry.includes(".test.")) continue;
      const lang: Lang = entry.endsWith(".d.ts") ? "dts" : entry.endsWith(".tsx") ? "tsx" : "ts";
      out.push({
        path: relative(SRC, full).split("\\").join("/"),
        text: readFileSync(full, "utf8"),
        lang,
      });
    }
  })(SRC);
  out.sort((a, b) => (a.path < b.path ? -1 : a.path > b.path ? 1 : 0));
  cached = out;
  return out;
}

/** An ESTree node as `parseAst` returns it. */
export interface Node {
  type: string;
  start: number;
  end: number;
  [key: string]: unknown;
}

export function isNode(value: unknown): value is Node {
  return (
    typeof value === "object" &&
    value !== null &&
    typeof (value as { type?: unknown }).type === "string"
  );
}

/** Parses one module's text into its `Program`. */
export function parse(text: string, lang: Lang): Node {
  return parseAst(text, { lang }) as unknown as Node;
}

/** A node's direct children with the field each is held under, in source order. */
function fieldsOf(node: Node): { child: Node; key: string }[] {
  const out: { child: Node; key: string }[] = [];
  for (const [key, value] of Object.entries(node)) {
    if (key === "type" || key === "start" || key === "end") continue;
    if (Array.isArray(value)) {
      for (const item of value) if (isNode(item)) out.push({ child: item, key });
    } else if (isNode(value)) {
      out.push({ child: value, key });
    }
  }
  return out.sort((a, b) => a.child.start - b.child.start);
}

/** A node's direct children, in source order. */
export function childrenOf(node: Node): Node[] {
  return fieldsOf(node).map(({ child }) => child);
}

/**
 * Visits every node under `root`, depth first and in source order, with the
 * node that holds it and the field it is held under — which is what tells the
 * `fetch` in `fetch(url)` from the one in `loader.fetch()`. Returning `false`
 * from `visit` skips that node's children.
 */
export function walk(
  root: Node,
  visit: (node: Node, parent: Node | null, key: string) => boolean | void,
): void {
  const step = (node: Node, parent: Node | null, key: string): void => {
    if (visit(node, parent, key) === false) return;
    for (const { child, key: field } of fieldsOf(node)) step(child, node, field);
  };
  step(root, null, "");
}

/** Maps an offset in `text` to its 1-based line. */
export function lineOf(text: string): (offset: number) => number {
  const starts = [0];
  for (let i = 0; i < text.length; i++) if (text[i] === "\n") starts.push(i + 1);
  return (offset) => {
    let lo = 0;
    let hi = starts.length - 1;
    while (lo < hi) {
      const mid = (lo + hi + 1) >> 1;
      if ((starts[mid] ?? 0) <= offset) lo = mid;
      else hi = mid - 1;
    }
    return lo + 1;
  };
}

/**
 * The value a node holds when it is a CONSTANT string — a string literal, or a
 * template with nothing substituted — and null when it is anything else.
 *
 * Exactly what `internal/clientsource` accepts as a literal, so a rule that
 * holds a call to a constant name here holds it to one the Go gates can read.
 */
export function stringValue(node: Node | undefined): string | null {
  if (!node) return null;
  if (node.type === "Literal") return typeof node.value === "string" ? node.value : null;
  if (node.type === "TemplateLiteral" && (node.expressions as Node[]).length === 0) {
    const value = (node.quasis as Node[])[0]?.value as
      { cooked?: string | null; raw: string } | undefined;
    return value?.cooked ?? value?.raw ?? "";
  }
  return null;
}

/**
 * The name a member expression reads: `b` in `a.b`, and in `a["b"]` too,
 * because a computed key that is a constant string names a property just as
 * surely. Null for anything else.
 */
export function memberName(node: Node): string | null {
  if (node.type !== "MemberExpression") return null;
  const property = node.property as Node;
  if (!node.computed) return property.type === "Identifier" ? String(property.name) : null;
  return stringValue(property);
}

/**
 * Whether an import names a module of this tree rather than a package: a
 * relative path, or the `~` alias for `src/`.
 */
export function isLocalSource(source: string): boolean {
  return source.startsWith("./") || source.startsWith("../") || source.startsWith("~/");
}

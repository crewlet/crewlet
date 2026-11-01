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

let cached: { modules: Module[]; suites: Module[] } | null = null;

/** The one walk: every `.ts` and `.tsx` file under `src/`, modules and suites apart, each sorted by path. */
function tree(): { modules: Module[]; suites: Module[] } {
  if (cached) return cached;
  const modules: Module[] = [];
  const suites: Module[] = [];
  (function walk(dir: string): void {
    for (const entry of readdirSync(dir)) {
      const full = join(dir, entry);
      if (statSync(full).isDirectory()) {
        walk(full);
        continue;
      }
      if (!/\.tsx?$/.test(entry)) continue;
      const lang: Lang = entry.endsWith(".d.ts") ? "dts" : entry.endsWith(".tsx") ? "tsx" : "ts";
      (entry.includes(".test.") ? suites : modules).push({
        path: relative(SRC, full).split("\\").join("/"),
        text: readFileSync(full, "utf8"),
        lang,
      });
    }
  })(SRC);
  const byPath = (a: Module, b: Module) => (a.path < b.path ? -1 : a.path > b.path ? 1 : 0);
  cached = { modules: modules.sort(byPath), suites: suites.sort(byPath) };
  return cached;
}

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
  return tree().modules;
}

/**
 * Every suite under `src/` — a file whose name carries `.test.` — sorted by
 * path, from the same walk: what a gate over the suites themselves reads.
 */
export function suites(): Module[] {
  return tree().suites;
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

/**
 * Every `Program` this worker has parsed, by language and text.
 *
 * ONE PARSE OF A MODULE PER SUITE, not one per gate. A suite holding three
 * gates over the tree parsed all of it three times — `parseAst` hands its tree
 * across as JSON, and decoding that is most of what a gate costs — and the
 * gates are CPU-bound work inside the runner's per-test budget, so a gate that
 * re-parsed the tree was a gate that ran out of time on a loaded machine
 * (measured as the CPU each case spent, from before its first `beforeEach` to
 * after its last `afterEach`, over the 411 cases of the 24 suites that parsed
 * at the time, one run each way in a four-core runner slot with nothing else
 * running in it: 41.8 s before, 26.2 s after). The key is the text itself, so
 * a gate parsing its own snippet gets its own tree, and a module that is
 * edited between runs is a new key. No gate writes to a tree it was handed,
 * which is what makes sharing one between the gates of a suite sound.
 */
const parsed: Record<Lang, Map<string, Node>> = { ts: new Map(), tsx: new Map(), dts: new Map() };

/** Parses one module's text into its `Program`, once per worker. */
export function parse(text: string, lang: Lang): Node {
  const known = parsed[lang].get(text);
  if (known) return known;
  const program = parseAst(text, { lang }) as unknown as Node;
  parsed[lang].set(text, program);
  return program;
}

/**
 * A node's direct children with the field each is held under, in source order.
 *
 * SORTED ONLY WHEN THE FIELDS ARE NOT ALREADY IN ORDER. ESTree lists a node's
 * fields in the order their text appears for almost every node, so the
 * children come out in source order as they are read; a node whose fields do
 * not (a template literal lists all its quasis before all its substitutions,
 * which interleave in the text) is sorted, stably, which is exactly what
 * sorting every node did — without paying for the sort on the four hundred
 * thousand nodes a gate over the tree visits.
 */
function fieldsOf(node: Node): { child: Node; key: string }[] {
  const out: { child: Node; key: string }[] = [];
  let ordered = true;
  let last = -Infinity;
  const take = (child: Node, key: string) => {
    if (child.start < last) ordered = false;
    last = child.start;
    out.push({ child, key });
  };
  for (const key in node) {
    if (key === "type" || key === "start" || key === "end") continue;
    const value = node[key];
    if (Array.isArray(value)) {
      for (const item of value) if (isNode(item)) take(item, key);
    } else if (isNode(value)) {
      take(value, key);
    }
  }
  return ordered ? out : out.sort((a, b) => a.child.start - b.child.start);
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

/** One piece of text a module ships, and the line it starts on. */
export interface ShippedText {
  text: string;
  line: number;
}

/**
 * Every piece of text a module SHIPS: each string literal, each constant part
 * of a template and each run of JSX text, in source order — and no comment,
 * which is free to say anything.
 *
 * ONE WALK FOR EVERY COPY GATE. Each workspace that holds its sentences to a
 * rule (no em dash, say) reads them through this rather than through a node
 * switch of its own, so two gates cannot disagree about what counts as copy:
 * one that forgot JSX text, or read a template's cooked value where the other
 * read its raw one, would pass a sentence the other refused.
 */
export function shippedText(module: Module): ShippedText[] {
  const line = lineOf(module.text);
  const out: ShippedText[] = [];
  walk(parse(module.text, module.lang), (node: Node) => {
    const text =
      node.type === "Literal"
        ? typeof node.value === "string"
          ? node.value
          : ""
        : node.type === "TemplateElement"
          ? String((node.value as { raw: string }).raw)
          : node.type === "JSXText"
            ? String(node.value)
            : "";
    if (text !== "") out.push({ text, line: line(node.start) });
  });
  return out;
}

/**
 * The design system as the gates read it: which of its modules an import
 * names, and which names each module hands an importer, read from the package
 * as it is installed.
 *
 * READ, NEVER LISTED. The package exports every module by its path (`"./*"` in
 * its `exports`), and some modules hand on others: its root, and its
 * `generated` module, export every component the package has. So which modules
 * reach a component is the package's to say, release by release. A gate that
 * listed them certified the release it was written against: one that knew the
 * root and the diffs' own modules let a namespace import of
 * `@crewlethq/ui/generated` reach the package's hued diffs unreported.
 *
 * THE BUILT MODULES ARE READ — the files an import resolves to (`"import":
 * "./dist/<module>/index.js"`) — rather than the declarations, so what is
 * judged is what a browser loads.
 *
 * Not a suite itself, and never imported by anything that ships: it reads the
 * filesystem.
 */

import { existsSync, readFileSync } from "node:fs";
import { createRequire } from "node:module";
import { dirname, join } from "node:path";
import { parse, type Node } from "./source.ts";

/** The package, as an import names it: its root, or a module of it (`@crewlethq/ui/<module>`). */
export const KIT = "@crewlethq/ui";

/**
 * The package's built modules, found through an export it declares.
 * `process.cwd()` rather than `import.meta.url` for the reason `source.ts`
 * gives, so a suite under jsdom reads it too.
 */
const DIST = dirname(
  createRequire(join(process.cwd(), "package.json")).resolve(`${KIT}/styles.css`),
);

/**
 * Which module of the package `source` names: "" for its root, the module's
 * path (`DiffView` in `@crewlethq/ui/DiffView`), or null for anything that is
 * not the package.
 */
export function kitModule(source: unknown): string | null {
  if (source === KIT) return "";
  return typeof source === "string" && source.startsWith(`${KIT}/`)
    ? source.slice(KIT.length + 1)
    : null;
}

/** The name an import or export specifier spells, as an identifier or a string. */
export function spelled(node: Node): string {
  return String(node.type === "Identifier" ? node.name : node.value);
}

/** What one built module exports by name, and the files it re-exports whole. */
interface Own {
  named: string[];
  whole: string[];
}

/** Each built module's own exports, by its file: read once per worker. */
const owns = new Map<string, Own>();

/**
 * What the built module at `file` exports by name, and the files it re-exports
 * whole. A file that is not there exports nothing, since an import of it would
 * not build.
 *
 * A DECLARATION THIS CANNOT NAME FAILS THE READING rather than being left out
 * of it: a name missed here is a way past a gate, and the wrong direction for
 * a gate is the quiet one.
 */
function ownExports(file: string): Own {
  const known = owns.get(file);
  if (known) return known;
  const own: Own = { named: [], whole: [] };
  if (existsSync(file)) {
    for (const node of parse(readFileSync(file, "utf8"), "ts").body as Node[]) {
      if (node.type === "ExportAllDeclaration") {
        const as = node.exported as Node | null;
        if (as) own.named.push(spelled(as));
        else own.whole.push(join(dirname(file), String((node.source as Node).value)));
        continue;
      }
      if (node.type !== "ExportNamedDeclaration") continue;
      for (const spec of node.specifiers as Node[]) own.named.push(spelled(spec.exported as Node));
      const declaration = node.declaration as Node | null;
      if (declaration) own.named.push(...bound(declaration, file));
    }
  }
  // KEPT ONLY ONCE READ WHOLE, so a reading that failed fails again rather
  // than answering with the names it had reached.
  owns.set(file, own);
  return own;
}

/** The names an exported declaration binds: a function's, a class's, or each variable's. */
function bound(declaration: Node, file: string): string[] {
  const named = (id: Node | null | undefined): string => {
    if (id?.type === "Identifier") return String(id.name);
    throw new Error(`${file}: cannot name what an export binds (${id?.type ?? declaration.type})`);
  };
  if (declaration.type === "VariableDeclaration") {
    return (declaration.declarations as Node[]).map((d) => named(d.id as Node));
  }
  return [named(declaration.id as Node | null | undefined)];
}

/**
 * Every name the built module at `file` hands an importer: each it exports by
 * name, and every name of each module it re-exports whole, however deep —
 * every module of the chain read whole, so a chain that leads back into
 * itself is read once and still complete.
 */
export function moduleExports(file: string): ReadonlySet<string> {
  const names = new Set<string>();
  const seen = new Set<string>();
  const visit = (at: string) => {
    if (seen.has(at)) return;
    seen.add(at);
    const own = ownExports(at);
    for (const name of own.named) names.add(name);
    for (const next of own.whole) visit(next);
  };
  visit(file);
  return names;
}

/** Every name a module of the package (as `kitModule` names it) hands an importer. */
export function kitExports(module: string): ReadonlySet<string> {
  return moduleExports(module === "" ? join(DIST, "index.js") : join(DIST, module, "index.js"));
}

/** Whether a module of the package, as `kitModule` names it, exports any of `names`. */
export function kitReaches(module: string | null, names: Iterable<string>): boolean {
  if (module === null) return false;
  const exported = kitExports(module);
  for (const name of names) if (exported.has(name)) return true;
  return false;
}

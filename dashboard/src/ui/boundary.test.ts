// @vitest-environment node
/**
 * The component library stands alone.
 *
 * `ui/` is meant to be liftable into a shared design system without an edit:
 * a primitive that reaches into the wire types, the store bindings or a
 * screen has welded this engine's data model into something that was supposed
 * to be generic, and the weld is invisible until the day somebody tries to
 * move the file. So the rule is asserted rather than remembered, and it is
 * asserted in its strong form: every import in `ui/` names a package or a
 * file that is itself inside `ui/`. That covers `~/protocol`, `~/lib` and
 * `~/routes` by name, and also the relative spellings (`../lib/format.ts`)
 * and the directories nobody thought to list.
 *
 * Sources are read as TEXT, in the idiom of `protocol/proxy.test.ts`: the
 * point is what the files say, including an import behind a branch nothing
 * executes.
 */

import { readFileSync, readdirSync } from "node:fs";
import { dirname, join, relative, resolve, sep } from "node:path";
import { fileURLToPath } from "node:url";
import { describe, expect, test } from "vitest";

const UI = dirname(fileURLToPath(import.meta.url));
const SRC = resolve(UI, "..");

/** Every .ts and .tsx file under ui/, tests included: a test moves with its primitive. */
function files(dir: string): string[] {
  const out: string[] = [];
  for (const entry of readdirSync(dir, { withFileTypes: true })) {
    const path = join(dir, entry.name);
    if (entry.isDirectory()) out.push(...files(path));
    else if (/\.tsx?$/.test(entry.name)) out.push(path);
  }
  return out;
}

/**
 * Every module specifier a source names.
 *
 * Comments are removed first, so a module doc that QUOTES an import (as this
 * one does) is not read as one. Only whole-line `//` comments are stripped:
 * a `//` inside a string is a URL, and cutting from it to the end of the line
 * could hide a real import rather than invent one.
 */
function specifiers(source: string): string[] {
  const code = source.replace(/\/\*[\s\S]*?\*\//g, "").replace(/^\s*\/\/.*$/gm, "");
  const found: string[] = [];
  const patterns = [
    // import x from "a"; import { a, b } from "a"; export { a } from "a"; import type ...
    /(?:^|[;\s])(?:import|export)\s[^;"']*?\sfrom\s*["']([^"']+)["']/g,
    // import "a";  (a side effect, such as a stylesheet)
    /(?:^|[;\s])import\s*["']([^"']+)["']/g,
    // import("a"), vi.mock("a"), vi.importActual("a")
    /\b(?:import|mock|importActual|doMock)\(\s*["']([^"']+)["']/g,
  ];
  for (const pattern of patterns) {
    for (const match of code.matchAll(pattern)) found.push(match[1]!);
  }
  return found;
}

/** Where a local specifier lands, or null for a package. */
function target(file: string, spec: string): string | null {
  if (spec.startsWith("~/")) return join(SRC, spec.slice(2));
  if (spec.startsWith(".") || spec.startsWith("/")) return resolve(dirname(file), spec);
  return null;
}

describe("the ui/ boundary", () => {
  test("the scanner finds the imports it is meant to police", () => {
    // A scanner that silently matched nothing would pass the real test below
    // forever. These are imports the tree is known to hold.
    const field = readFileSync(join(UI, "Field.tsx"), "utf8");
    expect(specifiers(field)).toContain("./Problems.tsx");
    expect(specifiers(field)).toContain("react");
    expect(specifiers('import { a } from "~/lib/format.ts";')).toEqual(["~/lib/format.ts"]);
    expect(
      specifiers('import {\n  a,\n  type B,\n} from "../protocol/index.ts";\nimport "./x.css";'),
    ).toEqual(["../protocol/index.ts", "./x.css"]);
    expect(specifiers('const m = await import("~/routes/Org.tsx");')).toEqual(["~/routes/Org.tsx"]);
    expect(specifiers('/* import x from "~/lib/a.ts" */\n// import "~/lib/b.ts"')).toEqual([]);
  });

  test("nothing in ui/ imports from outside ui/", () => {
    const violations: string[] = [];
    // This suite is the one file excused, and only because its fixtures are
    // forbidden imports written as string literals for the scanner to find.
    const self = fileURLToPath(import.meta.url);
    for (const file of files(UI).filter((f) => f !== self)) {
      for (const spec of specifiers(readFileSync(file, "utf8"))) {
        const landed = target(file, spec);
        if (landed === null) continue;
        if (landed === UI || landed.startsWith(UI + sep)) continue;
        violations.push(`${relative(SRC, file)} imports ${spec}`);
      }
    }
    // A violation is fixed by moving the dependency into ui/ (if it is
    // generic) or by passing the value in as a prop (if it is engine data),
    // never by widening this test.
    expect(violations).toEqual([]);
  });
});

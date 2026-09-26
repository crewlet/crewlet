// @vitest-environment node
import { readdirSync, readFileSync } from "node:fs";
import { createRequire } from "node:module";
import { join, relative } from "node:path";
import { fileURLToPath } from "node:url";
import { describe, expect, test } from "vitest";

/**
 * Every custom property the stylesheets read must be declared.
 *
 * # Why this is a test
 *
 * `var(--surface-sunken)` on a token that does not exist is not an error, not
 * a warning, and not a build failure. The declaration is simply DROPPED: the
 * element keeps whatever it inherited, which for a colour is usually something
 * plausible and for a background is nothing at all. It is the same silent
 * shape `classes.test.ts` was written for, one level down — a name in a global
 * namespace with nothing connecting it to its definition.
 *
 * Eight had accumulated when this was written, and seven of them landed in one
 * afternoon: a workload bar's TRACK read `--surface-sunken` where the scale
 * had an inset rung, so the channel behind every bar was transparent and the
 * column looked empty. `--ink` and `--ink-muted` were the same mistake: a
 * plausible name from another design system, spelled confidently. The eighth,
 * `--text-primary`, had been colouring prose and every markdown heading with
 * an inherited value since the day it was written.
 *
 * # What a fallback means
 *
 * `var(--peek-w, 420px)` is DELIBERATE: a property a script sets at runtime,
 * with the default written at the point of use. A two-argument `var()` is
 * therefore exempt — it cannot fail silently, because the fallback IS the
 * value when nothing sets it.
 */

const SRC = fileURLToPath(new URL("..", import.meta.url));
const require_ = createRequire(import.meta.url);

/** Every stylesheet under `dashboard/src`, wherever a screen keeps one. */
function ours(): { name: string; text: string }[] {
  const walk = (dir: string): string[] =>
    readdirSync(dir, { withFileTypes: true }).flatMap((e) => {
      const at = join(dir, e.name);
      return e.isDirectory() ? walk(at) : e.name.endsWith(".css") ? [at] : [];
    });
  return walk(SRC).map((at) => ({
    name: relative(SRC, at).split("\\").join("/"),
    text: readFileSync(at, "utf8"),
  }));
}

/**
 * Where a custom property can be declared: our sheets, and the design
 * system's.
 *
 * THE DESIGN SYSTEM IS THE DECLARATION SOURCE, not a black box. Every sheet
 * here reads `--spacing-*`, `--color-*`, `--size-*` and the rest under the
 * package's own names, and those are declared in `@crewlethq/tokens` rather
 * than here — so a gate reading only this tree would call every one of them
 * undeclared, which is the opposite of what it is for.
 *
 * READ FROM THE INSTALLED PACKAGE, not from a copy or a list. That is what
 * keeps the check honest across a bump: if a release renames `--spacing-7`,
 * every rule reading it fails here rather than resolving to nothing in a
 * browser, where an unset custom property is simply an empty value and a
 * padding silently becomes zero. Every entry `main.tsx` imports is read.
 */
function sheets(): { name: string; text: string }[] {
  const theirs = ["", "/themes", "/density", "/fonts", "/base"].map((entry) => ({
    name: `@crewlethq/tokens/css${entry}`,
    text: readFileSync(require_.resolve(`@crewlethq/tokens/css${entry}`), "utf8"),
  }));
  return [...ours(), ...theirs];
}

/**
 * Every colour or shadow a stylesheet of OURS declares.
 *
 * THE PALETTE IS THE PACKAGE'S, ALL OF IT. A `--color-*` declared here is a
 * second value for a name the design system owns — it wins or loses on
 * import order, and the palette suite (`palette.test.ts`) measures the
 * package's value, not the one a reader sees. A `--shadow-*` is the same
 * mistake one family over. And a NEW name in either family is a colour of our
 * own under the package's namespace, which is how a local palette starts. So
 * a stylesheet under `dashboard/src` declares neither; what it paints with, it
 * reads.
 */
export function declaresPalette(all: { name: string; text: string }[]): string[] {
  const out: string[] = [];
  for (const { name, text } of all) {
    const bare = text.replace(/\/\*[\s\S]*?\*\//g, (c) => c.replace(/[^\n]/g, " "));
    for (const m of bare.matchAll(/(^|[;{\s])(--(?:color|shadow)-[a-zA-Z0-9_-]*)\s*:/g)) {
      const line = bare.slice(0, m.index).split("\n").length;
      out.push(`${name}:${line} declares ${m[2]}, which is the design system's to declare`);
    }
  }
  return out;
}

/**
 * Every no-fallback `var()` in `all` whose name no sheet declares.
 *
 * EXTRACTED SO BOTH TESTS BELOW RUN THE SAME CODE. They used to run two
 * copies: the real check inlined its regexes over the stylesheets, and the
 * "it can tell" case re-spelled them over a literal — so the mutation test
 * never touched the thing it claimed to cover and stayed green for every
 * possible break in it. One function is what makes the second test evidence
 * about the first.
 */
export function undeclared(all: { name: string; text: string }[]): string[] {
  const declared = new Set<string>();
  for (const { text } of all) {
    for (const m of text.matchAll(/^\s*(--[a-zA-Z0-9_-]+)\s*:/gm)) declared.add(m[1]!);
  }
  const missing: string[] = [];
  for (const { name, text } of all) {
    for (const m of text.matchAll(/var\((--[a-zA-Z0-9_-]+)\s*([,)])/g)) {
      // A two-argument var() carries its own default and cannot fail
      // silently — see the note above.
      if (m[2] === ",") continue;
      if (declared.has(m[1]!)) continue;
      const line = text.slice(0, m.index).split("\n").length;
      missing.push(`${name}:${line} reads ${m[1]}, which nothing declares`);
    }
  }
  return missing;
}

describe("the token namespace", () => {
  test("every var() with no fallback names a property something declares", () => {
    expect(undeclared(sheets())).toEqual([]);
  });

  test("no stylesheet of ours declares a colour or a shadow", () => {
    expect(declaresPalette(ours())).toEqual([]);
  });

  test("it can tell — a colour or a shadow declared here is caught, a read is not", () => {
    // THE MUTATION THE GATE IS NAMED FOR, against the function the case above
    // calls: `--color-brand-accent` redeclared in components.css.
    const found = declaresPalette([
      {
        name: "components.css",
        text: ":root {\n  --row-inline: 1px;\n  --color-brand-accent: #f00;\n}\n.a { --shadow-x: none; color: var(--color-text-primary); }\n/* --color-in-a-comment: 1 */",
      },
    ]);
    expect(found).toEqual([
      "components.css:3 declares --color-brand-accent, which is the design system's to declare",
      "components.css:5 declares --shadow-x, which is the design system's to declare",
    ]);
  });

  test("is reading the stylesheets at all, so the scan cannot pass on nothing", () => {
    // The check above is vacuous if `sheets()` comes back empty or short — a
    // stylesheet moved into a subdirectory, a changed extension — and an
    // empty list satisfies it perfectly. Same idiom as classes.test.ts's own
    // floor.
    const all = sheets();
    expect(ours().length).toBeGreaterThan(4);
    expect(all.length).toBeGreaterThan(ours().length + 4);
    expect(all.some((s) => /^\s*--color-text-primary\s*:/m.test(s.text))).toBe(true);
    expect(ours().some((s) => /var\(--color-text-primary\)/.test(s.text))).toBe(true);
  });

  test("it can tell — a property nobody declares is caught, a fallback is not", () => {
    // The mutation this file exists to catch, run against THE FUNCTION THE
    // TEST ABOVE CALLS. Both arms of the exemption are exercised: `--nope`
    // has no fallback and must be reported, `--nope2` carries one and must
    // not be.
    const missing = undeclared([
      {
        name: "fake.css",
        text: ":root {\n  --text: #fff;\n}\n.a {\n  color: var(--text);\n  border-color: var(--nope);\n  width: var(--nope2, 1px);\n}",
      },
    ]);
    expect(missing).toEqual(["fake.css:6 reads --nope, which nothing declares"]);
  });
});

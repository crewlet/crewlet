// @vitest-environment node
import { readdirSync, readFileSync } from "node:fs";
import { createRequire } from "node:module";
import { join } from "node:path";
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
 * afternoon: a workload bar's TRACK read `--surface-sunken` where the scale is
 * `--surface-inset`, so the channel behind every bar was transparent and the
 * column looked empty. `--ink` and `--ink-muted` (the scale is `--text` and
 * `--text-muted`), `--radius-sm`/`--radius-md` (`--r-sm`/`--r-md`),
 * `--surface`/`--surface-raised` (`--surface-1`/`--surface-2`) are the same
 * mistake: a plausible name from another design system, spelled confidently.
 * The eighth, `--text-primary`, had been colouring prose and every markdown
 * heading with an inherited value since the day it was written.
 *
 * # What a fallback means
 *
 * `var(--peek-w, 420px)` is DELIBERATE: a property a script sets at runtime,
 * with the default written at the point of use. A two-argument `var()` is
 * therefore exempt — it cannot fail silently, because the fallback IS the
 * value when nothing sets it.
 */

const STYLES = fileURLToPath(new URL(".", import.meta.url));
const require_ = createRequire(import.meta.url);

/**
 * Where a custom property can be declared: our sheets, and uilet's.
 *
 * THE DESIGN SYSTEM IS A DECLARATION SOURCE, not a black box. `uilet.css`
 * resolves our short names onto `--spacing-*`, `--color-*` and the rest, and
 * those are declared in `@crewlethq/tokens` rather than here — so a gate
 * reading only this directory would call every one of them undeclared, which
 * is the opposite of what it is for.
 *
 * READ FROM THE INSTALLED PACKAGE, not from a copy or a list. That is what
 * keeps the check honest across a bump: if 0.4.0 renames `--spacing-7`, the
 * alias pointing at it fails here rather than resolving to nothing in a
 * browser, where an unset custom property is simply an empty value and a
 * padding silently becomes zero.
 */
function sheets(): { name: string; text: string }[] {
  const ours = readdirSync(STYLES)
    .filter((f) => f.endsWith(".css"))
    .map((name) => ({ name, text: readFileSync(join(STYLES, name), "utf8") }));
  const theirs = ["", "/themes", "/density", "/base"].map((entry) => ({
    name: `@crewlethq/tokens/css${entry}`,
    text: readFileSync(require_.resolve(`@crewlethq/tokens/css${entry}`), "utf8"),
  }));
  return [...ours, ...theirs];
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

  test("is reading the stylesheets at all, so the scan cannot pass on nothing", () => {
    // The check above is vacuous if `sheets()` comes back empty or short — a
    // stylesheet moved into a subdirectory, a changed extension — and an
    // empty list satisfies it perfectly. Same idiom as classes.test.ts's own
    // floor.
    const all = sheets();
    expect(all.length).toBeGreaterThan(4);
    expect(all.some((s) => /^\s*--text\s*:/m.test(s.text))).toBe(true);
    expect(all.some((s) => /var\(--text\)/.test(s.text))).toBe(true);
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

/**
 * A NUMBER THIS TREE RESTATES FROM THE PACKAGE, held against the package.
 *
 * `--select-menu-w-max` is the cap `components.css` grows a portalled dropdown
 * panel to, and it is deliberately the SAME number `@crewlethq/ui`'s Select
 * caps an `auto`-width picker at — a list has no business standing wider than
 * the control it belongs to may. It cannot be read with `var()`: the panel is
 * portalled to the document body, so a property declared on `.crewlet-select`
 * never reaches it and the fallback would quietly be the value every time.
 *
 * So it is written twice, and this is what stops the two drifting. A bump that
 * retunes the picker's cap fails HERE, naming both numbers, rather than
 * leaving a panel that may grow past the trigger it hangs from — which is a
 * difference nobody sees until an option is long enough to reach it.
 */
describe("the dropdown cap", () => {
  const selectCSS = () => {
    // Through the SUBPATH the package actually exports, which is the one
    // `classes.test.ts` already reads for the same reason: the package has no
    // main entry, so resolving its name alone throws.
    const dist = join(require_.resolve("@crewlethq/ui/styles.css"), "..");
    const sheet = readdirSync(dist).find((f) => /^Select-.*\.css$/.test(f));
    if (!sheet) throw new Error("no Select stylesheet in @crewlethq/ui/dist");
    return readFileSync(join(dist, sheet), "utf8");
  };

  test("is the package's own cap for a picker", () => {
    const theirs = /--crewlet-select-auto-max-width:\s*([^;]+);/.exec(selectCSS());
    const ours = /--select-menu-w-max:\s*([^;]+);/.exec(
      readFileSync(join(STYLES, "tokens.css"), "utf8"),
    );
    expect(theirs?.[1]?.trim()).toBeDefined();
    expect(ours?.[1]?.trim()).toBe(theirs?.[1]?.trim());
  });
});

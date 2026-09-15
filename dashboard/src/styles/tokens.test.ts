// @vitest-environment node
import { readdirSync, readFileSync } from "node:fs";
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

function sheets(): { name: string; text: string }[] {
  return readdirSync(STYLES)
    .filter((f) => f.endsWith(".css"))
    .map((name) => ({ name, text: readFileSync(join(STYLES, name), "utf8") }));
}

describe("the token namespace", () => {
  test("every var() with no fallback names a property something declares", () => {
    const all = sheets();
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
    expect(missing).toEqual([]);
  });

  test("it can tell — a property nobody declares is caught", () => {
    // The mutation this file exists to catch, run against itself: without it
    // the test above passes on any stylesheet, including one that reads
    // nothing real.
    const declared = new Set(["--text"]);
    const text = ".a { color: var(--text); background: var(--nope); }";
    const missing = [...text.matchAll(/var\((--[a-zA-Z0-9_-]+)\s*([,)])/g)]
      .filter((m) => m[2] === ")" && !declared.has(m[1]!))
      .map((m) => m[1]);
    expect(missing).toEqual(["--nope"]);
  });
});

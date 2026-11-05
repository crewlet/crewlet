// @vitest-environment node
import { readdirSync, readFileSync } from "node:fs";
import { createRequire } from "node:module";
import { join } from "node:path";
import { describe, expect, test } from "vitest";
import { sheetRules, STYLES } from "../test/sheets.ts";

/**
 * EVERY SCROLLBAR IS THE KIT'S, AND CHROMIUM DRAWS THE ONE IT DEFINES.
 *
 * The kit's base sheet draws the bar with `::-webkit-scrollbar` rules, and
 * Chromium ignores those wherever a `scrollbar-width` or `scrollbar-color`
 * applies, drawing its own wider bar instead. Our base sheet kept a copy of the
 * kit's scrollbar block, `* { scrollbar-width: thin }` included, so every bar
 * was Chromium's, wider than the gutter `app/layout.ts` counts, and a change
 * to the kit's bar could not show. Hiding a bar (`none`) is not that trap.
 */

const require_ = createRequire(import.meta.url);

/** Where `::-webkit-scrollbar` is not read: the only place the two properties may style a bar. */
const NO_WEBKIT_BAR = "@supports not selector(::-webkit-scrollbar)";

/** A `scrollbar-width` or `scrollbar-color` declaration, and its value. */
const DECLARATION = /(?<![\w-])(scrollbar-(?:width|color))\s*:\s*([^;}]+)/g;

/** A declaration that hides the bar rather than styling it. */
function hides(property: string, value: string): boolean {
  return property === "scrollbar-width" && /^none(\s*!important)?$/.test(value.trim());
}

/** A sheet with its comments removed and every `NO_WEBKIT_BAR` block cut out, braces balanced. */
function whereWebkitBarIsRead(css: string): string {
  let out = css.replace(/\/\*[\s\S]*?\*\//g, "");
  for (let at = out.indexOf(NO_WEBKIT_BAR); at >= 0; at = out.indexOf(NO_WEBKIT_BAR)) {
    let i = out.indexOf("{", at) + 1;
    for (let depth = 1; i < out.length && depth > 0; i++) {
      if (out[i] === "{") depth++;
      else if (out[i] === "}") depth--;
    }
    out = out.slice(0, at) + out.slice(i);
  }
  return out;
}

/**
 * Every stylesheet the dashboard loads, by name: each kit sheet `main.tsx`
 * imports, the design system sheet `vite.config.ts` assembles (the ui
 * package's `styles.css` and the icons' sheets), and ours.
 */
function loadedSheets(): { name: string; css: string }[] {
  const boot = readFileSync(join(STYLES, "..", "main.tsx"), "utf8");
  const kit = [...boot.matchAll(/^import "(@crewlethq\/[^"]+)";$/gm)].map((m) => m[1]!);
  expect(kit, "main.tsx no longer imports the kit's base sheet").toContain(
    "@crewlethq/tokens/css/base",
  );
  const files = kit.map((name) => ({ name, file: require_.resolve(name) }));
  files.push({
    name: "@crewlethq/ui/styles.css",
    file: require_.resolve("@crewlethq/ui/styles.css"),
  });
  // As `vite.config.ts` finds them: the package exports no stylesheet path.
  const icons = join(process.cwd(), "node_modules", "@crewlethq", "icons", "dist");
  for (const f of readdirSync(icons).filter((f) => f.endsWith(".css"))) {
    files.push({ name: `@crewlethq/icons/${f}`, file: join(icons, f) });
  }
  for (const f of readdirSync(STYLES).filter((f) => f.endsWith(".css"))) {
    files.push({ name: f, file: join(STYLES, f) });
  }
  return files.map(({ name, file }) => ({ name, css: readFileSync(file, "utf8") }));
}

describe("scrollbars", () => {
  // Mutation: put `scrollbar-width: thin` back on `*` in the kit's base sheet,
  // or on any rule of ours outside the `@supports` block.
  test("nothing the dashboard loads sets scrollbar-width or scrollbar-color where Chromium reads ::-webkit-scrollbar", () => {
    const styled = loadedSheets().flatMap(({ name, css }) =>
      [...whereWebkitBarIsRead(css).matchAll(DECLARATION)]
        .filter((m) => !hides(m[1]!, m[2]!))
        .map((m) => `${name}: ${m[1]}: ${m[2]!.trim()}`),
    );
    expect(styled).toEqual([]);
  });

  // Mutation: restore the copy of the kit's scrollbar block in base.css.
  test("no sheet of ours styles a scrollbar: hiding one is all a sheet here does", () => {
    const styled: string[] = [];
    for (const { file, selector, body } of sheetRules()) {
      for (const m of body.matchAll(DECLARATION)) {
        if (!hides(m[1]!, m[2]!)) styled.push(`${file} ${selector}: ${m[1]}: ${m[2]!.trim()}`);
      }
      if (selector.includes("::-webkit-scrollbar") && !/^\s*display:\s*none;?\s*$/.test(body)) {
        styled.push(`${file} ${selector}: ${body.trim()}`);
      }
    }
    expect(styled).toEqual([]);
  });
});

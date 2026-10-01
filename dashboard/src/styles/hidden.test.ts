// @vitest-environment node
import { readdirSync, readFileSync } from "node:fs";
import { createRequire } from "node:module";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";
import { expect, test } from "vitest";

/**
 * THE HIDDEN ATTRIBUTE HIDES.
 *
 * The browser's `[hidden] { display: none }` is a user-agent rule, which any
 * author `display` outranks. The kit's Disclosure closes its panel with
 * `hidden` and styles that same panel `display: flex`, so every closed
 * disclosure in the product drew its panel — and nothing about that fails: a
 * screenshot of an open-looking section reads as a section somebody opened.
 * `styles/base.css` restores the attribute's meaning for every element, and
 * this holds it there, together with the reason it has to exist: the moment
 * the kit stops setting a display on the panel, the second assertion says so
 * and the rule can be re-judged rather than kept by habit.
 */

const here = dirname(fileURLToPath(import.meta.url));
const base = readFileSync(join(here, "base.css"), "utf8");

test("base.css makes the hidden attribute beat any class's display", () => {
  const rule =
    /\[hidden\]:not\(\[hidden="until-found"\]\)\s*\{\s*display:\s*none\s*!important;\s*\}/;
  expect(base).toMatch(rule);
});

test("the kit's disclosure panel still sets the display the rule is there to beat", () => {
  // THE PACKAGE'S OWN DIST, found through an export it declares.
  const dist = dirname(createRequire(import.meta.url).resolve("@crewlethq/ui/styles.css"));
  const sheet = readdirSync(dist).find((f) => /^Disclosure-.*\.css$/.test(f));
  expect(sheet, "the kit ships a Disclosure stylesheet").toBeDefined();
  const css = readFileSync(join(dist, sheet!), "utf8");
  expect(css).toMatch(/\.crewlet-disclosure__panel\s*\{[^}]*display:\s*flex/);
});

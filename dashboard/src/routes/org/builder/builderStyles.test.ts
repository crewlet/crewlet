// @vitest-environment node
/**
 * The builder's stylesheet keeps the two promises its views rely on and a
 * browser-free suite cannot observe: a live state badge never resizes the
 * card it sits in, and no builder card or row is coloured by what it holds.
 *
 * The canvas lays cards out by their MEASURED size. A badge whose slot could
 * shrink, wrap or push the seat's name onto a second line would change a
 * card's height on a live push, twice per tool-loop round, and every card
 * beside it would move. jsdom has no layout to catch that, so the rules that
 * prevent it are read from the stylesheet.
 */

import { readFileSync } from "node:fs";
import { fileURLToPath } from "node:url";
import { expect, test } from "vitest";

const css = readFileSync(
  fileURLToPath(new URL("../../../styles/screens.css", import.meta.url)),
  "utf8",
);

/** Every innermost rule as `[selector, declarations]`, comments removed. */
function rules(source: string): [string, string][] {
  const bare = source.replace(/\/\*[\s\S]*?\*\//g, "");
  return [...bare.matchAll(/([^{}]+)\{([^{}]*)\}/g)].map((m) => [m[1]!.trim(), m[2]!]);
}

const rule = (selector: string) =>
  rules(css)
    .filter(([s]) => s.split(",").some((part) => part.trim() === selector))
    .map(([, body]) => body)
    .join(";");

test("the live state slot neither shrinks nor wraps, and the name beside it truncates instead", () => {
  expect(rule(".bnode-state")).toMatch(/flex:\s*none/);
  expect(rule(".bnode-state")).toMatch(/white-space:\s*nowrap/);
  // A flex child keeps its content's minimum width unless told otherwise, and
  // a long name would then push the badge rather than truncate.
  expect(rule(".bchart-text")).toMatch(/min-width:\s*0/);
  expect(rule(".bchart-line")).toMatch(/min-width:\s*0/);
});

test("a builder card or row takes no status or data hue, only the accent for the selection", () => {
  const builder = rules(css).filter(([selector]) => /\.(bchart|boutline|bnode)/.test(selector));
  expect(builder.length).toBeGreaterThan(10);
  const hues = builder
    .filter(([, body]) => /var\(--(positive|caution|critical|info|data|phase)[-\w]*\)/.test(body))
    .map(([selector]) => selector);
  expect(hues).toEqual([]);
});

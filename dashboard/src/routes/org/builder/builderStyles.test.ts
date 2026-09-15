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
  fileURLToPath(new URL("../../../styles/org.css", import.meta.url)),
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

test("the live state and problem count slots neither shrink nor wrap, and the name truncates", () => {
  for (const slot of [".bnode-state", ".bnode-count"]) {
    expect(rule(slot), slot).toMatch(/flex:\s*none/);
    expect(rule(slot), slot).toMatch(/white-space:\s*nowrap/);
  }
  // A flex child keeps its content's minimum width unless told otherwise, and
  // a long name would then push the badge rather than truncate.
  expect(rule(".bchart-text")).toMatch(/min-width:\s*0/);
  expect(rule(".bchart-line")).toMatch(/min-width:\s*0/);
});

// A scroll box clips only descendants whose containing block is inside it:
// the outline's screen-reader column header, absolutely positioned against the
// frame, escaped the grid's sideways scroller and gave the page a sideways
// overflow as wide as the grid, which a focus or a find in page then scrolled.
test("the outline scrolls sideways in its own box, which contains everything it holds", () => {
  expect(rule(".boutline-wrap")).toMatch(/overflow-x:\s*auto/);
  expect(rule(".boutline-wrap")).toMatch(/position:\s*relative/);
});

// Both views read the design system's own tree-card variable, and they read it
// BARE: the fallback in `var(--x, 15rem)` belongs to a component that must draw
// without a consumer, and this application is the consumer. An undeclared
// custom property is invalid at computed-value time and takes its whole
// declaration with it, so a missing declaration is not a wrong width but no
// width at all: cards at `width: auto` and an outline whose sideways scroller
// has no minimum. Nothing in jsdom computes a custom property, so the
// declaration is read from the stylesheet.
test("the card width the chart and the outline both read is declared", () => {
  expect(rule(".org-builder")).toMatch(/--crewlet-tree-canvas-card-width:\s*\S/);
  expect(rule(".bchart-box")).toMatch(/width:\s*var\(--crewlet-tree-canvas-card-width\)/);
  expect(rule(".boutline")).toMatch(/var\(--crewlet-tree-canvas-card-width\)/);
});

/**
 * A status, data or phase hue, under the names the design system gives them.
 *
 * Spelled as a pattern over the FAMILY rather than over each token, and paired
 * with the case below, because this guard was already dead once: it matched
 * `--data-*` while the tokens were named `--viz-*`, and the rename to the
 * design system's names made it unable to match anything at all.
 */
const CARRIED_HUE = /var\(--color-(feedback|data|phase)-[-\w]*\)/;

test("a builder card or row takes no status or data hue, only the accent for the selection", () => {
  const builder = rules(css).filter(([selector]) => /\.(bchart|boutline|bnode)/.test(selector));
  expect(builder.length).toBeGreaterThan(10);
  const hues = builder.filter(([, body]) => CARRIED_HUE.test(body)).map(([selector]) => selector);
  expect(hues).toEqual([]);
});

test("the hue guard recognises a hue, and lets the accent through", () => {
  expect(CARRIED_HUE.test("color: var(--color-feedback-danger-ink);")).toBe(true);
  expect(CARRIED_HUE.test("background: var(--color-data-3);")).toBe(true);
  expect(CARRIED_HUE.test("border-color: var(--color-phase-execute-soft);")).toBe(true);
  // Where the reader is, which is the one colour a builder card may carry.
  expect(CARRIED_HUE.test("background: var(--color-brand-accent-soft);")).toBe(false);
  expect(CARRIED_HUE.test("color: var(--color-text-tertiary);")).toBe(false);
});

// @vitest-environment node
/**
 * The builder's stylesheet keeps the promises its views rely on and a
 * browser-free suite cannot observe: a live state badge never resizes the card
 * it sits in, no builder card or row is coloured by what it holds, and nothing
 * here redraws what the design system's chart and table already draw.
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

test("the live state and problem count slots neither shrink nor wrap", () => {
  for (const slot of [".bnode-state", ".bnode-count"]) {
    expect(rule(slot), slot).toMatch(/flex:\s*none/);
    expect(rule(slot), slot).toMatch(/white-space:\s*nowrap/);
  }
});

/*
 * A WIRING MARK KEEPS ITS SIZE TOO. It is a glyph on a chart node's caption,
 * and a node one rank tall is measured: a mark that could shrink or grow would
 * relay the chart. The name beside it truncating, and the slot the live state
 * and the count sit in, are the design system's `OrgNodeLabel` now, with the
 * suite that reads them; this is the one part of that promise this screen
 * still draws for itself.
 */
test("a wiring mark keeps its size", () => {
  expect(rule(".bnode-mark")).toMatch(/flex:\s*none/);
});

// The chart reads the design system's own tree-card variable, and it reads it
// with a fallback, because a component has to draw without a consumer. This
// application IS the consumer, and the one declaration it writes is what every
// card, every row measured inside one and every connector lays out against.
// Nothing in jsdom computes a custom property, so the declaration is read from
// the stylesheet.
test("the card width the chart is drawn at is declared", () => {
  expect(rule(".org-builder")).toMatch(/--crewlet-tree-canvas-card-width:\s*\S/);
});

/*
 * THE CHART'S FRAME IS NOT DRAWN HERE ANY MORE, and the chart's whole shape
 * went with it. The card, its dashed variant, the inset on each node, the focus
 * ring, the accent ring on the selection, the connectors and the reveal of the
 * pointer's controls are the design system's `TreeCanvas`, so a rule here that
 * drew one again would be this screen quietly disagreeing with every other
 * chart in the product. The same holds for the treegrid this file used to draw
 * by hand, which is `DataView` now.
 */
test("no builder rule redraws what the design system's chart and table draw", () => {
  const redrawn = rules(css)
    .filter(([selector]) => /^\.(bchart|btable)/.test(selector.trim()))
    .filter(([, body]) =>
      /box-shadow|border-radius:\s*var\(--radius-lg\)|grid-template-columns/.test(body),
    )
    .map(([selector]) => selector);
  expect(redrawn).toEqual([]);
  // And nothing names the elements the component owns.
  expect(css).not.toMatch(/\.bchart-(card|box|links|gap-probe|actions|head|row-item)\b/);
  // And nothing here draws what a chart node SAYS: the name, its caption, the
  // marks on that caption and a unit's lead are `OrgNodeLabel` and
  // `OrgNodeLead`, so this screen cannot say a name at a second size.
  expect(css).not.toMatch(/\.bchart-(name|meta|text|line|marks|lead|rows?|row)\b/);
  expect(css).not.toMatch(/\.boutline/);
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

/*
 * A NODE HUE IS NOT ONE OF THESE, and it does not come from here. An agent
 * seat is tinted with one of the design system's six node hues, chosen from
 * the seat's own key (`nodeTone.ts`) and handed to the chart as a NAME, which
 * the design system turns into that hue's four measured steps. So the hue
 * still reaches no rule in this stylesheet, and this guard means what it
 * always meant: nothing here paints a builder node by what it holds.
 */
test("a builder node or row takes no status or data hue, only the accent for the selection", () => {
  const builder = rules(css).filter(([selector]) => /\.(bchart|btable|bnode)/.test(selector));
  expect(builder.length).toBeGreaterThan(5);
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

// @vitest-environment node
/**
 * The builder's stylesheet keeps the promises its views rely on and a
 * browser-free suite cannot observe: a live state badge never resizes the row
 * it sits in, no builder card or row is coloured by what it holds, and nothing
 * here redraws what the design system's chart and table already draw.
 *
 * The canvas lays cards out by their MEASURED size. A badge whose slot could
 * shrink, wrap or push the seat's name onto a second line would change a
 * card's height on a live push, twice per tool-loop round, and every card
 * beside it would move. jsdom has no layout to catch that, so the rules that
 * prevent it are read from the stylesheet.
 *
 * WHAT A RULE'S TEXT CANNOT SAY is whether it reaches the element this screen
 * renders, whether a later rule beats it, and whether it restates something
 * the design system already set there. That is `TableView.drawing.test.tsx`,
 * which puts the shipped stylesheet and this one into one document and reads
 * the cascade over the table this screen actually draws.
 */

import { readFileSync } from "node:fs";
import { fileURLToPath } from "node:url";
import { expect, test } from "vitest";

/*
 * THE SCREEN'S STYLESHEET IS `screens.css` IN THIS TREE. The builder is
 * Company › Structure (`#/company/org`), and every screen's recipes
 * live in one sheet here, so the builder's sit beside the chart's rather than
 * in a file of their own — which is what `org.css` was before the two screens
 * became one. Reading the whole sheet costs nothing: every assertion below is
 * keyed on a selector this screen writes.
 */
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

test("the live state's slot neither shrinks nor wraps", () => {
  expect(rule(".bnode-state")).toMatch(/flex:\s*none/);
  expect(rule(".bnode-state")).toMatch(/white-space:\s*nowrap/);
});

/*
 * THE HANDLES ON THE TABLE'S CELLS DECLARE NOTHING, and that is the whole of
 * what they are for: `.btable-name` is how this suite finds a row by its NAME
 * cell rather than by whichever cell happens to mention a name. It declared
 * display, align-items, gap and min-width, and `.crewlet-org-label--row`
 * arrives on that same element through `className` and declares the same four,
 * byte for byte; `.btable-name > :first-child { flex: none }` restated the
 * package's own rule for the icon, and `.btable-label { min-width: 0 }` was
 * inert, since the span is not a flex item and the ellipsis lives on the
 * package's name element. An absence is the one thing a rendered document
 * cannot show (a restatement that agrees computes to the same answer), so it
 * is asserted here, over the text.
 */
test("the handles this screen puts on the table's cells draw nothing", () => {
  for (const handle of [".btable-name", ".btable-label", ".btable-name > :first-child"]) {
    expect(rule(handle), handle).toBe("");
  }
});

/*
 * A WIRING MARK KEEPS ITS SIZE TOO. It is a glyph on a chart node's caption
 * and on a table row's, and both are one rank tall: a mark that could shrink
 * or grow would relay the chart and change a row's height with its wiring. The
 * name beside it truncating, and the slot the live state and the count sit in,
 * are the design system's `OrgNodeLabel` and `OrgTableName` now, with the
 * suites that read them; this is the one part of that promise this screen
 * still draws for itself.
 */
/*
 * A STATUS'S SECOND LINE IS NOT LARGER THAN ITS FIRST. The kit's two-line cell
 * sets its caption at a fixed step, and this column's values are a step smaller.
 * Mutation: drop the rule, and "213 waiting" reads larger than "Working" over it.
 */
test("a status's second line keeps the cell's size", () => {
  expect(rule(".btable-value .crewlet-text--caption")).toMatch(/font-size:\s*inherit/);
});

/*
 * A FOUND SEAT IS ITS NAME OVER WHERE IT SITS, whole. On the name's line the kit's
 * hint was cut partway through its team. Mutation: leave the hint on the name's line.
 */
test("a finder's option draws its hint on a line of its own", () => {
  expect(rule(".agents-find .crewlet-listbox__option")).toMatch(/flex-wrap:\s*wrap/);
  const hint = rule(".agents-find .crewlet-listbox__hint");
  expect(hint).toMatch(/flex:\s*0 1 100%/);
  expect(hint).toMatch(/white-space:\s*normal/);
  expect(hint).toMatch(/margin-left:\s*0/);
});

/*
 * ON A PHONE THE LIST SPANS ITS FIELD, which the frame gives the whole line
 * (`.page-find`), so it stays inside the screen: as wide as what it said, it ran 34px
 * past the edge. Mutation: drop the phone's rule.
 */
test("on a phone the finder's list spans its field", () => {
  const list = rule(".agents-find .crewlet-combobox__panel");
  expect(list).toMatch(/left:\s*0/);
  expect(list).toMatch(/right:\s*0/);
  expect(list).toMatch(/max-width:\s*none/);
});

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
 * by hand, which is `OrgTable` over `TreeGrid` now.
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
 * A status, data or phase hue, under the names THIS TREE gives them.
 *
 * Spelled as a pattern over the FAMILY rather than over each token, and paired
 * with the case below, because this guard was already dead twice: it matched
 * `--data-*` while the tokens were named `--viz-*`, and then it matched only
 * the design system's names while every sheet here was written against short
 * aliases of them, so it could not match a single declaration and passed on
 * everything. There is no alias layer now: every sheet reads the design
 * system's `--color-feedback-*` and `--color-data-*` under their own names,
 * which is the one spelling this has to know.
 */
const CARRIED_HUE = /var\(--color-(?:feedback|data)-[-\w]*\)/;

/*
 * NOTHING HERE PAINTS A BUILDER NODE BY WHAT IT HOLDS, and nothing anywhere
 * else does either: every node is the chart's neutral surface (the canvas asks
 * the design system for no `cardTone`, which `CanvasView.test.tsx`'s "colour
 * is not identity" holds), so the only colour a builder card or row takes is
 * the accent of the selection.
 */
test("a builder node or row takes no status or data hue, only the accent for the selection", () => {
  const builder = rules(css).filter(([selector]) => /\.(bchart|btable|bnode)/.test(selector));
  // The filter really reaches this screen's own rules: a pattern that matched
  // nothing would make the guard below pass on an empty list.
  expect(builder.map(([selector]) => selector)).toEqual(
    expect.arrayContaining([".bchart", ".bnode-state", ".bnode-mark"]),
  );
  const hues = builder.filter(([, body]) => CARRIED_HUE.test(body)).map(([selector]) => selector);
  expect(hues).toEqual([]);
});

test("the hue guard recognises a hue, and lets the accent through", () => {
  expect(CARRIED_HUE.test("color: var(--color-feedback-danger-ink);")).toBe(true);
  expect(CARRIED_HUE.test("box-shadow: inset 2px 0 0 var(--color-feedback-warning);")).toBe(true);
  expect(CARRIED_HUE.test("background: var(--color-data-3);")).toBe(true);
  expect(CARRIED_HUE.test("border-color: var(--color-data-other);")).toBe(true);
  // Where the reader is, which is the one colour a builder card may carry.
  expect(CARRIED_HUE.test("background: var(--color-brand-accent-soft);")).toBe(false);
  expect(CARRIED_HUE.test("color: var(--color-text-tertiary);")).toBe(false);
  expect(CARRIED_HUE.test("border: 1px solid var(--color-border-default);")).toBe(false);
});

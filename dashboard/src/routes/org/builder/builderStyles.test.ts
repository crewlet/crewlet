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
 * THE SCREEN'S STYLESHEET IS `screens.css` IN THIS TREE. The builder is a lens
 * of the company screen (`#/company?lens=builder`), and every screen's recipes
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

test("the live state and problem count slots neither shrink nor wrap", () => {
  for (const slot of [".bnode-state", ".bnode-count"]) {
    expect(rule(slot), slot).toMatch(/flex:\s*none/);
    expect(rule(slot), slot).toMatch(/white-space:\s*nowrap/);
  }
});

/*
 * THE HANDLES ON THE TABLE'S CELLS DECLARE NOTHING, and that is the whole of
 * what they are for: `.btable-name` is how this suite finds a row by its NAME
 * cell rather than by whichever cell happens to mention a name. It declared
 * display, align-items, gap and min-width, and `.crewlet-org-table__node`
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
 * `--data-*` while the tokens were named `--viz-*`, and then it matched ONLY
 * the design system's canonical `--color-feedback-*` / `--color-data-*` names,
 * which no stylesheet in this tree writes — every sheet here is written
 * against the short aliases `styles/uilet.css` declares (`--critical`,
 * `--info`, `--phase-execute`), so the canonical spelling could not match a
 * single declaration and the guard passed on everything.
 *
 * BOTH SPELLINGS, therefore. The alias is what a rule here would be written
 * with today and the canonical name is what it resolves to, so a rule reaching
 * for a hue is caught whichever way its author spelled it — and the day the
 * aliases go, this guard does not quietly stop working.
 */
const CARRIED_HUE =
  /var\(--(?:critical|caution|positive|info|phase)[-\w]*\)|var\(--color-(?:feedback|data|phase)-[-\w]*\)/;

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
  // The filter really reaches this screen's own rules: a pattern that matched
  // nothing would make the guard below pass on an empty list.
  expect(builder.map(([selector]) => selector)).toEqual(
    expect.arrayContaining([".bchart", ".bnode-state,\n.bnode-count", ".bnode-mark"]),
  );
  const hues = builder.filter(([, body]) => CARRIED_HUE.test(body)).map(([selector]) => selector);
  expect(hues).toEqual([]);
});

test("the hue guard recognises a hue, and lets the accent through", () => {
  expect(CARRIED_HUE.test("color: var(--critical-ink);")).toBe(true);
  expect(CARRIED_HUE.test("box-shadow: inset 2px 0 0 var(--caution);")).toBe(true);
  expect(CARRIED_HUE.test("background: var(--color-data-3);")).toBe(true);
  expect(CARRIED_HUE.test("border-color: var(--phase-execute-ink);")).toBe(true);
  expect(CARRIED_HUE.test("color: var(--color-feedback-danger-ink);")).toBe(true);
  // Where the reader is, which is the one colour a builder card may carry.
  expect(CARRIED_HUE.test("background: var(--accent-soft);")).toBe(false);
  expect(CARRIED_HUE.test("color: var(--text-muted);")).toBe(false);
  expect(CARRIED_HUE.test("border: 1px solid var(--border-subtle);")).toBe(false);
});

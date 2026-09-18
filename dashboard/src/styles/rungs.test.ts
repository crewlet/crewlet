// @vitest-environment node
import { readdirSync, readFileSync } from "node:fs";
import { join } from "node:path";
import { fileURLToPath } from "node:url";
import { describe, expect, test } from "vitest";

/**
 * WHICH RUNG OF A COLOUR IS SPENT AS TEXT, which `palette.test.ts` cannot see.
 *
 * Every status family is three rungs: the base is what a thing is painted
 * WITH — a primary button, a status dot, a meter bar — the `-soft` is the
 * ground it tints, and the `-ink` is the one of the three that is a TEXT
 * colour, moved off the hue's own lightness until it can be read. The design
 * system measures the pairs it PUBLISHES and finds them all clean; nothing
 * measures which rung this application then spends where, and both mistakes
 * are silent: a valid stylesheet, no warning, and a suite that renders the
 * frame stays green because jsdom computes no cascade.
 *
 * Both had happened. Five rules spent `--accent` as text — the rail's current
 * row, the sidebar's, a grid's more-row and two hover states — against nine
 * that already took `--accent-ink`, so the drift was visible in the file
 * before it was visible on a screen. Measured in a browser: 3.09:1 on the soft
 * ground the first two also carry and 3.71 on the page, against the 4.5 small
 * text needs, where the ink gives 5.96 and 7.15.
 *
 * And the rail's attention badge took the one pairing of the three the set
 * does not offer — the ink ON the fill — at 1.72:1 on a 9px semibold digit.
 * The unread count was a dot.
 *
 * KEYED ON THE DECLARATION rather than a rendered pixel, for the reason every
 * gate in this directory gives: a screenshot is the only other witness.
 */

const STYLES = fileURLToPath(new URL(".", import.meta.url));

/** The families that publish an ink rung. */
const FILLS = ["accent", "positive", "caution", "critical", "info"];

const SHEETS = readdirSync(STYLES).filter((f) => f.endsWith(".css"));

/**
 * A rule that spends a fill on something that is NOT text, and the reason.
 *
 * Keyed on the selector, with a `why` that has to hold: an entry nothing
 * matches fails as loudly as a site nothing excuses, so a mark that becomes
 * text — or a rule that is deleted — comes back here to be re-justified rather
 * than quietly widening the exemption.
 */
const ALLOWED: { selector: string; why: string }[] = [
  ...["positive", "caution", "critical", "info"].map((tone) => ({
    selector: `.cell-status.${tone} .cell-glyph`,
    why:
      "A MARK, NOT TEXT. `cells.tsx` renders this glyph `aria-hidden` beside " +
      "the visible label it reinforces, so it carries no meaning of its own " +
      "and takes the 3:1 non-text floor rather than 4.5 — which every one of " +
      "the four clears. The fill is also the right rung for a drawn mark: " +
      "`.dot.critical` and the meter bars take the same one, and an ink here " +
      "would make the glyph a different hue from the dot meaning the same thing.",
  })),
];

/** Rules with a declaration block, comments stripped. */
function rules(css: string): { selector: string; body: string }[] {
  const bare = css.replace(/\/\*[\s\S]*?\*\//g, "");
  return [...bare.matchAll(/([^{}]+)\{([^{}]*)\}/g)].map((m) => ({
    selector: m[1]!.trim().replace(/\s+/g, " "),
    body: m[2]!,
  }));
}

/**
 * A `color:` declaration naming a fill directly.
 *
 * `border-color` and `accent-color` are deliberately not matched: a border and
 * a form control's tick are non-text, take the 3:1 floor rather than 4.5, and
 * the fill is the right rung for both.
 */
function fillAsText(css: string): string[] {
  const out: string[] = [];
  for (const rule of rules(css)) {
    for (const fill of FILLS) {
      if (new RegExp(`(^|[;\\s])color:\\s*var\\(--${fill}\\)`).test(rule.body)) {
        out.push(`${rule.selector} — color: var(--${fill})`);
      }
    }
  }
  return out;
}

/** An `-ink` painted over its own family's solid fill. */
function inkOnFill(css: string): string[] {
  const out: string[] = [];
  for (const rule of rules(css)) {
    for (const fill of FILLS) {
      if (!new RegExp(`color:\\s*var\\(--${fill}-ink\\)`).test(rule.body)) continue;
      if (new RegExp(`background(-color)?:\\s*var\\(--${fill}\\)`).test(rule.body)) {
        out.push(`${rule.selector} — var(--${fill}-ink) on var(--${fill})`);
      }
    }
  }
  return out;
}

describe("a fill is not an ink", () => {
  const found = SHEETS.flatMap((name) =>
    fillAsText(readFileSync(join(STYLES, name), "utf8")).map((o) => `${name}: ${o}`),
  );

  test("text takes the ink, never the fill it is drawn beside", () => {
    const excused = new Set(ALLOWED.map((a) => a.selector));
    expect(
      found.filter((o) => !excused.has(o.split(" — ")[0]!.split(": ").slice(1).join(": "))),
      "a fill is the hue at its own lightness; the `-ink` rung is the readable one",
    ).toEqual([]);
  });

  // THE OTHER SIDE. An exemption that stopped matching is one nobody would
  // ever notice had gone stale, and it is the half that keeps the list from
  // growing into a blanket.
  test("every exemption still has a site to excuse", () => {
    const seen = new Set(found.map((o) => o.split(" — ")[0]!.split(": ").slice(1).join(": ")));
    expect(
      ALLOWED.filter((a) => !seen.has(a.selector)).map((a) => a.selector),
      "these rules no longer spend a fill as text — drop the entry",
    ).toEqual([]);
    for (const entry of ALLOWED) expect(entry.why.length).toBeGreaterThan(40);
  });

  test("an ink is never put on the fill of its own family", () => {
    const offenders = SHEETS.flatMap((name) =>
      inkOnFill(readFileSync(join(STYLES, name), "utf8")).map((o) => `${name}: ${o}`),
    );
    expect(
      offenders,
      "an `-ink` is the ink for its `-soft`; on the solid fill it is the one pairing of the three nobody measured",
    ).toEqual([]);
  });

  // A VACUITY FLOOR. A broken regex finds nothing, which is indistinguishable
  // from a clean tree by either assertion above.
  test("the scan reads a rule at all, and only the wrong ones", () => {
    expect(fillAsText(".a { color: var(--accent); }")).toHaveLength(1);
    expect(fillAsText(".a { color: var(--accent-ink); }")).toHaveLength(0);
    expect(fillAsText(".a { border-color: var(--accent); }")).toHaveLength(0);
    expect(fillAsText(".a { accent-color: var(--accent); }")).toHaveLength(0);
    expect(fillAsText(".a { background: var(--caution); }")).toHaveLength(0);
    expect(inkOnFill(".a { background: var(--caution); color: var(--caution-ink); }")).toHaveLength(
      1,
    );
    expect(
      inkOnFill(".a { background: var(--caution-soft); color: var(--caution-ink); }"),
    ).toHaveLength(0);
    // AND IT READS THE SHEETS, not an empty directory: a resolve that returned
    // nothing would pass both rules above on no input at all.
    expect(SHEETS.length).toBeGreaterThan(4);
  });
});

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

/**
 * A MARK, ONLY. `--text-faint` is about 3:1 against the ground in BOTH themes —
 * #8f8f95 on white is 3.22, #686b70 on the dark ground 3.35 — which is the
 * floor WCAG sets for a NON-TEXT mark and not the 4.5 a word needs.
 *
 * `uilet.css` has said "DECORATION ONLY" at the alias since it was written, and
 * nothing measured it: the rung had decayed onto twenty-six rules. Every grid
 * column head, every sidebar section name and sub-label, the state bar's facts,
 * a fact's label and its provenance, the properties rail's section heads, the
 * palette's group heads and its own scope footer — the one thing the palette
 * doc says must be readable, since "a sigil nobody is told about is a feature
 * that does not exist" — and the empty states, "No goal has been set."
 * included. All at three to one, in both themes, at 11px.
 *
 * What is left is ten marks, named here with the reason each is one. Two-sided:
 * a new rule reaching for this rung fails until somebody writes down why it is
 * a mark, and an entry whose rule is gone fails so the list cannot outlive what
 * it excuses.
 */
describe("the faint rung is decoration", () => {
  const MARKS: { selector: string; why: string }[] = [
    {
      selector: ".faint",
      why:
        "The utility, and both call sites are drawings: `Trace.tsx`'s `└` tree " +
        "branch and `Seat.tsx`'s `→` between the entries of a fallback chain, " +
        'which carries `title="falls back to"` for the meaning.',
    },
    {
      selector: ".rail-lock",
      why: 'A KeyGlyph with `title="needs an operator credential"`. An icon, and the title is the text.',
    },
    {
      selector: ".rail-collapse",
      why:
        "An icon-only button — a chevron with an `aria-label`. A UI component " +
        "takes the 3:1 floor rather than 4.5, it clears it, and `:hover` takes " +
        "it to `--text`.",
    },
    {
      selector: ".side-twist",
      why: "The sidebar tree's disclosure chevron: the same icon-only control.",
    },
    {
      selector: ".crumb-sep",
      why: "The `/` between breadcrumb parts. A separator draws a boundary the layout already has.",
    },
    {
      selector: ".cell-status.neutral .cell-glyph",
      why:
        "`cells.tsx` renders this glyph `aria-hidden` beside the visible label " +
        "it reinforces — the same four marks the fill rule above excuses, for " +
        "the same reason.",
    },
    {
      selector: ".work-type",
      why: "A work item's type mark: a `Mark` with an `.sr-only` name beside it and a `title`.",
    },
    {
      selector: ".work-nobody",
      why:
        'The unassigned mark: a PersonGlyph with an `.sr-only` "Unassigned". ' +
        "Where the name is also drawn it is a `.muted` span of its own, so this " +
        "rung paints the glyph and nothing else.",
    },
    { selector: ".work-hist-row > svg", why: "The history row's icon — an SVG, never a word." },
    {
      selector: ".pulse-glyph",
      why: "The pulse row's leading icon, likewise: a drawing beside the sentence that says what happened.",
    },
  ];

  const faint = SHEETS.flatMap((name) => {
    const css = readFileSync(join(STYLES, name), "utf8");
    return rules(css)
      .filter((r) => /(^|[;\s])color:\s*var\(--text-faint\)/.test(r.body))
      .map((r) => r.selector);
  });

  test("nothing paints a word with it", () => {
    const excused = new Set(MARKS.map((m) => m.selector));
    expect(
      faint.filter((sel) => !excused.has(sel)),
      "this rung is about 3:1 in both themes — a word takes `--text-muted`",
    ).toEqual([]);
  });

  test("every mark named is still a mark that exists", () => {
    const seen = new Set(faint);
    expect(
      MARKS.filter((m) => !seen.has(m.selector)).map((m) => m.selector),
      "these no longer take the faint rung — drop the entry",
    ).toEqual([]);
    for (const mark of MARKS) expect(mark.why.length).toBeGreaterThan(40);
  });

  test("and the scan found the rung at all", () => {
    expect(faint.length).toBe(MARKS.length);
    expect(faint.length).toBeGreaterThan(5);
  });
});

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

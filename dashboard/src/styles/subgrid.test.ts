// @vitest-environment node
import { readFileSync, readdirSync } from "node:fs";
import { join } from "node:path";
import { fileURLToPath } from "node:url";
import { expect, test } from "vitest";

/**
 * A SUBGRID ITEM'S OWN PADDING COMES OUT OF THE TRACKS IT SPANS.
 *
 * A row that takes its columns from the list with `grid-template-columns:
 * subgrid` is still a box with padding, and the two are not independent: the
 * item's margin, border and padding at the start and end edges are SUBTRACTED
 * from its first and last tracks. The tracks do not move — the ones in between
 * stay exactly where the parent put them — so the arithmetic lands entirely on
 * the two at the ends.
 *
 * A browser gives it back where the track is CONTENT-BASED: the subgrid's inset
 * is part of what it contributes to sizing an `auto` track, so the trailing
 * column simply comes out that much wider. It cannot give it back to a FIXED
 * one. `.work-rows` opened with a bare `16px` for the priority mark while
 * `.work-row` padded itself by `--row-inline`, which is `--space-4` — 16px, the
 * same number — so the cell resolved to exactly zero wide. The mark overflowed
 * into the gutter and came to rest on the key: measured at 1400px, a mark
 * spanning [37,49] against a key starting at 49, so `↑LEAD-3` read as one
 * identifier with a stray character on the front. The phone list had the same
 * shape with worse consequences — there the priority cell is `display: none`,
 * so the KEY was the item in track one and `PROD-4` painted over the status
 * pill beside it.
 *
 * # Why it is a scan and not a lookup
 *
 * Nothing in either rule mentions the other. The padding is on the row in one
 * declaration, the track list is on the list in another, and they can be edited
 * a year apart by people who never see them together — which is exactly how
 * the same mistake was made twice, once per breakpoint. A case that asserted
 * the track list as written would restate the fix; what this asserts is the
 * RULE the fix follows from, over every subgrid in the sheets.
 *
 * The pairing is the one thing a scan cannot derive, because which grid owns a
 * subgrid's tracks is a fact about the markup. So it is recorded below, and
 * recorded two-sidedly: a subgrid that starts padding itself and is not in the
 * table fails until somebody names its grid, and a table entry whose rule has
 * stopped padding fails as stale. A count would go green the day one was fixed
 * and another appeared.
 */

const STYLES = fileURLToPath(new URL(".", import.meta.url));

/**
 * Which grid owns each padded subgrid's tracks.
 *
 * `.work-row` is `RowList`'s child in `components/work.tsx`; `.work-rows` is
 * the only element between it and the list panel.
 */
const OWNERS: Record<string, string> = {
  ".work-row": ".work-rows",
};

/** Every rule in every sheet, as `[selector, declarations]`. */
function rules(): [string, string][] {
  const out: [string, string][] = [];
  for (const file of readdirSync(STYLES).filter((f) => f.endsWith(".css"))) {
    const bare = readFileSync(join(STYLES, file), "utf8").replace(/\/\*[\s\S]*?\*\//g, "");
    // SPLIT ON THE CLOSING BRACE, for the reason `sticky.test.ts` gives at the
    // same idiom: a regex that matches whole rules has to consume the `}` that
    // precedes one to anchor on it, which leaves the next rule without one and
    // silently reports every other rule.
    for (const chunk of bare.split("}")) {
      const at = chunk.lastIndexOf("{");
      if (at < 0) continue;
      // FROM THE BRACE BEFORE IT, not from the start of the chunk. A rule
      // inside an at-rule shares its chunk with that at-rule's own prelude, so
      // taking everything before the brace reads the selector as
      // `@media (max-width: 860px) { .work-rows` — which every scan of this
      // shape then discards as an at-rule. The phone list is declared in
      // exactly that position and had exactly this bug, so a gate that could
      // not see it would have certified one breakpoint of two.
      const selector = chunk
        .slice(chunk.lastIndexOf("{", at - 1) + 1, at)
        .trim()
        .replace(/\s+/g, " ");
      if (!selector || selector.startsWith("@")) continue;
      out.push([selector, chunk.slice(at + 1)]);
    }
  }
  return out;
}

/** The top-level terms of a value, so `minmax(0, 1fr)` stays one term. */
function terms(value: string): string[] {
  const out: string[] = [];
  let depth = 0;
  let cur = "";
  for (const ch of value.trim()) {
    if (ch === "(") depth++;
    if (ch === ")") depth--;
    if (depth === 0 && /\s/.test(ch)) {
      if (cur) out.push(cur);
      cur = "";
      continue;
    }
    cur += ch;
  }
  if (cur) out.push(cur);
  return out;
}

/**
 * What a rule insets itself by on the inline axis, or "" for nothing.
 *
 * The shorthand's inline term is its second value (or its first, where one
 * value covers both axes), which is the form every rule in these sheets uses.
 */
function inlineInset(body: string): string {
  const longhand = /(?:^|;)\s*(?:padding|margin)-inline\s*:\s*([^;]+)/.exec(body);
  if (longhand) return terms(longhand[1]!)[0] ?? "";
  const side = /(?:^|;)\s*(?:padding|margin)-(?:left|inline-start)\s*:\s*([^;]+)/.exec(body);
  if (side) return terms(side[1]!)[0] ?? "";
  const short = /(?:^|;)\s*(?:padding|margin)\s*:\s*([^;]+)/.exec(body);
  if (!short) return "";
  const parts = terms(short[1]!);
  const inline = parts.length === 1 ? parts[0]! : parts[1]!;
  return inline === "0" || inline === "0px" ? "" : inline;
}

/**
 * A track whose end cell cannot come up short, for one of two reasons.
 *
 * SIZED FROM CONTENT — `auto`, `min-content`, `max-content`, `fit-content()`.
 * The subgrid's own inset is part of what it contributes to sizing the track,
 * so the browser hands it straight back and the column simply comes out that
 * much wider.
 *
 * OR SIZED FROM WHAT IS LEFT — a `fr`. A flexible track takes the slack after
 * every other track is placed, so the row still fits and nothing is pushed
 * into a neighbour; what the inset costs there is that the cell's content box
 * is that much narrower than the track, which on the one flexible column these
 * lists have — the title, which truncates — is invisible. It is also the only
 * honest answer available: `calc(var(--row-inline) + 1fr)` is not a length and
 * no CSS states an inset on a flex track, so a gate that refused one would be
 * a gate nothing could ever satisfy.
 *
 * What is left is a FIXED track, which is the one that resolves to zero or
 * less and overflows into the column beside it.
 */
const absorbs = (track: string) =>
  ["auto", "min-content", "max-content"].includes(track) ||
  track.startsWith("fit-content(") ||
  /(^|[\s,(])[\d.]+fr\s*\)?$/.test(track);

/**
 * What is wrong with one owning grid's track list, given the inset its subgrid
 * child takes out of the ends.
 */
function faults(tracks: string[], inset: string): string[] {
  const out: string[] = [];
  for (const [end, track] of [
    ["first", tracks[0]!],
    ["last", tracks[tracks.length - 1]!],
  ] as const) {
    if (absorbs(track)) continue;
    if (track.includes(inset)) continue;
    out.push(
      `the ${end} track is \`${track}\`, which is neither content-sized nor ` +
        `carries \`${inset}\`: its subgrid child's own inset comes out of it, ` +
        `so the cell there is ${inset} narrower than the track says`,
    );
  }
  return out;
}

/** Every subgrid rule that insets itself on the inline axis. */
function paddedSubgrids(): Map<string, string> {
  const out = new Map<string, string>();
  for (const [selector, body] of rules()) {
    if (!/grid-template-columns\s*:\s*subgrid/.test(body)) continue;
    const inset = inlineInset(body);
    if (inset) out.set(selector, inset);
  }
  return out;
}

/**
 * A track list that is one `var(--x)`, resolved to what `--x` is declared as.
 *
 * A SET DECLARED ONCE AND USED TWICE IS STILL A TRACK LIST. The narrow set is
 * declared beside the wide one — a track list is positional, so two copies
 * that drift hand every cell the wrong column — and read literally this scan
 * saw one opaque term at each end and reported both as faults. Resolving it is
 * what keeps the indirection and the check in the same tree: the alternative
 * was to excuse a `var()` end, which would have made every future set
 * invisible to the gate by spelling it as one.
 *
 * ONE HOP AND NO MORE: a custom property whose value is another is a chain
 * nothing here writes, and a resolver that followed one would need a cycle
 * guard for a case that does not exist.
 */
function resolveVar(tracks: string[]): string[] {
  if (tracks.length !== 1) return tracks;
  const name = /^var\(\s*(--[\w-]+)\s*\)$/.exec(tracks[0]!)?.[1];
  if (!name) return tracks;
  for (const [, body] of rules()) {
    const m = new RegExp(`(?:^|;)\\s*${name}\\s*:\\s*([^;]+)`).exec(body);
    if (m) return terms(m[1]!);
  }
  // DECLARED NOWHERE is a fault of its own, and it stays one term so the
  // check below reports it rather than passing an empty list.
  return tracks;
}

/** Every track list declared for `selector`, base rule and breakpoints alike. */
function trackLists(selector: string): string[][] {
  const out: string[][] = [];
  for (const [sel, body] of rules()) {
    if (sel !== selector) continue;
    const m = /(?:^|;)\s*grid-template-columns\s*:\s*([^;]+)/.exec(body);
    if (m) out.push(resolveVar(terms(m[1]!)));
  }
  return out;
}

test("every padded subgrid in the sheets has its owning grid recorded", () => {
  const found = paddedSubgrids();
  // A GATE OVER AN EMPTY SET CERTIFIES NOTHING, and this one is a scan.
  expect(found.size).toBeGreaterThan(0);
  for (const selector of found.keys()) {
    expect(
      OWNERS[selector],
      `${selector} takes its columns from a subgrid AND insets itself, so its ` +
        `own inset comes out of the first and last tracks it spans. Name the ` +
        `grid that declares those tracks in OWNERS, in styles/subgrid.test.ts.`,
    ).toBeDefined();
  }
  // And the other direction: an entry whose rule stopped padding is stale, and
  // a stale entry is a check that silently stopped checking anything.
  for (const selector of Object.keys(OWNERS)) {
    expect(found.has(selector), `${selector} no longer insets itself — drop it from OWNERS`).toBe(
      true,
    );
  }
});

test("and the tracks at both ends of the owning grid carry that inset", () => {
  const found = paddedSubgrids();
  let checked = 0;
  for (const [selector, inset] of found) {
    const owner = OWNERS[selector]!;
    const lists = trackLists(owner);
    expect(lists.length, `${owner} declares no track list for ${selector} to take`).toBeGreaterThan(
      0,
    );
    for (const tracks of lists) {
      expect(faults(tracks, inset), `${owner} (holding ${selector}): ${tracks.join(" ")}`).toEqual(
        [],
      );
      checked++;
    }
  }
  // BOTH BREAKPOINTS. The desktop list and the phone list each declare their
  // own tracks, and each of them got this wrong independently.
  expect(checked).toBeGreaterThanOrEqual(2);
});

test("and the check can tell — a bare fixed track at either end is reported", () => {
  // THE MUTATION, run rather than described: the track list exactly as it was
  // written before this was fixed, against the inset `.work-row` really takes.
  expect(
    faults(terms("16px 74px auto minmax(0, 1fr) auto auto auto auto"), "var(--row-inline)"),
  ).toHaveLength(1);
  expect(faults(terms("16px 70px minmax(0, 1fr) 24px"), "var(--row-inline)")).toHaveLength(2);
  // And the two shapes that are right come back clean, so the check is not
  // simply refusing every list it is shown.
  expect(faults(terms("calc(var(--row-inline) + 16px) 74px auto"), "var(--row-inline)")).toEqual(
    [],
  );
  expect(faults(terms("auto 74px auto"), "var(--row-inline)")).toEqual([]);
  // A FLEXIBLE END IS CLEAN and a fixed one beside it is still reported, so
  // the `fr` arm widened the predicate by exactly one shape.
  expect(faults(terms("16px 70px auto minmax(0, 1fr)"), "var(--row-inline)")).toHaveLength(1);
  expect(
    faults(terms("calc(var(--row-inline) + 16px) 70px auto minmax(0, 1fr)"), "var(--row-inline)"),
  ).toEqual([]);
  expect(faults(terms("1fr 70px 24px"), "var(--row-inline)")).toHaveLength(1);
});

// AND A SET DECLARED ONCE IS READ THROUGH. `--work-rows-narrow` holds the
// phone and peek track list, and a scan that stopped at the `var()` would
// report two faults on a list that is right — or, worse, none on one that is
// not, the day somebody spells a bare `16px` end behind a name.
test("a track list behind a custom property is resolved before it is checked", () => {
  const lists = trackLists(".work-rows");
  expect(lists.length).toBeGreaterThanOrEqual(2);
  for (const tracks of lists) {
    expect(tracks.length, tracks.join(" ")).toBeGreaterThan(1);
    expect(
      tracks.some((t) => t.startsWith("var(--work-rows")),
      tracks.join(" "),
    ).toBe(false);
  }
});

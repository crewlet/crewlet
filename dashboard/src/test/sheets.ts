/**
 * The dashboard's own stylesheets, read as rules: the one walk every suite
 * about the sheets shares.
 *
 * WHY IT IS ONE WALK. `sticky.test.ts` and `subgrid.test.ts` each carried a
 * copy, and the two had already drifted on the case that matters: a rule
 * nested in an at-rule shares its chunk with that at-rule's prelude, so taking
 * everything before the brace reads its selector as `@media (…) { .thread-list`
 * — which every scan of this shape then discards as an at-rule. `subgrid`'s
 * copy had learned that the hard way (the phone list's track set sat exactly
 * there, and a gate that could not see it certified one breakpoint of two);
 * `sticky`'s had not, so the FIRST rule inside every `@media` and `@container`
 * block was invisible to the "nothing sticks under the toolbar" gate.
 *
 * SPLIT ON THE CLOSING BRACE rather than matching whole rules. A single
 * `/\}\s*([^{}]*)\{([^{}]*)\}/g` has to consume the `}` that precedes a rule in
 * order to anchor on it, which leaves the next rule without one — so it
 * silently reports every OTHER rule.
 *
 * Comments are stripped first, so a comment that names what a rule forbids is
 * never read as the rule. Not a suite itself, and never imported by anything
 * that ships: it reads the filesystem.
 */

import { readFileSync, readdirSync } from "node:fs";
import { join } from "node:path";

/** The stylesheet directory. `process.cwd()` for the reason `source.ts` gives. */
export const STYLES = join(process.cwd(), "src", "styles");

/** One rule of one sheet. */
export interface SheetRule {
  /** The sheet's file name, e.g. `screens.css`. */
  file: string;
  /** The selector, whitespace collapsed to single spaces. */
  selector: string;
  /** The declarations between the braces, comments removed. */
  body: string;
}

/** Every style rule in every sheet, in file and source order. At-rules' own preludes are not rules. */
export function sheetRules(): SheetRule[] {
  const out: SheetRule[] = [];
  for (const file of readdirSync(STYLES)
    .filter((f) => f.endsWith(".css"))
    .sort()) {
    const bare = readFileSync(join(STYLES, file), "utf8").replace(/\/\*[\s\S]*?\*\//g, "");
    for (const chunk of bare.split("}")) {
      const at = chunk.lastIndexOf("{");
      if (at < 0) continue;
      // FROM THE BRACE (or the statement end) BEFORE IT, not from the start of
      // the chunk — see the package note. The `;` covers a statement at-rule
      // (`@import …;`) standing before the first rule of a sheet.
      const from = Math.max(chunk.lastIndexOf("{", at - 1), chunk.lastIndexOf(";", at - 1));
      const selector = chunk
        .slice(from + 1, at)
        .trim()
        .replace(/\s+/g, " ");
      if (!selector || selector.startsWith("@")) continue;
      out.push({ file, selector, body: chunk.slice(at + 1) });
    }
  }
  return out;
}

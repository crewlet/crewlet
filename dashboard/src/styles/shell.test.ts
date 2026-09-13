// @vitest-environment node
/**
 * The shell's text clears the text floor.
 *
 * `--text-faint` is measured at between 2.8:1 and 4.5:1 on a panel
 * (`palette.test.ts`) and declared DECORATION ONLY in `tokens.css`. The shell
 * used it for words a reader has to read: the search footer's instructions,
 * the hint beside each result (a seat's live state, "needs a token"), the
 * group headings in search and the rail, and the rail's "3 live" count. The
 * palette test measures the token, not where it is spent, so nothing caught
 * that. This scans where it is spent.
 */

import { readFileSync } from "node:fs";
import { fileURLToPath } from "node:url";
import { expect, test } from "vitest";

const css = readFileSync(fileURLToPath(new URL("./shell.css", import.meta.url)), "utf8");

/** Every innermost rule as `[selector, declarations]`, comments removed. */
function rules(source: string): [string, string][] {
  const bare = source.replace(/\/\*[\s\S]*?\*\//g, "");
  return [...bare.matchAll(/([^{}]+)\{([^{}]*)\}/g)].map((m) => [m[1]!.trim(), m[2]!]);
}

test("the shell spends --text-faint as a text colour only on icons", () => {
  const offenders = rules(css)
    .filter(([, body]) => /(^|[;\s])color:\s*var\(--text-faint\)/.test(body))
    .flatMap(([selector]) => selector.split(",").map((s) => s.trim()))
    // A glyph beside a label is decoration: the label carries the words.
    .filter((selector) => !/(^|\s|>)svg$/.test(selector));
  expect(offenders).toEqual([]);
});

test("the scan reads the stylesheet it claims to", () => {
  // A parser that found no rules would pass the test above for any file.
  const selectors = rules(css).map(([selector]) => selector);
  expect(selectors).toContain(".palette-foot");
  expect(selectors).toContain(".nav-item > svg");
});

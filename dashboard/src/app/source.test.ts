// @vitest-environment node

/**
 * Rules about the source that no type and no runtime check catches.
 *
 * There is no ESLint in this tree — the gates are prettier, `tsc` and this
 * suite — so a rule that would be a lint elsewhere is a test here, written the
 * way `styles/classes.test.ts` is: read the source, apply the rule, name the
 * file and line.
 */

import { readdirSync, readFileSync, statSync } from "node:fs";
import { join, relative } from "node:path";
import { fileURLToPath } from "node:url";
import { expect, test } from "vitest";

const SRC = fileURLToPath(new URL("..", import.meta.url));

/** Every component source, excluding the suites themselves. */
function sources(): { path: string; text: string }[] {
  const out: { path: string; text: string }[] = [];
  (function walk(dir: string): void {
    for (const entry of readdirSync(dir)) {
      const full = join(dir, entry);
      if (statSync(full).isDirectory()) {
        walk(full);
        continue;
      }
      if (!full.endsWith(".tsx")) continue;
      if (full.includes(".test.")) continue;
      out.push({ path: relative(SRC, full), text: readFileSync(full, "utf8") });
    }
  })(SRC);
  return out;
}

/**
 * A NUMBER IS NOT A GUARD.
 *
 * `{rows.length && <Panel/>}` renders the string "0" when the list is empty,
 * because `0 && x` is `0` and React renders a zero. It is the one JSX mistake
 * that produces no error, no warning and no type failure — the page simply
 * grows a stray digit, unlabelled, in the middle of the content.
 *
 * It shipped: the fleet screen drew a bare `0` beneath its panels whenever
 * nothing was unplaceable, which on a screen an operator reads *looking for a
 * number that is wrong* is the worst possible place for one.
 *
 * The rule is `> 0`, or a ternary. This scans for the guard shapes that are
 * numeric by their own text — anything ending in `.length`, and the count-ish
 * fields this wire format actually carries.
 */
test("no JSX guard is a bare number", () => {
  // `{ <expr> && (` or `{ <expr> && <`, where <expr> ends in a numeric-looking
  // member. `> 0`, `=== 0`, `!== 0` and `> 1` are all fine and excluded by the
  // absence of a comparison in the captured text.
  const guard =
    /\{\s*\(?[^}<>=!]*?\.(length|count|calls|rounds|seats|total|used|unread|primary)\s*(\)\s*)?&&/g;
  const offenders: string[] = [];
  for (const { path, text } of sources()) {
    const lines = text.split("\n");
    lines.forEach((line, i) => {
      guard.lastIndex = 0;
      if (!guard.test(line)) return;
      // A comparison anywhere in the guard makes it a boolean.
      if (/[<>]=?\s*\d|===|!==/.test(line.slice(0, line.indexOf("&&")))) return;
      offenders.push(`${path}:${i + 1} — ${line.trim().slice(0, 90)}`);
    });
  }
  expect(
    offenders,
    'these render the string "0" when the count is zero: compare with `> 0` or use a ternary',
  ).toEqual([]);
});

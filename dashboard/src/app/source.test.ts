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

import { RAIL } from "./nav.ts";

const SRC = fileURLToPath(new URL("..", import.meta.url));

/** Every source file of the given extensions, excluding the suites. */
function sources(exts: string[] = [".tsx"]): { path: string; text: string }[] {
  const out: { path: string; text: string }[] = [];
  (function walk(dir: string): void {
    for (const entry of readdirSync(dir)) {
      const full = join(dir, entry);
      if (statSync(full).isDirectory()) {
        walk(full);
        continue;
      }
      if (!exts.some((ext) => full.endsWith(ext))) continue;
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

/**
 * EVERY INTERNAL LINK NAMES A LIVE ROUTE.
 *
 * `href(["seats", handle])` compiles, renders, and takes the reader to a
 * screen that says "there is no such screen" — a dead link with no error, no
 * warning and no type failure, and the only way to find one is to click it.
 *
 * Nine files carried them after the routes moved: an event's link from a phase
 * card, a seat's from a colleague chip, a page's from a search hit, a turn's
 * from an item's history, the sprint report's from an item. Every one of them
 * is a way OUT of the screen a reader is on, which is the half of navigation
 * the rail cannot provide.
 *
 * The check is on the FIRST SEGMENT, because that is what route dispatch
 * switches on: a literal that names a segment no workspace owns cannot reach
 * a screen whatever follows it.
 *
 * EVERY SPELLING AND BOTH EXTENSIONS. A link is written as `href([...])`, as
 * `nav.to([...])` for one a control performs rather than one a reader points
 * at, or as a bare `path: [...]` on a value some other component turns into
 * one — and the last lives in plain `.ts`, which is how the ENTIRE attention
 * queue kept pointing at `spend`, `fleet`, `runs`, `config` and `seats` after
 * every one of those moved. That is the Inbox's "needs a person" band: ten
 * rows, all of them dead, in a file this gate was not reading.
 *
 * THEN THE SAME THING HAPPENED AGAIN THROUGH THE SPELLING THIS GATE DID NOT
 * READ. `nav.to` was outside the pattern, so the workspace move left eighteen
 * of them behind — every "Trace" button in the product, every "the turn" from
 * an event, a run and a seat's own list, "Its seat" from a turn and a phase,
 * "the run" from a seat, "spend" from Live now and "back to people" from two
 * screens. Each rendered normally and each landed on Not Found, because the
 * failure of a moved route is a screen that says nothing is there rather than
 * a build that says the link is wrong.
 */
test("every link names a segment a workspace owns", () => {
  const owned = new Set(RAIL.flatMap((r) => r.owns));
  const literal = /(?:href\(|nav\.to\(|\bpath:\s*)\[\s*"([a-z0-9_-]+)"/g;
  const dead: string[] = [];
  for (const { path, text } of sources([".tsx", ".ts"])) {
    const lines = text.split("\n");
    lines.forEach((line, i) => {
      literal.lastIndex = 0;
      let m: RegExpExecArray | null;
      while ((m = literal.exec(line))) {
        const head = m[1];
        if (head && !owned.has(head)) dead.push(`${path}:${i + 1} — #/${head}`);
      }
    });
  }
  expect(dead, "these links go to a screen that does not exist").toEqual([]);
});

// @vitest-environment node
import { readdirSync, readFileSync } from "node:fs";
import { join } from "node:path";
import { fileURLToPath } from "node:url";
import { expect, test } from "vitest";

/**
 * `--radius-circle` ROUNDS A SQUARE, AND ONLY A SQUARE.
 *
 * The token is `50%` — "avatars, status dots, circular badges" in its own
 * words — and a percentage radius is taken of EACH side: on a box that is not
 * square it draws an ellipse. The roster's Workload meters took it on an
 * 8px-high track 350px long, so every bar in both themes was a pointed
 * spindle, and the track clipped its fill into a lens. Nothing else noticed: a
 * valid stylesheet, no warning, and jsdom computes no geometry. A bar, a chip
 * or a pill takes `--radius-pill`, which rounds the ends and leaves the length
 * straight.
 *
 * So a rule that spends it must say it draws a square: an equal `width` and
 * `height`, or `aspect-ratio: 1`. Keyed on the declaration, for the reason
 * every gate in this directory gives: a screenshot is the only other witness.
 */

const STYLES = fileURLToPath(new URL(".", import.meta.url));
const SHEETS = readdirSync(STYLES).filter((f) => f.endsWith(".css"));

/** Rules with a declaration block, comments stripped. */
function rules(css: string): { selector: string; body: string }[] {
  const bare = css.replace(/\/\*[\s\S]*?\*\//g, "");
  return [...bare.matchAll(/([^{}]+)\{([^{}]*)\}/g)].map((m) => ({
    selector: m[1]!.trim().replace(/\s+/g, " "),
    body: m[2]!,
  }));
}

/** A declaration's value in a block, or undefined. */
function value(body: string, property: string): string | undefined {
  return new RegExp(`(?:^|[;\\s])${property}:\\s*([^;]+)`).exec(body)?.[1]?.trim();
}

/** Every rule rounding a box that does not say it is square. */
function circlesOnNonSquares(css: string): string[] {
  return rules(css)
    .filter((rule) => /border-radius:\s*var\(--radius-circle\)/.test(rule.body))
    .filter((rule) => {
      const square = value(rule.body, "aspect-ratio") === "1";
      const width = value(rule.body, "width");
      return !square && (width === undefined || width !== value(rule.body, "height"));
    })
    .map((rule) => rule.selector);
}

test("every box rounded with --radius-circle is a square", () => {
  const found = SHEETS.flatMap((sheet) =>
    circlesOnNonSquares(readFileSync(join(STYLES, sheet), "utf8")).map((s) => `${sheet}: ${s}`),
  );
  expect(found).toEqual([]);
});

// THE GATE SEES THE SHAPE IT WAS WRITTEN FOR: the meter the roster drew.
test("a bar rounded as a circle is caught", () => {
  expect(
    circlesOnNonSquares(
      ".wl-track { height: 8px; border-radius: var(--radius-circle); }\n" +
        ".mark { width: 26px; height: 26px; border-radius: var(--radius-circle); }",
    ),
  ).toEqual([".wl-track"]);
});

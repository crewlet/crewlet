// @vitest-environment node
import { readFileSync } from "node:fs";
import { createRequire } from "node:module";
import { join } from "node:path";
import { fileURLToPath } from "node:url";
import { describe, expect, test } from "vitest";
import { contrast, paletteStates, parseHex, type Rgb } from "@crewlethq/tokens/test/palette";

/**
 * WHAT A SCREEN'S TINT ACTUALLY RESOLVES TO, measured against the installed
 * palette rather than asserted from the token's name.
 *
 * The calendar's neighbouring-month cells carried `background: var(--surface-2)`
 * — a rule that had rendered nothing since the day it was written. That token is
 * `--color-surface-topbar-lift`, which is the SAME bytes as
 * `--color-surface-subtle` in every state of the cascade, and
 * `--color-surface-subtle` is what every `Card` paints. So the rule painted the
 * card in the card's own colour, the whole grid came back one flat field, and
 * the leading 31 of the previous month was indistinguishable from the 1st.
 *
 * THE RAMP IS THE ROOT CAUSE, NOT THE CALL SITE, and it was wrong at both ends.
 * `--surface-1` was `--color-surface-topbar`, which is the same bytes as
 * `--color-surface-background` — so the page bar, the rail, the workspace
 * sidebar, every card and every grid painted THE PAGE'S OWN COLOUR and were
 * told apart from it by a 1px border. `--surface-2` was the card's colour, as
 * above. Twenty-two declarations at one end and thirteen at the other, none of
 * which drew anything.
 *
 * The package has exactly three opaque values and the ramp has two names over
 * the two that are not the page: `--surface-panel` and `--surface-raised`. That
 * is what this file measures — every pair, not the one cell a reader happened
 * to notice.
 *
 * ASSERTED IN THE SHEET plus the palette, because jsdom computes no layout and
 * resolves no custom property: no rendered-DOM suite can see a background at
 * all, and a test that asserted "the rule says `--surface-3`" would pass again
 * the moment somebody swapped in another same-as-ground token.
 */

const STYLES = fileURLToPath(new URL(".", import.meta.url));
const require_ = createRequire(import.meta.url);

const sheet = (name: string) => readFileSync(join(STYLES, name), "utf8");

/** The body of the first rule whose selector is exactly `selector`. */
function block(css: string, selector: string): string {
  const at = new RegExp(
    `(^|\\})\\s*${selector.replace(/[.*+?^${}()|[\]\\]/g, "\\$&")}\\s*\\{`,
    "m",
  );
  const m = at.exec(css);
  expect(m, `${selector} is not declared at all`).not.toBeNull();
  const from = m!.index + m![0].length;
  const to = css.indexOf("}", from);
  expect(to, `${selector} has no closing brace`).toBeGreaterThan(-1);
  return css.slice(from, to);
}

/** One declaration's `var(--x)` name, out of a rule body. */
function token(css: string, selector: string, property: string): string {
  const m = new RegExp(`${property}:\\s*var\\((--[a-zA-Z0-9-]+)\\)`).exec(block(css, selector));
  expect(m, `${selector} declares no ${property}`).not.toBeNull();
  return m![1]!;
}

/** Our short name → the package name it aliases, from uilet's one `:root`. */
function aliases(): Map<string, string> {
  const out = new Map<string, string>();
  for (const m of sheet("uilet.css").matchAll(/^\s*(--[\w-]+):\s*var\((--[\w-]+)\)\s*;/gm)) {
    out.set(m[1]!, m[2]!);
  }
  return out;
}

/** The product's three states. `base` is the marketing root; no screen paints it. */
function themes(): [string, Map<string, string>][] {
  const at = (n: string) => readFileSync(require_.resolve(`@crewlethq/tokens/css${n}`), "utf8");
  const states = paletteStates({ tokens: at(""), themes: at("/themes") });
  return Object.entries(states).filter(([name]) => name !== "base");
}

/** A token our sheets spell, as the colour a browser paints for it. */
export function resolved(
  values: Map<string, string>,
  alias: Map<string, string>,
  name: string,
): Rgb {
  const raw = values.get(alias.get(name) ?? name);
  expect(raw, `${name} resolves to nothing`).toBeTruthy();
  const rgb = parseHex(raw!);
  expect(rgb, `${name} is "${raw}", which is not an opaque colour`).not.toBeNull();
  return rgb!;
}

const hex = (c: Rgb) => `${c.r},${c.g},${c.b}`;

/** Every Card in the product paints this, so it is the ground a screen tint lands on. */
const CARD_GROUND = "--color-surface-subtle";

describe("a screen's tint against the ground it lands on", () => {
  // THE ONE THAT GOES RED ON A REVERT. It reads the token the rule names rather
  // than asserting the name, so it also fails for `--bg-sunken`, for any future
  // token that collapses onto the card ground, and for a tokens bump that
  // flattens the ramp.
  test("a neighbouring month's cell is a different colour from the card it sits on", () => {
    const alias = aliases();
    const name = token(sheet("screens.css"), ".work-cal-cell.out", "background");
    for (const [state, values] of themes()) {
      const tint = resolved(values, alias, name);
      const ground = resolved(values, alias, CARD_GROUND);
      expect(
        hex(tint),
        `${state}: ${name} is the card's own colour, so the rule draws nothing`,
      ).not.toBe(hex(ground));
    }
  });

  // THE GROUND ALONE IS NOT THE DISTINCTION: a month beginning on Tuesday opens
  // with a SINGLE out-of-month cell, and one tinted square at the head of a row
  // reads as a day that is highlighted rather than as a day that is elsewhere.
  test("its numeral is a different ink from the month being read, and both stay readable", () => {
    const alias = aliases();
    const css = sheet("screens.css");
    const inMonth = token(css, ".work-cal-day", "color");
    const out = token(css, ".work-cal-cell.out .work-cal-day", "color");
    const ground = token(css, ".work-cal-cell.out", "background");
    for (const [state, values] of themes()) {
      const a = resolved(values, alias, inMonth);
      const b = resolved(values, alias, out);
      expect(hex(a), state).not.toBe(hex(b));
      expect(contrast(a, resolved(values, alias, CARD_GROUND)), state).toBeGreaterThanOrEqual(4.5);
      expect(contrast(b, resolved(values, alias, ground)), state).toBeGreaterThanOrEqual(4.5);
      // A STEP RATHER THAN A ROUNDING.
      expect(contrast(a, b), state).toBeGreaterThanOrEqual(2);
    }
  });

  // EQUAL SPECIFICITY, so source order is the only thing deciding — and today
  // IS an out-of-month cell whenever the reader pages one month on.
  test("the out-of-month ink cannot land on today's pill", () => {
    const css = sheet("screens.css");
    expect(css.indexOf(".work-cal-cell.out .work-cal-day")).toBeLessThan(
      css.indexOf(".work-cal-cell.today .work-cal-day"),
    );
  });

  // THE ROOT-CAUSE CASE, and the one that would have caught all thirty-five
  // dead declarations at once rather than the single cell a reader noticed.
  //
  // A ramp is only a ramp if its rungs are different colours. The package
  // publishes nine surface names over three opaque values, so a name is no
  // evidence at all: `--surface-1` was `topbar`, which is `background`, and
  // `--surface-2` was `topbar-lift`, which is `subtle`. Both parsed, both
  // applied, neither drew anything, and nothing in this suite could tell —
  // because it measured one call site rather than the ladder.
  test("every rung of the ramp is a different colour from every other", () => {
    const alias = aliases();
    const RAMP = ["--bg", "--surface-panel", "--surface-raised"];
    for (const [state, values] of themes()) {
      const seen = new Map<string, string>();
      for (const rung of RAMP) {
        const at = hex(resolved(values, alias, rung));
        const already = seen.get(at);
        expect(
          already,
          `${state}: ${rung} is the same colour as ${already} — a declaration ` +
            `naming either one draws nothing on the other`,
        ).toBeUndefined();
        seen.set(at, rung);
      }
    }
  });

  // AND A CARD STANDS ON RUNG ONE, which is why there is exactly one rung a
  // card can show. Stated here rather than in prose alone: if the package ever
  // moves `Card` onto another value, the sentence every surface comment in
  // this tree rests on stops being true and this is what says so.
  test("a card paints the panel rung, so only the raised one shows inside it", () => {
    const alias = aliases();
    for (const [state, values] of themes()) {
      expect(hex(resolved(values, alias, "--surface-panel")), state).toBe(
        hex(resolved(values, alias, CARD_GROUND)),
      );
    }
  });

  // A VACUITY FLOOR, the house idiom: an empty resolve satisfies the first case
  // perfectly otherwise.
  test("the palette it measures against is the real one", () => {
    const states = themes();
    expect(states).toHaveLength(3);
    for (const [, values] of states) expect(values.size).toBeGreaterThan(20);
    expect(aliases().get("--surface-raised")).toBe("--color-surface-elevated");
  });

  // AND IT CAN TELL. The mutation the cases above are worth nothing without:
  // exercised against a fixture whose tint aliases straight onto the card
  // ground, `resolved` must report the collision.
  test("it reports a tint that collapses onto the card ground", () => {
    const alias = new Map([["--fake-tint", CARD_GROUND]]);
    for (const [, values] of themes()) {
      expect(hex(resolved(values, alias, "--fake-tint"))).toBe(
        hex(resolved(values, alias, CARD_GROUND)),
      );
    }
  });
});

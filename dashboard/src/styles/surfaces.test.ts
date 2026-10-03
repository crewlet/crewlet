// @vitest-environment node
import { readFileSync, readdirSync } from "node:fs";
import { createRequire } from "node:module";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";
import { describe, expect, test } from "vitest";
import { contrast, paletteStates, parseHex, type Rgb } from "@crewlethq/tokens/test/palette";

/**
 * WHAT A SCREEN'S TINT ACTUALLY RESOLVES TO, measured against the installed
 * palette rather than asserted from the token's name.
 *
 * The calendar's neighbouring-month cells once carried a background that had
 * rendered nothing since the day it was written: the token it named was
 * byte-identical to the one every `Card` paints, in every state of the
 * cascade, so the rule painted the card in the card's own colour and the
 * leading 31 of the previous month was indistinguishable from the 1st. The
 * page bar, the rail, the workspace sidebar and every grid were the same
 * mistake at the other end — each painted the page's own colour and was told
 * apart from it by a 1px border.
 *
 * THE RAMP IS THE ROOT CAUSE, NOT THE CALL SITE. The design system publishes
 * FOUR opaque rungs, each a name for where a box stands: `frame` (the window
 * and its navigation), `background` (the sheet everything is read on),
 * `subtle` (a card) and `elevated` (a block inside a card). This file measures
 * every pair, that each rung is painted by the element it names, and that no
 * interaction state paints one — not the one cell a reader happened to notice.
 *
 * ASSERTED IN THE SHEET plus the palette, because jsdom computes no layout and
 * resolves no custom property: no rendered-DOM suite can see a background at
 * all, and a test that asserted "the rule says `--x`" would pass again the
 * moment somebody swapped in another same-as-ground token.
 */

const STYLES = fileURLToPath(new URL(".", import.meta.url));
const require_ = createRequire(import.meta.url);

const sheet = (name: string) => readFileSync(join(STYLES, name), "utf8");

/** Every stylesheet in the tree, for the cases that scan rather than look up. */
const sheets = (): [string, string][] =>
  readdirSync(STYLES)
    .filter((f) => f.endsWith(".css"))
    .map((f) => [f, sheet(f)] as [string, string]);

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

/** The product's three states. `base` is the marketing root; no screen paints it. */
function themes(): [string, Map<string, string>][] {
  const at = (n: string) => readFileSync(require_.resolve(`@crewlethq/tokens/css${n}`), "utf8");
  const states = paletteStates({ tokens: at(""), themes: at("/themes") });
  return Object.entries(states).filter(([name]) => name !== "base");
}

/** A token our sheets spell, as the colour a browser paints for it. */
export function resolved(values: Map<string, string>, name: string): Rgb {
  const raw = values.get(name);
  expect(raw, `${name} resolves to nothing`).toBeTruthy();
  const rgb = parseHex(raw!);
  expect(rgb, `${name} is "${raw}", which is not an opaque colour`).not.toBeNull();
  return rgb!;
}

const hex = (c: Rgb) => `${c.r},${c.g},${c.b}`;

/** Every Card in the product paints this, so it is the ground a screen tint lands on. */
const CARD_GROUND = "--color-surface-subtle";

/** The four structural rungs, bottom to top. */
const RUNGS = [
  "--color-surface-frame",
  "--color-surface-background",
  "--color-surface-subtle",
  "--color-surface-elevated",
];

/** Every pair of rungs that resolves to one colour in a palette state. */
export function collisions(values: Map<string, string>): string[] {
  const seen = new Map<string, string>();
  const out: string[] = [];
  for (const rung of RUNGS) {
    const at = hex(resolved(values, rung));
    const already = seen.get(at);
    if (already) out.push(`${rung} is the same colour as ${already}`);
    seen.set(at, rung);
  }
  return out;
}

/** The rule a kit component's stylesheet paints its root with. */
function kitSheet(component: string): string {
  const dir = dirname(require_.resolve("@crewlethq/ui/styles.css"));
  const file = readdirSync(dir).find((f) => f.startsWith(`${component}-`) && f.endsWith(".css"));
  expect(file, `@crewlethq/ui ships no ${component} stylesheet`).toBeTruthy();
  return readFileSync(join(dir, file!), "utf8");
}

describe("a screen's tint against the ground it lands on", () => {
  // THE ONE THAT GOES RED ON A REVERT. It reads the token the rule names rather
  // than asserting the name, so it also fails for `--bg-sunken`, for any future
  // token that collapses onto the card ground, and for a tokens bump that
  // flattens the ramp.
  test("a neighbouring month's cell is a different colour from the card it sits on", () => {
    const name = token(sheet("screens.css"), ".work-cal-cell.out", "background");
    for (const [state, values] of themes()) {
      const tint = resolved(values, name);
      const ground = resolved(values, CARD_GROUND);
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
    const css = sheet("screens.css");
    const inMonth = token(css, ".work-cal-day", "color");
    const out = token(css, ".work-cal-cell.out .work-cal-day", "color");
    const ground = token(css, ".work-cal-cell.out", "background");
    for (const [state, values] of themes()) {
      const a = resolved(values, inMonth);
      const b = resolved(values, out);
      expect(hex(a), state).not.toBe(hex(b));
      expect(contrast(a, resolved(values, CARD_GROUND)), state).toBeGreaterThanOrEqual(4.5);
      expect(contrast(b, resolved(values, ground)), state).toBeGreaterThanOrEqual(4.5);
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
  // A ramp is only a ramp if its rungs are different colours. A name is no
  // evidence at all — two names over one value both parse, both apply and
  // neither draws anything on the other — so this measures the ladder.
  test("every rung of the ramp is a different colour from every other", () => {
    for (const [state, values] of themes()) {
      expect(
        collisions(values),
        `${state}: a declaration naming either of two equal rungs draws nothing on the other`,
      ).toEqual([]);
    }
  });

  // AND EACH RUNG IS PAINTED BY WHAT IT NAMES. The window and its navigation
  // stand on the frame, the page column is the sheet, and a card is the card
  // rung — which is the kit's own `Card`, read from its stylesheet, because if
  // the package ever moves `Card` onto another value the sentence every
  // surface comment in this tree rests on stops being true and this is what
  // says so.
  //
  // THE WINDOW, THE SIDEBAR AND THE SHEET ARE THE KIT'S `AppShell` now, so
  // they are read from its stylesheet for the same reason `Card` is; what is
  // OURS is the page header, which stands on the sheet and must paint it.
  test("the sidebar paints the frame, the page the sheet, and a card the card rung", () => {
    const shell = kitSheet("AppShell");
    for (const frame of [".crewlet-app-shell", ".crewlet-app-shell__rail"]) {
      expect(token(shell, frame, "background"), frame).toBe("--color-surface-frame");
    }
    for (const page of [".crewlet-app-shell__sheet", ".crewlet-app-shell__topbar"]) {
      expect(token(shell, page, "background"), page).toBe("--color-surface-background");
    }
    expect(token(sheet("frame.css"), ".page-head", "background")).toBe(
      "--color-surface-background",
    );
    expect(token(kitSheet("Card"), ".crewlet-card", "background-color")).toBe(CARD_GROUND);
  });

  // AN INTERACTION STATE IS NOT A RUNG.
  //
  // The structural rungs are opaque and say where a box STANDS; `--surface-
  // hover`, `--surface-active` and `--surface-inset` are translucent overlays
  // and say what is happening TO it. An overlay lifts whatever it lands on, so
  // it is correct wherever the element sits; a rung is correct only against
  // the one ground it was chosen for, and it draws nothing the day that ground
  // moves.
  //
  // Which is exactly what happened: `.int-seat-summary:hover` painted the rung
  // above the page, which worked while `.int-seat-form` stood on the page —
  // and the form moved to the raised rung (it opens inside a modal, and a
  // modal paints the panel one), so the summary's hover became its own
  // parent's colour and the block lost its hover affordance with nothing in
  // the diff to read.
  test("no interaction state paints a structural rung", () => {
    const STATES = /:hover|:focus|:active/;
    const offenders: string[] = [];
    let states = 0;
    for (const [file, css] of sheets()) {
      const bare = css.replace(/\/\*[\s\S]*?\*\//g, " ");
      for (const m of bare.matchAll(/(^|\})\s*([^{}]*)\{([^{}]*)\}/gm)) {
        const selector = m[2]!.trim().replace(/\s+/g, " ");
        if (!STATES.test(selector)) continue;
        const paint = /background(?:-color)?:\s*var\((--[\w-]+)\)/.exec(m[3]!);
        if (!paint) continue;
        states += 1;
        if (RUNGS.includes(paint[1]!)) offenders.push(`${file} ${selector} -> ${paint[1]}`);
      }
    }
    // A VACUITY FLOOR: a scan that matched no interaction state at all would
    // pass this perfectly while checking nothing.
    expect(
      states,
      "no :hover/:focus rule paints anything, so this checked nothing",
    ).toBeGreaterThan(5);
    expect(
      offenders,
      "an overlay lifts any ground; a rung only lifts the one it was picked for",
    ).toEqual([]);
  });

  // A VACUITY FLOOR, the house idiom: an empty resolve satisfies the first case
  // perfectly otherwise.
  test("the palette it measures against is the real one", () => {
    const states = themes();
    expect(states).toHaveLength(3);
    for (const [, values] of states) expect(values.size).toBeGreaterThan(20);
  });

  // AND IT CAN TELL. The mutation the cases above are worth nothing without:
  // a palette in which two rungs collapse must be reported by the same
  // measurement the ladder case makes.
  test("it reports a rung that collapses onto another", () => {
    for (const [, values] of themes()) {
      const flattened = new Map(values);
      flattened.set("--color-surface-elevated", values.get(CARD_GROUND)!);
      expect(collisions(flattened)).toEqual([
        "--color-surface-elevated is the same colour as --color-surface-subtle",
      ]);
    }
  });
});

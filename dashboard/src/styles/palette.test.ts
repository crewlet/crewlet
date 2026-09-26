/**
 * The palette this product paints in clears its contrast floors.
 *
 * # It is uilet's suite now, not ours
 *
 * This file used to carry the measurement itself: 306 lines of rules over
 * `tokens.css`, on top of 183 lines of colour maths in `color.ts` — contrast,
 * ΔE, chroma, the protan/deutan simulation. All of it existed because the
 * palette was ours and nobody else was going to check it.
 *
 * The palette is `@crewlethq/tokens` now, and that package publishes the same
 * gate over its own built sheets: `runPalette({tokens, themes})` measures every
 * rule in all four states of the cascade — base, dark on the bare root, light
 * by media query, light by attribute — and hands back the failures. Keeping a second
 * implementation beside it would be two ideas of what "4.5:1 on every surface
 * it can land on" means, which is the failure `textcut` and `whsec` are named
 * after in this repository.
 *
 * WHAT IS LEFT HERE IS THE ASSERTION, and it is not ceremony: a design system
 * can publish a palette that misses a floor, and the day it does this build
 * fails rather than shipping it. It measures the INSTALLED package, not a
 * fixture — the same bytes the browser gets — so a bump that regresses the
 * palette is a red suite before it is a screenshot somebody squints at.
 *
 * # What this does not cover
 *
 * Nothing of ours, because nothing of ours is a colour: `tokens.css` keeps
 * three layout values and DECLARES no colour and no shadow, which
 * `tokens.test.ts` enforces. The focus ring is the package's `--shadow-focus`
 * and a row's height, a control's and the rail's geometry are its `--size-*`.
 * The chart ramp is GONE too: `--viz-1` … `--viz-5` and
 * `--viz-other` were retired when the last reader moved to `dataColor()` and
 * `DATA_COLOR_OTHER`, so the hues a chart spends are inside what `runPalette`
 * measures now rather than beside it.
 */

import { readFileSync } from "node:fs";
import { createRequire } from "node:module";
import { describe, expect, test } from "vitest";
import { describeFailure, runPalette, tightest } from "@crewlethq/tokens/test/palette";

const require_ = createRequire(import.meta.url);

/** The built sheets, read from the installed package rather than a copy. */
function sources() {
  const at = (name: string) =>
    readFileSync(require_.resolve(`@crewlethq/tokens/css${name}`), "utf8");
  return { tokens: at(""), themes: at("/themes") };
}

describe("the design system's palette", () => {
  test("clears every contrast floor, in every state of the cascade", () => {
    const { failures } = runPalette(sources());
    expect(failures.map(describeFailure)).toEqual([]);
  });

  test("and the suite measured something, rather than passing on nothing", () => {
    // A VACUITY FLOOR. `runPalette` over two empty strings finds no tokens,
    // makes no checks and reports no failures — which is indistinguishable
    // from a clean palette by the assertion above. A resolve that quietly
    // returned the wrong file, or a package that stopped shipping its
    // stylesheets, would land exactly there.
    const { checks, states } = runPalette(sources());
    expect(checks.length).toBeGreaterThan(20);
    expect(Object.keys(states).length).toBeGreaterThan(1);
    for (const values of Object.values(states)) {
      expect(values.size).toBeGreaterThan(20);
    }
  });

  test("the tightest measurement under each rule is reported, not just the verdict", () => {
    // What a reader needs when they are about to change a colour: the margin
    // each rule is passing by, rather than a green tick.
    const worst = tightest(runPalette(sources()).checks);
    expect(worst.length).toBeGreaterThan(0);
    for (const check of worst) expect(check.detail).toBeTruthy();
  });
});

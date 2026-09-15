// @vitest-environment node
/**
 * The palette's promises, measured over the stylesheets that ship.
 *
 * The rule table and the colour maths are `@crewlethq/tokens/test/palette`,
 * the same implementation the tokens package runs over its own build, so the
 * two gates cannot come to disagree about what a floor is. What is here is the
 * reading of the INSTALLED package in import order, and the assertion.
 *
 * WHY THE ENGINE MEASURES A RATIO AT ALL, when the package already does. The
 * dashboard's npm surface is watched by Dependabot and its bumps auto-merge on
 * green, so a tokens release that lowered a contrast ratio would arrive here
 * with nothing in `make check` measuring one: `designSystem.test.ts` scans
 * where a token is SPENT and never what it resolves to. The owner's constraint
 * is that the floors are kept, not that the measurement moved to another
 * repository.
 */

import { readFileSync } from "node:fs";
import { fileURLToPath } from "node:url";
import { describe, expect, test } from "vitest";
import { describeFailure, paletteStates, runPalette } from "@crewlethq/tokens/test/palette";

const read = (name: string) =>
  readFileSync(
    fileURLToPath(new URL(`../../node_modules/@crewlethq/tokens/dist/css/${name}`, import.meta.url)),
    "utf8",
  );

// The order main.tsx imports them in. Both files declare on `:root` and
// neither adds specificity, so the second one is the one that paints; reading
// them the other way round would measure a palette no reader ever sees.
const sources = { tokens: read("tokens.css"), themes: read("themes.css") };

describe("the palette the dashboard installed", () => {
  const { checks, failures } = runPalette(sources);

  test("every rule holds in every theme state", () => {
    expect(failures.map(describeFailure)).toEqual([]);
  });

  test("the suite measured the installed stylesheets", () => {
    // A rule table that read an empty token map reports no failures for any
    // stylesheet at all, which is the one way this file can lie.
    const states = paletteStates(sources);
    expect(Object.keys(states)).toEqual(["base", "light", "dark (media query)", "dark (attribute)"]);
    for (const [name, values] of Object.entries(states)) {
      expect(values.size, `${name} resolved only ${values.size} tokens`).toBeGreaterThan(80);
    }
    expect(states.light?.get("--color-surface-background")).not.toEqual(
      states["dark (attribute)"]?.get("--color-surface-background"),
    );
    expect(checks.length, `only ${checks.length} checks ran`).toBeGreaterThan(400);
  });
});

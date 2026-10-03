/**
 * What a chart CLAIMS when the number behind it is nothing — and what it draws
 * when nobody named a colour.
 *
 * A zero is the one value a chart is most likely to draw dishonestly, because
 * every readability floor a mark carries — "never shorter than this, so a tiny
 * value is still visible" — draws exactly the same mark for zero. The floor is
 * right; applying it to zero is not.
 *
 * AN ABSENT COLOUR IS THE SAME KIND OF LIE, and it is the one this file gained
 * when the ramp moved: a series or a band that names no colour falls back to
 * the design system's, and a fallback left pointing at a token this product no
 * longer publishes resolves to nothing. The mark is then drawn in transparent —
 * present, counted in the peak, and invisible — which no rendered screenshot
 * distinguishes from a series that had no data.
 */

import { cleanup, render } from "~/test/inCase.ts";
import { afterEach, expect, test } from "vitest";
import { dataColor } from "@crewlethq/ui";
import { TimeSeries } from "./charts.tsx";

afterEach(cleanup);

const WINDOW = { from: 0, to: 1000 };

const strokes = (el: HTMLElement) =>
  [...el.querySelectorAll("polyline")].map((p) => p.getAttribute("stroke"));

// A SERIES THAT NAMES NO COLOUR IS DRAWN IN THE RAMP'S OWN, in the ramp's own
// order. This is the assertion that would have caught the retired `--viz-*`
// tokens: the component kept compiling, kept computing a peak, kept drawing a
// polyline, and painted it in a variable nothing defined.
test("a series with no colour of its own takes its place on the data ramp", () => {
  const { container } = render(
    <TimeSeries
      {...WINDOW}
      series={[
        {
          name: "first",
          points: [
            { t: 0, v: 1 },
            { t: 1000, v: 2 },
          ],
        },
        {
          name: "second",
          points: [
            { t: 0, v: 3 },
            { t: 1000, v: 1 },
          ],
        },
      ]}
    />,
  );
  const drawn = strokes(container);
  expect(drawn).toEqual([dataColor(0), dataColor(1)]);
  // And the ramp is a real value, not an empty string a `stroke` would ignore.
  for (const s of drawn) expect(s).toMatch(/^var\(--color-data-/);
});

// FOUR HUES, THEN THE RESIDUAL. The kit's ramp is four data colours and a
// neutral for everything past them — which is what this product's figures
// are designed around: a phase chart draws exactly four bands, and a fifth
// series is "other" rather than a fifth hue nobody chose. Written as the
// literal variables, not through `dataColor`, so a kit bump that grows or
// shrinks the ramp, or wraps it back to the first hue, turns this red rather
// than agreeing with itself.
test("the data ramp is four distinct hues, and a fifth series is the residual", () => {
  const line = (name: string) => ({
    name,
    points: [
      { t: 0, v: 1 },
      { t: 1000, v: 2 },
    ],
  });
  const { container } = render(
    <TimeSeries {...WINDOW} series={["a", "b", "c", "d", "e"].map(line)} />,
  );
  expect(strokes(container)).toEqual([
    "var(--color-data-1)",
    "var(--color-data-2)",
    "var(--color-data-3)",
    "var(--color-data-4)",
    "var(--color-data-other)",
  ]);
});

test("a series that names its own colour keeps it", () => {
  const { container } = render(
    <TimeSeries
      {...WINDOW}
      series={[{ name: "ideal", color: "var(--color-text-secondary)", points: [{ t: 0, v: 1 }] }]}
    />,
  );
  expect(strokes(container)).toEqual(["var(--color-text-secondary)"]);
});

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

import { cleanup, render } from "@testing-library/react";
import { afterEach, expect, test } from "vitest";
import { DATA_COLOR_OTHER, dataColor } from "@crewlethq/ui";
import { StackedTimeSeries, TimeSeries, phaseColor } from "./charts.tsx";

afterEach(cleanup);

const WINDOW = { from: 0, to: 1000 };

const strokes = (el: HTMLElement) =>
  [...el.querySelectorAll("polyline")].map((p) => p.getAttribute("stroke"));

const stacks = (el: HTMLElement) =>
  [...el.querySelectorAll<HTMLElement>(".stackseries-stack")].map((s) => s.style.height);

const segments = (el: HTMLElement) =>
  [...el.querySelectorAll<HTMLElement>(".stackseries-stack > span")].map((s) =>
    s.getAttribute("style"),
  );

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

test("a series that names its own colour keeps it", () => {
  const { container } = render(
    <TimeSeries
      {...WINDOW}
      series={[{ name: "ideal", color: "var(--text-secondary)", points: [{ t: 0, v: 1 }] }]}
    />,
  );
  expect(strokes(container)).toEqual(["var(--text-secondary)"]);
});

// A BAND THE LEGEND DOES NOT NAME IS THE RESIDUAL. A transparent segment still
// takes its share of the column, so a fallback that resolves to nothing leaves
// the stack short by exactly the amount nobody can see.
test("a band outside the legend is drawn in the residual, never in nothing", () => {
  const { container } = render(
    <StackedTimeSeries
      bands={[{ key: "plan", label: "plan", color: dataColor(0) }]}
      buckets={[
        {
          at: "2026-06-14T12:00:00Z",
          total: 10,
          parts: [
            { key: "plan", value: 6 },
            { key: "", value: 4 },
          ],
        },
      ]}
    />,
  );
  const drawn = segments(container);
  expect(drawn).toHaveLength(2);
  expect(drawn[0]).toContain(dataColor(0));
  expect(drawn[1]).toContain(DATA_COLOR_OTHER);
});

// A QUIET BUCKET IS A GAP OF FULL HEIGHT, not a column squeezed out and not a
// column floored to something visible: the engine returns every bucket in the
// window, so an hour in which the company spent nothing has to draw as nothing.
test("a bucket worth nothing draws a stack of no height at all", () => {
  const { container } = render(
    <StackedTimeSeries
      bands={[{ key: "plan", label: "plan", color: dataColor(0) }]}
      buckets={[
        { at: "a", total: 40, parts: [{ key: "plan", value: 40 }] },
        { at: "b", total: 0, parts: [] },
      ]}
    />,
  );
  expect(stacks(container)).toEqual(["100%", "0%"]);
});

// THE GHOST AND THE COLUMN SHARE ONE SCALE, and a period that spent nothing
// draws no ghost rather than a flat mark along the floor: "the previous period
// was tiny" and "there was no previous period" must not render identically.
test("the ghost is drawn against the same peak, and a zero prior draws none", () => {
  const { container } = render(
    <StackedTimeSeries
      bands={[]}
      ghost={[100, 0]}
      buckets={[
        { at: "a", total: 50, parts: [] },
        { at: "b", total: 50, parts: [] },
      ]}
    />,
  );
  const ghosts = [...container.querySelectorAll<HTMLElement>(".stackseries-ghost")];
  expect(ghosts.map((g) => g.style.height)).toEqual(["100%"]);
  // The columns are HALF, because the peak spans the ghost. Scaled to their
  // own maximum they would both draw full and a period that cost twice as
  // much would look the same as the one it is compared with.
  expect(stacks(container)).toEqual(["50%", "50%"]);
});

// PHASE IS NOT A SLOT ON THE RAMP. A data hue means "this series" and would
// name a different phase on every chart that ordered its bands differently, so
// the three phases have tokens of their own — and anything else is the
// residual, which is a colour this product still publishes.
test("each phase takes its own token, and an unknown one takes the residual", () => {
  expect(phaseColor("onboarding")).toBe("var(--phase-onboarding)");
  expect(phaseColor("Execute")).toBe("var(--phase-execute)");
  expect(phaseColor("review")).toBe("var(--phase-review)");
  for (const unknown of ["", "triage", "self_iterate"]) {
    expect(phaseColor(unknown)).toBe(DATA_COLOR_OTHER);
    expect(phaseColor(unknown)).toMatch(/^var\(--/);
  }
  // And never a data hue: a chart colour says "this series", never "execute".
  expect([phaseColor("onboarding"), phaseColor("execute"), phaseColor("review")]).not.toContain(
    dataColor(0),
  );
});

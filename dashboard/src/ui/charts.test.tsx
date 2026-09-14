/**
 * What a chart CLAIMS when the number behind it is nothing.
 *
 * A zero is the one value a chart is most likely to draw dishonestly, because
 * every readability floor a bar or a mark carries — "never thinner than this,
 * so a tiny value is still visible" — draws exactly the same mark for zero.
 * The floor is right; applying it to zero is not.
 */

import { cleanup, render } from "@testing-library/react";
import { afterEach, expect, test } from "vitest";
import { BarList } from "./charts.tsx";

afterEach(cleanup);

const widths = (el: HTMLElement) =>
  [...el.querySelectorAll<HTMLElement>(".bar-fill")].map((b) => b.style.width);

// A ZERO DRAWS NOTHING. The minimum width exists so a value that is merely
// small beside the largest still has a mark — and applied to zero it draws
// the same sliver, so "nobody delivered anything" looks exactly like
// "somebody delivered a little". A sprint report is full of real zeroes: a
// sprint that has just started has delivered nothing, and its velocity bar
// must not suggest otherwise.
test("a zero value draws no bar, and a small one still draws a visible mark", () => {
  const { container } = render(
    <BarList
      data={[
        { label: "none at all", value: 0 },
        { label: "barely any", value: 1 },
        { label: "most of it", value: 400 },
      ]}
    />,
  );
  const drawn = widths(container);
  expect(drawn).toHaveLength(3);
  const [zero, tiny, big] = drawn as [string, string, string];
  expect(zero).toBe("0%");
  // Its true share is 0.25%, which is under a pixel on any real width.
  expect(parseFloat(tiny)).toBeGreaterThanOrEqual(1.5);
  expect(parseFloat(big)).toBe(100);
});

// EVERY VALUE ZERO IS NOT A DIVISION BY ZERO, and it is not a full row
// either: a team that delivered nothing in any sprint has an empty chart,
// which is the honest drawing of it.
test("a set of nothing but zeroes draws no bars at all", () => {
  const { container } = render(
    <BarList
      data={[
        { label: "a", value: 0 },
        { label: "b", value: 0 },
      ]}
    />,
  );
  expect(widths(container)).toEqual(["0%", "0%"]);
});

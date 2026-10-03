/**
 * The ruler's two layout rules: it prints only as many marks as its width has
 * room for, and a mark anchors to the end of the track only when it IS the end.
 */

import { cleanup, render } from "~/test/inCase.ts";
import { afterEach, describe, expect, test } from "vitest";

import { ticks, tickLabel } from "~/lib/waterfall.ts";
import { AXIS_MARK_PX, TimeAxis, marksFor } from "./TimeAxis.tsx";

afterEach(cleanup);

describe("the ruler's width decides its marks", () => {
  test("a phone's track gets two steps, not a laptop's six run together", () => {
    const most = marksFor(125);
    expect(most).toBe(2);
    const { step, marks } = ticks(53_000, most);
    expect(marks.map((t) => tickLabel(t, step))).toEqual(["0", "50s"]);
    // Every mark has its label's room.
    expect(125 / (53_000 / step)).toBeGreaterThanOrEqual(AXIS_MARK_PX);
  });

  test("a wide track gets more marks", () => {
    expect(marksFor(640)).toBe(10);
    expect(marksFor(0)).toBe(2);
  });
});

describe("where a mark anchors", () => {
  function anchors(to: number) {
    const { container } = render(<TimeAxis from={0} to={to} />);
    return [...container.querySelectorAll(".time-axis-mark")].map((m) => [
      m.textContent,
      m.getAttribute("data-anchor"),
    ]);
  }

  test("the last mark short of the end is centred on its instant", () => {
    expect(anchors(49_000)).toEqual([
      ["0", "start"],
      ["10s", "centre"],
      ["20s", "centre"],
      ["30s", "centre"],
      ["40s", "centre"],
    ]);
  });

  test("a mark at the end of the track ends there", () => {
    expect(anchors(50_000).at(-1)).toEqual(["50s", "end"]);
  });
});

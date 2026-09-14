/**
 * What a meter announces, which is not what it draws.
 *
 * Both defects here are invisible in a screenshot and load-bearing to anyone
 * reading the screen through anything but their eyes: a bar with no name is a
 * number with no subject, and a value outside its own declared range is a
 * value the spec gives an assistive technology no rule for rendering.
 */

import { cleanup, render, screen } from "@testing-library/react";
import { afterEach, expect, test } from "vitest";
import { Meter } from "./primitives.tsx";

afterEach(cleanup);

// A METER WITH NO NAME ANNOUNCES AS A NUMBER WITH NO SUBJECT, on screens that
// render several. The visible legend is not the name: two call sites pass none
// at all, and the ones that do pass a reading ("94% of the meter used") rather
// than a noun.
test("a meter carries an accessible name", () => {
  render(<Meter used={40} max={100} ariaLabel="Company token budget" />);
  expect(screen.getByRole("meter", { name: "Company token budget" })).toBeTruthy();
});

// aria-valuenow MAY NOT EXCEED aria-valuemax. The bar's fill was clamped and
// the value was not, so a budget LOWERED under a counter that has already
// spent past it — which is the exact state an operator opens this screen in —
// published an out-of-range value a screen reader may render as anything.
test("a meter spent past its ceiling reports in range, and says the real figures", () => {
  render(<Meter used={130} max={100} ariaLabel="Company token budget" />);
  const meter = screen.getByRole("meter");
  expect(meter.getAttribute("aria-valuenow")).toBe("100");
  expect(meter.getAttribute("aria-valuemax")).toBe("100");
  // CLAMPED, NOT HIDDEN: the overage is the whole message here.
  expect(meter.getAttribute("aria-valuetext")).toBe("130 of 100");
});

// A NEGATIVE READING IS THE OTHER END OF THE SAME RULE.
test("a meter below its floor reports in range too", () => {
  render(<Meter used={-5} max={100} ariaLabel="Company token budget" />);
  expect(screen.getByRole("meter").getAttribute("aria-valuenow")).toBe("0");
});

// NO SCALE, NO METER. `aria-valuemax` defaults to 100 when it is absent or not
// above the minimum, so a meter with an unknown ceiling would announce "0 out
// of 100" — a confident claim that nothing has been spent, where the truth is
// that nobody has said what the limit is.
test("a meter with no ceiling is not announced as an empty one", () => {
  render(<Meter used={0} max={0} ariaLabel="Company token budget" label="Unlimited" />);
  expect(screen.queryByRole("meter")).toBeNull();
  // The legend still carries whatever is known.
  expect(screen.getByText("Unlimited")).toBeTruthy();
});

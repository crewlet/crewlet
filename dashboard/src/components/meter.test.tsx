/**
 * What a meter announces, which is not what it draws.
 *
 * Every defect here is invisible in a screenshot and load-bearing to anyone
 * reading the screen through anything but their eyes: a bar with no name is a
 * number with no subject, and a bar whose fill disagrees with its figures
 * tells a sighted reader and a screen-reader one two different things about
 * the same budget.
 *
 * FOUR SCREENS DRAW ONE, and every one of them draws a budget: the company's
 * on Overview and Spend, a seat's on Seat and in the Spend table's headroom
 * column, a goal's on Goals. A budget is the one figure here that a reader
 * arrives at already over, because lowering a cap under a counter that has
 * spent past it is an ordinary operator gesture, so the cases below are
 * written around that state rather than around a tidy 40 percent.
 *
 * The control is the design system's. These are the properties the dashboard
 * relies on it for and would lose silently on a bump, in the tradition of
 * `uilet.test.tsx`: the accessible name, and the fill agreeing with the
 * figures at both ends of the range.
 *
 * ONE HALF OF THAT CONTRACT IS NOT HELD HERE, and it is written down rather
 * than dropped quietly. [Meter] publishes `aria-valuenow` and `aria-valuemax`
 * raw, so the budget above reaches an assistive technology as 130 against a
 * maximum of 100, which ARIA gives it no rule for rendering. The fill is
 * clamped and the value text carries the real figures, which is what the cases
 * below pin; the number itself is a one-line fix in `@crewlethq/ui`
 * (`Math.min(Math.max(value, 0), max)` on the track, leaving `valueText` to
 * carry the overage) and asserting the wrong behaviour here would make that fix
 * arrive as a red suite.
 *
 * NEITHER IS THE REFUSAL TO DRAW A METER WITH NO CEILING. `max={0}` still
 * renders `role="meter"` with `aria-valuemax="0"`, which announces as a
 * confident claim that nothing has been spent where the truth is that nobody
 * has set a limit. Every call site guards on it instead (Overview's
 * `orgMeter.max > 0`, Spend, Seat and Goals likewise), so nothing is wrong on
 * screen; what is gone is the guard that would catch the next call site
 * forgetting.
 */

import { cleanup, render, screen } from "@testing-library/react";
import { afterEach, expect, test } from "vitest";
import { Meter } from "@crewlethq/ui";

afterEach(cleanup);

/**
 * The bar itself, which carries the fill as its only child.
 *
 * Read by POSITION rather than by class: the class that paints it belongs to
 * uilet and changes on a bump, and a suite that spells one is asserting the
 * package's stylesheet rather than the screen's claim.
 */
function fillOf(meter: HTMLElement): HTMLElement {
  const fill = meter.firstElementChild;
  expect(fill).not.toBeNull();
  return fill as HTMLElement;
}

// A METER WITH NO NAME ANNOUNCES AS A NUMBER WITH NO SUBJECT, on screens that
// render several. The legend IS the name here, linked to the bar rather than
// drawn beside it: the meter this replaced named nothing, so a screen reader
// announced "meter, 62 percent" with no idea of what, and the call sites that
// did pass something passed a reading ("94% of the meter used") rather than a
// noun.
test("a meter is named by its legend", () => {
  render(<Meter value={40} max={100} label="Company token budget" />);
  expect(screen.getByRole("meter", { name: "Company token budget" })).toBeTruthy();
});

// A NAME THE READER CANNOT SEE IS STILL A NAME. The Spend table draws a bar
// inside a cell whose column header already says what it measures, so the
// legend is hidden rather than dropped: dropping it is how a bar in a table
// came to announce as a bare percentage in the first place.
test("a meter with no visible legend keeps its name", () => {
  render(<Meter value={40} max={100} label="Researcher budget" hideLabel valueText="40 of 100" />);
  expect(screen.getByRole("meter", { name: "Researcher budget" })).toBeTruthy();
  expect(screen.queryByText("40 of 100")).toBeNull();
});

// A BUDGET SPENT PAST ITS CEILING IS THE STATE AN OPERATOR OPENS THIS SCREEN
// IN, and the bar cannot draw it: a fill past 100 percent would run out of its
// own track, and a width the CSSOM refuses leaves the bar at whatever it last
// had rather than reading full.
test("a meter spent past its ceiling draws a full bar and says the real figures", () => {
  render(<Meter value={130} max={100} label="Company token budget" valueText="130 of 100" />);
  const meter = screen.getByRole("meter");
  expect(fillOf(meter).style.width).toBe("100%");
  // CLAMPED, NOT HIDDEN: the overage is the whole message, so the figures are
  // what a reader is told rather than the fraction the bar could draw.
  expect(meter.getAttribute("aria-valuetext")).toBe("130 of 100");
  expect(screen.getByText("130 of 100")).toBeTruthy();
});

// A NEGATIVE READING IS THE OTHER END OF THE SAME RULE, and it used not to be
// clamped at all: the fill was handed `width: -5%`, which CSSOM drops, so the
// bar kept whatever width it last had instead of reading empty. A sighted
// reader and a screen-reader one were told different things.
test("a meter below its floor draws an empty bar", () => {
  render(<Meter value={-5} max={100} label="Company token budget" />);
  expect(fillOf(screen.getByRole("meter")).style.width).toBe("0%");
});

// AND THE ORDINARY CASE IS A PROPORTION, or the two above would pass on a
// component whose fill is always one of the ends.
test("a meter between its ends draws the proportion", () => {
  render(<Meter value={25} max={200} label="Company token budget" />);
  expect(fillOf(screen.getByRole("meter")).style.width).toBe("12.5%");
});

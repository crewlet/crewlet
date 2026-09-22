/**
 * What a meter announces, which is not what it draws — and which way full
 * means, which it cannot work out for itself.
 *
 * The ARIA defects here are invisible in a screenshot and load-bearing to
 * anyone reading the screen through anything but their eyes: a bar with no
 * name is a number with no subject, and a value outside its own declared
 * range is a value the spec gives an assistive technology no rule for
 * rendering. The tone defect is the opposite — visible to everyone, and
 * wrong in opposite directions on two screens at once.
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
  render(<Meter used={40} max={100} ariaLabel="Company token budget" fullMeans="spent" />);
  expect(screen.getByRole("meter", { name: "Company token budget" })).toBeTruthy();
});

// aria-valuenow MAY NOT EXCEED aria-valuemax. The bar's fill was clamped and
// the value was not, so a budget LOWERED under a counter that has already
// spent past it — which is the exact state an operator opens this screen in —
// published an out-of-range value a screen reader may render as anything.
test("a meter spent past its ceiling reports in range, and says the real figures", () => {
  render(<Meter used={130} max={100} ariaLabel="Company token budget" fullMeans="spent" />);
  const meter = screen.getByRole("meter");
  expect(meter.getAttribute("aria-valuenow")).toBe("100");
  expect(meter.getAttribute("aria-valuemax")).toBe("100");
  // CLAMPED, NOT HIDDEN: the overage is the whole message here.
  expect(meter.getAttribute("aria-valuetext")).toBe("130 of 100");
});

// A NEGATIVE READING IS THE OTHER END OF THE SAME RULE.
test("a meter below its floor reports in range too", () => {
  render(<Meter used={-5} max={100} ariaLabel="Company token budget" fullMeans="spent" />);
  const meter = screen.getByRole("meter");
  expect(meter.getAttribute("aria-valuenow")).toBe("0");
  // AND THE BAR AGREES WITH THE NUMBER. Only the top end used to be clamped,
  // so the fill was handed `width: -5%` — which CSSOM drops, leaving the bar
  // at whatever width it last had rather than reading empty. A sighted reader
  // and a screen-reader one would have been told different things.
  const fill = meter.querySelector<HTMLElement>(".meter-fill");
  expect(fill).not.toBeNull();
  expect(fill!.style.width).toBe("0%");
});

// NO SCALE, NO METER. `aria-valuemax` defaults to 100 when it is absent or not
// above the minimum, so a meter with an unknown ceiling would announce "0 out
// of 100" — a confident claim that nothing has been spent, where the truth is
// that nobody has said what the limit is.
test("a meter with no ceiling is not announced as an empty one", () => {
  render(
    <Meter used={0} max={0} ariaLabel="Company token budget" label="Unlimited" fullMeans="spent" />,
  );
  expect(screen.queryByRole("meter")).toBeNull();
  // The legend still carries whatever is known.
  expect(screen.getByText("Unlimited")).toBeTruthy();
});

// A BAR AT 100% IS TWO OPPOSITE PIECES OF NEWS. A budget at 100% is refused
// charges; a completion bar at 100% is the thing finished. The tone was
// derived from the fill alone — 75% caution, 100% critical — which is exactly
// right for a budget and exactly backwards for progress: three quarters of the
// way there rendered as a WARNING, and fully achieved would have rendered as
// a CRISIS. The direction cannot be derived from a ratio, so the caller states
// it and the prop is required rather than defaulted — which is what this case
// keeps true now that no screen passes `achieved`.
const toneOf = (el: HTMLElement) => el.querySelector(".meter-fill")?.getAttribute("data-tone");

test("a budget warns as it fills and progress celebrates", () => {
  const spent = (used: number) =>
    toneOf(render(<Meter used={used} max={100} ariaLabel="Budget" fullMeans="spent" />).container);
  const achieved = (used: number) =>
    toneOf(
      render(<Meter used={used} max={100} ariaLabel="Progress" fullMeans="achieved" />).container,
    );

  // A budget: comfortable, close to the line, over it.
  expect(spent(40)).toBe("accent");
  expect(spent(80)).toBe("caution");
  expect(spent(100)).toBe("critical");

  // Progress: nothing short of done is a fault the bar can diagnose, and done
  // is good news.
  expect(achieved(40)).toBe("accent");
  expect(achieved(80)).toBe("accent");
  expect(achieved(100)).toBe("positive");
});

// A CALLER MAY KNOW WHAT THE RATIO DOES NOT — a budget already refusing
// charges is critical at any fill — so an explicit tone still wins over both.
test("an explicit tone overrides the direction", () => {
  const { container } = render(
    <Meter used={10} max={100} ariaLabel="Budget" fullMeans="spent" tone="critical" />,
  );
  expect(toneOf(container)).toBe("critical");
});

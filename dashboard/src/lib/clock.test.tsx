/**
 * The shared clock: one instant for every relative time on screen.
 */

import { act, cleanup, render, screen } from "@testing-library/react";
import { afterEach, expect, test, vi } from "vitest";
import { useNow } from "./clock.ts";

function Now() {
  return <span data-testid="now">{useNow()}</span>;
}

afterEach(() => {
  cleanup();
  vi.useRealTimers();
});

// A STOPPED CLOCK IS NOT A CLOCK: the first reader to mount after every
// subscriber had gone gets the wall clock, not the instant of the last tick —
// which was the moment the module was imported, or hours before.
test("the first reader after the clock stopped reads the wall clock, not the last tick", () => {
  // Mount and unmount once, so the module's instant is from a tick.
  render(<Now />);
  cleanup();
  vi.useFakeTimers({ toFake: ["Date"] });
  for (const at of ["2031-01-01T00:00:00Z", "2020-06-15T12:00:00Z"]) {
    vi.setSystemTime(new Date(at));
    render(<Now />);
    expect(Number(screen.getByTestId("now").textContent)).toBe(Date.parse(at));
    cleanup();
  }
});

// WITHIN A TICK IT IS ONE NUMBER, which is what two reads in one render need.
test("two readers mounted within a tick read the same instant", () => {
  vi.useFakeTimers({ toFake: ["Date"] });
  vi.setSystemTime(new Date("2031-01-01T00:00:00Z"));
  render(
    <>
      <Now />
      <Now />
    </>,
  );
  act(() => {
    vi.setSystemTime(new Date("2031-01-01T00:00:00.500Z"));
  });
  const [a, b] = screen.getAllByTestId("now").map((el) => el.textContent);
  expect(a).toBe(b);
});

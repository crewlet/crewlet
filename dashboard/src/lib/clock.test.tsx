/**
 * The shared clock: one instant for every relative time on screen.
 */

import { act, cleanup, render, screen } from "@testing-library/react";
import { afterEach, expect, test, vi } from "vitest";
import { useNow } from "./clock.ts";
import { afterMicrotasks } from "~/test/settle.ts";

function Now() {
  return <span data-testid="now">{useNow()}</span>;
}

afterEach(() => {
  cleanup();
  vi.useRealTimers();
});

// A STOPPED CLOCK IS NOT A CLOCK: the first reader to mount after every
// subscriber had gone gets the wall clock, not the instant of the last tick —
// which was the moment the module was imported, or hours before. Each mounts
// in a pass of its own, as a reader arriving later does: within one pass the
// clock reads one instant.
test("the first reader after the clock stopped reads the wall clock, not the last tick", async () => {
  // Mount and unmount once, so the module's instant is from a tick.
  render(<Now />);
  cleanup();
  vi.useFakeTimers({ toFake: ["Date"] });
  for (const at of ["2031-01-01T00:00:00Z", "2020-06-15T12:00:00Z"]) {
    vi.setSystemTime(new Date(at));
    await afterMicrotasks();
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

// TWO READS IN ONE RENDER AGREE, EVEN ACROSS A TICK. React reads a snapshot
// twice in a row and warns where the two differ; a stopped clock re-read its
// wall clock on each, so a render whose two reads fell either side of a tick
// read two instants (CI run 41761533850, announcer.test.tsx). The wall clock
// here moves a millisecond per read, from a millisecond short of a tick.
test("a render's two reads of a stopped clock agree when a tick falls between them", async () => {
  vi.useFakeTimers({ toFake: ["Date"] });
  vi.setSystemTime(new Date("2031-01-01T00:00:00Z"));
  render(<Now />);
  cleanup();
  await afterMicrotasks();
  const last = Date.now();
  let read = last + 999;
  const wall = vi.spyOn(Date, "now").mockImplementation(() => read++);
  try {
    render(<Now />);
  } finally {
    wall.mockRestore();
  }
  expect(Number(screen.getByTestId("now").textContent)).toBe(last);
});

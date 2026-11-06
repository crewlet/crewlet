import { act, cleanup, render, screen } from "@testing-library/react";
import { afterEach, beforeEach, expect, test, vi } from "vitest";
import { holdNewSeats, NEW_SEAT_SETTLE_MS, useNewSeats } from "./newSeats.ts";

function Seen() {
  return <p>{[...useNewSeats()].join(",") || "none"}</p>;
}

beforeEach(() => {
  vi.useFakeTimers();
});

afterEach(() => {
  act(() => void vi.runAllTimers());
  cleanup();
  vi.useRealTimers();
});

// HELD ONLY AS LONG AS THE ENGINE MAY NOT HAVE PLACED IT: past two reconcile ticks
// an unplaced seat is what it says. A second save's seats keep their own time.
// Mutation: hold for good, or let a later save's expiry take the earlier one's.
test("a new seat is held until the engine has had time to place it", () => {
  render(<Seen />);
  expect(screen.getByText("none")).toBeDefined();
  act(() => holdNewSeats(["dev"]));
  expect(screen.getByText("dev")).toBeDefined();
  act(() => void vi.advanceTimersByTime(NEW_SEAT_SETTLE_MS / 2));
  act(() => holdNewSeats(["ops"]));
  expect(screen.getByText("dev,ops")).toBeDefined();
  act(() => void vi.advanceTimersByTime(NEW_SEAT_SETTLE_MS / 2));
  expect(screen.getByText("ops")).toBeDefined();
  act(() => void vi.advanceTimersByTime(NEW_SEAT_SETTLE_MS / 2));
  expect(screen.getByText("none")).toBeDefined();
});

test("a save that added no seat holds nothing", () => {
  render(<Seen />);
  act(() => holdNewSeats([]));
  expect(screen.getByText("none")).toBeDefined();
});

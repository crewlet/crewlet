/**
 * Reading again after asking the engine for something.
 *
 * A write returns before its effect is visible: the config surface answers as
 * soon as the revision is stored and activated, and the read that follows can
 * land before the engine has applied it. An operator pressed Connect, the
 * card said Connect, and nothing moved until they refreshed by hand.
 */

import { act, renderHook } from "@testing-library/react";
import { afterEach, beforeEach, expect, test, vi } from "vitest";

import { SETTLE_MS, WATCH_MS, useRecheck } from "./recheck.ts";

beforeEach(() => vi.useFakeTimers());
afterEach(() => vi.useRealTimers());

// THE SECOND READ IS THE ONE THAT LANDS, and it is the whole point: the first
// is honestly the old answer, because the engine has not applied the revision
// it just stored.
test("a write reads now and again once the apply has had a moment", () => {
  const read = vi.fn();
  const { result } = renderHook(() => useRecheck(read));

  // NOTHING UNTIL SOMETHING IS ASKED FOR. A screen nobody is writing through
  // reads on its own slow cadence.
  expect(read).not.toHaveBeenCalled();
  expect(result.current.watching).toBe(false);

  act(() => result.current.watch());
  expect(read).toHaveBeenCalledTimes(1);
  expect(result.current.watching).toBe(true);

  act(() => void vi.advanceTimersByTime(SETTLE_MS));
  expect(read).toHaveBeenCalledTimes(2);
});

// THE QUICK CADENCE IS HELD, AND BOUNDED. A change that never shows up is a
// fault to read about rather than a reason to poll for ever.
test("the window closes on its own", () => {
  const read = vi.fn();
  const { result } = renderHook(() => useRecheck(read));

  act(() => result.current.watch());
  act(() => void vi.advanceTimersByTime(WATCH_MS - 1));
  expect(result.current.watching).toBe(true);

  act(() => void vi.advanceTimersByTime(1));
  expect(result.current.watching).toBe(false);
});

// A POLL THAT OUTLIVES ITS SCREEN is a request nobody will ever read, and a
// state update on a component that has gone.
test("every timer is dropped when the screen goes away", () => {
  const read = vi.fn();
  const { result, unmount } = renderHook(() => useRecheck(read));

  act(() => result.current.watch());
  expect(read).toHaveBeenCalledTimes(1);

  unmount();
  vi.advanceTimersByTime(WATCH_MS * 2);
  expect(read).toHaveBeenCalledTimes(1);
});

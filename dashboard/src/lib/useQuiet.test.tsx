/**
 * A typed value is asked about once it rests, never on every key.
 */

import { act, renderHook } from "@testing-library/react";
import { afterEach, beforeEach, expect, test, vi } from "vitest";
import { QUIET_MS, useQuiet } from "./useQuiet.ts";

beforeEach(() => {
  vi.useFakeTimers();
});

afterEach(() => {
  vi.useRealTimers();
});

test("the first value is settled at once, so a field that opens filled is asked about", () => {
  const { result } = renderHook(() => useQuiet("runbook"));
  expect(result.current).toBe("runbook");
});

test("a value settles only after it has stopped moving for the quiet span", () => {
  const { result, rerender } = renderHook(({ value }) => useQuiet(value), {
    initialProps: { value: "" },
  });
  rerender({ value: "r" });
  act(() => vi.advanceTimersByTime(QUIET_MS - 1));
  rerender({ value: "ru" });
  act(() => vi.advanceTimersByTime(QUIET_MS - 1));
  // Two keys inside the span: nothing has settled yet.
  expect(result.current).toBe("");
  act(() => vi.advanceTimersByTime(1));
  expect(result.current).toBe("ru");
});

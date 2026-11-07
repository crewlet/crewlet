import { renderHook } from "@testing-library/react";
import { expect, test } from "vitest";
import { useStableValue } from "./useStableValue.ts";

// AN UNCHANGED RE-READ IS THE SAME VALUE; a changed one is the new value.
// Mutation: return `value` and the second render hands back a new object.
test("a reading keeps its identity while it says the same thing", () => {
  const first = { seats: [{ handle: "qa", on: false }] };
  const { result, rerender } = renderHook(({ v }) => useStableValue(v), {
    initialProps: { v: first },
  });
  rerender({ v: { seats: [{ handle: "qa", on: false }] } });
  expect(result.current).toBe(first);
  const changed = { seats: [{ handle: "qa", on: true }] };
  rerender({ v: changed });
  expect(result.current).toBe(changed);
});

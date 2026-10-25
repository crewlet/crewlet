/**
 * A gallery's picks: a mode, a range in the drawn order set to the state of
 * the box pressed, and a new question clearing them.
 */

import { act, renderHook } from "@testing-library/react";
import { expect, test } from "vitest";
import { useGallerySelection } from "./selection.ts";

const ORDER = ["a", "b", "c", "d", "e"];

test("picking is a mode the toggle starts, and anything picked keeps it on", () => {
  const { result } = renderHook(() => useGallerySelection(ORDER, "q1"));
  expect(result.current.selecting).toBe(false);
  act(() => result.current.setToggled(true));
  expect(result.current.selecting).toBe(true);
  act(() => result.current.setToggled(false));
  act(() => result.current.pick("b", true, false));
  expect(result.current.selecting).toBe(true);
  expect([...result.current.selected]).toEqual(["b"]);
});

test("Shift picks every card drawn between the last pressed and this one, all one way", () => {
  const { result } = renderHook(() => useGallerySelection(ORDER, "q1"));
  act(() => result.current.pick("b", true, false));
  act(() => result.current.pick("d", true, true));
  expect([...result.current.selected].sort()).toEqual(["b", "c", "d"]);
  act(() => result.current.pick("e", false, true));
  expect([...result.current.selected].sort()).toEqual(["b", "c"]);
});

test("a new question clears the picks and leaves the toggle as the reader set it", () => {
  let question = "q1";
  const { result, rerender } = renderHook(() => useGallerySelection(ORDER, question));
  act(() => result.current.setToggled(true));
  act(() => result.current.pick("a", true, false));
  question = "q2";
  rerender();
  expect(result.current.selected.size).toBe(0);
  expect(result.current.toggled).toBe(true);
  act(() => result.current.clear());
  expect(result.current.toggled).toBe(false);
});

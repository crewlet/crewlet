/**
 * Which edge a panel hangs from, by the window it is drawn in.
 *
 * On a phone the work list's toolbar wraps its Display trigger to the left
 * edge, and an end-aligned panel there closed on the frame it opened (the
 * kit's anchor-gone test reads the end-aligned panel's left edge — see
 * `usePanelAlign`). A phone draws every panel start-aligned; a wider window
 * keeps the caller's choice.
 */

import { renderHook, act } from "@testing-library/react";
import { afterEach, expect, test } from "vitest";

import { usePanelAlign } from "./media.ts";
import { installWindow } from "~/testing.tsx";

let restore: (() => void) | null = null;
afterEach(() => {
  restore?.();
  restore = null;
});

test("a phone draws a panel from its trigger's start, and a wider window as asked", () => {
  const win = installWindow(390);
  restore = win.restore;
  const { result } = renderHook(() => usePanelAlign("end"));
  expect(result.current).toBe("start");
  act(() => win.set(1280));
  expect(result.current).toBe("end");
  // At the phone breakpoint's own edge, both sides.
  act(() => win.set(639));
  expect(result.current).toBe("start");
  act(() => win.set(640));
  expect(result.current).toBe("end");
});

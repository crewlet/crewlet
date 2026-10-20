/**
 * The frame queue the view harness stands in for the browser's with.
 *
 * What these protect: a screen that requests and cancels animation frames
 * meets the browser's contract under the harness — a handle names one request
 * for as long as the page lives, so cancelling a frame that has already run
 * cancels nothing, and cancelling a frame of the batch being run stops it
 * running. A queue that handed a handle out twice let a component's cleanup,
 * cancelling the frame it had requested long before, silently drop whatever
 * frame another component had requested since.
 */

import { afterEach, beforeEach, expect, test, vi } from "vitest";
import { LayoutObserver, runFrames } from "./viewTestkit.tsx";

let restore: () => void;
beforeEach(() => {
  restore = LayoutObserver.install();
});
afterEach(() => {
  restore();
});

test("a handle names one request, so cancelling a frame that ran cancels nothing", () => {
  const first = vi.fn();
  const handle = requestAnimationFrame(first);
  runFrames(0);
  expect(first).toHaveBeenCalledTimes(1);

  const later = vi.fn();
  const next = requestAnimationFrame(later);
  expect(next).not.toBe(handle);
  // What a component's cleanup does when it unmounts after its frame ran.
  cancelAnimationFrame(handle);
  runFrames(16);
  expect(later).toHaveBeenCalledTimes(1);
});

test("a frame cancelled by an earlier frame of its batch does not run", () => {
  const second = vi.fn();
  let secondHandle = 0;
  requestAnimationFrame(() => cancelAnimationFrame(secondHandle));
  secondHandle = requestAnimationFrame(second);
  runFrames(0);
  expect(second).not.toHaveBeenCalled();
});

test("a frame requested by a frame runs at the next one, not in the batch that asked", () => {
  const order: string[] = [];
  requestAnimationFrame(() => {
    order.push("first");
    requestAnimationFrame(() => order.push("asked by first"));
  });
  runFrames(0);
  expect(order).toEqual(["first"]);
  runFrames(16);
  expect(order).toEqual(["first", "asked by first"]);
});

test("each batch is handed the time it is run at", () => {
  const at = vi.fn();
  requestAnimationFrame(at);
  runFrames(1234);
  expect(at).toHaveBeenCalledWith(1234);
});

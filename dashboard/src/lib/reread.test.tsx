/**
 * The one timer a screen's hand-rolled REST read asks itself again on.
 *
 * Two properties hold every caller up: an answer's wait REPLACES whatever an
 * earlier answer armed, so two answers never become two reads, and a screen
 * that has gone asks nothing more — a read nobody will render is waste against
 * a node that may already be struggling.
 */

import { cleanup, render } from "@testing-library/react";
import { afterEach, beforeEach, expect, test, vi } from "vitest";
import { useReread, type Reread } from "./reread.ts";

let held: Reread | null = null;

function Holder() {
  held = useReread();
  return null;
}

beforeEach(() => {
  vi.useFakeTimers();
  held = null;
});

afterEach(() => {
  cleanup();
  vi.useRealTimers();
});

test("an answer's wait replaces the one an earlier answer armed", () => {
  render(<Holder />);
  const read = vi.fn();
  held!.after(5_000, read);
  held!.after(12_000, read);
  vi.advanceTimersByTime(11_999);
  expect(read).not.toHaveBeenCalled();
  vi.advanceTimersByTime(1);
  expect(read).toHaveBeenCalledTimes(1);
  vi.advanceTimersByTime(60_000);
  expect(read).toHaveBeenCalledTimes(1);
});

test("null arms nothing, and disarms what was armed", () => {
  render(<Holder />);
  const read = vi.fn();
  held!.after(5_000, read);
  held!.after(null, read);
  vi.advanceTimersByTime(60_000);
  expect(read).not.toHaveBeenCalled();
});

test("a read that is starting cancels the one that was due", () => {
  render(<Holder />);
  const read = vi.fn();
  held!.after(5_000, read);
  held!.cancel();
  vi.advanceTimersByTime(60_000);
  expect(read).not.toHaveBeenCalled();
});

test("a screen that has gone asks nothing more", () => {
  const view = render(<Holder />);
  const read = vi.fn();
  held!.after(5_000, read);
  view.unmount();
  vi.advanceTimersByTime(60_000);
  expect(read).not.toHaveBeenCalled();
});

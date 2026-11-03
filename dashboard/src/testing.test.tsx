/**
 * The reference renders the suites ask the design system's parts of, and
 * the window a suite stands at.
 *
 * What these protect: every render this file makes on a suite's behalf
 * leaves nothing in the suite's output, so a warning a suite prints is about
 * its own screen. Two kinds used to come from here on every run:
 *
 * - the reference chart's stand-in observer answered from inside
 *   `observe()`, which the canvas calls from a ref callback while React is
 *   committing; the canvas answers a size with a `flushSync`, and React
 *   refused each one with "flushSync was called from inside a lifecycle
 *   method", printed under whichever chart suite read the parts first;
 * - every render here ran inside React's own `act` with no act environment
 *   declared, which React answers with "The current testing environment is
 *   not configured to support act(...)".
 */

import { cleanup, render, screen } from "@testing-library/react";
import { useEffect, useState } from "react";
import { afterEach, expect, test, vi } from "vitest";
import { Tag } from "@crewlethq/ui";
import { drawnClasses, installWindow, treeCanvasParts } from "~/testing.tsx";

afterEach(() => {
  cleanup();
  vi.restoreAllMocks();
});

/** What the console was told while `run` ran, a message a line. */
function errorsDuring(run: () => void): string[] {
  const errors = vi.spyOn(console, "error");
  run();
  return errors.mock.calls.map((call) => String(call[0]));
}

test("the tree canvas's parts are read from a chart laid out without a warning", () => {
  let parts: ReturnType<typeof treeCanvasParts> | null = null;
  expect(errorsDuring(() => (parts = treeCanvasParts()))).toEqual([]);
  // Measured, or it would draw no connectors to name.
  expect(Object.values(parts ?? {}).every((name) => name !== "")).toBe(true);
});

test("the observer the chart is measured with is the suite's own again afterwards", () => {
  const before = globalThis.ResizeObserver;
  treeCanvasParts();
  expect(globalThis.ResizeObserver).toBe(before);
});

test("a component's classes are read without a warning", () => {
  let classes: string[] = [];
  expect(errorsDuring(() => (classes = drawnClasses(Tag, { children: "x" })))).toEqual([]);
  expect(classes.length).toBeGreaterThan(0);
});

/** A screen that follows the window's width, as the shell does. */
function Width() {
  const query = "(width < 1024px)";
  const [narrow, setNarrow] = useState(() => matchMedia(query).matches);
  useEffect(() => {
    const media = matchMedia(query);
    const follow = () => setNarrow(media.matches);
    media.addEventListener("change", follow);
    return () => media.removeEventListener("change", follow);
  }, []);
  return <p>{narrow ? "narrow" : "wide"}</p>;
}

test("moving the window a screen follows re-renders it without a warning", () => {
  const win = installWindow(1280);
  try {
    render(<Width />);
    expect(screen.getByText("wide")).toBeDefined();
    expect(errorsDuring(() => win.set(800))).toEqual([]);
    expect(screen.getByText("narrow")).toBeDefined();
  } finally {
    win.restore();
  }
});

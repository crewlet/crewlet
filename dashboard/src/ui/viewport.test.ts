// @vitest-environment node
/**
 * The viewport's promises, as arithmetic.
 *
 * None of these can be observed in jsdom, which has no layout, so every one of
 * them would otherwise ship on trust: a zoom that drifts off the cursor, a pan
 * that loses the chart, a fit that centres a tree's root off the top of the
 * screen, a reveal that jumps when nothing needed to move.
 */

import { describe, expect, test } from "vitest";
import {
  FIT_MAX_ZOOM,
  MAX_ZOOM,
  MIN_ZOOM,
  PAN_MARGIN,
  WHEEL_LINE_PX,
  ZOOM_STEP,
  anchor,
  beyondSlop,
  clampPan,
  fit,
  pinch,
  placePopup,
  reveal,
  screenRect,
  toScreen,
  toWorld,
  wheelPixels,
  wheelZoomFactor,
  zoomAt,
  type Point,
  type View,
} from "./viewport.ts";

const close = (a: number, b: number) => Math.abs(a - b) < 1e-9;

/** mulberry32: a seeded PRNG, so a failing property names the run that found it. */
function prng(seed: number): () => number {
  let s = seed >>> 0;
  return () => {
    s = (s + 0x6d2b79f5) >>> 0;
    let t = s;
    t = Math.imul(t ^ (t >>> 15), t | 1);
    t ^= t + Math.imul(t ^ (t >>> 7), t | 61);
    return ((t ^ (t >>> 14)) >>> 0) / 4294967296;
  };
}

describe("zoomAt", () => {
  test("the world point under the focus does not move, at any zoom and at the limits", () => {
    const failures: string[] = [];
    for (let seed = 1; seed <= 300; seed++) {
      const rand = prng(seed);
      const view: View = {
        x: rand() * 2000 - 1000,
        y: rand() * 2000 - 1000,
        k: 0.25 + rand() * 1.75,
      };
      const focus: Point = { x: rand() * 1200, y: rand() * 800 };
      // Factors far outside the limits, so clamping is exercised too.
      const factor = Math.pow(2, rand() * 8 - 4);
      const before = toWorld(view, focus);
      const next = zoomAt(view, factor, focus);
      const after = toWorld(next, focus);
      if (!close(before.x, after.x) || !close(before.y, after.y)) failures.push(`seed ${seed}`);
      if (next.k < MIN_ZOOM || next.k > MAX_ZOOM) failures.push(`seed ${seed}: k ${next.k}`);
    }
    expect(failures).toEqual([]);
  });

  test("a step in and a step out return to the same view", () => {
    const view = { x: 40, y: -12, k: 1 };
    const focus = { x: 300, y: 200 };
    const back = zoomAt(zoomAt(view, ZOOM_STEP, focus), 1 / ZOOM_STEP, focus);
    expect(close(back.x, view.x) && close(back.y, view.y) && close(back.k, view.k)).toBe(true);
  });
});

describe("clampPan", () => {
  const content = { x: 0, y: 0, width: 1000, height: 600 };
  const viewport = { width: 800, height: 500 };

  test("a view that keeps the content on screen is left exactly as it was", () => {
    const view = { x: -100, y: 20, k: 1 };
    expect(clampPan(view, content, viewport)).toEqual(view);
  });

  test("dragging the content away leaves a margin of it on every side", () => {
    const right = clampPan({ x: 5000, y: 0, k: 1 }, content, viewport);
    expect(right.x).toBe(viewport.width - PAN_MARGIN);
    const left = clampPan({ x: -5000, y: 0, k: 1 }, content, viewport);
    expect(screenRect(left, content).x + content.width).toBe(PAN_MARGIN);
    const up = clampPan({ x: 0, y: -5000, k: 0.5 }, content, viewport);
    expect(screenRect(up, content).y + content.height * 0.5).toBe(PAN_MARGIN);
  });

  test("content smaller than the margin is kept whole rather than demanded past its size", () => {
    const tiny = { x: 0, y: 0, width: 10, height: 10 };
    const view = clampPan({ x: -500, y: -500, k: 1 }, tiny, viewport);
    expect(screenRect(view, tiny).x).toBe(0);
    expect(screenRect(view, tiny).y).toBe(0);
  });
});

describe("fit", () => {
  test("shows everything, centred, and never enlarges a small tree past actual size", () => {
    const small = fit({ x: 0, y: 0, width: 200, height: 100 }, { width: 800, height: 600 });
    expect(small.k).toBe(FIT_MAX_ZOOM);
    const drawn = screenRect(small, { x: 0, y: 0, width: 200, height: 100 });
    expect(drawn.x + drawn.width / 2).toBe(400);
    expect(drawn.y + drawn.height / 2).toBe(300);
  });

  test("shrinks wide content into the padded room, and respects content that does not start at zero", () => {
    const content = { x: -500, y: 40, width: 2000, height: 300 };
    const view = fit(content, { width: 1048, height: 800 }, 24);
    expect(view.k).toBe(0.5);
    const drawn = screenRect(view, content);
    expect(drawn.x).toBe(24);
    expect(drawn.x + drawn.width).toBe(1024);
  });

  test("content too tall to fit even at the smallest zoom is aligned to its top, where the root is", () => {
    const content = { x: 0, y: 0, width: 400, height: 40000 };
    const view = fit(content, { width: 800, height: 600 }, 24);
    expect(view.k).toBe(MIN_ZOOM);
    expect(screenRect(view, content).y).toBe(24);
  });

  test("an unmeasured viewport yields the identity rather than an infinite zoom", () => {
    expect(fit({ x: 0, y: 0, width: 100, height: 100 }, { width: 0, height: 0 })).toEqual({
      x: 0,
      y: 0,
      k: 1,
    });
  });
});

describe("reveal", () => {
  const viewport = { width: 800, height: 600 };

  test("a target already in view moves nothing", () => {
    const view = { x: -40, y: 30, k: 1.5 };
    expect(reveal(view, { x: 100, y: 100, width: 200, height: 80 }, viewport, 24)).toEqual(view);
  });

  test("a target off the right or bottom is brought just inside the padding", () => {
    const view = reveal(
      { x: 0, y: 0, k: 1 },
      { x: 900, y: 700, width: 200, height: 80 },
      viewport,
      24,
    );
    const drawn = screenRect(view, { x: 900, y: 700, width: 200, height: 80 });
    expect(drawn.x + drawn.width).toBe(776);
    expect(drawn.y + drawn.height).toBe(576);
  });

  test("a target off the left or top is brought just inside, at any zoom", () => {
    const target = { x: -300, y: -200, width: 100, height: 50 };
    const view = reveal({ x: 0, y: 0, k: 0.5 }, target, viewport, 24);
    const drawn = screenRect(view, target);
    expect(drawn.x).toBe(24);
    expect(drawn.y).toBe(24);
  });

  test("a target larger than the viewport shows its start", () => {
    const target = { x: 0, y: 0, width: 3000, height: 3000 };
    const drawn = screenRect(reveal({ x: -900, y: -900, k: 1 }, target, viewport, 24), target);
    expect(drawn.x).toBe(24);
    expect(drawn.y).toBe(24);
  });
});

test("anchor draws the moved world point where the old one was drawn", () => {
  const view = { x: 120, y: -40, k: 0.75 };
  const before = { x: 300, y: 500 };
  const after = { x: 420, y: 460 };
  const next = anchor(view, before, after);
  expect(toScreen(next, after)).toEqual(toScreen(view, before));
});

describe("pinch", () => {
  const start = { view: { x: 0, y: 0, k: 1 }, a: { x: 100, y: 100 }, b: { x: 300, y: 100 } };

  test("spreading the fingers to twice the distance doubles the zoom about the midpoint", () => {
    const next = pinch(start, { x: 0, y: 100 }, { x: 400, y: 100 });
    expect(next.k).toBe(2);
    expect(toScreen(next, toWorld(start.view, { x: 200, y: 100 }))).toEqual({ x: 200, y: 100 });
  });

  test("moving both fingers together pans without zooming", () => {
    const next = pinch(start, { x: 150, y: 160 }, { x: 350, y: 160 });
    expect(next).toEqual({ x: 50, y: 60, k: 1 });
  });

  test("two pointers that began on one spot do not divide by zero", () => {
    const next = pinch({ ...start, b: start.a }, { x: 100, y: 100 }, { x: 500, y: 100 });
    expect(Number.isFinite(next.k) && Number.isFinite(next.x)).toBe(true);
    expect(next.k).toBe(1);
  });

  test("a pinch is clamped to the zoom limits", () => {
    expect(pinch(start, { x: 199, y: 100 }, { x: 201, y: 100 }).k).toBe(MIN_ZOOM);
    expect(pinch(start, { x: -5000, y: 100 }, { x: 5000, y: 100 }).k).toBe(MAX_ZOOM);
  });
});

describe("the wheel", () => {
  test("pixels, lines and pages all come back as pixels", () => {
    expect(wheelPixels({ deltaX: 0, deltaY: 3, deltaMode: 1 }, 700)).toEqual({
      x: 0,
      y: 3 * WHEEL_LINE_PX,
    });
    expect(wheelPixels({ deltaX: 0, deltaY: 1, deltaMode: 2 }, 700)).toEqual({ x: 0, y: 700 });
    expect(wheelPixels({ deltaX: 5, deltaY: 7, deltaMode: 0 }, 700)).toEqual({ x: 5, y: 7 });
  });

  test("Shift turns a vertical wheel into a horizontal pan, unless the platform already did", () => {
    expect(wheelPixels({ deltaX: 0, deltaY: 40, deltaMode: 0, shiftKey: true }, 700)).toEqual({
      x: 40,
      y: 0,
    });
    expect(wheelPixels({ deltaX: 40, deltaY: 0, deltaMode: 0, shiftKey: true }, 700)).toEqual({
      x: 40,
      y: 0,
    });
  });

  test("one mouse wheel notch is one zoom step, and rolling up zooms in", () => {
    expect(close(wheelZoomFactor(-100), ZOOM_STEP)).toBe(true);
    expect(close(wheelZoomFactor(100), 1 / ZOOM_STEP)).toBe(true);
  });
});

test("a finger may roll further than a mouse before a tap becomes a drag", () => {
  const a = { x: 0, y: 0 };
  const b = { x: 6, y: 0 };
  expect(beyondSlop(a, b, "mouse")).toBe(true);
  expect(beyondSlop(a, b, "touch")).toBe(false);
});

describe("placePopup", () => {
  const bounds = { x: 0, y: 0, width: 800, height: 600 };
  const size = { width: 200, height: 160 };

  test("below the anchor and aligned to its start when there is room", () => {
    expect(placePopup({ x: 100, y: 100, width: 32, height: 24 }, size, bounds, 4)).toEqual({
      x: 100,
      y: 128,
    });
  });

  test("above the anchor when there is not room below and more room above", () => {
    expect(placePopup({ x: 100, y: 520, width: 32, height: 24 }, size, bounds, 4)).toEqual({
      x: 100,
      y: 356,
    });
  });

  test("slid back inside the bounds at the right edge", () => {
    expect(placePopup({ x: 780, y: 100, width: 20, height: 24 }, size, bounds, 4).x).toBe(600);
  });
});

// @vitest-environment node
/** A comment picture's region: an element with room around it or a point's window, in bounds. */

import { expect, test } from "vitest";
import { CROP_POINT_SIZE, cropScale, padRegion, pointRegion } from "./crop.ts";

const SLIDE = { w: 1920, h: 1080 };

test("an element is drawn with room around it, inside the drawing", () => {
  expect(padRegion({ x: 100, y: 100, w: 400, h: 200 }, SLIDE)).toEqual({
    x: 76,
    y: 76,
    w: 448,
    h: 248,
  });
  expect(padRegion({ x: 0, y: 1000, w: 1920, h: 80 }, SLIDE)).toEqual({
    x: 0,
    y: 976,
    w: 1920,
    h: 104,
  });
});

test("a point is drawn in the window around it, slid inside the drawing at an edge", () => {
  expect(pointRegion({ x: 0.5, y: 0.5 }, SLIDE)).toEqual({
    x: 960 - CROP_POINT_SIZE.w / 2,
    y: 540 - CROP_POINT_SIZE.h / 2,
    ...CROP_POINT_SIZE,
  });
  expect(pointRegion({ x: 0, y: 1 }, SLIDE)).toEqual({
    x: 0,
    y: 1080 - CROP_POINT_SIZE.h,
    ...CROP_POINT_SIZE,
  });
  expect(pointRegion({ x: 0.5, y: 0.5 }, { w: 200, h: 100 })).toEqual({
    x: 0,
    y: 0,
    w: 200,
    h: 100,
  });
});

test("a picture's long side is at most 800 pixels, and a small one is never grown", () => {
  expect(cropScale({ x: 0, y: 0, w: 1600, h: 400 })).toBe(0.5);
  expect(cropScale({ x: 0, y: 0, w: 300, h: 1000 })).toBe(0.8);
  expect(cropScale({ x: 0, y: 0, w: 200, h: 100 })).toBe(1);
});

// @vitest-environment node
/** A comment picture's region: an element with room around it or a point's window, in bounds. */

import { expect, test } from "vitest";
import {
  CROP_POINT_SIZE,
  cropScale,
  padRegion,
  pointRegion,
  svgDataUrl,
  windowAround,
} from "./crop.ts";
import { fixedAt, scrollerDisplays, stuckBy } from "./cropcopy.ts";

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

test("a picture is as sharp as the reader's screen, within the 800 pixel bound", () => {
  expect(cropScale({ x: 0, y: 0, w: 200, h: 100 }, 2)).toBe(2);
  expect(cropScale({ x: 0, y: 0, w: 480, h: 300 }, 2)).toBe(800 / 480);
  expect(cropScale({ x: 0, y: 0, w: 1600, h: 400 }, 3)).toBe(0.5);
  // A screen with fewer pixels than CSS pixels still gets one per CSS pixel.
  expect(cropScale({ x: 0, y: 0, w: 200, h: 100 }, 0.5)).toBe(1);
});

test("a point of the viewport is the window around it once the page's scroll is added", () => {
  const doc = { w: 1280, h: 6000 };
  // Shown at 300 down a viewport scrolled 2000: the document's 2300.
  expect(windowAround({ x: 640, y: 300 + 2000 }, doc)).toEqual({
    x: 640 - CROP_POINT_SIZE.w / 2,
    y: 2300 - CROP_POINT_SIZE.h / 2,
    ...CROP_POINT_SIZE,
  });
  expect(windowAround({ x: 10, y: 5990 }, doc)).toMatchObject({
    x: 0,
    y: 6000 - CROP_POINT_SIZE.h,
  });
});

test("a box fixed to the viewport is drawn where the reader sees it, at the document's scroll", () => {
  // A copy's corner is the document's, which sits at minus the scroll in the viewport.
  expect(fixedAt({ x: 0, y: 552 }, { x: 0, y: -1900 })).toEqual({ x: 0, y: 2452 });
  expect(fixedAt({ x: 20, y: 10 }, { x: 0, y: 0 })).toEqual({ x: 20, y: 10 });
  // A drawing shown at half its size: the viewport's pixels are halved ones of the drawing.
  expect(fixedAt({ x: 150, y: 120 }, { x: 100, y: 20 }, 0.5)).toEqual({ x: 100, y: 200 });
});

test("a sticky box is drawn as far from where it flows as it is stuck, less a stuck box around it", () => {
  expect(stuckBy({ x: 0, y: 60 }, { x: 0, y: -1040 })).toEqual({ x: 0, y: 1100 });
  expect(stuckBy({ x: 0, y: 60 }, { x: 0, y: 60 })).toEqual({ x: 0, y: 0 });
  expect(stuckBy({ x: 0, y: 1100 }, { x: 0, y: 0 }, { x: 0, y: 900 })).toEqual({ x: 0, y: 200 });
});

test("a scrolled element's copy clips as a block around a wrapper laying out its children as it did", () => {
  expect(scrollerDisplays("block")).toEqual({ outer: "block", inner: "flow-root" });
  expect(scrollerDisplays("flex")).toEqual({ outer: "block", inner: "flex" });
  expect(scrollerDisplays("inline-flex")).toEqual({ outer: "inline-block", inner: "flex" });
  expect(scrollerDisplays("grid")).toEqual({ outer: "block", inner: "grid" });
  expect(scrollerDisplays("inline-grid")).toEqual({ outer: "inline-block", inner: "grid" });
  expect(scrollerDisplays("inline-block")).toEqual({ outer: "inline-block", inner: "flow-root" });
  expect(scrollerDisplays("list-item")).toEqual({ outer: "list-item", inner: "flow-root" });
});

test("a picture's SVG reaches the image whole: no fragment cut at a #, no escape read at a %, no line break dropped", () => {
  const svg = '<svg><text xml:space="preserve">#1 of 100%\n\tnext\r</text></svg>';
  const url = svgDataUrl(svg);
  const body = url.slice("data:image/svg+xml;charset=utf-8,".length);
  expect(body).not.toMatch(/[#\t\n\r]|%(?![0-9a-f]{2})/);
  expect(new URL(url).hash).toBe("");
  expect(decodeURIComponent(body)).toBe(svg);
});

// @vitest-environment node
/** Where an image's pixels land in its box, and which part of it a picture needs. */

import { expect, test } from "vitest";
import { familyKey, offsetOf, overlap, paintedRect } from "./cropmedia.ts";

const BOX = { w: 400, h: 200 };

test("an image fills its box by default, and keeps its shape when it is contained or covers", () => {
  expect(paintedRect("fill", "50% 50%", BOX, { w: 100, h: 100 })).toEqual({
    x: 0,
    y: 0,
    w: 400,
    h: 200,
  });
  expect(paintedRect("contain", "50% 50%", BOX, { w: 100, h: 100 })).toEqual({
    x: 100,
    y: 0,
    w: 200,
    h: 200,
  });
  expect(paintedRect("cover", "50% 50%", BOX, { w: 100, h: 100 })).toEqual({
    x: 0,
    y: -100,
    w: 400,
    h: 400,
  });
});

test("an image shown at its own size is placed by object-position, and scale-down shrinks only a large one", () => {
  expect(paintedRect("none", "0% 0%", BOX, { w: 50, h: 40 })).toEqual({ x: 0, y: 0, w: 50, h: 40 });
  expect(paintedRect("none", "100% 100%", BOX, { w: 50, h: 40 })).toEqual({
    x: 350,
    y: 160,
    w: 50,
    h: 40,
  });
  expect(paintedRect("scale-down", "50% 50%", BOX, { w: 50, h: 40 })).toEqual({
    x: 175,
    y: 80,
    w: 50,
    h: 40,
  });
  expect(paintedRect("scale-down", "50% 50%", BOX, { w: 800, h: 800 })).toEqual({
    x: 100,
    y: 0,
    w: 200,
    h: 200,
  });
});

test("object-position takes lengths, percentages and their sums as computed styles write them", () => {
  expect(offsetOf("12px", 300)).toBe(12);
  expect(offsetOf("25%", 300)).toBe(75);
  expect(offsetOf("calc(100% - 8px)", 300)).toBe(292);
  expect(offsetOf("calc(50% + -4px)", 100)).toBe(46);
  expect(paintedRect("contain", "calc(100% - 10px) 0px", BOX, { w: 100, h: 100 })).toMatchObject({
    x: 190,
    y: 0,
  });
});

test("an image without a size of its own is drawn to its box", () => {
  expect(paintedRect("contain", "50% 50%", BOX, { w: 0, h: 0 })).toEqual({
    x: 0,
    y: 0,
    w: 400,
    h: 200,
  });
});

test("only the part of an image inside the picture is drawn, and an image outside it not at all", () => {
  const picture = { x: 100, y: 1000, w: 480, h: 300 };
  expect(overlap({ x: 0, y: 900, w: 1200, h: 750 }, picture)).toEqual(picture);
  expect(overlap({ x: 500, y: 1200, w: 300, h: 300 }, picture)).toEqual({
    x: 500,
    y: 1200,
    w: 80,
    h: 100,
  });
  expect(overlap({ x: 0, y: 0, w: 1200, h: 750 }, picture)).toBeNull();
  expect(overlap({ x: 580, y: 1000, w: 10, h: 10 }, picture)).toBeNull();
});

test("a family is matched however its name is quoted or cased", () => {
  expect(familyKey(' "Geist" ')).toBe("geist");
  expect(familyKey("'Inter Var'")).toBe("inter var");
  expect(familyKey("system-ui")).toBe("system-ui");
});

// @vitest-environment node
/** What a picture's copy keeps of each element: the rows it reads in full, the styles it states, the margins it carries. */

import { expect, test } from "vitest";
import { BAND_SLACK, collapsed, inBand, stated } from "./cropcopy.ts";

test("adjoining margins collapse to the largest, less the most negative", () => {
  expect(collapsed(16, 0)).toBe(16);
  expect(collapsed(28, 16, 8)).toBe(28);
  expect(collapsed(16, -4)).toBe(12);
  expect(collapsed(-10, -4)).toBe(-10);
  expect(collapsed()).toBe(0);
});

test("a box is read in full when it reaches the picture's rows, whatever its column", () => {
  const band = { top: 1000 - BAND_SLACK, bottom: 1300 + BAND_SLACK };
  expect(inBand(900, 1010, band)).toBe(true);
  expect(inBand(1290, 2000, band)).toBe(true);
  expect(inBand(0, 50_000, band)).toBe(true);
  expect(inBand(1300 + BAND_SLACK + 1, 1500, band)).toBe(false);
  expect(inBand(0, 1000 - BAND_SLACK - 1, band)).toBe(false);
});

const NAMES = [
  "color",
  "display",
  "margin-top",
  "background-color",
  "font-size",
  "cursor",
  "--brand",
  "transition-duration",
];

test("a style states what differs from its parent or its default, and always its box", () => {
  const parent = ["rgb(0, 0, 0)", "block", "0px", "rgba(0, 0, 0, 0)", "16px", "auto", "red", "0s"];
  const fallback = ["rgb(0, 0, 0)", "block", "0px", "rgba(0, 0, 0, 0)", "16px", "auto", "", "0s"];
  const own = ["rgb(0, 0, 0)", "block", "0px", "rgb(1, 2, 3)", "16px", "pointer", "red", "2s"];
  expect(stated(NAMES, own, parent, fallback)).toBe(
    "display:block;margin-top:0px;background-color:rgb(1, 2, 3);font-size:16px;",
  );
});

test("a value a parent passes down is stated where the tag's default differs, so a moved element keeps it", () => {
  const fallback = ["rgb(0, 0, 0)", "inline", "0px", "rgba(0, 0, 0, 0)", "16px", "auto", "", "0s"];
  const own = ["rgb(9, 9, 9)", "inline", "0px", "rgba(0, 0, 0, 0)", "16px", "auto", "", "0s"];
  expect(stated(NAMES, own, own, fallback)).toContain("color:rgb(9, 9, 9);");
  expect(stated(NAMES, own, null, fallback)).toContain("color:rgb(9, 9, 9);");
});

test("a pseudo-element's rule wins over anything the copy carries", () => {
  const values = ["rgb(0, 0, 0)", "block", "0px", "rgba(0, 0, 0, 0)", "16px", "auto", "", "0s"];
  expect(stated(NAMES, values, values, values, true)).toBe(
    "display:block !important;margin-top:0px !important;font-size:16px !important;",
  );
});

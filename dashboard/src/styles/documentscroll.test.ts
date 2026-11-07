// @vitest-environment node
import { readFileSync } from "node:fs";
import { fileURLToPath } from "node:url";
import { expect, test } from "vitest";

/**
 * THE DOCUMENT CANNOT SCROLL, EVEN FROM A SCRIPT.
 *
 * `body { overflow: hidden }` reaches the viewport, which stays a scroll
 * container, so a box positioned against the initial containing block stretched
 * it and a `scrollIntoView` moved the whole frame up for good. The rule lives
 * in CSS where no unit test draws it, so the declarations are held here; the
 * browser's own behaviour is what they are for.
 */
const css = readFileSync(fileURLToPath(new URL("./base.css", import.meta.url)), "utf8");
const screen = /@media screen \{([\s\S]*?)\n\}/.exec(css)?.[1] ?? "";

test("on screen the viewport keeps the hidden and body is a clipping containing block", () => {
  expect(screen).toMatch(/html \{\s*overflow: hidden;\s*\}/);
  expect(screen).toMatch(/body \{\s*position: relative;\s*overflow: clip;\s*\}/);
});

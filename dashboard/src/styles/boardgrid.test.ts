// @vitest-environment node
import { readFileSync } from "node:fs";
import { join } from "node:path";
import { fileURLToPath } from "node:url";
import { expect, test } from "vitest";

/**
 * THE BOARD'S PAGE GRID is twelve equal columns, and it is one rule for every
 * board: Home's blocks and a dashboard's widgets each span their own width of
 * it, so a widget laid out at four columns is four columns wherever it stands.
 * jsdom lays nothing out, so the rule is read rather than measured.
 */

const STYLES = fileURLToPath(new URL(".", import.meta.url));
const css = readFileSync(join(STYLES, "screens.css"), "utf8").replace(/\/\*[\s\S]*?\*\//g, "");

test("the board grid is twelve equal columns", () => {
  const body = /\n\.board-grid \{([^}]*)\}/.exec(css)?.[1] ?? "";
  expect(body).toMatch(/display:\s*grid;/);
  expect(body).toMatch(/grid-template-columns:\s*repeat\(12,\s*minmax\(0,\s*1fr\)\);/);
});

test("no board keeps a grid of its own", () => {
  expect(css).not.toMatch(/\.home-board-grid\b/);
  expect(css).not.toMatch(/\.dashboard-board\s*\{[^}]*display:\s*grid/);
});

// @vitest-environment node
import { readFileSync } from "node:fs";
import { join } from "node:path";
import { fileURLToPath } from "node:url";
import { expect, test } from "vitest";

/**
 * THE BOARD'S PAGE GRID is the design system's ArrangeGrid, one grid for every
 * board: Home's blocks and a dashboard's widgets each span their own width of
 * it, so a widget laid out at four columns is four columns wherever it stands.
 * No stylesheet here draws a board grid of its own. jsdom lays nothing out, so
 * the rules are read rather than measured.
 */

const STYLES = fileURLToPath(new URL(".", import.meta.url));
const css = readFileSync(join(STYLES, "screens.css"), "utf8").replace(/\/\*[\s\S]*?\*\//g, "");

test("no board keeps a grid of its own", () => {
  expect(css).not.toMatch(/\.(?:home-)?board-grid\b/);
  expect(css).not.toMatch(/\.home-group-body\b/);
  expect(css).not.toMatch(/\.dashboard-board\s*\{[^}]*display:\s*grid/);
});

// A ROW OF STATS AND METERS SHARES ONE BASELINE: each figure stands at the
// head of its body, under a title the frame holds at two lines in a row
// (DashboardWidget passes holdTitle), so a meter's maximum and bar hang
// under its number rather than lifting it a line above the stats beside it.
// Every figure fills its body, so an empty chart is centred in it.
test("a figure fills its body, and a stat or a meter stands at its head", () => {
  const figure = /\n\.dashboard-figure \{([^}]*)\}/.exec(css)?.[1] ?? "";
  expect(figure).toMatch(/height:\s*100%;/);
  expect(css).not.toMatch(/\.dashboard-figure:is\(\[data-figure="stat"\]/);
});

// THE VIEW'S SECTIONS ARE SET ON THE BOARD'S OWN GAP: the filters, the board
// and the Sources rail are one step apart, the step between two widgets.
test("a dashboard's sections are one board gap apart", () => {
  const view = /\n\.dashboard-view \{([^}]*)\}/.exec(css)?.[1] ?? "";
  const body = /\n\.dashboard-body \{([^}]*)\}/.exec(css)?.[1] ?? "";
  expect(view).toMatch(/gap:\s*var\(--spacing-4\);/);
  expect(body).toMatch(/gap:\s*var\(--spacing-4\);/);
});

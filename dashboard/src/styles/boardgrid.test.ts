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

// A ROW OF STATS SHARES ONE BASELINE: each stat stands on the foot of its
// frame, over the citation, whether its title took one line or two.
test("a stat or a meter stands on the foot of its frame", () => {
  const body =
    /\n\.dashboard-figure:is\(\[data-figure="stat"\], \[data-figure="meter"\]\) \{([^}]*)\}/.exec(
      css,
    )?.[1] ?? "";
  expect(body).toMatch(/justify-content:\s*flex-end;/);
  expect(body).toMatch(/height:\s*100%;/);
});

// THE VIEW'S SECTIONS ARE SET ON THE BOARD'S OWN GAP: the filters, the board
// and the Sources rail are one step apart, the step between two widgets.
test("a dashboard's sections are one board gap apart", () => {
  const view = /\n\.dashboard-view \{([^}]*)\}/.exec(css)?.[1] ?? "";
  const body = /\n\.dashboard-body \{([^}]*)\}/.exec(css)?.[1] ?? "";
  expect(view).toMatch(/gap:\s*var\(--spacing-4\);/);
  expect(body).toMatch(/gap:\s*var\(--spacing-4\);/);
});

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

// THE FIGURES FOLD INTO ROWS THAT FILL. Three a row left a third-width hole
// beside the last two of five, and a four-figure row one lone tile: four fold
// two by two, and five three over two on six tracks.
test("folded figures fill their rows, four two by two and five three over two", () => {
  const at = css.indexOf("@container page (width < 928px) {\n  .home-kpis");
  expect(at).toBeGreaterThan(-1);
  const narrow = css.slice(at, css.indexOf("@container page (width < 640px)", at));
  expect(narrow).toMatch(
    /\.home-kpis\[data-count="4"\] \{\s*grid-template-columns: repeat\(2, minmax\(0, 1fr\)\);/,
  );
  expect(narrow).toMatch(
    /\.home-kpis\[data-count="5"\] \{\s*grid-template-columns: repeat\(6, minmax\(0, 1fr\)\);/,
  );
  expect(narrow).toMatch(/\.home-kpis\[data-count="5"\] > \* \{\s*grid-column: span 2;/);
  expect(narrow).toMatch(
    /\.home-kpis\[data-count="5"\] > :nth-child\(n \+ 4\) \{\s*grid-column: span 3;/,
  );
  expect(narrow).not.toMatch(/repeat\(3, minmax\(0, 1fr\)\)/);
});

// @vitest-environment node
import { expect, test } from "vitest";
import { sheetRules } from "../test/sheets.ts";

/**
 * THE FOLDERS' TILES STAND IN THE CARDS' COLUMNS.
 *
 * The Folders tab and a folder's page draw a gallery of folder tiles over a
 * gallery of artifact cards. The two grids used to declare their own counts
 * and steps (four across with a 16px step over four across with a 20px one,
 * and four over five on a wide page), so each tile's edges stood a few pixels
 * off the card's under it. The count and the column step are declared once,
 * on both grids together in `screens.css` (the frame's sheet, which is always
 * there, where `folders.css` arrives later with the folder surfaces), and each
 * grid reads them.
 */

const rules = sheetRules();
const shared = ".artifacts-cards, .artifacts-folders";
const columns = "repeat(var(--artifacts-grid-columns), minmax(0, 1fr))";

test("the column count and step are declared once, on both grids together", () => {
  const declaring = rules.filter((rule) =>
    /--artifacts-grid-(columns|column-gap)\s*:/.test(rule.body),
  );
  // Every rule that sets the count for both grids names both; the cards' own
  // two and one across (where the tiles are rows) are the only exceptions.
  for (const rule of declaring) {
    expect(rule.file, rule.selector).toBe("screens.css");
    if (rule.selector !== shared) {
      expect(rule.selector).toBe(".artifacts-cards");
      expect(rule.body).not.toMatch(/--artifacts-grid-column-gap/);
    }
  }
  const both = declaring
    .filter((rule) => rule.selector === shared)
    .map((rule) => rule.body.replace(/\s+/g, " ").trim());
  expect(both).toEqual([
    "--artifacts-grid-columns: 4; --artifacts-grid-column-gap: var(--spacing-5);",
    "--artifacts-grid-columns: 5;",
    "--artifacts-grid-columns: 3; --artifacts-grid-column-gap: var(--spacing-4);",
  ]);
});

test("the cards and the folder tiles read the same columns and column step", () => {
  for (const [file, selector] of [
    ["screens.css", ".artifacts-cards"],
    ["folders.css", ".artifacts-folders"],
  ] as const) {
    const grid = rules.find(
      (rule) =>
        rule.file === file && rule.selector === selector && /display:\s*grid/.test(rule.body),
    );
    expect(grid, selector).toBeDefined();
    expect(grid!.body, selector).toContain(`grid-template-columns: ${columns};`);
    expect(grid!.body, selector).toMatch(
      /gap:\s*var\(--spacing-\d\) var\(--artifacts-grid-column-gap\);/,
    );
  }
});

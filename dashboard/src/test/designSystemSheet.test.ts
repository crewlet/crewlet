// @vitest-environment node

/**
 * The design-system sheet plugin (`vite.config.ts`) recognises the kit's
 * component stylesheets wherever `node_modules` really is.
 *
 * An isolated worktree has no install of its own: its `dashboard/node_modules`
 * is a symbolic link into the main checkout's, and the resolver names every
 * file there by the link's target. Judged by the spelled path, no sheet
 * matched in that layout, the kit's rules were split into lazy chunks, and
 * the bundle a worktree built was not the committed one.
 */

import { mkdirSync, mkdtempSync, rmSync, symlinkSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { afterAll, expect, test } from "vitest";
import { isComponentSheet } from "../../vite.config.ts";

const root = mkdtempSync(join(tmpdir(), "design-system-sheet-"));
afterAll(() => rmSync(root, { recursive: true, force: true }));

// main/node_modules is the install; worktree/node_modules links to it.
const install = join(root, "main", "node_modules");
for (const file of [
  "@crewlethq/ui/dist/button.css",
  "@crewlethq/ui/dist/styles.css",
  "@crewlethq/ui/dist/button.js",
  "@crewlethq/icons/dist/spinner.css",
]) {
  mkdirSync(join(install, file, ".."), { recursive: true });
  writeFileSync(join(install, file), "");
}
mkdirSync(join(root, "worktree", "src"), { recursive: true });
symlinkSync(install, join(root, "worktree", "node_modules"));
const linked = join(root, "worktree", "node_modules");
const ui = `${join(linked, "@crewlethq/ui/dist")}/`;
const icons = `${join(linked, "@crewlethq/icons/dist")}/`;

test("a component sheet resolved through a linked node_modules is the kit's", () => {
  // As the resolver names them: under the link's target.
  expect(isComponentSheet(join(install, "@crewlethq/ui/dist/button.css"), ui, icons)).toBe(true);
  expect(isComponentSheet(join(install, "@crewlethq/icons/dist/spinner.css"), ui, icons)).toBe(
    true,
  );
  // And as spelled through the link, which a plain install resolves to.
  expect(isComponentSheet(join(ui, "button.css"), ui, icons)).toBe(true);
});

test("the single-sheet build, a script and the application's own sheets are not", () => {
  expect(isComponentSheet(join(install, "@crewlethq/ui/dist/styles.css"), ui, icons)).toBe(false);
  expect(isComponentSheet(join(ui, "styles.css"), ui, icons)).toBe(false);
  expect(isComponentSheet(join(install, "@crewlethq/ui/dist/button.js"), ui, icons)).toBe(false);
  expect(isComponentSheet(join(root, "worktree", "src", "app.css"), ui, icons)).toBe(false);
});

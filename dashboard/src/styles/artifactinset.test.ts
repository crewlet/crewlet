// @vitest-environment node
import { readFileSync } from "node:fs";
import { fileURLToPath } from "node:url";
import { expect, test } from "vitest";

/**
 * AN ARTIFACT'S BODY STANDS 16PX IN FROM BOTH EDGES OF A PHONE.
 *
 * It folded to 12px under 640px while the shell around it kept 16px, and its
 * scrollbar gutter was reserved at the end edge only (`stable`), so where a
 * classic scrollbar is drawn the content stood 11px further from the right
 * than from the left. On a phone the gutter is reserved on both edges: an
 * overlay scrollbar reserves nothing, a classic one the same on each side.
 */
const css = readFileSync(fileURLToPath(new URL("./screens.css", import.meta.url)), "utf8");

test("on a phone an artifact's body is inset 16px and its gutter is the same on both sides", () => {
  const phone =
    /@container crewlet-artifact-workspace \(width < 640px\) \{\s*\.artifact-body \{([^}]*)\}/.exec(
      css,
    )?.[1] ?? "";
  expect(phone).toMatch(/--artifact-inset:\s*var\(--spacing-4\);/);
  expect(phone).toMatch(/scrollbar-gutter:\s*stable both-edges;/);
});

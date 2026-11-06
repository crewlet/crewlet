// @vitest-environment node
import { readFileSync } from "node:fs";
import { fileURLToPath } from "node:url";
import { expect, test } from "vitest";

/**
 * A BAR THAT LEADS WITH ITS FIND FIELD NEVER STACKS ONE CONTROL ON ANOTHER.
 *
 * On a phone the field and the actions shared one line and all shrank: a button
 * wider than its squeezed box lay under the next one and the field fell to a
 * magnifier. The field holds a floor; what will not fit takes the next line.
 */
const css = readFileSync(fileURLToPath(new URL("./frame.css", import.meta.url)), "utf8");
const rule = (selector: string) =>
  [...css.matchAll(new RegExp(`${selector.replace(/[.:()>]/g, "\\$&")} \\{([^}]*)\\}`, "g"))]
    .map((m) => m[1])
    .join("");

test("the find field keeps a floor on a phone", () => {
  expect(rule(".page-find")).toMatch(/min-width: min\(var\(--page-find-floor, 10rem\), 100%\);/);
});

test("the actions wrap and never shrink under the field", () => {
  expect(rule(".page-controls:has(.page-find) .page-actions")).toMatch(/flex-wrap: wrap;/);
  expect(rule(".page-controls:has(.page-find) .page-actions > :not(.page-find)")).toMatch(
    /flex: none;/,
  );
});

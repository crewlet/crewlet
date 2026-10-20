/**
 * A prototype page loads the stylesheets the shipped page loads, in the
 * shipped order.
 *
 * `harness/boot.tsx` restates `src/main.tsx`'s stylesheet imports because the
 * shipped boot cannot be imported without dialling an engine, and the order
 * of those imports IS the cascade (`src/main.tsx` says why). This holds the
 * two lists equal, so a sheet added to or moved in the shipped boot fails
 * here until the prototype's follows.
 */

import { readFileSync } from "node:fs";
import { join } from "node:path";
import { expect, test } from "vitest";

/** The stylesheet imports a module makes, in order, as written. */
function sheets(file: string): string[] {
  return [...readFileSync(file, "utf-8").matchAll(/^import "([^"]+)";$/gm)]
    .map((m) => m[1] ?? "")
    .filter(
      (spec) => spec.endsWith(".css") || spec.includes("/css") || spec.startsWith("virtual:"),
    );
}

test("boot.tsx loads main.tsx's stylesheets, in main.tsx's order", () => {
  const shipped = sheets(join(process.cwd(), "src", "main.tsx")).map((s) =>
    s.replace(/^\.\//, "~/"),
  );
  const prototype = sheets(join(process.cwd(), "prototype", "harness", "boot.tsx")).filter(
    (s) => s !== "./VariantBar.css",
  );
  expect(shipped.length).toBeGreaterThan(5);
  expect(prototype).toEqual(shipped);
});

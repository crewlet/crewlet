// @vitest-environment node
/**
 * The Spend workspace's sentences carry no em dash: every string, template
 * and run of JSX text its modules ship, read from the parsed source so that a
 * comment is free to use one. The project's copy uses commas, colons,
 * parentheses and full stops instead.
 */

import { expect, test } from "vitest";
import { modules, shippedText } from "~/test/source.ts";

/** Spelled by its code point, so this file holds no em dash of its own. */
const EM_DASH = String.fromCodePoint(0x2014);

/** The workspace's modules: its screens, and the ceiling editor Budgets draws. */
const SPEND = (path: string) =>
  path.startsWith("routes/spend/") || path === "components/budgetWrite.tsx";

test("Spend's shipped sentences carry no em dash", () => {
  const read = modules().filter((m) => SPEND(m.path));
  expect(read.length, "the gate read none of the workspace's modules").toBeGreaterThan(10);
  const found: string[] = [];
  for (const module of read) {
    for (const { text, line } of shippedText(module)) {
      if (text.includes(EM_DASH)) found.push(`${module.path}:${line} ${text.trim()}`);
    }
  }
  expect(found).toEqual([]);
});

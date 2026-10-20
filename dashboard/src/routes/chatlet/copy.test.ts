// @vitest-environment node
/**
 * The Chatlet workspace's sentences carry no em dash: every string, template and
 * run of JSX text its modules ship, read through the one walk every copy gate
 * shares (`shippedText`), so a comment is free to use one. The copy uses
 * commas, colons, parentheses and full stops instead, and says what is wrong
 * and what to do rather than how the engine got there.
 */

import { expect, test } from "vitest";
import { modules, shippedText } from "~/test/source.ts";

/** Spelled by its code point, so this file holds no em dash of its own. */
const EM_DASH = String.fromCodePoint(0x2014);

test("Chatlet's shipped sentences carry no em dash", () => {
  const read = modules().filter((m) => m.path.startsWith("routes/chatlet/"));
  expect(read.length, "the gate read none of the workspace's modules").toBeGreaterThan(10);
  const found: string[] = [];
  for (const module of read) {
    for (const { text, line } of shippedText(module)) {
      if (text.includes(EM_DASH)) found.push(`${module.path}:${line} ${text.trim()}`);
    }
  }
  expect(found).toEqual([]);
});

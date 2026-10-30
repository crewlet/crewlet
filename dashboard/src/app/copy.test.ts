// @vitest-environment node
/**
 * The dashboard's sentences carry no em dash: every string, template and run
 * of JSX text any of its modules ships, read through the one walk every copy
 * gate shares (`shippedText`), so a comment is free to use one. The copy uses
 * commas, colons, parentheses and full stops instead.
 *
 * ONE GATE FOR THE WHOLE DASHBOARD. It was two, one for the Chatlet workspace
 * and one for Spend, and an em dash outside both shipped in a navigation hint,
 * the direct-message refusal, the org builder's contact help and a trace
 * tooltip with nothing to stop it.
 *
 * A TEXT THAT IS AN EM DASH AND NOTHING ELSE is a mark rather than a
 * sentence (a group's React key, a cell's empty value) and is not judged.
 */

import { expect, test } from "vitest";
import { modules, shippedText } from "~/test/source.ts";

/** Spelled by its code point, so this file holds no em dash of its own. */
const EM_DASH = String.fromCodePoint(0x2014);

test("the dashboard's shipped sentences carry no em dash", () => {
  const read = modules().filter((m) => !m.path.startsWith("test/"));
  expect(read.length, "the gate read too few of the dashboard's modules").toBeGreaterThan(300);
  const found: string[] = [];
  for (const module of read) {
    for (const { text, line } of shippedText(module)) {
      if (text.includes(EM_DASH) && text.trim() !== EM_DASH) {
        found.push(`${module.path}:${line} ${text.trim()}`);
      }
    }
  }
  expect(found).toEqual([]);
});

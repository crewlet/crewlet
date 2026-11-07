// @vitest-environment node
/**
 * The kit's layout primitives are used on the axis they draw.
 *
 * A STACK IS A COLUMN, so `wrap` on one means a row was meant: a column of
 * buttons cannot wrap, and `align="center"` on it centres them across the
 * column. That is how the Inbox's Mark as read and Snooze came to sit one
 * under the other in the middle of a left-aligned pane. A row of controls
 * that may wrap is an `Inline`.
 */

import { expect, test } from "vitest";
import { lineOf, modules, parse, walk, type Node } from "~/test/source.ts";

/** Every `<Stack wrap>` in `text`, as `line` numbers. */
function wrappedStacks(text: string): number[] {
  const at = lineOf(text);
  const out: number[] = [];
  walk(parse(text, "tsx"), (node) => {
    if (node.type !== "JSXOpeningElement") return;
    if ((node.name as Node & { name?: string }).name !== "Stack") return;
    const wraps = (node.attributes as Node[]).some(
      (a) => (a.name as (Node & { name?: string }) | undefined)?.name === "wrap",
    );
    if (wraps) out.push(at(node.start));
  });
  return out;
}

test("no Stack is asked to wrap: a row that wraps is an Inline", () => {
  const read = modules().filter((m) => m.lang === "tsx");
  expect(read.length, "the gate read too few of the dashboard's modules").toBeGreaterThan(200);
  const found = read.flatMap((m) => wrappedStacks(m.text).map((line) => `${m.path}:${line}`));
  expect(found).toEqual([]);
});

test("the gate sees a wrapped Stack and lets an Inline wrap", () => {
  expect(wrappedStacks(`const a = <Stack gap={2} align="center" wrap><b /></Stack>;`)).toEqual([1]);
  expect(wrappedStacks(`const a = <Inline gap={2} wrap><b /></Inline>;`)).toEqual([]);
  expect(wrappedStacks(`const a = <Stack gap={2}><b /></Stack>;`)).toEqual([]);
});

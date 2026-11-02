/**
 * Where prose spells out an address or a path, and whether a token falls in
 * one: the rule both readers of keys in prose share (`lib/writtenOut.ts`).
 */

import { describe, expect, test } from "vitest";
import { LINEAR_BOUND, growth } from "../test/growth.ts";
import { overlapsAny, writtenOutSpans } from "./writtenOut.ts";

describe("overlapsAny", () => {
  const spans = writtenOutSpans("see a/b and /c/d, then x");

  test("a token inside a span, across its edge or between spans", () => {
    expect(spans).toEqual([
      [4, 7],
      [12, 17],
    ]);
    expect(overlapsAny(spans, 5, 6)).toBe(true);
    expect(overlapsAny(spans, 2, 5)).toBe(true);
    expect(overlapsAny(spans, 16, 20)).toBe(true);
    expect(overlapsAny(spans, 7, 12)).toBe(false);
    expect(overlapsAny(spans, 0, 4)).toBe(false);
    expect(overlapsAny(spans, 23, 24)).toBe(false);
    expect(overlapsAny([], 0, 1)).toBe(false);
  });

  // ONE SEARCH PER TOKEN, not one walk of every span: a run of many paths and
  // many keys or numbers asked each token of every path. Held by GROWTH,
  // timed with room to spare as the token cost cases are (`test/growth.ts`):
  // thirty-two times the tokens against thirty-two times the spans must take
  // under 181 times as long. A search takes about forty-seven (thirty-two,
  // times the log of the spans), a walk about a thousand.
  //
  // Mutation: answer with `spans.some(...)` and the growth is about 1,024.
  test("thirty-two times the tokens against thirty-two times the spans takes well under 1,024 times as long", () => {
    const ratio = growth((n) => {
      const spans = Array.from({ length: n }, (_, i) => [i * 4, i * 4 + 2] as const);
      return () => {
        for (let i = 0; i < n; i++)
          if (overlapsAny(spans, i * 4 + 2, i * 4 + 3)) throw new Error("overlap");
      };
    }, 1_000);
    expect(ratio, "the larger run took no longer, so this measured nothing").toBeGreaterThan(4);
    expect(ratio).toBeLessThan(LINEAR_BOUND);
  });
});

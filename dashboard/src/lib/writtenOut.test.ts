/**
 * Where prose spells out an address or a path, and whether a token falls in
 * one: the rule both readers of keys in prose share (`lib/writtenOut.ts`).
 */

import { describe, expect, test } from "vitest";
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
  // many keys or numbers asked each token of every path. At 20,000 of each a
  // walk is 400 million checks, seconds on every render; a binary search is
  // a few hundred thousand.
  //
  // Mutation: answer with `spans.some(...)` and this exceeds the bound.
  test("20,000 tokens against 20,000 spans read in well under a second", () => {
    const many = Array.from({ length: 20_000 }, (_, i) => [i * 4, i * 4 + 2] as const);
    const start = performance.now();
    let hits = 0;
    for (let i = 0; i < 20_000; i++) if (overlapsAny(many, i * 4 + 2, i * 4 + 3)) hits++;
    expect(performance.now() - start).toBeLessThan(250);
    expect(hits).toBe(0);
  });
});

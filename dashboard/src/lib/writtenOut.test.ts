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
  // many keys or numbers asked each token of every path. Held by GROWTH, as
  // the token cost cases are (`inlineTokens.test.ts`): four times the tokens
  // against four times the spans must take well under the sixteen times a
  // walk takes, here under eight, timed as the median of five samples after
  // a warm-up, each sample enough calls to take 10 ms.
  //
  // Mutation: answer with `spans.some(...)` and the ratio is about sixteen.
  test("four times the tokens against four times the spans takes well under sixteen times as long", () => {
    const run = (n: number) => {
      const spans = Array.from({ length: n }, (_, i) => [i * 4, i * 4 + 2] as const);
      return () => {
        let hits = 0;
        for (let i = 0; i < n; i++) if (overlapsAny(spans, i * 4 + 2, i * 4 + 3)) hits++;
        return hits;
      };
    };
    const time = (f: () => number, calls: number) => {
      const start = performance.now();
      for (let i = 0; i < calls; i++) expect(f()).toBe(0);
      return performance.now() - start;
    };
    const median = (xs: number[]) => xs.sort((x, y) => x - y)[Math.floor(xs.length / 2)]!;
    const small = run(5_000);
    const large = run(20_000);
    time(small, 1);
    time(large, 1);
    // EACH SAMPLE ENOUGH CALLS TO TAKE 10 ms, so a timer's grain decides nothing.
    let calls = 1;
    while (calls < 256 && time(small, calls) < 10) calls *= 2;
    const ratio =
      median(Array.from({ length: 5 }, () => time(large, calls))) /
      median(Array.from({ length: 5 }, () => time(small, calls)));
    expect(ratio).toBeLessThan(8);
  });
});

// @vitest-environment node
/** Gitlet › Changes' query: its readiness words. */

import { expect, test } from "vitest";
import { READINESS_BUCKETS } from "~/contract/code.ts";
import { READINESS_WORDS, isReadinessBucket } from "./changeQuery.ts";

// Mutation: drop a bucket's words, or let a value the engine does not take pass as a bucket.
// Mutation: drop a bucket's words.
test("every readiness bucket the engine takes has its words, and nothing else is a bucket", () => {
  for (const bucket of READINESS_BUCKETS) {
    expect(READINESS_WORDS[bucket], bucket).toMatch(/^[A-Z]/);
    expect(isReadinessBucket(bucket)).toBe(true);
  }
  expect(Object.keys(READINESS_WORDS)).toEqual([...READINESS_BUCKETS]);
  expect(isReadinessBucket("person")).toBe(false);
});

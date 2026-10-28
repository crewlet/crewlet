// @vitest-environment node

/**
 * The code host's values as the Code workspace says them: what a push read in
 * brief left out.
 */

import { describe, expect, test } from "vitest";
import { omittedOf } from "./code.ts";

describe("what a push read in brief left out", () => {
  // THE REFUSALS FIRST, then the engine's order: what did not happen is what a
  // reader acts on. Mutation: drop the reordering, and refused is said last.
  test("is said refusals first, then in the engine's order, and summed", () => {
    expect(omittedOf({ deleted: 5, created: 990, refused: 2, moved: 1 })).toEqual({
      total: 998,
      outcomes: [
        { outcome: "refused", count: 2 },
        { outcome: "created", count: 990 },
        { outcome: "moved", count: 1 },
        { outcome: "deleted", count: 5 },
      ],
    });
  });

  // AN OUTCOME A NEWER ENGINE COUNTS BY is said by its key and never dropped
  // from the sum: "and 5 more" over four counted would be a count the row
  // contradicts.
  test("says an outcome this build has no word for by its key, and counts it", () => {
    expect(omittedOf({ created: 1, renamed: 4 })).toEqual({
      total: 5,
      outcomes: [
        { outcome: "created", count: 1 },
        { outcome: "renamed", count: 4 },
      ],
    });
  });

  test("is nothing where the push left nothing out", () => {
    expect(omittedOf(undefined)).toEqual({ total: 0, outcomes: [] });
    expect(omittedOf({})).toEqual({ total: 0, outcomes: [] });
  });
});

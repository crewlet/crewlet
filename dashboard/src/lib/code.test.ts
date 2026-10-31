// @vitest-environment node

/**
 * The code host's values as the Code workspace says them: what a push read in
 * brief left out.
 */

import { describe, expect, test } from "vitest";
import {
  changeRef,
  omittedOf,
  parseChangeRef,
  preconditionWord,
  standingBlockers,
} from "./code.ts";

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

describe("a change's reference", () => {
  // THE ENGINE'S GRAMMAR, both halves: a repository's name and a number from
  // 1. Mutation: accept a number 0, or a name no repository can have, and a
  // reference nothing can answer is offered as one.
  test("is a repository's name and a number from 1, and nothing else", () => {
    expect(parseChangeRef("web!42")).toEqual({ repo: "web", number: 42 });
    expect(parseChangeRef("dev.scratch!1")).toEqual({ repo: "dev.scratch", number: 1 });
    expect(changeRef("web", 42)).toBe("web!42");
    for (const text of [
      "web!0",
      "web!042",
      "web!",
      "!42",
      "Web!42",
      "web!4x",
      "web!!4",
      "web 42",
    ]) {
      expect(parseChangeRef(text), text).toBeNull();
    }
  });
});

describe("a change's readiness, as a row says it", () => {
  // THE STANDING BLOCKERS ARE THE READINESS'S: those it names and those it
  // counts beyond them, and none where the precondition is met.
  test("counts the blockers it names and those beyond, and none once met", () => {
    const at = (state: string) => ({
      met: 0,
      total: 1,
      first: "",
      preconditions: [
        {
          kind: "blocker",
          state,
          detail: "",
          blockers: [
            {
              finding_id: "f",
              state: "open",
              cleared_by: { raiser: { handle: "a", kind: "agent" } },
            },
          ],
          blockers_omitted: 4,
        },
      ],
    });
    expect(standingBlockers(at("unmet"))).toBe(5);
    expect(standingBlockers(at("met"))).toBe(0);
    expect(standingBlockers(undefined)).toBe(0);
  });

  // A KIND A NEWER ENGINE JUDGES is said by its own word rather than dropped.
  test("names a precondition this build knows, and one it does not by its kind", () => {
    expect(preconditionWord("approval")).toBe("Approved");
    expect(preconditionWord("rebase_needed")).toBe("rebase needed");
  });
});

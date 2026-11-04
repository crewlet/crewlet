import { describe, expect, test } from "vitest";
import { droppedWords } from "./dropped.ts";

// WHAT A RUN'S RECORD DROPPED IS SAID BY TOOL, one wording wherever a reader
// meets it: each tool with how often it was called and how many of those
// failed, the tools in name order, and the calls whose tool was not kept
// either said as such — nothing at all where the record kept no tool of any.
//
// Mutation: drop the failures, or the calls whose tool was not kept, and the
// sentence says less than the record does.
describe("droppedWords", () => {
  test("says the dropped calls by tool", () => {
    expect(
      droppedWords([
        { name: "read_file", calls: 100 },
        { name: "read_file", calls: 20, failed: 3 },
        { name: "post_message", calls: 1 },
        { calls: 5 },
      ]),
    ).toBe(
      " By tool: post_message once, read_file 120 times (3 failed) and 5 calls to tools the record didn’t keep.",
    );
  });

  test("says nothing of a record that kept no tool", () => {
    expect(droppedWords([])).toBe("");
  });
});

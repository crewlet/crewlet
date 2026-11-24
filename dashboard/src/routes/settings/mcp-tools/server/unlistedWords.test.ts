import { describe, expect, it } from "vitest";
import { unlistedWords } from "./ToolsTab";

// A COUNT PAST THE LIST IS A BOUND, NOT A FACT: a node's status row cut its
// keys, so the list may miss some, and the sentence says "up to" and "may".
//
// Mutation: say "a node's status holds none by name" for an empty cut list,
// or drop the singular, and the cases below go red.
describe("unlistedWords", () => {
  it("names a whole list by its length", () => {
    expect(unlistedWords(1, 0)).toBe(
      "One tool_annotations key matches no tool any instance lists: ",
    );
    expect(unlistedWords(3, 0)).toBe("3 tool_annotations keys match no tool any instance lists: ");
  });
  it("names a cut list as the first of at most its bound", () => {
    expect(unlistedWords(2, 5)).toBe(
      "The first 2 of up to 7 tool_annotations keys that match no tool any instance lists " +
        "(a node's status could not name them all): ",
    );
  });
  it("says an empty cut list names no key every node names, never that a node named none", () => {
    expect(unlistedWords(0, 1)).toBe(
      "Up to one tool_annotations key may match no tool any instance lists; no key is named by every node.",
    );
    expect(unlistedWords(0, 4)).toBe(
      "Up to 4 tool_annotations keys may match no tool any instance lists; no key is named by every node.",
    );
  });
});

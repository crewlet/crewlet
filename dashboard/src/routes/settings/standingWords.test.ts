/**
 * A node's standing in the Nodes screen's words: what it runs, and never "0
 * epochs behind" for a node the epoch alone cannot place.
 */

import { describe, expect, test } from "vitest";
import { standingCaption, standingWords } from "./standingWords.ts";

const PREVIOUS = "01JPREVIOUS000000000000000P";

describe("a standing in words", () => {
  test("a refusal says what the node runs instead", () => {
    expect(standingWords({ kind: "refused", serving: PREVIOUS })).toBe(
      `refused the active revision; runs ${PREVIOUS.slice(0, 10)}`,
    );
    expect(standingWords({ kind: "refused" })).toBe("refused the active revision; runs none");
    expect(standingCaption({ kind: "refused", serving: PREVIOUS })).toBe("refused");
  });

  test("another revision is named, and the active one from a replaced activation said so", () => {
    expect(
      standingWords({
        kind: "elsewhere",
        attempted: PREVIOUS,
        serving: PREVIOUS,
        servesActive: false,
      }),
    ).toBe(`on another revision, ${PREVIOUS.slice(0, 10)}`);
    expect(standingWords({ kind: "elsewhere", servesActive: true })).toBe(
      "on the active revision, from a replaced activation",
    );
    expect(standingCaption({ kind: "elsewhere", servesActive: false })).toBe("elsewhere");
  });

  test("an older epoch is behind by the difference, and a silent node not reported", () => {
    expect(standingWords({ kind: "behind", epochs: 3 })).toBe("3 epochs behind");
    expect(standingWords({ kind: "unreported" })).toBe("no apply reported");
    expect(standingCaption({ kind: "unreported" })).toBe("not reported");
  });

  // A CROWDED PLACE SAYS NOTHING of a node that is fine, or of an answer with
  // no target to be behind.
  test("current and no target have no caption", () => {
    expect(standingWords({ kind: "current" })).toBe("running the active revision");
    expect(standingCaption({ kind: "current" })).toBe("");
    expect(standingCaption({ kind: "no-target" })).toBe("");
    expect(standingWords({ kind: "degraded" })).toBe("applied the active revision only in part");
    expect(standingCaption({ kind: "degraded" })).toBe("partial");
  });
});

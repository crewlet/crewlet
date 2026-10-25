// @vitest-environment node

/**
 * The assistant writes only into a part of the principles the person has
 * open, and only a value of that part's own shape.
 */

import { describe, expect, test } from "vitest";
import { parseCharterDraft } from "./assistantAction.ts";
import { INITIAL, reducer } from "./state.ts";

const open = reducer(INITIAL, {
  type: "open",
  part: "vision",
  base: "Old vision",
  origin: "edit",
  focus: false,
});

describe("principles.draft.set", () => {
  test("takes a new draft of an open part", () => {
    expect(parseCharterDraft({ part: "vision", value: "A new vision." }, open)).toEqual({
      ok: true,
      value: { part: "vision", value: "A new vision." },
    });
  });

  test("refuses a part the person has not opened, rather than opening it", () => {
    expect(parseCharterDraft({ part: "mission", value: "x" }, open)).toEqual({
      ok: false,
      reason: "Open the mission to write it first.",
    });
  });

  test("refuses a value of the wrong shape, an unknown part and extra keys", () => {
    expect(parseCharterDraft({ part: "vision", value: ["a"] }, open).ok).toBe(false);
    expect(parseCharterDraft({ part: "values", value: "x" }, open).ok).toBe(false);
    expect(parseCharterDraft({ part: "vision", value: "x", save: true }, open).ok).toBe(false);
    expect(parseCharterDraft("A new vision.", open).ok).toBe(false);
  });
});

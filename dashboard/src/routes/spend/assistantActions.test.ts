// @vitest-environment node

/**
 * The Spend screen's page action takes only a window its own offer has and a
 * split its chart draws, worded as the window control and the split menu word
 * them.
 */

import { describe, expect, test } from "vitest";
import { parseUrlPatch } from "~/app/assistant/urlAction.ts";
import { spendGroupKeys, spendRangeKeys } from "./assistantActions.ts";
import { spendOffer } from "./model.ts";

const NOW = Date.parse("2026-10-24T09:41:00Z");
const keys = { ...spendRangeKeys(spendOffer("UTC"), () => NOW), ...spendGroupKeys() };

describe("the spend window and split", () => {
  test("take a window the screen offers, and refuse one it would replace with its fallback", () => {
    expect(parseUrlPatch({ window: "7d" }, keys)).toEqual({ ok: true, value: { window: "7d" } });
    expect(parseUrlPatch({ window: "1h" }, keys)).toEqual({
      ok: false,
      reason: "This screen does not offer that window.",
    });
    expect(parseUrlPatch({ window: "nonsense" }, keys).ok).toBe(false);
  });

  test("take a window the reader could name with the custom control", () => {
    const custom = "2026-10-01T00:00:00.000Z/2026-10-08T00:00:00.000Z";
    expect(parseUrlPatch({ window: custom }, keys).ok).toBe(true);
  });

  test("write the screen's own fallback as a clear, as its controls do", () => {
    expect(parseUrlPatch({ window: "30d", group: "phase" }, keys)).toEqual({
      ok: true,
      value: { window: null, group: null },
    });
  });

  test("take only a split the chart draws, worded as the menu words it", () => {
    expect(parseUrlPatch({ group: "model" }, keys).ok).toBe(true);
    expect(parseUrlPatch({ group: "turn" }, keys).ok).toBe(false);
    expect(keys.group!.words!("seat")).toBe("Seat");
    expect(keys.window!.words!("7d")).toBe("7 days");
  });

  test("are both sections, so applying one is a place the reader can go back from", () => {
    expect(keys.window!.history).toBe("section");
    expect(keys.group!.history).toBe("section");
  });
});

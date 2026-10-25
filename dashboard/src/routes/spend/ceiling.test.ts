// @vitest-environment node

/** A prepared ceiling is a scope the screen draws, one of its windows, and a value the field takes. */

import { describe, expect, test } from "vitest";
import { parseCeilingPrefill } from "./assistantActions.ts";

const seats = new Set(["pm"]);

describe("budgets.ceiling.prefill", () => {
  test("takes the company's or a drawn seat's window, in the field's own spelling", () => {
    expect(parseCeilingPrefill({ period: "month", ceiling: "80M" }, seats)).toEqual({
      ok: true,
      value: { scope: "company", period: "month", text: "80M", value: 80_000_000 },
    });
    expect(parseCeilingPrefill({ seat: "pm", period: "day", ceiling: 5000 }, seats)).toMatchObject({
      ok: true,
      value: { scope: "pm", value: 5000 },
    });
    expect(parseCeilingPrefill({ period: "week", ceiling: null }, seats)).toMatchObject({
      ok: true,
      value: { text: "", value: null },
    });
  });

  test("refuses a seat the screen does not draw, a window it has not, and a value the field refuses", () => {
    expect(parseCeilingPrefill({ seat: "ceo", period: "day", ceiling: "1M" }, seats).ok).toBe(
      false,
    );
    expect(parseCeilingPrefill({ period: "year", ceiling: "1M" }, seats).ok).toBe(false);
    expect(parseCeilingPrefill({ period: "day", ceiling: "lots" }, seats).ok).toBe(false);
    expect(parseCeilingPrefill({ period: "day", ceiling: "1M", save: true }, seats).ok).toBe(false);
  });
});

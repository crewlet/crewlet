// @vitest-environment node

import { describe, expect, test } from "vitest";
import { classifyConfigRefusal } from "~/protocol/configAnswer.ts";
import { readCeiling } from "./budget.ts";
import { CONFIG_WRITE_REASONS } from "./useWriteAccess.ts";
import {
  ceilingSummary,
  ceilingText,
  changesFrom,
  documentPatch,
  documentCeilings,
  refusalWords,
  seatWithCeilings,
} from "./ceilings.ts";

describe("reading a typed ceiling", () => {
  // THE SPELLING THE SCREEN DRAWS. A field that took only digits asked the
  // reader to count zeros under the `40M` they had just read.
  test("takes digits, grouped digits and the screen's own suffixes", () => {
    expect(readCeiling("day", "40000000")).toEqual({ ok: true, value: 40_000_000 });
    expect(readCeiling("day", "40,000,000")).toEqual({ ok: true, value: 40_000_000 });
    expect(readCeiling("day", "40_000_000")).toEqual({ ok: true, value: 40_000_000 });
    expect(readCeiling("day", "40M")).toEqual({ ok: true, value: 40_000_000 });
    expect(readCeiling("day", "2.3m")).toEqual({ ok: true, value: 2_300_000 });
    expect(readCeiling("day", "750k")).toEqual({ ok: true, value: 750_000 });
    expect(readCeiling("day", "1.5B")).toEqual({ ok: true, value: 1_500_000_000 });
  });

  test("empty is no ceiling, and 0 or a fraction of a token is refused", () => {
    expect(readCeiling("week", "  ")).toEqual({ ok: true, value: null });
    expect(readCeiling("day", "0")).toEqual({
      ok: false,
      error: "A ceiling of 0 is refused: leave it empty for no daily ceiling.",
    });
    expect(readCeiling("day", "0k").ok).toBe(false);
    expect(readCeiling("day", "1.2345k").ok).toBe(false);
    expect(readCeiling("day", "12.5").ok).toBe(false);
    expect(readCeiling("day", "lots").ok).toBe(false);
    expect(readCeiling("day", "-5").ok).toBe(false);
  });
});

describe("a ceiling as a field shows it", () => {
  // EXACT, OR THE DIGITS: a prefilled field that rounded saves a different
  // ceiling the moment somebody presses Save without touching it.
  test("the shortest spelling that reads back as exactly this number", () => {
    expect(ceilingText(40_000_000)).toBe("40M");
    expect(ceilingText(2_500_000)).toBe("2.5M");
    expect(ceilingText(750_000)).toBe("750k");
    expect(ceilingText(45_678_901)).toBe("45678901");
    expect(ceilingText(999)).toBe("999");
    expect(ceilingText(undefined)).toBe("");
    for (const n of [1, 1_000, 1_234_000, 45_678_901, 3_000_000_000, 12_345]) {
      const read = readCeiling("month", ceilingText(n));
      expect(read, String(n)).toEqual({ ok: true, value: n });
    }
  });
});

describe("what a change writes", () => {
  test("the company's is a merge patch of only the windows changed, null removing one", () => {
    expect(documentPatch({ kind: "company" }, { day: 50_000_000 })).toEqual({
      token_budget: { day: 50_000_000 },
    });
    expect(documentPatch({ kind: "company" }, { week: null })).toEqual({
      token_budget: { week: null },
    });
  });

  // A NESTED KEY OF THE SAME DOCUMENT: a merge patch merges `assistant` key by
  // key, so its model chain and its step cap survive a ceiling raised here.
  test("each person's assistant ceiling is a merge patch one level down", () => {
    expect(documentPatch({ kind: "assistant" }, { day: 200_000, week: null })).toEqual({
      assistant: { token_budget: { day: 200_000, week: null } },
    });
    expect(
      documentCeilings(
        { kind: "assistant" },
        { token_budget: { day: 1 }, assistant: { llm: ["p"], token_budget: { day: 9 } } },
      ),
    ).toEqual({ day: 9 });
    expect(documentCeilings({ kind: "assistant" }, { token_budget: { day: 1 } })).toBeUndefined();
    expect(documentCeilings({ kind: "company" }, { token_budget: { day: 1 } })).toEqual({ day: 1 });
  });

  test("the history names each person's assistant as whose ceiling changed", () => {
    expect(ceilingSummary({ kind: "assistant" }, { day: 200_000 }, { day: 100_000 })).toBe(
      "Raise Ask Crewlet's per-person daily token ceiling from 100k to 200k",
    );
  });

  // THE SEAT AS READ, masks included — the engine restores a mask against the
  // revision it served — and no empty `token_budget` once nothing is capped.
  test("a seat is sent back whole, with its ceilings changed", () => {
    const seat = {
      name: "PM",
      handle: "pm",
      api_key: "__redacted__",
      token_budget: { day: 5, week: 9 },
    };
    expect(seatWithCeilings(seat, { day: 7 })).toEqual({
      ...seat,
      token_budget: { day: 7, week: 9 },
    });
    const bare = seatWithCeilings(seat, { day: null, week: null });
    expect(bare).toEqual({ name: "PM", handle: "pm", api_key: "__redacted__" });
    expect("token_budget" in bare).toBe(false);
    expect(seatWithCeilings({ name: "X" }, { month: 3 })).toEqual({
      name: "X",
      token_budget: { month: 3 },
    });
  });

  test("only the windows whose value differs from what is held are changes", () => {
    expect(changesFrom({ day: "40M", week: "" }, { day: 40_000_000 })).toEqual({});
    expect(changesFrom({ day: "50M", week: "" }, { day: 40_000_000, week: 1 })).toEqual({
      day: 50_000_000,
      week: null,
    });
    expect(changesFrom({ day: "0" }, {})).toBeUndefined();
  });

  // THE HISTORY'S LINE: whose, which window, from what and to what.
  test("the summary says whose, which window, and from what to what", () => {
    const pm = { kind: "seat", handle: "pm", name: "Agent PM" } as const;
    expect(ceilingSummary(pm, { day: 5_000_000 }, { day: 2_000_000 })).toBe(
      "Raise Agent PM's daily token ceiling from 2M to 5M",
    );
    expect(ceilingSummary({ kind: "company" }, { month: 1_000 }, { month: 2_000 })).toBe(
      "Lower the company's monthly token ceiling from 2,000 to 1,000",
    );
    expect(ceilingSummary(pm, { week: 9, day: null }, { day: 3 })).toBe(
      "Remove Agent PM's daily token ceiling (was 3); set Agent PM's weekly token ceiling to 9",
    );
  });
});

describe("what the engine's answer means", () => {
  test("a refusal whose remedy is a fresh read says so", () => {
    const scope = { kind: "seat", handle: "pm", name: "PM" } as const;
    expect(
      refusalWords(
        { kind: "conflict", reason: "revision_advanced", currentRevisionId: null },
        scope,
      ).reload,
    ).toBe(true);
    expect(
      refusalWords({ kind: "unsettled", answered: false, detail: "", status: 0 }, scope),
    ).toEqual({
      message:
        "The engine did not answer. The change may or may not have landed. Reload before trying again.",
      reload: true,
    });
    // AN ENGINE THAT ANSWERED AND FAILED is said as one, with its own words.
    expect(
      refusalWords(
        { kind: "unsettled", answered: true, detail: "the store could not commit", status: 500 },
        scope,
      ),
    ).toEqual({
      message:
        "The engine failed: the store could not commit. The change may or may not have landed. Reload before trying again.",
      reload: true,
    });
    // A PROXY'S 403 OR 413 IS NO REFUSAL OF THE ENGINE'S, however it is
    // spelled: unmarked, it says nothing about the change, and never asks for
    // a grant.
    for (const status of [403, 413]) {
      const read = classifyConfigRefusal({
        status,
        body: { error: "unauthorized", detail: "blocked by policy" },
        fromEngine: false,
      });
      expect(refusalWords(read, scope)).toEqual({
        message:
          "The engine did not answer. The change may or may not have landed. Reload before trying again.",
        reload: true,
      });
    }
    // A REFUSAL ON AUTHORITY names the grant, never a token this browser
    // does not hold.
    expect(refusalWords({ kind: "guarded", code: "unauthorized" }, scope)).toEqual({
      message: CONFIG_WRITE_REASONS.no_grant,
      reload: false,
    });
    expect(refusalWords({ kind: "missing" }, scope).message).toBe(
      "PM is no longer in the company's configuration.",
    );
    expect(
      refusalWords(
        {
          kind: "problems",
          problems: [
            {
              path: "roles[0].token_budget.day",
              segments: null,
              kind: "invalid",
              message: "must be at least 1",
            },
          ],
          derived: null,
          code: "validation_error",
          hint: "",
        },
        scope,
      ),
    ).toEqual({ message: "must be at least 1", reload: false });
  });
});

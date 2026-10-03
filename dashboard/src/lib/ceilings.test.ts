// @vitest-environment node

import { describe, expect, test } from "vitest";
import { readCeiling } from "./budget.ts";
import {
  ceilingSummary,
  ceilingText,
  changesFrom,
  companyPatch,
  chartRefusalWords,
  refusalWords,
  runtimeWithCeilings,
  seatCeilingBody,
} from "./ceilings.ts";
import { RestError } from "~/protocol/rest.ts";

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
    expect(companyPatch({ day: 50_000_000 })).toEqual({ token_budget: { day: 50_000_000 } });
    expect(companyPatch({ week: null })).toEqual({ token_budget: { week: null } });
  });

  // THE RUNTIME AS READ, masks included — the chart restores a mask from the
  // row it patches — and no empty `token_budget` once nothing is capped.
  test("a seat's runtime half is sent back whole, with its ceilings changed", () => {
    const runtime = {
      llm: "zulu",
      mcp_env: { github: { GITHUB_TOKEN: "${CHART_SEAT_X_GITHUB}" } },
      token_budget: { day: 5, week: 9 },
    };
    expect(runtimeWithCeilings(runtime, { day: 7 })).toEqual({
      ...runtime,
      token_budget: { day: 7, week: 9 },
    });
    const bare = runtimeWithCeilings(runtime, { day: null, week: null });
    expect(bare).toEqual({ llm: "zulu", mcp_env: runtime.mcp_env });
    expect("token_budget" in bare).toBe(false);
    expect(runtimeWithCeilings({}, { month: 3 })).toEqual({ token_budget: { month: 3 } });
  });

  // THE WHOLE CONTENT, because a content write is the object's post-state —
  // a field left out is a field cleared — and NEITHER of the two structural
  // fields the content route refuses by name.
  test("a seat's content write restates its content and states its runtime", () => {
    const seat = {
      handle: "pm",
      kind: "agent",
      unit: "product",
      name: "PM",
      email: "${CHART_SEAT_PM_EMAIL}",
      goal: "ship",
      responsibilities: ["plan"],
      former_handles: ["old-pm"],
      runtime: { llm: "zulu", token_budget: { month: 1000 } },
    };
    const body = seatCeilingBody(seat, { day: 2_500_000 });
    expect(body).toEqual({
      unit: "product",
      name: "PM",
      email: "${CHART_SEAT_PM_EMAIL}",
      backstory: "",
      goal: "ship",
      responsibilities: ["plan"],
      behavioral_guidelines: [],
      project: "",
      space: "",
      runtime: { llm: "zulu", token_budget: { month: 1000, day: 2_500_000 } },
    });
    expect("kind" in body || "manages" in body || "handle" in body).toBe(false);
    // A RUNTIME THE CHANGE EMPTIES IS CLEARED, never sent as `{}`.
    const cleared = seatCeilingBody(
      { handle: "x", runtime: { token_budget: { day: 1 } } },
      { day: null },
    );
    expect(cleared.clear_runtime).toBe(true);
    expect("runtime" in cleared).toBe(false);
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
    expect(refusalWords({ kind: "unreachable", detail: "" }, scope)).toEqual({
      message:
        "The engine did not answer, so the change may or may not have landed. Reload before trying again.",
      reload: true,
    });
    expect(refusalWords({ kind: "missing" }, scope).message).toBe(
      "PM is no longer in the org chart.",
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

describe("what the chart's answer to a seat's ceiling means", () => {
  const scope = { kind: "seat", handle: "pm", name: "PM" } as const;
  const refused = (status: number, body: Record<string, unknown>) =>
    chartRefusalWords(new RestError(status, body), scope);

  test("a refusal on authority names the grant", () => {
    expect(
      refused(403, { error: "unauthorized", reason: "no_grant", grants: ["config:write"] }),
    ).toEqual({
      message:
        "Changing PM's ceilings needs config:write, which the credential you presented does not carry.",
      reload: false,
    });
  });

  // A COLLEAGUE'S WRITE TO THIS SEAT LANDED FIRST: re-read, never overwrite.
  test("a stale write saves nothing and asks for a fresh read", () => {
    expect(refused(409, { error: "stale", detail: "lost a race" }).reload).toBe(true);
  });

  // THE CHART'S OWN RULE, in its own words.
  test("a ceiling the chart will not hold is refused in the rule's words", () => {
    expect(
      refused(422, { error: "refused", detail: "token_budget.day must be at least 1" }),
    ).toEqual({ message: "token_budget.day must be at least 1", reload: false });
  });

  test("a seat a removal took says so", () => {
    expect(refused(404, { error: "not_found" })).toEqual({
      message: "PM is no longer in the org chart.",
      reload: true,
    });
  });
});

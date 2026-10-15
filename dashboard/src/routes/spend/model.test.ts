import { describe, expect, test } from "vitest";
import {
  agentLines,
  cacheShare,
  changeVsPrevious,
  changeWords,
  expensiveTasksParams,
  spendCsv,
  spendOffer,
  spendWindowParams,
  taskFacts,
  usedByParts,
  usedByWords,
  windowWords,
} from "./model.ts";
import type { AgentSpendRow, BudgetWindow, Rollup } from "~/protocol/types.ts";

const row = (handle: string, total: number, turns?: number): AgentSpendRow => ({
  role: handle.toUpperCase(),
  handle,
  agent_id: `id-${handle}`,
  input_tokens: total,
  output_tokens: 0,
  total_tokens: total,
  calls: 1,
  by_phase: {},
  turns,
});

const rollup = (extra: Partial<Rollup> = {}): Rollup => ({
  since: "2026-09-23T00:00:00Z",
  until: "2026-09-30T00:00:00Z",
  from: "2026-09-23",
  to: "2026-09-29",
  days: 7,
  totals: { input_tokens: 100, output_tokens: 0, total_tokens: 100, calls: 2 },
  by_phase: [],
  by_model: [],
  by_provider: [],
  by_worker: [],
  by_agent: [],
  ...extra,
});

describe("the window spend asks for", () => {
  // THE ENGINE'S NINETY, and nothing finer than a day: the usage domain holds
  // company days, and a "24 hours" would be answered as two of them.
  test("offers a week, a month and a quarter of company days, and two dates", () => {
    const offer = spendOffer("Asia/Tokyo");
    expect(offer.ranges).toEqual(["7d", "30d", "90d"]);
    expect(offer).toMatchObject({
      custom: true,
      customDays: true,
      fallback: "30d",
      zone: "Asia/Tokyo",
    });
  });

  test("is `days` for a named range and two company dates for a custom one", () => {
    expect(spendWindowParams("30d", "UTC")).toEqual({ days: 30 });
    // The company's clock, not the reader's: 15:00 UTC on the 1st is
    // midnight on the 2nd in Tokyo — and the end is EXCLUSIVE, so a window
    // ending at Tokyo's midnight on the 9th covers the 8th and not the 9th.
    const w = { from: Date.parse("2026-09-01T15:00:00Z"), to: Date.parse("2026-09-08T15:00:00Z") };
    expect(spendWindowParams(w, "Asia/Tokyo")).toEqual({
      since: "2026-09-02",
      until: "2026-09-08",
    });
    // Read on UTC's clock the same two instants are the 1st to the 8th.
    expect(spendWindowParams(w, "UTC")).toEqual({ since: "2026-09-01", until: "2026-09-08" });
  });

  test("is labelled from the answer", () => {
    expect(windowWords(rollup(), true)).toBe("last 7 days");
    expect(windowWords(rollup(), false)).toBe("2026-09-23 – 2026-09-29");
    expect(windowWords(rollup({ from: "2026-09-29", to: "2026-09-29", days: 1 }), false)).toBe(
      "2026-09-29",
    );
  });
});

describe("the change against the window before", () => {
  test("is a signed whole percentage, with a true minus", () => {
    expect(changeWords(changeVsPrevious(118, 100)!)).toBe("+18%");
    expect(changeWords(changeVsPrevious(96, 100)!)).toBe("−4%");
    expect(changeWords(changeVsPrevious(100, 100)!)).toBe("no change");
  });

  // "+∞%" is not a figure: a window before that spent nothing compares with
  // nothing.
  test("is nothing when the window before spent nothing", () => {
    expect(changeVsPrevious(50, 0)).toBeNull();
  });
});

describe("the prompt cache's share", () => {
  // THE ENGINE'S CONTRACT: input already includes the cached prefix, so the
  // share is read ÷ input, never read ÷ (input + read).
  test("is cache reads over input", () => {
    expect(
      cacheShare({
        input_tokens: 800,
        output_tokens: 0,
        total_tokens: 800,
        calls: 1,
        cache_read_tokens: 352,
      }),
    ).toBe(0.44);
  });

  test("is absent where nothing reported a cache, never a zero", () => {
    expect(
      cacheShare({ input_tokens: 800, output_tokens: 0, total_tokens: 800, calls: 1 }),
    ).toBeNull();
    expect(
      cacheShare({
        input_tokens: 800,
        output_tokens: 0,
        total_tokens: 800,
        calls: 1,
        cache_read_tokens: 0,
        cache_write_tokens: 0,
      }),
    ).toBeNull();
    // A cache that was WRITTEN and never read is a reported cache at 0%.
    expect(
      cacheShare({
        input_tokens: 800,
        output_tokens: 0,
        total_tokens: 800,
        calls: 1,
        cache_read_tokens: 0,
        cache_write_tokens: 10,
      }),
    ).toBe(0);
  });
});

describe("the by-agent lines", () => {
  const day: BudgetWindow = {
    period: "day",
    window: "2026-09-29",
    starts_at: "2026-09-29T00:00:00Z",
    resets_at: "2026-09-30T00:00:00Z",
    used: 1,
    limit: 2,
    state: "ok",
  };

  test("rank by tokens, share the window's total and divide by ended turns", () => {
    const lines = agentLines(
      rollup({
        totals: { input_tokens: 400, output_tokens: 0, total_tokens: 400, calls: 2 },
        by_agent: [row("ceo", 100, 4), row("swe", 300, 0), row("idle", 0, 0)],
      }),
      (id) => (id === "id-swe" ? day : undefined),
    );
    expect(lines.map((l) => l.row.handle)).toEqual(["swe", "ceo"]);
    expect(lines.map((l) => l.share)).toEqual([0.75, 0.25]);
    // No turn ended: no average of nothing.
    expect(lines[0]!.perTurn).toBeNull();
    expect(lines[1]!.perTurn).toBe(25);
    expect(lines[0]!.day).toBe(day);
    expect(lines[1]!.day).toBeUndefined();
  });
});

test("'used by' names the top seats and counts the rest", () => {
  const name = (h: string) => h.toUpperCase();
  expect(usedByWords(["swe", "cto", "pm"], 5, name)).toBe("SWE, CTO, PM and 2 more");
  expect(usedByWords(["swe"], 1, name)).toBe("SWE");
  expect(usedByWords([], 0, name)).toBe("");
  // The count travels apart from the names, so a row that cuts the names to
  // fit still says how many more there were.
  expect(usedByParts(["swe", "cto", "pm"], 7, name)).toEqual({ names: "SWE, CTO, PM", more: 4 });
  expect(usedByParts([], 3, name)).toEqual({ names: "3 seats", more: 0 });
});

describe("what drove a task", () => {
  test("says each fact that is not zero, and always the turns", () => {
    expect(taskFacts({ tokens: 1, turns: 22, workers: 0, sent_back: 0, reopens: 4 })).toBe(
      "22 turns · reopened 4 times",
    );
    expect(taskFacts({ tokens: 1, turns: 1, workers: 3, sent_back: 1, reopens: 1 })).toBe(
      "1 turn · reopened 1 time · 3 workers · sent back 1 time",
    );
  });

  // THE WINDOW CHOOSES WHICH TASKS; the order and the facts are the tracker's.
  // BOTH EDGES: a window that ended last month lists nothing that moved only
  // this week.
  test("asks the tracker for what last changed inside the window, by tokens", () => {
    expect(expensiveTasksParams("2026-09-23T00:00:00Z", "2026-09-30T00:00:00Z", 3)).toEqual({
      sort: "-spend_tokens",
      closed_since: "2026-09-23T00:00:00Z",
      updated: "range:2026-09-23T00:00:00Z..2026-09-30T00:00:00Z",
      spend_tokens: "gt:0",
      fields: "spend",
      limit: 3,
    });
  });
});

describe("the export", () => {
  // TOKENS ONLY, every row labelled with the window and its section.
  test("carries the rollup and the by-agent table, and no price", () => {
    const r = rollup({
      by_phase: [
        { phase: "execute", input_tokens: 90, output_tokens: 0, total_tokens: 90, calls: 1 },
      ],
      by_agent: [row("=cmd", 90, 3)],
    });
    const csv = spendCsv(
      r,
      agentLines(r, () => undefined),
      "2026-09-23..2026-09-29",
    );
    const lines = csv.split("\r\n");
    expect(lines[0]).toBe(
      '"window","section","name","tokens","input_tokens","output_tokens","cache_read_tokens","calls","turns"',
    );
    expect(lines).toContain('"2026-09-23..2026-09-29","phase","execute",90,90,0,0,1,""');
    // A seat's handle a person typed is text in a spreadsheet, never a formula.
    expect(lines).toContain(`"2026-09-23..2026-09-29","agent","'=cmd",90,90,0,0,1,3`);
    expect(csv).not.toMatch(/cost|usd|price/i);
  });
});

/**
 * The readings a seat's profile makes of what the engine sent, pinned one by
 * one — each test names what the reading protects.
 */

import { describe, expect, test } from "vitest";

import {
  companyCeilings,
  companyCeilingShort,
  AGENT_TABS,
  HUMAN_TABS,
  feedRows,
  fortnight,
  mayReadRecord,
  pillWord,
  placementWords,
  sandboxWords,
  seatSchedules,
  signed,
  tokensTile,
  turnTokens,
  workersWords,
} from "./profile.ts";
import { indexOrg } from "~/lib/seats.ts";
import type {
  AgentRow,
  BudgetWindow,
  ConfigRole,
  ScheduleRow,
  SeatActivityRow,
  TurnRow,
} from "~/protocol/index.ts";

const window_ = (period: BudgetWindow["period"], used: number, limit?: number): BudgetWindow => ({
  period,
  window: period,
  starts_at: "2026-09-21T00:00:00Z",
  resets_at: "2026-09-22T00:00:00Z",
  used,
  ...(limit !== undefined ? { limit } : {}),
  state: "ok",
});

describe("the tabs", () => {
  test("a person has no runtime tab, and every tab of theirs is an agent's too", () => {
    expect(HUMAN_TABS).toEqual(["overview", "work", "settings"]);
    for (const tab of HUMAN_TABS) expect(AGENT_TABS).toContain(tab);
    // THE TABS AN EARLIER BUILD HAD are gone, so a link naming one is stale.
    for (const gone of ["model", "cost", "access", "threads"]) {
      expect(AGENT_TABS as readonly string[]).not.toContain(gone);
    }
  });
});

describe("the tokens tile", () => {
  // ONE WINDOW'S FIGURE OVER THAT WINDOW'S CEILING: the week's spend over a
  // day's cap would be a ratio of two different quantities.
  test("is the capped window nearest its ceiling, with that ceiling", () => {
    const got = tokensTile([window_("day", 100, 1_000), window_("week", 900, 1_000)], 5);
    expect(got.window?.period).toBe("week");
    expect(got.label).toBe("Tokens · this week");
    expect(got.value).toBe(900);
    expect(got.of).toBe(1_000);
  });

  test("breaks a tie by the window that resets sooner", () => {
    const got = tokensTile([window_("month", 500, 1_000), window_("day", 50, 100)], 5);
    expect(got.window?.period).toBe("day");
  });

  test("is the counted week, with no ceiling, where nothing caps the seat", () => {
    const got = tokensTile([window_("day", 100)], 4_321);
    expect(got).toEqual({ window: null, label: "Tokens · 7 days", value: 4_321, of: null });
    expect(tokensTile(undefined, undefined).value).toBeUndefined();
  });
});

describe("the week's change", () => {
  test("carries its own sign, and none for no change", () => {
    expect(signed(9)).toBe("+9");
    expect(signed(-3)).toBe("−3");
    expect(signed(0)).toBe("±0");
  });

  test("the fortnight is the week before and then this one, oldest first, from one answer", () => {
    const row = {
      per_day: [{ day: "2026-09-21", turns: 6, failed: 0, tokens: 0 }],
      previous: { per_day: [{ day: "2026-09-14", turns: 5, failed: 1, tokens: 0 }] },
    } as unknown as SeatActivityRow;
    expect(fortnight(row).map((d) => d.day)).toEqual(["2026-09-14", "2026-09-21"]);
    expect(fortnight(undefined)).toEqual([]);
  });
});

describe("the state pill", () => {
  test("names why a stopped seat stopped, in the engine's own reason", () => {
    const stopped = (reason: string) =>
      ({ role: "SWE", activity: "stopped", stopped_reason: reason }) as unknown as AgentRow;
    expect(pillWord(stopped("paused"))).toBe("Paused");
    expect(pillWord(stopped("budget"))).toBe("Stopped · budget");
    expect(pillWord(stopped("unplaced"))).toBe("Not placed");
    // A reason this build does not know is not guessed at.
    expect(pillWord(stopped("gremlins"))).toBe("Stopped");
  });
});

describe("the current turn's calls", () => {
  const row = (executions: number, running: boolean): AgentRow =>
    ({
      role: "SWE",
      live_call: {
        turn_id: "t-7",
        in_progress: true,
        total_tokens: 2_500,
        tool_executions: Array.from({ length: executions }, (_, i) => ({
          name: `tool.${i}`,
          round: i,
          arguments: { i: String(i) },
          started_at: "2026-09-21T09:48:02Z",
          duration_ms: 300,
        })),
        running_call: running
          ? { round: 9, name: "sandbox.run", arguments: "{}", started_at: "2026-09-21T09:51:31Z" }
          : null,
      },
    }) as unknown as AgentRow;

  // THE RUNNING CALL IS THE ROW THE CARD EXISTS FOR, so it is never the one cut.
  test("keeps the running call and the newest calls before it, within the limit", () => {
    const rows = feedRows(row(8, true), 5);
    expect(rows).toHaveLength(5);
    expect(rows.map((r) => r.name)).toEqual([
      "tool.4",
      "tool.5",
      "tool.6",
      "tool.7",
      "sandbox.run",
    ]);
    expect(rows.at(-1)?.running).toBe(true);
    expect(feedRows(row(8, false), 5).map((r) => r.name)[0]).toBe("tool.3");
  });

  test("reads each call's time and duration off the engine's own record", () => {
    const [first] = feedRows(row(1, false));
    expect(first).toMatchObject({ at: "2026-09-21T09:48:02Z", tookMs: 300, words: "i 0" });
  });

  test("counts the turn's tokens as its recorded phases and the one running", () => {
    const newest = { turn_id: "t-7", total_tokens: 40_000 } as TurnRow;
    expect(turnTokens("t-7", newest, true, row(0, true))).toBe(42_500);
    // A newest row naming another turn is an answer: nothing recorded yet.
    expect(turnTokens("t-7", { ...newest, turn_id: "t-6" }, true, row(0, true))).toBe(2_500);
    // No answer is not a zero.
    expect(turnTokens("t-7", undefined, false, row(0, true))).toBeNull();
  });
});

describe("the setup card's words", () => {
  const role = (over: Partial<ConfigRole>): ConfigRole => ({ name: "SWE", ...over });

  test("a sandbox is not offered until it is enabled, and a default is said as one", () => {
    expect(sandboxWords(role({}))).toBe("not offered");
    expect(sandboxWords(role({ sandbox: { enabled: false, run_in: "e2b" } }))).toBe("not offered");
    expect(
      sandboxWords(role({ sandbox: { enabled: true, run_in: "e2b", coding_agent: "opencode" } })),
    ).toBe("e2b · opencode");
    expect(sandboxWords(role({ sandbox: { enabled: true } }))).toBe("the provider's default cell");
  });

  test("placement is a pinned node, a set of labels, or any node", () => {
    expect(placementWords(role({ placement: { node: "node-2" } }))).toBe("only node-2");
    expect(placementWords(role({ placement: { labels: { pool: "build", gpu: "no" } } }))).toBe(
      "nodes labelled pool=build, gpu=no",
    );
    expect(placementWords(role({}))).toBe("any node");
  });

  // `workers:` NARROWS the company's templates; empty is every one, not none.
  test("an empty worker list is every template, never none", () => {
    expect(workersWords(role({}))).toBe("every template the company defines");
    expect(workersWords(role({ workers: ["test-writer"] }))).toBe("test-writer");
  });
});

describe("the recurring work", () => {
  const row = (over: Partial<ScheduleRow>): ScheduleRow => ({
    scope_type: "role",
    scope_id: "swe",
    scope_name: "swe",
    name: "x",
    cron: "0 9 * * *",
    timezone: "UTC",
    task: "t",
    target: "",
    enabled: true,
    timeout_seconds: 0,
    catchup: false,
    runners: ["swe"],
    ...over,
  });

  test("is the seat's own and every unit schedule it runs, soonest first, the unfireable last", () => {
    const rows = [
      row({ name: "later", next_run: "2026-09-22T09:00:00Z" }),
      row({ name: "broken", problem: "unknown zone" }),
      row({
        name: "unit",
        scope_type: "unit",
        scope_id: "Core",
        scope_name: "Core",
        next_run: "2026-09-21T10:00:00Z",
      }),
      row({
        name: "not-ours",
        scope_id: "cto",
        scope_name: "cto",
        runners: ["cto"],
        next_run: "2026-09-21T08:00:00Z",
      }),
    ];
    expect(seatSchedules(rows, "swe").map((r) => r.name)).toEqual(["unit", "later", "broken"]);
  });
});

// THE COMPANY'S CEILING IS NAMED — its figure and its window — so a seat that
// writes none of its own says what binds it, as the Settings tab does.
describe("the company's ceilings", () => {
  test("one window is named with its rate, and nothing capped is nothing", () => {
    expect(companyCeilings({ day: 60_000_000 })).toBe("the company's 60M/day applies");
    expect(companyCeilings({})).toBe("");
    expect(companyCeilings(undefined)).toBe("");
  });
  test("several windows are listed in the calendar's order and agree in number", () => {
    expect(companyCeilings({ month: 900_000_000, day: 60_000_000 })).toBe(
      "the company's 60M/day and 900M/month apply",
    );
    expect(companyCeilings({ day: 1_000_000, week: 5_000_000, month: 20_000_000 })).toBe(
      "the company's 1M/day, 5M/week and 20M/month apply",
    );
  });
  // A TILE'S LINE HOLDS ONE: the first window, and how many more there are.
  test("a tile's line names the first window and counts the rest", () => {
    expect(companyCeilingShort({ month: 900_000_000, day: 60_000_000 })).toBe(
      "company cap 60M/day +1",
    );
    expect(companyCeilingShort({ week: 5_000_000 })).toBe("company cap 5M/week");
    expect(companyCeilingShort({})).toBe("");
  });
});

describe("whose record this page asks for", () => {
  const seat = (handle: string, reports: string[], managers: string[]) => ({
    handle,
    name: handle,
    kind: "human",
    placed_by_ref: false,
    manager: managers[0] ?? "",
    managers: managers.length ? managers : null,
    reports: reports.length ? reports : null,
    auto_reports: null,
    onboarding_chain: null,
  });
  const roles = ["ana", "ceo", "bo"].map((handle) => ({ name: handle, handle, kind: "human" }));
  // Ana leads the CEO; Bo leads nobody.
  const led = indexOrg({
    roles,
    units: [],
    derived: {
      units: [],
      seats: [seat("ana", ["ceo"], []), seat("ceo", [], ["ana"]), seat("bo", [], [])],
    },
  } as never);
  const reader = (handle: string, operatesFleet = false) => ({ handle, operatesFleet });

  // THE ENGINE'S RULE, all three arms: the owner, whoever leads them, and
  // `fleet:operate`.
  test("is the owner's, a lead's and fleet:operate's", () => {
    expect(mayReadRecord(reader("ceo"), led, "ceo")).toBe(true);
    expect(mayReadRecord(reader("ana"), led, "ceo")).toBe(true);
    expect(mayReadRecord(reader("", true), led, "ceo")).toBe(true);
  });

  test("is not asked by a colleague who leads them not, nor by nobody", () => {
    expect(mayReadRecord(reader("bo"), led, "ceo")).toBe(false);
    expect(mayReadRecord(reader(""), led, "ceo")).toBe(false);
  });

  // NULL IS NOT FALSE: with no hierarchy this client cannot tell who leads
  // whom, so it asks and the engine decides.
  test("is asked where the chart does not say who leads whom", () => {
    expect(mayReadRecord(reader("bo"), indexOrg({ roles } as never), "ceo")).toBe(true);
  });
});

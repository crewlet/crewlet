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
  lastTurnEnded,
  liveRow,
  mayReadRecord,
  mayReadTrail,
  seatRun,
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
  ExecutorEntry,
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
  });
});

describe("which live row is the seat's", () => {
  const seat = { name: "SWE", handle: "swe-platform" } as Parameters<typeof liveRow>[2];
  // A ROSTER ROW'S `id` IS ITS SEAT'S HANDLE, as the engine sends it.
  const row = (handle: string, role: string) => ({ id: handle, role, handle }) as AgentRow;

  // A ROLE NAME IS SHARED by two unit seats stamped from one template, so the
  // sibling listed first under it is not this seat's row — the handle says
  // which one is.
  test("the seat's own handle wins over an earlier sibling sharing its role name", () => {
    const agents = [row("swe-core", "SWE"), row("swe-platform", "SWE")];
    expect(liveRow(agents, "swe-platform", seat)?.id).toBe("swe-platform");
  });

  // A ROUTE THAT NAMED SOMETHING ELSE still finds the row by the handle the
  // seat resolved to.
  test("a seat the route named otherwise is found by the handle it resolved to", () => {
    const agents = [row("swe-core", "SWE"), row("swe-platform", "SWE")];
    expect(liveRow(agents, "SWE", seat)?.id).toBe("swe-platform");
  });

  // NEVER BY THE ROLE NAME, which two seats may share: a row that is not the
  // seat's own is no row of the seat's, whatever it is called.
  test("a row is never matched by its role name", () => {
    expect(liveRow([row("swe-core", "SWE")], "swe-platform", seat)).toBeUndefined();
    expect(liveRow([row("swe-core", "SWE")], "swe-platform", null)).toBeUndefined();
    expect(liveRow([row("swe-platform", "SWE")], "swe-platform", null)?.id).toBe("swe-platform");
  });
});

describe("which coding run is the seat's", () => {
  const seat = { name: "SWE", handle: "swe-platform" } as Parameters<typeof seatRun>[1];
  const run = (turn: string, role: string, handle: string) =>
    ({ turn_id: turn, role, agent_handle: handle }) as ExecutorEntry;

  // A SIBLING'S RUN parked on a question is not this seat's question.
  test("a run is the seat's by its handle, never a sibling's by the shared role name", () => {
    expect(seatRun([run("t-core", "SWE", "swe-core")], seat)).toBeNull();
    const runs = [run("t-core", "SWE", "swe-core"), run("t-mine", "SWE", "swe-platform")];
    expect(seatRun(runs, seat)?.turn_id).toBe("t-mine");
  });

  test("the role name is never read", () => {
    expect(seatRun([run("t-nameless", "SWE", "")], seat)).toBeNull();
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

describe("the current turn's calls", () => {
  const row = (executions: number, running: boolean | number): AgentRow =>
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
          result: "",
          success: true,
          started_at: "2026-09-21T09:48:02Z",
          duration_ms: 300,
          call_id: `done-${i}`,
        })),
        running_calls: Array.from({ length: Number(running) }, (_, i) => ({
          id: `run-${i}`,
          round: 9,
          name: i === 0 ? "sandbox.run" : `read.${i}`,
          arguments: "{}",
          started_at: "2026-09-21T09:51:31Z",
        })),
      },
    }) as unknown as AgentRow;

  // THE RUNNING CALLS ARE THE ROWS THE CARD EXISTS FOR, so an answered call is
  // cut before any of them.
  test("keeps the running calls and the newest calls before them, within the limit", () => {
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
    // Three side by side: every one of them, and two answered calls.
    const three = feedRows(row(8, 3), 5);
    expect(three.map((r) => [r.name, r.running])).toEqual([
      ["tool.6", false],
      ["tool.7", false],
      ["sandbox.run", true],
      ["read.1", true],
      ["read.2", true],
    ]);
    // More side by side than the card holds: the newest of them, and nothing
    // answered.
    expect(feedRows(row(8, 7), 5).map((r) => r.name)).toEqual([
      "read.2",
      "read.3",
      "read.4",
      "read.5",
      "read.6",
    ]);
  });

  // A CALL IS ONE ROW FOR ITS WHOLE LIFE: keyed by its own id running and
  // answered alike, so the row a reader is watching is not replaced by another
  // when its result lands.
  test("keys a call by its own id, running and answered alike", () => {
    expect(feedRows(row(1, 1)).map((r) => r.key)).toEqual(["call-done-0", "call-run-0"]);
  });

  test("reads each call's time and duration off the engine's own record", () => {
    const [first] = feedRows(row(1, false));
    expect(first).toMatchObject({ at: "2026-09-21T09:48:02Z", tookMs: 300, words: "i 0" });
  });

  // A FAILED CALL THAT SAID NOTHING IS STILL A FAILURE: the engine writes
  // `success: false` on every one, and `error` only repeats its words.
  test("reads a failure off success, whether or not the call said anything", () => {
    const failing = (result: string): AgentRow => {
      const r = row(1, false);
      const ex = r.live_call!.tool_executions![0]!;
      r.live_call!.tool_executions = [
        { ...ex, result, success: false, ...(result ? { error: result } : {}) },
      ];
      return r;
    };
    expect(feedRows(failing(""))[0]?.failed).toBe(true);
    expect(feedRows(failing("exit status 1"))[0]?.failed).toBe(true);
    expect(feedRows(row(1, false))[0]?.failed).toBe(false);
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

  // EVERY AGENT SEAT'S EXECUTOR RUNS IN A BOX, so the row never says one is
  // missing: a seat with no block runs on the catalogue's defaults, and one
  // with a block is said by what it adds.
  test("a seat's box is the catalogue's defaults, or what its own block adds", () => {
    expect(sandboxWords(role({}))).toBe("The catalogue's defaults");
    expect(sandboxWords(role({ sandbox: {} }))).toBe("The catalogue's defaults");
    expect(
      sandboxWords(
        role({
          sandbox: {
            setup: [{ name: "deps" }, { name: "lint" }],
            env: { GH_TOKEN: "${GH}", REGION: "eu" },
            mcp: { servers: ["github"] },
            max_turns: 40,
          },
        }),
      ),
    ).toBe("2 setup steps · 2 variables · 1 tool server · up to 40 turns");
    // Zero is an explicit "uncapped", said as itself rather than as a cap of 0.
    expect(sandboxWords(role({ sandbox: { max_turns: 0 } }))).toBe("No turn cap");
  });

  test("placement is a pinned node, a set of labels, or any node", () => {
    expect(placementWords(role({ placement: { node: "node-2" } }))).toBe("Only node-2");
    expect(placementWords(role({ placement: { labels: { pool: "build", gpu: "no" } } }))).toBe(
      "Nodes labelled pool=build, gpu=no",
    );
    expect(placementWords(role({}))).toBe("Any node");
  });

  // `workers:` NARROWS the company's templates; empty is every one, not none.
  test("an empty worker list is every template, never none", () => {
    expect(workersWords(role({}))).toBe("Every template the company defines");
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
  const reader = (handle: string, admin = false, worklet = true) => ({
    handle,
    admin,
    worklet: worklet || admin,
  });

  // THE ENGINE'S RULE, all three arms, each with Worklet: the owner, whoever
  // leads them, and an admin, who holds every app.
  test("is the owner's, a lead's and an admin's", () => {
    expect(mayReadRecord(reader("ceo"), led, "ceo")).toBe(true);
    expect(mayReadRecord(reader("ana"), led, "ceo")).toBe(true);
    expect(mayReadRecord(reader("", true), led, "ceo")).toBe(true);
  });

  test("is not asked by a colleague who leads them not, nor by nobody", () => {
    expect(mayReadRecord(reader("bo"), led, "ceo")).toBe(false);
    expect(mayReadRecord(reader(""), led, "ceo")).toBe(false);
  });

  // A PERSON'S RECORD IS WORKLET'S: without the app nobody's is asked, their
  // own and their report's included, since the engine refuses every record
  // question to a reader without Worklet.
  test("is asked by nobody without Worklet, the owner and the lead included", () => {
    expect(mayReadRecord(reader("ceo", false, false), led, "ceo")).toBe(false);
    expect(mayReadRecord(reader("ana", false, false), led, "ceo")).toBe(false);
  });
});

// A SEAT'S TRAIL — its turns, its memory, the body of a call — is an admin's
// and its lead's (`audit.seat.read`), and nobody else's, whatever app they
// hold: everybody reads its status, never its transcript.
describe("mayReadTrail", () => {
  const seat = (handle: string, reports: string[], managers: string[]) => ({
    handle,
    name: handle,
    kind: handle === "agent" ? "agent" : "human",
    placed_by_ref: false,
    manager: managers[0] ?? "",
    managers: managers.length ? managers : null,
    reports: reports.length ? reports : null,
    auto_reports: null,
    onboarding_chain: null,
  });
  // Ana leads the agent; Bo leads nobody.
  const roles = ["ana", "agent", "bo"].map((handle) => ({
    name: handle,
    handle,
    kind: handle === "agent" ? "agent" : "human",
  }));
  const led = indexOrg({
    roles,
    units: [],
    derived: {
      units: [],
      seats: [seat("ana", ["agent"], []), seat("agent", [], ["ana"]), seat("bo", [], [])],
    },
  } as never);

  test("is the lead's and an admin's", () => {
    expect(mayReadTrail({ handle: "ana", admin: false }, led, "agent")).toBe(true);
    expect(mayReadTrail({ handle: "", admin: true }, led, "agent")).toBe(true);
  });

  test("is not a colleague's who leads it not, nor nobody's", () => {
    expect(mayReadTrail({ handle: "bo", admin: false }, led, "agent")).toBe(false);
    expect(mayReadTrail({ handle: "", admin: false }, led, "agent")).toBe(false);
  });
});

// A TURN A PERSON STOPPED IS SAID TO HAVE BEEN STOPPED — never "failed", which it
// did not, and never a bare "ended", which hides that anybody stopped it.
test("a seat's last turn says how it ended, a person's stop included", () => {
  expect(lastTurnEnded("completed")).toBe("ended");
  expect(lastTurnEnded("failed")).toBe("failed");
  expect(lastTurnEnded("stopped")).toBe("was stopped");
});

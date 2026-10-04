/**
 * The landing screen's sentences and its arithmetic, over values.
 *
 * The one failure these guard is prose that reads wrong the day a number is
 * zero or one: "0 agents are working", "1 conditions need", a "+0%" delta over
 * a window nothing happened in. Each clause is exercised at none, one and many.
 */

import { describe, expect, test } from "vitest";
import type {
  AgentRow,
  BudgetWindow,
  FeedEntry,
  FeedScheduleRun,
  WorkFlowPoint,
} from "~/protocol/index.ts";
import {
  budgetCaption,
  completedIn,
  crewOf,
  crewParts,
  deltaWords,
  flowPoints,
  mergePages,
  rangeOf,
  scheduleVerb,
  sentenceText,
  statusSentence,
  inProgressParts,
  joinParts,
  troubleParts,
  waitingOf,
} from "./model.ts";
import { seatDecisionsFor } from "~/components/DecisionRow.tsx";

const base = {
  company: "Nimbus",
  connected: true,
  authRejected: false,
  configured: true,
  posture: "converged",
  draining: false,
  nodes: 3,
  waiting: null,
  conditions: 0,
  working: 0,
};

describe("the status sentence", () => {
  test("never prints a zero or a wrong plural", () => {
    expect(
      sentenceText(
        statusSentence({ ...base, waiting: { count: 0, floor: false, oldestAt: null } }),
      ),
    ).toBe(
      "Nimbus is running on 3 nodes. Nothing needs your decision, and no agents are working right now.",
    );
    expect(
      sentenceText(
        statusSentence({
          ...base,
          waiting: { count: 1, floor: false, oldestAt: null },
          conditions: 1,
          working: 1,
        }),
      ),
    ).toBe(
      "Nimbus is running on 3 nodes. 1 decision is waiting on you, 1 condition needs a look, and 1 agent is working right now.",
    );
    expect(
      sentenceText(
        statusSentence({
          ...base,
          nodes: 1,
          waiting: { count: 3, floor: false, oldestAt: null },
          conditions: 2,
          working: 4,
        }),
      ),
    ).toBe(
      "Nimbus is running on 1 node. 3 decisions are waiting on you, 2 conditions need a look, and 4 agents are working right now.",
    );
    // EVERY CLAUSE AT NONE, ONE AND MANY — the fleet's own included: a
    // presence read that counted nobody is not "running on 0 nodes".
    for (const n of [0, 1, 2, 3, 4]) {
      for (const nodes of [0, 1, 2, 3]) {
        const text = sentenceText(
          statusSentence({
            ...base,
            nodes,
            waiting: { count: n, floor: false, oldestAt: null },
            conditions: n,
            working: n,
          }),
        );
        expect(text).not.toMatch(/\b0 /);
        expect(text).not.toMatch(/\b1 (decisions|conditions|agents|nodes)\b/);
        expect(text).not.toMatch(/\b1 condition need\b|\b[2-9] conditions needs\b/);
        expect(text).not.toMatch(/\b[2-9] node\b/);
      }
    }
  });

  test("weights the decisions figure, and only it", () => {
    const runs = statusSentence({
      ...base,
      waiting: { count: 3, floor: false, oldestAt: null },
      working: 4,
    });
    expect(runs.filter((r) => r.strong).map((r) => r.text)).toEqual(["3 decisions"]);
  });

  test("a capped count is a floor, and plural", () => {
    expect(
      sentenceText(statusSentence({ ...base, waiting: { count: 1, floor: true, oldestAt: null } })),
    ).toContain("1+ decisions are waiting on you");
  });

  test("says nothing about decisions for a reader nobody bound", () => {
    expect(sentenceText(statusSentence({ ...base, working: 2 }))).toBe(
      "Nimbus is running on 3 nodes. 2 agents are working right now.",
    );
  });

  test("an engine condition takes the sentence over", () => {
    expect(sentenceText(statusSentence({ ...base, authRejected: true }))).toMatch(
      /refused this browser's token/,
    );
    expect(sentenceText(statusSentence({ ...base, connected: false }))).toMatch(/^Not connected/);
    expect(sentenceText(statusSentence({ ...base, configured: false }))).toMatch(
      /^No configuration is active/,
    );
    expect(sentenceText(statusSentence({ ...base, posture: "shed" }))).toMatch(
      /released its seats/,
    );
    expect(sentenceText(statusSentence({ ...base, draining: true }))).toMatch(
      /this node is draining/,
    );
  });

  test("an unknown fleet size is not written as a number", () => {
    expect(sentenceText(statusSentence({ ...base, nodes: undefined }))).toMatch(
      /^Nimbus is running\. /,
    );
    expect(sentenceText(statusSentence({ ...base, nodes: 0 }))).toMatch(/^Nimbus is running\. /);
  });

  test("a condition that needs a look says where it is", () => {
    const runs = statusSentence({ ...base, conditions: 1, conditionsHref: "#/inbox?row=x" });
    const clause = runs.find((r) => r.text.includes("needs a look"));
    expect(clause?.href).toBe("#/inbox?row=x");
    expect(runs.filter((r) => r.href)).toHaveLength(1);
  });
});

describe("the figures", () => {
  const day = (completed: number, active = 0): WorkFlowPoint => ({
    window: "2026-09-01",
    start: "",
    end: "",
    not_started: 0,
    active,
    done: 0,
    closed: 0,
    completed,
  });

  test("the window and the window before it", () => {
    const points = [1, 2, 3, 4, 5, 6, 7, 10, 10, 10, 10, 10, 10, 10].map((n) => day(n));
    expect(completedIn(points, 7)).toEqual({ current: 70, previous: 28 });
    expect(completedIn(points, 1)).toEqual({ current: 10, previous: 10 });
    expect(completedIn(points.slice(-10), 7).previous).toBeNull();
    expect(flowPoints(rangeOf("30d"))).toBe(60);
    expect(flowPoints(rangeOf("today"))).toBe(14);
    expect(rangeOf("bogus").value).toBe("7d");
  });

  test("a delta is signed, and absent where there is nothing to compare", () => {
    expect(deltaWords(46, 41, true)).toBe("+12%");
    expect(deltaWords(18, 22, false)).toBe("−4");
    expect(deltaWords(5, 0, true)).toBe("+5");
    expect(deltaWords(5, null, true)).toBeNull();
  });

  test("a sub-line names only the parts with somebody in them", () => {
    const row = (activity: AgentRow["activity"]): AgentRow => ({
      id: activity ?? "",
      agent_id: activity ?? "",
      role: "r",
      activity,
    });
    const crew = crewOf([row("working"), row("working"), row("needs"), row("idle")]);
    expect(crew).toEqual({ working: 2, needs: 1, stopped: 0, idle: 1, total: 4 });
    expect(joinParts(crewParts(crew))).toBe("1 waiting · 1 idle");
    expect(joinParts(crewParts(crewOf([])))).toBe("No agent seats in the chart");
    expect(troubleParts({ blocked: 2, overdue: 0 })).toEqual(["2 blocked"]);
    expect(troubleParts({ blocked: 0, overdue: 0 })).toEqual([]);
    // THE PARTS ARE WHOLE FACTS, so a narrow tile breaks between them.
    expect(inProgressParts(true, ["2 blocked", "1 overdue"])).toEqual([
      "vs last week",
      "2 blocked",
      "1 overdue",
    ]);
    expect(inProgressParts(false, [])).toEqual([]);
  });

  test("the budget caption is the week's and says when it resets", () => {
    const week: BudgetWindow = {
      period: "week",
      window: "2026-W39",
      starts_at: "2026-09-21T00:00:00Z",
      resets_at: "2026-09-28T00:00:00Z",
      used: 630,
      limit: 1000,
      state: "ok",
    };
    expect(joinParts(budgetCaption(week, "UTC"))).toBe("63% of this week's budget · resets Mon");
    expect(joinParts(budgetCaption({ ...week, state: "refusing" }, "UTC"))).toBe(
      "This week's budget is spent · resets Mon",
    );
  });

  // A STOPPED SEAT WAITS ON A READER ONLY WHERE THEY CAN TAKE A WAY OUT:
  // raise the ceiling (a `/config` write, under `config:write`) or hand the
  // item on (`update_work_item`, and only with an item to hand).
  test("a stopped seat is the reader's decision only where they can act on it", () => {
    const onItem = {
      row: { role: "SWE", turn: { work_item: { id: "t-1", key: "ENG-1" } } } as never,
    };
    const between = { row: { role: "DevRel" } as never };
    const operator = { grants: ["config:write"], acts: [] };
    const reassigner = { grants: ["state:read"], acts: ["update_work_item"] };
    const neither = { grants: ["state:read"], acts: ["comment_on_work_item"] };
    expect(seatDecisionsFor([onItem, between], operator)).toEqual({
      mine: [onItem, between],
      others: [],
    });
    expect(seatDecisionsFor([onItem, between], reassigner)).toEqual({
      mine: [onItem],
      others: [between],
    });
    expect(seatDecisionsFor([onItem, between], neither)).toEqual({
      mine: [],
      others: [onItem, between],
    });
  });

  test("what waits is the engine's count plus the stopped seats, oldest of all", () => {
    const waiting = waitingOf(
      { handle: "jane", items: [], total: 2, capped: false, oldest_at: "2026-09-22T10:00:00Z" },
      [{ at: "2026-09-22T08:00:00Z" }],
    );
    expect(waiting).toEqual({ count: 3, floor: false, oldestAt: "2026-09-22T08:00:00Z" });
    expect(waitingOf(null, [])).toBeNull();
  });
});

describe("the feed", () => {
  const run = (over: Partial<FeedScheduleRun> = {}): FeedScheduleRun => ({
    scope_type: "role",
    scope_id: "pm",
    name: "backlog-sweep",
    target: "pm",
    outcome: "fired",
    runs: 1,
    ...over,
  });

  test("a tick nobody ran never reads as a run", () => {
    expect(scheduleVerb(run())).toBe("ran");
    expect(scheduleVerb(run({ runs: 12 }))).toBe("ran 12 times");
    for (const outcome of ["skipped_catchup", "skipped_paused", "something_new"]) {
      for (const runs of [1, 3]) {
        expect(scheduleVerb(run({ outcome, runs }))).not.toMatch(/\bran\b/);
      }
    }
    expect(scheduleVerb(run({ outcome: "skipped_catchup" }))).toBe("skipped a missed tick");
    expect(scheduleVerb(run({ outcome: "skipped_catchup", runs: 3 }))).toBe(
      "skipped 3 missed ticks",
    );
  });

  test("a schedule's runs split across a page boundary are one row", () => {
    const entry = (at: string, over: Partial<FeedScheduleRun> = {}): FeedEntry => ({
      kind: "schedule",
      at,
      schedule: run(over),
    });
    const rows = mergePages([
      {
        complete: true,
        rows: [
          { kind: "completed", at: "2026-09-22T10:00:00Z", work: undefined },
          entry("2026-09-22T09:50:00Z", { runs: 4, since: "2026-09-22T09:20:00Z" }),
        ],
      },
      {
        complete: true,
        rows: [
          entry("2026-09-22T09:10:00Z", { runs: 2, since: "2026-09-22T09:00:00Z" }),
          entry("2026-09-22T08:50:00Z", { target: "cto" }),
        ],
      },
    ]);
    expect(rows).toHaveLength(3);
    expect(rows[1]!.schedule).toMatchObject({ runs: 6, since: "2026-09-22T09:00:00Z" });
    expect(rows[1]!.at).toBe("2026-09-22T09:50:00Z");
    expect(rows[2]!.schedule?.target).toBe("cto");
  });
});

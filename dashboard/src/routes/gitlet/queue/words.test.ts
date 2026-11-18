// @vitest-environment node
/** Gitlet › Queue's words, each from what the engine answered and nothing guessed. */

import { describe, expect, test } from "vitest";

import type { CodeCheckRow, CodeEjectedRow, CodeRuleGate } from "~/protocol/index.ts";
import {
  checksWords,
  ejectionWords,
  elapsedWords,
  gateAsksApproval,
  gateChecks,
  summaryWords,
  trainEnd,
  trainMeter,
} from "./words.ts";

const NOW = Date.parse("2026-11-18T12:00:00Z");
const ago = (minutes: number) => new Date(NOW - minutes * 60_000).toISOString();

function gate(pattern: string, checks?: string[], approvals = "one"): CodeRuleGate {
  return {
    pattern,
    gate: {
      merge_by: "writers",
      approvals,
      checks,
      landing: { depth: 8, batch: 4 },
      delete_source: "seats",
    },
  };
}

function check(name: string, state: string, more: Partial<CodeCheckRow> = {}): CodeCheckRow {
  return {
    repo_id: "id-web",
    repo: "web",
    oid: "c".repeat(40),
    target: "refs/heads/main",
    name,
    attempt: 1,
    state,
    required: true,
    ...more,
  };
}

function ejected(more: Partial<CodeEjectedRow>): CodeEjectedRow {
  return {
    repo_id: "id-api",
    repo: "api",
    entry_id: "e1",
    target_ref: "refs/heads/main",
    change_id: "c1",
    number: 17,
    head_oid: "a".repeat(40),
    enqueued_by: { handle: "cto", kind: "agent" },
    reason: "check_failed",
    enqueued_at: ago(23),
    ejected_at: ago(20),
    ...more,
  };
}

describe("gateChecks", () => {
  // Mutation: keep a gate whose pattern the engine did not match, or keep a duplicate.
  test("the union over the matched rules' gates, sorted, each once", () => {
    const gates = [gate("main", ["unit", "e2e"]), gate("m*", ["lint", "unit"]), gate("dev", ["x"])];
    expect(gateChecks(["main", "m*"], gates)).toEqual(["e2e", "lint", "unit"]);
  });

  test("a matched rule with no checks requires none", () => {
    expect(gateChecks(["main"], [gate("main")])).toEqual([]);
  });

  // Mutation: answer [] for a read that is missing, which draws "No required checks".
  test("either read missing is null", () => {
    expect(gateChecks(null, [gate("main", ["unit"])])).toBeNull();
    expect(gateChecks(["main"], null)).toBeNull();
  });

  test("an approval is asked where any matched gate asks one", () => {
    expect(gateAsksApproval(["main"], [gate("main", ["unit"], "none")])).toBe(false);
    expect(gateAsksApproval(["main"], [gate("main", ["unit"], "one")])).toBe(true);
  });
});

describe("a train's checks", () => {
  // Mutation: list the gate's checks in the rows' order.
  test("each gate check with its state, in the gate's order", () => {
    const rows = [check("e2e", "running"), check("unit", "passed")];
    expect(checksWords(["e2e", "unit"], rows)).toBe("e2e running · unit passed");
  });

  test("all unstarted is one clause", () => {
    expect(checksWords(["e2e", "unit"], [])).toBe("e2e and unit not started");
    expect(checksWords(["e2e", "lint", "unit"], [])).toBe("e2e, lint, and unit not started");
  });

  test("a check with no row is not started, and a failed one says so", () => {
    expect(checksWords(["e2e", "unit"], [check("unit", "failed")])).toBe(
      "e2e not started · unit failed",
    );
  });

  // Mutation: drop a required row the gate does not name.
  test("a required row the gate does not name follows the gate's, and an optional one is left out", () => {
    const rows = [
      check("unit", "passed"),
      check("perf", "running"),
      check("lint", "failed", { required: false }),
    ];
    expect(checksWords(["unit"], rows)).toBe("unit passed · perf running");
    expect(trainMeter(["unit"], rows).total).toBe(2);
  });

  // Mutation: total the required rows alone.
  test("the meter counts against the gate's checks", () => {
    const meter = trainMeter(["e2e", "unit"], [check("unit", "passed")]);
    expect(meter.total).toBe(2);
    expect(meter.parts.map((p) => p.value)).toEqual([1, 0, 0]);
  });

  test("an unread gate counts the required rows", () => {
    expect(trainMeter(null, [check("unit", "passed")]).total).toBe(1);
    expect(checksWords(null, [check("unit", "passed")])).toBe("unit passed");
  });

  // Mutation: say "No required checks" where the gate was not read.
  test("an unread gate with no rows says its checks have not started", () => {
    expect(checksWords(null, [])).toBe("Its checks have not started");
    expect(checksWords([], [])).toBe("No required checks");
  });

  test("a rerun stands at its latest attempt", () => {
    const rows = [check("unit", "failed"), check("unit", "running", { attempt: 2 })];
    expect(checksWords(["unit"], rows)).toBe("unit running");
  });
});

describe("elapsedWords", () => {
  const timed = (minutes: number, timeout?: number) =>
    check("e2e", "running", { started_at: ago(minutes), timeout_seconds: timeout });

  // Mutation: measure from the latest start instead of the earliest.
  test("minutes against the slowest timeout, from the earliest start", () => {
    const rows = [
      timed(4, 1800),
      check("unit", "passed", { started_at: ago(2), timeout_seconds: 600 }),
    ];
    expect(elapsedWords(null, rows, NOW)).toEqual({
      words: "4 of 30 minutes",
      bar: { value: 4, max: 30 },
    });
  });

  test("under a minute, and past an hour", () => {
    expect(elapsedWords(null, [timed(0.5, 1800)], NOW).words).toBe("under a minute of 30 minutes");
    expect(elapsedWords(null, [timed(65, 7200)], NOW).words).toBe("1 hour 5 minutes of 2 hours");
  });

  // Mutation: count to now once every check has finished.
  test("a train whose checks all finished ran until the last finish, and stays", () => {
    const rows = [
      check("e2e", "passed", { started_at: ago(30), finished_at: ago(20), timeout_seconds: 1800 }),
      check("unit", "passed", { started_at: ago(28), finished_at: ago(25) }),
    ];
    expect(trainEnd(["e2e", "unit"], rows)).toBe(NOW - 20 * 60_000);
    expect(elapsedWords(["e2e", "unit"], rows, NOW)).toEqual({
      words: "ran 10 of 30 minutes",
      bar: { value: 10, max: 30 },
    });
    expect(elapsedWords(["e2e", "unit"], rows, NOW + 60 * 60_000).words).toBe(
      "ran 10 of 30 minutes",
    );
  });

  // Mutation: end the train before a check the gate names has a row, or while one runs.
  test("a train still running, or with a gate check not yet queued, has not ended", () => {
    const done = check("unit", "passed", { started_at: ago(5), finished_at: ago(4) });
    expect(trainEnd(["e2e", "unit"], [done])).toBeNull();
    expect(trainEnd(null, [done, timed(5, 1800)])).toBeNull();
    expect(trainEnd(null, [done])).toBe(NOW - 4 * 60_000);
  });

  // Mutation: say "35 of 30 minutes" past the timeout.
  test("past the timeout it says over, and the bar stays full", () => {
    expect(elapsedWords(null, [timed(35, 1800)], NOW)).toEqual({
      words: "over 30 minutes",
      bar: { value: 30, max: 30 },
    });
  });

  // Mutation: say the timeout as a fraction of minutes.
  test("a timeout of part of a minute is whole minutes rounded up, or seconds under a minute", () => {
    expect(elapsedWords(null, [timed(1, 90)], NOW).words).toBe("1 of 2 minutes");
    expect(elapsedWords(null, [timed(0.25, 45)], NOW).words).toBe("under a minute of 45 seconds");
  });

  test("no timeout declared is words alone; no start is not started", () => {
    expect(elapsedWords(null, [timed(4)], NOW)).toEqual({ words: "running 4 minutes" });
    expect(elapsedWords(null, [check("e2e", "queued")], NOW)).toEqual({ words: "not started yet" });
  });
});

describe("ejectionWords", () => {
  // Mutation: name the failed commit's id as the base.
  test("a failed check on a train names its base", () => {
    const e = ejected({
      checks: ["e2e"],
      tested_in: {
        candidate_id: "k",
        base_oid: "5c3e8b1".padEnd(40, "0"),
        commit_oid: "d".repeat(40),
      },
    });
    expect(ejectionWords(e)).toEqual({
      why: "e2e failed on the train's merge onto main at",
      onto: "5c3e8b1".padEnd(40, "0"),
    });
  });

  test("without its train, and past three checks", () => {
    expect(ejectionWords(ejected({ checks: ["e2e", "lint"] })).why).toBe(
      "e2e and lint failed on the train it was tested in",
    );
    expect(ejectionWords(ejected({ checks: ["e2e", "lint", "unit", "a", "b"] })).why).toBe(
      "e2e, lint, unit and 2 more failed on the train it was tested in",
    );
  });

  test("a conflict names its paths; another reason is its word", () => {
    expect(ejectionWords(ejected({ reason: "conflict", paths: ["a.go"] })).why).toBe(
      "its merge conflicted on a.go",
    );
    expect(
      ejectionWords(ejected({ reason: "conflict", paths: ["a.go", "b.go", "c.go"] })).why,
    ).toBe("its merge conflicted on a.go and 2 more paths");
    expect(ejectionWords(ejected({ reason: "head_moved" })).why).toBe("its head moved");
  });
});

describe("summaryWords", () => {
  const said = (h: { figure?: string; words: string }[]) =>
    h.map((p) => `${p.figure ?? ""}${p.words}`);

  test("both halves, named or not", () => {
    expect(said(summaryWords({ live: 2 }, { total: 1 }, ""))).toEqual([
      "2 changes queued",
      "1 taken out in the last day",
    ]);
    expect(said(summaryWords({ live: 1 }, null, "web"))).toEqual(["1 change queued in web"]);
  });

  test("a bound reached says at least", () => {
    expect(said(summaryWords({ live: 4096, more: true }, { total: 50, more: true }, ""))).toEqual([
      "At least 4,096 changes queued",
      "at least 50 taken out in the last day",
    ]);
  });

  // Mutation: draw 0 for a half not answered, or for none taken out.
  test("a true zero is said in words, none taken out is not drawn, and a missing half is not drawn", () => {
    expect(said(summaryWords({ live: 0 }, { total: 0 }, ""))).toEqual(["Nothing queued"]);
    expect(said(summaryWords(null, { total: 1 }, ""))).toEqual(["1 taken out in the last day"]);
    expect(summaryWords(null, null, "")).toEqual([]);
  });
});

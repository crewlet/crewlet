/** A run's key in the address and the words a standing row and its pane say, held as pure functions. */

import { describe, expect, test } from "vitest";

import type { CodeCheckRow } from "~/protocol/index.ts";
import {
  checkedWords,
  failingLines,
  parseRunKey,
  runKey,
  standingWords,
  tailLines,
  tailRangeWords,
  testsWords,
} from "./standing.ts";

const OID = "9b2e4d7".padEnd(40, "0");

function row(extra: Partial<CodeCheckRow> = {}): CodeCheckRow {
  return {
    repo_id: "id-api",
    repo: "api",
    oid: OID,
    target: "refs/heads/main",
    name: "e2e",
    attempt: 1,
    state: "failed",
    ...extra,
  };
}

/** The board's eighteen lines, numbered 1184 to 1201. */
const BOARD_TAIL = [
  "=== RUN   TestKeysetPagesInOrder",
  "    orders_test.go:71: seeded 120 orders",
  "    orders_test.go:88: got 49 rows, want 50",
  "    orders_test.go:91: ord_8f21 seen twice",
  "--- FAIL: TestKeysetPagesInOrder (0.42s)",
  "=== RUN   TestKeysetEmptyPage",
  "--- PASS: TestKeysetEmptyPage (0.03s)",
  "=== RUN   TestKeysetForeignCursor",
  "--- PASS: TestKeysetForeignCursor (0.01s)",
  "FAIL",
  "FAIL    nimbus/api/internal/http      1.874s",
  "ok      nimbus/api/internal/orders    0.912s",
  "ok      nimbus/api/internal/store     2.301s",
  "ok      nimbus/api/internal/billing   0.655s",
  "tearing down the postgres container (12s)",
  "FAIL",
  "make: *** [Makefile:41: e2e] Error 1",
  "exit status 1",
];

describe("a run's key", () => {
  // Mutation: split the key on every colon, or let a short id or a bare branch through.
  test("reads back what it wrote, a branch with slashes whole, and refuses anything else", () => {
    const at = row({ target: "refs/heads/release/2026-11" });
    expect(parseRunKey(runKey(at))).toEqual({
      repo: "api",
      name: "e2e",
      attempt: 1,
      oid: OID,
      target: "refs/heads/release/2026-11",
    });
    for (const bad of [
      "",
      "api:e2e:1",
      `api:e2e:0:${OID}:refs/heads/main`,
      `api:e2e:1:9b2e4d7:refs/heads/main`,
      `api:e2e:1:${OID}:main`,
      `api:E2E:1:${OID}:refs/heads/main`,
      `:e2e:1:${OID}:refs/heads/main`,
    ]) {
      expect(parseRunKey(bad), bad).toBeNull();
    }
  });
});

describe("what a standing row judged", () => {
  // Mutation: draw a candidate as a head, or a tip with no target.
  test("names its change, says the queue for a candidate, and the branch for a tip", () => {
    const change = { repo: "api", ref: "api!17", title: "page by keyset", state: "open" };
    expect(standingWords(row({ purpose: "head", change }))).toEqual({
      before: "for ",
      strong: "api!17",
    });
    expect(standingWords(row({ purpose: "candidate", change }))).toEqual({
      before: "for ",
      strong: "api!17",
      after: " in the queue",
    });
    expect(standingWords(row({ purpose: "tip" }))).toEqual({ before: "main's tip" });
    expect(standingWords(row({ purpose: "bisect" }))).toEqual({
      before: "a landing on main, bisected",
    });
    expect(standingWords(row({ purpose: "head" }))).toEqual({ before: "a change's head" });
  });

  // Mutation: drop the base, or say "in the queue" of a head.
  test("the pane's Checked says onto what and at which base", () => {
    expect(checkedWords(row({ purpose: "candidate", base: "5c3e8b1".padEnd(40, "0") }))).toEqual({
      before: "merged onto ",
      after: " at 5c3e8b1 in the queue",
    });
    expect(checkedWords(row({ purpose: "candidate" }))).toEqual({
      before: "merged onto ",
      after: " in the queue",
    });
    expect(checkedWords(row({ purpose: "head" }))?.before).toBe("its head, for ");
    expect(checkedWords(row({ purpose: "tip" }))?.after).toBe("'s tip");
    expect(checkedWords(row({ purpose: undefined }))).toBeNull();
  });
});

describe("a run's tests", () => {
  // Mutation: draw an absent count as 0, or drop the exit code.
  test("counts the failed of the total, a pass, and the exit code alone where nothing was counted", () => {
    expect(testsWords({ tests: { total: 214, failed: 1 }, exit_code: 1 })).toEqual({
      words: "1 of 214 tests failed · exit code 1",
      failed: true,
    });
    expect(testsWords({ tests: { total: 1_214, failed: 2, skipped: 3 } })?.words).toBe(
      "2 of 1,214 tests failed, 3 skipped",
    );
    expect(testsWords({ tests: { total: 214 }, exit_code: 0 })).toEqual({
      words: "214 tests passed · exit code 0",
      failed: false,
    });
    expect(testsWords({ tests: { total: 214, skipped: 3 } })?.words).toBe(
      "211 of 214 tests passed, 3 skipped",
    );
    expect(testsWords({ exit_code: 1 })).toEqual({ words: "exit code 1", failed: null });
    expect(testsWords({})).toBeNull();
  });
});

describe("the end of its output", () => {
  // Mutation: number from 1, or count a trailing newline as a line.
  test("is numbered from its place in the whole output", () => {
    const lines = tailLines(`${BOARD_TAIL.join("\n")}\n`);
    expect(lines).toHaveLength(18);
    expect(tailRangeWords({ log_lines: 1201, tail_first_line: 1184 }, lines.length)).toBe(
      "lines 1,184 to 1,201 of 1,201",
    );
    expect(tailRangeWords({}, lines.length)).toBeNull();
  });

  // Mutation: match FAIL anywhere in a line, or a place without its line.
  test("marks a line that starts with a failure mark or prints a failure's place, and no other", () => {
    const failures = [{ path: "internal/http/orders_test.go", line: 88, message: "got 49 rows" }];
    const marked = [...failingLines(BOARD_TAIL, failures)].map((i) => 1184 + i);
    expect(marked).toEqual([1186, 1188, 1193, 1194, 1199]);
    expect([
      ...failingLines(
        ["FAILURES are listed", "  panic: nil map", "x orders_test.go:881", "FAILED (2)"],
        failures,
      ),
    ]).toEqual([1, 3]);
  });
});

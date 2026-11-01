/**
 * The checks and queues model every code screen shares.
 *
 * The invariants: an attempt a check was rerun past is history, never its
 * state; a meter of a commit's checks counts only the ones its branch
 * REQUIRES, by state, the rest its remainder; an entry's elapsed time is read
 * against the slowest required check's timeout; a queue is drawn a band per
 * repository and branch, in queue order, each entry beside the train
 * testing it and each band's ejections beside its live entries.
 */

import { expect, test } from "vitest";

import type { CodeCandidateRow, CodeCheckRow, CodeQueueRow } from "~/protocol/index.ts";
import {
  checkStateTone,
  checkStateWord,
  ejectReasonWord,
  latestAttempts,
  longestTimeout,
  queueBands,
  requiredMeter,
} from "./checks.ts";

const A = "a".repeat(40);
const B = "b".repeat(40);
const POSITION = { stream: "CREWLET_CODE_LOG", generation: 1, seq: 9 };

function attempt(
  name: string,
  n: number,
  state: string,
  extra: Partial<CodeCheckRow> = {},
): CodeCheckRow {
  return {
    repo_id: "id-web",
    repo: "web",
    oid: A,
    target: "refs/heads/main",
    name,
    attempt: n,
    state,
    ...extra,
  };
}

test("a check's latest attempt is its state: a rerun leaves the earlier one history", () => {
  const rows = [
    attempt("unit", 1, "errored"),
    attempt("lint", 1, "passed"),
    attempt("unit", 2, "passed"),
  ];
  expect(latestAttempts(rows).map((r) => `${r.name}#${r.attempt}`)).toEqual(["unit#2", "lint#1"]);
});

test("one name checked for two branches is two checks", () => {
  const rows = [
    attempt("unit", 1, "passed"),
    attempt("unit", 1, "failed", { target: "refs/heads/release" }),
  ];
  expect(latestAttempts(rows)).toHaveLength(2);
});

test("a meter counts the required checks alone, by state, the rest its remainder", () => {
  const meter = requiredMeter([
    attempt("unit", 1, "failed", { required: true }),
    attempt("unit", 2, "passed", { required: true }),
    attempt("vet", 1, "failed", { required: true }),
    attempt("e2e", 1, "running", { required: true }),
    attempt("bench", 1, "queued", { required: true }),
    attempt("lint", 1, "failed"),
  ]);
  expect(meter.total).toBe(4);
  expect(Object.fromEntries(meter.parts.map((p) => [p.id, p.value]))).toEqual({
    passed: 1,
    failed: 1,
    running: 1,
  });
});

test("an entry's time is read against its slowest required check's timeout", () => {
  expect(
    longestTimeout([
      attempt("unit", 1, "running", { required: true, timeout_seconds: 600 }),
      attempt("e2e", 1, "queued", { required: true, timeout_seconds: 1800 }),
      attempt("soak", 1, "queued", { timeout_seconds: 7200 }),
    ]),
  ).toBe(1800);
  expect(longestTimeout([attempt("unit", 1, "queued")])).toBe(0);
});

function entry(
  repo: string,
  target: string,
  place: number,
  extra: Partial<CodeQueueRow> = {},
): CodeQueueRow {
  return {
    repo_id: `id-${repo}`,
    repo,
    entry_id: `${repo}-${target}-${place}`,
    target_ref: target,
    change_id: `c-${place}`,
    number: place,
    place,
    seq: POSITION,
    head_oid: A,
    method: "merge",
    enqueued_by: { handle: "cto", kind: "agent" },
    state: "waiting",
    enqueued_at: "2026-11-01T10:00:00Z",
    ...extra,
  };
}

test("a queue is a band per repository and branch, in queue order, each entry beside its train", () => {
  const train: CodeCandidateRow = {
    repo_id: "id-web",
    candidate_id: "cand-1",
    target_ref: "refs/heads/main",
    train: "t1",
    ordinal: 1,
    base_oid: B,
    tip_oid: A,
    entries: [{ entry_id: "web-refs/heads/main-1", commit: A }],
    state: "testing",
    position: POSITION,
  };
  const bands = queueBands(
    [
      entry("web", "refs/heads/main", 2),
      entry("web", "refs/heads/main", 1, { state: "testing" }),
      entry("api", "refs/heads/main", 1),
    ],
    [train],
    [
      {
        repo_id: "id-web",
        repo: "web",
        entry_id: "gone",
        target_ref: "refs/heads/release",
        change_id: "c-9",
        number: 9,
        head_oid: A,
        enqueued_by: { handle: "cto", kind: "agent" },
        reason: "check_failed",
        checks: ["unit"],
        enqueued_at: "2026-11-01T09:00:00Z",
        ejected_at: "2026-11-01T09:30:00Z",
      },
    ],
  );
  expect(bands.map((b) => `${b.repo} ${b.target}`)).toEqual([
    "web refs/heads/main",
    "api refs/heads/main",
    "web refs/heads/release",
  ]);
  expect(bands[0]!.places.map((p) => p.row.place)).toEqual([1, 2]);
  expect(bands[0]!.places[0]!.candidate?.base_oid).toBe(B);
  expect(bands[0]!.places[1]!.candidate).toBeNull();
  expect(bands[2]!.places).toEqual([]);
  expect(bands[2]!.ejected.map((e) => e.number)).toEqual([9]);
});

test("states and reasons are the engine's words, and an unknown one is drawn as sent", () => {
  expect(checkStateWord("passed")).toBe("passed");
  expect(checkStateWord("frozen")).toBe("frozen");
  expect(checkStateTone("passed")).toBe("success");
  expect(checkStateTone("failed")).toBe("danger");
  expect(checkStateTone("errored")).toBe("neutral");
  expect(ejectReasonWord("check_failed")).toBe("a required check failed");
  expect(ejectReasonWord("tide_turned")).toBe("tide_turned");
});

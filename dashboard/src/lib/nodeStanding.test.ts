/**
 * Where a node stands on a revision this tab's write activated, decided by
 * the attempt that names the write before the applied epoch, and judged alike
 * whether the node said it on its own health push or on its fleet row: every
 * case below is one the applied epoch alone, or a fleet rule of its own, got
 * wrong or could not word.
 */

import { describe, expect, test } from "vitest";
import type { ConfigAttempt } from "~/contract/health.ts";
import type { FleetNode } from "~/protocol/types.ts";
import { healthFrame } from "~/test/health.ts";
import { fleetNodeStanding, nodeStanding, type Standing } from "./nodeStanding.ts";

const write = { revisionId: "r-saved", epoch: 4 };

/** What a node said, as both wires carry it. */
interface Said {
  /** The epoch it last applied; 0 before its first apply. */
  readonly applied: number;
  readonly attempt?: ConfigAttempt;
}

/** The node's health push saying `said`. */
const push = (said: Said) =>
  healthFrame({
    applied_epoch: said.applied,
    ...(said.attempt ? { config_attempt: said.attempt } : {}),
  });

/**
 * The node's fleet row saying `said`, as the fleet view renders one: the
 * attempt's three fields, 0 and empty for none, and the applied epoch only
 * once there is one.
 */
const row = (said: Said, over: Partial<FleetNode> = {}): FleetNode => ({
  id: "a",
  roles: ["seats"],
  broker: "member",
  seats: 1,
  config_epoch: said.attempt?.epoch ?? 0,
  config_revision_id: said.attempt?.revision_id ?? "",
  config_status: said.attempt?.status ?? "",
  ...(said.applied > 0 ? { config_applied_epoch: said.applied } : {}),
  ...over,
});

const attempt = (epoch: number, revision_id: string, status: string): ConfigAttempt => ({
  epoch,
  revision_id,
  status,
});

// ONE RULE OVER EITHER WIRE. The fleet path once judged by a rule of its own
// that could not see the applied epoch, so a node that applied a later epoch
// and refused a still later one was applied to a lead and passed to an
// Admin. Mutations: leave `config_applied_epoch` out of the row's report and
// the three applied-epoch cases read applying or passed on the row alone;
// compare the applied epoch before the attempt and the refusal beside 41
// reads applied; match the attempt on its epoch alone and the other
// revision's attempt reads refused; ignore the attempt's status and every
// refusal reads applied; drop the later-attempt branch and "passed" reads
// applying.
describe("a node's standing, alike on its push and on its fleet row", () => {
  const cases: readonly [string, Said, Standing][] = [
    ["applied the write's epoch", { applied: 4 }, { kind: "applied" }],
    ["applied a later epoch, which went ok", { applied: 6 }, { kind: "applied" }],
    ["behind it, with nothing attempted since", { applied: 3 }, { kind: "applying" }],
    ["reported nothing at all", { applied: 0 }, { kind: "applying" }],
    [
      "attempted the write and it went ok",
      { applied: 4, attempt: attempt(4, "r-saved", "ok") },
      { kind: "applied" },
    ],
    // A FAILED ATTEMPT LEAVES THE APPLIED EPOCH WHERE IT WAS, so read alone it
    // said "applying" of a refusal for as long as the tab stayed open.
    [
      "refused the write",
      { applied: 3, attempt: attempt(4, "r-saved", "error") },
      { kind: "refused" },
    ],
    [
      "refused the write after rebuilding part of the company",
      { applied: 3, attempt: attempt(4, "r-saved", "degraded") },
      { kind: "refused" },
    ],
    // A RUNNING NODE KEEPS THE EPOCH IT APPLIED UNDER THE PREVIOUS POINTER, so
    // after a rebuilt coordination store restarted the count an applied epoch
    // of 41 stands beside a refusal of this write at epoch 4.
    [
      "refused the write beside an applied epoch the previous pointer left above it",
      { applied: 41, attempt: attempt(4, "r-saved", "error") },
      { kind: "refused" },
    ],
    [
      "applied the write beside an applied epoch the previous pointer left above it",
      { applied: 41, attempt: attempt(4, "r-saved", "ok") },
      { kind: "applied" },
    ],
    // ONE EPOCH NUMBER, ANOTHER REVISION: a rebuilt coordination store
    // restarted the count, and that attempt is not about this write.
    [
      "attempted another revision at the write's epoch",
      { applied: 3, attempt: attempt(4, "r-other", "error") },
      { kind: "applying" },
    ],
    // AN ATTEMPT AT A LATER EPOCH, this one never applied: the node will not
    // come back to it.
    [
      "attempted a later epoch and never applied this one",
      { applied: 3, attempt: attempt(5, "r-later", "error") },
      { kind: "passed", by: "r-later" },
    ],
    // SKIPPED THE WRITE, APPLIED THE NEXT, REFUSED THE ONE AFTER: it runs
    // past the write, and its last attempt being a refusal of something else
    // takes nothing back.
    [
      "applied a later epoch and then refused a still later one",
      { applied: 5, attempt: attempt(6, "r-third", "error") },
      { kind: "applied" },
    ],
  ];

  test.each(cases)("%s", (_, said, standing) => {
    expect(nodeStanding(push(said), write)).toEqual(standing);
    expect(fleetNodeStanding(row(said), write)).toEqual(standing);
  });
});

describe("what a fleet row says that a push does not", () => {
  // THE REVISION A NODE RUNS is on its row alone, and a node running the
  // written revision has applied it — a node that restarted on it has an
  // applied epoch of nothing again. Mutation: drop the serving revision from
  // the rule and this reads passed.
  test("a node running the written revision has applied it", () => {
    const restarted = row(
      { applied: 0, attempt: attempt(5, "r-later", "error") },
      { config_serving_revision_id: "r-saved" },
    );
    expect(fleetNodeStanding(restarted, write)).toEqual({ kind: "applied" });
  });

  // A REFUSED RE-ACTIVATION OF THE REVISION A NODE RUNS names it in both
  // fields, and is a refusal all the same. Mutation: read the serving
  // revision before the attempt and this reads applied.
  test("a node that refused the write is refused although it runs that revision", () => {
    const rotation = row(
      { applied: 3, attempt: attempt(4, "r-saved", "error") },
      { config_serving_revision_id: "r-saved" },
    );
    expect(fleetNodeStanding(rotation, write)).toEqual({ kind: "refused" });
  });
});

/**
 * Whether a node runs the fleet's revision, decided by more than its epoch.
 *
 * Every case below is one the epoch-only rule — `config_epoch <
 * target_epoch` — got wrong or could not word: a node that refused the target
 * reports the target's epoch, a node on another revision can report it too
 * once a rebuilt coordination store restarted the count, and an answer with no
 * activation has no target to be behind.
 */

import { describe, expect, test } from "vitest";
import type { FleetAnswer, FleetNode } from "~/protocol/types.ts";
import { configStanding, offRevision } from "./configStanding.ts";

const ACTIVE = "01JACTIVE00000000000000000A";
const PREVIOUS = "01JPREVIOUS000000000000000P";

/** A fleet whose pointer names ACTIVE at epoch 4. */
const answer: Pick<FleetAnswer, "activation"> = {
  activation: { epoch: 4, revision_id: ACTIVE, at: "2026-10-26T08:00:00Z", summary: "" },
};

function node(over: Partial<FleetNode> = {}): FleetNode {
  return {
    id: "n1",
    roles: [],
    broker: "member",
    seats: 0,
    config_epoch: 4,
    config_status: "ok",
    config_revision_id: ACTIVE,
    config_serving_revision_id: ACTIVE,
    ...over,
  };
}

describe("a node's standing on the fleet's revision", () => {
  test("current is the target epoch, on the target's revision, applied and running", () => {
    expect(configStanding(answer, node())).toEqual({ kind: "current", serving: ACTIVE });
    expect(offRevision(answer, node())).toBe(false);
  });

  // THE COMMONEST PER-NODE FAILURE: an env var or an MCP binary one node
  // lacks. It reports the target's epoch beside `error`, and runs the one
  // before — which the epoch-only rule called current.
  test("a node that refused the target is not current, and keeps what it runs", () => {
    const refused = node({ config_status: "error", config_serving_revision_id: PREVIOUS });
    expect(configStanding(answer, refused)).toEqual({ kind: "refused", serving: PREVIOUS });
    expect(offRevision(answer, refused)).toBe(true);
  });

  test("a refusal on a node that never served anything runs none", () => {
    const refused = node({ config_status: "error", config_serving_revision_id: undefined });
    expect(configStanding(answer, refused)).toEqual({ kind: "refused", serving: undefined });
  });

  // DEGRADED NEVER COUNTS AS CONVERGED (`configplane.ApplyStatus.Converged`):
  // the apply failed after a restart-required subsystem had been mutated.
  test("a partial apply of the target is not current either", () => {
    const partial = node({ config_status: "degraded", config_serving_revision_id: PREVIOUS });
    expect(configStanding(answer, partial).kind).toBe("degraded");
    expect(offRevision(answer, partial)).toBe(true);
  });

  // A REBUILT COORDINATION STORE RESTARTS THE EPOCH, so the target's number can
  // name another revision than the one this node attempted under it.
  test("the target epoch on another revision is not current", () => {
    const other = node({ config_revision_id: PREVIOUS, config_serving_revision_id: PREVIOUS });
    expect(configStanding(answer, other)).toEqual({
      kind: "elsewhere",
      attempted: PREVIOUS,
      serving: PREVIOUS,
      servesActive: false,
    });
    expect(offRevision(answer, other)).toBe(true);
  });

  test("an ok report at the target epoch that runs another revision is not current", () => {
    const stale = node({ config_serving_revision_id: PREVIOUS });
    expect(configStanding(answer, stale).kind).toBe("elsewhere");
  });

  test("an epoch the pointer never reached is not current, whatever it runs", () => {
    const ahead = node({ config_epoch: 41 });
    expect(configStanding(answer, ahead)).toMatchObject({ kind: "elsewhere", servesActive: true });
  });

  test("an older epoch is behind by the difference", () => {
    const behind = node({ config_epoch: 1, config_revision_id: PREVIOUS });
    expect(configStanding(answer, behind)).toMatchObject({ kind: "behind", epochs: 3 });
  });

  // SILENT IS NOT BEHIND: no record at all is a node that has not spoken. It
  // is counted off the revision — nothing says it runs it — and worded apart.
  test("a node that reported nothing is unreported, and counted off the revision", () => {
    const silent: FleetNode = { id: "n2", roles: [], broker: "member", seats: 0 };
    expect(configStanding(answer, silent).kind).toBe("unreported");
    expect(offRevision(answer, silent)).toBe(true);
  });

  test("an answer with no activation has no target, and counts no node behind", () => {
    for (const none of [{ activation: null }, {}]) {
      for (const n of [node(), node({ config_epoch: 1 }), node({ config_status: "error" })]) {
        expect(configStanding(none, n)).toEqual({ kind: "no-target" });
        expect(offRevision(none, n)).toBe(false);
      }
    }
  });
});

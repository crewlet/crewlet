/**
 * The figures beside Settings' sections say what the engine answered, and
 * nothing when it did not.
 *
 * Each case names the fact a figure stands for: a node behind the activated
 * epoch is said in the Nodes row's NAME as well as drawn, and a standing
 * alarm is the only reason Backups & retention draws a figure at all.
 */

import { describe, expect, test } from "vitest";

import { settingsFigures } from "./settingsFigures.tsx";
import type { FleetAnswer, FleetNode, RetentionReport } from "~/protocol/index.ts";
import { healthFrame } from "~/test/health.ts";

const NONE = { health: null, fleet: null, retention: null };

function node(over: Partial<FleetNode>): FleetNode {
  return { id: "n", roles: [], broker: "member", seats: 0, ...over };
}

/** The revision a fleet's pointer names at epoch `target`. */
const ACTIVE = "01JACTIVE00000000000000000A";

/** A fleet whose pointer names ACTIVE at `target`, or nothing at 0. */
function fleet(target: number, nodes: FleetNode[]): FleetAnswer {
  const activation =
    target > 0 ? { epoch: target, revision_id: ACTIVE, at: "", summary: "" } : null;
  return {
    nodes,
    seats: [],
    duties: [],
    target_epoch: target,
    activation,
  } as unknown as FleetAnswer;
}

/** A node that runs the fleet's revision at `epoch`. */
const runs = (id: string, epoch: number): FleetNode =>
  node({
    id,
    config_epoch: epoch,
    config_status: "ok",
    config_revision_id: ACTIVE,
    config_serving_revision_id: ACTIVE,
  });

function retention(kinds: string[], held = 0): RetentionReport {
  return {
    domains: Array.from({ length: held }, (_, i) => ({
      domain: `d${i}`,
      blocked_by: "backup_floor",
    })),
    alarms: kinds.map((kind) => ({ kind, detail: "", remedy: "" })),
  } as unknown as RetentionReport;
}

describe("settingsFigures", () => {
  test("nothing answered draws nothing — absent is never a zero", () => {
    expect(settingsFigures(NONE)).toEqual({});
  });

  test("Nodes is the health push's count, and names the nodes behind the activated epoch", () => {
    const health = healthFrame({ nodes: 3 });
    expect(settingsFigures({ ...NONE, health }).nodes?.count).toMatchObject({
      value: 3,
      label: "nodes live",
      mark: undefined,
    });
    const f = settingsFigures({
      ...NONE,
      health,
      fleet: fleet(4, [
        runs("a", 4),
        node({ id: "b", config_epoch: 3 }),
        // A node that has not reported an apply at all runs nothing anybody
        // can name — counted, since nothing says it runs the fleet's revision.
        node({ id: "c" }),
      ]),
    });
    expect(f.nodes?.count?.value).toBe(3);
    expect(f.nodes?.count?.label).toBe("nodes live, 2 behind on config");
    expect(f.nodes?.count?.mark).toBeDefined();
  });

  // THE EPOCH IS THE ONE A NODE ATTEMPTED. A refusal of the target reports the
  // target's epoch while the node runs the one before, and so does a node on
  // another revision once the pointer's count restarted: the epoch-only rule
  // counted neither, which is what the comment above the figure said it did.
  test("a refusal and another revision at the target epoch are behind on config", () => {
    const health = healthFrame({ nodes: 3 });
    const f = settingsFigures({
      ...NONE,
      health,
      fleet: fleet(4, [
        runs("a", 4),
        node({
          id: "refused",
          config_epoch: 4,
          config_status: "error",
          config_revision_id: ACTIVE,
          config_serving_revision_id: "01JPREVIOUS000000000000000P",
        }),
        node({
          id: "other",
          config_epoch: 4,
          config_status: "ok",
          config_revision_id: "01JOTHER0000000000000000OO",
          config_serving_revision_id: "01JOTHER0000000000000000OO",
        }),
      ]),
    });
    expect(f.nodes?.count?.label).toBe("nodes live, 2 behind on config");
  });

  test("with no activation no node is behind one", () => {
    const health = healthFrame({ nodes: 2 });
    const f = settingsFigures({
      ...NONE,
      health,
      fleet: fleet(0, [node({ id: "a", config_epoch: 3 }), node({ id: "b" })]),
    });
    expect(f.nodes?.count?.label).toBe("nodes live");
    expect(f.nodes?.count?.mark).toBeUndefined();
  });

  // THE FIGURE IS READ INTO THE ROW'S NAME, so one node is "Nodes, 1 node
  // live" — "1 nodes live" was the row every single-node install heard.
  test("one node reads as one node", () => {
    const health = healthFrame({ nodes: 1 });
    expect(settingsFigures({ ...NONE, health }).nodes?.count?.label).toBe("node live");
    const behind = settingsFigures({
      ...NONE,
      health,
      fleet: fleet(2, [node({ id: "a", config_epoch: 1 })]),
    });
    expect(behind.nodes?.count?.label).toBe("node live, 1 behind on config");
  });

  test("Configuration names the epoch this node applied in words", () => {
    const f = settingsFigures({ ...NONE, health: healthFrame({ applied_epoch: 2 }) });
    expect(f.config?.count?.value).toBe("epoch 2");
    expect(f.nodes).toBeUndefined();
  });

  // THE STANDING ALARMS, the rail's count: four logs held by one missing
  // backup are one alarm, and a figure counting the logs said 4 beside the
  // rail's 5 for the one problem.
  test("Backups & retention counts the standing alarms, and draws nothing without one", () => {
    expect(settingsFigures({ ...NONE, retention: retention([]) }).backups).toBe(undefined);
    const one = settingsFigures({ ...NONE, retention: retention(["backup_age"], 4) });
    expect(one.backups?.count).toMatchObject({ value: 1, label: "standing alarm" });
    const two = settingsFigures({ ...NONE, retention: retention(["backup_age", "volume_low"]) });
    expect(two.backups?.count?.label).toBe("standing alarms");
  });
});

/**
 * The figures beside Settings' sections say what the engine answered, and
 * nothing when it did not.
 *
 * Each case names the fact a figure stands for: the Integrations badge is the
 * engine's own roll-up (never a re-derived phase), a node behind the activated
 * epoch is said in the Nodes row's NAME as well as drawn, and a held trim is
 * the only reason Backups & retention draws a figure at all.
 */

import { describe, expect, test } from "vitest";

import { settingsFigures } from "./settingsFigures.tsx";
import type { IntegrationsAnswer } from "~/contract/integrations.ts";
import type { FleetAnswer, FleetNode, RetentionReport } from "~/protocol/index.ts";

const NONE = { health: null, fleet: null, retention: null, integrations: null };

function tools(...states: ("attention" | "connected" | "not_connected" | "not_in_use")[]) {
  return {
    integrations: [],
    traffic_known: true,
    traffic_since: null,
    tools: states.map((state, i) => ({
      key: `t${i}`,
      surfaces: [],
      state,
      label: state,
      reason: "",
    })),
  } as IntegrationsAnswer;
}

function node(over: Partial<FleetNode>): FleetNode {
  return { id: "n", roles: [], seats: 0, ...over };
}

function fleet(target: number, nodes: FleetNode[]): FleetAnswer {
  return { nodes, seats: [], duties: [], target_epoch: target } as unknown as FleetAnswer;
}

function retention(blocked: (string | undefined)[]): RetentionReport {
  return {
    domains: blocked.map((b, i) => ({ domain: `d${i}`, blocked_by: b })),
  } as unknown as RetentionReport;
}

describe("settingsFigures", () => {
  test("nothing answered draws nothing — absent is never a zero", () => {
    expect(settingsFigures(NONE)).toEqual({});
  });

  test("the Integrations pill counts the tools the engine's roll-up says need a person", () => {
    const f = settingsFigures({
      ...NONE,
      integrations: tools("attention", "not_connected", "attention", "connected"),
    });
    // `not_connected` is the engine mid-flight — amber on the card, but nothing
    // a person can do — so it is not waiting on the reader.
    expect(f.integrations?.attention).toEqual({ value: 2, label: "need attention" });
    // A STATE, NOT AN UNREAD COUNT: never the accent's badge, never a quiet count.
    expect(f.integrations?.badge).toBeUndefined();
    expect(f.integrations?.count).toBeUndefined();
    // And one tool reads as one: "Integrations, 1 needs attention".
    expect(
      settingsFigures({ ...NONE, integrations: tools("attention") }).integrations?.attention,
    ).toEqual({ value: 1, label: "needs attention" });
  });

  test("an integrations answer with nothing owed draws no pill", () => {
    expect(settingsFigures({ ...NONE, integrations: tools("connected") }).integrations).toBe(
      undefined,
    );
  });

  test("Nodes is the health push's count, and names the nodes behind the activated epoch", () => {
    const health = { status: "ok", nodes: 3 };
    expect(settingsFigures({ ...NONE, health }).nodes?.count).toMatchObject({
      value: 3,
      label: "nodes live",
      mark: undefined,
    });
    const f = settingsFigures({
      ...NONE,
      health,
      fleet: fleet(4, [
        node({ id: "a", config_epoch: 4 }),
        node({ id: "b", config_epoch: 3 }),
        // A node that has not reported an apply at all is on epoch 0 — behind.
        node({ id: "c" }),
      ]),
    });
    expect(f.nodes?.count?.value).toBe(3);
    expect(f.nodes?.count?.label).toBe("nodes live, 2 behind on config");
    expect(f.nodes?.count?.mark).toBeDefined();
  });

  // THE FIGURE IS READ INTO THE ROW'S NAME, so one node is "Nodes, 1 node
  // live" — "1 nodes live" was the row every single-node install heard.
  test("one node reads as one node", () => {
    const health = { status: "ok", nodes: 1 };
    expect(settingsFigures({ ...NONE, health }).nodes?.count?.label).toBe("node live");
    const behind = settingsFigures({
      ...NONE,
      health,
      fleet: fleet(2, [node({ id: "a", config_epoch: 1 })]),
    });
    expect(behind.nodes?.count?.label).toBe("node live, 1 behind on config");
  });

  test("Configuration names the epoch this node applied in words", () => {
    const f = settingsFigures({ ...NONE, health: { status: "ok", applied_epoch: 2 } });
    expect(f.config?.count?.value).toBe("epoch 2");
    expect(f.nodes).toBeUndefined();
  });

  test("Backups & retention draws a figure only for a held trim", () => {
    expect(settingsFigures({ ...NONE, retention: retention([undefined]) }).backups).toBe(undefined);
    const one = settingsFigures({ ...NONE, retention: retention(["hold", undefined]) });
    expect(one.backups?.count).toMatchObject({ value: 1, label: "domain whose trim is held" });
    const two = settingsFigures({ ...NONE, retention: retention(["hold", "snapshot"]) });
    expect(two.backups?.count?.label).toBe("domains whose trim is held");
  });
});

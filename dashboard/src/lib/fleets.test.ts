/**
 * The fleet rules every model picker shares: a fleet stands alone in a chain,
 * a written chain is refused in the engine's words, and a fleet option says
 * how many of its members have room.
 */

import { describe, expect, test } from "vitest";
import type { Fleet } from "~/protocol/fleets.ts";
import { fleetChainProblem, fleetOfRef, fleetRef, fleetRoom, standAlone } from "./fleets.ts";

const fleet = (over: Partial<Fleet>): Fleet => ({
  key: "claude-opus",
  name: "Claude Opus",
  is_default: false,
  class: "Claude Opus",
  harness: "claude-code",
  strategy: "balanced",
  threshold_points: 5,
  spread: null,
  members: [],
  ...over,
});

const member = (entry: string, state: Fleet["members"][number]["state"]) => ({
  entry,
  kind: "plan" as const,
  state,
  until: null,
  why: "",
  share: null,
  overflow: false,
  last_served_at: null,
  serving: [],
});

describe("a fleet stands alone in a chain", () => {
  test("picking a fleet makes it the whole chain", () => {
    expect(standAlone(["smart", "fast"], ["smart", "fast", "fleet:claude-opus"])).toEqual([
      "fleet:claude-opus",
    ]);
  });

  test("picking a model while a fleet is held replaces the fleet", () => {
    expect(standAlone(["fleet:claude-opus"], ["fleet:claude-opus", "smart"])).toEqual(["smart"]);
  });

  test("a removal or a move of models is kept as it is", () => {
    expect(standAlone(["smart", "fast"], ["fast", "smart"])).toEqual(["fast", "smart"]);
    expect(standAlone(["fleet:claude-opus"], [])).toEqual([]);
  });
});

describe("a written chain is refused in the engine's words", () => {
  test("a fleet inside a list", () => {
    expect(fleetChainProblem(["fleet:claude-opus", "smart"], null)).toBe(
      "A fleet stands alone: choose the fleet or a list of models",
    );
  });

  test("a fleet that does not exist, once the fleets are read", () => {
    expect(fleetChainProblem(["fleet:gone"], [fleet({})])).toBe("gone is not a fleet");
    expect(fleetChainProblem(["fleet:gone"], null)).toBeUndefined();
    expect(fleetChainProblem(["fleet:claude-opus"], [fleet({})])).toBeUndefined();
    expect(fleetChainProblem(["smart", "fast"], [fleet({})])).toBeUndefined();
  });
});

test("a fleet option counts the members that have room, and the default says so", () => {
  expect(
    fleetRoom(
      fleet({ members: [member("a", "available"), member("b", "near"), member("c", "limit")] }),
    ),
  ).toBe("2 of 3 members have room");
  expect(fleetRoom(fleet({ is_default: true }))).toBe("Default · No members yet");
});

test("a reference names its fleet, and a model's key names none", () => {
  expect(fleetOfRef(fleetRef("claude-opus"))).toBe("claude-opus");
  expect(fleetOfRef("smart")).toBeNull();
});

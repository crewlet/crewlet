// @vitest-environment node
/**
 * The reporting forest.
 *
 * What these protect: every seat of the derivation appears exactly once, for
 * any manages graph the engine could derive, including cycles and managers
 * that name no seat; a seat with no manager is a root; and a cycle is shown
 * as a group broken at its first member in engine order rather than dropped.
 * The random graphs come from a seeded PRNG in this file, and a failure names
 * its seed.
 */

import { describe, expect, test } from "vitest";
import type { Derived, DerivedSeat } from "~/protocol/index.ts";
import { reportingForest, walkForest, type ReportingNode } from "./reporting.ts";

/** mulberry32: a seeded PRNG, so a failing property names the run that found it. */
function prng(seed: number): () => number {
  let s = seed >>> 0;
  return () => {
    s = (s + 0x6d2b79f5) >>> 0;
    let t = s;
    t = Math.imul(t ^ (t >>> 15), t | 1);
    t ^= t + Math.imul(t ^ (t >>> 7), t | 61);
    return ((t ^ (t >>> 14)) >>> 0) / 4294967296;
  };
}

function seat(handle: string, manager = ""): DerivedSeat {
  return {
    handle,
    name: handle.toUpperCase(),
    kind: "agent",
    placed_by_ref: false,
    manager,
    managers: manager ? [manager] : null,
    reports: null,
    auto_reports: null,
    onboarding_chain: null,
  };
}

const derived = (seats: DerivedSeat[]): Derived => ({ seats, units: null });
const shape = (node: ReportingNode): unknown =>
  node.reports.length === 0 ? node.seat.handle : { [node.seat.handle]: node.reports.map(shape) };

describe("reportingForest", () => {
  test("hangs each seat under its primary manager, roots first, in engine order", () => {
    const forest = reportingForest(
      derived([
        seat("ceo"),
        seat("cto", "ceo"),
        seat("dev", "cto"),
        seat("pm", "ceo"),
        seat("advisor"),
      ]),
    );
    expect(forest.roots.map(shape)).toEqual([{ ceo: [{ cto: ["dev"] }, "pm"] }, "advisor"]);
    expect(forest.cycles).toEqual([]);
  });

  test("a manager that names no seat leaves its report a root", () => {
    const forest = reportingForest(derived([seat("a", "ghost"), seat("b", "a")]));
    expect(forest.roots.map(shape)).toEqual([{ a: ["b"] }]);
  });

  test("a management cycle is a group broken at its first member, with everything under it", () => {
    const forest = reportingForest(
      derived([
        seat("root"),
        seat("hanger", "b"),
        seat("b", "a"),
        seat("a", "b"),
        seat("self", "self"),
      ]),
    );
    expect(forest.roots.map(shape)).toEqual(["root"]);
    // Broken at b, which the engine lists before a: b's manager edge is the
    // one ignored, and a and the seat hanging off the cycle sit under it.
    expect(forest.cycles.map(shape)).toEqual([{ b: ["hanger", "a"] }, "self"]);
    const members = forest.cycles[0]!;
    expect(members.cycle).toEqual([2, 3]);
    expect(members.reports.find((r) => r.seat.handle === "hanger")?.cycle).toBeUndefined();
    expect(forest.cycles[1]!.cycle).toEqual([4]);

    // A seat hanging off the cycle can lead the walk into it at its LATER
    // member; the break is still the member the engine lists first.
    const entered = reportingForest(
      derived([seat("root"), seat("hanger", "a"), seat("b", "a"), seat("a", "b")]),
    );
    expect(entered.cycles.map(shape)).toEqual([{ b: [{ a: ["hanger"] }] }]);
    expect(entered.cycles[0]!.cycle).toEqual([2, 3]);
  });

  test("a handle listed twice is two nodes, and its reports hang under the first", () => {
    const forest = reportingForest(derived([seat("dup"), seat("dup"), seat("x", "dup")]));
    expect([...walkForest(forest)].map((n) => n.index)).toEqual([0, 2, 1]);
  });

  test("an absent or empty derivation is an empty forest", () => {
    expect(reportingForest(null)).toEqual({ roots: [], cycles: [] });
    expect(reportingForest({ seats: null, units: null })).toEqual({ roots: [], cycles: [] });
  });

  test("every seat appears exactly once, for any manages graph", () => {
    const failures: string[] = [];
    for (let seed = 1; seed <= 400; seed++) {
      const rand = prng(seed);
      const count = 1 + Math.floor(rand() * 25);
      const handles = Array.from(
        { length: count },
        (_, i) => `s${Math.floor(rand() * count * 1.2)}x${i % 3 === 0 ? 0 : i}`,
      );
      const seats = handles.map((h) => {
        const roll = rand();
        const manager =
          roll < 0.2 ? "" : roll < 0.3 ? "ghost" : handles[Math.floor(rand() * count)]!;
        return seat(h, manager);
      });
      const forest = reportingForest(derived(seats));
      const seen = [...walkForest(forest)].map((n) => n.index).sort((a, b) => a - b);
      const expected = seats.map((_, i) => i);
      if (JSON.stringify(seen) !== JSON.stringify(expected)) {
        failures.push(`seed ${seed}: saw ${JSON.stringify(seen)} of ${count} seats`);
      }
      for (const root of forest.roots) {
        const m = seats[root.index]!.manager;
        if (m !== "" && seats.some((s) => s.handle === m))
          failures.push(`seed ${seed}: root ${root.index} has a manager`);
      }
    }
    expect(failures).toEqual([]);
  });
});

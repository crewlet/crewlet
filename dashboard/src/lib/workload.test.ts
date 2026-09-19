import { describe, expect, it } from "vitest";
import type { WorkloadRow } from "~/protocol/index.ts";
import type { Seat } from "./seats.ts";
import {
  OverCapacityAlarm,
  capacityText,
  loadOf,
  loadRows,
  loadSentence,
  loadTone,
} from "./workload.ts";

function row(handle: string, extra: Partial<WorkloadRow> = {}): WorkloadRow {
  return {
    handle,
    open: 1,
    points: 0,
    estimate_min: 0,
    blocked: 0,
    overdue: 0,
    unscheduled: 0,
    ...extra,
  };
}

function seat(handle: string): Seat {
  return { handle, name: handle, kind: "agent" } as Seat;
}

describe("a capacity nobody declared", () => {
  it("is unknown, not zero", () => {
    // A policy naming nobody would otherwise put every person in the company
    // permanently over, which is the failure that makes the screen useless.
    const load = loadOf(row("ada", { points: 8 }));
    expect(load.state).toBe("unknown");
    expect(load.capacity).toBeNull();
    expect(load.fraction).toBeNull();
    // NOT A DASH. "Nobody set a capacity" is a sentence a reader can act on;
    // a punctuation mark is read as "dash" or skipped.
    expect(capacityText(load)).toBe("not set");
  });

  it("wears no tone, because nothing is known either way", () => {
    // Colouring it would be a claim about the person rather than about the
    // company's configuration.
    expect(loadTone(loadOf(row("ada", { points: 40 })))).toBe("neutral");
  });

  it("says which of the two unknowns it is", () => {
    // "Nobody said" and "it cannot be added up" send a reader to two
    // different places — the first to a sprint policy, the second to two.
    const mixed = loadOf(row("ada", { points: 8, mixed_measures: true }));
    expect(mixed.state).toBe("unsummable");
    expect(capacityText(mixed)).toBe("mixed units");
    expect(loadSentence(mixed)).toContain("different units");
    expect(loadSentence(loadOf(row("bo")))).toContain("No sprint policy");
  });
});

describe("a capacity somebody declared", () => {
  it("compares in the measure the capacity is stated in", () => {
    // A capacity in points against a sum of minutes is two numbers about
    // different things.
    const inMinutes = loadOf(
      row("ada", {
        points: 99,
        estimate_min: 300,
        capacity: 600,
        capacity_measure: "estimate",
      }),
    );
    expect(inMinutes.held).toBe(300);
    expect(inMinutes.state).toBe("under");
  });

  it("is over when what they hold exceeds it", () => {
    const load = loadOf(row("ada", { points: 8, capacity: 5, capacity_measure: "points" }));
    expect(load.state).toBe("over");
    expect(load.fraction).toBeCloseTo(1.6);
  });

  it("escalates past the alarm rather than at it", () => {
    // A quarter over is the overflow a fortnight absorbs. Past that the plan
    // is wrong rather than tight.
    const tight = loadOf(row("ada", { points: 5, capacity: 4.5, capacity_measure: "points" }));
    expect(tight.fraction! < OverCapacityAlarm).toBe(true);
    expect(loadTone(tight)).toBe("caution");
    const broken = loadOf(row("bo", { points: 10, capacity: 4, capacity_measure: "points" }));
    expect(loadTone(broken)).toBe("critical");
    expect(loadTone(loadOf(row("cy", { points: 1, capacity: 5 })))).toBe("positive");
  });

  it("does not divide by a capacity of zero", () => {
    // Somebody declared to have no capacity at all is over by any amount of
    // work, and a division would answer Infinity — a bar of unbounded width.
    const load = loadOf(row("ada", { points: 3, capacity: 0, capacity_measure: "points" }));
    expect(load.state).toBe("over");
    expect(load.fraction).toBeNull();
    expect(loadTone(load)).toBe("critical");
  });

  it("says when the number covers more than one project", () => {
    const load = loadOf(
      row("ada", { points: 2, capacity: 8, capacity_from: 2, capacity_measure: "points" }),
    );
    expect(loadSentence(load)).toContain("across 2 projects");
    expect(loadSentence(loadOf(row("bo", { capacity: 5, capacity_from: 1 })))).not.toContain(
      "across",
    );
  });
});

describe("the rows a screen draws", () => {
  it("keeps the engine's order and adds the people holding nothing", () => {
    // A workload screen that listed only the busy would answer "is anybody
    // free" with silence.
    const rows = loadRows(
      [row("ada", { open: 4, points: 9 }), row("bo", { open: 1 })],
      [seat("bo"), seat("ada"), seat("cy")],
    );
    expect(rows.map((r) => r.handle)).toEqual(["ada", "bo", "cy"]);
    expect(rows[2]?.open).toBe(0);
    expect(rows[2]?.state).toBe("unknown");
  });

  it("never re-sorts by how far over somebody is", () => {
    // One point over a capacity of two would outrank forty against a
    // capacity nobody declared, which is not the question.
    const rows = loadRows(
      [row("ada", { open: 40, points: 40 }), row("bo", { open: 1, points: 3, capacity: 2 })],
      [],
    );
    expect(rows.map((r) => r.handle)).toEqual(["ada", "bo"]);
  });

  it("keeps a handle the roster does not know", () => {
    // Somebody who left with work still assigned. Worth seeing, so it is a
    // row rather than a filter.
    const rows = loadRows([row("departed", { open: 3 })], [seat("ada")]);
    expect(rows.map((r) => r.handle)).toEqual(["departed", "ada"]);
    expect(rows[0]?.seat).toBeUndefined();
    expect(rows[1]?.seat?.handle).toBe("ada");
  });
});

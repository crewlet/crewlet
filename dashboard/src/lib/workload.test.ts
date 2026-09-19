import { describe, expect, it } from "vitest";
import type { WorkloadRow } from "~/protocol/index.ts";
import type { Seat } from "./seats.ts";
import { HeavyQueue, loadFraction, loadOf, loadRows, loadSentence, loadTone } from "./workload.ts";

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

describe("a seat holding nothing", () => {
  it("is idle, which is not an open count of zero", () => {
    // "Nobody has given this seat anything" is a fact about the company
    // rather than about them, and it is the answer "is anybody free" wants.
    const load = loadOf(row("ada", { open: 0 }));
    expect(load.state).toBe("idle");
    expect(loadSentence(load)).toContain("no open work");
  });

  it("wears no tone, because holding nothing is neither good nor bad", () => {
    expect(loadTone(loadOf(row("ada", { open: 0 })))).toBe("neutral");
  });

  it("draws no bar, because a bar is a comparison", () => {
    // A bar of zero width and a bar at the two-pixel floor are the same two
    // pixels, so the row's own counts are what say which.
    expect(loadFraction(loadOf(row("ada", { open: 0 })), 0)).toBeNull();
  });
});

describe("a queue nothing in it can move", () => {
  it("is stuck only when EVERY open item is blocked", () => {
    // One blocker in a queue is an ordinary queue; a queue where nothing can
    // move is a person waiting, and the two read identically until it is said.
    expect(loadOf(row("ada", { open: 4, blocked: 4 })).state).toBe("stuck");
    expect(loadOf(row("bo", { open: 4, blocked: 3 })).state).toBe("working");
  });

  it("is critical rather than caution", () => {
    // It is the one row on this screen somebody has to act on.
    expect(loadTone(loadOf(row("ada", { open: 4, blocked: 4 })))).toBe("critical");
    expect(loadSentence(loadOf(row("ada", { open: 4, blocked: 4 })))).toContain("can move none");
  });
});

describe("a queue somebody is working through", () => {
  it("escalates past the heavy mark rather than at it", () => {
    const light = loadOf(row("ada", { open: HeavyQueue - 1 }));
    expect(loadTone(light)).toBe("positive");
    const heavy = loadOf(row("bo", { open: HeavyQueue }));
    expect(loadTone(heavy)).toBe("caution");
  });

  it("names what is wrong with it, and nothing that is not", () => {
    const load = loadOf(row("ada", { open: 4, blocked: 1, overdue: 2 }));
    expect(loadSentence(load)).toContain("1 blocked");
    expect(loadSentence(load)).toContain("2 overdue");
    expect(loadSentence(loadOf(row("bo", { open: 2 })))).not.toContain("blocked");
  });
});

describe("the bar's own scale", () => {
  it("is the heaviest queue on screen, not an absolute ceiling", () => {
    // A queue of thirty is heavy in one company and a quiet week in another,
    // so there is no number that is "full".
    expect(loadFraction(loadOf(row("ada", { open: 40 })), 40)).toBe(1);
    expect(loadFraction(loadOf(row("bo", { open: 10 })), 40)).toBeCloseTo(0.25);
  });

  it("never exceeds the track", () => {
    expect(loadFraction(loadOf(row("ada", { open: 80 })), 40)).toBe(1);
  });

  it("is null when nobody holds anything", () => {
    // A bar is a comparison, and with no queue anywhere there is nothing to
    // compare against — a bar of some arbitrary length would be a proportion
    // of a number that does not exist.
    expect(loadFraction(loadOf(row("ada", { open: 0 })), 0)).toBeNull();
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
    expect(rows[2]?.state).toBe("idle");
  });

  it("never re-sorts by anything of its own", () => {
    // The engine answers heaviest first; a second ordering here would make
    // two screens reading one answer disagree about who is at the top.
    const rows = loadRows(
      [row("ada", { open: 40, points: 40 }), row("bo", { open: 1, points: 3 })],
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

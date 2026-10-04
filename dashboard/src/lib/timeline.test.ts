import { describe, expect, it } from "vitest";
import type { WorkSummary } from "~/protocol/index.ts";
import {
  MaxTimelineDays,
  MinTimelineDays,
  barTitle,
  daysBetween,
  shiftDay,
  timelineOf,
  weekTicks,
} from "./timeline.ts";

/** A row with only what a timeline reads. Local noon, so a bucket by local day
 *  is not a test of the runner's zone. */
function task(id: string, start?: string, due?: string, extra: Partial<WorkSummary> = {}) {
  const at = (day?: string) => (day ? new Date(`${day}T12:00:00`).toISOString() : undefined);
  return {
    id,
    key: `ENG-${id}`,
    project: "ENG",
    title: id,
    type: "task",
    status: "todo",
    updated: "2026-06-01T00:00:00Z",
    version: 1,
    start: at(start),
    due: at(due),
    ...extra,
  } as WorkSummary;
}

const NOW = new Date("2026-06-15T09:00:00").getTime();

describe("where a bar sits", () => {
  it("spans the days between the two dates, inclusive at both ends", () => {
    const line = timelineOf([task("a", "2026-06-15", "2026-06-17")], { now: NOW });
    const bar = line.bars[0]!;
    // `to` is ONE PAST, so a width is always to - from: three days here.
    expect(bar.to - bar.from).toBe(3);
    expect(bar.kind).toBe("span");
    expect(bar.fromDay).toBe("2026-06-15");
    expect(bar.toDay).toBe("2026-06-17");
  });

  it("gives a task with one date a single day, and says which end is missing", () => {
    // Rendering these identically to a span is what makes a timeline look
    // like it knows more than it does.
    const [deadline] = timelineOf([task("a", undefined, "2026-06-20")], { now: NOW }).bars;
    expect(deadline?.kind).toBe("deadline");
    expect(deadline!.to - deadline!.from).toBe(1);
    const [began] = timelineOf([task("b", "2026-06-20")], { now: NOW }).bars;
    expect(began?.kind).toBe("began");
    expect(began!.to - began!.from).toBe(1);
  });

  it("does not swap dates that are the wrong way round", () => {
    // Swapping renders a coherent plan out of incoherent data, and the person
    // who typed it never finds out.
    const line = timelineOf([task("a", "2026-06-20", "2026-06-10")], { now: NOW });
    const bar = line.bars[0]!;
    expect(bar.kind).toBe("inverted");
    expect(bar.fromDay).toBe("2026-06-10");
    expect(bar.toDay).toBe("2026-06-20");
    expect(barTitle(bar)).toContain("the wrong way round");
  });

  it("leaves a row with neither date off the axis rather than placing it", () => {
    // Bucketing it under today would invent a deadline nobody set.
    const line = timelineOf([task("a", "2026-06-15", "2026-06-16"), task("b")], { now: NOW });
    expect(line.bars).toHaveLength(1);
    expect(line.unscheduled.map((r) => r.id)).toEqual(["b"]);
  });

  it("keeps the rows in the order they arrived", () => {
    // Re-sorting here would make a page's order depend on which rows the page
    // happened to hold.
    const line = timelineOf(
      [task("c", "2026-06-20"), task("a", "2026-06-10"), task("b", "2026-06-15")],
      { now: NOW },
    );
    expect(line.bars.map((b) => b.row.id)).toEqual(["c", "a", "b"]);
  });
});

describe("the window the data defines", () => {
  it("covers the earliest start through the latest due", () => {
    const line = timelineOf(
      [task("a", "2026-06-01", "2026-06-05"), task("b", "2026-06-20", "2026-06-30")],
      { now: NOW },
    );
    expect(line.from).toBe("2026-06-01");
    expect(line.to).toBe("2026-06-30");
    expect(line.days).toBe(30);
  });

  it("pads a narrow one around the data rather than against a wall", () => {
    // A bar in a one-day window has nothing to be read against.
    const line = timelineOf([task("a", "2026-06-15", "2026-06-15")], { now: NOW });
    expect(line.days).toBe(MinTimelineDays);
    const bar = line.bars[0]!;
    expect(bar.from).toBeGreaterThan(0);
    expect(bar.to).toBeLessThan(line.days);
  });

  it("caps a window one distant task would otherwise stretch, keeping the near end", () => {
    // A window trimmed from the left would drop today off an axis whose whole
    // point is where today is.
    const line = timelineOf(
      [task("near", "2026-06-15", "2026-06-16"), task("far", "2031-01-01", "2031-01-02")],
      { now: NOW },
    );
    expect(line.days).toBe(MaxTimelineDays);
    expect(line.from).toBe("2026-06-15");
    expect(line.today).toBeGreaterThanOrEqual(0);
    // AND THE CLIPPED ROW STILL HAS A BAR: a timeline missing rows the list
    // shows is a timeline nobody trusts.
    expect(line.bars).toHaveLength(2);
    const far = line.bars.find((b) => b.row.id === "far")!;
    expect(far.to).toBeLessThanOrEqual(line.days);
    expect(far.to).toBeGreaterThan(far.from);
  });

  it("answers an empty window for rows that carry no dates at all", () => {
    const line = timelineOf([task("a"), task("b")], { now: NOW });
    expect(line.days).toBe(0);
    expect(line.today).toBe(-1);
    expect(line.unscheduled).toHaveLength(2);
    expect(weekTicks(line)).toEqual([]);
  });

  it("marks today only when today is inside the window", () => {
    // The 11-day span pads to the 14-day minimum, one day of it before the
    // data, so the origin is the 9th and today is its seventh column.
    const inside = timelineOf([task("a", "2026-06-10", "2026-06-20")], { now: NOW });
    expect(inside.from).toBe("2026-06-09");
    expect(inside.today).toBe(6);
    const past = timelineOf([task("a", "2025-01-10", "2025-01-20")], { now: NOW });
    expect(past.today).toBe(-1);
  });
});

describe("the arrows", () => {
  const blocked = task("dep", "2026-06-18", "2026-06-20", {
    waiting_on: [{ id: "blk", open: true }],
  });
  const blocker = task("blk", "2026-06-15", "2026-06-17");

  it("joins two rows that are both on the axis", () => {
    const line = timelineOf([blocker, blocked], { now: NOW });
    expect(line.edges).toEqual([{ from: "blk", to: "dep", open: true, oneSided: false }]);
    expect(line.edgesOffAxis).toBe(0);
  });

  it("counts an edge whose blocker is not on the axis rather than dropping it", () => {
    // A reader who cannot see the omission reads the arrows as every
    // dependency there is.
    const line = timelineOf([blocked], { now: NOW });
    expect(line.edges).toEqual([]);
    expect(line.edgesOffAxis).toBe(1);
  });

  it("counts an edge whose blocker is on the page but unscheduled", () => {
    // It has a row and no bar, so there is no end to draw the arrow at.
    const line = timelineOf([task("blk"), blocked], { now: NOW });
    expect(line.edges).toEqual([]);
    expect(line.edgesOffAxis).toBe(1);
  });

  it("carries the edge's own state rather than re-deriving it", () => {
    const line = timelineOf(
      [
        blocker,
        task("dep", "2026-06-18", "2026-06-20", {
          waiting_on: [{ id: "blk", open: false, one_sided: true }],
        }),
      ],
      { now: NOW },
    );
    expect(line.edges[0]).toEqual({ from: "blk", to: "dep", open: false, oneSided: true });
  });
});

describe("the axis rules", () => {
  it("rules on Mondays, and always labels its own first column", () => {
    // Monday because `internal/tracker/dates.go` pins the week's start there.
    // A ruled axis whose leftmost column has no label reads as a column that
    // is not there.
    const line = timelineOf([task("a", "2026-06-03", "2026-06-24")], { now: NOW });
    const ticks = weekTicks(line);
    expect(ticks[0]?.day).toBe("2026-06-03");
    expect(ticks.slice(1).map((t) => t.day)).toEqual(["2026-06-08", "2026-06-15", "2026-06-22"]);
    for (const tick of ticks.slice(1)) {
      expect(new Date(`${tick.day}T00:00:00`).getDay()).toBe(1);
    }
  });

  it("drops its own first label when a Monday would overprint it", () => {
    // A window starting on a Sunday rendered "Sep 2Sep 28", which is not a
    // date at all. The Monday wins: a week rule is what the axis is ruled by.
    const line = timelineOf([task("a", "2026-09-27", "2026-10-10")], { now: NOW });
    expect(line.from).toBe("2026-09-27");
    const ticks = weekTicks(line);
    expect(ticks[0]?.day).toBe("2026-09-28");
    expect(ticks.map((t) => t.at)).not.toContain(0);
  });

  it("keeps its first label when the next Monday is far enough away", () => {
    // Wednesday the 23rd: the Monday is five columns off, so both fit and the
    // leftmost column is labelled rather than looking like it is not there.
    const line = timelineOf([task("a", "2026-09-23", "2026-10-10")], { now: NOW });
    const ticks = weekTicks(line);
    expect(ticks[0]?.day).toBe("2026-09-23");
    expect(ticks[1]?.day).toBe("2026-09-28");
  });
});

describe("day arithmetic", () => {
  it("counts whole days across a month and a year boundary", () => {
    expect(daysBetween("2026-06-28", "2026-07-02")).toBe(4);
    expect(daysBetween("2026-12-30", "2027-01-02")).toBe(3);
    expect(daysBetween("2026-06-05", "2026-06-01")).toBe(-4);
  });

  it("counts a span across a daylight-saving change as whole days", () => {
    // A truncating divide turns a 23- or 25-hour day into an off-by-one on
    // every bar crossing the change.
    expect(daysBetween("2026-03-28", "2026-03-30")).toBe(2);
    expect(daysBetween("2026-10-24", "2026-10-26")).toBe(2);
  });

  it("shifts across a leap day", () => {
    expect(shiftDay("2028-02-28", 1)).toBe("2028-02-29");
    expect(shiftDay("2028-02-29", 1)).toBe("2028-03-01");
    expect(shiftDay("2026-01-01", -1)).toBe("2025-12-31");
  });

  it("answers a value it cannot read rather than throwing", () => {
    expect(daysBetween("not a day", "2026-06-01")).toBe(0);
    expect(shiftDay("not a day", 3)).toBe("not a day");
  });
});

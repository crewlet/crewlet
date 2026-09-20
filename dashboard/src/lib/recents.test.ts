import { beforeEach, describe, expect, it, vi } from "vitest";
import { MaxRecents, forgetAll, remember, resetForTest } from "./recents.ts";

/** Read the list the way the hook's snapshot does, without rendering. */
function stored(): { path: string[]; label: string; at: number }[] {
  const raw = localStorage.getItem("crewlet_recents");
  return raw ? (JSON.parse(raw) as { path: string[]; label: string; at: number }[]) : [];
}

beforeEach(() => {
  localStorage.clear();
  resetForTest();
});

describe("what the reader opened", () => {
  it("puts a place the reader has not been at the top", () => {
    remember({ path: ["work", "ENG-1"], label: "ENG-1", workspace: "work" });
    remember({ path: ["knowledge", "ENG", "Runbook"], label: "Runbook", workspace: "knowledge" });
    expect(stored().map((r) => r.label)).toEqual(["Runbook", "ENG-1"]);
  });

  it("stores a revisit once, and leaves it exactly where it was", () => {
    // Keyed on the PATH: without that, a reader who keeps returning to one
    // item fills the whole list with it.
    //
    // AND THE SLOT DOES NOT MOVE, which is the half this list got wrong.
    // Re-inserting at the top is fine for a ranking and wrong for a DRAWN
    // list: the workspace sidebar's Recent section sent the row the reader
    // had just pressed to the first position and slid the rows above it down,
    // under the pointer, at the instant it was hit.
    remember({ path: ["work", "ENG-1"], label: "ENG-1", workspace: "work" });
    remember({ path: ["work", "ENG-2"], label: "ENG-2", workspace: "work" });
    remember({ path: ["work", "ENG-3"], label: "ENG-3", workspace: "work" });
    remember({ path: ["work", "ENG-2"], label: "ENG-2", workspace: "work" });
    expect(stored().map((r) => r.label)).toEqual(["ENG-3", "ENG-2", "ENG-1"]);
  });

  it("records when each place was last opened, for the readers that rank", () => {
    // `at` had no reader at all while the order was the ranking. It is the
    // palette's order and the cap's eviction rule now, so a revisit has to
    // move it even though it moves nothing else.
    vi.useFakeTimers();
    try {
      vi.setSystemTime(new Date("2026-09-20T10:00:00Z"));
      remember({ path: ["work", "ENG-1"], label: "ENG-1", workspace: "work" });
      vi.setSystemTime(new Date("2026-09-20T10:05:00Z"));
      remember({ path: ["work", "ENG-2"], label: "ENG-2", workspace: "work" });
      vi.setSystemTime(new Date("2026-09-20T10:09:00Z"));
      remember({ path: ["work", "ENG-1"], label: "ENG-1", workspace: "work" });
    } finally {
      vi.useRealTimers();
    }
    const at = Object.fromEntries(stored().map((r) => [r.label, r.at]));
    expect(at["ENG-1"]).toBeGreaterThan(at["ENG-2"]!);
  });

  it("takes the latest label for a path", () => {
    // A page's title lands a render after its route, so the first label for a
    // path is often the raw identifier and the second is the name the reader
    // recognises.
    remember({ path: ["work", "ENG-1"], label: "ENG-1", workspace: "work" });
    remember({ path: ["work", "ENG-1"], label: "Drop the Pulsar backend", workspace: "work" });
    expect(stored().map((r) => r.label)).toEqual(["Drop the Pulsar backend"]);
  });

  it("keeps only a few, and drops the one nobody has opened in longest", () => {
    // NOT THE LAST IN THE LIST, which under arrival order is merely the one
    // that has been here longest — the board somebody opens every morning.
    // The entry to lose is the one with the oldest VISIT.
    vi.useFakeTimers();
    try {
      vi.setSystemTime(new Date("2026-09-20T09:00:00Z"));
      for (let i = 0; i < MaxRecents; i++) {
        vi.advanceTimersByTime(60_000);
        remember({ path: ["work", `ENG-${i}`], label: `ENG-${i}`, workspace: "work" });
      }
      // The oldest ARRIVAL is opened again, so the oldest VISIT is now the
      // second one in — and that is what the next arrival has to evict.
      vi.advanceTimersByTime(60_000);
      remember({ path: ["work", "ENG-0"], label: "ENG-0", workspace: "work" });
      vi.advanceTimersByTime(60_000);
      remember({ path: ["work", "NEW"], label: "NEW", workspace: "work" });
    } finally {
      vi.useRealTimers();
    }
    const labels = stored().map((r) => r.label);
    expect(labels).toHaveLength(MaxRecents);
    expect(labels[0]).toBe("NEW");
    expect(labels, "the revisited entry was evicted although it was just opened").toContain(
      "ENG-0",
    );
    expect(labels, "the least recently opened entry survived the cap").not.toContain("ENG-1");
  });

  it("keeps a revisited entry in its own slot when the list is full", () => {
    for (let i = 0; i < MaxRecents; i++) {
      remember({ path: ["work", `ENG-${i}`], label: `ENG-${i}`, workspace: "work" });
    }
    const before = stored().map((r) => r.label);
    remember({ path: ["work", "ENG-3"], label: "ENG-3", workspace: "work" });
    expect(stored().map((r) => r.label)).toEqual(before);
  });

  it("stores nothing for a path with no segments", () => {
    remember({ path: [], label: "nowhere", workspace: "" });
    expect(stored()).toEqual([]);
  });

  it("forgets everything on request", () => {
    remember({ path: ["work", "ENG-1"], label: "ENG-1", workspace: "work" });
    forgetAll();
    expect(stored()).toEqual([]);
  });
});

describe("a store that will not cooperate", () => {
  it("drops a row a previous build wrote in a shape this one cannot use", () => {
    // The value survives upgrades, so a row in an old shape is an ordinary
    // thing to find — and rendering a recent with no path is not an option.
    localStorage.setItem(
      "crewlet_recents",
      JSON.stringify([{ label: "no path" }, { path: ["work", "ENG-1"], label: "ENG-1", at: 1 }]),
    );
    resetForTest();
    remember({ path: ["work", "ENG-2"], label: "ENG-2", workspace: "work" });
    expect(stored().map((r) => r.label)).toEqual(["ENG-2", "ENG-1"]);
  });

  it("carries on when storage throws", () => {
    // A private window, blocked site data, a quota refusal. None of them is a
    // reason for a palette not to open.
    const setItem = vi.spyOn(Storage.prototype, "setItem").mockImplementation(() => {
      throw new Error("blocked");
    });
    const getItem = vi.spyOn(Storage.prototype, "getItem").mockImplementation(() => {
      throw new Error("blocked");
    });
    resetForTest();
    expect(() =>
      remember({ path: ["work", "ENG-1"], label: "ENG-1", workspace: "work" }),
    ).not.toThrow();
    setItem.mockRestore();
    getItem.mockRestore();
  });

  it("reads a value that is not even a list as nothing", () => {
    localStorage.setItem("crewlet_recents", '"not a list"');
    resetForTest();
    remember({ path: ["work", "ENG-1"], label: "ENG-1", workspace: "work" });
    expect(stored().map((r) => r.label)).toEqual(["ENG-1"]);
  });
});

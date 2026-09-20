import { beforeEach, describe, expect, it, vi } from "vitest";
import { MaxRecents, forgetAll, remember, resetForTest } from "./recents.ts";

/** Read the list the way the hook's snapshot does, without rendering. */
type Row = { path: string[]; label: string; workspace: string; at: number };

function stored(): Row[] {
  const raw = localStorage.getItem("crewlet_recents");
  return raw ? (JSON.parse(raw) as Row[]) : [];
}

beforeEach(() => {
  localStorage.clear();
  resetForTest();
});

describe("what the reader opened", () => {
  it("puts a place the reader has not been at the top", () => {
    remember({ path: ["work", "ENG-1"], label: "ENG-1", workspace: "work" }, true);
    remember(
      { path: ["knowledge", "ENG", "Runbook"], label: "Runbook", workspace: "knowledge" },
      true,
    );
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
    remember({ path: ["work", "ENG-1"], label: "ENG-1", workspace: "work" }, true);
    remember({ path: ["work", "ENG-2"], label: "ENG-2", workspace: "work" }, true);
    remember({ path: ["work", "ENG-3"], label: "ENG-3", workspace: "work" }, true);
    remember({ path: ["work", "ENG-2"], label: "ENG-2", workspace: "work" }, true);
    expect(stored().map((r) => r.label)).toEqual(["ENG-3", "ENG-2", "ENG-1"]);
  });

  it("records when each place was last opened, for the readers that rank", () => {
    // `at` had no reader at all while the order was the ranking. It is the
    // palette's order and the cap's eviction rule now, so a revisit has to
    // move it even though it moves nothing else.
    vi.useFakeTimers();
    try {
      vi.setSystemTime(new Date("2026-09-20T10:00:00Z"));
      remember({ path: ["work", "ENG-1"], label: "ENG-1", workspace: "work" }, true);
      vi.setSystemTime(new Date("2026-09-20T10:05:00Z"));
      remember({ path: ["work", "ENG-2"], label: "ENG-2", workspace: "work" }, true);
      vi.setSystemTime(new Date("2026-09-20T10:09:00Z"));
      remember({ path: ["work", "ENG-1"], label: "ENG-1", workspace: "work" }, true);
    } finally {
      vi.useRealTimers();
    }
    const at = Object.fromEntries(stored().map((r) => [r.label, r.at]));
    expect(at["ENG-1"]).toBeGreaterThan(at["ENG-2"]!);
  });

  it("takes the latest label a screen resolved for a path", () => {
    // A page's title lands a render after its route, so the first label for a
    // path is often the raw identifier and the second is the name the reader
    // recognises.
    remember({ path: ["work", "ENG-1"], label: "ENG-1", workspace: "work" }, false);
    remember(
      { path: ["work", "ENG-1"], label: "Drop the Pulsar backend", workspace: "work" },
      true,
    );
    expect(stored().map((r) => r.label)).toEqual(["Drop the Pulsar backend"]);
  });

  it("never replaces a name it holds with the object's own identifier", () => {
    // THE FLASH ON THE ROW THE READER PRESSED. Opening a recent re-navigates
    // to it, and the first write of every navigation carries the raw segment
    // because the screen publishes its name a render later. Overwriting on
    // that write turned the row's title into a hex string at the instant it
    // was clicked, until the query came back.
    remember({ path: ["activity", "turns", "abc"], label: "abc", workspace: "activity" }, false);
    remember(
      { path: ["activity", "turns", "abc"], label: "Cut the release", workspace: "activity" },
      true,
    );
    remember({ path: ["activity", "turns", "abc"], label: "abc", workspace: "activity" }, false);
    expect(stored().map((r) => r.label)).toEqual(["Cut the release"]);
  });

  it("stores an identifier for a place nothing has ever named", () => {
    // An object NOTHING names — a turn with no plan summary — has its id and
    // nothing else, and a rail that dropped it would lose a place the reader
    // was. `routes/activity/crumbs.test.tsx` pins the same rule from the
    // screen's end.
    remember({ path: ["activity", "turns", "abc"], label: "abc", workspace: "activity" }, false);
    expect(stored().map((r) => r.label)).toEqual(["abc"]);
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
        remember({ path: ["work", `ENG-${i}`], label: `ENG-${i}`, workspace: "work" }, true);
      }
      // The oldest ARRIVAL is opened again, so the oldest VISIT is now the
      // second one in — and that is what the next arrival has to evict.
      vi.advanceTimersByTime(60_000);
      remember({ path: ["work", "ENG-0"], label: "ENG-0", workspace: "work" }, true);
      vi.advanceTimersByTime(60_000);
      remember({ path: ["work", "NEW"], label: "NEW", workspace: "work" }, true);
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
      remember({ path: ["work", `ENG-${i}`], label: `ENG-${i}`, workspace: "work" }, true);
    }
    const before = stored().map((r) => r.label);
    remember({ path: ["work", "ENG-3"], label: "ENG-3", workspace: "work" }, true);
    expect(stored().map((r) => r.label)).toEqual(before);
  });

  it("stores nothing for a path with no segments", () => {
    remember({ path: [], label: "nowhere", workspace: "" }, true);
    expect(stored()).toEqual([]);
  });

  it("forgets everything on request", () => {
    remember({ path: ["work", "ENG-1"], label: "ENG-1", workspace: "work" }, true);
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
      JSON.stringify([
        { label: "no path" },
        // A ROW WITH NO WORKSPACE is the shape every build before the cap
        // became per-workspace could write, and it is one no rail can draw.
        { path: ["work", "ENG-0"], label: "ENG-0", at: 0 },
        { path: ["work", "ENG-1"], label: "ENG-1", workspace: "work", at: 1 },
      ]),
    );
    resetForTest();
    remember({ path: ["work", "ENG-2"], label: "ENG-2", workspace: "work" }, true);
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
      remember({ path: ["work", "ENG-1"], label: "ENG-1", workspace: "work" }, true),
    ).not.toThrow();
    setItem.mockRestore();
    getItem.mockRestore();
  });

  it("reads a value that is not even a list as nothing", () => {
    localStorage.setItem("crewlet_recents", '"not a list"');
    resetForTest();
    remember({ path: ["work", "ENG-1"], label: "ENG-1", workspace: "work" }, true);
    expect(stored().map((r) => r.label)).toEqual(["ENG-1"]);
  });
});

/**
 * A SECOND TAB IS THE SAME STORAGE, and a write replaces the whole key.
 *
 * The module holds a render snapshot for `useSyncExternalStore`, which must
 * be referentially stable. Built a mutation on it, this list handed back
 * whatever the tab last painted — so with two tabs open, everywhere the
 * first one had been since the second one's last click was gone at that
 * click. See `lib/starred.ts`, where the same shape cost decisions rather
 * than a reading habit.
 */
describe("two tabs on one origin", () => {
  it("keeps where another tab has been while this one was painting", () => {
    remember({ path: ["work", "ENG-1"], label: "ENG-1", workspace: "work" }, true);
    // This tab paints, so the snapshot is taken.
    expect(stored().map((r) => r.label)).toEqual(["ENG-1"]);
    // The other tab goes somewhere. No event is delivered here.
    localStorage.setItem(
      "crewlet_recents",
      JSON.stringify([
        { path: ["work", "ENG-2"], label: "ENG-2", workspace: "work", at: 2 },
        { path: ["work", "ENG-1"], label: "ENG-1", workspace: "work", at: 1 },
      ]),
    );
    remember({ path: ["work", "ENG-3"], label: "ENG-3", workspace: "work" }, true);
    expect(stored().map((r) => r.label)).toEqual(["ENG-3", "ENG-2", "ENG-1"]);
  });
});

/**
 * The cap counts what a rail DRAWS, which is one workspace's rows.
 *
 * It was a single global bound, written when the command palette was the only
 * reader and offered the whole list. The sidebar's Recent section came later
 * and filters to the workspace it is drawn in, so across a rail of eight
 * workspaces the reader saw one or two rows where the number says eight.
 */
describe("the cap", () => {
  it("is per workspace, so one workspace cannot empty another", () => {
    for (let i = 0; i < MaxRecents + 3; i++) {
      remember({ path: ["work", `ENG-${i}`], label: `ENG-${i}`, workspace: "work" }, true);
    }
    remember({ path: ["activity", "turns", "t1"], label: "A turn", workspace: "activity" }, true);
    for (let i = MaxRecents + 3; i < MaxRecents + 8; i++) {
      remember({ path: ["work", `ENG-${i}`], label: `ENG-${i}`, workspace: "work" }, true);
    }
    const kept = stored();
    expect(kept.filter((r) => r.workspace === "work")).toHaveLength(MaxRecents);
    expect(
      kept.filter((r) => r.workspace === "activity").map((r) => r.label),
      "a morning in Work emptied the Activity rail",
    ).toEqual(["A turn"]);
  });

  it("refuses a place no workspace owns rather than storing one nothing draws", () => {
    remember({ path: ["nowhere", "x"], label: "X", workspace: "" }, true);
    expect(stored()).toEqual([]);
  });
});

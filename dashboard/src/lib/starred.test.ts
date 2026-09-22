import { beforeEach, describe, expect, it, vi } from "vitest";
import { MaxStars, isStarred, resetForTest, starredIn, toggleStar } from "./starred.ts";

function stored(): { path: string[]; label: string }[] {
  const raw = localStorage.getItem("crewlet_starred");
  return raw ? (JSON.parse(raw) as { path: string[]; label: string }[]) : [];
}

function star(label: string, ...path: string[]) {
  return toggleStar({ path, label, workspace: "work" });
}

beforeEach(() => {
  localStorage.clear();
  resetForTest();
});

describe("keeping a shortcut", () => {
  it("adds it, and says which way the toggle went", () => {
    expect(star("ENG-1", "work", "ENG-1")).toBe("starred");
    expect(isStarred(["work", "ENG-1"])).toBe(true);
    expect(star("ENG-1", "work", "ENG-1")).toBe("unstarred");
    expect(isStarred(["work", "ENG-1"])).toBe(false);
    expect(stored()).toEqual([]);
  });

  it("keeps them in the order they were kept, newest LAST", () => {
    // Unlike recents. A star's order is the order somebody chose, and a list
    // that reshuffled on every visit would be a recents list wearing a star.
    star("first", "work", "a");
    star("second", "work", "b");
    star("third", "work", "c");
    expect(stored().map((s) => s.label)).toEqual(["first", "second", "third"]);
  });

  it("is keyed on the path, so two labels for one route are one star", () => {
    star("ENG-1", "work", "ENG-1");
    expect(star("Drop the Pulsar backend", "work", "ENG-1")).toBe("unstarred");
    expect(stored()).toEqual([]);
  });

  it("refuses at the cap rather than dropping somebody's oldest", () => {
    // Every entry here is one somebody chose. Silently evicting a decision to
    // make room for another is the one behaviour a bookmark list may not have.
    for (let i = 0; i < MaxStars; i++) star(`item-${i}`, "work", `ENG-${i}`);
    expect(stored()).toHaveLength(MaxStars);
    expect(star("one too many", "work", "ENG-999")).toBe("full");
    expect(stored()).toHaveLength(MaxStars);
    expect(stored()[0]?.label).toBe("item-0");
    // AND UNSTARRING STILL WORKS AT THE CAP, which is how a reader gets out
    // of it — a refusal that also froze the list would be a dead end.
    expect(star("item-0", "work", "ENG-0")).toBe("unstarred");
    expect(star("one too many", "work", "ENG-999")).toBe("starred");
  });

  /**
   * THE REFUSAL AND THE READ HAVE TO AGREE, and they did not.
   *
   * [MaxStars] says at its own definition that reaching the cap is a refusal
   * which SAYS SO rather than a silent drop of the oldest, since every entry
   * is one somebody chose. `toggleStar` honoured that; `stored()` had a
   * `.slice(0, MaxStars)` on it, which is that silent drop — and because
   * `write` replaces the WHOLE key, the next unstar of any row PERSISTED the
   * shortened list, destroying stars nobody had touched while reporting
   * "unstarred" about the one that was clicked.
   *
   * The over-cap value is written directly, which is how it arrives in
   * practice: another tab on a build with a different cap, or a hand-edited
   * key. One origin, one key, no coordination.
   */
  it("carries an over-cap list whole rather than trimming it on the way in", () => {
    const many = Array.from({ length: MaxStars + 3 }, (_, i) => ({
      path: ["work", `ENG-${i}`],
      label: `item-${i}`,
      workspace: "work",
      at: i,
    }));
    localStorage.setItem("crewlet_starred", JSON.stringify(many));
    resetForTest();
    // THE LAST ONE IS STILL THERE — the row the cut dropped first.
    expect(isStarred(["work", `ENG-${MaxStars + 2}`])).toBe(true);
    // AND AN UNSTAR TAKES EXACTLY THE ROW THAT WAS CLICKED WITH IT.
    expect(star("item-0", "work", "ENG-0")).toBe("unstarred");
    expect(stored()).toHaveLength(MaxStars + 2);
    expect(isStarred(["work", `ENG-${MaxStars + 2}`])).toBe(true);
  });

  it("still refuses to add while the stored list is over the cap", () => {
    // The bound the cap exists for holds from the only place that can grow the
    // list. Reading an over-cap value is not permission to write past it.
    const many = Array.from({ length: MaxStars + 1 }, (_, i) => ({
      path: ["work", `ENG-${i}`],
      label: `item-${i}`,
      workspace: "work",
      at: i,
    }));
    localStorage.setItem("crewlet_starred", JSON.stringify(many));
    resetForTest();
    expect(star("one too many", "work", "ENG-999")).toBe("full");
    expect(stored()).toHaveLength(MaxStars + 1);
  });

  it("stores nothing for a path with no segments", () => {
    expect(star("nowhere")).toBe("full");
    expect(stored()).toEqual([]);
  });

  it("answers over a list the caller already holds, the same way", () => {
    // `StarPage` subscribes to the list so its button fills the moment a star
    // is kept; `isStarred` reads storage. They must not be able to disagree,
    // which is why one is written in terms of the other.
    star("ENG-1", "work", "ENG-1");
    const held = [{ path: ["work", "ENG-1"], label: "ENG-1", workspace: "work", at: 1 }];
    expect(starredIn(held, ["work", "ENG-1"])).toBe(isStarred(["work", "ENG-1"]));
    expect(starredIn(held, ["work", "ENG-2"])).toBe(isStarred(["work", "ENG-2"]));
  });
});

describe("a store that will not cooperate", () => {
  it("drops a row a previous build wrote in a shape this one cannot use", () => {
    localStorage.setItem(
      "crewlet_starred",
      JSON.stringify([{ label: "no path" }, { path: ["work", "ENG-1"], label: "ENG-1", at: 1 }]),
    );
    resetForTest();
    expect(isStarred(["work", "ENG-1"])).toBe(true);
    star("ENG-2", "work", "ENG-2");
    expect(stored().map((s) => s.label)).toEqual(["ENG-1", "ENG-2"]);
  });

  it("carries on when storage throws", () => {
    const setItem = vi.spyOn(Storage.prototype, "setItem").mockImplementation(() => {
      throw new Error("blocked");
    });
    const getItem = vi.spyOn(Storage.prototype, "getItem").mockImplementation(() => {
      throw new Error("blocked");
    });
    resetForTest();
    expect(() => star("ENG-1", "work", "ENG-1")).not.toThrow();
    setItem.mockRestore();
    getItem.mockRestore();
  });

  it("reads a value that is not even a list as nothing", () => {
    localStorage.setItem("crewlet_starred", '"not a list"');
    resetForTest();
    star("ENG-1", "work", "ENG-1");
    expect(stored().map((s) => s.label)).toEqual(["ENG-1"]);
  });
});

/**
 * A SECOND TAB IS THE SAME STORAGE, and a write replaces the whole key.
 *
 * The module holds a render snapshot for `useSyncExternalStore`, which must
 * be referentially stable. Built a mutation on it, this list handed back
 * whatever the tab last painted — so with two tabs open on one company, the
 * second one's star destroyed the first one's, silently, while `toggleStar`
 * returned "starred". This list refuses at the cap rather than evicting,
 * because every entry is a decision somebody made; this path evicted up to
 * forty-nine of them.
 *
 * The other tab is simulated by writing the key directly, which is exactly
 * what it does: one origin, one key, no coordination.
 */
describe("two tabs on one origin", () => {
  it("keeps what another tab starred while this one was painting", () => {
    star("ENG-1", "work", "ENG-1");
    // This tab paints, so the snapshot is taken.
    expect(isStarred(["work", "ENG-1"])).toBe(true);
    // The other tab stars something. No event is delivered here.
    localStorage.setItem(
      "crewlet_starred",
      JSON.stringify([
        { path: ["work", "ENG-1"], label: "ENG-1", workspace: "work", at: 1 },
        { path: ["work", "ENG-2"], label: "ENG-2", workspace: "work", at: 2 },
      ]),
    );
    star("ENG-3", "work", "ENG-3");
    expect(stored().map((s) => s.label)).toEqual(["ENG-1", "ENG-2", "ENG-3"]);
  });

  it("unstars against what is stored, not against what it last drew", () => {
    star("ENG-1", "work", "ENG-1");
    localStorage.setItem(
      "crewlet_starred",
      JSON.stringify([
        { path: ["work", "ENG-1"], label: "ENG-1", workspace: "work", at: 1 },
        { path: ["work", "ENG-2"], label: "ENG-2", workspace: "work", at: 2 },
      ]),
    );
    expect(star("ENG-1", "work", "ENG-1")).toBe("unstarred");
    expect(stored().map((s) => s.label)).toEqual(["ENG-2"]);
  });
});

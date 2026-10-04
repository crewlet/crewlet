import { beforeEach, describe, expect, it, vi } from "vitest";
import { act, renderHook } from "@testing-library/react";
import {
  MaxRecents,
  forgetAll,
  recentsKey,
  remember,
  resetForTest,
  useRecents,
} from "./recents.ts";
import { noteReader } from "./reader.ts";

/** Read the list the way the hook's snapshot does, without rendering. */
type Row = { path: string[]; label: string; workspace: string };

function stored(reader = "p-1"): Row[] {
  const raw = localStorage.getItem(recentsKey(reader));
  return raw ? (JSON.parse(raw) as Row[]) : [];
}

beforeEach(() => {
  localStorage.clear();
  sessionStorage.clear();
  noteReader("p-1");
  resetForTest();
});

/**
 * ONE BROWSER IS OFTEN SEVERAL PEOPLE. A single list per browser drew the last
 * person's recent titles — a colleague's review, an incident, a person's page —
 * in the next person's palette and rail on a shared machine.
 */
describe("whose list it is", () => {
  it("is the tab's reader's, and a second reader of the browser has their own", () => {
    remember({ path: ["work", "ENG-1"], label: "ENG-1", workspace: "work" }, true);
    const { result } = renderHook(() => useRecents());
    expect(result.current.map((r) => r.label)).toEqual(["ENG-1"]);

    act(() => noteReader("p-2"));
    expect(result.current).toEqual([]);
    act(() => remember({ path: ["work", "ENG-2"], label: "ENG-2", workspace: "work" }, true));
    expect(result.current.map((r) => r.label)).toEqual(["ENG-2"]);
    // AND THE FIRST PERSON'S IS STILL THERE for when they are back.
    expect(stored("p-1").map((r) => r.label)).toEqual(["ENG-1"]);
    expect(stored("p-2").map((r) => r.label)).toEqual(["ENG-2"]);
  });

  it("is nobody's until the tab knows who reads it, and records nothing", () => {
    sessionStorage.clear();
    resetForTest();
    const { result } = renderHook(() => useRecents());
    remember({ path: ["work", "ENG-1"], label: "ENG-1", workspace: "work" }, true);
    expect(result.current).toEqual([]);
    for (let i = 0; i < localStorage.length; i++) {
      expect(localStorage.key(i) ?? "").not.toMatch(/^crewlet_recents/);
    }
  });
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

  it("stores a revisit once, and moves it to the top", () => {
    // Keyed on the PATH: without that, a reader who keeps returning to one
    // item fills the whole list with it. And the palette's first row is where
    // the reader just was, so a revisit is newest.
    remember({ path: ["work", "ENG-1"], label: "ENG-1", workspace: "work" }, true);
    remember({ path: ["work", "ENG-2"], label: "ENG-2", workspace: "work" }, true);
    remember({ path: ["work", "ENG-3"], label: "ENG-3", workspace: "work" }, true);
    remember({ path: ["work", "ENG-2"], label: "ENG-2", workspace: "work" }, true);
    expect(stored().map((r) => r.label)).toEqual(["ENG-2", "ENG-3", "ENG-1"]);
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
    remember({ path: ["live", "turns", "abc"], label: "abc", workspace: "live" }, false);
    remember({ path: ["live", "turns", "abc"], label: "Cut the release", workspace: "live" }, true);
    remember({ path: ["live", "turns", "abc"], label: "abc", workspace: "live" }, false);
    expect(stored().map((r) => r.label)).toEqual(["Cut the release"]);
  });

  it("stores an identifier for a place nothing has ever named", () => {
    // An object NOTHING names — a turn with no plan summary — has its id and
    // nothing else, and a rail that dropped it would lose a place the reader
    // was. `routes/live/crumbs.test.tsx` pins the same rule from the
    // screen's end.
    remember({ path: ["live", "turns", "abc"], label: "abc", workspace: "live" }, false);
    expect(stored().map((r) => r.label)).toEqual(["abc"]);
  });

  it("keeps only a few, and drops the one nobody has opened in longest", () => {
    // NOT THE FIRST ONE IN: the board somebody opens every morning arrived
    // first and is still wanted. A revisit moves it up, so the bottom row is
    // always the oldest VISIT.
    for (let i = 0; i < MaxRecents; i++) {
      remember({ path: ["work", `ENG-${i}`], label: `ENG-${i}`, workspace: "work" }, true);
    }
    remember({ path: ["work", "ENG-0"], label: "ENG-0", workspace: "work" }, true);
    remember({ path: ["work", "NEW"], label: "NEW", workspace: "work" }, true);
    const labels = stored().map((r) => r.label);
    expect(labels).toHaveLength(MaxRecents);
    expect(labels[0]).toBe("NEW");
    expect(labels, "the revisited entry was evicted although it was just opened").toContain(
      "ENG-0",
    );
    expect(labels, "the least recently opened entry survived the cap").not.toContain("ENG-1");
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
      recentsKey("p-1"),
      JSON.stringify([
        { label: "no path" },
        // A ROW WITH NO WORKSPACE is a shape an older build wrote, and one
        // the palette has no hint for.
        { path: ["work", "ENG-0"], label: "ENG-0" },
        // A ROUTE THIS BUILD DOES NOT HAVE, which a row outlives: drawn, it
        // would lead to Not Found for ever.
        { path: ["activity", "turns"], label: "Turns", workspace: "activity" },
        { path: ["work", "ENG-1"], label: "ENG-1", workspace: "work" },
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
    localStorage.setItem(recentsKey("p-1"), '"not a list"');
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
      recentsKey("p-1"),
      JSON.stringify([
        { path: ["work", "ENG-2"], label: "ENG-2", workspace: "work" },
        { path: ["work", "ENG-1"], label: "ENG-1", workspace: "work" },
      ]),
    );
    remember({ path: ["work", "ENG-3"], label: "ENG-3", workspace: "work" }, true);
    expect(stored().map((r) => r.label)).toEqual(["ENG-3", "ENG-2", "ENG-1"]);
  });
});

/**
 * The cap counts what the palette DRAWS, which is the whole list.
 *
 * It was per workspace while a sidebar drew one workspace's share; with the
 * palette the only reader, that let an empty palette open on up to eight rows
 * for each of nine workspaces.
 */
describe("the cap", () => {
  it("is one bound over every workspace", () => {
    remember({ path: ["live", "turns", "t1"], label: "A turn", workspace: "live" }, true);
    for (let i = 0; i < MaxRecents + 3; i++) {
      remember({ path: ["work", `ENG-${i}`], label: `ENG-${i}`, workspace: "work" }, true);
    }
    const kept = stored();
    expect(kept).toHaveLength(MaxRecents);
    expect(kept.some((r) => r.workspace === "live")).toBe(false);
  });

  it("refuses a place no workspace owns rather than storing one nothing draws", () => {
    remember({ path: ["nowhere", "x"], label: "X", workspace: "" }, true);
    expect(stored()).toEqual([]);
  });
});

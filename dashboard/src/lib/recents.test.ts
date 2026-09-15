import { beforeEach, describe, expect, it, vi } from "vitest";
import { MaxRecents, forgetAll, remember, resetForTest } from "./recents.ts";

/** Read the list the way the hook's snapshot does, without rendering. */
function stored(): { path: string[]; label: string }[] {
  const raw = localStorage.getItem("crewlet_recents");
  return raw ? (JSON.parse(raw) as { path: string[]; label: string }[]) : [];
}

beforeEach(() => {
  localStorage.clear();
  resetForTest();
});

describe("what the reader opened", () => {
  it("puts the newest first", () => {
    remember({ path: ["work", "ENG-1"], label: "ENG-1", workspace: "work" });
    remember({ path: ["knowledge", "ENG", "Runbook"], label: "Runbook", workspace: "knowledge" });
    expect(stored().map((r) => r.label)).toEqual(["Runbook", "ENG-1"]);
  });

  it("moves a revisit to the top rather than storing it twice", () => {
    // Keyed on the PATH: without that, a reader who keeps returning to one
    // item fills the whole list with it.
    remember({ path: ["work", "ENG-1"], label: "ENG-1", workspace: "work" });
    remember({ path: ["work", "ENG-2"], label: "ENG-2", workspace: "work" });
    remember({ path: ["work", "ENG-1"], label: "ENG-1", workspace: "work" });
    expect(stored().map((r) => r.label)).toEqual(["ENG-1", "ENG-2"]);
  });

  it("takes the latest label for a path", () => {
    // A page's title lands a render after its route, so the first label for a
    // path is often the raw identifier and the second is the name the reader
    // recognises.
    remember({ path: ["work", "ENG-1"], label: "ENG-1", workspace: "work" });
    remember({ path: ["work", "ENG-1"], label: "Drop the Pulsar backend", workspace: "work" });
    expect(stored().map((r) => r.label)).toEqual(["Drop the Pulsar backend"]);
  });

  it("keeps only the newest few", () => {
    for (let i = 0; i < MaxRecents + 5; i++) {
      remember({ path: ["work", `ENG-${i}`], label: `ENG-${i}`, workspace: "work" });
    }
    const got = stored();
    expect(got).toHaveLength(MaxRecents);
    expect(got[0]?.label).toBe(`ENG-${MaxRecents + 4}`);
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

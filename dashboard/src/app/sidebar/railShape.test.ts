/**
 * The sidebar's shape is the reader's choice, kept for them in this browser:
 * each reader their own, the full sidebar for anybody who has not chosen, and
 * anything storage holds but the one word that folds it read as the full
 * sidebar, since storage is untrusted input.
 */

import { act, renderHook } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, test } from "vitest";

import { noteReader } from "~/lib/reader.ts";
import { STORAGE_KEYS } from "~/lib/storage.ts";
import { setRailShape, toggleRailShape, useRailShape } from "./railShape.ts";

const kept = (reader: string) => localStorage.getItem(`${STORAGE_KEYS.railShape}${reader}`);

beforeEach(() => {
  localStorage.clear();
  sessionStorage.clear();
});

afterEach(() => {
  // The store is the tab's: hand the next case the shape every reader starts from.
  setRailShape("full");
  localStorage.clear();
  sessionStorage.clear();
});

describe("the sidebar's shape", () => {
  test("is the full sidebar until the reader folds it, and theirs once they do", () => {
    noteReader("p-1");
    const { result } = renderHook(() => useRailShape());
    expect(result.current).toBe("full");
    act(() => toggleRailShape());
    expect(result.current).toBe("compact");
    expect(kept("p-1")).toBe(JSON.stringify("compact"));
    act(() => toggleRailShape());
    expect(result.current).toBe("full");
    expect(kept("p-1")).toBe(JSON.stringify("full"));
  });

  test("is each reader's own at a shared browser", () => {
    noteReader("p-1");
    const { result } = renderHook(() => useRailShape());
    act(() => setRailShape("compact"));
    act(() => noteReader("p-2"));
    expect(result.current).toBe("full");
    expect(kept("p-1")).toBe(JSON.stringify("compact"));
    expect(kept("p-2")).toBeNull();
  });

  test("reads anything storage holds but the folding word as the full sidebar", () => {
    for (const raw of ['"wide"', "1", "{}", "not json"]) {
      localStorage.setItem(`${STORAGE_KEYS.railShape}p-${raw.length}`, raw);
      noteReader(`p-${raw.length}`);
      const { result, unmount } = renderHook(() => useRailShape());
      expect(result.current, raw).toBe("full");
      unmount();
    }
  });
});

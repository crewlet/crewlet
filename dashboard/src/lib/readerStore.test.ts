/**
 * A value kept per reader in this browser: one person's arrangement of a
 * page at one desk, validated every time it is read.
 *
 * ONE BROWSER IS OFTEN SEVERAL PEOPLE, so the value is kept under the reader's
 * own key, and a tab that has not learned who reads it keeps it in memory
 * rather than in a list nobody owns. Storage that refuses keeps the value for
 * the visit; another tab's write is read back.
 */

import { act, renderHook } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, test, vi } from "vitest";
import { noteReader } from "./reader.ts";
import { readerStore } from "./readerStore.ts";
import { STORAGE_KEYS } from "./storage.ts";

const PREFIX = STORAGE_KEYS.homeBoard;

/** A list of strings, the way a caller validates: anything else is dropped. */
const strings = (raw: unknown): string[] =>
  Array.isArray(raw) ? raw.filter((v): v is string => typeof v === "string") : [];

const store = () => readerStore<string[]>(PREFIX, ["default"], strings);

beforeEach(() => {
  localStorage.clear();
  sessionStorage.clear();
});

afterEach(() => {
  vi.restoreAllMocks();
});

describe("whose value it is", () => {
  test("is the tab's reader's, under the registered prefix and their id", () => {
    noteReader("p-1");
    const kept = store();
    kept.write(["projects"]);
    expect(JSON.parse(localStorage.getItem(`${PREFIX}p-1`)!)).toEqual(["projects"]);
    expect(kept.read()).toEqual(["projects"]);
  });

  test("a second reader of the browser keeps their own", () => {
    noteReader("p-1");
    const kept = store();
    kept.write(["projects"]);
    const { result } = renderHook(() => kept.use());
    act(() => noteReader("p-2"));
    expect(result.current).toEqual(["default"]);
    act(() => kept.write(["inbox"]));
    expect(JSON.parse(localStorage.getItem(`${PREFIX}p-1`)!)).toEqual(["projects"]);
    expect(JSON.parse(localStorage.getItem(`${PREFIX}p-2`)!)).toEqual(["inbox"]);
  });

  test("a tab that has not learned its reader keeps the value in memory and stores nothing", () => {
    const kept = store();
    kept.write(["projects"]);
    expect(kept.read()).toEqual(["projects"]);
    expect(localStorage.length).toBe(0);
  });
});

describe("what is read back", () => {
  test("is validated: whatever was stored is handed to the caller's check", () => {
    noteReader("p-1");
    localStorage.setItem(`${PREFIX}p-1`, JSON.stringify(["kept", 7, { a: 1 }, "also"]));
    expect(store().read()).toEqual(["kept", "also"]);
  });

  test("a value that is not JSON is the default", () => {
    noteReader("p-1");
    localStorage.setItem(`${PREFIX}p-1`, "{not json");
    expect(store().read()).toEqual(["default"]);
  });

  test("storage that refuses to be read is the default", () => {
    noteReader("p-1");
    vi.spyOn(Storage.prototype, "getItem").mockImplementation(() => {
      throw new DOMException("blocked", "SecurityError");
    });
    expect(store().read()).toEqual(["default"]);
  });

  test("storage that refuses a write keeps the value for the visit", () => {
    noteReader("p-1");
    const kept = store();
    vi.spyOn(Storage.prototype, "setItem").mockImplementation(() => {
      throw new DOMException("full", "QuotaExceededError");
    });
    expect(() => kept.write(["projects"])).not.toThrow();
    expect(kept.read()).toEqual(["projects"]);
  });

  test("update edits what is there now", () => {
    noteReader("p-1");
    const kept = store();
    kept.write(["a"]);
    kept.update((now) => [...now, "b"]);
    expect(kept.read()).toEqual(["a", "b"]);
  });
});

describe("another tab", () => {
  test("writing this reader's value is read back here", () => {
    noteReader("p-1");
    const kept = store();
    kept.write(["here"]);
    const { result } = renderHook(() => kept.use());
    expect(result.current).toEqual(["here"]);
    act(() => {
      localStorage.setItem(`${PREFIX}p-1`, JSON.stringify(["there"]));
      window.dispatchEvent(new StorageEvent("storage", { key: `${PREFIX}p-1` }));
    });
    expect(result.current).toEqual(["there"]);
  });

  test("writing somebody else's key changes nothing here", () => {
    noteReader("p-1");
    const kept = store();
    kept.write(["here"]);
    const { result } = renderHook(() => kept.use());
    act(() => {
      localStorage.setItem(`${PREFIX}p-1`, JSON.stringify(["unannounced"]));
      window.dispatchEvent(new StorageEvent("storage", { key: `${PREFIX}p-2` }));
    });
    expect(result.current).toEqual(["here"]);
  });
});

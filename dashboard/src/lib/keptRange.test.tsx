/**
 * A screen's range is the reader's choice, kept for them: the screen opens on
 * it however they come back (a reload of a bare address, the sidebar, a new
 * tab), a link that names a range still opens that range, and opening one is
 * not a choice of their own.
 */

import { act, cleanup, renderHook } from "@testing-library/react";
import type { ReactNode } from "react";
import { afterEach, beforeEach, describe, expect, test } from "vitest";

import { Router } from "~/app/router.tsx";
import { noteReader } from "~/lib/reader.ts";
import { STORAGE_KEYS } from "~/lib/storage.ts";
import { useTimeRange, type Offer } from "./range.ts";
import { useKeptRange } from "./keptRange.ts";

const NOW = Date.UTC(2026, 5, 10, 12);
const OFFER: Offer = {
  screen: "kept-test",
  ranges: ["1d", "7d", "30d"],
  custom: true,
  fallback: "7d",
  today: true,
};

const wrapper = ({ children }: { children: ReactNode }) => <Router>{children}</Router>;

/** The screen as a fresh page load of `hash` draws it: a new tree over the same browser. */
function open(hash: string) {
  cleanup();
  history.replaceState(null, "", hash);
  return renderHook(() => useTimeRange(NOW, OFFER), { wrapper });
}

/** The reader opening the screen and choosing `window` on it. */
function choose(window: "30d") {
  const screen = open("#/spend");
  act(() => screen.result.current.set(window));
}

const stored = (reader: string) =>
  JSON.parse(localStorage.getItem(`${STORAGE_KEYS.keptRanges}${reader}`) ?? "{}") as Record<
    string,
    string
  >;

/**
 * A READER OF ITS OWN PER CASE: the store is the tab's and holds what it last
 * read for a reader, so a case that reused one would start from the last
 * case's choice rather than from none.
 */
let reader = "";
let cases = 0;

beforeEach(() => {
  localStorage.clear();
  sessionStorage.clear();
  reader = `p-${++cases}`;
  noteReader(reader);
});

afterEach(() => {
  cleanup();
  localStorage.clear();
  sessionStorage.clear();
  history.replaceState(null, "", "#/");
});

describe("a screen's range, kept for its reader", () => {
  test("a reload of the bare address still shows the range the reader chose", () => {
    const first = open("#/spend");
    expect(first.result.current.window).toBe("7d");
    act(() => first.result.current.set("30d"));
    expect(first.result.current.window).toBe("30d");

    // The sidebar's link, a new tab, the page a sign-in lands on: no range.
    const again = open("#/spend");
    expect(again.result.current.window).toBe("30d");
  });

  test("a choice is written into the address in full, the fallback included", () => {
    const screen = open("#/spend");
    act(() => screen.result.current.set("30d"));
    expect(location.hash).toBe("#/spend?window=30d");
    act(() => screen.result.current.set("7d"));
    expect(location.hash).toBe("#/spend?window=7d");
    expect(stored(reader)).toEqual({ "kept-test": "7d" });
  });

  test("a kept choice is noted into a bare address without a history entry", () => {
    choose("30d");
    const before = history.length;
    open("#/spend");
    expect(location.hash).toBe("#/spend?window=30d");
    expect(history.length).toBe(before);
  });

  test("the sidebar's link to the screen a reader is on is noted again", () => {
    choose("30d");
    const screen = open("#/spend");
    expect(location.hash).toBe("#/spend?window=30d");
    // A link's own navigation: the browser moves the address, then says so.
    act(() => {
      history.pushState(null, "", "#/spend");
      window.dispatchEvent(new HashChangeEvent("hashchange"));
    });
    expect(screen.result.current.window).toBe("30d");
    expect(location.hash).toBe("#/spend?window=30d");
  });

  test("a link that names a range opens it, and does not become the reader's own", () => {
    choose("30d");
    const shared = open("#/spend?window=1d");
    expect(shared.result.current.window).toBe("1d");
    expect(stored(reader)).toEqual({ "kept-test": "30d" });
    expect(open("#/spend").result.current.window).toBe("30d");
  });

  test("today is kept as the day so far, and moves with the clock", () => {
    const screen = open("#/spend");
    act(() => screen.result.current.set({ from: NOW - 3_600_000, to: NOW, today: true }));
    expect(stored(reader)).toEqual({ "kept-test": "today" });
    const again = open("#/spend").result.current.window;
    expect(typeof again === "object" && again.today).toBe(true);
  });

  test("two instants a reader typed are shown and linked, never kept", () => {
    const screen = open("#/spend");
    act(() => screen.result.current.set("30d"));
    act(() => screen.result.current.set({ from: NOW - 7_200_000, to: NOW - 3_600_000 }));
    expect(typeof screen.result.current.window).toBe("object");
    expect(stored(reader)).toEqual({ "kept-test": "30d" });
    expect(open("#/spend").result.current.window).toBe("30d");
  });

  test("is each reader's own at a shared browser", () => {
    choose("30d");
    act(() => noteReader(`${reader}-other`));
    expect(open("#/spend").result.current.window).toBe("7d");
  });

  test("a stored word the screen does not offer is its fallback", () => {
    for (const raw of ['{"kept-test":"90d"}', '{"kept-test":7}', '["30d"]', "not json", '"30d"']) {
      act(() => noteReader(`${reader}-${raw.length}`));
      localStorage.setItem(`${STORAGE_KEYS.keptRanges}${reader}-${raw.length}`, raw);
      expect(open("#/spend").result.current.window, raw).toBe("7d");
      expect(location.hash, raw).toBe("#/spend");
    }
  });

  test("screens keep their own choices under their own names", () => {
    const pick = (screen: string) =>
      renderHook(() => useKeptRange("range", screen, "7d", () => true, "filter"), { wrapper });
    history.replaceState(null, "", "#/");
    const home = pick("a");
    act(() => home.result.current[1]("30d"));
    cleanup();
    history.replaceState(null, "", "#/");
    expect(pick("b").result.current[0]).toBe("7d");
    cleanup();
    history.replaceState(null, "", "#/");
    expect(pick("a").result.current[0]).toBe("30d");
  });
});

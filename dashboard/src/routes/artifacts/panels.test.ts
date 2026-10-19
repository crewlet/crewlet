/**
 * An artifact's panel in its address: which one a link opens, the old tabs'
 * and rail's spellings read as what they became, and a panel's own keys
 * leaving with it.
 */

import { expect, test } from "vitest";
import { panelOf, spelledTheOldWay, withPanel } from "./panels.ts";

const q = (s: string) => new URLSearchParams(s);

test("panel= names the open panel; anything else names none", () => {
  expect(panelOf(q("panel=history"), false)).toBe("history");
  expect(panelOf(q("panel=nonsense"), false)).toBeNull();
  expect(panelOf(q(""), false)).toBeNull();
});

test("Data is a panel only where the artifact keeps data", () => {
  expect(panelOf(q("panel=data"), false)).toBeNull();
  expect(panelOf(q("panel=data"), true)).toBe("data");
});

test("an old tab or the people rail opens the panel it became, and View opens none", () => {
  expect(panelOf(q("tab=history"), false)).toBe("history");
  expect(panelOf(q("tab=settings"), false)).toBe("settings");
  expect(panelOf(q("tab=data"), true)).toBe("data");
  expect(panelOf(q("tab=view"), false)).toBeNull();
  expect(panelOf(q("rail=people"), false)).toBe("people");
  expect(spelledTheOldWay(q("tab=view&version=2"))).toBe(true);
  expect(spelledTheOldWay(q("panel=history"))).toBe(false);
});

test("opening a panel keeps the reader's place and drops the old spelling", () => {
  expect(withPanel(q("version=2&tab=history"), "history").toString()).toBe(
    "version=2&panel=history",
  );
});

test("leaving a panel takes its own keys with it; staying on it keeps them", () => {
  const comparing = q("panel=history&against=1&lens=source&expand=1&version=2");
  expect(Object.fromEntries(withPanel(comparing, "details"))).toEqual({
    panel: "details",
    version: "2",
  });
  expect(Object.fromEntries(withPanel(comparing, null))).toEqual({ version: "2" });
  expect(withPanel(comparing, "history").get("against")).toBe("1");
});

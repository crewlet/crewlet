/**
 * An artifact's panel in its address: which one a link opens, the old tabs'
 * and rail's spellings read as what they became, and a panel's own keys
 * leaving with it.
 */

import { expect, test } from "vitest";
import { panelOf, spelledTheOldWay, withPanel } from "./panels.ts";

const q = (s: string) => new URLSearchParams(s);
const NONE = { data: false, files: false };
const DATA = { data: true, files: false };

test("panel= names the open panel; anything else names none", () => {
  expect(panelOf(q("panel=history"), NONE)).toBe("history");
  expect(panelOf(q("panel=nonsense"), NONE)).toBeNull();
  expect(panelOf(q(""), NONE)).toBeNull();
});

test("Data is a panel only where the artifact keeps data", () => {
  expect(panelOf(q("panel=data"), NONE)).toBeNull();
  expect(panelOf(q("panel=data"), DATA)).toBe("data");
});

// Mutation: drop the Files check in `panelOf` and a link to a doc's Files
// opens a panel whose store its page can never show.
test("Files is a panel only where the artifact runs a page of its own", () => {
  expect(panelOf(q("panel=files"), NONE)).toBeNull();
  expect(panelOf(q("panel=files"), { data: false, files: true })).toBe("files");
});

test("an old tab or the people rail opens the panel it became, and View opens none", () => {
  expect(panelOf(q("tab=history"), NONE)).toBe("history");
  expect(panelOf(q("tab=settings"), NONE)).toBe("settings");
  expect(panelOf(q("tab=data"), DATA)).toBe("data");
  expect(panelOf(q("tab=view"), NONE)).toBeNull();
  expect(panelOf(q("rail=people"), NONE)).toBe("people");
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

test("Comments is a panel of every artifact, and its thread and filter leave with it", () => {
  expect(panelOf(q("panel=comments"), NONE)).toBe("comments");
  const reading = q("panel=comments&thread=t1&filter=resolved&version=2");
  expect(Object.fromEntries(withPanel(reading, "history"))).toEqual({
    panel: "history",
    version: "2",
  });
  expect(withPanel(reading, "comments").get("thread")).toBe("t1");
});

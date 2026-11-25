/**
 * The directory files each integration by what a company does with it, and
 * draws only what this build serves: the growth products are filed ahead of
 * their catalogue entries, so a line whose app is not served is drawn nowhere
 * and a group left empty is not drawn at all.
 */

import { expect, test } from "vitest";
import { GROWTH_PRODUCTS } from "~/components/growth/words.ts";
import { CATALOG, type Entry } from "./catalogue.tsx";
import { DIRECTORY, SHOWN, shownDirectory } from "./categories.ts";

/** A catalogue that also serves `keys`, each as a copy of a served entry under its own key. */
function serving(...keys: string[]): Entry[] {
  const base = CATALOG.find((e) => e.key === "app_store_connect")!;
  return [
    ...CATALOG,
    ...keys.filter((key) => !CATALOG.some((e) => e.key === key)).map((key) => ({ ...base, key })),
  ];
}

const keysOf = (groups: ReturnType<typeof shownDirectory>) =>
  groups.map((g) => [g.label, g.lines.map((l) => l.key)] as const);

test("with every growth product served, each sits in the group of what it does", () => {
  const shown = keysOf(shownDirectory(serving(...GROWTH_PRODUCTS)));
  expect(shown.slice(-4)).toEqual([
    ["Mobile apps", ["expo", "app_store_connect", "google_play"]],
    ["Advertising", ["google_ads", "meta_ads", "apple_ads"]],
    ["Analytics", ["google_analytics", "search_console"]],
    ["Ad revenue", ["adsense", "admob"]],
  ]);
});

test("a line whose app is not served is drawn nowhere, and a group left empty is not drawn", () => {
  const unserved = ["google_ads", "meta_ads", "apple_ads", "google_play"];
  const shown = keysOf(shownDirectory(CATALOG.filter((e) => !unserved.includes(e.key))));
  const labels = shown.map(([label]) => label);
  expect(labels).toContain("Analytics");
  expect(labels).not.toContain("Advertising");
  expect(shown.find(([label]) => label === "Analytics")![1]).toEqual([
    "google_analytics",
    "search_console",
  ]);
  expect(shown.find(([label]) => label === "Ad revenue")![1]).toEqual(["adsense", "admob"]);
  expect(shown.find(([label]) => label === "Mobile apps")![1]).not.toContain("google_play");
});

test("this build draws the table over its own catalogue, every drawn app line served", () => {
  expect(SHOWN).toEqual(shownDirectory(CATALOG));
  for (const group of SHOWN) {
    expect(group.lines.length, group.label).toBeGreaterThan(0);
    for (const line of group.lines) {
      if (line.kind === "service") continue;
      const app =
        line.kind === "product" ? line.app : line.kind === "google" ? "google_cloud" : line.key;
      expect(
        CATALOG.some((e) => e.key === app),
        line.key,
      ).toBe(true);
    }
  }
});

test("only a growth product is filed ahead of its catalogue entry: any other unserved line is a typo", () => {
  for (const group of DIRECTORY) {
    for (const line of group.lines) {
      if (line.kind === "service") continue;
      const app =
        line.kind === "product" ? line.app : line.kind === "google" ? "google_cloud" : line.key;
      if (CATALOG.some((e) => e.key === app)) continue;
      expect(GROWTH_PRODUCTS.has(app), `${group.label}: ${app}`).toBe(true);
    }
  }
});

/**
 * How projects are ordered against a query: the switcher's order and ⌘K's.
 */

import { expect, test } from "vitest";
import { projectRank, rankProjects } from "./projectRank.ts";

const projects = [
  { key: "SPLIT", name: "Split engine" },
  { key: "SETL", name: "Settle up" },
  { key: "RCPT", name: "Receipt scanning" },
  { key: "FAIR", name: "Product and Engineering" },
  { key: "S", name: "Support" },
  { key: "APP", name: "Mobile app" },
];

test("an exact key, then a key's start, then a word's start, then anywhere", () => {
  expect(projectRank({ key: "S", name: "Support" }, "s")).toBe(0);
  expect(projectRank({ key: "SPLIT", name: "Split engine" }, "s")).toBe(1);
  expect(projectRank({ key: "RCPT", name: "Receipt scanning" }, "s")).toBe(2);
  expect(projectRank({ key: "APP", name: "Mobile app" }, "p")).toBe(3);
  expect(projectRank({ key: "FAIR", name: "Product and Engineering" }, "gin")).toBe(4);
  expect(projectRank({ key: "FAIR", name: "Product and Engineering" }, "zz")).toBe(-1);
});

test("case does not matter, and ties go by key", () => {
  expect(rankProjects(projects, "S").map((p) => p.key)).toEqual(["S", "SETL", "SPLIT", "RCPT"]);
  expect(rankProjects(projects, " s ").map((p) => p.key)).toEqual(["S", "SETL", "SPLIT", "RCPT"]);
});

test("an empty query keeps the order the projects came in", () => {
  expect(rankProjects(projects, "").map((p) => p.key)).toEqual(projects.map((p) => p.key));
});

// A ROW THE ENGINE MATCHED ON ITS PURPOSE is kept, last, rather than dropped
// for a reason the reader cannot see.
test("an engine-matched row that matches nothing here is kept, after the rest", () => {
  const found = [
    { key: "OPS", name: "Operations" },
    { key: "BILL", name: "Billing" },
  ];
  expect(rankProjects(found, "invoice").map((p) => p.key)).toEqual([]);
  expect(rankProjects(found, "bill", { matched: true }).map((p) => p.key)).toEqual(["BILL", "OPS"]);
});

test("a word starts after a space, a dash, an underscore, a slash or a middle dot", () => {
  for (const name of [
    "Core platform",
    "Core-platform",
    "core_platform",
    "Core/platform",
    "Core·platform",
  ]) {
    expect(projectRank({ key: "ENG", name }, "plat"), name).toBe(2);
  }
});

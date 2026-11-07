import { expect, test } from "vitest";
import {
  branchesFrom,
  branchesOf,
  closedOf,
  openingDepth,
  rankWidths,
  readView,
  tagOf,
  writeView,
} from "./chartPlace.ts";

test("a chart's branches are the nodes that have children", () => {
  const nodes = [
    {
      id: "company",
      label: "Kestrel",
      children: [
        { id: "core", label: "Core", children: [{ id: "ana", label: "Ana" }] },
        { id: "robin", label: "Robin" },
      ],
    },
  ];
  expect(branchesOf(nodes)).toEqual(["company", "core"]);
  expect([...closedOf("core,,growth")]).toEqual(["core", "growth"]);
});

// A SAVED VIEW IS PUT BACK ONLY ON THE CHART IT WAS TAKEN ON: tagged with what
// the chart is (its arrangement, legend key and closed branches), so the
// teams' zoom is never laid over the reporting lines.
//
// Mutation: drop the tag check and this fails.
test("a view is read back only for the chart it was written for", () => {
  const tag = tagOf("structure|all|");
  const value = writeView({ k: 0.4567, x: -12.34, y: 8 }, tag);
  expect(readView(value, tag)).toEqual({ k: 0.4567, x: -12.3, y: 8 });
  expect(readView(value, tagOf("reporting|all|"))).toBeUndefined();
  expect(readView("", tag)).toBeUndefined();
  expect(readView("abc,1,2," + tag, tag)).toBeUndefined();
});

const FOREST = [
  {
    id: "company",
    label: "Kestrel",
    children: [
      { id: "robin", label: "Robin" },
      {
        id: "core",
        label: "Core",
        children: [
          { id: "ana", label: "Ana", children: [{ id: "ivy", label: "Ivy" }] },
          { id: "bo", label: "Bo" },
          { id: "cy", label: "Cy" },
          { id: "di", label: "Di" },
        ],
      },
      { id: "growth", label: "Growth", children: [{ id: "eve", label: "Eve" }] },
    ],
  },
];

// A CHART OPENS ON THE DEEPEST RANK IT CAN BE READ AT. Its whole fit scales by
// how much narrower the ranks it shows are than its widest, and a chart whose
// whole fit can be read opens whole. Rank 0 is the floor. Mutation: start the
// walk at rank 0, or ignore the ratio.
test("the opening is the deepest rank that can be read", () => {
  expect(rankWidths(FOREST)).toEqual([1, 3, 5, 1]);
  expect(openingDepth([1, 3, 5, 1], 0.5, 0.5)).toBeNull();
  // 5 wide at 0.4: 3 wide is 0.67, and 1 wide would be 2.
  expect(openingDepth([1, 3, 5, 1], 0.4, 0.5)).toBe(1);
  // 14 wide at 0.27 and at 0.08: three cards read at 1.26, and not at 0.37.
  expect(openingDepth([1, 3, 14], 0.27, 0.5)).toBe(1);
  expect(openingDepth([1, 3, 14], 0.08, 0.5)).toBe(0);
  // A narrow deep chain keeps every rank it can: the widest is the third.
  expect(openingDepth([1, 2, 8, 8, 3], 0.2, 0.5)).toBe(1);
});

// THE BRANCHES CLOSED ARE THOSE AT THE LAST RANK DRAWN AND BELOW, but for the
// way to the selected node. Mutation: close what is above the rank, or the
// selected node's own ancestors.
test("the opening closes the branches from its rank down, and keeps the selected path", () => {
  expect(branchesFrom(FOREST, 1, null)).toEqual(["ana", "core", "growth"]);
  expect(branchesFrom(FOREST, 2, null)).toEqual(["ana"]);
  expect(branchesFrom(FOREST, 0, null)).toEqual(["ana", "core", "growth", "company"]);
  expect(branchesFrom(FOREST, 1, "eve")).toEqual(["ana", "core"]);
  expect(branchesFrom(FOREST, 0, "ivy")).toEqual(["growth"]);
});

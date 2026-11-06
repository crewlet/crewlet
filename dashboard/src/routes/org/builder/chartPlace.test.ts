import { expect, test } from "vitest";
import { branchesOf, closedOf, readView, tagOf, writeView } from "./chartPlace.ts";

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

// A SAVED VIEW IS PUT BACK ONLY ON THE CHART IT WAS TAKEN ON: tagged with the
// chart's fit key, so the teams' zoom is never laid over the reporting lines,
// nor over a layout that has been refitted since.
//
// Mutation: drop the tag check and this fails.
test("a view is read back only for the chart it was written for", () => {
  const tag = tagOf("structure:0|all|1|");
  const value = writeView({ k: 0.4567, x: -12.34, y: 8 }, tag);
  expect(readView(value, tag)).toEqual({ k: 0.4567, x: -12.3, y: 8 });
  expect(readView(value, tagOf("reporting:0|all|1|"))).toBeUndefined();
  expect(readView("", tag)).toBeUndefined();
  expect(readView("abc,1,2," + tag, tag)).toBeUndefined();
});

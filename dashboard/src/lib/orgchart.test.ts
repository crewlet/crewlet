import { expect, test } from "vitest";
import { projectsByUnit, unitPath } from "./orgchart.ts";
import { indexOrg } from "./seats.ts";
import type { WorkProjectRow } from "~/protocol/types.ts";
import { CHART_ORG } from "~/test/orgchart.ts";

const PROJECTS = [
  { key: "ENG", unit: { key: "core", name: "Core", resolved: true } },
  { key: "OLD", unit: { key: "core", name: "Core", resolved: true }, archived: true },
  { key: "PROD", unit: { key: "management", name: "Management", resolved: true } },
] as unknown as WorkProjectRow[];

test("each unit's live projects are listed by the unit's key", () => {
  expect([...projectsByUnit(PROJECTS)]).toEqual([
    ["core", ["ENG"]],
    ["management", ["PROD"]],
  ]);
});

// BY KEY, NEVER BY NAME: two units may share a name, and keyed on it each
// drew the other's projects.
test("two units sharing a name keep their own projects", () => {
  const twins = [
    { key: "A", unit: { key: "platform", name: "Platform", resolved: true } },
    { key: "B", unit: { key: "platform-2", name: "Platform", resolved: true } },
  ] as unknown as WorkProjectRow[];
  expect([...projectsByUnit(twins)]).toEqual([
    ["platform", ["A"]],
    ["platform-2", ["B"]],
  ]);
});

test("a unit's path is its chain of names, and a seat above every unit has none", () => {
  const index = indexOrg(CHART_ORG);
  const nested = index.units.find((u) => u.chain.length > 1);
  expect(nested).toBeDefined();
  expect(unitPath(nested)).toBe(nested!.chain.map((u) => u.name).join(" · "));
  expect(unitPath(null)).toBe("");
});

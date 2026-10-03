/**
 * The live org chart's model: its tree, its unit boxes and its legend, off
 * the APPLIED projection alone.
 */

import { readFileSync } from "node:fs";
import { join } from "node:path";
import { describe, expect, test } from "vitest";
import type { TreeInput } from "@crewlethq/ui";
import { balanceRoots, buildOrgChart, placeLine, stateCounts, unitPath } from "./orgchart.ts";
import { indexOrg } from "./seats.ts";
import type { AgentRow, OrgProjection, WorkProjectRow } from "~/protocol/types.ts";
import { CHART_ORG } from "~/test/orgchart.ts";

const PROJECTS = [
  { key: "ENG", unit: { name: "Core", resolved: true } },
  { key: "OLD", unit: { name: "Core", resolved: true }, archived: true },
  { key: "PROD", unit: { name: "Management", resolved: true } },
] as unknown as WorkProjectRow[];

/** The tree as `parent > child` lines, for a readable equality. */
function edges(nodes: readonly TreeInput[], parent = ""): string[] {
  return nodes.flatMap((n) => [`${parent}>${n.id}`, ...edges(n.children ?? [], n.id)]);
}

// THE TREE IS WHO REPORTS TO WHOM, off the engine's derived primary manager.
test("each card hangs under its primary manager", () => {
  const chart = buildOrgChart(indexOrg(CHART_ORG), PROJECTS);
  expect(edges(chart.nodes)).toEqual([
    ">jane",
    "jane>ceo",
    "ceo>cto",
    "cto>swe",
    "cto>fe",
    "ceo>pm",
    "pm>devrel",
  ]);
});

// A BOX IS A UNIT UNDER THE LEAD IT REPORTS TO: Core under the CTO, who
// leads it from Executives, and Developer Relations under the PM, who leads it
// by inheritance from Management. The CTO beside the CEO in their own unit is
// NOT boxed — peers in the lead's own unit are a line, not a team.
test("the seats of a unit its lead leads from outside are boxed, with the unit's project", () => {
  const chart = buildOrgChart(indexOrg(CHART_ORG), PROJECTS);
  expect(chart.groups.map((g) => [g.label, g.memberIds, g.projectKeys])).toEqual([
    ["Engineering · Core", ["swe", "fe"], ["ENG"]],
    // The box drops "Product", which the PM's own card already says.
    ["Developer Relations", ["devrel"], []],
  ]);
});

// AN ENGINE THAT REPORTS NO HIERARCHY leaves nobody reporting to anybody:
// every seat is a root, and no unit has a lead to box it under. The chart
// draws that rather than a hierarchy this client made up.
test("with no derived hierarchy every seat is a root and nothing is boxed", () => {
  const { derived: _drop, ...flat } = CHART_ORG as OrgProjection & { derived: unknown };
  const chart = buildOrgChart(indexOrg(flat as OrgProjection), PROJECTS);
  expect(chart.nodes.every((n) => !n.children)).toBe(true);
  expect(chart.nodes).toHaveLength(7);
  expect(chart.groups).toEqual([]);
});

// THE LEGEND IS THE ENGINE'S WORD, COUNTED — and a seat with no row yet is
// counted nowhere, because "no state yet" is not idle. A human is never
// counted: the engine runs no human seat.
test("the legend counts each agent seat by its activity and nothing else", () => {
  const index = indexOrg(CHART_ORG);
  const agents = [
    { role: "CEO", activity: "idle" },
    { role: "CTO", activity: "working" },
    { role: "SWE", activity: "working" },
    { role: "FE", activity: "needs" },
    { role: "PM", activity: "stopped" },
    { role: "Jane Founder", activity: "working" },
    { role: "Gone", activity: "working" },
  ] as unknown as AgentRow[];
  expect(stateCounts(index, agents)).toEqual({ working: 2, needs: 1, stopped: 1, idle: 1 });
});

test("a seat's place is its unit path, or above every unit", () => {
  const index = indexOrg(CHART_ORG);
  expect(placeLine(index.byHandle.get("swe")!)).toBe("Engineering · Core");
  expect(placeLine(index.byHandle.get("jane")!)).toBe("Above every unit");
  expect(unitPath(null)).toBe("");
});

// THE CHART DRAWS WHAT IS RUNNING, NEVER A DRAFT. The builder's model draws a
// document somebody is editing; a chart that shared it could draw a seat
// nobody saved as if it ran. The boundary is an import this module never makes.
test("the live chart never reads the builder's draft model", () => {
  for (const file of ["src/lib/orgchart.ts", "src/routes/agents/OrgChart.tsx"]) {
    const source = readFileSync(join(process.cwd(), file), "utf8");
    // THE IMPORTS, not the prose: the doc comment names the module it keeps
    // away from. The one builder file the chart does import is the note that
    // says the chart is a revision behind a save.
    const imports = [...source.matchAll(/from\s+["']([^"']+)["']/g)].map((m) => m[1]!);
    expect(imports.length, file).toBeGreaterThan(0);
    for (const spec of imports) {
      expect(spec, file).not.toMatch(/chartModel/);
      expect(spec, file).not.toMatch(/builder\/(?!AfterSaveStrip)/);
    }
  }
});

// A PERSON WHO LEADS NOBODY AND REPORTS TO NOBODY is a root with no tree, and
// the canvas packs such a card right beside the root it follows, on the top
// row alone. After a founder whose tree already leans right (a CTO with three
// reports beside a PM with one puts the CEO over the right half) it pushed the
// top of the chart further right still: centred on its box, the chart read as
// sitting right of centre. The lone root goes to whichever side centres the
// top row over the drawing.
describe("a root that leads nobody is placed to centre the top row", () => {
  const leaf = (id: string): TreeInput => ({ id, label: id });
  const node = (id: string, ...children: TreeInput[]): TreeInput => ({ id, label: id, children });
  // CEO over CTO (three reports) and PM (one): the tree's root sits right of
  // its middle.
  const leansRight = node(
    "jane",
    node("ceo", node("cto", leaf("swe"), leaf("fe"), leaf("ai")), node("pm", leaf("devrel"))),
  );

  test("after a tree that leans right, it is drawn to the left of the root", () => {
    expect(balanceRoots([leansRight, leaf("maya")]).map((n) => n.id)).toEqual(["maya", "jane"]);
  });

  test("after a tree that leans left, it stays on the right", () => {
    const leansLeft = node(
      "jane",
      node("ceo", node("pm", leaf("devrel")), node("cto", leaf("swe"), leaf("fe"), leaf("ai"))),
    );
    expect(balanceRoots([leansLeft, leaf("maya")]).map((n) => n.id)).toEqual(["jane", "maya"]);
  });

  test("a tie keeps the order the document wrote", () => {
    const even = node("jane", node("a", leaf("a1")), node("b", leaf("b1")));
    expect(balanceRoots([even, leaf("maya")]).map((n) => n.id)).toEqual(["jane", "maya"]);
    expect(balanceRoots([leaf("maya"), even]).map((n) => n.id)).toEqual(["maya", "jane"]);
  });

  test("two lone roots flank a tree that leans right", () => {
    expect(balanceRoots([leansRight, leaf("maya"), leaf("rui")]).map((n) => n.id)).toEqual([
      "maya",
      "jane",
      "rui",
    ]);
  });

  test("a chart with no tree, or no lone root, is left as it is", () => {
    const flat = [leaf("a"), leaf("b"), leaf("c")];
    expect(balanceRoots(flat)).toEqual(flat);
    expect(balanceRoots([leansRight])).toEqual([leansRight]);
  });

  test("the chart builder applies it to the company's roots", () => {
    const org = {
      ...CHART_ORG,
      roles: [
        ...(CHART_ORG as unknown as { roles: unknown[] }).roles,
        { name: "Maya Ops", handle: "maya", kind: "human" },
      ],
      derived: {
        ...(CHART_ORG as unknown as { derived: { seats: unknown[] } }).derived,
        seats: [
          ...(CHART_ORG as unknown as { derived: { seats: unknown[] } }).derived.seats,
          {
            handle: "maya",
            name: "Maya Ops",
            kind: "human",
            manager: "",
            managers: [],
            reports: [],
            auto_reports: [],
            placed_by_ref: false,
          },
        ],
      },
    } as unknown as OrgProjection;
    // CHART_ORG's CEO has a CTO with two reports and a PM with one, so its
    // tree leans right and Maya is drawn on the founder's left.
    expect(buildOrgChart(indexOrg(org), PROJECTS).nodes.map((n) => n.id)).toEqual(["maya", "jane"]);
  });
});

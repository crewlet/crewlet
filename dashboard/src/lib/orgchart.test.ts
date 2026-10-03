/**
 * The live org chart's model: its tree, its unit boxes and its legend, off
 * the APPLIED projection alone.
 */

import { readFileSync } from "node:fs";
import { join } from "node:path";
import { describe, expect, test } from "vitest";
import type { TreeInput } from "@crewlethq/ui";
import {
  balanceRoots,
  buildOrgChart,
  placeLine,
  projectsByUnit,
  stateCounts,
  unitOfRef,
  unitPath,
} from "./orgchart.ts";
import { indexOrg } from "./seats.ts";
import type { AgentRow, OrgProjection, WorkProjectRow } from "~/protocol/types.ts";
import { CHART_ORG } from "~/test/orgchart.ts";

// AS THE ENGINE ANSWERS THEM: the key the row was filed under, and the
// chart's name for the unit it resolves to. CHART_ORG declares no ids, so a
// unit's key there is its name.
const PROJECTS = [
  { key: "ENG", unit: { key: "Core", name: "Core", resolved: true } },
  { key: "OLD", unit: { key: "Core", name: "Core", resolved: true }, archived: true },
  { key: "PROD", unit: { key: "Management", name: "Management", resolved: true } },
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
    { role: "CEO", handle: "ceo", activity: "idle" },
    { role: "CTO", handle: "cto", activity: "working" },
    { role: "SWE", handle: "swe", activity: "working" },
    { role: "FE", handle: "fe", activity: "needs" },
    { role: "PM", handle: "pm", activity: "stopped" },
    { role: "Jane Founder", handle: "jane", activity: "working" },
    { role: "Gone", handle: "gone", activity: "working" },
  ] as unknown as AgentRow[];
  expect(stateCounts(index, agents)).toEqual({ working: 2, needs: 1, stopped: 1, idle: 1 });
});

// EACH SEAT IS COUNTED BY ITS OWN ROW. Paired by name, two seats sharing one
// were both counted in whichever row came last — here both as stopped.
//
// Mutation: pair by `a.role` / `seat.name` again, and the working one is lost.
test("two seats sharing a name are each counted by their own row", () => {
  const derived = (handle: string) => ({
    handle,
    name: "Engineer",
    kind: "agent",
    manager: "",
    managers: [],
    reports: [],
    auto_reports: [],
    placed_by_ref: false,
  });
  const org = {
    name: "Acme",
    roles: [
      { name: "Engineer", handle: "eng-a" },
      { name: "Engineer", handle: "eng-b" },
    ],
    units: [],
    derived: { seats: [derived("eng-a"), derived("eng-b")], units: [] },
  } as unknown as OrgProjection;
  const agents = [
    { role: "Engineer", handle: "eng-a", activity: "working" },
    { role: "Engineer", handle: "eng-b", activity: "stopped" },
  ] as unknown as AgentRow[];
  expect(stateCounts(indexOrg(org), agents)).toEqual({
    working: 1,
    needs: 0,
    stopped: 1,
    idle: 0,
  });
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

// ---------------------------------------------------------------------------
// Which unit a project is filed to
// ---------------------------------------------------------------------------

/**
 * Two teams called Platform — one keyed `platform`, one `infra` — and an
 * Operations team renamed from the key `ops` (its origin) through `sre`.
 * The projection carries every unit's CURRENT key as `id` and the rest on the
 * derived block, exactly as the engine publishes them.
 */
const KEYED = {
  name: "Acme",
  roles: [],
  units: [
    { id: "platform", name: "Platform", roles: [{ name: "Web" }] },
    { id: "infra", name: "Platform", roles: [{ name: "Metal" }] },
    { id: "operations", name: "Operations", roles: [{ name: "Oncall" }] },
  ],
  derived: {
    seats: [
      { handle: "web", name: "Web", kind: "agent" },
      { handle: "metal", name: "Metal", kind: "agent" },
      { handle: "oncall", name: "Oncall", kind: "agent" },
    ],
    units: [
      { id: "platform", name: "Platform", seats: ["web"] },
      { id: "infra", name: "Platform", seats: ["metal"] },
      {
        id: "operations",
        name: "Operations",
        seats: ["oncall"],
        origin_key: "ops",
        former_keys: ["sre"],
      },
    ],
  },
} as unknown as OrgProjection;

const ref = (key: string, name: string, resolved = true) => ({ key, name, resolved });

// TWO UNITS SHARING A NAME ARE TWO UNITS. The projects were keyed on the
// unit's name, so both Platform boxes drew both teams' projects.
//
// Mutation: key the map on `unit.name` again, and the two entries merge.
test("two units sharing a name each carry only their own projects", () => {
  const index = indexOrg(KEYED);
  const [platform, infra] = index.units;
  const keys = projectsByUnit(
    [
      { key: "WEB", unit: ref("platform", "Platform") },
      { key: "MTL", unit: ref("infra", "Platform") },
    ] as unknown as WorkProjectRow[],
    index,
  );
  expect(keys.get(platform!)).toEqual(["WEB"]);
  expect(keys.get(infra!)).toEqual(["MTL"]);
  // AND A NAME TWO UNITS CARRY NAMES NEITHER: a row the engine resolved by
  // a key this projection does not hold is not guessed onto either Platform.
  expect(unitOfRef(index, ref("platform-old", "Platform"))).toBeNull();
});

// A RENAMED UNIT KEEPS THE PROJECTS FILED BEFORE ITS RENAME. The row holds
// the key the unit had when it was filed — its origin, or one it answered to
// since — and only the CURRENT key is the projection's `id`, so a lookup on
// `id` alone drew none of a renamed team's work.
//
// Mutation: resolve the ref's key against `unit.id` only, and the two older
// projects fall out of the box.
test("a renamed unit carries the projects filed under every key it answered to", () => {
  const index = indexOrg(KEYED);
  const operations = index.units[2]!;
  const keys = projectsByUnit(
    [
      { key: "NOW", unit: ref("operations", "Operations") },
      { key: "FIRST", unit: ref("ops", "Operations") },
      { key: "THEN", unit: ref("sre", "Operations") },
    ] as unknown as WorkProjectRow[],
    index,
  );
  expect(keys.get(operations)).toEqual(["FIRST", "NOW", "THEN"]);
});

// A ROW FILED UNDER A UNIT'S NAME — before the chart gave it a key — is the
// unit's when that name is one unit's, which the engine says by answering it
// resolved; and a reference the chart could not resolve is nobody's.
test("a row under a unit's name is that unit's, and an unresolved one is nobody's", () => {
  const index = indexOrg(KEYED);
  expect(unitOfRef(index, ref("Ops Team", "Operations"))).toBe(index.units[2]);
  expect(unitOfRef(index, ref("gone", "", false))).toBeNull();
  expect(unitOfRef(index, ref("", "Operations", false))).toBeNull();
});

/**
 * The live org chart, as data: the company in either of the two arrangements
 * the org builder draws (`ui/orgNodes.tsx` `ChartKind`), and how many seats
 * are in each state.
 *
 * # Pure, and fed the APPLIED company
 *
 * Its inputs are the org index built over the projection this node has
 * APPLIED (`lib/seats.ts` `indexOrg`) and the `agents` push, never the
 * builder's draft. The builder (`routes/org/builder/chartModel.ts`) draws a
 * document somebody is editing, and a chart that shared its model would draw a
 * seat nobody has saved as if it were running; `orgchart.test.ts` holds that
 * as a boundary this module never imports across. What the two charts SHARE
 * is the drawing of a node, which is not a fact about either model.
 *
 * # The structure is the company, its units and the seats in each
 *
 * The company is the one root. Its root seats and its top units hang off it,
 * and each unit's seats and child units hang off that unit, seats first, as
 * the builder's structure chart draws a draft. A unit's seats are the ones the
 * engine placed in it, a root seat placed by its `unit:` reference included.
 *
 * # The reporting chart is who reports to whom
 *
 * A seat hangs under its PRIMARY MANAGER, which is the engine's derived
 * `manager`, the one line the engine routes escalation up. Every seat has at
 * most one, so the chart is a forest rather than a graph, and a seat the
 * engine gave none is a root. Nothing here re-derives a reporting line.
 */

import type { TreeInput } from "@crewlethq/ui";
import type { SeatActivity } from "~/contract/wire.ts";
import type { AgentRow, WorkProjectRow } from "~/protocol/types.ts";
import type { OrgIndex, Seat, Unit } from "./seats.ts";

/**
 * The structure chart's root, the company. Never a seat's key, which is a
 * handle or `#n`, and never a unit's (`unitNodeId`).
 */
export const COMPANY_NODE = "company:";

/** What the structure chart's root is called when the company writes no name. */
export const UNNAMED_COMPANY = "Unnamed company";

/** A unit's node: never a seat's key, which is a handle or `#n`. */
export function unitNodeId(unit: Pick<Unit, "key">): string {
  return `unit:${unit.key}`;
}

export interface OrgChartModel {
  /** The nodes as the tree pattern reads them. */
  nodes: TreeInput[];
  /** A seat node's seat, by the node's id (the seat's key). */
  seats: Map<string, Seat>;
  /** A unit node's unit, by the node's id. Empty on the reporting chart. */
  units: Map<string, Unit>;
}

/** A unit's path as a reader names it: "Engineering · Core". */
export function unitPath(unit: Unit | null | undefined): string {
  return unit ? unit.chain.map((u) => u.name).join(" · ") : "";
}

/**
 * The line under a seat's name on its card: where it sits. A seat above every
 * unit says so, rather than leaving a blank line that reads as a unit nobody
 * named.
 */
export function placeLine(seat: Seat): string {
  return unitPath(seat.unit) || "Above every unit";
}

/**
 * Every project filed to each unit, by unit NAME — the address a unit has
 * everywhere on the anonymous surface (`lib/seats.ts` `Unit.name`).
 */
export function projectsByUnit(
  projects: readonly Pick<WorkProjectRow, "key" | "unit" | "archived">[],
): Map<string, string[]> {
  const out = new Map<string, string[]>();
  for (const p of projects) {
    // AN ARCHIVED PROJECT IS NOT WHERE A UNIT'S WORK GOES: its key on the
    // unit's node would send a reader to a board nobody files to.
    if (p.archived || !p.unit?.name) continue;
    out.set(p.unit.name, [...(out.get(p.unit.name) ?? []), p.key]);
  }
  for (const keys of out.values()) keys.sort();
  return out;
}

/**
 * The structure chart: the company, its root seats and units, and each unit's
 * seats and child units under it.
 */
export function buildStructureChart(index: OrgIndex, company: string): OrgChartModel {
  const seats = new Map<string, Seat>();
  const units = new Map<string, Unit>();
  const seatNode = (seat: Seat): TreeInput => {
    seats.set(seat.key, seat);
    return { id: seat.key, label: seat.name };
  };
  const unitNode = (unit: Unit): TreeInput => {
    const id = unitNodeId(unit);
    units.set(id, unit);
    const children = [...unit.seats.map(seatNode), ...unit.children.map(unitNode)];
    return { id, label: unit.name, ...(children.length ? { children } : {}) };
  };
  const children = [...index.rootSeats.map(seatNode), ...index.topUnits.map(unitNode)];
  const root: TreeInput = {
    id: COMPANY_NODE,
    label: company.trim() || UNNAMED_COMPANY,
    ...(children.length ? { children } : {}),
  };
  return { nodes: [root], seats, units };
}

/**
 * The reporting chart: every seat under its primary manager.
 *
 * Children are ordered by the unit they sit in (depth first, as the document
 * writes units) and then by the chart's own seat order, so the members of one
 * team read side by side under the lead they report to.
 */
export function buildReportingChart(index: OrgIndex): OrgChartModel {
  const seats = new Map(index.seats.map((s) => [s.key, s]));
  const unitOrder = new Map(index.units.map((u, i) => [u, i]));
  const seatOrder = new Map(index.seats.map((s, i) => [s, i]));
  // A MANAGER CHAIN THAT LOOPS cannot be a tree. The engine's derivation
  // cannot produce one, but a chart that trusted that would recurse for ever
  // on a projection that did; such a seat is drawn as a root instead.
  const parentOf = (seat: Seat): Seat | null => {
    const seen = new Set<Seat>([seat]);
    for (let at = seat.manager; at; at = at.manager) {
      if (seen.has(at)) return null;
      seen.add(at);
    }
    return seat.manager && seats.has(seat.manager.key) ? seat.manager : null;
  };
  const children = new Map<Seat | null, Seat[]>();
  for (const seat of index.seats) {
    const parent = parentOf(seat);
    children.set(parent, [...(children.get(parent) ?? []), seat]);
  }
  const rank = (s: Seat): [number, number] => [
    s.unit ? (unitOrder.get(s.unit) ?? -1) : -1,
    seatOrder.get(s) ?? 0,
  ];
  for (const list of children.values()) {
    list.sort((a, b) => {
      const [ua, sa] = rank(a);
      const [ub, sb] = rank(b);
      return ua - ub || sa - sb;
    });
  }
  const build = (seat: Seat): TreeInput => {
    const kids = children.get(seat) ?? [];
    return {
      id: seat.key,
      label: seat.name,
      ...(kids.length ? { children: kids.map(build) } : {}),
    };
  };
  const nodes = balanceRoots((children.get(null) ?? []).map(build));
  return { nodes, seats, units: new Map() };
}

/**
 * The roots in the order that keeps the chart's top row over the middle of
 * the tree it stands on.
 *
 * # Why a root's place is decided at all
 *
 * A seat that reports to nobody and leads nobody — a person "above every
 * unit" with nobody under them — is a root with no tree. The canvas lays the
 * roots out left to right and packs each one as close to its neighbour as the
 * rows they share allow, so such a card sits right beside the root it follows,
 * on the TOP row only. In document order it followed the founder, and a tree
 * whose root already sat right of its own middle (a CTO with three reports
 * beside a PM with one puts the CEO over the right half) grew a second card
 * further right still: the chart was centred on its bounding box and read as
 * pushed to the right, with the top-left of the canvas empty.
 *
 * # What decides the side
 *
 * The TREES keep their order — they are the chart — and the lone roots are
 * split between the two ends of the top row: however many to the left of the
 * first tree's root and the rest to the right of the last one, choosing the
 * split whose top row is centred closest to the middle of the whole drawing.
 * Positions are reckoned the way a tidy tree places cards of one width: a leaf
 * one slot along from the last, a parent midway between its first and last
 * child. That is not the canvas's exact arithmetic, since nodes are as wide
 * as their names, but it is the same shape, which is all the side depends on.
 * A tie keeps the document's order.
 */
export function balanceRoots(nodes: readonly TreeInput[]): TreeInput[] {
  const trees = nodes.filter((n) => n.children?.length);
  const lone = nodes.filter((n) => !n.children?.length);
  if (!trees.length || !lone.length) return [...nodes];

  // WHERE EACH TREE'S ROOT FALLS, in leaf slots, trees laid side by side.
  let slot = 0;
  const place = (n: TreeInput): number => {
    const kids = n.children ?? [];
    if (!kids.length) return slot++;
    const xs = kids.map(place);
    return (xs[0]! + xs[xs.length - 1]!) / 2;
  };
  const rootXs = trees.map(place);
  const firstRoot = rootXs[0]!;
  const lastRoot = rootXs[rootXs.length - 1]!;
  const lastLeaf = slot - 1;

  const offCentre = (left: number): number => {
    const rowStart = firstRoot - left;
    const rowEnd = lastRoot + (lone.length - left);
    const drawnStart = Math.min(0, rowStart);
    const drawnEnd = Math.max(lastLeaf, rowEnd);
    return Math.abs((rowStart + rowEnd) / 2 - (drawnStart + drawnEnd) / 2);
  };
  // THE DOCUMENT'S OWN SPLIT is where to start: the lone roots it wrote ahead
  // of the first tree go left, and a card moves off that side only for a row
  // STRICTLY closer to the middle.
  const first = nodes.findIndex((n) => n.children?.length);
  let best = nodes.slice(0, first).length;
  let bestOff = offCentre(best);
  for (let left = 0; left <= lone.length; left++) {
    const off = offCentre(left);
    if (off < bestOff - 1e-9) {
      best = left;
      bestOff = off;
    }
  }
  return [...lone.slice(0, best), ...trees, ...lone.slice(best)];
}

/** How many seats are in each of the four states the legend names. */
export type StateCounts = Record<SeatActivity, number>;

/**
 * The legend's four figures: every agent seat of the chart, counted by the
 * ENGINE's word for it. A seat with no row yet is counted nowhere — the
 * legend says what the engine reported, and "no state yet" is not idle.
 */
export function stateCounts(index: OrgIndex, agents: readonly AgentRow[]): StateCounts {
  const counts: StateCounts = { working: 0, needs: 0, stopped: 0, idle: 0 };
  const byRole = new Map(agents.map((a) => [a.role, a]));
  for (const seat of index.seats) {
    if (seat.kind === "human") continue;
    const activity = byRole.get(seat.name)?.activity;
    if (activity && activity in counts) counts[activity] += 1;
  }
  return counts;
}

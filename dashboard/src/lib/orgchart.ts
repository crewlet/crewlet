/**
 * The live org chart, as data: which seat hangs under which, which runs of
 * seats sit in one unit's box, and how many seats are in each state.
 *
 * # Pure, and fed the APPLIED company
 *
 * Its inputs are the org index built over the projection this node has
 * APPLIED (`lib/seats.ts` `indexOrg`), the `agents` push and the tracker's
 * `work_projects` rows — never the builder's draft. The builder
 * (`routes/org/builder/chartModel.ts`) draws a document somebody is editing,
 * and a chart that shared its model would draw a seat nobody has saved as if
 * it were running; `orgchart.test.ts` holds that as a boundary this module
 * never imports across.
 *
 * # The tree is who reports to whom
 *
 * A card hangs under its PRIMARY MANAGER, which is the engine's derived
 * `manager` — the one line the engine routes escalation up. Every seat has at
 * most one, so the chart is a forest rather than a graph, and a seat the
 * engine gave none is a root. Nothing here re-derives a reporting line: an
 * engine that sends no derived hierarchy leaves every seat a root, and the
 * screen says why (`OrgIndex.hierarchy`).
 *
 * # A box is a unit under the lead it reports to
 *
 * The lines say who reports to whom; they cannot say which seats WORK
 * TOGETHER — two teams under one lead are two runs of children with nothing
 * between them. So the children of a card that are members of a unit that
 * card LEADS from outside it are drawn in one box, labelled with the unit.
 * A lead's own unit gets no box round the seats beside the lead: the CEO and
 * the CTO sharing Executives are peers on a line, and a box round the CTO
 * alone would say the CTO is a team.
 */

import type { TreeInput } from "@crewlethq/ui";
import type { SeatActivity } from "~/contract/wire.ts";
import type { AgentRow, WorkProjectRow, WorkUnitRef } from "~/protocol/types.ts";
import { unitByKey, type OrgIndex, type Seat, type Unit } from "./seats.ts";

/** One box round a run of sibling cards: a unit, under its lead. */
export interface ChartGroup {
  /** Never a card's id: a seat key is a handle or `#n`, and this is `unit:`. */
  id: string;
  unit: Unit;
  /** The unit's path BELOW what the lead's own card already says. */
  label: string;
  /** The tracker projects this unit's work is filed under, by key. */
  projectKeys: string[];
  /** The cards inside, consecutive children of one card, in chart order. */
  memberIds: string[];
}

export interface OrgChartModel {
  /** Every seat as the tree pattern reads it, nested by primary manager. */
  nodes: TreeInput[];
  groups: ChartGroup[];
  /** A card's seat, by the card's id (the seat's key). */
  seats: Map<string, Seat>;
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
 * The unit a tracker row's unit reference names, as this chart holds it — or
 * null where the chart cannot say which unit it is.
 *
 * THE ROW HOLDS WHAT WAS WRITTEN, not what the chart calls the unit today
 * (`tracker.UnitRef`): a project filed before a rename keeps the key its unit
 * had then, so `ref.key` is the unit's current key, the key it was created
 * under or one it answered to since — and only the first of those is the
 * projection's `id`. So the key is resolved the engine's way, through every
 * spelling a unit answers to ([unitByKey]: the current key, then the origin,
 * then each former key), and never compared with `id` alone.
 *
 * AND BY THE CHART'S OWN NAME ONLY WHEN IT IS ONE UNIT'S. The engine resolves
 * a row filed under a unit's name (before the chart gave it a key) and answers
 * that unit's current name beside the stored key, so a resolved reference
 * whose key this projection does not carry is matched on that name — but a
 * name is prose two units may share, and a name two units carry names
 * neither: it was keyed on the name, and two teams called Platform drew each
 * other's projects in their boxes.
 */
export function unitOfRef(
  index: Pick<OrgIndex, "units">,
  ref: WorkUnitRef | null | undefined,
): Unit | null {
  if (!ref) return null;
  const key = (ref.key ?? "").trim();
  const byKey = key ? unitByKey(index, key) : null;
  if (byKey) return byKey;
  if (!ref.resolved || !ref.name) return null;
  const named = index.units.filter((u) => u.name === ref.name);
  return named.length === 1 ? named[0]! : null;
}

/**
 * Every live project filed to each unit, keyed on the UNIT — the very
 * objects of the index it is handed, so a caller looks a unit up with one of
 * that index's units and two units sharing a name are two entries.
 *
 * It was keyed on the unit's NAME, so two teams of one name pooled their
 * projects, and a team renamed since its projects were filed drew none.
 */
export function projectsByUnit(
  projects: readonly Pick<WorkProjectRow, "key" | "unit" | "archived">[],
  index: Pick<OrgIndex, "units">,
): Map<Unit, string[]> {
  const out = new Map<Unit, string[]>();
  for (const p of projects) {
    // AN ARCHIVED PROJECT IS NOT WHERE A UNIT'S WORK GOES: its key on the box
    // would send a reader to a board nobody files to.
    if (p.archived) continue;
    const unit = unitOfRef(index, p.unit);
    if (!unit) continue;
    out.set(unit, [...(out.get(unit) ?? []), p.key]);
  }
  for (const keys of out.values()) keys.sort();
  return out;
}

/**
 * Build the chart.
 *
 * Children are ordered so a unit's members are CONSECUTIVE — a box encloses a
 * run of siblings, and a unit whose members were interleaved with another's
 * could not be drawn as one — by the unit's place in the tree (depth first,
 * as the document writes units) and then by the chart's own seat order.
 */
export function buildOrgChart(
  index: OrgIndex,
  projects: readonly Pick<WorkProjectRow, "key" | "unit" | "archived">[] = [],
): OrgChartModel {
  const seats = new Map(index.seats.map((s) => [s.key, s]));
  const unitOrder = new Map(index.units.map((u, i) => [u, i]));
  const seatOrder = new Map(index.seats.map((s, i) => [s, i]));
  const keysOf = projectsByUnit(projects, index);

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

  const groups: ChartGroup[] = [];
  const build = (seat: Seat): TreeInput => {
    const kids = children.get(seat) ?? [];
    // THE RUNS OF CHILDREN IN A UNIT THIS SEAT LEADS FROM OUTSIDE IT.
    let run: { unit: Unit; ids: string[] } | null = null;
    const close = () => {
      if (!run) return;
      groups.push({
        id: `unit:${run.unit.key}:${seat.key}`,
        unit: run.unit,
        label: pathBelow(run.unit, seat.unit),
        projectKeys: keysOf.get(run.unit) ?? [],
        memberIds: run.ids,
      });
      run = null;
    };
    for (const kid of kids) {
      const boxed = kid.unit && kid.unit.effectiveLead === seat && seat.unit !== kid.unit;
      if (boxed && run?.unit === kid.unit) {
        run.ids.push(kid.key);
        continue;
      }
      close();
      if (boxed) run = { unit: kid.unit!, ids: [kid.key] };
    }
    close();
    return {
      id: seat.key,
      label: seat.name,
      ...(kids.length ? { children: kids.map(build) } : {}),
    };
  };
  const nodes = balanceRoots((children.get(null) ?? []).map(build));
  return { nodes, groups, seats };
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
 * child. That is not the canvas's exact arithmetic — a unit's box adds its
 * padding — but it is the same shape, which is all the side depends on. A tie
 * keeps the document's order.
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

/**
 * A unit's path below the part the lead's card already names.
 *
 * "Engineering · Core" under a CTO who sits in Leadership · Executives says
 * everything; "Product · Developer Relations" under a PM who sits in
 * Product · Management says Product twice, once on the card and once on the
 * box under it, so the box drops what the two share.
 */
function pathBelow(unit: Unit, leadUnit: Unit | null): string {
  const lead = leadUnit?.chain ?? [];
  let shared = 0;
  while (
    shared < lead.length &&
    shared < unit.chain.length - 1 &&
    lead[shared] === unit.chain[shared]
  ) {
    shared++;
  }
  return unit.chain
    .slice(shared)
    .map((u) => u.name)
    .join(" · ");
}

/** How many seats are in each of the four states the legend names. */
export type StateCounts = Record<SeatActivity, number>;

/**
 * The legend's four figures: every agent seat of the chart, counted by the
 * ENGINE's word for it. A seat with no row yet is counted nowhere — the
 * legend says what the engine reported, and "no state yet" is not idle.
 *
 * EACH SEAT BY ITS OWN ROW, paired by HANDLE ([liveRowFor]'s rule): it was
 * paired by NAME, which two seats may share, so two "Engineer"s were each
 * counted in the state of whichever row came last.
 */
export function stateCounts(index: OrgIndex, agents: readonly AgentRow[]): StateCounts {
  const counts: StateCounts = { working: 0, needs: 0, stopped: 0, idle: 0 };
  const byHandle = new Map(agents.flatMap((a) => (a.handle ? [[a.handle, a] as const] : [])));
  for (const seat of index.seats) {
    if (seat.kind === "human" || !seat.handle) continue;
    const activity = byHandle.get(seat.handle)?.activity;
    if (activity && activity in counts) counts[activity] += 1;
  }
  return counts;
}

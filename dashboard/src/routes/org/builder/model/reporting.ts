/**
 * The reporting chart: who each seat reports to, as a forest.
 *
 * THE EDGES ARE THE ENGINE'S. A seat hangs under its PRIMARY manager, the
 * `manager` the engine derived (the first seat in its own seat order that
 * lists it, after unit references were expanded and leads were given their
 * members). Nothing here decides who manages whom; it only arranges what the
 * derivation already says. That is also why this chart and the structure
 * chart can disagree: a unit lead manages that unit's direct members, and
 * nobody manages a child unit's lead unless a `manages` entry says so.
 *
 * THREE KINDS OF TOP LEVEL, and every seat appears exactly once:
 *
 * - A seat with no manager is a root, drawn under the label "No manager".
 * - A seat whose manager names no seat of this derivation is a root as well,
 *   because there is nobody to hang it under.
 * - A seat that no root reaches sits in a management CYCLE, or under one: A
 *   manages B and B manages A is valid configuration, every member of it has a
 *   manager, and so none of them is a root. Walking only from roots would drop
 *   the whole cycle from the chart without a word, which hides exactly the
 *   configuration a reporting chart exists to show. Those seats go under a
 *   "Reporting cycle" group, each cycle broken at its member that comes first
 *   in the engine's seat order, and each member is marked with the cycle it
 *   belongs to.
 *
 * Seats are identified by their position in the derivation, not by handle:
 * a stored revision from before handles had to be unique can list one handle
 * twice, and two seats must still be two nodes. A manager handle held by
 * several seats resolves to the first, as a name does everywhere else in the
 * engine.
 */

import type { Derived, DerivedSeat } from "~/protocol/index.ts";

/** One seat in the reporting forest. */
export interface ReportingNode {
  /** Position in `derived.seats`. */
  readonly index: number;
  readonly seat: DerivedSeat;
  /** The cycle this seat is a member of, as positions in `derived.seats`; absent when none. */
  readonly cycle?: readonly number[];
  readonly reports: readonly ReportingNode[];
}

export interface ReportingForest {
  /** Seats with no manager (or a manager that is no seat), in engine order, with everyone below them. */
  readonly roots: readonly ReportingNode[];
  /** The "Reporting cycle" group: one entry per cycle, broken at its first member in engine order. */
  readonly cycles: readonly ReportingNode[];
}

/** Arranges a derivation's primary-manager edges into the reporting forest. */
export function reportingForest(derived: Derived | null): ReportingForest {
  const seats = derived?.seats ?? [];
  const byHandle = new Map<string, number>();
  seats.forEach((seat, i) => {
    if (seat.handle && !byHandle.has(seat.handle)) byHandle.set(seat.handle, i);
  });
  const managerOf = (i: number): number | undefined => {
    const handle = seats[i]!.manager;
    return handle ? byHandle.get(handle) : undefined;
  };

  // Children in engine order, so a manager's reports are drawn in the order
  // the engine lists seats.
  const children = seats.map((): number[] => []);
  seats.forEach((_, i) => {
    const m = managerOf(i);
    if (m !== undefined && m !== i) children[m]!.push(i);
  });

  const placed = new Set<number>();
  const cycleOf = new Map<number, readonly number[]>();
  const build = (i: number): ReportingNode => {
    placed.add(i);
    const reports = children[i]!.filter((c) => !placed.has(c)).map(build);
    const cycle = cycleOf.get(i);
    return { index: i, seat: seats[i]!, ...(cycle ? { cycle } : {}), reports };
  };

  const roots: ReportingNode[] = [];
  seats.forEach((_, i) => {
    const m = managerOf(i);
    // A seat listing itself is its own manager and nobody else's report: a
    // cycle of one, handled with the other cycles below.
    if (m === undefined) roots.push(build(i));
  });

  const cycles: ReportingNode[] = [];
  for (let start = 0; start < seats.length; start++) {
    if (placed.has(start)) continue;
    // Every unplaced seat leads, by manager edges, into a cycle: the chain
    // cannot reach a root (it would have been placed from it) and cannot end
    // (a seat with no manager is a root). Walk it until a seat repeats.
    const order: number[] = [];
    const seen = new Map<number, number>();
    let at: number | undefined = start;
    while (at !== undefined && !seen.has(at)) {
      seen.set(at, order.length);
      order.push(at);
      at = managerOf(at);
    }
    const members = order.slice(seen.get(at!)!).sort((a, b) => a - b);
    for (const member of members) cycleOf.set(member, members);
    const breakAt = members[0]!;
    cycles.push(build(breakAt));
  }

  return { roots, cycles };
}

/** Every seat position the forest holds, depth-first, for a caller that walks it. */
export function* walkForest(forest: ReportingForest): Generator<ReportingNode> {
  function* walk(node: ReportingNode): Generator<ReportingNode> {
    yield node;
    for (const report of node.reports) yield* walk(report);
  }
  for (const root of forest.roots) yield* walk(root);
  for (const cycle of forest.cycles) yield* walk(cycle);
}

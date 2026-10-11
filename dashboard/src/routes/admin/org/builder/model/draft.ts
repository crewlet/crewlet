/**
 * The draft: the company document as a tree of keyed nodes.
 *
 * THE SAME SHAPE AS THE DOCUMENT, with a key beside each entity. Root seats,
 * units, a unit's seats and its child units sit in exactly the lists and the
 * order the document holds them, so turning a draft back into a document is a
 * walk rather than a reconstruction, and the authored path of every node is
 * the path the engine will report a problem at. Where the ENGINE moves a node
 * (a root seat placed in a unit by its `unit:` reference) is not modelled
 * here: that is a derivation, and the builder reads it from the engine's
 * `derived` block rather than computing it again.
 *
 * AN ENTITY'S DATA IS ITS AUTHORED JSON, WHOLE. A seat's `data` is the role
 * object as `GET /config` served it, every key included; a unit's `data` is
 * the unit object without its `roles` and `children`, which the tree holds as
 * nodes instead. The company's `data` is the document without `roles` and
 * `units`. Nothing is typed away, so a field this build does not model
 * survives every edit.
 *
 * Every function here is pure and returns a new draft that shares every branch
 * it did not change.
 */

import type { CompanyDocument, ConfigRole, ConfigUnit } from "~/protocol/index.ts";
import { COMPANY_KEY, type NodeKey } from "./keys.ts";

/** One seat: its key and its authored role object. */
export interface DraftSeat {
  readonly key: NodeKey;
  readonly data: ConfigRole;
}

/**
 * One unit: its key, its authored unit object WITHOUT `roles` and `children`,
 * and those two lists as nodes.
 */
export interface DraftUnit {
  readonly key: NodeKey;
  readonly data: ConfigUnit;
  readonly roles: readonly DraftSeat[];
  readonly children: readonly DraftUnit[];
}

/** The whole draft. `company` is the document without `roles` and `units`. */
export interface Draft {
  readonly company: CompanyDocument;
  readonly roles: readonly DraftSeat[];
  readonly units: readonly DraftUnit[];
}

/** A draft with no charter, seats or units: the start of create mode. */
export const EMPTY_DRAFT: Draft = { company: {}, roles: [], units: [] };

/**
 * Where a node sits: under which parent (a unit, or [COMPANY_KEY] for the
 * root) and directly after which sibling of its own kind (`null` for first).
 *
 * NEVER AN INDEX. An index means something only in the list it was counted
 * in, and the list an operation replays into after a rebase can have gained
 * or lost a sibling before the node. A neighbour's identity either still
 * stands beside the slot or visibly does not.
 */
export interface Placement {
  readonly parent: NodeKey;
  readonly after: NodeKey | null;
}

/** A node found in the tree, with where it sits. */
export type Located =
  | {
      readonly kind: "seat";
      readonly node: DraftSeat;
      readonly parent: NodeKey;
      readonly index: number;
      readonly siblings: readonly DraftSeat[];
    }
  | {
      readonly kind: "unit";
      readonly node: DraftUnit;
      readonly parent: NodeKey;
      readonly index: number;
      readonly siblings: readonly DraftUnit[];
    };

/** Finds a node by key anywhere in the draft. */
export function locate(draft: Draft, key: NodeKey): Located | undefined {
  const inSeats = (seats: readonly DraftSeat[], parent: NodeKey): Located | undefined => {
    const index = seats.findIndex((s) => s.key === key);
    return index < 0
      ? undefined
      : { kind: "seat", node: seats[index]!, parent, index, siblings: seats };
  };
  const inUnits = (units: readonly DraftUnit[], parent: NodeKey): Located | undefined => {
    const index = units.findIndex((u) => u.key === key);
    if (index >= 0) return { kind: "unit", node: units[index]!, parent, index, siblings: units };
    for (const unit of units) {
      const found = inSeats(unit.roles, unit.key) ?? inUnits(unit.children, unit.key);
      if (found) return found;
    }
    return undefined;
  };
  return inSeats(draft.roles, COMPANY_KEY) ?? inUnits(draft.units, COMPANY_KEY);
}

/** Every unit, depth-first, parents before children: the document's own order. */
export function* allUnits(draft: Draft): Generator<{ unit: DraftUnit; parent: NodeKey }> {
  function* walk(
    units: readonly DraftUnit[],
    parent: NodeKey,
  ): Generator<{ unit: DraftUnit; parent: NodeKey }> {
    for (const unit of units) {
      yield { unit, parent };
      yield* walk(unit.children, unit.key);
    }
  }
  yield* walk(draft.units, COMPANY_KEY);
}

/**
 * Every seat, in the document's own walk order (root seats, then each unit's
 * seats before its children), with the key of the list that holds it.
 */
export function* allSeats(draft: Draft): Generator<{ seat: DraftSeat; parent: NodeKey }> {
  for (const seat of draft.roles) yield { seat, parent: COMPANY_KEY };
  for (const { unit } of allUnits(draft)) {
    for (const seat of unit.roles) yield { seat, parent: unit.key };
  }
}

/** Every key a unit's subtree holds: the unit, its descendants and every seat among them. */
export function subtreeKeys(unit: DraftUnit): NodeKey[] {
  const out: NodeKey[] = [unit.key];
  for (const seat of unit.roles) out.push(seat.key);
  for (const child of unit.children) out.push(...subtreeKeys(child));
  return out;
}

/** Whether `key` is `ancestor` or sits anywhere inside its subtree. */
export function isWithin(draft: Draft, key: NodeKey, ancestor: NodeKey): boolean {
  if (key === ancestor) return true;
  const found = locate(draft, ancestor);
  return found?.kind === "unit" ? subtreeKeys(found.node).includes(key) : false;
}

/** Every key in the draft, company included. */
export function allKeys(draft: Draft): Set<NodeKey> {
  const out = new Set<NodeKey>([COMPANY_KEY]);
  for (const { seat } of allSeats(draft)) out.add(seat.key);
  for (const { unit } of allUnits(draft)) out.add(unit.key);
  return out;
}

/** A copy of the draft with one seat's data replaced. Unchanged when the key names no seat. */
export function updateSeatData(
  draft: Draft,
  key: NodeKey,
  update: (data: ConfigRole) => ConfigRole,
): Draft {
  const seats = (list: readonly DraftSeat[]): readonly DraftSeat[] => {
    const index = list.findIndex((s) => s.key === key);
    if (index < 0) return list;
    const seat = list[index]!;
    const data = update(seat.data);
    if (data === seat.data) return list;
    const next = [...list];
    next[index] = { key: seat.key, data };
    return next;
  };
  const roles = seats(draft.roles);
  if (roles !== draft.roles) return { ...draft, roles };
  const units = mapUnits(draft.units, (unit) => {
    const nextRoles = seats(unit.roles);
    return nextRoles === unit.roles ? unit : { ...unit, roles: nextRoles };
  });
  return units === draft.units ? draft : { ...draft, units };
}

/** A copy of the draft with one unit's own data replaced. Unchanged when the key names no unit. */
export function updateUnitData(
  draft: Draft,
  key: NodeKey,
  update: (data: ConfigUnit) => ConfigUnit,
): Draft {
  const units = mapUnits(draft.units, (unit) => {
    if (unit.key !== key) return unit;
    const data = update(unit.data);
    return data === unit.data ? unit : { ...unit, data };
  });
  return units === draft.units ? draft : { ...draft, units };
}

/** A copy of the draft with `update` applied to every seat's data. */
export function mapSeatData(
  draft: Draft,
  update: (data: ConfigRole, key: NodeKey) => ConfigRole,
): Draft {
  const seats = (list: readonly DraftSeat[]): readonly DraftSeat[] => {
    let changed = false;
    const next = list.map((seat) => {
      const data = update(seat.data, seat.key);
      if (data === seat.data) return seat;
      changed = true;
      return { key: seat.key, data };
    });
    return changed ? next : list;
  };
  const roles = seats(draft.roles);
  const units = mapUnits(draft.units, (unit) => {
    const nextRoles = seats(unit.roles);
    return nextRoles === unit.roles ? unit : { ...unit, roles: nextRoles };
  });
  return roles === draft.roles && units === draft.units ? draft : { ...draft, roles, units };
}

/** A copy of the draft with `update` applied to every unit's own data. */
export function mapUnitData(
  draft: Draft,
  update: (data: ConfigUnit, key: NodeKey) => ConfigUnit,
): Draft {
  const units = mapUnits(draft.units, (unit) => {
    const data = update(unit.data, unit.key);
    return data === unit.data ? unit : { ...unit, data };
  });
  return units === draft.units ? draft : { ...draft, units };
}

/**
 * Maps every unit bottom-up, children first, returning the SAME array when no
 * unit changed so an untouched branch is shared rather than copied.
 */
function mapUnits(
  units: readonly DraftUnit[],
  fn: (unit: DraftUnit) => DraftUnit,
): readonly DraftUnit[] {
  let changed = false;
  const next = units.map((unit) => {
    const children = mapUnits(unit.children, fn);
    const withChildren = children === unit.children ? unit : { ...unit, children };
    const mapped = fn(withChildren);
    if (mapped !== unit) changed = true;
    return mapped;
  });
  return changed ? next : units;
}

/** A copy of the draft without the node, and the node that was removed. */
export function detach(draft: Draft, key: NodeKey): { draft: Draft; node: Located } | undefined {
  const found = locate(draft, key);
  if (!found) return undefined;
  const without = <T extends { key: NodeKey }>(list: readonly T[]) =>
    list.filter((n) => n.key !== key);
  if (found.parent === COMPANY_KEY) {
    return {
      draft:
        found.kind === "seat"
          ? { ...draft, roles: without(draft.roles) }
          : { ...draft, units: without(draft.units) },
      node: found,
    };
  }
  const units = mapUnits(draft.units, (unit) => {
    if (unit.key !== found.parent) return unit;
    return found.kind === "seat"
      ? { ...unit, roles: without(unit.roles) }
      : { ...unit, children: without(unit.children) };
  });
  return { draft: { ...draft, units }, node: found };
}

/**
 * A copy of the draft with a node inserted at a placement. The caller has
 * checked that the parent exists and the neighbour stands in it; an absent
 * neighbour here is a programming error, not a replay outcome.
 */
export function attach(
  draft: Draft,
  placement: Placement,
  node: DraftSeat | DraftUnit,
  kind: "seat" | "unit",
): Draft {
  const insert = <T extends { key: NodeKey }>(list: readonly T[], item: T): T[] => {
    const at = placement.after === null ? 0 : list.findIndex((n) => n.key === placement.after) + 1;
    if (at === 0 && placement.after !== null) {
      throw new RangeError(`attach: ${placement.after} is not in ${placement.parent}`);
    }
    const next = [...list];
    next.splice(at, 0, item);
    return next;
  };
  if (placement.parent === COMPANY_KEY) {
    return kind === "seat"
      ? { ...draft, roles: insert(draft.roles, node as DraftSeat) }
      : { ...draft, units: insert(draft.units, node as DraftUnit) };
  }
  let found = false;
  const units = mapUnits(draft.units, (unit) => {
    if (unit.key !== placement.parent) return unit;
    found = true;
    return kind === "seat"
      ? { ...unit, roles: insert(unit.roles, node as DraftSeat) }
      : { ...unit, children: insert(unit.children, node as DraftUnit) };
  });
  if (!found) throw new RangeError(`attach: no unit ${placement.parent}`);
  return { ...draft, units };
}

/**
 * The siblings a node of `kind` placed under `parent` would sit among, or
 * `undefined` when the parent is not a unit of this draft (or the company).
 */
export function siblingsAt(
  draft: Draft,
  parent: NodeKey,
  kind: "seat" | "unit",
): readonly { key: NodeKey }[] | undefined {
  if (parent === COMPANY_KEY) return kind === "seat" ? draft.roles : draft.units;
  const found = locate(draft, parent);
  if (found?.kind !== "unit") return undefined;
  return kind === "seat" ? found.node.roles : found.node.children;
}

/** Where a located node sits, as a [Placement]. */
export function placementOf(found: Located): Placement {
  return {
    parent: found.parent,
    after: found.index === 0 ? null : found.siblings[found.index - 1]!.key,
  };
}

/** Every seat name in the draft, in walk order, repeats included. */
export function seatNames(draft: Draft): string[] {
  return [...allSeats(draft)].map(({ seat }) => seat.data.name);
}

/** Every unit name in the draft, depth-first, repeats included. */
export function unitNames(draft: Draft): string[] {
  return [...allUnits(draft)].map(({ unit }) => unit.data.name);
}

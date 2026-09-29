/**
 * The draft: the company's org chart as a tree of keyed nodes, beside its
 * settings document.
 *
 * THE CHART'S OWN SHAPE, with a key beside each node. A seat's `data` is the
 * seat as `GET /chart` served it and a unit's is the unit, minus the two
 * things the TREE holds instead: where a node sits (its unit, its parent) is
 * the list it is in, so a move is a detach and an attach rather than a field
 * edit. Everything else is the engine's JSON verbatim — prose, the relations
 * authority is derived from, the structural `kind`, `lead` and `manages`,
 * and the opaque RUNTIME half under `runtime` — so a key this build does not
 * model survives every edit.
 *
 * EVERY REFERENCE IS AN ADDRESS. A unit's `lead` is a seat's handle and a
 * `manages:` entry a seat's handle or a unit's key, exactly as the chart
 * stores them; a name is display only, and two seats may share one.
 *
 * `company` is the SETTINGS document — the revision `GET /config` serves,
 * which carries no seats and no units. The builder edits its charter and the
 * two integration values a seat edit reaches (the Datadog fallback, the GitLab
 * access levels), and nothing else in it.
 *
 * Every function here is pure and returns a new draft that shares every branch
 * it did not change.
 */

import type { CompanyDocument, SeatRuntime, UnitRuntime } from "~/protocol/index.ts";
import { jsonEqual } from "./json.ts";
import { COMPANY_KEY, type NodeKey } from "./keys.ts";

/** One seat's data: the chart's seat, less the placement the tree holds. */
export interface SeatData {
  /**
   * The seat's address. A new one on a seat the chart already holds is the
   * chart's RENAME: the seat keeps its identity, and the old handle goes on
   * resolving to it until something else takes it.
   */
  handle: string;
  /** What holds the seat. Absent is an agent, as the engine reads it. */
  kind?: string;
  name: string;
  email?: string;
  goal?: string;
  backstory?: string;
  responsibilities?: string[];
  behavioral_guidelines?: string[];
  project?: string;
  space?: string;
  /** Seat handles and unit keys, as the chart stores them. */
  manages?: string[];
  /** The runtime half; absent where the seat has none OR this reader was not shown it. */
  runtime?: SeatRuntime;
  [key: string]: unknown;
}

/** One unit's data: the chart's unit, less its parent and what it holds. */
export interface UnitData {
  /** The unit's address. A new one on a unit the chart holds is its rename, as a seat's is. */
  key: string;
  name: string;
  type?: string;
  purpose?: string;
  goals?: string[];
  /** The AUTHORED lead's handle; absent where the unit inherits one. */
  lead?: string;
  channel?: string;
  project?: string;
  space?: string;
  knowledge_refs?: string[];
  runtime?: UnitRuntime;
  [key: string]: unknown;
}

/** One seat: its key and its data. */
export interface DraftSeat {
  readonly key: NodeKey;
  readonly data: SeatData;
}

/** One unit: its key, its own data, and the seats and units it holds as nodes. */
export interface DraftUnit {
  readonly key: NodeKey;
  readonly data: UnitData;
  readonly roles: readonly DraftSeat[];
  readonly children: readonly DraftUnit[];
}

/** The whole draft. `roles` and `units` are the ones at the org root. */
export interface Draft {
  readonly company: CompanyDocument;
  readonly roles: readonly DraftSeat[];
  readonly units: readonly DraftUnit[];
}

/** A draft with no charter, seats or units: the start of create mode. */
export const EMPTY_DRAFT: Draft = { company: {}, roles: [], units: [] };

/**
 * Where a node sits: under which parent (a unit, or [COMPANY_KEY] for the
 * root).
 *
 * A PARENT AND NOTHING ELSE, because the chart keeps nothing else. A unit's
 * seats and its child units are rows, served in ADDRESS order (a seat's handle,
 * a unit's key) and stored in no other: there is no position a save could
 * write and no order a reading could return but that one. So the draft holds
 * every list in address order too ([compareAddress]), and "where a node sits"
 * is only which list it is in. A draft that let somebody arrange siblings would
 * be showing an arrangement the save silently drops.
 */
export interface Placement {
  readonly parent: NodeKey;
}

/**
 * The order the chart serves rows in: by address, compared code point by code
 * point — which is the order of the addresses' UTF-8 bytes, the store's own
 * binary collation. A plain `<` compares UTF-16 code units, which disagrees
 * with the bytes for a character past the surrogate range.
 */
export function compareAddress(a: string, b: string): number {
  const left = [...a];
  const right = [...b];
  for (let i = 0; i < Math.min(left.length, right.length); i++) {
    const x = left[i]!.codePointAt(0)!;
    const y = right[i]!.codePointAt(0)!;
    if (x !== y) return x < y ? -1 : 1;
  }
  return left.length - right.length;
}

/** A node's address: a seat's handle, a unit's key. */
export function addressOf(node: DraftSeat | DraftUnit): string {
  return "roles" in node ? node.data.key : node.data.handle;
}

/** A list in the chart's order. The same array when it already is. */
export function inAddressOrder<T extends DraftSeat | DraftUnit>(list: readonly T[]): readonly T[] {
  for (let i = 1; i < list.length; i++) {
    if (compareAddress(addressOf(list[i - 1]!), addressOf(list[i]!)) > 0) {
      return [...list].sort((x, y) => compareAddress(addressOf(x), addressOf(y)));
    }
  }
  return list;
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

/** Every unit, depth-first, parents before children, each list in the chart's own order. */
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
 * Every seat, in walk order (root seats, then each unit's seats before its
 * children), with the key of the list that holds it.
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

/**
 * Whether two drafts hold the same chart: the same nodes under the same keys,
 * in the same places, with the same data. The settings are not compared.
 *
 * What says a derivation of one still describes the other: the engine's
 * answers about a chart (who inherits which lead, who reports to whom) hold
 * for exactly the rows they were derived from, so a draft that changed any of
 * them is one no derivation has described yet.
 */
export function sameChart(a: Draft, b: Draft): boolean {
  if (a.roles === b.roles && a.units === b.units) return true;
  return jsonEqual(a.roles, b.roles) && jsonEqual(a.units, b.units);
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
  update: (data: SeatData) => SeatData,
): Draft {
  const seats = (list: readonly DraftSeat[]): readonly DraftSeat[] => {
    const index = list.findIndex((s) => s.key === key);
    if (index < 0) return list;
    const seat = list[index]!;
    const data = update(seat.data);
    if (data === seat.data) return list;
    const next = [...list];
    next[index] = { key: seat.key, data };
    // A NEW HANDLE moves the seat among its siblings: the list stays in the
    // order the chart will serve it in.
    return data.handle === seat.data.handle ? next : inAddressOrder(next);
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
  update: (data: UnitData) => UnitData,
): Draft {
  let rekeyed = false;
  const units = mapUnits(draft.units, (unit) => {
    if (unit.key !== key) return unit;
    const data = update(unit.data);
    if (data.key !== unit.data.key) rekeyed = true;
    return data === unit.data ? unit : { ...unit, data };
  });
  if (units === draft.units) return draft;
  // A NEW KEY moves the unit among its siblings: the lists stay in the chart's order.
  return { ...draft, units: rekeyed ? sortUnitLists(units) : units };
}

/** Every list of units in address order, sharing each list that already is. */
function sortUnitLists(units: readonly DraftUnit[]): readonly DraftUnit[] {
  return inAddressOrder(
    mapUnits(units, (unit) => {
      const children = inAddressOrder(unit.children);
      return children === unit.children ? unit : { ...unit, children };
    }),
  );
}

/** A copy of the draft with `update` applied to every seat's data. */
export function mapSeatData(
  draft: Draft,
  update: (data: SeatData, key: NodeKey) => SeatData,
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
  update: (data: UnitData, key: NodeKey) => UnitData,
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
 * A copy of the draft with a node inserted under a parent, where its address
 * puts it among its siblings. The caller has checked that the parent exists;
 * an absent parent here is a programming error, not a replay outcome.
 */
export function attach(
  draft: Draft,
  placement: Placement,
  node: DraftSeat | DraftUnit,
  kind: "seat" | "unit",
): Draft {
  const insert = <T extends DraftSeat | DraftUnit>(list: readonly T[], item: T): readonly T[] => {
    const at = list.findIndex((n) => compareAddress(addressOf(n), addressOf(item)) > 0);
    const next = [...list];
    next.splice(at < 0 ? next.length : at, 0, item);
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
  return { parent: found.parent };
}

/** Every seat name in the draft, in walk order, repeats included. */
export function seatNames(draft: Draft): string[] {
  return [...allSeats(draft)].map(({ seat }) => seat.data.name);
}

/** Every unit name in the draft, depth-first, repeats included. */
export function unitNames(draft: Draft): string[] {
  return [...allUnits(draft)].map(({ unit }) => unit.data.name);
}

/** Every seat handle in the draft, in walk order, repeats included. */
export function seatHandles(draft: Draft): string[] {
  return [...allSeats(draft)].map(({ seat }) => seat.data.handle);
}

/** Every unit key in the draft, depth-first, repeats included. */
export function unitKeys(draft: Draft): string[] {
  return [...allUnits(draft)].map(({ unit }) => unit.data.key);
}

/** What every address of a draft resolves to: see [addressIndex]. */
export interface AddressIndex {
  readonly seats: ReadonlyMap<string, DraftSeat>;
  readonly units: ReadonlyMap<string, DraftUnit>;
}

/**
 * The seat each handle and the unit each key resolves to, as the chart
 * resolves a reference: by the address a node answers to NOW, and then by an
 * address it USED to answer to (`former_handles`, `former_keys`) that nothing
 * has claimed since — the chart keeps a retired address resolving until
 * something else takes it (`org.Organization.Role`). The first holder wins
 * where two nodes of a draft claim one address, which the draft's own check
 * reports.
 */
export function addressIndex(draft: Draft): AddressIndex {
  const seats = new Map<string, DraftSeat>();
  const units = new Map<string, DraftUnit>();
  const all = [...allSeats(draft)].map(({ seat }) => seat);
  const allU = [...allUnits(draft)].map(({ unit }) => unit);
  for (const seat of all) if (!seats.has(seat.data.handle)) seats.set(seat.data.handle, seat);
  for (const unit of allU) if (!units.has(unit.data.key)) units.set(unit.data.key, unit);
  const former = (value: unknown): string[] =>
    Array.isArray(value) ? value.filter((v): v is string => typeof v === "string") : [];
  for (const seat of all) {
    for (const address of former(seat.data.former_handles)) {
      if (!seats.has(address)) seats.set(address, seat);
    }
  }
  for (const unit of allU) {
    for (const address of former(unit.data.former_keys)) {
      if (!units.has(address)) units.set(address, unit);
    }
  }
  return { seats, units };
}

/** The node a seat handle names in the draft, or `undefined`. */
export function seatByHandle(draft: Draft, handle: string): DraftSeat | undefined {
  for (const { seat } of allSeats(draft)) if (seat.data.handle === handle) return seat;
  return undefined;
}

/** The node a unit key names in the draft, or `undefined`. */
export function unitByKey(draft: Draft, key: string): DraftUnit | undefined {
  for (const { unit } of allUnits(draft)) if (unit.data.key === key) return unit;
  return undefined;
}

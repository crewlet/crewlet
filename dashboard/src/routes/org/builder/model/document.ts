/**
 * Between the engine's document and the builder's draft, in both directions,
 * and the merge patch a save sends.
 *
 * `fromDocument` keys a fetched document by the identities it carries (see
 * `keys.ts`): every seat the engine stores declares its handle and every unit
 * its key, so a document read from it is keyed at once, with no check to wait
 * for and nothing derived in the client.
 *
 * `toDocument` walks the draft back into a document and returns, beside it,
 * THE PATH INDEX OF THAT EXACT DOCUMENT: which node sits at `units[0].roles[1]`.
 * A problem the engine reports names a path in the document it validated, so
 * the index is only meaningful together with the bytes it was built beside,
 * and the dry-run scheduler carries the two as one value.
 *
 * `buildPatch` names only the top-level keys the builder edits, and only when
 * they changed. `roles` and `units` are sent whole when anything in them
 * changed, because a JSON merge patch replaces an array wholesale and cannot
 * address one element; the engine merges back what the element held that this
 * build cannot represent. The two integration keys a seat edit reaches are
 * sent as the single values they are, with `null` for a removed key, because
 * a merge patch keeps every key it does not name: sending the access level map
 * of the seats that remain would leave a removed seat's entry in place, and
 * that entry grants its level to the next seat given the same handle.
 */

import type {
  CompanyDocument,
  ConfigRole,
  ConfigUnit,
  Derived,
  DerivedSeat,
  DerivedUnit,
} from "~/protocol/index.ts";
import { cloneJson, getPath, isRecord, jsonEqual, setPath, type JsonRecord } from "./json.ts";
import { COMPANY_KEY, seatKey, unitKey, type NodeKey } from "./keys.ts";
import {
  allUnits,
  declaredHandle,
  declaredUnitKey,
  type Draft,
  type DraftSeat,
  type DraftUnit,
} from "./draft.ts";
import { mintHandle, mintUnitKey } from "./identity.ts";

/** One step of a document path: a key, or a list index. */
export type Segment = string | number;

/** The charter fields: the company node's own editable keys. */
export const CHARTER_FIELDS = ["name", "mission", "vision", "policies"] as const;

/** Where the Datadog fallback seat lives in the document. */
export const DATADOG_ROUTE_TO: readonly string[] = ["integrations", "datadog", "route_to"];

/** Where the per-handle GitLab access levels live in the document. */
export const GITLAB_ACCESS_LEVELS: readonly string[] = [
  "integrations",
  "gitlab",
  "provisioning",
  "access_levels",
];

/**
 * Which node sits at which authored path in one document, both ways.
 *
 * The company is indexed at the empty path. Paths are spelled the way the
 * engine spells them (`units[0].children[1].roles[2]`), and segments are the
 * same place split, so a problem's `segments` can be matched without parsing
 * its `path`.
 */
export interface PathIndex {
  readonly byPath: ReadonlyMap<string, NodeKey>;
  readonly pathOf: ReadonlyMap<NodeKey, string>;
  readonly segmentsOf: ReadonlyMap<NodeKey, readonly Segment[]>;
}

/** A document and the index of its own paths. */
export interface IndexedDocument {
  readonly document: CompanyDocument;
  readonly index: PathIndex;
}

/** Renders segments as the engine renders a path: dotted keys, bracketed indexes. */
export function pathOfSegments(segments: readonly Segment[]): string {
  let out = "";
  for (const segment of segments) {
    if (typeof segment === "number") out += `[${segment}]`;
    else out += out === "" ? segment : `.${segment}`;
  }
  return out;
}

/**
 * Keys a fetched document into a draft: every seat by the handle it declares,
 * every unit by the key it declares.
 *
 * A NODE THAT DECLARES NONE is keyed by the identity the engine's own minting
 * rule gives its name (`identity.ts`), made free of every key already used.
 * The engine writes both identities into every revision it stores, so that
 * happens only for a document that did not come from it, and the node's data
 * is left as it is: keying writes nothing into the document a save compares.
 * A document holding one identity twice, which the engine refuses, keys the
 * second node the same way, so no two nodes ever share a key.
 */
export function fromDocument(doc: CompanyDocument | null): Draft {
  if (!doc) return { company: {}, roles: [], units: [] };
  const source = cloneJson(doc);

  const seats = new Set<string>();
  const units = new Set<string>();
  const seatNode = (role: ConfigRole): DraftSeat => {
    const declared = declaredHandle(role);
    const name = typeof role.name === "string" ? role.name : "";
    const handle = declared && !seats.has(declared) ? declared : mintHandle(name, seats);
    seats.add(handle);
    return { key: seatKey(handle), data: role };
  };
  const unitNode = (unit: ConfigUnit): DraftUnit => {
    const { roles = [], children = [], ...data } = unit;
    const declared = declaredUnitKey(unit);
    const name = typeof unit.name === "string" ? unit.name : "";
    const id = declared && !units.has(declared) ? declared : mintUnitKey(name, units);
    units.add(id);
    return {
      key: unitKey(id),
      data: data as ConfigUnit,
      roles: (Array.isArray(roles) ? roles : []).map(seatNode),
      children: (Array.isArray(children) ? children : []).map(unitNode),
    };
  };

  const { roles = [], units: list = [], ...company } = source;
  return {
    company: company as CompanyDocument,
    roles: (Array.isArray(roles) ? roles : []).map(seatNode),
    units: (Array.isArray(list) ? list : []).map(unitNode),
  };
}

/**
 * The document a draft stands for, and the path index of that document.
 *
 * An empty list is left out, as the engine itself writes a document: a
 * `roles: []` the base never had would read as an edit.
 */
export function toDocument(draft: Draft): IndexedDocument {
  const byPath = new Map<string, NodeKey>([["", COMPANY_KEY]]);
  const pathOf = new Map<NodeKey, string>([[COMPANY_KEY, ""]]);
  const segmentsOf = new Map<NodeKey, readonly Segment[]>([[COMPANY_KEY, []]]);
  const record = (key: NodeKey, segments: Segment[]) => {
    const path = pathOfSegments(segments);
    byPath.set(path, key);
    pathOf.set(key, path);
    segmentsOf.set(key, segments);
  };

  const seat = (node: DraftSeat, segments: Segment[]): ConfigRole => {
    record(node.key, segments);
    return node.data;
  };
  const unit = (node: DraftUnit, segments: Segment[]): ConfigUnit => {
    record(node.key, segments);
    const out: ConfigUnit = { ...node.data };
    if (node.roles.length > 0)
      out.roles = node.roles.map((s, i) => seat(s, [...segments, "roles", i]));
    if (node.children.length > 0) {
      out.children = node.children.map((c, i) => unit(c, [...segments, "children", i]));
    }
    return out;
  };

  const document: CompanyDocument = { ...draft.company };
  if (draft.roles.length > 0) document.roles = draft.roles.map((s, i) => seat(s, ["roles", i]));
  if (draft.units.length > 0) document.units = draft.units.map((u, i) => unit(u, ["units", i]));
  return { document, index: { byPath, pathOf, segmentsOf } };
}

/**
 * A document a check was sent and the derivation the engine answered with.
 *
 * THE TWO TRAVEL TOGETHER, because each is readable only through the other:
 * a derivation names seats and units by their paths in the document it
 * describes, and the draft may have moved a node since, so `units[1]` may be
 * another unit now. Its paths become node keys through the path index built
 * beside that very document, never the draft's.
 */
export interface CheckedDocument {
  readonly sent: IndexedDocument;
  readonly derived: Derived;
}

/** A derivation placed on the nodes of the document it describes. */
export interface PlacedDerivation {
  readonly seatByKey: ReadonlyMap<NodeKey, DerivedSeat>;
  readonly unitByKey: ReadonlyMap<NodeKey, DerivedUnit>;
  /**
   * The seat the derivation gives each handle. The first, where a draft the
   * engine refused gives one handle to two seats.
   */
  readonly keyOfHandle: ReadonlyMap<string, NodeKey>;
}

/** A derivation that places nothing: what a draft no check has described reads. */
export const NO_DERIVATION: PlacedDerivation = {
  seatByKey: new Map(),
  unitByKey: new Map(),
  keyOfHandle: new Map(),
};

/**
 * Places a derivation on the keys of the document it describes. ONE PLACING
 * for every reader (the charts, the dialogs, the review's changes, the
 * problems and the reducer's handles), so no two of them can map one path to
 * two nodes. A seat or unit the derivation carries no path for (an answer
 * from an engine that omits paths) is simply absent.
 */
export function placeDerivation(index: PathIndex, derived: Derived | null): PlacedDerivation {
  const seatByKey = new Map<NodeKey, DerivedSeat>();
  const unitByKey = new Map<NodeKey, DerivedUnit>();
  const keyOfHandle = new Map<string, NodeKey>();
  for (const seat of derived?.seats ?? []) {
    const key = seat.path ? index.byPath.get(seat.path) : undefined;
    if (key === undefined || key === COMPANY_KEY) continue;
    seatByKey.set(key, seat);
    if (seat.handle && !keyOfHandle.has(seat.handle)) keyOfHandle.set(seat.handle, key);
  }
  for (const unit of derived?.units ?? []) {
    const key = unit.path ? index.byPath.get(unit.path) : undefined;
    if (key !== undefined && key !== COMPANY_KEY) unitByKey.set(key, unit);
  }
  return { seatByKey, unitByKey, keyOfHandle };
}

/** A node's JSON as it stood in an indexed document, or `undefined` when it held no such node. */
export function nodeDataIn(
  indexed: IndexedDocument,
  key: NodeKey,
): Record<string, unknown> | undefined {
  const segments = indexed.index.segmentsOf.get(key);
  if (!segments) return undefined;
  let at: unknown = indexed.document;
  for (const segment of segments) {
    if (typeof segment === "number") at = Array.isArray(at) ? at[segment] : undefined;
    else at = isRecord(at) && Object.hasOwn(at, segment) ? at[segment] : undefined;
  }
  return isRecord(at) ? at : undefined;
}

/**
 * Where each node of `before` is in `after`, for two drafts of ONE document
 * keyed two ways: the draft a save sent, whose created nodes carry the keys
 * they were minted with, and the base that save becomes, which keys them by
 * their handles and unit keys. Each key is followed to the key of the node at
 * the same authored path; only keys that changed are listed.
 */
export function rekeying(before: Draft, after: Draft): Map<NodeKey, NodeKey> {
  const was = toDocument(before).index;
  const now = toDocument(after).index;
  const out = new Map<NodeKey, NodeKey>();
  for (const [key, path] of was.pathOf) {
    const moved = now.byPath.get(path);
    if (moved !== undefined && moved !== key) out.set(key, moved);
  }
  return out;
}

/** A JSON merge patch (RFC 7396): `null` removes a key, an object merges, anything else replaces. */
export type MergePatch = JsonRecord;

/** The top-level keys a patch may name whole. */
const WHOLE_KEYS = ["name", "mission", "vision", "policies", "roles", "units"] as const;

/** A list the engine never writes empty reads as absent. */
function normalized(value: unknown): unknown {
  return Array.isArray(value) && value.length === 0 ? undefined : value;
}

/**
 * The merge patch that turns `base` into `draft`, naming only what the builder
 * edits and only what changed. An empty object when nothing did.
 */
export function buildPatch(base: CompanyDocument | null, draft: CompanyDocument): MergePatch {
  let patch: MergePatch = {};
  for (const key of WHOLE_KEYS) {
    const before = normalized(base?.[key]);
    const after = normalized(draft[key]);
    if (!jsonEqual(before, after)) patch[key] = after === undefined ? null : after;
  }

  const routeBefore = getPath(base, DATADOG_ROUTE_TO);
  const routeAfter = getPath(draft, DATADOG_ROUTE_TO);
  if (!jsonEqual(routeBefore, routeAfter)) {
    patch = setPath(patch, DATADOG_ROUTE_TO, routeAfter === undefined ? null : routeAfter);
  }

  const levelsBefore = getPath(base, GITLAB_ACCESS_LEVELS);
  const levelsAfter = getPath(draft, GITLAB_ACCESS_LEVELS);
  const before = isRecord(levelsBefore) ? levelsBefore : {};
  const after = isRecord(levelsAfter) ? levelsAfter : {};
  for (const handle of new Set([...Object.keys(before), ...Object.keys(after)])) {
    if (jsonEqual(before[handle], after[handle])) continue;
    patch = setPath(
      patch,
      [...GITLAB_ACCESS_LEVELS, handle],
      after[handle] === undefined ? null : after[handle],
    );
  }
  return patch;
}

/** Every unit of the draft by key, for the modules that look units up often. */
export function unitsByKey(draft: Draft): Map<NodeKey, DraftUnit> {
  return new Map([...allUnits(draft)].map(({ unit }) => [unit.key, unit]));
}

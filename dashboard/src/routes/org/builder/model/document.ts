/**
 * Between the engine's document and the builder's draft, in both directions,
 * and the merge patch a save sends.
 *
 * `fromDocument` keys a fetched document (see `keys.ts` for why a key is the
 * engine's identity for the node). The engine's `derived` block supplies the
 * handle of every seat by authored path: the client never derives a handle,
 * because a divergent derivation names a different seat and orphans the
 * memory of the one it meant.
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
 * that entry grants its level to the next seat that derives the same handle.
 */

import type {
  CompanyDocument,
  ConfigRole,
  ConfigUnit,
  Derived,
  DerivedSeat,
  DerivedUnit,
} from "~/protocol/index.ts";
import { REDACTED } from "~/lib/format.ts";
import { cloneJson, getPath, isRecord, jsonEqual, setPath, type JsonRecord } from "./json.ts";
import {
  COMPANY_KEY,
  handleOfKey,
  seatKey,
  seatPathKey,
  unitKey,
  unitPathKey,
  type NodeKey,
} from "./keys.ts";
import { allSeats, allUnits, locate, type Draft, type DraftSeat, type DraftUnit } from "./draft.ts";

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
 * Keys a fetched document into a draft.
 *
 * `derived` is the engine's derivation of THIS document (the answer to a dry
 * run of it). Without one, a seat is keyed by the handle it declares or else
 * by its authored path, and a unit whose name is missing or repeated is keyed
 * by its path: see `keys.ts` for what such a key can and cannot survive.
 */
export function fromDocument(doc: CompanyDocument | null, derived: Derived | null): Draft {
  if (!doc) return { company: {}, roles: [], units: [] };
  const source = cloneJson(doc);

  const handleByPath = new Map<string, string>();
  for (const seat of derived?.seats ?? []) {
    if (seat.path && seat.handle) handleByPath.set(seat.path, seat.handle);
  }

  // Collect every seat's identity first, so two seats claiming one handle
  // (which the engine never stores, but a caller could hand in) both fall
  // back to their paths rather than one silently winning the key.
  const seatHandles = new Map<string, string | undefined>();
  const unitNamesSeen = new Map<string, number>();
  walkDocument(source, {
    seat: (role, path) => {
      seatHandles.set(path, handleByPath.get(path) ?? declaredHandle(role));
    },
    unit: (unit) => {
      if (typeof unit.name === "string" && unit.name !== "") {
        unitNamesSeen.set(unit.name, (unitNamesSeen.get(unit.name) ?? 0) + 1);
      }
    },
  });
  const handleCount = new Map<string, number>();
  for (const handle of seatHandles.values()) {
    if (handle) handleCount.set(handle, (handleCount.get(handle) ?? 0) + 1);
  }

  const seatNode = (role: ConfigRole, path: string): DraftSeat => {
    const handle = seatHandles.get(path);
    const key = handle && handleCount.get(handle) === 1 ? seatKey(handle) : seatPathKey(path);
    return { key, data: role };
  };
  const unitNode = (unit: ConfigUnit, path: string): DraftUnit => {
    const { roles = [], children = [], ...data } = unit;
    const name = typeof unit.name === "string" ? unit.name : "";
    const key = name !== "" && unitNamesSeen.get(name) === 1 ? unitKey(name) : unitPathKey(path);
    return {
      key,
      data: data as ConfigUnit,
      roles: (Array.isArray(roles) ? roles : []).map((r, i) => seatNode(r, `${path}.roles[${i}]`)),
      children: (Array.isArray(children) ? children : []).map((c, i) =>
        unitNode(c, `${path}.children[${i}]`),
      ),
    };
  };

  const { roles = [], units = [], ...company } = source;
  return {
    company: company as CompanyDocument,
    roles: (Array.isArray(roles) ? roles : []).map((r, i) => seatNode(r, `roles[${i}]`)),
    units: (Array.isArray(units) ? units : []).map((u, i) => unitNode(u, `units[${i}]`)),
  };
}

/** Visits every seat and unit of a raw document with its authored path. */
function walkDocument(
  doc: CompanyDocument,
  visit: {
    seat: (role: ConfigRole, path: string) => void;
    unit: (unit: ConfigUnit, path: string) => void;
  },
): void {
  const units = (list: unknown, path: string) => {
    if (!Array.isArray(list)) return;
    list.forEach((unit: ConfigUnit, i) => {
      const here = `${path}[${i}]`;
      visit.unit(unit, here);
      if (Array.isArray(unit.roles))
        unit.roles.forEach((r, j) => visit.seat(r, `${here}.roles[${j}]`));
      units(unit.children, `${here}.children`);
    });
  };
  if (Array.isArray(doc.roles)) doc.roles.forEach((r, i) => visit.seat(r, `roles[${i}]`));
  units(doc.units, "units");
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
 * The handle a seat declares, or `undefined` when it declares none.
 *
 * ONE READING FOR RECORD, EVALUATE, APPLY AND DISPLAY. Whether a rename pins
 * the handle is decided three times (when it is recorded, when its
 * preconditions are checked, when it is applied), and the three must agree on
 * what "declares a handle" means or an operation that just recorded fails to
 * apply, which throws inside the reducer; a screen that read it another way
 * would name a handle the operation does not write.
 */
export function declaredHandle(data: Readonly<Record<string, unknown>>): string | undefined {
  return typeof data.handle === "string" && data.handle !== "" ? data.handle : undefined;
}

/**
 * The handle each seat of `draft` runs under, where one is known.
 *
 * The one it declares; else the one its key carries (a seat of the saved
 * company, whose handle every rename pins); else the one `checked` derived
 * for it, while the seat still declares none and is still called what that
 * check saw. The engine derives an undeclared handle from the seat's own name
 * and from nothing else (`org.Role.Handle` over `org.Slugify`), so that
 * derivation holds exactly as long as the name does: a seat this draft
 * created and then renamed runs under a handle no check has reported yet,
 * and naming the old one would name a seat that will never exist.
 *
 * WHICHEVER CHECK SAW THE NAME. A check of an older draft still vouches for
 * every seat whose name has not changed since, so a handle stays known while
 * the next check is out rather than blinking away on every edit. One rule for
 * the reducer that records an operation by it, the charts that draw it and
 * the dialogs that offer it, so none of them offers a handle another refuses.
 */
export function knownHandles(draft: Draft, checked: CheckedDocument | null): Map<NodeKey, string> {
  const derived = checked ? placeDerivation(checked.sent.index, checked.derived).seatByKey : null;
  const out = new Map<NodeKey, string>();
  for (const { seat } of allSeats(draft)) {
    const handle =
      declaredHandle(seat.data) ??
      handleOfKey(seat.key) ??
      (checked && derived ? checkedHandle(seat, checked, derived) : undefined);
    if (handle) out.set(seat.key, handle);
  }
  return out;
}

/** The handle `checked` derived for a seat that declared none, while its name is unchanged. */
function checkedHandle(
  seat: DraftSeat,
  checked: CheckedDocument,
  derived: ReadonlyMap<NodeKey, DerivedSeat>,
): string | undefined {
  const handle = derived.get(seat.key)?.handle;
  const was = nodeDataIn(checked.sent, seat.key);
  if (!handle || !was || declaredHandle(was) !== undefined || was.name !== seat.data.name) {
    return undefined;
  }
  return handle;
}

/**
 * Where each node of `before` is in `after`, for two drafts of ONE document
 * keyed two ways: the base before and after it is keyed, when the engine's
 * first description of it moves the seats that declare no handle from their
 * path keys to their handles, or a save moves the nodes it created from the
 * keys they were minted with. Each key is followed to the key of the node at
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

/**
 * A name not yet taken, starting from the one the operator typed.
 *
 * A convenience, not a validator: seat names and unit names must each be
 * unique (a lead or a `manages` entry names exactly one), and pre-filling
 * "Software Engineer 2" saves a round trip to the engine to learn that. The
 * engine still decides.
 */
export function suggestUniqueName(taken: Iterable<string>, desired: string): string {
  const names = new Set(taken);
  const wanted = desired.trim();
  if (!names.has(wanted)) return wanted;
  const numbered = /^(.*\S)\s+(\d+)$/.exec(wanted);
  const stem = numbered ? numbered[1]! : wanted;
  let n = numbered ? Number(numbered[2]) + 1 : 2;
  while (names.has(`${stem} ${n}`)) n++;
  return `${stem} ${n}`;
}

/**
 * The authored paths of the masked literal credentials a unit rename strands,
 * in the document the draft stands for. Paths only, never values.
 *
 * ONLY THE UNIT'S OWN FIELDS. The engine restores a masked value by the
 * identity of the entity holding it, over the whole prior document: a unit by
 * its name, a seat by its handle. Renaming a unit changes the identity of that
 * unit alone, so its own masks can no longer be matched and the save is
 * refused naming them, while a child unit (its own name) and every seat inside
 * (its handle) still restore. Listing the subtree would send the operator to
 * move credentials that were never at risk.
 */
export function maskedCredentialPaths(draft: Draft, key: NodeKey): string[] {
  const found = locate(draft, key);
  if (found?.kind !== "unit") return [];
  const { index } = toDocument(draft);
  const base = index.pathOf.get(key) ?? "";
  const out: string[] = [];
  const walk = (value: unknown, segments: Segment[]) => {
    if (value === REDACTED) out.push(pathOfSegments(segments));
    else if (Array.isArray(value)) value.forEach((v, i) => walk(v, [...segments, i]));
    else if (isRecord(value)) for (const [k, v] of Object.entries(value)) walk(v, [...segments, k]);
  };
  for (const [k, v] of Object.entries(found.node.data)) walk(v, [k]);
  return out.map((p) => (p.startsWith("[") ? base + p : `${base}.${p}`));
}

/** Every unit of the draft by key, for the modules that look units up often. */
export function unitsByKey(draft: Draft): Map<NodeKey, DraftUnit> {
  return new Map([...allUnits(draft)].map(({ unit }) => [unit.key, unit]));
}

/**
 * Between the engine's two answers and the builder's draft.
 *
 * TWO SOURCES, ONE DRAFT. The company is two things with two lifecycles now:
 * its SETTINGS are a revision `GET /config` serves (the charter, the
 * integrations, every company-wide value), and its ORG CHART is a log of its
 * own that `GET /chart` serves (the units, the seats, who leads and manages
 * whom). [fromChart] keys the chart into the draft's tree beside the settings
 * document; the save divides the draft again (`save.ts`).
 *
 * THE CHART NEEDS NO DESCRIBING. Every seat it serves carries its handle and
 * every unit its key, and a renamed one the address it was created under — its
 * identity, which is what a node is keyed by (`keys.ts`) — so the base is
 * keyed the moment it is read: there is no derivation to wait for and no path
 * to fall back on. What the engine DERIVES
 * from the chart (the lead a unit inherits, a seat's primary manager) comes
 * from the org projection's `derived` block and is placed on the nodes by
 * handle and by unit key ([placeDerivation]).
 *
 * WHAT "SOMEBODY ELSE SAVED FIRST" MEANS for the chart is that its ROWS
 * changed, not that its position moved: every chart read is linearizable, and
 * a linearizable read appends a barrier record to the log, so the position an
 * answer reports moves on every read anybody makes. [chartPrint] is the rows
 * alone, canonical, and a check compares that.
 */

import type {
  ChartRead,
  ChartSeat,
  ChartUnit,
  CompanyDocument,
  Derived,
  DerivedSeat,
  DerivedUnit,
} from "~/protocol/index.ts";
import { cloneJson, getPath, isRecord, jsonEqual, setPath, type JsonRecord } from "./json.ts";
import { COMPANY_KEY, seatKey, unitKey, type NodeKey } from "./keys.ts";
import {
  allSeats,
  allUnits,
  inAddressOrder,
  locate,
  type Draft,
  type DraftSeat,
  type DraftUnit,
  type SeatData,
  type UnitData,
} from "./draft.ts";

/** One step of a document path: a key, or a list index. */
export type Segment = string | number;

/** The charter fields: the company node's own editable keys. */
export const CHARTER_FIELDS = ["name", "mission", "vision", "policies"] as const;

/** Where the Datadog fallback seat lives in the settings document. */
export const DATADOG_ROUTE_TO: readonly string[] = ["integrations", "datadog", "route_to"];

/** Where the per-handle GitLab access levels live in the settings document. */
export const GITLAB_ACCESS_LEVELS: readonly string[] = [
  "integrations",
  "gitlab",
  "provisioning",
  "access_levels",
];

/** Renders segments as the engine renders a path: dotted keys, bracketed indexes. */
export function pathOfSegments(segments: readonly Segment[]): string {
  let out = "";
  for (const segment of segments) {
    if (typeof segment === "number") out += `[${segment}]`;
    else out += out === "" ? segment : `.${segment}`;
  }
  return out;
}

// ---------------------------------------------------------------------------
// Reading the chart
// ---------------------------------------------------------------------------

/**
 * Whether a served value is one the draft keeps. An empty string and an empty
 * list are how the chart serves a field nobody set — its views omit them, and
 * an older answer may not — so they are read as absent: a draft holding "" for
 * a goal the chart never had would read as an edit nobody made.
 */
const kept = (value: unknown): boolean =>
  value !== undefined &&
  value !== null &&
  value !== "" &&
  !(Array.isArray(value) && value.length === 0);

/**
 * A seat's data as the draft holds it, from the seat the chart served and its
 * authored `manages:` list.
 *
 * AN AGENT'S KIND IS LEFT UNWRITTEN, as the builder writes every agent seat: a
 * seat is an agent unless it says it is a person, so "agent" and absent are one
 * value, and holding the two spellings apart would read a kind change and its
 * undo as an edit. A kind this build does not know is kept as served.
 *
 * WHERE IT SITS IS THE TREE'S, so `unit` is left out, and the addresses it used
 * to answer to are kept for the screens that explain a stale reference.
 */
export function seatDataOf(seat: ChartSeat, manages: readonly string[] | undefined): SeatData {
  // THE IDENTITY IS THE KEY'S, not the data's: nothing edits it, and no write
  // states it.
  const {
    unit: _unit,
    origin_handle: _origin,
    handle,
    kind,
    name,
    runtime,
    ...rest
  } = cloneJson(seat);
  const data: SeatData = { handle, name: typeof name === "string" ? name : "" };
  if (typeof kind === "string" && kept(kind) && kind !== "agent") data.kind = kind;
  for (const [field, value] of Object.entries(rest)) if (kept(value)) data[field] = value;
  if (manages && manages.length > 0) data.manages = [...manages];
  if (isRecord(runtime) && Object.keys(runtime).length > 0) data.runtime = runtime;
  return data;
}

/** A unit's data as the draft holds it, from the unit the chart served. */
export function unitDataOf(unit: ChartUnit): UnitData {
  const { parent: _parent, origin_key: _origin, key, name, runtime, ...rest } = cloneJson(unit);
  const data: UnitData = { key, name: typeof name === "string" ? name : "" };
  for (const [field, value] of Object.entries(rest)) if (kept(value)) data[field] = value;
  if (isRecord(runtime) && Object.keys(runtime).length > 0) data.runtime = runtime;
  return data;
}

/**
 * Keys a chart reading into a draft beside the settings document.
 *
 * ADDRESS ORDER, which is the only order the chart keeps (see
 * `draft.Placement`): every list is sorted here rather than trusted to arrive
 * sorted, so two readings of one chart build one draft. A unit whose parent the
 * reading does not hold (a row a record left dangling) is placed at the root
 * rather than dropped: a node the draft cannot show is one the save would read
 * as removed.
 */
export function fromChart(settings: CompanyDocument | null, chart: ChartRead | null): Draft {
  const company = settings ? stripChart(cloneJson(settings)) : {};
  if (!chart) return { company, roles: [], units: [] };
  const manages = chart.manages ?? {};
  const unitsByKey = new Map<string, ChartUnit>();
  for (const unit of chart.units ?? []) unitsByKey.set(unit.key, unit);
  const childKeys = new Map<string, string[]>();
  const rootUnits: string[] = [];
  for (const unit of chart.units ?? []) {
    const parent = unit.parent && unitsByKey.has(unit.parent) ? unit.parent : "";
    if (parent === "") rootUnits.push(unit.key);
    else childKeys.set(parent, [...(childKeys.get(parent) ?? []), unit.key]);
  }
  const seatsIn = new Map<string, ChartSeat[]>();
  const rootSeats: ChartSeat[] = [];
  for (const seat of chart.seats ?? []) {
    const home = seat.unit && unitsByKey.has(seat.unit) ? seat.unit : "";
    if (home === "") rootSeats.push(seat);
    else seatsIn.set(home, [...(seatsIn.get(home) ?? []), seat]);
  }
  const seatNode = (seat: ChartSeat): DraftSeat => ({
    key: seatKey(seat.origin_handle || seat.handle),
    data: seatDataOf(seat, manages[seat.handle]),
  });
  const placed = new Set<string>();
  const unitNode = (key: string): DraftUnit => {
    placed.add(key);
    const unit = unitsByKey.get(key)!;
    return {
      key: unitKey(unit.origin_key || key),
      data: unitDataOf(unit),
      roles: inAddressOrder((seatsIn.get(key) ?? []).map(seatNode)),
      children: inAddressOrder(
        (childKeys.get(key) ?? []).filter((k) => !placed.has(k)).map(unitNode),
      ),
    };
  };
  const units = rootUnits.map(unitNode);
  // A CYCLE THE ROWS SHOULD NEVER HOLD is still drawn rather than lost: every
  // unit no walk from the root reached goes to the root, as the engine's own
  // view builder places it.
  for (const unit of chart.units ?? []) if (!placed.has(unit.key)) units.push(unitNode(unit.key));
  return {
    company,
    roles: inAddressOrder(rootSeats.map(seatNode)),
    units: inAddressOrder(units),
  };
}

/**
 * The settings document without the two keys a settings revision never holds:
 * every door that writes one keeps them out, and the builder draws the chart
 * from the chart alone, so a document that did would put no seat on the
 * canvas.
 */
function stripChart(settings: CompanyDocument): CompanyDocument {
  const { roles: _roles, units: _units, ...rest } = settings;
  return rest;
}

/**
 * The chart's rows, canonical: what "the chart has not changed" is compared
 * on. Keys sorted at every depth and the seats and units in address order, so
 * two readings of the same rows are the same string however the answer happened
 * to list them; the answer's own position and level are left out, because every
 * read moves the position (see the module doc).
 */
export function chartPrint(chart: ChartRead | null): string {
  if (!chart) return "";
  const byAddress = <T>(list: readonly T[], address: (t: T) => string) =>
    [...list].sort((a, b) => (address(a) < address(b) ? -1 : address(a) > address(b) ? 1 : 0));
  return canonical({
    units: byAddress(chart.units ?? [], (u) => u.key),
    seats: byAddress(chart.seats ?? [], (s) => s.handle),
    manages: chart.manages ?? {},
    leads: chart.leads ?? {},
  });
}

/**
 * A short, stable name for a print: what a kept draft records of the chart it
 * was made on, so a restore can tell "the same chart" from "a changed one"
 * without keeping the chart — its model chains, its contact identities and
 * its credential names — in browser storage.
 *
 * TWO INDEPENDENT 32-BIT FNV-1a HASHES over the UTF-16 code units, sixty-four
 * bits together: this names one chart among the handful a tab ever meets,
 * against nobody choosing the input, so a cryptographic digest (async, in
 * this browser) would buy nothing a comparison needs.
 */
export function fingerprint(print: string): string {
  let a = 0x811c9dc5;
  let b = 0x01000193 ^ 0x9e3779b9;
  for (let i = 0; i < print.length; i++) {
    const c = print.charCodeAt(i);
    a = Math.imul(a ^ c, 0x01000193);
    b = Math.imul(b ^ c, 0x01000197);
  }
  const hex = (n: number) => (n >>> 0).toString(16).padStart(8, "0");
  return hex(a) + hex(b);
}

/** JSON with every object's keys sorted, so key order never reads as a change. */
function canonical(value: unknown): string {
  if (Array.isArray(value)) return `[${value.map(canonical).join(",")}]`;
  if (isRecord(value)) {
    const keys = Object.keys(value)
      .filter((k) => value[k] !== undefined)
      .sort();
    return `{${keys.map((k) => `${JSON.stringify(k)}:${canonical(value[k])}`).join(",")}}`;
  }
  return JSON.stringify(value ?? null);
}

// ---------------------------------------------------------------------------
// The engine's derivation, placed
// ---------------------------------------------------------------------------

/** A derivation placed on the nodes of the draft it describes. */
export interface PlacedDerivation {
  readonly seatByKey: ReadonlyMap<NodeKey, DerivedSeat>;
  readonly unitByKey: ReadonlyMap<NodeKey, DerivedUnit>;
  /** The node each derived handle names. */
  readonly keyOfHandle: ReadonlyMap<string, NodeKey>;
}

/** A derivation that places nothing: what a draft no derivation describes reads. */
export const NO_DERIVATION: PlacedDerivation = {
  seatByKey: new Map(),
  unitByKey: new Map(),
  keyOfHandle: new Map(),
};

/**
 * Places the engine's derivation on the nodes of a draft: a derived seat on the
 * node holding its handle, a derived unit on the node holding its key.
 *
 * ONE PLACING for every reader (the charts, the dialogs, the review's changes),
 * so no two of them can put one derived fact on two nodes. A derived seat or
 * unit the draft holds no node for is simply absent.
 */
export function placeDerivation(draft: Draft, derived: Derived | null): PlacedDerivation {
  if (!derived) return NO_DERIVATION;
  const seatNodes = new Map<string, NodeKey>();
  for (const { seat } of allSeats(draft)) seatNodes.set(seat.data.handle, seat.key);
  const unitNodes = new Map<string, NodeKey>();
  for (const { unit } of allUnits(draft)) unitNodes.set(unit.data.key, unit.key);
  const seatByKey = new Map<NodeKey, DerivedSeat>();
  const unitByKey = new Map<NodeKey, DerivedUnit>();
  const keyOfHandle = new Map<string, NodeKey>();
  for (const seat of derived.seats ?? []) {
    const key = seatNodes.get(seat.handle);
    if (key === undefined) continue;
    seatByKey.set(key, seat);
    keyOfHandle.set(seat.handle, key);
  }
  for (const unit of derived.units ?? []) {
    const key = unit.id ? unitNodes.get(unit.id) : undefined;
    if (key !== undefined) unitByKey.set(key, unit);
  }
  return { seatByKey, unitByKey, keyOfHandle };
}

/**
 * Whether a derivation describes exactly the chart a draft was keyed from:
 * the same seats, the same units, and every unit's declared lead the one the
 * derivation resolved when it declares one.
 *
 * THE ORG PUSH AND THE CHART READ ARE TWO ANSWERS, and the push can trail the
 * read by an apply. A derivation of a chart that has since gained a seat or
 * moved a lead would put facts about one chart on another's nodes, so a
 * derivation that does not match is not used at all — the charts then draw what
 * the draft itself says, as they do for a draft that has changed.
 */
export function describes(draft: Draft, derived: Derived | null): boolean {
  if (!derived) return false;
  const handles = new Set([...allSeats(draft)].map(({ seat }) => seat.data.handle));
  const derivedHandles = new Set((derived.seats ?? []).map((s) => s.handle));
  if (handles.size !== derivedHandles.size) return false;
  for (const handle of handles) if (!derivedHandles.has(handle)) return false;
  const units = new Map([...allUnits(draft)].map(({ unit }) => [unit.data.key, unit.data]));
  const derivedUnits = derived.units ?? [];
  if (units.size !== derivedUnits.length) return false;
  for (const unit of derivedUnits) {
    const data = unit.id ? units.get(unit.id) : undefined;
    if (!data) return false;
    if (data.lead && !unit.lead_inherited && unit.lead !== data.lead) return false;
  }
  return true;
}

/** A node's data in a draft, or `undefined` when it holds no such node. */
export function nodeDataIn(draft: Draft, key: NodeKey): Record<string, unknown> | undefined {
  if (key === COMPANY_KEY) return draft.company;
  return locate(draft, key)?.node.data;
}

/** The handle each seat of a draft runs under: the one its data carries. */
export function knownHandles(draft: Draft): Map<NodeKey, string> {
  const out = new Map<NodeKey, string>();
  for (const { seat } of allSeats(draft)) out.set(seat.key, seat.data.handle);
  return out;
}

/**
 * Where each node of `before` is in `after`, for two drafts of ONE chart keyed
 * two ways: a save moves the nodes it created from the keys they were minted
 * with to the identities the chart gave them — the address each was created
 * under, which is the one it holds when the save reads the chart back. Matched
 * by handle and by unit key; only keys that changed are listed, so a node the
 * chart already held keeps its key through its own rename.
 */
export function rekeying(before: Draft, after: Draft): Map<NodeKey, NodeKey> {
  const seats = new Map([...allSeats(after)].map(({ seat }) => [seat.data.handle, seat.key]));
  const units = new Map([...allUnits(after)].map(({ unit }) => [unit.data.key, unit.key]));
  const out = new Map<NodeKey, NodeKey>();
  for (const { seat } of allSeats(before)) {
    const moved = seats.get(seat.data.handle);
    if (moved !== undefined && moved !== seat.key) out.set(seat.key, moved);
  }
  for (const { unit } of allUnits(before)) {
    const moved = units.get(unit.data.key);
    if (moved !== undefined && moved !== unit.key) out.set(unit.key, moved);
  }
  return out;
}

// ---------------------------------------------------------------------------
// The settings a save writes
// ---------------------------------------------------------------------------

/** A JSON merge patch (RFC 7396): `null` removes a key, an object merges, anything else replaces. */
export type MergePatch = JsonRecord;

/** A list the engine never writes empty reads as absent. */
function normalized(value: unknown): unknown {
  return Array.isArray(value) && value.length === 0 ? undefined : value;
}

/**
 * The merge patch that turns the base settings into the draft's, naming only
 * what the builder edits and only what changed: the charter, the Datadog
 * fallback, and the GitLab access levels. An empty object when nothing did.
 *
 * The two integration keys are sent as the single values they are, with
 * `null` for a removed key, because a merge patch keeps every key it does not
 * name: sending the access level map of the seats that remain would leave a
 * removed seat's entry in place, and that entry would grant its level to the
 * next seat given the same handle.
 */
export function settingsPatch(base: CompanyDocument | null, draft: CompanyDocument): MergePatch {
  let patch: MergePatch = {};
  for (const key of CHARTER_FIELDS) {
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

// ---------------------------------------------------------------------------
// Names and addresses
// ---------------------------------------------------------------------------

/**
 * A name not yet taken, starting from the one the operator typed.
 *
 * A convenience, not a rule: the chart lets two seats share a name, because a
 * name is prose and every reference is a handle. Pre-filling "Software Engineer
 * 2" beside an existing "Software Engineer" still spares a reader two cards
 * they cannot tell apart.
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

/** The longest address the chart takes, in bytes: a handle is a subject token and a login's width. */
export const MAX_ADDRESS = 64;

/**
 * An address made from a name: lowercase letters and digits, runs of anything
 * else as one hyphen, trimmed, at most [MAX_ADDRESS] bytes.
 *
 * A SUGGESTION, NEVER A DERIVATION. The engine derives nothing from a name any
 * more: a seat is created under the handle the request states and a unit under
 * the key, so what this produces is only what the add form offers first, and
 * the operator may type any other. Accents are folded to their base letter so
 * "Ingénierie" suggests `ingenierie` rather than `ing-nierie`.
 */
export function slugOf(name: string): string {
  const folded = name.normalize("NFKD").replace(/[\u0300-\u036f]/g, "");
  const slug = folded
    .toLowerCase()
    .replace(/[^a-z0-9]+/g, "-")
    .replace(/^-+|-+$/g, "");
  return slug.slice(0, MAX_ADDRESS).replace(/-+$/, "");
}

/**
 * A free address for a new node, from its name: the slug, or the slug with the
 * first free number after it. `fallback` stands in for a name that slugs to
 * nothing ("—", or a name in a script with no Latin letters).
 */
export function suggestAddress(taken: Iterable<string>, name: string, fallback: string): string {
  const used = new Set(taken);
  const stem = slugOf(name) || fallback;
  if (!used.has(stem)) return stem;
  for (let n = 2; ; n++) {
    const suffix = `-${n}`;
    const candidate = stem.slice(0, MAX_ADDRESS - suffix.length).replace(/-+$/, "") + suffix;
    if (!used.has(candidate)) return candidate;
  }
}

/** Every unit of the draft by key, for the modules that look units up often. */
export function unitsByKey(draft: Draft): Map<NodeKey, DraftUnit> {
  return new Map([...allUnits(draft)].map(({ unit }) => [unit.key, unit]));
}

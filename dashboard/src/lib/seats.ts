/**
 * What a seat IS, and what it is doing.
 *
 * THE ENGINE DERIVES THE HIERARCHY; THIS INDEXES IT. Every screen that shows a
 * person needs the same facts about them: the handle they run under, the unit
 * they actually sit in, that unit's effective lead, who manages them and whom
 * they manage. Each of those is a rule the engine applies to the document —
 * handle derivation, a root seat moved into the unit its `unit:` reference
 * names, lead and channel inheritance, unit names expanded inside `manages`,
 * automatic management by a unit lead, which of several managers is primary —
 * and the org projection carries the RESULT, in its `derived` block.
 *
 * This module used to work all of that out again in TypeScript, and the copy
 * had drifted from Go on four counts: `slugify` lower-cased "İlker" into
 * `i-lker` where the engine derives `ilker` (the handle keys a seat's memory,
 * so a link pinning this client's version would point at nothing), a root
 * seat's `unit:` placement was ignored entirely, lead auto-management ran past
 * the engine's shield rule, and every one of those was invisible because
 * nothing compared the two. So no manager, lead, `manages` or handle logic
 * remains here, and there is no client-side handle derivation at all.
 *
 * AN OLDER ENGINE SENDS NO `derived` BLOCK, and the index then SAYS SO
 * (`hierarchy: false`) rather than guessing: seats sit where the document
 * wrote them, a handle is known only where the document declares one, and
 * every question only the engine can answer is reported as unknown. A block
 * that does not describe the tree it arrived with is treated the same way,
 * because a chart drawn from a hierarchy that disagrees with its own seats is
 * a chart that lies.
 *
 * THE GUARDED HALF IS NOT HERE EITHER. `/org` is anonymously readable, so the
 * projection carries a charter, a tree and the budgets as written — and, to a
 * reader holding the grant that reads the company's configuration, each agent
 * seat's RESOLVED model chain and tool sources, which are derived from the
 * runtime half and follow its rule. Nothing else: a seat's authored model
 * chain, token budget, contact identities, tool credentials and schedules are
 * the org chart's RUNTIME half, read through `GET /chart/seats/{handle}` where
 * the reader may have it (`lib/chartReads.ts`, [useSeatSetup]), and
 * [seatReading] below says what that read answered.
 *
 * Resolved ONCE, into an index, and screens consume seats. Doing it per screen
 * is how the previous dashboard ended up walking the whole roster once per
 * rendered row: `managerOf` was a linear scan called per seat AND again per
 * row, which on a 200-seat company was roughly 80,000 array scans per push.
 */

import { useMemo } from "react";
import { parseUTC, plural } from "./format.ts";
import { needsSentence } from "./refusal.ts";
import { chartSeat, chartUnit, useChartRead, WITH_RUNTIME } from "./chartReads.ts";
import type { ChartReading } from "./chartReads.ts";
import { useOrg, useOrgPushes } from "./store-hooks.ts";
import { useViewer } from "./viewer.ts";
import { DELEGATE_TASKS, DELEGATE_TOOL, type SeatActivity } from "~/contract/wire.ts";
import type { EngineHealth } from "~/contract/health.ts";
import type {
  AgentRow,
  ChartSeat,
  ChartSeatRead,
  ChartUnit,
  ChartUnitRead,
  Derived,
  LiveCall,
  OrgProjection,
  OrgSeat,
  OrgUnit,
  ProviderKeys,
  RestFailure,
  SandboxEntry,
} from "~/protocol/index.ts";

/** One unit, as the projection wrote it and as the engine resolved it. */
export interface Unit {
  /** Stable React key and DOM id suffix: the unit's depth-first position. */
  key: string;
  /**
   * The unit's KEY, and what ADDRESSES it — here and in every route
   * ([unitPath]).
   *
   * The engine's own `Unit.Key`, carried on the projection: the declared `id:`
   * where the unit has one and its name where it does not. Never the name
   * alone. A name is prose, two units may share one, and a unit that declares
   * an id is addressed by the chart, a schedule's scope and a report's finding
   * under that id — so a page looked up by name opened the first unit of that
   * name, or "no unit called" for a link carrying the id.
   */
  id: string;
  /** What a person reads. Display only: nothing resolves a unit by it. */
  name: string;
  /**
   * The keys a rename moved this unit off, which still ADDRESS it: the key it
   * was created under ("" until a rename moved it off it) and every key it has
   * answered to since, newest first. [unitByKey] resolves through them in the
   * engine's own order, so a link somebody kept opens the unit it named.
   */
  originKey: string;
  formerKeys: string[];
  /** The EFFECTIVE type where the hierarchy is reported, else as written. */
  type: string;
  purpose: string;
  goals: string[];
  /** Free-text knowledge references. NOT a read scope. */
  knowledge: string[];
  /**
   * The lead AS THE DOCUMENT WRITES IT: a seat name, "" when the unit
   * inherits one. Deliberately still a name rather than a resolved seat —
   * `effectiveLead` is the resolved one, and the two are different facts.
   */
  lead: string;
  /** The effective lead, declared or inherited, resolved to a seat. */
  effectiveLead: Seat | null;
  leadInherited: boolean;
  channel: string;
  channelInherited: boolean;
  parent: Unit | null;
  children: Unit[];
  /** Outermost unit first, this unit last. */
  chain: Unit[];
  /** Direct members, after root seats were attached where the engine said so. */
  seats: Seat[];
  /**
   * This unit's members AND every member of every unit under it.
   *
   * THE OTHER HONEST ANSWER to "how big is this team", and it is here rather
   * than re-derived per screen because it was re-derived per screen: the
   * workspace rail filtered the whole roster on `unitChain` once per unit, and
   * the org chart counted `seats` instead, so one name carried two numbers
   * three inches apart. Filled in one pass over the depth-first unit list, so
   * a hundred-unit company costs one walk rather than a hundred scans.
   *
   * It is the ENGINE's placement, never the document's nesting: a root seat
   * whose `unit:` reference moved it into a team is a member of that team's
   * subtree and of nothing it was written under.
   */
  allSeats: Seat[];
  raw: OrgUnit;
}

/**
 * Which kind of seat a handle names.
 *
 * A NAMED TYPE because it is drawn as well as read: the dashed avatar ring is
 * the one variant an identity badge has, and the screens that draw it were
 * each spelling the union inline — so the cell that meant to accept it typed
 * `"agent" | "human" | string`, which is `string`, and accepted anything.
 */
export type SeatKind = "agent" | "human";

export interface Seat {
  /**
   * Stable React key and DOM id suffix: the handle, or `#<position>` where no
   * unique handle is known. The position key carries a `#`, which no handle
   * can, so the two can never collide.
   */
  key: string;
  name: string;
  /**
   * The handle this seat RUNS UNDER, as the engine reports it. "" when the
   * projection carries no derived hierarchy and the document declares none: a
   * handle this client derived would be a second implementation of the rule
   * that keys the seat's memory, so it never makes one up.
   */
  handle: string;
  /**
   * The handles a rename moved this seat off, which still ADDRESS it: the one
   * it was created under ("" until a rename moved it off it) and every one it
   * has answered to since, newest first. [seatByAddress] resolves through them
   * in the engine's own order.
   */
  originHandle: string;
  formerHandles: string[];
  kind: SeatKind;
  goal: string;
  backstory: string;
  responsibilities: string[];
  guidelines: string[];
  /** The `manages` entries AS WRITTEN: seat and unit names, unexpanded. */
  manages: string[];
  availability: string;
  /** Root → own unit. Empty for a root-level seat. */
  unitChain: Unit[];
  unit: Unit | null;
  /**
   * The effective unit lead's NAME, declared or inherited, and "" where the
   * engine did not say. A name rather than a seat because every caller draws
   * it as words beside the unit.
   */
  unitLead: string;
  /** A root seat the engine moved into a unit because of its `unit:` reference. */
  placedByRef: boolean;
  /** The engine's primary manager. Null when it has none, or none was reported. */
  manager: Seat | null;
  /** Every seat whose `manages` reaches this one, in engine order. */
  managers: Seat[];
  /** Direct reports after expansion, explicit and automatic. */
  reports: Seat[];
  /** The subset of `reports` that come from leading a unit. */
  autoReports: Seat[];
  raw: OrgSeat;
}

export interface OrgIndex {
  /**
   * Whether the engine's derived hierarchy is present AND describes this tree.
   * False means every reporting line, inherited lead and placement below is
   * UNKNOWN rather than absent, and a screen has to say so rather than
   * drawing a zero.
   */
  hierarchy: boolean;
  seats: Seat[];
  /** The seats above every unit. */
  rootSeats: Seat[];
  units: Unit[];
  /** The outermost units, in document order. */
  topUnits: Unit[];
  byHandle: Map<string, Seat>;
}

/**
 * The route segments that open a seat's page: its handle, or its name where no
 * handle is known.
 *
 * ONE HELPER, because the rule for a seat the engine reported no handle for
 * has to live in one place: the seat screen resolves a name as well as a
 * handle, so such a seat still has a page a link can reach.
 */
export function seatPath(seat: Pick<Seat, "handle" | "name">): string[] {
  return ["agents", "seats", seat.handle || seat.name];
}

/**
 * The route segments that open a unit's page: its KEY, never its name.
 *
 * ONE HELPER for the reason [seatPath] is one: five screens built this path
 * for themselves, every one of them out of the unit's name, so two units
 * sharing a name opened the same page and a unit that declares an id was
 * unreachable from the chart's own findings and a schedule's scope, which
 * both name it by key.
 */
export function unitPath(unit: Pick<Unit, "id">): string[] {
  return ["agents", "teams", unit.id];
}

/**
 * The unit a route's segment addresses: the one whose KEY it is, or null.
 *
 * Exactly the key the projection carried, which is the engine's own
 * `Unit.Key` and so the value every link this product builds — [unitPath], a
 * schedule's scope, a report's finding — already holds. Never the name: two
 * units may share one, and a lookup by name opened whichever came first.
 */
export function unitByKey(index: Pick<OrgIndex, "units">, key: string): Unit | null {
  if (!key) return null;
  // A RETIRED KEY TOO, and only after every live one has missed — the engine's
  // own order (`Organization.Unit`): the key a unit was created under, then
  // the keys it has answered to since. A link somebody kept to a team that has
  // since been re-keyed opened "No unit" when this matched the current key
  // alone; the screen then replaces the route with the key it holds now.
  return (
    index.units.find((u) => u.id === key) ??
    index.units.find((u) => u.originKey === key) ??
    index.units.find((u) => u.formerKeys.includes(key)) ??
    null
  );
}

/**
 * The seat a route's segment addresses, or null — resolved in the engine's own
 * order (`Organization.Role`): every current handle, then the handle each seat
 * was created under, then every handle a rename retired. A live handle never
 * loses to another seat's retired one, which is why these are three passes and
 * not one.
 *
 * NEVER A NAME, except for the one seat a name is the address of: a seat the
 * engine reported no handle for, which [seatPath] links to by name. A name is
 * prose and two seats may share it, so resolving any other link by name opened
 * whichever namesake came first — a different person's page under a URL that
 * looked right.
 *
 * A HANDLE IS LOWER CASE, so an address typed with capitals is read as the
 * handle it spells rather than as nobody.
 */
export function seatByAddress(
  index: Pick<OrgIndex, "seats" | "byHandle">,
  address: string,
): Seat | null {
  if (!address) return null;
  const found =
    index.byHandle.get(address) ??
    index.seats.find((s) => s.originHandle === address) ??
    index.seats.find((s) => s.formerHandles.includes(address)) ??
    index.seats.find((s) => s.handle === "" && s.name === address) ??
    null;
  if (found) return found;
  const lower = address.toLowerCase();
  return lower === address ? null : seatByAddress(index, lower);
}

const list = <T>(value: T[] | null | undefined): T[] => (Array.isArray(value) ? value : []);

interface Authored {
  units: { raw: OrgUnit; parent: number }[];
  /** Seats in walk order, each with the index of its container unit (-1 at the root). */
  seats: { raw: OrgSeat; container: number }[];
}

/** The tree as the document wrote it: units depth first, seats in walk order. */
function walk(org: OrgProjection | null | undefined): Authored {
  const out: Authored = { units: [], seats: [] };
  for (const raw of list(org?.roles)) out.seats.push({ raw, container: -1 });
  const visit = (raw: OrgUnit, parent: number): void => {
    const at = out.units.length;
    out.units.push({ raw, parent });
    for (const seat of list(raw.roles)) out.seats.push({ raw: seat, container: at });
    for (const child of list(raw.children)) visit(child, at);
  };
  for (const unit of list(org?.units)) visit(unit, -1);
  return out;
}

function newUnit(raw: OrgUnit, at: number): Unit {
  return {
    key: `u${at}`,
    id: raw.id ?? "",
    name: raw.name ?? "",
    originKey: "",
    formerKeys: [],
    type: raw.type ?? "",
    purpose: raw.purpose ?? "",
    goals: list(raw.goals),
    knowledge: list(raw.knowledge),
    lead: raw.lead ?? "",
    effectiveLead: null,
    leadInherited: false,
    channel: raw.channel ?? "",
    channelInherited: false,
    parent: null,
    children: [],
    chain: [],
    seats: [],
    allSeats: [],
    raw,
  };
}

function newSeat(raw: OrgSeat, handle: string, key: string): Seat {
  return {
    key,
    name: raw.name ?? "",
    handle,
    originHandle: "",
    formerHandles: [],
    kind: raw.kind === "human" ? "human" : "agent",
    goal: raw.goal ?? "",
    backstory: raw.backstory ?? "",
    responsibilities: list(raw.responsibilities),
    guidelines: list(raw.behavioral_guidelines),
    manages: list(raw.manages),
    availability: raw.availability ?? "",
    unitChain: [],
    unit: null,
    unitLead: "",
    placedByRef: false,
    manager: null,
    managers: [],
    reports: [],
    autoReports: [],
    raw,
  };
}

/** Link the units into a tree. Shared by both halves below. */
function link(authored: Authored): Unit[] {
  const units = authored.units.map(({ raw }, i) => newUnit(raw, i));
  units.forEach((unit, i) => {
    const parent = authored.units[i]!.parent;
    unit.parent = parent < 0 ? null : units[parent]!;
    unit.parent?.children.push(unit);
    unit.chain = [...(unit.parent?.chain ?? []), unit];
  });
  return units;
}

/**
 * Lay the engine's derived block over the authored tree, or report that it
 * cannot be.
 *
 * The block names units in depth-first order and seats by name, so the pairing
 * is CHECKED rather than assumed: the same number of units with the same keys
 * and names in the same order, every seat accounted for exactly once, and every handle
 * it mentions belonging to one of them. Anything else returns null and the
 * caller falls back to the authored tree, because half a hierarchy drawn as a
 * whole one is worse than an honest "the engine did not say".
 */
function overlay(authored: Authored, derived: Derived | undefined): OrgIndex | null {
  if (!derived || typeof derived !== "object") return null;
  const dUnits = list(derived.units);
  const dSeats = list(derived.seats);
  if (dUnits.length !== authored.units.length || dSeats.length !== authored.seats.length) {
    return null;
  }

  const units = link(authored);
  for (let i = 0; i < units.length; i++) {
    const d = dUnits[i];
    const unit = units[i]!;
    // BY KEY AND NAME, in position: the key is what tells two units sharing a
    // name apart, and the name is what the tree draws.
    if (!d || (d.id ?? "") !== unit.id || d.name !== unit.name) return null;
    unit.type = d.type || unit.type;
    unit.originKey = d.origin_key ?? "";
    unit.formerKeys = list(d.former_keys);
    unit.channel = d.channel ?? "";
    unit.channelInherited = !!d.channel_inherited;
    unit.leadInherited = !!d.lead_inherited;
  }

  // Seats pair by NAME, and a name that repeats (legal: a name is prose and
  // nothing holds it unique) is not paired by position. The
  // engine's order is not the document's — a root seat moved into a unit comes
  // after that unit's own seats — so the first "Designer" the engine lists can
  // be the second one the document wrote, and pairing them in turn would draw
  // one seat's goal under the other's handle. A declared handle is what tells
  // two such seats apart, so a seat pairs with the authored one declaring its
  // handle, or else with one declaring none.
  const unpaired = new Map<string, OrgSeat[]>();
  for (const { raw } of authored.seats) {
    const name = raw.name ?? "";
    unpaired.set(name, [...(unpaired.get(name) ?? []), raw]);
  }
  const claim = (name: string, handle: string): OrgSeat | null => {
    const candidates = unpaired.get(name) ?? [];
    let at = candidates.findIndex((raw) => raw.handle === handle);
    if (at < 0) at = candidates.findIndex((raw) => !raw.handle);
    return at < 0 ? null : candidates.splice(at, 1)[0]!;
  };
  const byHandle = new Map<string, Seat>();
  const seats: Seat[] = [];
  for (const d of dSeats) {
    if (!d.handle || byHandle.has(d.handle)) return null;
    const raw = claim(d.name, d.handle);
    if (!raw) return null;
    const seat = newSeat(raw, d.handle, d.handle);
    // The derived kind is the ENGINE'S reading of the field, so a value off
    // the wire this build does not know is the same value everywhere.
    seat.kind = d.kind === "human" ? "human" : "agent";
    seat.placedByRef = !!d.placed_by_ref;
    seat.originHandle = d.origin_handle ?? "";
    seat.formerHandles = list(d.former_handles);
    byHandle.set(d.handle, seat);
    seats.push(seat);
  }

  const resolve = (handles: string[] | null | undefined): Seat[] | null => {
    const out: Seat[] = [];
    for (const handle of list(handles)) {
      const seat = byHandle.get(handle);
      if (!seat) return null;
      out.push(seat);
    }
    return out;
  };

  for (let i = 0; i < units.length; i++) {
    const d = dUnits[i]!;
    const unit = units[i]!;
    const members = resolve(d.seats);
    if (!members) return null;
    for (const seat of members) {
      if (seat.unit) return null;
      seat.unit = unit;
      seat.unitChain = unit.chain;
    }
    unit.seats = members;
    if (d.lead) {
      const lead = byHandle.get(d.lead);
      if (!lead) return null;
      unit.effectiveLead = lead;
    }
  }
  for (const seat of seats) seat.unitLead = seat.unit?.effectiveLead?.name ?? "";

  for (let i = 0; i < dSeats.length; i++) {
    const d = dSeats[i]!;
    const seat = seats[i]!;
    const managers = resolve(d.managers);
    const reports = resolve(d.reports);
    const autoReports = resolve(d.auto_reports);
    if (!managers || !reports || !autoReports) return null;
    seat.managers = managers;
    seat.reports = reports;
    seat.autoReports = autoReports;
    if (d.manager) {
      const manager = byHandle.get(d.manager);
      if (!manager) return null;
      seat.manager = manager;
    }
  }

  return {
    hierarchy: true,
    seats,
    rootSeats: seats.filter((s) => !s.unit),
    units,
    topUnits: units.filter((u) => !u.parent),
    byHandle,
  };
}

/**
 * The authored tree with nothing derived: what an engine that sends no
 * `derived` block leaves a reader able to state.
 */
function authoredOnly(authored: Authored): OrgIndex {
  const units = link(authored);
  // A key is the declared handle where that is unique, and the seat's position
  // otherwise. The position key carries a `#`, which no handle can (a handle
  // is lower-case letters, digits and hyphens), so a seat declaring the handle
  // `s1` cannot collide with the second seat's position key.
  const keys = new Set<string>();
  const seats = authored.seats.map(({ raw, container }, i) => {
    const handle = raw.handle ?? "";
    const key = handle && !keys.has(handle) ? handle : `#${i}`;
    keys.add(key);
    const seat = newSeat(raw, handle, key);
    if (container >= 0) {
      const unit = units[container]!;
      seat.unit = unit;
      seat.unitChain = unit.chain;
      unit.seats.push(seat);
    }
    return seat;
  });
  const byHandle = new Map<string, Seat>();
  for (const seat of seats) {
    if (seat.handle && !byHandle.has(seat.handle)) byHandle.set(seat.handle, seat);
  }
  // A lead the unit DECLARES is a fact of the document and names a seat by its
  // exact name; an inherited one is the engine's conclusion, so it stays
  // unknown here rather than being cascaded by a second implementation.
  const firstByName = new Map<string, Seat>();
  for (const seat of seats) if (!firstByName.has(seat.name)) firstByName.set(seat.name, seat);
  for (const unit of units) {
    unit.effectiveLead = unit.lead ? (firstByName.get(unit.lead) ?? null) : null;
  }
  for (const seat of seats) seat.unitLead = seat.unit?.effectiveLead?.name ?? "";
  return {
    hierarchy: false,
    seats,
    rootSeats: seats.filter((s) => !s.unit),
    units,
    topUnits: units.filter((u) => !u.parent),
    byHandle,
  };
}

/** Index the org projection once. Everything a screen needs comes off this. */
/**
 * Fill every unit's [Unit.allSeats] from its own members and its children's.
 *
 * ONE REVERSE PASS, which is all it takes: `units` is depth first, so a child
 * always sits after its parent and walking backwards means every child is
 * finished before the parent that reads it. The obvious alternative — recursing
 * from each top unit — is the same work and re-enters a shared subtree once per
 * ancestor; the other obvious one, filtering the roster per unit, is what the
 * two surfaces this replaces were each doing separately.
 *
 * Order is a unit's own members first, then its children's in tree order, so a
 * caller that lists the array reads it the way the chart draws it.
 */
function fillSubtrees(units: Unit[]): void {
  for (let i = units.length - 1; i >= 0; i--) {
    const unit = units[i]!;
    unit.allSeats = [...unit.seats, ...unit.children.flatMap((c) => c.allSeats)];
  }
}

export function indexOrg(org: OrgProjection | null | undefined): OrgIndex {
  const authored = walk(org);
  const built = overlay(authored, org?.derived) ?? authoredOnly(authored);
  // AFTER whichever half built the tree, because both build one and the pass
  // reads only `seats` and `children` — which both of them have set by here.
  fillSubtrees(built.units);
  return built;
}

// ---------------------------------------------------------------------------
// A unit's headcount, and a seat's
// ---------------------------------------------------------------------------

/**
 * A unit's headcount, both ways round, and what sits under it.
 *
 * TWO NUMBERS, BECAUSE A UNIT THAT HOLDS UNITS HAS TWO HONEST ANSWERS. The
 * whole reason this type exists is that every surface used to pick one of them
 * and draw it bare: "Leadership 5" in the workspace rail was its whole
 * subtree, the org chart's block under it counted its own members, and the
 * roster's unit group counted a third figure.
 */
export interface UnitTally {
  /** Seats whose own unit is this one. */
  direct: number;
  /** Seats in this unit and in every unit under it. */
  total: number;
  /** Units directly under this one. */
  subUnits: number;
}

export function unitTally(unit: Unit): UnitTally {
  return {
    direct: unit.seats.length,
    total: unit.allSeats.length,
    subUnits: unit.children.length,
  };
}

/**
 * What a unit's headline number counts, in the one sentence every surface says
 * it with. A number beside a name is read as "how many there are", and one
 * that is really something else tells a reader something false about their own
 * company — the rule [FacetRail] already makes a required prop for a chip.
 */
export const UNIT_TOTAL_HINT = "seats in this unit and everything under it";

/**
 * A unit's headcount AS WORDS: what a chart block draws.
 *
 * The subtree comes FIRST because that is the question a reader asks of a team
 * — "how big is Engineering" — and the direct count is appended only where the
 * two differ: a unit with no sub-units has one honest number, and "4 seats, 4
 * directly" reads as two facts about a team that has one.
 */
export function unitSeatsLabel(tally: UnitTally): string {
  return tally.direct === tally.total
    ? plural(tally.total, "seat")
    : `${plural(tally.total, "seat")}, ${tally.direct} directly`;
}

/**
 * The other half, for a surface that can only ever show direct members: a seat
 * sits in exactly one group, so a roster grouped by unit is the unit's own
 * members and nothing under it. Spelled here rather than on that screen so
 * "directly" stays one word across the product.
 */
export function unitDirectLabel(n: number): string {
  return `${plural(n, "seat")} directly in it`;
}

/**
 * What a seat's direct-report COUNT is made of, in the one line under it.
 *
 * THE CAPTION BREAKS THE NUMBER DOWN AND NEVER NAMES ANOTHER RELATION. The
 * tile read "reports to <manager>" beneath a count of who reports to THIS seat
 * — the opposite direction, in the line a reader takes as that number's own
 * footnote. The manager is already an object-header fact and a "Who this is"
 * row on the same screen, so the caption was spending itself on a duplicate of
 * the one relation the number is not.
 *
 * WHAT IT SAYS INSTEAD is where the reports came from, because that is the
 * question a surprising count actually raises: a seat that leads a unit manages
 * that unit's direct members without anybody writing it down, and a lead
 * looking at a number larger than their own `manages:` list has no other way to
 * find out why. [Seat.autoReports] is the engine's own subset, so this is a
 * reading of what the engine derived rather than a rule re-applied here.
 *
 * AND WITHOUT THE DERIVED BLOCK THERE IS NO COUNT TO BREAK DOWN. The tile draws
 * a marked absence in that case, so the caption says what is missing rather
 * than explaining a number that is not on screen.
 */
export function reportsCaption(seat: Seat, hierarchy: boolean): string {
  if (!hierarchy) return "this engine did not report its hierarchy";
  const total = seat.reports.length;
  if (total === 0) return "nobody reports to this seat";
  const auto = seat.autoReports.length;
  if (auto === 0) return "all from its manages list";
  if (auto === total) return "all by leading a unit";
  return `${total - auto} from its manages list, ${auto} by leading a unit`;
}

/**
 * Whether `lead` is anywhere above `handle` in the chart — a lead in their
 * LINE — or null where this client cannot say.
 *
 * ANY ANCESTOR, not only the direct manager, which is the engine's own reading
 * of "a lead may set what somebody in their line does next" (`leadsOf` in
 * `cmd/crewlet/main.go` walks every manager up the chain): a founder leads
 * everybody, and an authority that stopped one level up would make a line mean
 * the people directly under you. The walk is over [Seat.managers], the
 * engine's derived relation, never a second reading of `manages:` here.
 *
 * NULL IS NOT FALSE. Without the engine's derived hierarchy every reporting
 * line is unknown ([OrgIndex.hierarchy]), and a screen that turned that into
 * "you do not lead them" would refuse somebody the chart simply did not
 * describe — so the caller says which of the two it is.
 */
export function leadsInLine(index: OrgIndex, lead: string, handle: string): boolean | null {
  if (!index.hierarchy) return null;
  if (!lead || !handle || lead === handle) return false;
  const start = index.byHandle.get(handle);
  if (!start) return false;
  // A CONFIG CAN EXPRESS A MANAGEMENT CYCLE, which the engine's own walk
  // ends rather than refuses (`internal/org/hierarchy.go`), so this one
  // remembers where it has been rather than looping the tab forever.
  const seen = new Set<string>([start.key]);
  const queue = [...start.managers];
  while (queue.length > 0) {
    const next = queue.shift()!;
    if (next.handle === lead) return true;
    if (seen.has(next.key)) continue;
    seen.add(next.key);
    queue.push(...next.managers);
  }
  return false;
}

/**
 * The round a live call is on, ONE-BASED: `round_num` is the engine's
 * zero-based round and `rounds_used` the same count one-based — the rounds
 * that came back, and the one in flight from the frame the loop publishes as
 * its provider call is made — so the round is whichever of `round_num + 1`
 * and `rounds_used` is ahead, and never less than ONE while there is a call,
 * 0 for none. Every
 * "round x of y" the product draws — the stepper, the peek, a task card's
 * strip — reads it here, because each that read `round_num` raw named a round
 * one lower than the others.
 *
 * AT LEAST ONE, because a call's first frame IS its first round. The opening
 * frame (`round_num` -1) is published immediately before the phase's first
 * provider call, and it carries the granted cap precisely so a row can say
 * "round 1 of 24" before the model has answered once
 * (`internal/agent/runner/telemetry.go`). Read as round zero, a slow first
 * answer drew a bare "Execute" with no round for as long as the model took —
 * eight seconds on the harness's slowed stub — while the card beside it said
 * "starting" and the task strip said "round 1".
 */
export function roundOf(call: LiveCall | null | undefined): number {
  return call ? Math.max(1, call.rounds_used ?? 0, (call.round_num ?? -1) + 1) : 0;
}

/**
 * WHICH ROUND A LIVE CALL IS ON, as a reader reads it — and the one value that
 * is not a round at all.
 *
 * THE NUMBER IS [roundOf]'s, so the roster's card and the attention queue name
 * the round the stepper, the peek and a task's strips name: this helper once
 * decoded `round_num` itself, a second reading of one field beside the one the
 * rest of the product shares. What it adds is the HINT for a first round that
 * has not come back — `round_num` is `-1` then, which is round one in flight
 * rather than a missing value — and the word for no call at all.
 *
 * A `hint` rather than a second word on screen, because the card has room for a
 * short label and not for a clause; the clause is what a reader gets on hover
 * and what assistive technology reads.
 */
export function roundLabel(call: LiveCall | null | undefined): {
  text: string;
  hint: string;
} {
  if (!call) {
    return { text: "starting", hint: "the turn has begun and no model round has opened yet" };
  }
  const round = roundOf(call);
  return {
    text: `round ${round}`,
    hint:
      (call.round_num ?? -1) < 0 && (call.rounds_used ?? 0) === 0
        ? "the first model round is in flight and has not come back"
        : "the model round this turn is on, counting from one",
  };
}

// ---------------------------------------------------------------------------
// The guarded half: what only the org chart's own read says
// ---------------------------------------------------------------------------

/**
 * WHAT THIS READER CAN SAY ABOUT A SEAT'S GUARDED HALF, read off the org
 * chart (`GET /chart/seats/{handle}`, and the home unit's
 * `GET /chart/units/{key}` for the credentials its members inherit).
 *
 * SIX OUTCOMES, NOT A NULLABLE SEAT. "The chart holds no seat by this handle",
 * "you may not read the chart", "you may read the chart and not the seat's
 * runtime half", "the engine could not answer", "the read has not come back
 * yet" and "here it is" are six different facts — and a `ChartSeat | null`
 * collapses five of them into one. Every screen that did that printed the
 * same sentence for all of them, and the sentence it picked was the reader's:
 * the seat header's MODEL fact read "needs an operator token" on five of
 * eight tabs, because those tabs simply did not ask.
 *
 * ONLY `refused` IS A REFUSAL, and `stripped` is not one: the chart served
 * the seat's rows without the runtime half — its model chain, budget, contact
 * identities, tool credentials and schedules — because reading that half
 * takes the grant that reads the company's configuration, and it SAID so
 * (`runtime: false`). A refusal carries the grants the engine named, because
 * what a signed-in reader lacks is a grant, never "an operator token".
 *
 * IT WAS THE COMPANY DOCUMENT, found by the seat's NAME. The document holds
 * no seats any more — the org chart left it for a log of its own — so that
 * lookup found nothing for every seat in every company, and a name was never
 * an address anyway: two seats may share one. The chart is read by the
 * HANDLE, which is what every reference names a seat by.
 */
export type SeatReading =
  | { state: "read"; seat: ChartSeat; unit: ChartUnit | null }
  /** The chart answered without the runtime half: the reader may not read it. */
  | { state: "stripped"; seat: ChartSeat }
  /** The chart holds no seat by this handle. */
  | { state: "absent" }
  /**
   * The engine refused the read on authority. `grants` are the ones it named,
   * any ONE of which would admit this reader — empty for a 401, where nothing
   * the engine accepted was presented — and `reason` the deciding rule's.
   */
  | { state: "refused"; grants: readonly string[]; reason: string }
  /**
   * The engine could not answer, or nothing came back from it: a node
   * catching up, a fault, a request past its deadline. `failure` says which,
   * in the terms `QueryState` draws — see `ChartReading`.
   */
  | { state: "failed"; failure: RestFailure }
  /** Nothing has been asked, or nothing has come back. Never a claim. */
  | { state: "unread" };

/**
 * The seat reading from the two chart reads it is made of: the seat's own,
 * and — when it sits in a unit — that unit's, which carries the tool
 * credentials its direct agent members inherit.
 *
 * THE UNIT IS PART OF THE ANSWER, so a unit read still out is an answer still
 * out: a seat's credentials drawn without the ones it inherits would be a
 * list that shrinks when the second read lands. A unit the chart no longer
 * holds (a row a record left dangling) is no unit rather than a failure.
 */
export function seatReading(
  seat: ChartReading<ChartSeatRead>,
  unit: ChartReading<ChartUnitRead> | null,
): SeatReading {
  if (seat.state !== "read") return seat;
  const row = seat.value.seat;
  if (!seat.value.runtime) return { state: "stripped", seat: row };
  if (!row.unit || unit === null) return { state: "read", seat: row, unit: null };
  switch (unit.state) {
    case "read":
      return { state: "read", seat: row, unit: unit.value.unit };
    case "absent":
      return { state: "read", seat: row, unit: null };
    default:
      return unit;
  }
}

/**
 * What [useSeatSetup] answers. A type of its own so a screen that READ it once
 * can hand it to every panel drawing it: two callers of the hook are two
 * reads of the seat and of its home unit.
 */
export interface SeatSetup {
  /** The seat the address names — followed through a rename — or undefined. */
  seat: Seat | undefined;
  /** What this reader can say about the seat's guarded half. See [SeatReading]. */
  reading: SeatReading;
}

/**
 * The grant the chart serves a runtime half under, and the org projection a
 * seat's RESOLVED setup under (`config:read`): its model chain and its tool
 * sources are derived from that half, so they follow its rule.
 */
export const RUNTIME_GRANT = "config:read";

/**
 * What it means that the org projection carried no model chain (`llm`) or no
 * tool sources (`tool_sources`) for an agent seat — which depends on WHO is
 * reading, because the engine strips both for every audience without
 * `config:read` (`internal/api`'s `OrgProjection.For`):
 *
 *   - `none` — the reader holds the grant, so the engine would have sent the
 *     value, and its absence is the seat's own: no provider, no tools;
 *   - `withheld` — the reader does not, so the absence says nothing about the
 *     seat at all. Read as `none`, every such reader was told "No provider
 *     configured" and "none granted" about a seat that has both;
 *   - `unknown` — the viewer has not answered, which claims nothing either way.
 */
export type ResolvedAbsence = "none" | "withheld" | "unknown";

/** See [ResolvedAbsence]. */
export function resolvedAbsence(viewer: {
  readonly grants: readonly string[];
  readonly loading: boolean;
}): ResolvedAbsence {
  if (viewer.grants.includes(RUNTIME_GRANT)) return "none";
  return viewer.loading ? "unknown" : "withheld";
}

/**
 * The sentence a reader without `config:read` reads where a resolved value
 * would be — `what` is the value, "its model chain" — naming the grant that
 * would show it rather than an empty value nobody could tell from a seat that
 * has none.
 */
export function resolvedWithheld(what: string): string {
  return needsSentence(`Reading ${what}`, [RUNTIME_GRANT]);
}

/**
 * The guarded half of one seat, for any screen that draws it: the org chart's
 * own row for the seat and its home unit, read by HANDLE through
 * `lib/chartReads.ts`, and the six-way [seatReading] of them.
 *
 * NEVER `/config`: the company document holds no seats, and a seat found in it
 * by NAME was nothing for every seat in every company. The seat is resolved
 * from the address a route carries ([seatByAddress]), so a link kept from
 * before a rename opens the seat it named.
 *
 * ASKED BY WHAT THE READER HOLDS. Nobody is asked anything until the viewer
 * has answered, nor for an anonymous one — both are known before the
 * question, and asking only puts a refusal on the wire and a banner over a
 * page that never had a chance; such a reader stays `unread`, which claims
 * nothing, and the screen says what signing in would show. The RUNTIME half is
 * asked for (`?runtime=true`) only by a reader holding `config:read`, the
 * grant the chart serves it under; anybody else is asked for the rows alone,
 * which the chart answers without the half and SAYS so, so the reading is
 * `stripped` rather than a runtime request the engine was always going to
 * strip.
 *
 * RE-READ ON EVERY ORG PUSH (`useOrgPushes`), which is what follows a chart
 * write that landed — a change to the runtime half included, which the
 * projection itself does not carry.
 */
export function useSeatSetup(address: string): SeatSetup {
  const org = useOrg();
  const pushes = useOrgPushes();
  const viewer = useViewer();
  const index = useMemo(() => indexOrg(org), [org]);
  const seat = (address ? seatByAddress(index, address) : null) ?? undefined;
  const ask = !!seat?.handle && !viewer.loading && !viewer.anonymous;
  const query = viewer.grants.includes(RUNTIME_GRANT) ? WITH_RUNTIME : undefined;
  const seatRead = useChartRead<ChartSeatRead>(
    ask && seat ? chartSeat(seat.handle) : null,
    query,
    pushes,
  );
  const home = seatRead.state === "read" ? (seatRead.value.seat.unit ?? "") : "";
  const unitRead = useChartRead<ChartUnitRead>(home !== "" ? chartUnit(home) : null, query, pushes);
  const reading = useMemo(
    () => seatReading(seatRead, home !== "" ? unitRead : null),
    [seatRead, unitRead, home],
  );
  return useMemo(() => ({ seat, reading }), [seat, reading]);
}

/**
 * The tool credentials a seat runs with: its home unit's `mcp_env` merged down,
 * the seat's OWN entries winning per variable.
 *
 * Per VARIABLE rather than per server, which is `org.MCPEnv`'s own rule: a seat
 * that overrides one header must not silently drop the token beside it. A
 * human seat inherits none, because it runs no tools (`org.inheritMCPEnv`).
 */
export function mcpEnvOf(
  reading: SeatReading,
  kind: Seat["kind"],
): Record<string, Record<string, string>> {
  if (reading.state !== "read") return {};
  const out: Record<string, Record<string, string>> = {};
  if (kind === "agent") {
    for (const [server, vars] of Object.entries(reading.unit?.runtime?.mcp_env ?? {})) {
      out[server] = { ...(out[server] ?? {}), ...vars };
    }
  }
  for (const [server, vars] of Object.entries(reading.seat.runtime?.mcp_env ?? {})) {
    out[server] = { ...(out[server] ?? {}), ...vars };
  }
  return out;
}

/**
 * The provider keys one phase's chain names, first choice first.
 *
 * TWO SHAPES REACH THIS BROWSER, because `org.ProviderKeys` marshals as a
 * string for one provider and an array for a fallback chain. The type once
 * declared a string, so a chain rendered as `fast,backup` and any consumer
 * that called a string method on one threw on a seat the engine accepts.
 *
 * An empty key is dropped rather than listed — a seat that names nothing takes
 * the default provider, which is not a provider called "" — and a key listed
 * twice is one model.
 */
export function llmChain(llm: ProviderKeys | undefined): string[] {
  if (llm == null) return [];
  const keys = typeof llm === "string" ? [llm] : Array.isArray(llm) ? llm : [];
  return dedupe(keys.filter((key) => typeof key === "string" && key !== ""));
}

function dedupe(keys: string[]): string[] {
  return keys.filter((key, i) => keys.indexOf(key) === i);
}

// ---------------------------------------------------------------------------
// Live state
// ---------------------------------------------------------------------------

/**
 * Name a handle by what the chart calls it, and say which kind of seat it is.
 *
 * THE TWO THINGS [SeatCell] NEEDS AND A BARE HANDLE CANNOT SUPPLY, so it is
 * wanted by every screen that renders a handle out of an answer — an item's
 * assignee, a project's lead, a saved view's author. It was written twice,
 * once per file, with the two copies already differing in the name of a local
 * variable; the next difference would have been which of them falls back to
 * the handle.
 *
 * FALLS BACK TO THE HANDLE rather than to an em dash: a handle that is not in
 * the chart is a seat that was renamed or removed, and the tracker still
 * holds its work. The name it was filed under is what a reader needs to find
 * that work, and a dash would lose it.
 */
export function seatLookup(index: OrgIndex): (handle: string) => { name: string; kind?: SeatKind } {
  return (handle) => {
    const seat = index.byHandle.get(handle);
    return seat ? { name: seat.name, kind: seat.kind } : { name: handle };
  };
}

/**
 * A seat by its ADDRESS — the pair a badge needs — through [seatByAddress]:
 * its handle, a handle it answered to before a rename, or the name of a seat
 * the engine reported no handle for.
 *
 * NEVER BY NAME OTHERWISE. A name is prose two seats may share, and a badge
 * looked up by one drew the first namesake's kind and name over the second's
 * row; every row that names a seat carries its handle now (a turn, a budget
 * and a spend row by the seat they pair with on agent id). A key that names no
 * seat is drawn as it came: a renamed-away handle nobody holds, or a login.
 */
export function seatBadgeOf(index: OrgIndex): (key: string) => { name: string; kind?: SeatKind } {
  return (key) => {
    const seat = seatByAddress(index, key);
    return seat ? { name: seat.name, kind: seat.kind } : { name: key };
  };
}

/**
 * A seat's name and kind by its address, off the chart the store holds.
 *
 * HERE, BESIDE [seatBadgeOf], rather than in `store-hooks.ts`, which it used
 * to be: that put `store-hooks.ts → seats.ts → useQuery.ts → store-hooks.ts`
 * in the import graph, a cycle whose evaluation order decided whether a
 * suite's mock of the store hooks reached `useQuery` at all — so adding an
 * unrelated import to a screen could turn a passing suite's queries into
 * "useClient outside a ClientContext provider". This module already reads
 * the store; the store no longer reads this module.
 */
export function useSeatBadgeOf(): (key: string) => { name: string; kind?: SeatKind } {
  const org = useOrg();
  return useMemo(() => seatBadgeOf(indexOrg(org)), [org]);
}

/**
 * The same two answers, as the pair a ROW RENDERER takes.
 *
 * A grid cell is handed resolvers rather than the chart — see the row chrome
 * every tracker surface passes down — because a card that could reach an org
 * context would need one to render, and these draw in tests and in a peek
 * panel with no provider above them.
 *
 * THE PAIR RATHER THAN ONE FUNCTION EACH, because the half that was missing is
 * exactly the half every screen forgot: seven chrome builders each wrote their
 * own name resolver inline and not one of them carried the kind, so a human
 * seat was drawn as an agent on every tracker surface in the product. Spread
 * into the chrome, a builder cannot thread one and drop the other.
 *
 * THE KIND IS UNDEFINED WHERE THE CHART HAS NO SUCH SEAT, and that is a third
 * answer rather than a missing one: "this is an agent" and "this company has
 * no such seat" must not collapse, because the second is how a renamed or
 * removed seat still appears on the work it was filed against. The name falls
 * back to the handle for the same reason. The kit's badge has two outlines and
 * no third, so a renderer draws an unknown kind with the kit's default (the
 * agent's squircle) — which is why a screen holding its writers' recorded
 * kinds layers them on with [kindWithAuthors] before it draws anybody.
 */
export function seatResolvers(index: OrgIndex): {
  seatName: (handle: string) => string;
  seatKind: (handle: string) => SeatKind | undefined;
} {
  const lookup = seatLookup(index);
  return {
    seatName: (handle) => lookup(handle).name,
    seatKind: (handle) => lookup(handle).kind,
  };
}

/**
 * The kind of a writer the CHART does not hold, from what the record says
 * wrote it.
 *
 * AN OPERATOR IS SOMEBODY WHO IS NOT A SEAT. A write by a caller the identity
 * directory binds to no seat — a person outside the chart, a pipeline's
 * token, through the dashboard or `/operator/mcp` alike — carries their whole
 * LOGIN as its author, with author kind `operator` (`iam.ActorFor`) — so a
 * task one of them filed has a reporter no chart lists, and the chart's
 * answer for it is "no such seat". Drawn from that alone the badge fell to the
 * kit's default outline, the agent's squircle, and the one fact the outline
 * encodes was wrong for every operator-authored write on the item page.
 *
 * The record already says it: every change row and every comment carries its
 * writer's `actor_kind` / `author_kind`. So a handle the chart misses takes
 * the kind its own writes were recorded under — `agent` for an agent,
 * `human` and `operator` for a person. `system` names no one and is left
 * unresolved, and so is a handle the answer never saw write anything.
 * The CHART still wins where it has the seat: it is the declaration, and a
 * row is one writer's claim about one commit.
 */
export function authorKinds(
  rows: readonly { actor?: string; actor_kind?: string; author?: string; author_kind?: string }[],
): Map<string, SeatKind> {
  const out = new Map<string, SeatKind>();
  for (const row of rows) {
    const who = row.actor ?? row.author;
    const kind = kindOfAuthor(row.actor_kind ?? row.author_kind);
    if (who && kind && !out.has(who)) out.set(who, kind);
  }
  return out;
}

/** An author kind off the wire, as the badge's outline; undefined names no one. */
export function kindOfAuthor(kind: string | undefined): SeatKind | undefined {
  switch (kind) {
    case "agent":
      return "agent";
    case "human":
    case "operator":
      return "human";
    default:
      return undefined;
  }
}

/** The chart's kind for a handle, or else the kind its recorded writes carry. */
export function kindWithAuthors(
  seatKind: (handle: string) => SeatKind | undefined,
  authors: ReadonlyMap<string, SeatKind>,
): (handle: string) => SeatKind | undefined {
  return (handle) => seatKind(handle) ?? authors.get(handle);
}

// ---------------------------------------------------------------------------
// What a seat is doing: the engine's word, mapped and never derived
// ---------------------------------------------------------------------------
//
// THE ENGINE COMPUTES ONE WORD PER SEAT (`activity`: working, needs, stopped,
// idle) from its turn, its coding runs' durable record, its pause, its
// placement across the fleet and its budget windows, and serves it on every
// `agents` row with the reason beside it (`stopped_reason`, `paused`,
// `last_turn`). This client used to compute three words of its own from a
// fraction of those — the sidebar off an old `state`, the live screen and this
// library each folding the running-runs panel a different way — and a run
// parked past a twelve-hour age-out dropped out of every ring at once.
//
// So everything below is a MAPPER: the engine's word to a ring tone, a label
// and a line of prose. Nothing here, and nothing anywhere else, reads a live
// call's state to decide whether a seat is working — `seats.test.ts` holds
// that as a source gate over the whole tree.

/**
 * What a screen draws for a seat: the engine's word, or `offline` for a seat
 * no row is held for yet (a seat just added, a node that has not pushed).
 */
export type SeatState = SeatActivity | "offline";

/** The engine's word for a seat, or `offline` while no row is held for it. */
export function activityOf(row: AgentRow | null | undefined): SeatState {
  return row?.activity ?? "offline";
}

/**
 * The seats the engine says are WORKING, the turn that has been going longest
 * first — the order every list of running turns draws (Home's Live now, Live ›
 * Now running), because the longest-running turn is the one a reader is most
 * likely looking for, and two lists of one set in two orders read as two sets.
 */
export function workingLongestFirst(agents: readonly AgentRow[]): AgentRow[] {
  return agents
    .filter((a) => activityOf(a) === "working")
    .sort(
      (a, b) =>
        (Date.parse(a.turn?.started_at ?? "") || 0) - (Date.parse(b.turn?.started_at ?? "") || 0),
    );
}

/**
 * Whether a detached coding run is waiting on a person.
 *
 * THE ENGINE'S OWN TWO WORDS: `awaiting_clarification`, and `reseed` — the box
 * was reaped past its pause TTL, so the work is gone and only the question
 * survives (`sandbox.Awaiting` counts it too). It classifies a RUN for the
 * runs panel; what a SEAT is doing is `activity`.
 */
export function awaitingPerson(status: string | undefined): boolean {
  return status === "awaiting_clarification" || status === "reseed";
}

/**
 * The live row for a seat of the org index: the roster row carrying the same
 * HANDLE, or undefined for a seat the roster does not carry (a person, or a
 * seat this node has no chart row for yet).
 *
 * BY HANDLE, because both halves carry the handle the seat answers to NOW —
 * the org projection and the roster are cut from the same chart view — and a
 * handle is unique. It paired by NAME, which is prose two seats may share, so
 * the second "Engineer" in a company wore the first one's state, its live call
 * and its sandbox, on every screen that drew it. A seat with no known handle
 * (an engine that reports no derived hierarchy) pairs with nothing rather than
 * with a namesake.
 */
export function liveRowFor(
  agents: readonly AgentRow[],
  seat: Pick<Seat, "handle"> | null | undefined,
): AgentRow | undefined {
  const handle = seat?.handle;
  if (!handle) return undefined;
  return agents.find((a) => a.handle === handle);
}

/**
 * The address a row NAMING a seat — a spend row, a budget row — opens it by:
 * its HANDLE. A row carrying none is a seat the chart no longer holds, and it
 * opens by its agent id, which answers the honest "no such seat" — where the
 * NAME it fell back to opened whichever seat of that name came first, somebody
 * else entirely. The name is never an address here, so it is not even read.
 */
export function seatAddress(row: { handle?: string; agent_id: string }): string {
  return row.handle || row.agent_id;
}

/**
 * What a list narrowed to ONE SEAT asks the engine for.
 *
 * A link and a URL carry the seat's HANDLE — the address every screen gives
 * a seat — and the engine narrows a turn list, a phase list and a spend
 * rollup by the seat's AGENT ID, which neither a rename nor a namesake moves.
 * They narrowed by the seat's NAME, so a filter on one "Engineer" listed every
 * Engineer's turns as that seat's.
 *
 * `agentId` is "" while the filter names no seat (`handle` empty) AND when it
 * names one this node cannot place — a handle no agent seat answers to — which
 * `seat` being null tells apart: a caller must not read that second case as
 * "every seat", since a filter that silently widened would answer a question
 * nobody asked.
 */
export function seatFilter(
  index: OrgIndex,
  agents: readonly AgentRow[],
  handle: string,
): { seat: Seat | null; agentId: string } {
  if (!handle) return { seat: null, agentId: "" };
  const seat = index.byHandle.get(handle) ?? null;
  return { seat, agentId: liveRowFor(agents, seat)?.agent_id ?? "" };
}

/**
 * The detached coding run a seat's row is parked on, or null — matched by
 * AGENT ID, which the run carries for exactly this and which neither a rename
 * nor a namesake moves. It matched the run's role name, so a seat whose
 * namesake was coding read as coding too.
 */
export function sandboxFor(
  sandboxes: readonly SandboxEntry[],
  agent: Pick<AgentRow, "agent_id"> | null | undefined,
): SandboxEntry | null {
  const id = agent?.agent_id;
  if (!id) return null;
  return sandboxes.find((s) => s.agent_id === id) ?? null;
}

/** The ring round a seat's badge: the one place a seat's state has a hue. */
export type SeatRing = "info" | "warning" | "danger";

/**
 * The ring for a state, and NONE for idle.
 *
 * WORKING IS INFO, NEEDS YOU IS WARNING, STOPPED IS DANGER, and an idle seat
 * draws no ring at all. An idle seat drawn as a green pill read as activity —
 * the product's own complaint was "when agent is idle it has this blob
 * lighting which feels like it is working" — and the fix for that is not a
 * duller hue, it is none. Amber is reserved for the one state that asks for a
 * person.
 */
export function ringOf(state: SeatState | undefined): SeatRing | undefined {
  switch (state) {
    case "working":
      return "info";
    case "needs":
      return "warning";
    case "stopped":
      return "danger";
    default:
      return undefined;
  }
}

/**
 * Which node holds a seat's lease, as far as the PUBLIC health push can say.
 *
 * WHAT EVERY READER IS TOLD, whatever they hold, on the profile and in the
 * peek alike. `/health` is unguarded, so a node's own name and the seats it
 * holds are already within every reader's reach: withholding "this node ·
 * node-2" in one place and printing it in the next was a rule nobody could
 * state. What the push does NOT say is WHICH peer holds a seat this node does
 * not — that is the `fleet` answer, behind `fleet:operate` — so a seat held
 * elsewhere is "another node", and a reader holding that grant is shown the
 * lease itself.
 *
 * IT WAS THE AGENT INSTANCE ID, which exists only while a turn runs — so an
 * idle seat THIS node held read "not running on this node" on its own page.
 * The engine calls a seat nobody holds `unplaced`.
 */
export function heldBy(
  handle: string,
  agent: AgentRow | undefined,
  health: EngineHealth | null,
): string {
  if (agent?.stopped_reason === "unplaced") return "no node — not placed";
  if (!health?.seats) return "not reported by this node";
  if (health.seats.includes(handle))
    return health.node ? `this node · ${health.node}` : "this node";
  return "another node";
}

/** The kit tone a state's pill takes: its ring's, or neutral where it has none. */
export function toneOf(state: SeatState | undefined): SeatRing | "neutral" {
  return ringOf(state) ?? "neutral";
}

/** The state in one lowercase word or two, for a pill. */
export function activityWord(state: SeatState): string {
  return state === "needs" ? "needs you" : state;
}

/**
 * Who a seat's pauser is, by the name a reader knows them by.
 *
 * `paused.by` is the name the engine records every author under
 * (`iam.ActorFor`): the SEAT HANDLE of a person bound to one, or the whole
 * login of anybody bound to none — an address either way, and "Paused by
 * jane-founder" names an address where a person is meant. A screen passes the
 * chart's lookup; a key the chart does not hold (a login, a seat since
 * removed) is drawn as it came.
 */
export type NameOf = (key: string) => string;

const AS_WRITTEN: NameOf = (key) => key;

/** The chart's [NameOf]: a seat handle's name, and any other key as written. */
export function nameOfIn(index: OrgIndex): NameOf {
  return (key) => index.byHandle.get(key)?.name ?? key;
}

/** Why a stopped seat cannot take work, in a sentence, keyed on the engine's reason. */
export function stoppedLine(row: AgentRow | null | undefined, nameOf: NameOf = AS_WRITTEN): string {
  switch (row?.stopped_reason) {
    case "paused":
      return row.paused?.by ? `paused by ${nameOf(row.paused.by)}` : "paused";
    case "unplaced":
      return "not placed on any node";
    case "budget":
      return "its token budget is spent until the window resets";
    case "provider":
      return "its model provider is unreachable";
    default:
      return "stopped";
  }
}

/**
 * A seat's state as its LABEL: "Working", "Needs you · run parked", "Paused by
 * Jane · 12m", "Stopped · budget", "Not placed on any node", "Idle".
 *
 * Every word is the engine's: the reason is `stopped_reason`, the pauser is
 * `paused.by`, the age is `paused.at` against the reader's clock. A reason
 * this build does not know draws "Stopped" rather than a guess.
 */
export function labelOf(
  row: AgentRow | null | undefined,
  now: number,
  nameOf: NameOf = AS_WRITTEN,
): string {
  switch (activityOf(row)) {
    case "working":
      return "Working";
    case "needs":
      return row?.turn?.stage === "parked" ? "Needs you · run parked" : "Needs you";
    case "idle":
      return "Idle";
    case "offline":
      return "No state from the engine yet";
    case "stopped":
      switch (row?.stopped_reason) {
        case "paused": {
          const by = row.paused?.by ? `Paused by ${nameOf(row.paused.by)}` : "Paused";
          const since = row.paused?.at ? shortAge(row.paused.at, now) : "";
          return since ? `${by} · ${since}` : by;
        }
        case "unplaced":
          return "Not placed on any node";
        case "budget":
          return "Stopped · budget";
        case "provider":
          return "Stopped · provider";
        default:
          return "Stopped";
      }
  }
}

/**
 * What a phase is doing, for the state line: the words alone, and the words
 * before the item the turn is on.
 */
const PHASE_DOING: Record<string, { alone: string; on: string }> = {
  context: { alone: "Reading context", on: "Reading context for" },
  onboarding: { alone: "Onboarding", on: "Onboarding on" },
  execute: { alone: "Executing", on: "Executing" },
  review: { alone: "Reviewing", on: "Reviewing" },
};

/**
 * How many workers a seat's call in flight is running: the tasks of the
 * `delegate` call it is waiting on, or 0 when its running call is anything
 * else, or none.
 *
 * OFF THE CALL'S OWN ARGUMENTS, because that is the one record that exists
 * WHILE the workers run: each worker's phase record lands when it finishes and
 * the batch's summary when the last one does, so both describe a fan-out that
 * is over. Arguments that do not read as the tool's schema count nothing
 * rather than a guess.
 */
export function delegatedWorkers(call: LiveCall | null | undefined): number {
  const running = call?.running_call;
  if (!running || running.name !== DELEGATE_TOOL) return 0;
  try {
    const args: unknown = JSON.parse(running.arguments);
    const tasks = (args as Record<string, unknown> | null)?.[DELEGATE_TASKS];
    return Array.isArray(tasks) ? tasks.length : 0;
  } catch {
    return 0;
  }
}

/**
 * What a seat is doing, in one line under its name: "Executing ENG-412",
 * "3 workers on ENG-405", "Coding run on ENG-9", "Idle · last turn 24m ago".
 *
 * THE WORK ITEM IS THE TURN'S OWN, `live_call.work_item` — the one item the
 * engine charges the turn to — and never a `work_key` a prompt happened to
 * mention. The workers are the ones the call in flight is running
 * ([delegatedWorkers]), off the same row, so every surface that draws this
 * line says the same thing about a fan-out. A human seat is not run by the
 * engine and says so.
 */
export function stateLine(
  row: AgentRow | null | undefined,
  opts: { now: number; seat?: Seat | null; nameOf?: NameOf },
): string {
  const { now, seat, nameOf = AS_WRITTEN } = opts;
  if (seat?.kind === "human") {
    return seat.availability || "Human teammate — not run by the engine";
  }
  const state = activityOf(row);
  const item = row?.live_call?.work_item?.key || row?.turn?.work_item?.key || "";
  switch (state) {
    case "working": {
      // A DETACHED CODING RUN parks the turn while the box works: the seat is
      // working and no model call is in flight, so the phase says nothing.
      if (row?.turn?.stage === "parked") {
        return item ? `Coding run on ${item}` : "Coding run in progress";
      }
      const workers = delegatedWorkers(row?.live_call);
      if (workers > 0) {
        const who = plural(workers, "worker");
        return item ? `${who} on ${item}` : `${who} running`;
      }
      const doing = PHASE_DOING[row?.live_call?.phase ?? row?.current_phase ?? ""] ?? {
        alone: "Working",
        on: "Working on",
      };
      return item ? `${doing.on} ${item}` : doing.alone;
    }
    case "idle": {
      const last = row?.last_turn?.ended_at;
      return last ? `Idle · last turn ${shortAge(last, now)} ago` : "Idle";
    }
    default:
      return labelOf(row, now, nameOf);
  }
}

/**
 * What a task's card says about the turn running on it: "SWE · executing ·
 * round 7 of 20", "AI Systems · 3 workers running", "SWE · coding run".
 *
 * JOINED ON THE ITEM THE ENGINE CHARGES THE TURN TO — `live_call.work_item`,
 * then the turn's own — and never on a `work_key`, which is the unit of work a
 * TRIGGER named and says nothing about which task the turn is spending on. A
 * card that joined on it drew a strip on whichever task the webhook happened
 * to mention.
 *
 * ONLY A WORKING SEAT, by the engine's own word (`activity`): a seat that
 * stopped mid-turn is not running anything on the task, and the ring round its
 * avatar already says which state it is in.
 */
export interface CardLive {
  /** The seat running the turn. */
  handle: string;
  /** The words after the seat's name. */
  doing: string;
  /** When the turn began, for the elapsed time at the strip's end. */
  since?: string;
}

/**
 * Every working seat's turn, keyed on the ID of the task the turn is charged
 * to — never its key. A key two tasks hold (`key_collision`) is one value for
 * two tasks, so a map keyed on it drew one task's turn on both their cards; the
 * id is the one name no two tasks share, and a card looks itself up by its own.
 */
export function liveOnItems(rows: readonly AgentRow[]): Map<string, CardLive> {
  const out = new Map<string, CardLive>();
  for (const row of rows) {
    if (row.activity !== "working") continue;
    const id = row.live_call?.work_item?.id || row.turn?.work_item?.id || "";
    const handle = row.handle ?? "";
    if (!id || !handle) continue;
    out.set(id, {
      handle,
      doing: doingWords(row),
      since: row.turn?.started_at ?? row.live_call?.started_at,
    });
  }
  return out;
}

/**
 * The words a working seat's strip carries after its name: "executing · round
 * 7 of 25", "3 workers running", "coding run". A task card's strip and the task
 * page's live row both read it here — the page kept a private copy that read
 * the zero-based `round_num` raw and called every phase but review
 * "executing", so the two named different rounds, and different phases, of
 * one turn.
 */
export function doingWords(row: AgentRow): string {
  if (row.turn?.stage === "parked") return "coding run";
  const workers = delegatedWorkers(row.live_call);
  if (workers > 0) return `${plural(workers, "worker")} running`;
  const phase = row.live_call?.phase ?? row.current_phase ?? "";
  const verb = (PHASE_DOING[phase]?.alone ?? "working").toLowerCase();
  const round = roundOf(row.live_call);
  const max = row.live_call?.max_rounds;
  if (round <= 0) return verb;
  return max ? `${verb} · round ${round} of ${max}` : `${verb} · round ${round}`;
}

/**
 * A handle as a reader sees it: `@pm`, and NOTHING for a seat the engine
 * reported no handle for. A bare `@` printed beside a name read as a handle
 * that is empty, which is a claim about the seat rather than about what this
 * client was told.
 */
export function handleLabel(handle: string | null | undefined): string {
  const h = (handle ?? "").trim();
  return h ? `@${h}` : "";
}

/**
 * "12m", "3h", "2d" — the AGE of an instant against the reader's clock: how
 * long something has been going on, which is what a seat's pause, its last
 * turn and a card's live band all say.
 *
 * NOT THE INBOX'S `shortWhen`, which is the other compact rule and answers a
 * different question — WHEN a notice arrived, so past a day it names the
 * weekday or the date. "Parked · Tue" says when a run stopped; "Parked · 2d"
 * says how long it has waited, which is what a reader deciding whether to go
 * and look needs.
 */
export function shortAge(at: string | undefined, now: number): string {
  const then = parseUTC(at)?.getTime() ?? Number.NaN;
  if (!Number.isFinite(then)) return "";
  const minutes = Math.max(0, Math.round((now - then) / 60_000));
  if (minutes < 60) return `${minutes}m`;
  const hours = Math.round(minutes / 60);
  if (hours < 48) return `${hours}h`;
  return `${Math.round(hours / 24)}d`;
}

// ---------------------------------------------------------------------------
// Staleness
// ---------------------------------------------------------------------------
//
// A live row animates, which sells motion — so a turn that has been on round 3
// for eleven minutes looked exactly like one that started two seconds ago, and
// "is anything stuck?" was unanswerable on the one screen whose whole job was
// to answer it.

/**
 * A round with no update for this long is suspicious: either a genuinely long
 * tool call (a sandbox launch, a slow MCP server) or a hang. Two minutes clears
 * every builtin tool and the engine's own model latency with room to spare.
 */
export const STALE_MS = 120_000;

/** And this long means it is not coming back — long past any provider timeout. */
export const STALLED_MS = 600_000;

export function staleness(
  updatedAt: string | undefined,
  now: number,
  stage?: string | null,
): "" | "stale" | "stalled" {
  // A PARKED TURN IS SILENT ON PURPOSE. A detached coding run suspends the
  // loop and the round stops moving by design — for as long as the run takes,
  // and for as long as a question waits on a person — so "no update for ten
  // minutes" is its normal state, and an alarm keyed on it fired on every
  // legitimately silent run.
  if (stage === "parked") return "";
  if (!updatedAt) return "";
  const age = now - new Date(updatedAt.endsWith("Z") ? updatedAt : `${updatedAt}Z`).getTime();
  if (!Number.isFinite(age)) return "";
  if (age >= STALLED_MS) return "stalled";
  if (age >= STALE_MS) return "stale";
  return "";
}

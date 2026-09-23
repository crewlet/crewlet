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
 * projection carries a charter and a tree and nothing else: a seat's email,
 * model chain, token budget, contact identities, tool credentials,
 * integrations and schedules are read from the company document through the
 * operator-gated `config` query, which is what [seatSettings] and
 * [unitSettings] below are for.
 *
 * Resolved ONCE, into an index, and screens consume seats. Doing it per screen
 * is how the previous dashboard ended up walking the whole roster once per
 * rendered row: `managerOf` was a linear scan called per seat AND again per
 * row, which on a 200-seat company was roughly 80,000 array scans per push.
 */

import { plural } from "./format.ts";
import type {
  AgentRow,
  CompanyDocument,
  ConfigRole,
  ConfigUnit,
  Derived,
  OrgProjection,
  OrgSeat,
  OrgUnit,
  PhaseLLM,
  ProviderKeys,
  QueryErrorCode,
  QueryRefusal,
  SandboxEntry,
  ScheduleSpec,
} from "~/protocol/index.ts";

/** One unit, as the projection wrote it and as the engine resolved it. */
export interface Unit {
  /** Stable React key and DOM id suffix: the unit's depth-first position. */
  key: string;
  /**
   * A unit is ADDRESSED BY NAME, here and in every route.
   *
   * `org.Unit.Key` prefers the declared `id:`, and this client cannot: the id
   * is GUARDED — `internal/api/orgprojection_test.go` classifies it as "a
   * durable key rather than a name: everything filed against the unit keys on
   * it and nobody reads it, while the chart draws Name" — so the anonymous
   * projection carries no id at all, and a link built from one would be a
   * link built from a value this screen was never given. Read it through
   * [unitSettings] where a token allows and something actually needs the key.
   */
  name: string;
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
  kind: "agent" | "human";
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
  /** The FIRST seat with each name. */
  byName: Map<string, Seat>;
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
  return ["company", "people", seat.handle || seat.name];
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
    name: raw.name ?? "",
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
 * is CHECKED rather than assumed: the same number of units with the same names
 * in the same order, every seat accounted for exactly once, and every handle
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
    if (!d || d.name !== unit.name) return null;
    unit.type = d.type || unit.type;
    unit.channel = d.channel ?? "";
    unit.channelInherited = !!d.channel_inherited;
    unit.leadInherited = !!d.lead_inherited;
  }

  // Seats pair by NAME, and a name that repeats (only possible in a revision
  // stored before names had to be unique) is not paired by position. The
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
    byName: new Map(),
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
    byName: new Map(),
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
  for (const seat of built.seats) {
    if (!built.byName.has(seat.name)) built.byName.set(seat.name, seat);
  }
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
 * WHICH ROUND A LIVE CALL IS ON, as a reader reads it — and the one value that
 * is not a round at all.
 *
 * `round_num` is the engine's ZERO-BASED counter, so the number a person reads
 * is `round_num + 1`; a row printing the raw field named a round one lower than
 * the one the phase card beside it showed. And the field is `-1` before the
 * first model round has come back, which is not round zero and is not a missing
 * value: it is a turn that has started and is waiting. Drawn as a dash, two of
 * five working seats on the roster read "round —" with nothing saying why — the
 * one fact the sentinel carries.
 *
 * A `hint` rather than a second word on screen, because the card has room for a
 * short label and not for a clause; the clause is what a reader gets on hover
 * and what assistive technology reads.
 */
export function roundLabel(roundNum: number | null | undefined): {
  text: string;
  hint: string;
} {
  // `< 0` RATHER THAN `=== -1`, and `== null` for a field an older engine may
  // not send at all: both are "there is no round yet", and a build that met a
  // second sentinel would otherwise print it.
  if (roundNum == null || roundNum < 0) {
    return {
      text: "starting",
      hint: "the turn has begun and its first model round has not come back",
    };
  }
  return {
    text: `round ${roundNum + 1}`,
    hint: "the model round this turn is on, counting from one",
  };
}

// ---------------------------------------------------------------------------
// The guarded half: what only the company document says
// ---------------------------------------------------------------------------

/** What the company document holds for one seat, or why it cannot be said. */
export type SeatSettings =
  | { state: "found"; role: ConfigRole; unit: ConfigUnit | null }
  | { state: "missing" }
  | { state: "ambiguous" };

/**
 * WHAT THIS READER CAN SAY ABOUT A SEAT'S GUARDED HALF.
 *
 * FIVE OUTCOMES, NOT A NULLABLE ROLE. The company document is behind a grant
 * (`config:read`), so "this seat is not in the active revision", "you may not
 * read it", "the engine could not answer", "the read has not come back yet"
 * and "here it is" are five different facts — and a `ConfigRole | null`
 * collapses the first four into one. Every screen that did that printed the
 * same sentence for all of them, and the sentence it picked was the reader's:
 * the seat header's MODEL fact read "needs an operator token" on five of
 * eight tabs, because those tabs simply did not ask.
 *
 * ONLY `unauthorized` IS A REFUSAL. Every error used to read as one, so a
 * node still catching up after a restart told every reader of every seat that
 * THEY were missing a credential — a claim about the reader, made by an
 * answer about the node. And a refusal carries the grants the engine named,
 * because what a signed-in reader lacks is a grant, never "an operator
 * token".
 *
 * THE ERROR IS CHECKED FIRST, and that order is the whole of it. `useQuery`
 * keeps its last good answer through a failed ask — which suits a poll and is
 * exactly wrong for a guarded read: once a token is cleared or refused, the
 * document it had been allowed to read is still in hand, so a reading that
 * looked at the answer first would keep printing a revoked reader's model,
 * budget and schedules beside a banner saying the answer needs a token.
 */
export type SeatReading =
  | { state: "read"; role: ConfigRole; unit: ConfigUnit | null }
  /** The revision answered and names no single seat by this name. */
  | { state: "absent" }
  /**
   * The engine refused the read on authority. `grants` are the ones it named,
   * any ONE of which would admit this reader — empty for a 401, where nothing
   * the engine accepted was presented.
   */
  | { state: "refused"; grants: readonly string[] }
  /** The engine could not answer: a node catching up, a socket gone, a fault. */
  | { state: "failed" }
  /** Nothing has been asked, or nothing has come back. Never a claim. */
  | { state: "unread" };

export function seatReading(
  settings: SeatSettings | null | undefined,
  error?: QueryErrorCode | null,
  refusal?: QueryRefusal | null,
): SeatReading {
  if (error === "unauthorized") return { state: "refused", grants: refusal?.grants ?? [] };
  if (error) return { state: "failed" };
  if (!settings) return { state: "unread" };
  if (settings.state === "found") {
    return { state: "read", role: settings.role, unit: settings.unit };
  }
  // `missing` AND `ambiguous` ARE ONE ANSWER HERE. Both mean the revision
  // holds no single entry this page may attribute to this seat, and the second
  // is only reachable from a revision stored before names had to be unique —
  // so a distinct sentence for it would be a sentence nobody will ever read.
  return { state: "absent" };
}

/** Every unit in a company document, depth first, parents before children. */
export function documentUnits(doc: CompanyDocument | null | undefined): ConfigUnit[] {
  const out: ConfigUnit[] = [];
  const visit = (unit: ConfigUnit): void => {
    out.push(unit);
    for (const child of list(unit.children)) visit(child);
  };
  for (const unit of list(doc?.units)) visit(unit);
  return out;
}

/**
 * The document's own entry for a seat, from the operator-gated `config` answer.
 *
 * Contact identities, email, the model chain, the token budget, schedules, the
 * seat's integration blocks and its tool credential names are not on the
 * anonymous projection, so every screen that shows one reads it here.
 *
 * The seat is found by NAME, which is the identity the document itself
 * addresses a seat by. Two seats with one name can only come from a revision
 * stored before names had to be unique, and attributing either one's settings
 * to the page would be a guess — so that answer is `ambiguous` rather than the
 * first match.
 *
 * `unit` is the document's entry for the seat's home unit, whose `mcp_env` its
 * direct agent members inherit. Null at the root, and null where the unit name
 * repeats, for the same reason.
 */
export function seatSettings(doc: CompanyDocument | null | undefined, seat: Seat): SeatSettings {
  const roles: ConfigRole[] = [...list(doc?.roles)];
  const units = documentUnits(doc);
  for (const unit of units) for (const role of list(unit.roles)) roles.push(role);

  const matches = roles.filter((r) => r.name === seat.name);
  if (matches.length === 0) return { state: "missing" };
  if (matches.length > 1) return { state: "ambiguous" };
  const home = seat.unit ? units.filter((u) => u.name === seat.unit?.name) : [];
  return { state: "found", role: matches[0]!, unit: home.length === 1 ? home[0]! : null };
}

/**
 * The document's own entry for a unit: its stable id, the tracker project and
 * knowledge container it owns, and the credentials its members inherit.
 *
 * Guarded for the reasons `internal/api/orgprojection_test.go` records against
 * each field, so it is null without a token — never an empty value, which
 * would read as a unit that owns nothing.
 */
export function unitSettings(
  doc: CompanyDocument | null | undefined,
  unit: Pick<Unit, "name">,
): ConfigUnit | null {
  const matches = documentUnits(doc).filter((u) => u.name === unit.name);
  return matches.length === 1 ? matches[0]! : null;
}

/**
 * The tool credentials a seat runs with: its home unit's `mcp_env` merged down,
 * the seat's OWN entries winning per variable.
 *
 * Per VARIABLE rather than per server, which is `org.MCPEnv`'s own rule: a seat
 * that overrides one header must not silently drop the token beside it. A
 * human seat inherits none, because it runs no tools.
 */
export function mcpEnvOf(
  settings: SeatSettings,
  kind: Seat["kind"],
): Record<string, Record<string, string>> {
  if (settings.state !== "found") return {};
  const out: Record<string, Record<string, string>> = {};
  if (kind === "agent") {
    for (const [server, vars] of Object.entries(settings.unit?.mcp_env ?? {})) {
      out[server] = { ...(out[server] ?? {}), ...vars };
    }
  }
  for (const [server, vars] of Object.entries(settings.role.mcp_env ?? {})) {
    out[server] = { ...(out[server] ?? {}), ...vars };
  }
  return out;
}

/** A seat's schedules, from the document. Empty where the document did not answer. */
export function schedulesOf(settings: SeatSettings): ScheduleSpec[] {
  return settings.state === "found" ? list(settings.role.schedules) : [];
}

/**
 * The provider keys a seat's `llm:` names, in declaration order.
 *
 * THREE SHAPES REACH THIS BROWSER, because `config.PhaseLLM` accepts three:
 * `llm: fast`, `llm: [fast, backup]`, and `llm: {default: fast, judge: tiny}`.
 * The type declared a string, so the other two were rendered by whatever
 * happened to be asked of them — an array as `fast,backup`, a mapping as
 * `[object Object]` — and any consumer that called a string method on one
 * threw on a config the engine accepts.
 *
 * The mapping form flattens in PHASE ORDER with `default` first, because the
 * question a reader has on a seat page is "which models does this seat run
 * on", not "which model runs its judge". Duplicates are dropped: a chain that
 * lists one key twice is one key, and the same key reached through two phases
 * is not two models.
 *
 * `phase` picks one phase out of the mapping instead, falling back to
 * `default` exactly as the engine does.
 */
export function llmChain(
  llm: PhaseLLM | undefined,
  phase?: "default" | "review" | "subagent" | "auxiliary" | "judge" | "sandbox",
): string[] {
  const keys = (value: ProviderKeys | undefined): string[] =>
    typeof value === "string"
      ? value
        ? [value]
        : []
      : Array.isArray(value)
        ? value.filter(Boolean)
        : [];
  if (llm == null) return [];
  if (typeof llm === "string" || Array.isArray(llm)) return dedupe(keys(llm));
  const mapping = llm as Record<string, ProviderKeys | undefined>;
  if (phase) {
    const own = keys(mapping[phase]);
    return dedupe(own.length ? own : keys(mapping["default"]));
  }
  return dedupe([
    ...keys(mapping["default"]),
    ...keys(mapping["review"]),
    ...keys(mapping["subagent"]),
    ...keys(mapping["auxiliary"]),
    ...keys(mapping["judge"]),
    ...keys(mapping["sandbox"]),
  ]);
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
export function seatLookup(
  index: OrgIndex,
): (handle: string) => { name: string; kind?: "agent" | "human" } {
  return (handle) => {
    const seat = index.byHandle.get(handle);
    return seat ? { name: seat.name, kind: seat.kind } : { name: handle };
  };
}

export type RunState =
  "working" | "awaiting_sandbox" | "idle" | "afk" | "failed" | "terminated" | "offline" | "human";

/**
 * Whether a detached coding run is waiting on a person.
 *
 * THE ENGINE'S OWN TWO WORDS. Six call sites compared against
 * `awaiting_input`, which `sandbox.PendingRun` cannot write — its statuses are
 * `launching`, `running`, `awaiting_clarification`, `resumed`, `done`,
 * `failed` and `reseed` (`internal/sandbox/pending.go`) — so every one of them
 * was permanently false and the state this product most needs to surface
 * reached no screen through any of them. `reseed` counts because
 * `sandbox.Awaiting` counts it: the box was reaped past its pause TTL, so the
 * work is gone and only the question survives.
 *
 * One predicate rather than six comparisons, because six copies of a
 * vocabulary is how five of them come to be wrong at once.
 */
export function awaitingPerson(status: string | undefined): boolean {
  return status === "awaiting_clarification" || status === "reseed";
}

/**
 * What a seat is actually doing.
 *
 * A seat with an in-flight detached sandbox run is still busy even though its
 * kick-off turn already completed — which the projection reads as idle. The
 * live sandbox set is folded in here, at read time, so it is right on the
 * first snapshot and on every push after it.
 */
export function runState(agent: AgentRow | null | undefined, sandboxes: SandboxEntry[]): RunState {
  if (!agent) return "offline";
  const role = agent.role;
  if (role && sandboxes.some((s) => s.role === role)) return "awaiting_sandbox";
  return (agent.state as RunState) || "offline";
}

/**
 * The four tones a seat's chrome may take, and NONE of them is its identity.
 *
 * `quiet` is deliberately not a hue. An idle seat used to draw a tinted,
 * glowing tile that read as activity — reported as "when agent is idle it has
 * this blob lighting which feels like it is working" — and the fix for that is
 * not a duller hue, it is none.
 *
 * `needs` and `broken` are separate on purpose: a seat parked on a question and
 * a seat that fell over have both stopped, and only one of them is a failure.
 * Red is reserved for failure.
 */
export type SeatTone = "working" | "needs" | "broken" | "quiet";

export function seatTone(agent: AgentRow | null | undefined, sandboxes: SandboxEntry[]): SeatTone {
  if (!agent) return "quiet";
  const sandbox = sandboxes.find((s) => s.role === agent.role);
  if (awaitingPerson(sandbox?.status)) return "needs";
  if (agent.last_error) return "broken";
  const state = runState(agent, sandboxes);
  if (state === "afk") return "broken";
  if (state === "working" || state === "awaiting_sandbox") return "working";
  return "quiet";
}

export function toneOf(state: RunState): "positive" | "caution" | "critical" | "info" | "neutral" {
  switch (state) {
    case "working":
    case "awaiting_sandbox":
      return "info";
    case "idle":
      return "positive";
    case "afk":
    case "failed":
      return "critical";
    default:
      return "neutral";
  }
}

export function stateLabel(state: RunState): string {
  // "sandbox", not "awaiting sandbox": the badge shares a row with the seat's
  // name, and the longer phrase pushed the name into an ellipsis on every card
  // carrying it.
  return state === "awaiting_sandbox" ? "sandbox" : state;
}

/** Why a seat is AFK, in a sentence, keyed on the engine-detected cause. */
export function afkReason(reason: string | undefined): string {
  const reasons: Record<string, string> = {
    llm_unavailable: "the LLM provider was unreachable",
    stall: "the turn made no forward progress and was given up",
    max_iter: "the round cap was reached before the turn finished",
    unhandled_exception: "an unhandled error ended the turn",
    budget_exhausted: "the token budget is spent",
    depth_cap: "delegation went deeper than the cap allows",
    scheduled_timeout: "a scheduled turn ran past its wall-clock cap",
  };
  return reasons[reason ?? ""] ?? "the engine paused this seat";
}

const PHASE_DOING: Record<string, string> = {
  onboarding: "reading the team's onboarding pages",
  execute: "working on the task",
  review: "reviewing its own work",
};

/** What this seat is doing, in a sentence. Derived from live state only. */
export function statusLine(
  agent: AgentRow | null | undefined,
  opts: { sandbox?: SandboxEntry | null; seat?: Seat | null } = {},
): string {
  const { sandbox, seat } = opts;
  if (seat?.kind === "human") {
    return seat.availability || "human teammate — not run by the engine";
  }
  if (sandbox) {
    return awaitingPerson(sandbox.status)
      ? "waiting on an answer to keep coding"
      : `writing code in a sandbox (${sandbox.coding_agent || "coding agent"})`;
  }
  const state = (agent?.state as RunState) || "offline";
  if (state === "afk") return afkReason(agent?.afk_reason);
  if (state === "working") return PHASE_DOING[agent?.current_phase ?? ""] ?? "working on a task";
  if (state === "terminated") return "terminated";
  if (state === "offline") return "not running on this node";
  return "idle — nothing in the inbox";
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

export function staleness(updatedAt: string | undefined, now: number): "" | "stale" | "stalled" {
  if (!updatedAt) return "";
  const age = now - new Date(updatedAt.endsWith("Z") ? updatedAt : `${updatedAt}Z`).getTime();
  if (!Number.isFinite(age)) return "";
  if (age >= STALLED_MS) return "stalled";
  if (age >= STALE_MS) return "stale";
  return "";
}

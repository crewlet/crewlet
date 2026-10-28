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
 * THE BLOCK IS THE AUTHORITY, AND IT IS NEVER CHECKED AGAINST ITS TREE. The
 * engine derives both halves from one document in one call
 * (`internal/api/orgprojection.go`), so they pair by construction: every seat
 * the block names is a seat of the tree, by its handle, every unit by its key,
 * and every handle it mentions is one of its own seats. There is no "hierarchy not reported"
 * state for a screen to draw, because every projection that carries a seat
 * carries the block. `{}`, the answer of a node running no company, carries
 * neither half and indexes as an empty company, as does an org not read yet.
 *
 * THE GUARDED HALF IS NOT HERE EITHER. `/org` is the overview's projection,
 * which every signed-in reader is sent, so it carries a charter, a tree, each
 * human seat's holder and the budgets as written — and, to an admin, each
 * agent seat's RESOLVED model chain and tool sources ([resolvedAbsence] says
 * what their absence means to everybody else). Nothing else: a seat's email,
 * authored `llm` fields, contact identities, tool credentials, integrations
 * and schedules are read from the company document through the `config`
 * query, which takes Admin and is what [seatSettings] and [unitSettings]
 * below are for.
 *
 * Resolved ONCE, into an index, and screens consume seats. Doing it per screen
 * is how the previous dashboard ended up walking the whole roster once per
 * rendered row: `managerOf` was a linear scan called per seat AND again per
 * row, which on a 200-seat company was roughly 80,000 array scans per push.
 */

import { useMemo } from "react";
import { isNodeHue, type AvatarStatus, type Principal } from "@crewlethq/ui";
import { avatarOf, type AgentAvatar } from "./avatar.ts";
import { parseUTC, plural } from "./format.ts";
import { useLeadScope } from "./leadScope.ts";
import { ADMIN, accessNames, holds } from "./access.ts";
import { useQuery } from "./useQuery.ts";
import { useRest } from "./useRest.ts";
import { useOrg } from "./store-hooks.ts";
import { useViewer } from "./viewer.ts";
import { loginKind, loginWords } from "./login.ts";
import { rest } from "~/protocol/rest.ts";
import { DELEGATE_TASKS, DELEGATE_TOOL, type SeatActivity } from "~/contract/wire.ts";
import type { EngineHealth } from "~/contract/health.ts";
import type { ProviderErrorKind } from "~/contract/failures.ts";
import type {
  AgentRow,
  CompanyDocument,
  ConfigRole,
  ConfigUnit,
  Derived,
  LiveCall,
  LogRefusal,
  OrgProjection,
  OrgSeat,
  OrgUnit,
  PhaseLLM,
  ProviderKeys,
  QueryRefusal,
  ScheduleSpec,
} from "~/protocol/index.ts";

/** One unit, as the projection wrote it and as the engine resolved it. */
export interface Unit {
  /** Stable React key and DOM id suffix: the unit's depth-first position. */
  key: string;
  /**
   * The unit's KEY — the engine's `Unit.Key`, its declared `id:`, which the
   * engine mints from the name when a document declares none — and how a
   * unit is ADDRESSED, here and in every route ([unitRoute]). Every reference
   * in the document names a unit by it: a `manages:` entry, a seat's `unit:`.
   */
  id: string;
  /** What a person reads. Prose, and two units may share one, so nothing
   *  addresses a unit by it. */
  name: string;
  /** The EFFECTIVE type, after the engine's default. */
  type: string;
  purpose: string;
  goals: string[];
  /** Free-text knowledge references. NOT a read scope. */
  knowledge: string[];
  /**
   * The lead AS THE DOCUMENT WRITES IT: a seat handle, "" when the unit
   * inherits one. Deliberately still the reference rather than a resolved
   * seat — `effectiveLead` is the resolved one, and the two are different
   * facts.
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
  /** Stable React key and DOM id suffix: the handle, unique in a company. */
  key: string;
  name: string;
  /**
   * The handle this seat RUNS UNDER, as the engine derived it. Never derived
   * here: a handle this client worked out would be a second implementation of
   * the rule that keys the seat's memory.
   */
  handle: string;
  kind: SeatKind;
  /**
   * How an agent seat is drawn: its character and colour, as the engine
   * published them. Null for a person, who is drawn as themself.
   */
  avatar: AgentAvatar | null;
  goal: string;
  backstory: string;
  responsibilities: string[];
  guidelines: string[];
  /** The `manages` entries AS WRITTEN: seat handles and unit keys, unexpanded. */
  manages: string[];
  availability: string;
  /** Root → own unit. Empty for a root-level seat. */
  unitChain: Unit[];
  unit: Unit | null;
  /**
   * The effective unit lead's NAME, declared or inherited, and "" at the root
   * or where the unit has no lead. A name rather than a seat because every
   * caller draws it as words beside the unit.
   */
  unitLead: string;
  /** A root seat the engine moved into a unit because of its `unit:` reference. */
  placedByRef: boolean;
  /** The engine's primary manager. Null when it has none. */
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
 * A seat as a principal. A seat the chart does not hold keeps its handle; it is
 * drawn as a person where the handle is a login (somebody signed in with no
 * seat, as a Tier A token's session acts under `token:<id>`), and as an agent,
 * the kit's default, otherwise. Drawn as an agent, a login wore an agent's
 * square face and the share dialog said it could act "in its turns", which a
 * login has none of.
 *
 * THE LINE UNDER A NAME IS THE SEAT'S UNIT OR NOTHING. A handle there repeats
 * the name in lower case ("Miles", then "miles"), which tells a reader nothing
 * the name did not.
 */
export function principalOfSeat(seat: Seat | undefined, handle: string): Principal {
  if (!seat) return { id: handle, kind: loginKind(handle) ? "person" : "agent", name: handle };
  const out: Principal = {
    id: seat.handle,
    kind: seat.kind === "human" ? "person" : "agent",
    name: seat.name,
  };
  if (seat.unit) out.detail = seat.unit.name;
  if (seat.kind === "agent" && seat.avatar) {
    out.character = seat.avatar.character as Principal["character"];
    if (isNodeHue(seat.avatar.color)) out.hue = seat.avatar.color;
  }
  return out;
}

/**
 * The route segments that open a seat's page: its handle, or its name where no
 * handle is known.
 *
 * ONE HELPER, because the rule for a seat addressed from its document entry —
 * which declares a handle only where it overrides the derived one, so the
 * builder can hold a saved seat with none — has to live in one place: the
 * seat screen resolves a name as well as a handle, so such a seat still has a
 * page a link can reach.
 */
export function seatPath(seat: Pick<Seat, "handle" | "name">): string[] {
  return ["company", "org", "seats", seat.handle || seat.name];
}

/** The route segments that open a unit's page: its KEY, never its name. */
export function unitRoute(unit: Pick<Unit, "id">): string[] {
  return ["company", "org", "teams", unit.id];
}

/**
 * The unit a route's segment addresses — the one whose KEY it is — or null.
 * Never the name: two units may share one, and a lookup by name opened
 * whichever came first.
 */
export function unitByKey(index: Pick<OrgIndex, "units">, key: string): Unit | null {
  if (!key) return null;
  return index.units.find((u) => u.id === key) ?? null;
}

/**
 * The roster row carrying a seat's live state, matched by the seat's HANDLE —
 * which is the roster row's `id` — and never by its name: two seats may share
 * a name, and a ring read off a namesake's row is a state the seat is not in.
 */
export function liveRowFor(
  agents: readonly AgentRow[],
  seat: Pick<Seat, "handle"> | null | undefined,
): AgentRow | undefined {
  const handle = seat?.handle;
  if (!handle) return undefined;
  return agents.find((a) => a.id === handle);
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
    avatar: raw.kind === "human" ? null : avatarOf(raw.avatar),
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

/** Link the units into a tree, in the depth-first order [walk] visited them. */
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
 * Lay the engine's derived block over the authored tree.
 *
 * Units pair by KEY — the block names each by the key the engine minted into
 * the document, which is [Unit.id] — and seats by HANDLE, which the engine
 * mints into every stored seat (ADR-0013). Neither pairs by position or by
 * name: the engine's seat order is not the document's, since a root seat moved
 * into a unit comes after that unit's own seats, and a NAME IS PROSE, held by
 * as many seats and units as a company likes, so a pairing by it hands one
 * seat its namesake's document. Only an entry that declares no handle — a
 * fixture, a document nothing has stored — is claimed by its name, the first
 * unclaimed one, and never an entry declaring another seat's handle. Every
 * seat, unit, lead and reporting line in the index is the block's; the tree
 * contributes only what the document wrote about each (its goal, its purpose,
 * its knowledge).
 *
 * TOTAL rather than validating: both halves come from one document in one
 * call, so there is no mismatch a working engine can send, and a reader that
 * second-guessed the block would need a "the engine did not say" state for
 * every screen to draw. A unit the block does not name keeps what the
 * document wrote, and a handle the block mentions that it does not list is
 * skipped rather than thrown on, because this runs in the sidebar, outside
 * every error boundary.
 */
function overlay(authored: Authored, derived: Derived): OrgIndex {
  const units = link(authored);
  const dByKey = new Map(list(derived.units).map((d) => [d.id, d]));
  const derivedOf = (unit: Unit) => (unit.id ? dByKey.get(unit.id) : undefined);
  for (const unit of units) {
    const d = derivedOf(unit);
    if (!d) continue;
    unit.type = d.type || unit.type;
    unit.channel = d.channel ?? "";
    unit.channelInherited = !!d.channel_inherited;
    unit.leadInherited = !!d.lead_inherited;
  }

  // The entries that declare a handle, by it, and the ones that declare none,
  // by name — the only entries a name may claim.
  const declared = new Map<string, OrgSeat>();
  const undeclared = new Map<string, OrgSeat[]>();
  for (const { raw } of authored.seats) {
    if (raw.handle) {
      if (!declared.has(raw.handle)) declared.set(raw.handle, raw);
      continue;
    }
    const name = raw.name ?? "";
    undeclared.set(name, [...(undeclared.get(name) ?? []), raw]);
  }
  const claim = (name: string, handle: string): OrgSeat => {
    const own = declared.get(handle);
    if (own) {
      declared.delete(handle);
      return own;
    }
    return undeclared.get(name)?.shift() ?? { name };
  };
  const byHandle = new Map<string, Seat>();
  const seats: Seat[] = [];
  const dSeats = list(derived.seats);
  for (const d of dSeats) {
    const seat = newSeat(claim(d.name, d.handle), d.handle, d.handle);
    // The derived kind is the ENGINE'S reading of the field, so a value off
    // the wire this build does not know is the same value everywhere.
    seat.kind = d.kind === "human" ? "human" : "agent";
    seat.placedByRef = !!d.placed_by_ref;
    byHandle.set(d.handle, seat);
    seats.push(seat);
  }

  const resolve = (handles: string[] | null | undefined): Seat[] =>
    list(handles).flatMap((handle) => byHandle.get(handle) ?? []);

  for (const unit of units) {
    const d = derivedOf(unit);
    if (!d) continue;
    unit.seats = resolve(d.seats);
    for (const seat of unit.seats) {
      seat.unit = unit;
      seat.unitChain = unit.chain;
    }
    unit.effectiveLead = d.lead ? (byHandle.get(d.lead) ?? null) : null;
  }
  for (const seat of seats) seat.unitLead = seat.unit?.effectiveLead?.name ?? "";

  dSeats.forEach((d, i) => {
    const seat = seats[i]!;
    seat.managers = resolve(d.managers);
    seat.reports = resolve(d.reports);
    seat.autoReports = resolve(d.auto_reports);
    seat.manager = d.manager ? (byHandle.get(d.manager) ?? null) : null;
  });

  return {
    seats,
    rootSeats: seats.filter((s) => !s.unit),
    units,
    topUnits: units.filter((u) => !u.parent),
    byHandle,
    byName: new Map(),
  };
}

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

/** What `{}` (a node running no company) and an org not read yet describe:
 *  no seats and no units, laid over their empty tree. */
const NO_HIERARCHY: Derived = { seats: [], units: [] };

/** Index the org projection once. Everything a screen needs comes off this. */
export function indexOrg(org: OrgProjection | null | undefined): OrgIndex {
  const authored = walk(org);
  const built = overlay(authored, org?.derived ?? NO_HIERARCHY);
  // AFTER the overlay, because the pass reads only `seats` and `children` —
  // which it has set by here.
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
 */
export function reportsCaption(seat: Seat): string {
  const total = seat.reports.length;
  if (total === 0) return "nobody reports to this seat";
  const auto = seat.autoReports.length;
  if (auto === 0) return "all from its manages list";
  if (auto === total) return "all by leading a unit";
  return `${total - auto} from its manages list, ${auto} by leading a unit`;
}

/**
 * Whether `lead` is anywhere above `handle` in the chart — a lead in their
 * LINE.
 *
 * ANY ANCESTOR, not only the direct manager, which is the engine's own reading
 * of "a lead may set what somebody in their line does next" (`leadsOf` in
 * `cmd/crewlet/main.go` walks every manager up the chain): a founder leads
 * everybody, and an authority that stopped one level up would make a line mean
 * the people directly under you. The walk is over [Seat.managers], the
 * engine's derived relation, never a second reading of `manages:` here.
 */
export function leadsInLine(index: OrgIndex, lead: string, handle: string): boolean {
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
 * Whether the reader is the person this seat is held by — the seat the
 * identity directory binds them to — and so whether what draws it speaks to
 * them.
 *
 * ONE ANSWER FOR EVERY SURFACE THAT DRAWS A SEAT: the overview said "Your day
 * … waiting on you" while the callout above it said "Held by jane.doe" and the
 * work card beside it "Nothing open is assigned to them", because each asked
 * for itself, or did not ask. Here rather than beside the profile because a
 * shared control asks it too — Message holds itself on the reader's own seat
 * (`components/writes.tsx`), and the peek draws that control as the page does.
 */
export function isOwnSeat(viewer: { readonly handle: string }, handle: string): boolean {
  return viewer.handle !== "" && viewer.handle === handle;
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
    return { text: "starting", hint: "No model round yet" };
  }
  const round = roundOf(call);
  return {
    text: `round ${round}`,
    hint:
      (call.round_num ?? -1) < 0 && (call.rounds_used ?? 0) === 0
        ? "First round in progress"
        : "Current model round",
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
 * FIVE OUTCOMES, NOT A NULLABLE ROLE. The company document takes Admin, so
 * "this seat is not in the active revision", "you may not
 * read it", "nobody asked, because you may not", "the read has not come back
 * yet" and "here it is" are five different facts — and a `ConfigRole | null`
 * collapses the first four into one. Every screen that did that printed the
 * same sentence for all of them, and the sentence it picked was the reader's:
 * the seat header's MODEL fact read "needs a credential" on five of eight
 * tabs, because those tabs simply did not ask; and the Settings tab told a
 * reader nobody had asked for that "No company configuration is active".
 *
 * THE ERROR IS CHECKED FIRST, and that order is the whole of it. `useQuery`
 * keeps its last good answer through a failed ask — which suits a poll and is
 * exactly wrong for a guarded read: once Admin is withdrawn, the document it
 * had been allowed to read is still in hand, so a reading that looked at the
 * answer first would keep printing a revoked reader's model, budget and
 * schedules beside a banner saying the answer needs Admin.
 */
export type SeatReading =
  | { state: "read"; role: ConfigRole; unit: ConfigUnit | null }
  /** The revision answered and names no seat by this handle. */
  | { state: "absent" }
  /** The engine refused the read: this reader does not hold Admin. */
  | { state: "refused" }
  /**
   * NEVER ASKED, because the answer was known before the question: the viewer
   * has answered, is no admin and leads no unit over the seat, so the engine
   * would refuse it ([useSeatSetup]). A claim about the READER — which access
   * reads the seat's half — and never one about the company.
   */
  | { state: "withheld" }
  /**
   * Nothing has come back yet — or nothing could be asked yet, because the
   * viewer has not said who this is. Never a claim.
   */
  | { state: "unread" };

export function seatReading(
  settings: SeatSettings | null | undefined,
  error?: unknown,
  withheld = false,
): SeatReading {
  // NOT ASKED HAS NO ANSWER TO WEIGH: a disabled read holds neither a
  // document nor an error (`useQuery`, `useRest`), so this is decided first.
  if (withheld) return { state: "withheld" };
  if (error) return { state: "refused" };
  if (!settings) return { state: "unread" };
  if (settings.state === "found") {
    return { state: "read", role: settings.role, unit: settings.unit };
  }
  // `missing` AND `ambiguous` ARE ONE ANSWER HERE. Both mean the revision
  // holds no single entry this page may attribute to this seat. The second is
  // only reachable from a document holding one handle twice, which the engine
  // refuses; the seat's own screen says which ([SeatSettings]), and a panel
  // reading the reading only needs to know it has nothing to draw.
  return { state: "absent" };
}

/**
 * The access the company document is read under, and the org projection a
 * seat's RESOLVED setup under: its model chain and its tool sources are
 * derived from the document's own half, so they follow its rule.
 */
export const RUNTIME_GRANT = ADMIN;

/**
 * What it means that the org projection carried no model chain (`llm`) or no
 * tool sources (`tool_sources`) for an agent seat — which depends on WHO is
 * reading, because the engine strips both for every audience but an admin:
 *
 *   - `none` — the reader is an admin, so the engine would have sent the
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
  if (holds(viewer.grants, RUNTIME_GRANT)) return "none";
  return viewer.loading ? "unknown" : "withheld";
}

/**
 * What a reader without Admin reads where a resolved value would be, in a
 * fact cell: the access that would show it ("Admin only"), rather than an
 * empty value nobody could tell from a seat that has none. A cell takes a
 * short value; the card says once, below its facts, why they are hidden.
 */
export function resolvedWithheld(): string {
  return `${accessNames([RUNTIME_GRANT])} only`;
}

/**
 * What [useSeatSetup] answers. A type of its own so a screen that READ it once
 * can hand it to every panel drawing it: `useQuery` shares no request between
 * two callers, so a panel calling the hook again fetches the whole guarded
 * document a second time.
 */
export interface SeatSetup {
  seat: Seat | undefined;
  settings: SeatSettings | null;
  reading: SeatReading;
  /** The raw answer, for a panel that states its own read: the document
   *  (never beside a refusal), whether it is in flight — or waiting on the
   *  viewer before it can be asked — and the refusal. */
  config: {
    doc: CompanyDocument | null;
    loading: boolean;
    error: string | null;
    refusal: QueryRefusal | LogRefusal | null;
  };
}

/**
 * The guarded half of one seat, for any screen that draws it: the company
 * document's entry for the seat and the five-state reading of it.
 *
 * ONE READ, ONE ORDER. The document takes Admin, and every screen that read
 * it on its own either forgot the refusal-first order [seatReading] exists
 * for — and printed a revoked reader's model beside a banner saying the
 * answer needs Admin — or collapsed "absent", "refused"
 * and "not read yet" into one sentence. The seat is resolved by handle, or by
 * name for a link built from a document entry that declares no handle, which
 * is how [seatPath] addresses one.
 *
 * A reader without Admin is never asked: see the query below. A LEAD of
 * the seat's unit is, through the reads the engine admits them instead — the
 * seat and its unit (`GET /config/roles/{handle}`, `GET /config/units/{key}`,
 * `lib/leadScope.ts`) — composed into the small document [seatSettings]
 * reads, so the seat's page shows a lead what the org builder lets them edit.
 */
export function useSeatSetup(handle: string): SeatSetup {
  const org = useOrg();
  const viewer = useViewer();
  const scope = useLeadScope();
  const index = useMemo(() => indexOrg(org), [org]);
  const seat = handle ? (index.byHandle.get(handle) ?? index.byName.get(handle)) : undefined;
  // NOT ASKED WITHOUT ADMIN. The document takes it, so a reader without it is
  // refused on every ask — the answer is known before the question, and
  // asking only puts a refusal on the wire and a `refused` banner over a page
  // that never had a chance. Such a reader is `withheld` once the viewer has
  // answered — a claim about the reader, which the screen says by naming the
  // access — and `unread`, claiming nothing, until it has.
  const granted = holds(viewer.grants, RUNTIME_GRANT);
  const config = useQuery("config", undefined, { enabled: !!seat && granted });
  const led = !granted && !!seat?.handle && scope.seats.has(seat.handle) ? seat : null;
  const asked = !!seat && (granted || led !== null);
  const withheld = !!seat && !asked && !viewer.loading;
  const lead = useRest(
    led ? `/config/roles/${led.handle}+${led.unit?.id ?? ""}` : null,
    (signal) => (led ? leadDocument(led, signal) : Promise.resolve(null)),
  );
  const read = led
    ? {
        data: lead.data,
        loading: lead.loading,
        error: lead.code,
        refusal: lead.refusal,
      }
    : config;
  // NOTHING FROM THE DOCUMENT BESIDE A REFUSAL: `useQuery` keeps its last good
  // answer through a failed ask, which suits a poll and is wrong for a guarded
  // read.
  const settings = useMemo<SeatSettings | null>(
    () => (seat && read.data && !read.error ? seatSettings(read.data, seat) : null),
    [seat, read.data, read.error],
  );
  const reading = useMemo(
    () => seatReading(settings, read.error, withheld),
    [settings, read.error, withheld],
  );
  return {
    seat,
    settings,
    reading,
    config: {
      doc: read.error ? null : (read.data ?? null),
      // WAITING ON THE VIEWER IS WAITING TOO: until it answers nothing can be
      // asked, and a panel reading "not loading, no document" as an answer
      // told every reader "No company configuration is active" while it did.
      loading: read.loading || (!!seat && !asked && !withheld),
      error: read.error,
      refusal: read.refusal,
    },
  };
}

/**
 * The part of the company document a lead may read about one of their seats,
 * as a document [seatSettings] reads: the seat, and its home unit's own fields
 * (whose `mcp_env` the seat inherits) without the unit's members, so the seat
 * is found once.
 */
async function leadDocument(seat: Seat, signal: AbortSignal): Promise<CompanyDocument> {
  const [role, unit] = await Promise.all([
    rest.get(`/config/roles/${encodeURIComponent(seat.handle)}`, signal),
    seat.unit ? rest.get(`/config/units/${encodeURIComponent(seat.unit.id)}`, signal) : null,
  ]);
  const own = isRecordValue(unit) ? { ...unit, roles: undefined, children: undefined } : null;
  return {
    roles: [role as ConfigRole],
    ...(own ? { units: [own as ConfigUnit] } : {}),
  };
}

const isRecordValue = (value: unknown): value is Record<string, unknown> =>
  typeof value === "object" && value !== null && !Array.isArray(value);

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
 * The document's own entry for a seat, from the `config` answer.
 *
 * Contact identities, email, the model chain, the token budget, schedules, the
 * seat's integration blocks and its tool credential names are not on the
 * overview's projection, so every screen that shows one reads it here.
 *
 * The seat is found by HANDLE, which is the identity the document itself
 * addresses a seat by: the engine mints one for every seat it stores and
 * refuses a document holding one twice, so a second match is a document this
 * engine did not write — and attributing either entry to the page would be a
 * guess, so that answer is `ambiguous` rather than the first match.
 *
 * `unit` is the document's entry for the seat's home unit, whose `mcp_env` its
 * direct agent members inherit, found by its KEY. Null at the root.
 */
export function seatSettings(doc: CompanyDocument | null | undefined, seat: Seat): SeatSettings {
  const roles: ConfigRole[] = [...list(doc?.roles)];
  const units = documentUnits(doc);
  for (const unit of units) for (const role of list(unit.roles)) roles.push(role);

  const matches = seat.handle ? roles.filter((r) => r.handle === seat.handle) : [];
  if (matches.length === 0) return { state: "missing" };
  if (matches.length > 1) return { state: "ambiguous" };
  return {
    state: "found",
    role: matches[0]!,
    unit: seat.unit ? unitSettings(doc, seat.unit) : null,
  };
}

/**
 * The document's own entry for a unit, found by its KEY: the tracker project
 * and knowledge container it owns, and the credentials its members inherit.
 *
 * Guarded for the reasons `internal/api/orgprojection_test.go` records against
 * each field, so it is null without Admin — never an empty value,
 * which would read as a unit that owns nothing.
 */
export function unitSettings(
  doc: CompanyDocument | null | undefined,
  unit: Pick<Unit, "id">,
): ConfigUnit | null {
  if (!unit.id) return null;
  const matches = documentUnits(doc).filter((u) => u.id === unit.id);
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
 * THE TWO THINGS [linkedSeat] NEEDS AND A BARE HANDLE CANNOT SUPPLY, so it is
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
export function seatLookup(index: OrgIndex): (handle: string) => SeatBadge {
  return (handle) => badgeOfSeat(index.byHandle.get(handle), handle);
}

/**
 * What a seat's badge is drawn from: its name, its kind, and how an agent is
 * drawn. KIND AND AVATAR ARE ABSENT for a key the chart does not hold, which
 * is a third answer rather than a missing one (see [seatResolvers]): a renamed
 * or removed seat keeps its name and is drawn with the kit's defaults.
 */
export interface SeatBadge {
  name: string;
  kind?: SeatKind;
  avatar?: AgentAvatar | null;
}

/** A seat's badge, or the key's own words where the chart has no such seat. */
function badgeOfSeat(seat: Seat | undefined, key: string): SeatBadge {
  return seat ? { name: seat.name, kind: seat.kind, avatar: seat.avatar } : { name: key };
}

/**
 * A seat by HANDLE or, failing that, by NAME — the pair a badge needs.
 *
 * Half the rows that name a seat carry its handle (a lease, a channel end) and
 * half carry its role NAME (a turn, a budget, a spend row), so a screen drawing
 * the badge in both kinds of column needs both lookups. Handle first, because
 * a handle is unique and a name only the first seat's.
 */
export function seatBadgeOf(index: OrgIndex): (key: string) => SeatBadge {
  return (key) => badgeOfSeat(index.byHandle.get(key) ?? index.byName.get(key), key);
}

/**
 * A seat's name and kind by its handle or its name, off the chart the store
 * holds.
 *
 * HERE, BESIDE [seatBadgeOf], rather than in `store-hooks.ts`, which it used
 * to be: that put `store-hooks.ts → seats.ts → useQuery.ts → store-hooks.ts`
 * in the import graph, a cycle whose evaluation order decided whether a
 * suite's mock of the store hooks reached `useQuery` at all — so adding an
 * unrelated import to a screen could turn a passing suite's queries into
 * "useClient outside a ClientContext provider". This module already reads
 * the store; the store no longer reads this module.
 */
export function useSeatBadgeOf(): (key: string) => SeatBadge {
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
  seatAvatar: (handle: string) => AgentAvatar | null | undefined;
} {
  const lookup = seatLookup(index);
  return {
    seatName: (handle) => lookup(handle).name,
    seatKind: (handle) => lookup(handle).kind,
    seatAvatar: (handle) => lookup(handle).avatar,
  };
}

/**
 * The kind of a writer the CHART does not hold, from what the record says
 * wrote it.
 *
 * AN OPERATOR IS A PERSON WHO IS NOT A SEAT. A write through `/operator/mcp`
 * carries the TOKEN's name as its author, with author kind `operator` — so a
 * task an operator filed has a reporter no chart lists, and the chart's
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
// So everything below is a MAPPER: the engine's word to an avatar status, a
// pill's tone, a label and a line of prose. Nothing here, and nothing anywhere else, reads a live
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
 *
 * A SEAT WITH NO READABLE START SORTS LAST. The engine calls a seat working
 * while a coding run of its is out even when its projection holds no turn
 * record for it — a parked turn it never saw start — and when that turn
 * resumes, the record it creates is stamped with the RESUME instant, the
 * newest start there is. So last is where the row will land anyway, and a
 * row that arrives last never moves: read as 0 it led every list and then
 * jumped to the bottom, which is the reshuffle "a turn that starts is
 * appended" promises a reader never sees. The compare is explicit three-way
 * because two unknowns are a tie (stable, so they keep the push's order),
 * where `Infinity - Infinity` is NaN and leaves the order to the engine.
 */
export function workingLongestFirst(agents: readonly AgentRow[]): AgentRow[] {
  const started = (a: AgentRow) => {
    const at = Date.parse(a.turn?.started_at ?? "");
    return Number.isFinite(at) ? at : Infinity;
  };
  return agents
    .filter((a) => activityOf(a) === "working")
    .map((row) => ({ row, at: started(row) }))
    .sort((a, b) => (a.at === b.at ? 0 : a.at < b.at ? -1 : 1))
    .map(({ row }) => row);
}

/**
 * How many running turns a SHORT list of them names before it hands the rest
 * to Live › Now running, which lists every one: Home's Live now card, and the
 * sidebar's Running group.
 *
 * ONE NUMBER FOR BOTH, because they are one list in one order
 * ([workingLongestFirst]) and two caps would name two different sets of seats
 * on the same screen — and one SELECTION for both, `lib/turns.ts`'
 * `runningShortList`, since one cap applied after two different filters
 * named two sets just the same. Four is what Home's card holds level with the figures
 * beside it, and four sidebar rows are about the height of the Pinned group a
 * reader keeps there — a fifth working seat is the point where a shortcut
 * becomes a second copy of Now running, so it is a "more" row instead.
 */
export const RUNNING_ROWS = 4;

/**
 * The key of the work item a seat's turn is CHARGED to — its call's, then its
 * turn's — or "" for a turn on none. The item the engine charges the turn to,
 * never a `work_key` a trigger happened to name (see [stateLine]).
 *
 * A LABEL, never a join: two tasks can hold one key (`key_collision`), so
 * anything that pairs a turn with a task joins on the item's id
 * ([liveOnItems]) and only draws this.
 */
export function turnItemKey(row: AgentRow | null | undefined): string {
  return row?.live_call?.work_item?.key || row?.turn?.work_item?.key || "";
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
 * The status badge on a seat's avatar: the engine's own word, and NONE for an
 * idle seat or one no row is held for.
 *
 * THE KIT'S STATUSES ARE THE ENGINE'S WORDS (`working`, `needs`, `stopped`), so
 * this maps nothing but idle and offline away. The badge is a glyph on the
 * avatar's corner whose shape says the state (play, a question, a square),
 * and the row it stands in says the state in words as well; a badge drawn
 * alone is what the glyph is for. An idle seat carries none: the product's
 * own complaint about the green it used to wear was "when agent is idle it
 * has this blob lighting which feels like it is working", and the fix for
 * that is not a duller mark, it is none.
 */
export function statusOf(state: ShownState | undefined): AvatarStatus | undefined {
  // A PAUSED SEAT IS A STOPPED ONE to the engine, and its badge is the stop's
  // square: the words beside it are what tell the two apart.
  if (state === "paused") return "stopped";
  return state === "working" || state === "needs" || state === "stopped" ? state : undefined;
}

/** The kit tone a state's pill takes. */
export type SeatTone = "info" | "warning" | "danger";

/**
 * Which node holds a seat's lease, as far as the PUBLIC health push can say.
 *
 * WHAT EVERY READER IS TOLD, operator or not, on the profile and in the peek
 * alike. `/health` is unguarded and its push reaches an anonymous tab, so a
 * node's own name and the seats it holds are already on every reader's
 * screen: withholding "this node · node-2" in one place and printing it in
 * the next was a rule nobody could state. What the push does NOT say is WHICH
 * peer holds a seat this node does not — that is the operator-only `fleet`
 * answer — so a seat held elsewhere is "another node", and an operator is
 * shown the lease itself.
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
  if (agent?.stopped_reason === "unplaced") return "no node";
  if (!health) return "not reported by this node";
  if (health.seats.includes(handle))
    return health.node ? `this node · ${health.node}` : "this node";
  return "another node";
}

/**
 * The kit tone a state's pill takes, the same fill its avatar's status badge
 * stands on: WORKING IS INFO, NEEDS YOU IS WARNING, STOPPED IS DANGER, and idle
 * is neutral. Amber is reserved for the one state that asks for a person.
 */
export function toneOf(state: ShownState | undefined): SeatTone | "neutral" {
  switch (state) {
    case "working":
      return "info";
    case "needs":
      return "warning";
    case "stopped":
    case "paused":
      return "danger";
    default:
      return "neutral";
  }
}

/**
 * A seat's state as a screen NAMES it: the engine's `activity`, with a paused
 * seat told apart from a stopped one.
 *
 * ONE STATE, TWO WORDS FOR IT was the defect this closes. The engine calls a
 * seat a person paused `stopped` with the reason `paused`, and the seat page's
 * pill said "Paused" while the roster filed the same seat under "Stopped", the
 * chart's caption said "Stopped" and the Inbox said it "is stopped". A pause
 * is a person's decision and a stop is a fault, so every surface now says
 * "Paused" for the one and "Stopped" for the other, off this one reading.
 */
export type ShownState = SeatState | "paused";

/** Which [ShownState] a seat's row is in. */
export function shownStateOf(row: AgentRow | null | undefined): ShownState {
  const state = activityOf(row);
  return state === "stopped" && row?.stopped_reason === "paused" ? "paused" : state;
}

/**
 * The SHORT form of a seat's state: one or two words, sentence case, for a
 * pill, a chart caption, a roster group and a legend.
 *
 * Every screen that names a state in a word takes it from here, so the badge,
 * the roster, the chart and Home cannot drift apart again. The LONG form is
 * [labelOf], which always opens with this word.
 */
export function stateWord(state: ShownState): string {
  switch (state) {
    case "working":
      return "Working";
    case "needs":
      return "Needs you";
    case "paused":
      return "Paused";
    case "stopped":
      return "Stopped";
    case "idle":
      return "Idle";
    case "offline":
      return "No state yet";
  }
}

/**
 * The heading over a GROUP of seats in one engine state: a roster group, a
 * chart legend's entry, a Home tile.
 *
 * A group is keyed on the engine's `activity`, and `stopped` holds the seats a
 * person paused as well as the ones a fault stopped, so its heading says both
 * ("Paused or stopped") and its count stays true. Each seat inside it still
 * names its own state in [stateWord]'s word.
 */
export function stateGroupWord(state: SeatState): string {
  return state === "stopped" ? "Paused or stopped" : stateWord(state);
}

/** The short form of a seat row's state: [stateWord] over [shownStateOf]. */
export function seatWord(row: AgentRow | null | undefined): string {
  return stateWord(shownStateOf(row));
}

/**
 * Who a seat's pauser is, by the name a reader knows them by.
 *
 * `paused.by` is the name the engine records every author under
 * (`iam.ActorFor`): the SEAT HANDLE of a person bound to one, or the whole
 * login of anybody bound to none, an address either way, and "Paused by
 * jane-founder" names an address where a person is meant. A screen passes the
 * chart's lookup; a key the chart does not hold is a login, said in
 * [loginWords]'s plain words ("review (API token)", never `token:review`).
 */
export type NameOf = (key: string) => string;

const AS_WRITTEN: NameOf = loginWords;

/**
 * The chart's [NameOf]: a seat handle's name, and any other key as a login is
 * told ([loginWords]), so a credential's spelling never reaches a sentence.
 */
export function nameOfIn(index: OrgIndex): NameOf {
  return (key) => index.byHandle.get(key)?.name ?? loginWords(key);
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
      return "its models are at their usage limit";
    default:
      return "stopped";
  }
}

/** What a failed model call is called: a word for a tag, and a sentence for a reader. */
export interface FailureWords {
  /** One or two words, for a phase's tag: "Rate limited", "Timed out". */
  tag: string;
  /** The plain sentence a banner or an Inbox row leads with. */
  sentence: string;
}

/**
 * The classifier's kinds in plain words, keyed on the engine's own spelling
 * (`contract/failures.ts`, held to `llm.ErrorKind` by a Go gate), so a kind
 * the engine adds fails the typecheck here until it has words.
 *
 * THE MACHINE CODE IS NEVER THE HEADLINE. The seat page drew "server: llm
 * chain exhausted after 1 providers ..." and the Inbox titled the row with the
 * bare kind. The raw engine message stays available, behind the kit's
 * disclosure ([TechnicalDetails] in components/common.tsx), for whoever is
 * fixing it.
 */
const FAILURE_WORDS: Record<ProviderErrorKind, FailureWords> = {
  rate_limit: {
    tag: "Rate limited",
    sentence: "Its model provider refused the call because a rate or usage limit was reached.",
  },
  auth: {
    tag: "Sign-in needed",
    sentence: "Its model provider's sign-in has expired or is missing, so the call was refused.",
  },
  timeout: {
    tag: "Timed out",
    sentence: "The model call ran out of time before an answer came back.",
  },
  server: {
    tag: "Provider error",
    sentence: "Its model provider ran but gave no usable answer.",
  },
  canceled: {
    tag: "Canceled",
    sentence: "The turn was stopped before the model answered.",
  },
  fatal: {
    tag: "Engine fault",
    sentence: "The engine could not start the model call. An operator needs to check the setup.",
  },
};

/** What a failure the classifier did not record reads as: never its raw code. */
const FAILED: FailureWords = {
  tag: "Failed",
  sentence: "The turn ended before it finished its work.",
};

/**
 * A failure's plain words, by the kind the engine recorded. A kind outside the
 * classifier's set (a guard breach, a budget, an event type) reads as a failed
 * turn rather than as its machine code.
 */
export function failureWords(kind: string | null | undefined): FailureWords {
  return Object.hasOwn(FAILURE_WORDS, kind ?? "")
    ? FAILURE_WORDS[kind as ProviderErrorKind]
    : FAILED;
}

/**
 * The LONG form of a seat's state, as its label: "Working", "Needs you · run
 * parked", "Paused by Jane · 12m", "Stopped: its model provider is
 * unreachable", "Idle".
 *
 * IT OPENS WITH THE SHORT FORM ([seatWord]) for every state the engine
 * reports, so a pill and the line beside it can never name two states. Every other word is the engine's:
 * the reason is `stopped_reason` (said by [stoppedLine]), the pauser is
 * `paused.by`, the age is `paused.at` against the reader's clock. A reason this
 * build does not know draws "Stopped" rather than a guess.
 */
export function labelOf(
  row: AgentRow | null | undefined,
  now: number,
  nameOf: NameOf = AS_WRITTEN,
): string {
  const state = shownStateOf(row);
  const word = stateWord(state);
  switch (state) {
    case "needs":
      return row?.turn?.stage === "parked" ? `${word} · run parked` : word;
    case "offline":
      return "No state from the engine yet";
    case "paused": {
      const by = row?.paused?.by ? `${word} by ${nameOf(row.paused.by)}` : word;
      const since = row?.paused?.at ? shortAge(row.paused.at, now) : "";
      return since ? `${by} · ${since}` : by;
    }
    case "stopped": {
      const why = stoppedLine(row, nameOf);
      return why === "stopped" ? word : `${word}: ${why}`;
    }
    default:
      return word;
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
 * How many workers a seat's calls in flight are running: the tasks of every
 * `delegate` call it is waiting on, summed — a CLI runs a response's calls side
 * by side, so two fan-outs can be out at once — or 0 when none of its running
 * calls is one.
 *
 * OFF THE CALLS' OWN ARGUMENTS, because that is the one record that exists
 * WHILE the workers run: each worker's phase record lands when it finishes and
 * the batch's summary when the last one does, so both describe a fan-out that
 * is over. Arguments that do not read as the tool's schema count nothing
 * rather than a guess.
 */
export function delegatedWorkers(call: LiveCall | null | undefined): number {
  let workers = 0;
  for (const running of call?.running_calls ?? []) {
    // A WITHHELD BODY carries no arguments, so the workers are not counted
    // rather than guessed: the call is still `delegate`, and says so.
    if (running.name !== DELEGATE_TOOL || running.arguments === undefined) continue;
    try {
      const args: unknown = JSON.parse(running.arguments);
      const tasks = (args as Record<string, unknown> | null)?.[DELEGATE_TASKS];
      if (Array.isArray(tasks)) workers += tasks.length;
    } catch {
      // Not the tool's schema: counted as none.
    }
  }
  return workers;
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
    return seat.availability || "Human teammate";
  }
  const state = activityOf(row);
  const item = turnItemKey(row);
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
      // THE LINE SAYS WHAT THE STATE DOES NOT. Beside a pill reading Idle, a
      // line reading "Idle" said the state twice; a seat that has never run
      // says that instead, which is the fact the pill cannot carry.
      const last = row?.last_turn?.ended_at;
      return last ? `Idle · last turn ${shortAge(last, now)} ago` : "No turn yet";
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
 * to — never its key, which two tasks can hold (`key_collision`): keyed on it,
 * the working seat's strip was drawn on both.
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
 * A handle as a reader sees it: `@pm`, and NOTHING where there is none — a
 * builder seat the engine has not derived one for yet, or a row that names no
 * seat. A bare `@` printed beside a name read as a handle that is empty, which
 * is a claim about the seat rather than about what this client was told.
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

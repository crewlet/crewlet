/**
 * What a seat IS, and what it is doing.
 *
 * THE ENGINE DERIVES THE HIERARCHY; THIS INDEXES IT. Every screen that shows a
 * person needs the same facts about them: the handle they run under, the unit
 * they actually sit in, that unit's effective lead, who manages them and whom
 * they manage. Each of those is a rule the engine applies to the document
 * (handle derivation, root seats moved into a unit by a `unit:` reference,
 * lead inheritance, unit references in `manages`, automatic management by a
 * unit lead, which of several managers is primary), and the org projection
 * carries the result in its `derived` block. This module used to compute them
 * again in TypeScript, and the copy had drifted from Go: its handle derivation
 * lowercased "İlker Demir" into `i-lker-demir` where the engine derives
 * `ilker-demir` (the handle keys a seat's memory, so a write pinning the
 * client's version would orphan it), it never moved a root seat into the unit
 * its `unit:` field names, and its automatic management by a unit lead did
 * not follow the engine's rule for which seats a unit reference shields. So
 * no manager, lead or `manages` logic remains here, and there is no
 * client-side handle derivation at all.
 *
 * AN OLDER ENGINE SENDS NO `derived` BLOCK, and the index then says so
 * (`hierarchy: false`) rather than guessing: seats sit where the document
 * wrote them, a handle is known only where the document declares one, and
 * every question only the engine can answer (reporting lines, inherited
 * leads, placement by reference) is reported as unknown. A block that does
 * not describe the tree it arrived with is treated the same way, because a
 * chart drawn from a hierarchy that disagrees with its own seats is a chart
 * that lies.
 *
 * Resolved ONCE, into an index, and screens consume seats. Doing it per
 * screen is how the previous dashboard ended up walking the whole roster once
 * per rendered row.
 */

import type {
  AgentRow,
  CompanyDocument,
  ConfigRole,
  ConfigUnit,
  Derived,
  OrgProjection,
  OrgSeat,
  OrgUnit,
  SandboxEntry,
} from "~/protocol/index.ts";

export interface Unit {
  /** Stable React key: the unit's depth-first position, unique even when names repeat. */
  key: string;
  name: string;
  /** The engine's effective type when the hierarchy is reported, else as written ("" when unwritten). */
  type: string;
  purpose: string;
  goals: string[];
  knowledge: string[];
  /** The lead as the document writes it: a seat name, "" when the unit inherits. */
  declaredLead: string;
  /**
   * The effective lead, resolved to a seat. With no reported hierarchy this is
   * only a lead the unit declares itself, and an inherited one is unknown.
   */
  lead: Seat | null;
  leadInherited: boolean;
  channel: string;
  channelInherited: boolean;
  parent: Unit | null;
  children: Unit[];
  /** Outermost unit first, this unit last. */
  chain: Unit[];
  /** Direct members: after root seats were attached, when the hierarchy is reported. */
  seats: Seat[];
  raw: OrgUnit;
}

export interface Seat {
  /** Stable React key: the handle, or the seat's position when no handle is known. */
  key: string;
  name: string;
  /**
   * The handle this seat runs under, as the engine reports it. "" when the
   * projection carries no derived hierarchy and the document declares none:
   * a handle this client derived would be a second implementation of the
   * rule that keys the seat's memory, so it never makes one up.
   */
  handle: string;
  kind: "agent" | "human";
  goal: string;
  backstory: string;
  responsibilities: string[];
  guidelines: string[];
  /** The `manages` entries as written: seat and unit names, unexpanded. */
  manages: string[];
  availability: string;
  /** The seat's home unit, or null at the root. */
  unit: Unit | null;
  /** Outermost unit first, the home unit last. Empty at the root. */
  unitChain: Unit[];
  /** A root seat the engine moved into its unit because of a `unit:` reference. */
  placedByRef: boolean;
  /** The engine's primary manager. Null when it has none, or the hierarchy is not reported. */
  manager: Seat | null;
  /** Every seat that manages this one, in engine order. */
  managers: Seat[];
  /** Direct reports after expansion, explicit and automatic. */
  reports: Seat[];
  /** The subset of `reports` that come from leading a unit. */
  autoReports: Seat[];
  raw: OrgSeat;
}

export interface OrgIndex {
  /**
   * Whether the engine's derived hierarchy is present and describes this
   * tree. False means every reporting line, inherited lead and placement by
   * reference below is UNKNOWN, not absent, and a screen has to say so.
   */
  hierarchy: boolean;
  seats: Seat[];
  /** Seats above every unit. */
  rootSeats: Seat[];
  /** Every unit, depth-first, parents before children. */
  units: Unit[];
  /** The outermost units, in document order. */
  topUnits: Unit[];
  byHandle: Map<string, Seat>;
  /** The first seat with each name. */
  byName: Map<string, Seat>;
}

/** The route segments that open a seat's page: its handle, or its name when no handle is known. */
export function seatPath(seat: Pick<Seat, "handle" | "name">): string[] {
  // The seat screen resolves a name as well as a handle, so a seat whose
  // handle this engine did not report still has a page a link can reach.
  return ["seats", seat.handle || seat.name];
}

const list = <T>(value: T[] | null | undefined): T[] => (Array.isArray(value) ? value : []);

interface Authored {
  units: { raw: OrgUnit; parent: number }[];
  /** Seats in walk order, each with the index of the unit that contains it (-1 at the root). */
  seats: { raw: OrgSeat; container: number }[];
}

/** The tree as the document wrote it: units depth-first, seats in walk order. */
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
    declaredLead: raw.lead ?? "",
    lead: null,
    leadInherited: false,
    channel: raw.channel ?? "",
    channelInherited: false,
    parent: null,
    children: [],
    chain: [],
    seats: [],
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
    unit: null,
    unitChain: [],
    placedByRef: false,
    manager: null,
    managers: [],
    reports: [],
    autoReports: [],
    raw,
  };
}

/**
 * Lay the engine's derived block over the authored tree, or report that it
 * cannot be.
 *
 * The block names units in depth-first order and seats by name, so the
 * pairing is checked rather than assumed: the same number of units with the
 * same names in the same order, every seat accounted for exactly once, and
 * every handle it mentions belonging to one of them. Anything else returns
 * null, and the caller falls back to the authored tree.
 */
function overlay(
  authored: Authored,
  derived: Derived | undefined,
): Omit<OrgIndex, "byName"> | null {
  if (!derived || typeof derived !== "object") return null;
  const dUnits = list(derived.units);
  const dSeats = list(derived.seats);
  if (dUnits.length !== authored.units.length || dSeats.length !== authored.seats.length) {
    return null;
  }

  const units = authored.units.map(({ raw }, i) => newUnit(raw, i));
  for (let i = 0; i < units.length; i++) {
    const d = dUnits[i];
    const unit = units[i]!;
    if (!d || d.name !== unit.name) return null;
    const parent = authored.units[i]!.parent;
    unit.parent = parent < 0 ? null : units[parent]!;
    unit.parent?.children.push(unit);
    unit.chain = [...(unit.parent?.chain ?? []), unit];
    unit.type = d.type ?? unit.type;
    unit.channel = d.channel ?? "";
    unit.channelInherited = !!d.channel_inherited;
    unit.leadInherited = !!d.lead_inherited;
  }

  // Seats pair by NAME, in the order each name appears. The engine's order
  // differs from the document's only where a root seat was moved into a
  // unit, and a name can repeat only in a stored revision that predates the
  // uniqueness rule, where the engine itself resolves the first.
  const byNameQueue = new Map<string, OrgSeat[]>();
  for (const { raw } of authored.seats) {
    const name = raw.name ?? "";
    byNameQueue.set(name, [...(byNameQueue.get(name) ?? []), raw]);
  }
  const byHandle = new Map<string, Seat>();
  const seats: Seat[] = [];
  for (const d of dSeats) {
    const raw = byNameQueue.get(d.name)?.shift();
    if (!raw || !d.handle || byHandle.has(d.handle)) return null;
    const seat = newSeat(raw, d.handle, d.handle);
    // The derived kind is the engine's reading of the field, so an unknown
    // value off the wire is the same value everywhere.
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
      unit.lead = lead;
    }
  }

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
function authoredOnly(authored: Authored): Omit<OrgIndex, "byName"> {
  const units = authored.units.map(({ raw }, i) => newUnit(raw, i));
  units.forEach((unit, i) => {
    const parent = authored.units[i]!.parent;
    unit.parent = parent < 0 ? null : units[parent]!;
    unit.parent?.children.push(unit);
    unit.chain = [...(unit.parent?.chain ?? []), unit];
  });
  const seats = authored.seats.map(({ raw, container }, i) => {
    const handle = raw.handle ?? "";
    const seat = newSeat(raw, handle, handle || `s${i}`);
    if (container >= 0) {
      const unit = units[container]!;
      seat.unit = unit;
      seat.unitChain = unit.chain;
      unit.seats.push(seat);
    }
    return seat;
  });
  const byHandle = new Map<string, Seat>();
  for (const seat of seats)
    if (seat.handle && !byHandle.has(seat.handle)) byHandle.set(seat.handle, seat);
  // A lead the unit DECLARES is a fact of the document and names a seat by
  // its exact name; an inherited one is the engine's conclusion, so it stays
  // unknown here.
  const firstByName = new Map<string, Seat>();
  for (const seat of seats) if (!firstByName.has(seat.name)) firstByName.set(seat.name, seat);
  for (const unit of units)
    unit.lead = unit.declaredLead ? (firstByName.get(unit.declaredLead) ?? null) : null;
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
export function indexOrg(org: OrgProjection | null | undefined): OrgIndex {
  const authored = walk(org);
  const built = overlay(authored, org?.derived) ?? authoredOnly(authored);
  const byName = new Map<string, Seat>();
  for (const seat of built.seats) if (!byName.has(seat.name)) byName.set(seat.name, seat);
  return { ...built, byName };
}

// ---------------------------------------------------------------------------
// The guarded half
// ---------------------------------------------------------------------------

/** What the company document holds for one seat, or why it cannot be said. */
export type SeatSettings =
  | { state: "found"; role: ConfigRole; unit: ConfigUnit | null }
  | { state: "missing" }
  | { state: "ambiguous" };

/**
 * The document's own entry for a seat, from the operator-gated `config` answer.
 *
 * Contact identities, email, the model, the token budget, schedules, the
 * seat's integration blocks and its tool credential names are not on the
 * anonymous projection, so the seat screen reads them here. The seat is found
 * by NAME, which is the identity the document itself addresses a seat by;
 * two seats with one name can only come from a revision stored before names
 * had to be unique, and attributing either one's settings to the page would
 * be a guess, so that answer is `ambiguous`.
 *
 * `unit` is the document's entry for the seat's home unit, whose `mcp_env`
 * its direct agent members inherit. Null at the root, and null where the name
 * repeats for the same reason.
 */
export function seatSettings(doc: CompanyDocument | null | undefined, seat: Seat): SeatSettings {
  const roles: ConfigRole[] = [];
  const units: ConfigUnit[] = [];
  const visit = (unit: ConfigUnit): void => {
    units.push(unit);
    for (const role of list(unit.roles)) roles.push(role);
    for (const child of list(unit.children)) visit(child);
  };
  for (const role of list(doc?.roles)) roles.push(role);
  for (const unit of list(doc?.units)) visit(unit);

  const matches = roles.filter((r) => r.name === seat.name);
  if (matches.length === 0) return { state: "missing" };
  if (matches.length > 1) return { state: "ambiguous" };
  const home = seat.unit ? units.filter((u) => u.name === seat.unit?.name) : [];
  return { state: "found", role: matches[0]!, unit: home.length === 1 ? home[0]! : null };
}

// ---------------------------------------------------------------------------
// Live state
// ---------------------------------------------------------------------------

export type RunState =
  "working" | "awaiting_sandbox" | "idle" | "afk" | "failed" | "terminated" | "offline" | "human";

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
  if (sandbox && sandbox.status === "awaiting_input") return "needs";
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
    return sandbox.status === "awaiting_input"
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

/**
 * What a seat IS, and what it is doing.
 *
 * The org tree arrives verbatim — the config's own field names — and every
 * screen that shows a person needs the same four derivations off it: the unit
 * chain, the effective (possibly inherited) unit lead, the manager, and the
 * MCP surfaces the seat inherits. Doing that per screen is how the previous
 * dashboard ended up walking the whole roster once per row: `managerOf` was a
 * linear scan called per seat AND again per rendered row, which on a 200-seat
 * company was roughly 80,000 array scans per event push.
 *
 * So it is resolved ONCE, into an index, and screens consume seats.
 */

import type { AgentRow, OrgRole, OrgTree, OrgUnit, SandboxEntry } from "~/protocol/index.ts";

export interface Seat {
  name: string;
  handle: string;
  kind: "agent" | "human";
  goal: string;
  backstory: string;
  email: string;
  responsibilities: string[];
  guidelines: string[];
  manages: string[];
  tokenBudget: number;
  llm: string;
  llmAuxiliary: string;
  availability: string;
  contact: Record<string, string>;
  /** Root → own unit. Empty for a root-level seat. */
  unitChain: OrgUnit[];
  unit: OrgUnit | null;
  /** The unit's own lead, or the nearest ancestor's — lead inheritance. */
  unitLead: string;
  /** MCP env merged down the unit chain, the seat's own entries winning. */
  mcpEnv: Record<string, Record<string, string>>;
  schedules: { name: string; cron: string; task: string }[];
  raw: OrgRole;
}

export interface OrgIndex {
  seats: Seat[];
  byHandle: Map<string, Seat>;
  byName: Map<string, Seat>;
  /** seat name → the seat that manages it. */
  managerOf: Map<string, Seat>;
  /** seat name → the seats it manages, expanded from unit names. */
  reportsOf: Map<string, Seat[]>;
  units: OrgUnit[];
}

function mergeEnv(
  chain: OrgUnit[],
  own: Record<string, Record<string, string>> | undefined,
): Record<string, Record<string, string>> {
  const out: Record<string, Record<string, string>> = {};
  for (const unit of chain) {
    for (const [server, vars] of Object.entries(unit.mcp_env ?? {})) {
      out[server] = { ...(out[server] ?? {}), ...vars };
    }
  }
  for (const [server, vars] of Object.entries(own ?? {})) {
    out[server] = { ...(out[server] ?? {}), ...vars };
  }
  return out;
}

function toSeat(role: OrgRole, chain: OrgUnit[], inheritedLead: string): Seat {
  const unit = chain.length ? (chain[chain.length - 1] as OrgUnit) : null;
  return {
    name: role.name,
    // The engine derives an empty handle from the role name the same way.
    handle: role.handle || slugify(role.name),
    kind: role.kind === "human" ? "human" : "agent",
    goal: role.goal ?? "",
    backstory: role.backstory ?? "",
    email: role.email ?? "",
    responsibilities: role.responsibilities ?? [],
    guidelines: role.behavioral_guidelines ?? [],
    manages: role.manages ?? [],
    tokenBudget: role.token_budget ?? 0,
    llm: role.llm ?? "",
    llmAuxiliary: role.llm_auxiliary ?? "",
    availability: role.availability ?? "",
    contact: role.contact ?? {},
    unitChain: chain,
    unit,
    unitLead: unit?.lead || inheritedLead,
    mcpEnv: mergeEnv(chain, role.mcp_env),
    schedules: (role.schedules ?? []).map((s) => ({ name: s.name, cron: s.cron, task: s.task })),
    raw: role,
  };
}

export function slugify(name: string): string {
  return String(name ?? "")
    .toLowerCase()
    .replace(/[^a-z0-9]+/g, "-")
    .replace(/^-+|-+$/g, "");
}

/** Flatten the org tree once. Everything a screen needs comes off this. */
export function indexOrg(org: OrgTree | null | undefined): OrgIndex {
  const seats: Seat[] = [];
  const units: OrgUnit[] = [];

  for (const role of org?.roles ?? []) seats.push(toSeat(role, [], ""));

  const walk = (unit: OrgUnit, chain: OrgUnit[], inheritedLead: string): void => {
    units.push(unit);
    const nextChain = [...chain, unit];
    // Lead inheritance: a child unit with no lead of its own takes the nearest
    // ancestor's, cascading through any number of levels.
    const lead = unit.lead || inheritedLead;
    for (const role of unit.roles ?? []) seats.push(toSeat(role, nextChain, lead));
    for (const child of unit.children ?? []) walk(child, nextChain, lead);
  };
  for (const unit of org?.units ?? []) walk(unit, [], "");

  const byHandle = new Map(seats.map((s) => [s.handle, s]));
  const byName = new Map(seats.map((s) => [s.name, s]));
  const unitByName = new Map(units.map((u) => [u.name, u]));

  // `manages` accepts role names AND unit names, and a unit name expands to
  // every role in it INCLUDING descendants. A name that matches both is the
  // role — the engine resolves it the same way.
  const seatsInUnit = (name: string): Seat[] =>
    seats.filter((s) => s.unitChain.some((u) => u.name === name));

  const reportsOf = new Map<string, Seat[]>();
  const managerOf = new Map<string, Seat>();
  for (const seat of seats) {
    const reports: Seat[] = [];
    for (const entry of seat.manages) {
      const asRole = byName.get(entry);
      if (asRole) {
        reports.push(asRole);
        continue;
      }
      if (unitByName.has(entry)) reports.push(...seatsInUnit(entry));
    }
    const unique = [...new Map(reports.map((r) => [r.name, r])).values()].filter(
      (r) => r.name !== seat.name,
    );
    reportsOf.set(seat.name, unique);
    for (const r of unique) if (!managerOf.has(r.name)) managerOf.set(r.name, seat);
  }

  // A unit lead auto-manages any seat in its unit nobody else already does.
  for (const seat of seats) {
    if (managerOf.has(seat.name) || !seat.unitLead || seat.unitLead === seat.name) continue;
    const lead = byName.get(seat.unitLead);
    if (!lead) continue;
    managerOf.set(seat.name, lead);
    reportsOf.set(lead.name, [...(reportsOf.get(lead.name) ?? []), seat]);
  }

  return { seats, byHandle, byName, managerOf, reportsOf, units };
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
  plan: "planning the work",
  execute: "carrying out the plan",
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

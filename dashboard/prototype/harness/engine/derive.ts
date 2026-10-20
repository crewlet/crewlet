/**
 * What the engine derives from a company document, derived the same way:
 * the hierarchy (`config.Derive`), the anonymous org projection
 * (`internal/api/orgprojection.go`), the roster the live overlay is merged
 * onto (`internal/api/roster.go`) and the resolved schedule rows.
 *
 * A PORT, rule for rule, of the Go it names, because the dashboard draws
 * every chart, reporting line and unit membership from these answers and
 * never derives one of its own: a demo that guessed would draw an org the
 * engine would not, and an org edit made in the prototype would land
 * somewhere the real builder would not put it. The order of every list is
 * the engine's, since screens list in it.
 */

import type {
  AgentRow,
  CompanyDocument,
  ConfigRole,
  ConfigUnit,
  Derived,
  DerivedSeat,
  DerivedUnit,
  OrgProjection,
  OrgSeat,
  OrgUnit,
  ScheduleRow,
  ScheduleSpec,
  TokenBudget,
} from "~/protocol/types.ts";
import { AVATAR_CHARACTERS, AVATAR_COLORS } from "~/contract/avatar.ts";
import { nextFires } from "~/lib/cron.ts";
import { agentIdFor } from "./ids.ts";
import { cronProblem, zoneProblem } from "./schedules.ts";

/**
 * The engine's own slug rule for a handle derived from a name (`org.Slugify`).
 *
 * LOWERCASED A RUNE AT A TIME, as Go's `strings.ToLower` is: JavaScript's
 * special casing lowers a dotted capital I to an i with a combining dot, which
 * would derive a different seat than the engine's plain i.
 */
export function slugify(name: string): string {
  return Array.from(name.trim(), (ch) => (ch === "\u0130" ? "i" : ch.toLowerCase()))
    .join("")
    .replace(/[^a-z0-9]+/g, "-")
    .replace(/^-+|-+$/g, "");
}

/** The handle a seat runs under: the one it declares, or its name's slug. */
export function handleOf(role: Pick<ConfigRole, "name" | "handle">): string {
  return role.handle || slugify(role.name);
}

/**
 * The key a unit gets where its document declares none (`config.MintUnitID`):
 * its name reduced to the key grammar, `unit-` prefixed where that would be
 * empty or start with something other than a letter.
 */
function mintUnitId(name: string): string {
  let slug = slugify(name);
  if (slug === "") slug = "unit";
  else if (slug[0]! < "a" || slug[0]! > "z") slug = `unit-${slug}`;
  if (slug.length > 64) slug = slug.slice(0, 64).replace(/[-_]+$/, "");
  return slug;
}

/** A unit's KEY (`org.Unit.Key`): its id, or its name where it has none. */
export function unitKeyOf(unit: Pick<ConfigUnit, "id" | "name">): string {
  return unit.id?.trim() || unit.name.trim();
}

/**
 * Write every unit's key and every seat's handle into `doc` where it declares
 * none (`config.MintIdentities`), as every engine write does before it stores
 * a document, so a name corrected later moves neither.
 */
export function mintIdentities(doc: CompanyDocument): void {
  const roles = (list: ConfigRole[] | undefined) => {
    for (const role of list ?? []) if (!role.handle?.trim()) role.handle = handleOf(role);
  };
  const units = (list: ConfigUnit[] | undefined) => {
    for (const unit of list ?? []) {
      if (!unit.id?.trim()) unit.id = mintUnitId(unit.name);
      roles(unit.roles);
      units(unit.children);
    }
  };
  roles(doc.roles);
  units(doc.units);
}

/** The phases a seat's chain is resolved for, in the engine's order (`phase.All`). */
const PHASES = ["execute", "review", "subagent", "auxiliary", "judge", "sandbox", "onboarding"];

/** A seat as the engine's normalised organization holds it. */
interface RunSeat {
  role: ConfigRole;
  handle: string;
  /** Where the document wrote it. */
  path: string;
  /** Where it sits once root seats are moved into the unit they reference. */
  home: RunUnit | null;
  placedByRef: boolean;
  /** `manages` as seat handles: auto-managed reports added and unit keys expanded. */
  manages: string[];
  autoManaged: string[];
}

/** A unit as the engine's normalised organization holds it. */
interface RunUnit {
  unit: ConfigUnit;
  path: string;
  type: string;
  declaredLead: string;
  lead: string;
  declaredChannel: string;
  channel: string;
  roles: RunSeat[];
  children: RunUnit[];
}

/** The document normalised as a running node normalises it (`org.Organization.Normalize`). */
interface Organization {
  roots: RunSeat[];
  units: RunUnit[];
}

function organize(doc: CompanyDocument): Organization {
  const seat = (role: ConfigRole, path: string): RunSeat => ({
    role,
    handle: handleOf(role),
    path,
    home: null,
    placedByRef: false,
    manages: [...(role.manages ?? [])],
    autoManaged: [],
  });
  const unitOf = (unit: ConfigUnit, path: string): RunUnit => {
    const run: RunUnit = {
      unit,
      path,
      type: unit.type || "team",
      declaredLead: unit.lead ?? "",
      lead: unit.lead ?? "",
      declaredChannel: unit.channel ?? "",
      channel: unit.channel ?? "",
      roles: [],
      children: [],
    };
    run.roles = (unit.roles ?? []).map((r, i) => seat(r, `${path}.roles[${i}]`));
    for (const s of run.roles) s.home = run;
    run.children = (unit.children ?? []).map((c, i) => unitOf(c, `${path}.children[${i}]`));
    return run;
  };
  const org: Organization = {
    roots: (doc.roles ?? []).map((r, i) => seat(r, `roles[${i}]`)),
    units: (doc.units ?? []).map((u, i) => unitOf(u, `units[${i}]`)),
  };

  // A root seat naming a unit, by its key, moves into it.
  if (org.units.length > 0) {
    const kept: RunSeat[] = [];
    for (const s of org.roots) {
      const target = s.role.unit ? findUnit(org, s.role.unit) : null;
      if (!target) {
        kept.push(s);
        continue;
      }
      target.roles.push(s);
      s.home = target;
      s.placedByRef = true;
    }
    org.roots = kept;
  }
  // Leads and channels inherited down the tree.
  const propagate = (u: RunUnit, lead: string, channel: string): void => {
    u.lead = u.declaredLead || lead;
    u.channel = u.declaredChannel || channel;
    for (const c of u.children) propagate(c, u.lead, u.channel);
  };
  for (const u of org.units) propagate(u, "", "");

  const index = managesIndex(org);
  autoManageByLead(org, index);
  for (const s of allSeats(org)) {
    if (s.manages.length === 0) continue;
    const expanded: string[] = [];
    for (const entry of s.manages) {
      for (const handle of index.resolve(s.handle, entry)) {
        if (!expanded.includes(handle)) expanded.push(handle);
      }
    }
    s.manages = expanded;
  }
  return org;
}

/** Every unit, depth first, parents before children. */
function allUnits(org: Organization): RunUnit[] {
  const out: RunUnit[] = [];
  const walk = (u: RunUnit): void => {
    out.push(u);
    u.children.forEach(walk);
  };
  org.units.forEach(walk);
  return out;
}

/** Every seat in the engine's order: the root seats, then each unit's, depth first. */
function allSeats(org: Organization): RunSeat[] {
  const out = [...org.roots];
  const walk = (u: RunUnit): void => {
    out.push(...u.roles);
    u.children.forEach(walk);
  };
  org.units.forEach(walk);
  return out;
}

/** The unit a reference names, by its KEY (`Organization.Unit`). */
function findUnit(org: Organization, key: string): RunUnit | null {
  return allUnits(org).find((u) => unitKeyOf(u.unit) === key) ?? null;
}

/** The seat with this HANDLE (`Organization.Role`). */
function seatByHandle(org: Organization, handle: string): RunSeat | null {
  return allSeats(org).find((s) => s.handle === handle) ?? null;
}

/** Every seat a unit holds, its descendants' included. */
function seatsUnder(u: RunUnit): RunSeat[] {
  return [...u.roles, ...u.children.flatMap(seatsUnder)];
}

/**
 * What a `manages:` entry stands for (`org.managesIndex`): a seat's HANDLE is
 * that seat, a unit's KEY every seat in its subtree but the manager, and
 * anything else itself, verbatim.
 */
function managesIndex(org: Organization) {
  const seats = new Set(allSeats(org).map((s) => s.handle));
  const unitSeats = new Map<string, string[]>();
  for (const u of allUnits(org)) {
    const key = unitKeyOf(u.unit);
    if (!unitSeats.has(key))
      unitSeats.set(
        key,
        seatsUnder(u).map((s) => s.handle),
      );
  }
  const resolve = (manager: string, entry: string): string[] => {
    if (seats.has(entry)) return [entry];
    const members = unitSeats.get(entry);
    if (!members) return [entry];
    return members.filter((handle) => handle !== manager);
  };
  const managed = (s: RunSeat): Set<string> =>
    new Set(s.manages.flatMap((entry) => resolve(s.handle, entry)));
  return { resolve, managed };
}

function autoManageByLead(org: Organization, index: ReturnType<typeof managesIndex>): void {
  for (const u of allUnits(org)) {
    if (!u.lead || u.roles.length === 0) continue;
    const lead = seatByHandle(org, u.lead);
    if (!lead) continue;
    const shielded = new Set<string>();
    const managedBy = new Map<RunSeat, Set<string>>();
    for (const r of u.roles) {
      const set = index.managed(r);
      managedBy.set(r, set);
      for (const handle of set) shielded.add(handle);
    }
    const byLead = index.managed(lead);
    for (const r of u.roles) {
      const handle = r.handle;
      if (handle === u.lead || shielded.has(handle) || byLead.has(handle)) continue;
      if (managedBy.get(r)?.has(u.lead)) continue;
      lead.manages.push(handle);
      lead.autoManaged.push(handle);
      byLead.add(handle);
    }
  }
}

/** The unit chain a seat onboards against, outermost first. */
function unitChain(org: Organization, s: RunSeat): string[] {
  const find = (u: RunUnit): string[] | null => {
    if (u.roles.includes(s)) return [u.unit.name];
    for (const c of u.children) {
      const chain = find(c);
      if (chain) return [u.unit.name, ...chain];
    }
    return null;
  };
  for (const u of org.units) {
    const chain = find(u);
    if (chain) return chain;
  }
  return [];
}

const orNull = <T>(list: T[]): T[] | null => (list.length > 0 ? list : null);

/**
 * The hierarchy the engine derives from `doc`, as `config.Derive` writes it;
 * `withPaths` false is the anonymous projection's form (`WithoutPaths`).
 */
export function derive(doc: CompanyDocument, withPaths = true): Derived {
  const org = organize(doc);
  const seats = allSeats(org);
  const units = allUnits(org);
  const derivedSeats: DerivedSeat[] = seats.map((s) => {
    const managers = seats.filter((m) => m.manages.includes(s.handle));
    const seat: DerivedSeat = {
      handle: s.handle,
      name: s.role.name,
      kind: s.role.kind === "human" ? "human" : "agent",
      manager: managers[0]?.handle ?? "",
      managers: orNull(managers.map((m) => m.handle)),
      reports: orNull(
        s.manages.flatMap((handle) => {
          const found = seatByHandle(org, handle);
          return found ? [found.handle] : [];
        }),
      ),
      auto_reports: orNull(
        s.autoManaged.flatMap((handle) => {
          const found = seatByHandle(org, handle);
          return found ? [found.handle] : [];
        }),
      ),
      onboarding_chain: orNull(unitChain(org, s)),
    };
    if (withPaths) {
      // ABSENT FROM THE PROJECTION with the path: it has no document to have
      // been moved in.
      seat.placed_by_ref = s.placedByRef;
      seat.path = s.path;
      // OMITTED AT THE ROOT, as the engine's `omitempty` leaves it.
      if (s.home) seat.unit_path = s.home.path;
    }
    return seat;
  });
  const derivedUnits: DerivedUnit[] = units.map((u) => {
    const lead = u.lead ? seatByHandle(org, u.lead) : null;
    const unit: DerivedUnit = {
      id: unitKeyOf(u.unit),
      name: u.unit.name,
      type: u.type,
      lead: lead?.handle ?? "",
      lead_inherited: lead ? u.declaredLead === "" : false,
      channel: u.channel,
      channel_inherited: u.declaredChannel === "" && u.channel !== "",
      seats: orNull(u.roles.map((r) => r.handle)),
    };
    if (withPaths) unit.path = u.path;
    return unit;
  });
  return { seats: orNull(derivedSeats), units: orNull(derivedUnits) };
}

/** `providers.llm`'s keys, in the order the document writes them. */
function providerOrder(doc: CompanyDocument): string[] {
  const llm = (doc.providers as { llm?: Record<string, unknown> } | undefined)?.llm;
  return llm ? Object.keys(llm) : [];
}

/** One phase's chain as a turn resolves it (`phase.Resolve`), over provider keys alone. */
function chainFor(role: ConfigRole, ph: string, order: string[]): string[] {
  const asList = (v: unknown): string[] =>
    typeof v === "string"
      ? [v]
      : Array.isArray(v)
        ? v.filter((k): k is string => typeof k === "string")
        : [];
  const llm = role.llm;
  const mapping =
    llm && typeof llm === "object" && !Array.isArray(llm) ? (llm as Record<string, unknown>) : null;
  let candidates = asList(role[`llm_${ph}`]);
  if (candidates.length === 0 && mapping) candidates = asList(mapping[ph]);
  if (candidates.length === 0) candidates = mapping ? asList(mapping.default) : asList(llm);
  const known = candidates.filter((k, i) => k && order.includes(k) && candidates.indexOf(k) === i);
  return known.length > 0 ? known : order.slice(0, 1);
}

/** The MCP servers declared in the document, and whom each grants. */
function serversOf(
  doc: CompanyDocument,
): { name: string; grants: (role: ConfigRole) => boolean }[] {
  const servers =
    (doc.mcp_servers as { name?: string; roles?: string[]; shared?: boolean }[] | undefined) ?? [];
  return servers.flatMap((server) =>
    server.name
      ? [
          {
            name: server.name,
            grants: (role: ConfigRole) =>
              server.shared === true ||
              !server.roles ||
              server.roles.length === 0 ||
              server.roles.includes(role.name) ||
              server.roles.includes(handleOf(role)),
          },
        ]
      : [],
  );
}

function tokenBudget(budget: TokenBudget | undefined): TokenBudget | undefined {
  return budget && Object.keys(budget).length > 0 ? { ...budget } : undefined;
}

/** The company as the anonymous `org` push and the snapshot carry it. */
export function orgProjection(doc: CompanyDocument): OrgProjection {
  const order = providerOrder(doc);
  const servers = serversOf(doc);
  const seats = (roles: ConfigRole[] | undefined): OrgSeat[] | undefined => {
    if (!roles || roles.length === 0) return undefined;
    return roles.map((r) => {
      const seat: OrgSeat = { name: r.name };
      if (r.kind) seat.kind = r.kind;
      if (r.handle) seat.handle = r.handle;
      if (r.goal) seat.goal = r.goal;
      if (r.backstory) seat.backstory = r.backstory;
      if (r.responsibilities?.length) seat.responsibilities = [...r.responsibilities];
      if (r.behavioral_guidelines?.length)
        seat.behavioral_guidelines = [...r.behavioral_guidelines];
      if (r.manages?.length) seat.manages = [...r.manages];
      if (r.availability) seat.availability = r.availability;
      const budget = tokenBudget(r.token_budget);
      if (budget) seat.token_budget = budget;
      if (r.kind !== "human") {
        if (order.length > 0) {
          seat.llm = Object.fromEntries(PHASES.map((ph) => [ph, chainFor(r, ph, order)]));
        }
        seat.tool_sources = [
          "builtin",
          ...servers.filter((s) => s.grants(r)).map((s) => `mcp:${s.name}`),
        ];
        seat.avatar = {
          character: r.avatar?.character ?? AVATAR_CHARACTERS[0],
          color: r.avatar?.color ?? AVATAR_COLORS[0],
        };
      }
      return seat;
    });
  };
  const units = (list: ConfigUnit[] | undefined): OrgUnit[] | undefined => {
    if (!list || list.length === 0) return undefined;
    return list.map((u) => {
      const unit: OrgUnit = { id: unitKeyOf(u), name: u.name };
      if (u.type) unit.type = u.type;
      if (u.purpose) unit.purpose = u.purpose;
      if (u.lead) unit.lead = u.lead;
      if (u.goals?.length) unit.goals = [...u.goals];
      if (u.channel) unit.channel = u.channel;
      if (u.knowledge?.length) unit.knowledge = [...u.knowledge];
      const roles = seats(u.roles);
      if (roles) unit.roles = roles;
      const children = units(u.children);
      if (children) unit.children = children;
      return unit;
    });
  };
  const projection: OrgProjection = {
    name: doc.name ?? "",
    mission: doc.mission ?? "",
    vision: doc.vision ?? "",
    policies: [...(doc.policies ?? [])],
    timezone: companyZone(doc),
    derived: derive(doc, false),
  };
  const budget = tokenBudget(doc.token_budget as TokenBudget | undefined);
  if (budget) projection.token_budget = budget;
  const roles = seats(doc.roles);
  if (roles) projection.roles = roles;
  const tree = units(doc.units);
  if (tree) projection.units = tree;
  return projection;
}

/** The company's one clock (ADR-0018): the zone the document writes, or UTC. */
export function companyZone(doc: CompanyDocument): string {
  return typeof doc.timezone === "string" && doc.timezone ? doc.timezone : "UTC";
}

/**
 * The static seat rows the live overlay is merged onto: every AGENT seat, in
 * the engine's order, by handle (`internal/api/roster.go`).
 */
export function roster(doc: CompanyDocument): AgentRow[] {
  return (derive(doc).seats ?? [])
    .filter((s) => s.kind === "agent")
    .map((s) => ({
      id: s.handle,
      agent_id: agentIdFor(doc.name ?? "", s.handle),
      role: s.name,
      handle: s.handle,
    }));
}

/** Every agent seat's handle, name and agent id. */
export function agentSeats(
  doc: CompanyDocument,
): { handle: string; name: string; agentId: string }[] {
  return roster(doc).map((r) => ({
    handle: r.handle ?? r.id,
    name: r.role,
    agentId: r.agent_id ?? "",
  }));
}

/** The cap on one scheduled turn where a schedule names none (`org.DefaultScheduleTimeout`). */
const DEFAULT_SCHEDULE_TIMEOUT_SECONDS = 180;

/**
 * Who a unit schedule's fire reaches (`schedule.Entry.Runners`): the unit's
 * effective lead for `target: lead`, unless that lead is a person, and
 * otherwise every direct agent member, `each` being the default.
 */
function unitRunners(org: Organization, u: RunUnit, spec: ScheduleSpec): string[] {
  if (spec.target === "lead") {
    const lead = u.lead ? seatByHandle(org, u.lead) : null;
    return lead && lead.role.kind !== "human" ? [lead.handle] : [];
  }
  return u.roles.filter((r) => r.role.kind !== "human").map((r) => r.handle);
}

/**
 * What decides whether a unit's schedules can run, by the unit as the
 * document holds it: its direct agent members (`Unit.hasDirectAgent`) and its
 * effective lead's seat (`Organization.EffectiveLead`).
 */
export function unitRunnerFacts(
  doc: CompanyDocument,
): Map<ConfigUnit, { agents: string[]; lead: ConfigRole | null }> {
  const org = organize(doc);
  return new Map(
    allUnits(org).map((u) => [
      u.unit,
      {
        agents: u.roles.filter((r) => r.role.kind !== "human").map((r) => r.handle),
        lead: u.lead ? (seatByHandle(org, u.lead)?.role ?? null) : null,
      },
    ]),
  );
}

/** The span `cron.Expr.Next` looks ahead before it gives up (`cron.Horizon`, 2925 days), as Go prints it. */
const CRON_HORIZON = "70200h0m0s";

/**
 * Why an enabled schedule has no next fire (`schedule.nextRun`): a zone of its
 * own that does not load, an expression that does not parse, or a date the
 * calendar never reaches.
 */
function nextRunProblem(spec: ScheduleSpec): string {
  return (
    (spec.timezone ? zoneProblem(spec.timezone) : null) ??
    cronProblem(spec.cron) ??
    `no fire within ${CRON_HORIZON}`
  );
}

/**
 * Every declared schedule, resolved as `schedule.Describe` resolves it: the
 * zone it fires in, the seats a fire reaches and the next fire. A unit's
 * target is `each` where it names none, and a role's is empty. A seat's
 * schedule is keyed on its AGENT ID and named by its handle; a unit's is keyed
 * and named by its key (`schedule.Entry`).
 */
export function scheduleRows(doc: CompanyDocument, now: Date): ScheduleRow[] {
  const org = organize(doc);
  const zone = companyZone(doc);
  const row = (
    scopeType: "role" | "unit",
    scopeId: string,
    scopeName: string,
    spec: ScheduleSpec,
    runners: string[],
  ): ScheduleRow => {
    const enabled = spec.enabled !== false;
    const tz = spec.timezone || zone;
    const next = enabled ? nextFires(spec.cron, now, tz, 1)[0] : undefined;
    const timeout = spec.timeout_seconds ?? 0;
    const out: ScheduleRow = {
      scope_type: scopeType,
      scope_id: scopeId,
      scope_name: scopeName,
      name: spec.name,
      cron: spec.cron,
      timezone: tz,
      task: spec.task,
      target: scopeType === "unit" ? spec.target || "each" : "",
      enabled,
      timeout_seconds: timeout > 0 ? timeout : DEFAULT_SCHEDULE_TIMEOUT_SECONDS,
      catchup: spec.catchup !== false,
      runners,
    };
    if (next) out.next_run = next.toISOString();
    else if (enabled) out.problem = nextRunProblem(spec);
    return out;
  };
  const rows: ScheduleRow[] = [];
  for (const s of allSeats(org)) {
    const id = agentIdFor(doc.name ?? "", s.handle);
    for (const spec of s.role.schedules ?? [])
      rows.push(row("role", id, s.handle, spec, [s.handle]));
  }
  for (const u of allUnits(org)) {
    for (const spec of u.unit.schedules ?? []) {
      const key = unitKeyOf(u.unit);
      rows.push(row("unit", key, key, spec, unitRunners(org, u, spec)));
    }
  }
  return rows;
}

/**
 * A schedule scope as a person reads it (`queries.scopeNames`): a seat's
 * handle for its agent id, and anything else, a unit's key above all, as itself.
 */
export function scopeNameOf(doc: CompanyDocument, scope: string, id: string): string {
  if (scope !== "role") return id;
  return agentSeats(doc).find((s) => s.agentId === id)?.handle ?? id;
}

/** A seat's authored role, by handle, wherever the document wrote it. */
export function roleByHandle(doc: CompanyDocument, handle: string): ConfigRole | null {
  return allSeats(organize(doc)).find((s) => s.handle === handle)?.role ?? null;
}

/**
 * The project a seat's work is filed in: its own `project`, else the nearest
 * one its unit or an ancestor declares (`engine.ProjectOfSeat`), or "".
 */
export function projectOfSeat(doc: CompanyDocument, handle: string): string {
  const org = organize(doc);
  const seat = allSeats(org).find((s) => s.handle === handle);
  if (!seat) return "";
  const own = seat.role.project?.trim();
  if (own) return own;
  const units = allUnits(org);
  for (let u = seat.home ?? null; u; u = units.find((p) => p.children.includes(u!)) ?? null) {
    const project = u.unit.project?.trim();
    if (project) return project;
  }
  return "";
}

/** Every agent seat's role and handle, in the engine's order (`Company.EachRole`, people left out). */
export function agentRoles(doc: CompanyDocument): { handle: string; role: ConfigRole }[] {
  return allSeats(organize(doc))
    .filter((s) => s.role.kind !== "human")
    .map((s) => ({ handle: s.handle, role: s.role }));
}

/**
 * The unit a seat sits in once root seats are placed, by handle: its KEY,
 * which is what a row is filed under, and its name, which is what a band is
 * called. Both "" at the root.
 */
export function unitOfSeat(doc: CompanyDocument, handle: string): { key: string; name: string } {
  const home = allSeats(organize(doc)).find((s) => s.handle === handle)?.home;
  return home ? { key: unitKeyOf(home.unit), name: home.unit.name } : { key: "", name: "" };
}

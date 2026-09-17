/**
 * What a save changes, and what follows from it.
 *
 * DERIVED FROM THE TWO ORGANIZATIONS, NEVER FROM OPERATION TYPES ALONE. A
 * rename does not only rename: it re-onboards the seats whose chain it is in
 * and gives a unit's schedules a new identity. A reorder can change who a seat
 * reports to, because the engine's primary manager is the first seat that
 * lists it. Deleting a lead changes the lead of every child unit that
 * inherited it, and where unrouted tracker work goes. None of that is visible
 * in the operation that caused it, so this module compares the BASE and the
 * DRAFT as the engine derived them (each side's `derived` block, placed on
 * nodes through that side's own path index) and reads the operation log only
 * for what no document can say afterwards: the fields a kind change removed,
 * and the references an operation cleared.
 *
 * Nodes are matched by key, which is the engine's identity (see `keys.ts`), so
 * "the same seat" here means what it means to the engine's memory and
 * mailboxes, not "a seat with the same name".
 *
 * A SIDE WITHOUT A DERIVATION SAYS SO. Every consequence that needs the
 * engine's derivation (handles, onboarding, reporting lines, effective leads
 * and channels, routing, inherited credentials) is left empty and
 * `derivedKnown` is false, rather than filled in by a guess. An empty
 * organization needs no derivation to be known.
 */

import type { CompanyDocument, Derived, DerivedSeat, DerivedUnit } from "~/protocol/index.ts";
import { plural } from "~/lib/format.ts";
import { COMPANY_KEY, type NodeKey } from "./keys.ts";
import { allSeats, allUnits, type Draft } from "./draft.ts";
import {
  CHARTER_FIELDS,
  DATADOG_ROUTE_TO,
  GITLAB_ACCESS_LEVELS,
  type PathIndex,
  placeDerivation,
  toDocument,
} from "./document.ts";
import { getPath, isRecord, jsonEqual } from "./json.ts";
import {
  fieldName,
  isCredentialField,
  kindOf,
  type ApplyReport,
  type Operation,
  type ReferenceEffect,
} from "./operations.ts";

/** One side of the comparison: a keyed draft and the engine's derivation of its document. */
export interface Side {
  readonly draft: Draft;
  /** The derivation of `toDocument(draft).document`, or `null` when none is known. */
  readonly derived: Derived | null;
}

export interface ChangeInputs {
  readonly base: Side;
  readonly next: Side;
  /** The draft's operation log, and what applying each one did (aligned). */
  readonly ops: readonly Operation[];
  readonly reports: readonly ApplyReport[];
  /** Seats removed in recent revisions: handle to the name the seat had. */
  readonly recentlyRemoved?: ReadonlyMap<string, string>;
}

/** A seat or a unit, by key, with the name it has on the side it was read from. */
export interface EntityRef {
  readonly key: NodeKey;
  readonly kind: "seat" | "unit" | "company";
  readonly name: string;
}

export type OnboardingCause = "company_rename" | "seat_rename" | "unit_rename" | "move";

export type Acknowledgement =
  "company_rename" | "handle_change" | "kind_change" | "credential_servers" | "mass_removal";

export interface ChangeSet {
  readonly derivedKnown: boolean;
  readonly added: readonly EntityRef[];
  readonly removed: readonly EntityRef[];
  readonly renamed: readonly {
    readonly ref: EntityRef;
    readonly before: string;
    readonly after: string;
  }[];
  readonly moved: readonly {
    readonly ref: EntityRef;
    readonly from: EntityRef;
    readonly to: EntityRef;
  }[];
  readonly edited: readonly { readonly ref: EntityRef; readonly fields: readonly string[] }[];
  /** Charter fields that changed. */
  readonly charter: readonly string[];
  readonly companyRename: { readonly before: string; readonly after: string } | null;
  readonly handleChanges: readonly {
    readonly ref: EntityRef;
    readonly before: string;
    readonly after: string;
  }[];
  /** Agent seats whose onboarding chain changed, grouped by the first cause that applies. */
  readonly onboarding: readonly {
    readonly cause: OnboardingCause;
    readonly seats: readonly EntityRef[];
  }[];
  /** Renamed units: their schedules get a new identity, and onboarding pages are looked up under the new name. */
  readonly unitRenames: readonly {
    readonly ref: EntityRef;
    readonly before: string;
    readonly after: string;
    readonly schedules: readonly string[];
  }[];
  readonly reportsTo: readonly {
    readonly ref: EntityRef;
    readonly before: EntityRef | null;
    readonly after: EntityRef | null;
    /** The seat's managers are the same seats; only which of them is primary changed. */
    readonly orderOnly: boolean;
  }[];
  readonly leads: readonly {
    readonly ref: EntityRef;
    readonly before: EntityRef | null;
    readonly beforeInherited: boolean;
    readonly after: EntityRef | null;
    readonly afterInherited: boolean;
  }[];
  readonly channels: readonly {
    readonly ref: EntityRef;
    readonly before: string;
    readonly beforeInherited: boolean;
    readonly after: string;
    readonly afterInherited: boolean;
  }[];
  /**
   * Where unrouted work in a Jira project or Confluence space goes, per
   * declaration: a unit declaring it routes to the unit's effective lead, a
   * root seat declaring it to itself. `shared` marks a scope declared with
   * different owners, where the engine routes to the first declaration it
   * walks and reports the ambiguity in its own log.
   */
  readonly routing: readonly {
    readonly tool: "jira" | "confluence";
    readonly scope: string;
    readonly holder: EntityRef;
    readonly before: EntityRef | null;
    readonly after: EntityRef | null;
    readonly shared: boolean;
  }[];
  /** Tool credential server names an agent seat gains or loses (names only). */
  readonly credentialServers: readonly {
    readonly ref: EntityRef;
    readonly gained: readonly string[];
    readonly lost: readonly string[];
  }[];
  readonly strippedFields: readonly {
    readonly ref: EntityRef;
    readonly fields: readonly { readonly name: string; readonly credential: boolean }[];
  }[];
  readonly clearedReferences: readonly {
    readonly kind: ReferenceEffect["kind"];
    readonly holder: EntityRef;
    readonly from: string;
  }[];
  readonly datadogFallback: {
    readonly before: string | null;
    readonly after: string | null;
  } | null;
  readonly gitlabAccessLevels: readonly {
    readonly handle: string;
    readonly before: string | null;
    readonly after: string | null;
  }[];
  /** An added seat the engine gives the handle of a removed seat: it reattaches that seat's memory. */
  readonly memoryReuse: readonly {
    readonly ref: EntityRef;
    readonly handle: string;
    readonly previous: string;
  }[];
  readonly massRemoval: { readonly removed: number; readonly total: number } | null;
  readonly acknowledgements: readonly Acknowledgement[];
  /** The default audit summary line. */
  readonly summary: string;
}

// ---------------------------------------------------------------------------
// One side, indexed
// ---------------------------------------------------------------------------

interface Indexed {
  readonly draft: Draft;
  readonly index: PathIndex;
  readonly document: CompanyDocument;
  readonly known: boolean;
  readonly seatByKey: ReadonlyMap<NodeKey, DerivedSeat>;
  readonly unitByKey: ReadonlyMap<NodeKey, DerivedUnit>;
  readonly keyOfHandle: ReadonlyMap<string, NodeKey>;
  readonly refs: ReadonlyMap<NodeKey, EntityRef>;
  /** Structural parent of every seat and unit. */
  readonly parentOf: ReadonlyMap<NodeKey, NodeKey>;
}

function indexSide(side: Side): Indexed {
  const { document, index } = toDocument(side.draft);
  const empty = side.draft.roles.length === 0 && side.draft.units.length === 0;
  const known = side.derived !== null || empty;
  const { seatByKey, unitByKey, keyOfHandle } = placeDerivation(index, side.derived);
  const refs = new Map<NodeKey, EntityRef>([
    [COMPANY_KEY, { key: COMPANY_KEY, kind: "company", name: companyName(document) }],
  ]);
  const parentOf = new Map<NodeKey, NodeKey>();
  for (const { seat, parent } of allSeats(side.draft)) {
    refs.set(seat.key, { key: seat.key, kind: "seat", name: seat.data.name });
    parentOf.set(seat.key, parent);
  }
  for (const { unit, parent } of allUnits(side.draft)) {
    refs.set(unit.key, { key: unit.key, kind: "unit", name: unit.data.name });
    parentOf.set(unit.key, parent);
  }
  return {
    draft: side.draft,
    index,
    document,
    known,
    seatByKey,
    unitByKey,
    keyOfHandle,
    refs,
    parentOf,
  };
}

function companyName(doc: CompanyDocument): string {
  return typeof doc.name === "string" ? doc.name : "";
}

/** The key of a seat's effective home unit on a side, [COMPANY_KEY] at the root; `undefined` when not derived. */
function homeOf(side: Indexed, key: NodeKey): NodeKey | undefined {
  const seat = side.seatByKey.get(key);
  if (!seat || seat.unit_path === undefined) return undefined;
  if (seat.unit_path === "") return COMPANY_KEY;
  return side.index.byPath.get(seat.unit_path);
}

/** A unit and its ancestors, nearest first, as keys. */
function unitChain(side: Indexed, unit: NodeKey | undefined): NodeKey[] {
  const out: NodeKey[] = [];
  let at = unit;
  while (at !== undefined && at !== COMPANY_KEY) {
    out.push(at);
    at = side.parentOf.get(at);
  }
  return out;
}

const keysOf = (map: unknown): string[] => (isRecord(map) ? Object.keys(map) : []);

// ---------------------------------------------------------------------------
// The comparison
// ---------------------------------------------------------------------------

/** Every change between the base and the draft, and every consequence the engine will act on. */
export function deriveChanges(inputs: ChangeInputs): ChangeSet {
  const base = indexSide(inputs.base);
  const next = indexSide(inputs.next);
  const derivedKnown = base.known && next.known;
  const both = (key: NodeKey) => base.refs.has(key) && next.refs.has(key);
  const ref = (side: Indexed, key: NodeKey): EntityRef =>
    side.refs.get(key) ?? { key, kind: key === COMPANY_KEY ? "company" : "seat", name: "" };

  // Structure --------------------------------------------------------------
  const added: EntityRef[] = [];
  const removed: EntityRef[] = [];
  const renamed: ChangeSet["renamed"][number][] = [];
  const moved: ChangeSet["moved"][number][] = [];
  const edited: ChangeSet["edited"][number][] = [];

  for (const [key, entity] of next.refs) {
    if (key !== COMPANY_KEY && !base.refs.has(key)) added.push(entity);
  }
  for (const [key, entity] of base.refs) {
    if (key !== COMPANY_KEY && !next.refs.has(key)) removed.push(entity);
  }

  const baseSeats = new Map([...allSeats(base.draft)].map(({ seat }) => [seat.key, seat.data]));
  const nextSeats = new Map([...allSeats(next.draft)].map(({ seat }) => [seat.key, seat.data]));
  const baseUnits = new Map([...allUnits(base.draft)].map(({ unit }) => [unit.key, unit.data]));
  const nextUnits = new Map([...allUnits(next.draft)].map(({ unit }) => [unit.key, unit.data]));

  const entityChanges = (
    key: NodeKey,
    before: Record<string, unknown>,
    after: Record<string, unknown>,
  ) => {
    if (before.name !== after.name) {
      renamed.push({
        ref: ref(next, key),
        before: String(before.name ?? ""),
        after: String(after.name ?? ""),
      });
    }
    const fields = changedFields(before, after);
    if (fields.length > 0) edited.push({ ref: ref(next, key), fields });
  };
  for (const [key, after] of nextSeats) {
    const before = baseSeats.get(key);
    if (before) entityChanges(key, before, after);
  }
  for (const [key, after] of nextUnits) {
    const before = baseUnits.get(key);
    if (before) entityChanges(key, before, after);
  }

  for (const [key] of next.refs) {
    if (key === COMPANY_KEY || !both(key)) continue;
    const isSeat = nextSeats.has(key);
    // A seat is where the engine places it: its effective home, when both
    // sides are derived, so a root seat whose `unit:` reference was replaced
    // by a physical placement in that same unit has not moved.
    const homeFrom = isSeat && derivedKnown ? homeOf(base, key) : undefined;
    const homeTo = isSeat && derivedKnown ? homeOf(next, key) : undefined;
    const homes = homeFrom !== undefined && homeTo !== undefined;
    const from = homes ? homeFrom : base.parentOf.get(key)!;
    const to = homes ? homeTo : next.parentOf.get(key)!;
    if (from !== to) moved.push({ ref: ref(next, key), from: ref(base, from), to: ref(next, to) });
  }

  const charter = CHARTER_FIELDS.filter((f) => !jsonEqual(base.document[f], next.document[f]));
  const beforeName = companyName(base.document);
  const afterName = companyName(next.document);
  const companyRename =
    beforeName !== "" && beforeName !== afterName ? { before: beforeName, after: afterName } : null;

  // Derived consequences ---------------------------------------------------
  const handleChanges: ChangeSet["handleChanges"][number][] = [];
  const onboardingBy = new Map<OnboardingCause, EntityRef[]>();
  const reportsTo: ChangeSet["reportsTo"][number][] = [];
  const leads: ChangeSet["leads"][number][] = [];
  const channels: ChangeSet["channels"][number][] = [];
  const credentialServers: ChangeSet["credentialServers"][number][] = [];

  if (derivedKnown) {
    for (const [key, after] of next.seatByKey) {
      const before = base.seatByKey.get(key);
      if (!before) continue;
      const seatRef = ref(next, key);
      if (before.handle !== after.handle)
        handleChanges.push({ ref: seatRef, before: before.handle, after: after.handle });

      const agentBoth =
        kindOf(baseSeats.get(key)!) === "agent" && kindOf(nextSeats.get(key)!) === "agent";
      if (agentBoth) {
        const chainChanged =
          beforeName !== afterName ||
          before.name !== after.name ||
          !jsonEqual(before.onboarding_chain ?? [], after.onboarding_chain ?? []);
        if (chainChanged) {
          let cause: OnboardingCause;
          if (beforeName !== afterName) cause = "company_rename";
          else if (before.name !== after.name) cause = "seat_rename";
          else if (
            jsonEqual(unitChain(base, homeOf(base, key)), unitChain(next, homeOf(next, key)))
          )
            cause = "unit_rename";
          else cause = "move";
          const group = onboardingBy.get(cause);
          if (group) group.push(seatRef);
          else onboardingBy.set(cause, [seatRef]);
        }

        const servers = (side: Indexed, data: Record<string, unknown>) => {
          const home = homeOf(side, key);
          const unit =
            home === undefined || home === COMPANY_KEY
              ? undefined
              : (side === base ? baseUnits : nextUnits).get(home);
          return new Set([...keysOf(data.mcp_env), ...keysOf(unit?.mcp_env)]);
        };
        const had = servers(base, baseSeats.get(key)!);
        const has = servers(next, nextSeats.get(key)!);
        const gained = [...has].filter((s) => !had.has(s)).sort();
        const lost = [...had].filter((s) => !has.has(s)).sort();
        if (gained.length > 0 || lost.length > 0)
          credentialServers.push({ ref: seatRef, gained, lost });
      }

      const managerBefore = before.manager ? base.keyOfHandle.get(before.manager) : undefined;
      const managerAfter = after.manager ? next.keyOfHandle.get(after.manager) : undefined;
      if (managerBefore !== managerAfter) {
        const managerSet = (side: Indexed, seat: DerivedSeat) =>
          new Set((seat.managers ?? []).map((h) => side.keyOfHandle.get(h) ?? `handle:${h}`));
        const a = managerSet(base, before);
        const b = managerSet(next, after);
        const orderOnly = a.size === b.size && [...a].every((k) => b.has(k));
        reportsTo.push({
          ref: seatRef,
          before: managerBefore === undefined ? null : ref(base, managerBefore),
          after: managerAfter === undefined ? null : ref(next, managerAfter),
          orderOnly,
        });
      }
    }

    for (const [key, after] of next.unitByKey) {
      const before = base.unitByKey.get(key);
      if (!before) continue;
      const unitRef = ref(next, key);
      const leadBefore = before.lead ? base.keyOfHandle.get(before.lead) : undefined;
      const leadAfter = after.lead ? next.keyOfHandle.get(after.lead) : undefined;
      if (leadBefore !== leadAfter || before.lead_inherited !== after.lead_inherited) {
        leads.push({
          ref: unitRef,
          before: leadBefore === undefined ? null : ref(base, leadBefore),
          beforeInherited: before.lead_inherited,
          after: leadAfter === undefined ? null : ref(next, leadAfter),
          afterInherited: after.lead_inherited,
        });
      }
      if (
        before.channel !== after.channel ||
        before.channel_inherited !== after.channel_inherited
      ) {
        channels.push({
          ref: unitRef,
          before: before.channel,
          beforeInherited: before.channel_inherited,
          after: after.channel,
          afterInherited: after.channel_inherited,
        });
      }
    }
  }

  const onboardingOrder: OnboardingCause[] = [
    "company_rename",
    "seat_rename",
    "unit_rename",
    "move",
  ];
  const onboarding = onboardingOrder
    .filter((c) => onboardingBy.has(c))
    .map((cause) => ({ cause, seats: onboardingBy.get(cause)! }));

  const unitRenames = renamed
    .filter((r) => r.ref.kind === "unit")
    .map((r) => {
      const schedules = nextUnits.get(r.ref.key)?.schedules;
      return {
        ref: r.ref,
        before: r.before,
        after: r.after,
        schedules: Array.isArray(schedules) ? schedules.map((s) => s.name) : [],
      };
    });

  const routing = derivedKnown
    ? routingChanges(base, next, baseUnits, nextUnits, baseSeats, nextSeats, ref)
    : [];

  // What only the log can say -------------------------------------------------
  const stripped = new Map<NodeKey, Map<string, boolean>>();
  for (const op of inputs.ops) {
    if (op.type !== "changeKind" || !next.refs.has(op.target)) continue;
    const fields = stripped.get(op.target) ?? new Map<string, boolean>();
    for (const field of op.stripped)
      fields.set(fieldName(field.path), isCredentialField(field.path));
    stripped.set(op.target, fields);
  }
  const strippedFields = [...stripped]
    .filter(([, fields]) => fields.size > 0)
    .map(([key, fields]) => ({
      ref: ref(next, key),
      fields: [...fields].map(([name, credential]) => ({ name, credential })),
    }));

  const clearedReferences = inputs.reports.flatMap((report) =>
    report.cleared.map((effect) => ({
      kind: effect.kind,
      holder: next.refs.get(effect.holder) ?? ref(base, effect.holder),
      from: effect.from,
    })),
  );

  const routeBefore = getPath(base.document, DATADOG_ROUTE_TO);
  const routeAfter = getPath(next.document, DATADOG_ROUTE_TO);
  const datadogFallback = jsonEqual(routeBefore, routeAfter)
    ? null
    : {
        before: typeof routeBefore === "string" ? routeBefore : null,
        after: typeof routeAfter === "string" ? routeAfter : null,
      };

  const levelsBefore = getPath(base.document, GITLAB_ACCESS_LEVELS);
  const levelsAfter = getPath(next.document, GITLAB_ACCESS_LEVELS);
  const lb = isRecord(levelsBefore) ? levelsBefore : {};
  const la = isRecord(levelsAfter) ? levelsAfter : {};
  const gitlabAccessLevels = [...new Set([...Object.keys(lb), ...Object.keys(la)])]
    .sort()
    .filter((handle) => !jsonEqual(lb[handle], la[handle]))
    .map((handle) => ({
      handle,
      before: typeof lb[handle] === "string" ? (lb[handle] as string) : null,
      after: typeof la[handle] === "string" ? (la[handle] as string) : null,
    }));

  const memoryReuse: ChangeSet["memoryReuse"][number][] = [];
  if (derivedKnown) {
    const removedHandles = new Map<string, string>();
    for (const entity of removed) {
      const seat = base.seatByKey.get(entity.key);
      if (seat?.handle) removedHandles.set(seat.handle, entity.name);
    }
    for (const entity of added) {
      const seat = next.seatByKey.get(entity.key);
      if (!seat?.handle) continue;
      const previous = removedHandles.get(seat.handle) ?? inputs.recentlyRemoved?.get(seat.handle);
      if (previous !== undefined) memoryReuse.push({ ref: entity, handle: seat.handle, previous });
    }
  }

  const baseSeatCount = baseSeats.size;
  const removedSeats = removed.filter((r) => r.kind === "seat").length;
  const massRemoval =
    baseSeatCount > 0 && removedSeats * 2 > baseSeatCount
      ? { removed: removedSeats, total: baseSeatCount }
      : null;

  const kindChanged = [...nextSeats].some(([key, data]) => {
    const before = baseSeats.get(key);
    return before !== undefined && kindOf(before) !== kindOf(data);
  });
  const acknowledgements: Acknowledgement[] = [];
  if (companyRename) acknowledgements.push("company_rename");
  if (handleChanges.length > 0) acknowledgements.push("handle_change");
  if (kindChanged) acknowledgements.push("kind_change");
  if (
    credentialServers.length > 0 ||
    strippedFields.some((s) => s.fields.some((f) => f.credential))
  ) {
    acknowledgements.push("credential_servers");
  }
  if (massRemoval) acknowledgements.push("mass_removal");

  return {
    derivedKnown,
    added,
    removed,
    renamed,
    moved,
    edited,
    charter,
    companyRename,
    handleChanges,
    onboarding,
    unitRenames,
    reportsTo,
    leads,
    channels,
    routing,
    credentialServers,
    strippedFields,
    clearedReferences,
    datadogFallback,
    gitlabAccessLevels,
    memoryReuse,
    massRemoval,
    acknowledgements,
    summary: auditSummary({ added, removed, renamed, moved, edited, charter, ops: inputs.ops }),
  };
}

/**
 * The fields that differ between two versions of an entity's data, by
 * authored name: top-level keys, and one level into `integrations`, where a
 * seat's separate tools live. `name` is reported as a rename instead.
 */
function changedFields(before: Record<string, unknown>, after: Record<string, unknown>): string[] {
  const out: string[] = [];
  const keys = [...new Set([...Object.keys(before), ...Object.keys(after)])]
    .filter((k) => k !== "name")
    .sort();
  for (const key of keys) {
    if (jsonEqual(before[key], after[key])) continue;
    if (key === "integrations" && (isRecord(before[key]) || isRecord(after[key]))) {
      const b = isRecord(before[key]) ? before[key] : {};
      const a = isRecord(after[key]) ? after[key] : {};
      for (const tool of [...new Set([...Object.keys(b), ...Object.keys(a)])].sort()) {
        if (!jsonEqual(b[tool], a[tool])) out.push(`integrations.${tool}`);
      }
    } else {
      out.push(key);
    }
  }
  return out;
}

/** Scope keys are compared as the engine keys them: trimmed and upper case. */
const scopeKey = (value: unknown): string =>
  typeof value === "string" ? value.trim().toUpperCase() : "";

function routingChanges(
  base: Indexed,
  next: Indexed,
  baseUnits: ReadonlyMap<NodeKey, Record<string, unknown>>,
  nextUnits: ReadonlyMap<NodeKey, Record<string, unknown>>,
  baseSeats: ReadonlyMap<NodeKey, Record<string, unknown>>,
  nextSeats: ReadonlyMap<NodeKey, Record<string, unknown>>,
  ref: (side: Indexed, key: NodeKey) => EntityRef,
): ChangeSet["routing"][number][] {
  type Declaration = {
    tool: "jira" | "confluence";
    scope: string;
    holder: NodeKey;
    owner: NodeKey | undefined;
  };
  const TOOLS = [
    { tool: "jira", field: "project" },
    { tool: "confluence", field: "space" },
  ] as const;

  const declarations = (
    side: Indexed,
    units: ReadonlyMap<NodeKey, Record<string, unknown>>,
    seats: ReadonlyMap<NodeKey, Record<string, unknown>>,
  ): Declaration[] => {
    const out: Declaration[] = [];
    for (const [key, data] of units) {
      for (const { tool, field } of TOOLS) {
        const scope = scopeKey(getPath(data, ["integrations", tool, field]));
        if (scope === "") continue;
        const lead = side.unitByKey.get(key)?.lead;
        out.push({
          tool,
          scope,
          holder: key,
          owner: lead ? side.keyOfHandle.get(lead) : undefined,
        });
      }
    }
    for (const [key, data] of seats) {
      // Only a seat the engine keeps at the root owns a scope; a member of a
      // unit declares where it writes, and its unit's lead owns the scope.
      if (homeOf(side, key) !== COMPANY_KEY) continue;
      for (const { tool, field } of TOOLS) {
        const scope = scopeKey(getPath(data, ["integrations", tool, field]));
        if (scope !== "") out.push({ tool, scope, holder: key, owner: key });
      }
    }
    return out;
  };

  const before = declarations(base, baseUnits, baseSeats);
  const after = declarations(next, nextUnits, nextSeats);
  const id = (d: Declaration) => `${d.tool}\u0000${d.scope}\u0000${d.holder}`;
  const shared = (list: Declaration[]) => {
    const owners = new Map<string, Set<string>>();
    for (const d of list) {
      const k = `${d.tool}\u0000${d.scope}`;
      const set = owners.get(k) ?? new Set<string>();
      set.add(d.owner ?? "");
      owners.set(k, set);
    }
    return (d: Declaration) => (owners.get(`${d.tool}\u0000${d.scope}`)?.size ?? 0) > 1;
  };
  const sharedBefore = shared(before);
  const sharedAfter = shared(after);
  const beforeById = new Map(before.map((d) => [id(d), d]));
  const afterById = new Map(after.map((d) => [id(d), d]));

  const out: ChangeSet["routing"][number][] = [];
  for (const key of [...new Set([...beforeById.keys(), ...afterById.keys()])].sort()) {
    const b = beforeById.get(key);
    const a = afterById.get(key);
    if (b && a && b.owner === a.owner) continue;
    const any = (a ?? b)!;
    out.push({
      tool: any.tool,
      scope: any.scope,
      holder: a ? ref(next, a.holder) : ref(base, b!.holder),
      before: b?.owner === undefined ? null : ref(base, b.owner),
      after: a?.owner === undefined ? null : ref(next, a.owner),
      shared: (b ? sharedBefore(b) : false) || (a ? sharedAfter(a) : false),
    });
  }
  return out;
}

/** The default audit summary: what changed, in counts, as one sentence. */
function auditSummary(parts: {
  added: readonly EntityRef[];
  removed: readonly EntityRef[];
  renamed: readonly { ref: EntityRef }[];
  moved: readonly { ref: EntityRef }[];
  edited: readonly { ref: EntityRef }[];
  charter: readonly string[];
  ops: readonly Operation[];
}): string {
  const template = parts.ops.find((op) => op.type === "applyTemplate");
  const count = (refs: readonly { kind: string }[]) => {
    const seats = refs.filter((r) => r.kind === "seat").length;
    const units = refs.filter((r) => r.kind === "unit").length;
    return [seats > 0 ? plural(seats, "seat") : "", units > 0 ? plural(units, "unit") : ""]
      .filter(Boolean)
      .join(" and ");
  };
  const phrases: string[] = [];
  if (template && template.type === "applyTemplate")
    phrases.push(`created ${template.charter.name} in the organization builder`);
  const add = (verb: string, refs: readonly { kind: string }[]) => {
    const text = count(refs);
    if (text) phrases.push(`${verb} ${text}`);
  };
  if (!template) add("added", parts.added);
  add("removed", parts.removed);
  add(
    "renamed",
    parts.renamed.map((r) => r.ref),
  );
  add(
    "moved",
    parts.moved.map((r) => r.ref),
  );
  add(
    "edited",
    parts.edited.map((r) => r.ref),
  );
  if (!template && parts.charter.length > 0) phrases.push("edited the charter");
  if (phrases.length === 0)
    return "Saved from the organization builder with no changes to the chart";
  const sentence = phrases.join(", ");
  return sentence.charAt(0).toUpperCase() + sentence.slice(1);
}

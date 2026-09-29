/**
 * What a save changes, and what follows from it.
 *
 * DERIVED FROM THE TWO CHARTS, NEVER FROM OPERATION TYPES ALONE. A unit's
 * rename re-onboards the agent seats inside it, a move changes the tool
 * credentials a seat receives from its unit, and removing most of the company
 * is worth a second look. None of that is visible in the operation that caused
 * it, so this module compares the BASE and the DRAFT node by node, and reads
 * the operation log only for what no chart can say afterwards: the fields a
 * kind change removed, and the references an operation cleared.
 *
 * Nodes are matched by key (see `keys.ts`), so "the same seat" here means the
 * seat the chart holds under that address — the identity its memory, its
 * mailbox and whoever is bound to it are keyed on — and never "a seat with the
 * same name".
 *
 * WHAT THE ENGINE DERIVES IS NOT GUESSED AT. Who a seat reports to, the lead
 * and channel a unit inherits, and where unrouted work goes are the engine's
 * derivation of the chart it holds, and there is no derivation of a draft: the
 * chart has no dry run. So this module says nothing about them, and the review
 * says that it does not, rather than working the answers out again here and
 * drifting from the engine the day one of its rules moves. What it does say is
 * read off the tree itself: which unit a seat sits in, and the names of the
 * units above it.
 */

import type { CompanyDocument } from "~/protocol/index.ts";
import { plural } from "~/lib/format.ts";
import { COMPANY_KEY, type NodeKey } from "./keys.ts";
import { allSeats, allUnits, type Draft, type SeatData, type UnitData } from "./draft.ts";
import { CHARTER_FIELDS, DATADOG_ROUTE_TO, GITLAB_ACCESS_LEVELS } from "./document.ts";
import { getPath, isRecord, jsonEqual } from "./json.ts";
import {
  fieldName,
  isCredentialField,
  kindOf,
  type ApplyReport,
  type Operation,
  type ReferenceEffect,
} from "./operations.ts";

export interface ChangeInputs {
  /** The chart the draft was made on. */
  readonly base: Draft;
  /** The draft as it stands. */
  readonly next: Draft;
  /** The draft's operation log, and what applying each one did (aligned). */
  readonly ops: readonly Operation[];
  readonly reports: readonly ApplyReport[];
}

/** A seat or a unit, by key, with the name it has on the side it was read from. */
export interface EntityRef {
  readonly key: NodeKey;
  readonly kind: "seat" | "unit" | "company";
  readonly name: string;
}

/**
 * Why an agent seat onboards again. The engine's onboarding marker is a hash
 * of the company's name and the IDENTITIES of the units above the seat and of
 * the seat itself (`learning.ChainHash`) — never their names or addresses —
 * so only these two change it: a company rename, which re-derives every
 * seat's id, and a move, which puts a seat under other units.
 */
export type OnboardingCause = "company_rename" | "move";

export type Acknowledgement =
  "company_rename" | "kind_change" | "credential_servers" | "mass_removal";

export interface ChangeSet {
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
  /**
   * Seats and units given a new ADDRESS — a handle, a key — that the chart
   * holds under the old one: the chart's own rename, which keeps the object
   * and leaves the old address resolving to it until something else takes it.
   */
  readonly addressChanges: readonly {
    readonly ref: EntityRef;
    readonly before: string;
    readonly after: string;
  }[];
  /** Agent seats whose onboarding chain changes, grouped by the first cause that applies. */
  readonly onboarding: readonly {
    readonly cause: OnboardingCause;
    readonly seats: readonly EntityRef[];
  }[];
  /** Renamed units: onboarding pages are looked up under the new name. */
  readonly unitRenames: readonly {
    readonly ref: EntityRef;
    readonly before: string;
    readonly after: string;
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
  readonly company: CompanyDocument;
  readonly refs: ReadonlyMap<NodeKey, EntityRef>;
  /** Structural parent of every seat and unit. */
  readonly parentOf: ReadonlyMap<NodeKey, NodeKey>;
  readonly seats: ReadonlyMap<NodeKey, SeatData>;
  readonly units: ReadonlyMap<NodeKey, UnitData>;
}

function indexSide(draft: Draft): Indexed {
  const refs = new Map<NodeKey, EntityRef>([
    [COMPANY_KEY, { key: COMPANY_KEY, kind: "company", name: companyName(draft.company) }],
  ]);
  const parentOf = new Map<NodeKey, NodeKey>();
  const seats = new Map<NodeKey, SeatData>();
  const units = new Map<NodeKey, UnitData>();
  for (const { seat, parent } of allSeats(draft)) {
    refs.set(seat.key, { key: seat.key, kind: "seat", name: seat.data.name });
    parentOf.set(seat.key, parent);
    seats.set(seat.key, seat.data);
  }
  for (const { unit, parent } of allUnits(draft)) {
    refs.set(unit.key, { key: unit.key, kind: "unit", name: unit.data.name });
    parentOf.set(unit.key, parent);
    units.set(unit.key, unit.data);
  }
  return { draft, company: draft.company, refs, parentOf, seats, units };
}

function companyName(doc: CompanyDocument): string {
  return typeof doc.name === "string" ? doc.name : "";
}

/** The units above a node, outermost first, as keys. */
function chainOf(side: Indexed, key: NodeKey): NodeKey[] {
  const out: NodeKey[] = [];
  let at = side.parentOf.get(key);
  while (at !== undefined && at !== COMPANY_KEY) {
    out.unshift(at);
    at = side.parentOf.get(at);
  }
  return out;
}

const keysOf = (map: unknown): string[] => (isRecord(map) ? Object.keys(map) : []);
const mcpEnvOf = (data: SeatData | UnitData | undefined): unknown =>
  (data?.runtime as { mcp_env?: unknown } | undefined)?.mcp_env;

// ---------------------------------------------------------------------------
// The comparison
// ---------------------------------------------------------------------------

/** Every change between the base and the draft, and what follows from it. */
export function deriveChanges(inputs: ChangeInputs): ChangeSet {
  const base = indexSide(inputs.base);
  const next = indexSide(inputs.next);
  const both = (key: NodeKey) => base.refs.has(key) && next.refs.has(key);
  const ref = (side: Indexed, key: NodeKey): EntityRef =>
    side.refs.get(key) ?? { key, kind: key === COMPANY_KEY ? "company" : "seat", name: "" };

  // Structure --------------------------------------------------------------
  const added: EntityRef[] = [];
  const removed: EntityRef[] = [];
  const renamed: ChangeSet["renamed"][number][] = [];
  const moved: ChangeSet["moved"][number][] = [];
  const edited: ChangeSet["edited"][number][] = [];
  const addressChanges: ChangeSet["addressChanges"][number][] = [];

  for (const [key, entity] of next.refs) {
    if (key !== COMPANY_KEY && !base.refs.has(key)) added.push(entity);
  }
  for (const [key, entity] of base.refs) {
    if (key !== COMPANY_KEY && !next.refs.has(key)) removed.push(entity);
  }

  const entityChanges = (
    key: NodeKey,
    address: "handle" | "key",
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
    if (before[address] !== after[address]) {
      addressChanges.push({
        ref: ref(next, key),
        before: String(before[address] ?? ""),
        after: String(after[address] ?? ""),
      });
    }
    const fields = changedFields(before, after, address);
    if (fields.length > 0) edited.push({ ref: ref(next, key), fields });
  };
  for (const [key, after] of next.seats) {
    const before = base.seats.get(key);
    if (before) entityChanges(key, "handle", before, after);
  }
  for (const [key, after] of next.units) {
    const before = base.units.get(key);
    if (before) entityChanges(key, "key", before, after);
  }

  for (const [key] of next.refs) {
    if (key === COMPANY_KEY || !both(key)) continue;
    const from = base.parentOf.get(key)!;
    const to = next.parentOf.get(key)!;
    if (from !== to) moved.push({ ref: ref(next, key), from: ref(base, from), to: ref(next, to) });
  }

  const charter = CHARTER_FIELDS.filter((f) => !jsonEqual(base.company[f], next.company[f]));
  const beforeName = companyName(base.company);
  const afterName = companyName(next.company);
  const companyRename =
    beforeName !== "" && beforeName !== afterName ? { before: beforeName, after: afterName } : null;

  // What follows from the tree ----------------------------------------------
  //
  // AN AGENT SEAT ONBOARDS AGAIN when its onboarding marker stops matching:
  // the company is renamed, or the units above it (read here off the tree,
  // outermost first, which is the chain the engine walks) are other units.
  // The chain is compared by KEY, the draft's own stable name for a node,
  // because the engine hashes identities: renaming a seat, a unit or either's
  // address re-onboards nobody.
  const onboardingBy = new Map<OnboardingCause, EntityRef[]>();
  const credentialServers: ChangeSet["credentialServers"][number][] = [];
  for (const [key, after] of next.seats) {
    const before = base.seats.get(key);
    if (!before || kindOf(before) !== "agent" || kindOf(after) !== "agent") continue;
    const seatRef = ref(next, key);
    const chainBefore = chainOf(base, key);
    const chainAfter = chainOf(next, key);
    let cause: OnboardingCause | null = null;
    if (beforeName !== afterName) cause = "company_rename";
    else if (!jsonEqual(chainBefore, chainAfter)) cause = "move";
    if (cause) onboardingBy.set(cause, [...(onboardingBy.get(cause) ?? []), seatRef]);

    // A SEAT'S TOOL CREDENTIALS are its own `mcp_env` and its unit's: the
    // unit's go to its DIRECT agent members.
    const servers = (side: Indexed, data: SeatData) => {
      const home = side.parentOf.get(key);
      const unit = home === undefined || home === COMPANY_KEY ? undefined : side.units.get(home);
      return new Set([...keysOf(mcpEnvOf(data)), ...keysOf(mcpEnvOf(unit))]);
    };
    const had = servers(base, before);
    const has = servers(next, after);
    const gained = [...has].filter((s) => !had.has(s)).sort();
    const lost = [...had].filter((s) => !has.has(s)).sort();
    if (gained.length > 0 || lost.length > 0)
      credentialServers.push({ ref: seatRef, gained, lost });
  }
  const onboardingOrder: OnboardingCause[] = ["company_rename", "move"];
  const onboarding = onboardingOrder
    .filter((c) => onboardingBy.has(c))
    .map((cause) => ({ cause, seats: onboardingBy.get(cause)! }));

  const unitRenames = renamed
    .filter((r) => r.ref.kind === "unit")
    .map((r) => ({ ref: r.ref, before: r.before, after: r.after }));

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

  const routeBefore = getPath(base.company, DATADOG_ROUTE_TO);
  const routeAfter = getPath(next.company, DATADOG_ROUTE_TO);
  const datadogFallback = jsonEqual(routeBefore, routeAfter)
    ? null
    : {
        before: typeof routeBefore === "string" ? routeBefore : null,
        after: typeof routeAfter === "string" ? routeAfter : null,
      };

  const levelsBefore = getPath(base.company, GITLAB_ACCESS_LEVELS);
  const levelsAfter = getPath(next.company, GITLAB_ACCESS_LEVELS);
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

  const baseSeatCount = base.seats.size;
  const removedSeats = removed.filter((r) => r.kind === "seat").length;
  const massRemoval =
    baseSeatCount > 0 && removedSeats * 2 > baseSeatCount
      ? { removed: removedSeats, total: baseSeatCount }
      : null;

  const kindChanged = [...next.seats].some(([key, data]) => {
    const before = base.seats.get(key);
    return before !== undefined && kindOf(before) !== kindOf(data);
  });
  const acknowledgements: Acknowledgement[] = [];
  if (companyRename) acknowledgements.push("company_rename");
  if (kindChanged) acknowledgements.push("kind_change");
  if (
    credentialServers.length > 0 ||
    strippedFields.some((s) => s.fields.some((f) => f.credential))
  ) {
    acknowledgements.push("credential_servers");
  }
  if (massRemoval) acknowledgements.push("mass_removal");

  return {
    added,
    removed,
    renamed,
    moved,
    edited,
    charter,
    companyRename,
    addressChanges,
    onboarding,
    unitRenames,
    credentialServers,
    strippedFields,
    clearedReferences,
    datadogFallback,
    gitlabAccessLevels,
    massRemoval,
    acknowledgements,
    summary: auditSummary({ added, removed, renamed, moved, edited, charter, ops: inputs.ops }),
  };
}

/**
 * The fields that differ between two versions of a node's data, by the name a
 * person reads: top-level keys, and one level into the runtime half, where a
 * seat's model chain, its tools and its own vendor identities live. The name
 * and the address are reported on their own lines, and the addresses the
 * chart keeps beside an object are the chart's.
 */
function changedFields(
  before: Record<string, unknown>,
  after: Record<string, unknown>,
  address: "handle" | "key",
): string[] {
  const skip = new Set(["name", address, "former_handles", "former_keys"]);
  const out: string[] = [];
  const keys = [...new Set([...Object.keys(before), ...Object.keys(after)])]
    .filter((k) => !skip.has(k))
    .sort();
  for (const key of keys) {
    if (jsonEqual(before[key], after[key])) continue;
    if (key === "runtime" && (isRecord(before[key]) || isRecord(after[key]))) {
      const b = isRecord(before[key]) ? before[key] : {};
      const a = isRecord(after[key]) ? after[key] : {};
      for (const field of [...new Set([...Object.keys(b), ...Object.keys(a)])].sort()) {
        if (!jsonEqual(b[field], a[field])) out.push(field);
      }
    } else {
      out.push(key);
    }
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

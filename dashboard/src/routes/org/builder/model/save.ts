/**
 * Saving: what a draft becomes on the wire, in what order, and what each
 * answer means.
 *
 * A SAVE IS A SEQUENCE, BECAUSE THE COMPANY IS TWO LOGS AND THE CHART IS
 * ARBITRATED PER OBJECT. The settings are one revision, written whole or not
 * at all. The chart takes STRUCTURE as batches — one record each, serialised
 * against every other structural write so no two reorganisations can close a
 * cycle between them — and CONTENT as one full-post-state write per object on
 * that object's own subject. So a save is a plan of steps ([planSave]), each
 * its own write with its own outcome, sent in an order every step can land
 * in:
 *
 * - CREATE mode writes the settings FIRST (`PUT /config`, `If-None-Match: *`),
 *   because that write is what makes the company exist and is refused whole if
 *   somebody else made it first — then the chart it holds.
 * - STRUCTURE before content, because a content write never creates its
 *   object: a seat's goal is written onto a seat a batch already placed, and
 *   its `unit` must name the unit the batch put it in.
 * - Placements before REMOVALS, in batches of their own: the chart refuses a
 *   batch that does both (a removal installs a gate, and whether a record
 *   installs one must be answerable from its envelope), and a unit is removed
 *   only once it is empty — so what the draft moved out of it goes first.
 * - EDIT mode writes the settings LAST: the Datadog fallback and the GitLab
 *   access levels name seats by handle, and those handles are the chart's
 *   once the structure has landed.
 *
 * EVERY STEP IS NAMED BEFORE IT IS SENT. A chart write whose outcome this node
 * cannot establish answers `unknown` with its operation id, and the ONLY safe
 * retry is under that same id — the chart's ledger recognises a second arrival
 * and answers the first one's outcome rather than writing twice. So each step
 * carries an id derived from the save's own write id ([planSave]'s `writeId`)
 * and sends it as `Idempotency-Key`, and a retry resends the very step. The
 * settings write is signed the settings surface's way, in its audit summary
 * (`writes.ts`).
 *
 * A STEP STOPS THE SEQUENCE when it is refused or its outcome is unknown: the
 * steps after it may depend on it (a content write on a seat whose create did
 * not land), and a person decides what happens next. What already landed
 * stays landed — each step is its own record — so the builder reads the
 * company back and carries on from what is actually there.
 */

import type { ChartOperation, CompanyDocument, ConfigWarning } from "~/protocol/index.ts";
import { cloneJson, isRecord, jsonEqual } from "./json.ts";
import { COMPANY_KEY, type NodeKey } from "./keys.ts";
import {
  allSeats,
  allUnits,
  locate,
  type Draft,
  type DraftSeat,
  type DraftUnit,
  type SeatData,
  type UnitData,
} from "./draft.ts";
import { kindOf } from "./operations.ts";
import {
  settingsChanged,
  settingsSaveRequest,
  type BuilderMode,
  type EngineRequest,
  type HttpAnswer,
} from "./transport.ts";

/**
 * The most operations one batch carries (`chart.MaxBatchOperations`): one
 * batch is one record, and the applier runs it in one transaction holding the
 * store's only writer. A larger change is several batches, each arbitrated on
 * its own.
 */
export const MAX_BATCH_OPERATIONS = 500;

/** What one step of a save writes. */
export type StepKind = "settings" | "structure" | "removal" | "unit" | "seat";

/** One write of a save. */
export interface SaveStep {
  /** The operation id: sent as `Idempotency-Key` on a chart write, and resent by a retry. */
  readonly id: string;
  readonly kind: StepKind;
  readonly request: EngineRequest;
  /**
   * The nodes the step is about: a batch's, one per operation in order (so a
   * refusal naming operation N names its node); a content write's, its one
   * node; the settings', the company.
   */
  readonly nodes: readonly NodeKey[];
  /**
   * The nodes this step CREATES, by the key the draft minted and the address
   * the chart will hold them under: what a save that landed this step and
   * failed later hands the rebase (`history.rebase`'s aliases).
   */
  readonly creates: readonly Creation[];
}

/** A node a save creates. */
export interface Creation {
  readonly key: NodeKey;
  readonly kind: "seat" | "unit";
  readonly address: string;
}

/** Everything a save sends, in order. */
export interface SavePlan {
  readonly steps: readonly SaveStep[];
}

/** What a plan is made from. */
export interface PlanInputs {
  readonly mode: BuilderMode;
  /** The settings revision the draft was built on, and its document; `null` in create mode. */
  readonly baseRevision: string | null;
  readonly baseSettings: CompanyDocument | null;
  /** The chart the draft was built on, as a draft. */
  readonly baseDraft: Draft;
  readonly draft: Draft;
  /**
   * Whether the chart served this reader each object's runtime half. When it
   * did not, the draft never held one, and a content write STATES none — which
   * keeps whatever the object has, where stating an empty one would clear it.
   */
  readonly runtimeVisible: boolean;
  /** The save's write id, minted in the event handler; every step's id derives from it. */
  readonly writeId: string;
  /** The settings write's audit summary, already signed with the write id. */
  readonly summary: string;
}

/** A node's address and where it sits, as the chart will hold it. */
interface Placed {
  readonly kind: "seat" | "unit";
  readonly address: string;
  /** The unit key it sits under; `""` at the org root. */
  readonly parent: string;
}

/** Every node of a draft by key: its address and its parent's address. */
function placedNodes(draft: Draft): Map<NodeKey, Placed> {
  const out = new Map<NodeKey, Placed>();
  const unitAddress = new Map<NodeKey, string>();
  for (const { unit } of allUnits(draft)) unitAddress.set(unit.key, unit.data.key);
  const parentOf = (key: NodeKey) => (key === COMPANY_KEY ? "" : (unitAddress.get(key) ?? ""));
  for (const { unit, parent } of allUnits(draft)) {
    out.set(unit.key, { kind: "unit", address: unit.data.key, parent: parentOf(parent) });
  }
  for (const { seat, parent } of allSeats(draft)) {
    out.set(seat.key, { kind: "seat", address: seat.data.handle, parent: parentOf(parent) });
  }
  return out;
}

const listOf = (value: unknown): string[] => (Array.isArray(value) ? value.map(String) : []);
const textOf = (value: unknown): string => (typeof value === "string" ? value : "");

/**
 * The structural operations that turn the base's chart into the draft's, in
 * an order the chart replays without refusing on its own account.
 *
 * RENAMES FIRST, so every later operation names an object by the address it
 * ends with. CREATES AND MOVES OF UNITS NEXT, parents before children, a move
 * that would close a cycle in the chart as it stands at that point of the
 * batch taken the long way round through the org root. SEATS AFTER THE UNITS
 * they go into. THE RELATIONS LAST — a kind, a lead, a `manages:` list —
 * where every object they name is already where it will be.
 */
function placementOperations(
  base: Draft,
  draft: Draft,
): { ops: ChartOperation[]; nodes: NodeKey[] } {
  const before = placedNodes(base);
  const after = placedNodes(draft);
  const ops: ChartOperation[] = [];
  const nodes: NodeKey[] = [];
  const emit = (op: ChartOperation, node: NodeKey) => {
    ops.push(op);
    nodes.push(node);
  };
  const ref = (placed: Placed) => ({ kind: placed.kind, id: placed.address });

  // Renames: an object the base holds under another address.
  for (const [key, now] of after) {
    const was = before.get(key);
    if (was && was.address !== now.address) {
      emit({ kind: "rename", object: ref(was), to: now.address }, key);
    }
  }

  // Units: the chart's parent of each, as it stands through the batch, so a
  // move is checked against the tree the replay will be looking at.
  const working = new Map<string, string>();
  for (const [key, now] of before) {
    if (now.kind === "unit") working.set(after.get(key)?.address ?? now.address, now.parent);
  }
  const renamedParent = (parent: string): string => {
    for (const [key, was] of before) {
      if (was.kind === "unit" && was.address === parent) return after.get(key)?.address ?? parent;
    }
    return parent;
  };
  for (const [address, parent] of [...working]) working.set(address, renamedParent(parent));
  const within = (address: string, ancestor: string): boolean => {
    for (let at: string | undefined = address, hops = 0; at && hops <= working.size; hops++) {
      if (at === ancestor) return true;
      at = working.get(at);
    }
    return false;
  };

  for (const { unit } of allUnits(draft)) {
    const now = after.get(unit.key)!;
    if (before.has(unit.key)) continue;
    emit(
      {
        kind: "create_unit",
        object: ref(now),
        ...(now.parent !== "" ? { parent: now.parent } : {}),
        ...(unit.data.lead ? { lead: unit.data.lead } : {}),
      },
      unit.key,
    );
    working.set(now.address, now.parent);
  }

  const pending = new Map<NodeKey, Placed>();
  for (const { unit } of allUnits(draft)) {
    const now = after.get(unit.key)!;
    const was = before.get(unit.key);
    if (was && renamedParent(was.parent) !== now.parent) pending.set(unit.key, now);
  }
  // A MOVE THAT WOULD CLOSE A CYCLE WAITS for the move that opens it, and
  // where every remaining move waits on another, the one blocking the first
  // goes through the org root: nothing placed at the root is inside anything.
  // Every detour takes a pending unit out of the chain that blocked, so the
  // loop ends; the bound is a guard against a defect, past which the moves
  // are sent as they are and the chart refuses the one it must.
  for (let guard = 4 * pending.size + 4; pending.size > 0 && guard > 0; guard--) {
    const ready = [...pending].find(
      ([, now]) => now.parent === "" || !within(now.parent, now.address),
    );
    if (ready) {
      const [key, now] = ready;
      emit(
        { kind: "move", object: ref(now), ...(now.parent !== "" ? { parent: now.parent } : {}) },
        key,
      );
      working.set(now.address, now.parent);
      pending.delete(key);
      continue;
    }
    const [, blocked] = [...pending][0]!;
    let at = blocked.parent;
    let detour: [NodeKey, Placed] | undefined;
    while (at && at !== blocked.address && !detour) {
      detour = [...pending].find(([, p]) => p.address === at);
      at = working.get(at) ?? "";
    }
    const [key, now] = detour ?? [...pending][0]!;
    emit({ kind: "move", object: ref(now) }, key);
    working.set(now.address, "");
  }
  for (const [key, now] of pending) {
    emit(
      { kind: "move", object: ref(now), ...(now.parent !== "" ? { parent: now.parent } : {}) },
      key,
    );
  }

  for (const { seat } of allSeats(draft)) {
    const now = after.get(seat.key)!;
    const was = before.get(seat.key);
    if (!was) {
      emit(
        {
          kind: "create_seat",
          object: ref(now),
          ...(now.parent !== "" ? { parent: now.parent } : {}),
          seat_kind: kindOf(seat.data),
        },
        seat.key,
      );
    } else if (renamedParent(was.parent) !== now.parent) {
      emit(
        { kind: "move", object: ref(now), ...(now.parent !== "" ? { parent: now.parent } : {}) },
        seat.key,
      );
    }
  }

  for (const { seat } of allSeats(draft)) {
    const found = locate(base, seat.key);
    if (found?.kind === "seat" && kindOf(found.node.data) !== kindOf(seat.data)) {
      emit(
        { kind: "set_kind", object: ref(after.get(seat.key)!), seat_kind: kindOf(seat.data) },
        seat.key,
      );
    }
  }
  for (const { unit } of allUnits(draft)) {
    const found = locate(base, unit.key);
    if (found?.kind !== "unit") continue;
    const lead = textOf(unit.data.lead);
    if (lead !== textOf(found.node.data.lead)) {
      emit(
        { kind: "set_lead", object: ref(after.get(unit.key)!), ...(lead ? { lead } : {}) },
        unit.key,
      );
    }
  }
  for (const { seat } of allSeats(draft)) {
    const found = locate(base, seat.key);
    const was = found?.kind === "seat" ? listOf(found.node.data.manages) : [];
    const now = listOf(seat.data.manages);
    if (!jsonEqual(was, now)) {
      emit(
        {
          kind: "set_manages",
          object: ref(after.get(seat.key)!),
          ...(now.length > 0 ? { manages: now } : {}),
        },
        seat.key,
      );
    }
  }
  return { ops, nodes };
}

/**
 * The removals, deepest first: a unit's seats and its child units before the
 * unit itself, because the chart removes only an empty unit.
 */
function removalOperations(base: Draft, draft: Draft): { ops: ChartOperation[]; nodes: NodeKey[] } {
  const kept = new Set<NodeKey>([
    ...[...allSeats(draft)].map(({ seat }) => seat.key),
    ...[...allUnits(draft)].map(({ unit }) => unit.key),
  ]);
  const ops: ChartOperation[] = [];
  const nodes: NodeKey[] = [];
  const seat = (node: DraftSeat) => {
    if (kept.has(node.key)) return;
    ops.push({ kind: "remove", object: { kind: "seat", id: node.data.handle } });
    nodes.push(node.key);
  };
  const unit = (node: DraftUnit) => {
    node.roles.forEach(seat);
    node.children.forEach(unit);
    if (kept.has(node.key)) return;
    ops.push({ kind: "remove", object: { kind: "unit", id: node.data.key } });
    nodes.push(node.key);
  };
  base.roles.forEach(seat);
  base.units.forEach(unit);
  return { ops, nodes };
}

/** The content fields of a seat, as a content write states them: full post-state. */
function seatContent(data: SeatData, unit: string): Record<string, unknown> {
  return {
    unit,
    name: textOf(data.name),
    email: textOf(data.email),
    backstory: textOf(data.backstory),
    goal: textOf(data.goal),
    responsibilities: listOf(data.responsibilities),
    behavioral_guidelines: listOf(data.behavioral_guidelines),
    project: textOf(data.project),
    space: textOf(data.space),
  };
}

/** The content fields of a unit, as a content write states them. */
function unitContent(data: UnitData): Record<string, unknown> {
  return {
    name: textOf(data.name),
    type: textOf(data.type),
    purpose: textOf(data.purpose),
    goals: listOf(data.goals),
    channel: textOf(data.channel),
    project: textOf(data.project),
    space: textOf(data.space),
    knowledge_refs: listOf(data.knowledge_refs),
  };
}

/**
 * The runtime half a content write states, as the body's own two fields.
 *
 * STATED ONLY WHEN IT CHANGED. A write that states the runtime half asks for
 * the company's grant (`config:write`), because the half is a seat's model
 * chain, its credentials and what runs on every engine host; a write that
 * leaves it out KEEPS it. So a lead correcting their seat's goal sends none,
 * and is asked for nothing they do not hold — and a draft whose reader was
 * never shown the half never states one.
 */
function runtimeFields(
  base: unknown,
  now: unknown,
  visible: boolean,
  created: boolean,
): Record<string, unknown> {
  if (!visible) return {};
  const had = isRecord(base) && Object.keys(base).length > 0;
  const has = isRecord(now) && Object.keys(now).length > 0;
  if (created) return has ? { runtime: cloneJson(now) } : {};
  if (jsonEqual(had ? base : undefined, has ? now : undefined)) return {};
  return has ? { runtime: cloneJson(now) } : had ? { clear_runtime: true } : {};
}

/** A content write's body, or `null` when the node's content is unchanged. */
function contentBody(
  base: Draft,
  found: { kind: "seat"; node: DraftSeat } | { kind: "unit"; node: DraftUnit },
  unit: string,
  visible: boolean,
): Record<string, unknown> | null {
  const prior = locate(base, found.node.key);
  const created = prior === undefined;
  if (found.kind === "seat") {
    const now = seatContent(found.node.data, unit);
    const runtime = runtimeFields(
      prior?.kind === "seat" ? prior.node.data.runtime : undefined,
      found.node.data.runtime,
      visible,
      created,
    );
    // A CREATED SEAT IS ALWAYS WRITTEN: the chart does not place a seat no
    // content record has filled, so a hire whose content write never lands
    // never gets a mailbox.
    if (!created && prior?.kind === "seat") {
      const was = seatContent(prior.node.data, unit);
      if (jsonEqual(was, now) && Object.keys(runtime).length === 0) return null;
    }
    return { ...now, ...runtime };
  }
  const now = unitContent(found.node.data);
  const runtime = runtimeFields(
    prior?.kind === "unit" ? prior.node.data.runtime : undefined,
    found.node.data.runtime,
    visible,
    created,
  );
  if (!created && prior?.kind === "unit") {
    if (jsonEqual(unitContent(prior.node.data), now) && Object.keys(runtime).length === 0) {
      return null;
    }
  }
  return { ...now, ...runtime };
}

/** A path segment, encoded. */
const segment = (value: string) => encodeURIComponent(value);

/** The chart write request: a batch, or one object's content. */
function chartRequest(
  method: "POST" | "PATCH",
  path: string,
  id: string,
  body: Record<string, unknown>,
): EngineRequest {
  return {
    method,
    path,
    query: {},
    contentType: "application/json",
    headers: { "Idempotency-Key": id },
    body,
  };
}

/** Splits a list into runs of at most `size`. */
function chunks<T>(list: readonly T[], size: number): T[][] {
  const out: T[][] = [];
  for (let at = 0; at < list.length; at += size) out.push(list.slice(at, at + size));
  return out;
}

/**
 * Everything a save sends, in order (see the module doc). An empty plan is a
 * draft with nothing to write.
 */
export function planSave(inputs: PlanInputs): SavePlan {
  // A CREATE DRAFT THAT HOLDS NOTHING WRITES NOTHING. Its settings write is
  // what makes the company exist, so it is planned whatever else changed — but
  // a company of no charter, no seats and no units is not one anybody asked
  // for, and a save of it would create a nameless company the next template
  // is then refused on.
  const { company, roles, units } = inputs.draft;
  if (
    inputs.mode === "create" &&
    Object.keys(company).length === 0 &&
    roles.length === 0 &&
    units.length === 0
  ) {
    return { steps: [] };
  }
  const steps: SaveStep[] = [];
  const id = () => `${inputs.writeId}-${steps.length + 1}`;
  const settings = {
    mode: inputs.mode,
    baseRevision: inputs.baseRevision,
    base: inputs.baseSettings,
    draft: inputs.draft.company,
  };
  const settingsStep = (): SaveStep => ({
    id: id(),
    kind: "settings",
    request: settingsSaveRequest(settings, inputs.summary),
    nodes: [COMPANY_KEY],
    creates: [],
  });
  if (inputs.mode === "create") steps.push(settingsStep());

  const after = placedNodes(inputs.draft);
  const placements = placementOperations(inputs.baseDraft, inputs.draft);
  const batches = chunks(
    placements.ops.map((op, i) => ({ op, node: placements.nodes[i]! })),
    MAX_BATCH_OPERATIONS,
  );
  for (const batch of batches) {
    const creates: Creation[] = batch
      .filter(({ op }) => op.kind === "create_seat" || op.kind === "create_unit")
      .map(({ op, node }) => ({ key: node, kind: op.object.kind, address: op.object.id }));
    const step = id();
    steps.push({
      id: step,
      kind: "structure",
      request: chartRequest("POST", "/chart/batch", step, {
        operations: batch.map(({ op }) => op),
      }),
      nodes: batch.map(({ node }) => node),
      creates,
    });
  }
  const removals = removalOperations(inputs.baseDraft, inputs.draft);
  for (const batch of chunks(
    removals.ops.map((op, i) => ({ op, node: removals.nodes[i]! })),
    MAX_BATCH_OPERATIONS,
  )) {
    const step = id();
    steps.push({
      id: step,
      kind: "removal",
      request: chartRequest("POST", "/chart/batch", step, {
        operations: batch.map(({ op }) => op),
      }),
      nodes: batch.map(({ node }) => node),
      creates: [],
    });
  }

  for (const { unit } of allUnits(inputs.draft)) {
    const body = contentBody(
      inputs.baseDraft,
      { kind: "unit", node: unit },
      "",
      inputs.runtimeVisible,
    );
    if (!body) continue;
    const step = id();
    steps.push({
      id: step,
      kind: "unit",
      request: chartRequest("PATCH", `/chart/units/${segment(unit.data.key)}`, step, body),
      nodes: [unit.key],
      creates: [],
    });
  }
  for (const { seat } of allSeats(inputs.draft)) {
    const home = after.get(seat.key)!.parent;
    const body = contentBody(
      inputs.baseDraft,
      { kind: "seat", node: seat },
      home,
      inputs.runtimeVisible,
    );
    if (!body) continue;
    const step = id();
    steps.push({
      id: step,
      kind: "seat",
      request: chartRequest("PATCH", `/chart/seats/${segment(seat.data.handle)}`, step, body),
      nodes: [seat.key],
      creates: [],
    });
  }

  if (inputs.mode === "edit" && settingsChanged(settings)) steps.push(settingsStep());
  return { steps };
}

/** Whether a draft has anything to save: a plan with at least one step. */
export function hasSaveSteps(inputs: Omit<PlanInputs, "writeId" | "summary">): boolean {
  return planSave({ ...inputs, writeId: "check", summary: "" }).steps.length > 0;
}

/** Every node the plan creates, across all its steps. */
export function createdBy(steps: readonly SaveStep[]): Creation[] {
  return steps.flatMap((s) => s.creates);
}

// ---------------------------------------------------------------------------
// What an answer means
// ---------------------------------------------------------------------------

/** What one step's answer means. */
export type StepOutcome =
  /**
   * Durable, and applied on the node that answered: its next read sees it.
   * A settings write carries the revision and the epoch it activated.
   */
  | {
      readonly kind: "applied";
      readonly position?: string;
      readonly revisionId?: string;
      readonly epoch?: number;
      readonly warnings?: readonly ConfigWarning[];
    }
  /** Durable at `position`; every node will apply it and this one has not yet. */
  | { readonly kind: "pending"; readonly position: string }
  /**
   * Nothing could establish what happened. A chart write is resent under the
   * same id (`opId`), which the chart's ledger answers with the first
   * arrival's outcome; a settings write is settled by reading the revisions.
   */
  | { readonly kind: "unknown"; readonly opId: string; readonly detail: string }
  /**
   * The engine will not take it as sent. `node` is the node the refusal names
   * (a batch's refused operation, a content write's object), `null` when it
   * names none.
   */
  | {
      readonly kind: "refused";
      readonly status: number;
      readonly code: string;
      readonly detail: string;
      /** The grants any one of which would admit the write (403). */
      readonly grants: readonly string[];
      /** The fields a content write's authority refusal names (403). */
      readonly fields: readonly string[];
      /** The rule a batch broke (422). */
      readonly rule?: string;
      readonly node: NodeKey | null;
    }
  /**
   * Somebody else's write got there first: a newer settings revision (409,
   * 412), or a write that lost a race on one of the chart's objects (`409
   * stale`). The draft is updated onto what is there now, never overwritten.
   */
  | {
      readonly kind: "conflict";
      readonly detail: string;
      readonly currentRevisionId: string | null;
    };

const text = (value: unknown): string => (typeof value === "string" ? value : "");
const strings = (value: unknown): string[] =>
  Array.isArray(value) ? value.filter((v): v is string => typeof v === "string") : [];

/**
 * What one step's answer means.
 *
 * AN ANSWER THAT NEVER ARRIVED, OR A FAILURE ON THE WAY, IS UNKNOWN — never
 * refused and never landed. Status 0 and every 5xx but one: a gateway that gave
 * up waiting says nothing about the write behind it, and a chart write the
 * node could not settle says so itself (`503` with `op_id`). The one certain
 * 5xx is `503 draining`, which the drain gate answers before the handler runs,
 * so nothing was written; it is still resent under the same id, which is
 * always safe and is the same thing the unknown case does.
 */
export function classifyStep(answer: HttpAnswer, step: SaveStep): StepOutcome {
  const body = isRecord(answer.body) ? answer.body : {};
  const code = text(body.error);
  const detail = text(body.detail) || text(body.message) || code;
  if (answer.status >= 200 && answer.status < 300) {
    if (step.kind === "settings") {
      return {
        kind: "applied",
        revisionId: text(body.revision_id),
        epoch: typeof body.epoch === "number" ? body.epoch : 0,
        warnings: Array.isArray(body.warnings) ? (body.warnings as ConfigWarning[]) : [],
      };
    }
    const position = text(body.position);
    return answer.status === 202 || body.outcome === "pending"
      ? { kind: "pending", position }
      : { kind: "applied", position };
  }
  if (answer.status === 0 || answer.status >= 500) {
    return {
      kind: "unknown",
      opId: text(body.op_id) || step.id,
      detail: detail || "The engine could not be reached.",
    };
  }
  if (answer.status === 409 || answer.status === 412) {
    return {
      kind: "conflict",
      detail,
      currentRevisionId: text(body.current_revision_id) || null,
    };
  }
  let node: NodeKey | null = step.nodes.length === 1 ? step.nodes[0]! : null;
  if (typeof body.index === "number" && step.nodes[body.index] !== undefined) {
    node = step.nodes[body.index]!;
  }
  return {
    kind: "refused",
    status: answer.status,
    code,
    detail: detail || `The engine refused the write with status ${answer.status}.`,
    grants: strings(body.grants),
    fields: strings(body.fields),
    ...(typeof body.rule === "string" ? { rule: body.rule } : {}),
    node,
  };
}

/** Whether an outcome landed: the write is durable, applied here or not. */
export function landed(outcome: StepOutcome | undefined): boolean {
  return outcome?.kind === "applied" || outcome?.kind === "pending";
}

/** The chart position a save's landed chart writes reached, the furthest of them. */
export function furthestPosition(outcomes: readonly (StepOutcome | undefined)[]): string | null {
  let best: { position: string; generation: number; seq: number } | null = null;
  for (const outcome of outcomes) {
    if (!outcome || (outcome.kind !== "applied" && outcome.kind !== "pending")) continue;
    const position = outcome.position;
    if (!position) continue;
    const parsed = parsePosition(position);
    if (!parsed) continue;
    if (
      !best ||
      parsed.generation > best.generation ||
      (parsed.generation === best.generation && parsed.seq > best.seq)
    ) {
      best = { position, ...parsed };
    }
  }
  return best?.position ?? null;
}

/**
 * A chart position, `STREAM@generation:seq`, as numbers. The sequence stays
 * exact below 2^53, which a log reaches after nine quadrillion records.
 */
export function parsePosition(
  position: string,
): { stream: string; generation: number; seq: number } | null {
  const match = /^(.+)@(\d+):(\d+)$/.exec(position);
  if (!match) return null;
  return { stream: match[1]!, generation: Number(match[2]), seq: Number(match[3]) };
}

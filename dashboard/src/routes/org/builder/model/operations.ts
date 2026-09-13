/**
 * Builder operations: what an edit IS, as data that replays.
 *
 * AN OPERATION IS RECORDED, THEN APPLIED, AND CAN BE REPLAYED ANYWHERE. The UI
 * builds an [Intent] (what the operator asked for, with any new node's key
 * minted in the event handler) and [record]s it against the current draft.
 * Recording is where every PRECONDITION is captured: the value each changed
 * field held, the whole node a removal deletes, the parent a move takes a node
 * out of and the neighbour it puts the node beside. The recorded operation is
 * JSON, carries no function and no reference into the draft, and is what the
 * log keeps, persists and replays.
 *
 * [evaluate] asks an operation whether it still applies to a draft, and says
 * one of three things. It applies. Its target is GONE (the node, the parent it
 * moves into, the schedule it toggles). Or it CONFLICTS: a precondition value
 * differs, which after a rebase means somebody else changed the same thing,
 * and the conflict carries the base value, their value and this operation's
 * value so a person chooses. Nothing is ever replayed over a changed value
 * silently: that is the lost update `If-Match` exists to prevent, and a
 * builder that rebased by re-applying new values would reintroduce it one step
 * later.
 *
 * AN EDIT CHANGES ONLY WHAT IT NAMES. `updateSeat` carries the fields that
 * differ from the form's initial values and nothing else, so a field somebody
 * else changed upstream and this operator never touched survives a rebase.
 *
 * WHAT FOLLOWS FROM NAMES IS DECIDED AT APPLY TIME. A unit's `lead`, a
 * `manages` entry and a root seat's `unit:` name another entity by its name.
 * When an operation renames or removes that entity, the references that named
 * it are followed or cleared as the operation applies, against the draft it
 * applies to, and reported. A reference somebody added upstream is therefore
 * followed too, instead of being left pointing at a name that no longer
 * exists. GitLab access levels are the exception: they are keyed by HANDLE,
 * which the client never derives, so the entries an operation clears are
 * resolved when it is recorded and carried, with their values, as
 * preconditions.
 *
 * The seat rules mirrored here are the ones an operation's own meaning needs
 * and nothing more: which fields a kind forbids (a kind change strips them,
 * and the engine refuses a seat that keeps them). The engine remains the
 * validator of the result: every draft these functions produce is checked by a
 * dry run.
 */

import type { CompanyDocument, ConfigRole, ConfigUnit } from "~/protocol/index.ts";
import { plural } from "~/lib/format.ts";
import { cloneJson, getPath, isRecord, jsonEqual, setPath } from "./json.ts";
import { COMPANY_KEY, handleOfKey, isMintedKey, type NodeKey } from "./keys.ts";
import {
  allSeats,
  allUnits,
  attach,
  detach,
  isWithin,
  locate,
  mapSeatData,
  mapUnitData,
  placementOf,
  siblingsAt,
  subtreeKeys,
  updateSeatData,
  updateUnitData,
  type Draft,
  type DraftSeat,
  type DraftUnit,
  type Located,
  type Placement,
} from "./draft.ts";
import { CHARTER_FIELDS, DATADOG_ROUTE_TO, GITLAB_ACCESS_LEVELS } from "./document.ts";

// ---------------------------------------------------------------------------
// Shapes
// ---------------------------------------------------------------------------

/**
 * The version of the operation shapes below. A persisted log carries it, and a
 * log of any other version is discarded on restore rather than interpreted:
 * replaying an operation under a meaning it was not recorded with is exactly
 * the silent corruption preconditions exist to stop.
 */
export const OPERATIONS_VERSION = 1;

/** Who holds a seat. An absent or unrecognised `kind` is an agent seat, as the engine reads it. */
export type SeatKind = "agent" | "human";

/** One field of an entity's own data, and the value it held when the edit was recorded. */
export interface FieldChange {
  /** Keys from the entity's data down to the field: `["integrations", "github", "tier"]`. */
  readonly path: readonly string[];
  /** The recorded value; omitted when the field was not set. */
  readonly before?: unknown;
  /** The new value; omitted to remove the field. */
  readonly after?: unknown;
}

/** One entry of `integrations.gitlab.provisioning.access_levels`, by handle. */
export interface AccessLevelChange {
  readonly handle: string;
  readonly before?: string;
  readonly after?: string;
}

/** `integrations.datadog.route_to`: the handle an alert naming nobody wakes. */
export interface RouteToChange {
  readonly before?: string;
  readonly after?: string;
}

/** A unit whose lead a move clears, and the lead it held. */
export interface LeadClear {
  readonly unit: NodeKey;
  readonly before: string;
}

/** A node as it stood when its removal was recorded. */
export interface NodeSnapshot {
  readonly key: NodeKey;
  /** The node's document JSON: a role object, or a unit with its `roles` and `children`. */
  readonly json: unknown;
}

/** The starting shapes `applyTemplate` can carry (see `templates.ts`). "empty" is the charter alone. */
export type TemplateId = "empty" | "new_company" | "established_company";

export interface AddUnit {
  readonly type: "addUnit";
  readonly key: NodeKey;
  readonly placement: Placement;
  /** The unit's own fields; never `roles` or `children`. */
  readonly data: ConfigUnit;
}

export interface AddSeat {
  readonly type: "addSeat";
  readonly key: NodeKey;
  readonly placement: Placement;
  readonly data: ConfigRole;
}

export interface Remove {
  readonly type: "remove";
  readonly target: NodeKey;
  /** The node and, for a unit, its whole subtree, as it stood. */
  readonly snapshot: NodeSnapshot;
  /**
   * For a unit: what happens to the root seats its `unit:` references place in
   * it. `remove` deletes them with the unit; `keep` leaves them at the root with
   * the reference cleared.
   */
  readonly placedSeats: "remove" | "keep";
  /** The root seats placed in the removed subtree by reference, as they stood. */
  readonly placed: readonly NodeSnapshot[];
  /** The access level entries of every removed seat, cleared. */
  readonly accessLevels: readonly AccessLevelChange[];
  /** A replacement Datadog fallback, when a removed seat was it. */
  readonly routeTo?: RouteToChange;
}

export interface RenameSeat {
  readonly type: "renameSeat";
  readonly target: NodeKey;
  readonly before: string;
  readonly after: string;
  /**
   * The engine-reported handle written as the seat's declared handle, so a
   * rename keeps the identity its memory and mailbox attach to. Set for a seat
   * of the base that declared none.
   */
  readonly pin?: string;
  /** For a seat created in this draft: the entry its old derived handle held, cleared. */
  readonly accessLevels: readonly AccessLevelChange[];
}

export interface RenameUnit {
  readonly type: "renameUnit";
  readonly target: NodeKey;
  readonly before: string;
  readonly after: string;
}

export interface Move {
  readonly type: "move";
  readonly target: NodeKey;
  readonly from: Placement;
  readonly to: Placement;
  /** The seat's `unit:` reference as it stood, removed: the move is the placement now. */
  readonly unitRef?: string;
  /** Units whose lead the operator chose to clear as the seat moves. */
  readonly clearLeads: readonly LeadClear[];
}

export interface Reorder {
  readonly type: "reorder";
  readonly target: NodeKey;
  readonly from: Placement;
  readonly to: Placement;
}

export interface UpdateSeat {
  readonly type: "updateSeat";
  readonly target: NodeKey;
  readonly changes: readonly FieldChange[];
  readonly accessLevels: readonly AccessLevelChange[];
}

export interface UpdateUnit {
  readonly type: "updateUnit";
  readonly target: NodeKey;
  readonly changes: readonly FieldChange[];
}

export interface SetLead {
  readonly type: "setLead";
  readonly target: NodeKey;
  /** Seat NAMES, as the document writes a lead. Omitted for none. */
  readonly before?: string;
  readonly after?: string;
}

export interface SetManages {
  readonly type: "setManages";
  readonly target: NodeKey;
  /** The explicit list, seat or unit names. Omitted for none. */
  readonly before?: readonly string[];
  readonly after?: readonly string[];
}

export interface ChangeKind {
  readonly type: "changeKind";
  readonly target: NodeKey;
  /** The `kind` as written; omitted when unwritten. */
  readonly before?: string;
  readonly after: SeatKind;
  /** Every field the new kind forbids, as it stood. Stripped. */
  readonly stripped: readonly FieldChange[];
  /** A human seat's contact identity, when becoming human. */
  readonly contact?: Record<string, string>;
  readonly routeTo?: RouteToChange;
}

export interface SetScheduleEnabled {
  readonly type: "setScheduleEnabled";
  readonly target: NodeKey;
  readonly schedule: string;
  readonly before?: boolean | null;
  readonly after: boolean;
}

export interface SetDatadogRouteTo {
  readonly type: "setDatadogRouteTo";
  readonly change: RouteToChange;
}

export interface UpdateCompany {
  readonly type: "updateCompany";
  readonly changes: readonly FieldChange[];
}

export interface ApplyTemplate {
  readonly type: "applyTemplate";
  readonly template: TemplateId;
  /** The charter the create form collected. */
  readonly charter: { readonly name: string; readonly mission?: string };
  /** The seats and units the template produced, with the keys the handler minted. */
  readonly roles: readonly DraftSeat[];
  readonly units: readonly DraftUnit[];
}

/** Every recorded operation. */
export type Operation =
  | AddUnit
  | AddSeat
  | Remove
  | RenameSeat
  | RenameUnit
  | Move
  | Reorder
  | UpdateSeat
  | UpdateUnit
  | SetLead
  | SetManages
  | ChangeKind
  | SetScheduleEnabled
  | SetDatadogRouteTo
  | UpdateCompany
  | ApplyTemplate;

export type OperationType = Operation["type"];

/** One field to set, as an editor emits it: only fields whose value differs from the form's start. */
export interface FieldSet {
  readonly path: readonly string[];
  /** Omitted to remove the field. */
  readonly value?: unknown;
}

/** What an operator asked for, before the draft supplies the preconditions. */
export type Intent =
  | {
      readonly type: "addUnit";
      readonly key: NodeKey;
      readonly placement: Placement;
      readonly data: ConfigUnit;
    }
  | {
      readonly type: "addSeat";
      readonly key: NodeKey;
      readonly placement: Placement;
      readonly data: ConfigRole;
    }
  | {
      readonly type: "remove";
      readonly target: NodeKey;
      readonly placedSeats?: "remove" | "keep";
      /** The replacement Datadog fallback handle, when a removed seat is it. */
      readonly routeTo?: string;
    }
  | { readonly type: "renameSeat"; readonly target: NodeKey; readonly name: string }
  | { readonly type: "renameUnit"; readonly target: NodeKey; readonly name: string }
  | {
      readonly type: "move";
      readonly target: NodeKey;
      readonly to: Placement;
      readonly clearLeads?: readonly NodeKey[];
    }
  | { readonly type: "reorder"; readonly target: NodeKey; readonly to: Placement }
  | {
      readonly type: "updateSeat";
      readonly target: NodeKey;
      readonly set: readonly FieldSet[];
      /** The seat's GitLab access level: a level to set, `null` to clear, omitted to leave. */
      readonly accessLevel?: string | null;
    }
  | { readonly type: "updateUnit"; readonly target: NodeKey; readonly set: readonly FieldSet[] }
  | { readonly type: "setLead"; readonly target: NodeKey; readonly lead?: string }
  | { readonly type: "setManages"; readonly target: NodeKey; readonly manages: readonly string[] }
  | {
      readonly type: "changeKind";
      readonly target: NodeKey;
      readonly kind: SeatKind;
      readonly contact?: Record<string, string>;
      readonly routeTo?: string;
    }
  | {
      readonly type: "setScheduleEnabled";
      readonly target: NodeKey;
      readonly schedule: string;
      readonly enabled: boolean;
    }
  | { readonly type: "setDatadogRouteTo"; readonly routeTo?: string }
  | { readonly type: "updateCompany"; readonly set: readonly FieldSet[] }
  | {
      readonly type: "applyTemplate";
      readonly template: TemplateId;
      readonly charter: { readonly name: string; readonly mission?: string };
      readonly roles: readonly DraftSeat[];
      readonly units: readonly DraftUnit[];
    };

/**
 * What recording needs that the draft cannot say: the handle the engine
 * derived for a seat that declares none, from the last check of THIS draft.
 * Existing seats carry their handle in their key and need no lookup.
 */
export interface RecordContext {
  readonly handleOf?: (key: NodeKey) => string | undefined;
}

/** Why an intent could not be recorded against the draft. */
export type RecordRefusal =
  | "missing_target"
  | "wrong_kind"
  | "missing_parent"
  | "missing_neighbour"
  | "key_in_use"
  | "not_minted"
  | "into_itself"
  | "across_parents"
  | "forbidden_field"
  | "unknown_handle"
  | "no_schedule"
  | "no_datadog"
  | "no_gitlab"
  | "not_empty"
  | "no_change";

export type Recorded =
  | { readonly ok: true; readonly op: Operation }
  | { readonly ok: false; readonly refusal: RecordRefusal; readonly message: string };

/** What an operation's preconditions say about a draft. */
export type Outcome =
  | { readonly kind: "applies" }
  | { readonly kind: "gone"; readonly reason: string }
  | { readonly kind: "conflict"; readonly conflicts: readonly Conflict[] };

/** One precondition that no longer holds, with every value a person needs to choose. */
export interface Conflict {
  /** What it is about, as a short phrase: "goal", "placement", "the whole seat". */
  readonly subject: string;
  /** The value when this operation was recorded. */
  readonly base?: unknown;
  /** The value in the draft now. */
  readonly theirs?: unknown;
  /** The value this operation writes. */
  readonly mine?: unknown;
}

/** A reference an operation cleared or followed, for the announcement and the review. */
export interface ReferenceEffect {
  readonly kind: "lead" | "manages" | "unit" | "gitlab_access_level";
  /** The node holding the reference; [COMPANY_KEY] for an integration entry. */
  readonly holder: NodeKey;
  readonly from: string;
  /** The new value when followed; omitted when cleared. */
  readonly to?: string;
}

/** What applying an operation did beyond its own target. */
export interface ApplyReport {
  readonly cleared: readonly ReferenceEffect[];
  readonly followed: readonly ReferenceEffect[];
  /** Fields a kind change removed, by authored name. */
  readonly stripped: readonly string[];
}

// ---------------------------------------------------------------------------
// Seat kind rules
// ---------------------------------------------------------------------------

/**
 * The fields a human seat must not carry, in the order the engine reports
 * them (`org.Role.humanForbidden`, by authored name), with the ones that hold
 * credentials marked: a credential a kind change strips was masked in the
 * document the builder holds, so it cannot be re-entered here and is gone for
 * good once the change is saved.
 */
export const HUMAN_FORBIDDEN: readonly {
  readonly path: readonly string[];
  readonly credential: boolean;
}[] = [
  { path: ["llm"], credential: false },
  { path: ["llm_review"], credential: false },
  { path: ["llm_subagent"], credential: false },
  { path: ["llm_auxiliary"], credential: false },
  { path: ["llm_judge"], credential: false },
  { path: ["llm_sandbox"], credential: false },
  { path: ["sandbox"], credential: true },
  { path: ["token_budget"], credential: false },
  { path: ["workers"], credential: false },
  { path: ["learning_enabled"], credential: false },
  { path: ["schedules"], credential: false },
  { path: ["integrations", "slack"], credential: true },
  { path: ["integrations", "mattermost"], credential: true },
  { path: ["integrations", "jira"], credential: false },
  { path: ["integrations", "confluence"], credential: false },
  { path: ["mcp_env"], credential: true },
  { path: ["behavioral_guidelines"], credential: false },
];

/** The fields an agent seat must not carry (`org.Role.Validate`). */
export const AGENT_FORBIDDEN: readonly {
  readonly path: readonly string[];
  readonly credential: boolean;
}[] = [
  { path: ["contact"], credential: false },
  { path: ["availability"], credential: false },
];

/** Who holds a seat, as the engine reads its `kind`. */
export function kindOf(data: ConfigRole): SeatKind {
  return data.kind === "human" ? "human" : "agent";
}

/** The authored name of a field path: `integrations.slack`. */
export function fieldName(path: readonly string[]): string {
  return path.join(".");
}

/** Whether a field path holds a credential a kind change would strip. */
export function isCredentialField(path: readonly string[]): boolean {
  return [...HUMAN_FORBIDDEN, ...AGENT_FORBIDDEN].some(
    (f) => f.credential && jsonEqual(f.path, path),
  );
}

/** The fields a seat holds that `kind` forbids, with their values. */
function forbiddenFor(data: ConfigRole, kind: SeatKind): FieldChange[] {
  const rules = kind === "human" ? HUMAN_FORBIDDEN : AGENT_FORBIDDEN;
  const out: FieldChange[] = [];
  for (const rule of rules) {
    const value = getPath(data, rule.path);
    if (value !== undefined) out.push({ path: rule.path, before: cloneJson(value) });
  }
  return out;
}

// ---------------------------------------------------------------------------
// Which fields each operation owns
// ---------------------------------------------------------------------------

/**
 * The seat fields `updateSeat` may not write, each because another operation
 * owns it and carries what that change means: a rename pins the handle and
 * follows references, a kind change strips forbidden fields, `manages` is
 * compared whole, a move replaces `unit:` with a placement, and a schedule is
 * only ever toggled here. `handle` is refused on a seat of the base, whose
 * handle is its identity; a seat this draft created may still choose one.
 */
const SEAT_OWNED = new Set(["name", "kind", "manages", "unit", "schedules"]);

/** The unit fields `updateUnit` may not write. */
const UNIT_OWNED = new Set(["name", "lead", "roles", "children", "schedules"]);

/** Whether `updateSeat` may write a field of a seat; `minted` says the seat was created in this draft. */
function seatFieldWritable(path: readonly string[], minted: boolean): boolean {
  const head = path[0];
  if (head === undefined || SEAT_OWNED.has(head)) return false;
  return head !== "handle" || minted;
}

/** Whether `updateUnit` may write a field of a unit. */
function unitFieldWritable(path: readonly string[]): boolean {
  const head = path[0];
  return head !== undefined && !UNIT_OWNED.has(head);
}

/** Whether `updateCompany` may write a field: the charter, and nothing else. */
function charterFieldWritable(path: readonly string[]): boolean {
  return path.length === 1 && (CHARTER_FIELDS as readonly string[]).includes(path[0]!);
}

/**
 * Why an operation could never have been recorded, whatever the draft: a
 * field another operation owns, an existing seat's handle, anything outside
 * the charter in a company edit, or a created node without a minted key.
 * `null` when it is well formed.
 *
 * [record] refuses each of these as it builds an operation. A log read back
 * from storage is held to the same rules before it replays, because
 * [evaluate] checks only what a draft can say about an operation, and a stored
 * log that renamed a seat through `updateSeat` would otherwise skip the pin
 * that keeps the seat's identity.
 */
export function malformedReason(op: Operation): string | null {
  switch (op.type) {
    case "addUnit":
    case "addSeat":
      return isMintedKey(op.key) ? null : "a created node has no minted key";
    case "updateSeat":
      return op.changes.every((c) => seatFieldWritable(c.path, isMintedKey(op.target)))
        ? null
        : "a seat edit writes a field its own action owns";
    case "updateUnit":
      return op.changes.every((c) => unitFieldWritable(c.path))
        ? null
        : "a unit edit writes a field its own action owns";
    case "updateCompany":
      return op.changes.every((c) => charterFieldWritable(c.path))
        ? null
        : "a company edit writes outside the charter";
    case "applyTemplate":
      return templateKeys(op.roles, op.units).every(isMintedKey)
        ? null
        : "a template node has no minted key";
    default:
      return null;
  }
}

// ---------------------------------------------------------------------------
// Record
// ---------------------------------------------------------------------------

const refuse = (refusal: RecordRefusal, message: string): Recorded => ({
  ok: false,
  refusal,
  message,
});
const recorded = (op: Operation): Recorded => ({ ok: true, op });

/** The handle of a seat in the draft: its key's, its declared one, or the last check's. */
function seatHandle(seat: DraftSeat, ctx: RecordContext): string | undefined {
  const declared =
    typeof seat.data.handle === "string" && seat.data.handle !== "" ? seat.data.handle : undefined;
  return declared ?? handleOfKey(seat.key) ?? ctx.handleOf?.(seat.key);
}

/** The access level entry for a handle in the draft, when there is one. */
function accessLevel(draft: Draft, handle: string): string | undefined {
  const value = getPath(draft.company, [...GITLAB_ACCESS_LEVELS, handle]);
  return typeof value === "string" ? value : undefined;
}

/** Whether the draft holds any GitLab access level entry at all. */
function hasAccessLevels(draft: Draft): boolean {
  const levels = getPath(draft.company, GITLAB_ACCESS_LEVELS);
  return isRecord(levels) && Object.keys(levels).length > 0;
}

/**
 * The refusal for an operation that would leave a seat's GitLab access level
 * behind because the seat's handle is not known.
 *
 * A LEVEL NOBODY CAN FIND IS A GRANT WAITING FOR A SEAT. Access levels are
 * keyed by handle, the client never derives one, and the handle of a seat this
 * draft created is only known from a check of the draft as it stands, which
 * any later edit makes stale until the next check answers. Removing or
 * renaming such a seat in that window would clear nothing, and the entry it
 * held would grant its level to the next seat the engine gives that handle.
 * So while the draft holds access levels, an operation that must clear one by
 * an unknown handle waits for the check instead of guessing; a draft with no
 * access levels has nothing to leave behind and is not held up.
 */
function unknownHandleForLevels(action: string): Recorded {
  return refuse(
    "unknown_handle",
    `The engine has not reported this seat's handle yet, and GitLab access levels are keyed by it. Wait for the check to finish, then ${action}.`,
  );
}

/** The GitLab provisioning block that holds the access levels. */
const GITLAB_PROVISIONING = GITLAB_ACCESS_LEVELS.slice(0, -1);
/** The Datadog block that holds the fallback seat. */
const DATADOG_BLOCK = DATADOG_ROUTE_TO.slice(0, -1);

const hasBlock = (company: CompanyDocument, block: readonly string[]) =>
  isRecord(getPath(company, block));

/**
 * The company document with one value inside an integration block set or
 * removed, never creating the block and never pruning it.
 *
 * AN INTEGRATION BLOCK IS A CONNECTION, NOT A CONTAINER. `integrations.gitlab`
 * and `integrations.datadog` exist because an operator connected the tool
 * from Integrations, and a seat edit only reaches one value inside each. So a
 * value is written only into a block that is there (recording refuses one
 * that is not), and removing the last access level leaves the provisioning
 * block standing: [setPath] alone would prune the objects the removal
 * emptied, all the way up to the block itself.
 */
function withinBlock(
  company: CompanyDocument,
  path: readonly string[],
  blockLength: number,
  value: unknown,
): CompanyDocument {
  const block = path.slice(0, blockLength);
  const current = getPath(company, block);
  if (!isRecord(current)) return company;
  return setPath(
    company,
    block,
    setPath(current, path.slice(blockLength), value),
  ) as CompanyDocument;
}

function withAccessLevel(
  company: CompanyDocument,
  handle: string,
  level: string | undefined,
): CompanyDocument {
  return withinBlock(company, [...GITLAB_ACCESS_LEVELS, handle], GITLAB_PROVISIONING.length, level);
}

function withRouteTo(company: CompanyDocument, handle: string | undefined): CompanyDocument {
  return withinBlock(company, DATADOG_ROUTE_TO, DATADOG_BLOCK.length, handle);
}

/** The document JSON of a located node. */
export function nodeJson(found: Located): unknown {
  return found.kind === "seat" ? found.node.data : unitJson(found.node);
}

function unitJson(unit: DraftUnit): ConfigUnit {
  const out: ConfigUnit = { ...unit.data };
  if (unit.roles.length > 0) out.roles = unit.roles.map((s) => s.data);
  if (unit.children.length > 0) out.children = unit.children.map(unitJson);
  return out;
}

/**
 * The root seats a unit's subtree holds by reference: each root seat whose
 * `unit:` names a unit in the subtree, when no unit outside it carries that
 * name (a duplicate outside would still resolve the reference).
 */
function placedByReference(draft: Draft, unit: DraftUnit): DraftSeat[] {
  const inside = new Set(subtreeKeys(unit));
  const insideNames = new Set<string>();
  const outsideNames = new Set<string>();
  for (const { unit: u } of allUnits(draft)) {
    (inside.has(u.key) ? insideNames : outsideNames).add(u.data.name);
  }
  return draft.roles.filter(
    (s) =>
      typeof s.data.unit === "string" &&
      insideNames.has(s.data.unit) &&
      !outsideNames.has(s.data.unit),
  );
}

function routeToChange(draft: Draft, after: string | undefined): RouteToChange | undefined {
  if (after === undefined) return undefined;
  const before = getPath(draft.company, DATADOG_ROUTE_TO);
  return { before: typeof before === "string" ? before : undefined, after };
}

/** Resolves a placement whose neighbour is no longer beside its slot to the end of the list. */
function placementOrEnd(
  draft: Draft,
  placement: Placement,
  kind: "seat" | "unit",
  self?: NodeKey,
): Placement {
  const siblings = (siblingsAt(draft, placement.parent, kind) ?? []).filter((s) => s.key !== self);
  if (placement.after === null || siblings.some((s) => s.key === placement.after)) return placement;
  return {
    parent: placement.parent,
    after: siblings.length > 0 ? siblings[siblings.length - 1]!.key : null,
  };
}

/**
 * Records an intent against the draft: fills in every precondition from the
 * draft as it stands, or refuses with the reason and a sentence to show.
 *
 * With `lenientPlacement`, a placement whose neighbour is not beside the slot
 * lands at the end of the list instead of being refused. That is the "keep
 * mine" of a placement conflict, and nothing else asks for it.
 */
export function record(
  draft: Draft,
  intent: Intent,
  ctx: RecordContext = {},
  options: { lenientPlacement?: boolean } = {},
): Recorded {
  const seatAt = (key: NodeKey) => {
    const found = locate(draft, key);
    return found?.kind === "seat" ? found : undefined;
  };
  const unitAt = (key: NodeKey) => {
    const found = locate(draft, key);
    return found?.kind === "unit" ? found : undefined;
  };
  const missing = (key: NodeKey) =>
    locate(draft, key)
      ? refuse("wrong_kind", "That operation does not apply to this kind of node.")
      : refuse("missing_target", "That node is no longer in the draft.");

  const checkPlacement = (
    placement: Placement,
    kind: "seat" | "unit",
    self?: NodeKey,
  ): Placement | Recorded => {
    const siblings = siblingsAt(draft, placement.parent, kind);
    if (!siblings) return refuse("missing_parent", "The destination is no longer in the draft.");
    if (placement.after === placement.parent || placement.after === self) {
      return refuse("missing_neighbour", "A node cannot be placed beside itself.");
    }
    if (placement.after !== null && !siblings.some((s) => s.key === placement.after)) {
      if (options.lenientPlacement) return placementOrEnd(draft, placement, kind, self);
      return refuse("missing_neighbour", "The neighbour for that position is no longer there.");
    }
    return placement;
  };
  const isRecorded = (value: Placement | Recorded): value is Recorded => "ok" in value;

  switch (intent.type) {
    case "addUnit":
    case "addSeat": {
      if (!isMintedKey(intent.key)) {
        return refuse("not_minted", "A new node needs a key minted for it.");
      }
      if (locate(draft, intent.key)) return refuse("key_in_use", "That key already names a node.");
      const kind = intent.type === "addUnit" ? "unit" : "seat";
      const placement = checkPlacement(intent.placement, kind);
      if (isRecorded(placement)) return placement;
      if (intent.type === "addUnit") {
        const { roles: _roles, children: _children, ...data } = cloneJson(intent.data);
        return recorded({ type: "addUnit", key: intent.key, placement, data: data as ConfigUnit });
      }
      return recorded({
        type: "addSeat",
        key: intent.key,
        placement,
        data: cloneJson(intent.data),
      });
    }

    case "remove": {
      const found = locate(draft, intent.target);
      if (!found) return refuse("missing_target", "That node is no longer in the draft.");
      const placed = found.kind === "unit" ? placedByReference(draft, found.node) : [];
      const placedSeats = intent.placedSeats ?? "keep";
      const removedSeats: DraftSeat[] =
        found.kind === "seat"
          ? [found.node]
          : [
              ...[...allSeats(draft)]
                .filter(({ parent }) => isWithin(draft, parent, found.node.key))
                .map(({ seat }) => seat),
              ...(placedSeats === "remove" ? placed : []),
            ];
      const accessLevels: AccessLevelChange[] = [];
      for (const seat of removedSeats) {
        const handle = seatHandle(seat, ctx);
        if (handle === undefined) {
          if (hasAccessLevels(draft)) return unknownHandleForLevels("remove it");
          continue;
        }
        const level = accessLevel(draft, handle);
        if (level !== undefined) accessLevels.push({ handle, before: level });
      }
      if (intent.routeTo !== undefined && !hasBlock(draft.company, DATADOG_BLOCK)) {
        return refuse(
          "no_datadog",
          "Datadog is not connected, so there is no fallback seat to replace.",
        );
      }
      const routeTo = routeToChange(draft, intent.routeTo);
      return recorded({
        type: "remove",
        target: intent.target,
        snapshot: { key: found.node.key, json: cloneJson(nodeJson(found)) },
        placedSeats,
        placed: placed.map((s) => ({ key: s.key, json: cloneJson(s.data) })),
        accessLevels,
        ...(routeTo ? { routeTo } : {}),
      });
    }

    case "renameSeat": {
      const found = seatAt(intent.target);
      if (!found) return missing(intent.target);
      const before = found.node.data.name;
      const after = intent.name.trim();
      if (after === before) return refuse("no_change", "The name is unchanged.");
      const declared = typeof found.node.data.handle === "string" && found.node.data.handle !== "";
      const accessLevels: AccessLevelChange[] = [];
      let pin: string | undefined;
      if (!declared) {
        if (isMintedKey(found.node.key)) {
          // A seat this draft created runs under whatever the engine derives
          // from its name, so the handle changes with it: an access level set
          // for the old handle would be left keyed to a seat that no longer
          // exists and grant its level to the next seat deriving that handle.
          const handle = ctx.handleOf?.(found.node.key);
          if (handle === undefined) {
            if (hasAccessLevels(draft)) return unknownHandleForLevels("rename it");
          } else {
            const level = accessLevel(draft, handle);
            if (level !== undefined) accessLevels.push({ handle, before: level });
          }
        } else {
          // The key names the engine's handle for a seat of the base. A seat
          // keyed by its path (the base had not been checked when it was
          // keyed) takes the handle the last check of this draft reported:
          // that is still the engine's derivation, of the name the seat has
          // held since the base, because every rename pins.
          pin = handleOfKey(found.node.key) ?? ctx.handleOf?.(found.node.key);
          if (pin === undefined) {
            return refuse(
              "unknown_handle",
              "The engine has not reported this seat's handle yet, so renaming it could change its identity. Wait for the check to finish, then rename it.",
            );
          }
        }
      }
      return recorded({
        type: "renameSeat",
        target: intent.target,
        before,
        after,
        ...(pin !== undefined ? { pin } : {}),
        accessLevels,
      });
    }

    case "renameUnit": {
      const found = unitAt(intent.target);
      if (!found) return missing(intent.target);
      const before = found.node.data.name;
      const after = intent.name.trim();
      if (after === before) return refuse("no_change", "The name is unchanged.");
      return recorded({ type: "renameUnit", target: intent.target, before, after });
    }

    case "move":
    case "reorder": {
      const found = locate(draft, intent.target);
      if (!found) return refuse("missing_target", "That node is no longer in the draft.");
      const from = placementOf(found);
      if (intent.type === "reorder" && intent.to.parent !== from.parent) {
        return refuse(
          "across_parents",
          "A reorder keeps a node under the same parent. Use Move to.",
        );
      }
      if (
        found.kind === "unit" &&
        intent.to.parent !== COMPANY_KEY &&
        isWithin(draft, intent.to.parent, found.node.key)
      ) {
        return refuse(
          "into_itself",
          "A unit cannot be moved into itself or into one of its own units.",
        );
      }
      const to = checkPlacement(intent.to, found.kind, found.node.key);
      if (isRecorded(to)) return to;
      if (intent.type === "reorder") {
        if (jsonEqual(to, from)) return refuse("no_change", "The position is unchanged.");
        return recorded({ type: "reorder", target: intent.target, from, to });
      }
      // EVERY seat's `unit:` goes, not only a root seat's. On a root seat it
      // is the placement this move replaces. On a nested seat the engine
      // ignores it, and it would stop being ignored the moment the seat
      // reached the root: a move to the root would then silently place the
      // seat back in whatever unit the stale reference names.
      const unitRef =
        found.kind === "seat" && typeof found.node.data.unit === "string"
          ? found.node.data.unit
          : undefined;
      if (jsonEqual(to, from) && unitRef === undefined) {
        return refuse("no_change", "The position is unchanged.");
      }
      const clearLeads: LeadClear[] = [];
      for (const key of intent.clearLeads ?? []) {
        const unit = unitAt(key);
        if (!unit) return missing(key);
        const lead = unit.node.data.lead;
        if (typeof lead === "string" && lead !== "") clearLeads.push({ unit: key, before: lead });
      }
      return recorded({
        type: "move",
        target: intent.target,
        from,
        to,
        ...(unitRef !== undefined ? { unitRef } : {}),
        clearLeads,
      });
    }

    case "updateSeat": {
      const found = seatAt(intent.target);
      if (!found) return missing(intent.target);
      const changes: FieldChange[] = [];
      const accessLevels: AccessLevelChange[] = [];
      /** The level a handle change carries from the old handle to the new one. */
      let carried: { readonly handle: string; readonly level: string } | undefined;
      for (const set of intent.set) {
        const head = set.path[0];
        if (!seatFieldWritable(set.path, isMintedKey(found.node.key))) {
          return refuse(
            "forbidden_field",
            head === "handle"
              ? "An existing seat keeps its handle: it is the identity its memory and mailbox attach to."
              : `The ${fieldName(set.path)} field is changed by its own action.`,
          );
        }
        if (head === "handle") {
          // Choosing a handle takes a new seat's access level off the handle
          // it had, for the reason renameSeat gives, and onto the handle it
          // chose: the operator changed what the seat is called, not what it
          // may do. With no handle chosen the engine derives one, which is
          // not known until the next check, so the level is cleared and the
          // review lists it.
          const old = seatHandle(found.node, ctx);
          if (old === undefined) {
            if (hasAccessLevels(draft)) return unknownHandleForLevels("choose its handle");
          } else {
            const level = accessLevel(draft, old);
            if (level !== undefined && old !== set.value) {
              accessLevels.push({ handle: old, before: level });
              if (typeof set.value === "string" && set.value !== "")
                carried = { handle: set.value, level };
            }
          }
        }
        const before = getPath(found.node.data, set.path);
        if (jsonEqual(before, set.value)) continue;
        changes.push(fieldChange(set.path, before, set.value));
      }
      if (typeof intent.accessLevel === "string" && !hasBlock(draft.company, GITLAB_PROVISIONING)) {
        return refuse(
          "no_gitlab",
          "GitLab provisioning is not connected. Connect it from Integrations before setting an access level.",
        );
      }
      if (intent.accessLevel !== undefined) {
        const renamed = intent.set.find((s) => jsonEqual(s.path, ["handle"]));
        const handle =
          typeof renamed?.value === "string" ? renamed.value : seatHandle(found.node, ctx);
        if (handle === undefined) {
          return refuse(
            "unknown_handle",
            "The engine has not reported this seat's handle yet, and the access level is keyed by it. Wait for the check to finish, then set it.",
          );
        }
        const before = accessLevel(draft, handle);
        const after = intent.accessLevel ?? undefined;
        if (before !== after && !accessLevels.some((a) => a.handle === handle)) {
          accessLevels.push({
            handle,
            ...(before !== undefined ? { before } : {}),
            ...(after !== undefined ? { after } : {}),
          });
        }
      } else if (carried !== undefined) {
        const before = accessLevel(draft, carried.handle);
        if (before !== carried.level) {
          accessLevels.push({
            handle: carried.handle,
            ...(before !== undefined ? { before } : {}),
            after: carried.level,
          });
        }
      }
      if (changes.length === 0 && accessLevels.length === 0)
        return refuse("no_change", "Nothing changed.");
      return recorded({ type: "updateSeat", target: intent.target, changes, accessLevels });
    }

    case "updateUnit": {
      const found = unitAt(intent.target);
      if (!found) return missing(intent.target);
      const changes: FieldChange[] = [];
      for (const set of intent.set) {
        if (!unitFieldWritable(set.path)) {
          return refuse(
            "forbidden_field",
            `The ${fieldName(set.path)} field is changed by its own action.`,
          );
        }
        const before = getPath(found.node.data, set.path);
        if (!jsonEqual(before, set.value)) changes.push(fieldChange(set.path, before, set.value));
      }
      if (changes.length === 0) return refuse("no_change", "Nothing changed.");
      return recorded({ type: "updateUnit", target: intent.target, changes });
    }

    case "setLead": {
      const found = unitAt(intent.target);
      if (!found) return missing(intent.target);
      const before = nonEmpty(found.node.data.lead);
      const after = nonEmpty(intent.lead);
      if (before === after) return refuse("no_change", "The lead is unchanged.");
      return recorded({
        type: "setLead",
        target: intent.target,
        ...(before !== undefined ? { before } : {}),
        ...(after !== undefined ? { after } : {}),
      });
    }

    case "setManages": {
      const found = seatAt(intent.target);
      if (!found) return missing(intent.target);
      const before = nonEmptyList(found.node.data.manages);
      const after = nonEmptyList(intent.manages);
      if (jsonEqual(before, after)) return refuse("no_change", "The reports are unchanged.");
      return recorded({
        type: "setManages",
        target: intent.target,
        ...(before !== undefined ? { before } : {}),
        ...(after !== undefined ? { after } : {}),
      });
    }

    case "changeKind": {
      const found = seatAt(intent.target);
      if (!found) return missing(intent.target);
      if (kindOf(found.node.data) === intent.kind)
        return refuse("no_change", "The seat is already that kind.");
      if (intent.routeTo !== undefined && !hasBlock(draft.company, DATADOG_BLOCK)) {
        return refuse(
          "no_datadog",
          "Datadog is not connected, so there is no fallback seat to replace.",
        );
      }
      const routeTo = routeToChange(draft, intent.routeTo);
      return recorded({
        type: "changeKind",
        target: intent.target,
        ...(typeof found.node.data.kind === "string" ? { before: found.node.data.kind } : {}),
        after: intent.kind,
        stripped: forbiddenFor(found.node.data, intent.kind),
        ...(intent.kind === "human" && intent.contact
          ? { contact: cloneJson(intent.contact) }
          : {}),
        ...(routeTo ? { routeTo } : {}),
      });
    }

    case "setScheduleEnabled": {
      const found = locate(draft, intent.target);
      if (!found) return refuse("missing_target", "That node is no longer in the draft.");
      const schedule = scheduleOf(found.node.data, intent.schedule);
      if (!schedule) return refuse("no_schedule", "That schedule is no longer on this node.");
      const before = schedule.enabled;
      // Unset and `null` both run the schedule, so writing `enabled: true`
      // over either changes nothing the engine does and would show as an
      // edit nobody made.
      if ((before ?? true) === intent.enabled) {
        return refuse("no_change", "The schedule is already set that way.");
      }
      return recorded({
        type: "setScheduleEnabled",
        target: intent.target,
        schedule: intent.schedule,
        ...(before !== undefined ? { before } : {}),
        after: intent.enabled,
      });
    }

    case "setDatadogRouteTo": {
      if (!hasBlock(draft.company, DATADOG_BLOCK)) {
        return refuse("no_datadog", "Datadog is not connected.");
      }
      const current = getPath(draft.company, DATADOG_ROUTE_TO);
      const before = typeof current === "string" ? current : undefined;
      const after = nonEmpty(intent.routeTo);
      if (before === after) return refuse("no_change", "The Datadog fallback is unchanged.");
      return recorded({
        type: "setDatadogRouteTo",
        change: {
          ...(before !== undefined ? { before } : {}),
          ...(after !== undefined ? { after } : {}),
        },
      });
    }

    case "updateCompany": {
      const changes: FieldChange[] = [];
      for (const set of intent.set) {
        if (!charterFieldWritable(set.path)) {
          return refuse(
            "forbidden_field",
            `The builder edits the charter only, not ${fieldName(set.path)}.`,
          );
        }
        const before = getPath(draft.company, set.path);
        if (!jsonEqual(before, set.value)) changes.push(fieldChange(set.path, before, set.value));
      }
      if (changes.length === 0) return refuse("no_change", "Nothing changed.");
      return recorded({ type: "updateCompany", changes });
    }

    case "applyTemplate": {
      if (draft.roles.length > 0 || draft.units.length > 0) {
        return refuse(
          "not_empty",
          "A template starts a company, and this draft already has seats or units.",
        );
      }
      for (const key of templateKeys(intent.roles, intent.units)) {
        if (!isMintedKey(key))
          return refuse("not_minted", "Every node a template creates needs a key minted for it.");
      }
      return recorded({
        type: "applyTemplate",
        template: intent.template,
        charter: cloneJson(intent.charter),
        roles: cloneJson(intent.roles),
        units: cloneJson(intent.units),
      });
    }
  }
}

/** Every key a template fragment carries, in walk order. */
function templateKeys(roles: readonly DraftSeat[], units: readonly DraftUnit[]): NodeKey[] {
  const walk = (u: DraftUnit): NodeKey[] => [
    u.key,
    ...u.roles.map((s) => s.key),
    ...u.children.flatMap(walk),
  ];
  return [...roles.map((s) => s.key), ...units.flatMap(walk)];
}

function fieldChange(path: readonly string[], before: unknown, after: unknown): FieldChange {
  return {
    path: [...path],
    ...(before !== undefined ? { before: cloneJson(before) } : {}),
    ...(after !== undefined ? { after: cloneJson(after) } : {}),
  };
}

function nonEmpty(value: unknown): string | undefined {
  return typeof value === "string" && value.trim() !== "" ? value : undefined;
}

function nonEmptyList(value: unknown): string[] | undefined {
  return Array.isArray(value) && value.length > 0 ? value.map(String) : undefined;
}

function scheduleOf(data: ConfigRole | ConfigUnit, name: string) {
  return Array.isArray(data.schedules) ? data.schedules.find((s) => s?.name === name) : undefined;
}

/** The intent an operation was recorded from, for recording it again ("keep mine"). */
export function intentOf(op: Operation): Intent {
  switch (op.type) {
    case "addUnit":
    case "addSeat":
      return op;
    case "remove":
      return {
        type: "remove",
        target: op.target,
        placedSeats: op.placedSeats,
        ...(op.routeTo?.after !== undefined ? { routeTo: op.routeTo.after } : {}),
      };
    case "renameSeat":
      return { type: "renameSeat", target: op.target, name: op.after };
    case "renameUnit":
      return { type: "renameUnit", target: op.target, name: op.after };
    case "move":
      return {
        type: "move",
        target: op.target,
        to: op.to,
        clearLeads: op.clearLeads.map((c) => c.unit),
      };
    case "reorder":
      return { type: "reorder", target: op.target, to: op.to };
    case "updateSeat": {
      const own = op.accessLevels.filter((a) => a.after !== undefined);
      return {
        type: "updateSeat",
        target: op.target,
        set: op.changes.map((c) => ({
          path: c.path,
          ...(c.after !== undefined ? { value: c.after } : {}),
        })),
        ...(own.length > 0
          ? { accessLevel: own[0]!.after! }
          : op.accessLevels.length > 0
            ? { accessLevel: null }
            : {}),
      };
    }
    case "updateUnit":
      return {
        type: "updateUnit",
        target: op.target,
        set: op.changes.map((c) => ({
          path: c.path,
          ...(c.after !== undefined ? { value: c.after } : {}),
        })),
      };
    case "setLead":
      return {
        type: "setLead",
        target: op.target,
        ...(op.after !== undefined ? { lead: op.after } : {}),
      };
    case "setManages":
      return { type: "setManages", target: op.target, manages: op.after ?? [] };
    case "changeKind":
      return {
        type: "changeKind",
        target: op.target,
        kind: op.after,
        ...(op.contact ? { contact: op.contact } : {}),
        ...(op.routeTo?.after !== undefined ? { routeTo: op.routeTo.after } : {}),
      };
    case "setScheduleEnabled":
      return {
        type: "setScheduleEnabled",
        target: op.target,
        schedule: op.schedule,
        enabled: op.after,
      };
    case "setDatadogRouteTo":
      return {
        type: "setDatadogRouteTo",
        ...(op.change.after !== undefined ? { routeTo: op.change.after } : {}),
      };
    case "updateCompany":
      return {
        type: "updateCompany",
        set: op.changes.map((c) => ({
          path: c.path,
          ...(c.after !== undefined ? { value: c.after } : {}),
        })),
      };
    case "applyTemplate":
      return op;
  }
}

// ---------------------------------------------------------------------------
// Evaluate
// ---------------------------------------------------------------------------

const APPLIES: Outcome = { kind: "applies" };
const gone = (reason: string): Outcome => ({ kind: "gone", reason });

/** Whether an operation's preconditions hold on a draft. */
export function evaluate(draft: Draft, op: Operation): Outcome {
  const conflicts: Conflict[] = [];
  const expect = (subject: string, base: unknown, theirs: unknown, mine?: unknown) => {
    if (!jsonEqual(base, theirs)) conflicts.push({ subject, base, theirs, mine });
  };
  let goneReason: string | undefined;
  const expectAccessLevels = (changes: readonly AccessLevelChange[]) => {
    for (const change of changes) {
      if (change.after !== undefined && !hasBlock(draft.company, GITLAB_PROVISIONING)) {
        goneReason = "GitLab provisioning is no longer connected.";
      }
      expect(
        `GitLab access level for ${change.handle}`,
        change.before,
        accessLevel(draft, change.handle),
        change.after,
      );
    }
  };
  const expectRouteTo = (change: RouteToChange | undefined) => {
    if (!change) return;
    if (change.after !== undefined && !hasBlock(draft.company, DATADOG_BLOCK)) {
      goneReason = "Datadog is no longer connected.";
    }
    const current = getPath(draft.company, DATADOG_ROUTE_TO);
    expect(
      "Datadog fallback",
      change.before,
      typeof current === "string" ? current : undefined,
      change.after,
    );
  };
  const expectPlacementSlot = (
    placement: Placement,
    kind: "seat" | "unit",
    self: NodeKey,
  ): Outcome | undefined => {
    const siblings = siblingsAt(draft, placement.parent, kind);
    if (!siblings) return gone("The destination is no longer in the organization.");
    if (
      placement.after !== null &&
      !siblings.some((s) => s.key === placement.after && s.key !== self)
    ) {
      conflicts.push({
        subject: "position",
        base: placement.after,
        theirs: null,
        mine: placement.after,
      });
    }
    return undefined;
  };
  const finish = (): Outcome =>
    goneReason !== undefined
      ? gone(goneReason)
      : conflicts.length > 0
        ? { kind: "conflict", conflicts }
        : APPLIES;

  switch (op.type) {
    case "addUnit":
    case "addSeat": {
      if (locate(draft, op.key)) return gone("It has already been added.");
      const slot = expectPlacementSlot(
        op.placement,
        op.type === "addUnit" ? "unit" : "seat",
        op.key,
      );
      return slot ?? finish();
    }

    case "remove": {
      const found = locate(draft, op.target);
      if (!found) return gone("It has already been removed.");
      expect(
        found.kind === "seat" ? "the whole seat" : "the whole unit",
        op.snapshot.json,
        nodeJson(found),
      );
      if (found.kind === "unit") {
        const placed = placedByReference(draft, found.node).map((s) => ({
          key: s.key,
          json: s.data,
        }));
        expect("seats placed in it by reference", op.placed, placed);
      }
      expectAccessLevels(op.accessLevels);
      expectRouteTo(op.routeTo);
      return finish();
    }

    case "renameSeat":
    case "renameUnit": {
      const found = locate(draft, op.target);
      const kind = op.type === "renameSeat" ? "seat" : "unit";
      if (found?.kind !== kind) return gone("It is no longer in the organization.");
      expect("name", op.before, found.node.data.name, op.after);
      if (op.type === "renameSeat") {
        if (op.pin !== undefined) {
          const declared = found.node.data.handle;
          if (declared !== undefined && declared !== op.pin) {
            conflicts.push({ subject: "handle", base: undefined, theirs: declared, mine: op.pin });
          }
        }
        expectAccessLevels(op.accessLevels);
      }
      return finish();
    }

    case "move":
    case "reorder": {
      const found = locate(draft, op.target);
      if (!found) return gone("It is no longer in the organization.");
      const at = placementOf(found);
      if (op.type === "reorder") {
        expect("position", op.from, at, op.to);
      } else {
        expect("where it sits", op.from.parent, at.parent, op.to.parent);
      }
      if (
        found.kind === "unit" &&
        op.to.parent !== COMPANY_KEY &&
        isWithin(draft, op.to.parent, found.node.key)
      ) {
        return gone("The destination is now inside the unit being moved.");
      }
      const slot = expectPlacementSlot(op.to, found.kind, found.node.key);
      if (slot) return slot;
      if (op.type === "move") {
        const ref = found.kind === "seat" ? found.node.data.unit : undefined;
        expect("unit reference", op.unitRef, typeof ref === "string" ? ref : undefined);
        for (const clear of op.clearLeads) {
          const unit = locate(draft, clear.unit);
          expect(
            "lead",
            clear.before,
            unit?.kind === "unit" ? nonEmpty(unit.node.data.lead) : undefined,
            undefined,
          );
        }
      }
      return finish();
    }

    case "updateSeat":
    case "updateUnit": {
      const found = locate(draft, op.target);
      const kind = op.type === "updateSeat" ? "seat" : "unit";
      if (found?.kind !== kind) return gone("It is no longer in the organization.");
      for (const change of op.changes) {
        expect(
          fieldName(change.path),
          change.before,
          getPath(found.node.data, change.path),
          change.after,
        );
      }
      if (op.type === "updateSeat") expectAccessLevels(op.accessLevels);
      return finish();
    }

    case "setLead": {
      const found = locate(draft, op.target);
      if (found?.kind !== "unit") return gone("The unit is no longer in the organization.");
      expect("lead", op.before, nonEmpty(found.node.data.lead), op.after);
      return finish();
    }

    case "setManages": {
      const found = locate(draft, op.target);
      if (found?.kind !== "seat") return gone("The seat is no longer in the organization.");
      expect("manages", op.before, nonEmptyList(found.node.data.manages), op.after);
      return finish();
    }

    case "changeKind": {
      const found = locate(draft, op.target);
      if (found?.kind !== "seat") return gone("The seat is no longer in the organization.");
      expect("kind", op.before, found.node.data.kind, op.after);
      expect("fields the new kind removes", op.stripped, forbiddenFor(found.node.data, op.after));
      expectRouteTo(op.routeTo);
      return finish();
    }

    case "setScheduleEnabled": {
      const found = locate(draft, op.target);
      if (!found) return gone("It is no longer in the organization.");
      const schedule = scheduleOf(found.node.data, op.schedule);
      if (!schedule) return gone(`The schedule ${op.schedule} is no longer there.`);
      expect(`schedule ${op.schedule}`, op.before, schedule.enabled, op.after);
      return finish();
    }

    case "setDatadogRouteTo": {
      if (!hasBlock(draft.company, DATADOG_BLOCK)) return gone("Datadog is no longer connected.");
      expectRouteTo(op.change);
      return finish();
    }

    case "updateCompany": {
      for (const change of op.changes) {
        expect(
          fieldName(change.path),
          change.before,
          getPath(draft.company, change.path),
          change.after,
        );
      }
      return finish();
    }

    case "applyTemplate": {
      if (draft.roles.length > 0 || draft.units.length > 0) {
        conflicts.push({
          subject: "the organization",
          base: "empty",
          theirs: "has seats or units",
        });
      }
      return finish();
    }
  }
}

// ---------------------------------------------------------------------------
// Apply
// ---------------------------------------------------------------------------

/** An operation was applied to a draft its preconditions do not hold on. */
export class ApplyError extends Error {
  readonly outcome: Outcome;
  constructor(op: Operation, outcome: Outcome) {
    super(
      `${op.type} does not apply: ${
        outcome.kind === "gone"
          ? outcome.reason
          : outcome.kind === "conflict"
            ? outcome.conflicts.map((c) => c.subject).join(", ")
            : ""
      }`,
    );
    this.name = "ApplyError";
    this.outcome = outcome;
  }
}

const NO_EFFECTS: ApplyReport = { cleared: [], followed: [], stripped: [] };

/**
 * Applies an operation whose preconditions hold, and says what it did to
 * anything beyond its target. Throws [ApplyError] when they do not hold:
 * applying over a changed value is never an option this function offers.
 */
export function apply(draft: Draft, op: Operation): { draft: Draft; report: ApplyReport } {
  const outcome = evaluate(draft, op);
  if (outcome.kind !== "applies") throw new ApplyError(op, outcome);

  switch (op.type) {
    case "addUnit":
      return {
        draft: attach(
          draft,
          op.placement,
          { key: op.key, data: cloneJson(op.data), roles: [], children: [] },
          "unit",
        ),
        report: NO_EFFECTS,
      };

    case "addSeat":
      return {
        draft: attach(draft, op.placement, { key: op.key, data: cloneJson(op.data) }, "seat"),
        report: NO_EFFECTS,
      };

    case "remove": {
      const found = locate(draft, op.target)!;
      const removedSeatNames: string[] = [];
      const removedUnitNames: string[] = [];
      if (found.kind === "seat") {
        removedSeatNames.push(found.node.data.name);
      } else {
        for (const key of subtreeKeys(found.node)) {
          const inner = locate(draft, key)!;
          (inner.kind === "seat" ? removedSeatNames : removedUnitNames).push(inner.node.data.name);
        }
      }
      let next = detach(draft, op.target)!.draft;
      if (found.kind === "unit" && op.placedSeats === "remove") {
        for (const placed of op.placed) {
          const seat = locate(next, placed.key);
          if (seat) {
            removedSeatNames.push(seat.node.data.name);
            next = detach(next, placed.key)!.draft;
          }
        }
      }
      const cleared: ReferenceEffect[] = [];
      next = clearReferences(next, new Set(removedSeatNames), new Set(removedUnitNames), cleared);
      for (const change of op.accessLevels) {
        next = { ...next, company: withAccessLevel(next.company, change.handle, undefined) };
        cleared.push({ kind: "gitlab_access_level", holder: COMPANY_KEY, from: change.handle });
      }
      if (op.routeTo) next = { ...next, company: withRouteTo(next.company, op.routeTo.after) };
      return { draft: next, report: { cleared, followed: [], stripped: [] } };
    }

    case "renameSeat": {
      let next = updateSeatData(draft, op.target, (data) => {
        const renamed: ConfigRole = { ...data, name: op.after };
        if (op.pin !== undefined && (data.handle === undefined || data.handle === ""))
          renamed.handle = op.pin;
        return renamed;
      });
      const cleared: ReferenceEffect[] = [];
      for (const change of op.accessLevels) {
        next = { ...next, company: withAccessLevel(next.company, change.handle, undefined) };
        cleared.push({ kind: "gitlab_access_level", holder: COMPANY_KEY, from: change.handle });
      }
      const followed: ReferenceEffect[] = [];
      const others = [...allSeats(next)].some(
        ({ seat }) => seat.key !== op.target && seat.data.name === op.before,
      );
      if (!others) next = followSeatName(next, op.before, op.after, followed);
      return { draft: next, report: { cleared, followed, stripped: [] } };
    }

    case "renameUnit": {
      let next = updateUnitData(draft, op.target, (data) => ({ ...data, name: op.after }));
      const followed: ReferenceEffect[] = [];
      const others = [...allUnits(next)].some(
        ({ unit }) => unit.key !== op.target && unit.data.name === op.before,
      );
      if (!others) next = followUnitName(next, op.before, op.after, followed);
      return { draft: next, report: { cleared: [], followed, stripped: [] } };
    }

    case "move":
    case "reorder": {
      const detached = detach(draft, op.target)!;
      let node = detached.node.node;
      const cleared: ReferenceEffect[] = [];
      if (op.type === "move" && op.unitRef !== undefined && detached.node.kind === "seat") {
        const { unit: _unit, ...data } = (node as DraftSeat).data;
        node = { key: node.key, data: data as ConfigRole };
        cleared.push({ kind: "unit", holder: op.target, from: op.unitRef });
      }
      let next = attach(detached.draft, op.to, node, detached.node.kind);
      if (op.type === "move") {
        for (const clear of op.clearLeads) {
          next = updateUnitData(next, clear.unit, (data) => {
            const { lead: _lead, ...rest } = data;
            return rest as ConfigUnit;
          });
          cleared.push({ kind: "lead", holder: clear.unit, from: clear.before });
        }
      }
      return { draft: next, report: { cleared, followed: [], stripped: [] } };
    }

    case "updateSeat": {
      let next = updateSeatData(draft, op.target, (data) =>
        op.changes.reduce((acc, c) => setPath(acc, c.path, cloneJson(c.after)) as ConfigRole, data),
      );
      for (const change of op.accessLevels) {
        next = { ...next, company: withAccessLevel(next.company, change.handle, change.after) };
      }
      return { draft: next, report: NO_EFFECTS };
    }

    case "updateUnit":
      return {
        draft: updateUnitData(draft, op.target, (data) =>
          op.changes.reduce(
            (acc, c) => setPath(acc, c.path, cloneJson(c.after)) as ConfigUnit,
            data,
          ),
        ),
        report: NO_EFFECTS,
      };

    case "setLead":
      return {
        draft: updateUnitData(
          draft,
          op.target,
          (data) => setPath(data, ["lead"], op.after) as ConfigUnit,
        ),
        report: NO_EFFECTS,
      };

    case "setManages":
      return {
        draft: updateSeatData(
          draft,
          op.target,
          (data) =>
            setPath(
              data,
              ["manages"],
              op.after === undefined ? undefined : [...op.after],
            ) as ConfigRole,
        ),
        report: NO_EFFECTS,
      };

    case "changeKind": {
      let next = updateSeatData(draft, op.target, (data) => {
        let out = data;
        for (const field of op.stripped) out = setPath(out, field.path, undefined) as ConfigRole;
        out = setPath(out, ["kind"], op.after === "human" ? "human" : undefined) as ConfigRole;
        if (op.contact) out = setPath(out, ["contact"], cloneJson(op.contact)) as ConfigRole;
        return out;
      });
      if (op.routeTo) next = { ...next, company: withRouteTo(next.company, op.routeTo.after) };
      return {
        draft: next,
        report: { cleared: [], followed: [], stripped: op.stripped.map((f) => fieldName(f.path)) },
      };
    }

    case "setScheduleEnabled": {
      const update = <T extends ConfigRole | ConfigUnit>(data: T): T => ({
        ...data,
        schedules: (data.schedules ?? []).map((s) =>
          s.name === op.schedule ? { ...s, enabled: op.after } : s,
        ),
      });
      const found = locate(draft, op.target)!;
      return {
        draft:
          found.kind === "seat"
            ? updateSeatData(draft, op.target, update)
            : updateUnitData(draft, op.target, update),
        report: NO_EFFECTS,
      };
    }

    case "setDatadogRouteTo":
      return {
        draft: { ...draft, company: withRouteTo(draft.company, op.change.after) },
        report: NO_EFFECTS,
      };

    case "updateCompany":
      return {
        draft: {
          ...draft,
          company: op.changes.reduce(
            (acc, c) => setPath(acc, c.path, cloneJson(c.after)) as CompanyDocument,
            draft.company,
          ),
        },
        report: NO_EFFECTS,
      };

    case "applyTemplate":
      return {
        draft: {
          company: {
            ...draft.company,
            name: op.charter.name,
            ...(op.charter.mission ? { mission: op.charter.mission } : {}),
          },
          roles: cloneJson(op.roles),
          units: cloneJson(op.units),
        },
        report: NO_EFFECTS,
      };
  }
}

/**
 * Clears the references a removal left naming something else or nothing: a
 * unit's `lead` that named a removed seat, `manages` entries that named a
 * removed seat or unit, and a root seat's `unit:` that named a removed unit.
 *
 * WHAT AN ENTRY NAMED IS DECIDED AS THE ENGINE READS IT, before the removal:
 * a `manages` entry matching both a seat and a unit named the SEAT. So an
 * entry that named a removed seat is cleared even when a unit of that name
 * remains, because leaving it would silently widen it to the whole unit. An
 * entry is kept only when what it named still exists under that name.
 */
function clearReferences(
  draft: Draft,
  seatNames: Set<string>,
  unitNames: Set<string>,
  cleared: ReferenceEffect[],
): Draft {
  const remainingSeats = new Set([...allSeats(draft)].map(({ seat }) => seat.data.name));
  const remainingUnits = new Set([...allUnits(draft)].map(({ unit }) => unit.data.name));
  const namedRemovedSeat = (entry: string) => seatNames.has(entry) && !remainingSeats.has(entry);
  const namedRemovedUnit = (entry: string) =>
    unitNames.has(entry) &&
    !seatNames.has(entry) &&
    !remainingSeats.has(entry) &&
    !remainingUnits.has(entry);
  let next = mapUnitData(draft, (data, key) => {
    const lead = data.lead;
    if (typeof lead !== "string" || !namedRemovedSeat(lead)) return data;
    cleared.push({ kind: "lead", holder: key, from: lead });
    return setPath(data, ["lead"], undefined) as ConfigUnit;
  });
  next = mapSeatData(next, (data, key) => {
    let out = data;
    if (Array.isArray(data.manages)) {
      const kept = data.manages.filter((entry) => {
        const dangling = namedRemovedSeat(entry) || namedRemovedUnit(entry);
        if (dangling) cleared.push({ kind: "manages", holder: key, from: entry });
        return !dangling;
      });
      if (kept.length !== data.manages.length) {
        out = setPath(out, ["manages"], kept.length > 0 ? kept : undefined) as ConfigRole;
      }
    }
    return out;
  });
  const roots = new Set(next.roles.map((s) => s.key));
  next = mapSeatData(next, (data, key) => {
    const ref = data.unit;
    if (
      !roots.has(key) ||
      typeof ref !== "string" ||
      !unitNames.has(ref) ||
      remainingUnits.has(ref)
    )
      return data;
    cleared.push({ kind: "unit", holder: key, from: ref });
    return setPath(data, ["unit"], undefined) as ConfigRole;
  });
  return next;
}

/** Follows a seat rename in every `lead` and `manages` entry that named the seat. */
function followSeatName(
  draft: Draft,
  from: string,
  to: string,
  followed: ReferenceEffect[],
): Draft {
  let next = mapUnitData(draft, (data, key) => {
    if (data.lead !== from) return data;
    followed.push({ kind: "lead", holder: key, from, to });
    return { ...data, lead: to };
  });
  next = mapSeatData(next, (data, key) => {
    if (!Array.isArray(data.manages) || !data.manages.includes(from)) return data;
    followed.push({ kind: "manages", holder: key, from, to });
    return { ...data, manages: data.manages.map((m) => (m === from ? to : m)) };
  });
  return next;
}

/**
 * Follows a unit rename in every root seat's `unit:` and every `manages` entry
 * that named the unit. An entry that also names a seat named the SEAT (a seat
 * name wins over a unit name), so it is not the unit's to follow.
 */
function followUnitName(
  draft: Draft,
  from: string,
  to: string,
  followed: ReferenceEffect[],
): Draft {
  const seatHoldsName = [...allSeats(draft)].some(({ seat }) => seat.data.name === from);
  const roots = new Set(draft.roles.map((s) => s.key));
  return mapSeatData(draft, (data, key) => {
    let out = data;
    if (roots.has(key) && data.unit === from) {
      followed.push({ kind: "unit", holder: key, from, to });
      out = { ...out, unit: to };
    }
    if (!seatHoldsName && Array.isArray(data.manages) && data.manages.includes(from)) {
      followed.push({ kind: "manages", holder: key, from, to });
      out = { ...out, manages: data.manages.map((m) => (m === from ? to : m)) };
    }
    return out;
  });
}

// ---------------------------------------------------------------------------
// Reading an operation
// ---------------------------------------------------------------------------

/**
 * The nodes an operation acts on, the one to focus first. Undo and redo move
 * focus to the first of these that exists afterwards, falling back to the
 * parent a removed or un-added node sat in.
 */
export function touchedKeys(op: Operation): NodeKey[] {
  switch (op.type) {
    case "addUnit":
    case "addSeat":
      return [op.key, op.placement.parent];
    case "remove":
      return [op.target];
    case "move":
    case "reorder":
      return [op.target, op.to.parent, op.from.parent];
    case "setDatadogRouteTo":
    case "updateCompany":
    case "applyTemplate":
      return [COMPANY_KEY];
    default:
      return [op.target];
  }
}

/**
 * One sentence saying what an operation did, for the live region and the
 * operation list. `before` is the draft the operation was applied to, which
 * still holds a removed node's name.
 */
export function describeOperation(op: Operation, before: Draft): string {
  const nameOf = (key: NodeKey): string => {
    if (key === COMPANY_KEY) return "the company";
    const found = locate(before, key);
    return found ? found.node.data.name || "an unnamed node" : "a node no longer in the draft";
  };
  const where = (parent: NodeKey) => (parent === COMPANY_KEY ? "the company" : nameOf(parent));
  switch (op.type) {
    case "addUnit":
      return `Added unit ${op.data.name} to ${where(op.placement.parent)}.`;
    case "addSeat":
      return `Added ${kindOf(op.data)} seat ${op.data.name} to ${where(op.placement.parent)}.`;
    case "remove": {
      const found = locate(before, op.target);
      if (found?.kind === "unit") {
        const seats = subtreeKeys(found.node).filter(
          (k) => locate(before, k)?.kind === "seat",
        ).length;
        return `Removed unit ${found.node.data.name} and ${plural(seats, "seat")} in it.`;
      }
      return `Removed seat ${nameOf(op.target)}.`;
    }
    case "renameSeat":
      return `Renamed seat ${op.before} to ${op.after}.`;
    case "renameUnit":
      return `Renamed unit ${op.before} to ${op.after}.`;
    case "move":
      return `Moved ${nameOf(op.target)} to ${where(op.to.parent)}.`;
    case "reorder":
      return op.to.after === null
        ? `Moved ${nameOf(op.target)} to the top of ${where(op.to.parent)}.`
        : `Moved ${nameOf(op.target)} after ${nameOf(op.to.after)}.`;
    case "updateSeat":
    case "updateUnit": {
      const fields = op.changes.map((c) => fieldName(c.path));
      if (op.type === "updateSeat" && op.accessLevels.length > 0)
        fields.push("GitLab access level");
      return `Edited ${nameOf(op.target)}: ${fields.join(", ")}.`;
    }
    case "setLead":
      return op.after === undefined
        ? `Cleared the lead of ${nameOf(op.target)}.`
        : `Set the lead of ${nameOf(op.target)} to ${op.after}.`;
    case "setManages":
      return `Changed whom ${nameOf(op.target)} manages.`;
    case "changeKind":
      return `Changed ${nameOf(op.target)} to a ${op.after} seat.`;
    case "setScheduleEnabled":
      return `${op.after ? "Enabled" : "Disabled"} schedule ${op.schedule} on ${nameOf(op.target)}.`;
    case "setDatadogRouteTo":
      return op.change.after === undefined
        ? "Cleared the Datadog fallback seat."
        : `Set the Datadog fallback seat to ${op.change.after}.`;
    case "updateCompany":
      return `Edited the charter: ${op.changes.map((c) => fieldName(c.path)).join(", ")}.`;
    case "applyTemplate":
      return `Started ${op.charter.name} from a template.`;
  }
}

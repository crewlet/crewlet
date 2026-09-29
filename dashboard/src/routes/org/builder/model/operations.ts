/**
 * Builder operations: what an edit IS, as data that replays.
 *
 * AN OPERATION IS RECORDED, THEN APPLIED, AND CAN BE REPLAYED ANYWHERE. The UI
 * builds an [Intent] (what the operator asked for, with any new node's key
 * minted in the event handler) and [record]s it against the current draft.
 * Recording is where every PRECONDITION is captured: the value each changed
 * field held, the whole node a removal deletes, the parent a move takes a node
 * out of. The recorded operation is
 * JSON, carries no function and no reference into the draft, and is what the
 * log keeps, persists and replays.
 *
 * [evaluate] asks an operation whether it still applies to a draft, and says
 * one of three things. It applies. Its target is GONE (the node, the parent it
 * moves into, the schedule it toggles). Or it CONFLICTS: a precondition value
 * differs — or the address a node it creates would take is held — which after
 * a rebase means somebody else changed the same thing,
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
 * EVERY REFERENCE IS AN ADDRESS, AND WHAT FOLLOWS FROM ONE IS DECIDED AT APPLY
 * TIME. A unit's `lead` names a seat by its handle, and a `manages` entry a
 * seat by its handle or a unit by its key, exactly as the org chart stores
 * them. A NAME is prose: renaming a seat or a unit changes what a person reads
 * and nothing any reference resolves. When an operation removes a node, or
 * gives a node a different address, the references that
 * named it are cleared or followed as the operation applies, against the draft
 * it applies to, and reported — so a reference somebody added upstream is
 * handled too. GitLab access levels are keyed by handle as well, and the
 * entries an operation clears are carried, with their values, as
 * preconditions.
 *
 * ONE ADDRESS, ONE NODE. The chart refuses a second seat under a handle it
 * holds and a second unit under a key, so recording refuses an add, or an
 * address change, onto an address another node
 * of the draft holds: a draft that held two would be a save that cannot land.
 *
 * The seat rules mirrored here are the ones an operation's own meaning needs
 * and nothing more: which fields a kind forbids (a kind change strips them, and
 * the engine's organization refuses a seat that keeps them). The engine remains
 * the validator of the result: the save's writes are decided there, each
 * refusal naming the operation or the object it is about.
 */

import type { CompanyDocument } from "~/protocol/index.ts";
import { plural } from "~/lib/format.ts";
import { cloneJson, getPath, isRecord, jsonEqual, setPath } from "./json.ts";
import { COMPANY_KEY, isMintedKey, type NodeKey } from "./keys.ts";
import {
  addressIndex,
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
  type SeatData,
  type UnitData,
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
 *
 * VERSION 2 is the org chart's vocabulary: a seat's data is the chart's seat
 * with its runtime half under `runtime`, every reference an address, and a
 * name only prose. A version-1 log named seats and units by name and wrote the
 * company document's shapes, so it is not replayed onto a chart at all.
 */
export const OPERATIONS_VERSION = 2;

/** Who holds a seat. An absent or unrecognised `kind` is an agent seat, as the engine reads it. */
export type SeatKind = "agent" | "human";

/** One field of an entity's own data, and the value it held when the edit was recorded. */
export interface FieldChange {
  /** Keys from the entity's data down to the field: `["runtime", "github", "tier"]`. */
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

/** A node as it stood when its removal was recorded, or as the chart holds one an add meets. */
export interface NodeSnapshot {
  readonly key: NodeKey;
  /** The node's JSON: a seat's data, or a unit's with its `roles` and `children`. */
  readonly json: unknown;
}

/** The starting shapes `applyTemplate` can carry (see `templates.ts`). "empty" is the charter alone. */
export type TemplateId = "empty" | "new_company" | "established_company";

export interface AddUnit {
  readonly type: "addUnit";
  readonly key: NodeKey;
  readonly placement: Placement;
  /** The unit's own fields, its chosen `key` among them. */
  readonly data: UnitData;
}

export interface AddSeat {
  readonly type: "addSeat";
  readonly key: NodeKey;
  readonly placement: Placement;
  /** The seat's own fields, its chosen `handle` among them. */
  readonly data: SeatData;
}

export interface Remove {
  readonly type: "remove";
  readonly target: NodeKey;
  /** The node and, for a unit, its whole subtree, as it stood. */
  readonly snapshot: NodeSnapshot;
  /** The access level entries of every removed seat, cleared. */
  readonly accessLevels: readonly AccessLevelChange[];
  /** A replacement Datadog fallback, when a removed seat was it. */
  readonly routeTo?: RouteToChange;
}

/** A seat's display NAME changed. Nothing resolves a seat by it, so nothing follows. */
export interface RenameSeat {
  readonly type: "renameSeat";
  readonly target: NodeKey;
  readonly before: string;
  readonly after: string;
}

/** A unit's display NAME changed. Nothing resolves a unit by it, so nothing follows. */
export interface RenameUnit {
  readonly type: "renameUnit";
  readonly target: NodeKey;
  readonly before: string;
  readonly after: string;
}

/** A node moved to another parent: a batch's `move`, the one structural change of where. */
export interface Move {
  readonly type: "move";
  readonly target: NodeKey;
  readonly from: Placement;
  readonly to: Placement;
  /** Units whose lead the operator chose to clear as the seat moves. */
  readonly clearLeads: readonly LeadClear[];
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
  /** Seat HANDLES, as the chart stores a lead. Omitted for none. */
  readonly before?: string;
  readonly after?: string;
}

export interface SetManages {
  readonly type: "setManages";
  readonly target: NodeKey;
  /** The authored list: seat handles and unit keys. Omitted for none. */
  readonly before?: readonly string[];
  readonly after?: readonly string[];
}

/**
 * A seat's kind set, and every field that kind forbids stripped.
 *
 * `before` may already be `after`: a seat whose kind is right but still holds
 * a field the kind forbids is brought into line by the same operation, which
 * is what a save that set the kind and failed before it stripped the fields
 * leaves behind.
 */
export interface ChangeKind {
  readonly type: "changeKind";
  readonly target: NodeKey;
  /** The `kind` as written; omitted when unwritten. */
  readonly before?: string;
  readonly after: SeatKind;
  /** Every field the new kind forbids, as it stood. Stripped. */
  readonly stripped: readonly FieldChange[];
  /** A human seat's contact identity (`runtime.contact`), when becoming human. */
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

/**
 * The operations a node editor's form can make of one node, and nothing else:
 * no structure (add, remove, move), no kind change and no template.
 * Each keeps the precondition and reference rules of its own type inside an
 * [Edit].
 */
export type EditPart =
  | RenameSeat
  | RenameUnit
  | UpdateSeat
  | UpdateUnit
  | SetLead
  | SetManages
  | SetScheduleEnabled
  | UpdateCompany;

/** The operation types an [Edit] may hold. */
export const EDIT_PART_TYPES: ReadonlySet<OperationType> = new Set<EditPart["type"]>([
  "renameSeat",
  "renameUnit",
  "updateSeat",
  "updateUnit",
  "setLead",
  "setManages",
  "setScheduleEnabled",
  "updateCompany",
]);

/**
 * Several changes to ONE node, made together in its editor and applied as one
 * operation.
 *
 * ONE STEP, ALL OR NOTHING. A person who renames a seat, rewrites its goal and
 * changes whom it manages pressed Apply once, so Undo takes all of it back at
 * once and the live region says it once. Recorded as separate operations, a
 * refusal of the third (a field its own action owns) would leave the first two
 * applied: a form half saved into the draft, which is not what anybody asked
 * for. So the parts are recorded in order against the draft the earlier parts
 * produce, and any refusal refuses the whole edit.
 *
 * EACH PART KEEPS ITS OWN RULES. A new seat's changed handle still follows the
 * references that named it, a seat update still refuses a field another
 * operation owns, and each part records its own preconditions. A rebase holds the edit as one
 * choice, the same as an update of several fields: keeping theirs drops the
 * edit, keeping mine records its parts again over their values.
 */
export interface Edit {
  readonly type: "edit";
  readonly target: NodeKey;
  /** At least two parts; recording a single change yields that change's own operation. */
  readonly ops: readonly EditPart[];
}

/** Every recorded operation. */
export type Operation =
  | AddUnit
  | AddSeat
  | Remove
  | RenameSeat
  | RenameUnit
  | Move
  | UpdateSeat
  | UpdateUnit
  | SetLead
  | SetManages
  | ChangeKind
  | SetScheduleEnabled
  | SetDatadogRouteTo
  | UpdateCompany
  | ApplyTemplate
  | Edit;

export type OperationType = Operation["type"];

/** One field to set, as an editor emits it: only fields whose value differs from the form's start. */
export interface FieldSet {
  readonly path: readonly string[];
  /** Omitted to remove the field. */
  readonly value?: unknown;
}

/** What an operator asked for, before the draft supplies the preconditions. */
export type Intent =
  | SingleIntent
  | {
      readonly type: "edit";
      readonly target: NodeKey;
      /** In the order they apply: a rename first, so a later part names the node as it now is. */
      readonly intents: readonly EditPartIntent[];
    };

/** The intent of one change an editor's form can make, as an [Edit] groups them. */
export type EditPartIntent = Extract<SingleIntent, { readonly type: EditPart["type"] }>;

/** Every intent but an edit's. */
type SingleIntent =
  | {
      readonly type: "addUnit";
      readonly key: NodeKey;
      readonly placement: Placement;
      readonly data: UnitData;
    }
  | {
      readonly type: "addSeat";
      readonly key: NodeKey;
      readonly placement: Placement;
      readonly data: SeatData;
    }
  | {
      readonly type: "remove";
      readonly target: NodeKey;
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

/** Why an intent could not be recorded against the draft. */
export type RecordRefusal =
  | "missing_target"
  | "wrong_kind"
  | "missing_parent"
  | "key_in_use"
  | "address_in_use"
  | "no_address"
  | "not_minted"
  | "into_itself"
  | "forbidden_field"
  | "no_schedule"
  | "no_datadog"
  | "no_gitlab"
  | "not_empty"
  | "not_editable"
  | "no_change";

export type Recorded =
  | { readonly ok: true; readonly op: Operation }
  | { readonly ok: false; readonly refusal: RecordRefusal; readonly message: string };

/** What an operation's preconditions say about a draft. */
export type Outcome =
  | { readonly kind: "applies" }
  | { readonly kind: "gone"; readonly reason: string }
  | { readonly kind: "conflict"; readonly conflicts: readonly Conflict[] };

/**
 * What a conflict's values ARE, for a view that shows them to a person.
 *
 * Most conflicts are about a field, and their values are that field's values
 * as the chart writes them. The rest carry the builder's own structures,
 * which are not something to print: node keys never leave the builder, a
 * removal's snapshot is a whole node (credential references and all), and a
 * kind change records the very fields it strips. So each of those says which
 * structure it holds, and the view names what it is about instead of dumping
 * it.
 *
 * - `parent`: the node key of the unit a node sits in ([COMPANY_KEY] at the root).
 * - `snapshot`: a whole node — `theirs` the node the draft holds now, `mine`
 *   absent for a removal and, for an add whose address the chart already
 *   holds, the node this draft created there. An `address` conflict (below)
 *   has this shape.
 * - `fields`: [FieldChange] entries of the fields a kind change removes.
 */
export type ConflictShape = "parent" | "snapshot" | "fields";

/** One precondition that no longer holds, with every value a person needs to choose. */
export interface Conflict {
  /** What it is about, as a short phrase: "goal", "placement", "the whole seat". */
  readonly subject: string;
  /** What the three values are; absent for a field's own values. */
  readonly shape?: ConflictShape;
  /**
   * Set when the conflict is an ADDRESS another node now holds: an add, or a
   * created node's chosen handle or key. "Keep mine" of an add writes this
   * draft's node onto the one holding it (`history.rebase`); of an address
   * change it has nothing to write, the address being taken.
   */
  readonly address?: string;
  /** The value when this operation was recorded. */
  readonly base?: unknown;
  /** The value in the draft now. */
  readonly theirs?: unknown;
  /** The value this operation writes. */
  readonly mine?: unknown;
}

/** A reference an operation cleared or followed, for the announcement and the review. */
export interface ReferenceEffect {
  readonly kind: "lead" | "manages" | "gitlab_access_level" | "datadog_route_to";
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
 * them (`org.Role.humanForbidden`), with the ones that hold credentials
 * marked: a credential reference a kind change strips is gone once the change
 * is saved, and its sealed value with it.
 *
 * `runtime.github` comes last because a different rule refuses it: a seat's
 * own GitHub App is the bot identity an agent acts as, and a person acts as
 * their own login (`contact.github_login`). A kind change that kept the block
 * would leave a working-looking app on a seat nothing ever runs.
 */
export const HUMAN_FORBIDDEN: readonly {
  readonly path: readonly string[];
  readonly credential: boolean;
}[] = [
  { path: ["runtime", "llm"], credential: false },
  { path: ["runtime", "llm_review"], credential: false },
  { path: ["runtime", "llm_subagent"], credential: false },
  { path: ["runtime", "llm_auxiliary"], credential: false },
  { path: ["runtime", "llm_judge"], credential: false },
  { path: ["runtime", "llm_sandbox"], credential: false },
  { path: ["runtime", "sandbox"], credential: true },
  { path: ["runtime", "token_budget"], credential: false },
  { path: ["runtime", "workers"], credential: false },
  { path: ["runtime", "learning_enabled"], credential: false },
  { path: ["runtime", "schedules"], credential: false },
  { path: ["runtime", "slack"], credential: true },
  { path: ["runtime", "mattermost"], credential: true },
  { path: ["project"], credential: false },
  { path: ["space"], credential: false },
  { path: ["runtime", "mcp_env"], credential: true },
  { path: ["behavioral_guidelines"], credential: false },
  { path: ["runtime", "github"], credential: true },
];

/** The fields an agent seat must not carry (`org.Role.Validate`). */
export const AGENT_FORBIDDEN: readonly {
  readonly path: readonly string[];
  readonly credential: boolean;
}[] = [
  { path: ["runtime", "contact"], credential: false },
  { path: ["runtime", "availability"], credential: false },
];

/** Who holds a seat, as the engine reads its `kind`. */
export function kindOf(data: SeatData): SeatKind {
  return data.kind === "human" ? "human" : "agent";
}

/**
 * The name of a field path as a person reads it: `runtime.slack` is the
 * seat's `slack` block, because "runtime" is where the chart keeps it and not a
 * word anybody wrote.
 */
export function fieldName(path: readonly string[]): string {
  return (path[0] === "runtime" ? path.slice(1) : path).join(".");
}

/** Whether a field path holds a credential a kind change would strip. */
export function isCredentialField(path: readonly string[]): boolean {
  return [...HUMAN_FORBIDDEN, ...AGENT_FORBIDDEN].some(
    (f) => f.credential && jsonEqual(f.path, path),
  );
}

/** The fields a seat holds that `kind` forbids, with their values. */
function forbiddenFor(data: SeatData, kind: SeatKind): FieldChange[] {
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
 * owns it and carries what that change means: a rename is its own gesture in
 * the log, a kind change strips forbidden fields, `manages` is compared whole,
 * and a schedule is only ever toggled here. The addresses the chart keeps
 * beside a seat (`former_handles`) are the chart's to write.
 *
 * `handle` IS WRITABLE, on every seat. It is the address every reference and
 * every chart route names the seat by, and changing it on a seat the chart
 * holds is the chart's own RENAME: the seat keeps its identity (the address it
 * was created under, which its memory, its mailbox and whoever is bound to it
 * are keyed on), its old handle goes on resolving to it, and the references
 * the draft holds follow at apply time.
 */
const SEAT_OWNED = new Set(["name", "kind", "manages", "former_handles"]);

/** The unit fields `updateUnit` may not write. Its `key` is writable, as a seat's handle is. */
const UNIT_OWNED = new Set(["name", "lead", "former_keys"]);

/** Whether a path is a node's schedules, which only `setScheduleEnabled` writes. */
const isSchedules = (path: readonly string[]) => path[0] === "runtime" && path[1] === "schedules";

/** Whether `updateSeat` may write a field of a seat. */
function seatFieldWritable(path: readonly string[]): boolean {
  const head = path[0];
  return head !== undefined && !SEAT_OWNED.has(head) && !isSchedules(path);
}

/** Whether `updateUnit` may write a field of a unit. */
function unitFieldWritable(path: readonly string[]): boolean {
  const head = path[0];
  return head !== undefined && !UNIT_OWNED.has(head) && !isSchedules(path);
}

/** Whether `updateCompany` may write a field: the charter, and nothing else. */
function charterFieldWritable(path: readonly string[]): boolean {
  return path.length === 1 && (CHARTER_FIELDS as readonly string[]).includes(path[0]!);
}

/**
 * Why an operation could never have been recorded, whatever the draft: a
 * field another operation owns, anything outside the charter in a company
 * edit, or a created node without a minted key. `null` when it is well formed.
 *
 * [record] refuses each of these as it builds an operation. A log read back
 * from storage is held to the same rules before it replays, because
 * [evaluate] checks only what a draft can say about an operation, and a stored
 * log that changed a seat's kind through `updateSeat` would otherwise skip the
 * fields a kind change strips.
 */
export function malformedReason(op: Operation): string | null {
  switch (op.type) {
    case "addUnit":
    case "addSeat":
      return isMintedKey(op.key) ? null : "a created node has no minted key";
    case "updateSeat":
      return op.changes.every((c) => seatFieldWritable(c.path))
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
    case "applyTemplate": {
      if (!templateKeys(op.roles, op.units).every(isMintedKey)) {
        return "a template node has no minted key";
      }
      // Its nodes are applied as adds, and an add refuses an address another
      // node holds, so a template naming one twice could never have applied.
      const draft = { company: {}, roles: op.roles, units: op.units };
      const handles = [...allSeats(draft)].map(({ seat }) => seat.data.handle);
      const keys = [...allUnits(draft)].map(({ unit }) => unit.data.key);
      const unique = (list: string[]) =>
        list.every((a) => typeof a === "string" && a !== "") && new Set(list).size === list.length;
      return unique(handles) && unique(keys) ? null : "a template names an address twice";
    }
    case "edit": {
      // Recording yields a single change as its own operation, so an edit of
      // fewer than two parts is not something this build writes.
      if (op.ops.length < 2) return "an edit groups fewer than two changes";
      for (const part of op.ops) {
        if (!EDIT_PART_TYPES.has(part.type)) return "an edit holds a change no editor makes";
        if (partTarget(part) !== op.target) return "an edit changes a node other than its own";
        const reason = malformedReason(part);
        if (reason !== null) return reason;
      }
      return null;
    }
    default:
      return null;
  }
}

/** The node one part of an edit changes: its target, or the company for a charter edit. */
function partTarget(part: { readonly type: string; readonly target?: NodeKey }): NodeKey {
  return part.type === "updateCompany" ? COMPANY_KEY : (part.target ?? COMPANY_KEY);
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

/** The kind a seat writes, or `undefined` when it writes none (read the same way everywhere). */
function writtenKind(data: SeatData): string | undefined {
  return typeof data.kind === "string" ? data.kind : undefined;
}

/** The access level entry for a handle in the draft, when there is one. */
function accessLevel(draft: Draft, handle: string): string | undefined {
  const value = getPath(draft.company, [...GITLAB_ACCESS_LEVELS, handle]);
  return typeof value === "string" ? value : undefined;
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

/** The JSON of a located node: a seat's data, or a unit's with what it holds. */
export function nodeJson(found: Located): unknown {
  return found.kind === "seat" ? found.node.data : unitJson(found.node);
}

function unitJson(unit: DraftUnit): Record<string, unknown> {
  const out: Record<string, unknown> = { ...unit.data };
  if (unit.roles.length > 0) out.roles = unit.roles.map((s) => s.data);
  if (unit.children.length > 0) out.children = unit.children.map(unitJson);
  return out;
}

function routeToChange(draft: Draft, after: string | undefined): RouteToChange | undefined {
  if (after === undefined) return undefined;
  const before = getPath(draft.company, DATADOG_ROUTE_TO);
  return { before: typeof before === "string" ? before : undefined, after };
}

/**
 * The node of a draft holding an address: the seat whose handle it is, or the
 * unit whose key it is. Seats and units are addressed apart (the chart keeps a
 * seat's handle and a unit's key in two namespaces), so a unit keyed `sam`
 * does not hold a seat's `sam`.
 */
export function holderOf(
  draft: Draft,
  kind: "seat" | "unit",
  address: string,
): Located | undefined {
  if (kind === "seat") {
    for (const { seat } of allSeats(draft)) {
      if (seat.data.handle === address) return locate(draft, seat.key);
    }
    return undefined;
  }
  for (const { unit } of allUnits(draft)) {
    if (unit.data.key === address) return locate(draft, unit.key);
  }
  return undefined;
}

/** A node's address as its data carries it, or `""` for none. */
function addressIn(data: SeatData | UnitData, kind: "seat" | "unit"): string {
  const value = kind === "seat" ? (data as SeatData).handle : (data as UnitData).key;
  return typeof value === "string" ? value : "";
}

/** How a person reads an address kind. */
const addressWord = (kind: "seat" | "unit") => (kind === "seat" ? "handle" : "key");

/**
 * Records an intent against the draft: fills in every precondition from the
 * draft as it stands, or refuses with the reason and a sentence to show.
 */
export function record(draft: Draft, intent: Intent): Recorded {
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
  /** Why `address` cannot be given to a node of `kind` other than `self`, or `undefined`. */
  const addressRefusal = (
    kind: "seat" | "unit",
    address: unknown,
    self?: NodeKey,
  ): Recorded | undefined => {
    if (typeof address !== "string" || address.trim() === "") {
      return refuse(
        "no_address",
        kind === "seat"
          ? "A seat needs a handle: it is the address the chart and every reference name it by."
          : "A unit needs a key: it is the address the chart and every reference name it by.",
      );
    }
    const holder = holderOf(draft, kind, address);
    if (holder && holder.node.key !== self) {
      return refuse(
        "address_in_use",
        `The ${addressWord(kind)} ${address} already names ${kind === "seat" ? "a seat" : "a unit"} in this draft (${holder.node.data.name || address}).`,
      );
    }
    return undefined;
  };

  switch (intent.type) {
    case "addUnit":
    case "addSeat": {
      if (!isMintedKey(intent.key)) {
        return refuse("not_minted", "A new node needs a key minted for it.");
      }
      if (locate(draft, intent.key)) return refuse("key_in_use", "That key already names a node.");
      const kind = intent.type === "addUnit" ? "unit" : "seat";
      if (!siblingsAt(draft, intent.placement.parent, kind)) {
        return refuse("missing_parent", "The destination is no longer in the draft.");
      }
      const refused = addressRefusal(kind, addressIn(intent.data, kind));
      if (refused) return refused;
      const placement: Placement = { parent: intent.placement.parent };
      if (intent.type === "addUnit") {
        const {
          roles: _roles,
          children: _children,
          ...data
        } = cloneJson(intent.data as Record<string, unknown>);
        return recorded({ type: "addUnit", key: intent.key, placement, data: data as UnitData });
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
      const removedSeats: DraftSeat[] =
        found.kind === "seat"
          ? [found.node]
          : [...allSeats(draft)]
              .filter(({ parent }) => isWithin(draft, parent, found.node.key))
              .map(({ seat }) => seat);
      const accessLevels: AccessLevelChange[] = [];
      for (const seat of removedSeats) {
        const level = accessLevel(draft, seat.data.handle);
        if (level !== undefined) accessLevels.push({ handle: seat.data.handle, before: level });
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
        accessLevels,
        ...(routeTo ? { routeTo } : {}),
      });
    }

    case "renameSeat":
    case "renameUnit": {
      const found = intent.type === "renameSeat" ? seatAt(intent.target) : unitAt(intent.target);
      if (!found) return missing(intent.target);
      const before = found.node.data.name;
      const after = intent.name.trim();
      if (after === before) return refuse("no_change", "The name is unchanged.");
      return recorded({ type: intent.type, target: intent.target, before, after });
    }

    case "move": {
      const found = locate(draft, intent.target);
      if (!found) return refuse("missing_target", "That node is no longer in the draft.");
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
      if (!siblingsAt(draft, intent.to.parent, found.kind)) {
        return refuse("missing_parent", "The destination is no longer in the draft.");
      }
      const from = placementOf(found);
      const to: Placement = { parent: intent.to.parent };
      if (to.parent === from.parent) return refuse("no_change", "It already sits there.");
      const clearLeads: LeadClear[] = [];
      for (const key of intent.clearLeads ?? []) {
        const unit = unitAt(key);
        if (!unit) return missing(key);
        const lead = nonEmpty(unit.node.data.lead);
        if (lead !== undefined) clearLeads.push({ unit: key, before: lead });
      }
      return recorded({ type: "move", target: intent.target, from, to, clearLeads });
    }

    case "updateSeat": {
      const found = seatAt(intent.target);
      if (!found) return missing(intent.target);
      const changes: FieldChange[] = [];
      const accessLevels: AccessLevelChange[] = [];
      const current = found.node.data.handle;
      let handle = current;
      for (const set of intent.set) {
        // A FIELD SET TO WHAT IT HOLDS IS NO CHANGE, whoever owns it: a seat's
        // own handle stated back is not an attempt to move it.
        const before = getPath(found.node.data, set.path);
        if (jsonEqual(before, set.value)) continue;
        if (!seatFieldWritable(set.path)) {
          return refuse(
            "forbidden_field",
            `The ${fieldName(set.path)} field is changed by its own action.`,
          );
        }
        if (set.path.length === 1 && set.path[0] === "handle" && set.value !== current) {
          const refused = addressRefusal("seat", set.value, found.node.key);
          if (refused) return refused;
          handle = set.value as string;
        }
        changes.push(fieldChange(set.path, before, set.value));
      }
      if (typeof intent.accessLevel === "string" && !hasBlock(draft.company, GITLAB_PROVISIONING)) {
        return refuse(
          "no_gitlab",
          "GitLab provisioning is not connected. Connect it from Integrations before setting an access level.",
        );
      }
      // A SEAT'S LEVEL TRAVELS WITH ITS HANDLE. The level is keyed by the
      // handle, and the operator changed what the seat is addressed as, not
      // what it may do: an entry left under the old handle would grant its
      // level to the next seat given that handle.
      const carried = handle !== current ? accessLevel(draft, current) : undefined;
      if (carried !== undefined) accessLevels.push({ handle: current, before: carried });
      const wanted = intent.accessLevel === undefined ? carried : (intent.accessLevel ?? undefined);
      const held = accessLevel(draft, handle);
      if (wanted !== held && (intent.accessLevel !== undefined || carried !== undefined)) {
        accessLevels.push({
          handle,
          ...(held !== undefined ? { before: held } : {}),
          ...(wanted !== undefined ? { after: wanted } : {}),
        });
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
        const before = getPath(found.node.data, set.path);
        if (jsonEqual(before, set.value)) continue;
        if (!unitFieldWritable(set.path)) {
          return refuse(
            "forbidden_field",
            `The ${fieldName(set.path)} field is changed by its own action.`,
          );
        }
        if (set.path.length === 1 && set.path[0] === "key" && set.value !== found.node.data.key) {
          const refused = addressRefusal("unit", set.value, found.node.key);
          if (refused) return refused;
        }
        changes.push(fieldChange(set.path, before, set.value));
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
      const stripped = forbiddenFor(found.node.data, intent.kind);
      if (kindOf(found.node.data) === intent.kind && stripped.length === 0)
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
        ...(writtenKind(found.node.data) !== undefined
          ? { before: writtenKind(found.node.data) }
          : {}),
        after: intent.kind,
        stripped,
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

    case "edit": {
      const parts: EditPart[] = [];
      let at = draft;
      for (const part of intent.intents) {
        if (!EDIT_PART_TYPES.has(part.type) || partTarget(part) !== intent.target) {
          return refuse(
            "not_editable",
            "An edit changes the fields of one node. Add, remove, move or change the kind of a node on its own.",
          );
        }
        const result = record(at, part);
        if (!result.ok) {
          // A field the form left as it was is no change, not a failure:
          // the rest of the edit still stands.
          if (result.refusal === "no_change") continue;
          return result;
        }
        parts.push(result.op as EditPart);
        at = apply(at, result.op).draft;
      }
      if (parts.length === 0) return refuse("no_change", "Nothing changed.");
      if (parts.length === 1) return recorded(parts[0]!);
      return recorded({ type: "edit", target: intent.target, ops: parts });
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

/** A node's schedule by name, from the runtime half where the chart keeps it. */
export function scheduleOf(
  data: SeatData | UnitData,
  name: string,
): { name?: string; enabled?: boolean | null } | undefined {
  const list = (data.runtime as { schedules?: unknown } | undefined)?.schedules;
  if (!Array.isArray(list)) return undefined;
  return list.find(
    (s): s is { name?: string; enabled?: boolean | null } => isRecord(s) && s.name === name,
  );
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
    case "updateSeat": {
      // The level THIS seat ends with: the entry under the handle it has once
      // the change applies. A handle change also clears the old handle's,
      // which recording it again does of its own accord.
      const moved = op.changes.find((c) => c.path.length === 1 && c.path[0] === "handle");
      const final = typeof moved?.after === "string" ? moved.after : undefined;
      const level = op.accessLevels.find((a) => final === undefined || a.handle === final);
      return {
        type: "updateSeat",
        target: op.target,
        set: op.changes.map((c) => ({
          path: c.path,
          ...(c.after !== undefined ? { value: c.after } : {}),
        })),
        ...(level !== undefined ? { accessLevel: level.after ?? null } : {}),
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
    case "edit":
      return {
        type: "edit",
        target: op.target,
        intents: op.ops.map((part) => intentOf(part) as EditPartIntent),
      };
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
  const expect = (
    subject: string,
    base: unknown,
    theirs: unknown,
    mine?: unknown,
    shape?: ConflictShape,
  ) => {
    if (!jsonEqual(base, theirs)) {
      conflicts.push({ subject, ...(shape ? { shape } : {}), base, theirs, mine });
    }
  };
  /** An address this operation gives a node, held by another node of the draft. */
  const expectFree = (kind: "seat" | "unit", address: string, self: NodeKey, mine: unknown) => {
    const holder = holderOf(draft, kind, address);
    if (!holder || holder.node.key === self) return;
    conflicts.push({
      subject: `the ${addressWord(kind)} ${address}`,
      shape: "snapshot",
      address,
      base: undefined,
      theirs: nodeJson(holder),
      mine,
    });
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
      const kind = op.type === "addUnit" ? "unit" : "seat";
      if (!siblingsAt(draft, op.placement.parent, kind)) {
        return gone("The unit it goes into is no longer in the organization.");
      }
      expectFree(kind, addressIn(op.data, kind), op.key, op.data);
      return finish();
    }

    case "remove": {
      const found = locate(draft, op.target);
      if (!found) return gone("It has already been removed.");
      expect(
        found.kind === "seat" ? "the whole seat" : "the whole unit",
        op.snapshot.json,
        nodeJson(found),
        undefined,
        "snapshot",
      );
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
      return finish();
    }

    case "move": {
      const found = locate(draft, op.target);
      if (!found) return gone("It is no longer in the organization.");
      expect("where it sits", op.from.parent, found.parent, op.to.parent, "parent");
      if (!siblingsAt(draft, op.to.parent, found.kind)) {
        return gone("The destination is no longer in the organization.");
      }
      if (
        found.kind === "unit" &&
        op.to.parent !== COMPANY_KEY &&
        isWithin(draft, op.to.parent, found.node.key)
      ) {
        return gone("The destination is now inside the unit being moved.");
      }
      for (const clear of op.clearLeads) {
        const unit = locate(draft, clear.unit);
        expect(
          "lead",
          clear.before,
          unit?.kind === "unit" ? nonEmpty(unit.node.data.lead) : undefined,
          undefined,
        );
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
        const moves = change.path.length === 1 && change.path[0] === addressWord(kind);
        if (moves && typeof change.after === "string") {
          expectFree(kind, change.after, op.target, undefined);
        }
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
      expect(
        "kind",
        op.before,
        writtenKind(found.node.data),
        op.after === "human" ? "human" : undefined,
      );
      // MINE IS NONE LEFT: once the change applies the seat holds no field
      // its kind forbids, so a draft already there agrees with this change.
      expect(
        "fields the new kind removes",
        op.stripped,
        forbiddenFor(found.node.data, op.after),
        [],
        "fields",
      );
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

    case "edit": {
      // Each part is evaluated against the draft the parts before it
      // produce, as they were recorded. A part that conflicts is not applied,
      // and every conflict of the edit is reported together, because the
      // person choosing is choosing for the whole edit.
      let at = draft;
      for (const part of op.ops) {
        const outcome = evaluate(at, part);
        if (outcome.kind === "gone") return outcome;
        if (outcome.kind === "conflict") {
          conflicts.push(...outcome.conflicts);
          continue;
        }
        at = apply(at, part).draft;
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
      const removed = new Set(found.kind === "seat" ? [found.node.key] : subtreeKeys(found.node));
      let next = detach(draft, op.target)!.draft;
      const cleared: ReferenceEffect[] = [];
      next = clearReferences(draft, next, removed, cleared);
      for (const change of op.accessLevels) {
        next = { ...next, company: withAccessLevel(next.company, change.handle, undefined) };
        cleared.push({ kind: "gitlab_access_level", holder: COMPANY_KEY, from: change.handle });
      }
      if (op.routeTo) next = { ...next, company: withRouteTo(next.company, op.routeTo.after) };
      return { draft: next, report: { cleared, followed: [], stripped: [] } };
    }

    case "renameSeat":
      return {
        draft: updateSeatData(draft, op.target, (data) => ({ ...data, name: op.after })),
        report: NO_EFFECTS,
      };

    case "renameUnit":
      return {
        draft: updateUnitData(draft, op.target, (data) => ({ ...data, name: op.after })),
        report: NO_EFFECTS,
      };

    case "move": {
      const detached = detach(draft, op.target)!;
      let next = attach(detached.draft, op.to, detached.node.node, detached.node.kind);
      const cleared: ReferenceEffect[] = [];
      for (const clear of op.clearLeads) {
        next = updateUnitData(next, clear.unit, (data) => {
          const { lead: _lead, ...rest } = data;
          return rest as UnitData;
        });
        cleared.push({ kind: "lead", holder: clear.unit, from: clear.before });
      }
      return { draft: next, report: { cleared, followed: [], stripped: [] } };
    }

    case "updateSeat": {
      const before = locate(draft, op.target)!.node.data as SeatData;
      let next = updateSeatData(draft, op.target, (data) =>
        op.changes.reduce((acc, c) => setPath(acc, c.path, cloneJson(c.after)) as SeatData, data),
      );
      for (const change of op.accessLevels) {
        next = { ...next, company: withAccessLevel(next.company, change.handle, change.after) };
      }
      const after = locate(next, op.target)!.node.data as SeatData;
      const followed: ReferenceEffect[] = [];
      if (after.handle !== before.handle) {
        next = followSeatAddress(next, before.handle, after.handle, followed);
      }
      return { draft: next, report: { cleared: [], followed, stripped: [] } };
    }

    case "updateUnit": {
      const before = locate(draft, op.target)!.node.data as UnitData;
      let next = updateUnitData(draft, op.target, (data) =>
        op.changes.reduce((acc, c) => setPath(acc, c.path, cloneJson(c.after)) as UnitData, data),
      );
      const after = locate(next, op.target)!.node.data as UnitData;
      const followed: ReferenceEffect[] = [];
      if (after.key !== before.key) {
        next = followUnitAddress(next, before.key, after.key, followed);
      }
      return { draft: next, report: { cleared: [], followed, stripped: [] } };
    }

    case "setLead":
      return {
        draft: updateUnitData(
          draft,
          op.target,
          (data) => setPath(data, ["lead"], op.after) as UnitData,
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
            ) as SeatData,
        ),
        report: NO_EFFECTS,
      };

    case "changeKind": {
      let next = updateSeatData(draft, op.target, (data) => {
        let out = data;
        for (const field of op.stripped) out = setPath(out, field.path, undefined) as SeatData;
        out = setPath(out, ["kind"], op.after === "human" ? "human" : undefined) as SeatData;
        if (op.contact) {
          out = setPath(out, ["runtime", "contact"], cloneJson(op.contact)) as SeatData;
        }
        return out;
      });
      if (op.routeTo) next = { ...next, company: withRouteTo(next.company, op.routeTo.after) };
      return {
        draft: next,
        report: { cleared: [], followed: [], stripped: op.stripped.map((f) => fieldName(f.path)) },
      };
    }

    case "setScheduleEnabled": {
      const update = <T extends SeatData | UnitData>(data: T): T => {
        const schedules = (data.runtime as { schedules?: unknown[] } | undefined)?.schedules ?? [];
        return setPath(
          data,
          ["runtime", "schedules"],
          schedules.map((s) =>
            isRecord(s) && s.name === op.schedule ? { ...s, enabled: op.after } : s,
          ),
        ) as T;
      };
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

    case "applyTemplate": {
      // THROUGH THE ADDS, so a template's nodes land in the chart's order and
      // under the same rules an add of each would meet.
      let next: Draft = {
        ...draft,
        company: {
          ...draft.company,
          name: op.charter.name,
          ...(op.charter.mission ? { mission: op.charter.mission } : {}),
        },
      };
      for (const part of templateAdds(op)) next = apply(next, part).draft;
      return { draft: next, report: NO_EFFECTS };
    }

    case "edit": {
      let next = draft;
      const cleared: ReferenceEffect[] = [];
      const followed: ReferenceEffect[] = [];
      const stripped: string[] = [];
      for (const part of op.ops) {
        const applied = apply(next, part);
        next = applied.draft;
        cleared.push(...applied.report.cleared);
        followed.push(...applied.report.followed);
        stripped.push(...applied.report.stripped);
      }
      return { draft: next, report: { cleared, followed, stripped } };
    }
  }
}

/** A template's nodes as the adds that create them, parents before what they hold. */
function templateAdds(op: ApplyTemplate): Array<AddSeat | AddUnit> {
  const out: Array<AddSeat | AddUnit> = [];
  const seat = (node: DraftSeat, parent: NodeKey) =>
    out.push({ type: "addSeat", key: node.key, placement: { parent }, data: cloneJson(node.data) });
  const unit = (node: DraftUnit, parent: NodeKey) => {
    out.push({ type: "addUnit", key: node.key, placement: { parent }, data: cloneJson(node.data) });
    for (const s of node.roles) seat(s, node.key);
    for (const u of node.children) unit(u, node.key);
  };
  for (const s of op.roles) seat(s, COMPANY_KEY);
  for (const u of op.units) unit(u, COMPANY_KEY);
  return out;
}

/**
 * A template as the operations it amounts to, recorded against the draft it
 * was applied to: the charter as a company edit, then one add per node.
 *
 * WHAT A TEMPLATE BECOMES ONCE ITS COMPANY EXISTS. A template starts a company
 * from nothing, and a save that created the company but not all of its chart
 * leaves a draft to finish on a company that is no longer nothing — onto
 * which a template does not apply at all. Its adds do, each on its own terms:
 * one the save already made meets its own node and is resolved like any add
 * the chart already holds (`history.rebase`), and the rest are still adds.
 */
export function expandTemplate(draft: Draft, op: ApplyTemplate): Operation[] {
  const out: Operation[] = [];
  const charter = record(draft, {
    type: "updateCompany",
    set: [
      { path: ["name"], value: op.charter.name },
      ...(op.charter.mission ? [{ path: ["mission"], value: op.charter.mission }] : []),
    ],
  });
  if (charter.ok) out.push(charter.op);
  out.push(...templateAdds(op));
  return out;
}

/**
 * Clears the references a removal left naming nothing: a unit's `lead` and
 * the `manages` entries that resolved to a removed seat or unit.
 *
 * WHAT AN ENTRY NAMED IS DECIDED AS THE CHART RESOLVES IT, in the draft BEFORE
 * the removal (`draft.addressIndex`): an address a node answers to now, then
 * one it used to answer to that nothing has claimed since — so an entry
 * written with a removed seat's old handle named that seat and goes with it,
 * where matching on handles alone left it naming nobody. And a `manages`
 * entry resolves to a SEAT before a unit, so one that named a removed seat is
 * cleared even when a unit of that key remains: leaving it would silently
 * widen it to the whole unit. An entry is kept only when what it named is
 * still there.
 *
 * THE CHART ITSELF CLEARS NOTHING on a removal — it tombstones the address,
 * and the organization reports what still names it as a dangling reference —
 * so the builder does it here, where the review lists every entry it clears.
 */
function clearReferences(
  before: Draft,
  draft: Draft,
  removed: ReadonlySet<NodeKey>,
  cleared: ReferenceEffect[],
): Draft {
  const { seats, units } = addressIndex(before);
  const namesRemoved = (entry: string, seatsOnly: boolean): boolean => {
    const seat = seats.get(entry);
    if (seat) return removed.has(seat.key);
    if (seatsOnly) return false;
    const unit = units.get(entry);
    return unit !== undefined && removed.has(unit.key);
  };
  let next = mapUnitData(draft, (data, key) => {
    const lead = data.lead;
    if (typeof lead !== "string" || !namesRemoved(lead, true)) return data;
    cleared.push({ kind: "lead", holder: key, from: lead });
    return setPath(data, ["lead"], undefined) as UnitData;
  });
  next = mapSeatData(next, (data, key) => {
    if (!Array.isArray(data.manages)) return data;
    const kept = data.manages.filter((entry) => {
      const dangling = namesRemoved(entry, false);
      if (dangling) cleared.push({ kind: "manages", holder: key, from: entry });
      return !dangling;
    });
    return kept.length === data.manages.length
      ? data
      : (setPath(data, ["manages"], kept.length > 0 ? kept : undefined) as SeatData);
  });
  return next;
}

/**
 * Follows a seat's new handle in every unit's `lead`, every `manages` entry and
 * the Datadog fallback that named its old one — as the chart's own rename moves
 * every reference to the object it renames. A `manages` entry
 * matching a seat's handle named the seat (a seat wins over a unit), so every
 * entry equal to the old handle was this seat's.
 */
function followSeatAddress(
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
  if (getPath(next.company, DATADOG_ROUTE_TO) === from) {
    next = { ...next, company: withRouteTo(next.company, to) };
    followed.push({ kind: "datadog_route_to", holder: COMPANY_KEY, from, to });
  }
  return next;
}

/**
 * Follows a unit's new key in every `manages` entry that named its old one. An entry that also matches a seat's handle named the SEAT, so it is not
 * the unit's to follow.
 */
function followUnitAddress(
  draft: Draft,
  from: string,
  to: string,
  followed: ReferenceEffect[],
): Draft {
  if ([...allSeats(draft)].some(({ seat }) => seat.data.handle === from)) return draft;
  return mapSeatData(draft, (data, key) => {
    if (!Array.isArray(data.manages) || !data.manages.includes(from)) return data;
    followed.push({ kind: "manages", holder: key, from, to });
    return { ...data, manages: data.manages.map((m) => (m === from ? to : m)) };
  });
}

// ---------------------------------------------------------------------------
// Re-keying and restating, for a rebase
// ---------------------------------------------------------------------------

/**
 * The operation with every node key it names mapped through `keys`, and the
 * same object when it names none of them.
 *
 * WHAT AN ADD BECOMES ONCE ITS NODE EXISTS. An add whose node the chart turns
 * out to hold already (a save that created it and failed later, or a
 * colleague who created the same address) is resolved onto the node that
 * holds it, and every later operation that named the node by the key this
 * draft minted names it by its address from then on.
 */
export function rekeyOperation(op: Operation, keys: ReadonlyMap<NodeKey, NodeKey>): Operation {
  if (keys.size === 0) return op;
  const k = (key: NodeKey) => keys.get(key) ?? key;
  switch (op.type) {
    case "addUnit":
    case "addSeat":
      return { ...op, key: k(op.key), placement: { parent: k(op.placement.parent) } };
    case "move":
      return {
        ...op,
        target: k(op.target),
        from: { parent: k(op.from.parent) },
        to: { parent: k(op.to.parent) },
        clearLeads: op.clearLeads.map((c) => ({ ...c, unit: k(c.unit) })),
      };
    case "remove":
      return { ...op, target: k(op.target), snapshot: { ...op.snapshot, key: k(op.snapshot.key) } };
    case "edit":
      return {
        ...op,
        target: k(op.target),
        ops: op.ops.map((part) => rekeyOperation(part, keys) as EditPart),
      };
    case "setDatadogRouteTo":
    case "updateCompany":
    case "applyTemplate":
      return op;
    default:
      return { ...op, target: k(op.target) };
  }
}

/** The seat fields a restatement writes through `updateSeat`: every one it may. */
const restatableSeat = (field: string) =>
  !["handle", "name", "kind", "manages", "former_handles"].includes(field);
/** The unit fields a restatement writes through `updateUnit`. */
const restatableUnit = (field: string) => !["key", "name", "lead", "former_keys"].includes(field);

/**
 * The operations that make the node at `holder` what an add would have
 * created: moved under the add's parent, its kind, its name and every field
 * set to the add's, recorded against `draft` in order.
 *
 * "KEEP MINE" OF AN ADD THE CHART ALREADY HOLDS. The address is taken, so the
 * only way to have this draft's node there is to write it over the one that
 * is — and the operations say exactly which of its values that replaces,
 * as preconditions, so the review shows it and a later rebase holds it to
 * them like any edit.
 */
export function restateOnto(draft: Draft, op: AddSeat | AddUnit, holder: NodeKey): Operation[] {
  const out: Operation[] = [];
  let at = draft;
  const take = (intent: Intent) => {
    const result = record(at, intent);
    if (!result.ok) return;
    out.push(result.op);
    at = apply(at, result.op).draft;
  };
  const found = locate(at, holder);
  if (!found) return out;
  if (found.parent !== op.placement.parent && siblingsAt(at, op.placement.parent, found.kind)) {
    take({ type: "move", target: holder, to: { parent: op.placement.parent } });
  }
  if (op.type === "addSeat") {
    const theirs = found.node.data as SeatData;
    take({ type: "changeKind", target: holder, kind: kindOf(op.data) });
    const fields = new Set(
      [...Object.keys(theirs), ...Object.keys(op.data)].filter(restatableSeat),
    );
    take({
      type: "edit",
      target: holder,
      intents: [
        { type: "renameSeat", target: holder, name: op.data.name },
        {
          type: "updateSeat",
          target: holder,
          set: [...fields].map((field) => ({
            path: [field],
            ...(op.data[field] !== undefined ? { value: op.data[field] } : {}),
          })),
        },
        { type: "setManages", target: holder, manages: op.data.manages ?? [] },
      ],
    });
  } else {
    const theirs = found.node.data as UnitData;
    const fields = new Set(
      [...Object.keys(theirs), ...Object.keys(op.data)].filter(restatableUnit),
    );
    take({
      type: "edit",
      target: holder,
      intents: [
        { type: "renameUnit", target: holder, name: op.data.name },
        {
          type: "updateUnit",
          target: holder,
          set: [...fields].map((field) => ({
            path: [field],
            ...(op.data[field] !== undefined ? { value: op.data[field] } : {}),
          })),
        },
        { type: "setLead", target: holder, ...(op.data.lead ? { lead: op.data.lead } : {}) },
      ],
    });
  }
  return out;
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
  const seatNamed = (handle: string): string => {
    const seat = holderOf(before, "seat", handle);
    return seat ? seat.node.data.name || handle : handle;
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
        return seats === 0
          ? `Removed unit ${found.node.data.name}.`
          : `Removed unit ${found.node.data.name} and ${plural(seats, "seat")} in it.`;
      }
      return `Removed seat ${nameOf(op.target)}.`;
    }
    case "renameSeat":
      return `Renamed seat ${op.before} to ${op.after}.`;
    case "renameUnit":
      return `Renamed unit ${op.before} to ${op.after}.`;
    case "move":
      return `Moved ${nameOf(op.target)} to ${where(op.to.parent)}.`;
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
        : `Set the lead of ${nameOf(op.target)} to ${seatNamed(op.after)}.`;
    case "setManages":
      return `Changed whom ${nameOf(op.target)} manages.`;
    case "changeKind": {
      const kind = op.after === "human" ? "a human" : "an agent";
      return op.before === (op.after === "human" ? "human" : undefined)
        ? `Removed from ${nameOf(op.target)} what ${kind} seat does not carry.`
        : `Changed ${nameOf(op.target)} to ${kind} seat.`;
    }
    case "setScheduleEnabled":
      return `${op.after ? "Enabled" : "Disabled"} schedule ${op.schedule} on ${nameOf(op.target)}.`;
    case "setDatadogRouteTo":
      return op.change.after === undefined
        ? "Cleared the Datadog fallback seat."
        : `Set the Datadog fallback seat to ${seatNamed(op.change.after)}.`;
    case "updateCompany":
      return `Edited the charter: ${op.changes.map((c) => fieldName(c.path)).join(", ")}.`;
    case "applyTemplate":
      return `Started ${op.charter.name} from a template.`;
    case "edit": {
      const parts = op.ops.flatMap(editPartPhrases).join(", ");
      return op.target === COMPANY_KEY
        ? `Edited the charter: ${parts}.`
        : `Edited ${nameOf(op.target)}: ${parts}.`;
    }
  }
}

/** What one part of an edit changed, as the phrases of an edit's sentence. */
function editPartPhrases(part: EditPart): string[] {
  switch (part.type) {
    case "renameSeat":
    case "renameUnit":
      return [`renamed to ${part.after}`];
    case "updateSeat":
      return [
        ...part.changes.map((c) => fieldName(c.path)),
        ...(part.accessLevels.length > 0 ? ["GitLab access level"] : []),
      ];
    case "updateUnit":
    case "updateCompany":
      return part.changes.map((c) => fieldName(c.path));
    case "setLead":
      return ["lead"];
    case "setManages":
      return ["manages"];
    case "setScheduleEnabled":
      return [`${part.after ? "enabled" : "disabled"} schedule ${part.schedule}`];
  }
}

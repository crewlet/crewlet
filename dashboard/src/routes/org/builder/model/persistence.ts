/**
 * Keeping the operator's work across a reload of the tab.
 *
 * ONLY THE LOG IS KEPT. One key, `crewlet_org_draft`, holds
 * `{ v, mode, baseRevision, ops, undone, savedAt }` and nothing else: never
 * the base document, the draft or the problems. The document holds contact
 * identities, emails, policies and `${VAR}` names; kept in storage it would
 * outlive the operator's token and be offered to whoever uses the tab next.
 * The log refers to the company only by keys and the values its own edits
 * wrote, and on restore the base is fetched again, so what is replayed is
 * always replayed onto what the engine holds now.
 *
 * ONE KEY, NOT ONE PER REVISION. A draft saved against revision A must still
 * be found once the active revision is B, because that is exactly when it
 * matters: somebody saved while the operator was away, and the draft needs
 * updating rather than silently vanishing.
 *
 * WHAT IS READ BACK IS UNTRUSTED. Storage is shared with every script on the
 * origin and outlives builds, so a restored value is validated against the
 * exact shapes this build records before a single operation is replayed, and
 * anything that does not match is discarded whole: a log with one operation
 * missing replays into something the operator never made.
 *
 * STORAGE CAN REFUSE. A private window, a blocked origin or a full quota
 * throws on access, so every access is caught and reported as `refused`,
 * which the builder shows as a caution: the work continues, it just will not
 * survive a reload.
 *
 * WHAT CLEARS IT: a save, a discard, a change of operator token, and a check
 * the engine refused with 401 or 403. The last two are the tab changing hands,
 * and a colleague's draft is not something to offer the next operator.
 * [persistencePlan] turns the builder's state into the one write or removal
 * that matches it.
 */

import type { ConfigRole, ConfigUnit } from "~/protocol/index.ts";
import { isRecord } from "./json.ts";
import { isMintedKey, isNodeKey } from "./keys.ts";
import type { DraftSeat, DraftUnit, Placement } from "./draft.ts";
import {
  EDIT_PART_TYPES,
  OPERATIONS_VERSION,
  malformedReason,
  type FieldChange,
  type Operation,
} from "./operations.ts";
import type { Log } from "./history.ts";
import type { BuilderMode } from "./transport.ts";

/** The storage key. */
export const DRAFT_STORAGE_KEY = "crewlet_org_draft";

/**
 * The most operations (applied and undone together) a kept draft may hold.
 *
 * Web Storage allows about five megabytes per origin and counts UTF-16 code
 * units, so roughly 2.5 million characters. A field edit records in well under
 * a kilobyte, but a removal keeps a snapshot of what it removed, and removing
 * a large unit keeps the whole subtree. Five hundred operations at an
 * average of two kilobytes is one megabyte, which leaves the quota room for a
 * handful of unusually large removals. It is also far beyond a coherent
 * session of edits: a draft that size is a restructuring better saved in
 * steps. A draft over the cap keeps working; it is removed from storage and
 * the operator is told it will not survive a reload or leaving the builder.
 */
export const MAX_KEPT_OPERATIONS = 500;

/** The subset of Web Storage the builder uses. */
export interface DraftStorage {
  getItem(key: string): string | null;
  setItem(key: string, value: string): void;
  removeItem(key: string): void;
}

/** A kept draft. */
export interface KeptDraft {
  readonly v: number;
  readonly mode: BuilderMode;
  /** The revision the log was recorded on; `null` in create mode. */
  readonly baseRevision: string | null;
  readonly ops: readonly Operation[];
  readonly undone: readonly Operation[];
  /** Milliseconds since the epoch, from the injected clock. */
  readonly savedAt: number;
}

export type KeepResult = "kept" | "cleared" | "too_large" | "refused" | "unavailable";

/** Writes a kept draft, or removes it when the log is over the cap. */
export function keepDraft(storage: DraftStorage | null, kept: KeptDraft): KeepResult {
  if (!storage) return "unavailable";
  if (kept.ops.length + kept.undone.length > MAX_KEPT_OPERATIONS) {
    return clearDraft(storage) === "refused" ? "refused" : "too_large";
  }
  try {
    storage.setItem(DRAFT_STORAGE_KEY, JSON.stringify(kept));
    return "kept";
  } catch {
    return "refused";
  }
}

/** Removes the kept draft. */
export function clearDraft(storage: DraftStorage | null): KeepResult {
  if (!storage) return "unavailable";
  try {
    storage.removeItem(DRAFT_STORAGE_KEY);
    return "cleared";
  } catch {
    return "refused";
  }
}

export type Restored =
  | { readonly kind: "none" }
  | { readonly kind: "restored"; readonly kept: KeptDraft }
  /** Something was stored and it was not a draft this build can replay; it has been removed. */
  | { readonly kind: "discarded" }
  | { readonly kind: "refused" }
  | { readonly kind: "unavailable" };

/** Reads the kept draft, validating it whole. */
export function restoreDraft(storage: DraftStorage | null): Restored {
  if (!storage) return { kind: "unavailable" };
  let raw: string | null;
  try {
    raw = storage.getItem(DRAFT_STORAGE_KEY);
  } catch {
    return { kind: "refused" };
  }
  if (raw === null) return { kind: "none" };
  let parsed: unknown;
  try {
    parsed = JSON.parse(raw);
  } catch {
    parsed = undefined;
  }
  const kept = parseKeptDraft(parsed);
  if (kept) return { kind: "restored", kept };
  clearDraft(storage);
  return { kind: "discarded" };
}

/** What to offer when a kept draft meets the company that was just loaded. */
export type RestoreOffer =
  /** Same mode and revision: offer Keep or Discard. */
  | { readonly kind: "keep_or_discard" }
  /** Edit mode, and the revision moved: run the update-my-draft flow. */
  | { readonly kind: "update"; readonly from: string; readonly to: string }
  /** The draft was made for a different mode: discard it and say so. */
  | {
      readonly kind: "discard_mode_changed";
      readonly kept: BuilderMode;
      readonly loaded: BuilderMode;
    };

/** Decides what a restored draft offers against the company as loaded. */
export function restoreOffer(
  kept: KeptDraft,
  loaded: { mode: BuilderMode; revision: string | null },
): RestoreOffer {
  if (kept.mode !== loaded.mode)
    return { kind: "discard_mode_changed", kept: kept.mode, loaded: loaded.mode };
  if (
    kept.mode === "edit" &&
    kept.baseRevision !== loaded.revision &&
    kept.baseRevision !== null &&
    loaded.revision !== null
  ) {
    return { kind: "update", from: kept.baseRevision, to: loaded.revision };
  }
  return { kind: "keep_or_discard" };
}

// ---------------------------------------------------------------------------
// Validation
// ---------------------------------------------------------------------------

/** A kept draft, or `undefined` when the value is anything else. */
export function parseKeptDraft(value: unknown): KeptDraft | undefined {
  if (
    !isRecord(value) ||
    !exactKeys(value, ["v", "mode", "baseRevision", "ops", "undone", "savedAt"])
  )
    return undefined;
  if (value.v !== OPERATIONS_VERSION) return undefined;
  if (value.mode !== "edit" && value.mode !== "create") return undefined;
  if (value.mode === "edit" ? !isNonEmptyString(value.baseRevision) : value.baseRevision !== null)
    return undefined;
  if (typeof value.savedAt !== "number" || !Number.isFinite(value.savedAt)) return undefined;
  if (!Array.isArray(value.ops) || !Array.isArray(value.undone)) return undefined;
  if (value.ops.length + value.undone.length > MAX_KEPT_OPERATIONS) return undefined;
  const mode = value.mode;
  // Shaped right is not enough: an operation must also be one the builder
  // could have recorded, and a template exists only in create mode.
  const valid = (op: unknown) =>
    isOperation(op) &&
    malformedReason(op) === null &&
    (op.type !== "applyTemplate" || mode === "create");
  if (!value.ops.every(valid) || !value.undone.every(valid)) return undefined;
  return value as unknown as KeptDraft;
}

function exactKeys(
  record: Record<string, unknown>,
  required: readonly string[],
  optional: readonly string[] = [],
): boolean {
  const allowed = new Set([...required, ...optional]);
  return required.every((k) => k in record) && Object.keys(record).every((k) => allowed.has(k));
}

const isNonEmptyString = (v: unknown): v is string => typeof v === "string" && v !== "";
const isOptionalString = (v: unknown) => v === undefined || typeof v === "string";
const isStringList = (v: unknown) => Array.isArray(v) && v.every((s) => typeof s === "string");
const isPath = (v: unknown) =>
  Array.isArray(v) && v.length > 0 && v.every((s) => typeof s === "string" && s !== "");

function isPlacement(v: unknown): v is Placement {
  return (
    isRecord(v) &&
    exactKeys(v, ["parent", "after"]) &&
    isNodeKey(v.parent) &&
    (v.after === null || isNodeKey(v.after))
  );
}

/** A role object: a record with a string name. Its other fields are the engine's to judge. */
function isRoleData(v: unknown): v is ConfigRole {
  return isRecord(v) && typeof v.name === "string";
}

/** A unit's own fields: a record with a string name and never the lists the tree holds. */
function isUnitData(v: unknown): v is ConfigUnit {
  return isRecord(v) && typeof v.name === "string" && !("roles" in v) && !("children" in v);
}

function isFieldChange(v: unknown): v is FieldChange {
  return isRecord(v) && exactKeys(v, ["path"], ["before", "after"]) && isPath(v.path);
}

function isAccessLevelChange(v: unknown): boolean {
  return (
    isRecord(v) &&
    exactKeys(v, ["handle"], ["before", "after"]) &&
    isNonEmptyString(v.handle) &&
    isOptionalString(v.before) &&
    isOptionalString(v.after)
  );
}

function isRouteToChange(v: unknown): boolean {
  return (
    isRecord(v) &&
    exactKeys(v, [], ["before", "after"]) &&
    isOptionalString(v.before) &&
    isOptionalString(v.after)
  );
}

function isSnapshot(v: unknown): boolean {
  return isRecord(v) && exactKeys(v, ["key", "json"]) && isNodeKey(v.key) && isRecord(v.json);
}

function isDraftSeat(v: unknown): v is DraftSeat {
  return isRecord(v) && exactKeys(v, ["key", "data"]) && isMintedKey(v.key) && isRoleData(v.data);
}

function isDraftUnit(v: unknown): v is DraftUnit {
  return (
    isRecord(v) &&
    exactKeys(v, ["key", "data", "roles", "children"]) &&
    isMintedKey(v.key) &&
    isUnitData(v.data) &&
    Array.isArray(v.roles) &&
    v.roles.every(isDraftSeat) &&
    Array.isArray(v.children) &&
    v.children.every(isDraftUnit)
  );
}

const list = (v: unknown, each: (item: unknown) => boolean) => Array.isArray(v) && v.every(each);

/** Whether a value is one operation, exactly as this build records it. */
export function isOperation(v: unknown): v is Operation {
  if (!isRecord(v) || typeof v.type !== "string") return false;
  switch (v.type) {
    case "addUnit":
      return (
        exactKeys(v, ["type", "key", "placement", "data"]) &&
        isMintedKey(v.key) &&
        isPlacement(v.placement) &&
        isUnitData(v.data)
      );
    case "addSeat":
      return (
        exactKeys(v, ["type", "key", "placement", "data"]) &&
        isMintedKey(v.key) &&
        isPlacement(v.placement) &&
        isRoleData(v.data)
      );
    case "remove":
      return (
        exactKeys(
          v,
          ["type", "target", "snapshot", "placedSeats", "placed", "accessLevels"],
          ["routeTo"],
        ) &&
        isNodeKey(v.target) &&
        isSnapshot(v.snapshot) &&
        (v.placedSeats === "remove" || v.placedSeats === "keep") &&
        list(v.placed, isSnapshot) &&
        list(v.accessLevels, isAccessLevelChange) &&
        (v.routeTo === undefined || isRouteToChange(v.routeTo))
      );
    case "renameSeat":
      return (
        exactKeys(v, ["type", "target", "before", "after", "accessLevels"], ["pin"]) &&
        isNodeKey(v.target) &&
        typeof v.before === "string" &&
        typeof v.after === "string" &&
        (v.pin === undefined || isNonEmptyString(v.pin)) &&
        list(v.accessLevels, isAccessLevelChange)
      );
    case "renameUnit":
      return (
        exactKeys(v, ["type", "target", "before", "after"]) &&
        isNodeKey(v.target) &&
        typeof v.before === "string" &&
        typeof v.after === "string"
      );
    case "move":
      return (
        exactKeys(v, ["type", "target", "from", "to", "clearLeads"], ["unitRef"]) &&
        isNodeKey(v.target) &&
        isPlacement(v.from) &&
        isPlacement(v.to) &&
        isOptionalString(v.unitRef) &&
        list(
          v.clearLeads,
          (c) =>
            isRecord(c) &&
            exactKeys(c, ["unit", "before"]) &&
            isNodeKey(c.unit) &&
            typeof c.before === "string",
        )
      );
    case "reorder":
      return (
        exactKeys(v, ["type", "target", "from", "to"]) &&
        isNodeKey(v.target) &&
        isPlacement(v.from) &&
        isPlacement(v.to)
      );
    case "updateSeat":
      return (
        exactKeys(v, ["type", "target", "changes", "accessLevels"]) &&
        isNodeKey(v.target) &&
        list(v.changes, isFieldChange) &&
        list(v.accessLevels, isAccessLevelChange)
      );
    case "updateUnit":
      return (
        exactKeys(v, ["type", "target", "changes"]) &&
        isNodeKey(v.target) &&
        list(v.changes, isFieldChange)
      );
    case "setLead":
      return (
        exactKeys(v, ["type", "target"], ["before", "after"]) &&
        isNodeKey(v.target) &&
        isOptionalString(v.before) &&
        isOptionalString(v.after)
      );
    case "setManages":
      return (
        exactKeys(v, ["type", "target"], ["before", "after"]) &&
        isNodeKey(v.target) &&
        (v.before === undefined || isStringList(v.before)) &&
        (v.after === undefined || isStringList(v.after))
      );
    case "changeKind":
      return (
        exactKeys(v, ["type", "target", "after", "stripped"], ["before", "contact", "routeTo"]) &&
        isNodeKey(v.target) &&
        isOptionalString(v.before) &&
        (v.after === "agent" || v.after === "human") &&
        list(v.stripped, isFieldChange) &&
        (v.contact === undefined ||
          (isRecord(v.contact) && Object.values(v.contact).every((s) => typeof s === "string"))) &&
        (v.routeTo === undefined || isRouteToChange(v.routeTo))
      );
    case "setScheduleEnabled":
      return (
        exactKeys(v, ["type", "target", "schedule", "after"], ["before"]) &&
        isNodeKey(v.target) &&
        typeof v.schedule === "string" &&
        (v.before === undefined || v.before === null || typeof v.before === "boolean") &&
        typeof v.after === "boolean"
      );
    case "setDatadogRouteTo":
      return exactKeys(v, ["type", "change"]) && isRouteToChange(v.change);
    case "updateCompany":
      return exactKeys(v, ["type", "changes"]) && list(v.changes, isFieldChange);
    case "applyTemplate":
      return (
        exactKeys(v, ["type", "template", "charter", "roles", "units"]) &&
        (v.template === "empty" ||
          v.template === "new_company" ||
          v.template === "established_company") &&
        isRecord(v.charter) &&
        exactKeys(v.charter, ["name"], ["mission"]) &&
        typeof v.charter.name === "string" &&
        isOptionalString(v.charter.mission) &&
        list(v.roles, isDraftSeat) &&
        list(v.units, isDraftUnit)
      );
    case "edit":
      // Its parts are operations of their own and are held to their own
      // shapes; an edit inside an edit is not one of them.
      return (
        exactKeys(v, ["type", "target", "ops"]) &&
        isNodeKey(v.target) &&
        list(v.ops, (part) => isOperation(part) && EDIT_PART_TYPES.has(part.type))
      );
    default:
      return false;
  }
}

// ---------------------------------------------------------------------------
// What to store for a state
// ---------------------------------------------------------------------------

/** The builder state persistence reads. */
export interface PersistableState {
  readonly mode: BuilderMode;
  readonly baseRevision: string | null;
  readonly log: Log;
  /** False from a token change or a refused check until the next operation. */
  readonly keep: boolean;
}

/** The one storage action a state calls for. */
export type PersistencePlan =
  { readonly action: "clear" } | { readonly action: "keep"; readonly kept: KeptDraft };

/**
 * What storage should hold for a state: its log, or nothing. Nothing when the
 * log is empty (saved, discarded, or never started) or the state says the tab
 * changed hands.
 */
export function persistencePlan(state: PersistableState, now: number): PersistencePlan {
  if (!state.keep || (state.log.ops.length === 0 && state.log.undone.length === 0))
    return { action: "clear" };
  return {
    action: "keep",
    kept: {
      v: OPERATIONS_VERSION,
      mode: state.mode,
      baseRevision: state.baseRevision,
      ops: state.log.ops,
      undone: state.log.undone,
      savedAt: now,
    },
  };
}

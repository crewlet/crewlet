/**
 * What the org builder takes from the assistant: changes to its DRAFT, never
 * to the company (`org.draft.apply_ops`).
 *
 * THE MODEL SPEAKS IN THE DOCUMENT'S IDENTITIES, THE BUILDER IN NODE KEYS.
 * A node key never leaves the builder (`model/keys.ts`), so the assistant
 * names a seat by its handle and a unit by its key, as the company document
 * does, and [parseOrgOps] turns each change into the builder's own [Intent],
 * minting a new node's key from the builder's own source.
 *
 * EVERY CHANGE GOES THROUGH THE BUILDER'S ONE DOOR. Each intent is recorded
 * against the draft as the one before it left it, with `recordIntent`, which
 * is what every dialog asks and what the reducer records through: the mode
 * guard, a lead's subtree and every precondition a person's own edit meets.
 * One that the builder would refuse refuses the whole set, in the builder's
 * own sentence, so nothing is ever half applied. What is applied lands in the
 * operation log like a person's own edits: undo, the dry run and the review
 * work unchanged, and saving stays the person's.
 *
 * NOTHING HERE CREATES A COMPANY: a template is the create flow's alone, and
 * the vocabulary below has no way to name one.
 */

import type { AssistantActionArgs } from "~/contract/assistant.ts";
import type { ConfigRole, ConfigUnit } from "~/protocol/index.ts";
import type { PageActionParse } from "~/app/assistant/context.ts";
import type { AssistantDraftSpec } from "~/app/assistant/draft.ts";
import {
  allSeats,
  allUnits,
  locate,
  seatWithHandle,
  unitIdOf,
  unitWithKey,
  type Draft,
  type Placement,
} from "./model/draft.ts";
import { COMPANY_KEY, mintKey, type KeySource, type NodeKey } from "./model/keys.ts";
import { apply as applyOperation, describeOperation, type Intent } from "./model/operations.ts";
import { builderReducer, recordIntent, type BuilderState } from "./model/reducer.ts";

/** The most changes one proposal makes: a team's worth, read on one card. */
export const ORG_DRAFT_MAX_OPS = 20;

/** The largest seat or unit object one change carries, as JSON. */
const MAX_DATA_BYTES = 8 * 1024;

/** The longest name, handle, key or path segment a change names. */
const MAX_TEXT = 200;

/** How deep a field path reaches into a seat or a unit. */
const MAX_PATH = 8;

/** The changes, recorded, and what each does in the builder's own words. */
export interface OrgDraftChange {
  readonly intents: readonly Intent[];
  readonly words: readonly string[];
}

type Raw = Record<string, unknown>;

const encoder = new TextEncoder();

function isObject(value: unknown): value is Raw {
  return value !== null && typeof value === "object" && !Array.isArray(value);
}

function text(value: unknown): value is string {
  return (
    typeof value === "string" &&
    value.length > 0 &&
    value.length <= MAX_TEXT &&
    !/[\u0000-\u001f\u007f-\u009f]/.test(value)
  );
}

/** Refuse a key the change's own shape does not have, by name. */
function onlyKeys(change: Raw, allowed: readonly string[]): string | null {
  const extra = Object.keys(change).find((k) => !allowed.includes(k));
  return extra === undefined ? null : `A change cannot carry "${extra}".`;
}

/** A seat's or a unit's object, plain JSON and bounded. */
function dataOf(value: unknown): Raw | null {
  if (!isObject(value)) return null;
  let json: string;
  try {
    json = JSON.stringify(value);
  } catch {
    return null;
  }
  if (encoder.encode(json).length > MAX_DATA_BYTES) return null;
  return JSON.parse(json) as Raw;
}

/** The node a change names: `seat: <handle>` or `unit: <key>`, and exactly one. */
function nodeOf(change: Raw, draft: Draft): { key: NodeKey; kind: "seat" | "unit" } | string {
  const seat = change.seat;
  const unit = change.unit;
  if ((seat === undefined) === (unit === undefined)) {
    return "A change names one seat by its handle or one unit by its key.";
  }
  if (seat !== undefined) {
    if (!text(seat)) return "That is not a seat's handle.";
    const found = seatWithHandle(draft, seat);
    return found ? { key: found.key, kind: "seat" } : `There is no seat "${seat}" in this draft.`;
  }
  if (!text(unit)) return "That is not a unit's key.";
  const found = unitWithKey(draft, unit);
  return found ? { key: found.key, kind: "unit" } : `There is no unit "${unit}" in this draft.`;
}

/** The unit a change places something in: absent for the company itself. */
function parentOf(value: unknown, draft: Draft): NodeKey | string {
  if (value === undefined || value === null || value === "") return COMPANY_KEY;
  if (!text(value)) return "That is not a unit's key.";
  const found = unitWithKey(draft, value);
  return found ? found.key : `There is no unit "${value}" in this draft.`;
}

/** At the end of `parent`'s nodes of `kind`, leaving `except` out (a move's own node). */
function atEnd(draft: Draft, parent: NodeKey, kind: "seat" | "unit", except?: NodeKey): Placement {
  const host = parent === COMPANY_KEY ? null : locate(draft, parent);
  const unit = host?.kind === "unit" ? host.node : null;
  const nodes =
    kind === "seat" ? (unit ? unit.roles : draft.roles) : unit ? unit.children : draft.units;
  const others = nodes.filter((n) => n.key !== except);
  return { parent, after: others.at(-1)?.key ?? null };
}

/** A field path: a few bounded segments. */
function pathOf(value: unknown): readonly string[] | null {
  if (!Array.isArray(value) || value.length === 0 || value.length > MAX_PATH) return null;
  return value.every(text) ? (value as string[]) : null;
}

/** One change as the builder's intent, against the draft as the changes before it left it. */
function intentOf(change: Raw, draft: Draft, keys: KeySource): Intent | string {
  switch (change.op) {
    case "add_seat":
    case "add_unit": {
      const extra = onlyKeys(change, ["op", "in", "data"]);
      if (extra) return extra;
      const parent = parentOf(change.in, draft);
      if (parent !== COMPANY_KEY && locate(draft, parent) === undefined) return parent;
      const data = dataOf(change.data);
      if (!data || !text(data.name)) {
        return "A new seat or unit needs its object, with a name, of at most 8 KiB.";
      }
      const kind = change.op === "add_seat" ? "seat" : "unit";
      const key = mintKey(keys);
      return kind === "seat"
        ? {
            type: "addSeat",
            key,
            placement: atEnd(draft, parent, "seat"),
            data: data as ConfigRole,
          }
        : {
            type: "addUnit",
            key,
            placement: atEnd(draft, parent, "unit"),
            data: data as ConfigUnit,
          };
    }
    case "remove": {
      const extra = onlyKeys(change, ["op", "seat", "unit"]);
      if (extra) return extra;
      const node = nodeOf(change, draft);
      return typeof node === "string" ? node : { type: "remove", target: node.key };
    }
    case "rename": {
      const extra = onlyKeys(change, ["op", "seat", "unit", "name"]);
      if (extra) return extra;
      const node = nodeOf(change, draft);
      if (typeof node === "string") return node;
      if (!text(change.name)) return "A rename needs the new name.";
      return node.kind === "seat"
        ? { type: "renameSeat", target: node.key, name: change.name }
        : { type: "renameUnit", target: node.key, name: change.name };
    }
    case "move": {
      const extra = onlyKeys(change, ["op", "seat", "unit", "to"]);
      if (extra) return extra;
      const node = nodeOf(change, draft);
      if (typeof node === "string") return node;
      const parent = parentOf(change.to, draft);
      if (parent !== COMPANY_KEY && locate(draft, parent) === undefined) return parent;
      return { type: "move", target: node.key, to: atEnd(draft, parent, node.kind, node.key) };
    }
    case "set_lead": {
      const extra = onlyKeys(change, ["op", "unit", "lead"]);
      if (extra) return extra;
      const node = nodeOf(change, draft);
      if (typeof node === "string") return node;
      if (change.lead !== undefined && change.lead !== null && !text(change.lead)) {
        return "A lead is a seat's handle, or nothing to clear it.";
      }
      return {
        type: "setLead",
        target: node.key,
        ...(typeof change.lead === "string" ? { lead: change.lead } : {}),
      };
    }
    case "update": {
      const extra = onlyKeys(change, ["op", "seat", "unit", "set"]);
      if (extra) return extra;
      const node = nodeOf(change, draft);
      if (typeof node === "string") return node;
      if (!Array.isArray(change.set) || change.set.length === 0 || change.set.length > 32) {
        return "An update names the fields it sets.";
      }
      const set: { path: readonly string[]; value?: unknown }[] = [];
      for (const field of change.set) {
        if (!isObject(field)) return "Each field set is a path and a value.";
        const fieldExtra = onlyKeys(field, ["path", "value"]);
        if (fieldExtra) return fieldExtra;
        const path = pathOf(field.path);
        if (!path) return "A field's path is a few names.";
        if (!("value" in field)) {
          set.push({ path });
          continue;
        }
        const value = dataOf({ value: field.value });
        if (!value) return "A field's value is plain data of at most 8 KiB.";
        set.push({ path, value: value.value });
      }
      return node.kind === "seat"
        ? { type: "updateSeat", target: node.key, set }
        : { type: "updateUnit", target: node.key, set };
    }
    default:
      return `"${String(change.op)}" is not a change the builder makes.`;
  }
}

/** A name, a handle or a key, as [text] holds one. */
const NAME: AssistantActionArgs = { type: "string", minLength: 1, maxLength: MAX_TEXT };

/** One operation's shape: its `op`, pinned, and the members it carries. */
function op(
  name: string,
  description: string,
  members: Record<string, AssistantActionArgs>,
  required: readonly string[],
): AssistantActionArgs {
  return {
    type: "object",
    description,
    additionalProperties: false,
    required: ["op", ...required],
    properties: { op: { type: "string", const: name }, ...members },
  };
}

const SEAT = { ...NAME, description: "The seat's handle." };
const UNIT = { ...NAME, description: "The unit's key." };

/**
 * The operations [intentOf] reads, each with exactly the members it reads, so
 * the model is told the vocabulary rather than guessing it. A remove, a rename
 * or a move names one seat or one unit, which the builder's own parse decides.
 */
const ORG_OPS: AssistantActionArgs = {
  type: "object",
  description:
    "Changes to the org chart draft, applied in order through the builder. Seats by " +
    "handle, units by key. Nothing is saved until the person saves the draft.",
  additionalProperties: false,
  required: ["ops"],
  properties: {
    ops: {
      type: "array",
      minItems: 1,
      maxItems: ORG_DRAFT_MAX_OPS,
      items: {
        oneOf: [
          op(
            "add_seat",
            "Add a seat. `in` is the unit's key, or leave it out for the company.",
            { in: UNIT, data: { type: "object", description: "The seat, with name and handle." } },
            ["data"],
          ),
          op(
            "add_unit",
            "Add a unit. `in` is the parent unit's key, or leave it out for the company.",
            { in: UNIT, data: { type: "object", description: "The unit, with name and id." } },
            ["data"],
          ),
          op("remove", "Remove one seat or one unit.", { seat: SEAT, unit: UNIT }, []),
          op("rename", "Rename one seat or one unit.", { seat: SEAT, unit: UNIT, name: NAME }, [
            "name",
          ]),
          op(
            "move",
            "Move one seat or unit into the unit `to`, or to the company.",
            { seat: SEAT, unit: UNIT, to: UNIT },
            [],
          ),
          op(
            "set_lead",
            "Set a unit's lead to a seat's handle, or null to clear it.",
            { unit: UNIT, lead: { type: ["string", "null"], maxLength: MAX_TEXT } },
            ["unit"],
          ),
          op(
            "update",
            "Set fields of one seat or unit, each by its path; a field with no value is removed.",
            {
              seat: SEAT,
              unit: UNIT,
              set: {
                type: "array",
                minItems: 1,
                maxItems: 32,
                items: {
                  type: "object",
                  additionalProperties: false,
                  required: ["path"],
                  properties: {
                    path: { type: "array", minItems: 1, maxItems: MAX_PATH, items: NAME },
                    value: { description: "The field's new value." },
                  },
                },
              },
            },
            ["set"],
          ),
        ],
      },
    },
  },
};

/** A handle no seat of the draft holds, for an example that adds one. */
function freshHandle(draft: Draft): string {
  for (let n = 1; ; n++) {
    const handle = n === 1 ? "technical-writer" : `technical-writer-${n}`;
    if (!seatWithHandle(draft, handle)) return handle;
  }
}

/**
 * What `org.draft.apply_ops` takes on this draft: [ORG_OPS], with an example
 * written against the draft itself (its first unit, a handle it does not
 * hold), so the example is one the builder takes as it stands.
 */
export function orgOpsArgs(draft: Draft): AssistantActionArgs {
  const first = allUnits(draft).next();
  const unit = first.done ? undefined : unitIdOf(first.value.unit);
  return {
    ...ORG_OPS,
    examples: [
      {
        ops: [
          {
            op: "add_seat",
            ...(unit ? { in: unit } : {}),
            data: { name: "Technical Writer", handle: freshHandle(draft) },
          },
        ],
      },
    ],
  };
}

/**
 * Turn what the model proposed into the builder's intents, each recorded
 * against the draft the ones before it leave, or refuse the whole set in the
 * builder's own words. Nothing is dispatched.
 */
export function parseOrgOps(
  raw: unknown,
  state: BuilderState,
  keys: KeySource,
): PageActionParse<OrgDraftChange> {
  if (!isObject(raw) || !Array.isArray(raw.ops) || Object.keys(raw).some((k) => k !== "ops")) {
    return { ok: false, reason: "That is not a list of changes to the draft." };
  }
  if (raw.ops.length === 0) return { ok: false, reason: "That changes nothing." };
  if (raw.ops.length > ORG_DRAFT_MAX_OPS) {
    return { ok: false, reason: `At most ${ORG_DRAFT_MAX_OPS} changes at once.` };
  }
  let at = state;
  const intents: Intent[] = [];
  const words: string[] = [];
  for (const [i, change] of raw.ops.entries()) {
    const n = i + 1;
    if (!isObject(change)) return { ok: false, reason: `Change ${n} is not a change.` };
    const intent = intentOf(change, at.draft, keys);
    if (typeof intent === "string") return { ok: false, reason: `Change ${n}: ${intent}` };
    const recorded = recordIntent(at, intent);
    if (!recorded.ok) return { ok: false, reason: `Change ${n}: ${recorded.message}` };
    words.push(describeOperation(recorded.op, at.draft));
    intents.push(intent);
    at = builderReducer(at, { type: "record", intent });
  }
  return { ok: true, value: { intents, words } };
}

/** What the assistant is told about the builder, beside its draft's fields. */
const ORG_HELP =
  "The person is editing the org chart as a draft. Change it with org.draft.apply_ops, " +
  "naming seats by handle and units by key; read the configuration for a node's full " +
  "values. Nothing is saved until the person reviews and saves the draft.";

/** Each operation of the draft's log, in the builder's own words, each against the draft before it. */
export function draftChanges(state: BuilderState): string[] {
  let before = state.baseDraft;
  return state.log.ops.map((op) => {
    const words = describeOperation(op, before);
    before = applyOperation(before, op).draft;
    return words;
  });
}

/**
 * The builder's draft as the page declares it (`useAssistantDraft`): what is
 * being edited, read only. A change to the org is a structural one, which
 * `org.draft.apply_ops` makes through the builder's one door, so no field
 * here is set by `draft.set`.
 */
export function orgDraftSpec(args: {
  state: BuilderState;
  /** The open view (`visualization`, `table`). */
  view: string;
  /** The selected node's name, or null. */
  selected: string | null;
}): AssistantDraftSpec<BuilderState> {
  const { state, view, selected } = args;
  const count = <T>(items: Iterable<T>) => String([...items].length);
  return {
    kind: "client",
    id: state.base.revision === null ? null : "company",
    revision: state.base.revision,
    step: view,
    scope: "This org chart draft (not saved)",
    help: ORG_HELP,
    value: state,
    fields: [
      {
        path: "scope",
        label: "Editing",
        read: (s) => (s.scope === null ? "The whole company" : `The unit ${s.scope}`),
      },
      {
        path: "changes",
        label: "Changes not saved",
        read: draftChanges,
      },
      { path: "units", label: "Units", read: (s) => count(allUnits(s.draft)) },
      { path: "seats", label: "Seats", read: (s) => count(allSeats(s.draft)) },
      { path: "selected", label: "Selected", read: () => selected ?? "Nothing" },
    ],
    // NOTHING HERE IS SET (see above): no apply, so the page takes no
    // draft.set at all.
  };
}

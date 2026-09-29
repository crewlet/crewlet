/**
 * The operation log: undo, redo, replay, and rebase onto a newer revision.
 *
 * THE DRAFT IS A FUNCTION OF ITS BASE AND ITS LOG. Nothing edits a draft in
 * place: an undo replays the remaining operations from the base, and a redo
 * applies the operation it restores to the draft it was recorded on. So what
 * the canvas shows, what a check sends and what a save writes are always
 * `replay(base, ops)`, and the log is the whole of the operator's work: it is
 * what persists across a reload and what survives a newer revision.
 *
 * A REBASE SORTS, IT NEVER OVERWRITES. When the base moves (another operator
 * saved, or a restored log meets a different revision) every operation is
 * evaluated, in order, against the newer base with the operations before it
 * already applied, and lands in one of three outcomes:
 *
 * - it applies: every precondition still holds, so it is applied as recorded;
 * - its target is gone: the node, the parent it moves into or the schedule it
 *   toggles no longer exists, so it is dropped and reported;
 * - it conflicts: a value it recorded differs upstream, so it is held with the
 *   base value, their value and this operation's value, and a person chooses.
 *   "Keep theirs" drops it. "Keep mine" records the same intent again against
 *   the draft as it stands, which captures THEIR value as the new
 *   precondition, and applies that.
 *
 * A held conflict is not applied, and the operations after it are evaluated
 * without it, so one that depends on it (an edit of a seat a conflicted
 * operation added) reports its target gone until the conflict is resolved.
 * Choices are keyed by the operation's position in the original log, and a
 * rebase is a pure function of the base, the log and the choices, so the
 * review re-runs it after every choice and always shows the outcome the
 * confirmation will adopt.
 *
 * WHAT THE BASE ALREADY SAYS IS NOT A CONFLICT. An operation whose every
 * differing value differs only because the base already holds THIS
 * operation's own value — a save that landed part of the draft and failed on
 * the rest, or a colleague who made the same change — is not held for a
 * person to choose: it is recorded again over the values that are there, which
 * keeps whatever part of it the base does not hold yet (a kind already set
 * whose forbidden fields are still there) and drops it when nothing is left.
 * The review lists it as already in the chart.
 *
 * AN ADD THE CHART ALREADY HOLDS is the one conflict with a third reading.
 * The chart names nodes by address and never holds two under one, so a seat
 * this draft created that the chart now holds under the same handle is either
 * this draft's own (a save that created it, then failed) or a colleague's.
 * "Keep theirs" drops the add, and every later operation on the node it would
 * have made reports its target gone. "Keep mine" writes this draft's node
 * over the one that holds the address ([restateOnto]) and RE-KEYS every later
 * operation from the key this draft minted to that node's address, so an edit
 * of the created seat lands on the seat the chart holds.
 *
 * A SAVE THAT FAILED PART WAY names its own creations ([rebase]'s `aliases`):
 * each node this draft created that the save did create, by the key the draft
 * minted and the address the chart now holds it under. That is its FINAL
 * address, which need not be the one its add recorded (a created seat's handle
 * can change after the add), so the add is resolved onto that node before
 * anything is evaluated, and listed as already in the chart.
 *
 * WHAT "THE SAME ENTITY" MEANS is the chart's identity, carried by the key
 * (see `keys.ts`): a seat's handle and a unit's key. An operation recorded
 * against a seat never lands on a different seat that merely shares its name,
 * because a name is prose and the key is the address.
 *
 * The redo stack does not survive a rebase: its operations were recorded
 * against a draft that no longer exists, and nothing in the review offers to
 * choose for them.
 */

import { jsonEqual } from "./json.ts";
import type { NodeKey } from "./keys.ts";
import { locate, type Draft } from "./draft.ts";
import {
  apply,
  evaluate,
  holderOf,
  intentOf,
  record,
  rekeyOperation,
  restateOnto,
  type AddSeat,
  type AddUnit,
  type ApplyReport,
  type Conflict,
  type Operation,
} from "./operations.ts";

/** Every operation applied, and every operation undone (most recent last). */
export interface Log {
  readonly ops: readonly Operation[];
  readonly undone: readonly Operation[];
}

export const EMPTY_LOG: Log = { ops: [], undone: [] };

/** The log with a new operation at its end. Recording anything new forgets what was undone. */
export function pushOperation(log: Log, op: Operation): Log {
  return { ops: [...log.ops, op], undone: [] };
}

/** The log with its last operation undone, and that operation; `undefined` when there is none. */
export function undoOperation(log: Log): { log: Log; op: Operation } | undefined {
  const op = log.ops[log.ops.length - 1];
  if (op === undefined) return undefined;
  return { log: { ops: log.ops.slice(0, -1), undone: [...log.undone, op] }, op };
}

/** The log with its most recently undone operation restored, and that operation. */
export function redoOperation(log: Log): { log: Log; op: Operation } | undefined {
  const op = log.undone[log.undone.length - 1];
  if (op === undefined) return undefined;
  return { log: { ops: [...log.ops, op], undone: log.undone.slice(0, -1) }, op };
}

/** A log replayed without a failure: the draft, and what each operation did. */
export interface Replayed {
  readonly draft: Draft;
  /** One per operation, in log order. */
  readonly reports: readonly ApplyReport[];
}

/** An operation of a log did not apply to the draft the operations before it produced. */
export class ReplayError extends Error {
  readonly index: number;
  readonly op: Operation;
  constructor(index: number, op: Operation, cause: unknown) {
    super(
      `replay: operation ${index} (${op.type}) does not apply: ${cause instanceof Error ? cause.message : String(cause)}`,
    );
    this.name = "ReplayError";
    this.index = index;
    this.op = op;
  }
}

/**
 * The draft a log produces from its base.
 *
 * For a log that was recorded on this base (an undo, a redo, the draft a save
 * sends), every operation applies by construction, so a failure is a defect
 * and throws [ReplayError]. A log whose base may have moved goes through
 * [rebase] instead, which reports rather than throws.
 */
export function replay(base: Draft, ops: readonly Operation[]): Replayed {
  let draft = base;
  const reports: ApplyReport[] = [];
  ops.forEach((op, index) => {
    try {
      const applied = apply(draft, op);
      draft = applied.draft;
      reports.push(applied.report);
    } catch (err) {
      throw new ReplayError(index, op, err);
    }
  });
  return { draft, reports };
}

/** How a person resolved one conflict. */
export type Choice = "mine" | "theirs";

/** What became of one operation of the log. */
export type RebaseEntry =
  | {
      readonly outcome: "applies";
      /** Position in the log that was rebased. */
      readonly index: number;
      /** The operation as it was applied: re-keyed where an earlier add was resolved onto a node. */
      readonly op: Operation;
    }
  | {
      readonly outcome: "gone";
      readonly index: number;
      readonly op: Operation;
      /** A sentence saying what is no longer there. */
      readonly reason: string;
    }
  | {
      /**
       * Every value it recorded differs only because the base already holds
       * this operation's own value. `resolved` is what was left to apply,
       * recorded again over the base's values; absent when nothing was.
       */
      readonly outcome: "already";
      readonly index: number;
      readonly op: Operation;
      readonly conflicts: readonly Conflict[];
      readonly resolved?: readonly Operation[];
    }
  | {
      readonly outcome: "conflict";
      readonly index: number;
      readonly op: Operation;
      readonly conflicts: readonly Conflict[];
      /** Absent until a person chooses. */
      readonly choice?: Choice;
      /**
       * For "mine": the operations recorded again over their values (for an
       * add the chart holds, the ones that write this draft's node over it),
       * or absent when their values already are this operation's, so nothing
       * is left to apply.
       */
      readonly resolved?: readonly Operation[];
    };

/** A rebased log. */
export interface Rebased {
  /** The newer base with every applied operation on it. */
  readonly draft: Draft;
  /** The operations the draft carries, in order: the log to adopt. */
  readonly ops: readonly Operation[];
  /** One per applied operation, aligned with `ops`. */
  readonly reports: readonly ApplyReport[];
  /** One per operation of the original log. */
  readonly entries: readonly RebaseEntry[];
  /** How many conflicts still wait for a choice. The rebase can be adopted only at zero. */
  readonly pending: number;
  /**
   * The minted key of every add resolved onto a node the base already held,
   * and that node's key: what a surface still holding the minted key (a
   * selection, an open dialog) reads the node through.
   */
  readonly rekeyed: ReadonlyMap<NodeKey, NodeKey>;
}

/** The node an add's address conflict names, in the draft it was evaluated against. */
function addressHolder(draft: Draft, op: AddSeat | AddUnit): NodeKey | undefined {
  const kind = op.type === "addSeat" ? "seat" : "unit";
  const address = op.type === "addSeat" ? op.data.handle : op.data.key;
  return holderOf(draft, kind, address)?.node.key;
}

/**
 * Sorts a log against a newer base, applying what still applies and the
 * conflicts a person resolved. See the module doc for the rules.
 *
 * `aliases` maps the key this draft minted for a node a save created to that
 * node's key in `base` (see the module doc).
 */
export function rebase(
  base: Draft,
  ops: readonly Operation[],
  choices: ReadonlyMap<number, Choice> = new Map(),
  aliases: ReadonlyMap<NodeKey, NodeKey> = new Map(),
): Rebased {
  let draft = base;
  const applied: Operation[] = [];
  const reports: ApplyReport[] = [];
  const entries: RebaseEntry[] = [];
  const rekeyed = new Map<NodeKey, NodeKey>();
  let pending = 0;

  const adopt = (op: Operation) => {
    const next = apply(draft, op);
    draft = next.draft;
    applied.push(op);
    reports.push(next.report);
  };
  /** The same request, recorded over the values that are there now, and applied. */
  const recordAgain = (op: Operation): Operation[] | string | null => {
    const again = record(draft, intentOf(op));
    if (again.ok) {
      adopt(again.op);
      return [again.op];
    }
    return again.refusal === "no_change" ? null : again.message;
  };

  ops.forEach((original, index) => {
    const op = rekeyOperation(original, rekeyed);
    const alias = op.type === "addSeat" || op.type === "addUnit" ? aliases.get(op.key) : undefined;
    if (alias !== undefined && (op.type === "addSeat" || op.type === "addUnit")) {
      if (locate(draft, alias)) {
        const resolved = restateOnto(draft, op, alias);
        for (const part of resolved) adopt(part);
        rekeyed.set(op.key, alias);
        entries.push({
          outcome: "already",
          index,
          op,
          conflicts: [],
          ...(resolved.length > 0 ? { resolved } : {}),
        });
        return;
      }
    }
    const outcome = evaluate(draft, op);
    if (outcome.kind === "applies") {
      adopt(op);
      entries.push({ outcome: "applies", index, op });
      return;
    }
    if (outcome.kind === "gone") {
      entries.push({ outcome: "gone", index, op, reason: outcome.reason });
      return;
    }
    const { conflicts } = outcome;
    const already = conflicts.every((c) => jsonEqual(c.theirs, c.mine));
    const isAdd = op.type === "addSeat" || op.type === "addUnit";
    const holder = isAdd && conflicts.some((c) => c.address) ? addressHolder(draft, op) : undefined;
    const choice = already ? "mine" : choices.get(index);

    if (choice === undefined) {
      pending++;
      entries.push({ outcome: "conflict", index, op, conflicts });
      return;
    }
    if (choice === "theirs") {
      entries.push({ outcome: "conflict", index, op, conflicts, choice });
      return;
    }
    let resolved: Operation[] | string | null;
    if (isAdd && holder !== undefined) {
      // Keep mine of an add the chart holds: write this draft's node over the
      // one holding its address, and let every later operation on the node
      // this add would have made name that one.
      resolved = restateOnto(draft, op, holder);
      for (const part of resolved) adopt(part);
      rekeyed.set(op.key, holder);
      if (resolved.length === 0) resolved = null;
    } else {
      resolved = recordAgain(op);
    }
    if (typeof resolved === "string") {
      entries.push({ outcome: "gone", index, op, reason: resolved });
      return;
    }
    const outcomeKind = already ? "already" : "conflict";
    entries.push(
      outcomeKind === "already"
        ? { outcome: "already", index, op, conflicts, ...(resolved ? { resolved } : {}) }
        : { outcome: "conflict", index, op, conflicts, choice, ...(resolved ? { resolved } : {}) },
    );
  });

  return { draft, ops: applied, reports, entries, pending, rekeyed };
}

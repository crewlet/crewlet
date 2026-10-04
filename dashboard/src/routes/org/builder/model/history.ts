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
 * WHAT "THE SAME ENTITY" MEANS is the engine's identity, carried by the key
 * (see `keys.ts`): a seat's handle and a unit's name. An operation recorded
 * against a seat never lands on a different seat that merely shares its name,
 * because a different seat has a different handle and therefore a different
 * key. A seat the engine gives the SAME handle is, to every subsystem that
 * attaches to a seat (its memory, its mailbox, its masked credentials), the
 * same seat, and an operation on it is held to its recorded values like any
 * other.
 *
 * The redo stack does not survive a rebase: its operations were recorded
 * against a draft that no longer exists, and nothing in the review offers to
 * choose for them.
 */

import type { Draft } from "./draft.ts";
import {
  apply,
  evaluate,
  intentOf,
  record,
  type ApplyReport,
  type Conflict,
  type Operation,
  type RecordContext,
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
      readonly outcome: "conflict";
      readonly index: number;
      readonly op: Operation;
      readonly conflicts: readonly Conflict[];
      /** Absent until a person chooses. */
      readonly choice?: Choice;
      /**
       * For "mine": the operation recorded again over their values, or absent
       * when their values already are this operation's, so nothing is left to
       * apply.
       */
      readonly resolved?: Operation;
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
}

/**
 * Sorts a log against a newer base, applying what still applies and the
 * conflicts a person resolved. See the module doc for the rules.
 *
 * `ctx` is the recording context of the NEWER base (the handles its last
 * check reported), used when "keep mine" records an operation again.
 */
export function rebase(
  base: Draft,
  ops: readonly Operation[],
  choices: ReadonlyMap<number, Choice> = new Map(),
  ctx: RecordContext = {},
): Rebased {
  let draft = base;
  const applied: Operation[] = [];
  const reports: ApplyReport[] = [];
  const entries: RebaseEntry[] = [];
  let pending = 0;

  const adopt = (op: Operation) => {
    const next = apply(draft, op);
    draft = next.draft;
    applied.push(op);
    reports.push(next.report);
  };

  ops.forEach((op, index) => {
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
    const choice = choices.get(index);
    if (choice === undefined) {
      pending++;
      entries.push({ outcome: "conflict", index, op, conflicts: outcome.conflicts });
      return;
    }
    if (choice === "theirs") {
      entries.push({ outcome: "conflict", index, op, conflicts: outcome.conflicts, choice });
      return;
    }
    // Keep mine: the same request, recorded over the values that are there
    // now. A placement whose neighbour moved lands at the end of the list,
    // which is the only reading of "put it where I put it" a changed list
    // still supports.
    const again = record(draft, intentOf(op), ctx, { lenientPlacement: true });
    if (again.ok) {
      adopt(again.op);
      entries.push({
        outcome: "conflict",
        index,
        op,
        conflicts: outcome.conflicts,
        choice,
        resolved: again.op,
      });
    } else if (again.refusal === "no_change") {
      entries.push({ outcome: "conflict", index, op, conflicts: outcome.conflicts, choice });
    } else {
      entries.push({ outcome: "gone", index, op, reason: again.message });
    }
  });

  return { draft, ops: applied, reports, entries, pending };
}

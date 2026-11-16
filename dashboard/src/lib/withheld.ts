/**
 * What a reader is told in place of the arguments, and the result, of a call
 * whose arguments no record holds.
 *
 * SEALED: the two executor calls whose arguments no record holds in the clear,
 * a call the run HELD for a person's approval and the call they APPROVED. Both
 * ride the run's row sealed (ADR-0077), so `(none)` or `{}`, what the records
 * carry for them, would say a call that sent arguments sent nothing.
 *
 * NOT KNOWN: a call whose arguments are in text that could not be read, or an
 * executor run's call still in flight as its round was read, whose entry kept
 * none (`ArgumentsUnknown`). Drawn as `{}` or `(none)`, it read as the tool
 * called with none, a different call, which the engine's own ledger already
 * says in words for the model (ADR-0073).
 *
 * ONE WORDING for the executors board's log, the turn's execute card and the
 * trace's call detail, so such a call reads the same wherever a reader meets
 * it, through [argumentsInPlace].
 */

import type { ArgumentsUnknown } from "~/contract/calls.ts";

/** The approved call's arguments: the resume made it off the sealed ones. */
export const SEALED_ARGUMENTS = "(sealed with the approval, not kept on the record)";

/** The approved call's result: handed to the job that carried the work on, sealed. */
export const SEALED_RESULT = "(handed to the run sealed, not kept on the record)";

/** The held call's arguments: sealed beside its question, for whoever may approve it. */
export const HELD_ARGUMENTS = "(sealed beside the run's question, for whoever may approve it)";

/**
 * The arguments of a call whose arguments are not known, by why. EXHAUSTIVE
 * over the engine's reasons (`toolloop.ArgsUnknown`), each saying they are
 * not known and why, since the two tell a reader different things: one in
 * text that could not be read was answered for it (its result says how) or
 * its record was damaged; one still running as its round was read may have
 * taken effect, which its result says to check.
 */
export const UNKNOWN_ARGUMENTS: Record<ArgumentsUnknown, string> = {
  unread: "(not known: sent in text that could not be read)",
  unkept:
    "(not known: the call was still running when its round was read, and no record keeps them)",
};

/**
 * The arguments of a call whose arguments are not known for a reason this
 * build has no words for: a newer engine's, read on an older node's screen.
 * Still not known, never `{}`.
 */
export const ARGUMENTS_NOT_KNOWN = "(not known: no record keeps them)";

/** The words for a call whose arguments are not known for `reason`. */
export function unknownArguments(reason: string): string {
  return Object.hasOwn(UNKNOWN_ARGUMENTS, reason)
    ? UNKNOWN_ARGUMENTS[reason as ArgumentsUnknown]
    : ARGUMENTS_NOT_KNOWN;
}

/**
 * What a reader is told in place of a call's arguments, or "" where the record
 * holds what the call was made with (a call made with none among them), which
 * is then drawn as it stands.
 */
export function argumentsInPlace(call: {
  held?: boolean;
  approved?: boolean;
  argsUnknown?: string;
}): string {
  if (call.approved) return SEALED_ARGUMENTS;
  if (call.held) return HELD_ARGUMENTS;
  if (call.argsUnknown) return unknownArguments(call.argsUnknown);
  return "";
}

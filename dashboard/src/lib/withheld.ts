/**
 * What a reader is told in place of the arguments, and the result, of the two
 * executor calls whose arguments no record holds in the clear: a call the run
 * HELD for a person's approval, and the call they APPROVED. Both ride the
 * run's row sealed (ADR-0077), so `(none)` or `{}` — what the records carry
 * for them — would say a call that sent arguments sent nothing.
 *
 * ONE WORDING for the executors board's log and the turn's execute card, so a
 * sealed call reads the same wherever a reader meets it.
 */

/** The approved call's arguments: the resume made it off the sealed ones. */
export const SEALED_ARGUMENTS = "(sealed with the approval, not kept on the record)";

/** The approved call's result: handed to the job that carried the work on, sealed. */
export const SEALED_RESULT = "(handed to the run sealed, not kept on the record)";

/** The held call's arguments: sealed beside its question, for whoever may approve it. */
export const HELD_ARGUMENTS = "(sealed beside the run's question, for whoever may approve it)";

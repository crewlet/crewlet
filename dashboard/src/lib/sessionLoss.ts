/**
 * A run's verdict on a session it did not continue, in the engine's one
 * sentence (`executorkeep.NotContinued.Sentence`): every surface that shows a
 * launch's verdict says it in these words, built from the contract's loss
 * clauses, which a Go gate holds to the engine's.
 */

import {
  SESSION_LOSS_WORDS,
  SESSION_UNNAMED_LOSS,
  type SessionInstead,
  type SessionLoss,
  type SessionNotContinued,
} from "~/contract/sessions.ts";

/** A loss's clause, or the words for one this build does not name. */
export function lossWords(loss: string): string {
  return Object.hasOwn(SESSION_LOSS_WORDS, loss)
    ? SESSION_LOSS_WORDS[loss as SessionLoss]
    : SESSION_UNNAMED_LOSS;
}

/** The verdict as the engine says it. Its detail is never in it. */
export function notContinuedSentence(nc: SessionNotContinued): string {
  const whose = nc.lost_turn ? `turn ${nc.lost_turn}'s session` : "its session";
  const said = `The run did not continue ${whose}: ${lossWords(nc.loss)}.`;
  if (nc.instead === "fresh") return `${said} It started a fresh session.`;
  if (nc.instead !== "older") return said;
  if (nc.older_turn) {
    return `${said} It continued turn ${nc.older_turn}'s older session instead, which ends before that work.`;
  }
  if (nc.older) {
    return (
      `${said} It continued an older capture of its turn (launch ${nc.older.launch_seq}, ` +
      `capture ${nc.older.capture_seq}) instead, which ends before that work.`
    );
  }
  return `${said} It continued an older session instead, which ends before that work.`;
}

/** The verdict a launch event carries, or null where it carries none. */
export function notContinuedOf(value: unknown): SessionNotContinued | null {
  if (value === null || typeof value !== "object") return null;
  const v = value as Record<string, unknown>;
  if (typeof v.loss !== "string" || typeof v.instead !== "string") return null;
  const older = v.older as { launch_seq?: unknown; capture_seq?: unknown } | undefined;
  return {
    loss: v.loss as SessionLoss,
    instead: v.instead as SessionInstead,
    lost_turn: typeof v.lost_turn === "string" ? v.lost_turn : undefined,
    older_turn: typeof v.older_turn === "string" ? v.older_turn : undefined,
    older:
      older && typeof older.launch_seq === "number" && typeof older.capture_seq === "number"
        ? { launch_seq: older.launch_seq, capture_seq: older.capture_seq }
        : undefined,
    detail: typeof v.detail === "string" ? v.detail : undefined,
  };
}

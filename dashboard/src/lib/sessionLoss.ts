/**
 * A run's verdict on a session it did not continue, in the engine's one
 * sentence (`executorkeep.NotContinued.Sentence`): every surface that shows a
 * launch's verdict says it in these words, built from the contract's loss
 * clauses, which a Go gate holds to the engine's.
 */

import type { EventRecord } from "~/protocol/index.ts";
import { tsKey } from "./format.ts";
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

/** One launch's verdict, as its turn's events say it. */
export interface LaunchVerdict {
  /** The launch it is of (`launch_id`). */
  launchId: string;
  verdict: SessionNotContinued;
  /** Whether its collection said it (`executor_collected`): the final verdict,
   *  which a box whose restore fell back turns into `restore_failed`. False
   *  while the launch runs, said as it started (`executor_launched`). */
  collected: boolean;
}

/**
 * Each launch of a turn that did not continue a session, in the order the
 * launches started, off the turn's `executor_launched` and `executor_collected`
 * events: the collected record's final verdict where the launch was collected,
 * else what the launch decided as it started, so a run still working says it
 * too. A launch whose collection lost nothing is absent, whatever it started
 * with. An event that names no launch is passed over: the engine names the
 * launch on both, and one named by anything else could not be paired with its
 * collection, so it would say one launch twice.
 */
export function turnVerdicts(events: readonly EventRecord[] | undefined): LaunchVerdict[] {
  // A Map keeps the order its keys were first set in: the order the launches
  // started. Null is a launch that lost nothing.
  const launches = new Map<string, LaunchVerdict | null>();
  const final = new Set<string>();
  const sorted = [...(events ?? [])].sort((a, b) => tsKey(a.timestamp) - tsKey(b.timestamp));
  for (const ev of sorted) {
    if (ev.type !== "executor_launched" && ev.type !== "executor_collected") continue;
    const p = (ev.payload ?? {}) as Record<string, unknown>;
    const launchId = typeof p.launch_id === "string" ? p.launch_id : "";
    if (!launchId) continue;
    const collected = ev.type === "executor_collected";
    if (!collected && final.has(launchId)) continue;
    if (collected) final.add(launchId);
    const verdict = notContinuedOf(p.session_not_continued);
    launches.set(launchId, verdict ? { launchId, verdict, collected } : null);
  }
  return [...launches.values()].filter((v): v is LaunchVerdict => v !== null);
}

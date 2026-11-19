/**
 * Applying a finding's fix: re-checked first, sent under the revision the
 * re-check read, then told to the engine, which checks its effect.
 *
 * # Why a re-check before every send
 *
 * A finding may sit unread for days while the engine moves under it, so what
 * was true and safe when it was found may be neither now. Apply therefore
 * asks the engine to derive the finding again from the latest records and
 * rebuild its fix (`POST /advisor/findings/{id}/recheck`), and draws what it
 * answers:
 *
 * - `holds`: the same condition and the same change, with the preview of
 *   what sending it does now;
 * - `changed`: the condition holds but its numbers or its change moved, so
 *   each moved value is shown, was beside now, with the updated change, and
 *   nothing is sent until the person confirms that one;
 * - `resolved`: the condition no longer holds, so the finding is closed and
 *   nothing is sent;
 * - `not_applicable`: what it was about is gone, or its change can no longer
 *   be built, so it is retired and nothing is sent.
 *
 * A re-check answers for ten minutes (`expires_at`): an older answer is asked
 * again before anything is sent, and sent only where the engine answers the
 * same change (its digest) again; any other answer is shown in its place.
 *
 * # Why the person's session sends it
 *
 * The engine never sends a fix (ADR-0115). The change goes from this session,
 * under the route's own authority, after its dry run, with the `If-Match` of
 * the exact revision the re-check read: a target that moved between the
 * re-check and the send is refused (412) rather than overwritten, and the
 * finding is re-checked again and shown. The engine is then told what the
 * send came to (`applied`), and checks the change's effect itself.
 */

import { RestError, pathSegment, rest } from "~/protocol/rest.ts";
import type {
  AdvisorAppliedReport,
  AdvisorKeptFinding,
  AdvisorRecheck,
  AdvisorSend,
} from "~/protocol/advisorFindings.ts";

/** Where an Apply stands. */
export type ApplyState =
  | { kind: "checking" }
  /** The engine answered the re-check; the person decides from it. */
  | { kind: "checked"; check: AdvisorRecheck }
  | { kind: "sending"; check: AdvisorRecheck }
  /** The change was sent and the engine took it: its effect is being checked. */
  | { kind: "applied"; finding: AdvisorKeptFinding }
  /** The re-check, the send or the report failed in a way a re-check does not answer. */
  | { kind: "failed"; message: string; check: AdvisorRecheck | null };

/** Ask the engine whether the finding still holds, now, with its fix's `option` built again. */
export async function recheck(
  id: string,
  option: number,
  signal?: AbortSignal,
): Promise<AdvisorRecheck> {
  const answer = await rest.request("POST", `/advisor/findings/${pathSegment(id)}/recheck`, {
    body: { option },
    ...(signal ? { signal } : {}),
  });
  return answer.body as AdvisorRecheck;
}

/** A send the target refused because it moved since the re-check read it. */
export class MovedSinceRecheck extends Error {
  constructor() {
    super("What the change was built on moved since it was checked.");
  }
}

/**
 * Send the change the re-check built: its dry run first, where its route has
 * one, then the change itself, each under the re-check's revision.
 */
export async function sendFix(send: AdvisorSend, signal?: AbortSignal): Promise<void> {
  const options = {
    ...(send.body === undefined || send.body === null ? {} : { body: send.body }),
    headers: send.headers,
    ...(signal ? { signal } : {}),
  };
  try {
    if (send.dry_run_path) await rest.request(send.method, send.dry_run_path, options);
    await rest.request(send.method, send.path, options);
  } catch (err) {
    if (err instanceof RestError && err.status === 412) throw new MovedSinceRecheck();
    throw err;
  }
}

/** The engine was told of the send and holds a newer revision: re-check before anything else. */
export class RecheckFirst extends Error {
  constructor() {
    super("The finding moved since it was checked.");
  }
}

/**
 * Tell the engine the change was sent: it reads the target itself and marks
 * the finding applied, its effect to be checked.
 */
export async function reportApplied(
  id: string,
  digest: string,
  signal?: AbortSignal,
): Promise<AdvisorKeptFinding> {
  const report: AdvisorAppliedReport = { digest };
  try {
    const reply = await rest.request("POST", `/advisor/findings/${pathSegment(id)}/applied`, {
      body: report,
      ...(signal ? { signal } : {}),
    });
    return reply.body as AdvisorKeptFinding;
  } catch (err) {
    if (err instanceof RestError && err.status === 409) throw new RecheckFirst();
    throw err;
  }
}

/** Whether a re-check's answer may still be sent from, at `now`. */
export function fresh(check: AdvisorRecheck, now: number): boolean {
  const until = Date.parse(check.expires_at);
  return Number.isFinite(until) && now < until;
}

/** Whether a re-check's answer offers a change to send. */
export function sendable(check: AdvisorRecheck): boolean {
  return (
    (check.outcome === "holds" || check.outcome === "changed") &&
    check.send !== null &&
    check.refusal === null
  );
}

/** A failure in the person's words: the engine's sentence where it wrote one. */
export function failureWords(err: unknown): string {
  if (
    err instanceof MovedSinceRecheck ||
    err instanceof RecheckFirst ||
    err instanceof SentUntold
  ) {
    return err.message;
  }
  if (err instanceof RestError) {
    return err.detail || err.sentence || err.hint || "The engine refused the change.";
  }
  return "Couldn't reach the engine. Try again in a moment.";
}

/**
 * Send the change a re-check answered, at `now`, and say what came of it:
 * applied, or the fresh re-check to show the person in place of the one they
 * pressed on.
 *
 * A STALE ANSWER IS ASKED AGAIN, and sent only where the engine answers the
 * same change (its digest) again. A target that moved since the re-check
 * (412), or an engine that holds a newer revision of the finding (409), is
 * re-checked once and that answer shown: the person always decides on what is
 * true now, and is never sent a change they were not shown.
 */
export async function applyFrom(
  id: string,
  option: number,
  pressed: AdvisorRecheck,
  now: () => number,
  signal?: AbortSignal,
): Promise<
  { kind: "applied"; finding: AdvisorKeptFinding } | { kind: "rechecked"; check: AdvisorRecheck }
> {
  let check = pressed;
  if (!fresh(check, now())) {
    const again = await recheck(id, option, signal);
    if (again.digest !== pressed.digest || !sendable(again))
      return { kind: "rechecked", check: again };
    check = again;
  }
  try {
    await sendFix(check.send!, signal);
  } catch (err) {
    if (err instanceof MovedSinceRecheck)
      return { kind: "rechecked", check: await recheck(id, option, signal) };
    throw err;
  }
  try {
    return { kind: "applied", finding: await reportApplied(id, check.digest, signal) };
  } catch (err) {
    if (err instanceof RecheckFirst)
      return { kind: "rechecked", check: await recheck(id, option, signal) };
    throw new SentUntold();
  }
}

/**
 * The change landed but the engine could not be told: nothing is lost, since
 * the finding's next re-derivation finds the change on its own.
 */
export class SentUntold extends Error {
  constructor() {
    super(
      "The change was saved, but the Advisor could not be told. Its next check finds the change on its own.",
    );
  }
}

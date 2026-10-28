/**
 * A question the engine turned away unsent: whether it goes back in the box,
 * whether the box waits, and what the refusal's notice says about both.
 *
 * ONE RULE FOR THE THREE, because they are one promise. The notice used to say
 * "your question is back in the box below" wherever the engine named a wait,
 * while the drawer put the question back for one code alone, so a spent budget
 * promised a restore that never came and left the box empty. Here the turn
 * that bounced ([bouncedOf]) is the one the drawer restores, and the notice
 * says the question is back only where the drawer reports it put it there.
 *
 * EVERY REFUSED QUESTION GOES BACK. A refusal is an answer to the request,
 * before any turn started, so nothing of the question was kept anywhere; what
 * the person wrote is theirs to send again once the reason passes, whatever
 * the reason was.
 *
 * ONLY THE PERSON'S OWN QUESTION LIMIT HOLDS THE BOX SHUT ([locksTheBox]). Its
 * wait is the one thing that clears it. A spent budget clears as soon as
 * somebody raises the ceiling, and a model that is not ready as soon as
 * somebody signs it in, so a box held shut until the window resets would
 * refuse the question the moment it could have been answered.
 */

import { fmtWait } from "~/lib/format.ts";
import type { SendRefusal, TurnView } from "./turn.ts";

/** The turn whose question was turned away unsent and is the latest: the one the box holds again. */
export function bouncedOf(turns: readonly TurnView[]): TurnView | null {
  const last = turns.at(-1);
  return last?.phase === "refused" && last.refusal !== null ? last : null;
}

/** Whether a refusal holds the box shut until its wait passes. */
export function locksTheBox(refusal: SendRefusal): boolean {
  return refusal.code === "assistant_busy" && refusal.retryAfterSeconds !== null;
}

/**
 * The seconds left of the wait the engine named for a refused turn, at `now`,
 * or null where it named none. Counted from when the question was asked, so a
 * notice read later says what is left rather than what was; and never more
 * than the engine named, since the drawer's clock may read a tick behind the
 * moment the question was asked.
 */
export function waitLeft(turn: TurnView, now: number): number | null {
  const wait = turn.refusal?.retryAfterSeconds ?? null;
  if (wait === null) return null;
  const until = Date.parse(turn.askedAt) + wait * 1000;
  if (!Number.isFinite(until)) return null;
  return Math.min(wait, Math.max(0, Math.ceil((until - now) / 1000)));
}

/**
 * What a refused question's notice says under its sentence: what to do (a model
 * not ready names its fix), when the wait ends and what else ends it, and
 * whether the question is back in the box. `inBox` is the drawer's own word
 * that it put the question back, never a guess from the code.
 */
export function refusalLines(turn: TurnView, inBox: boolean, now: number): string {
  const refusal = turn.refusal;
  if (refusal === null) return "";
  const lines: string[] = [];
  if (refusal.hint) lines.push(refusal.hint);
  const left = waitLeft(turn, now);
  if (left !== null) {
    // A LIMIT SOMEBODY CAN RAISE IS NEVER A WAIT ALONE: the window turning
    // over is one way back, and the ceiling raised is the other.
    if (refusal.remedy !== null) {
      lines.push(
        left > 0
          ? `It resets ${fmtWait(left)}, or as soon as the ceiling is raised.`
          : "The window has reset, so you can ask again.",
      );
    } else {
      lines.push(`You can ask again ${fmtWait(left)}.`);
    }
  }
  if (inBox) lines.push("Your question is back in the box below.");
  return lines.length > 0 ? lines.join(" ") : "Nothing changed.";
}

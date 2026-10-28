/**
 * What a person is told when the engine refused them — in the engine's own
 * words, written once.
 *
 * A refusal on AUTHORITY is said by `lib/access.ts`'s `needsWords`, over the
 * access the refusal itself names ([refusedGrants]): "Taking a backup needs
 * Admin access. An admin can give it to you." What the reader lacks is
 * ACCESS, and the engine's refusal names it — never a sentence naming a
 * permission typed here, which would be a second statement of the rule.
 */

import { RestError } from "~/protocol/index.ts";
import { asSentence, fmtWait } from "./format.ts";

/**
 * What a person is told when the sign-in surface refuses them — in the
 * engine's own words.
 *
 * THE ENGINE WROTE THE SENTENCE, SO THIS DOES NOT. Every refusal it answers
 * carries a `message` written for its code, and the sign-in surface is where
 * that matters most: a failed sign-in is ONE refusal, whatever went wrong, so
 * that nobody can learn from it who works here — and a screen that wrote its
 * own sentence per branch would be exactly the roster the engine refuses to
 * be. So the words are the engine's: the `detail` where it named what to
 * change (a password too short, a login somebody holds, a code that does not
 * match), the code's own sentence where it did not, and the `hint` beside
 * either.
 *
 * TWO FACTS THE BODY CANNOT CARRY are said here: a `429`'s wait is in its
 * `Retry-After`, never in its body, and a request that never reached the
 * engine has no body at all. "Try again" with no number reads as a lockout,
 * and "not accepted" about a request nobody answered is a claim nothing here
 * can make.
 */
export function refusalText(err: unknown): string {
  if (!(err instanceof RestError)) {
    return asSentence(err instanceof Error ? err.message : String(err));
  }
  if (err.status === 0) {
    // NOT "refused": nothing answered, so nothing here knows what the engine
    // did — and every gesture on the sign-in surface is safe to send again.
    return `No answer came back from the engine. ${asSentence(err.detail)}`.trim();
  }
  const said =
    asSentence(err.detail) ||
    err.sentence ||
    (err.code
      ? `The engine refused this (${err.code}).`
      : `Something in front of the engine answered ${err.status}.`);
  const parts = [said];
  if (err.hint) parts.push(asSentence(err.hint));
  if (err.status === 429 && err.retryAfterSeconds !== null) {
    // THE WAIT AS A TIME TO PLAN BY ("in 22h 33m"), never a count of seconds.
    parts.push(`Try again ${fmtWait(err.retryAfterSeconds)}.`);
  }
  return parts.join(" ");
}

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

import { REFUSAL_SENTENCES } from "~/contract/authority.ts";
import { ACT_ERRORS } from "~/contract/errors.ts";
import { isLogRefusal, RestError, type QueryRefusal } from "~/protocol/index.ts";
import { needsWords } from "./access.ts";
import { remedyFor } from "./authorityRemedy.ts";
import { asSentence, fmtWait } from "./format.ts";

/**
 * The one sentence for a refusal on AUTHORITY, about `what` ("Taking a
 * backup"), decided by WHAT REFUSED: a REST answer or a socket refusal.
 *
 * - A 401 accepted no credential, so the repair is to sign in.
 * - A rule's refusal (`403 unauthorized` with its reason, or the socket's
 *   frame) names the access any one of which would admit the reader where
 *   access is a way past it ([needsWords]), and otherwise is the rule's own
 *   sentence ([REFUSAL_SENTENCES]), exactly as `QueryState` draws it.
 * - Any other 403 names the access it carries where that access is the way
 *   past it ([remedyFor], by the rule its `reason` names: a proof gone stale
 *   carries the access that admitted the reader), and is otherwise the
 *   engine's own sentence for its code (a seat taken away, a proof gone
 *   stale); a refusal saying nothing more is no access.
 *
 * NOT BY WHETHER IT NAMED ACCESS. A refusal naming none went to the sign-in
 * sentence, which is the 401's, so a signed-in person a rule refused was told
 * to sign in on Audit, Secrets, the backups dialog and Integrations, while
 * every other screen told the same refusal as no access.
 */
export function authorityWords(what: string, refused: RestError | QueryRefusal | null): string {
  if (refused instanceof RestError) {
    if (refused.status === 401) return SIGN_IN;
    const rule = refused.refusal;
    if (rule !== null && !isLogRefusal(rule)) return ruleWords(what, rule);
    if (remedyFor(refused.reason, refused.grants).namesAccess) {
      return needsWords(what, refused.grants);
    }
    return asSentence(refused.sentence) || NO_ACCESS;
  }
  return refused === null ? NO_ACCESS : ruleWords(what, refused);
}

/** A rule's refusal as a reader is told it: see [authorityWords]. */
function ruleWords(what: string, rule: QueryRefusal): string {
  return remedyFor(rule.reason, rule.grants).namesAccess
    ? needsWords(what, rule.grants)
    : (REFUSAL_SENTENCES[rule.reason] ?? "You can’t see this, and no access would change that.");
}

/** Nothing the engine accepted was presented. */
const SIGN_IN = "Sign in to see this.";

/** A refusal that said nothing more than that it refused. */
const NO_ACCESS = ACT_ERRORS.unauthorized;

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

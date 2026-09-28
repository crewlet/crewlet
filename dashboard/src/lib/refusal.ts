/**
 * What a screen says when the engine refused it on AUTHORITY, written once.
 *
 * EVERY ONE OF THESE SAID "needs an operator token", which was the whole of
 * authority while a Tier A token was the only credential. It stopped being
 * true the day a person could sign in: a signed-in reader whose grants do not
 * reach a screen was sent to find a token they have no use for, and a reader
 * holding a token whose grants do not reach it was told to set the token they
 * had already set. What the reader lacks is a GRANT, and the engine's refusal
 * names it.
 *
 * FROM THE ANSWER, NEVER WRITTEN HERE: `grants` comes from the refusal's own
 * body ([refusedGrants]). A screen that named a grant itself would be a second
 * statement of the rule, and the copy that goes stale the day the rule's
 * grant moves.
 */

import { RestError } from "~/protocol/index.ts";
import { plural } from "./format.ts";

/**
 * One sentence about `what` (a capitalised gesture: "Editing the
 * organization") and the grants the refusal named.
 *
 * With grants, the credential was ACCEPTED and does not carry any of them.
 * Without, nothing the engine accepted was presented — a 401 — and the repair
 * is a credential rather than a grant.
 */
export function needsSentence(what: string, grants: readonly string[]): string {
  if (grants.length > 0) {
    return `${what} needs ${grants.join(" or ")}, which the credential you presented does not carry.`;
  }
  return `${what} needs a credential the engine accepts. Sign in.`;
}

/** A phrase the engine wrote in its own lower case, as a sentence. */
function asSentence(text: string): string {
  const trimmed = text.trim();
  if (trimmed === "") return "";
  const opened = trimmed[0]!.toUpperCase() + trimmed.slice(1);
  return /[.!?]$/.test(opened) ? opened : `${opened}.`;
}

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
  if (err.status === 429 && err.retryAfter !== null) {
    parts.push(`Try again in ${plural(err.retryAfter, "second")}.`);
  }
  return parts.join(" ");
}

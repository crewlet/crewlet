/**
 * What a reader can do about a refusal on authority, by the rule that refused.
 *
 * The banner a screen draws for an `unauthorized` answer offered ONE control
 * for every rule: Sign in. That is the repair for a reader short of an app —
 * somebody who holds it signs in — and for nothing else the table refuses by.
 * A proof of identity older than the verb allows is cleared by confirming who
 * you are, in this tab, without signing anybody out; an account that is not
 * active is cleared by an admin, after which asking again is the whole of it;
 * a credential acting as no seat is cleared by signing in AS YOURSELF, since
 * every person holds a human seat; a role on an artifact below the one the
 * verb asks is cleared by its owner sharing it higher, after which asking
 * again is the whole of it too; and a verb only an agent's seat makes is
 * cleared by nothing a reader can do, so a button there is a promise the
 * screen cannot keep.
 *
 * AN ARTIFACT IS CLEARED BY A SHARE, NOT A PERMISSION. A refusal on one names
 * `admin` beside it, because the admin path admits whatever the shares say,
 * and drawn as "needs Admin access" it would send every reader of a
 * colleague's page to ask for the permission that runs the company, when the
 * remedy is the owner's share. So `role` and `not_shared` (which a screen never meets as
 * `unauthorized`: the engine answers it as an artifact nobody holds) do not
 * draw their grants; a lead's row and an author's do, because who leads whom
 * is the company's document, which only an admin changes.
 *
 * KEYED LIKE `REFUSAL_SENTENCES` (`contract/authority.ts`): every reason the
 * engine's authority table reports, null for an admission, which no refusal
 * carries. The sentences are held to the engine's reasons by a Go gate, and
 * this table is held to the sentences by its suite, so a reason the engine
 * grows is a reason this file has to decide a control for.
 *
 * THE ASSISTANT'S REFUSAL CARD ASKS IT TOO, for `namesAccess` alone: a refusal
 * the Dashboard Assistant met says a permission is missing, names it and
 * offers a request to copy for an admin only where access is a way past the
 * rule, and is the engine's own sentence for it otherwise
 * (`app/assistant/chunk/blocks.tsx`). It draws none of these controls: the
 * card's foot holds only what the person sends an admin themselves.
 */

import { youNeedWords } from "./access.ts";

/** The one control a refusal offers — see [AUTHORITY_REMEDIES]. */
export type AuthorityAction =
  /** Sign in as somebody who holds what was asked for. */
  | "sign_in"
  /** Sign in as yourself — a person's own sign-in, never a token's. */
  | "sign_in_yourself"
  /** Confirm who you are (the step-up), and the screen asks again. */
  | "confirm"
  /** Ask again, once somebody else — an admin, an artifact's owner — has acted. */
  | "try_again"
  /** Load the dashboard the engine serves, which is the engine's own build. */
  | "reload";

/**
 * What clears one rule's refusal.
 *
 * NOT A `Remedy`: that is the engine's, the setting a refusal a configurable
 * limit caused is changed in (`contract/remedies.ts`, ADR-0060), declared once
 * because a gate reads it. This is the dashboard's own, keyed by the rule.
 */
export interface RuleRemedy {
  /**
   * The control a refusal naming NO access offers, or null where nothing on
   * this screen clears it. A refusal whose grants are a way past it offers
   * `sign_in` whatever this says — somebody holding them does.
   */
  action: AuthorityAction | null;
  /**
   * Whether the access a refusal names is a way past it. True for a missing
   * permission and for the RELATIONS — a lead's row, an author's, somebody's
   * own record — whose rows the admin path admits whatever the chart says.
   * False for every rule no permission changes: there a refusal's `grants`
   * are never the remedy, and a stale proof's are what ADMITTED the reader,
   * so "needs Admin access" drawn from them told an admin to ask for admin.
   */
  grantsCure: boolean;
}

export const AUTHORITY_REMEDIES: Readonly<Record<string, RuleRemedy | null>> = {
  grant: null,
  self: null,
  author: null,
  lead: null,
  everybody: null,
  asked: null,
  no_grant: { action: "sign_in", grantsCure: true },
  not_self: { action: "sign_in", grantsCure: true },
  not_lead: { action: "sign_in", grantsCure: true },
  not_author: { action: "sign_in", grantsCure: true },
  // THE SCREEN'S OWN MISTAKE: the request named nothing for the rule to read.
  unnamed: { action: null, grantsCure: true },
  // A person may: a browser holds no agent's seat, so a person's sign-in is it.
  seat_refused: { action: "sign_in", grantsCure: false },
  token_refused: { action: "sign_in_yourself", grantsCure: false },
  stage: { action: "try_again", grantsCure: false },
  // The engine serves its own dashboard, so a reload is its build.
  unknown_action: { action: "reload", grantsCure: false },
  step_up: { action: "confirm", grantsCure: false },
  seat_only: { action: null, grantsCure: false },
  unseated: { action: "sign_in_yourself", grantsCure: false },
  shared: null,
  // The owner's share clears both, so asking again once they have is all of it.
  not_shared: { action: "try_again", grantsCure: false },
  role: { action: "try_again", grantsCure: false },
  // An agent acting for somebody who may not: nothing a reader holds changes it.
  ceiling: { action: null, grantsCure: false },
  // A request the Dashboard Assistant made: its person takes the gesture
  // themselves, or approves the change it proposes, in its panel, so no grant
  // clears either and its refusal card asks an admin for nothing. A screen's
  // own requests are its reader's and never meet either, so it offers nothing.
  assist_refused: { action: null, grantsCure: false },
  assist_scope: { action: null, grantsCure: false },
};

/**
 * What clears a refusal by `reason` naming `grants`: the rule's own remedy,
 * with the access named as the way past it where it is one. A reason this
 * build does not know — a newer engine's — trusts its grants, and offers
 * nothing where it names none.
 */
export function remedyFor(
  reason: string,
  grants: readonly string[],
): { action: AuthorityAction | null; namesAccess: boolean } {
  const rule = AUTHORITY_REMEDIES[reason] ?? { action: null, grantsCure: true };
  const namesAccess = rule.grantsCure && grants.length > 0;
  return { action: namesAccess ? "sign_in" : rule.action, namesAccess };
}

/**
 * What a refused change says about access: "You need Worklet access for this.
 * Ask an admin." where the access the refusal names is a way past its rule
 * ([remedyFor]), and null where it is not, whose own sentence says what does
 * clear it.
 *
 * ONE ANSWER FOR EVERY WRITE, a press's strip (`components/WriteButton.tsx`), a
 * hand-off's notice (`routes/artifacts/new/AskAgent.tsx`) and a keyed write's
 * refusal (`lib/keyedWrite.ts`) alike. Each drew the grants whenever the
 * refusal named some, so a proof gone stale told an admin who had not
 * confirmed to ask for Admin, and a role on an artifact below the verb's sent
 * the reader to ask for the permission that runs the company rather than for
 * its owner's share.
 */
export function accessToAsk(refusal: { reason: string; grants: readonly string[] }): string | null {
  return remedyFor(refusal.reason, refusal.grants).namesAccess
    ? `${youNeedWords(refusal.grants)} Ask an admin.`
    : null;
}

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
 * every person holds a human seat; and a verb only an agent's seat makes is
 * cleared by nothing a reader can do, so a button there is a promise the
 * screen cannot keep.
 *
 * KEYED LIKE `REFUSAL_SENTENCES` (`contract/authority.ts`): every reason the
 * engine's authority table reports, null for an admission, which no refusal
 * carries. The sentences are held to the engine's reasons by a Go gate, and
 * this table is held to the sentences by its suite, so a reason the engine
 * grows is a reason this file has to decide a control for.
 */

/** The one control a refusal offers — see [AUTHORITY_REMEDIES]. */
export type AuthorityAction =
  /** Sign in as somebody who holds what was asked for. */
  | "sign_in"
  /** Sign in as yourself — a person's own sign-in, never a token's. */
  | "sign_in_yourself"
  /** Confirm who you are (the step-up), and the screen asks again. */
  | "confirm"
  /** Ask again, once somebody else — an admin — has acted. */
  | "try_again"
  /** Load the dashboard the engine serves, which is the engine's own build. */
  | "reload";

/** What clears one rule's refusal. */
export interface Remedy {
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

export const AUTHORITY_REMEDIES: Readonly<Record<string, Remedy | null>> = {
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

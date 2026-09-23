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
  return `${what} needs a credential the engine accepts. Sign in, or set a token.`;
}

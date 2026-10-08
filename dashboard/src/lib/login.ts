/**
 * Whether a login fits its holder's grammar — asked by every form that takes
 * one (an invitation's redemption, a service account, an edit of somebody's
 * row) BEFORE it posts, so the rule is said under the field rather than come
 * back as a refusal at the foot of the form.
 *
 * THE ENGINE'S GRAMMAR, read from `contract/identity.ts`, which a Go gate
 * holds against the engine's own check. The engine still decides: this only
 * spares a person the round trip, and the sentence names the shape to type.
 */

import { CREDENTIAL_CLASSES, MACHINE_LOGIN, MAX_LOGIN, PERSON_LOGIN } from "~/contract/identity.ts";

/** Whose login it is: a person's (dots) or a service account's (a colon). */
export type LoginKind = "person" | "machine";

const GRAMMAR: Record<LoginKind, RegExp> = {
  person: new RegExp(PERSON_LOGIN),
  machine: new RegExp(MACHINE_LOGIN),
};

/**
 * What is wrong with `login` for a holder of `kind`, as the one sentence to
 * show under the field, or null when the engine's grammar admits it. An empty
 * login is null too: that the field is required is its form's to say.
 */
export function loginProblem(kind: LoginKind, login: string): string | null {
  if (login === "") return null;
  if (!GRAMMAR[kind].test(login)) {
    return kind === "person"
      ? "Use lowercase words joined by dots, such as jane.doe — no capitals or spaces."
      : "Use lowercase words joined by a colon, such as ci:release — no capitals or spaces.";
  }
  const credential = kind === "machine" && CREDENTIAL_CLASSES.find((c) => login.startsWith(c));
  if (credential) {
    return `A login cannot begin with ${credential}, which names a credential rather than an account.`;
  }
  if (login.length > MAX_LOGIN) {
    return `At most ${MAX_LOGIN} characters; this is ${login.length}.`;
  }
  return null;
}

/**
 * Whose grammar a login is in — a person's (dots) or a machine's (a colon) —
 * or null for one in neither. The two grammars are disjoint by the shape of
 * the value, which is what lets a screen tell a person from a service account
 * or a Tier A token's session by the login alone.
 */
export function loginKind(login: string): LoginKind | null {
  if (GRAMMAR.person.test(login)) return "person";
  if (GRAMMAR.machine.test(login)) return "machine";
  return null;
}

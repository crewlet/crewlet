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
import type { SessionAnswer } from "~/protocol/index.ts";

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
      ? "Use lowercase words with dots, like jane.doe."
      : "Use lowercase words with colons, like ci:release.";
  }
  const credential = kind === "machine" && CREDENTIAL_CLASSES.find((c) => login.startsWith(c));
  if (credential) {
    return `A login can’t start with ${credential}.`;
  }
  if (login.length > MAX_LOGIN) {
    return `Use ${MAX_LOGIN} characters or fewer (this has ${login.length}).`;
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

/**
 * The Tier A token id a login names (`review` for `token:review`), or "" for
 * any other login. A Tier A token's session acts under `token:<id>`, the one
 * machine login the engine mints itself (`iam.TokenLogin`).
 */
export function tierATokenOf(login: string): string {
  return login.startsWith("token:") ? login.slice("token:".length) : "";
}

/**
 * Whether a session is a PERSON'S, with a directory row of their own: a
 * profile, a password, a second factor, sessions and tokens to manage.
 *
 * NOT THE SESSION'S `kind` ALONE. A Tier A token bound to a human seat acts
 * as that seat's holder, so the engine answers its session `kind: "person"`
 * (`internal/api/auth`, `principalFor`), and its id is derived from the
 * token rather than a row's. Read as a person, the Account page asked the
 * directory for a profile nobody holds and drew "Not found". The login says
 * which it is, by its grammar.
 */
export function ownsAccount(session: Pick<SessionAnswer, "kind" | "login">): boolean {
  return session.kind === "person" && tierATokenOf(session.login) === "";
}

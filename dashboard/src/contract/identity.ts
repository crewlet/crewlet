/**
 * The identity directory's closed sets that People & access offers back to the
 * engine: the grants a person, an invitation or a token may carry.
 *
 * EXACTLY THE ENGINE'S — held by
 * `internal/api/iamapi.TestTheDashboardOffersExactlyTheEnginesGrants` in both
 * directions and in order. A grant the engine grew that this lacks is one no
 * administrator can confer from the dashboard; one this keeps that the engine
 * dropped is a checkbox every write refuses.
 */

/** Every grant, in the engine's own order (`iam.AllGrants`). */
export const GRANTS = [
  "state:read",
  "audit:read",
  "config:read",
  "secrets:read",
  "work:write",
  "knowledge:write",
  "config:write",
  "secrets:write",
  "fleet:operate",
  "people:manage",
  "sandbox:run",
] as const;

/**
 * The grants a machine token never carries, whatever its owner holds
 * (`iam.PersonPresentGrants`): revealing a credential's value, and deciding who
 * may do anything at all, need a person present. A token mint naming one is
 * refused, so the dialog says so before it is sent.
 */
export const TOKEN_WITHHELD_GRANTS = ["secrets:read", "people:manage"] as const;

/**
 * The two login grammars, as the engine holds them (`iam.ValidLoginFor`):
 * lowercase words of letters and digits with inner hyphens, a person's joined
 * by DOTS and a machine's by a COLON, at most [MAX_LOGIN] characters — and a
 * machine's never in a credential's class ([CREDENTIAL_CLASSES]).
 *
 * HELD BY BEHAVIOUR, not by spelling:
 * `internal/api/iamapi.TestTheDashboardChecksALoginByTheEnginesGrammar` reads
 * these four, compiles the two patterns and runs a corpus of logins through
 * them and through the engine's own check, failing on any login the two
 * answer differently. Before the forms held a login to them, `Frank` was
 * posted and came back as a 400 carrying the domain's own sentence.
 *
 * STRINGS rather than regular expressions, because a contract module is data
 * and `lib/login.ts` is where they are compiled.
 */
export const PERSON_LOGIN = "^[a-z0-9]+(?:-[a-z0-9]+)*(?:\\.[a-z0-9]+(?:-[a-z0-9]+)*)+$";

/** A machine's login: see [PERSON_LOGIN]. */
export const MACHINE_LOGIN = "^[a-z0-9]+(?:-[a-z0-9]+)*(?::[a-z0-9]+(?:-[a-z0-9]+)*)+$";

/** The longest login either grammar admits (`iam.MaxLogin`): a seat handle's own width. */
export const MAX_LOGIN = 64;

/**
 * The prefixes that name a CREDENTIAL in the operator column (`pat:` a
 * machine token, `session:` a browser session), which no machine's login may
 * begin with.
 */
export const CREDENTIAL_CLASSES = ["pat:", "session:"] as const;

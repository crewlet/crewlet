/**
 * `access`: who can reach the company through the engine's own surface, and as
 * whom — the credentials the auth guard accepts, the people in the chart, and
 * the posture. LABELS, NEVER VALUES: the engine's answer has no member a
 * token's value could travel in.
 *
 * EXACTLY WHAT `internal/api/queries` SENDS — held there by
 * `TestTheAccessScreenReadsWhatThisAnswerSends` in both directions, and the
 * two unions by `TestTheDashboardKnowsExactlyTheAccessStates`.
 */

/**
 * What one credential reaches. `operator`: every guarded read, `/config`,
 * `/secrets`, `/setup`, `/backup` and the operator MCP surface, under its own
 * label. `person`: all of that, plus acting from the dashboard as the human
 * seat that binds it.
 */
export type TokenScope = "operator" | "person";

/**
 * How a human seat's `contact.crewlet_operator_id` stands against the
 * credentials the guard accepts. Each failure has its own remedy: `unbound`
 * names no token (an ordinary state), `unresolved` is a `${VAR}` unset in the
 * engine's environment, `no_token` names a label the guard does not accept.
 */
export type AccessBinding = "bound" | "unbound" | "unresolved" | "no_token";

/** The posture the guard enforces (`api.auth` in Tier A). */
export interface AccessAuth {
  disabled: boolean;
  anonymous_read: boolean;
  /** Empty is same-origin only. */
  allowed_origins: string[];
}

/** A seat a token acts as. */
export interface AccessSeat {
  handle: string;
  name: string;
}

/** One accepted credential. Never its value. */
export interface AccessToken {
  /** The `api.auth.tokens[].id` label, recorded as the author of its writes. */
  id: string;
  scope: TokenScope;
  /** The human seat binding it, or null. */
  seat: AccessSeat | null;
  /** Whether this browser's own token is this one. */
  yours: boolean;
}

/** One identity field of a person's contact, as written in the chart. */
export interface AccessContact {
  /** The config key: `slack_user_id`, `github_login`, … */
  key: string;
  /** Verbatim — a literal id or a `${VAR}` reference, never the variable's value. */
  value: string;
  reference: boolean;
  /** Whether the engine can use it. */
  resolves: boolean;
}

/** One human seat. */
export interface AccessPerson {
  handle: string;
  name: string;
  email: string;
  availability: string;
  /** `contact.crewlet_operator_id` verbatim, or "". */
  operator_id: string;
  binding: AccessBinding;
  /** Where agents reach them. The binding is not among these. */
  contacts: AccessContact[];
}

/** The whole answer. */
export interface AccessAnswer {
  auth: AccessAuth;
  /** In label order. */
  tokens: AccessToken[];
  /** Human seats in handle order. */
  people: AccessPerson[];
}

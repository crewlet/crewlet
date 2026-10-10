/**
 * `access`: who can reach the company through the engine's own surface, how
 * far, and as whom — the keys the auth guard accepts and the role each one
 * carries, the people in the chart, and the posture. LABELS, NEVER VALUES: the
 * engine's answer has no member a key's value could travel in.
 *
 * EXACTLY WHAT `internal/api/queries` SENDS — held there by
 * `TestTheAccessScreenReadsWhatThisAnswerSends` in both directions, and the
 * closed sets by `TestTheDashboardKnowsExactlyTheAccessStates`.
 */

/**
 * What one key is FOR (`api.auth.tokens[].role`, ADR-0031). `member`: what the
 * company published — its work, its pages, its chart — and acting from the
 * dashboard as the person it is linked to. `admin`: all of that, plus what the
 * machine processed and how it is run — transcripts, events, spend,
 * `/config`, `/secrets`, `/setup`, nodes and backups.
 */
export type TokenRole = "member" | "admin";

/**
 * What a caller presenting NO key reaches (`api.auth.anonymous`). `public`:
 * the company's name, mission and chart. `none`: the probes and the dashboard
 * shell, and nothing of the company at all.
 */
export type AnonymousAccess = "none" | "public";

/**
 * How far one caller reaches, lowest first — the guard's own order, which
 * every question, route and push declares the reach it needs against. `open`
 * is anybody who can reach the port; `public` the company's public face;
 * `member` and `admin` the two keyed reaches, one per [TokenRole]. Read off
 * the viewer answer, so a screen asks the engine's own comparison rather than
 * re-deriving it from a role.
 */
export type Reach = "open" | "public" | "member" | "admin";

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
  anonymous: AnonymousAccess;
  /** Empty is same-origin only. */
  allowed_origins: string[];
  /**
   * `api.auth.company_writers`: the admin keys that alone may change the
   * company document, as Tier A orders them (ADR-0030). Empty is every admin
   * key.
   */
  company_writers: string[];
}

/** A seat a key acts as. */
export interface AccessSeat {
  handle: string;
  name: string;
}

/** One accepted key. Never its value. */
export interface AccessToken {
  /** The `api.auth.tokens[].id` label, recorded as the author of its writes. */
  id: string;
  /**
   * What the key reaches. A DIFFERENT FACT from `seat`: the role says what
   * the key may read and run, the link says who it acts as.
   */
  role: TokenRole;
  /** The human seat linked to it, or null. */
  seat: AccessSeat | null;
  /** Whether this browser's own key is this one. */
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

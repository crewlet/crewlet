/**
 * `credential_pool`: every model the company configures, the keys each rotates
 * through, and which of them a vendor is refusing now — this node's pools
 * beside the fleet's cooldown ledger, the later deadline winning. NAMES, NEVER
 * VALUES: a key is its variable's name, where it came from, and a 12-character
 * hint nobody can reverse.
 *
 * EXACTLY WHAT `internal/api/queries` SENDS — held there by
 * `TestTheModelsScreenReadsWhatTheCredentialPoolSends` in both directions, the
 * three unions included.
 */

/**
 * One key's condition. `ready`: a call can lease it now. `cooling`: benched
 * after a rate-limit or auth refusal, by this node or a peer, until
 * `cooling_until`. `unresolved`: it resolved to nothing on the answering node
 * and is not in the pool at all. `duplicate`: the same value as the key at
 * `same_as`, held once.
 */
export type CredentialKeyState = "ready" | "cooling" | "unresolved" | "duplicate";

/**
 * Where a key's value comes from. `reference`: a whole `${VAR}`, named by
 * `ref`. `default`: the vendor's conventional variable, read because the entry
 * names no `api_keys` at all. `inline`: written into the document; it has no
 * name, only a position.
 */
export type CredentialKeySource = "reference" | "default" | "inline";

/**
 * A model entry as a whole. `ready`: every key resolves and none cools.
 * `degraded`: some can be leased and some cannot. `exhausted`: every key that
 * resolves is cooling, so each call falls through to the seat's next model.
 * `no_key`: nothing resolves, so every call is a 401. `login`: a cli-agent
 * entry — one login held by the CLI, nothing that rotates or cools.
 */
export type CredentialPoolState = "ready" | "degraded" | "exhausted" | "no_key" | "login";

/** One configured key. There is no member a value could travel in. */
export interface CredentialKeyRow {
  /** The variable's name; empty for an `inline` key. */
  ref: string;
  source: CredentialKeySource;
  /** The hint the engine's `credential_cooled` log lines carry; empty when unresolved. */
  hint: string;
  state: CredentialKeyState;
  /** RFC 3339, when a `cooling` key lifts; null otherwise. */
  cooling_until: string | null;
  /** 1-based position of the key a `duplicate` repeats; 0 otherwise. */
  same_as: number;
  /** The answering node's leases of it since its configuration was applied. */
  uses: number;
  in_flight: number;
}

/** One `providers.llm` entry. */
export interface CredentialPoolRow {
  /** The entry's config key — what a seat's `llm:` names. */
  key: string;
  type: string;
  /** As written: a `${VAR}` stays its name. */
  model: string;
  state: CredentialPoolState;
  /** Keys a call could lease now. */
  ready: number;
  /** The bench times in force, defaults applied. */
  rate_limit_seconds: number;
  auth_seconds: number;
  /** Declaration order; empty for a `login` entry. */
  keys: CredentialKeyRow[];
}

/** The whole answer. */
export interface CredentialPoolAnswer {
  /** The node that answered: `uses`, `in_flight` and `unresolved` are its own. */
  node: string;
  /** Whether the fleet's cooldown ledger was read; false leaves only this node's. */
  fleet: boolean;
  /** Why it was not, when it was not. */
  fleet_error: string;
  /** Config order — the order a seat that names no model falls back through. */
  providers: CredentialPoolRow[];
}

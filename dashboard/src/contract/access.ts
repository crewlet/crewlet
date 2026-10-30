/**
 * Per-seat integration access: what `GET /setup/integrations/{kind}/access`
 * answers, what `POST .../access/plan` and `PUT .../access` take, and what a
 * seat action (`POST .../access/seats/{handle}/{lane}/{action}`) answers.
 *
 * The engine's own shapes (`internal/seataccess`: wire.go, step.go,
 * seataccess.go, vendor.go), held against them by the setup API's contract
 * test, which reads these interfaces by name.
 *
 * IN THE VENDOR'S OWN TERMS. A level is the identifier the vendor's API takes
 * (a Datadog role id; a GitLab `access_level` such as "30", or
 * "member_role:<id>"; a Jira project role id; a Confluence space role id, or
 * sorted "<op>:<target>" permissions; Mattermost roles such as "system_user
 * system_post_all_public") and is drawn under the vendor's exact name, which
 * `options` carries. Datadog's, Jira's and Confluence's lists are read live
 * from the vendor, so no list of them lives here. GitHub has no levels; a
 * seat holds GitHub App permissions.
 */

/** GitHub's access to one permission. Absent from a map means No access. */
export type GitHubAccess = "read" | "write";

/** One product a seat can be on: Atlassian has two, every other integration one. */
export type AccessLaneKey =
  "datadog" | "github" | "gitlab" | "jira" | "confluence" | "slack" | "mattermost";

/** One level a lane offers, as the vendor defines it. */
interface AccessLevel {
  /** The identifier the vendor's API takes, which the document stores. */
  id: string;
  /** The vendor's exact name. */
  name: string;
  /** A role the company defined at the vendor rather than one the vendor ships. */
  custom?: boolean;
  /** An administration grant: raising a seat to it asks for a fresh step-up (D10). */
  administers?: boolean;
}

/** One GitHub App permission a seat can hold, in GitHub's own name. */
interface AccessPermission {
  key: string;
  name: string;
  /** Workflows is Read and write or nothing. */
  write_only?: boolean;
  /** Read and write on it is an administration grant (Contents, Workflows). */
  administers_on_write?: boolean;
}

/** What one lane offers a seat, in the vendor's own terms. */
export interface AccessOptions {
  lane: AccessLaneKey;
  /** The vendor's levels in its own order, least access first; absent where it has none. */
  levels?: AccessLevel[];
  /** Whether the levels were read live from the vendor, rather than its fixed set. */
  live: boolean;
  /** GitHub only: the permissions a seat's app can hold. */
  permissions?: AccessPermission[];
  /** What a seat may be scoped to (repositories, projects, spaces, channels), from the company's connection. */
  scope?: string[];
  /** Whether a scope entry outside `scope` may be typed. */
  scope_open?: boolean;
  /** Whether a person can ask Crewlet to rotate a seat's credential on this lane. */
  rotatable?: boolean;
  /**
   * "external" where the engine does not provision this lane for the company:
   * a seat's switch still binds its tool to the credential, and its access is
   * managed outside Crewlet.
   */
  managed?: "external";
}

/** What the company document asks for one seat on one lane. */
export interface AccessDesired {
  enabled: boolean;
  level?: string;
  permissions?: Record<string, GitHubAccess>;
  scope?: string[];
}

/** A seat's credential: the engine-owned variable it is sealed in, and when. Never the value. */
interface AccessCredential {
  variable: string;
  /** Further variables the same credential seals (an Atlassian account address, a Slack signing secret). */
  others?: string[];
  minted_at: string;
  /** Who the pass that sealed it ran for, as the engine names actors. */
  by?: string;
  /** Whether Crewlet minted it or it was provided from outside Crewlet. */
  source?: "minted" | "external";
}

/** What the vendor showed on the last sync, read back from the engine's account ledger. */
export interface AccessObserved {
  exists: boolean;
  enabled: boolean;
  /** Who turned the account off: this engine, or somebody at the vendor. */
  disabled_by?: "crewlet" | "hand";
  /** The account as the vendor names it. */
  account?: string;
  account_id?: string;
  level?: string;
  permissions?: Record<string, GitHubAccess>;
  scope?: string[];
  /** GitHub only: the permissions the app is registered with, which tokens are narrowed from. */
  ceiling?: Record<string, GitHubAccess>;
  credential?: AccessCredential;
  created_at?: string;
  /** When a sync found the account changed at the vendor by somebody else. */
  changed_by_hand_at?: string;
}

type AccessOp = "create" | "enable" | "level" | "scope" | "mint" | "disable" | "remove" | "rotate";

type AccessVerb =
  | "create"
  | "grant"
  | "mint"
  | "enable"
  | "rerole"
  | "scope"
  | "disable"
  | "revoke"
  | "remove"
  | "rotate"
  | "restore"
  | "approve";

/** One thing a sync does at the vendor, or one thing a person must do there. */
export interface AccessStep {
  op: AccessOp;
  verb: AccessVerb;
  /** The step, as one sentence with no closing full stop. */
  text: string;
  note?: string;
  by: "crewlet" | "you";
  /** Where a person takes the step, at the vendor. */
  action_url?: string;
  /** The step restores a change somebody made at the vendor by hand. */
  drift?: boolean;
}

/** Why a sync stopped: the failed step, the vendor's reason, the remedy, who acts and where. */
interface AccessFailure {
  step: AccessVerb;
  said: string;
  remedy: string;
  actor: "crewlet" | "you";
  action_url?: string;
}

interface AccessSync {
  at: string;
  outcome: "ok" | "failed" | "waiting";
  /** The steps the sync set out to take, and whether each was taken. */
  steps: (AccessStep & { done: boolean })[];
  error?: AccessFailure;
}

/** Whether the seat can use its credential through a tool right now. */
type AccessToolState = "live" | "builtin" | "waiting" | "no_server" | "off";

export interface AccessSeatLane {
  desired: AccessDesired;
  observed: AccessObserved;
  sync?: AccessSync;
  /** What the next sync does for this lane as the document stands. */
  plan: AccessStep[];
  tool: AccessToolState;
  /** The desired setting is an administration grant. */
  administers?: boolean;
}

export interface AccessSeat {
  handle: string;
  name: string;
  /** A seat that left the company document whose account is still in the ledger. */
  departed?: boolean;
  lanes: Partial<Record<AccessLaneKey, AccessSeatLane>>;
}

/** A company-level notice on an integration's Agents tab (the shared-credential repair, D12). */
interface AccessNotice {
  tone: "danger" | "warning" | "info";
  title: string;
  body: string;
  /** Offers the integration's own key replacement. */
  replace_key?: boolean;
}

/** `GET /setup/integrations/{kind}/access`, and the answer to every write below. */
export interface AccessAnswer {
  kind: string;
  /** The company document's entity tag; a save sends it back as If-Match. */
  revision: string;
  syncing: boolean;
  started_at?: string;
  last_sync_at?: string;
  notice?: AccessNotice;
  /**
   * "operator" where the Kubernetes operator writes the company document:
   * saves and Keep are refused, and actions at the vendor still run.
   */
  managed_by?: "operator";
  /** What each lane offers now, for the pickers. */
  options: AccessOptions[];
  seats: AccessSeat[];
}

/** A draft: the desired state per seat and lane, only for what changed. */
export type AccessDraft = Record<string, Partial<Record<AccessLaneKey, AccessDesired>>>;

/** `POST .../access/plan`, and `PUT .../access` with the revision as If-Match. */
export interface AccessDraftRequest {
  seats: AccessDraft;
}

export interface AccessPlanAnswer {
  kind: string;
  revision: string;
  plans: { handle: string; lane: AccessLaneKey; steps: AccessStep[]; step_up?: boolean }[];
  /** Saving this draft asks for a fresh step-up (D10). */
  step_up?: boolean;
}

/** `POST /setup/integrations/{kind}/access/seats/{handle}/{lane}/{action}`. */
export type AccessSeatAction = "rotate" | "remove" | "keep" | "restore" | "retry" | "done";

/** `POST .../access/seats/{handle}/{lane}/install`: a fresh, single-use install link. */
export interface AccessInstallLink {
  url: string;
}

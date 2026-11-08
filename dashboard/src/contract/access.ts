/**
 * Per-seat integration access: what `GET /setup/integrations/{kind}/access`
 * answers, what `POST .../access/plan` and `PUT .../access` take, and what a
 * seat action (`POST .../access/seats/{handle}/{lane}/{action}`) answers.
 *
 * The engine's own shapes (`internal/seataccess`: wire.go, step.go,
 * seataccess.go, vendor.go), held against them member by member by
 * `internal/seataccess.TestTheDashboardReadsAccessAsTheEngineWritesIt`, and
 * the unions by `TestTheDashboardKnowsExactlyTheAccessValues`.
 *
 * IN THE VENDOR'S OWN TERMS. A level is the identifier the vendor's API takes
 * (a Datadog role id; a GitLab `access_level` such as "30", or
 * "member_role:<id>"; a Jira project role id; a Confluence space role id, or
 * sorted "<op>:<target>" permissions; Mattermost roles such as "system_user
 * system_post_all_public"; Cloudflare's `read`, `operate`, `configure` or `secure`,
 * Crewlet's own sets of permission groups; Expo's `viewer`, `runner`,
 * `developer` or `release_manager`, each a set of Expo permissions named for
 * the Expo role that holds them; DigitalOcean's `read`, `operate` or
 * `administer`, each a set of DigitalOcean token scopes sent as its `grants`;
 * Scaleway's `read`, `operate` or `administer`, Crewlet's own sets of Scaleway
 * permission sets; AWS's `read`, `operate` or `administer`, Crewlet's own
 * customer managed policies; Google Cloud's `viewer`, `operator` or
 * `administer`, Crewlet's own sets of Google's predefined roles) and is drawn
 * under the
 * vendor's exact name, which
 * `options` carries. Datadog's, Jira's and Confluence's lists are read live
 * from the vendor, so no list of them lives here. GitHub has no levels; a
 * seat holds GitHub App permissions.
 */

import type { SetupVerdict } from "./setup.ts";

/** GitHub's access to one permission. Absent from a map means No access. */
export type GitHubAccess = "read" | "write";

/** One product a seat can be on: Atlassian has two, every other integration one. */
export type AccessLaneKey =
  | "datadog"
  | "github"
  | "gitlab"
  | "jira"
  | "confluence"
  | "slack"
  | "mattermost"
  | "cloudflare"
  | "expo"
  | "digitalocean"
  | "scaleway"
  | "aws"
  | "google_cloud"
  | "ssh";

/** "external" on a lane the engine does not provision for the company. */
type AccessManaged = "external";

/** Who wrote the company document instead of the dashboard: the Kubernetes operator. */
type AccessManagedBy = "operator";

/**
 * Whether Crewlet minted and sealed a seat's credential, it was provided from
 * outside Crewlet, or Crewlet issues it at use and never seals it (a token
 * that lasts an hour, issued by each node for its own runs).
 */
type AccessCredentialSource = "minted" | "external" | "issued";

/** Who turned an account off: this engine, or somebody at the vendor by hand. */
type AccessDisabledBy = "crewlet" | "hand";

/** Who takes a step or a remedy: Crewlet, or a person. */
type AccessActor = "crewlet" | "you";

/** How a sync ended for one lane. */
type AccessOutcome = "ok" | "failed" | "waiting";

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
  /** What a seat on it then sees, where the name alone does not say (product access only). */
  note?: string;
  /** Why no seat can be given it yet: offered disabled, with this as the reason. */
  unavailable?: string;
  /**
   * Every vendor permission the level holds, in the vendor's own names, where a
   * person grants them by hand at the vendor (DigitalOcean's token scopes).
   */
  grants?: string[];
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
  /** Whether the lane narrows a seat by scope at all, whatever `scope` lists now. */
  scoped?: boolean;
  /** What a seat may be scoped to (repositories, projects, spaces, channels), from the company's connection. */
  scope?: string[];
  /**
   * A person's name for each `scope` entry, keyed by the identifier stored,
   * where the two differ (a Scaleway project's ID). Absent where the
   * identifier is the name.
   */
  scope_names?: Record<string, string>;
  /** Whether `scope` was read just now, so an empty one means the vendor has none to offer. */
  scope_live?: boolean;
  /** Whether a scope entry outside `scope` may be typed. */
  scope_open?: boolean;
  /** Whether a seat that is on must name at least one entry: an empty scope reaches nothing. */
  scope_required?: boolean;
  /** Whether a person can ask Crewlet to rotate a seat's credential on this lane. */
  rotatable?: boolean;
  /** Where a person takes this lane's steps at the vendor, for a step that names no page of its own. */
  action_url?: string;
  /**
   * Every vendor permission no level grants that a pasted credential must not
   * hold. With `unneeded` and the levels' `grants`, each permission the
   * vendor offers is in exactly one.
   */
  refused?: string[];
  /** Every vendor permission no level grants that a pasted credential may still hold. */
  unneeded?: string[];
  /** The account, organization or team the company connected, as the vendor shows it. */
  vendor_account?: string;
  /**
   * Whether a person may keep a turned-off agent's account at the vendor
   * rather than delete it there (the `release` action, on its remove wait).
   */
  releasable?: boolean;
  /**
   * The `mcp_servers` entry a per-seat tool server for this lane is started
   * from, where one takes the seat's credential; absent where none does
   * (Expo, whose CLI in the agent's box is its only tool).
   */
  tool_server?: string;
  /**
   * The lane's access is a SET of levels rather than one (Confluence's space
   * permissions by operation, on a site without space roles): a seat's level
   * is the chosen ids, sorted and space-separated.
   */
  multiple?: boolean;
  /**
   * On a `multiple` lane, the level every other level in a set needs, so a set
   * naming any level holds this one too: Confluence's View (`read:space`).
   * The engine refuses a set without it. Absent where no level is required.
   */
  required_level?: string;
  /**
   * "external" where the engine does not provision this lane for the company:
   * a seat's switch still binds its tool to the credential, and its access is
   * managed outside Crewlet.
   */
  managed?: AccessManaged;
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
  /** When it was sealed. Absent for an issued credential, which each node issues for itself. */
  minted_at?: string;
  /** Who the pass that sealed it ran for, as the engine names actors. */
  by?: string;
  /** Whether Crewlet minted it or it was provided from outside Crewlet. */
  source?: AccessCredentialSource;
}

/** What the vendor showed on the last sync, read back from the engine's account ledger. */
export interface AccessObserved {
  exists: boolean;
  enabled: boolean;
  /** Who turned the account off: this engine, or somebody at the vendor. */
  disabled_by?: AccessDisabledBy;
  /** The account as the vendor names it. */
  account?: string;
  /**
   * The vendor's identifier for the account, or an opaque fingerprint where
   * every seat acts as the same person (DigitalOcean's tokens). Never drawn.
   */
  account_id?: string;
  level?: string;
  permissions?: Record<string, GitHubAccess>;
  scope?: string[];
  /** GitHub only: the permissions the app is registered with, which tokens are narrowed from. */
  ceiling?: Record<string, GitHubAccess>;
  credential?: AccessCredential;
  /** The vendor shows the account holds no credential Crewlet minted, so the next sync mints one. */
  credential_gone?: boolean;
  created_at?: string;
  /** When a sync found the account changed at the vendor by somebody else. */
  changed_by_hand_at?: string;
  /**
   * The credentials the account still holds besides the sealed one (an old
   * token after a rotate), for a person to delete at the vendor. Absent where
   * the vendor cannot tell.
   */
  retired?: AccessRetired[];
}

/** One credential the account still holds that is not the sealed one. */
interface AccessRetired {
  /** What the person looks for at the vendor (Expo's token prefix), drawn as it is. */
  label: string;
  /** When the vendor says it was created. */
  since?: string;
}

type AccessOp =
  | "create"
  | "enable"
  | "level"
  | "scope"
  | "mint"
  | "disable"
  | "revoke"
  | "remove"
  | "rotate"
  | "retire";

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
  | "approve"
  | "observe";

/** One thing a sync does at the vendor, or one thing a person must do there. */
export interface AccessStep {
  op: AccessOp;
  verb: AccessVerb;
  /** The step, as one sentence with no closing full stop. */
  text: string;
  note?: string;
  by: AccessActor;
  /** Where a person takes the step, at the vendor. */
  action_url?: string;
  /** The step restores a change somebody made at the vendor by hand. */
  drift?: boolean;
}

/** A step on a sync's record, and whether it was taken. */
interface AccessStepOutcome extends AccessStep {
  done: boolean;
}

/** Why a sync stopped: the failed step, the vendor's reason, the remedy, who acts and where. */
interface AccessFailure {
  step: AccessVerb;
  said: string;
  remedy: string;
  actor: AccessActor;
  action_url?: string;
}

interface AccessSync {
  at: string;
  outcome: AccessOutcome;
  /** The steps the sync set out to take, and whether each was taken. */
  steps: AccessStepOutcome[];
  error?: AccessFailure;
}

/** Whether the seat can use its credential through a tool right now.
 *  `server_off`: every tool server reading the credential is turned off. */
type AccessToolState = "live" | "builtin" | "waiting" | "no_server" | "server_off" | "off";

/**
 * One variable Crewlet sets in a seat's executor box: one of the seat's own
 * credentials, by its sealed or issued name (`${EXPO_TOKEN_QA}`), or a
 * non-secret setting.
 */
export interface AccessBoxLine {
  name: string;
  value: string;
  /** A credential the lane's tool, a CLI in the box, reads. */
  tool?: boolean;
}

/** What Crewlet puts in a seat's executor box for the lane while the seat is on. */
export interface AccessBox {
  /** The credentials the lane's tool reads first, then every other variable. */
  env: AccessBoxLine[];
  /** Files the box holds (a token file a CLI reads), each naming its variable. */
  files?: AccessBoxFile[];
}

/** One file in a seat's executor box: what it holds, by reference, never the value. */
interface AccessBoxFile {
  /** Relative to the box's home directory. */
  path: string;
  /** The variable reference the file's whole body is (`${<VARIABLE>_<HANDLE>}`). */
  value: string;
  /** Whether the engine rewrites it while a run is live, so it stays current. */
  rewritten: boolean;
}

export interface AccessSeatLane {
  desired: AccessDesired;
  observed: AccessObserved;
  sync?: AccessSync;
  /** What the next sync does for this lane as the document stands. */
  plan: AccessStep[];
  tool: AccessToolState;
  /** Why the tool is in its state, in the engine's words, where the state alone does not say. */
  tool_reason?: string;
  /** The desired setting is an administration grant. */
  administers?: boolean;
  /** Where the lane's tool is a CLI in the seat's own box (Expo's EAS CLI), what that box holds for it. */
  box?: AccessBox;
  /**
   * Where a person makes the seat's credential at the vendor: the name to give
   * it at the seat's desired level. Computed by the engine, never read back.
   */
  hand_in?: AccessHandIn;
}

/** What a person makes at the vendor for a seat. */
interface AccessHandIn {
  name: string;
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
  managed_by?: AccessManagedBy;
  /** What each lane offers now, for the pickers. */
  options: AccessOptions[];
  seats: AccessSeat[];
}

/**
 * `POST .../access/plan`, and `PUT .../access` with the revision as If-Match:
 * the desired state per seat and lane, only for what changed.
 */
export interface AccessDraftRequest {
  seats: Record<string, Partial<Record<AccessLaneKey, AccessDesired>>>;
}

/** What the next sync does for one seat on one lane, as a draft stands. */
interface AccessSeatPlan {
  handle: string;
  lane: AccessLaneKey;
  steps: AccessStep[];
  /** This seat's change is an administration grant. */
  step_up?: boolean;
}

/**
 * One agent whose access the draft changes, by the closed change list it is
 * told, with open work raised under its access now: it reviews that work once
 * the draft is saved.
 */
interface AccessSeatReview {
  handle: string;
  /** Its open questions, what its executor waits on, and its work in progress or blocked. */
  open: number;
  /** The change lines it will be told, at most twenty. */
  changes: string[];
}

export interface AccessPlanAnswer {
  kind: string;
  revision: string;
  plans: AccessSeatPlan[];
  /** Saving this draft asks for a fresh step-up (D10). */
  step_up?: boolean;
  /** Absent where the open work could not be counted. */
  reviews?: AccessSeatReview[];
}

/** The control of one seat's lane an `access_not_offered` refusal is about. */
export type AccessRefusalField = "level" | "permissions" | "scope";

/**
 * An `access_not_offered` refusal's body, beside the envelope's `error` and
 * `message`: the first seat whose draft asks for what the vendor does not
 * offer, on which lane and control, and why.
 */
export interface AccessRefusal {
  seat: string;
  lane: AccessLaneKey;
  field: AccessRefusalField;
  detail: string;
  /** What the lane offers instead, where it lists them. */
  offered?: string[];
  /** The level every other level in a set needs, where the refusal is its absence. */
  required?: string;
}

/** `POST /setup/integrations/{kind}/access/seats/{handle}/{lane}/{action}`. */
export type AccessSeatAction =
  "rotate" | "remove" | "keep" | "restore" | "retry" | "done" | "release";

/**
 * `POST .../access/seats/{handle}/{lane}/credential`: a credential a person
 * made at the vendor, checked and sealed. `verdict` says what was checked and
 * kept, as a Test does; `access` is the tab as it now stands.
 */
export interface AccessCredentialAnswer {
  verdict: SetupVerdict;
  access: AccessAnswer;
}

/** `POST .../access/seats/{handle}/{lane}/install`: a fresh, single-use install link. */
export interface AccessInstallLink {
  url: string;
}

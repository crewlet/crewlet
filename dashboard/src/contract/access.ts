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
import type { VendorTextPart } from "./vendortext.ts";

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
  | "ssh"
  | "meta_ads"
  | "app_store_connect"
  | "apple_ads"
  | "google_analytics"
  | "admob"
  | "adsense"
  | "google_ads"
  | "search_console"
  | "google_play";

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

/**
 * One permission a lane's live catalogue offers a seat beside its level (a
 * Cloudflare permission group), as the company's connection can grant it now.
 */
export interface AccessGrantEntry {
  /** The vendor's own name for it, which a grant stores ("Origin Write"). */
  id: string;
  /** How a person finds it at the vendor ("Origin Rules: Edit"). */
  name: string;
  /** The heading a picker lists it under ("Zone", "Account"). */
  group?: string;
  /** What its scope entries are, in the vendor's word ("zone"), or "account" where it takes none. */
  scope_kind: string;
  /** It may change things at the vendor: granting it asks for a fresh step-up. */
  write?: boolean;
  /** The lowest level that already holds it. */
  in_level?: string;
  /** Why the connection cannot grant it yet: offered disabled with this reason. */
  unavailable?: string;
  /** What to add at the vendor first, as a person adds it there. */
  missing?: string[];
  action_url?: string;
  /** What granting it allows beyond its name. */
  note?: string;
  /** Granted for one change: it lasts at most this many hours, never for good. */
  max_hours?: number;
}

/**
 * One permission a seat holds beside its level, as the company document stores
 * it. The engine writes `id`, `granted_by` and `granted_at`: a draft sends a
 * new grant without them, and what a credential holds carries none.
 */
export interface AccessGrant {
  id?: string;
  permission: string;
  /** Where it applies (zone names); absent is its whole reach. */
  scope?: string[];
  /** When it lapses, absent for permanent. */
  expires_at?: string;
  granted_by?: string;
  granted_at?: string;
  reason: string;
  /** The agent's request it answered, where it answered one. */
  request?: string;
  /** What the one change the agent asked it for changes (a bucket), copied from the request. */
  target?: string;
}

/**
 * Every option of an agent's request for one grant begins with this, followed
 * by the request's id and `-t` (for its hours) or `-p` (for good). Held to the
 * engine's own seataccess.GrantOptionTemporary.
 */
export const GRANT_REQUEST_OPTION_PREFIX = "grant-r";

/** One agent's stored request for one grant, as the engine wrote it when the agent asked. */
export interface AccessGrantRequest {
  id: string;
  kind: string;
  lane: AccessLaneKey;
  handle: string;
  permission: string;
  scope?: string[];
  /** How long the temporary option grants it for. */
  hours: number;
  /** The agent's own words for why. */
  reason: string;
  /** What the one change it is asked for changes (a bucket). */
  target?: string;
  item: string;
  ask: string;
  asked_at: string;
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
  /**
   * The second identifier the vendor gives each `scope` entry beside its ID
   * (an app's bundle ID), keyed by the identifier stored, for a picker's quiet
   * line. Absent where the vendor gives none.
   */
  scope_details?: Record<string, string>;
  /**
   * What holds each `scope` entry (an Analytics property's account), keyed by
   * the identifier stored, for a picker's headings. Absent where entries stand alone.
   */
  scope_groups?: Record<string, string>;
  /** The shape a typed entry must have, a regular expression; absent where any text may be one. */
  scope_typed?: string;
  /** Why the entries could not be listed, in a person's words; absent where they were. */
  scope_unlisted?: string;
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
  /**
   * Every permission a seat may be granted beside its level, from the
   * vendor's live catalogue; absent on a lane that takes no grants.
   */
  grants?: AccessGrantEntry[];
  /** The entries a grant's scope may name (the zones the company's agents reach). */
  grant_scope?: string[];
  /**
   * Where a scoped grant naming no entry applies ("every zone the company's
   * agents reach", "the agent's own projects"), in the lane's own words.
   */
  grant_whole?: string;
  /** Where a grant on the whole connection applies ("the account", "the Organization"). */
  grant_account?: string;
  /** When this offer was read from the vendor; absent where it was not read. */
  read_at?: string;
}

/** What the company document asks for one seat on one lane. */
export interface AccessDesired {
  enabled: boolean;
  level?: string;
  permissions?: Record<string, GitHubAccess>;
  scope?: string[];
  /** The seat's grants beside its level, as stored; only the live ones are applied. */
  grants?: AccessGrant[];
  /** Who signs the seat's calls where the vendor offers it an identity of its own; absent is the lane's default. */
  identity?: AccessIdentityMode;
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
  /** The grants the credential holds beside its level, as the last sync read it back. */
  grants?: AccessGrant[];
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
  | "grants"
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
  | "ungrant"
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
 * One seat's lane as one word, computed by the engine once so the Agents tab
 * and Ask Crewlet cannot word a lane two ways. A draft's "Not saved" is the tab's own.
 */
export type AccessLaneStatus =
  | "syncing"
  | "refused"
  | "changed_at_vendor"
  | "needs_you"
  | "preparing"
  | "next_sync"
  | "role_unchecked"
  | "in_sync"
  | "off_account_kept"
  | "off"
  | "departed";

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
  /** When a pass last read the vendor for this lane, a dry run included; `sync.at` is the last writing sync. */
  observed_at?: string;
  status: AccessLaneStatus;
  /** The status in the tab's words, which a status this bundle does not know is drawn by. */
  status_label: string;
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
  /** Who the vendor sees for the seat; absent where each seat holds an account of its own and nothing is shared. */
  identity?: AccessIdentity;
}

/** What a person makes at the vendor for a seat. */
interface AccessHandIn {
  name: string;
}

/** The document's choice of who signs a seat's calls. */
export type AccessIdentityMode = "own" | "company";

/** Where a seat's identity at a vendor stands: one chip set for every vendor. */
export type AccessIdentityStatus =
  | "company"
  | "none"
  | "preparing"
  | "ready"
  | "unchecked"
  | "wrong_role"
  | "wrong_team"
  | "refused"
  | "replacing";

/** Whose identity signs the seat's calls now; `none` is a seat whose calls are refused. */
export type AccessIdentityHolds = "own" | "company" | "none";

/** A seat's identity at a vendor, worded by the engine. */
export interface AccessIdentity {
  /** The vendor offers the seat an identity of its own; where it does not, the company's is the normal state. */
  offered: boolean;
  mode: AccessIdentityMode;
  holds: AccessIdentityHolds;
  status: AccessIdentityStatus;
  /** The status in the chip's words, which a status this bundle does not know is drawn by. */
  status_label: string;
  name?: string;
  id?: string;
  /** The role the vendor holds it at, as last read back. */
  role?: string;
  /** The role the seat's level needs, in the vendor's words. */
  needs?: string;
  team?: string;
  checked_at?: string;
  /** The box's one line: who the vendor sees, and what to do. */
  said: string;
  problem?: AccessIdentityProblem;
  /** Why the vendor cannot give the seat its own now (its limit). */
  blocked?: string;
  set_up: boolean;
  replace: boolean;
  use_company: boolean;
  /** The identities no longer used, to revoke at the vendor, oldest first. */
  retiring?: AccessRetiringIdentity[];
  guide?: AccessIdentityGuide;
}

/** What needs a person in the agent's box. */
interface AccessIdentityProblem {
  title: string;
  body: string;
}

/** An identity no longer used, which a person revokes at the vendor. */
interface AccessRetiringIdentity {
  id: string;
  name?: string;
  since?: string;
}

/** The three-step sheet that sets up a seat's own identity, in the vendor's words. */
export interface AccessIdentityGuide {
  vendor: string;
  noun: string;
  records: string;
  why: string;
  company?: string;
  /** The name to give it, from the one shared name rule, and the vendor's rule under it. */
  name: string;
  name_rule: string;
  name_what: string;
  role: string;
  never?: string;
  steps: AccessGuideStep[];
  /** The seat's entries to add it to, each with the vendor's page for it and, once checked, where it stands. */
  entries?: AccessIdentityEntry[];
  /** Adding it once to what holds the entries (an Analytics account) instead. */
  wider?: AccessGuideWider;
  /** When the vendor can find a new identity, while it is getting ready. */
  ready_at?: string;
  /** The vendor's page as a button, only where no step links it. */
  link?: AccessGuideLink;
  note?: VendorTextPart[];
  back: AccessGuideBack;
  make?: AccessGuideMake;
  checks: AccessGuideCheck[];
}

/** Where a seat's identity stands on one scope entry. */
type AccessEntryState = "missing" | "applying" | "added" | "wrong_role" | "unchecked" | "";

/** One of a seat's scope entries as a check found its identity there, and where a person adds it. */
export interface AccessIdentityEntry {
  id: string;
  name?: string;
  group?: string;
  /** Empty before any check read it. */
  state: AccessEntryState;
  role?: string;
  add_url?: string;
  group_url?: string;
}

/** The one add on what holds a seat's entries, which reaches every entry there. */
interface AccessGuideWider {
  label: string;
  does: VendorTextPart[];
  links?: AccessGuideLink[];
}

interface AccessGuideStep {
  text: VendorTextPart[];
  copy?: AccessGuideCopy;
}

interface AccessGuideCopy {
  label: string;
  value: string;
  rule?: string;
}

interface AccessGuideLink {
  label: string;
  url: string;
}

/** What a person brings back from the vendor: a file, a token, or nothing. */
export interface AccessGuideBack {
  kind: "file" | "token" | "nothing";
  accept?: string;
  hint?: VendorTextPart[];
  label?: string;
  text?: VendorTextPart[];
  /** One optional value asked for beside the file, which the person may leave empty. */
  detail?: AccessGuideDetail;
}

/** The optional value a sheet asks for beside a file: its label and when it is needed. */
interface AccessGuideDetail {
  label: string;
  hint: string;
}

/** Crewlet making the identity itself, or why it cannot now. */
interface AccessGuideMake {
  label: string;
  does: VendorTextPart[];
  unavailable?: string;
  /** What making it does before the check's reads, as its check lists them. */
  reads?: AccessGuideCheck[];
}

interface AccessGuideCheck {
  id: string;
  label: string;
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
  /** The tracker stopped counting at its ceiling, so `open` is a floor. */
  at_least?: boolean;
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
export type AccessRefusalField = "level" | "permissions" | "scope" | "grants";

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

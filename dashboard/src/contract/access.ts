/**
 * Per-seat integration access: what `GET /setup/integrations/{kind}/access`
 * answers, what `POST .../access/plan` and `PUT .../access` take, and what a
 * seat action (`POST .../access/seats/{handle}/{lane}/{action}`) answers.
 *
 * IN THE VENDOR'S OWN TERMS. A level is the identifier the vendor's API takes
 * (a Datadog role id, a GitLab `access_level`, a Jira project role id, a
 * Confluence space role id, Mattermost role names) and is drawn under the
 * vendor's exact name, which the engine sends with it: the lists Datadog,
 * Jira and Confluence offer are read live from the vendor, so no list of them
 * lives here. GitHub has no levels; a seat holds GitHub App permissions.
 *
 * Held against what the engine sends by the setup API's contract test, which
 * reads these interfaces by name.
 */

/** GitHub's access to one permission. Absent from a map means No access. */
export type GitHubAccess = "read" | "write";

/** One GitHub App permission a seat can hold, in GitHub's own name. */
interface AccessPermission {
  key: string;
  name: string;
  /** Workflows is Read and write or nothing. */
  write_only?: boolean;
  /** Read and write on it asks for a fresh step-up (Contents, Workflows). */
  admin_write?: boolean;
}

/** One level a lane offers, as the vendor defines it. */
interface AccessLevel {
  /** The identifier the vendor's API takes, which the document stores. */
  id: string;
  /** The vendor's exact name. */
  name: string;
  /** What a seat holding it can do, in a few words. */
  hint?: string;
  /** A role the company defined at the vendor rather than one the vendor ships. */
  custom?: boolean;
  /** An administration role: raising a seat to it asks for a fresh step-up. */
  admin?: boolean;
}

/** Where a lane's levels come from: read live from the vendor, or the vendor's fixed set. */
interface AccessLevelsSource {
  live: boolean;
  /** One line, drawn as the access menu's heading. */
  text: string;
}

/**
 * One product a seat can be on within an integration: Atlassian has two (Jira
 * and Confluence); every other integration has one, keyed as the integration.
 */
export interface AccessLane {
  key: string;
  name: string;
  /** The vendor's levels in its own order, least access first; empty where it has none. */
  levels: AccessLevel[];
  levels_source?: AccessLevelsSource;
  /** GitHub only: the permissions a seat's app can hold. */
  permissions?: AccessPermission[];
  /** Slack only: the bot token scopes every agent's app holds. */
  scopes?: string[];
  /** What a seat may be scoped to (repositories, projects, spaces, channels), from the company's connection. */
  scope_options?: string[];
  /** Whether the engine can rotate a seat's credential here. */
  rotatable: boolean;
  /** The `mcp_servers` entry a seat's tool is started from, where the tool is a server. */
  tool_server?: string;
  /** Where a person settles what only the vendor can (its user administration). */
  console_url?: string;
}

/** What the company document asks for one seat on one lane. */
export interface AccessDesired {
  enabled: boolean;
  level?: string;
  permissions?: Record<string, GitHubAccess>;
  scope?: string[];
}

/** What the vendor showed on the last sync, read back from the engine's account ledger. */
export interface AccessObserved {
  exists: boolean;
  enabled: boolean;
  /** Who turned the account off: this engine, or somebody at the vendor. */
  disabled_by?: "crewlet" | "hand";
  /** The account as the vendor names it. */
  account?: string;
  level?: string;
  permissions?: Record<string, GitHubAccess>;
  /** GitHub only: the permissions the app is registered with, which tokens are narrowed from. */
  ceiling?: Record<string, GitHubAccess>;
  scope?: string[];
  /** The engine-owned variable the credential is sealed in, and when it was minted. */
  credential?: { variable: string; minted_at: string };
  created_at?: string;
  /** When a sync found the account changed at the vendor by somebody else. */
  changed_by_hand_at?: string;
}

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
  verb: AccessVerb;
  /** The step, as one sentence with no closing full stop. */
  text: string;
  by: "crewlet" | "you";
  note?: string;
  /** On a sync's record: whether the step was taken. */
  done?: boolean;
}

interface AccessSync {
  at: string;
  outcome: "ok" | "failed" | "waiting";
  steps: AccessStep[];
  error?: { said: string; remedy: string; actor: "crewlet" | "you" };
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
  /** The engine-owned variable this seat's credential is, or will be, sealed in. */
  credential_variable: string;
}

export interface AccessSeat {
  handle: string;
  name: string;
  lanes: Record<string, AccessSeatLane>;
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
  /** The company document revision this was read at; a save sends it back as `if_match`. */
  revision_id: string;
  syncing: boolean;
  started_at?: string;
  last_sync_at?: string;
  notice?: AccessNotice;
  lanes: AccessLane[];
  seats: AccessSeat[];
}

/** A draft: the desired state per seat and lane, only for what changed. */
export type AccessDraft = Record<string, Record<string, AccessDesired>>;

/** `POST /setup/integrations/{kind}/access/plan`. */
export interface AccessPlanRequest {
  seats: AccessDraft;
}

export interface AccessPlanAnswer {
  kind: string;
  revision_id: string;
  plans: { handle: string; lane: string; steps: AccessStep[] }[];
}

/** `PUT /setup/integrations/{kind}/access`: one revision of the company document, then a sync. */
export interface AccessSaveRequest {
  seats: AccessDraft;
  if_match: string;
}

/** `POST /setup/integrations/{kind}/access/seats/{handle}/{lane}/{action}`. */
export type AccessSeatAction = "rotate" | "remove" | "keep" | "restore" | "retry" | "done";

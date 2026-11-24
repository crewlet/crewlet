/**
 * The model questions and the model routes, typed: the client half of
 * `internal/api/queries/models.go` (`llm_entries`, `llm_runners`, `llm_models`,
 * `llm_device_sign_in`) and `internal/api/llmapi` (the credential, check,
 * retry, host login and device sign-in routes).
 *
 * The enums an engine gate holds are not declared here; they live in
 * `../contract/models.ts`, and this file composes them. RELATIVE imports, as
 * in every protocol file: it is also built alone as `protocol.js`.
 */

import type {
  ModelAlarmKind,
  ModelCheckState,
  ModelDeviceStatus,
  ModelHealth,
  ModelJob,
  ModelLimitState,
  ModelLimitWindow,
  ModelSignInRoute,
  RunnerCellName,
} from "../contract/models.ts";

/** One live node, and whether its status record carried its models' state. */
export interface ModelNode {
  id: string;
  reported: boolean;
}

/** A host harness login on one node: its state as the harness's own status command reports it. */
export interface ModelHostLogin {
  node: string;
  state: "signed_in" | "signed_out" | "unknown" | (string & {});
  method: string | null;
  account: string | null;
  detail?: string | null;
  checked_at: string | null;
  /** The command that makes the login on that node: `crewlet llm login <key>`. */
  command: string;
}

/** How a model signs in, as the answering node resolves it. Names and hints, never a value. */
export interface ModelSignIn {
  route: ModelSignInRoute;
  mode: string;
  variable: string | null;
  source: "store" | "environment" | "none";
  resolves: boolean;
  hint: string | null;
  stored_at: string | null;
  stored_by: string | null;
  expires_at: string | null;
  expiry_basis: string | null;
  host_logins: ModelHostLogin[];
}

/** A plan or rate limit the model sits out, across the nodes that report it. */
export interface ModelSittingOut {
  until: string;
  resets_at: string | null;
  window: ModelLimitWindow;
  /** The vendor's own sentence; absent from a member's reading. */
  said?: string;
  since: string;
  nodes: string[];
}

/** One window of a model's sign-in's vendor limit, as the newest report across nodes left it. */
export interface ModelLimit {
  window: ModelLimitWindow;
  /** The window's length in hours as the vendor names it; 0 where unsaid. */
  hours: number;
  /** The model family a `model` window is of ("opus"). */
  model?: string;
  state: ModelLimitState;
  /** The fraction used; null where unsaid, or the window has turned over since. */
  used: number | null;
  resets_at: string | null;
  /** When the model is tried again, where it sits this window out. */
  until: string | null;
  seen: string;
  stale: boolean;
}

export interface ModelLastFailure {
  kind: "auth" | "rate_limit" | "timeout" | "server" | "fatal" | (string & {});
  at: string;
  said?: string;
  node: string;
}

/** Seats parked on the model, waiting for its limit to lift. */
export interface ModelWaiting {
  seats: string[];
  since: string;
}

/** A model's calls and tokens over its window. The dashboard draws tokens and never money (dashboard-design rule 19), so no price field is read. */
export interface ModelUsage {
  days: number;
  calls: number;
  total_tokens: number;
}

/**
 * One place that runs on a model: a seat's job, Ask Crewlet's chat, or the
 * Advisor's findings. `position` 0 is the place that runs on it; a later
 * position is a fallback.
 */
export interface ModelUse {
  seat?: string;
  job?: ModelJob;
  assistant?: "chat";
  /** The Advisor explains its findings with the model (`advisor.model`). */
  advisor?: "findings";
  position: number;
  written?: boolean;
  source?: "seat" | "executor" | "default-entry" | "first-entry";
}

/** A Codex device sign-in session. */
export interface ModelDevice {
  id: string;
  entry: string;
  node: string;
  url?: string;
  code?: string;
  status: ModelDeviceStatus;
  detail?: string;
  started_at: string;
  expires_at: string;
  reload?: { revision_id: string; epoch: number };
}

/** One model in an admin's reading. */
export interface ModelEntry {
  key: string;
  harness: string;
  model: string;
  health: ModelHealth;
  /** The sign-in the model shares a plan with others on; "" where none. */
  plan: string;
  sign_in: ModelSignIn;
  sitting_out: ModelSittingOut | null;
  last_success: string | null;
  last_failure: ModelLastFailure | null;
  waiting: ModelWaiting | null;
  usage: ModelUsage;
  used_by: ModelUse[];
  device_sign_in: ModelDevice | null;
  limits: ModelLimit[];
  /** Why no helper (Ask Crewlet, the Advisor) can run on it, in a person's words; null where one can. */
  helper_problem: string | null;
}

/** One model in a member's reading: nothing that names a credential, an account or a vendor's sentence. */
export interface MemberModelEntry {
  key: string;
  harness: string;
  model: string;
  health: ModelHealth;
  sign_in: { route: ModelSignInRoute; resolves: boolean };
  sitting_out: ModelSittingOut | null;
  last_success: string | null;
  waiting: ModelWaiting | null;
  used_by: ModelUse[];
  limits: ModelLimit[];
}

/** One model an alarm names, with what it says about it. */
export interface ModelAlarmEntry {
  key: string;
  detail: string;
  since?: string;
}

/** One of the five model alarms (statelog), as this node last evaluated it. */
export interface ModelAlarm {
  kind: ModelAlarmKind;
  detail: string;
  remedy: string;
  entries?: ModelAlarmEntry[];
}

/** `llm_entries`: every model, in an admin's reading or a member's. */
export type ModelEntriesAnswer =
  | {
      reading: "admin";
      node: string;
      nodes: ModelNode[];
      entries: ModelEntry[];
      alarms: ModelAlarm[] | null;
      /** The harnesses this node's build runs a seat's executor on, in the profiles' order. */
      executor_harnesses: string[];
    }
  | {
      reading: "member";
      node: string;
      nodes: ModelNode[];
      entries: MemberModelEntry[];
      alarms: null;
      /** The harnesses this node's build runs a seat's executor on, in the profiles' order. */
      executor_harnesses: string[];
    };

/** One runner cell's probe on one node. */
export interface RunnerNodeCell {
  state: "ready" | "not_ready" | "unconfigured" | (string & {});
  detail: string;
  fix: string | null;
  checked_at: string | null;
}

/** `llm_runners`: where a model's executors can run, and on which nodes each is ready. */
export interface RunnersAnswer {
  /** Empty where the company resolves no default: no `providers.sandbox`, or `local:` without `default_run_in`. */
  default_run_in: RunnerCellName | "";
  /** Whether the company declares `providers.sandbox`, which sets up both places at once. */
  declared: boolean;
  cells: Record<RunnerCellName, { ready_on: string[]; not_ready_on: string[] }>;
  nodes: Array<{ id: string; reported: boolean } & Record<RunnerCellName, RunnerNodeCell | null>>;
}

/** One model in the catalogue, by the id the harness takes. */
export interface CatalogueModel {
  id: string;
  name: string;
  family?: string;
  released?: string;
}

/** A model name the harness resolves itself; `model` and `version` are what it resolves to on the vendor's own service, where the engine knows. */
export interface ModelAlias {
  id: string;
  name: string;
  detail: string;
  model?: string;
  version?: string;
}

/** `llm_models`: the models a harness can run, from models.dev, with its own aliases and docs. */
export interface ModelsAnswer {
  source: "models.dev" | "none";
  fetched_at: string | null;
  provider: string;
  aliases: ModelAlias[];
  models: CatalogueModel[];
  docs_url: string;
  reason?: string;
}

/** One check a test ran, in the doctor's order. */
export interface CheckLine {
  check: string;
  ok: boolean;
  detail: string;
  kind?: string;
  remedy?: string;
  /** On an auth failure: the vendor refused the credential, or the harness never used it. */
  meaning?: "refused" | "unread";
}

/** What a test call found on the node that ran it. */
export interface CheckAnswer {
  at: string;
  node: string;
  ok: boolean;
  failure?: string;
  lines: CheckLine[];
}

/**
 * A model's test, as `POST /llm/providers/{key}/check` and `llm_check` answer
 * it: running on the node that took it, ended, or lost with a node that
 * stopped. It runs on that node's own time, so the page asks `llm_check` again
 * while it is running.
 */
export interface ModelCheck {
  id: string;
  entry: string;
  node: string;
  state: ModelCheckState;
  draft: boolean;
  smoke: boolean;
  started_at: string;
  deadline: string;
  ended_at: string | null;
  by: string;
  /** What an ended test found. */
  check?: CheckAnswer;
  /** Why the test ended, or was lost, finding nothing. */
  failure?: string;
}

/** A vendor limit a candidate credential met: it authenticated, and the vendor is limiting it. */
export interface CredentialLimited {
  until: string;
  resets_at: string | null;
  window: ModelLimitWindow;
  said?: string;
}

/** `PUT /llm/providers/{key}/credential?dry_run=true`. */
export interface CredentialCheck {
  ok: true;
  verified: string;
  hints: Record<string, string>;
  check: CheckAnswer;
  limited: CredentialLimited | null;
}

/** A re-activation after a stored secret (`configapi.Reloaded`). */
export interface ConfigReloaded {
  revision_id?: string;
  epoch?: number;
  error?: string;
  detail?: string;
  deferred?: boolean;
  stored_revision_id?: string;
  node?: string;
}

/** `PUT /llm/providers/{key}/credential`: stored, after its test passed. */
export interface CredentialStored {
  verified: string;
  check: CheckAnswer;
  names: string[];
  hints: Record<string, string>;
  /** Every model that reads one of the names, and so signs in with it now. */
  entries: string[];
  /** The re-activation that makes running models read it: landed, or why it did not. */
  reload: ConfigReloaded | null;
  cleared: string[];
  /** Runs still on the old value, or null where the count could not be read. */
  running_runs: number | null;
  limited: CredentialLimited | null;
}

/** `POST /llm/providers/{key}/retry`. */
export interface RetryAnswer {
  cleared: boolean;
  mode: "probe" | "clear";
  node: string;
  still: CredentialLimited | null;
  /** Why a probe failed for a reason other than a limit: nothing was cleared. */
  failed?: string;
}

/** `POST /llm/providers/{key}/host-login/check`. */
export interface HostLoginAnswer {
  node: string;
  state: ModelHostLogin["state"];
  method: string | null;
  account: string | null;
  detail: string | null;
  checked_at: string | null;
  command: string;
  shares_login_with: string[];
}

/** One written reference to a model, by identity: a seat's job, Ask Crewlet's chat, or the Advisor's findings (`configapi.RemovalAt`). */
export interface ModelReferenceAt {
  seat?: string;
  job?: ModelJob;
  assistant?: "chat";
  advisor?: "findings";
}

/** A dependant the remove refusal names: where it is, the field, and the whole chain written there. */
export interface ModelReference {
  at: ModelReferenceAt;
  segments: Array<string | number>;
  chain: string[];
}

/** What one reference to a removed model becomes: another model, or null to leave the job unset. */
export interface ModelRepoint {
  at: ModelReferenceAt;
  to: string | null;
}

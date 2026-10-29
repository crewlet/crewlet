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
  ModelDeviceStatus,
  ModelHealth,
  ModelJob,
  ModelLimitWindow,
  ModelSignInRoute,
  RunnerCellName,
} from "../contract/models.ts";

/** One live node, and whether its heartbeat carried its models' state. */
export interface ModelNode {
  id: string;
  reported: boolean;
}

/** A host CLI login on one node: its state as the CLI's own status command reports it. */
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
 * One place that runs on a model: a seat's job, or the assistant. `position`
 * 0 is the place that runs on it; a later position is a fallback.
 */
export interface ModelUse {
  seat?: string;
  job?: ModelJob;
  assistant?: "chat" | "deep_work";
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
  agent: string;
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
}

/** One model in a member's reading: nothing that names a credential, an account or a vendor's sentence. */
export interface MemberModelEntry {
  key: string;
  agent: string;
  model: string;
  health: ModelHealth;
  sign_in: { route: ModelSignInRoute; resolves: boolean };
  sitting_out: ModelSittingOut | null;
  last_success: string | null;
  waiting: ModelWaiting | null;
  used_by: ModelUse[];
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
    }
  | {
      reading: "member";
      node: string;
      nodes: ModelNode[];
      entries: MemberModelEntry[];
      alarms: null;
    };

/** One runner cell's probe on one node. */
export interface RunnerNodeCell {
  state: "ready" | "not_ready" | "unconfigured" | (string & {});
  detail: string;
  fix: string | null;
  checked_at: string | null;
}

/** `llm_runners`: where a model's coding runs can happen, and on which nodes each is ready. */
export interface RunnersAnswer {
  default_run_in: RunnerCellName;
  cells: Record<RunnerCellName, { declared: boolean; ready_on: string[]; not_ready_on: string[] }>;
  nodes: Array<{ id: string; reported: boolean } & Record<RunnerCellName, RunnerNodeCell | null>>;
}

/** One model in the catalogue, by the id the CLI takes. */
export interface CatalogueModel {
  id: string;
  name: string;
  family?: string;
  released?: string;
}

/** `llm_models`: the models a CLI can run, from models.dev, with its own aliases and docs. */
export interface ModelsAnswer {
  source: "models.dev" | "none";
  fetched_at: string | null;
  provider: string;
  aliases: Array<{ id: string; name: string; detail: string }>;
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
}

/** What a test call found on the node that ran it. */
export interface CheckAnswer {
  at: string;
  node: string;
  ok: boolean;
  failure?: string;
  lines: CheckLine[];
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

/** One written reference to a model, by identity: a seat's job, or the assistant's chat or deep work (`configapi.RemovalAt`). */
export interface ModelReferenceAt {
  seat?: string;
  job?: ModelJob;
  assistant?: "chat" | "deep_work";
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

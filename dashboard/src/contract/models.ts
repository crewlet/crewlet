/**
 * How the engine reads one `providers.llm` entry: the closed set of its
 * `auth.mode`, and what each field the entry leaves out comes to.
 *
 * Settings › Models & keys draws every entry off the company document, which
 * holds what an operator WROTE — and an entry that names no `agent` or no
 * `auth.mode` still runs as something. What it runs as is the engine's
 * default, so the screen says it from here rather than guessing, and the words
 * for each sign-in are keyed on this union so a mode the engine adds is a
 * typecheck failure rather than a row drawn with nothing said.
 *
 * HELD AGAINST `config.CLIAgentAuthModes`, `config.CodingAgents` and an unset
 * `config.LLMProvider`'s `AgentName`, `ResolvedAuthMode`, `Timeout` and
 * `Concurrency` by
 * `internal/config.TestTheModelsScreenReadsAnEntryAsTheEngineDoes`: a default
 * that drifted would name a CLI, a sign-in or a cap the engine does not apply
 * to the entry on screen, and an executor set that drifted would say a model
 * runs an executor that the engine skips.
 */

/**
 * How an entry's CLI signs in. `subscription`: the operator's own CLI login —
 * a headless token, or the files `crewlet llm login` wrote. `api-key`: one key,
 * put in the CLI's own key variable, billed to the metered account.
 * `inherit-env`: whichever of the CLI's two variables the engine's own
 * environment holds. `env`: the entry's own env signs the CLI in (a cloud
 * provider's switch and credentials, or a gateway's token), and neither the
 * token nor the key reaches the CLI.
 */
export type CLIAuthMode = "subscription" | "api-key" | "inherit-env" | "env";

/**
 * What each built-in CLI is called where a person reads it: "Claude Code
 * subscription limit reached", never "claude-code". EXACTLY the engine's
 * built-in profiles (`cliprofile.BuiltinNames`) bar `custom`, held by
 * `internal/config.TestEveryBuiltInCLIHasAProductName`, so a profile the
 * engine ships is named the day it ships. An entry on `custom` describes a
 * CLI of its own and is called by its key.
 */
export const CLI_PRODUCTS: Readonly<Record<string, string>> = {
  "claude-code": "Claude Code",
  codex: "Codex",
  copilot: "GitHub Copilot",
  "cursor-agent": "Cursor",
  "gemini-cli": "Gemini CLI",
  grok: "Grok",
  hermes: "Hermes",
  "kimi-code": "Kimi Code",
  "muse-code": "Muse Code",
  opencode: "OpenCode",
  pi: "Pi",
  "qwen-code": "Qwen Code",
};

/** The CLI an entry that names no `agent` drives. */
export const DEFAULT_CLI_AGENT = "claude-code";

/**
 * The CLIs that can run a seat's executor: the coding agents with an in-box
 * runner. Every agent seat's executor is its CLI's own run, placed by the
 * launching entry's `run_in`, and a member of an executor chain on any other
 * CLI is skipped at launch — that entry serves the text phases alone (the
 * reviewer, workers, onboarding and side passes).
 */
export const EXECUTOR_CLI_AGENTS = ["claude-code", "opencode"] as const;

/** An entry's `auth.mode` when it names none. */
export const DEFAULT_CLI_AUTH_MODE: CLIAuthMode = "subscription";

/** The wall-clock cap on one stretch of a CLI's own work — no tool call in flight — when `timeout_seconds` is unset. */
export const DEFAULT_CLI_TIMEOUT_SECONDS = 600;

/** How many CLI processes an entry runs at once when `max_concurrent` is unset. */
export const DEFAULT_CLI_MAX_CONCURRENT = 4;

/**
 * ONE reading of a model's condition, decided by the engine
 * (`queries.ModelHealth`) so the list, the model's page and the By job view
 * cannot each decide it differently. Held against `queries.ModelHealths` by
 * `internal/api/queries.TestTheModelsScreenReadsTheEnginesHealthAndAlarms`.
 */
export type ModelHealth =
  "sitting_out" | "no_sign_in" | "rejected" | "expiring" | "untested" | "ready";

/**
 * The five model alarms (statelog) `llm_entries` carries. Held against the
 * engine's own five by the same gate, so an alarm the engine raises is never
 * one the page has no words for.
 */
export type ModelAlarmKind =
  | "llm_credential_expiring"
  | "llm_credential_expires_soon"
  | "llm_sign_in_refused"
  | "llm_limit"
  | "llm_limit_waiting";

/**
 * Where a device sign-in is (`coord.DeviceSignInStatus`). Held against
 * `coord.DeviceSignInStatuses` by the health gate, so the sign-in panel has
 * words for every state it follows a session through.
 */
export type ModelDeviceStatus =
  "starting" | "pending" | "cancel_requested" | "approved" | "failed" | "expired" | "cancelled";

/**
 * How a model's CLI signs in as one node resolves it (`cliagent.SignInRoute`).
 * Held against `cliagent.SignInRoutes` by
 * `internal/providers/llm/cliagent.TestTheModelsScreenNamesTheEnginesSignIns`.
 */
export type ModelSignInRoute =
  "token" | "credentials" | "api-key" | "env" | "inherit-env" | "environment" | "none";

/**
 * Which window of a vendor's limit a model sits out (`llm.LimitWindow`): a
 * session or a weekly window holds every model on the plan, a model window
 * holds the one model. Held against `llm.LimitWindows` by the sign-in gate.
 */
export type ModelLimitWindow = "session" | "weekly" | "model" | "unknown";

/**
 * One line of a model's test (`cliagent.CheckName`), in the doctor's order.
 * Held against the doctor's own order by the sign-in gate.
 */
export type ModelCheckName =
  "binary" | "version" | "sign-in" | "usage" | "smoke" | "shell" | "web" | "executor";

/**
 * The work a written reference points at a model (`config.ProviderJob`): a
 * seat's executor or one of its satellites, or the assistant's chat or deep
 * work. Held against `config.ProviderJobs` by
 * `internal/config.TestTheModelsScreenReadsAnEntryAsTheEngineDoes`.
 */
export type ModelJob =
  "executor" | "review" | "subagent" | "auxiliary" | "judge" | "assistant" | "deep_work";

/**
 * The seat field each seat job is written in, as the page writes it: the
 * executor in `llm`, each satellite flat beside it. Held by the same gate to
 * the fields `config.Company.ProviderReferences` reports for each job, so a
 * write from the page lands in the field the engine resolves.
 */
export const SEAT_JOB_FIELDS = {
  executor: "llm",
  review: "llm_review",
  subagent: "llm_subagent",
  auxiliary: "llm_auxiliary",
  judge: "llm_judge",
} as const;

/**
 * Where a model's coding runs happen (`config.Placement`). Held against
 * `config.Placements` by the entry gate.
 */
export type RunnerCellName = "direct" | "container" | "e2b";

/**
 * The longest the engine lets each model route work before it answers, in
 * seconds: a test runs the CLI for real (up to three runs), a credential's test
 * one isolated run, a retry one probe. The dashboard waits a little past each
 * rather than abandoning a request the engine is still answering. Held against
 * `llmapi.CheckBudget`, `CredentialBudget` and `RetryBudget` by
 * `internal/api/llmapi.TestTheModelsScreenWaitsAsLongAsTheEngineWorks`.
 */
export const MODEL_CHECK_BUDGET_SECONDS = 180;
export const MODEL_CREDENTIAL_BUDGET_SECONDS = 120;
export const MODEL_RETRY_BUDGET_SECONDS = 60;
